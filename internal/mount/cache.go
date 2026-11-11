package mount

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/HeLingQi/tencent-weiyun-go/internal/remote"
	"golang.org/x/sys/unix"
)

const (
	chunkSize    = 1 << 20
	identityFile = "identity.json"
	lockFile     = ".mount.lock"
	manifestsDir = "manifests"
	dataDir      = "data"
	recoveryDir  = "recovery"
)

var (
	ErrCacheLocked    = errors.New("cache directory is already mounted")
	ErrUnsafeCache    = errors.New("unsafe cache directory")
	ErrCacheIdentity  = errors.New("cache belongs to another authorization or remote root")
	ErrUnsafeManifest = errors.New("unsafe cache manifest")
	ErrPathOverlap    = errors.New("cache directory and mountpoint must not contain each other")
)

type Identity struct {
	RemoteRoot       string `json:"remote_root"`
	TokenFingerprint string `json:"token_fingerprint"`
}

type manifest struct {
	ID        string          `json:"id"`
	Path      string          `json:"path"`
	Version   *remote.Version `json:"version,omitempty"`
	Size      int64           `json:"size"`
	MTime     int64           `json:"mtime"`
	Dirty     bool            `json:"dirty"`
	LocalOnly bool            `json:"local_only"`
	Sparse    bool            `json:"sparse,omitempty"`
	Chunks    []byte          `json:"chunks,omitempty"`
	LastError string          `json:"last_error,omitempty"`
}

type readSession struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type inflightChunk struct {
	done chan struct{}
	err  error
}

type fileState struct {
	manifest
	mu               sync.Mutex
	readers          int
	openReaders      int
	session          *readSession
	inflight         map[int]*inflightChunk
	chunksDirty      bool
	pendingReads     int
	writer           bool
	sessionModified  bool
	generation       uint64
	syncedGeneration uint64
}

func numChunks(size int64) int {
	if size <= 0 {
		return 0
	}
	return int((size + chunkSize - 1) / chunkSize)
}

func (s *fileState) hasChunkLocked(idx int) bool {
	if !s.Sparse {
		return true
	}
	if idx < 0 || idx >= numChunks(s.Size) {
		return true
	}
	b := idx / 8
	return b < len(s.Chunks) && (s.Chunks[b]&(1<<uint(idx%8))) != 0
}

func (s *fileState) markChunksLocked(start, end int) {
	if !s.Sparse {
		return
	}
	total := numChunks(s.Size)
	wantLen := (total + 7) / 8
	if len(s.Chunks) < wantLen {
		padded := make([]byte, wantLen)
		copy(padded, s.Chunks)
		s.Chunks = padded
	}
	for i := start; i <= end && i < total; i++ {
		s.Chunks[i/8] |= 1 << uint(i%8)
	}
	s.chunksDirty = true
	if allChunksPresent(s.Chunks, total) {
		s.Sparse = false
		s.Chunks = nil
	}
}

func allChunksPresent(chunks []byte, total int) bool {
	fullBytes := total / 8
	if len(chunks) < fullBytes {
		return false
	}
	for i := 0; i < fullBytes; i++ {
		if chunks[i] != 0xff {
			return false
		}
	}
	rem := total % 8
	if rem == 0 {
		return true
	}
	if len(chunks) <= fullBytes {
		return false
	}
	mask := byte((1 << uint(rem)) - 1)
	return chunks[fullBytes]&mask == mask
}

type Cache struct {
	root     string
	lock     *os.File
	mu       sync.RWMutex
	entries  map[string]*fileState
	identity Identity
}

func Fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}

func DefaultCacheDir(token, remoteRoot string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "weiyun", "mounts", Fingerprint(token), Fingerprint(remoteRoot)), nil
}

func ValidatePaths(cacheDir, mountpoint string) error {
	cacheAbs, err := filepath.Abs(cacheDir)
	if err != nil {
		return err
	}
	mountAbs, err := filepath.Abs(mountpoint)
	if err != nil {
		return err
	}
	if containsPath(cacheAbs, mountAbs) || containsPath(mountAbs, cacheAbs) {
		return ErrPathOverlap
	}
	return nil
}

func containsPath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func OpenCache(root string, identity Identity) (*Cache, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if abs == string(filepath.Separator) {
		return nil, ErrUnsafeCache
	}
	if err := makePrivateDir(abs); err != nil {
		return nil, err
	}
	for _, name := range []string{manifestsDir, dataDir, recoveryDir} {
		if err := makePrivateDir(filepath.Join(abs, name)); err != nil {
			return nil, err
		}
	}

	lockPath := filepath.Join(abs, lockFile)
	if info, err := os.Lstat(lockPath); err == nil {
		if !privateOwnedRegular(info) {
			return nil, ErrUnsafeCache
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lock.Chmod(0o600); err != nil {
		lock.Close()
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrCacheLocked
		}
		return nil, err
	}

	cache := &Cache{root: abs, lock: lock, entries: make(map[string]*fileState), identity: identity}
	if err := cache.bindIdentity(); err != nil {
		cache.Close()
		return nil, err
	}
	if err := cache.load(); err != nil {
		cache.Close()
		return nil, err
	}
	return cache, nil
}

func (c *Cache) Close() error {
	if c.lock == nil {
		return nil
	}
	lock := c.lock
	c.lock = nil
	unlockErr := unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	closeErr := lock.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func (c *Cache) bindIdentity() error {
	filename := filepath.Join(c.root, identityFile)
	info, err := os.Lstat(filename)
	if err == nil {
		if !privateOwnedRegular(info) {
			return ErrUnsafeCache
		}
		data, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		var stored Identity
		if decodeStrict(data, &stored) != nil || stored != c.identity {
			return ErrCacheIdentity
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := json.Marshal(c.identity)
	if err != nil {
		return err
	}
	return atomicWrite(filename, data)
}

func (c *Cache) load() error {
	entries, err := os.ReadDir(filepath.Join(c.root, manifestsDir))
	if err != nil {
		return err
	}
	for _, item := range entries {
		if item.Type()&os.ModeSymlink != 0 || item.IsDir() || !strings.HasSuffix(item.Name(), ".json") {
			return ErrUnsafeManifest
		}
		filename := filepath.Join(c.root, manifestsDir, item.Name())
		info, err := os.Lstat(filename)
		if err != nil || !privateOwnedRegular(info) || info.Size() > 1<<20 {
			return ErrUnsafeManifest
		}
		data, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		var value manifest
		if decodeStrict(data, &value) != nil || item.Name() != value.ID+".json" || !validID(value.ID) || !validManifestPath(value.Path) || value.Size < 0 {
			return ErrUnsafeManifest
		}
		dataInfo, err := os.Lstat(c.dataPath(value.ID))
		if err != nil || !privateOwnedRegular(dataInfo) {
			return ErrUnsafeManifest
		}
		if dataInfo.Size() != value.Size {
			if !value.Dirty {
				return ErrUnsafeManifest
			}
			value.Size = dataInfo.Size()
		}
		if value.Sparse {
			if value.Dirty || value.Size <= 0 {
				return ErrUnsafeManifest
			}
			wantLen := (numChunks(value.Size) + 7) / 8
			if len(value.Chunks) > wantLen {
				return ErrUnsafeManifest
			}
			if len(value.Chunks) < wantLen {
				padded := make([]byte, wantLen)
				copy(padded, value.Chunks)
				value.Chunks = padded
			}
		}
		if _, exists := c.entries[value.Path]; exists {
			return ErrUnsafeManifest
		}
		c.entries[value.Path] = &fileState{manifest: value}
	}
	return nil
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && strings.ToLower(id) == id
}

func validManifestPath(name string) bool {
	return strings.HasPrefix(name, "/") && name != "/" && path.Clean(name) == name && !strings.ContainsRune(name, '\x00')
}

func (c *Cache) newLocal(name string) (*fileState, error) {
	if !validManifestPath(name) {
		return nil, ErrUnsafeManifest
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[name]; exists {
		return nil, os.ErrExist
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(c.dataPath(id), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		os.Remove(c.dataPath(id))
		return nil, err
	}
	state := &fileState{manifest: manifest{ID: id, Path: name, Dirty: true, LocalOnly: true}}
	state.generation = 1
	if err := c.persistLocked(state); err != nil {
		os.Remove(c.dataPath(id))
		return nil, err
	}
	c.entries[name] = state
	return state, nil
}

func randomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (c *Cache) persistLocked(state *fileState) error {
	data, err := json.Marshal(state.manifest)
	if err != nil {
		return err
	}
	return atomicWrite(c.manifestPath(state.ID), data)
}

func (c *Cache) entry(name string) *fileState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.entries[name]
}

func (c *Cache) installSparseRemote(name string, version remote.Version) (*fileState, error) {
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	target := c.dataPath(id)
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(target)
		}
	}()
	if version.Size > 0 {
		if err := file.Truncate(version.Size); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	sparse := version.Size > 0
	var chunks []byte
	if sparse {
		chunks = make([]byte, (numChunks(version.Size)+7)/8)
	}
	state := &fileState{manifest: manifest{
		ID:      id,
		Path:    name,
		Version: &version,
		Size:    version.Size,
		MTime:   version.MTime,
		Sparse:  sparse,
		Chunks:  chunks,
	}}
	if err := c.persistLocked(state); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if _, exists := c.entries[name]; exists {
		c.mu.Unlock()
		_ = os.Remove(c.manifestPath(id))
		return nil, os.ErrExist
	}
	c.entries[name] = state
	c.mu.Unlock()
	keep = true
	return state, nil
}

func (c *Cache) removeClean(state *fileState) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.Dirty || state.readers != 0 || state.pendingReads != 0 || state.writer {
		return syscall.EBUSY
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, state.Path)
	if err := os.Remove(c.manifestPath(state.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(c.dataPath(state.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (c *Cache) recover(state *fileState) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.readers != 0 || state.pendingReads != 0 || state.writer {
		return syscall.EBUSY
	}
	record := filepath.Join(c.root, recoveryDir, state.ID+".json")
	data, err := json.Marshal(state.manifest)
	if err != nil {
		return err
	}
	if err := atomicWrite(record, data); err != nil {
		return err
	}
	if err := os.Rename(c.dataPath(state.ID), filepath.Join(c.root, recoveryDir, state.ID+".data")); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.entries, state.Path)
	c.mu.Unlock()
	if err := os.Remove(c.manifestPath(state.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(filepath.Join(c.root, recoveryDir))
}

func (c *Cache) children(dir string) []*fileState {
	prefix := dir
	if prefix != "/" {
		prefix += "/"
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	var result []*fileState
	for name, state := range c.entries {
		rest := strings.TrimPrefix(name, prefix)
		if rest != name && rest != "" && !strings.Contains(rest, "/") {
			result = append(result, state)
		}
	}
	return result
}

func (c *Cache) hasDescendant(dir string) bool {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	c.mu.RLock()
	defer c.mu.RUnlock()
	for name := range c.entries {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func (c *Cache) renameClean(state *fileState, name string) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.Dirty || state.readers != 0 || state.pendingReads != 0 || state.writer {
		return syscall.EBUSY
	}
	old := state.Path
	state.Path = name
	if err := c.persistLocked(state); err != nil {
		state.Path = old
		return err
	}
	c.mu.Lock()
	delete(c.entries, old)
	c.entries[name] = state
	c.mu.Unlock()
	return nil
}

func (c *Cache) dataPath(id string) string {
	return filepath.Join(c.root, dataDir, id)
}

func (c *Cache) manifestPath(id string) string {
	return filepath.Join(c.root, manifestsDir, id+".json")
}

func makePrivateDir(name string) error {
	if err := rejectSymlinkComponents(name); err != nil {
		return err
	}
	if err := os.MkdirAll(name, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(name)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		return ErrUnsafeCache
	}
	return nil
}

func rejectSymlinkComponents(name string) error {
	clean := filepath.Clean(name)
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafeCache
		}
		if info.IsDir() && info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
			return ErrUnsafeCache
		}
	}
	return nil
}

func privateOwnedRegular(info os.FileInfo) bool {
	return info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular() && info.Mode().Perm()&0o077 == 0 && ownedByCurrentUser(info)
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func atomicWrite(filename string, data []byte) error {
	dir := filepath.Dir(filename)
	file, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, filename); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync cache directory: %w", err)
	}
	return nil
}

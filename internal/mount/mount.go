package mount

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/HeLingQi/tencent-weiyun-go/internal/remote"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"
)

type Backend interface {
	List(context.Context, string) (remote.Listing, error)
	Resolve(context.Context, string) (remote.Entry, error)
	ReadRange(context.Context, remote.Version, int64, []byte) error
	PutFile(context.Context, string, *remote.Version, io.ReaderAt, int64) (remote.Version, error)
	Mkdir(context.Context, string) (string, error)
	Move(context.Context, string, string) error
	Trash(context.Context, string) error
}

const (
	maxBatchChunks = 8
	prefetchChunks = 2
)

type FileSystem struct {
	root     string
	backend  Backend
	cache    *Cache
	cacheMu  sync.Mutex
	stopping atomic.Bool
}

func NewFileSystem(root string, backend Backend, cache *Cache) *FileSystem {
	return &FileSystem{root: root, backend: backend, cache: cache}
}

func (f *FileSystem) remotePath(name string) string {
	return "wy:" + path.Join(f.root, name)
}

func (f *FileSystem) ensureStateLocked(ctx context.Context, name string) (*fileState, syscall.Errno) {
	if state := f.cache.entry(name); state != nil {
		state.mu.Lock()
		dirty := state.Dirty
		version := state.Version
		busy := state.readers != 0 || state.pendingReads != 0 || state.writer
		state.mu.Unlock()
		if dirty || version == nil {
			return state, 0
		}
		entry, err := f.backend.Resolve(ctx, f.remotePath(name))
		if err != nil {
			return nil, errno(err)
		}
		current, ok := entryVersion(entry)
		if !ok {
			return nil, syscall.EISDIR
		}
		if current == *version {
			return state, 0
		}
		if busy {
			return nil, syscall.EBUSY
		}
		if err := f.cache.removeClean(state); err != nil {
			return nil, syscall.EIO
		}
	}

	entry, err := f.backend.Resolve(ctx, f.remotePath(name))
	if err != nil {
		return nil, errno(err)
	}
	version, ok := entryVersion(entry)
	if !ok {
		return nil, syscall.EISDIR
	}
	state, err := f.cache.installSparseRemote(name, version)
	if err != nil {
		return nil, errno(err)
	}
	return state, 0
}

func entryVersion(entry remote.Entry) (remote.Version, bool) {
	if entry.File == nil {
		return remote.Version{}, false
	}
	return remote.Version{FileID: entry.File.FileID, ParentKey: entry.ParentKey, Name: entry.Name, Size: entry.File.FileSize, MTime: entry.File.FileMTime}, true
}

func (f *FileSystem) open(ctx context.Context, name string, flags uint32) (*FileHandle, syscall.Errno) {
	f.cacheMu.Lock()
	defer f.cacheMu.Unlock()
	return f.openLocked(ctx, name, flags)
}

func (f *FileSystem) openLocked(ctx context.Context, name string, flags uint32) (*FileHandle, syscall.Errno) {
	state, eno := f.ensureStateLocked(ctx, name)
	if eno != 0 {
		return nil, eno
	}
	access := flags & syscall.O_ACCMODE
	writable := access == syscall.O_WRONLY || access == syscall.O_RDWR
	if writable && f.stopping.Load() {
		return nil, syscall.EBUSY
	}
	var session *readSession
	state.mu.Lock()
	if writable {
		if state.writer || state.readers != 0 || state.pendingReads != 0 {
			state.mu.Unlock()
			return nil, syscall.EBUSY
		}
		state.writer = true
	} else {
		if state.writer {
			state.mu.Unlock()
			return nil, syscall.EBUSY
		}
		state.readers++
		state.openReaders++
		if state.session == nil {
			sctx, cancel := context.WithCancel(context.Background())
			state.session = &readSession{ctx: sctx, cancel: cancel}
		}
		session = state.session
	}
	state.mu.Unlock()

	openFlags := os.O_RDONLY
	if writable {
		openFlags = os.O_RDWR
	}
	file, err := os.OpenFile(f.cache.dataPath(state.ID), openFlags, 0)
	if err != nil {
		f.releaseSlot(state, writable, session)
		return nil, errno(err)
	}
	h := &FileHandle{fs: f, state: state, file: file, session: session, writable: writable}
	if writable && flags&syscall.O_TRUNC != 0 {
		if eno := h.truncateWithContext(ctx, 0); eno != 0 {
			_ = h.Release(ctx)
			return nil, eno
		}
	}
	return h, 0
}

func (f *FileSystem) releaseSlot(state *fileState, writable bool, session *readSession) {
	if writable {
		state.mu.Lock()
		state.writer = false
		state.mu.Unlock()
		return
	}
	wait := false
	state.mu.Lock()
	state.openReaders--
	if state.openReaders == 0 && state.session == session && session != nil {
		state.session = nil
		session.cancel()
		wait = true
	}
	state.mu.Unlock()
	if wait {
		session.wg.Wait()
	}
	state.mu.Lock()
	state.readers--
	if state.readers == 0 && state.chunksDirty && !state.Dirty {
		state.chunksDirty = false
		_ = f.cache.persistLocked(state)
	}
	state.mu.Unlock()
}

func (f *FileSystem) ensureChunkRange(ctx context.Context, state *fileState, startChunk, endChunk int) error {
	for c := startChunk; c <= endChunk; {
		state.mu.Lock()
		total := numChunks(state.Size)
		if !state.Sparse || state.Version == nil || c >= total {
			state.mu.Unlock()
			return nil
		}
		if state.hasChunkLocked(c) {
			state.mu.Unlock()
			c++
			continue
		}
		if state.inflight == nil {
			state.inflight = make(map[int]*inflightChunk)
		}
		if wait, ok := state.inflight[c]; ok {
			state.mu.Unlock()
			select {
			case <-wait.done:
				if wait.err != nil {
					return wait.err
				}
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		batchEnd := c
		maxEnd := endChunk
		if total-1 < maxEnd {
			maxEnd = total - 1
		}
		if c+maxBatchChunks-1 < maxEnd {
			maxEnd = c + maxBatchChunks - 1
		}
		for batchEnd+1 <= maxEnd && !state.hasChunkLocked(batchEnd+1) && state.inflight[batchEnd+1] == nil {
			batchEnd++
		}
		call := &inflightChunk{done: make(chan struct{})}
		for i := c; i <= batchEnd; i++ {
			state.inflight[i] = call
		}
		ver := *state.Version
		byteStart := int64(c) * chunkSize
		byteEnd := int64(batchEnd+1) * chunkSize
		if byteEnd > state.Size {
			byteEnd = state.Size
		}
		dataPath := f.cache.dataPath(state.ID)
		state.mu.Unlock()

		buf := make([]byte, byteEnd-byteStart)
		err := f.backend.ReadRange(ctx, ver, byteStart, buf)
		if err == nil {
			out, openErr := os.OpenFile(dataPath, os.O_WRONLY, 0)
			if openErr != nil {
				err = openErr
			} else {
				_, writeErr := out.WriteAt(buf, byteStart)
				closeErr := out.Close()
				if writeErr != nil {
					err = writeErr
				} else if closeErr != nil {
					err = closeErr
				}
			}
		}

		state.mu.Lock()
		for i := c; i <= batchEnd; i++ {
			delete(state.inflight, i)
		}
		if err == nil {
			state.markChunksLocked(c, batchEnd)
		}
		call.err = err
		close(call.done)
		state.mu.Unlock()

		if err != nil {
			return err
		}
		c = batchEnd + 1
	}
	return nil
}

func (f *FileSystem) maybePrefetch(session *readSession, state *fileState, startChunk, endChunk int) {
	if session == nil {
		return
	}
	state.mu.Lock()
	total := numChunks(state.Size)
	if !state.Sparse || state.openReaders == 0 || session.ctx.Err() != nil || startChunk >= total {
		state.mu.Unlock()
		return
	}
	if endChunk >= total {
		endChunk = total - 1
	}
	need := false
	for i := startChunk; i <= endChunk; i++ {
		if !state.hasChunkLocked(i) && (state.inflight == nil || state.inflight[i] == nil) {
			need = true
			break
		}
	}
	if !need {
		state.mu.Unlock()
		return
	}
	session.wg.Add(1)
	state.mu.Unlock()

	go func() {
		defer session.wg.Done()
		_ = f.ensureChunkRange(session.ctx, state, startChunk, endChunk)
	}()
}

func (f *FileSystem) SyncDirty(ctx context.Context) error {
	f.stopping.Store(true)
	f.cache.mu.RLock()
	states := make([]*fileState, 0, len(f.cache.entries))
	for _, state := range f.cache.entries {
		states = append(states, state)
	}
	f.cache.mu.RUnlock()
	var first error
	for _, state := range states {
		state.mu.Lock()
		shouldSync := state.Dirty && state.sessionModified
		state.mu.Unlock()
		if !shouldSync {
			continue
		}
		file, err := os.OpenFile(f.cache.dataPath(state.ID), os.O_RDWR, 0)
		if err == nil {
			err = f.commit(ctx, state, file)
			_ = file.Close()
		}
		if err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (f *FileSystem) commit(ctx context.Context, state *fileState, file *os.File) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.Dirty || !state.sessionModified || state.generation == state.syncedGeneration {
		return file.Sync()
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if state.Size == 0 {
		state.LastError = "empty file upload is unsupported"
		_ = f.cache.persistLocked(state)
		return syscall.EOPNOTSUPP
	}
	old := state.manifest
	oldSyncedGeneration := state.syncedGeneration
	version, err := f.backend.PutFile(ctx, f.remotePath(state.Path), state.Version, file, state.Size)
	if err != nil {
		state.LastError = "remote upload failed"
		_ = f.cache.persistLocked(state)
		return err
	}
	state.Version = &version
	state.Size = version.Size
	state.MTime = version.MTime
	state.Dirty = false
	state.LocalOnly = false
	state.Sparse = false
	state.Chunks = nil
	state.chunksDirty = false
	state.LastError = ""
	state.syncedGeneration = state.generation
	if err := f.cache.persistLocked(state); err != nil {
		state.manifest = old
		state.syncedGeneration = oldSyncedGeneration
		state.Dirty = true
		state.LastError = "remote result could not be committed locally"
		return err
	}
	return nil
}

type FileHandle struct {
	fs          *FileSystem
	state       *fileState
	file        *os.File
	session     *readSession
	writable    bool
	modified    bool
	hasRead     bool
	lastReadEnd int64
	mu          sync.Mutex
	released    bool
}

func (h *FileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if off < 0 {
		return nil, syscall.EINVAL
	}
	h.mu.Lock()
	if h.released {
		h.mu.Unlock()
		return nil, syscall.EBADF
	}
	if h.writable {
		defer h.mu.Unlock()
		h.state.mu.Lock()
		size := h.state.Size
		sparse := h.state.Sparse
		h.state.mu.Unlock()
		if len(dest) == 0 || off >= size {
			return fuse.ReadResultData(nil), 0
		}
		reqLen := len(dest)
		if off+int64(reqLen) > size {
			reqLen = int(size - off)
		}
		if sparse {
			firstChunk := int(off / chunkSize)
			lastChunk := int((off + int64(reqLen) - 1) / chunkSize)
			if err := h.fs.ensureChunkRange(ctx, h.state, firstChunk, lastChunk); err != nil {
				return nil, errno(err)
			}
		}
		data := make([]byte, reqLen)
		h.state.mu.Lock()
		n, err := h.file.ReadAt(data, off)
		h.state.mu.Unlock()
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, errno(err)
		}
		return fuse.ReadResultData(data[:n]), 0
	}

	h.state.mu.Lock()
	size := h.state.Size
	sparse := h.state.Sparse
	h.state.mu.Unlock()
	if len(dest) == 0 || off >= size {
		h.mu.Unlock()
		return fuse.ReadResultData(nil), 0
	}
	reqLen := len(dest)
	if off+int64(reqLen) > size {
		reqLen = int(size - off)
	}
	sequential := h.hasRead && off == h.lastReadEnd
	h.hasRead = true
	h.lastReadEnd = off + int64(reqLen)
	fd, err := unix.Dup(int(h.file.Fd()))
	if err != nil {
		h.mu.Unlock()
		return nil, errno(err)
	}
	h.state.mu.Lock()
	h.state.pendingReads++
	h.state.mu.Unlock()
	session := h.session
	h.mu.Unlock()

	if sparse {
		firstChunk := int(off / chunkSize)
		lastChunk := int((off + int64(reqLen) - 1) / chunkSize)
		if err := h.fs.ensureChunkRange(ctx, h.state, firstChunk, lastChunk); err != nil {
			_ = unix.Close(fd)
			h.state.mu.Lock()
			h.state.pendingReads--
			h.state.mu.Unlock()
			return nil, errno(err)
		}
		if sequential || lastChunk > firstChunk {
			h.fs.maybePrefetch(session, h.state, lastChunk+1, lastChunk+prefetchChunks)
		}
	}
	return &fdReadResult{ReadResult: fuse.ReadResultFd(uintptr(fd), off, reqLen), fd: fd, state: h.state}, 0
}

type fdReadResult struct {
	fuse.ReadResult
	fd    int
	state *fileState
	once  sync.Once
}

func (r *fdReadResult) Done() {
	r.once.Do(func() {
		r.ReadResult.Done()
		_ = unix.Close(r.fd)
		r.state.mu.Lock()
		r.state.pendingReads--
		r.state.mu.Unlock()
	})
}

func (h *FileHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released || !h.writable {
		return 0, syscall.EBADF
	}
	if h.fs.stopping.Load() {
		return 0, syscall.EBUSY
	}
	h.state.mu.Lock()
	needHydrate := h.state.Sparse && !(off == 0 && int64(len(data)) >= h.state.Size)
	totalChunks := numChunks(h.state.Size)
	h.state.mu.Unlock()
	if needHydrate && totalChunks > 0 {
		if err := h.fs.ensureChunkRange(ctx, h.state, 0, totalChunks-1); err != nil {
			return 0, errno(err)
		}
	}

	h.state.mu.Lock()
	defer h.state.mu.Unlock()
	if h.fs.stopping.Load() {
		return 0, syscall.EBUSY
	}
	if err := h.markDirtyLocked(); err != nil {
		return 0, syscall.EIO
	}
	n, err := h.file.WriteAt(data, off)
	if err != nil {
		return uint32(n), errno(err)
	}
	info, err := h.file.Stat()
	if err != nil {
		return uint32(n), errno(err)
	}
	h.state.Size = info.Size()
	h.state.Sparse = false
	h.state.Chunks = nil
	h.state.chunksDirty = false
	h.state.generation++
	h.modified = true
	if err := h.fs.cache.persistLocked(h.state); err != nil {
		return uint32(n), syscall.EIO
	}
	return uint32(n), 0
}

func (h *FileHandle) markDirtyLocked() error {
	if !h.state.Dirty {
		h.state.Dirty = true
	}
	h.state.sessionModified = true
	return h.fs.cache.persistLocked(h.state)
}

func (h *FileHandle) truncate(size int64) syscall.Errno {
	return h.truncateWithContext(context.Background(), size)
}

func (h *FileHandle) truncateWithContext(ctx context.Context, size int64) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released || !h.writable {
		return syscall.EBADF
	}
	if h.fs.stopping.Load() {
		return syscall.EBUSY
	}
	h.state.mu.Lock()
	sparse := h.state.Sparse
	oldSize := h.state.Size
	h.state.mu.Unlock()
	if sparse && size > 0 {
		keep := size
		if oldSize < keep {
			keep = oldSize
		}
		needChunks := numChunks(keep)
		if needChunks > 0 {
			if err := h.fs.ensureChunkRange(ctx, h.state, 0, needChunks-1); err != nil {
				return errno(err)
			}
		}
	}

	h.state.mu.Lock()
	defer h.state.mu.Unlock()
	if h.fs.stopping.Load() {
		return syscall.EBUSY
	}
	if err := h.markDirtyLocked(); err != nil {
		return syscall.EIO
	}
	if err := h.file.Truncate(size); err != nil {
		return errno(err)
	}
	h.state.Size = size
	h.state.Sparse = false
	h.state.Chunks = nil
	h.state.chunksDirty = false
	h.state.generation++
	h.modified = true
	if err := h.fs.cache.persistLocked(h.state); err != nil {
		return syscall.EIO
	}
	return 0
}

func (h *FileHandle) Fsync(ctx context.Context, _ uint32) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released {
		return syscall.EBADF
	}
	if !h.writable || !h.modified {
		return errno(h.file.Sync())
	}
	err := h.fs.commit(ctx, h.state, h.file)
	if err == nil {
		h.modified = false
		return 0
	}
	if errors.Is(err, remote.ErrVersionConflict) {
		return syscall.ESTALE
	}
	if errors.Is(err, syscall.EOPNOTSUPP) {
		return syscall.EOPNOTSUPP
	}
	return syscall.EIO
}

func (h *FileHandle) Flush(ctx context.Context) syscall.Errno {
	eno := h.Fsync(ctx, 0)
	if eno == syscall.EOPNOTSUPP {
		return syscall.EIO
	}
	return eno
}

func (h *FileHandle) Release(context.Context) syscall.Errno {
	h.mu.Lock()
	if h.released {
		h.mu.Unlock()
		return 0
	}
	h.released = true
	session := h.session
	err := h.file.Close()
	h.mu.Unlock()
	h.fs.releaseSlot(h.state, h.writable, session)
	return errno(err)
}

func (h *FileHandle) Getattr(_ context.Context, out *fuse.AttrOut) syscall.Errno {
	h.state.mu.Lock()
	defer h.state.mu.Unlock()
	fillFileAttr(&out.Attr, h.state.Size, h.state.MTime)
	return 0
}

func (h *FileHandle) Setattr(ctx context.Context, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if in.Valid & ^uint32(fuse.FATTR_SIZE|fuse.FATTR_FH|fuse.FATTR_LOCKOWNER) != 0 {
		return syscall.EOPNOTSUPP
	}
	size, ok := in.GetSize()
	if !ok {
		return syscall.EOPNOTSUPP
	}
	if eno := h.truncateWithContext(ctx, int64(size)); eno != 0 {
		return eno
	}
	return h.Getattr(ctx, out)
}

func errno(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	var eno syscall.Errno
	if errors.As(err, &eno) {
		return eno
	}
	switch {
	case errors.Is(err, remote.ErrNotFound), errors.Is(err, os.ErrNotExist):
		return syscall.ENOENT
	case errors.Is(err, remote.ErrConflict), errors.Is(err, os.ErrExist):
		return syscall.EEXIST
	case errors.Is(err, remote.ErrCrossDirectoryRename):
		return syscall.EXDEV
	case errors.Is(err, remote.ErrVersionConflict):
		return syscall.ESTALE
	default:
		return syscall.EIO
	}
}

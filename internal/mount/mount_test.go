package mount

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/HeLingQi/tencent-weiyun-go/internal/remote"
	"github.com/HeLingQi/tencent-weiyun-go/internal/weiyun"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestDirtyRecoveryDoesNotUploadUntilModifiedThisSession(t *testing.T) {
	cache, backend := testCacheAndBackend(t)
	state, err := cache.newLocal("/a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache.dataPath(state.ID), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	state.Size = 3
	state.Dirty = true
	state.sessionModified = false
	err = cache.persistLocked(state)
	state.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	fsys := NewFileSystem("/", backend, cache)
	h, errno := fsys.open(context.Background(), "/a", syscall.O_RDWR)
	if errno != 0 {
		t.Fatal(errno)
	}
	if errno := h.Flush(context.Background()); errno != 0 {
		t.Fatalf("flush=%v", errno)
	}
	if backend.putCalls != 0 {
		t.Fatalf("uploads=%d", backend.putCalls)
	}
	if _, errno := h.Write(context.Background(), []byte("x"), 0); errno != 0 {
		t.Fatal(errno)
	}
	if errno := h.Fsync(context.Background(), 0); errno != 0 {
		t.Fatal(errno)
	}
	if backend.putCalls != 1 {
		t.Fatalf("uploads=%d", backend.putCalls)
	}
	_ = h.Release(context.Background())
}

func TestUploadFailureConflictDuplicateFlushAndZeroByte(t *testing.T) {
	t.Run("failure keeps dirty", func(t *testing.T) {
		cache, backend := testCacheAndBackend(t)
		state, _ := cache.newLocal("/a")
		fsys := NewFileSystem("/", backend, cache)
		h, errno := fsys.open(context.Background(), "/a", syscall.O_RDWR)
		if errno != 0 {
			t.Fatal(errno)
		}
		if _, errno = h.Write(context.Background(), []byte("abc"), 0); errno != 0 {
			t.Fatal(errno)
		}
		putErr := errors.New("transport secret")
		backend.setPutErr(putErr)
		if errno = h.Flush(context.Background()); errno != syscall.EIO {
			t.Fatalf("flush=%v", errno)
		}
		if !state.Dirty || state.LastError == "" || state.LastError == putErr.Error() {
			t.Fatalf("state=%+v", state.manifest)
		}
		_ = h.Release(context.Background())
	})

	t.Run("conflict is stale", func(t *testing.T) {
		cache, backend := testCacheAndBackend(t)
		backend.files["/a"] = fakeFile{data: []byte("old"), version: remote.Version{FileID: "f1", ParentKey: "root", Name: "a", Size: 3, MTime: 1}}
		fsys := NewFileSystem("/", backend, cache)
		h, errno := fsys.open(context.Background(), "/a", syscall.O_RDWR)
		if errno != 0 {
			t.Fatal(errno)
		}
		if _, errno = h.Write(context.Background(), []byte("new"), 0); errno != 0 {
			t.Fatal(errno)
		}
		backend.setPutErr(remote.ErrVersionConflict)
		if errno = h.Fsync(context.Background(), 0); errno != syscall.ESTALE {
			t.Fatalf("fsync=%v", errno)
		}
		_ = h.Release(context.Background())
	})

	t.Run("duplicate flush uploads once", func(t *testing.T) {
		cache, backend := testCacheAndBackend(t)
		_, _ = cache.newLocal("/a")
		fsys := NewFileSystem("/", backend, cache)
		h, errno := fsys.open(context.Background(), "/a", syscall.O_RDWR)
		if errno != 0 {
			t.Fatal(errno)
		}
		_, _ = h.Write(context.Background(), []byte("abc"), 0)
		if h.Flush(context.Background()) != 0 || h.Flush(context.Background()) != 0 || backend.putCalls != 1 {
			t.Fatalf("uploads=%d", backend.putCalls)
		}
		_ = h.Release(context.Background())
	})

	t.Run("zero byte unsupported", func(t *testing.T) {
		cache, backend := testCacheAndBackend(t)
		state, _ := cache.newLocal("/a")
		state.sessionModified = true
		fsys := NewFileSystem("/", backend, cache)
		h, errno := fsys.open(context.Background(), "/a", syscall.O_RDWR)
		if errno != 0 {
			t.Fatal(errno)
		}
		h.modified = true
		if errno = h.Fsync(context.Background(), 0); errno != syscall.EOPNOTSUPP {
			t.Fatalf("fsync=%v", errno)
		}
		if errno = h.Flush(context.Background()); errno != syscall.EIO {
			t.Fatalf("flush=%v", errno)
		}
		if !state.Dirty || backend.putCalls != 0 {
			t.Fatalf("dirty=%v uploads=%d", state.Dirty, backend.putCalls)
		}
		_ = h.Release(context.Background())
	})
}

func TestReadersAndWriterAreMutuallyExclusive(t *testing.T) {
	cache, backend := testCacheAndBackend(t)
	backend.files["/a"] = fakeFile{data: []byte("abc"), version: remote.Version{FileID: "f", ParentKey: "root", Name: "a", Size: 3, MTime: 1}}
	fsys := NewFileSystem("/", backend, cache)
	r1, errno := fsys.open(context.Background(), "/a", syscall.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	if _, errno = fsys.open(context.Background(), "/a", syscall.O_RDWR); errno != syscall.EBUSY {
		t.Fatalf("writer=%v", errno)
	}
	r2, errno := fsys.open(context.Background(), "/a", syscall.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	_ = r1.Release(context.Background())
	_ = r2.Release(context.Background())
	w, errno := fsys.open(context.Background(), "/a", syscall.O_RDWR)
	if errno != 0 {
		t.Fatal(errno)
	}
	if _, errno = fsys.open(context.Background(), "/a", syscall.O_RDONLY); errno != syscall.EBUSY {
		t.Fatalf("reader=%v", errno)
	}
	if _, errno = fsys.open(context.Background(), "/a", syscall.O_WRONLY); errno != syscall.EBUSY {
		t.Fatalf("writer2=%v", errno)
	}
	_ = w.Release(context.Background())
}

func TestStoppingRejectsExistingHandleWritesAndTruncate(t *testing.T) {
	cache, backend := testCacheAndBackend(t)
	_, _ = cache.newLocal("/a")
	fsys := NewFileSystem("/", backend, cache)
	handle, eno := fsys.open(context.Background(), "/a", syscall.O_RDWR)
	if eno != 0 {
		t.Fatal(eno)
	}
	fsys.stopping.Store(true)
	if _, eno := handle.Write(context.Background(), []byte("x"), 0); eno != syscall.EBUSY {
		t.Fatalf("write=%v", eno)
	}
	if eno := handle.truncate(0); eno != syscall.EBUSY {
		t.Fatalf("truncate=%v", eno)
	}
	_ = handle.Release(context.Background())
}

func TestLocalOnlyRenameReturnsBusyWithoutRemoteLookup(t *testing.T) {
	cache, backend := testCacheAndBackend(t)
	_, _ = cache.newLocal("/a")
	root := NewRoot(NewFileSystem("/", backend, cache))
	if eno := root.Rename(context.Background(), "a", root, "b", 0); eno != syscall.EBUSY {
		t.Fatalf("rename=%v", eno)
	}
}

func TestReadResultDoneKeepsWriterExcludedAfterRelease(t *testing.T) {
	cache, backend := testCacheAndBackend(t)
	backend.files["/a"] = fakeFile{data: []byte("abc"), version: remote.Version{FileID: "f", ParentKey: "root", Name: "a", Size: 3, MTime: 1}}
	fsys := NewFileSystem("/", backend, cache)
	reader, eno := fsys.open(context.Background(), "/a", syscall.O_RDONLY)
	if eno != 0 {
		t.Fatal(eno)
	}
	result, eno := reader.Read(context.Background(), make([]byte, 3), 0)
	if eno != 0 {
		t.Fatal(eno)
	}
	_ = reader.Release(context.Background())
	if _, eno := fsys.open(context.Background(), "/a", syscall.O_RDWR); eno != syscall.EBUSY {
		t.Fatalf("writer before Done=%v", eno)
	}
	result.Done()
	writer, eno := fsys.open(context.Background(), "/a", syscall.O_RDWR)
	if eno != 0 {
		t.Fatal(eno)
	}
	_ = writer.Release(context.Background())
}

func TestReaddirRejectsEscapingAndDuplicateRemoteNames(t *testing.T) {
	cache, backend := testCacheAndBackend(t)
	for _, listing := range []remote.Listing{
		{Files: []weiyun.FileItem{{Filename: "../escape"}}},
		{Dirs: []weiyun.DirItem{{DirName: "same"}}, Files: []weiyun.FileItem{{Filename: "same"}}},
	} {
		fsys := NewFileSystem("/", badListBackend{Backend: backend, listing: listing}, cache)
		if _, eno := NewRoot(fsys).Readdir(context.Background()); eno != syscall.EIO {
			t.Fatalf("listing=%+v errno=%v", listing, eno)
		}
	}
}

func TestAttrsConvertMillisecondsToTimespec(t *testing.T) {
	const mtime = int64(1700000000123)
	for name, attr := range map[string]fuse.Attr{
		"file": func() fuse.Attr { var a fuse.Attr; fillFileAttr(&a, 7, mtime); return a }(),
		"dir":  func() fuse.Attr { var a fuse.Attr; fillDirAttr(&a, mtime); return a }(),
	} {
		if attr.Mtime != 1700000000 || attr.Mtimensec != 123000000 {
			t.Fatalf("%s mtime=%d.%09d", name, attr.Mtime, attr.Mtimensec)
		}
	}
	state := &fileState{manifest: manifest{Size: 7, MTime: mtime}}
	var out fuse.AttrOut
	if eno := (&FileHandle{state: state}).Getattr(context.Background(), &out); eno != 0 {
		t.Fatal(eno)
	}
	if out.Mtime != 1700000000 || out.Mtimensec != 123000000 {
		t.Fatalf("handle mtime=%d.%09d", out.Mtime, out.Mtimensec)
	}
}

func TestCleanCacheDoesNotShadowDeletedRemoteFile(t *testing.T) {
	cache, backend := testCacheAndBackend(t)
	backend.files["/a"] = fakeFile{data: []byte("abc"), version: remote.Version{FileID: "f", ParentKey: "root", Name: "a", Size: 3, MTime: 1}}
	fsys := NewFileSystem("/", backend, cache)
	h, eno := fsys.open(context.Background(), "/a", syscall.O_RDONLY)
	if eno != 0 {
		t.Fatal(eno)
	}
	_ = h.Release(context.Background())
	backend.mu.Lock()
	delete(backend.files, "/a")
	backend.mu.Unlock()
	root := NewRoot(fsys)
	_ = fs.NewNodeFS(root, &fs.Options{})
	if _, eno := root.Lookup(context.Background(), "a", &fuse.EntryOut{}); eno != syscall.ENOENT {
		t.Fatalf("lookup=%v", eno)
	}
	stream, eno := root.Readdir(context.Background())
	if eno != 0 {
		t.Fatal(eno)
	}
	defer stream.Close()
	if stream.HasNext() {
		entry, _ := stream.Next()
		t.Fatalf("ghost entry=%+v", entry)
	}
}

func TestCleanCacheDoesNotChangeRemoteDirectoryIntoFile(t *testing.T) {
	cache, backend := testCacheAndBackend(t)
	backend.files["/a"] = fakeFile{data: []byte("abc"), version: remote.Version{FileID: "f", ParentKey: "root", Name: "a", Size: 3, MTime: 1}}
	fsys := NewFileSystem("/", backend, cache)
	h, eno := fsys.open(context.Background(), "/a", syscall.O_RDONLY)
	if eno != 0 {
		t.Fatal(eno)
	}
	_ = h.Release(context.Background())
	backend.mu.Lock()
	delete(backend.files, "/a")
	backend.dirs["/a"] = true
	backend.mu.Unlock()
	root := NewRoot(fsys)
	_ = fs.NewNodeFS(root, &fs.Options{})
	var out fuse.EntryOut
	inode, eno := root.Lookup(context.Background(), "a", &out)
	if eno != 0 {
		t.Fatal(eno)
	}
	if out.Mode&syscall.S_IFMT != syscall.S_IFDIR || !inode.IsDir() {
		t.Fatalf("lookup mode=%#o inode dir=%v", out.Mode, inode.IsDir())
	}
	stream, eno := root.Readdir(context.Background())
	if eno != 0 {
		t.Fatal(eno)
	}
	defer stream.Close()
	entry, eno := stream.Next()
	if eno != 0 || entry.Name != "a" || entry.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		t.Fatalf("entry=%+v errno=%v", entry, eno)
	}
}

func TestSetattrWithoutHandleCommitsTruncate(t *testing.T) {
	cache, backend := testCacheAndBackend(t)
	backend.files["/a"] = fakeFile{data: []byte("abcdef"), version: remote.Version{FileID: "f", ParentKey: "root", Name: "a", Size: 6, MTime: 1}}
	node := &Node{fsys: NewFileSystem("/", backend, cache), path: "/a"}
	in := &fuse.SetAttrIn{}
	in.Valid = fuse.FATTR_SIZE
	in.Size = 2
	if eno := node.Setattr(context.Background(), nil, in, &fuse.AttrOut{}); eno != 0 {
		t.Fatal(eno)
	}
	backend.mu.Lock()
	got := string(backend.files["/a"].data)
	backend.mu.Unlock()
	if got != "ab" {
		t.Fatalf("remote data=%q", got)
	}
}

func TestSetattrWithoutHandlePropagatesUploadFailure(t *testing.T) {
	cache, backend := testCacheAndBackend(t)
	backend.files["/a"] = fakeFile{data: []byte("abcdef"), version: remote.Version{FileID: "f", ParentKey: "root", Name: "a", Size: 6, MTime: 1}}
	backend.setPutErr(errors.New("injected upload failure"))
	node := &Node{fsys: NewFileSystem("/", backend, cache), path: "/a"}
	in := &fuse.SetAttrIn{}
	in.Valid = fuse.FATTR_SIZE
	in.Size = 2
	if eno := node.Setattr(context.Background(), nil, in, &fuse.AttrOut{}); eno != syscall.EIO {
		t.Fatalf("setattr=%v", eno)
	}
	state := cache.entry("/a")
	if state == nil || !state.Dirty {
		t.Fatalf("state=%+v", state)
	}
	backend.mu.Lock()
	got := string(backend.files["/a"].data)
	backend.mu.Unlock()
	if got != "abcdef" {
		t.Fatalf("remote data=%q", got)
	}
}

func TestMkdirRejectsLocalOnlyFileName(t *testing.T) {
	cache, backend := testCacheAndBackend(t)
	if _, err := cache.newLocal("/a"); err != nil {
		t.Fatal(err)
	}
	root := NewRoot(NewFileSystem("/", backend, cache))
	_ = fs.NewNodeFS(root, &fs.Options{})
	if _, eno := root.Mkdir(context.Background(), "a", 0, &fuse.EntryOut{}); eno != syscall.EEXIST {
		t.Fatalf("mkdir=%v", eno)
	}
}

func TestUnlinkSerializesWithOpenUntilRemoteMutationFinishes(t *testing.T) {
	cache, base := testCacheAndBackend(t)
	base.files["/a"] = fakeFile{data: []byte("abc"), version: remote.Version{FileID: "f", ParentKey: "root", Name: "a", Size: 3, MTime: 1}}
	backend := &blockingTrashBackend{fakeBackend: base, entered: make(chan struct{}), release: make(chan struct{})}
	fsys := NewFileSystem("/", backend, cache)
	h, eno := fsys.open(context.Background(), "/a", syscall.O_RDONLY)
	if eno != 0 {
		t.Fatal(eno)
	}
	_ = h.Release(context.Background())
	root := NewRoot(fsys)
	unlinkDone := make(chan syscall.Errno, 1)
	go func() { unlinkDone <- root.Unlink(context.Background(), "a") }()
	<-backend.entered
	type result struct {
		h   *FileHandle
		eno syscall.Errno
	}
	openDone := make(chan result, 1)
	go func() {
		h, eno := fsys.open(context.Background(), "/a", syscall.O_RDONLY)
		openDone <- result{h: h, eno: eno}
	}()
	var early *result
	select {
	case got := <-openDone:
		early = &got
	case <-time.After(100 * time.Millisecond):
	}
	close(backend.release)
	unlinkEno := <-unlinkDone
	if early != nil {
		if early.h != nil {
			_ = early.h.Release(context.Background())
		}
		t.Fatalf("open completed during unlink: %v; unlink=%v", early.eno, unlinkEno)
	}
	got := <-openDone
	if unlinkEno != 0 || got.eno != syscall.ENOENT {
		t.Fatalf("unlink=%v open=%v", unlinkEno, got.eno)
	}
}

func TestRenameSerializesWithCreateAtTarget(t *testing.T) {
	cache, base := testCacheAndBackend(t)
	base.files["/a"] = fakeFile{data: []byte("abc"), version: remote.Version{FileID: "f", ParentKey: "root", Name: "a", Size: 3, MTime: 1}}
	backend := &blockingMoveBackend{fakeBackend: base, entered: make(chan struct{}), release: make(chan struct{})}
	fsys := NewFileSystem("/", backend, cache)
	h, eno := fsys.open(context.Background(), "/a", syscall.O_RDONLY)
	if eno != 0 {
		t.Fatal(eno)
	}
	_ = h.Release(context.Background())
	root := NewRoot(fsys)
	_ = fs.NewNodeFS(root, &fs.Options{})
	renameDone := make(chan syscall.Errno, 1)
	go func() { renameDone <- root.Rename(context.Background(), "a", root, "b", 0) }()
	<-backend.entered
	type result struct {
		h   fs.FileHandle
		eno syscall.Errno
	}
	createDone := make(chan result, 1)
	go func() {
		_, h, _, eno := root.Create(context.Background(), "b", syscall.O_RDWR, 0, &fuse.EntryOut{})
		createDone <- result{h: h, eno: eno}
	}()
	var early *result
	select {
	case got := <-createDone:
		early = &got
	case <-time.After(100 * time.Millisecond):
	}
	close(backend.release)
	renameEno := <-renameDone
	if early != nil {
		if h, ok := early.h.(*FileHandle); ok {
			_ = h.Release(context.Background())
		}
		t.Fatalf("create completed during rename: %v; rename=%v", early.eno, renameEno)
	}
	got := <-createDone
	if renameEno != 0 || got.eno != syscall.EEXIST {
		t.Fatalf("rename=%v create=%v", renameEno, got.eno)
	}
}

func TestSparseOpenDownloadsZeroBytesAndReadsOnlyRequestedChunk(t *testing.T) {
	rootDir := filepath.Join(t.TempDir(), "cache")
	identity := Identity{RemoteRoot: "/", TokenFingerprint: Fingerprint(t.Name())}
	cache, err := OpenCache(rootDir, identity)
	if err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{files: map[string]fakeFile{}, dirs: map[string]bool{"/": true}}
	body := make([]byte, 5*chunkSize+321)
	for i := range body {
		body[i] = byte((i * 7) % 251)
	}
	backend.files["/big.bin"] = fakeFile{
		data:    body,
		version: remote.Version{FileID: "big-1", ParentKey: "root", Name: "big.bin", Size: int64(len(body)), MTime: 100},
	}
	fsys := NewFileSystem("/", backend, cache)

	h, eno := fsys.open(context.Background(), "/big.bin", syscall.O_RDONLY)
	if eno != 0 {
		t.Fatal(eno)
	}
	backend.mu.Lock()
	openCalls := backend.rangeCalls
	backend.mu.Unlock()
	if openCalls != 0 {
		t.Fatalf("expected 0 range calls on open, got %d", openCalls)
	}

	off := int64(3*chunkSize + 100)
	res, eno := h.Read(context.Background(), make([]byte, 4096), off)
	if eno != 0 {
		t.Fatal(eno)
	}
	buf, status := res.Bytes(make([]byte, 4096))
	res.Done()
	if status != fuse.OK || string(buf) != string(body[off:off+4096]) {
		t.Fatalf("status=%v mismatch at chunk 3", status)
	}

	backend.mu.Lock()
	callsAfterRead, bytesAfterRead := backend.rangeCalls, backend.rangeBytes
	backend.mu.Unlock()
	if callsAfterRead != 1 || bytesAfterRead != chunkSize {
		t.Fatalf("expected 1 chunk read (%d B), got calls=%d bytes=%d", chunkSize, callsAfterRead, bytesAfterRead)
	}
	if eno := h.Release(context.Background()); eno != 0 {
		t.Fatal(eno)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen cache: chunk 3 must remain persisted on disk without re-downloading.
	cache2, err := OpenCache(rootDir, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer cache2.Close()
	fsys2 := NewFileSystem("/", backend, cache2)
	h2, eno := fsys2.open(context.Background(), "/big.bin", syscall.O_RDONLY)
	if eno != 0 {
		t.Fatal(eno)
	}
	res2, eno := h2.Read(context.Background(), make([]byte, 4096), off)
	if eno != 0 {
		t.Fatal(eno)
	}
	buf2, status := res2.Bytes(make([]byte, 4096))
	res2.Done()
	_ = h2.Release(context.Background())
	if status != fuse.OK || string(buf2) != string(body[off:off+4096]) {
		t.Fatalf("reopened read mismatch: status=%v", status)
	}
	backend.mu.Lock()
	finalCalls := backend.rangeCalls
	backend.mu.Unlock()
	if finalCalls != 1 {
		t.Fatalf("expected persisted chunk hit (1 total call), got %d", finalCalls)
	}
}

func TestSequentialReadTriggersPrefetchAndCleanRelease(t *testing.T) {
	cache, backend := testCacheAndBackend(t)
	body := make([]byte, 4*chunkSize)
	for i := range body {
		body[i] = byte(i % 253)
	}
	backend.files["/stream.bin"] = fakeFile{
		data:    body,
		version: remote.Version{FileID: "stream-1", ParentKey: "root", Name: "stream.bin", Size: int64(len(body)), MTime: 1},
	}
	fsys := NewFileSystem("/", backend, cache)
	h, eno := fsys.open(context.Background(), "/stream.bin", syscall.O_RDONLY)
	if eno != 0 {
		t.Fatal(eno)
	}
	r1, eno := h.Read(context.Background(), make([]byte, 4096), 0)
	if eno != 0 {
		t.Fatal(eno)
	}
	r1.Done()
	// Consecutive read triggers background sequential prefetch of chunks 1..2.
	r2, eno := h.Read(context.Background(), make([]byte, 4096), 4096)
	if eno != 0 {
		t.Fatal(eno)
	}
	r2.Done()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		backend.mu.Lock()
		gotBytes := backend.rangeBytes
		backend.mu.Unlock()
		if gotBytes >= 2*chunkSize {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	backend.mu.Lock()
	gotBytes := backend.rangeBytes
	backend.mu.Unlock()
	if gotBytes < 2*chunkSize {
		t.Fatalf("expected sequential prefetch >= %d bytes, got %d", 2*chunkSize, gotBytes)
	}
	if eno := h.Release(context.Background()); eno != 0 {
		t.Fatal(eno)
	}
}

func TestSparsePartialWriteHydratesWhileTruncateZeroSkipsDownload(t *testing.T) {
	t.Run("partial write hydrates untouched chunks", func(t *testing.T) {
		cache, backend := testCacheAndBackend(t)
		body := make([]byte, 3*chunkSize)
		for i := range body {
			body[i] = byte((i + 3) % 251)
		}
		backend.files["/edit.bin"] = fakeFile{
			data:    append([]byte(nil), body...),
			version: remote.Version{FileID: "edit-1", ParentKey: "root", Name: "edit.bin", Size: int64(len(body)), MTime: 1},
		}
		fsys := NewFileSystem("/", backend, cache)
		h, eno := fsys.open(context.Background(), "/edit.bin", syscall.O_RDWR)
		if eno != 0 {
			t.Fatal(eno)
		}
		if _, eno := h.Write(context.Background(), []byte("PATCH"), 10); eno != 0 {
			t.Fatal(eno)
		}
		if eno := h.Fsync(context.Background(), 0); eno != 0 {
			t.Fatal(eno)
		}
		_ = h.Release(context.Background())

		want := append([]byte(nil), body...)
		copy(want[10:15], "PATCH")
		backend.mu.Lock()
		stored := backend.files["/edit.bin"].data
		backend.mu.Unlock()
		if string(stored) != string(want) {
			t.Fatal("uploaded data corrupted untouched sparse chunks")
		}
	})

	t.Run("O_TRUNC skips downloading old chunks", func(t *testing.T) {
		cache, backend := testCacheAndBackend(t)
		body := make([]byte, 3*chunkSize)
		backend.files["/trunc.bin"] = fakeFile{
			data:    body,
			version: remote.Version{FileID: "trunc-1", ParentKey: "root", Name: "trunc.bin", Size: int64(len(body)), MTime: 1},
		}
		fsys := NewFileSystem("/", backend, cache)
		h, eno := fsys.open(context.Background(), "/trunc.bin", syscall.O_RDWR|syscall.O_TRUNC)
		if eno != 0 {
			t.Fatal(eno)
		}
		if _, eno := h.Write(context.Background(), []byte("fresh"), 0); eno != 0 {
			t.Fatal(eno)
		}
		if eno := h.Fsync(context.Background(), 0); eno != 0 {
			t.Fatal(eno)
		}
		_ = h.Release(context.Background())

		backend.mu.Lock()
		calls := backend.rangeCalls
		stored := string(backend.files["/trunc.bin"].data)
		backend.mu.Unlock()
		if calls != 0 || stored != "fresh" {
			t.Fatalf("expected 0 range calls and fresh data, got calls=%d data=%q", calls, stored)
		}
	})
}

type badListBackend struct {
	Backend
	listing remote.Listing
}

func (b badListBackend) List(context.Context, string) (remote.Listing, error) {
	return b.listing, nil
}

type blockingTrashBackend struct {
	*fakeBackend
	entered chan struct{}
	release chan struct{}
}

func (b *blockingTrashBackend) Trash(ctx context.Context, name string) error {
	close(b.entered)
	<-b.release
	return b.fakeBackend.Trash(ctx, name)
}

type blockingMoveBackend struct {
	*fakeBackend
	entered chan struct{}
	release chan struct{}
}

func (b *blockingMoveBackend) Move(ctx context.Context, from, to string) error {
	close(b.entered)
	<-b.release
	return b.fakeBackend.Move(ctx, from, to)
}

func testCacheAndBackend(t *testing.T) (*Cache, *fakeBackend) {
	t.Helper()
	cache, err := OpenCache(filepath.Join(t.TempDir(), "cache"), Identity{RemoteRoot: "/", TokenFingerprint: Fingerprint(t.Name())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	return cache, &fakeBackend{files: map[string]fakeFile{}, dirs: map[string]bool{"/": true}}
}

type fakeFile struct {
	data    []byte
	version remote.Version
}
type fakeBackend struct {
	mu            sync.Mutex
	files         map[string]fakeFile
	dirs          map[string]bool
	rangeCalls    int
	rangeBytes    int64
	putCalls      int
	putErr        error
	putContextErr error
}

func (b *fakeBackend) setPutErr(err error) {
	b.mu.Lock()
	b.putErr = err
	b.mu.Unlock()
}

func (b *fakeBackend) List(_ context.Context, raw string) (remote.Listing, error) {
	p, _ := remote.Normalize(raw)
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.dirs[p] {
		return remote.Listing{}, remote.ErrNotFound
	}
	out := remote.Listing{Path: p, ParentKey: "root"}
	for name := range b.dirs {
		if name != "/" && filepath.ToSlash(filepath.Dir(name)) == p {
			out.Dirs = append(out.Dirs, weiyun.DirItem{DirKey: name, DirName: filepath.Base(name)})
		}
	}
	for name, f := range b.files {
		if filepath.ToSlash(filepath.Dir(name)) == p {
			out.Files = append(out.Files, weiyun.FileItem{FileID: f.version.FileID, Filename: filepath.Base(name), FileSize: int64(len(f.data)), FileMTime: f.version.MTime})
		}
	}
	return out, nil
}
func (b *fakeBackend) Resolve(_ context.Context, raw string) (remote.Entry, error) {
	p, _ := remote.Normalize(raw)
	b.mu.Lock()
	defer b.mu.Unlock()
	if f, ok := b.files[p]; ok {
		item := weiyun.FileItem{FileID: f.version.FileID, Filename: filepath.Base(p), FileSize: int64(len(f.data)), FileMTime: f.version.MTime}
		return remote.Entry{Path: p, Name: item.Filename, ParentKey: f.version.ParentKey, File: &item}, nil
	}
	if b.dirs[p] {
		item := weiyun.DirItem{DirKey: p, DirName: filepath.Base(p)}
		return remote.Entry{Path: p, Name: item.DirName, ParentKey: "root", Dir: &item}, nil
	}
	return remote.Entry{}, remote.ErrNotFound
}
func (b *fakeBackend) ReadRange(_ context.Context, version remote.Version, offset int64, dest []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rangeCalls++
	b.rangeBytes += int64(len(dest))
	for _, f := range b.files {
		if f.version.FileID == version.FileID {
			if offset < 0 || offset+int64(len(dest)) > int64(len(f.data)) {
				return io.ErrUnexpectedEOF
			}
			copy(dest, f.data[offset:offset+int64(len(dest))])
			return nil
		}
	}
	return remote.ErrNotFound
}
func (b *fakeBackend) PutFile(ctx context.Context, raw string, expected *remote.Version, r io.ReaderAt, size int64) (remote.Version, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.putCalls++
	b.putContextErr = ctx.Err()
	if b.putErr != nil {
		return remote.Version{}, b.putErr
	}
	p, _ := remote.Normalize(raw)
	data := make([]byte, size)
	if size > 0 {
		n, err := r.ReadAt(data, 0)
		if err != nil && !errors.Is(err, io.EOF) {
			return remote.Version{}, err
		}
		if int64(n) != size {
			return remote.Version{}, io.ErrUnexpectedEOF
		}
	}
	v := remote.Version{FileID: "stored", ParentKey: "root", Name: filepath.Base(p), Size: size, MTime: int64(b.putCalls + 1)}
	b.files[p] = fakeFile{data: data, version: v}
	return v, nil
}
func (b *fakeBackend) Mkdir(_ context.Context, raw string) (string, error) {
	p, _ := remote.Normalize(raw)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dirs[p] {
		return "", remote.ErrConflict
	}
	b.dirs[p] = true
	return p, nil
}
func (b *fakeBackend) Move(_ context.Context, from, to string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	a, _ := remote.Normalize(from)
	z, _ := remote.Normalize(to)
	if _, ok := b.files[z]; ok || b.dirs[z] {
		return remote.ErrConflict
	}
	if f, ok := b.files[a]; ok {
		delete(b.files, a)
		f.version.Name = filepath.Base(z)
		b.files[z] = f
		return nil
	}
	if !b.dirs[a] {
		return remote.ErrNotFound
	}
	movedDirs := make(map[string]bool)
	movedFiles := make(map[string]fakeFile)
	for name := range b.dirs {
		if name == a || strings.HasPrefix(name, a+"/") {
			movedDirs[z+strings.TrimPrefix(name, a)] = true
			delete(b.dirs, name)
		}
	}
	for name, file := range b.files {
		if strings.HasPrefix(name, a+"/") {
			movedFiles[z+strings.TrimPrefix(name, a)] = file
			delete(b.files, name)
		}
	}
	for name := range movedDirs {
		b.dirs[name] = true
	}
	for name, file := range movedFiles {
		b.files[name] = file
	}
	return nil
}
func (b *fakeBackend) Trash(_ context.Context, raw string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, _ := remote.Normalize(raw)
	delete(b.files, p)
	delete(b.dirs, p)
	return nil
}

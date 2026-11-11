package mount

import (
	"context"
	"errors"
	"hash/fnv"
	"path"
	"sort"
	"strings"
	"syscall"

	"github.com/HeLingQi/tencent-weiyun-go/internal/remote"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"
)

type Node struct {
	fs.Inode
	fsys *FileSystem
	path string
	dir  bool
}

func NewRoot(fsys *FileSystem) *Node {
	return &Node{fsys: fsys, path: "/", dir: true}
}

func (n *Node) stable(dir bool) fs.StableAttr {
	mode := uint32(syscall.S_IFREG)
	if dir {
		mode = syscall.S_IFDIR
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(n.path))
	return fs.StableAttr{Mode: mode, Ino: h.Sum64()}
}

func validRemoteName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00")
}

func (n *Node) currentPath() string {
	if name := n.Path(nil); name != "" {
		return "/" + name
	}
	if n.path != "" {
		return n.path
	}
	return "/"
}

func (n *Node) childPath(name string) string {
	return path.Join(n.currentPath(), name)
}

func (n *Node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if !validRemoteName(name) {
		return nil, syscall.EIO
	}
	childPath := n.childPath(name)
	if state := n.fsys.cache.entry(childPath); state != nil {
		state.mu.Lock()
		local := state.Dirty || state.LocalOnly
		if local {
			fillFileAttr(&out.Attr, state.Size, state.MTime)
		}
		state.mu.Unlock()
		if local {
			child := &Node{fsys: n.fsys, path: childPath}
			return n.NewInode(ctx, child, child.stable(false)), 0
		}
	}
	entry, err := n.fsys.backend.Resolve(ctx, n.fsys.remotePath(childPath))
	if err != nil {
		return nil, errno(err)
	}
	if entry.Name != name || !validRemoteName(entry.Name) {
		return nil, syscall.EIO
	}
	isDir := entry.Dir != nil
	if isDir {
		fillDirAttr(&out.Attr, entry.Dir.DirMTime)
	} else if entry.File != nil {
		fillFileAttr(&out.Attr, entry.File.FileSize, entry.File.FileMTime)
	} else {
		return nil, syscall.EIO
	}
	child := &Node{fsys: n.fsys, path: childPath, dir: isDir}
	return n.NewInode(ctx, child, child.stable(isDir)), 0
}

func (n *Node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	listing, err := n.fsys.backend.List(ctx, n.fsys.remotePath(n.currentPath()))
	if err != nil {
		return nil, errno(err)
	}
	entries := make(map[string]fuse.DirEntry)
	for _, dir := range listing.Dirs {
		if !validRemoteName(dir.DirName) {
			return nil, syscall.EIO
		}
		if _, exists := entries[dir.DirName]; exists {
			return nil, syscall.EIO
		}
		entries[dir.DirName] = fuse.DirEntry{Name: dir.DirName, Mode: syscall.S_IFDIR}
	}
	for _, file := range listing.Files {
		if !validRemoteName(file.Filename) {
			return nil, syscall.EIO
		}
		if _, exists := entries[file.Filename]; exists {
			return nil, syscall.EIO
		}
		entries[file.Filename] = fuse.DirEntry{Name: file.Filename, Mode: syscall.S_IFREG}
	}
	for _, state := range n.fsys.cache.children(n.currentPath()) {
		state.mu.Lock()
		local := state.Dirty || state.LocalOnly
		name := path.Base(state.Path)
		state.mu.Unlock()
		if local {
			entries[name] = fuse.DirEntry{Name: name, Mode: syscall.S_IFREG}
		}
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]fuse.DirEntry, 0, len(names))
	for _, name := range names {
		result = append(result, entries[name])
	}
	return fs.NewListDirStream(result), 0
}

func (n *Node) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	currentPath := n.currentPath()
	if n.dir {
		if currentPath == "/" {
			fillDirAttr(&out.Attr, 0)
			return 0
		}
		entry, err := n.fsys.backend.Resolve(ctx, n.fsys.remotePath(currentPath))
		if err != nil {
			return errno(err)
		}
		if entry.Dir == nil {
			return syscall.ENOTDIR
		}
		fillDirAttr(&out.Attr, entry.Dir.DirMTime)
		return 0
	}
	if handle, ok := fh.(*FileHandle); ok {
		return handle.Getattr(ctx, out)
	}
	if state := n.fsys.cache.entry(currentPath); state != nil {
		state.mu.Lock()
		local := state.Dirty || state.LocalOnly
		if local {
			fillFileAttr(&out.Attr, state.Size, state.MTime)
		}
		state.mu.Unlock()
		if local {
			return 0
		}
	}
	entry, err := n.fsys.backend.Resolve(ctx, n.fsys.remotePath(currentPath))
	if err != nil {
		return errno(err)
	}
	if entry.File == nil {
		return syscall.EISDIR
	}
	fillFileAttr(&out.Attr, entry.File.FileSize, entry.File.FileMTime)
	return 0
}

func fillFileAttr(out *fuse.Attr, size, mtime int64) {
	out.Mode = syscall.S_IFREG | 0o600
	out.Size = uint64(size)
	out.Mtime = uint64(mtime / 1000)
	out.Mtimensec = uint32(mtime%1000) * 1000000
}

func fillDirAttr(out *fuse.Attr, mtime int64) {
	out.Mode = syscall.S_IFDIR | 0o700
	out.Mtime = uint64(mtime / 1000)
	out.Mtimensec = uint32(mtime%1000) * 1000000
}

func (n *Node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	handle, eno := n.fsys.open(ctx, n.currentPath(), flags)
	return handle, 0, eno
}

func (n *Node) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if in.Valid & ^uint32(fuse.FATTR_SIZE|fuse.FATTR_FH|fuse.FATTR_LOCKOWNER) != 0 {
		return syscall.EOPNOTSUPP
	}
	if handle, ok := fh.(*FileHandle); ok {
		return handle.Setattr(ctx, in, out)
	}
	size, ok := in.GetSize()
	if !ok {
		return syscall.EOPNOTSUPP
	}
	handle, eno := n.fsys.open(ctx, n.currentPath(), syscall.O_RDWR)
	if eno != 0 {
		return eno
	}
	defer handle.Release(ctx)
	if eno := handle.truncateWithContext(ctx, int64(size)); eno != 0 {
		return eno
	}
	if eno := handle.Fsync(ctx, 0); eno != 0 {
		return eno
	}
	return handle.Getattr(ctx, out)
}

func (n *Node) Create(ctx context.Context, name string, flags, _ uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	if !validRemoteName(name) || n.fsys.stopping.Load() {
		return nil, nil, 0, syscall.EBUSY
	}
	n.fsys.cacheMu.Lock()
	defer n.fsys.cacheMu.Unlock()
	childPath := n.childPath(name)
	if n.fsys.cache.entry(childPath) != nil {
		return nil, nil, 0, syscall.EEXIST
	}
	if _, err := n.fsys.backend.Resolve(ctx, n.fsys.remotePath(childPath)); err == nil {
		return nil, nil, 0, syscall.EEXIST
	} else if !errors.Is(err, remote.ErrNotFound) {
		return nil, nil, 0, errno(err)
	}
	state, err := n.fsys.cache.newLocal(childPath)
	if err != nil {
		return nil, nil, 0, errno(err)
	}
	fillFileAttr(&out.Attr, 0, 0)
	child := &Node{fsys: n.fsys, path: childPath}
	inode := n.NewInode(ctx, child, child.stable(false))
	handle, eno := n.fsys.openLocked(ctx, state.Path, flags|syscall.O_RDWR)
	if eno != 0 {
		return nil, nil, 0, eno
	}
	state.mu.Lock()
	state.sessionModified = true
	state.mu.Unlock()
	handle.modified = true
	return inode, handle, 0, 0
}

func (n *Node) Mkdir(ctx context.Context, name string, _ uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if !validRemoteName(name) || n.fsys.stopping.Load() {
		return nil, syscall.EBUSY
	}
	n.fsys.cacheMu.Lock()
	defer n.fsys.cacheMu.Unlock()
	childPath := n.childPath(name)
	if n.fsys.cache.entry(childPath) != nil {
		return nil, syscall.EEXIST
	}
	if _, err := n.fsys.backend.Mkdir(ctx, n.fsys.remotePath(childPath)); err != nil {
		return nil, errno(err)
	}
	fillDirAttr(&out.Attr, 0)
	child := &Node{fsys: n.fsys, path: childPath, dir: true}
	return n.NewInode(ctx, child, child.stable(true)), 0
}

func (n *Node) Unlink(ctx context.Context, name string) syscall.Errno {
	if !validRemoteName(name) || n.fsys.stopping.Load() {
		return syscall.EBUSY
	}
	n.fsys.cacheMu.Lock()
	defer n.fsys.cacheMu.Unlock()
	childPath := n.childPath(name)
	state := n.fsys.cache.entry(childPath)
	if state != nil {
		state.mu.Lock()
		busy := state.readers != 0 || state.pendingReads != 0 || state.writer
		dirty := state.Dirty || state.LocalOnly
		state.mu.Unlock()
		if busy {
			return syscall.EBUSY
		}
		if dirty {
			if err := n.fsys.cache.recover(state); err != nil {
				return errno(err)
			}
			if state.Version == nil {
				return 0
			}
			return errno(n.fsys.backend.Trash(ctx, n.fsys.remotePath(childPath)))
		}
		if err := n.fsys.backend.Trash(ctx, n.fsys.remotePath(childPath)); err != nil {
			return errno(err)
		}
		return errno(n.fsys.cache.removeClean(state))
	}
	return errno(n.fsys.backend.Trash(ctx, n.fsys.remotePath(childPath)))
}

func (n *Node) Rmdir(ctx context.Context, name string) syscall.Errno {
	if !validRemoteName(name) || n.fsys.stopping.Load() {
		return syscall.EBUSY
	}
	n.fsys.cacheMu.Lock()
	defer n.fsys.cacheMu.Unlock()
	childPath := n.childPath(name)
	listing, err := n.fsys.backend.List(ctx, n.fsys.remotePath(childPath))
	if err != nil {
		return errno(err)
	}
	if len(listing.Dirs) != 0 || len(listing.Files) != 0 || len(n.fsys.cache.children(childPath)) != 0 {
		return syscall.ENOTEMPTY
	}
	return errno(n.fsys.backend.Trash(ctx, n.fsys.remotePath(childPath)))
}

func (n *Node) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	if flags != 0 && flags != unix.RENAME_NOREPLACE {
		return syscall.EOPNOTSUPP
	}
	other, ok := newParent.(*Node)
	if !ok || other.fsys != n.fsys {
		return syscall.EXDEV
	}
	if !validRemoteName(name) || !validRemoteName(newName) || n.fsys.stopping.Load() {
		return syscall.EBUSY
	}
	n.fsys.cacheMu.Lock()
	defer n.fsys.cacheMu.Unlock()
	source := n.childPath(name)
	target := other.childPath(newName)
	if n.fsys.cache.entry(target) != nil {
		return syscall.EEXIST
	}
	if _, err := n.fsys.backend.Resolve(ctx, n.fsys.remotePath(target)); err == nil {
		return syscall.EEXIST
	} else if !errors.Is(err, remote.ErrNotFound) {
		return errno(err)
	}
	state := n.fsys.cache.entry(source)
	if state != nil {
		state.mu.Lock()
		busy := state.Dirty || state.readers != 0 || state.pendingReads != 0 || state.writer
		state.mu.Unlock()
		if busy {
			return syscall.EBUSY
		}
	}
	entry, err := n.fsys.backend.Resolve(ctx, n.fsys.remotePath(source))
	if err != nil {
		return errno(err)
	}
	if n.currentPath() != other.currentPath() && name != newName {
		return syscall.EXDEV
	}
	if entry.Dir != nil && n.fsys.cache.hasDescendant(source) {
		return syscall.EBUSY
	}
	if err := n.fsys.backend.Move(ctx, n.fsys.remotePath(source), n.fsys.remotePath(target)); err != nil {
		return errno(err)
	}
	if state != nil {
		return errno(n.fsys.cache.renameClean(state, target))
	}
	return 0
}

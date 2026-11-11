package mount

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/HeLingQi/tencent-weiyun-go/internal/remote"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

const shutdownSyncTimeout = 2 * time.Minute

func Run(ctx context.Context, rawRoot, mountpoint, cacheDir, token string, backend Backend) error {
	root, err := remote.Normalize(rawRoot)
	if err != nil {
		return err
	}
	mountpoint, err = validateMountpoint(mountpoint)
	if err != nil {
		return err
	}
	if root != "/" {
		entry, resolveErr := backend.Resolve(ctx, "wy:"+root)
		if resolveErr != nil {
			return resolveErr
		}
		if entry.Dir == nil {
			return errors.New("mount source must be a remote directory")
		}
	}
	if cacheDir == "" {
		cacheDir, err = DefaultCacheDir(token, root)
		if err != nil {
			return err
		}
	}
	if err := ValidatePaths(cacheDir, mountpoint); err != nil {
		return err
	}
	cache, err := OpenCache(cacheDir, Identity{RemoteRoot: root, TokenFingerprint: Fingerprint(token)})
	if err != nil {
		return err
	}
	fsys := NewFileSystem(root, backend, cache)
	attrTTL := 5 * time.Second
	entryTTL := 5 * time.Second
	negTTL := 2 * time.Second
	server, err := fs.Mount(mountpoint, NewRoot(fsys), &fs.Options{
		AttrTimeout:     &attrTTL,
		EntryTimeout:    &entryTTL,
		NegativeTimeout: &negTTL,
		MountOptions: fuse.MountOptions{
			FsName: "weiyun", Name: "weiyun", DisableXAttrs: true,
		},
	})
	if err != nil {
		_ = cache.Close()
		return err
	}
	done := make(chan struct{})
	go func() {
		server.Wait()
		close(done)
	}()

	select {
	case <-done:
		return cache.Close()
	case <-ctx.Done():
	}

	syncCtx, cancel := context.WithTimeout(context.Background(), shutdownSyncTimeout)
	syncErr := fsys.SyncDirty(syncCtx)
	cancel()
	for {
		if err := server.Unmount(); err == nil {
			<-done
			break
		}
		select {
		case <-done:
			break
		case <-time.After(250 * time.Millisecond):
			continue
		}
		break
	}
	closeErr := cache.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func validateMountpoint(name string) (string, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("mountpoint must be an existing directory, not a symlink")
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return "", err
	}
	if len(entries) != 0 {
		return "", errors.New("mountpoint must be empty")
	}
	return filepath.EvalSymlinks(abs)
}

package mount

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/HeLingQi/tencent-weiyun-go/internal/remote"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestRunCancellationSyncsWithFreshContextAndReleasesLock(t *testing.T) {
	if os.Getenv("WEIYUN_FUSE_TEST") != "1" {
		t.Skip("set WEIYUN_FUSE_TEST=1 to run a real local FUSE mount")
	}
	base := t.TempDir()
	mountpoint := filepath.Join(base, "mnt")
	cacheDir := filepath.Join(base, "cache")
	if err := os.Mkdir(mountpoint, 0o700); err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{files: map[string]fakeFile{}, dirs: map[string]bool{"/": true}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, "wy:/", mountpoint, cacheDir, "integration-token", backend) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var stat syscall.Statfs_t
		if syscall.Statfs(mountpoint, &stat) == nil && uint64(stat.Type) == uint64(0x65735546) {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("mount exited early: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("mount did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	file, err := os.OpenFile(filepath.Join(mountpoint, "pending.txt"), os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("pending")); err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline = time.Now().Add(5 * time.Second)
	for {
		backend.mu.Lock()
		calls, contextErr := backend.putCalls, backend.putContextErr
		backend.mu.Unlock()
		if calls != 0 {
			if contextErr != nil {
				t.Fatalf("shutdown upload used cancelled context: %v", contextErr)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shutdown did not upload dirty file")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mount did not unmount after handle close")
	}
	cache, err := OpenCache(cacheDir, Identity{RemoteRoot: "/", TokenFingerprint: Fingerprint("integration-token")})
	if err != nil {
		t.Fatalf("cache lock was not released: %v", err)
	}
	_ = cache.Close()
}

func TestFUSEIntegration(t *testing.T) {
	if os.Getenv("WEIYUN_FUSE_TEST") != "1" {
		t.Skip("set WEIYUN_FUSE_TEST=1 to run a real local FUSE mount")
	}
	base := t.TempDir()
	mountpoint := filepath.Join(base, "mnt")
	if err := os.Mkdir(mountpoint, 0o700); err != nil {
		t.Fatal(err)
	}
	cache, err := OpenCache(filepath.Join(base, "cache"), Identity{RemoteRoot: "/", TokenFingerprint: Fingerprint("integration-token")})
	if err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{files: map[string]fakeFile{}, dirs: map[string]bool{"/": true, "/folder": true}}
	body := make([]byte, 1024*1024+123)
	for i := range body {
		body[i] = byte(i % 251)
	}
	backend.files["/hello.bin"] = fakeFile{data: body, version: remote.Version{FileID: "hello", ParentKey: "root", Name: "hello.bin", Size: int64(len(body)), MTime: 1700000000123}}
	backend.files["/folder/nested.txt"] = fakeFile{data: []byte("nested-data"), version: remote.Version{FileID: "nested", ParentKey: "/folder", Name: "nested.txt", Size: 11, MTime: 1700000000456}}
	fsys := NewFileSystem("/", backend, cache)
	server, err := fs.Mount(mountpoint, NewRoot(fsys), &fs.Options{MountOptions: fuse.MountOptions{DisableXAttrs: true, FsName: "weiyun-test", Name: "weiyun-test"}})
	if err != nil {
		_ = cache.Close()
		t.Skipf("local FUSE unavailable: %v", err)
	}
	mounted := true
	defer func() {
		if mounted {
			_ = server.Unmount()
			server.Wait()
		}
		_ = cache.Close()
	}()

	entries, err := os.ReadDir(mountpoint)
	if err != nil || len(entries) != 2 || entries[0].Name() != "folder" || entries[1].Name() != "hello.bin" {
		t.Fatalf("readdir=%v error=%v", entries, err)
	}
	helloPath := filepath.Join(mountpoint, "hello.bin")
	info, err := os.Stat(helloPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.ModTime(); got.Unix() != 1700000000 || got.Nanosecond() != 123000000 {
		t.Fatalf("mtime=%s", got)
	}
	if _, err := os.Stat(filepath.Join(mountpoint, "folder", "nested.txt")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(helloPath)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("read size=%d error=%v", len(got), err)
	}
	file, err := os.Open(filepath.Join(mountpoint, "hello.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	random := make([]byte, 4096)
	if _, err := file.ReadAt(random, 333333); err != nil || !bytes.Equal(random, body[333333:333333+4096]) {
		t.Fatalf("random read error=%v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	createdPath := filepath.Join(mountpoint, "created.txt")
	created, err := os.OpenFile(createdPath, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := created.WriteAt([]byte("created-data"), 0); err != nil {
		t.Fatal(err)
	}
	if err := created.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(createdPath); err != nil || string(got) != "created-data" {
		t.Fatalf("created read=%q error=%v", got, err)
	}

	zeroPath := filepath.Join(mountpoint, "zero.txt")
	zero, err := os.OpenFile(zeroPath, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := zero.Sync(); err == nil {
		t.Fatal("zero-byte fsync unexpectedly succeeded")
	}
	if err := zero.Close(); err == nil {
		t.Fatal("zero-byte close unexpectedly succeeded")
	}
	if state := cache.entry("/zero.txt"); state == nil || !state.Dirty {
		t.Fatalf("zero-byte state=%+v", state)
	}
	if err := os.Remove(zeroPath); err != nil {
		t.Fatal(err)
	}

	hello, err := os.OpenFile(helloPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer hello.Close()
	if err := hello.Truncate(17); err != nil {
		t.Fatal(err)
	}
	if _, err := hello.WriteAt([]byte("xyz"), 4); err != nil {
		t.Fatal(err)
	}
	if err := hello.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := hello.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(helloPath); err != nil || len(got) != 17 || string(got[4:7]) != "xyz" {
		t.Fatalf("truncate read=%q error=%v", got, err)
	}

	backend.setPutErr(errors.New("injected upload failure"))
	failed, err := os.OpenFile(filepath.Join(mountpoint, "failed.txt"), os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failed.Write([]byte("keep-local")); err != nil {
		t.Fatal(err)
	}
	if err := failed.Sync(); err == nil {
		t.Fatal("fsync unexpectedly succeeded")
	}
	_ = failed.Close()
	state := cache.entry("/failed.txt")
	if state == nil || !state.Dirty {
		t.Fatalf("failed state=%+v", state)
	}
	backend.setPutErr(nil)

	renamedPath := filepath.Join(mountpoint, "renamed.bin")
	if err := os.Rename(helloPath, renamedPath); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(renamedPath); err != nil || len(got) != 17 || string(got[4:7]) != "xyz" {
		t.Fatalf("renamed read=%q error=%v", got, err)
	}
	movedDir := filepath.Join(mountpoint, "moved-folder")
	if err := os.Rename(filepath.Join(mountpoint, "folder"), movedDir); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(movedDir, "nested.txt")); err != nil || string(got) != "nested-data" {
		t.Fatalf("moved nested read=%q error=%v", got, err)
	}
	if err := os.Remove(createdPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(createdPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed stat error=%v", err)
	}
	if err := os.Mkdir(filepath.Join(mountpoint, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(mountpoint, "empty")); err != nil {
		t.Fatal(err)
	}

	if err := server.Unmount(); err != nil {
		t.Fatal(err)
	}
	server.Wait()
	mounted = false
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
}

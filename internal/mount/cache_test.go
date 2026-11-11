package mount

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCacheLocksAndUsesPrivatePermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	identity := Identity{RemoteRoot: "/docs", TokenFingerprint: Fingerprint("token-a")}
	cache, err := OpenCache(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()

	if info, err := os.Stat(root); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("cache mode=%v error=%v", info.Mode().Perm(), err)
	}
	if _, err := OpenCache(root, identity); !errors.Is(err, ErrCacheLocked) {
		t.Fatalf("second open error = %v", err)
	}
	entry, err := cache.newLocal("/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{cache.dataPath(entry.ID), cache.manifestPath(entry.ID), filepath.Join(root, identityFile), filepath.Join(root, lockFile)} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode=%v error=%v", path, info.Mode().Perm(), err)
		}
	}
}

func TestCacheRejectsSymlinksUnsafePermissionsIdentityReuseAndNestedMountpoint(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	identity := Identity{RemoteRoot: "/", TokenFingerprint: Fingerprint("token-a")}
	if _, err := OpenCache(link, identity); !errors.Is(err, ErrUnsafeCache) {
		t.Fatalf("symlink error = %v", err)
	}

	unsafe := filepath.Join(parent, "unsafe")
	if err := os.Mkdir(unsafe, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenCache(unsafe, identity); !errors.Is(err, ErrUnsafeCache) {
		t.Fatalf("permission error = %v", err)
	}

	root := filepath.Join(parent, "bound")
	cache, err := OpenCache(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	other := Identity{RemoteRoot: "/", TokenFingerprint: Fingerprint("token-b")}
	if _, err := OpenCache(root, other); !errors.Is(err, ErrCacheIdentity) {
		t.Fatalf("identity error = %v", err)
	}

	mountpoint := filepath.Join(parent, "mount")
	if err := os.Mkdir(mountpoint, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePaths(filepath.Join(mountpoint, "cache"), mountpoint); !errors.Is(err, ErrPathOverlap) {
		t.Fatalf("nested cache error = %v", err)
	}
	if err := ValidatePaths(root, filepath.Join(root, "mount")); !errors.Is(err, ErrPathOverlap) {
		t.Fatalf("nested mount error = %v", err)
	}
}

func TestCacheRejectsMaliciousManifestPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	identity := Identity{RemoteRoot: "/", TokenFingerprint: Fingerprint("token")}
	cache, err := OpenCache(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}

	id := "0123456789abcdef0123456789abcdef"
	bad := manifest{ID: id, Path: "/../escape", Dirty: true, LocalOnly: true}
	data, err := json.Marshal(bad)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, manifestsDir, id+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, dataDir, id), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenCache(root, identity); !errors.Is(err, ErrUnsafeManifest) {
		t.Fatalf("manifest error = %v", err)
	}
}

func TestCacheRejectsSymlinkIdentityBeforeReadingIt(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "cache")
	identity := Identity{RemoteRoot: "/", TokenFingerprint: Fingerprint("token")}
	cache, err := OpenCache(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside.json")
	data, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, identityFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, identityFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenCache(root, identity); !errors.Is(err, ErrUnsafeCache) {
		t.Fatalf("identity symlink error=%v", err)
	}
}

func TestDirtyEntryReopensWithoutSessionModification(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	identity := Identity{RemoteRoot: "/", TokenFingerprint: Fingerprint("token")}
	cache, err := OpenCache(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := cache.newLocal("/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache.dataPath(entry.ID), []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry.mu.Lock()
	entry.Size = 3
	entry.Dirty = true
	entry.sessionModified = true
	entry.generation++
	err = cache.persistLocked(entry)
	entry.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenCache(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got := reopened.entry("/a.txt")
	if got == nil || !got.Dirty || !got.LocalOnly || got.Size != 3 || got.sessionModified {
		t.Fatalf("recovered = %+v", got)
	}
}

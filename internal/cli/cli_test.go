package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/HeLingQi/tencent-weiyun-go/internal/config"
	"github.com/HeLingQi/tencent-weiyun-go/internal/remote"
	"github.com/HeLingQi/tencent-weiyun-go/internal/weiyun"
)

func TestHelpDoesNotLoadCredentials(t *testing.T) {
	loaded := false
	stdout := new(bytes.Buffer)
	code := Run(context.Background(), []string{"--help"}, Dependencies{
		Stdout: stdout, Stderr: new(bytes.Buffer), Stdin: strings.NewReader(""),
		LoadConfig: func(string) (config.Config, error) { loaded = true; return config.Config{}, nil },
	})
	if code != 0 || loaded {
		t.Fatalf("code=%d loaded=%v", code, loaded)
	}
	for _, text := range []string{"~/.config/weiyun/config.toml", "TOML", "wy:/", "不可恢复", "rm -f"} {
		if !strings.Contains(stdout.String(), text) {
			t.Fatalf("help missing %q: %s", text, stdout.String())
		}
	}
}

func TestGlobalConfigAndSeparatorAreParsedWithoutTreatingLocalPathAsFlag(t *testing.T) {
	ops := &fakeOperations{}
	var loadedPath string
	code := Run(context.Background(), []string{"cp", "--config", "/tmp/custom.toml", "--", "-local", "wy:/dest"}, Dependencies{
		Stdin: strings.NewReader(""), Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer),
		LoadConfig:    func(path string) (config.Config, error) { loadedPath = path; return config.Config{Token: "token"}, nil },
		NewOperations: func(config.Config) (Operations, error) { return ops, nil },
	})
	if code != 0 || loadedPath != "/tmp/custom.toml" || ops.uploadLocal != "-local" || ops.uploadRemote != "wy:/dest" {
		t.Fatalf("code=%d config=%q upload=%q -> %q", code, loadedPath, ops.uploadLocal, ops.uploadRemote)
	}
}

func TestMountParsesCacheDirAndUsesDedicatedDependency(t *testing.T) {
	var gotConfig config.Config
	var gotRemote, gotMountpoint, gotCache string
	dependencies := Dependencies{
		Stdin: strings.NewReader(""), Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer),
		LoadConfig: func(string) (config.Config, error) { return config.Config{Token: "mount-token"}, nil },
		NewOperations: func(config.Config) (Operations, error) {
			t.Fatal("mount must not construct legacy operations")
			return nil, nil
		},
		Mount: func(_ context.Context, cfg config.Config, remotePath, mountpoint, cacheDir string) error {
			gotConfig, gotRemote, gotMountpoint, gotCache = cfg, remotePath, mountpoint, cacheDir
			return nil
		},
	}
	code := Run(context.Background(), []string{"mount", "wy:/docs", "/mnt/empty", "--cache-dir", "/tmp/cache"}, dependencies)
	if code != 0 || gotConfig.Token != "mount-token" || gotRemote != "wy:/docs" || gotMountpoint != "/mnt/empty" || gotCache != "/tmp/cache" {
		t.Fatalf("code=%d config=%+v remote=%q mount=%q cache=%q", code, gotConfig, gotRemote, gotMountpoint, gotCache)
	}
}

func TestMountRejectsInvalidOptionsBeforeLoadingConfig(t *testing.T) {
	for _, args := range [][]string{
		{"mount", "wy:/", "/mnt", "--cache-dir"},
		{"mount", "wy:/", "/mnt", "--cache-dir", "a", "--cache-dir", "b"},
		{"mount", "wy:/", "/mnt", "--bad"},
	} {
		loaded := false
		code := Run(context.Background(), args, Dependencies{
			Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer),
			LoadConfig: func(string) (config.Config, error) { loaded = true; return config.Config{}, nil },
		})
		if code == 0 || loaded {
			t.Fatalf("args=%v code=%d loaded=%v", args, code, loaded)
		}
	}
}

func TestCommandsMapToOperations(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		check func(*testing.T, *fakeOperations, string)
	}{
		{"ls", []string{"ls", "wy:/docs"}, func(t *testing.T, ops *fakeOperations, out string) {
			if ops.listPath != "wy:/docs" || !strings.Contains(out, "dir/") || !strings.Contains(out, "a.txt") {
				t.Fatalf("ops=%+v out=%q", ops, out)
			}
		}},
		{"mkdir", []string{"mkdir", "wy:/new"}, func(t *testing.T, ops *fakeOperations, out string) {
			if ops.mkdirPath != "wy:/new" || !strings.Contains(out, "wy:/new") {
				t.Fatalf("ops=%+v out=%q", ops, out)
			}
		}},
		{"download", []string{"cp", "wy:/a.txt", "out.txt"}, func(t *testing.T, ops *fakeOperations, _ string) {
			if ops.downloadRemote != "wy:/a.txt" || ops.downloadLocal != "out.txt" {
				t.Fatalf("ops=%+v", ops)
			}
		}},
		{"move", []string{"mv", "wy:/a", "wy:/b"}, func(t *testing.T, ops *fakeOperations, _ string) {
			if ops.moveSource != "wy:/a" || ops.moveDestination != "wy:/b" {
				t.Fatalf("ops=%+v", ops)
			}
		}},
		{"share", []string{"share", "wy:/a"}, func(t *testing.T, ops *fakeOperations, out string) {
			if ops.sharePath != "wy:/a" || strings.TrimSpace(out) != "https://share.example/x" {
				t.Fatalf("ops=%+v out=%q", ops, out)
			}
		}},
		{"trash", []string{"rm", "wy:/a"}, func(t *testing.T, ops *fakeOperations, _ string) {
			if ops.deletePath != "wy:/a" || ops.deletePermanent {
				t.Fatalf("ops=%+v", ops)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ops := &fakeOperations{}
			stdout := new(bytes.Buffer)
			code := Run(context.Background(), test.args, testDependencies(ops, strings.NewReader(""), stdout, new(bytes.Buffer), false))
			if code != 0 {
				t.Fatalf("code=%d", code)
			}
			test.check(t, ops, stdout.String())
		})
	}
}

func TestPermanentDeleteRequiresTTYAndExactConfirmationWithFlexibleFlagPosition(t *testing.T) {
	for _, args := range [][]string{{"rm", "-f", "wy:/a"}, {"rm", "wy:/a", "-f"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			ops := &fakeOperations{}
			code := Run(context.Background(), args, testDependencies(ops, strings.NewReader("DELETE\n"), new(bytes.Buffer), new(bytes.Buffer), true))
			if code != 0 || ops.deletePath != "wy:/a" || !ops.deletePermanent {
				t.Fatalf("code=%d ops=%+v", code, ops)
			}
		})
	}

	t.Run("confirmation ending at EOF", func(t *testing.T) {
		ops := &fakeOperations{}
		newOperationsCalled := false
		dependencies := testDependencies(ops, strings.NewReader("DELETE"), new(bytes.Buffer), new(bytes.Buffer), true)
		dependencies.NewOperations = func(config.Config) (Operations, error) {
			newOperationsCalled = true
			return ops, nil
		}
		code := Run(context.Background(), []string{"rm", "-f", "wy:/a"}, dependencies)
		if code == 0 || newOperationsCalled || ops.deletePath != "" {
			t.Fatalf("code=%d newOperationsCalled=%v ops=%+v", code, newOperationsCalled, ops)
		}
	})

	for name, input := range map[string]struct {
		terminal bool
		input    string
	}{
		"non tty": {false, "DELETE\n"}, "declined": {true, "yes\n"}, "eof": {true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			ops := &fakeOperations{}
			stderr := new(bytes.Buffer)
			code := Run(context.Background(), []string{"rm", "-f", "wy:/a"}, testDependencies(ops, strings.NewReader(input.input), new(bytes.Buffer), stderr, input.terminal))
			if code == 0 || ops.deletePath != "" || !strings.Contains(stderr.String(), "拒绝") {
				t.Fatalf("code=%d ops=%+v stderr=%q", code, ops, stderr.String())
			}
		})
	}
}

func TestUnknownFlagsAndInvalidCopyDirectionsFailBeforeLoadingConfig(t *testing.T) {
	for _, args := range [][]string{{"ls", "--bad", "wy:/"}, {"cp", "a", "b"}, {"cp", "wy:/a", "wy:/b"}} {
		loaded := false
		stderr := new(bytes.Buffer)
		code := Run(context.Background(), args, Dependencies{
			Stdin: strings.NewReader(""), Stdout: new(bytes.Buffer), Stderr: stderr,
			LoadConfig: func(string) (config.Config, error) {
				loaded = true
				return config.Config{}, errors.New("must not load")
			},
		})
		if code == 0 || loaded {
			t.Fatalf("args=%v code=%d loaded=%v stderr=%q", args, code, loaded, stderr.String())
		}
	}
}

func testDependencies(ops Operations, stdin *strings.Reader, stdout, stderr *bytes.Buffer, terminal bool) Dependencies {
	return Dependencies{
		Stdin: stdin, Stdout: stdout, Stderr: stderr, IsTerminal: func() bool { return terminal },
		DefaultConfigPath: func() (string, error) { return "/default/config.toml", nil },
		LoadConfig:        func(string) (config.Config, error) { return config.Config{Token: "token"}, nil },
		NewOperations:     func(config.Config) (Operations, error) { return ops, nil },
	}
}

type fakeOperations struct {
	listPath                      string
	mkdirPath                     string
	uploadLocal, uploadRemote     string
	downloadRemote, downloadLocal string
	moveSource, moveDestination   string
	sharePath                     string
	deletePath                    string
	deletePermanent               bool
}

func (f *fakeOperations) List(_ context.Context, path string) (remote.Listing, error) {
	f.listPath = path
	return remote.Listing{Dirs: []weiyun.DirItem{{DirName: "dir"}}, Files: []weiyun.FileItem{{Filename: "a.txt", FileSize: 3}}}, nil
}
func (f *fakeOperations) Mkdir(_ context.Context, path string) (string, error) {
	f.mkdirPath = path
	return "/new", nil
}
func (f *fakeOperations) Upload(_ context.Context, local, remotePath string) error {
	f.uploadLocal, f.uploadRemote = local, remotePath
	return nil
}
func (f *fakeOperations) Download(_ context.Context, remotePath, local string) error {
	f.downloadRemote, f.downloadLocal = remotePath, local
	return nil
}
func (f *fakeOperations) Move(_ context.Context, source, destination string) error {
	f.moveSource, f.moveDestination = source, destination
	return nil
}
func (f *fakeOperations) Share(_ context.Context, path string) (string, error) {
	f.sharePath = path
	return "https://share.example/x", nil
}
func (f *fakeOperations) Delete(_ context.Context, path string, permanently bool) error {
	f.deletePath, f.deletePermanent = path, permanently
	return nil
}

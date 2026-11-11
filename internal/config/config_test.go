package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadReadsMinimalConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("token = \" token-value \"\ncookie = \" uid=x \"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "token-value" || cfg.Cookie != "uid=x" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadAllowsOmittedCookie(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`token = "token-value"`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "token-value" || cfg.Cookie != "" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadRejectsInvalidConfigAndLoosePermissions(t *testing.T) {
	for name, data := range map[string]string{
		"missing token":    `cookie = "x"`,
		"empty token":      `token = "   "`,
		"unknown field":    "token = \"x\"\nlisten = \":8080\"",
		"duplicate token":  "token = \"x\"\ntoken = \"y\"",
		"wrong token type": `token = 123`,
		"invalid toml":     `token = "`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil || strings.Contains(err.Error(), "x") || strings.Contains(err.Error(), "y") {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`token = "secret"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("permission error = %v", err)
	}
}

func TestDefaultPathUsesUserConfigDirectory(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "config.toml" || filepath.Base(filepath.Dir(path)) != "weiyun" {
		t.Fatalf("DefaultPath() = %q", path)
	}
}

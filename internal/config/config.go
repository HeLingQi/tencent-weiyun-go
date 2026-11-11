package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	Token  string `toml:"token"`
	Cookie string `toml:"cookie,omitempty"`
}

func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "weiyun", "config.toml"), nil
}

func Load(path string) (Config, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Config{}, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Config{}, errors.New("config file permissions must be 0600 or stricter")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, errors.New("invalid config TOML")
	}
	cfg.Token = strings.TrimSpace(cfg.Token)
	cfg.Cookie = strings.TrimSpace(cfg.Cookie)
	if cfg.Token == "" {
		return Config{}, errors.New("config token is required")
	}
	return cfg, nil
}

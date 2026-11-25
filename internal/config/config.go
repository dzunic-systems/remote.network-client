// Package config is remote-client's settings: ~/.remote/client.yaml (the
// router, the local listen address, direct on/off) and the API key file
// (~/.remote/client-api-key, 0600). The key never appears in logs, output
// or URLs.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

// DefaultRouter is the public network entry point (overridable in the
// config file, the flag, or REMOTE_ROUTER).
const DefaultRouter = "https://spine.remote.network"

// DefaultListen is the proxy's default local address: loopback only —
// anything else requires --local-token (serve refuses to start without
// one on a non-loopback listen).
const DefaultListen = "127.0.0.1:4141"

// Config is the on-disk settings file.
type Config struct {
	// Router is the router base URL (https://…).
	Router string `yaml:"router,omitempty"`
	// Listen is the local proxy address (default 127.0.0.1:4141).
	Listen string `yaml:"listen,omitempty"`
	// Direct, false, sends everything through the router (the default is
	// on; the pointer distinguishes "unset" from "off").
	Direct *bool `yaml:"direct,omitempty"`
	// RouterAlt, when set, is a second router's base URL on a different
	// IP: its punch socket joins the NAT test so an address-dependent
	// mapping (a per-destination-IP translation) is caught — the same
	// cross-check the node listener runs when its fleet knows two routers.
	RouterAlt string `yaml:"router_alt,omitempty"`
}

// Dir is ~/.remote.
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".remote"), nil
}

// Path is the config file's location (~/.remote/client.yaml).
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "client.yaml"), nil
}

// Load reads the config file. A missing file is the zero config with
// defaults applied — not an error.
func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg.withDefaults(), nil
		}
		return Config{}, err
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg.withDefaults(), nil
}

func (c Config) withDefaults() Config {
	if c.Router == "" {
		c.Router = DefaultRouter
	}
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	return c
}

// DirectOn reports whether the direct path is enabled (default on).
func (c Config) DirectOn() bool { return c.Direct == nil || *c.Direct }

// Save writes the config file (0600).
func Save(cfg Config) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "client.yaml")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	// WriteFile's mode applies only to a fresh create; a file left looser
	// by an older version (or a careless copy) must be tightened here.
	return os.Chmod(path, 0o600)
}

// KeyFilePath is the API key file's location
// (~/.remote/client-api-key).
func KeyFilePath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "client-api-key"), nil
}

// ReadKeyFile reads an API key file ("" = the default path). A missing
// default file is not an error — it returns "".
func ReadKeyFile(path string) (string, error) {
	explicit := path != ""
	if !explicit {
		var err error
		path, err = KeyFilePath()
		if err != nil {
			return "", err
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !explicit && errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// WriteKeyFile stores the API key (0600). The caller prints nothing of
// the key.
func WriteKeyFile(path, key string) error {
	explicit := path != ""
	if !explicit {
		var err error
		path, err = KeyFilePath()
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(strings.TrimSpace(key)+"\n"), 0o600); err != nil {
		return err
	}
	// WriteFile's mode applies only to a fresh create (R7): an existing
	// looser file keeps its mode unless we tighten it.
	return os.Chmod(path, 0o600)
}

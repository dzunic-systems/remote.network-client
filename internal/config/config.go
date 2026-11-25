// Package config is remote-client's settings: ~/.remote/client.yaml (the
// router, the local listen address, direct on/off, the model and tool
// defaults) and the API key file (~/.remote/client-api-key, 0600). The
// key never appears in logs, output or URLs. Resolve decides every
// setting (flag > env > file > default) and records each value's source.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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

// Config is the on-disk settings file. The secrets (the API key, the
// local token) have no field here and must never gain one: they live in
// the key file / the environment / a flag only.
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
	// Port is the proxy's port, kept on the listen host (REMOTE_PORT /
	// --port). Conflicting with a listen value that names a different
	// port is a startup error.
	Port *int `yaml:"port,omitempty"`
	// APIKeyFile is where the API key lives (default
	// ~/.remote/client-api-key).
	APIKeyFile string `yaml:"api_key_file,omitempty"`
	// LogLevel is debug|info|warn (default info; --verbose means debug).
	LogLevel string `yaml:"log_level,omitempty"`
	// UI, false, turns the local web UI off (default on; --no-ui).
	UI *bool `yaml:"ui,omitempty"`
	// DefaultModel is the model chat (and the UI) start with.
	DefaultModel string `yaml:"default_model,omitempty"`
	// WebSearch is the default web-search state for the client's own chat
	// surfaces (default off).
	WebSearch *bool `yaml:"web_search,omitempty"`
	// MCPConfig is the local tools' server list
	// (default ~/.remote/mcp.json).
	MCPConfig string `yaml:"mcp_config,omitempty"`
	// Tools enables the local tool loop for the client's own chat
	// surfaces (default: on when an MCP config exists).
	Tools *bool `yaml:"tools,omitempty"`
}

// home is the directory every client file lives in: REMOTE_HOME moves
// them all together; the default is ~/.remote.
func home() (string, error) {
	if h := os.Getenv("REMOTE_HOME"); h != "" {
		return h, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".remote"), nil
}

// DefaultDir is the default ~/.remote (REMOTE_HOME unset).
func DefaultDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, ".remote")
}

// Dir is the settings directory (REMOTE_HOME, else ~/.remote).
func Dir() (string, error) { return home() }

// Path is the config file's location (~/.remote/client.yaml).
func Path() (string, error) {
	dir, err := home()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "client.yaml"), nil
}

// KeyFilePath is the API key file's location
// (~/.remote/client-api-key).
func KeyFilePath() (string, error) {
	dir, err := home()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "client-api-key"), nil
}

// Load reads the config file as written — no defaults applied (Resolve
// owns the defaults). A missing file is the zero config, not an error.
func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return Config{}, err
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Save writes the config file (0600).
func Save(cfg Config) error {
	dir, err := home()
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

// FileKeys are the config file keys `config set`/`unset` (and the web
// UI's settings page) may write, with the human name each is shown as.
// The secrets are refused everywhere: the API key has `login`, the local
// token has its flag.
var FileKeys = []string{
	"router", "router_alt", "listen", "port", "api_key_file",
	"direct", "log_level", "ui", "default_model", "web_search",
	"mcp_config", "tools",
}

// ValidFileKey reports whether key is a settable config file key.
func ValidFileKey(key string) bool {
	return slices.Contains(FileKeys, key)
}

// SetFileKey validates value as at startup and returns the config with
// key set to it. Secrets are refused (they are never written to the
// file); an unknown key is refused.
func SetFileKey(cfg Config, key, value string) (Config, error) {
	switch key {
	case "router":
		if err := ValidRouter(value); err != nil {
			return cfg, err
		}
		cfg.Router = value
	case "router_alt":
		if value != "" {
			if err := ValidRouter(value); err != nil {
				return cfg, err
			}
		}
		cfg.RouterAlt = value
	case "listen":
		if err := ValidListen(value); err != nil {
			return cfg, err
		}
		cfg.Listen = value
	case "port":
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return cfg, fmt.Errorf("port: %q is not a number", value)
		}
		if err := ValidPort(n); err != nil {
			return cfg, err
		}
		cfg.Port = &n
	case "api_key_file":
		if value == "" {
			return cfg, errors.New("api_key_file: a path is required")
		}
		cfg.APIKeyFile = value
	case "direct":
		b, err := ParseBoolText("direct", value)
		if err != nil {
			return cfg, err
		}
		cfg.Direct = &b
	case "log_level":
		if err := ValidLogLevel(value); err != nil {
			return cfg, err
		}
		cfg.LogLevel = value
	case "ui":
		b, err := ParseBoolText("ui", value)
		if err != nil {
			return cfg, err
		}
		cfg.UI = &b
	case "default_model":
		if value == "" {
			return cfg, errors.New("default_model: a model id is required")
		}
		cfg.DefaultModel = value
	case "web_search":
		b, err := ParseBoolText("web_search", value)
		if err != nil {
			return cfg, err
		}
		cfg.WebSearch = &b
	case "mcp_config":
		if value == "" {
			return cfg, errors.New("mcp_config: a path is required")
		}
		cfg.MCPConfig = value
	case "tools":
		b, err := ParseBoolText("tools", value)
		if err != nil {
			return cfg, err
		}
		cfg.Tools = &b
	default:
		// The secrets read plausibly as settings; name the way out.
		if key == "api_key" || key == "local_token" {
			return cfg, fmt.Errorf("%s is a secret and is never written to the config file: use `login` for the API key, --local-token for the token", key)
		}
		return cfg, fmt.Errorf("unknown setting %q (settable: %s)", key, strings.Join(FileKeys, ", "))
	}
	return cfg, nil
}

// UnsetFileKey returns the config with key cleared (the zero value drops
// out of the YAML, so the file stays small).
func UnsetFileKey(cfg Config, key string) (Config, error) {
	switch key {
	case "router":
		cfg.Router = ""
	case "router_alt":
		cfg.RouterAlt = ""
	case "listen":
		cfg.Listen = ""
	case "port":
		cfg.Port = nil
	case "api_key_file":
		cfg.APIKeyFile = ""
	case "direct":
		cfg.Direct = nil
	case "log_level":
		cfg.LogLevel = ""
	case "ui":
		cfg.UI = nil
	case "default_model":
		cfg.DefaultModel = ""
	case "web_search":
		cfg.WebSearch = nil
	case "mcp_config":
		cfg.MCPConfig = ""
	case "tools":
		cfg.Tools = nil
	default:
		if key == "api_key" || key == "local_token" {
			return cfg, fmt.Errorf("%s is a secret and is not stored in the config file", key)
		}
		return cfg, fmt.Errorf("unknown setting %q (settable: %s)", key, strings.Join(FileKeys, ", "))
	}
	return cfg, nil
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

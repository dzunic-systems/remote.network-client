// Resolve: one place where every remote-client setting is decided.
//
// Each setting is resolved in the order flag > environment > config file >
// default, and the winner's source is recorded, so `config show` and the
// web UI can say where a value came from (and lock the ones a flag or an
// environment variable owns — the user set them for this run on purpose).
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Source kinds, in resolution order.
const (
	SourceFlag    = "flag"
	SourceEnv     = "env"
	SourceFile    = "file"
	SourceDefault = "default"
	SourceUnset   = "unset"
)

// Source says where a setting's value came from. Name carries the flag
// or environment variable's name ("--listen", "REMOTE_LISTEN") so the UI
// can show "set by REMOTE_LISTEN"; it is empty for file/default/unset.
type Source struct {
	Kind string `json:"source"`
	Name string `json:"source_name,omitempty"`
}

// Unset is the source of a setting nothing provided.
var Unset = Source{Kind: SourceUnset}

// String renders the source the way `config show` prints it.
func (s Source) String() string {
	switch s.Kind {
	case SourceFlag:
		return "flag (" + s.Name + ")"
	case SourceEnv:
		return "env " + s.Name
	case SourceFile:
		return "config file"
	case SourceDefault:
		return "default"
	default:
		return "unset"
	}
}

// Locked reports whether the value cannot be changed through the config
// file (or the UI): a flag or an environment variable owns this run.
func (s Source) Locked() bool { return s.Kind == SourceFlag || s.Kind == SourceEnv }

// Value is one resolved setting: the effective value and where it came
// from.
type Value[T any] struct {
	Value  T
	Source Source
}

// Set reports whether anything provided the value (the source is not
// unset).
func (v Value[T]) Set() bool { return v.Source.Kind != SourceUnset }

// pickStr is the string chain, listed in resolution order: the first
// non-empty source wins. An empty default means nothing provides the
// setting (unset), not "a default of nothing".
func pickStr(flagV, envV, fileV, def, flagName, envName string) Value[string] {
	switch {
	case flagV != "":
		return Value[string]{flagV, Source{SourceFlag, flagName}}
	case envV != "":
		return Value[string]{envV, Source{SourceEnv, envName}}
	case fileV != "":
		return Value[string]{fileV, Source{SourceFile, ""}}
	case def != "":
		return Value[string]{def, Source{SourceDefault, ""}}
	default:
		return Value[string]{"", Unset}
	}
}

// resolveStr is pickStr with the environment read here.
func resolveStr(flagV, flagName, envName, fileV, def string) Value[string] {
	return pickStr(flagV, os.Getenv(envName), fileV, def, flagName, envName)
}

// pickBoolEnv resolves one boolean: a flag wins (with the value the flag
// MEANS — --no-direct's presence means off), then the env var's text,
// then the file's value, then the default. The env text is parsed
// strictly — anything but a boolean spelling is a startup error that
// names the variable.
func pickBoolEnv(flagGiven bool, flagValue bool, flagName, envName, envV string, fileV *bool, def bool) (Value[bool], error) {
	switch {
	case flagGiven:
		return Value[bool]{flagValue, Source{SourceFlag, flagName}}, nil
	case envV != "":
		b, err := ParseBoolText(envName, envV)
		if err != nil {
			return Value[bool]{}, err
		}
		return Value[bool]{b, Source{SourceEnv, envName}}, nil
	case fileV != nil:
		return Value[bool]{*fileV, Source{SourceFile, ""}}, nil
	default:
		return Value[bool]{def, Source{SourceDefault, ""}}, nil
	}
}

// ParseBoolText is the boolean spelling every text form of a boolean
// accepts: 1/0/true/false/on/off/yes/no (case-insensitive). Anything else
// is an error that names where the text came from.
func ParseBoolText(where, v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "on", "yes":
		return true, nil
	case "0", "false", "off", "no":
		return false, nil
	default:
		return false, fmt.Errorf("%s: %q is not a boolean (use 1/0/true/false/on/off/yes/no)", where, v)
	}
}

// ValidRouter checks a router base URL: https:// always, except
// http://localhost / http://127.0.0.1 / http://[::1] for development —
// the API key rides every request, so it must never travel plaintext to
// a remote host.
func ValidRouter(u string) error {
	p, err := url.Parse(u)
	if err != nil {
		return fmt.Errorf("%q is not a URL: %w", u, err)
	}
	switch p.Scheme {
	case "https":
		return nil
	case "http":
		host := p.Hostname()
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil
		}
		return fmt.Errorf("%q: a router over http is only allowed on localhost (the API key rides every request)", u)
	default:
		return fmt.Errorf("%q: the router URL must be https:// (http://localhost for development)", u)
	}
}

// ValidListen checks a listen address: host:port with a port 1–65535.
func ValidListen(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not a host:port listen address: %w", addr, err)
	}
	return validPortText(port)
}

func validPortText(p string) error {
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%q is not a port (1–65535)", p)
	}
	return nil
}

// ValidPort checks a port number.
func ValidPort(n int) error {
	if n < 1 || n > 65535 {
		return fmt.Errorf("%d is not a port (1–65535)", n)
	}
	return nil
}

// ValidLogLevel checks a log level: debug|info|warn.
func ValidLogLevel(s string) error {
	switch s {
	case "debug", "info", "warn":
		return nil
	default:
		return fmt.Errorf("%q is not a log level (debug|info|warn)", s)
	}
}

// Flags carries the command line's settings (empty = not given). The
// bools are presences: --no-direct turns direct off, --no-ui turns the
// UI off, --web-search turns search on, --verbose raises the log level.
type Flags struct {
	Router     string
	RouterAlt  string
	Listen     string
	Port       int // 0 = not given
	APIKey     string
	APIKeyFile string
	NoDirect   bool
	LocalToken string
	Verbose    bool
	NoUI       bool
	Model      string // chat's --model (the default model for its chat surfaces)
	WebSearch  bool   // chat's --web-search
}

// Settings is every resolved setting with its source. The secret values
// (APIKey, LocalToken) ride here for the commands that need them, but
// nothing prints them: show and the UI render presence and source only.
type Settings struct {
	Home         Value[string] // the ~/.remote directory (REMOTE_HOME moves it)
	Router       Value[string]
	RouterAlt    Value[string] // optional second router; unset when empty
	Listen       Value[string] // host:port the proxy listens on
	Port         Value[int]    // the effective port (kept on the listen host)
	APIKey       Value[string] // secret; unset when no source had one
	APIKeyFile   Value[string]
	Direct       Value[bool]
	LocalToken   Value[string] // secret; unset when none was given
	LogLevel     Value[string] // debug|info|warn
	UI           Value[bool]
	DefaultModel Value[string]
	WebSearch    Value[bool]
	MCPConfig    Value[string]
	Tools        Value[bool] // default: on when an MCP config file exists
}

// Resolve decides every setting. A missing config file is fine; a bad
// value from any source is a startup error that names that source.
func Resolve(fl Flags) (Settings, error) {
	var s Settings
	file, err := Load()
	if err != nil {
		return s, err
	}

	// Home: environment only — it decides where every other file lives,
	// so it is resolved before anything that names a file.
	s.Home = Value[string]{DefaultDir(), Source{SourceDefault, ""}}
	if h := os.Getenv("REMOTE_HOME"); h != "" {
		s.Home = Value[string]{h, Source{SourceEnv, "REMOTE_HOME"}}
	}

	s.Router = resolveStr(fl.Router, "--router", "REMOTE_ROUTER", file.Router, DefaultRouter)
	if err := ValidRouter(s.Router.Value); err != nil {
		return s, err
	}
	s.RouterAlt = resolveStr(fl.RouterAlt, "--router-alt", "REMOTE_ROUTER_ALT", file.RouterAlt, "")
	if s.RouterAlt.Set() {
		if err := ValidRouter(s.RouterAlt.Value); err != nil {
			return s, fmt.Errorf("router-alt: %w", err)
		}
	}

	// Listen and port are one setting read two ways: the port is kept on
	// the listen host. Setting both with different ports is an error — a
	// listen value from the flag, the environment or the file is "set";
	// the default's port is not, and the port setting replaces it.
	listen := resolveStr(fl.Listen, "--listen", "REMOTE_LISTEN", file.Listen, DefaultListen)
	if err := ValidListen(listen.Value); err != nil {
		return s, err
	}
	portSrc := Source{SourceDefault, ""}
	portNum := 0
	switch {
	case fl.Port != 0:
		if err := ValidPort(fl.Port); err != nil {
			return s, fmt.Errorf("--port: %w", err)
		}
		portNum, portSrc = fl.Port, Source{SourceFlag, "--port"}
	case os.Getenv("REMOTE_PORT") != "":
		pe := os.Getenv("REMOTE_PORT")
		n, err := strconv.Atoi(strings.TrimSpace(pe))
		if err != nil {
			return s, fmt.Errorf("REMOTE_PORT: %q is not a port (1–65535)", pe)
		}
		if err := ValidPort(n); err != nil {
			return s, fmt.Errorf("REMOTE_PORT: %w", err)
		}
		portNum, portSrc = n, Source{SourceEnv, "REMOTE_PORT"}
	case file.Port != nil:
		if err := ValidPort(*file.Port); err != nil {
			return s, fmt.Errorf("config file: port: %w", err)
		}
		portNum, portSrc = *file.Port, Source{SourceFile, ""}
	}
	listenHost, listenPort, err := net.SplitHostPort(listen.Value)
	if err != nil {
		return s, err // ValidListen already named the problem's shape
	}
	// A port set alongside an EXPLICIT listen value that names a
	// different port is an error. The default's port is not an explicit
	// value: the port replaces it, kept on the default's host.
	if portNum != 0 && listen.Source.Kind != SourceDefault && listenPort != strconv.Itoa(portNum) {
		return s, fmt.Errorf("port %d conflicts with listen %q: the listen address names a different port",
			portNum, listen.Value)
	}
	if portNum == 0 {
		n, err := strconv.Atoi(listenPort)
		if err != nil {
			return s, err
		}
		portNum, portSrc = n, listen.Source
	}
	listenSrc := listen.Source
	if listen.Source.Kind == SourceDefault && portSrc.Kind != SourceDefault {
		// The port shaped a value the default only half provided.
		listenSrc = portSrc
	}
	s.Listen = Value[string]{net.JoinHostPort(listenHost, strconv.Itoa(portNum)), listenSrc}
	s.Port = Value[int]{portNum, portSrc}

	// The API key: flag > env > the key file's contents (never a value in
	// the config file). A missing DEFAULT key file just means no key
	// there; an explicitly named one that is missing is an error. The
	// default hangs off the resolved home (REMOTE_HOME moves it).
	s.APIKeyFile = resolveStr(fl.APIKeyFile, "--api-key-file", "REMOTE_API_KEY_FILE",
		file.APIKeyFile, filepath.Join(s.Home.Value, "client-api-key"))
	s.APIKey = Value[string]{Source: Unset}
	switch {
	case fl.APIKey != "":
		s.APIKey = Value[string]{fl.APIKey, Source{SourceFlag, "--api-key"}}
	case os.Getenv("REMOTE_API_KEY") != "":
		s.APIKey = Value[string]{os.Getenv("REMOTE_API_KEY"), Source{SourceEnv, "REMOTE_API_KEY"}}
	default:
		keyPath := s.APIKeyFile.Value
		if s.APIKeyFile.Source.Kind == SourceDefault {
			keyPath = "" // ReadKeyFile reads the default leniently
		}
		k, err := ReadKeyFile(keyPath)
		if err != nil {
			return s, err
		}
		if k != "" {
			s.APIKey = Value[string]{k, Source{SourceFile, ""}}
		}
	}

	// --no-direct: the flag's PRESENCE means off (a plain --direct does
	// not exist — the default is on).
	if s.Direct, err = pickBoolEnv(fl.NoDirect, false, "--no-direct", "REMOTE_DIRECT",
		os.Getenv("REMOTE_DIRECT"), file.Direct, true); err != nil {
		return s, err
	}

	s.LocalToken = Value[string]{Source: Unset}
	switch {
	case fl.LocalToken != "":
		s.LocalToken = Value[string]{fl.LocalToken, Source{SourceFlag, "--local-token"}}
	case os.Getenv("REMOTE_LOCAL_TOKEN") != "":
		s.LocalToken = Value[string]{os.Getenv("REMOTE_LOCAL_TOKEN"), Source{SourceEnv, "REMOTE_LOCAL_TOKEN"}}
	}

	// --verbose is the log-level flag: its presence means debug, at flag
	// source.
	verbose := ""
	if fl.Verbose {
		verbose = "debug"
	}
	s.LogLevel = resolveStr(verbose, "--verbose", "REMOTE_LOG_LEVEL", file.LogLevel, "info")
	if err := ValidLogLevel(s.LogLevel.Value); err != nil {
		return s, err
	}

	// --no-ui: the flag's presence means the UI is off this run.
	if s.UI, err = pickBoolEnv(fl.NoUI, false, "--no-ui", "REMOTE_UI",
		os.Getenv("REMOTE_UI"), file.UI, true); err != nil {
		return s, err
	}

	s.DefaultModel = resolveStr(fl.Model, "--model", "REMOTE_DEFAULT_MODEL", file.DefaultModel, "")
	// --web-search: the flag's presence means search is on for this run's
	// own chat surfaces.
	if s.WebSearch, err = pickBoolEnv(fl.WebSearch, true, "--web-search", "REMOTE_WEB_SEARCH",
		os.Getenv("REMOTE_WEB_SEARCH"), file.WebSearch, false); err != nil {
		return s, err
	}

	// No command carries --mcp-config yet (the local tools arrive with the
	// tools phase); the environment and the file already work.
	s.MCPConfig = resolveStr("", "--mcp-config", "REMOTE_MCP_CONFIG",
		file.MCPConfig, filepath.Join(s.Home.Value, "mcp.json"))
	if s.Tools, err = pickBoolEnv(false, false, "", "REMOTE_TOOLS", os.Getenv("REMOTE_TOOLS"),
		file.Tools, mcpConfigExists(s.MCPConfig.Value)); err != nil {
		return s, err
	}
	return s, nil
}

// mcpConfigExists reports whether the MCP config file is there (the
// tools default: on when servers are configured).
func mcpConfigExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

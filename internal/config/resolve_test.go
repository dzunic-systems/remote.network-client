package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolate gives a test a clean environment: a temp HOME, no REMOTE_*
// variables, so the machine's real setup cannot leak into a case.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows: os.UserHomeDir reads USERPROFILE
	for _, name := range []string{
		"REMOTE_HOME", "REMOTE_ROUTER", "REMOTE_ROUTER_ALT", "REMOTE_LISTEN",
		"REMOTE_PORT", "REMOTE_API_KEY", "REMOTE_API_KEY_FILE", "REMOTE_DIRECT",
		"REMOTE_LOCAL_TOKEN", "REMOTE_LOG_LEVEL", "REMOTE_UI", "REMOTE_DEFAULT_MODEL",
		"REMOTE_WEB_SEARCH", "REMOTE_MCP_CONFIG", "REMOTE_TOOLS",
	} {
		t.Setenv(name, "")
	}
	return home
}

// writeFile writes a config file into the isolated home.
func writeFile(t *testing.T, text string) {
	t.Helper()
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPrecedenceFlagBeatsEnvBeatsFileBeatsDefault(t *testing.T) {
	isolate(t)
	writeFile(t, "router: https://file.example.com\nlog_level: warn\n")

	// No flag: the file speaks (over the default).
	s, err := Resolve(Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Router.Value != "https://file.example.com" || s.Router.Source.Kind != SourceFile {
		t.Fatalf("router = %v from %v, want the file's", s.Router.Value, s.Router.Source)
	}
	if s.LogLevel.Value != "warn" || s.LogLevel.Source.Kind != SourceFile {
		t.Fatalf("log_level = %v from %v", s.LogLevel.Value, s.LogLevel.Source)
	}
	if s.RouterAlt.Source.Kind != SourceUnset {
		t.Fatalf("router_alt should be unset, got %v", s.RouterAlt.Source)
	}

	// An env var beats the file.
	t.Setenv("REMOTE_ROUTER", "https://env.example.com")
	s, err = Resolve(Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Router.Value != "https://env.example.com" || s.Router.Source.Kind != SourceEnv || s.Router.Source.Name != "REMOTE_ROUTER" {
		t.Fatalf("router = %v from %v, want the env's", s.Router.Value, s.Router.Source)
	}

	// A flag beats both.
	s, err = Resolve(Flags{Router: "https://flag.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Router.Value != "https://flag.example.com" || s.Router.Source.Kind != SourceFlag || s.Router.Source.Name != "--router" {
		t.Fatalf("router = %v from %v, want the flag's", s.Router.Value, s.Router.Source)
	}
}

func TestDefaultsWhenNothingSet(t *testing.T) {
	isolate(t)
	s, err := Resolve(Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Router.Value != DefaultRouter || s.Router.Source.Kind != SourceDefault {
		t.Fatalf("router = %v (%v)", s.Router.Value, s.Router.Source)
	}
	if s.Listen.Value != DefaultListen || s.Listen.Source.Kind != SourceDefault {
		t.Fatalf("listen = %v (%v)", s.Listen.Value, s.Listen.Source)
	}
	if s.Port.Value != 4141 || s.Port.Source.Kind != SourceDefault {
		t.Fatalf("port = %d (%v)", s.Port.Value, s.Port.Source)
	}
	if !s.Direct.Value || s.Direct.Source.Kind != SourceDefault {
		t.Fatalf("direct = %v (%v)", s.Direct.Value, s.Direct.Source)
	}
	if s.LogLevel.Value != "info" {
		t.Fatalf("log_level = %v", s.LogLevel.Value)
	}
	if !s.UI.Value {
		t.Fatalf("ui = %v", s.UI.Value)
	}
	if s.WebSearch.Value {
		t.Fatalf("web_search = %v, want off by default", s.WebSearch.Value)
	}
	if s.Tools.Value {
		t.Fatalf("tools = %v, want off with no MCP config file", s.Tools.Value)
	}
	if s.APIKey.Set() {
		t.Fatalf("api_key should be unset, got %v", s.APIKey.Source)
	}
}

func TestBadBooleanNamesTheVariable(t *testing.T) {
	isolate(t)
	t.Setenv("REMOTE_DIRECT", "maybe")
	_, err := Resolve(Flags{})
	if err == nil || !strings.Contains(err.Error(), "REMOTE_DIRECT") || !strings.Contains(err.Error(), "maybe") {
		t.Fatalf("err = %v, want it to name REMOTE_DIRECT and the bad text", err)
	}
	t.Setenv("REMOTE_DIRECT", "")
	t.Setenv("REMOTE_WEB_SEARCH", "yeah")
	if _, err = Resolve(Flags{}); err == nil || !strings.Contains(err.Error(), "REMOTE_WEB_SEARCH") {
		t.Fatalf("err = %v, want it to name REMOTE_WEB_SEARCH", err)
	}
	// Every documented spelling parses.
	t.Setenv("REMOTE_WEB_SEARCH", "")
	for _, spelling := range []string{"1", "0", "true", "false", "on", "off", "yes", "no", "TRUE", "Off"} {
		t.Setenv("REMOTE_DIRECT", spelling)
		s, err := Resolve(Flags{})
		if err != nil {
			t.Fatalf("REMOTE_DIRECT=%s: %v", spelling, err)
		}
		if want := spelling == "1" || spelling == "true" || spelling == "on" || spelling == "yes" || spelling == "TRUE"; s.Direct.Value != want {
			t.Fatalf("REMOTE_DIRECT=%s: direct = %v", spelling, s.Direct.Value)
		}
	}
}

func TestBadRouterURLRejected(t *testing.T) {
	isolate(t)
	t.Setenv("REMOTE_ROUTER", "http://insecure.example.com")
	_, err := Resolve(Flags{})
	if err == nil || !strings.Contains(err.Error(), "http") {
		t.Fatalf("err = %v, want a plaintext-router refusal", err)
	}
	// Development loopback http is allowed.
	t.Setenv("REMOTE_ROUTER", "http://127.0.0.1:8080")
	if _, err = Resolve(Flags{}); err != nil {
		t.Fatalf("http://127.0.0.1 rejected: %v", err)
	}
	t.Setenv("REMOTE_ROUTER", "http://localhost:8080")
	if _, err = Resolve(Flags{}); err != nil {
		t.Fatalf("http://localhost rejected: %v", err)
	}
	// Not a URL at all.
	t.Setenv("REMOTE_ROUTER", "spine.remote.network")
	if _, err = Resolve(Flags{}); err == nil {
		t.Fatalf("a schemeless router URL must be refused")
	}
}

func TestPortConflictsWithListen(t *testing.T) {
	isolate(t)
	writeFile(t, "listen: 127.0.0.1:4141\n")
	t.Setenv("REMOTE_PORT", "5151")
	_, err := Resolve(Flags{})
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("err = %v, want a port/listen conflict", err)
	}
	// The same port twice is fine.
	t.Setenv("REMOTE_PORT", "4141")
	s, err := Resolve(Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Listen.Value != "127.0.0.1:4141" || s.Port.Source.Kind != SourceEnv {
		t.Fatalf("listen = %v, port source = %v", s.Listen.Value, s.Port.Source)
	}

	// No listen value: the port replaces the default's port, kept on the
	// default's host.
	isolate(t)
	t.Setenv("REMOTE_PORT", "5151")
	s, err = Resolve(Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Listen.Value != "127.0.0.1:5151" || s.Listen.Source.Kind != SourceEnv || s.Port.Value != 5151 {
		t.Fatalf("listen = %v (%v), port = %d (%v)", s.Listen.Value, s.Listen.Source, s.Port.Value, s.Port.Source)
	}
	// A --port flag with the default listen behaves the same.
	isolate(t)
	s, err = Resolve(Flags{Port: 6000})
	if err != nil {
		t.Fatal(err)
	}
	if s.Listen.Value != "127.0.0.1:6000" || s.Listen.Source.Kind != SourceFlag || s.Port.Source.Name != "--port" {
		t.Fatalf("listen = %v (%v), port (%v)", s.Listen.Value, s.Listen.Source, s.Port.Source)
	}
}

func TestRemoteHomeRelocatesEveryFile(t *testing.T) {
	isolate(t)
	moved := t.TempDir()
	t.Setenv("REMOTE_HOME", moved)

	if dir, err := Dir(); err != nil || dir != moved {
		t.Fatalf("Dir() = %s, %v; want %s", dir, err, moved)
	}
	if p, err := Path(); err != nil || p != filepath.Join(moved, "client.yaml") {
		t.Fatalf("Path() = %s, %v", p, err)
	}
	if p, err := KeyFilePath(); err != nil || p != filepath.Join(moved, "client-api-key") {
		t.Fatalf("KeyFilePath() = %s, %v", p, err)
	}
	s, err := Resolve(Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Home.Value != moved || s.Home.Source.Kind != SourceEnv {
		t.Fatalf("home = %v (%v)", s.Home.Value, s.Home.Source)
	}
	if s.APIKeyFile.Value != filepath.Join(moved, "client-api-key") {
		t.Fatalf("api_key_file = %s", s.APIKeyFile.Value)
	}
	if s.MCPConfig.Value != filepath.Join(moved, "mcp.json") {
		t.Fatalf("mcp_config = %s", s.MCPConfig.Value)
	}

	// The key file in the moved home is found and its presence is a key.
	if err := WriteKeyFile("", "fx-relocated-key"); err != nil {
		t.Fatal(err)
	}
	s, err = Resolve(Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if s.APIKey.Value != "fx-relocated-key" || s.APIKey.Source.Kind != SourceFile {
		t.Fatalf("api_key = %v from %v", s.APIKey.Value, s.APIKey.Source)
	}
}

func TestToolsDefaultFollowsMCPConfigPresence(t *testing.T) {
	isolate(t)
	s, err := Resolve(Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Tools.Value || s.Tools.Source.Kind != SourceDefault {
		t.Fatalf("tools = %v (%v) with no MCP config", s.Tools.Value, s.Tools.Source)
	}
	if err := os.MkdirAll(filepath.Dir(s.MCPConfig.Value), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.MCPConfig.Value, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = Resolve(Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Tools.Value {
		t.Fatalf("tools = %v with an MCP config present", s.Tools.Value)
	}
	// An explicit setting still wins over the presence default.
	t.Setenv("REMOTE_TOOLS", "off")
	s, err = Resolve(Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Tools.Value || s.Tools.Source.Kind != SourceEnv {
		t.Fatalf("tools = %v (%v)", s.Tools.Value, s.Tools.Source)
	}
}

func TestInvertedFlagsMeanFalse(t *testing.T) {
	isolate(t)
	// --no-direct turns the direct path OFF (a regression guard: the flag
	// means what it says, not "the setting is true").
	s, err := Resolve(Flags{NoDirect: true})
	if err != nil {
		t.Fatal(err)
	}
	if s.Direct.Value || s.Direct.Source.Kind != SourceFlag || s.Direct.Source.Name != "--no-direct" {
		t.Fatalf("direct = %v (%v), want off at flag source", s.Direct.Value, s.Direct.Source)
	}
	// --no-ui turns the UI off the same way.
	s, _ = Resolve(Flags{NoUI: true})
	if s.UI.Value {
		t.Fatalf("ui = %v with --no-ui", s.UI.Value)
	}
	// And an env REMOTE_DIRECT=off beats the file but not the flag.
	t.Setenv("REMOTE_DIRECT", "off")
	s, _ = Resolve(Flags{})
	if s.Direct.Value || s.Direct.Source.Kind != SourceEnv {
		t.Fatalf("direct = %v (%v)", s.Direct.Value, s.Direct.Source)
	}
	s, _ = Resolve(Flags{NoDirect: true})
	if s.Direct.Source.Kind != SourceFlag {
		t.Fatalf("the flag must win over the env: %v", s.Direct.Source)
	}
}

func TestVerboseMeansDebugAtFlagSource(t *testing.T) {
	isolate(t)
	s, err := Resolve(Flags{Verbose: true})
	if err != nil {
		t.Fatal(err)
	}
	if s.LogLevel.Value != "debug" || s.LogLevel.Source.Kind != SourceFlag || s.LogLevel.Source.Name != "--verbose" {
		t.Fatalf("log_level = %v (%v)", s.LogLevel.Value, s.LogLevel.Source)
	}
	// A file value holds without the flag.
	writeFile(t, "log_level: warn\n")
	s, err = Resolve(Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if s.LogLevel.Value != "warn" || s.LogLevel.Source.Kind != SourceFile {
		t.Fatalf("log_level = %v (%v)", s.LogLevel.Value, s.LogLevel.Source)
	}
}

func TestSecretsResolveFlagEnvFileButNeverFromFile(t *testing.T) {
	isolate(t)
	// The local token: flag and env only.
	s, err := Resolve(Flags{LocalToken: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if s.LocalToken.Value != "tok" || s.LocalToken.Source.Kind != SourceFlag {
		t.Fatalf("local_token = %v (%v)", s.LocalToken.Value, s.LocalToken.Source)
	}
	t.Setenv("REMOTE_LOCAL_TOKEN", "envtok")
	s, _ = Resolve(Flags{})
	if s.LocalToken.Value != "envtok" || s.LocalToken.Source.Kind != SourceEnv {
		t.Fatalf("local_token = %v (%v)", s.LocalToken.Value, s.LocalToken.Source)
	}

	// The API key: flag > env > key file.
	isolate(t)
	t.Setenv("REMOTE_API_KEY", "env-key")
	s, _ = Resolve(Flags{})
	if s.APIKey.Value != "env-key" || s.APIKey.Source.Kind != SourceEnv {
		t.Fatalf("api_key = %v (%v)", s.APIKey.Value, s.APIKey.Source)
	}
	isolate(t)
	s, _ = Resolve(Flags{APIKey: "flag-key"})
	if s.APIKey.Value != "flag-key" || s.APIKey.Source.Kind != SourceFlag {
		t.Fatalf("api_key = %v (%v)", s.APIKey.Value, s.APIKey.Source)
	}
	isolate(t)
	if err := WriteKeyFile("", "file-key"); err != nil {
		t.Fatal(err)
	}
	s, _ = Resolve(Flags{})
	if s.APIKey.Value != "file-key" || s.APIKey.Source.Kind != SourceFile {
		t.Fatalf("api_key = %v (%v)", s.APIKey.Value, s.APIKey.Source)
	}
}

func TestSaveNeverWritesSecretFields(t *testing.T) {
	isolate(t)
	cfg := Config{Router: "https://x.example.com"}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"api_key:", "local_token:"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("config file contains %q:\n%s", secret, b)
		}
	}
}

func TestSetFileKeyValidatesLikeStartup(t *testing.T) {
	cases := []struct {
		key, value string
		wantErr    string // "" = no error
	}{
		{"router", "https://ok.example.com", ""},
		{"router", "http://127.0.0.1:9000", ""},
		{"router", "http://bad.example.com", "http"},
		{"router_alt", "https://alt.example.com", ""},
		{"listen", "127.0.0.1:4141", ""},
		{"listen", "no-port", "host:port"},
		{"port", "5151", ""},
		{"port", "0", "not a port"},
		{"port", "seven-thousand", "not a number"},
		{"direct", "off", ""},
		{"direct", "sometimes", "not a boolean"},
		{"log_level", "debug", ""},
		{"log_level", "loud", "not a log level"},
		{"ui", "0", ""},
		{"web_search", "on", ""},
		{"default_model", "hf:org/model", ""},
		{"mcp_config", "~/x/mcp.json", ""},
		{"api_key_file", "/tmp/k", ""},
		{"tools", "yes", ""},
		{"api_key", "rn_sk_secret1", "secret"},
		{"local_token", "hunter2", "secret"},
		{"nonsense", "x", "unknown setting"},
	}
	for _, c := range cases {
		_, err := SetFileKey(Config{}, c.key, c.value)
		if c.wantErr == "" && err != nil {
			t.Errorf("set %s=%s: %v", c.key, c.value, err)
		}
		if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
			t.Errorf("set %s=%s: err = %v, want it to mention %q", c.key, c.value, err, c.wantErr)
		}
	}

	// The values land on the right fields.
	cfg, err := SetFileKey(Config{}, "direct", "off")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Direct == nil || *cfg.Direct {
		t.Fatalf("direct = %v", cfg.Direct)
	}
	cfg, err = SetFileKey(cfg, "port", "6000")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port == nil || *cfg.Port != 6000 {
		t.Fatalf("port = %v", cfg.Port)
	}
}

func TestUnsetFileKeyClearsAndRoundTrips(t *testing.T) {
	isolate(t)
	off, nine := false, 9
	cfg := Config{Router: "https://x", Direct: &off, Port: &nine}
	cfg, err := UnsetFileKey(cfg, "router")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = UnsetFileKey(cfg, "port")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Router != "" || cfg.Port != nil || cfg.Direct == nil {
		t.Fatalf("cfg = %+v", cfg)
	}
	if _, err := UnsetFileKey(cfg, "api_key"); err == nil {
		t.Fatalf("unsetting the api_key must be refused")
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	back, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if back.Direct == nil || *back.Direct {
		t.Fatalf("round trip lost direct=off: %+v", back)
	}
}

func TestFilePortConflictFailsResolution(t *testing.T) {
	isolate(t)
	writeFile(t, "listen: 127.0.0.1:4141\nport: 5151\n")
	_, err := Resolve(Flags{})
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("err = %v, want the file's port/listen conflict named", err)
	}
}

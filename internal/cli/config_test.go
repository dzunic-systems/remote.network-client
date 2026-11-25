package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateEnv pins one temp HOME for the whole test and clears every
// REMOTE_* variable, so the machine's real setup cannot leak into a case.
func isolateEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
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

// runConfig runs one config subcommand against the test's environment.
func runConfig(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := newRoot()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"config"}, args...))
	err := root.Execute()
	return out.String(), err
}

func configFile() string {
	return filepath.Join(os.Getenv("HOME"), ".remote", "client.yaml")
}

func TestConfigShowListsEverySettingWithSource(t *testing.T) {
	isolateEnv(t)
	out, err := runConfig(t, "show")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"home", "router", "router_alt", "listen", "port", "api_key",
		"api_key_file", "local_token", "direct", "log_level", "ui",
		"default_model", "web_search", "mcp_config", "tools",
	} {
		if !strings.Contains(out, "\n"+name) && !strings.HasPrefix(out, name) {
			t.Errorf("config show is missing %q:\n%s", name, out)
		}
	}
	// The defaults are named as defaults.
	if !strings.Contains(out, "https://spine.remote.network") || !strings.Contains(out, "default") {
		t.Errorf("config show should name the default router and its source:\n%s", out)
	}
	// Unset settings say so.
	if !strings.Contains(out, "(unset)") {
		t.Errorf("config show should mark unset settings:\n%s", out)
	}
}

func TestConfigShowNeverPrintsSecrets(t *testing.T) {
	isolateEnv(t)
	t.Setenv("REMOTE_API_KEY", "fx-secret-77")
	t.Setenv("REMOTE_LOCAL_TOKEN", "hunter2secret")
	out, err := runConfig(t, "show")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "fx-secret-77") || strings.Contains(out, "hunter2secret") {
		t.Fatalf("config show leaked a secret:\n%s", out)
	}
	if !strings.Contains(out, "set") {
		t.Fatalf("config show should say the secret is set:\n%s", out)
	}
	if !strings.Contains(out, "env REMOTE_API_KEY") {
		t.Fatalf("config show should name the secret's source:\n%s", out)
	}
}

func TestConfigSetValidatesWritesAndUnsetClears(t *testing.T) {
	isolateEnv(t)
	out, err := runConfig(t, "set", "router", "https://spine2.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "router written to") {
		t.Fatalf("output = %q", out)
	}
	b, err := os.ReadFile(configFile())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "router: https://spine2.example.com") {
		t.Fatalf("config file = %s", b)
	}

	// A bad value is refused, and nothing is written for it.
	if _, err := runConfig(t, "set", "router", "http://nope.example.com"); err == nil {
		t.Fatalf("a plaintext remote router must be refused")
	}
	// Secrets are refused with the way out named.
	if _, err := runConfig(t, "set", "api_key", "fx-secret-12345"); err == nil || !strings.Contains(err.Error(), "login") {
		t.Fatalf("err = %v, want a refusal naming login", err)
	}
	if _, err := runConfig(t, "set", "local_token", "tok"); err == nil {
		t.Fatalf("setting the local token must be refused")
	}
	// Unknown keys are refused with the list.
	if _, err := runConfig(t, "set", "colour", "green"); err == nil || !strings.Contains(err.Error(), "unknown setting") {
		t.Fatalf("err = %v", err)
	}
	// The file still holds only the first write.
	b, err = os.ReadFile(configFile())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "fx-secret") || strings.Contains(string(b), "colour") {
		t.Fatalf("config file = %s", b)
	}

	// unset clears the key out of the file entirely.
	if _, err := runConfig(t, "unset", "router"); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(configFile())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "router") {
		t.Fatalf("config file after unset = %s", b)
	}
}

func TestConfigSetPortConflictingWithFileListenIsRefused(t *testing.T) {
	isolateEnv(t)
	if _, err := runConfig(t, "set", "listen", "127.0.0.1:4141"); err != nil {
		t.Fatal(err)
	}
	if _, err := runConfig(t, "set", "port", "5151"); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("err = %v, want the port/listen conflict", err)
	}
}

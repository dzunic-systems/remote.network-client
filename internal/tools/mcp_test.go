package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

var fakeBin string

// TestMain builds the fake stdio MCP server once (the plan's "a fake
// stdio MCP server binary built in the test").
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakemcp-*")
	if err != nil {
		panic(err)
	}
	fakeBin = filepath.Join(dir, "fakemcp")
	if runtime.GOOS == "windows" {
		fakeBin += ".exe" // exec runs only named executables there
	}
	// The test binary's CWD is this package's dir; testdata is excluded
	// from ./... traversal but builds fine when named directly.
	cmd := exec.Command("go", "build", "-o", fakeBin, "./testdata/fakemcp")
	if out, err := cmd.CombinedOutput(); err != nil {
		panic("building the fake MCP server failed: " + string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func testConfig(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("REMOTE_HOME", "")
	dir := filepath.Join(home, ".remote")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return home, dir
}

func writeMCP(t *testing.T, dir, jsonText string) string {
	t.Helper()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(jsonText), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfig(t *testing.T) {
	// A missing file is an empty config.
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil || len(cfg) != 0 {
		t.Fatalf("missing file: cfg=%v err=%v", cfg, err)
	}
	_, dir := testConfig(t)

	// A well-formed file.
	cfg, err = LoadConfig(writeMCP(t, dir, `{"mcpServers":{"fs":{"command":"/bin/echo","args":["--x"],"env":{"A":"b"}}}}`))
	if err != nil || cfg["fs"].Command != "/bin/echo" || cfg["fs"].Args[0] != "--x" || cfg["fs"].Env["A"] != "b" {
		t.Fatalf("cfg=%v err=%v", cfg, err)
	}
	// Bad shapes are refused, naming the problem.
	for _, c := range []struct {
		text, want string
	}{
		{`{"servers":{}}`, `no "mcpServers"`},
		{`{`, `mcp.json`},
		{`{"mcpServers":{"bad__name":{"command":"x"}}}`, `not usable`},
		{`{"mcpServers":{"web_search":{"command":"x"}}}`, `reserved`},
		{`{"mcpServers":{"fs":{}}}`, `no command`},
	} {
		_, err := LoadConfig(writeMCP(t, dir, c.text))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("LoadConfig(%s) err = %v, want it to mention %q", c.text, err, c.want)
		}
	}
}

// newRegistry builds a registry over the fake server.
func newRegistry(t *testing.T) *Registry {
	t.Helper()
	_, dir := testConfig(t)
	path := writeMCP(t, dir, `{"mcpServers":{"fx":{"command":`+strconv.Quote(fakeBin)+`}}}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return NewRegistry(cfg, nil)
}

func TestRefreshListsExposedTools(t *testing.T) {
	r := newRegistry(t)
	defer r.Close()
	exposed, err := r.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range exposed {
		names = append(names, e.Name)
		if e.Server != "fx" {
			t.Fatalf("exposed %+v: server prefix wrong", e)
		}
	}
	if len(names) != 3 || names[0] != "fx__big" || names[1] != "fx__echo" || names[2] != "fx__slow" {
		t.Fatalf("exposed names = %v", names)
	}
	for _, e := range exposed {
		if len(e.Schema) == 0 || !strings.Contains(string(e.Schema), `"type":"object"`) {
			t.Fatalf("%s schema = %s", e.Name, e.Schema)
		}
	}
}

func TestCallEchoAndErrors(t *testing.T) {
	r := newRegistry(t)
	defer r.Close()
	text, isErr, err := r.Call(context.Background(), "fx__echo", json.RawMessage(`{"text":"hello"}`))
	if err != nil || isErr || text != "echo: hello" {
		t.Fatalf("call = %q isErr=%v err=%v", text, isErr, err)
	}
	// A tool the server does not have rides back as an error result.
	text, isErr, err = r.Call(context.Background(), "fx__nope", nil)
	if err != nil || !isErr || !strings.Contains(text, "no such tool") {
		t.Fatalf("unknown tool = %q isErr=%v err=%v", text, isErr, err)
	}
	// A reserved bare name is refused before any server is asked.
	if _, _, err := r.Call(context.Background(), "web_search", nil); err == nil || !strings.Contains(err.Error(), "bad tool name") {
		t.Fatalf("bare reserved name: err=%v", err)
	}
}

func TestCallTimeoutKillsAndRelaunches(t *testing.T) {
	r := newRegistry(t)
	defer r.Close()
	old := CallTimeout
	CallTimeout = 300 * time.Millisecond
	defer func() { CallTimeout = old }()

	if _, _, err := r.Call(context.Background(), "fx__slow", json.RawMessage(`{"ms":5000,"text":"late"}`)); err == nil {
		t.Fatal("the slow call must time out")
	}
	// The timed-out session was killed; the next call relaunches the
	// server fresh and works.
	text, isErr, err := r.Call(context.Background(), "fx__echo", json.RawMessage(`{"text":"again"}`))
	if err != nil || isErr || text != "echo: again" {
		t.Fatalf("after timeout: %q isErr=%v err=%v", text, isErr, err)
	}
}

func TestCancelledCallKillsTheServer(t *testing.T) {
	r := newRegistry(t)
	defer r.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := r.Call(ctx, "fx__slow", json.RawMessage(`{"ms":5000,"text":"late"}`))
		done <- err
	}()
	time.Sleep(150 * time.Millisecond) // let the call start
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the cancelled call must return an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled call did not return")
	}
	// And a fresh call relaunches.
	if _, _, err := r.Call(context.Background(), "fx__echo", json.RawMessage(`{"text":"ok"}`)); err != nil {
		t.Fatalf("relaunch after cancel: %v", err)
	}
}

func TestBigResultCutsNothingHere(t *testing.T) {
	// Truncation is the LOOP's job (64 KiB); the registry passes the text
	// whole. This pins the split: a 70k result comes back full here.
	r := newRegistry(t)
	defer r.Close()
	text, isErr, err := r.Call(context.Background(), "fx__big", json.RawMessage(`{"chars":70000}`))
	if err != nil || isErr || len(text) != 70000 {
		t.Fatalf("big: len=%d isErr=%v err=%v", len(text), isErr, err)
	}
}

// TestChildEnvDropsClientSecrets (review): a tool server never inherits
// the client's API key or local token; everything else passes.
func TestChildEnvDropsClientSecrets(t *testing.T) {
	got := childEnv([]string{"PATH=/bin", "REMOTE_API_KEY=rn_sk_FXSECRET", "REMOTE_LOCAL_TOKEN=tok", "REMOTE_ROUTER=https://x", "HOME=/h"})
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "FXSECRET") || strings.Contains(joined, "REMOTE_LOCAL_TOKEN") {
		t.Fatalf("secrets leaked: %v", got)
	}
	if !strings.Contains(joined, "PATH=/bin") || !strings.Contains(joined, "REMOTE_ROUTER=https://x") {
		t.Fatalf("ordinary variables dropped: %v", got)
	}
}

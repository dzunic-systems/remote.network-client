package ui

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"remote.network/client/internal/agent"
	"remote.network/client/internal/config"
	"remote.network/client/internal/permissions"
	"remote.network/client/internal/tools"
	"remote.network/node/clientproto"
)

var fakeMCPBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakemcp-ui-*")
	if err != nil {
		panic(err)
	}
	fakeMCPBin = filepath.Join(dir, "fakemcp")
	if runtime.GOOS == "windows" {
		fakeMCPBin += ".exe" // exec runs only named executables there
	}
	cmd := exec.Command("go", "build", "-o", fakeMCPBin, "../tools/testdata/fakemcp")
	if out, err := cmd.CombinedOutput(); err != nil {
		panic("building the fake MCP server failed: " + string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// scriptedProxy is the loop's fake local endpoint.
type scriptedProxy struct {
	mu      sync.Mutex
	rounds  []*clientproto.ChatRequest
	scripts []func(round int, req *clientproto.ChatRequest) any
}

func (f *scriptedProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req clientproto.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "body", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.rounds = append(f.rounds, &req)
	idx := len(f.rounds)
	var script func(round int, req *clientproto.ChatRequest) any
	if idx-1 < len(f.scripts) {
		script = f.scripts[idx-1]
	}
	f.mu.Unlock()
	out := map[string]any{
		"id": "x", "object": "chat.completion", "created": 1, "model": "m",
		"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "done"}, "finish_reason": "stop"}},
	}
	if script != nil {
		if v := script(idx, &req); v != nil {
			out = v.(map[string]any)
		}
	}
	// A streaming round gets the OpenAI SSE shape (what the real proxy
	// answers when the loop asks for a stream).
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		chunk := func(delta map[string]any, finish any) {
			choice := map[string]any{"index": 0, "delta": delta, "finish_reason": finish}
			b, _ := json.Marshal(map[string]any{
				"id": "x", "object": "chat.completion.chunk", "created": 1, "model": "m",
				"choices": []map[string]any{choice},
			})
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			flusher.Flush()
		}
		choices := out["choices"].([]map[string]any)
		msg := choices[0]["message"].(map[string]any)
		chunk(map[string]any{"role": "assistant"}, nil)
		if text, _ := msg["content"].(string); text != "" {
			chunk(map[string]any{"content": text}, nil)
		}
		if calls, _ := msg["tool_calls"].([]map[string]any); calls != nil {
			for _, tc := range calls {
				fn := tc["function"].(map[string]any)
				chunk(map[string]any{
					"tool_calls": []map[string]any{{
						"index": 0, "id": tc["id"], "type": "function",
						"function": map[string]any{"name": fn["name"], "arguments": fn["arguments"]},
					}},
				}, nil)
			}
		}
		finish, _ := choices[0]["finish_reason"].(string)
		chunk(map[string]any{}, finish)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}

func toolCallRound(name, args string) map[string]any {
	return map[string]any{
		"id": "x", "object": "chat.completion", "created": 1, "model": "m",
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role": "assistant", "content": "",
				"tool_calls": []map[string]any{{
					"id": "c1", "type": "function",
					"function": map[string]any{"name": name, "arguments": args},
				}},
			},
			"finish_reason": "tool_calls",
		}},
	}
}

// toolHarness is unused — the flow test builds the whole stack itself.

func TestUIChatToolTurnPermissionFlow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("REMOTE_HOME", "")
	dir := filepath.Join(home, ".remote")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"),
		[]byte(`{"mcpServers":{"fx":{"command":`+strconv.Quote(fakeMCPBin)+`}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := tools.LoadConfig(filepath.Join(dir, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(cfg, nil)
	defer reg.Close()
	perms, err := permissions.Load()
	if err != nil {
		t.Fatal(err)
	}

	var sawToolResult string
	proxy := &scriptedProxy{scripts: []func(round int, req *clientproto.ChatRequest) any{
		func(round int, req *clientproto.ChatRequest) any {
			return toolCallRound("fx__echo", `{"text":"hello from the model"}`)
		},
		func(round int, req *clientproto.ChatRequest) any {
			for _, msg := range req.Messages {
				if msg.Role == "tool" {
					sawToolResult = msg.Text()
				}
			}
			return map[string]any{
				"id": "x", "object": "chat.completion", "created": 1, "model": "m",
				"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "the final answer"}, "finish_reason": "stop"}},
			}
		},
	}}
	loop := &agent.Loop{Proxy: proxy, Tools: reg, Perms: perms}
	h := New(Config{
		Dist:     testDist(),
		Settings: func() (config.Settings, error) { return config.Resolve(config.Flags{}) },
		Loop:     loop, Reg: reg, Perms: perms,
		ToolsOn: func() bool { return true },
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	u := strings.TrimPrefix(srv.URL, "http://")

	// The settings row: tools on, and the MCP config is read-only.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/ui/api/settings", nil)
	req.Host = u
	req.Header.Set("Origin", "http://"+u)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var settingsDoc struct {
		ToolsEnabled bool `json:"tools_enabled"`
		Settings     []settingRow
	}
	_ = json.NewDecoder(resp.Body).Decode(&settingsDoc)
	resp.Body.Close()
	if !settingsDoc.ToolsEnabled {
		t.Fatal("tools_enabled = false")
	}
	for _, row := range settingsDoc.Settings {
		if row.Key == "mcp_config" && (!row.Locked || !strings.Contains(row.LockReason, "config set")) {
			t.Fatalf("mcp_config row = %+v, want locked with the config-set reason", row)
		}
	}

	// The tool turn: the permission prompt answers mid-stream.
	type ev struct {
		Type    string
		ID      string
		Text    string
		Name    string
		Content string
	}
	var mu sync.Mutex
	var events []ev
	var permissionID string
	permissionSeen := make(chan struct{}, 1)
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		body := `{"model":"m","messages":[{"role":"user","content":"use the tool"}]}`
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/ui/api/chat", strings.NewReader(body))
		req.Host = u
		req.Header.Set("Origin", "http://"+u)
		req.Header.Set("X-RC-CSRF", h.Token())
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				return
			}
			var e ev
			if json.Unmarshal([]byte(payload), &e) != nil {
				continue
			}
			mu.Lock()
			events = append(events, e)
			id := e.ID
			mu.Unlock()
			if e.Type == "permission" && id != "" {
				permissionID = id
				select {
				case permissionSeen <- struct{}{}:
				default:
				}
			}
		}
	}()

	select {
	case <-permissionSeen:
	case <-time.After(30 * time.Second):
		t.Fatal("no permission prompt arrived")
	}
	mu.Lock()
	id := permissionID
	mu.Unlock()
	decBody := `{"ask_id":"` + id + `","decision":"once"}`
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/ui/api/tools/decide", strings.NewReader(decBody))
	req.Host = u
	req.Header.Set("Origin", "http://"+u)
	req.Header.Set("X-RC-CSRF", h.Token())
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("decide: %d", resp.StatusCode)
	}

	select {
	case <-streamDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the turn never finished")
	}
	if sawToolResult != "echo: hello from the model" {
		t.Fatalf("the model saw %q", sawToolResult)
	}
	mu.Lock()
	defer mu.Unlock()
	var gotPermission, gotResult, gotDone, gotDisclosure bool
	for _, e := range events {
		switch e.Type {
		case "permission":
			gotPermission = true
		case "tool_result":
			gotResult = true
		case "done":
			gotDone = true
			if e.Content != "the final answer" {
				t.Fatalf("done content = %q", e.Content)
			}
		case "disclosure":
			gotDisclosure = true
		}
	}
	if !gotPermission || !gotResult || !gotDone || !gotDisclosure {
		t.Fatalf("events = %+v", events)
	}

	// The tools list names the fake server's tools with their modes.
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/ui/api/tools", nil)
	req.Host = u
	req.Header.Set("Origin", "http://"+u)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var toolsDoc struct {
		Servers    []serverView `json:"servers"`
		Disclosure string       `json:"disclosure"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&toolsDoc)
	resp.Body.Close()
	if len(toolsDoc.Servers) != 1 || toolsDoc.Servers[0].Name != "fx" || len(toolsDoc.Servers[0].Tools) != 3 {
		t.Fatalf("tools doc = %+v", toolsDoc)
	}
	if toolsDoc.Disclosure == "" {
		t.Fatal("no disclosure in the tools list")
	}

	// A mode change lands; an invented name is refused.
	setMode := func(name, mode string) int {
		b := `{"name":"` + name + `","mode":"` + mode + `"}`
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/ui/api/tools/mode", strings.NewReader(b))
		req.Host = u
		req.Header.Set("Origin", "http://"+u)
		req.Header.Set("X-RC-CSRF", h.Token())
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := setMode("fx__echo", "deny"); code != http.StatusOK {
		t.Fatalf("set deny: %d", code)
	}
	if perms.Mode("fx__echo") != permissions.Deny {
		t.Fatalf("mode = %s", perms.Mode("fx__echo"))
	}
	if code := setMode("other__nope", "allow"); code != http.StatusNotFound {
		t.Fatalf("invented tool: %d", code)
	}
	if code := setMode("fx__echo", "always"); code != http.StatusBadRequest {
		t.Fatalf("bad mode: %d", code)
	}
}

func TestUIChatOffWhenToolsDisabled(t *testing.T) {
	h := newTestUI(t, config.Flags{}) // no loop wired
	rec := do(h, "POST", "/ui/api/chat", map[string]string{
		"X-RC-CSRF": h.Token(), "Content-Type": "application/json",
		"Origin": "http://example.com",
	}, `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("chat with the loop off: %d", rec.Code)
	}
}

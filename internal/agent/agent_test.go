package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"remote.network/client/internal/permissions"
	"remote.network/client/internal/tools"
	"remote.network/node/clientproto"
)

var fakeBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakemcp-agent-*")
	if err != nil {
		panic(err)
	}
	fakeBin = filepath.Join(dir, "fakemcp")
	cmd := exec.Command("go", "build", "-o", fakeBin, "../tools/testdata/fakemcp")
	if out, err := cmd.CombinedOutput(); err != nil {
		panic("building the fake MCP server failed: " + string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// proxyScript scripts the fake local endpoint: one function per round,
// receiving the request the LOOP built (so the tests assert exactly what
// the model would see). The return is a completion JSON value, or a
// rawSSE for a streamed answer.
type proxyScript func(round int, req *clientproto.ChatRequest) any

// rawSSE makes the fake proxy answer with literal SSE bytes.
type rawSSE string

type fakeProxy struct {
	mu      sync.Mutex
	rounds  []*clientproto.ChatRequest // every round's parsed request
	scripts []proxyScript
	headers map[string]string // echoed onto every response (the path header)
}

func (f *fakeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req clientproto.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "body", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.rounds = append(f.rounds, &req)
	idx := len(f.rounds)
	var script proxyScript
	if idx-1 < len(f.scripts) {
		script = f.scripts[idx-1]
	} else if len(f.scripts) > 0 {
		script = f.scripts[len(f.scripts)-1]
	}
	f.mu.Unlock()

	var out any
	if script != nil {
		out = script(idx, &req)
	}
	if out == nil {
		out = completion("done", nil)
	}
	if sse, ok := out.(rawSSE); ok {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
		return
	}
	b, _ := json.Marshal(out)
	for k, v := range f.headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func completion(text string, calls []map[string]any) map[string]any {
	msg := map[string]any{"role": "assistant", "content": text}
	if calls != nil {
		msg["tool_calls"] = calls
	}
	return map[string]any{
		"id": "chatcmpl-x", "object": "chat.completion", "created": 1, "model": "m",
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finishOf(calls)}},
	}
}

func finishOf(calls []map[string]any) string {
	if len(calls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

func callEntry(id, name, args string) map[string]any {
	return map[string]any{
		"id": id, "type": "function",
		"function": map[string]any{"name": name, "arguments": args},
	}
}

// harness builds a loop over a scripted proxy, the fake MCP server, and
// a fresh permissions store.
type harness struct {
	loop   *Loop
	proxy  *fakeProxy
	reg    *tools.Registry
	perms  *permissions.Store
	events []Event
	close  func()
}

func newHarness(t *testing.T, scripts ...proxyScript) *harness {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("REMOTE_HOME", "")
	dir := filepath.Join(home, ".remote")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	mcpPath := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(mcpPath, []byte(`{"mcpServers":{"fx":{"command":"`+fakeBin+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := tools.LoadConfig(mcpPath)
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(cfg, nil)
	perms, err := permissions.Load()
	if err != nil {
		t.Fatal(err)
	}
	fx := &fakeProxy{scripts: scripts}
	h := &harness{
		proxy: fx, reg: reg, perms: perms,
		loop: &Loop{Proxy: fx, Tools: reg, Perms: perms},
	}
	h.close = reg.Close
	t.Cleanup(func() { reg.Close() })
	return h
}

// run drives one turn and returns the output and the event stream.
func (h *harness) run(t *testing.T, ask Asker) (TurnOutput, []Event, error) {
	t.Helper()
	out, err := h.loop.Turn(context.Background(), TurnInput{
		Model:    "m",
		Messages: []clientproto.Message{{Role: "user", Content: json.RawMessage(`"do it"`)}},
		Stream:   false, Ask: ask,
	}, func(ev Event) error {
		h.events = append(h.events, ev)
		return nil
	})
	return out, h.events, err
}

func TestPlainTurnNoTools(t *testing.T) {
	// No registry at all: the loop answers once and carries no tools.
	fx := &fakeProxy{scripts: []proxyScript{func(round int, req *clientproto.ChatRequest) any {
		if len(req.Tools) != 0 {
			t.Errorf("round %d carries tools with no registry", round)
		}
		return completion("the answer", nil)
	}}}
	loop := &Loop{Proxy: fx}
	out, err := loop.Turn(context.Background(), TurnInput{
		Model:    "m",
		Messages: []clientproto.Message{{Role: "user", Content: json.RawMessage(`"do it"`)}},
	}, func(ev Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "the answer" || out.Rounds != 1 {
		t.Fatalf("out = %+v", out)
	}
}

func TestAllowRunsToolAndRoundTwoAnswers(t *testing.T) {
	var roundOneTools []string
	var sawToolResult string
	h := newHarness(t,
		func(round int, req *clientproto.ChatRequest) any {
			if round == 1 {
				for _, tl := range req.Tools {
					roundOneTools = append(roundOneTools, tl.Function.Name)
				}
			}
			return completion("", []map[string]any{callEntry("c1", "fx__echo", `{"text":"hi"}`)})
		},
		func(round int, req *clientproto.ChatRequest) any {
			for _, msg := range req.Messages {
				if msg.Role == "tool" && msg.ToolCallID == "c1" {
					sawToolResult = msg.Text()
				}
			}
			return completion("used the tool", nil)
		},
	)
	defer h.close()
	if err := h.perms.Set("fx__echo", permissions.Allow); err != nil {
		t.Fatal(err)
	}
	out, _, err := h.run(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "used the tool" || out.Rounds != 2 {
		t.Fatalf("out = %+v", out)
	}
	// The exposed set is exactly what the server lists — nothing invented,
	// nothing dropped (the fake offers echo, slow and big).
	if len(roundOneTools) != 3 {
		t.Fatalf("exposed tools = %v", roundOneTools)
	}
	if sawToolResult != "echo: hi" {
		t.Fatalf("the model saw the tool result %q", sawToolResult)
	}
}

func TestAskOnceRunsAlwaysPersistsDenyRefuses(t *testing.T) {
	t.Run("once", func(t *testing.T) {
		h := newHarness(t,
			func(round int, _ *clientproto.ChatRequest) any {
				return completion("", []map[string]any{callEntry("c1", "fx__echo", `{"text":"x"}`)})
			},
			func(round int, req *clientproto.ChatRequest) any {
				return completion("ran", nil)
			},
		)
		defer h.close()
		asks := 0
		asker := askerFunc(func(ctx context.Context, req AskRequest) (Decision, error) {
			asks++
			return Once, nil
		})
		out, _, err := h.run(t, asker)
		if err != nil || out.Text != "ran" || asks != 1 {
			t.Fatalf("out=%+v asks=%d err=%v", out, asks, err)
		}
		if h.perms.Mode("fx__echo") != permissions.Ask {
			t.Fatalf("mode after allow-once = %s, want ask", h.perms.Mode("fx__echo"))
		}
	})

	t.Run("always persists", func(t *testing.T) {
		h := newHarness(t,
			func(round int, _ *clientproto.ChatRequest) any {
				return completion("", []map[string]any{callEntry("c1", "fx__echo", `{"text":"x"}`)})
			},
			func(round int, _ *clientproto.ChatRequest) any { return completion("ran", nil) },
		)
		defer h.close()
		asks := 0
		asker := askerFunc(func(ctx context.Context, req AskRequest) (Decision, error) {
			asks++
			return Always, nil
		})
		if _, _, err := h.run(t, asker); err != nil {
			t.Fatal(err)
		}
		if asks != 1 || h.perms.Mode("fx__echo") != permissions.Allow {
			t.Fatalf("asks=%d mode=%s, want one ask and allow persisted", asks, h.perms.Mode("fx__echo"))
		}
		// A second turn never asks.
		h.proxy.mu.Lock()
		h.proxy.rounds = nil
		h.proxy.mu.Unlock()
		if _, _, err := h.run(t, asker); err != nil {
			t.Fatal(err)
		}
		if asks != 1 {
			t.Fatalf("the second turn asked again (%d)", asks)
		}
	})

	t.Run("deny returns the tool result", func(t *testing.T) {
		var sawResult string
		h := newHarness(t,
			func(round int, _ *clientproto.ChatRequest) any {
				return completion("", []map[string]any{callEntry("c1", "fx__echo", `{"text":"x"}`)})
			},
			func(round int, req *clientproto.ChatRequest) any {
				for _, msg := range req.Messages {
					if msg.Role == "tool" {
						sawResult = msg.Text()
					}
				}
				return completion("noted", nil)
			},
		)
		defer h.close()
		out, events, err := h.run(t, askerFunc(func(ctx context.Context, req AskRequest) (Decision, error) {
			return Deny, nil
		}))
		if err != nil || out.Text != "noted" {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		if sawResult != "denied by the user" {
			t.Fatalf("the model saw %q", sawResult)
		}
		var denied bool
		for _, ev := range events {
			if ev.Type == "tool_result" && ev.Text == "denied by the user" {
				denied = true
			}
		}
		if !denied {
			t.Fatalf("no denied tool_result event: %+v", events)
		}
	})

	t.Run("no asker denies", func(t *testing.T) {
		h := newHarness(t,
			func(round int, _ *clientproto.ChatRequest) any {
				return completion("", []map[string]any{callEntry("c1", "fx__echo", `{"text":"x"}`)})
			},
			func(round int, _ *clientproto.ChatRequest) any { return completion("kept going", nil) },
		)
		defer h.close()
		out, _, err := h.run(t, nil) // ask mode, no asker
		if err != nil || out.Text != "kept going" {
			t.Fatalf("out=%+v err=%v", out, err)
		}
	})

	t.Run("deny mode never asks", func(t *testing.T) {
		h := newHarness(t,
			func(round int, _ *clientproto.ChatRequest) any {
				return completion("", []map[string]any{callEntry("c1", "fx__echo", `{"text":"x"}`)})
			},
			func(round int, _ *clientproto.ChatRequest) any { return completion("refused", nil) },
		)
		defer h.close()
		if err := h.perms.Set("fx__echo", permissions.Deny); err != nil {
			t.Fatal(err)
		}
		asks := 0
		out, _, err := h.run(t, askerFunc(func(ctx context.Context, req AskRequest) (Decision, error) {
			asks++
			return Once, nil
		}))
		if err != nil || out.Text != "refused" || asks != 0 {
			t.Fatalf("out=%+v asks=%d err=%v", out, asks, err)
		}
	})
}

func TestRoundCap(t *testing.T) {
	h := newHarness(t, func(round int, _ *clientproto.ChatRequest) any {
		return completion("", []map[string]any{callEntry(fmt.Sprintf("c%d", round), "fx__echo", `{"text":"x"}`)})
	})
	defer h.close()
	out, events, err := h.run(t, askerFunc(func(ctx context.Context, req AskRequest) (Decision, error) {
		return Once, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "" || out.Rounds != MaxRounds {
		t.Fatalf("out = %+v, want %d rounds and no text", out, MaxRounds)
	}
	var stopped bool
	for _, ev := range events {
		if ev.Type == "error" && strings.Contains(ev.Message, "10 rounds") {
			stopped = true
		}
	}
	if !stopped {
		t.Fatalf("no cap error event: %+v", events)
	}
}

func TestResultTruncatedAt64KiB(t *testing.T) {
	var sawResult string
	h := newHarness(t,
		func(round int, _ *clientproto.ChatRequest) any {
			return completion("", []map[string]any{callEntry("c1", "fx__big", `{"chars":70000}`)})
		},
		func(round int, req *clientproto.ChatRequest) any {
			for _, msg := range req.Messages {
				if msg.Role == "tool" {
					sawResult = msg.Text()
				}
			}
			return completion("saw it", nil)
		},
	)
	defer h.close()
	if err := h.perms.Set("fx__big", permissions.Allow); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.run(t, nil); err != nil {
		t.Fatal(err)
	}
	if len(sawResult) > MaxResultBytes+100 {
		t.Fatalf("result = %d bytes, want ≤ 64 KiB + marker", len(sawResult))
	}
	if !strings.Contains(sawResult, "truncated") {
		t.Fatalf("no truncation marker: …%s", sawResult[len(sawResult)-60:])
	}
	var resultEvent *Event
	for i, ev := range h.events {
		if ev.Type == "tool_result" {
			e := h.events[i]
			resultEvent = &e
		}
	}
	if resultEvent == nil || resultEvent.Bytes != MaxResultBytes+len("\n…[truncated at 64 KiB]") {
		t.Fatalf("tool_result event = %+v", resultEvent)
	}
}

func TestToolTimeoutContinuesTheTurn(t *testing.T) {
	old := tools.CallTimeout
	tools.CallTimeout = 250 * time.Millisecond
	defer func() { tools.CallTimeout = old }()

	h := newHarness(t,
		func(round int, _ *clientproto.ChatRequest) any {
			return completion("", []map[string]any{callEntry("c1", "fx__slow", `{"ms":5000,"text":"late"}`)})
		},
		func(round int, req *clientproto.ChatRequest) any {
			for _, msg := range req.Messages {
				if msg.Role == "tool" && !strings.Contains(msg.Text(), "tool failed") {
					t.Errorf("the model saw %q, want the failure text", msg.Text())
				}
			}
			return completion("carried on", nil)
		},
	)
	defer h.close()
	out, events, err := h.run(t, askerFunc(func(ctx context.Context, req AskRequest) (Decision, error) {
		return Once, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "carried on" {
		t.Fatalf("out = %+v", out)
	}
	var failed bool
	for _, ev := range events {
		if ev.Type == "tool_result" && !ev.OK && strings.Contains(ev.Text, "tool failed") {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("no failed tool_result event: %+v", events)
	}
}

func TestDisclosureOncePerServer(t *testing.T) {
	var disclosures int
	h := newHarness(t,
		func(round int, _ *clientproto.ChatRequest) any {
			return completion("", []map[string]any{callEntry("c1", "fx__echo", `{"text":"x"}`)})
		},
		func(round int, _ *clientproto.ChatRequest) any {
			return completion("", []map[string]any{callEntry("c2", "fx__big", `{"chars":10}`)})
		},
		func(round int, _ *clientproto.ChatRequest) any { return completion("fin", nil) },
	)
	defer h.close()
	if _, _, err := h.run(t, askerFunc(func(ctx context.Context, req AskRequest) (Decision, error) {
		return Once, nil
	})); err != nil {
		t.Fatal(err)
	}
	for _, ev := range h.events {
		if ev.Type == "disclosure" {
			disclosures++
			if ev.Text != Disclosure {
				t.Fatalf("disclosure text = %q", ev.Text)
			}
		}
	}
	if disclosures != 1 {
		t.Fatalf("%d disclosure events, want exactly one per server", disclosures)
	}
}

func TestStreamingRoundEmitsDeltas(t *testing.T) {
	// A streaming fake: the proxy answers with the OpenAI SSE shape.
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"lo"}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	fx := &fakeProxy{headers: map[string]string{}}
	fx.scripts = append(fx.scripts, func(round int, req *clientproto.ChatRequest) any {
		return rawSSE(sse)
	})
	reg := tools.NewRegistry(tools.Config{}, nil)
	defer reg.Close()
	loop := &Loop{Proxy: fx, Tools: reg}
	var text strings.Builder
	out, err := loop.Turn(context.Background(), TurnInput{
		Model: "m", Messages: []clientproto.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Stream: true,
	}, func(ev Event) error {
		if ev.Type == "delta" {
			text.WriteString(ev.Content)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "Hello" || text.String() != "Hello" {
		t.Fatalf("out=%q deltas=%q", out.Text, text.String())
	}
}

// askerFunc adapts a function to Asker.
type askerFunc func(ctx context.Context, req AskRequest) (Decision, error)

func (f askerFunc) Ask(ctx context.Context, req AskRequest) (Decision, error) { return f(ctx, req) }

// TestRoundStreamsLive (review): the first content delta reaches the
// caller while the round is still generating. The old recorder buffered
// the whole answer, so a long or thinking answer looked hung until it
// ended.
func TestRoundStreamsLive(t *testing.T) {
	release := make(chan struct{})
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Remote-Path", "direct")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		http.NewResponseController(w).Flush()
		<-release // the rest of the answer waits until the test saw "first"
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\" second\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	l := &Loop{Proxy: proxy}
	sawFirst := make(chan struct{})
	var once sync.Once
	done := make(chan TurnOutput, 1)
	go func() {
		out, err := l.Turn(context.Background(), TurnInput{Model: "m", Stream: true,
			Messages: []clientproto.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}}},
			func(ev Event) error {
				if ev.Type == "delta" && ev.Content == "first" {
					once.Do(func() { close(sawFirst) })
				}
				return nil
			})
		if err != nil {
			t.Error(err)
		}
		done <- out
	}()
	select {
	case <-sawFirst:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("the first delta did not arrive before the round finished")
	}
	close(release)
	if out := <-done; out.Text != "first second" || out.Path != "direct" {
		t.Fatalf("turn = %+v", out)
	}
}

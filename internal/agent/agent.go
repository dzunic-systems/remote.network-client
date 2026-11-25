// Package agent is the local tool loop: the client's OWN chat surfaces
// (the web UI and `remote-client chat`) run it; the OpenAI-compatible
// proxy never does. Each round is one new chat request through the local
// endpoint — direct to a node when a direct path exists, through the
// router otherwise — carrying the exposed MCP tools plus the visible
// history. When the model calls a tool, the user's permission decides
// (allow/ask/deny; ask blocks on the asker), the tool runs locally, and
// the result rides back as a tool message.
//
// The model's tool calls are untrusted input: names are matched against
// the registry's exposed names only, arguments are forwarded to the
// configured server untouched, results are truncated before they enter
// history, and nothing a tool returns is ever executed.
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"remote.network/client/internal/permissions"
	"remote.network/client/internal/tools"
	"remote.network/node/clientproto"
)

// MaxRounds is the loop bound: at most ten tool rounds per turn.
const MaxRounds = 10

// MaxResultBytes bounds one tool result entering history (and the
// model's window): 64 KiB, then a marker.
const MaxResultBytes = 64 << 10

// Disclosure is the privacy boundary, shown once per server when one of
// its tools first runs and on the settings page: local file contents
// leave the machine to wherever the model is served.
const Disclosure = "tool results are sent to the node that serves the model — a third party's machine unless it is your own node."

// Event is one turn event. The UI renders tool arguments and results as
// text only; nothing here is ever HTML.
type Event struct {
	Type      string `json:"type"` // round|delta|tool_call|permission|tool_result|disclosure|done|error
	Round     int    `json:"round,omitempty"`
	Content   string `json:"content,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
	ID        string `json:"id,omitempty"` // tool_call id, or the permission ask id on permission events
	Name      string `json:"name,omitempty"`
	Server    string `json:"server,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	OK        bool   `json:"ok,omitempty"`
	MS        int64  `json:"ms,omitempty"`
	Bytes     int    `json:"bytes,omitempty"`
	Text      string `json:"text,omitempty"`
	Path      string `json:"path,omitempty"` // the round's route: direct | router
	Model     string `json:"model,omitempty"`
	Finish    string `json:"finish,omitempty"` // the final round's finish_reason (length detection in the UI)
	Message   string `json:"message,omitempty"`
}

// Decision is the user's answer to a permission ask.
type Decision string

// Decisions.
const (
	Once   Decision = "once"
	Always Decision = "always"
	Deny   Decision = "deny"
)

// Asker resolves an ask-mode permission. Implementations block until the
// user answers (a TTY prompt, the UI's inline prompt); with no way to
// ask, callers pass nil — ask then denies.
type Asker interface {
	Ask(ctx context.Context, req AskRequest) (Decision, error)
}

// AskRequest is one permission prompt.
type AskRequest struct {
	Server    string
	Tool      string
	Name      string // the exposed name
	Arguments string // pretty-printed JSON
}

// Loop drives one turn.
type Loop struct {
	// Proxy is the local endpoint's handler (in-process requests; the
	// Host is 127.0.0.1, so the loopback gate passes).
	Proxy http.Handler
	// LocalToken, when the listener requires one, rides the in-process
	// requests too.
	LocalToken string
	// Tools is the MCP registry (nil = no tools; the loop answers once).
	Tools *tools.Registry
	// Perms is the permission store.
	Perms *permissions.Store
	// Ask resolves ask-mode prompts; nil means ask denies.
	Ask Asker
	Log *slog.Logger

	disclosedMu sync.Mutex
	disclosed   map[string]bool // servers whose disclosure already went out
}

// TurnInput is one turn's fixed shape: the visible history and the
// per-turn controls.
type TurnInput struct {
	Model     string
	Messages  []clientproto.Message
	Effort    string
	WebSearch bool
	Stream    bool // deliver content as deltas
	// Ask overrides the loop's asker for this turn (the UI binds one per
	// request so the permission prompt rides that turn's stream). Nil
	// falls back to Loop.Ask; neither means ask denies.
	Ask Asker
}

// TurnOutput is what the turn ended with.
type TurnOutput struct {
	Text      string
	Reasoning string
	Model     string
	Path      string // the last round's route
	Rounds    int
}

// Turn runs the loop, emitting events as it goes. A nil error means the
// turn completed (the model answered or the round cap ended it); a
// context cancellation ends the turn with ctx.Err() after killing the
// in-flight tool call.
func (l *Loop) Turn(ctx context.Context, in TurnInput, emit func(Event) error) (TurnOutput, error) {
	if l.Log == nil {
		l.Log = slog.Default()
	}
	if l.disclosed == nil {
		l.disclosed = map[string]bool{}
	}
	asker := in.Ask
	if asker == nil {
		asker = l.Ask
	}
	var out TurnOutput
	out.Model = in.Model
	history := make([]clientproto.Message, len(in.Messages))
	copy(history, in.Messages)

	var exposed []tools.Exposed
	if l.Tools != nil && !l.Tools.Empty() {
		var err error
		exposed, err = l.Tools.Refresh(ctx)
		if err != nil && len(exposed) == 0 {
			return out, fmt.Errorf("the local tool servers could not start: %w", err)
		}
		if err != nil {
			l.Log.Warn("some tool servers did not start — the healthy ones still serve this turn", "err", err)
		}
	}

	for round := 1; ; round++ {
		if round > MaxRounds {
			emit(Event{Type: "error", Message: fmt.Sprintf("the tool loop stopped after %d rounds without a final answer", MaxRounds)})
			return out, nil
		}
		req := clientproto.ChatRequest{
			Model:    in.Model,
			Messages: history,
			Stream:   in.Stream,
		}
		if in.Effort != "" {
			req.ReasoningEffort = in.Effort
		}
		if in.WebSearch {
			req.WebSearch = true
		}
		if len(exposed) > 0 {
			req.Tools = toolDefs(exposed)
			req.ToolChoice = json.RawMessage(`"auto"`)
		}
		if err := emit(Event{Type: "round", Round: round}); err != nil {
			return out, err
		}

		resp, err := l.round(ctx, &req)
		if err != nil {
			return out, err
		}
		if resp.Code != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			return out, fmt.Errorf("round %d failed (%d): %s", round, resp.Code, errorText(b))
		}
		out.Path = resp.Header.Get("X-Remote-Path")
		_ = emit(Event{Type: "round", Round: round, Path: out.Path})

		var content, reasoning, finish string
		var calls []clientproto.ToolCall
		if in.Stream {
			content, reasoning, finish, calls, err = parseStream(resp.Body, emit)
		} else {
			content, reasoning, finish, calls, err = parseBody(resp.Body)
		}
		resp.Body.Close()
		if err != nil {
			return out, err
		}
		out.Rounds = round
		if len(calls) == 0 {
			out.Text, out.Reasoning = content, reasoning
			_ = emit(Event{Type: "done", Content: content, Reasoning: reasoning, Path: out.Path, Model: in.Model, Finish: finish})
			return out, nil
		}
		if finish != "" && finish != "tool_calls" {
			// A model that declares stop AND calls tools is talking anyway;
			// the calls decide. Anything else rides the done event when the
			// loop ends without calls.
			l.Log.Debug("round ended with calls and a finish reason", "finish", finish)
		}

		// The model wants tools. The assistant turn joins history with all
		// its calls (the backends reject orphaned tool messages).
		history = append(history, clientproto.Message{
			Role: "assistant", Content: jsonRawString(content), ToolCalls: calls,
		})

		for _, call := range calls {
			result, ran, rerr := l.runTool(ctx, asker, emit, call)
			if rerr != nil {
				if ctx.Err() != nil {
					return out, ctx.Err()
				}
				return out, rerr
			}
			_ = ran
			history = append(history, clientproto.Message{
				Role:       "tool",
				ToolCallID: call.ID,
				Content:    jsonRawString(result),
			})
		}
	}
}

// runTool resolves the permission, runs one call, and emits the events.
// The returned string is what rides back to the model as the tool result
// — including refusals ("denied by the user") and failures, which the
// model is meant to see.
func (l *Loop) runTool(ctx context.Context, asker Asker, emit func(Event) error, call clientproto.ToolCall) (string, bool, error) {
	name := call.Function.Name
	server, tool, ok := tools.SplitName(name)
	if !ok || l.Tools == nil {
		return "no such local tool: " + name, false, nil
	}
	_ = emit(Event{Type: "tool_call", ID: call.ID, Name: name, Server: server, Tool: tool, Arguments: call.Function.Arguments})

	mode := permissions.Ask
	if l.Perms != nil {
		mode = l.Perms.Mode(name)
	}
	switch mode {
	case permissions.Allow:
	case permissions.Deny:
		_ = emit(Event{Type: "tool_result", ID: call.ID, Name: name, Server: server, Tool: tool,
			Text: "denied by the user", Bytes: len("denied by the user")})
		return "denied by the user", false, nil
	default: // ask
		if asker == nil {
			_ = emit(Event{Type: "tool_result", ID: call.ID, Name: name, Server: server, Tool: tool,
				Text: "denied by the user", Bytes: len("denied by the user")})
			return "denied by the user", false, nil
		}
		d, err := asker.Ask(ctx, AskRequest{Server: server, Tool: tool, Name: name, Arguments: call.Function.Arguments})
		if err != nil {
			if ctx.Err() != nil {
				return "", false, ctx.Err()
			}
			d = Deny // a broken asker denies; the turn continues
		}
		switch d {
		case Always:
			if l.Perms != nil {
				if serr := l.Perms.Set(name, permissions.Allow); serr != nil {
					l.Log.Warn("could not persist the allow", "tool", name, "err", serr)
				}
			}
		case Deny:
			_ = emit(Event{Type: "tool_result", ID: call.ID, Name: name, Server: server, Tool: tool,
				Text: "denied by the user", Bytes: len("denied by the user")})
			return "denied by the user", false, nil
		}
	}

	// The disclosure goes out once per server, before its first run.
	l.disclosedMu.Lock()
	first := !l.disclosed[server]
	l.disclosed[server] = true
	l.disclosedMu.Unlock()
	if first {
		_ = emit(Event{Type: "disclosure", Server: server, Text: Disclosure})
	}

	start := time.Now()
	text, isErr, err := l.Tools.Call(ctx, name, json.RawMessage(call.Function.Arguments))
	ms := time.Since(start).Milliseconds()
	if ctx.Err() != nil {
		l.Tools.Kill(server) // the cancelled turn kills the in-flight call
		return "", false, ctx.Err()
	}
	if err != nil {
		l.Tools.Kill(server)
		text = "tool failed: " + err.Error()
		isErr = true
	}
	if isErr && text == "" {
		text = "the tool reported an error"
	}
	truncated := ""
	if len(text) > MaxResultBytes {
		text = text[:MaxResultBytes] + "\n…[truncated at 64 KiB]"
		truncated = " (truncated)"
	}
	l.Log.Info("tool run", "tool", name, "ms", ms, "bytes", len(text), "error", isErr)
	_ = emit(Event{Type: "tool_result", ID: call.ID, Name: name, Server: server, Tool: tool,
		OK: !isErr, MS: ms, Bytes: len(text), Text: text, Message: truncated})
	return text, true, nil
}

// round posts one chat request to the local endpoint in-process and
// returns as soon as the proxy commits its status: the body is a pipe the
// caller reads WHILE the node generates (a recorder would buffer the whole
// answer, so a long or thinking answer would look hung until it ended).
// The caller must Close the body; closing early makes the proxy's next
// write fail, which it treats as a consumer gone.
func (l *Loop) round(ctx context.Context, req *clientproto.ChatRequest) (*roundResp, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://127.0.0.1/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if l.LocalToken != "" {
		httpReq.Header.Set("X-Local-Token", l.LocalToken)
	}
	pr, pw := io.Pipe()
	rw := &pipeWriter{hdr: http.Header{}, pw: pw, started: make(chan struct{})}
	go func() {
		l.Proxy.ServeHTTP(rw, httpReq)
		rw.commit(http.StatusOK) // a handler that wrote nothing still answers
		_ = pw.Close()
	}()
	select {
	case <-rw.started:
	case <-ctx.Done():
		_ = pr.CloseWithError(ctx.Err())
		return nil, ctx.Err()
	}
	return &roundResp{Code: rw.code, Header: rw.snap, Body: pr}, nil
}

// roundResp is one round's committed response: status, the headers as
// committed, and the streaming body.
type roundResp struct {
	Code   int
	Header http.Header
	Body   io.ReadCloser
}

// pipeWriter is the in-process ResponseWriter: headers and status are
// captured at commit, the body streams through the pipe. Flush is a no-op
// (every Write already reaches the reader).
type pipeWriter struct {
	hdr     http.Header
	snap    http.Header
	code    int
	pw      *io.PipeWriter
	once    sync.Once
	started chan struct{}
}

func (w *pipeWriter) Header() http.Header { return w.hdr }

func (w *pipeWriter) commit(code int) {
	w.once.Do(func() {
		w.code = code
		w.snap = w.hdr.Clone()
		close(w.started)
	})
}

func (w *pipeWriter) WriteHeader(code int) { w.commit(code) }

func (w *pipeWriter) Write(b []byte) (int, error) {
	w.commit(http.StatusOK)
	return w.pw.Write(b)
}

func (w *pipeWriter) Flush() {}

// toolDefs renders the exposed tools in the OpenAI shape.
func toolDefs(exposed []tools.Exposed) []clientproto.Tool {
	out := make([]clientproto.Tool, 0, len(exposed))
	for _, e := range exposed {
		out = append(out, clientproto.Tool{
			Type: "function",
			Function: clientproto.ToolFn{
				Name:        e.Name,
				Description: e.Description,
				Parameters:  e.Schema,
			},
		})
	}
	return out
}

// ── response parsing ────────────────────────────────────────────────

// parseBody reads one non-streaming completion.
func parseBody(r io.Reader) (content, reasoning, finish string, calls []clientproto.ToolCall, err error) {
	b, err := io.ReadAll(io.LimitReader(r, 64<<20))
	if err != nil {
		return "", "", "", nil, err
	}
	var doc struct {
		Choices []struct {
			Message struct {
				Content          string                 `json:"content"`
				ReasoningContent string                 `json:"reasoning_content"`
				ToolCalls        []clientproto.ToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return "", "", "", nil, fmt.Errorf("the answer did not parse: %w", err)
	}
	if doc.Error != nil && doc.Error.Message != "" {
		return "", "", "", nil, errors.New(doc.Error.Message)
	}
	if len(doc.Choices) == 0 {
		return "", "", "", nil, errors.New("the answer carried no choices")
	}
	c := doc.Choices[0]
	return c.Message.Content, c.Message.ReasoningContent, c.FinishReason, c.Message.ToolCalls, nil
}

// parseStream reads one streamed completion, emitting content deltas as
// they arrive and assembling the tool calls from their fragments.
func parseStream(r io.Reader, emit func(Event) error) (content, reasoning, finish string, calls []clientproto.ToolCall, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	type frag struct {
		id, name, args string
	}
	frags := map[int]*frag{}
	order := []int{}
	handle := func(payload string) error {
		if payload == "[DONE]" {
			return nil
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			return nil // keepalive or unparseable frame
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			return errors.New(chunk.Error.Message)
		}
		for _, ch := range chunk.Choices {
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
			d := ch.Delta
			if d.Content != "" {
				content += d.Content
				if err := emit(Event{Type: "delta", Content: d.Content}); err != nil {
					return err
				}
			}
			if d.ReasoningContent != "" {
				reasoning += d.ReasoningContent
				if err := emit(Event{Type: "delta", Reasoning: d.ReasoningContent}); err != nil {
					return err
				}
			}
			for _, tc := range d.ToolCalls {
				f := frags[tc.Index]
				if f == nil {
					f = &frag{}
					frags[tc.Index] = f
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					f.id = tc.ID
				}
				if tc.Function.Name != "" {
					f.name += tc.Function.Name
				}
				f.args += tc.Function.Arguments
			}
		}
		return nil
	}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		if err := handle(strings.TrimSpace(strings.TrimPrefix(line, "data:"))); err != nil {
			return "", "", "", nil, err
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return "", "", "", nil, err
	}
	sortInts(order)
	for _, i := range order {
		f := frags[i]
		calls = append(calls, clientproto.ToolCall{
			ID:   f.id,
			Type: "function",
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: f.name, Arguments: f.args},
		})
	}
	return content, reasoning, finish, calls, nil
}

func sortInts(xs []int) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

// errorText pulls the router's error message out of an error body.
func errorText(b []byte) string {
	var doc struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &doc) == nil && doc.Error.Message != "" {
		return doc.Error.Message
	}
	return strings.TrimSpace(string(b))
}

// jsonRawString marshals s as a JSON string (message Content rides as a
// JSON string, not bare text).
func jsonRawString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

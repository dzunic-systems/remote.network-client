// The local tools' UI API: the chat turns that run the agent loop
// (POST /ui/api/chat, SSE), the tools list with its permission modes
// (GET /ui/api/tools, POST /ui/api/tools/mode), and the permission
// decisions that unblock an ask (POST /ui/api/tools/decide).
//
// The reviewer's rule holds here by construction: no surface takes a
// server command. Servers are added by editing mcp.json (or `config
// set`); the page may only flip a tool's allow/ask/deny mode.
package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"remote.network/client/internal/agent"
	"remote.network/client/internal/permissions"
	"remote.network/client/internal/tools"
	"remote.network/node/clientproto"
)

// toolsOn is the page's "should turns use the loop" fact.
func (h *Handler) toolsOn() bool {
	return h.cfg.ToolsOn != nil && h.cfg.ToolsOn()
}

// ── the asker: pending asks the page answers ────────────────────────

// turnAsker is one chat turn's permission resolver. It delivers the
// prompt to the page as a permission event on that turn's stream and
// blocks on the decision (or the turn's cancellation).
type turnAsker struct {
	h    *Handler
	emit func(agent.Event) error
}

// pendingAsk is one open permission prompt: the channel the decision
// resolves and the exposed tool name "always" persists under.
type pendingAsk struct {
	ch   chan agent.Decision
	name string // the exposed tool name
}

// Ask implements agent.Asker.
func (t *turnAsker) Ask(ctx context.Context, req agent.AskRequest) (agent.Decision, error) {
	ch := make(chan agent.Decision, 1)
	id := randomToken()[:24]
	t.h.asksMu.Lock()
	t.h.asks[id] = &pendingAsk{ch: ch, name: req.Name}
	t.h.asksMu.Unlock()
	defer func() {
		t.h.asksMu.Lock()
		delete(t.h.asks, id)
		t.h.asksMu.Unlock()
	}()
	// The prompt shows the tool, its arguments and the server (arguments
	// render as text only — they are model output).
	err := t.emit(agent.Event{Type: "permission", ID: id, Server: req.Server, Tool: req.Tool, Name: req.Name, Arguments: req.Arguments})
	if err != nil {
		return agent.Deny, err
	}
	select {
	case <-ctx.Done():
		return agent.Deny, ctx.Err()
	case d := <-ch:
		return d, nil
	}
}

// decide is POST /ui/api/tools/decide: one pending ask's answer.
// "always" also persists the mode.
func (h *Handler) decide(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AskID    string `json:"ask_id"`
		Decision string `json:"decision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_request", "malformed body")
		return
	}
	var d agent.Decision
	switch agent.Decision(body.Decision) {
	case agent.Once:
		d = agent.Once
	case agent.Always:
		d = agent.Always
	case agent.Deny:
		d = agent.Deny
	default:
		h.writeError(w, http.StatusBadRequest, "invalid_request", `decision must be "once", "always" or "deny"`)
		return
	}
	h.asksMu.Lock()
	pa := h.asks[body.AskID]
	h.asksMu.Unlock()
	if pa == nil {
		h.writeError(w, http.StatusNotFound, "no_such_ask", "that permission prompt is gone (the turn moved on or was cancelled)")
		return
	}
	if d == agent.Always && h.cfg.Perms != nil {
		// "Always allow this tool" writes allow — for the TOOL the ask
		// named, never anything broader.
		if err := h.cfg.Perms.Set(pa.name, permissions.Allow); err != nil {
			h.writeError(w, http.StatusInternalServerError, "settings", err.Error())
			return
		}
	}
	select {
	case pa.ch <- d:
	default:
	}
	h.writeJSON(w, map[string]any{"ok": true})
}

// ── the tools list and modes ─────────────────────────────────────────

// toolView is one exposed tool with its mode.
type toolView struct {
	Name        string `json:"name"`
	Server      string `json:"server"`
	Tool        string `json:"tool"`
	Description string `json:"description"`
	Mode        string `json:"mode"`
}

// serverView is one server with its tools — never its command: the page
// cannot see or change how a server is launched.
type serverView struct {
	Name  string     `json:"name"`
	Tools []toolView `json:"tools"`
}

// listTools is GET /ui/api/tools: every server and tool with its mode,
// plus the privacy disclosure. Reading the list launches the configured
// servers (that is what they are configured for) under the list bound.
func (h *Handler) listTools(w http.ResponseWriter, r *http.Request) {
	doc := map[string]any{"disclosure": agent.Disclosure, "servers": []serverView{}}
	if h.cfg.Reg == nil || h.cfg.Reg.Empty() {
		h.writeJSON(w, doc)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), tools.MaxListWait)
	defer cancel()
	exposed, err := h.cfg.Reg.Refresh(ctx)
	byServer := map[string][]toolView{}
	for _, e := range exposed {
		mode := permissions.Ask
		if h.cfg.Perms != nil {
			mode = h.cfg.Perms.Mode(e.Name)
		}
		byServer[e.Server] = append(byServer[e.Server], toolView{
			Name: e.Name, Server: e.Server, Tool: e.Tool, Description: e.Description, Mode: mode,
		})
	}
	var names []string
	for n := range byServer {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]serverView, 0, len(names))
	for _, n := range names {
		out = append(out, serverView{Name: n, Tools: byServer[n]})
	}
	doc["servers"] = out
	if err != nil {
		doc["error"] = err.Error() // one broken server; the healthy ones still list
	}
	h.writeJSON(w, doc)
}

// setToolMode is POST /ui/api/tools/mode: one tool's allow/ask/deny.
// The name must be a tool a configured server actually exposes; the
// request cannot name servers, commands or new tools.
func (h *Handler) setToolMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_request", "malformed body")
		return
	}
	if !permissions.Valid(body.Mode) {
		h.writeError(w, http.StatusBadRequest, "invalid_value", `mode must be "allow", "ask" or "deny"`)
		return
	}
	server, _, ok := tools.SplitName(body.Name)
	if !ok {
		h.writeError(w, http.StatusBadRequest, "invalid_value", "the name must be a tool's exposed name (<server>__<tool>)")
		return
	}
	// The tool must actually exist on a configured server. This is what
	// keeps the API from inventing names; commands stay unreachable. (A
	// server that fails to launch still vouches for nothing — the mode
	// is refused until it lists its tools.)
	if h.cfg.Reg != nil && !h.cfg.Reg.Empty() {
		ctx, cancel := context.WithTimeout(r.Context(), tools.MaxListWait)
		exposed, rerr := h.cfg.Reg.Refresh(ctx)
		cancel()
		found := false
		for _, e := range exposed {
			if e.Name == body.Name && e.Server == server {
				found = true
				break
			}
		}
		if !found {
			msg := "no configured server exposes " + body.Name
			if rerr != nil {
				msg += " (" + rerr.Error() + ")"
			}
			h.writeError(w, http.StatusNotFound, "no_such_tool", msg)
			return
		}
	}
	if h.cfg.Perms == nil {
		h.writeError(w, http.StatusNotImplemented, "no_tools", "tools are not wired in this run")
		return
	}
	if err := h.cfg.Perms.Set(body.Name, body.Mode); err != nil {
		h.writeError(w, http.StatusInternalServerError, "settings", err.Error())
		return
	}
	h.writeJSON(w, map[string]any{"ok": true})
}

// ── the tool loop's chat surface ─────────────────────────────────────

// chat is POST /ui/api/chat: one turn through the agent loop, streamed
// as SSE events (one agent.Event JSON per data: frame, then [DONE]).
// The visible history rides the body — the chats still live in the
// browser.
func (h *Handler) chat(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Loop == nil {
		h.writeError(w, http.StatusNotImplemented, "no_tools",
			"the tool loop is off (tools: off, or no MCP servers configured) — the page talks to /v1/chat/completions itself")
		return
	}
	var body struct {
		Model     string               `json:"model"`
		Effort    string               `json:"effort"`
		WebSearch bool                 `json:"web_search"`
		Messages  []chatMessagePayload `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_request", "malformed body")
		return
	}
	if body.Model == "" || len(body.Messages) == 0 {
		h.writeError(w, http.StatusBadRequest, "invalid_request", "'model' and non-empty 'messages' are required")
		return
	}
	msgs := make([]clientproto.Message, 0, len(body.Messages))
	for _, m := range body.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			h.writeError(w, http.StatusBadRequest, "invalid_request", "role "+m.Role+" is not a chat role")
			return
		}
		if len(m.Content) == 0 {
			h.writeError(w, http.StatusBadRequest, "invalid_request", "a message has no content")
			return
		}
		msgs = append(msgs, clientproto.Message{Role: m.Role, Content: m.Content})
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.writeError(w, http.StatusInternalServerError, "internal", "streaming is not supported here")
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Events flow loop → page through one channel; the page's reader is
	// this goroutine. A page gone mid-turn closes quit, which unblocks
	// the loop's emits while the turn's context kills its work.
	events := make(chan agent.Event, 64)
	quit := make(chan struct{})
	defer close(quit)
	asker := &turnAsker{h: h, emit: func(ev agent.Event) error {
		select {
		case events <- ev:
			return nil
		case <-quit:
			return errStreamGone
		}
	}}
	turnDone := make(chan error, 1)
	go func() {
		_, err := h.cfg.Loop.Turn(r.Context(), agent.TurnInput{
			Model: body.Model, Messages: msgs, Effort: body.Effort,
			WebSearch: body.WebSearch, Stream: true, Ask: asker,
		}, func(ev agent.Event) error { return asker.emit(ev) })
		turnDone <- err
	}()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	writeFrame := func(ev agent.Event) {
		b, merr := json.Marshal(ev)
		if merr != nil {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	for {
		select {
		case ev := <-events:
			writeFrame(ev)
		case err := <-turnDone:
			// The turn is over — but its last emits may still sit in the
			// channel (done lands just before the goroutine returns).
			// Drain them before the terminal frames.
			for {
				select {
				case ev := <-events:
					writeFrame(ev)
					continue
				default:
				}
				break
			}
			if err != nil && r.Context().Err() == nil {
				b, _ := json.Marshal(agent.Event{Type: "error", Message: err.Error()})
				fmt.Fprintf(w, "data: %s\n\n", b)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n") // SSE keepalive between rounds
			flusher.Flush()
		}
	}
}

// errStreamGone ends a turn whose page stopped reading (the tab closed,
// the user hit Stop — the turn's context handles the killing).
var errStreamGone = fmt.Errorf("the stream stopped reading")

// chatMessagePayload is one history message as the page sends it: a
// plain string content, or the multimodal parts array for image turns.
type chatMessagePayload struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

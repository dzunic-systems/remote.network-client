// Package tools is remote-client's local MCP tool servers: the processes
// configured in ~/.remote/mcp.json, the tools they expose, and the calls
// the agent loop makes. Each exposed name is "<server>__<tool>" so one
// server's tool can never shadow another's.
//
// Only stdio servers: the client spawns the command and speaks the MCP
// wire over its stdin/stdout (the official Go SDK, MIT/Apache-2.0). A
// server is launched lazily on first use and stays warm for the serve
// process's life; a killed or timed-out session is relaunched on the
// next turn.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MaxListWait bounds one server's launch + initialize + tools/list — a
// server that cannot come up in 20 s fails its turn with a clear error,
// not a hang.
const MaxListWait = 20 * time.Second

// CallTimeout is the per-call bound the loop applies on top. A var so
// tests can shrink it (the fake server sleeps).
var CallTimeout = 60 * time.Second

// serverNameRE keeps exposed names parseable: no "__" inside a server
// name (that pair is the join separator), nothing that could collide
// with a router tool's bare name after the join.
var serverNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ReservedToolNames are the router-executed tools' names: a local server
// may not take one (a local "web_search" would silently stop meaning the
// router's).
var ReservedToolNames = []string{"web_search", "web_fetch", "read_document"}

// Server is one mcp.json entry.
type Server struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// Config is the parsed mcp.json.
type Config map[string]Server

// LoadConfig reads and validates an MCP config. A missing file is an
// empty config, not an error (tools off is the quiet default); a file
// that exists but is wrong is an error naming the problem.
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return nil, err
	}
	var doc struct {
		MCPServers map[string]Server `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg := doc.MCPServers
	if cfg == nil {
		return nil, fmt.Errorf("%s: no \"mcpServers\" object", path)
	}
	for name, srv := range cfg {
		if !serverNameRE.MatchString(name) || strings.Contains(name, "__") || len(name) > 64 {
			return nil, fmt.Errorf("%s: server name %q is not usable (letters, digits, '-' and '_'; no '__'; at most 64 chars)", path, name)
		}
		for _, r := range ReservedToolNames {
			if name == r {
				return nil, fmt.Errorf("%s: server name %q is reserved (the router's own tool)", path, name)
			}
		}
		if srv.Command == "" {
			return nil, fmt.Errorf("%s: server %q has no command", path, name)
		}
	}
	return cfg, nil
}

// Exposed is one tool a server offers, under its joined name.
type Exposed struct {
	Name        string // "<server>__<tool>"
	Server      string
	Tool        string
	Description string
	Schema      json.RawMessage
}

// session is one live server process.
type session struct {
	cmd     *exec.Cmd
	sess    *mcp.ClientSession
	cancel  context.CancelFunc // kills the process (CommandTransport's ctx)
	tools   []Exposed
	broken  bool // a call died mid-flight: relaunch before the next use
	closing bool
}

// Registry holds the configured servers and their live sessions.
type Registry struct {
	cfg  Config
	log  *slog.Logger
	mu   sync.Mutex
	live map[string]*session
}

// NewRegistry wraps a parsed config.
func NewRegistry(cfg Config, log *slog.Logger) *Registry {
	if log == nil {
		log = slog.Default()
	}
	return &Registry{cfg: cfg, log: log, live: map[string]*session{}}
}

// Empty reports whether no servers are configured.
func (r *Registry) Empty() bool { return r == nil || len(r.cfg) == 0 }

// Close shuts every live session down.
func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, s := range r.live {
		r.killLocked(name, s)
	}
}

// launch starts one server and lists its tools.
func (r *Registry) launch(ctx context.Context, name string) (*session, error) {
	srv := r.cfg[name]
	// The session's own context: killing the session kills the process,
	// independent of whichever turn's context launched it.
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cmd := exec.CommandContext(sctx, srv.Command, srv.Args...) //nolint:gosec — the command is the user's own configured one
	// The server inherits this process's environment minus the client's
	// own secrets: an MCP server is often third-party code, and the API
	// key or the local token must never reach it by accident. A server
	// that needs a credential gets it from its own "env" block.
	cmd.Env = append(childEnv(os.Environ()), sortedEnv(srv.Env)...)
	cmd.Stderr = os.Stderr // server logs flow to ours, never into tool results

	lctx, lcancel := context.WithTimeout(ctx, MaxListWait)
	defer lcancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "remote-client", Version: "dev"}, nil)
	sess, err := client.Connect(lctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("server %q (%s): %w", name, srv.Command, err)
	}
	list, err := sess.ListTools(lctx, nil)
	if err != nil {
		cancel()
		_ = sess.Close()
		return nil, fmt.Errorf("server %q: listing tools: %w", name, err)
	}
	s := &session{cmd: cmd, sess: sess, cancel: cancel}
	for _, t := range list.Tools {
		if t.Name == "" {
			continue
		}
		if slicesContains(ReservedToolNames, t.Name) {
			r.log.Warn("mcp server offers a reserved tool name — skipped", "server", name, "tool", t.Name)
			continue
		}
		schema, _ := json.Marshal(t.InputSchema)
		s.tools = append(s.tools, Exposed{
			Name: name + "__" + t.Name, Server: name, Tool: t.Name,
			Description: t.Description, Schema: schema,
		})
	}
	return s, nil
}

// Refresh brings every configured server up (or reports the first one
// that could not) and returns all exposed tools, sorted by name.
func (r *Registry) Refresh(ctx context.Context) ([]Exposed, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var firstErr error
	for _, name := range r.sortedNames() {
		s := r.live[name]
		if s != nil && !s.broken && !s.closing {
			continue // warm and healthy
		}
		if s != nil {
			r.killLocked(name, s)
		}
		ns, err := r.launch(ctx, name)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		r.live[name] = ns
	}
	var out []Exposed
	for _, s := range r.live {
		out = append(out, s.tools...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, firstErr
}

// Call runs one tool call. The result text is the server's textual
// content; error results come back with isError set (the model sees the
// failure and self-corrects, per the MCP contract). A broken session is
// relaunched once: a long-lived stdio server may have died between turns.
func (r *Registry) Call(ctx context.Context, name string, args json.RawMessage) (text string, isError bool, err error) {
	server, _, ok := SplitName(name)
	if !ok {
		return "", false, fmt.Errorf("bad tool name %q", name)
	}
	r.mu.Lock()
	s := r.live[server]
	if s == nil || s.broken || s.closing {
		r.mu.Unlock()
		if rerr := r.refreshOne(ctx, server); rerr != nil {
			return "", false, rerr
		}
		r.mu.Lock()
		s = r.live[server]
	}
	r.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	var arguments any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &arguments); err != nil {
			return "", false, fmt.Errorf("tool arguments do not parse: %w", err)
		}
	}
	res, err := s.sess.CallTool(cctx, &mcp.CallToolParams{Name: splitTool(name), Arguments: arguments})
	if err != nil {
		// The call died — transport or process. Mark the session for
		// relaunch; the model sees the failure either way.
		r.mu.Lock()
		if s2 := r.live[server]; s2 == s {
			s2.broken = true
		}
		r.mu.Unlock()
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		return "", false, err
	}
	return contentText(res), res.IsError, nil
}

// Kill stops one server (a cancelled or timed-out call leaves its
// process killed; the next turn relaunches it fresh).
func (r *Registry) Kill(server string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.live[server]; s != nil {
		r.killLocked(server, s)
	}
}

func (r *Registry) killLocked(name string, s *session) {
	s.closing = true
	s.cancel()
	_ = s.sess.Close()
	delete(r.live, name)
}

func (r *Registry) refreshOne(ctx context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.live[name]; s != nil {
		r.killLocked(name, s)
	}
	ns, err := r.launch(ctx, name)
	if err != nil {
		return err
	}
	r.live[name] = ns
	return nil
}

func (r *Registry) sortedNames() []string {
	out := make([]string, 0, len(r.cfg))
	for n := range r.cfg {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// SplitName splits an exposed "<server>__<tool>" name.
func SplitName(name string) (server, tool string, ok bool) {
	for i := 0; i+1 < len(name); i++ {
		if name[i] == '_' && name[i+1] == '_' {
			return name[:i], name[i+2:], true
		}
	}
	return "", "", false
}

func splitTool(name string) string {
	_, tool, _ := SplitName(name)
	return tool
}

// contentText flattens a tool result's textual content.
func contentText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func sortedEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func slicesContains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// secretEnv are the client's own credentials, never handed to a tool
// server's process.
var secretEnv = map[string]bool{"REMOTE_API_KEY": true, "REMOTE_LOCAL_TOKEN": true}

// childEnv is env without the client's secrets.
func childEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if secretEnv[strings.ToUpper(name)] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Package ui is the local web UI's server half: the embedded SPA under
// /ui/ and the small JSON API it talks to under /ui/api/. The handler is
// mounted by serve (proxy.Config.UI); the proxy's loopback gate has
// already run by the time a request gets here.
//
// Security shape:
//   - CSRF: every state-changing /ui/api call must carry X-RC-CSRF, a
//     random 32-byte token this serve process generated and baked into
//     the served index.html (<meta name="rc-csrf">). Missing or wrong →
//     403. A page from another origin can read neither the page (same-
//     origin policy) nor answer the header.
//   - Origin: an Origin header naming another host than this request's
//     is refused — the defense that works even when a local token is set
//     and the listener is not loopback-only.
//   - Secrets: the API key and the local token appear in API answers as
//     presence and source only, never as values. The page never needs
//     the API key: the proxy injects it server-side.
package ui

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"remote.network/client/internal/agent"
	"remote.network/client/internal/config"
	"remote.network/client/internal/permissions"
	"remote.network/client/internal/tools"
)

// Config builds the UI handler.
type Config struct {
	// Token is the per-process CSRF token (New generates one when empty —
	// tests pass their own to know what to send).
	Token string
	// Dist is the built SPA (web.Dist()). A dist without index.html (a
	// build that never ran npm) gets the fallback page.
	Dist fs.FS
	// Settings re-resolves the settings with the flags this serve run
	// started with, so the page always shows this run's truth.
	Settings func() (config.Settings, error)
	// Doctor runs the doctor checks (the same ones the doctor command
	// runs) and returns the report and whether the direct path works.
	Doctor func(ctx context.Context) (string, bool)
	// KeyFile is where Replace key writes (the resolved key file path).
	// Empty defers to config.WriteKeyFile's default.
	KeyFile string
	// OnKey, when set, hands a replaced key to the running proxy so it is
	// used at once (no restart).
	OnKey func(key string)
	// Loop drives the local tool turns for POST /ui/api/chat (nil = tools
	// off: the page sends its turns to /v1/chat/completions itself).
	Loop *agent.Loop
	// Reg is the MCP registry behind the settings page's tools list.
	Reg *tools.Registry
	// Perms is the permission store behind the tools list's modes.
	Perms *permissions.Store
	// ToolsOn says whether turns should run through the tool loop (the
	// resolved tools setting AND at least one configured server).
	ToolsOn func() bool
}

// Handler serves /ui/ (the SPA) and /ui/api/* (the JSON API).
type Handler struct {
	cfg    Config
	token  string
	dist   fs.FS
	mu     sync.Mutex
	index  []byte // index.html with the token injected ("" = not built yet)
	loaded bool

	// asksMu guards asks: the pending permission prompts (ask id → the
	// ask's channel and the tool name its "always" persists under).
	// Registered by the chat turn's asker, answered by
	// POST /ui/api/tools/decide.
	asksMu sync.Mutex
	asks   map[string]*pendingAsk
}

// New builds the handler, generating the per-process CSRF token when the
// config does not carry one.
func New(cfg Config) *Handler {
	token := cfg.Token
	if token == "" {
		token = randomToken()
	}
	return &Handler{cfg: cfg, token: token, dist: cfg.Dist, asks: map[string]*pendingAsk{}}
}

// Token is the process's CSRF token (tests use it to authenticate).
func (h *Handler) Token() string { return h.token }

func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func (h *Handler) writeError(w http.ResponseWriter, status int, code, message string) {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]string{"message": message, "code": code},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

func (h *Handler) writeJSON(w http.ResponseWriter, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(b, '\n'))
}

// ServeHTTP handles one /ui… request (the proxy routes the whole /ui
// prefix here).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Model output is untrusted and this page can change settings and
	// approve tool calls, so the page ships with a strict CSP: scripts
	// only from self (the CSRF token rides a <meta>, never an inline
	// script), styles self + inline (Vue's runtime style bindings),
	// images self + data: (pasted screenshots), everything else locked.
	// nosniff keeps a mislabeled response from being reinterpreted.
	hdr := w.Header()
	hdr.Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; "+
			"base-uri 'none'; form-action 'self'")
	hdr.Set("X-Content-Type-Options", "nosniff")

	p := strings.TrimPrefix(r.URL.Path, "/ui")
	if p == "" {
		p = "/"
	}
	if strings.HasPrefix(p, "/api/") {
		h.api(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	h.serveStatic(w, r, p)
}

// serveStatic serves the built SPA. Unknown paths 404: the SPA has no
// client routes (its tabs live in the query string).
func (h *Handler) serveStatic(w http.ResponseWriter, r *http.Request, p string) {
	name := strings.TrimPrefix(p, "/")
	if name == "" {
		name = "index.html"
	}
	if !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	if name == "index.html" {
		h.serveIndex(w, r)
		return
	}
	f, err := h.dist.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	// Content-hashed assets may live in browsers forever.
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, path.Base(name), zeroTime, rs)
		return
	}
	// A filesystem whose files cannot seek: read whole, serve as bytes.
	b, rerr := io.ReadAll(f)
	if rerr != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, path.Base(name), zeroTime, bytes.NewReader(b))
}

var zeroTime = time.Unix(0, 0).UTC()

// serveIndex answers with index.html, the CSRF token injected. The page
// is cached: the token is fixed for this process.
func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.loaded {
		h.loaded = true
		if b, err := fs.ReadFile(h.dist, "index.html"); err == nil {
			s := string(b)
			if strings.Contains(s, "__RC_CSRF__") {
				s = strings.ReplaceAll(s, "__RC_CSRF__", h.token)
			} else {
				s = strings.Replace(s, "<head>",
					"<head>\n    <meta name=\"rc-csrf\" content=\""+h.token+"\" />", 1)
			}
			h.index = []byte(s)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if h.index == nil {
		// The UI was never built into this binary.
		_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>remote client</title>
<p style="font-family: monospace; padding: 2rem">The web UI was not built into this
remote-client binary. Build it with <b>cd web &amp;&amp; npm ci &amp;&amp; npm run build</b>
and rebuild, or run <b>make build</b>. The OpenAI endpoint at /v1 works regardless.</p>`)
		return
	}
	_, _ = w.Write(h.index)
}

// api is the JSON surface under /ui/api/*.
func (h *Handler) api(w http.ResponseWriter, r *http.Request) {
	// CSRF: state-changing calls need the token; every call needs a
	// same-origin Origin when the browser sent one at all.
	if !h.guard(w, r) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/ui/api")
	switch {
	case rest == "/settings" && r.Method == http.MethodGet:
		h.getSettings(w)
	case rest == "/settings" && r.Method == http.MethodPost:
		h.postSetting(w, r)
	case rest == "/login" && r.Method == http.MethodPost:
		h.login(w, r)
	case rest == "/doctor" && r.Method == http.MethodPost:
		h.doctor(w, r)
	case rest == "/tools" && r.Method == http.MethodGet:
		h.listTools(w, r)
	case rest == "/tools/mode" && r.Method == http.MethodPost:
		h.setToolMode(w, r)
	case rest == "/tools/decide" && r.Method == http.MethodPost:
		h.decide(w, r)
	case rest == "/chat" && r.Method == http.MethodPost:
		h.chat(w, r)
	default:
		h.writeError(w, http.StatusNotFound, "not_found", "no such UI API surface")
	}
}

// guard is the CSRF + Origin gate. It returns false when the response is
// already written.
func (h *Handler) guard(w http.ResponseWriter, r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Host != r.Host {
			h.writeError(w, http.StatusForbidden, "cross_origin",
				"the UI API accepts same-origin requests only")
			return false
		}
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	got := r.Header.Get("X-RC-CSRF")
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(h.token)) != 1 {
		h.writeError(w, http.StatusForbidden, "csrf",
			"missing or wrong X-RC-CSRF token — reload the page")
		return false
	}
	return true
}

// getSettings answers every setting: value, source, lock state, and the
// lock's reason. Secrets are presence and source only.
func (h *Handler) getSettings(w http.ResponseWriter) {
	s, err := h.cfg.Settings()
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "settings", err.Error())
		return
	}
	h.writeJSON(w, map[string]any{"settings": h.views(s), "tools_enabled": h.toolsOn()})
}

// settingRow is one row of the settings page.
type settingRow struct {
	Key        string `json:"key"`
	Label      string `json:"label"`
	Value      any    `json:"value"`
	Set        bool   `json:"set"`
	Source     string `json:"source"`
	SourceName string `json:"source_name,omitempty"`
	Locked     bool   `json:"locked"`
	LockReason string `json:"lock_reason,omitempty"`
	Kind       string `json:"kind"` // string|bool|secret|info
	Secret     bool   `json:"secret"`
	Note       string `json:"note,omitempty"`
}

func (h *Handler) views(s config.Settings) []settingRow {
	rows := []settingRow{
		{
			Key: "home", Label: "home", Value: s.Home.Value, Set: true,
			Source: s.Home.Source.Kind, SourceName: s.Home.Source.Name,
			Kind: "info",
			Note: "moves the key, the config file, the identity key and the tool config together",
		},
		{
			// The router URL is read-only in the UI, always: changing it
			// sends the API key to the new host.
			Key: "router", Label: "router", Value: s.Router.Value, Set: true,
			Source: s.Router.Source.Kind, SourceName: s.Router.Source.Name,
			Locked:     true,
			LockReason: "read-only here — change it with `config set`, REMOTE_ROUTER or --router; changing it would send your API key to a new host",
			Kind:       "string",
		},
		strRow("router_alt", "router (second)", s.RouterAlt.Value, s.RouterAlt.Set(), s.RouterAlt.Source, "a second router strengthens the NAT test"),
		strRow("listen", "listen address", s.Listen.Value, true, s.Listen.Source, "takes effect on restart"),
		{
			Key: "port", Label: "listen port", Value: s.Port.Value, Set: true,
			Source: s.Port.Source.Kind, SourceName: s.Port.Source.Name,
			Locked:     s.Port.Source.Locked(),
			LockReason: lockedBecause(s.Port.Source, "set by REMOTE_PORT"),
			Kind:       "string", Note: "kept on the listen host; takes effect on restart",
		},
		{
			Key: "api_key", Label: "API key", Value: nil, Set: s.APIKey.Value != "",
			Source: s.APIKey.Source.Kind, SourceName: s.APIKey.Source.Name,
			Locked:     true,
			LockReason: lockedBecause(s.APIKey.Source, "set by REMOTE_API_KEY"),
			Kind:       "secret", Secret: true,
			Note: "stored with owner-only permissions; never shown",
		},
		strRow("api_key_file", "key file", s.APIKeyFile.Value, true, s.APIKeyFile.Source, ""),
		{
			Key: "local_token", Label: "local token", Value: nil, Set: s.LocalToken.Value != "",
			Source: s.LocalToken.Source.Kind, SourceName: s.LocalToken.Source.Name,
			Locked:     true,
			LockReason: lockedBecause(s.LocalToken.Source, "set by REMOTE_LOCAL_TOKEN"),
			Kind:       "secret", Secret: true,
			Note: "required when the listener is not loopback-only; set with --local-token",
		},
		boolRow("direct", "direct path", s.Direct.Value, s.Direct.Source, "straight to a node when a direct path exists; off sends everything through the router"),
		strRow("log_level", "log level", s.LogLevel.Value, true, s.LogLevel.Source, "debug|info|warn"),
		boolRow("ui", "web UI", s.UI.Value, s.UI.Source, "this page; takes effect on restart"),
		strRow("default_model", "default model", s.DefaultModel.Value, s.DefaultModel.Set(), s.DefaultModel.Source, "the model chat starts with"),
		boolRow("web_search", "web search", s.WebSearch.Value, s.WebSearch.Source, "the default for this client's own chat surfaces; the search runs on the router"),
		// Read-only in the UI, always (the review rule): this file names
		// commands the client runs, so a page that could repoint it could
		// run any command without a permission prompt. Servers are added
		// by editing the file, or with `config set` from a terminal.
		settingRow{
			Key: "mcp_config", Label: "MCP config", Value: s.MCPConfig.Value, Set: true,
			Source: s.MCPConfig.Source.Kind, SourceName: s.MCPConfig.Source.Name,
			Locked:     true,
			LockReason: "read-only here — change it by editing the file or with `config set`; the file names commands this machine runs, so the browser must never repoint it",
			Kind:       "string", Note: "the local tools' server list (stdio MCP servers)",
		},
		boolRow("tools", "local tools", s.Tools.Value, s.Tools.Source, "the local tool loop for the client's own chat surfaces; on by default when an MCP config exists"),
	}
	return rows
}

func strRow(key, label, value string, set bool, src config.Source, note string) settingRow {
	return settingRow{
		Key: key, Label: label, Value: value, Set: set,
		Source: src.Kind, SourceName: src.Name,
		Locked: src.Locked(), LockReason: lockedBecause(src, "set by "+src.Name),
		Kind: "string", Note: note,
	}
}

func boolRow(key, label string, value bool, src config.Source, note string) settingRow {
	return settingRow{
		Key: key, Label: label, Value: value, Set: true,
		Source: src.Kind, SourceName: src.Name,
		Locked: src.Locked(), LockReason: lockedBecause(src, "set by "+src.Name),
		Kind: "bool", Note: note,
	}
}

// lockedBecause names the flag or the variable that owns the value ("set
// by REMOTE_LISTEN"). src.Name already carries the right spelling for
// both kinds, so the reason is just the source's own phrasing.
func lockedBecause(src config.Source, _ string) string {
	if !src.Locked() {
		return ""
	}
	if src.Kind == config.SourceEnv {
		return "set by " + src.Name + " — change the variable, not the file"
	}
	return "set by " + src.Name + " — change the flag, not the file"
}

// postSetting writes one file-backed setting, exactly like
// `remote-client config set`. Locked settings, the router URL and the
// secrets are refused; a bad value is a 400 with the validation error.
func (h *Handler) postSetting(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key   string `json:"key"`
		Value any    `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_request", "malformed body")
		return
	}
	// The secrets and the router are refused before anything else, with
	// the way out named (the way `config set` refuses them).
	if body.Key == "api_key" || body.Key == "local_token" {
		h.writeError(w, http.StatusForbidden, "secret",
			body.Key+" is a secret: use Replace key for the API key, --local-token for the token")
		return
	}
	if body.Key == "router" {
		h.writeError(w, http.StatusForbidden, "router_readonly",
			"the router URL is read-only here — change it with `config set`, REMOTE_ROUTER or --router; changing it would send your API key to a new host")
		return
	}
	if !config.ValidFileKey(body.Key) {
		h.writeError(w, http.StatusBadRequest, "unknown_setting", "unknown setting "+strconv.Quote(body.Key))
		return
	}
	s, err := h.cfg.Settings()
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "settings", err.Error())
		return
	}
	if srcOf(s, body.Key).Locked() {
		src := srcOf(s, body.Key)
		h.writeError(w, http.StatusForbidden, "locked",
			lockedBecause(src, "")+" (this run set it at startup)")
		return
	}
	text := ""
	switch v := body.Value.(type) {
	case string:
		text = v
	case bool:
		if v {
			text = "on"
		} else {
			text = "off"
		}
	case float64:
		text = strconv.FormatFloat(v, 'f', -1, 64)
	default:
		h.writeError(w, http.StatusBadRequest, "invalid_request", "the value must be a string or a boolean")
		return
	}
	cfg, err := config.Load()
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "settings", err.Error())
		return
	}
	old, hadOld := readConfigFile()
	newCfg, err := config.SetFileKey(cfg, body.Key, text)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_value", err.Error())
		return
	}
	if err := config.Save(newCfg); err != nil {
		h.writeError(w, http.StatusInternalServerError, "settings", err.Error())
		return
	}
	// The file must still resolve: a port that now conflicts with an
	// explicit listen rolls the write back, with the resolver's error.
	if _, rerr := config.Resolve(config.Flags{}); rerr != nil {
		restoreConfigFile(old, hadOld)
		h.writeError(w, http.StatusBadRequest, "invalid_value", rerr.Error())
		return
	}
	h.writeJSON(w, map[string]any{"ok": true, "restart": body.Key == "listen" || body.Key == "port" || body.Key == "ui"})
}

// login stores a new API key (the same rules `login` applies: the shape
// is checked, the file lands 0600, the key is never echoed back).
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_request", "malformed body")
		return
	}
	if !strings.HasPrefix(body.APIKey, "rn_sk_") {
		h.writeError(w, http.StatusBadRequest, "invalid_key",
			"that does not look like a remote.network API key (expected rn_sk_…); nothing was stored")
		return
	}
	// A key an environment variable or a flag supplies wins over the
	// file: writing the file would change nothing this run uses, so say so
	// instead of pretending.
	if h.cfg.Settings != nil {
		if st, err := h.cfg.Settings(); err == nil && st.APIKey.Source.Locked() {
			h.writeError(w, http.StatusConflict, "locked",
				lockedBecause(st.APIKey.Source, "")+" — the key file would not be used")
			return
		}
	}
	if err := config.WriteKeyFile(h.cfg.KeyFile, body.APIKey); err != nil {
		h.writeError(w, http.StatusInternalServerError, "settings", err.Error())
		return
	}
	if h.cfg.OnKey != nil {
		h.cfg.OnKey(body.APIKey)
	}
	h.writeJSON(w, map[string]any{"ok": true})
}

// doctor runs the checks and answers the report verbatim (it names no
// key — the doctor command prints the same text to a terminal).
func (h *Handler) doctor(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Doctor == nil {
		h.writeError(w, http.StatusNotImplemented, "no_doctor", "doctor is not wired in this build")
		return
	}
	report, ok := h.cfg.Doctor(r.Context())
	h.writeJSON(w, map[string]any{"ok": ok, "report": report})
}

// srcOf reads the source of one key off the resolved settings.
func srcOf(s config.Settings, key string) config.Source {
	switch key {
	case "router":
		return s.Router.Source
	case "router_alt":
		return s.RouterAlt.Source
	case "listen":
		return s.Listen.Source
	case "port":
		return s.Port.Source
	case "api_key_file":
		return s.APIKeyFile.Source
	case "direct":
		return s.Direct.Source
	case "log_level":
		return s.LogLevel.Source
	case "ui":
		return s.UI.Source
	case "default_model":
		return s.DefaultModel.Source
	case "web_search":
		return s.WebSearch.Source
	case "mcp_config":
		return s.MCPConfig.Source
	case "tools":
		return s.Tools.Source
	default:
		return config.Source{Kind: config.SourceFile}
	}
}

func readConfigFile() ([]byte, bool) {
	path, err := config.Path()
	if err != nil {
		return nil, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return b, true
}

func restoreConfigFile(old []byte, had bool) {
	path, err := config.Path()
	if err != nil {
		return
	}
	if !had {
		_ = os.Remove(path)
		return
	}
	_ = os.WriteFile(path, old, 0o600)
}

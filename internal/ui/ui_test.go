package ui

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	"remote.network/client/internal/config"
)

// testDist is the built UI as the tests see it: an index.html with the
// placeholder, an asset, and the favicon.
func testDist() fstest.MapFS {
	return fstest.MapFS{
		"index.html":   &fstest.MapFile{Data: []byte("<!doctype html><html><head><meta name=\"rc-csrf\" content=\"__RC_CSRF__\"></head><body>ui</body></html>")},
		"favicon.svg":  &fstest.MapFile{Data: []byte("<svg/>")},
		"assets/a1.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	}
}

// isolate pins a temp home, clears every REMOTE_* variable, and returns
// a Settings resolver with the given flags baked in (the way serve does).
func isolate(t *testing.T, fl config.Flags) func() (config.Settings, error) {
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
	return func() (config.Settings, error) { return config.Resolve(fl) }
}

func newTestUI(t *testing.T, fl config.Flags) *Handler {
	t.Helper()
	resolve := isolate(t, fl)
	h := New(Config{
		Dist:     testDist(),
		Settings: resolve,
		Doctor: func(context.Context) (string, bool) {
			return "router ok\nverdict direct path OK", true
		},
	})
	return h
}

func do(h http.Handler, method, target string, headers map[string]string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestIndexServedWithTokenInjected(t *testing.T) {
	h := newTestUI(t, config.Flags{})
	rec := do(h, "GET", "/ui/", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `name="rc-csrf"`) {
		t.Fatalf("no csrf meta in %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "__RC_CSRF__") {
		t.Fatalf("the placeholder leaked:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), h.Token()) {
		t.Fatalf("the process token is not in the page:\n%s", rec.Body.String())
	}
	// The strict CSP the plan pins, and nosniff, ride every /ui response
	// (the token sits in a <meta> precisely so no inline script is needed).
	csp := "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; " +
		"base-uri 'none'; form-action 'self'"
	if got := rec.Header().Get("Content-Security-Policy"); got != csp {
		t.Fatalf("CSP = %q", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", got)
	}
	// The placeholder page without an injection point is rewritten too:
	// a dist whose index lacks the meta gets one added.
	h2 := New(Config{Dist: fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<!doctype html><html><head><title>x</title></head></html>")},
	}})
	rec = do(h2, "GET", "/ui/", nil, "")
	if !strings.Contains(rec.Body.String(), h2.Token()) {
		t.Fatalf("token not injected into a placeholder-less index:\n%s", rec.Body.String())
	}
}

func TestStaticAssetsAndFallback(t *testing.T) {
	h := newTestUI(t, config.Flags{})
	rec := do(h, "GET", "/ui/favicon.svg", nil, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<svg/>") {
		t.Fatalf("favicon: %d %q", rec.Code, rec.Body.String())
	}
	rec = do(h, "GET", "/ui/assets/a1.js", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("asset: %d", rec.Code)
	}
	if rec.Header().Get("Cache-Control") == "" {
		t.Fatalf("assets must be cache-immutable")
	}
	// Unknown paths 404 (no SPA client routes), never the router.
	rec = do(h, "GET", "/ui/nowhere", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path: %d", rec.Code)
	}
	// A dist that was never built (only .gitkeep committed) serves the
	// fallback page, not a crash.
	h2 := New(Config{Dist: fstest.MapFS{".gitkeep": &fstest.MapFile{Data: []byte("")}}, Settings: func() (config.Settings, error) { return config.Resolve(config.Flags{}) }})
	rec = do(h2, "GET", "/ui/", nil, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "npm") {
		t.Fatalf("fallback page: %d %q", rec.Code, firstLineUI(rec.Body.String()))
	}
}

func TestSettingsListAndSourceLocks(t *testing.T) {
	h := newTestUI(t, config.Flags{})
	t.Setenv("REMOTE_LISTEN", "127.0.0.1:4141") // locked: the env owns it
	t.Setenv("REMOTE_API_KEY", "fx-ui-secret-9")
	rec := do(h, "GET", "/ui/api/settings", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("settings: %d %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Settings []settingRow `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Settings) == 0 {
		t.Fatalf("no settings rows")
	}
	byKey := map[string]settingRow{}
	for _, s := range doc.Settings {
		byKey[s.Key] = s
	}
	// The key: presence and source, never the value.
	if byKey["api_key"].Kind != "secret" || !byKey["api_key"].Set {
		t.Fatalf("api_key row = %+v", byKey["api_key"])
	}
	if strings.Contains(rec.Body.String(), "fx-ui-secret-9") {
		t.Fatalf("the API key value leaked into /ui/api/settings:\n%s", rec.Body.String())
	}
	// An env-owned value is locked, with the variable named.
	if !byKey["listen"].Locked || byKey["listen"].Source != "env" || byKey["listen"].SourceName != "REMOTE_LISTEN" {
		t.Fatalf("listen row = %+v", byKey["listen"])
	}
	// A default value is editable.
	if byKey["direct"].Locked || byKey["direct"].Source != "default" {
		t.Fatalf("direct row = %+v", byKey["direct"])
	}
	// The router is always read-only in the UI.
	if !byKey["router"].Locked || byKey["router"].LockReason == "" {
		t.Fatalf("router row = %+v", byKey["router"])
	}
}

func TestCSRFAndOriginGuard(t *testing.T) {
	h := newTestUI(t, config.Flags{})
	tok := h.Token()
	// Missing token → 403.
	rec := do(h, "POST", "/ui/api/settings", map[string]string{"Content-Type": "application/json"}, `{"key":"direct","value":"off"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing csrf: %d", rec.Code)
	}
	// Wrong token → 403.
	rec = do(h, "POST", "/ui/api/settings", map[string]string{"X-RC-CSRF": "nope"}, `{"key":"direct","value":"off"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong csrf: %d", rec.Code)
	}
	// Foreign Origin → 403, even with a valid token.
	rec = do(h, "POST", "/ui/api/doctor", map[string]string{
		"X-RC-CSRF": tok, "Origin": "https://evil.example",
	}, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign origin: %d", rec.Code)
	}
	// Same-origin Origin passes (the test's Host is example.com).
	rec = do(h, "POST", "/ui/api/doctor", map[string]string{
		"X-RC-CSRF": tok, "Origin": "http://example.com",
	}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("same-origin doctor: %d %s", rec.Code, rec.Body.String())
	}
	// GETs need no token, and answer nothing secret.
	rec = do(h, "GET", "/ui/api/settings", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("settings GET: %d", rec.Code)
	}
}

func TestSetSettingWritesValidatesAndLocks(t *testing.T) {
	h := newTestUI(t, config.Flags{})
	tok := h.Token()
	head := map[string]string{"X-RC-CSRF": tok, "Content-Type": "application/json"}

	// A plain write lands in the file.
	rec := do(h, "POST", "/ui/api/settings", head, `{"key":"direct","value":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("direct off: %d %s", rec.Code, rec.Body.String())
	}
	b, err := os.ReadFile(mustPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "direct: false") {
		t.Fatalf("config file = %s", b)
	}
	// The value shows in the list with the file's source.
	rec = do(h, "GET", "/ui/api/settings", nil, "")
	if !strings.Contains(rec.Body.String(), `"source":"file"`) {
		t.Fatalf("source not shown:\n%s", rec.Body.String())
	}

	// A bad value is a 400 carrying the validation error.
	rec = do(h, "POST", "/ui/api/settings", head, `{"key":"log_level","value":"loud"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "log level") {
		t.Fatalf("bad value: %d %s", rec.Code, rec.Body.String())
	}
	// The router is refused even unlocked.
	rec = do(h, "POST", "/ui/api/settings", head, `{"key":"router","value":"https://other.example.com"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("router write: %d", rec.Code)
	}
	// Secrets are refused.
	rec = do(h, "POST", "/ui/api/settings", head, `{"key":"api_key","value":"fx-k"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("api_key write: %d", rec.Code)
	}
	// A locked setting (an env-owned listen) is refused with the reason.
	t.Setenv("REMOTE_LISTEN", "127.0.0.1:4141")
	rec = do(h, "POST", "/ui/api/settings", head, `{"key":"listen","value":"127.0.0.1:5151"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "REMOTE_LISTEN") {
		t.Fatalf("locked write: %d %s", rec.Code, rec.Body.String())
	}
	t.Setenv("REMOTE_LISTEN", "")

	// A port that conflicts with the file's listen rolls the write back.
	do(h, "POST", "/ui/api/settings", head, `{"key":"listen","value":"127.0.0.1:4141"}`)
	rec = do(h, "POST", "/ui/api/settings", head, `{"key":"port","value":5151}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "conflicts") {
		t.Fatalf("conflict: %d %s", rec.Code, rec.Body.String())
	}
	b, err = os.ReadFile(mustPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "port") {
		t.Fatalf("the conflicting port stayed in the file:\n%s", b)
	}
}

func TestReplaceKey(t *testing.T) {
	h := newTestUI(t, config.Flags{})
	tok := h.Token()
	head := map[string]string{"X-RC-CSRF": tok, "Content-Type": "application/json"}

	rec := do(h, "POST", "/ui/api/login", head, `{"api_key":"not-a-key"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad shape: %d", rec.Code)
	}
	// The rn_sk_ shape is required; the fake key avoids the denylist
	// spelling (uppercase, no 8-run of lowercase).
	rec = do(h, "POST", "/ui/api/login", head, `{"api_key":"rn_sk_FXTESTKEY"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	path, err := config.KeyFilePath()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != "rn_sk_FXTESTKEY" {
		t.Fatalf("key file = %q", b)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("key file perm = %o, want 0600", fi.Mode().Perm())
		}
	}
	// The key never echoes back.
	if strings.Contains(rec.Body.String(), "rn_sk_FXTESTKEY") {
		t.Fatalf("the key echoed back: %s", rec.Body.String())
	}
}

func TestDoctorEndpoint(t *testing.T) {
	h := newTestUI(t, config.Flags{})
	tok := h.Token()
	rec := do(h, "POST", "/ui/api/doctor", map[string]string{"X-RC-CSRF": tok}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("doctor: %d %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		OK     bool   `json:"ok"`
		Report string `json:"report"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.OK || !strings.Contains(doc.Report, "direct path OK") {
		t.Fatalf("doctor doc = %+v", doc)
	}
}

func TestUnknownAPISurfaceIs404(t *testing.T) {
	h := newTestUI(t, config.Flags{})
	rec := do(h, "GET", "/ui/api/whatever", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown api: %d", rec.Code)
	}
}

func mustPath(t *testing.T) string {
	t.Helper()
	p, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func firstLineUI(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// The dist the release embeds must at least be a valid FS; this pins the
// committed .gitkeep story (go build without npm still compiles).
func TestEmptyDistStillOpens(t *testing.T) {
	var f fs.FS = fstest.MapFS{".gitkeep": &fstest.MapFile{Data: []byte{}}}
	if _, err := f.Open(".gitkeep"); err != nil {
		t.Fatal(err)
	}
	_ = io.Discard
}

// TestReplaceKeyLiveAndLocked (review): a replaced key reaches the running
// proxy at once (OnKey), and a key an environment variable owns is not
// "replaced" by a file write this run would never read.
func TestReplaceKeyLiveAndLocked(t *testing.T) {
	resolve := isolate(t, config.Flags{})
	var got string
	h := New(Config{Dist: testDist(), Settings: resolve, OnKey: func(k string) { got = k }})
	head := map[string]string{"X-RC-CSRF": h.Token(), "Content-Type": "application/json"}
	if rec := do(h, "POST", "/ui/api/login", head, `{"api_key":"rn_sk_FXLIVEKEY"}`); rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	if got != "rn_sk_FXLIVEKEY" {
		t.Fatalf("OnKey got %q, want the new key", got)
	}

	t.Setenv("REMOTE_API_KEY", "rn_sk_FXENVKEY")
	got = ""
	rec := do(h, "POST", "/ui/api/login", head, `{"api_key":"rn_sk_FXOTHERKEY"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "REMOTE_API_KEY") {
		t.Fatalf("env-owned key: %d %s, want 409 naming REMOTE_API_KEY", rec.Code, rec.Body.String())
	}
	if got != "" {
		t.Fatal("OnKey ran for a refused replace")
	}
	path, _ := config.KeyFilePath()
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "FXOTHERKEY") {
		t.Fatal("the refused key was written")
	}
}

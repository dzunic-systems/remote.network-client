package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// testUI is the smallest SPA stand-in: index.html with the placeholder
// the ui package injects, plus one asset.
func testUIFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":  &fstest.MapFile{Data: []byte("<!doctype html><html><head><meta name=\"rc-csrf\" content=\"__RC_CSRF__\"></head><body>ui</body></html>")},
		"assets/a.js": &fstest.MapFile{Data: []byte("x")},
	}
}

// TestUIServedAndOtherPathsForwarded: /ui/ routes to the UI handler, /
// redirects a browser (Accept: text/html) there, and every other path —
// including /uix, which is not the UI prefix — forwards exactly as
// before (OpenAI SDKs must see no change). A forwarded path shows the
// fake router mux's text 404; a locally-handled 404 is the proxy's JSON
// error shape.
func TestUIServedAndOtherPathsForwarded(t *testing.T) {
	f := newFX(t)
	f.start(false)
	f.proxy.cfg.UI = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ui/" || r.URL.Path == "/ui" {
			_, _ = w.Write([]byte("ui-index"))
			return
		}
		// Every other path the UI prefix routes here is marked, so the
		// test can tell the UI handler's answer from the router's.
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("ui:" + r.URL.Path))
	})

	router404 := func(rec *httptest.ResponseRecorder, what string) {
		t.Helper()
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "page not found") {
			t.Fatalf("%s must forward to the router: %d %q", what, rec.Code, rec.Body.String())
		}
	}
	rec := f.get("/ui/", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "ui-index" {
		t.Fatalf("/ui/: %d %q", rec.Code, rec.Body.String())
	}
	rec = f.get("/", map[string]string{"Accept": "text/html,application/xhtml+xml"})
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/" {
		t.Fatalf("/ redirect: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	// An SDK's Accept never triggers the redirect: still a forward.
	router404(f.get("/", map[string]string{"Accept": "application/json"}), "/ with a JSON accept")
	// Unknown non-UI paths forward.
	router404(f.get("/v1/models", nil), "/v1/models")
	router404(f.get("/uix", nil), "/uix")
	// /ui/api is never forwarded: it reaches the UI handler ("ui:" prefix),
	// not the router's mux.
	rec = f.get("/ui/api/settings", nil)
	if rec.Code != http.StatusNotFound || !strings.HasPrefix(rec.Body.String(), "ui:") {
		t.Fatalf("/ui/api/settings must reach the UI handler, not the router: %d %q", rec.Code, rec.Body.String())
	}
}

// TestUIOffStillNeverForwardsUI: with the UI disabled (--no-ui), /ui/*
// answers the proxy's local JSON 404 instead of leaking to the router
// (whose 404 is the mux's text shape).
func TestUIOffStillNeverForwardsUI(t *testing.T) {
	f := newFX(t)
	f.start(false)
	rec := f.get("/ui/", nil)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "the web UI is off") {
		t.Fatalf("/ui/ with the UI off: %d %q", rec.Code, rec.Body.String())
	}
}

// TestUITokenModel: on a token'd listener the shell is served without
// the token (a browser navigation cannot carry a header — the page asks
// once), but /ui/api and the OpenAI surface still require it.
func TestUITokenModel(t *testing.T) {
	f := newFX(t)
	f.fallback = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	f.start(false)
	f.proxy.cfg.LocalToken = "sekrit"
	f.proxy.cfg.UI = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ui-index"))
	})

	rec := f.get("/ui/", nil) // no token
	if rec.Code != http.StatusOK || rec.Body.String() != "ui-index" {
		t.Fatalf("shell without token: %d %q", rec.Code, rec.Body.String())
	}
	rec = f.get("/ui/api/settings", nil) // no token
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("api without token: %d", rec.Code)
	}
	rec = f.get("/ui/api/settings", map[string]string{"X-Local-Token": "nope"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("api with the wrong token: %d", rec.Code)
	}
	rec = f.get("/ui/api/settings", map[string]string{"X-Local-Token": "sekrit"})
	if rec.Code != http.StatusOK {
		t.Fatalf("api with the right token: %d", rec.Code)
	}
	rec = f.get("/v1/models", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("openai surface without token: %d", rec.Code)
	}
}

// get sends one GET through the proxy handler.
func (f *fx) get(path string, headers map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1"+path, nil)
	req.Host = "127.0.0.1"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.proxy.Handler().ServeHTTP(rec, req)
	return rec
}

// TestLoopbackGateCoversUIToo: the gate that keeps web pages out of the
// OpenAI endpoint covers the UI the same way — a foreign Origin is
// refused before the UI handler runs.
func TestLoopbackGateCoversUIToo(t *testing.T) {
	f := newFX(t)
	f.start(false)
	f.proxy.cfg.UI = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ui-index"))
	})
	rec := f.get("/ui/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback GET /ui/: %d", rec.Code)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1/ui/", nil)
	req.Host = "rebound.example.com" // a rebound name is not a loopback host
	rec = httptest.NewRecorder()
	f.proxy.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("rebound host on /ui/: %d", rec.Code)
	}
	req, _ = http.NewRequest(http.MethodGet, "http://127.0.0.1/ui/", nil)
	req.Host = "127.0.0.1"
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	f.proxy.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign origin on /ui/: %d", rec.Code)
	}
}

// TestProxyNeverInjectsTools (the tools phase's §3.5 rule): the proxy is
// a conduit. A third-party request carrying its own tools routes with
// needs.tools=true and falls back byte-identical; a request without
// tools never gains any. No MCP machinery exists at this layer at all —
// this test pins it.
func TestProxyNeverInjectsTools(t *testing.T) {
	f := newFX(t)
	var routed map[string]any
	var fallbackBody string
	f.routeAnswer = func(w http.ResponseWriter, _ *http.Request, body map[string]any) {
		routed = body
		writeJSON(w, map[string]any{"direct": false, "reason": "disabled"})
	}
	f.fallback = func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fallbackBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}
	f.start(true) // the desk is asked (direct:false answers → the fallback)

	withTools := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"mine","parameters":{"type":"object"}}}]}`
	if rec := f.post(withTools); rec.Code != http.StatusOK {
		t.Fatalf("with tools: %d", rec.Code)
	}
	needs, _ := routed["needs"].(map[string]any)
	if needs == nil || needs["tools"] != true {
		t.Fatalf("route ask needs = %v, want tools:true", routed["needs"])
	}
	var fwd struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal([]byte(fallbackBody), &fwd); err != nil {
		t.Fatalf("fallback body: %v", err)
	}
	if len(fwd.Tools) != 1 || fwd.Tools[0]["function"].(map[string]any)["name"] != "mine" {
		t.Fatalf("the request's own tools were not forwarded verbatim: %s", fallbackBody)
	}

	// Without tools: nothing appears.
	f.mu.Lock()
	routed = nil
	f.mu.Unlock()
	if rec := f.post(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("without tools: %d", rec.Code)
	}
	needs, _ = routed["needs"].(map[string]any)
	if needs["tools"] != false {
		t.Fatalf("route ask needs = %v, want tools:false", routed["needs"])
	}
	if strings.Contains(fallbackBody, "tools") {
		t.Fatalf("tools appeared in a plain request: %s", fallbackBody)
	}
}

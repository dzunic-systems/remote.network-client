package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestChatReachesRouter: `chat` drives the proxy in-process; its request
// must pass the proxy's own loopback gate. Every release up to v0.1.4
// sent Host "local" and got loopback_only 403 — nothing ran chat end to
// end.
func TestChatReachesRouter(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, n := range []string{"REMOTE_HOME", "REMOTE_ROUTER", "REMOTE_API_KEY", "REMOTE_DIRECT", "REMOTE_DEFAULT_MODEL"} {
		t.Setenv(n, "")
	}
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer rn_sk_FXCHATKEY" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusTeapot)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answer":"hi from the router"}` + "\n"))
	}))
	defer router.Close()

	var out bytes.Buffer
	root := newRoot()
	root.SetOut(&out)
	root.SetArgs([]string{"chat", "--router", router.URL, "--api-key", "rn_sk_FXCHATKEY",
		"--no-direct", "--key-file", filepath.Join(home, "client.key"), "--model", "m", "hello"})
	if err := root.Execute(); err != nil {
		t.Fatalf("chat: %v (output %q)", err, out.String())
	}
	if !strings.Contains(out.String(), "hi from the router") {
		t.Fatalf("chat output = %q", out.String())
	}
}

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
	t.Setenv("USERPROFILE", home) // Windows: os.UserHomeDir reads USERPROFILE
	for _, n := range []string{"REMOTE_HOME", "REMOTE_ROUTER", "REMOTE_API_KEY", "REMOTE_DIRECT", "REMOTE_DEFAULT_MODEL"} {
		t.Setenv(n, "")
	}
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer rn_sk_FXCHATKEY" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusTeapot)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"pondering\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi from the router\"},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer router.Close()

	var out, errOut bytes.Buffer
	root := newRoot()
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"chat", "--router", router.URL, "--api-key", "rn_sk_FXCHATKEY",
		"--no-direct", "--key-file", filepath.Join(home, "client.key"), "--model", "m", "hello"})
	if err := root.Execute(); err != nil {
		t.Fatalf("chat: %v (output %q)", err, out.String())
	}
	// The answer is plain text on stdout (no JSON); the thinking is on
	// stderr, so a thinking model visibly works.
	if out.String() != "hi from the router\n" {
		t.Fatalf("chat stdout = %q, want the plain answer", out.String())
	}
	if !strings.Contains(errOut.String(), "thinking: pondering") {
		t.Fatalf("chat stderr = %q, want the thinking", errOut.String())
	}
}

// TestChatErrorIsAMessage: a refused request prints the router's message,
// never its JSON body.
func TestChatErrorIsAMessage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, n := range []string{"REMOTE_HOME", "REMOTE_ROUTER", "REMOTE_API_KEY", "REMOTE_DIRECT", "REMOTE_DEFAULT_MODEL"} {
		t.Setenv(n, "")
	}
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"this account is out of credits","code":"insufficient_credits"}}`))
	}))
	defer router.Close()
	var out bytes.Buffer
	root := newRoot()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"chat", "--router", router.URL, "--api-key", "rn_sk_FXCHATKEY",
		"--no-direct", "--key-file", filepath.Join(home, "client.key"), "--model", "m", "hello"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "out of credits") || strings.Contains(err.Error(), "{") {
		t.Fatalf("chat error = %v, want the router's message without JSON", err)
	}
}

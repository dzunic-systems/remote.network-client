package proxy

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"remote.network/client/internal/nat"
	"remote.network/node/clientproto"
	"remote.network/node/identity"
	"remote.network/node/ingress"
	"remote.network/node/wire"
)

// The goldens: the exact byte shapes the router pins in its own golden
// tests, with ID/CREATED/MODEL as placeholders. The direct client must
// produce them modulo id/timestamp.
const goldenStream = `data: {"id":"chatcmpl-ID","object":"chat.completion.chunk","created":CREATED,"model":"MODEL","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-ID","object":"chat.completion.chunk","created":CREATED,"model":"MODEL","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}

data: {"id":"chatcmpl-ID","object":"chat.completion.chunk","created":CREATED,"model":"MODEL","choices":[{"index":0,"delta":{"content":"lo!"},"finish_reason":null}]}

data: {"id":"chatcmpl-ID","object":"chat.completion.chunk","created":CREATED,"model":"MODEL","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"chatcmpl-ID","object":"chat.completion.chunk","created":CREATED,"model":"MODEL","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17,"cost":2.45e-10}}

data: [DONE]

`

const goldenBody = `{"id":"chatcmpl-ID","object":"chat.completion","created":CREATED,"model":"MODEL","choices":[{"index":0,"message":{"role":"assistant","content":"Hello!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17,"cost":2.45e-10}}
`

const goldenMidErr = `data: {"id":"chatcmpl-ID","object":"chat.completion.chunk","created":CREATED,"model":"MODEL","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-ID","object":"chat.completion.chunk","created":CREATED,"model":"MODEL","choices":[{"index":0,"delta":{"content":"partial answer"},"finish_reason":null}]}

data: {"error":{"message":"backend died mid-answer","type":"api_error","code":"backend_error"}}

data: [DONE]

`

const testAPIKey = "fx-consumer-key"

// script is one fake node's backend.
type script func(ctx context.Context, req wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error)

// fx is one test's world: a fake node listener, a fake router HTTP
// surface (route desk, receipts desk, chat fallback), and the client's
// proxy over a real NAT socket observed against a loopback UDP echo.
type fx struct {
	t         *testing.T
	rootPub   ed25519.PublicKey
	routerKey ed25519.PrivateKey
	cert      identity.RouterCert
	clientKey ed25519.PrivateKey
	natSock   *nat.Socket
	router    *httptest.Server
	proxy     *Server

	mu           sync.Mutex
	receiptPosts []map[string]any
	routeReqs    []map[string]any
	fallbacks    int

	// routeAnswer answers the desk (set per test; default: no candidates).
	routeAnswer func(w http.ResponseWriter, r *http.Request, body map[string]any)
	// nodeHomes are the nodes' home dirs (their receipt queues live in
	// <home>/receipts).
	nodeHomes []string
	// fallback answers the router's chat surface (default: 500).
	fallback func(w http.ResponseWriter, r *http.Request)
}

// fxLog honors FXLOG=1 (stderr, debug level) for debugging a failing
// fixture run.
func fxLog() *slog.Logger {
	dest := io.Discard
	level := slog.LevelInfo
	if os.Getenv("FXLOG") != "" {
		dest = os.Stderr
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(dest, &slog.HandlerOptions{Level: level}))
}

func newFX(t *testing.T) *fx {
	t.Helper()
	f := &fx{t: t}
	_, rootPriv, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	f.rootPub = rootPriv.Public().(ed25519.PublicKey)
	_, f.routerKey, err = identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := identity.SignRouterCert(rootPriv, f.routerKey.Public().(ed25519.PublicKey), time.Now().Add(24*time.Hour), 1)
	if err != nil {
		t.Fatal(err)
	}
	f.cert = cert
	_, f.clientKey, err = identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	// The NAT observation rides a loopback UDP echo speaking the real
	// observe protocol: the client learns 127.0.0.1:<port>, open.
	echo, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := echo.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if reply, ok := clientproto.ObserveAnswer(buf[:n], from.String()); ok {
				_, _ = echo.WriteToUDP(reply, from)
			}
		}
	}()
	f.natSock, err = nat.Bind(nat.Config{LANAddrs: func() []net.IP { return nil },
		Log: fxLog()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.natSock.Close)
	ectx, ecancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ecancel()
	ep, _ := net.ResolveUDPAddr("udp", echo.LocalAddr().String())
	if _, err := f.natSock.Observe(ectx, []nat.RouterEndpoints{{Main: ep, Alt: ep}}); err != nil {
		t.Fatalf("observe: %v", err)
	}

	// The fake router.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/direct/route", f.route)
	mux.HandleFunc("POST /api/v1/direct/receipts", f.receipt)
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.fallbacks++
		f.mu.Unlock()
		if f.fallback != nil {
			f.fallback(w, r)
			return
		}
		http.Error(w, "no fallback wired", http.StatusInternalServerError)
	})
	f.routeAnswer = func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		writeJSON(w, map[string]any{"direct": false, "reason": "disabled"})
	}
	f.router = httptest.NewServer(mux)
	t.Cleanup(f.router.Close)
	return f
}

// nodeEP is one fake node's dial info: its loopback endpoint and the
// public key its listener presents (what the route answer must name).
type nodeEP struct {
	Endpoint string
	NodePub  string
	NodeID   string
}

// addNode starts one scripted node listener.
func (f *fx) addNode(name string, h script) nodeEP {
	f.t.Helper()
	_, nodeKey, err := identity.Generate()
	if err != nil {
		f.t.Fatal(err)
	}
	home := f.t.TempDir()
	f.mu.Lock()
	f.nodeHomes = append(f.nodeHomes, home)
	f.mu.Unlock()
	l, err := ingress.StartClientListener(context.Background(), ingress.ClientListenerConfig{
		Identity: nodeKey,
		// The node-side root pin: exactly what production runs with
		// --router-root.
		RouterRoot: identity.PubString(f.rootPub),
		HomeDir:    home,
		Handler:    fxHandler(h),
		LANAddrs:   func() []net.IP { return nil },
		Log:        fxLog(),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(l.Close)
	// The listener binds the IPv6 wildcard; the client's (IPv4) NAT
	// socket dials the loopback spelling. Production endpoints come from
	// the router's observation and are real addresses already.
	return nodeEP{
		Endpoint: net.JoinHostPort("127.0.0.1", strconv.Itoa(l.Addr().Port)),
		NodePub:  identity.PubString(nodeKey.Public().(ed25519.PublicKey)),
		NodeID:   identity.ID(nodeKey.Public().(ed25519.PublicKey)),
	}
}

// candidate mints a signed ticket naming THIS node and answers the desk
// with it.
func (f *fx) candidate(node nodeEP, model string) func(w http.ResponseWriter, r *http.Request, body map[string]any) {
	f.t.Helper()
	return func(w http.ResponseWriter, _ *http.Request, body map[string]any) {
		now := time.Now()
		tk := clientproto.Ticket{
			V: clientproto.TicketVersion, ID: clientproto.NewTicketID(),
			RouterID:  identity.ID(f.routerKey.Public().(ed25519.PublicKey)),
			NodeID:    node.NodeID,
			ModelID:   model,
			ClientPub: identity.PubString(f.clientKey.Public().(ed25519.PublicKey)),
			PriceIn:   10, PriceOut: 25, MaxTokens: 64, MaxReqs: 1,
			NotBefore: now.Unix(), NotAfter: now.Add(clientproto.TicketLifetime).Unix(),
		}
		signed, err := clientproto.SignTicket(f.routerKey, f.cert, tk)
		if err != nil {
			f.t.Fatal(err)
		}
		writeJSON(w, map[string]any{
			"direct": true,
			"candidates": []map[string]any{{
				"node_id":                node.NodeID,
				"node_pub":               node.NodePub,
				"endpoints":              []string{node.Endpoint},
				"first_token_timeout_ms": 5000,
				"ticket":                 signed,
			}},
		})
	}
}

func (f *fx) route(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+testAPIKey {
		http.Error(w, "bad key", http.StatusUnauthorized)
		return
	}
	if r.Header.Get("X-Remote-Client") == "" {
		http.Error(w, "no version", http.StatusBadRequest)
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "body", http.StatusBadRequest)
		return
	}
	var doc map[string]any
	if json.Unmarshal(b, &doc) != nil {
		http.Error(w, "json", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.routeReqs = append(f.routeReqs, doc)
	f.mu.Unlock()
	f.routeAnswer(w, r, doc)
}

func (f *fx) receipt(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var doc map[string]any
	if json.Unmarshal(b, &doc) != nil {
		http.Error(w, "json", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.receiptPosts = append(f.receiptPosts, doc)
	f.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true})
}

// start builds the proxy against the fake router.
func (f *fx) start(direct bool) {
	f.t.Helper()
	var err error
	f.proxy, err = New(Config{
		Router: f.router.URL, APIKey: testAPIKey, ClientKey: f.clientKey,
		NAT: f.natSock, Direct: direct, Version: "0.1.0",
		Log: fxLog(),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(f.proxy.Close)
}

// post sends one chat request to the proxy and returns the response.
func (f *fx) post(body string) *httptest.ResponseRecorder {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	f.proxy.Handler().ServeHTTP(rec, req)
	return rec
}

// receipts waits for the async cross-check posts to land and returns
// them.
func (f *fx) receipts(n int) []map[string]any {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		got := len(f.receiptPosts)
		f.mu.Unlock()
		if got >= n {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any{}, f.receiptPosts...)
}

func (f *fx) fallbackCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fallbacks
}

// expectGolden compares rec's body to a golden with the response's own
// id/created/model substituted in.
func expectGolden(t *testing.T, rec *httptest.ResponseRecorder, golden string) {
	t.Helper()
	body := rec.Body.String()
	m := regexp.MustCompile(`"id":"(chatcmpl-[0-9a-f]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no response id in %q", firstLine(body))
	}
	c := regexp.MustCompile(`"created":(\d+)`).FindStringSubmatch(body)
	if c == nil {
		t.Fatalf("no created in %q", firstLine(body))
	}
	want := strings.NewReplacer("chatcmpl-ID", m[1], "CREATED", c[1], "MODEL", "test-model").Replace(golden)
	if body != want {
		t.Fatalf("bytes differ\n--- got ---\n%s\n--- want ---\n%s", body, want)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// handlerFunc adapts a script to InferenceHandler.
type fxHandler func(ctx context.Context, req wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error)

func (h fxHandler) HandleInference(ctx context.Context, req wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error) {
	return h(ctx, req, emit)
}

// goodScript streams the golden's chunks and reports 12 in / 5 out.
func goodScript(_ context.Context, req wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error) {
	for _, c := range []wire.InferenceChunkData{{Content: "Hel"}, {Content: "lo!"}, {FinishReason: "stop"}} {
		if err := emit(c); err != nil {
			return wire.InferenceDoneData{}, err
		}
	}
	return wire.InferenceDoneData{InputTokens: 12, OutputTokens: 5}, nil
}

// helloScript emits one content chunk (the non-streaming golden).
func helloScript(_ context.Context, req wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error) {
	if err := emit(wire.InferenceChunkData{Content: "Hello!"}); err != nil {
		return wire.InferenceDoneData{}, err
	}
	return wire.InferenceDoneData{InputTokens: 12, OutputTokens: 5}, nil
}

// dieScript emits a partial answer, then the backend dies (the node
// sends inference.error with output already streamed).
func dieScript(_ context.Context, req wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error) {
	if err := emit(wire.InferenceChunkData{Content: "partial answer"}); err != nil {
		return wire.InferenceDoneData{}, err
	}
	return wire.InferenceDoneData{}, errors.New("backend died mid-answer")
}

// busyScript refuses before any output: the next candidate's turn.
func busyScript(_ context.Context, req wire.InferenceRequestData, _ func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error) {
	return wire.InferenceDoneData{}, wire.ErrBusy
}

// TestGoldenStreamEqualsRouter: the same request through the direct path
// produces the router's exact stream bytes (its own golden), modulo the
// id and timestamp the responder owns.
func TestGoldenStreamEqualsRouter(t *testing.T) {
	f := newFX(t)
	ep := f.addNode("good", goodScript)
	f.routeAnswer = f.candidate(ep, "test-node-side")
	f.start(true)

	rec := f.post(`{"model":"test-model","stream":true,"stream_options":{"include_usage":true},
		"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	expectGolden(t, rec, goldenStream)

	// The cross-check landed with the physical counts.
	rs := f.receipts(1)
	if len(rs) != 1 {
		t.Fatalf("cross-check posts = %d", len(rs))
	}
	r := rs[0]
	if r["status"] != "ok" || r["chunks"].(float64) != 2 || r["bytes"].(float64) != 6 ||
		r["ticket_id"] == "" || r["request_id"] == "" {
		t.Fatalf("cross-check = %+v", r)
	}
	// And the router's chat surface was never touched.
	if n := f.fallbackCount(); n != 0 {
		t.Fatalf("fallback called %d times on the direct path", n)
	}
}

// TestGoldenBodyEqualsRouter: the non-streaming contract, byte for byte.
func TestGoldenBodyEqualsRouter(t *testing.T) {
	f := newFX(t)
	ep := f.addNode("good", helloScript)
	f.routeAnswer = f.candidate(ep, "test-node-side")
	f.start(true)

	rec := f.post(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	expectGolden(t, rec, goldenBody)
}

// TestFallbackOnDirectFalse: the desk's soft "use the router" answer is
// honored — the fallback answers, no direct attempt, no cross-check.
func TestFallbackOnDirectFalse(t *testing.T) {
	f := newFX(t)
	fallbackCalled := 0
	f.fallback = func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalled++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"router":true}` + "\n"))
	}
	f.start(true)

	rec := f.post(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK || rec.Body.String() != `{"router":true}`+"\n" {
		t.Fatalf("fallback response = %d %q", rec.Code, rec.Body.String())
	}
	if fallbackCalled != 1 || len(f.receipts(0)) != 0 {
		t.Fatalf("fallback=%d receipts=%d", fallbackCalled, len(f.receipts(0)))
	}
	// The desk was asked once and refused; nothing dials.
	if rs := f.routeReqs; len(rs) != 1 {
		t.Fatalf("route calls = %d", len(rs))
	}
}

// TestFallbackWhenAllCandidatesRefuse: every candidate refuses
// pre-token (a busy node here) and the router's chat surface answers.
func TestFallbackWhenAllCandidatesRefuse(t *testing.T) {
	f := newFX(t)
	ep := f.addNode("busy", busyScript)
	f.routeAnswer = f.candidate(ep, "test-node-side")
	refBody := `{"router":true}` + "\n"
	f.fallback = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(refBody))
	}
	f.start(true)

	rec := f.post(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK || rec.Body.String() != refBody {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
	if len(f.receipts(0)) != 0 {
		t.Fatalf("a refused request cross-checked: %+v", f.receipts(0))
	}
}

// TestSecondCandidateAfterBusy: the busy candidate is skipped pre-token
// and the next one answers — without the fallback.
func TestSecondCandidateAfterBusy(t *testing.T) {
	f := newFX(t)
	busyEP := f.addNode("busy", busyScript)
	goodEP := f.addNode("good", goodScript)

	busyCand := f.candidate(busyEP, "test-node-side")
	goodCand := f.candidate(goodEP, "test-node-side")
	f.routeAnswer = func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		rec := httptest.NewRecorder()
		busyCand(rec, nil, nil)
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		rec2 := httptest.NewRecorder()
		goodCand(rec2, nil, nil)
		var doc2 map[string]any
		if err := json.Unmarshal(rec2.Body.Bytes(), &doc2); err != nil {
			t.Fatal(err)
		}
		cands := doc["candidates"].([]any)
		cands = append(cands, doc2["candidates"].([]any)...)
		writeJSON(w, map[string]any{"direct": true, "candidates": cands})
	}
	f.fallback = func(w http.ResponseWriter, _ *http.Request) {
		t.Error("fallback must not run when a candidate answers")
	}
	f.start(true)

	rec := f.post(`{"model":"test-model","stream":true,"stream_options":{"include_usage":true},
		"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	expectGolden(t, rec, goldenStream)
	if n := f.fallbackCount(); n != 0 {
		t.Fatalf("fallback called %d times", n)
	}
}

// TestMidStreamDeathFrame: a node dying after the first token ends the
// response exactly like the router's failed_mid_stream — the SSE error
// frame + [DONE] — and the cross-check says failed_mid_stream with what
// was streamed.
func TestMidStreamDeathFrame(t *testing.T) {
	f := newFX(t)
	ep := f.addNode("die", dieScript)
	f.routeAnswer = f.candidate(ep, "test-node-side")
	f.fallback = func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a committed response must not fall back")
	}
	f.start(true)

	rec := f.post(`{"model":"test-model","stream":true,
		"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code) // headers committed before the death
	}
	expectGolden(t, rec, goldenMidErr)

	rs := f.receipts(1)
	if len(rs) != 1 || rs[0]["status"] != "failed_mid_stream" ||
		rs[0]["chunks"].(float64) != 1 || rs[0]["bytes"].(float64) != 14 {
		t.Fatalf("cross-check = %+v", rs)
	}
}

// TestNeedsRouterFallsBack: web_search, attachments and n > 1 go to the
// router without asking the desk.
func TestNeedsRouterFallsBack(t *testing.T) {
	f := newFX(t)
	f.fallback = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}` + "\n"))
	}
	f.start(true)
	for _, body := range []string{
		`{"model":"m","web_search":true,"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","attachments":[{"id":"a","name":"b"}],"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","n":3,"messages":[{"role":"user","content":"hi"}]}`,
	} {
		rec := f.post(body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", body, rec.Code)
		}
	}
	if rs := f.routeReqs; len(rs) != 0 {
		t.Fatalf("router-needed requests still asked the desk: %d", len(rs))
	}
}

// TestDirectOffFallsBack: the config switch sends everything to the
// router (the NAT socket stays unobserved — no route is even possible).
func TestDirectOffFallsBack(t *testing.T) {
	f := newFX(t)
	f.fallback = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}` + "\n"))
	}
	f.start(false)
	rec := f.post(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(f.routeReqs) != 0 {
		t.Fatalf("direct-off asked the desk")
	}
}

// TestRouteAdmissionSurfaced: the desk's non-200 (the same admission the
// router's chat runs) IS the answer — no fallback round trip.
func TestRouteAdmissionSurfaced(t *testing.T) {
	f := newFX(t)
	f.routeAnswer = func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"out of credits","type":"insufficient_credits","code":"insufficient_credits"}}`))
	}
	f.fallback = func(w http.ResponseWriter, _ *http.Request) {
		t.Error("an admission error must not fall back")
	}
	f.start(true)
	rec := f.post(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusPaymentRequired || !strings.Contains(rec.Body.String(), "insufficient_credits") {
		t.Fatalf("admission = %d %q", rec.Code, rec.Body.String())
	}
}

// TestForwardPassthrough: GET /v1/models and friends ride to the router
// with the client's key.
func TestForwardPassthrough(t *testing.T) {
	f := newFX(t)
	handled := false
	f.router.Config.Handler.(*http.ServeMux).HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		handled = true
		if r.Header.Get("Authorization") != "Bearer "+testAPIKey {
			t.Error("forwarded request carries no key")
		}
		writeJSON(w, map[string]any{"object": "list"})
	})
	f.start(false)
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1/v1/models", nil)
	rec := httptest.NewRecorder()
	f.proxy.Handler().ServeHTTP(rec, req)
	if !handled || rec.Code != http.StatusOK {
		t.Fatalf("forward: handled=%v code=%d", handled, rec.Code)
	}
}

// TestLocalTokenRequired: with a token set, a wrong token is rejected in
// constant time and the right one passes.
func TestLocalTokenRequired(t *testing.T) {
	f := newFX(t)
	f.fallback = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}` + "\n"))
	}
	f.start(false)
	f.proxy.cfg.LocalToken = "sekrit"

	post := func(setToken func(*http.Request)) *httptest.ResponseRecorder {
		req, _ := http.NewRequest(http.MethodPost, "http://local/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		if setToken != nil {
			setToken(req)
		}
		rec := httptest.NewRecorder()
		f.proxy.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := post(nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	if rec := post(func(r *http.Request) { r.Header.Set("Authorization", "Bearer sekrit") }); rec.Code != http.StatusOK {
		t.Fatalf("right token: %d", rec.Code)
	}
	if rec := post(func(r *http.Request) { r.Header.Set("X-Local-Token", "sekrit") }); rec.Code != http.StatusOK {
		t.Fatalf("token header: %d", rec.Code)
	}
	if rec := post(func(r *http.Request) { r.Header.Set("X-Local-Token", "nope") }); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", rec.Code)
	}
}

// TestHealthz.
func TestHealthz(t *testing.T) {
	f := newFX(t)
	f.start(false)
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1/healthz", nil)
	rec := httptest.NewRecorder()
	f.proxy.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != `{"ok":true}`+"\n" {
		t.Fatalf("healthz = %d %q", rec.Code, rec.Body.String())
	}
}

// TestDirectStreamFlushesFrameByFrame (R3): a real HTTP server (not a
// recorder — a recorder hides net/http's 4 KB buffering) consuming a
// direct stream whose node emits its last chunk 800 ms after its first:
// the first content frame must be readable well before the last chunk is
// sent, so the flush lands on every write and TTFT means TTFT.
func TestDirectStreamFlushesFrameByFrame(t *testing.T) {
	f := newFX(t)
	slow := func(ctx context.Context, _ wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error) {
		if err := emit(wire.InferenceChunkData{Content: "Hel"}); err != nil {
			return wire.InferenceDoneData{}, err
		}
		select {
		case <-ctx.Done():
			return wire.InferenceDoneData{}, ctx.Err()
		case <-time.After(800 * time.Millisecond):
		}
		if err := emit(wire.InferenceChunkData{Content: "lo!"}); err != nil {
			return wire.InferenceDoneData{}, err
		}
		return wire.InferenceDoneData{InputTokens: 12, OutputTokens: 5}, nil
	}
	ep := f.addNode("slow", slow)
	f.routeAnswer = f.candidate(ep, "test-node-side")
	f.start(true)

	ts := httptest.NewServer(f.proxy.Handler())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	start := time.Now()
	var firstContent time.Time
	sawDone := false
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			sawDone = true
			break
		}
		if firstContent.IsZero() && strings.Contains(payload, `"content":"Hel"`) {
			firstContent = time.Now()
		}
	}
	if sc.Err() != nil {
		t.Fatalf("read: %v", sc.Err())
	}
	total := time.Since(start)
	if firstContent.IsZero() {
		t.Fatalf("no first content frame in %.1f s", total.Seconds())
	}
	if d := firstContent.Sub(start); d > 500*time.Millisecond {
		t.Fatalf("first content frame after %s — the stream arrived buffered (last chunk sent at ~800 ms)", d)
	}
	if total < 750*time.Millisecond {
		t.Fatalf("stream ended after %s — the node's chunks were not spaced", total)
	}
	if !sawDone {
		t.Fatal("no [DONE]")
	}
}

// TestDefaultRouterClientStreamsLongBodies (R4): the default client must
// carry no overall http.Client.Timeout (it covers reading the body — a
// router chat stream longer than it was cut mid-answer) and keep a
// transport-level header bound instead; a spaced fallback stream must
// reach the consumer intact and incrementally under it.
func TestDefaultRouterClientStreamsLongBodies(t *testing.T) {
	f := newFX(t)
	fallbackCalls := 0
	f.fallback = func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		for _, part := range []string{"one", "two", "three"} {
			_, _ = w.Write([]byte("data: {\"content\":\"" + part + "\"}\n\n"))
			_ = rc.Flush()
			select {
			case <-rctxDone(w):
			case <-time.After(200 * time.Millisecond):
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		_ = rc.Flush()
	}
	f.start(false) // everything through the router, on the default client

	// The shape: no overall timeout, a 120 s header bound.
	if f.proxy.hc.Timeout != 0 {
		t.Fatalf("default client Timeout = %s — it would cut long streams mid-answer", f.proxy.hc.Timeout)
	}
	tr, ok := f.proxy.hc.Transport.(*http.Transport)
	if !ok || tr.ResponseHeaderTimeout != 120*time.Second {
		t.Fatalf("transport = %T, ResponseHeaderTimeout = %v — first tokens need a slow header bound", f.proxy.hc.Transport, func() time.Duration {
			if tr, ok := f.proxy.hc.Transport.(*http.Transport); ok {
				return tr.ResponseHeaderTimeout
			}
			return -1
		}())
	}

	// The behavior: the spaced stream arrives whole, in order.
	ts := httptest.NewServer(f.proxy.Handler())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	start := time.Now()
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	total := time.Since(start)
	if fallbackCalls != 1 || total < 550*time.Millisecond {
		t.Fatalf("fallback=%d total=%s — the spaced stream did not ride through", fallbackCalls, total)
	}
	for _, part := range []string{"one", "two", "three"} {
		if !strings.Contains(string(body), `"content":"`+part+`"`) {
			t.Fatalf("body lost %q: %s", part, body)
		}
	}
	if !strings.Contains(string(body), "data: [DONE]") {
		t.Fatalf("body lost [DONE]: %s", body)
	}
}

// rctxDone surfaces the request context behind a ResponseWriter (the
// fixture's fallback answers cancel the wait if the consumer walks off).
func rctxDone(w http.ResponseWriter) <-chan struct{} {
	type ctxer interface{ Context() context.Context }
	if c, ok := w.(ctxer); ok {
		return c.Context().Done()
	}
	return nil
}

// TestLoopbackGateWithoutToken (R5): with no local token, only loopback
// hosts are served (a rebound or remote name is refused) and an Origin
// header — which browsers attach to every cross-origin request and
// curl/OpenAI SDKs never send — must itself name a loopback host. A
// token set disables the gate entirely.
func TestLoopbackGateWithoutToken(t *testing.T) {
	f := newFX(t)
	f.fallback = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}` + "\n"))
	}
	f.start(false)

	try := func(host string, origin string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, "http://"+host+"/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		f.proxy.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	for _, host := range []string{"localhost:4141", "127.0.0.1:4141", "[::1]:4141", "localhost", "127.0.0.1", "[::1]"} {
		if code := try(host, ""); code != http.StatusOK {
			t.Fatalf("loopback host %q: code = %d, want 200", host, code)
		}
	}
	for _, host := range []string{"evil.example:4141", "198.51.100.7:4141", "127.0.0.1.evil.example", "localhost.evil.example"} {
		if code := try(host, ""); code != http.StatusForbidden {
			t.Fatalf("non-loopback host %q: code = %d, want 403 (DNS rebinding)", host, code)
		}
	}
	for _, origin := range []string{"https://evil.example", "http://198.51.100.7:8080", "localhost.evil.example", "not-a-url"} {
		if code := try("127.0.0.1:4141", origin); code != http.StatusForbidden {
			t.Fatalf("foreign Origin %q: code = %d, want 403 (CSRF)", origin, code)
		}
	}
	// A local dev page (vite on ::1) may talk to the endpoint.
	if code := try("127.0.0.1:4141", "http://[::1]:5173"); code != http.StatusOK {
		t.Fatalf("loopback Origin: code = %d, want 200", code)
	}

	// With a token, the gate is off (the token is the check).
	f.proxy.cfg.LocalToken = "sekrit"
	if code := try("evil.example:4141", ""); code != http.StatusUnauthorized {
		t.Fatalf("tokened non-loopback host: code = %d, want 401 from the token check", code)
	}
}

// TestDirectStallAfterFirstTokenEndsMidStream (R6): a node that sends one
// chunk and then stalls with the stream open (no done, no FIN) must not
// hang the consumer — with a short stall bound, the answer ends in the
// mid-stream error frame within roughly the bound, and nothing falls
// back (the first token committed).
func TestDirectStallAfterFirstTokenEndsMidStream(t *testing.T) {
	f := newFX(t)
	stall := func(ctx context.Context, _ wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error) {
		if err := emit(wire.InferenceChunkData{Content: "partial"}); err != nil {
			return wire.InferenceDoneData{}, err
		}
		<-ctx.Done() // the stall: silence, stream stays open
		return wire.InferenceDoneData{}, ctx.Err()
	}
	ep := f.addNode("staller", stall)
	f.routeAnswer = f.candidate(ep, "test-node-side")
	f.fallback = func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a committed response must not fall back")
	}
	f.start(true)
	f.proxy.cfg.Stall = 300 * time.Millisecond

	rec := f.post(`{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code) // headers committed before the stall
	}
	body := rec.Body.String()
	if !strings.Contains(body, "the node stalled mid-answer") ||
		!strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stall response = %q", firstLine(body))
	}
	rs := f.receipts(1)
	if len(rs) != 1 || rs[0]["status"] != "failed_mid_stream" {
		t.Fatalf("cross-check = %+v", rs)
	}
}

// TestDirectConsumerGoneStopsPulling (R6 follow-up): once the consumer
// walks away, the per-chunk stall re-arm must not override the
// consumer-gone deadline — the client stops reading (the node's emit
// fails, its cut-off receipt bills only what went out) instead of
// pulling the whole answer for nobody.
func TestDirectConsumerGoneStopsPulling(t *testing.T) {
	f := newFX(t)
	var mu sync.Mutex
	sent := 0
	fast := func(ctx context.Context, _ wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error) {
		for i := 0; i < 2000; i++ {
			if err := emit(wire.InferenceChunkData{Content: "tok "}); err != nil {
				return wire.InferenceDoneData{}, err
			}
			mu.Lock()
			sent++
			mu.Unlock()
			select {
			case <-ctx.Done():
				return wire.InferenceDoneData{}, ctx.Err()
			case <-time.After(2 * time.Millisecond):
			}
		}
		return wire.InferenceDoneData{OutputTokens: 2000}, nil
	}
	ep := f.addNode("fast", fast)
	f.routeAnswer = f.candidate(ep, "test-node-side")
	f.fallback = func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a committed response must not fall back")
	}
	f.start(true)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(150*time.Millisecond, cancel)
	start := time.Now()
	f.proxy.Handler().ServeHTTP(httptest.NewRecorder(), req)
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("handler ran %s after the consumer left", el)
	}
	// The node notices the reset, its handler returns, and it queues the
	// cut-off receipt on disk: wait for that file, so the test's temp dir
	// is quiet before cleanup.
	f.mu.Lock()
	home := f.nodeHomes[0]
	f.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if es, _ := os.ReadDir(filepath.Join(home, "receipts")); len(es) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the node never queued its cut-off receipt")
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	n := sent
	mu.Unlock()
	if n >= 2000 {
		t.Fatalf("the node streamed the whole answer (%d chunks) to a gone consumer", n)
	}
}

// TestOverflowHandsToRouter (R8, corrected): a pre-token context-length
// overflow on one node is not final — a wider node may fit the prompt,
// and the router owns the window-aware failover. The client stops trying
// the desk's candidates (the next one is never asked) and falls back to
// the router once; the router's answer is the response.
func TestOverflowHandsToRouter(t *testing.T) {
	f := newFX(t)
	overflowNode := func(_ context.Context, _ wire.InferenceRequestData, _ func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error) {
		// vLLM's wording, backend_error-coded: what a node relays today.
		return wire.InferenceDoneData{}, errors.New(`backend: This model's maximum context length is 8192 tokens. However, your request has 123456 input tokens, please reduce the prompt length`)
	}
	goodSeen := 0
	good := func(_ context.Context, _ wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error) {
		goodSeen++
		return helloScript(context.Background(), wire.InferenceRequestData{}, emit)
	}
	badEP := f.addNode("overflow", overflowNode)
	goodEP := f.addNode("good", good)

	badCand := f.candidate(badEP, "test-node-side")
	goodCand := f.candidate(goodEP, "test-node-side")
	f.routeAnswer = func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		rec := httptest.NewRecorder()
		badCand(rec, nil, nil)
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		rec2 := httptest.NewRecorder()
		goodCand(rec2, nil, nil)
		var doc2 map[string]any
		if err := json.Unmarshal(rec2.Body.Bytes(), &doc2); err != nil {
			t.Fatal(err)
		}
		writeJSON(w, map[string]any{"direct": true,
			"candidates": append(doc["candidates"].([]any), doc2["candidates"].([]any)...)})
	}
	f.fallback = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"from": "router"})
	}
	f.start(true)

	rec := f.post(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"router"`) {
		t.Fatalf("status = %d body = %s (want the router's answer)", rec.Code, rec.Body.String())
	}
	if goodSeen != 0 {
		t.Fatalf("the next direct candidate was still asked %d times", goodSeen)
	}
	if n := f.fallbackCount(); n != 1 {
		t.Fatalf("fallback = %d, want exactly 1", n)
	}
}

// TestOverflowErrorClasses (R8): what counts as a window overflow.
func TestOverflowErrorClasses(t *testing.T) {
	for _, tc := range []struct {
		code, msg string
		want      bool
	}{
		{"busy", "node at max_concurrent", false},         // capacity: the next node answers
		{"backend_error", "backend died", false},          // transient: retry elsewhere
		{"ticket_used", "ticket was already used", false}, // per-ticket: the next ticket is fresh
		{"backend_error", "This model's maximum context length is 8192 tokens. However, your request has 123456 input tokens", true}, // vLLM
		{"backend_error", "prompt is too long: 9000 tokens > 8192 maximum", true},                                                    // Anthropic-compatible
		{"backend_error", "input exceeds the available context size", true},                                                          // llama.cpp
		{"context_length_exceeded", "", true},                                                                                        // a node that codes it itself
	} {
		if got := overflowError(tc.code, tc.msg); got != tc.want {
			t.Fatalf("overflowError(%q, %q) = %v, want %v", tc.code, firstLine(tc.msg), got, tc.want)
		}
	}
}

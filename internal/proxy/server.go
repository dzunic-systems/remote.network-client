// Package proxy is remote-client serve: a local, OpenAI-compatible HTTP
// endpoint. POST /v1/chat/completions goes straight to a serving node
// over QUIC when a direct path exists — ticketed, pinned, rendered with
// the same response renderer the router drives — and through the router
// otherwise (a request needing router-side features never tries direct:
// web search, attachments, n > 1). Everything else is forwarded to the
// router verbatim. The API key rides only these internal requests, is
// never logged, printed or put in a URL.
package proxy

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/quic-go/quic-go"

	"remote.network/client/internal/conn"
	"remote.network/client/internal/nat"
	"remote.network/node/clientproto"
	"remote.network/node/identity"
	"remote.network/node/wire"
)

// maxRequestBody mirrors the router's chat body cap: images ride chat
// bodies as base64.
const maxRequestBody = 32 << 20

// routeCandidates asks the desk for up to four candidates; the client
// tries them in order.
const routeCandidates = 3

// routeTimeout bounds the /direct/route round trip.
const routeTimeout = 10 * time.Second

// Server is the local endpoint.
type Server struct {
	cfg  Config
	hc   *http.Client
	pool *conn.Pool
	log  *slog.Logger
}

// Config builds a Server.
type Config struct {
	// Router is the router base URL (https://…).
	Router string
	// APIKey is the consumer key for every router-bound request. Never
	// logged.
	APIKey string
	// LocalToken, when set, is required on every request to the local
	// endpoint (constant-time compared). Empty = loopback trust.
	LocalToken string
	// ClientKey is this client's identity (the TLS certificate the node
	// sees; tickets bind it).
	ClientKey ed25519.PrivateKey
	// NAT is the observed socket; its transport is what direct dials ride.
	NAT *nat.Socket
	// Direct off sends everything through the router.
	Direct bool
	// Stall bounds the silence between two chunks of a direct answer
	// after the first token (default 120 s, the router's stall bound in
	// chat.go). tryCandidate used to clear the read deadline at the first
	// chunk, so a node that stalled without closing hung the consumer
	// forever (R6) — the QUIC idle timeout is minutes and not a contract.
	Stall time.Duration
	// Version rides the route request (X-Remote-Client).
	Version string
	// HTTP is the router HTTP client (nil = defaultRouterClient: no
	// overall timeout, transport-level phase timeouts).
	HTTP *http.Client
	Log  *slog.Logger
}

// defaultRouterClient is the client for the surfaces that stream: no
// overall http.Client.Timeout — that bound covers reading the body, so a
// fallback (router) chat stream or a forwarded long response longer than
// it is cut mid-answer (R4). The transport still bounds each phase
// (response headers, dial, TLS) and every request rides its consumer's
// context; the route-desk call and the cross-check post keep their own
// short context deadlines on top.
func defaultRouterClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 120 * time.Second // first tokens can be slow
	return &http.Client{Transport: t}
}

// New builds the server (and, on the direct path, its connection pool
// over the NAT transport).
func New(cfg Config) (*Server, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.HTTP == nil {
		cfg.HTTP = defaultRouterClient()
	}
	if cfg.Direct && cfg.NAT == nil {
		return nil, errors.New("proxy: the direct path needs a NAT socket")
	}
	s := &Server{
		cfg: cfg,
		hc:  cfg.HTTP,
		log: cfg.Log.With("component", "proxy"),
	}
	if cfg.NAT != nil {
		s.pool = conn.New(cfg.NAT.Transport(), cfg.ClientKey)
	}
	return s, nil
}

// Close shuts the pool down.
func (s *Server) Close() { s.pool.Close() }

// Handler is the local HTTP surface.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.LocalToken == "" && !loopbackGate(r) {
			s.apiError(w, http.StatusForbidden, "invalid_request_error", "loopback_only",
				"the local endpoint accepts loopback requests only; run serve with --local-token to use it over the network")
			return
		}
		if err := s.authorize(r); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid local token","type":"invalid_request_error","code":"invalid_local_token"}}` + "\n"))
			return
		}
		switch {
		case r.URL.Path == "/healthz":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}` + "\n"))
		case r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost:
			s.chat(w, r)
		default:
			s.forward(w, r)
		}
	})
}

// authorize checks the optional --local-token in constant time.
func (s *Server) authorize(r *http.Request) error {
	if s.cfg.LocalToken == "" {
		return nil
	}
	got := r.Header.Get("X-Local-Token")
	if got == "" {
		got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.LocalToken)) != 1 {
		return errors.New("token mismatch")
	}
	return nil
}

// loopbackHosts are the host spellings the no-token endpoint accepts
// (port stripped) — in the request's Host and in an Origin header.
var loopbackHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}

// loopbackGate is the no-token trust model (R5). Loopback listening
// means local programs may use the endpoint — a web page must not: any
// page the user visits can POST to 127.0.0.1 cross-origin and spend
// their credits, and through DNS rebinding one can even read responses
// across every router surface the key reaches. So a request with no
// local token must name a loopback host (a rebound name does not), and
// an Origin header — browsers attach it to every cross-origin request;
// curl and the OpenAI SDKs never send one — must itself name a loopback
// host.
func loopbackGate(r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	} else {
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]") // "[::1]" (default port): SplitHostPort wants a port
	}
	if !loopbackHosts[host] {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || !loopbackHosts[u.Hostname()] {
			return false
		}
	}
	return true
}

// needsRouter is a request the direct path cannot serve — the router's
// own features ride only on its HTTP surface.
func needsRouter(req *clientproto.ChatRequest, n int) bool {
	if req.WebSearch || len(req.Attachments) > 0 || n > 1 {
		return true
	}
	return false
}

// hasVision reports whether any message carries image parts.
func hasVision(req *clientproto.ChatRequest) bool {
	for i := range req.Messages {
		if len(req.Messages[i].ImageParts()) > 0 {
			return true
		}
	}
	return false
}

// promptBytes is the router's input estimator's byte count: message text,
// tool-call names and arguments, tool declarations.
func promptBytes(req *clientproto.ChatRequest) int {
	n := 0
	for i := range req.Messages {
		m := &req.Messages[i]
		n += len(m.Text())
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
	}
	for _, t := range req.Tools {
		n += len(t.Function.Name) + len(t.Function.Description) + len(t.Function.Parameters)
	}
	return n
}

// chat runs one completion: route, try the candidates in order, fall back
// to the router when no candidate answers before its first token.
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		s.apiError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "", "request body too large")
		return
	}
	var req clientproto.ChatRequest
	var extra struct {
		N int `json:"n"`
	}
	if err := json.Unmarshal(body, &req); err != nil || json.Unmarshal(body, &extra) != nil {
		s.apiError(w, http.StatusBadRequest, "invalid_request_error", "", "malformed request body")
		return
	}
	if req.Model == "" || len(req.Messages) == 0 {
		s.apiError(w, http.StatusBadRequest, "invalid_request_error", "",
			"'model' and non-empty 'messages' are required")
		return
	}
	if req.Temperature != nil && (*req.Temperature < 0 || *req.Temperature > 2) {
		s.apiError(w, http.StatusBadRequest, "invalid_request_error", "",
			"temperature must be between 0 and 2")
		return
	}

	if s.cfg.Direct && !needsRouter(&req, extra.N) && s.tryDirect(w, r, body, &req) {
		return
	}
	s.fallback(w, r, body)
}

// tryDirect runs the route → candidates loop. true = the response was
// written (or the consumer is gone writing it); false = fall back.
func (s *Server) tryDirect(w http.ResponseWriter, r *http.Request, body []byte, req *clientproto.ChatRequest) bool {
	cands, state := s.route(w, r, req)
	switch state {
	case routeSurfaced:
		return true // the desk's admission error IS the response
	case routeFallback:
		return false
	}
	for _, cand := range cands {
		switch s.tryCandidate(w, r, req, cand) {
		case outcomeDone, outcomeCommitted:
			return true
		case outcomeRefused:
			continue
		case outcomeRouter:
			return false
		}
	}
	return false // every candidate refused pre-token: the router answers
}

// routeState is the desk call's verdict.
type routeState int

const (
	routeFallback routeState = iota // try the router
	routeSurfaced                   // an admission error was written; done
	routeOK                         // candidates in hand
)

// routeDoc is the desk's soft answer.
type routeDoc struct {
	Direct     bool             `json:"direct"`
	Reason     string           `json:"reason,omitempty"`
	Candidates []routeCandidate `json:"candidates,omitempty"`
}

type routeCandidate struct {
	NodeID              string             `json:"node_id"`
	NodePub             string             `json:"node_pub"`
	Endpoints           []string           `json:"endpoints"`
	FirstTokenTimeoutMs int64              `json:"first_token_timeout_ms"`
	Ticket              clientproto.Ticket `json:"ticket"`
}

// route asks the desk. A non-200 is surfaced verbatim (the desk runs the
// same admission the router's chat does — the error the client would get
// from the fallback, one round trip earlier). direct:false →
// routeFallback.
func (s *Server) route(w http.ResponseWriter, r *http.Request, req *clientproto.ChatRequest) ([]routeCandidate, routeState) {
	info := s.cfg.NAT.Info()
	if info.Public == "" {
		return nil, routeFallback // no observed path yet: nothing to name
	}
	ask, err := json.Marshal(map[string]any{
		"model":             req.Model,
		"conv_hash":         clientproto.ConvHash(req.Messages),
		"max_tokens":        req.MaxTokens,
		"est_prompt_tokens": (promptBytes(req) + 3) / 4,
		"needs": map[string]bool{
			"tools": len(req.Tools) > 0, "vision": hasVision(req),
		},
		"stream":       req.Stream,
		"client_pub":   identityPub(s.cfg.ClientKey),
		"client_nat":   info.NAT,
		"client_addrs": info.Addrs,
		"n":            routeCandidates,
	})
	if err != nil {
		return nil, routeFallback
	}
	rctx, cancel := context.WithTimeout(r.Context(), routeTimeout)
	defer cancel()
	req2, err := http.NewRequestWithContext(rctx, http.MethodPost,
		strings.TrimSuffix(s.cfg.Router, "/")+"/api/v1/direct/route", bytes.NewReader(ask))
	if err != nil {
		return nil, routeFallback
	}
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+s.cfg.APIKey) // never in a URL, never logged
	req2.Header.Set("X-Remote-Client", wireVersion(s.cfg.Version))
	resp, err := s.hc.Do(req2)
	if err != nil {
		s.log.Debug("route failed — falling back", "err", err)
		return nil, routeFallback
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		// The router saw this machine at another public address than the
		// last observation named: the network changed (a VPN went up or
		// down). That is the desk's refusal, not the request's — recheck
		// now and send this one through the router.
		if resp.StatusCode == http.StatusBadRequest && apiErrorCode(b) == "client_addr_mismatch" {
			s.log.Debug("public address changed — rechecking, this request goes through the router")
			s.cfg.NAT.Recheck()
			s.pool.ForgetFailures()
			return nil, routeFallback
		}
		// Admission (402/403/…): the same answer the fallback would get.
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(b)
		s.logRequest("fallback", "", 0, 0, 0, resp.StatusCode)
		return nil, routeSurfaced
	}
	var doc routeDoc
	if json.Unmarshal(b, &doc) != nil || !doc.Direct || len(doc.Candidates) == 0 {
		s.log.Debug("no direct route — falling back", "reason", doc.Reason)
		return nil, routeFallback // disabled, no direct node, client too old
	}
	return doc.Candidates, routeOK
}

// outcome is one candidate's attempt result.
type outcome int

const (
	outcomeRefused   outcome = iota // died before the first token: try the next
	outcomeCommitted                // the response is on the wire — a post-token death: over
	outcomeDone                     // completed
	outcomeRouter                   // pre-token, and only the router can choose better (a window overflow): fall back now
)

// defaultStallTimeout is the router's stall bound (chat.go's
// firstTokenTimeoutFor loading ceiling): the longest silence a direct
// answer may sit in after a chunk.
const defaultStallTimeout = 120 * time.Second

// stall is this server's configured stall bound.
func (s *Server) stall() time.Duration {
	if s.cfg.Stall > 0 {
		return s.cfg.Stall
	}
	return defaultStallTimeout
}

// tryCandidate serves the request over one candidate's direct connection,
// writing the consumer response exactly as the router would have. The
// first token commits: a death after it ends the response with the
// router's mid-stream error frame; before it, the next candidate gets its
// turn (and the eventual fallback).
func (s *Server) tryCandidate(w http.ResponseWriter, r *http.Request, req *clientproto.ChatRequest, cand routeCandidate) outcome {
	start := time.Now()
	_, stream, ok := s.openStream(r, cand)
	if !ok {
		return outcomeRefused
	}
	// Leaving this attempt, for any reason, stops the node: Close only
	// ends our send side, so without CancelRead (STOP_SENDING) a node
	// whose consumer walked off keeps streaming into the receive window
	// and bills the whole answer. After a completed answer it is a no-op.
	defer func() {
		stream.CancelRead(0)
		_ = stream.Close()
	}()
	// The consumer walking away must unblock the read: force the stream's
	// deadline. (QUIC reads do not watch the request context.)
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-r.Context().Done():
			_ = stream.SetReadDeadline(time.Now())
		case <-watchDone:
		}
	}()
	defer close(watchDone)

	reqID := newRequestID()
	inference := req.ToInference(cand.Ticket.ModelID)
	// The node clamps MaxTokens to the ticket's cap; never send a bigger
	// ask than the ticket allows.
	if cand.Ticket.MaxTokens > 0 && (inference.MaxTokens <= 0 || inference.MaxTokens > cand.Ticket.MaxTokens) {
		inference.MaxTokens = cand.Ticket.MaxTokens
	}
	if err := clientproto.SendDirectRequest(stream, clientproto.DirectRequestData{
		Ticket: cand.Ticket, RequestID: reqID, Request: inference,
	}, 5*time.Second); err != nil {
		s.pool.Drop(cand.NodeID)
		return outcomeRefused
	}

	// First-token window: the desk's number for this node.
	ftt := time.Duration(cand.FirstTokenTimeoutMs) * time.Millisecond
	if ftt <= 0 {
		ftt = 60 * time.Second
	}
	_ = stream.SetReadDeadline(start.Add(ftt))

	render := newRender(w, req, cand.Ticket)
	sc := clientproto.NewEnvelopeScanner(stream)
	for sc.Scan() {
		var env wire.Envelope
		if json.Unmarshal(sc.Bytes(), &env) != nil {
			continue
		}
		switch env.Type {
		case wire.TypeInferenceChunk:
			var chunk wire.InferenceChunkData
			if json.Unmarshal(env.Data, &chunk) != nil {
				continue
			}
			if !render.started {
				if !render.begin() {
					return outcomeCommitted // headers/role failed: over
				}
			}
			// Each chunk owes the next one within the stall bound (R6):
			// the first-token window no longer applies, but silence is
			// still bounded. On expiry the read fails and the stream-end
			// path below ends the response as for a node death.
			// The consumer-gone watcher's deadline must win: re-arming
			// after it fired would keep pulling (and paying for) the whole
			// answer for nobody. Checked before every re-arm.
			if r.Context().Err() != nil {
				render.chunk(chunk) // counted for the cross-check, written to nobody
				s.after(reqID, cand, render, "failed_mid_stream")
				return outcomeCommitted
			}
			_ = stream.SetReadDeadline(time.Now().Add(s.stall()))
			render.chunk(chunk)
		case wire.TypeInferenceDone:
			var done wire.InferenceDoneData
			if json.Unmarshal(env.Data, &done) != nil {
				continue
			}
			if !render.started && !render.begin() {
				return outcomeCommitted
			}
			render.finish(done)
			s.after(reqID, cand, render, "ok")
			s.logRequest("direct", cand.NodeID, time.Since(start).Milliseconds(),
				done.InputTokens, done.OutputTokens, http.StatusOK)
			return outcomeDone
		case wire.TypeInferenceError:
			var e wire.ErrorData
			if json.Unmarshal(env.Data, &e) != nil {
				continue
			}
			if !render.started {
				if overflowError(e.Code, e.Message) {
					// The prompt does not fit THIS node's window. Another
					// node may have a wider one, so this is not final — but
					// the desk's candidates carry no window, and the router
					// already does the window-aware failover over the whole
					// pool (and answers the 400 when nothing fits). Hand the
					// request to it now instead of burning the remaining
					// direct tickets on blind retries.
					s.log.Debug("candidate overflowed — the router picks a wider window",
						"node_id", cand.NodeID, "code", e.Code)
					return outcomeRouter
				}
				// Pre-token refusal — busy, ticket refused, whatever: the
				// next candidate (or the fallback) answers. The connection
				// itself is fine (the node spoke), so the pool keeps it.
				s.log.Debug("candidate refused pre-token", "node_id", cand.NodeID, "code", e.Code)
				return outcomeRefused
			}
			// The answer began; this is the router's failed_mid_stream end.
			render.fail(e.Message)
			s.after(reqID, cand, render, "failed_mid_stream")
			s.logRequest("direct", cand.NodeID, time.Since(start).Milliseconds(),
				0, render.resp.Chunks(), http.StatusBadGateway)
			return outcomeCommitted
		}
	}
	// The stream ended without a done (node death, deadline, consumer
	// gone). Pre-token: the next candidate answers, and a deadline-stale
	// connection should not linger in the pool. Post-token: the answer is
	// over, mid-stream-error frame like the router's — phrased by what
	// the read saw (R6: a stall deadline is not a node death).
	if !render.started {
		s.log.Debug("candidate ended before its first token", "node_id", cand.NodeID, "err", sc.Err())
		s.pool.Drop(cand.NodeID)
		return outcomeRefused
	}
	msg := "the node closed the stream mid-answer"
	if scErr := sc.Err(); errors.Is(scErr, os.ErrDeadlineExceeded) {
		msg = fmt.Sprintf("the node stalled mid-answer for over %s", s.stall())
	}
	render.fail(msg)
	s.after(reqID, cand, render, "failed_mid_stream")
	s.logRequest("direct", cand.NodeID, time.Since(start).Milliseconds(),
		0, render.resp.Chunks(), http.StatusBadGateway)
	return outcomeCommitted
}

// openStream gets a pooled connection (dialing when needed) and opens a
// stream on it. One stale-connection retry: a pool hit whose stream fails
// is dropped and re-dialed once.
func (s *Server) openStream(r *http.Request, cand routeCandidate) (*quic.Conn, *quic.Stream, bool) {
	for attempt := 0; attempt < 2; attempt++ {
		c, err := s.pool.Get(r.Context(), cand.NodeID, cand.NodePub, cand.Endpoints)
		if err != nil {
			s.log.Debug("dial failed", "node_id", cand.NodeID, "err", err)
			return nil, nil, false
		}
		st, err := c.OpenStreamSync(r.Context())
		if err == nil {
			return c, st, true
		}
		s.pool.Drop(cand.NodeID)
	}
	return nil, nil, false
}

// render owns the consumer response for one attempt. Nothing is written
// until the first token: begin commits the SSE headers + role chunk (or,
// non-streaming, nothing — the body renders whole at finish). After that
// every failure is final, shaped exactly like the router's.
type render struct {
	w       http.ResponseWriter
	req     *clientproto.ChatRequest
	ticket  clientproto.Ticket
	resp    *clientproto.Response
	started bool
	stream  bool
}

func newRender(w http.ResponseWriter, req *clientproto.ChatRequest, ticket clientproto.Ticket) *render {
	id := "chatcmpl-" + newRequestID()
	// The router's rule: a streaming renderer writes each SSE frame as it
	// folds; a non-streaming one only accumulates, and finish renders the
	// body from the accumulator. The streaming writer flushes after every
	// write (R3) — net/http's 4 KB buffer would hold the frames until it
	// fills or the answer ends, and the direct answer would arrive in
	// bursts with a TTFT nobody measured.
	var rw io.Writer
	if req.Stream {
		rw = flushWriter{w: w, rc: http.NewResponseController(w)}
	}
	rd := &render{
		w: w, req: req, ticket: ticket, stream: req.Stream,
		resp: clientproto.NewResponse(rw, id, req.Model, time.Now().Unix()),
	}
	// Cost prices the usage blocks; self-use is free (the ticket says).
	rd.resp.Cost = func(in, out int) float64 {
		if ticket.SelfUse {
			return 0
		}
		return float64(in*ticket.PriceIn+out*ticket.PriceOut) / 1e12
	}
	return rd
}

// flushWriter flushes after every write — the SSE frames go out one by
// one, and a proxy must never buffer them. The ResponseController is
// built on the original writer (a wrapping writer's Flush must reach the
// response anyway), and covers EmitRole: the role chunk rides the same
// writer, so the headers commit visibly the moment it emits.
type flushWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	_ = f.rc.Flush()
	return n, err
}

// begin commits the response. Streaming: the SSE headers and the OpenAI
// role chunk, like the router's relayStream. Reports success.
func (rd *render) begin() bool {
	rd.started = true
	if !rd.stream {
		return true
	}
	h := rd.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	rd.w.WriteHeader(http.StatusOK)
	return rd.resp.EmitRole() == nil
}

func (rd *render) chunk(c wire.InferenceChunkData) { rd.resp.Fold(c) }

// finish writes the terminal shape: SSE finish (+ usage when asked) +
// [DONE], or the whole completion body.
func (rd *render) finish(done wire.InferenceDoneData) {
	in, out := done.InputTokens, done.OutputTokens
	if rd.stream {
		_ = rd.resp.EmitFinish()
		if rd.req.StreamOptions != nil && rd.req.StreamOptions.IncludeUsage {
			_ = rd.resp.EmitUsage(in, out, done.Reasoning)
		}
		_ = rd.resp.EmitDone()
		return
	}
	b := rd.resp.Body(in, out, done.Reasoning, nil)
	h := rd.w.Header()
	h.Set("Content-Type", "application/json")
	rd.w.WriteHeader(http.StatusOK)
	_, _ = rd.w.Write(append(b, '\n'))
}

// fail ends a committed response the router's way: the SSE error event +
// [DONE], or a 502 JSON error for the non-streaming contract.
func (rd *render) fail(msg string) {
	if msg == "" {
		msg = "backend error"
	}
	if rd.stream {
		_ = rd.resp.EmitError("api_error", "backend_error", msg)
		return
	}
	b, _ := json.Marshal(map[string]any{
		"error": map[string]string{
			"message": msg, "type": "api_error", "code": "backend_error",
		},
	})
	rd.w.Header().Set("Content-Type", "application/json")
	rd.w.WriteHeader(http.StatusBadGateway)
	_, _ = rd.w.Write(append(b, '\n'))
}

// after posts the client's own count for the ticketed request — the
// cross-check (billing is the node's receipt; this only adds the verdict's
// inputs). The client can only know physical bounds: what it counted, and
// the bytes it spelled. Async; failures are log lines.
func (s *Server) after(reqID string, cand routeCandidate, rd *render, status string) {
	chunks, nbytes := rd.resp.Chunks(), rd.resp.Bytes()
	out := chunks
	if b4 := (nbytes + 3) / 4; b4 > out {
		out = b4
	}
	body := map[string]any{
		"ticket_id": cand.Ticket.ID, "request_id": reqID,
		"output_tokens": out, "status": status,
		"chunks": chunks, "bytes": nbytes,
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		b, err := json.Marshal(body)
		if err != nil {
			return
		}
		req2, err := http.NewRequestWithContext(ctx, http.MethodPost,
			strings.TrimSuffix(s.cfg.Router, "/")+"/api/v1/direct/receipts", bytes.NewReader(b))
		if err != nil {
			return
		}
		req2.Header.Set("Content-Type", "application/json")
		req2.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)
		resp, err := s.hc.Do(req2)
		if err != nil {
			s.log.Debug("cross-check post failed", "ticket_id", cand.Ticket.ID, "err", err)
			return
		}
		resp.Body.Close()
	}()
}

// fallback forwards the original body to the router's chat surface and
// streams the answer through unchanged.
func (s *Server) fallback(w http.ResponseWriter, r *http.Request, body []byte) {
	start := time.Now()
	req2, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		strings.TrimSuffix(s.cfg.Router, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		s.apiError(w, http.StatusInternalServerError, "api_error", "", "internal error")
		return
	}
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)
	for _, h := range []string{"X-Title", "HTTP-Referer"} {
		if v := r.Header.Get(h); v != "" {
			req2.Header.Set(h, v)
		}
	}
	resp, err := s.hc.Do(req2)
	if err != nil {
		s.apiError(w, http.StatusBadGateway, "api_error", "router_unreachable",
			"the router did not answer: "+err.Error())
		return
	}
	defer resp.Body.Close()
	h := w.Header()
	for _, k := range []string{"Content-Type", "Retry-After"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 8<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // consumer gone; the router's metering already saw it
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	s.logRequest("fallback", "", time.Since(start).Milliseconds(), 0, 0, resp.StatusCode)
}

// forward proxies everything else (models, admin surfaces the key can
// see, …) to the router verbatim, injecting the API key.
func (s *Server) forward(w http.ResponseWriter, r *http.Request) {
	target := strings.TrimSuffix(s.cfg.Router, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req2, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		s.apiError(w, http.StatusInternalServerError, "api_error", "", "internal error")
		return
	}
	for _, h := range []string{"Content-Type", "Accept"} {
		if v := r.Header.Get(h); v != "" {
			req2.Header.Set(h, v)
		}
	}
	req2.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)
	resp, err := s.hc.Do(req2)
	if err != nil {
		s.apiError(w, http.StatusBadGateway, "api_error", "router_unreachable",
			"the router did not answer: "+err.Error())
		return
	}
	defer resp.Body.Close()
	h := w.Header()
	for _, k := range []string{"Content-Type", "Retry-After"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// apiError writes the router's JSON error shape.
func (s *Server) apiError(w http.ResponseWriter, status int, typ, code, message string) {
	if code == "" {
		code = "api_error"
	}
	b, _ := json.Marshal(map[string]any{
		"error": map[string]string{"message": message, "type": typ, "code": code},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

// logRequest is the one line per request: path, node, TTFT, tokens.
func (s *Server) logRequest(path, node string, ms int64, in, out, status int) {
	s.log.Info("request", "path", path, "node", node, "ms", ms,
		"in", in, "out", out, "status", status)
}

func identityPub(key ed25519.PrivateKey) string {
	if key == nil {
		return ""
	}
	pub, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return ""
	}
	return identity.PubString(pub)
}

// wireVersion is the build version as the router's gate parses it: bare
// dotted numbers. Release builds are stamped "v0.1.0" (the tag); a
// leading v made v0.1.0 routers answer client_too_old to every route. A
// pre-release or build suffix ("-dirty", "+meta") is dropped too.
func wireVersion(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	return v
}

// newRequestID is a random 12-byte hex id (the router's req-id shape).
func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// apiErrorCode reads error.code from a router error body ("" when absent).
func apiErrorCode(b []byte) string {
	var doc struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return ""
	}
	return doc.Error.Code
}

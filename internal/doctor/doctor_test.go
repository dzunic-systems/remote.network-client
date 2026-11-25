package doctor

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"remote.network/node/clientproto"
	"remote.network/node/identity"
	"remote.network/node/ingress"
)

// echoSocket is a router punch socket: it answers observe requests.
func echoSocket(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if reply, ok := clientproto.ObserveAnswer(buf[:n], from.String()); ok {
				_, _ = c.WriteToUDP(reply, from)
			}
		}
	}()
	return c
}

// TestDoctorAgainstNodeListener: the doctor walks the whole path against a
// fake router and a REAL node client listener (the public node module's),
// and reports a working direct path.
func TestDoctorAgainstNodeListener(t *testing.T) {
	main, alt := echoSocket(t), echoSocket(t)
	nodePub, nodeKey, _ := identity.Generate()
	var listener *ingress.ClientListener
	var gotDoctor map[string]any
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/direct/info":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"router_id": "r", "udp_addr": main.LocalAddr().String(), "alt_udp_addr": alt.LocalAddr().String(),
			})
		case "/api/v1/direct/doctor":
			if r.Header.Get("Authorization") != "Bearer rn_sk_test" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewDecoder(r.Body).Decode(&gotDoctor)
			ep := net.JoinHostPort("127.0.0.1", strconv.Itoa(listener.Addr().Port))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"node_id": identity.ID(nodePub), "node_pub": identity.PubString(nodePub),
				"endpoints": []string{ep}, "node_nat": "open", "pairing": "direct", "agent_version": "test",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer router.Close()
	l, err := ingress.StartClientListener(context.Background(), ingress.ClientListenerConfig{
		Identity: nodeKey, RouterURLs: []string{router.URL}, AgentVersion: "test",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), LANAddrs: func() []net.IP { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	listener = l

	_, clientKey, _ := identity.Generate()
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := Run(ctx, Options{Router: router.URL, APIKey: "rn_sk_test", ClientKey: clientKey,
		LANAddrs: func() []net.IP { return nil }}, &out)
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out.String())
	}
	if !res.Connected || res.Pong == nil || res.Pong.NodeID != identity.ID(nodePub) || res.NAT != "open" {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(out.String(), "direct path OK") {
		t.Errorf("output:\n%s", out.String())
	}
	if gotDoctor["client_pub"] != identity.PubString(clientKey.Public().(ed25519.PublicKey)) {
		t.Errorf("doctor request = %+v", gotDoctor)
	}
}

func TestDoctorRouterErrorIsReadable(t *testing.T) {
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"this router has no QUIC endpoint","code":"direct_unavailable"}}`))
	}))
	defer router.Close()
	_, key, _ := identity.Generate()
	var out bytes.Buffer
	_, err := Run(context.Background(), Options{Router: router.URL, APIKey: "k", ClientKey: key}, &out)
	if err == nil || !strings.Contains(err.Error(), "no QUIC endpoint") {
		t.Errorf("err = %v", err)
	}
}

// Package doctor is `remote-client doctor`: can this machine reach a node
// directly? It learns the machine's public address and NAT class from a
// router's observe echo, asks the router for a node running a client
// listener (the router tells that node to open its NAT toward us), dials
// the node over QUIC with its key pinned, and pings it. Every step prints
// what it found, so a failure says where the path breaks.
package doctor

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"remote.network/client/internal/nat"
	"remote.network/node/clientproto"
	"remote.network/node/identity"
	"remote.network/node/wire"
)

// Options for one doctor run.
type Options struct {
	Router    string // router base URL (https://…)
	APIKey    string // rn_sk_…
	Model     string // check a node serving this model ("" = any listener)
	NodeID    string // check this node ("" = the router picks)
	ClientKey ed25519.PrivateKey
	HTTP      *http.Client
	Version   string
	// LANAddrs lists this machine's LAN addresses (nil = discover).
	LANAddrs func() []net.IP
}

// Result is what a run found (also printed step by step).
type Result struct {
	Public      string
	NAT         string
	NodeID      string
	Pairing     string
	Connected   bool
	HandshakeMs int64
	Pong        *wire.DirectPongData
}

// Run performs the check, printing to out. A nil error means the node
// answered over the direct path.
func Run(ctx context.Context, o Options, out io.Writer) (Result, error) {
	var res Result
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	if o.LANAddrs == nil {
		o.LANAddrs = lanAddrs
	}
	base := strings.TrimSuffix(o.Router, "/")
	say := func(format string, a ...any) { fmt.Fprintf(out, format+"\n", a...) }

	// 1. The router's punch sockets.
	var info struct {
		RouterID   string `json:"router_id"`
		UDPAddr    string `json:"udp_addr"`
		AltUDPAddr string `json:"alt_udp_addr"`
	}
	if err := getJSON(ctx, o.HTTP, base+"/api/v1/direct/info", &info); err != nil {
		say("router          %s — no direct endpoint: %v", base, err)
		return res, err
	}
	main, err1 := net.ResolveUDPAddr("udp", info.UDPAddr)
	alt, err2 := net.ResolveUDPAddr("udp", info.AltUDPAddr)
	if err := errors.Join(err1, err2); err != nil {
		return res, fmt.Errorf("router's direct endpoint: %w", err)
	}
	say("router          %s (QUIC %s)", base, info.UDPAddr)

	// 2. One socket for everything: observe, then dial from the same
	// mapping the router saw. The observe loop is the nat package's (the
	// same code `serve` runs continuously).
	sock, err := nat.Bind(nat.Config{LANAddrs: o.LANAddrs})
	if err != nil {
		return res, err
	}
	defer sock.Close()
	obs, err := sock.Observe(ctx, []nat.RouterEndpoints{{Main: main, Alt: alt}})
	if err != nil {
		say("this machine    no answer from the router over UDP — a firewall blocks outbound UDP?")
		return res, err
	}
	res.Public, res.NAT = obs.Public, obs.NAT
	say("this machine    %s, NAT %s", obs.Public, obs.NAT)

	// 3. Ask for a node; the router tells it to open its NAT toward us.
	addrs := obs.Addrs
	var doc struct {
		NodeID       string   `json:"node_id"`
		NodePub      string   `json:"node_pub"`
		Endpoints    []string `json:"endpoints"`
		NodeNAT      string   `json:"node_nat"`
		Pairing      string   `json:"pairing"`
		AgentVersion string   `json:"agent_version"`
	}
	err = postJSON(ctx, o.HTTP, base+"/api/v1/direct/doctor", o.APIKey, map[string]any{
		"node_id": o.NodeID, "model": o.Model,
		"client_pub":   identity.PubString(o.ClientKey.Public().(ed25519.PublicKey)),
		"client_nat":   obs.NAT,
		"client_addrs": addrs,
	}, &doc)
	if err != nil {
		say("node            %v", err)
		return res, err
	}
	res.NodeID, res.Pairing = doc.NodeID, doc.Pairing
	say("node            %s (agent %s), NAT %s, endpoints %s",
		short(doc.NodeID), doc.AgentVersion, doc.NodeNAT, strings.Join(doc.Endpoints, ", "))
	switch doc.Pairing {
	case "lan":
		say("path            same network as the node: dialing its LAN address first")
	case "private":
		say("path            trying the node's LAN addresses first (they work if your network routes to them, e.g. a VPN or site link), then its public address")
	}
	if doc.Pairing == "relay" {
		say("verdict         a direct path is not expected between these NATs (symmetric on one side); requests will go through a router")
	}

	// 4. Dial and ping.
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	conn, err := clientproto.DialNode(dctx, sock.Transport(), doc.Endpoints, o.ClientKey, doc.NodePub)
	if err != nil {
		say("direct QUIC     failed: %v", err)
		if doc.Pairing != "relay" {
			say("verdict         no direct path; requests will go through a router (a firewall may block inbound UDP on one side)")
		}
		return res, err
	}
	defer conn.CloseWithError(0, "doctor done")
	res.Connected, res.HandshakeMs = true, time.Since(start).Milliseconds()
	pong, err := clientproto.Ping(dctx, conn)
	if err != nil {
		say("direct QUIC     connected to %s in %d ms, but the ping failed: %v", conn.RemoteAddr(), res.HandshakeMs, err)
		return res, err
	}
	res.Pong = &pong
	say("direct QUIC     connected to %s in %d ms; node answered (serving direct requests: %v)",
		conn.RemoteAddr(), res.HandshakeMs, pong.Serving)
	say("verdict         direct path OK")
	return res, nil
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12] + "…"
	}
	return id
}

// apiError is the router's error shape.
type apiError struct {
	Error struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
}

func decodeOrError(resp *http.Response, into any) error {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e apiError
		if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
			return fmt.Errorf("%s (%s)", e.Error.Message, resp.Status)
		}
		return fmt.Errorf("%s", resp.Status)
	}
	return json.Unmarshal(body, into)
}

func getJSON(ctx context.Context, hc *http.Client, url string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	return decodeOrError(resp, into)
}

func postJSON(ctx context.Context, hc *http.Client, url, key string, body, into any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	return decodeOrError(resp, into)
}

// lanAddrs lists the machine's private IPv4 addresses.
func lanAddrs() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if ip4 := ipn.IP.To4(); ip4 != nil && ip4.IsPrivate() && len(out) < 3 {
				out = append(out, ip4)
			}
		}
	}
	return out
}

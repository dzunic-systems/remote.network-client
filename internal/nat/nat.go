// Package nat owns the client's one long-lived UDP socket: the QUIC
// transport every direct dial rides (from the same mapping the router
// observed), and the observe loop that keeps the machine's public address
// and NAT class current — one round at start, then every 25 s. When a
// second router is configured, its punch socket joins the test so an
// address-dependent mapping (a per-destination-IP translation) is caught,
// the same fold the node listener runs when its fleet knows two routers.
package nat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"remote.network/node/clientproto"
	"remote.network/node/wire"
)

// Observe cadence and per-round timeout (the doctor's numbers).
const (
	observeEvery    = 25 * time.Second
	observeTimeout  = 4 * time.Second
	dialTimeout     = 5 * time.Second
	observeResend   = 200 * time.Millisecond
	roundRetryUnset = 5 * time.Second // retry a failed round sooner than the cadence
)

// RouterEndpoints is one router's punch sockets (GET /api/v1/direct/info).
type RouterEndpoints struct {
	Main *net.UDPAddr
	Alt  *net.UDPAddr
}

// Info is the latest observation.
type Info struct {
	// Public is the router-observed public address ("" = none yet —
	// outbound UDP answered nothing so far; the direct path is not
	// attempted and requests use the router).
	Public string
	// NAT is open | cone | symmetric | unknown.
	NAT string
	// Addrs is what a route request names: the public address first,
	// then this machine's LAN addresses at the socket's port (a client on
	// the node's network dials those directly).
	Addrs []string
}

// Config tunes the socket.
type Config struct {
	// LANAddrs overrides the machine's LAN address discovery (tests).
	LANAddrs func() []net.IP
	Log      *slog.Logger
}

// Socket is the long-lived UDP socket, its transport and its observation.
type Socket struct {
	sock *net.UDPConn
	tr   *quic.Transport
	obs  *clientproto.Observer
	cfg  Config
	log  *slog.Logger

	mu   sync.Mutex
	info Info
}

// Bind opens the socket and transport. No network traffic happens until
// Observe/Run.
func Bind(cfg Config) (*Socket, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.LANAddrs == nil {
		cfg.LANAddrs = lanAddrs
	}
	sock, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		return nil, fmt.Errorf("bind udp: %w", err)
	}
	tr := &quic.Transport{Conn: sock}
	s := &Socket{
		sock: sock, tr: tr, cfg: cfg, log: cfg.Log.With("component", "nat"),
		obs: clientproto.NewObserver(func(b []byte, to net.Addr) error {
			_, err := tr.WriteTo(b, to)
			return err
		}),
	}
	// The transport's single non-QUIC reader: observe replies land here.
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err := tr.ReadNonQUICPacket(context.Background(), buf)
			if err != nil {
				return
			}
			s.obs.Deliver(buf[:n])
		}
	}()
	return s, nil
}

// Transport is the QUIC transport direct dials ride.
func (s *Socket) Transport() *quic.Transport { return s.tr }

// Info snapshots the latest observation.
func (s *Socket) Info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

// Observe runs one round now: both of the first router's sockets, plus a
// cross-router round against the second router's main socket when one is
// given (and the first verdict says punchable). Stores and returns the
// result. The public address alone survives a partial round — the class
// then stays unknown, which routes nothing direct but changes nothing
// else.
func (s *Socket) Observe(ctx context.Context, routers []RouterEndpoints) (Info, error) {
	if len(routers) == 0 || routers[0].Main == nil {
		return Info{}, errors.New("no router punch endpoint")
	}
	local := s.sock.LocalAddr().String()
	octx, cancel := context.WithTimeout(ctx, observeTimeout)
	defer cancel()
	obs, err := s.obs.Observe(octx, local, routers[0].Main, routers[0].Alt)
	if err != nil {
		return Info{}, err
	}
	// One router's two ports catch a NAT that maps per destination port;
	// a second router's IP is what catches a mapping per destination IP.
	mapping := "one router"
	if obs.Candidate && len(routers) > 1 && routers[1].Main != nil {
		cctx, ccancel := context.WithTimeout(ctx, observeTimeout)
		cross, xerr := s.obs.Observe(cctx, local, routers[0].Main, routers[1].Main)
		ccancel()
		obs, mapping = combineNAT(obs, cross, xerr)
	}
	info := s.store(obs)
	s.log.Debug("observe round", "public", info.Public, "nat", info.NAT, "tested_against", mapping)
	return info, nil
}

// combineNAT folds the cross-router round into the first round's verdict:
// a mapping that differs between two router IPs is address-dependent —
// symmetric for hole punching. (The node listener keeps its own copy of
// this fold in node/ingress; this is the client's.)
func combineNAT(first, cross clientproto.Observation, crossErr error) (clientproto.Observation, string) {
	switch {
	case crossErr != nil:
		return first, "one router (second router did not answer)"
	case cross.NAT == wire.NATSymmetric:
		first.NAT, first.Candidate = wire.NATSymmetric, false
		return first, "two routers: address-dependent mapping"
	default:
		return first, "two routers"
	}
}

func (s *Socket) store(obs clientproto.Observation) Info {
	port := s.sock.LocalAddr().(*net.UDPAddr).Port
	info := Info{Public: obs.Main, NAT: obs.NAT}
	if info.Public != "" {
		info.Addrs = append(info.Addrs, info.Public)
		for _, ip := range s.cfg.LANAddrs() {
			ep := net.JoinHostPort(ip.String(), fmt.Sprint(port))
			if ep != info.Public {
				info.Addrs = append(info.Addrs, ep)
			}
		}
	}
	s.mu.Lock()
	s.info = info
	s.mu.Unlock()
	return info
}

// Run keeps the observation current until ctx ends: a round now (retrying
// sooner when it fails — serve must degrade to the router, never stall on
// it), then every 25 s. fetch returns the routers' punch endpoints fresh
// each round.
func (s *Socket) Run(ctx context.Context, fetch func(ctx context.Context) ([]RouterEndpoints, error)) {
	next := roundRetryUnset
	for {
		rctx, cancel := context.WithTimeout(ctx, observeTimeout+2*time.Second)
		routers, err := fetch(rctx)
		if err == nil {
			if _, err = s.Observe(rctx, routers); err == nil {
				next = observeEvery
			}
		}
		cancel()
		if err != nil {
			s.mu.Lock()
			got := s.info.Public != ""
			s.mu.Unlock()
			if !got {
				s.log.Warn("no public address yet — requests go through the router until UDP answers", "err", err)
			} else {
				s.log.Debug("observe round failed — keeping the last verdict", "err", err)
			}
			next = roundRetryUnset
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(next):
		}
	}
}

// Close shuts the transport and socket down.
func (s *Socket) Close() {
	_ = s.tr.Close()
	_ = s.sock.Close()
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

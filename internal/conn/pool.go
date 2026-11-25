// Package conn pools the client's direct QUIC connections across
// requests: one connection per node, keyed by node id, pinned to the node
// key the router named (a changed key means the node re-keyed — the old
// connection is dead to us), reused until it sits idle for 60 s.
package conn

import (
	"context"
	"crypto/ed25519"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"remote.network/node/clientproto"
)

// IdleTTL is how long an unused connection stays open.
const IdleTTL = 60 * time.Second

// DialTimeout bounds one handshake (all endpoints raced inside it).
const DialTimeout = 5 * time.Second

type entry struct {
	conn *quic.Conn
	pub  string
	used time.Time
}

// Pool is the connection cache.
type Pool struct {
	tr        *quic.Transport
	clientKey ed25519.PrivateKey

	mu     sync.Mutex
	conns  map[string]*entry // node id → connection
	closed bool
}

// New builds a pool over the client's transport (the NAT socket's, so
// dials come from the observed mapping).
func New(tr *quic.Transport, clientKey ed25519.PrivateKey) *Pool {
	p := &Pool{tr: tr, clientKey: clientKey, conns: map[string]*entry{}}
	go p.janitor()
	return p
}

// Get returns a live connection to the node, dialing one when the pool
// has none (or one pinned to a different key). The connection is NOT
// health-checked here — a caller whose first stream fails on it calls
// Drop and asks again.
func (p *Pool) Get(ctx context.Context, nodeID, nodePub string, endpoints []string) (*quic.Conn, error) {
	p.mu.Lock()
	if e := p.conns[nodeID]; e != nil && e.pub == nodePub {
		e.used = time.Now()
		c := e.conn
		p.mu.Unlock()
		return c, nil
	}
	p.mu.Unlock()

	dctx, cancel := context.WithTimeout(ctx, DialTimeout)
	defer cancel()
	c, err := clientproto.DialNode(dctx, p.tr, endpoints, p.clientKey, nodePub)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = c.CloseWithError(0, "pool closed")
		return nil, ErrClosed
	}
	// One connection per node: a racing dial's loser goes away.
	if old := p.conns[nodeID]; old != nil && old.conn != c {
		go old.conn.CloseWithError(0, "replaced")
	}
	p.conns[nodeID] = &entry{conn: c, pub: nodePub, used: time.Now()}
	p.mu.Unlock()
	return c, nil
}

// Drop forgets a connection that failed at the stream level (the node
// went away while the pool held it): the next Get dials fresh.
func (p *Pool) Drop(nodeID string) {
	p.mu.Lock()
	e := p.conns[nodeID]
	delete(p.conns, nodeID)
	p.mu.Unlock()
	if e != nil {
		go e.conn.CloseWithError(0, "stale")
	}
}

// ErrClosed is Get's answer after Close.
var ErrClosed = errClosed{}

type errClosed struct{}

func (errClosed) Error() string { return "connection pool closed" }

func (p *Pool) janitor() {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for now := range t.C {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return
		}
		var stale []*entry
		for id, e := range p.conns {
			if now.Sub(e.used) > IdleTTL {
				stale = append(stale, e)
				delete(p.conns, id)
			}
		}
		p.mu.Unlock()
		for _, e := range stale {
			_ = e.conn.CloseWithError(0, "idle")
		}
	}
}

// Close shuts every pooled connection down.
func (p *Pool) Close() {
	p.mu.Lock()
	p.closed = true
	conns := make([]*entry, 0, len(p.conns))
	for id, e := range p.conns {
		conns = append(conns, e)
		delete(p.conns, id)
	}
	p.mu.Unlock()
	for _, e := range conns {
		_ = e.conn.CloseWithError(0, "pool closed")
	}
}

package conn

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// TestFailCooldown: a failed dial marks the node for FailCooldown (same
// endpoints); a new endpoint set, the end of the cooldown, or
// ForgetFailures dials again.
func TestFailCooldown(t *testing.T) {
	sock, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	tr := &quic.Transport{Conn: sock}
	defer tr.Close()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	p := New(tr, key)
	defer p.Close()
	now := time.Now()
	p.now = func() time.Time { return now }

	dead := []string{"127.0.0.1:1"}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	_, err = p.Get(ctx, "n1", "pub", dead)
	cancel()
	if err == nil {
		t.Fatal("dial to a dead endpoint succeeded")
	}
	// The caller's context ended the dial: no verdict on the node.
	if _, ok := p.failed["n1|127.0.0.1:1"]; ok {
		t.Fatal("a dial the caller cut short marked the node")
	}

	p.mu.Lock()
	p.failed["n1|127.0.0.1:1"] = now.Add(FailCooldown)
	p.mu.Unlock()
	if _, err := p.Get(context.Background(), "n1", "pub", dead); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("inside cooldown = %v, want ErrUnreachable", err)
	}
	p.ForgetFailures()
	if len(p.failed) != 0 {
		t.Fatal("ForgetFailures left cooldowns")
	}
	p.failed["n1|127.0.0.1:1"] = now.Add(FailCooldown)
	now = now.Add(FailCooldown + time.Second)
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := p.Get(ctx, "n1", "pub", dead); errors.Is(err, ErrUnreachable) {
		t.Fatal("cooldown outlived FailCooldown")
	}
}

// TestUnreachableReport (A1): the route request's report — one entry per
// cooled-down node id (however many endpoint sets are cooling), expired
// entries dropped, none when the pool is clean.
func TestUnreachableReport(t *testing.T) {
	sock, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	tr := &quic.Transport{Conn: sock}
	defer tr.Close()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	p := New(tr, key)
	defer p.Close()
	now := time.Now()
	p.now = func() time.Time { return now }

	if ids := p.Unreachable(); len(ids) != 0 {
		t.Fatalf("clean pool reported %v", ids)
	}
	p.mu.Lock()
	p.failed["n1|127.0.0.1:1"] = now.Add(FailCooldown)
	p.failed["n1|10.0.0.1:1"] = now.Add(FailCooldown) // same node, another endpoint set
	p.failed["n2|127.0.0.1:2"] = now.Add(FailCooldown)
	p.failed["n3|127.0.0.1:3"] = now.Add(-time.Second) // already expired
	p.mu.Unlock()
	got := map[string]bool{}
	for _, id := range p.Unreachable() {
		got[id] = true
	}
	if len(got) != 2 || !got["n1"] || !got["n2"] {
		t.Fatalf("unreachable = %v, want n1 (deduped) + n2, no expired n3", got)
	}
}

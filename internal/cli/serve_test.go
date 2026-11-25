package cli

import "testing"

// TestLoopbackListen is the serve surface's safety rule: anything that
// accepts connections from the network needs --local-token, loopback
// does not.
func TestLoopbackListen(t *testing.T) {
	for _, c := range []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:4141", true}, {"localhost:4141", true}, {"[::1]:4141", true},
		{"127.1.2.3:4141", true}, // the loopback /8
		{"0.0.0.0:4141", false}, {"[::]:4141", false}, {":4141", false},
		{"203.0.113.5:4141", false}, // documentation range, not loopback {"example.invalid:4141", false},
		{"no-port-at-all", false},
	} {
		got, err := isLoopbackListen(c.addr)
		if c.addr == "no-port-at-all" {
			if err == nil {
				t.Errorf("%s: expected a parse error", c.addr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.addr, err)
			continue
		}
		if got != c.ok {
			t.Errorf("isLoopbackListen(%q) = %v, want %v", c.addr, got, c.ok)
		}
	}
}

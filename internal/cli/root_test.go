package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	Version = "v0.0.0-test"
	var out bytes.Buffer
	root := newRoot()
	root.SetOut(&out)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "remote-client v0.0.0-test (") {
		t.Errorf("version output = %q", out.String())
	}
}

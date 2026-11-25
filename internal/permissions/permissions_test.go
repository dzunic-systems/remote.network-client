package permissions

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("REMOTE_HOME", "")
	return home
}

func TestLoadMissingIsAllAsk(t *testing.T) {
	isolate(t)
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.Mode("fx__echo") != Ask {
		t.Fatalf("default mode = %s, want ask", s.Mode("fx__echo"))
	}
}

func TestSetPersistsAndReloads0600(t *testing.T) {
	isolate(t)
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("fx__echo", Allow); err != nil {
		t.Fatal(err)
	}
	if s.Mode("fx__echo") != Allow || s.Mode("fx__slow") != Ask {
		t.Fatalf("modes = echo:%s slow:%s", s.Mode("fx__echo"), s.Mode("fx__slow"))
	}
	if err := s.Set("fx__echo", "sometimes"); err == nil {
		t.Fatal("an unknown mode must be refused")
	}
	path, _ := Path()
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("permissions.json perm = %o, want 0600", fi.Mode().Perm())
		}
	}
	// Reload sees it; a pre-existing loose file is tightened.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"fx__echo": "allow"`) {
		t.Fatalf("file = %s", b)
	}
	s2, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if s2.Mode("fx__echo") != Allow {
		t.Fatalf("reload lost the mode: %s", s2.Mode("fx__echo"))
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s2.Set("fx__slow", Deny); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("loose file not tightened: %o", fi.Mode().Perm())
	}
}

func TestRemoteHomeMovesTheFile(t *testing.T) {
	isolate(t)
	moved := t.TempDir()
	t.Setenv("REMOTE_HOME", moved)
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("fx__echo", Deny); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(moved, "permissions.json")); err != nil {
		t.Fatalf("permissions.json not in REMOTE_HOME: %v", err)
	}
}

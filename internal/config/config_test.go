package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestWritesTightenLooseFiles (R7): os.WriteFile's mode applies only on
// create — a file left 0644 by an older version (or a careless copy)
// stays loose forever. Both writers must tighten it to 0600.
func TestWritesTightenLooseFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits (the user-profile ACL protects the files)")
	}
	t.Setenv("HOME", t.TempDir())

	// The key file, via its explicit path.
	keyPath := filepath.Join(t.TempDir(), "client-api-key")
	if err := os.WriteFile(keyPath, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteKeyFile(keyPath, "fx-key"); err != nil {
		t.Fatal(err)
	}
	assertPerm(t, keyPath, 0o600)
	if k, err := ReadKeyFile(keyPath); err != nil || k != "fx-key" {
		t.Fatalf("key = %q err = %v", k, err)
	}

	// The config file, via the default path (HOME redirected above).
	cfgPath, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("router: https://old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Save(Config{Router: "https://new"}); err != nil {
		t.Fatal(err)
	}
	assertPerm(t, cfgPath, 0o600)
}

func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Fatalf("%s perm = %o, want %o (os.WriteFile never tightens an existing file)", path, got, want)
	}
}

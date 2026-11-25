// Package permissions is the local tools' per-tool permission modes:
// allow | ask | deny, one entry per exposed tool name, stored in
// ~/.remote/permissions.json (0600). The default for every tool is ask —
// nothing runs without the user having said so at least once.
package permissions

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Modes.
const (
	Allow = "allow"
	Ask   = "ask"
	Deny  = "deny"
)

// Valid reports whether s is a mode.
func Valid(s string) bool {
	return s == Allow || s == Ask || s == Deny
}

// Store is the on-disk permission map.
type Store struct {
	path  string
	mu    sync.Mutex
	modes map[string]string
}

// Path is the permissions file's location (~/.remote/permissions.json).
func Path() (string, error) {
	dir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if h := os.Getenv("REMOTE_HOME"); h != "" {
		dir = h
	} else {
		dir = filepath.Join(dir, ".remote")
	}
	return filepath.Join(dir, "permissions.json"), nil
}

// Load reads the store. A missing file is an empty store (every tool
// asks), not an error.
func Load() (*Store, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	s := &Store{path: path, modes: map[string]string{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	var doc struct {
		Tools map[string]string `json:"tools"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for name, mode := range doc.Tools {
		if !Valid(mode) {
			return nil, fmt.Errorf("%s: %q has mode %q (allow|ask|deny)", path, name, mode)
		}
		s.modes[name] = mode
	}
	return s, nil
}

// NewMemory is an empty store with nowhere to persist (the fallback when
// the permissions file is unreadable: every tool asks; an "always" is
// refused rather than silently forgotten — the turn still runs).
func NewMemory() *Store {
	return &Store{modes: map[string]string{}}
}

// Mode is the mode for one exposed tool name (default ask).
func (s *Store) Mode(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.modes[name]; Valid(m) {
		return m
	}
	return Ask
}

// Set writes one mode and persists. Unknown modes are refused.
func (s *Store) Set(name, mode string) error {
	if !Valid(mode) {
		return fmt.Errorf("%q is not a mode (allow|ask|deny)", mode)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modes[name] = mode
	return s.saveLocked()
}

// All returns a sorted copy of every set mode.
func (s *Store) All() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.modes))
	for k, v := range s.modes {
		out[k] = v
	}
	return out
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	names := make([]string, 0, len(s.modes))
	for n := range s.modes {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("{\n  \"tools\": {\n")
	for i, n := range names {
		comma := ","
		if i == len(names)-1 {
			comma = ""
		}
		fmt.Fprintf(&b, "    %q: %q%s\n", n, s.modes[n], comma)
	}
	b.WriteString("  }\n}\n")
	if err := os.WriteFile(s.path, []byte(b.String()), 0o600); err != nil {
		return err
	}
	// WriteFile's mode applies only on create (R7).
	return os.Chmod(s.path, 0o600)
}

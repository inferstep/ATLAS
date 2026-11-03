package sessionstore

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// testHome is the folder the whole test run uses in place of the user's own.
var testHome string

// TestMain keeps every test away from the real user folders. The store's tests
// save sessions, and the session folder hangs off os.UserCacheDir, which reads
// $XDG_CACHE_HOME on Linux and $HOME on macOS. Both point at one temporary
// folder for the run; isolateSessions gives a single test a fresh one.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "atlas-tui-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot create a folder for the test run:", err)
		os.Exit(1)
	}
	testHome = dir
	os.Setenv("HOME", dir)
	os.Setenv("XDG_CACHE_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestSessionsOfTheRunStayInItsOwnFolder(t *testing.T) {
	dir, err := sessionsDir()
	if err != nil {
		t.Fatalf("sessionsDir: %v", err)
	}
	if !strings.HasPrefix(dir, testHome) {
		t.Fatalf("sessions folder %q is outside the folder of the test run %q", dir, testHome)
	}
}

func TestIsolateSessionsGivesATestItsOwnFolder(t *testing.T) {
	isolateSessions(t)
	first, err := sessionsDir()
	if err != nil {
		t.Fatalf("sessionsDir: %v", err)
	}
	if strings.HasPrefix(first, testHome) {
		t.Fatalf("sessions folder %q is still the shared folder of the run", first)
	}
	if err := Save(Session{ID: "only-here", Messages: []ChatMessage{{Role: RoleUser, Body: "x"}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	isolateSessions(t)
	second, err := sessionsDir()
	if err != nil {
		t.Fatalf("sessionsDir: %v", err)
	}
	if second == first {
		t.Fatalf("a second call kept the folder %q", first)
	}
	sessions, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("a fresh folder lists %d session(s), want 0", len(sessions))
	}
}

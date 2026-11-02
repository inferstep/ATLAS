package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// testHome is the folder the whole test run uses in place of the user's own.
var testHome string

// TestMain keeps every test away from the real user folders. A test that
// drives the model can save a session without asking for a folder of its own,
// and the session folder hangs off os.UserCacheDir, which reads
// $XDG_CACHE_HOME on Linux and $HOME on macOS. Both point at one temporary
// folder for the run; isolateSessions gives a single test a fresh one.
// internal/sessionstore has the same guard for its own tests.
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

// The store keeps sessions under os.UserCacheDir (its own tests check that),
// so the model's saves stay in the folder of the run when that folder does.
func TestTheCacheFolderOfTheRunIsItsOwn(t *testing.T) {
	dir, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("UserCacheDir: %v", err)
	}
	if !strings.HasPrefix(dir, testHome) {
		t.Fatalf("cache folder %q is outside the folder of the test run %q", dir, testHome)
	}
}

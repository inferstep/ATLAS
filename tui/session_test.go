// Tests for the model's own session saves. The store's tests are in
// internal/sessionstore.

package main

import (
	"testing"
	"time"

	"github.com/itigges22/atlas-tui/internal/sessionstore"
)

// isolateSessions points os.UserCacheDir at a temp dir of this test, so the
// test sees no session of another test and never touches a real cache
// folder. os.UserCacheDir reads $XDG_CACHE_HOME on Linux and $HOME on macOS,
// so both are set.
func isolateSessions(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	t.Setenv("HOME", dir)
}

// A model persists per turn and reloads exactly.
func TestModelSaveSessionRoundTrip(t *testing.T) {
	isolateSessions(t)
	m := newTUIModel("http://test")
	m.persistEnabled = true
	m.mode = "yolo"
	m.chat = []sessionstore.ChatMessage{
		{Role: sessionstore.RoleUser, Body: "first ask"},
		{Role: sessionstore.RoleAssistant, Body: "a reply"},
	}
	m.saveSession()
	got, err := sessionstore.Load(m.sessionUID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Mode != "yolo" {
		t.Errorf("mode = %q, want yolo", got.Mode)
	}
	if got.Title != "first ask" {
		t.Errorf("title = %q, want 'first ask'", got.Title)
	}
	if len(got.Messages) != 2 {
		t.Errorf("messages len = %d, want 2", len(got.Messages))
	}
}

// A session keeps the time it was created; a model without one stamps the
// current time. (The round-trip tests above do not read CreatedAt.)
func TestBuildSessionKeepsTheCreationTime(t *testing.T) {
	m := &tuiModel{sessionCreatedAt: "2026-07-01T10:00:00Z"}
	if got := m.buildSession().CreatedAt; got != "2026-07-01T10:00:00Z" {
		t.Errorf("CreatedAt = %q, want the model's own 2026-07-01T10:00:00Z", got)
	}

	m = &tuiModel{}
	got := m.buildSession().CreatedAt
	if _, err := time.Parse(time.RFC3339, got); err != nil {
		t.Errorf("CreatedAt with no creation time = %q, want the current time in RFC 3339: %v", got, err)
	}
}

// Persistence is skipped when disabled (demo child models) or empty.
func TestSaveSessionRespectsGuards(t *testing.T) {
	isolateSessions(t)
	m := newTUIModel("http://test")
	m.persistEnabled = false
	m.chat = []sessionstore.ChatMessage{{Role: sessionstore.RoleUser, Body: "x"}}
	m.saveSession()
	if _, err := sessionstore.Load(m.sessionUID); err == nil {
		t.Error("disabled persistence should not write a file")
	}

	m.persistEnabled = true
	m.chat = nil
	m.saveSession()
	if _, err := sessionstore.Load(m.sessionUID); err == nil {
		t.Error("empty transcript should not write a file")
	}
}

// /clear (startNewSession) mints a fresh id so the prior file is preserved.
func TestStartNewSessionPreservesPriorFile(t *testing.T) {
	isolateSessions(t)
	m := newTUIModel("http://test")
	m.persistEnabled = true
	m.chat = []sessionstore.ChatMessage{{Role: sessionstore.RoleUser, Body: "keep me"}}
	m.saveSession()
	oldID := m.sessionUID

	m.startNewSession()
	if m.sessionUID == oldID {
		t.Fatal("startNewSession should mint a new id")
	}
	// The prior session's file is still on disk and intact.
	prior, err := sessionstore.Load(oldID)
	if err != nil {
		t.Fatalf("prior session file was lost: %v", err)
	}
	if len(prior.Messages) != 1 || prior.Messages[0].Body != "keep me" {
		t.Errorf("prior session altered: %+v", prior.Messages)
	}
}

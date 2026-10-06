// Tests for session persistence — round-trip save/load, listing order, cwd
// filtering, malformed-file tolerance, and that a resumed transcript feeds
// buildChatHistory.

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestSaveLoadRoundTrip(t *testing.T) {
	isolateSessions(t)
	sess := Session{
		ID:        "sess-1",
		Cwd:       "/work/proj",
		Mode:      "accept-edits",
		CreatedAt: "2026-07-01T10:00:00Z",
		UpdatedAt: "2026-07-01T10:05:00Z",
		Title:     "fix the bug",
		Messages: []chatMessage{
			{Role: roleUser, Body: "fix the bug"},
			{Role: roleAssistant, Body: "done"},
			{Role: roleSystem, Body: "· turn", Meta: "turn", Echo: true},
		},
	}
	if err := saveSession(sess); err != nil {
		t.Fatalf("saveSession: %v", err)
	}
	got, err := loadSession("sess-1")
	if err != nil {
		t.Fatalf("loadSession: %v", err)
	}
	if got.Cwd != sess.Cwd || got.Mode != sess.Mode || got.Title != sess.Title {
		t.Errorf("metadata mismatch: got %+v", got)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages len = %d, want 3", len(got.Messages))
	}
	if got.Messages[0].Body != "fix the bug" || got.Messages[1].Role != roleAssistant {
		t.Errorf("messages not preserved: %+v", got.Messages)
	}
	if !got.Messages[2].Echo || got.Messages[2].Meta != "turn" {
		t.Errorf("row flags not preserved: %+v", got.Messages[2])
	}
}

func TestListSessionsNewestFirst(t *testing.T) {
	isolateSessions(t)
	for _, s := range []Session{
		{ID: "a", UpdatedAt: "2026-07-01T10:00:00Z", Messages: []chatMessage{{Role: roleUser, Body: "a"}}},
		{ID: "b", UpdatedAt: "2026-07-01T12:00:00Z", Messages: []chatMessage{{Role: roleUser, Body: "b"}}},
		{ID: "c", UpdatedAt: "2026-07-01T11:00:00Z", Messages: []chatMessage{{Role: roleUser, Body: "c"}}},
	} {
		if err := saveSession(s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := listSessions()
	if err != nil {
		t.Fatalf("listSessions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("listSessions len = %d, want 3", len(got))
	}
	if got[0].ID != "b" || got[1].ID != "c" || got[2].ID != "a" {
		t.Errorf("order = %s,%s,%s, want b,c,a", got[0].ID, got[1].ID, got[2].ID)
	}
}

func TestMostRecentForCwd(t *testing.T) {
	isolateSessions(t)
	for _, s := range []Session{
		{ID: "a", Cwd: "/one", UpdatedAt: "2026-07-01T10:00:00Z", Messages: []chatMessage{{Role: roleUser, Body: "x"}}},
		{ID: "b", Cwd: "/two", UpdatedAt: "2026-07-01T12:00:00Z", Messages: []chatMessage{{Role: roleUser, Body: "x"}}},
		{ID: "c", Cwd: "/one", UpdatedAt: "2026-07-01T13:00:00Z", Messages: []chatMessage{{Role: roleUser, Body: "x"}}},
	} {
		if err := saveSession(s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := mostRecentForCwd("/one")
	if err != nil {
		t.Fatalf("mostRecentForCwd: %v", err)
	}
	if got == nil || got.ID != "c" {
		t.Fatalf("mostRecentForCwd(/one) = %+v, want id c", got)
	}
	none, err := mostRecentForCwd("/nowhere")
	if err != nil {
		t.Fatalf("mostRecentForCwd err: %v", err)
	}
	if none != nil {
		t.Errorf("mostRecentForCwd(/nowhere) = %+v, want nil", none)
	}
}

func TestListSessionsSkipsMalformed(t *testing.T) {
	isolateSessions(t)
	if err := saveSession(Session{ID: "good", UpdatedAt: "2026-07-01T10:00:00Z",
		Messages: []chatMessage{{Role: roleUser, Body: "x"}}}); err != nil {
		t.Fatal(err)
	}
	dir, err := sessionsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := listSessions()
	if err != nil {
		t.Fatalf("listSessions: %v", err)
	}
	if len(got) != 1 || got[0].ID != "good" {
		t.Errorf("listSessions = %+v, want only the good session", got)
	}
}

// A model persists per turn and reloads exactly.
func TestModelSaveSessionRoundTrip(t *testing.T) {
	isolateSessions(t)
	m := newTUIModel("http://test")
	m.persistEnabled = true
	m.mode = "yolo"
	m.chat = []chatMessage{
		{Role: roleUser, Body: "first ask"},
		{Role: roleAssistant, Body: "a reply"},
	}
	m.saveSession()
	got, err := loadSession(m.sessionUID)
	if err != nil {
		t.Fatalf("loadSession: %v", err)
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

// Persistence is skipped when disabled (demo child models) or empty.
func TestSaveSessionRespectsGuards(t *testing.T) {
	isolateSessions(t)
	m := newTUIModel("http://test")
	m.persistEnabled = false
	m.chat = []chatMessage{{Role: roleUser, Body: "x"}}
	m.saveSession()
	if _, err := loadSession(m.sessionUID); err == nil {
		t.Error("disabled persistence should not write a file")
	}

	m.persistEnabled = true
	m.chat = nil
	m.saveSession()
	if _, err := loadSession(m.sessionUID); err == nil {
		t.Error("empty transcript should not write a file")
	}
}

// A resumed transcript feeds buildChatHistory just like live rows.
func TestResumedTranscriptFeedsBuildChatHistory(t *testing.T) {
	isolateSessions(t)
	saved := Session{
		ID: "sess-x", Cwd: "/w", Mode: "default",
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Messages: []chatMessage{
			{Role: roleUser, Body: "earlier ask"},
			{Role: roleAssistant, Body: "earlier reply"},
			{Role: roleUser, Body: "current message"},
		},
	}
	if err := saveSession(saved); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSession("sess-x")
	if err != nil {
		t.Fatal(err)
	}
	m := newTUIModel("http://test")
	m.chat = loaded.Messages
	got := m.buildChatHistory()
	// The last user row is the message being sent this turn; the two earlier
	// rows become history.
	if len(got) != 2 {
		t.Fatalf("buildChatHistory len = %d, want 2 (got %+v)", len(got), got)
	}
	if got[0].Role != "user" || got[0].Content != "earlier ask" {
		t.Errorf("history[0] = %+v", got[0])
	}
}

// /clear (startNewSession) mints a fresh id so the prior file is preserved.
func TestStartNewSessionPreservesPriorFile(t *testing.T) {
	isolateSessions(t)
	m := newTUIModel("http://test")
	m.persistEnabled = true
	m.chat = []chatMessage{{Role: roleUser, Body: "keep me"}}
	m.saveSession()
	oldID := m.sessionUID

	m.startNewSession()
	if m.sessionUID == oldID {
		t.Fatal("startNewSession should mint a new id")
	}
	// The prior session's file is still on disk and intact.
	prior, err := loadSession(oldID)
	if err != nil {
		t.Fatalf("prior session file was lost: %v", err)
	}
	if len(prior.Messages) != 1 || prior.Messages[0].Body != "keep me" {
		t.Errorf("prior session altered: %+v", prior.Messages)
	}
}

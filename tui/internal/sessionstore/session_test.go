// Tests for session persistence — round-trip save/load, listing order, cwd
// filtering and malformed-file tolerance.

package sessionstore

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
		Messages: []ChatMessage{
			{Role: RoleUser, Body: "fix the bug"},
			{Role: RoleAssistant, Body: "done"},
			{Role: RoleSystem, Body: "· turn", Meta: "turn", Echo: true},
		},
	}
	if err := Save(sess); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load("sess-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Every field and every row comes back, with its role and flags.
	if !reflect.DeepEqual(*got, sess) {
		t.Errorf("loaded %+v, want what was saved: %+v", *got, sess)
	}
}

// List returns every saved session newest first, and skips a file
// that does not hold one.
func TestListSessionsNewestFirstSkippingMalformed(t *testing.T) {
	isolateSessions(t)
	for _, s := range []Session{
		{ID: "a", UpdatedAt: "2026-07-01T10:00:00Z", Messages: []ChatMessage{{Role: RoleUser, Body: "a"}}},
		{ID: "b", UpdatedAt: "2026-07-01T12:00:00Z", Messages: []ChatMessage{{Role: RoleUser, Body: "b"}}},
		{ID: "c", UpdatedAt: "2026-07-01T11:00:00Z", Messages: []ChatMessage{{Role: RoleUser, Body: "c"}}},
	} {
		if err := Save(s); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := sessionsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var ids []string
	for _, s := range got {
		ids = append(ids, s.ID)
	}
	if strings.Join(ids, ",") != "b,c,a" {
		t.Errorf("List = %v, want b,c,a", ids)
	}
}

func TestMostRecentForCwd(t *testing.T) {
	isolateSessions(t)
	for _, s := range []Session{
		{ID: "a", Cwd: "/one", UpdatedAt: "2026-07-01T10:00:00Z", Messages: []ChatMessage{{Role: RoleUser, Body: "x"}}},
		{ID: "b", Cwd: "/two", UpdatedAt: "2026-07-01T12:00:00Z", Messages: []ChatMessage{{Role: RoleUser, Body: "x"}}},
		{ID: "c", Cwd: "/one", UpdatedAt: "2026-07-01T13:00:00Z", Messages: []ChatMessage{{Role: RoleUser, Body: "x"}}},
	} {
		if err := Save(s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := MostRecentForCwd("/one")
	if err != nil {
		t.Fatalf("MostRecentForCwd: %v", err)
	}
	if got == nil || got.ID != "c" {
		t.Fatalf("MostRecentForCwd(/one) = %+v, want id c", got)
	}
	none, err := MostRecentForCwd("/nowhere")
	if err != nil {
		t.Fatalf("MostRecentForCwd err: %v", err)
	}
	if none != nil {
		t.Errorf("MostRecentForCwd(/nowhere) = %+v, want nil", none)
	}
}

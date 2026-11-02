// Package sessionstore is session persistence: the chat transcript is
// written to a per-session JSON file so `atlas --continue` / `atlas --resume`
// can reload it later. The raw ChatMessage rows are stored verbatim so both
// rendering (renderChatPane) and the /v1/agent history (buildChatHistory)
// reconstruct exactly.
//
// Storage layout: <user cache dir>/atlas-tui/sessions/<id>.json, one file per
// session. Writes are whole-file and atomic (temp file + rename) and guarded
// by a mutex, as internal/debuglog serializes its file access.

package sessionstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Session is the on-disk schema for one saved transcript.
type Session struct {
	ID        string        `json:"id"`
	Cwd       string        `json:"cwd"`
	Mode      string        `json:"mode"`
	Model     string        `json:"model,omitempty"`
	CreatedAt string        `json:"created_at"`
	UpdatedAt string        `json:"updated_at"`
	Title     string        `json:"title"`
	Messages  []ChatMessage `json:"messages"`
}

// sessionMu serializes whole-file rewrites so a save mid-flight never races a
// concurrent save for the same session.
var sessionMu sync.Mutex

// sessionsDir resolves (and creates) the sessions directory under the user
// cache dir. Returns the path or an error if the cache dir can't be resolved
// or the directory can't be created.
func sessionsDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "atlas-tui", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// sessionPath returns the file path for a session id.
func sessionPath(dir, id string) string {
	return filepath.Join(dir, id+".json")
}

// Save writes sess to <sessions>/<id>.json atomically (temp file +
// rename), truncating any prior content. No-op when the id is empty.
func Save(sess Session) error {
	if sess.ID == "" {
		return nil
	}
	dir, err := sessionsDir()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}
	sessionMu.Lock()
	defer sessionMu.Unlock()
	tmp, err := os.CreateTemp(dir, sess.ID+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, sessionPath(dir, sess.ID)); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// Load reads and decodes a single session by id.
func Load(id string) (*Session, error) {
	dir, err := sessionsDir()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(sessionPath(dir, id))
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// List returns every saved session, newest-first by UpdatedAt.
// Malformed or unreadable files are skipped rather than failing the whole
// list.
func List() ([]Session, error) {
	dir, err := sessionsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var s Session
		if json.Unmarshal(data, &s) != nil || s.ID == "" {
			continue // tolerate malformed files
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	return out, nil
}

// MostRecentForCwd returns the newest session whose stored cwd matches cwd, or
// nil when none exists.
func MostRecentForCwd(cwd string) (*Session, error) {
	all, err := List()
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Cwd == cwd {
			s := all[i]
			return &s, nil
		}
	}
	return nil, nil
}

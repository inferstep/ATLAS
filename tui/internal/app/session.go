// The model's side of session persistence: the saved Session built from the
// chat, its title, and when it is written. The store is internal/sessionstore.

package app

import (
	"strings"
	"time"

	"github.com/itigges22/atlas-tui/internal/sessionstore"
	"github.com/itigges22/atlas-tui/internal/termtext"
)

// sessionTitle derives a title from the first non-empty, non-echo user message
// body, truncated to ~60 characters.
func sessionTitle(chat []sessionstore.ChatMessage) string {
	for _, row := range chat {
		if row.Role != sessionstore.RoleUser || row.Echo {
			continue
		}
		body := strings.TrimSpace(row.Body)
		if body == "" {
			continue
		}
		return termtext.Truncate(body, 60)
	}
	return ""
}

// buildSession snapshots the model into a Session ready for saveSession.
func (m *tuiModel) buildSession() sessionstore.Session {
	created := m.sessionCreatedAt
	if created == "" {
		created = time.Now().UTC().Format(time.RFC3339)
	}
	// Copy the transcript so the Session is self-contained and never aliases
	// the live UI slice (which the model keeps appending to).
	msgs := make([]sessionstore.ChatMessage, len(m.chat))
	copy(msgs, m.chat)
	return sessionstore.Session{
		ID:        m.sessionUID,
		Cwd:       m.workingDir,
		Mode:      m.mode,
		CreatedAt: created,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Title:     sessionTitle(m.chat),
		Messages:  msgs,
	}
}

// saveSession persists the current transcript. No-op when persistence is
// disabled (demo child models), the id is unset, or the transcript is empty —
// so a bare startup never leaves an empty file behind.
func (m *tuiModel) saveSession() {
	if !m.persistEnabled || m.sessionUID == "" || len(m.chat) == 0 {
		return
	}
	_ = sessionstore.Save(m.buildSession())
}

// startNewSession mints a fresh persistence id + created stamp. Called on
// /clear so the cleared transcript is written under a new id and the prior
// session's file stays intact.
func (m *tuiModel) startNewSession() {
	m.sessionUID = newSessionID()
	m.sessionCreatedAt = time.Now().UTC().Format(time.RFC3339)
	// A new session does not inherit the previous session's "allow for
	// session" answers.
	m.sessionAllowedTools = map[string]bool{}
}

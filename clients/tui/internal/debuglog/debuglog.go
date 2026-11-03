// Package debuglog is the TUI's debug log: an append-only file capturing
// everything that flows through the TUI so the live view isn't the only
// artifact. Bubbletea takes over the terminal in alt-screen mode, which makes
// copy/inspect hard; the log gives the operator a flat record to read after
// the fact.
//
// Enabled by --log <path> on the CLI or $ATLAS_TUI_LOG. Disabled by
// default — emitting events to a file always-on isn't free and would
// surprise users with mystery files.
package debuglog

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	mu  sync.Mutex
	out io.Writer = io.Discard
)

// Open opens path for append. Empty path → no-op (logger
// stays at io.Discard). Returns the close func or nil.
func Open(path string) (func(), error) {
	// "off" is the documented opt-out (docs/reference/CLI.md, and the Python wrapper
	// strips ATLAS_TUI_LOG=off before exec'ing this binary). Anyone who runs
	// the binary directly bypasses that wrapper, and without this the word
	// was taken as a filename: `ATLAS_TUI_LOG=off atlas-tui` created a file
	// called "off" in the working directory. Matched case-insensitively, as
	// the wrapper does.
	if path == "" || strings.EqualFold(strings.TrimSpace(path), "off") {
		return nil, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log %s: %w", path, err)
	}
	mu.Lock()
	out = f
	mu.Unlock()
	Log("session", "started", map[string]interface{}{
		"pid": os.Getpid(),
	})
	return func() {
		Log("session", "ended", nil)
		mu.Lock()
		out = io.Discard
		mu.Unlock()
		_ = f.Close()
	}, nil
}

// Log writes one timestamped line. category groups events (chat,
// event, user, slash, turn, conn); subject is a short tag; fields is
// optional structured data dumped as JSON.
//
// Format: `2026-05-02T17:03:21.123Z chat:text {"content":"Hi!"}`
func Log(category, subject string, fields map[string]interface{}) {
	mu.Lock()
	defer mu.Unlock()
	if out == io.Discard {
		return
	}
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	line := fmt.Sprintf("%s %s:%s", ts, category, subject)
	if len(fields) > 0 {
		b, _ := json.Marshal(fields)
		line += " " + string(b)
	}
	_, _ = fmt.Fprintln(out, line)
}

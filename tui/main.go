// PC-062: Bubbletea TUI for ATLAS — main entry point.
//
// Connects to atlas-proxy /events (typed envelope SSE stream from
// PC-061) and renders the canonical chat UI: pipeline progress, event
// log, stats + chat input. The default `atlas` command launches this
// in interactive mode; pipe mode falls back to the built-in REPL.
//
// Bubbletea model is in model.go; pane rendering in panes.go;
// chat/agent client and the /events SSE consumer both in chat.go.

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	defaultProxyURL = "http://localhost:8090"
)

func main() {
	installTokenTransport()
	proxyURL := flag.String("proxy", envOr("ATLAS_PROXY_URL", defaultProxyURL),
		"atlas-proxy base URL (default: $ATLAS_PROXY_URL or http://localhost:8090)")
	logPath := flag.String("log", envOr("ATLAS_TUI_LOG", ""),
		"append-only debug log path (default: off; alt-screen makes copy hard, "+
			"so tail this file to see what the TUI saw)")
	mouseFlag := flag.String("mouse", envOr("ATLAS_TUI_MOUSE", "on"),
		"mouse capture: 'on' (wheel scrolls chat) or 'off' (lets you select/copy text). "+
			"Mid-session toggle: /mouse on|off")
	demoMode := flag.String("demo", "",
		"launch the split-pane recording demo directly (short|medium|long). "+
			"Skips the main TUI; same as typing /demo from inside it.")
	continueFlag := flag.Bool("continue", false,
		"resume the most recent saved session for the current directory")
	resumeFlag := flag.Bool("resume", false,
		"resume a saved session: 'atlas --resume <id>' by id, or 'atlas --resume' "+
			"to pick from a list")
	flag.Parse()

	// Cold-start --demo bypasses the main TUI entirely. Demo and the
	// continue/resume flags are mutually exclusive — demo wins by
	// short-circuiting here, so the resume flags are simply ignored.
	if *demoMode != "" {
		cwd, _ := os.Getwd()
		if err := runDemo(*proxyURL, cwd, *demoMode); err != nil {
			fmt.Fprintf(os.Stderr, "atlas-tui demo: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if closer, err := initDebugLog(*logPath); err != nil {
		fmt.Fprintf(os.Stderr, "atlas-tui: %v\n", err)
		os.Exit(1)
	} else if closer != nil {
		defer closer()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	model := newTopLevelModel(*proxyURL, *resumeFlag, *continueFlag)

	// Surface the Python wrapper's startup warning (workspace mismatch
	// etc.) inside the TUI. Without this the warning prints to stderr
	// and is immediately covered by alt-screen.
	if note := os.Getenv("ATLAS_TUI_STARTUP_NOTE"); note != "" {
		model.chat = append(model.chat, chatMessage{
			Role: roleSystem, Meta: "startup", Body: note,
		})
	}

	// SSE consumer goroutine: pushes envelopes onto a channel that
	// the Bubbletea program drains via a tea.Cmd.
	go streamEventsWithReconnect(ctx, *proxyURL+"/events", model.events)

	opts := programOptions(*mouseFlag)
	prog := tea.NewProgram(model, opts...)

	finalModel, err := prog.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "atlas-tui: %v\n", err)
		os.Exit(1)
	}

	// /demo handoff: the slash command sets launchDemoLength on the
	// model and quits; we now relaunch in the same terminal.
	if fm, ok := finalModel.(tuiModel); ok && fm.launchDemoLength != "" {
		cwd, _ := os.Getwd()
		if err := runDemo(*proxyURL, cwd, fm.launchDemoLength); err != nil {
			fmt.Fprintf(os.Stderr, "atlas-tui demo: %v\n", err)
			os.Exit(1)
		}
	}
}

// newTopLevelModel builds the model of the interactive TUI: it saves its
// session, and it starts from the saved session that --resume or --continue
// asks for.
func newTopLevelModel(proxyURL string, resumeFlag, continueFlag bool) tuiModel {
	model := newTUIModel(proxyURL)
	// Persistence is on for the top-level TUI. Demo child models are built
	// via newTUIModel too but never flip this, so they never write sessions.
	model.persistEnabled = true

	// --continue / --resume reload a saved transcript before the alt-screen
	// program starts (the resume picker reads stdin, which the alt-screen
	// would otherwise capture). --resume takes precedence if both are set.
	switch {
	case resumeFlag:
		applyResume(&model, flag.Arg(0))
	case continueFlag:
		applyContinue(&model)
	}
	return model
}

// programOptions returns the Bubble Tea options of the interactive TUI: the
// alternate screen, and mouse capture unless the --mouse flag is "off".
func programOptions(mouseFlag string) []tea.ProgramOption {
	// Mouse cell-motion capture so the wheel scrolls the chat pane.
	// Cell-motion (vs all-motion) only captures when buttons are held
	// or pressed, which keeps idle text selection working on most
	// modern terminals. iTerm2/Kitty/WezTerm let users hold Option/
	// Shift while dragging to override capture entirely; that's the
	// recommended escape hatch when copy/paste is needed.
	//
	// --mouse off (or ATLAS_TUI_MOUSE=off) skips the WithMouseCellMotion
	// option entirely so users who prefer to select/copy without holding
	// modifiers can launch the TUI with capture pre-disabled. The
	// /mouse slash command still toggles either way at runtime.
	opts := []tea.ProgramOption{tea.WithAltScreen()}
	if mouseFlag != "off" {
		opts = append(opts, tea.WithMouseCellMotion())
	}
	return opts
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// applyContinue loads the most recent saved session for the current directory
// into model. A missing session prints a friendly note and leaves the model
// fresh rather than failing.
func applyContinue(model *tuiModel) {
	cwd, _ := os.Getwd()
	sess, err := mostRecentForCwd(cwd)
	if err != nil || sess == nil {
		fmt.Fprintln(os.Stderr,
			"atlas-tui: no saved session for this directory — starting fresh")
		return
	}
	loadSessionInto(model, sess, cwd)
}

// applyResume loads a session by id, or shows a picker when id is empty. An
// unknown id or an invalid/blank selection starts fresh with a note.
func applyResume(model *tuiModel, id string) {
	cwd, _ := os.Getwd()
	var sess *Session
	if id != "" {
		s, err := loadSession(id)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"atlas-tui: session %q not found — starting fresh\n", id)
			return
		}
		sess = s
	} else {
		sess = pickSession()
		if sess == nil {
			return // pickSession already printed the reason
		}
	}
	loadSessionInto(model, sess, cwd)
}

// loadSessionInto splices a saved transcript into model, adopting its id, mode
// and created stamp and scrolling to the latest message. The CURRENT working
// directory is kept; if the session was saved elsewhere a system row warns
// that writes land in the current workspace, not the old path.
func loadSessionInto(model *tuiModel, sess *Session, cwd string) {
	model.chat = sess.Messages
	model.sessionUID = sess.ID
	if sess.CreatedAt != "" {
		model.sessionCreatedAt = sess.CreatedAt
	}
	if sess.Mode != "" {
		model.mode = sess.Mode
	}
	model.chatScroll = 0 // follow the latest message
	if sess.Cwd != "" && sess.Cwd != cwd {
		model.chat = append(model.chat, chatMessage{
			Role: roleSystem, Meta: "startup",
			Body: fmt.Sprintf(
				"resumed session was saved in %s — current workspace is %s; writes go to the current workspace.",
				sess.Cwd, cwd),
		})
	}
}

// pickSession prints an indexed, newest-first list of saved sessions and reads
// a selection from stdin. Returns the chosen session, or nil (with a note) on
// no sessions, a blank line, or an invalid selection.
func pickSession() *Session {
	all, err := listSessions()
	if err != nil || len(all) == 0 {
		fmt.Fprintln(os.Stderr, "atlas-tui: no saved sessions — starting fresh")
		return nil
	}
	fmt.Fprintln(os.Stdout, "Saved sessions (newest first):")
	for i, s := range all {
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(os.Stdout, "  [%d] %s  %s  %s\n",
			i+1, s.UpdatedAt, truncate(title, 50), s.Cwd)
	}
	fmt.Fprint(os.Stdout, "Select a session number (or Enter to start fresh): ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		fmt.Fprintln(os.Stderr, "atlas-tui: no selection — starting fresh")
		return nil
	}
	idx, err := strconv.Atoi(line)
	if err != nil || idx < 1 || idx > len(all) {
		fmt.Fprintln(os.Stderr, "atlas-tui: invalid selection — starting fresh")
		return nil
	}
	s := all[idx-1]
	return &s
}

// Debug log — append-only file capturing everything that flows through
// the TUI so the live view isn't the only artifact. Bubbletea takes
// over the terminal in alt-screen mode, which makes copy/inspect hard;
// the log gives the operator (and Claude) a flat record to read after
// the fact.
//
// Enabled by --log <path> on the CLI or $ATLAS_TUI_LOG. Disabled by
// default — emitting events to a file always-on isn't free and would
// surprise users with mystery files.

var (
	dlogMu sync.Mutex
	dlogW  io.Writer = io.Discard
)

// initDebugLog opens path for append. Empty path → no-op (logger
// stays at io.Discard). Returns the close func or nil.
func initDebugLog(path string) (func(), error) {
	// "off" is the documented opt-out (docs/CLI.md, and the Python wrapper
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
	dlogMu.Lock()
	dlogW = f
	dlogMu.Unlock()
	dlog("session", "started", map[string]interface{}{
		"pid": os.Getpid(),
	})
	return func() {
		dlog("session", "ended", nil)
		dlogMu.Lock()
		dlogW = io.Discard
		dlogMu.Unlock()
		_ = f.Close()
	}, nil
}

// dlog writes one timestamped line. category groups events (chat,
// event, user, slash, turn, conn); subject is a short tag; fields is
// optional structured data dumped as JSON.
//
// Format: `2026-05-02T17:03:21.123Z chat:text {"content":"Hi!"}`
func dlog(category, subject string, fields map[string]interface{}) {
	dlogMu.Lock()
	defer dlogMu.Unlock()
	if dlogW == io.Discard {
		return
	}
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	line := fmt.Sprintf("%s %s:%s", ts, category, subject)
	if len(fields) > 0 {
		b, _ := json.Marshal(fields)
		line += " " + string(b)
	}
	_, _ = fmt.Fprintln(dlogW, line)
}

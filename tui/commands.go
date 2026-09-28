// PC-062: slash command handling.
//
// User input starting with "/" is intercepted before /v1/agent send.
// Three categories:
//
//   local      — /help, /quit, /add, /drop  (mutate TUI state, no I/O)
//   git wrappers — /commit, /diff, /undo    (shell out to git, capture output)
//   shell      — /run <cmd>                 (shell out, capture output)
//
// Shell-out commands return their output as a slashResultMsg which the
// model appends to chat as a tool-style row.

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// copyToClipboard writes s to the system clipboard. Tries in order:
//  1. Local CLI tools — wl-copy / xclip / xsel / pbcopy (only useful
//     when the TUI is running on the same machine as the desktop).
//  2. OSC52 escape sequence — works over SSH because the TERMINAL
//     EMULATOR (iTerm2 / Kitty / WezTerm / Alacritty / modern xterm /
//     gnome-terminal with the option enabled / Windows Terminal /
//     Ghostty) intercepts ESC]52;c;<base64>BEL and pushes the payload
//     to the local clipboard. The user gets the right behavior whether
//     they ran `atlas tui` locally or via ssh atlas-host.
//
// Returns nil on the first success. The OSC52 path only fails when we
// can't write to the TTY at all — even on terminals that don't support
// the sequence the write itself is silent (the escape is just ignored).
func copyToClipboard(s string) (err error) {
	// Named return so the deferred tty.Close() below can promote a
	// close-time error when the write itself succeeded.
	tools := [][]string{
		{"wl-copy"},
		{"xclip", "-selection", "clipboard"},
		{"xsel", "--clipboard", "--input"},
		{"pbcopy"},
	}
	for _, cmd := range tools {
		if _, err := exec.LookPath(cmd[0]); err != nil {
			continue
		}
		c := exec.Command(cmd[0], cmd[1:]...)
		c.Stdin = strings.NewReader(s)
		if err := c.Run(); err == nil {
			return nil
		}
	}
	// Fallback: OSC52. Bubble Tea has its own clipboard cmd in newer
	// versions, but emitting the sequence directly works without a
	// dependency bump and lets us return synchronously.
	encoded := base64.StdEncoding.EncodeToString([]byte(s))
	// Some terminals enforce a payload cap on OSC52 (~ 8KB historically).
	// Truncate so the user gets *something* in the clipboard rather than
	// nothing — the original full text is still in chat scrollback.
	if len(encoded) > 7500 {
		encoded = base64.StdEncoding.EncodeToString([]byte(s[:5500]))
	}
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("OSC52 fallback: %w", err)
	}
	// Bare `defer tty.Close()` would swallow buffered-write errors on a
	// writable fd (go/unhandled-writable-file-close). Wrap so we surface
	// the close failure when the write itself succeeded — otherwise the
	// caller thinks the copy went through.
	defer func() {
		if cerr := tty.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("OSC52 tty close: %w", cerr)
		}
	}()
	if _, err = fmt.Fprintf(tty, "\x1b]52;c;%s\x07", encoded); err != nil {
		return fmt.Errorf("OSC52 write: %w", err)
	}
	return nil
}

// collectLastMessages returns the body of the last n chat messages
// joined with blank lines. Used by /copy / /yank.
func collectLastMessages(chat []chatMessage, n int) string {
	if n <= 0 || len(chat) == 0 {
		return ""
	}
	start := len(chat) - n
	if start < 0 {
		start = 0
	}
	parts := make([]string, 0, n)
	for _, m := range chat[start:] {
		parts = append(parts, m.Body)
	}
	return strings.Join(parts, "\n\n")
}

// slashResultMsg carries the output of a shelled-out slash command back
// to the model. err is non-nil when the command failed (non-zero exit
// or process error); output is still set so the user sees stderr.
type slashResultMsg struct {
	command string
	output  string
	err     error
}

// slashCommandHelp is the static text emitted by /help. Single source
// of truth — keep in lockstep with handleSlash's switch.
const slashCommandHelp = `Slash commands
  /help  (or ?)           Show this help.
  /add <path>             Add file to the agent's working context.
  /drop <path>            Remove file from the working context.
  /context                List files currently in context.
  /diff [path]            Show git diff.
  /commit [msg]           Stage all changes and create a commit.
  /undo                   Revert the last commit (keep changes in tree).
  /run <cmd>              Run a shell command in the working dir.
  /ask <message>          Send one message as a question, not a work request.
  /review                 List files the last pass wrote.
  /redo <path> [reason]   Ask the agent to regenerate a file.
  /clear                  Clear the chat history (keeps session tokens).
  /compact                Ask the agent to compact conversation history.
  /hide <pane>            Hide a pane: files, pipeline, events, or all.
  /show <pane>            Show a pane (or all).
  /mouse [on|off]         Toggle mouse capture (off lets you select text). No arg = off.
  /copy [N]               Copy last N chat messages to system clipboard (default 1).
  /demo [len]             Split-pane demo: base agent vs V3 (short|medium|long).
  /quit                   Exit.

Copying text  (TL;DR: just drag-highlight in chat — auto-copies on release)
  When mouse capture is on (default), drag-highlighting in the chat
  pane auto-copies the covered lines to the system clipboard on
  release and shows a "✓ copied N chars" toast in the chat. Uses
  OSC52 escape, so it works locally and over SSH on iTerm2 / Kitty /
  WezTerm / Alacritty / Ghostty / Windows Terminal / modern xterm.
  Other ways:
    /copy [N]      Copy last N chat messages to clipboard. Useful when
                   you want the WHOLE last reply without dragging.
    /mouse off     Disable capture so the terminal handles selection
                   natively. Then use your terminal's copy hotkey
                   (Ctrl+Shift+C / Cmd+C / right-click → Copy).
    Hold Shift     (Linux/Win) or Option (macOS) while dragging to
                   override capture for one selection without /mouse off.
  Ctrl+C cancels the in-flight turn — it does NOT copy text.
  Launch with capture pre-disabled: ATLAS_TUI_MOUSE=off atlas tui

Input modes
  message text            Send to agent (Enter).
  ! <cmd>                 Run as bash (no agent call). Same as /run.
  / <cmd>                 Slash command (this list).

Keys
  Enter                   Send message.
  Shift+Enter             Newline in input.
  Ctrl+C                  Cancel turn (or quit when idle).
  Ctrl+L                  Clear chat.
  Ctrl+T                  Cycle permission mode.
  Ctrl+R                  Resend last message.
  PgUp / PgDn             Scroll chat by ~10 rows.
  Mouse wheel             Scroll chat by ~3 rows.
  Ctrl+End / Ctrl+Home    Jump to bottom / top of chat.`

// handleSlash interprets a slash-prefixed input. Returns:
//
//	consumed = true  → the slash was a recognized command (handled here)
//	consumed = false → not a slash command; pass to /v1/agent as usual
//	cmd              → optional tea.Cmd to run async work (shell out)
//	quit             → true if the model should tea.Quit immediately
func (m *tuiModel) handleSlash(input string) (consumed bool, cmd tea.Cmd, quit bool) {
	if !strings.HasPrefix(input, "/") {
		return false, nil, false
	}

	// Echo the input as a "you" row so the chat reflects what was sent.
	// Echo rows are display-only — buildChatHistory never forwards them.
	m.chat = append(m.chat, chatMessage{Role: roleUser, Body: input, Echo: true})

	parts := strings.Fields(input)
	cmdName := parts[0]
	args := parts[1:]

	switch cmdName {
	case "/help", "/?":
		m.chat = append(m.chat, chatMessage{
			Role: roleSystem, Meta: "help", Body: slashCommandHelp,
		})
		return true, nil, false

	case "/quit", "/exit":
		return true, tea.Quit, true

	case "/demo":
		// Quit the main TUI and hand off to the split-pane recording demo
		// (base agent vs V3, same proxy/model). Length filters the prompt bank.
		length := "medium"
		if len(args) > 0 {
			switch args[0] {
			case "short", "medium", "long":
				length = args[0]
			default:
				m.chat = append(m.chat, chatMessage{
					Role: roleSystem, Meta: "error",
					Body: "usage: /demo [short|medium|long]",
				})
				return true, nil, false
			}
		}
		m.launchDemoLength = length
		return true, tea.Quit, true

	case "/add":
		return true, m.cmdAddContext(args), false

	case "/drop":
		return true, m.cmdDropContext(args), false

	case "/context":
		return true, m.cmdListContext(), false

	case "/diff":
		return true, runShellCmd(m.workingDir, "/diff",
			append([]string{"git", "diff", "--color=never"}, args...)), false

	case "/commit":
		msg := strings.Join(args, " ")
		if msg == "" {
			msg = "atlas-tui: checkpoint"
		}
		return true, runShellCmd(m.workingDir, "/commit",
			[]string{"git", "commit", "-am", msg}), false

	case "/undo":
		return true, runShellCmd(m.workingDir, "/undo",
			[]string{"git", "reset", "--soft", "HEAD~1"}), false

	case "/run":
		if len(args) == 0 {
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "error",
				Body: "/run requires a command (e.g. /run pytest -k snake)",
			})
			return true, nil, false
		}
		// Pass the rest as a single shell string so quoting/pipes work.
		return true, runShellCmd(m.workingDir, "/run",
			[]string{"bash", "-lc", strings.Join(args, " ")}), false

	case "/review":
		// List the files the last pass wrote, with any per-file verdicts.
		if len(m.lastPassFiles) == 0 {
			m.chat = append(m.chat, chatMessage{Role: roleSystem, Meta: "review",
				Body: "No files written in the last pass to review."})
			return true, nil, false
		}
		var b strings.Builder
		b.WriteString("Files written in the last pass (regenerate one with /redo <path> [reason]):\n")
		for _, f := range m.lastPassFiles {
			fmt.Fprintf(&b, "  · %s\n", f)
		}
		m.chat = append(m.chat, chatMessage{Role: roleSystem, Meta: "review", Body: strings.TrimRight(b.String(), "\n")})
		return true, nil, false

	case "/redo":
		// Ask the agent to regenerate a file from the last pass.
		if len(args) == 0 {
			m.chat = append(m.chat, chatMessage{Role: roleSystem, Meta: "error",
				Body: "usage: /redo <path> [reason]"})
			return true, nil, false
		}
		if m.turnActive {
			m.chat = append(m.chat, chatMessage{Role: roleSystem, Meta: "error",
				Body: "A turn is in progress — wait for it to finish before /redo."})
			return true, nil, false
		}
		path := args[0]
		reason := strings.Join(args[1:], " ")
		redo := fmt.Sprintf("Redo the file %s — the previous version was rejected.", path)
		if reason != "" {
			redo += " Reason: " + reason
		}
		m.chat = append(m.chat, chatMessage{Role: roleUser, Body: redo})
		return true, m.sendChatCmd(redo + m.contextSuffix()), false

	case "/clear":
		m.chat = nil
		m.chatScroll = 0
		// Start a fresh persistence session so the cleared transcript
		// doesn't overwrite the saved one on disk.
		m.startNewSession()
		return true, nil, false

	case "/ask":
		// One request answered as a question rather than as work. The mode is
		// a control the user operates; nothing here reads what they typed, so
		// the same sentence can be sent either way. The command word itself is
		// stripped -- the model sees only the message.
		if len(args) == 0 {
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "error",
				Body: "/ask requires a message (e.g. /ask what does solve.py do?)",
			})
			return true, nil, false
		}
		question := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(input), "/ask"))
		m.pendingTaskMode = taskModeQuestion
		m.chat = append(m.chat, chatMessage{Role: roleUser, Body: question})
		m.lastUserMsg = question
		return true, m.sendChatCmd(question + m.contextSuffix()), false

	case "/compact":
		// Ask the agent to compact via a synthetic user message. The
		// proxy's history-trimming kicks in at 12 messages; this lets
		// the user trigger an explicit summarization.
		compactMsg := "Summarize the conversation so far in 3-4 sentences and respond with only that summary, no tool calls."
		m.chat = append(m.chat, chatMessage{
			Role: roleSystem, Meta: "compact",
			Body: "Asking agent to compact conversation…",
		})
		return true, m.sendChatCmd(compactMsg), false

	case "/hide":
		if len(args) == 0 {
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "error",
				Body: "/hide files | pipeline | events | all",
			})
			return true, nil, false
		}
		m.applyPaneVisibility(args[0], true)
		return true, nil, false

	case "/show":
		if len(args) == 0 {
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "error",
				Body: "/show files | pipeline | events | all",
			})
			return true, nil, false
		}
		m.applyPaneVisibility(args[0], false)
		return true, nil, false

	case "/mouse":
		// Toggle mouse capture. With no arg, flips current state.
		// /mouse off → wheel-scroll stops working but the user can
		// drag-highlight text and copy it via the terminal's own
		// hotkey (Ctrl+Shift+C / Cmd+C / right-click).
		want := ""
		if len(args) > 0 {
			want = strings.ToLower(args[0])
		}
		if want == "" {
			// No arg → toggle. Track desired state so the next no-arg
			// toggle flips back. We don't have a model field for this
			// right now, so just default to "off" — the more useful
			// of the two states (users typically /mouse to enable copy).
			want = "off"
		}
		if want != "on" && want != "off" {
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "error",
				Body: "/mouse on | off  (no arg = off)",
			})
			return true, nil, false
		}
		if want == "off" {
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "mouse",
				Body: "mouse capture OFF — drag-highlight to select, then your terminal's own hotkey copies (Ctrl+Shift+C on Linux, Cmd+C on Mac, right-click → Copy elsewhere). Wheel scroll won't work until /mouse on. Tip: launch with ATLAS_TUI_MOUSE=off atlas tui to default to this.",
			})
			return true, tea.DisableMouse, false
		}
		m.chat = append(m.chat, chatMessage{
			Role: roleSystem, Meta: "mouse",
			Body: "mouse capture ON — wheel scrolls chat. Hold Shift/Option while dragging to override capture for one selection.",
		})
		return true, tea.EnableMouseCellMotion, false

	case "/copy", "/yank":
		// Copy the most recent assistant message body to the system
		// clipboard via xclip / wl-copy / pbcopy. Bypasses the whole
		// mouse-capture mess for users who just want to paste the
		// last reply elsewhere. With an arg /copy N copies the last N
		// messages (assistant + user) joined.
		n := 1
		if len(args) > 0 {
			if v, err := strconv.Atoi(args[0]); err == nil && v > 0 {
				n = v
			}
		}
		text := collectLastMessages(m.chat, n)
		if text == "" {
			m.showToast("/copy: no messages to copy yet")
			return true, nil, false
		}
		if err := copyToClipboard(text); err != nil {
			m.showToast(fmt.Sprintf("/copy failed: %v", err))
			return true, nil, false
		}
		m.showToast(fmt.Sprintf("✓ copied %d chars", len(text)))
		return true, nil, false
	}

	// Unknown slash command — show help instead of sending to the
	// agent (a typo'd /diff shouldn't trigger an LLM call).
	m.chat = append(m.chat, chatMessage{
		Role: roleSystem, Meta: "unknown",
		Body: fmt.Sprintf("unknown command %q. Type /help for the list.", cmdName),
	})
	return true, nil, false
}

// cmdAddContext adds files to the in-context set. The set is sent
// alongside each /v1/agent call so the agent knows which files the
// user considers in-scope. Returns nil cmd — purely state mutation.
func (m *tuiModel) cmdAddContext(paths []string) tea.Cmd {
	if len(paths) == 0 {
		m.chat = append(m.chat, chatMessage{
			Role: roleSystem, Meta: "error",
			Body: "/add requires at least one path",
		})
		return nil
	}
	if m.contextFiles == nil {
		m.contextFiles = map[string]bool{}
	}
	added := []string{}
	for _, p := range paths {
		if !m.contextFiles[p] {
			m.contextFiles[p] = true
			added = append(added, p)
		}
	}
	if len(added) == 0 {
		m.chat = append(m.chat, chatMessage{
			Role: roleSystem, Meta: "context",
			Body: "no new files added (all already in context)",
		})
		return nil
	}
	m.chat = append(m.chat, chatMessage{
		Role: roleSystem, Meta: "context",
		Body: fmt.Sprintf("added to context: %s", strings.Join(added, ", ")),
	})
	return nil
}

func (m *tuiModel) cmdDropContext(paths []string) tea.Cmd {
	if len(paths) == 0 {
		m.chat = append(m.chat, chatMessage{
			Role: roleSystem, Meta: "error",
			Body: "/drop requires at least one path",
		})
		return nil
	}
	dropped := []string{}
	for _, p := range paths {
		if m.contextFiles[p] {
			delete(m.contextFiles, p)
			dropped = append(dropped, p)
		}
	}
	if len(dropped) == 0 {
		m.chat = append(m.chat, chatMessage{
			Role: roleSystem, Meta: "context",
			Body: "nothing dropped (none of those paths were in context)",
		})
		return nil
	}
	m.chat = append(m.chat, chatMessage{
		Role: roleSystem, Meta: "context",
		Body: fmt.Sprintf("dropped from context: %s", strings.Join(dropped, ", ")),
	})
	return nil
}

func (m *tuiModel) cmdListContext() tea.Cmd {
	if len(m.contextFiles) == 0 {
		m.chat = append(m.chat, chatMessage{
			Role: roleSystem, Meta: "context",
			Body: "no files in context",
		})
		return nil
	}
	paths := make([]string, 0, len(m.contextFiles))
	for p := range m.contextFiles {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	m.chat = append(m.chat, chatMessage{
		Role: roleSystem, Meta: "context",
		Body: "files in context:\n  " + strings.Join(paths, "\n  "),
	})
	return nil
}

// applyPaneVisibility flips the hide flags for the named pane.
// Accepts: files, pipeline, events, all. Unknown names produce an
// error row in chat so the user can correct.
func (m *tuiModel) applyPaneVisibility(name string, hide bool) {
	verb := "shown"
	if hide {
		verb = "hidden"
	}
	switch strings.ToLower(name) {
	case "files":
		m.hideFiles = hide
	case "pipeline":
		m.hidePipeline = hide
	case "events":
		m.hideEvents = hide
	case "all":
		m.hideFiles = hide
		m.hidePipeline = hide
		m.hideEvents = hide
	default:
		m.chat = append(m.chat, chatMessage{
			Role: roleSystem, Meta: "error",
			Body: fmt.Sprintf("unknown pane %q. Use: files, pipeline, events, all.", name),
		})
		return
	}
	m.chat = append(m.chat, chatMessage{
		Role: roleSystem, Meta: "panes",
		Body: fmt.Sprintf("%s %s", name, verb),
	})
}

// runShellCmd shells out and returns a tea.Cmd that delivers the
// captured combined stdout/stderr as a slashResultMsg. Honors a
// 60-second deadline so a runaway command can't wedge the TUI.
func runShellCmd(workingDir, label string, argv []string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = workingDir
		out, err := cmd.CombinedOutput()
		return slashResultMsg{
			command: label,
			output:  strings.TrimRight(string(out), "\n"),
			err:     err,
		}
	}
}

// contextSuffix returns a string to append to the user's message so
// the agent sees the in-context file list. Empty if no files added.
//
// Format kept lightweight — just a single line listing the paths.
// The agent can then choose to read_file each one as needed.
func (m *tuiModel) contextSuffix() string {
	if len(m.contextFiles) == 0 {
		return ""
	}
	paths := make([]string, 0, len(m.contextFiles))
	for p := range m.contextFiles {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return "\n\n[atlas-tui context: " + strings.Join(paths, ", ") + "]"
}

// Calibration status badge — PC-059 (#101) + PC-061 (#113).
//
// On startup the TUI fetches /v1/calibration/status from the proxy and
// renders a compact badge next to the Pipeline pane title so users
// immediately see whether the loaded model has supported Lens artifacts
// and whether an ASA control vector is in play. Verdict comes from the
// proxy's CalibrationStatus (proxy/calibration_status.go).
//
// Why the TUI does this: when a user swaps in a non-default GGUF, the
// lens silently no-ops and the agent loop still "works" — but without
// G(x) verification half the value of ATLAS is missing. The badge is
// the only visible signal that something is up.

type calibrationStatus struct {
	Lens struct {
		Verdict         string `json:"verdict"`
		CostFieldLoaded bool   `json:"cost_field_loaded"`
		CostFieldDim    int    `json:"cost_field_dim"`
		EmbedDim        int    `json:"embed_dim"`
		GxLoaded        bool   `json:"gx_loaded"`
		CxCalibrated    bool   `json:"cx_calibrated"`
		GxCalibrated    bool   `json:"gx_calibrated"`
		Hint            string `json:"hint"`
	} `json:"lens"`
	ASA struct {
		Verdict       string `json:"verdict"`
		VectorPath    string `json:"vector_path"`
		VectorPresent bool   `json:"vector_present"`
		Hint          string `json:"hint"`
	} `json:"asa"`
}

type calibrationStatusMsg struct {
	status *calibrationStatus
	err    error
}

// calibrationRetryMsg fires when the model wants to retry the calibration
// fetch — typically because a prior fetch failed and the proxy may have
// come up since (common during `docker compose up -d`, where the TUI can
// launch faster than the proxy finishes its startup probe).
type calibrationRetryMsg struct{}

// calibrationRefreshMsg fires periodically after a successful fetch so
// the badge converges on truth over time. The original bug: a user who
// opens the TUI before lens weights are downloaded sees `Lens ⚠` and
// the badge is frozen for the rest of the session — even if they then
// run `atlas model install-artifacts` and restart the lens container,
// the TUI never re-probes. This msg drives the periodic re-fetch.
type calibrationRefreshMsg struct{}

// scheduleCalibrationRetry returns a Cmd that emits a retry trigger after
// the given delay. The model's Update handler decides whether to actually
// re-fire fetchCalibrationStatusCmd based on retry count + current state.
func scheduleCalibrationRetry(after time.Duration) tea.Cmd {
	return tea.Tick(after, func(time.Time) tea.Msg {
		return calibrationRetryMsg{}
	})
}

// scheduleCalibrationRefresh returns a Cmd that emits a refresh trigger
// after the given delay. Separate type from retry so the handler can
// apply different rules — refresh fires forever, doesn't care about
// the prior verdict, and runs at a longer interval.
func scheduleCalibrationRefresh(after time.Duration) tea.Cmd {
	return tea.Tick(after, func(time.Time) tea.Msg {
		return calibrationRefreshMsg{}
	})
}

// maxCalibrationRetries caps the *retry* loop (fast attempts after a
// failed initial fetch). At ~5s/retry, 5 attempts covers ~25s of proxy-
// startup warmup — long enough for the slowest realistic docker compose
// up, short enough that a permanently-down proxy doesn't keep poking
// forever. Once any response lands, retry stops and refresh takes over.
const maxCalibrationRetries = 5

// calibrationRetryInterval is the gap between retry attempts. Chosen to
// be long enough that we don't hammer a struggling proxy, short enough
// that the badge converges quickly once the proxy is healthy.
const calibrationRetryInterval = 5 * time.Second

// calibrationRefreshInterval is the gap between periodic refreshes after
// the initial fetch succeeds. 30s is short enough that a user who runs
// `atlas model install-artifacts` and restarts the lens container sees
// the badge flip to green within one refresh tick, long enough that the
// HTTP cost is trivial (120 calls/hour). Cap-free: the refresh runs for
// the lifetime of the TUI session.
const calibrationRefreshInterval = 30 * time.Second

// fetchCalibrationStatusCmd does an HTTP GET against the proxy. Fast — the
// proxy itself caches nothing here, but the upstream lens /health call is
// 3s-bounded and the rest is in-process. Total round-trip should be <4s.
// Fail-soft: on error we set status=nil and the badge falls back to a
// "not yet probed" placeholder rather than blocking startup.
func fetchCalibrationStatusCmd(proxyURL string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "GET",
			strings.TrimRight(proxyURL, "/")+"/v1/calibration/status", nil)
		if err != nil {
			return calibrationStatusMsg{err: err}
		}
		setProxyAuth(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return calibrationStatusMsg{err: err}
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return calibrationStatusMsg{err: nil, status: nil}
		}
		var s calibrationStatus
		if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
			return calibrationStatusMsg{err: err}
		}
		return calibrationStatusMsg{status: &s}
	}
}

// Lipgloss styles for the three verdict states. Colors picked from the
// same palette as the rest of the TUI (panes.go's titleStyle reads from
// 117 / blue; we add green/yellow/red for verdict states).
var (
	badgeOK = lipgloss.NewStyle().
		Foreground(lipgloss.Color("78")). // green
		Bold(true)
	badgeWarn = lipgloss.NewStyle().
			Foreground(lipgloss.Color("214")). // amber
			Bold(true)
	badgeFail = lipgloss.NewStyle().
			Foreground(lipgloss.Color("203")). // red
			Bold(true)
	badgeDim = lipgloss.NewStyle().
			Foreground(lipgloss.Color("245")) // grey
)

// renderCalibrationBadge produces the compact badge text that gets
// appended to the Pipeline pane title.
//
//	"  Lens ✓  ASA ✓"                                      — both supported
//	"  Lens ⚠  ASA ⚠  → atlas lens build · PUBLISHING.md"   — needs attention
//	"  cal …"                                              — fetch in flight / failed
//
// When either verdict is non-supported, an inline actionable hint is
// appended pointing the user at the relevant build command + the docs
// section that walks through the full contribution flow. The hint lives
// on the same line as the badge so we don't have to bump the pipeline
// pane height when calibration is in a warn/fail state.
//
// Returns empty string only when the proxy is reachable but returned
// nothing meaningful — better to omit than render a confusing placeholder.
func renderCalibrationBadge(s *calibrationStatus) string {
	if s == nil {
		return badgeDim.Render("  cal …")
	}
	badge := "  " + renderOneBadge("Lens", s.Lens.Verdict) +
		"  " + renderOneBadge("ASA", s.ASA.Verdict)
	if hint := badgeActionHint(s); hint != "" {
		badge += "  " + badgeDim.Render(hint)
	}
	return badge
}

func renderOneBadge(name, verdict string) string {
	switch verdict {
	case "supported", "active":
		return badgeOK.Render(name + " ✓")
	case "uncalibrated", "missing", "unverified":
		return badgeWarn.Render(name + " ⚠")
	// A lens verdict here means requests are refused: the lens is required.
	case "no-artifacts", "incomplete-artifacts", "dim-mismatch", "unreachable",
		"incompatible", "drifted", "self-test-failed", "model-server-unreachable":
		return badgeFail.Render(name + " ✗")
	default:
		return badgeDim.Render(name + " ?")
	}
}

// badgeActionHint returns the one-line "what should I do about this"
// pointer that gets rendered right next to the badge when either
// subsystem is in a non-supported state. Empty when both are happy.
//
// Suppresses on "unreachable" / "incompatible" because those mean
// services are down, not that the artifact is wrong — a "build" hint
// would be misleading there.
func badgeActionHint(s *calibrationStatus) string {
	// The lens is required: while it cannot score, requests are refused.
	const refused = " (requests are refused until the lens can score)"
	// A lens that is down may have fine artifacts, so the pointer is
	// doctor, not a rebuild.
	switch s.Lens.Verdict {
	case "unreachable", "drifted", "self-test-failed", "model-server-unreachable":
		return "→ atlas doctor" + refused
	}
	// Missing or mismatched artifacts block requests too; uncalibrated ones
	// do not (the lens scores), but a build calibrates them.
	lensBlocked := s.Lens.Verdict == "no-artifacts" ||
		s.Lens.Verdict == "incomplete-artifacts" ||
		s.Lens.Verdict == "dim-mismatch"
	lensWarn := lensBlocked || s.Lens.Verdict == "uncalibrated"
	asaWarn := s.ASA.Verdict == "missing"
	asaUnverified := s.ASA.Verdict == "unverified"
	hint := ""
	switch {
	case lensWarn && asaWarn:
		hint = "→ atlas lens build / atlas asa build · docs/PUBLISHING.md"
	case lensWarn:
		hint = "→ atlas lens build · docs/PUBLISHING.md"
	case asaWarn:
		hint = "→ atlas asa build · docs/PUBLISHING.md"
	case asaUnverified:
		hint = "→ atlas asa check"
	}
	if lensBlocked {
		hint += refused
	}
	return hint
}

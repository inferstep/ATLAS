// The model's answers to the keyboard and the mouse: the key bindings, Enter's
// submit, the permission modal's y/a/n keys, the wheel and highlight-to-copy,
// and the textarea, which takes every key with no binding of its own. Update
// in model.go hands each key and mouse message here.

package main

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// handlePermKey answers the permission modal. y=allow once, a=allow for the
// whole session (whitelists the tool so later requests skip the prompt),
// n/esc=deny. Every other key is swallowed. Each answer records a display-only
// system row (Echo=true so it never reaches /v1/agent as a fake turn) and
// returns a Cmd that POSTs the decision off the UI thread, mirroring the
// /cancel post-back pattern.
func (m tuiModel) handlePermKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	pp := m.pendingPerm
	if pp == nil {
		return m, nil
	}
	key := msg.String()
	if key == "a" && pp.oneTimeOnly {
		key = "y" // a deletion is approved one file at a time
	}
	switch key {
	case "y":
		m.pendingPerm = nil
		m.chat = append(m.chat, chatMessage{
			Role: roleSystem, Meta: "permission",
			Body: "allowed " + pp.toolName, Echo: true,
		})
		dlog("permission", "allow_once", map[string]interface{}{"tool": pp.toolName})
		return m, m.postPermissionCmd(pp.sessionID, pp.toolCallID, "allow", "once")
	case "a":
		if m.sessionAllowedTools == nil {
			m.sessionAllowedTools = map[string]bool{}
		}
		m.sessionAllowedTools[pp.toolName] = true
		m.pendingPerm = nil
		m.chat = append(m.chat, chatMessage{
			Role: roleSystem, Meta: "permission",
			Body: "allowed " + pp.toolName + " for session", Echo: true,
		})
		dlog("permission", "allow_session", map[string]interface{}{"tool": pp.toolName})
		return m, m.postPermissionCmd(pp.sessionID, pp.toolCallID, "allow", "session")
	case "n", "esc":
		// The proxy emits a permission_denied event once the deny lands, which
		// renders the transcript row — no local row here (avoids a duplicate).
		m.pendingPerm = nil
		dlog("permission", "deny", map[string]interface{}{"tool": pp.toolName})
		return m, m.postPermissionCmd(pp.sessionID, pp.toolCallID, "deny", "once")
	}
	// Swallow any other key while the modal is up.
	return m, nil
}

// postPermissionCmd returns a Cmd that POSTs a permission decision off the UI
// thread. Best-effort — a failed/404 POST is ignored (the proxy fail-safe and
// /cancel still bound the turn).
func (m *tuiModel) postPermissionCmd(sessionID, toolCallID, decision, scope string) tea.Cmd {
	proxyURL := m.proxyURL
	return func() tea.Msg {
		_ = postPermissionDecision(proxyURL, sessionID, toolCallID, decision, scope)
		return nil
	}
}

// handleKey runs the action bound to a key. While the permission modal is up,
// only its answer keys and Ctrl+C / Ctrl+D act. A key with no binding of its
// own goes on to the textarea.
func (m tuiModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Permission modal gates all input: while a request is pending only
	// the y/a/n (and esc) decision keys are live; every other key is
	// swallowed so it never reaches the textarea or the normal switch.
	// Ctrl+C / Ctrl+D still fall through to cancel/quit — cancelling the
	// turn unblocks the proxy's permission wait via the request context.
	if m.pendingPerm != nil {
		if s := msg.String(); s != "ctrl+c" && s != "ctrl+d" {
			return m.handlePermKey(msg)
		}
		m.pendingPerm = nil
	}
	switch msg.String() {
	case "ctrl+c":
		return m.cancelTurnOrQuit()
	case "ctrl+d":
		m.quitting = true
		return m, tea.Quit

	case "ctrl+l":
		m.chat = nil
		m.chatScroll = 0
		// Start a fresh persistence session so the cleared transcript
		// doesn't overwrite the saved one.
		m.startNewSession()
		return m, nil

	case "ctrl+t":
		return m.cycleMode()

	case "ctrl+r":
		if !m.turnActive && m.lastUserMsg != "" {
			m.chat = append(m.chat, chatMessage{
				Role: roleUser, Body: m.lastUserMsg,
			})
			return m, m.sendChatCmd(m.lastUserMsg + m.contextSuffix())
		}
		return m, nil

	case "pgup":
		m.scrollChat(10)
		return m, nil
	case "pgdown":
		m.scrollChat(-10)
		return m, nil
	case "ctrl+home":
		m.scrollChat(1 << 30) // clamped to lastChatTotal
		return m, nil
	case "ctrl+end":
		m.chatScroll = 0
		return m, nil
	case "enter":
		return m.submitInput()
	}
	return m.forwardToInput(msg)
}

// cancelTurnOrQuit answers Ctrl+C: it cancels the turn in flight, or quits
// when no turn runs.
func (m tuiModel) cancelTurnOrQuit() (tea.Model, tea.Cmd) {
	if m.turnActive && m.turnCancel != nil {
		// First Ctrl+C cancels the in-flight turn; second quits.
		// Belt-and-suspenders: cancel locally (closes TCP) AND
		// POST /cancel so the proxy aborts even when buffered.
		m.userCancelled = true
		m.turnCancel()
		sid := m.turnSessionID
		proxyURL := m.proxyURL
		m.turnActive = false
		// Stop the encoding/decoding tick from repainting after
		// cancel — otherwise the "encoding prompt … Ns" timer keeps
		// ticking forever because promptEvalStart/streamingLLM were
		// never cleared.
		m.promptEvalStart = time.Time{}
		m.streamingLLM = false
		m.chat = append(m.chat, chatMessage{
			Role: roleSystem, Meta: "cancelled",
			Body: "turn cancelled",
		})
		return m, func() tea.Msg {
			_ = cancelTurn(proxyURL, sid)
			return nil
		}
	}
	m.quitting = true
	return m, tea.Quit
}

// cycleMode answers Ctrl+T: it moves the permission mode on to the next one
// and notes the new mode in the chat.
func (m tuiModel) cycleMode() (tea.Model, tea.Cmd) {
	// Cycle permission mode. Visible in header.
	switch m.mode {
	case "default":
		m.mode = "accept-edits"
	case "accept-edits":
		m.mode = "yolo"
	default:
		m.mode = "default"
	}
	m.chat = append(m.chat, chatMessage{
		Role: roleSystem, Meta: "mode",
		Body: fmt.Sprintf("mode → %s", m.mode),
	})
	return m, nil
}

// submitInput answers Enter: it runs a line that starts with "!" as a shell
// command, runs a slash command, or sends the text to the agent as a new turn.
func (m tuiModel) submitInput() (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	// Enter sends; Shift+Enter (or Alt+Enter) inserts newline.
	// textarea handles Shift+Enter as KeyShiftEnter ("shift+enter").
	if m.turnActive {
		// Mid-turn Enter can't send — surface why instead of
		// silently inserting a newline into the pending input.
		m.showToast("turn in progress — Ctrl+C to cancel")
		return m, nil
	}
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return m, nil
	}
	m.input.Reset()
	dlog("user", "input", map[string]interface{}{"text": text})
	// Bash mode: leading "!" runs as a shell command in the
	// working dir, output appears as a system row. Same path
	// as /run but with the conversational shorthand devs
	// expect from Claude Code.
	if strings.HasPrefix(text, "!") {
		cmdStr := strings.TrimSpace(text[1:])
		if cmdStr == "" {
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "error",
				Body: "Bash mode: type ! followed by a command.",
			})
			return m, nil
		}
		m.chat = append(m.chat, chatMessage{
			Role: roleUser, Body: "! " + cmdStr, Echo: true,
		})
		return m, runShellCmd(m.workingDir, "!"+cmdStr,
			[]string{"bash", "-lc", cmdStr})
	}
	// "?" alone (or with trailing whitespace) is a shorthand
	// for /help — same convention as Claude Code so users
	// don't have to remember the slash form.
	if text == "?" {
		text = "/help"
	}
	// Slash commands intercepted before agent send.
	if consumed, slashCmd, quit := m.handleSlash(text); consumed {
		dlog("slash", "dispatched", map[string]interface{}{
			"input": text, "quit": quit,
		})
		if quit {
			m.quitting = true
		}
		if slashCmd != nil {
			cmds = append(cmds, slashCmd)
		}
		return m, tea.Batch(cmds...)
	}
	// Plain message → send to agent. Append context-files
	// hint so the agent knows the user's chosen scope.
	m.chat = append(m.chat, chatMessage{
		Role: roleUser, Body: text,
	})
	m.lastUserMsg = text
	dlog("turn", "started", map[string]interface{}{
		"session_id": "(set in sendChatCmd)",
		"len":        len(text),
	})
	cmds = append(cmds, m.sendChatCmd(text+m.contextSuffix()))
	return m, tea.Batch(cmds...)
}

// handleMouse answers the mouse: the wheel scrolls the pane under the cursor,
// and a left drag selects text in a pane and copies it on release.
func (m tuiModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// Wheel routes to whichever pane the cursor is over so events,
	// pipeline, files all scroll independently — not just chat.
	if msg.Action == tea.MouseActionPress {
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			m.scrollPaneAt(msg.X, msg.Y, 3)
			return m, nil
		case tea.MouseButtonWheelDown:
			m.scrollPaneAt(msg.X, msg.Y, -3)
			return m, nil
		}
	}
	// Highlight-to-copy in any pane. Press finds the pane under
	// (X,Y); motion updates the end; release extracts text from
	// that pane's snapshot and copies via OSC52 / CLI tool.
	switch msg.Action {
	case tea.MouseActionPress:
		if msg.Button == tea.MouseButtonLeft {
			m.startSelection(msg)
		}
	case tea.MouseActionMotion:
		if m.selecting {
			m.selEndX, m.selEndY = msg.X, msg.Y
		}
	case tea.MouseActionRelease:
		if m.selecting {
			return m.copySelection()
		}
	}
	return m, nil
}

// startSelection starts a highlight-to-copy selection when a left press lands
// inside a pane.
func (m *tuiModel) startSelection(msg tea.MouseMsg) {
	if pane := findPane(msg.X, msg.Y); pane != nil {
		m.selecting = true
		m.selPane = pane.name
		m.selStartX, m.selStartY = msg.X, msg.Y
		m.selEndX, m.selEndY = msg.X, msg.Y
		dlog("mouse", "press", map[string]interface{}{
			"x": msg.X, "y": msg.Y, "pane": pane.name,
			"paneTopY": pane.topY, "paneBottomY": pane.bottomY,
			"viewStart": pane.viewStart, "lines": len(pane.lines),
		})
	}
}

// copySelection ends the selection on release and copies the text it covers
// to the clipboard. A click with no drag copies nothing.
func (m tuiModel) copySelection() (tea.Model, tea.Cmd) {
	selPane := m.selPane
	m.selecting = false
	dy := m.selEndY - m.selStartY
	if dy < 0 {
		dy = -dy
	}
	dx := m.selEndX - m.selStartX
	if dx < 0 {
		dx = -dx
	}
	// Pure click (no drag) → no copy.
	if dy == 0 && dx < 2 {
		return m, nil
	}
	text := extractPaneSelection(selPane,
		m.selStartY, m.selEndY,
		m.selStartX, m.selEndX)
	dlog("mouse", "release", map[string]interface{}{
		"pane":   selPane,
		"startX": m.selStartX, "startY": m.selStartY,
		"endX": m.selEndX, "endY": m.selEndY,
		"text_len": len(text),
		"preview":  truncate(text, 60),
	})
	if text == "" {
		m.showToast("nothing to copy")
		return m, nil
	}
	if err := copyToClipboard(text); err != nil {
		m.showToast(fmt.Sprintf("copy failed: %v", err))
		return m, nil
	}
	m.showToast(fmt.Sprintf("✓ copied %d chars from %s pane",
		len(text), selPane))
	return m, nil
}

// forwardToInput hands a message that no handler took to the textarea, and
// sets the input mode from what is typed.
func (m tuiModel) forwardToInput(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	// Forward remaining keystrokes to the textarea (typing, arrows…).
	if !m.quitting {
		var taCmd tea.Cmd
		m.input, taCmd = m.input.Update(msg)
		cmds = append(cmds, taCmd)
		// Track input mode so the input-box border colors itself
		// (red=bash, purple=slash, default=cyan) and a completion
		// hint above the box can list matching commands.
		val := m.input.Value()
		switch {
		case strings.HasPrefix(val, "!"):
			m.inputMode = "bash"
		case strings.HasPrefix(val, "/"):
			m.inputMode = "slash"
		case strings.HasPrefix(val, "?"):
			m.inputMode = "help"
		default:
			m.inputMode = ""
		}
	}
	return m, tea.Batch(cmds...)
}

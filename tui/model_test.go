// Tests for the Bubbletea model — drive Update directly with synthetic
// messages and assert on the resulting state. Skips teatest's harness
// because direct Update calls give the same coverage with less plumbing.

package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// keyMsg builds a tea.KeyMsg matching what the runtime sends for a
// named key (e.g. "enter", "ctrl+l").
func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg(tea.Key{Type: tea.KeyEnter})
	case "ctrl+l":
		return tea.KeyMsg(tea.Key{Type: tea.KeyCtrlL})
	case "ctrl+t":
		return tea.KeyMsg(tea.Key{Type: tea.KeyCtrlT})
	case "ctrl+c":
		return tea.KeyMsg(tea.Key{Type: tea.KeyCtrlC})
	}
	// Default: treat as a single rune.
	return tea.KeyMsg(tea.Key{Type: tea.KeyRunes, Runes: []rune(s)})
}

// sized returns a model that has already received a WindowSizeMsg, so
// View() doesn't return the placeholder string.
func sized(width, height int) tuiModel {
	m := newTUIModel("http://test")
	updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return updated.(tuiModel)
}

// withStages returns a sized model with n pipeline stages started and
// enough chat rows that every pane renders at its full budget.
func withStages(t *testing.T, width, height, n int) tuiModel {
	t.Helper()
	m := sized(width, height)
	now := float64(time.Now().UnixNano()) / 1e9
	for i := 0; i < n; i++ {
		updated, _ := m.Update(envelopeMsg{ev: Envelope{
			EventID: fmt.Sprintf("e%d", i), Type: EvtStageStart,
			Stage: fmt.Sprintf("stage%d", i), Timestamp: now,
			Payload: map[string]interface{}{},
		}})
		m = updated.(tuiModel)
	}
	for i := 0; i < height; i++ {
		m.chat = append(m.chat,
			chatMessage{Role: roleUser, Body: fmt.Sprintf("ask %d", i)},
			chatMessage{Role: roleAssistant, Body: fmt.Sprintf("reply %d", i)})
	}
	return m
}

// One turn gets the configured context divided among the parallel slots; a
// model-neutral 32768 when the launcher set neither.
func TestConfiguredContextTokens(t *testing.T) {
	cases := []struct {
		name        string
		ctxSize     string
		slots       string
		wantPerTurn int
	}{
		{"context split among slots", "65536", "4", 16384},
		{"neutral fallback", "", "", 32768},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("ATLAS_CTX_SIZE", c.ctxSize)
			t.Setenv("ATLAS_PARALLEL_SLOTS", c.slots)
			if got := configuredContextTokens(); got != c.wantPerTurn {
				t.Errorf("configuredContextTokens() = %d, want %d", got, c.wantPerTurn)
			}
		})
	}
}

func TestEmptyEnterDoesNothing(t *testing.T) {
	m := sized(80, 30)
	updated, _ := m.Update(keyMsg("enter"))
	mu := updated.(tuiModel)
	if len(mu.chat) != 0 {
		t.Errorf("empty enter should not append to chat (got %d rows)", len(mu.chat))
	}
	if mu.turnActive {
		t.Errorf("empty enter should not start a turn")
	}
}

// Enter during an active turn surfaces a toast instead of silently
// inserting a newline into the pending input.
func TestEnterMidTurnShowsToast(t *testing.T) {
	m := sized(80, 30)
	m.turnActive = true
	m.input.SetValue("queued message")
	updated, _ := m.Update(keyMsg("enter"))
	mu := updated.(tuiModel)
	if got := mu.input.Value(); got != "queued message" {
		t.Errorf("input = %q, want unchanged (no newline)", got)
	}
	if len(mu.toasts) == 0 || !strings.Contains(mu.toasts[0].Body, "turn in progress") {
		t.Errorf("toasts = %+v, want a turn-in-progress notice", mu.toasts)
	}
	if !mu.turnActive {
		t.Errorf("mid-turn enter must not end the turn")
	}
}

func TestCtrlLClearsChat(t *testing.T) {
	m := sized(80, 30)
	m.chat = []chatMessage{
		{Role: roleUser, Body: "hi"},
		{Role: roleAssistant, Body: "hello"},
	}
	updated, _ := m.Update(keyMsg("ctrl+l"))
	mu := updated.(tuiModel)
	if len(mu.chat) != 0 {
		t.Errorf("ctrl+l should clear chat (got %d rows)", len(mu.chat))
	}
}

func TestCtrlTCyclesMode(t *testing.T) {
	m := sized(80, 30)
	want := []string{"accept-edits", "yolo", "default"}
	for i, w := range want {
		updated, _ := m.Update(keyMsg("ctrl+t"))
		m = updated.(tuiModel)
		if m.mode != w {
			t.Errorf("cycle %d: mode = %q, want %q", i, m.mode, w)
		}
	}
}

// Enter runs a slash command instead of sending it: /quit quits.
func TestEnterRunsASlashCommand(t *testing.T) {
	m := sized(80, 30)
	m.input.SetValue("/quit")
	updated, cmd := m.Update(keyMsg("enter"))
	if !updated.(tuiModel).quitting {
		t.Error("/quit did not quit")
	}
	if cmd == nil {
		t.Fatal("/quit returned no command")
	}
	if _, quits := cmd().(tea.QuitMsg); !quits {
		t.Errorf("/quit's command sends %T, want tea.QuitMsg", cmd())
	}
}

// A key with no binding of its own is typed into the textarea, and the input
// mode follows what is typed. A message that no handler takes reaches the
// textarea too.
func TestUnboundInputReachesTheTextarea(t *testing.T) {
	m := sized(80, 30)
	updated, _ := m.Update(keyMsg("!"))
	m = updated.(tuiModel)
	if got := m.input.Value(); got != "!" {
		t.Errorf("input = %q, want the typed key", got)
	}
	if m.inputMode != "bash" {
		t.Errorf("input mode = %q, want bash", m.inputMode)
	}
	m.input.SetValue("/help")
	updated, _ = m.Update(struct{}{})
	m = updated.(tuiModel)
	if m.inputMode != "slash" {
		t.Errorf("input mode after a message no handler takes = %q, want slash", m.inputMode)
	}
}

func TestEnvelopeMsgUpdatesPipelineState(t *testing.T) {
	m := sized(80, 30)
	updated, _ := m.Update(envelopeMsg{ev: Envelope{
		EventID: "e1", Type: EvtStageStart, Stage: "phase2",
		Timestamp: 1.0, Payload: map[string]interface{}{},
	}})
	m = updated.(tuiModel)
	stages := m.state.stages()
	if len(stages) != 1 || stages[0].Name != "phase2" {
		t.Fatalf("envelope didn't reach state: stages = %v", stages)
	}
	if len(m.envelope) != 1 {
		t.Errorf("envelope log length = %d, want 1", len(m.envelope))
	}
}

func TestChatStreamMsgAppendsAssistantText(t *testing.T) {
	m := sized(80, 30)
	updated, _ := m.Update(chatStreamMsg{ev: chatEvent{
		Type: "text", Data: json.RawMessage(`{"content": "hello world"}`),
	}})
	m = updated.(tuiModel)
	if len(m.chat) != 1 || m.chat[0].Role != roleAssistant || m.chat[0].Body != "hello world" {
		t.Errorf("chat = %+v, want one assistant row saying hello world", m.chat)
	}
}

func TestChatStreamMsgToolResultMarksSuccess(t *testing.T) {
	m := sized(80, 30)
	updated, _ := m.Update(chatStreamMsg{ev: chatEvent{
		Type: "tool_result", Data: json.RawMessage(`{"tool": "read_file", "success": true, "data": {"content": "42 lines"}}`),
	}})
	m = updated.(tuiModel)
	if len(m.chat) != 1 || !m.chat[0].Success {
		t.Errorf("expected one successful tool row; got %+v", m.chat)
	}
}

// The turn-end marker ends the turn, and shows its error as a system row
// when it carries one.
func TestTurnDoneMarkerEndsTheTurn(t *testing.T) {
	cases := []struct {
		name string
		data string
		rows []string // the bodies of the system rows it adds
	}{
		{"clean end", `{"err": ""}`, nil},
		{"end with an error", `{"err": "boom"}`, []string{"boom"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := sized(80, 30)
			m.turnActive = true
			updated, _ := m.Update(chatStreamMsg{ev: chatEvent{Type: "__turn_done__", Data: json.RawMessage(c.data)}})
			mu := updated.(tuiModel)
			if mu.turnActive {
				t.Errorf("turnActive should be false after __turn_done__")
			}
			if len(mu.chat) != len(c.rows) {
				t.Fatalf("chat = %+v, want %d row(s)", mu.chat, len(c.rows))
			}
			for i, want := range c.rows {
				if mu.chat[i].Role != roleSystem || mu.chat[i].Body != want {
					t.Errorf("row %d = %+v, want a system row %q", i, mu.chat[i], want)
				}
			}
		})
	}
}

func TestTickAdvancesSpinner(t *testing.T) {
	m := sized(80, 30)
	for i := 0; i < 3; i++ {
		updated, _ := m.Update(tickMsg(time.Now()))
		m = updated.(tuiModel)
	}
	if m.spinnerFrame != 3 {
		t.Errorf("spinnerFrame = %d, want 3", m.spinnerFrame)
	}
}

// Each of these messages reaches its own handler, and Update returns the
// model that handler changed with the command it asked for: a slash command's
// result shows as a row, a file scan replaces the sidebar's list, and the
// calibration badge keeps its first status, counts a retry and fetches again
// on a refresh.
func TestUpdateReturnsWhatEachHandlerChanged(t *testing.T) {
	status := &calibrationStatus{}
	cases := []struct {
		name    string
		msg     tea.Msg
		got     func(m tuiModel) interface{}
		want    interface{}
		wantCmd bool
	}{
		{"a slash command's result", slashResultMsg{command: "/run", output: "done"},
			func(m tuiModel) interface{} { return m.chat },
			[]chatMessage{{Role: roleSystem, Meta: "/run", Body: "done", Success: true}}, false},
		{"a file scan", scanFilesMsg{entries: []fileEntry{{RelPath: "a.go"}}, at: time.Now()},
			func(m tuiModel) interface{} { return m.fileEntries },
			[]fileEntry{{RelPath: "a.go"}}, false},
		{"the first calibration status", calibrationStatusMsg{status: status},
			func(m tuiModel) interface{} { return m.calibration }, status, true},
		{"a calibration retry", calibrationRetryMsg{},
			func(m tuiModel) interface{} { return m.calibrationRetries }, 1, true},
		{"a calibration refresh", calibrationRefreshMsg{},
			func(m tuiModel) interface{} { return m.calibration }, (*calibrationStatus)(nil), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := sized(80, 30)
			updated, cmd := m.Update(c.msg)
			if got := c.got(updated.(tuiModel)); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
			if (cmd != nil) != c.wantCmd {
				t.Errorf("a command came back: %v, want %v", cmd != nil, c.wantCmd)
			}
		})
	}
}

// The frame shows its panes, also before the terminal has sent its size:
// some terminals never send the first one, and the user must not be stuck on
// a placeholder.
func TestViewShowsThePanes(t *testing.T) {
	cases := []struct {
		name  string
		model tuiModel
		want  []string
	}{
		{"sized 120x40", sized(120, 40), []string{"Pipeline", "Chat", "Events", "Message"}},
		{"before the first size", newTUIModel("http://test"), []string{"Pipeline", "Chat", "Message"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			view := c.model.View()
			for _, want := range c.want {
				if !strings.Contains(view, want) {
					t.Errorf("view missing pane title %q", want)
				}
			}
		})
	}
}

// The rendered frame is exactly as tall as the terminal: one row over pushes
// the top of the alt screen out of view. That holds for any number of stages
// and in every mode: the "?" help is clipped to its budget, bash and slash
// modes add a hint row, and the permission modal fits in.
func TestViewHeightMatchesTerminal(t *testing.T) {
	cases := []struct {
		name          string
		width, height int
		stages        int
		inputMode     string
		input         string
		modal         bool
		show          []string // texts the frame must show as well
	}{
		{name: "100x30, no stage", width: 100, height: 30, stages: 0},
		{name: "100x30, one stage", width: 100, height: 30, stages: 1},
		{name: "100x30, two stages", width: 100, height: 30, stages: 2},
		{name: "100x30, four stages", width: 100, height: 30, stages: 4},
		{name: "80x24", width: 80, height: 24, stages: 2},
		{name: "120x40", width: 120, height: 40, stages: 2},
		{name: "90x26", width: 90, height: 26, stages: 2},
		{name: "help mode", width: 100, height: 30, stages: 1, inputMode: "help",
			show: []string{"/help for the full list"}},
		{name: "bash mode", width: 100, height: 30, stages: 1, inputMode: "bash"},
		{name: "slash mode", width: 100, height: 30, stages: 1, inputMode: "slash", input: "/"},
		{name: "permission modal", width: 100, height: 30, stages: 2, modal: true,
			show: []string{"Permission required", "[y] allow once"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := withStages(t, c.width, c.height, c.stages)
			m.inputMode = c.inputMode
			m.input.SetValue(c.input)
			if c.modal {
				m.pendingPerm = &permPrompt{toolName: "run_command", message: "Run command: npm install",
					toolCallID: "call_3", sessionID: "s1"}
			}
			view := m.View()
			if got := lipgloss.Height(view); got != c.height {
				t.Errorf("view height = %d, want %d", got, c.height)
			}
			for _, want := range c.show {
				if !strings.Contains(view, want) {
					t.Errorf("view missing %q", want)
				}
			}
		})
	}
}

// Each pane's snapshot holds the screen rows and columns its text fills, so
// the mouse finds the pane, and the pane's title is on the row just above. A
// hidden pane has no snapshot and the panes left take its rows. The frame is
// 120x40 with two stages: the header is row 0, and the input box, titled
// "Message", is the last six rows.
func TestPaneSnapshotsMatchTheFrame(t *testing.T) {
	type bounds struct {
		name                         string
		topY, bottomY, leftX, rightX int
		titled                       bool // the row above topY holds the pane's title
	}
	cases := []struct {
		name                                string
		hideFiles, hidePipeline, hideEvents bool
		panes                               []bounds // paneSnaps, in order
	}{
		{"all shown", false, false, false, []bounds{{"pipeline", 3, 4, 1, 90, true},
			{"chat", 8, 26, 1, 90, true}, {"events", 30, 31, 1, 90, true}, {"files", 3, 38, 95, 118, true}}},
		{"files hidden", true, false, false, []bounds{{"pipeline", 3, 4, 1, 116, true},
			{"chat", 8, 26, 1, 116, true}, {"events", 30, 31, 1, 116, true}}},
		{"pipeline hidden", false, true, false, []bounds{
			{"chat", 3, 26, 1, 90, true}, {"events", 30, 31, 1, 90, true}, {"files", 3, 38, 95, 118, true}}},
		{"events hidden", false, false, true, []bounds{
			{"pipeline", 3, 4, 1, 90, true}, {"chat", 8, 31, 1, 90, true}, {"files", 3, 38, 95, 118, true}}},
	}
	titles := map[string]string{"pipeline": "Pipeline", "chat": "Chat", "events": "Events", "files": "Files"}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := withStages(t, 120, 40, 2)
			m.hideFiles, m.hidePipeline, m.hideEvents = c.hideFiles, c.hidePipeline, c.hideEvents
			rows := strings.Split(ansi.Strip(m.View()), "\n")
			if len(rows) != 40 {
				t.Fatalf("frame has %d rows, want 40", len(rows))
			}
			var got []bounds
			for _, p := range paneSnaps {
				titled := strings.Contains(rows[p.topY-1], titles[p.name])
				got = append(got, bounds{p.name, p.topY, p.bottomY, p.leftX, p.rightX, titled})
			}
			if !reflect.DeepEqual(got, c.panes) {
				t.Errorf("snapshots = %+v, want %+v", got, c.panes)
			}
			if !strings.Contains(rows[35], "Message") {
				t.Errorf("row 35 = %q, want the input box's title", rows[35])
			}
		})
	}
}

// While a turn runs, the chat box ends with the "thinking…" row. Scrolled up,
// its title shows where the view is.
func TestChatBoxShowsTheTurnInFlightAndTheScroll(t *testing.T) {
	m := withStages(t, 120, 40, 2)
	m.turnActive = true
	m.chatScroll = 5
	view := ansi.Strip(m.View())
	if !strings.Contains(view, "(Ctrl+C to cancel)") {
		t.Error("no thinking row while a turn runs")
	}
	if !strings.Contains(view, "Chat — ↑") {
		t.Error("the chat title does not show the scroll position")
	}
}

func TestRenderHelpHintRespectsMaxLines(t *testing.T) {
	out := renderHelpHint(80, 9)
	if got := len(strings.Split(out, "\n")); got != 9 {
		t.Fatalf("help hint lines = %d, want 9", got)
	}
	// Unbounded budget returns the whole body.
	full := renderHelpHint(80, 0)
	want := strings.Count(slashCommandHelp, "\n") + 2 // + hint header
	if got := len(strings.Split(full, "\n")); got != want {
		t.Errorf("unclipped help hint lines = %d, want %d", got, want)
	}
}

// Wrapping cuts between runes, never inside a multi-byte one, keeps each line
// within the width, and loses no text (only the spaces at a cut).
func TestWrapPlainKeepsMultiByteRunesIntact(t *testing.T) {
	s := strings.Repeat("─", 30) + " — " + strings.Repeat("é", 20)
	lines := wrapPlain(s, 12)
	for _, line := range lines {
		if !utf8.ValidString(line) {
			t.Fatalf("wrapPlain produced invalid UTF-8: %q", line)
		}
		if n := len([]rune(line)); n > 12 {
			t.Errorf("line rune count = %d, want <= 12 (%q)", n, line)
		}
	}
	if strings.ReplaceAll(strings.Join(lines, ""), " ", "") != strings.ReplaceAll(s, " ", "") {
		t.Errorf("the lines %q lost text of %q", lines, s)
	}
}

// formatEventLine truncates styled lines by visible width — a byte
// slice would spend the budget on escape codes and cut mid-sequence.
func TestFormatEventLineTruncatesByVisibleWidth(t *testing.T) {
	ev := Envelope{Type: EvtError, Stage: "phase2", Timestamp: 1.0,
		Payload: map[string]interface{}{"message": strings.Repeat("x", 200)}}
	line := formatEventLine(ev, 40)
	if got := lipgloss.Width(line); got != 40 {
		t.Errorf("visible width = %d, want exactly 40", got)
	}
	if !utf8.ValidString(line) {
		t.Errorf("truncated line is invalid UTF-8")
	}
}

// With token counts the prompt row draws a 24-cell bar with round(pct × 24)
// cells filled, never more than 24. Without a total there is no bar, only a
// spinner and the time.
func TestFormatPromptProgressFillsA24CellBar(t *testing.T) {
	cases := []struct {
		name             string
		processed, total int
		pct              float64
		want             string
	}{
		{"half way: 12 cells", 500, 1000, 0.5,
			"encoding prompt  [" + strings.Repeat("█", 12) + strings.Repeat("░", 12) + "] 500/1000 (50%)  · 0.0s"},
		{"done: 24 cells", 1000, 1000, 1.0,
			"encoding prompt  [" + strings.Repeat("█", 24) + "] 1000/1000 (100%)  · 0.0s"},
		{"above 100%: still 24 cells", 1000, 1000, 1.2,
			"encoding prompt  [" + strings.Repeat("█", 24) + "] 1000/1000 (100%)  · 0.0s"},
		{"no total: no bar", 0, 0, 0.5, "encoding prompt  ⠋  0.0s"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formatPromptProgress(c.processed, c.total, c.pct, 0); got != c.want {
				t.Errorf("formatPromptProgress = %q, want %q", got, c.want)
			}
		})
	}
}

// Proxy stream-health events each render one muted system row.
func TestChatStreamRendersProxyInterventionEvents(t *testing.T) {
	cases := []struct {
		typ     string
		payload string
		want    string
	}{
		{"agent_reasoning_intervention", `{"turn": 3, "consecutive": 2, "reason": "Stop repeating."}`,
			"REASONING REPEAT at turn 3"},
		{"content_loop_cut", `{"chars": 1234}`,
			"content loop detected — stream cut after 1234 chars"},
		{"old_str_cut", `{"path": "executor_server.py", "chars": 803, "matched_lines": 8}`,
			"edit_file old_str stopped matching executor_server.py after 8 line(s)"},
		{"reasoning_budget_cut", `{"reasoning_chars": 9000}`,
			"reasoning budget exceeded (9000 chars"},
		{"symbol_index_injected", `{"matched": ["foo", "bar"], "n_files": 4, "skipped": 1}`,
			"injected 2 symbol snippet(s) from 4 project file(s)"},
	}
	for _, tc := range cases {
		t.Run(tc.typ, func(t *testing.T) {
			m := sized(80, 30)
			updated, _ := m.Update(chatStreamMsg{ev: chatEvent{Type: tc.typ, Data: json.RawMessage(tc.payload)}})
			mu := updated.(tuiModel)
			if len(mu.chat) != 1 || mu.chat[0].Role != roleSystem {
				t.Fatalf("chat = %+v, want one system row", mu.chat)
			}
			if !strings.Contains(mu.chat[0].Body, tc.want) {
				t.Errorf("body = %q, want it to contain %q", mu.chat[0].Body, tc.want)
			}
		})
	}
}

// Some events show one row per event: an attempt to fetch a file through the
// fence (#254: how it went, and what the wire showed when a watchdog cut it);
// a step in repairing a file left unparseable (#214: when it opens, when an
// attempt changes it and it still fails, when it is fixed, when it is handed
// back; a refused attempt changed nothing and shows no row); and a denied
// permission (the reason, when nobody was asked).
func TestEventRows(t *testing.T) {
	const refusal = "delete_file: the object could not be held for approval on this platform. " +
		"Nobody was asked, and nothing was deleted."
	cases := []struct {
		name   string
		typ    string
		events []string
		meta   string
		want   []string
	}{
		{"fenced fetch", "fenced_fetch", []string{
			`{"path": "store.py", "attempt": 1, "grammar": "fence", "outcome": "unusable",
			  "elapsed_ms": 38600, "content_chars": 709, "cut": "idle"}`,
			`{"path": "store.py", "attempt": 2, "grammar": "raw", "outcome": "stalled",
			  "elapsed_ms": 25100, "content_chars": 0, "reasoning_chars": 1840, "cut": "stalled"}`,
			`{"path": "app.py", "attempt": 1, "grammar": "fence", "outcome": "used",
			  "elapsed_ms": 4200, "content_chars": 512}`,
		}, "fenced", []string{
			"fenced store.py, attempt 1 (fence): unusable, 709 chars in 38.6s, cut by the idle watchdog",
			"fenced store.py, attempt 2 (raw): stalled, 0 chars in 25.1s, cut by the stalled watchdog, 1840 reasoning chars",
			"fenced app.py, attempt 1 (fence): used, 512 chars in 4.2s",
		}},
		{"repair", "repair", []string{
			`{"event": "opened", "path": "mod.py", "tool": "write_file", "landed": true,
			  "error": "SyntaxError: invalid syntax (line 4)"}`,
			`{"event": "attempt", "path": "mod.py", "tool": "edit_file", "landed": false,
			  "error": "old_str not found in mod.py"}`,
			`{"event": "attempt", "path": "mod.py", "tool": "replace_lines", "landed": true,
			  "error": "SyntaxError: invalid syntax (line 5)"}`,
			`{"event": "closed", "path": "mod.py", "how": "parses", "attempts": 3}`,
			`{"event": "opened", "path": "b.py", "tool": "write_file", "landed": true,
			  "error": "SyntaxError: '(' was never closed (line 2)"}`,
			`{"event": "handoff", "path": "b.py", "attempts": 1}`,
		}, "repair", []string{
			"mod.py does not parse: SyntaxError: invalid syntax (line 4)",
			"mod.py still does not parse after replace_lines: SyntaxError: invalid syntax (line 5)",
			"mod.py parses again (3 attempts)",
			"b.py does not parse: SyntaxError: '(' was never closed (line 2)",
			"b.py still does not parse after 1 attempt: handed back to you",
		}},
		{"permission denied", "permission_denied", []string{
			// Denied by the user: the row it always had.
			`{"tool": "run_command"}`,
			// Refused before anyone was asked: the reason.
			`{"tool": "delete_file", "reason": "` + refusal + `"}`,
		}, "denied", []string{
			"permission denied for run_command",
			refusal,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &tuiModel{}
			for _, ev := range c.events {
				m.appendChatEvent(chatEvent{Type: c.typ, Data: json.RawMessage(ev)})
			}
			var got []string
			for _, row := range m.chat {
				if row.Meta != c.meta {
					t.Errorf("row %q has meta %q, want %q", row.Body, row.Meta, c.meta)
				}
				got = append(got, row.Body)
			}
			if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
				t.Errorf("rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(c.want, "\n"))
			}
		})
	}
}

// The files pane pads BELOW its content (top-anchored); the selection
// mapper must not apply the top-pad offset the other panes need.
func TestExtractPaneSelectionFilesPaneTopAnchored(t *testing.T) {
	old := paneSnaps
	defer func() { paneSnaps = old }()
	paneSnaps = []paneSnapshot{{
		name: "files", topY: 3, bottomY: 12, leftX: 1, rightX: 24,
		viewStart: 0,
		lines:     []string{"● root", "  a.go", "  b.go"},
		padBottom: true,
	}}
	if got := extractPaneSelection("files", 4, 4, 1, 24); got != "  a.go" {
		t.Errorf("row 4 selection = %q, want %q", got, "  a.go")
	}
	// Rows in the trailing padding have nothing to copy.
	if got := extractPaneSelection("files", 9, 10, 1, 24); got != "" {
		t.Errorf("padding selection = %q, want empty", got)
	}
}

// The wheel scrolls the pane under the cursor. A left press there starts a
// selection, and a release with no drag ends it without copying anything.
func TestMouseScrollsAndSelectsThePaneUnderIt(t *testing.T) {
	old := paneSnaps
	defer func() { paneSnaps = old }()
	paneSnaps = []paneSnapshot{{name: "events", topY: 5, bottomY: 9, leftX: 1, rightX: 60,
		lines: []string{"one", "two", "three", "four", "five", "six"}}}
	m := sized(80, 30)

	updated, _ := m.Update(tea.MouseMsg{X: 10, Y: 6, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	m = updated.(tuiModel)
	if m.eventsScroll != 3 {
		t.Errorf("events scroll after a wheel turn up = %d, want 3", m.eventsScroll)
	}

	updated, _ = m.Update(tea.MouseMsg{X: 10, Y: 6, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m = updated.(tuiModel)
	if !m.selecting {
		t.Error("a left press in a pane did not start a selection")
	}
	if m.selPane != "events" {
		t.Errorf("selection pane = %q, want events", m.selPane)
	}

	updated, _ = m.Update(tea.MouseMsg{X: 10, Y: 6, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
	m = updated.(tuiModel)
	if m.selecting {
		t.Error("the release did not end the selection")
	}
	if len(m.toasts) != 0 {
		t.Errorf("a click with no drag showed %+v, want nothing copied", m.toasts)
	}
}

// The history sent with a message holds the earlier user and assistant text
// rows. It leaves out the message being sent, tool and system rows, echoes
// of slash and bash input, and empty rows. Assistant bodies are re-wrapped in
// the JSON envelope shape so the model keeps emitting JSON next turn (raw
// text in history teaches the model that text-only is OK and breaks the
// envelope contract).
func TestBuildChatHistory(t *testing.T) {
	cases := []struct {
		name string
		chat []chatMessage
		want []historyMessage
	}{
		{"empty chat", nil, nil},
		{"tool and system rows are left out", []chatMessage{
			{Role: roleUser, Body: "first ask"},
			{Role: roleAssistant, Body: "first reply"},
			{Role: roleTool, Body: "list_directory result", Meta: "list_directory"},
			{Role: roleSystem, Body: "spinner update", Meta: "llm"},
			{Role: roleUser, Body: "current message — being sent now"},
		}, []historyMessage{
			{Role: "user", Content: "first ask"},
			{Role: "assistant", Content: `{"content":"first reply","type":"text"}`},
		}},
		{"echo rows are left out", []chatMessage{
			{Role: roleUser, Body: "real ask"},
			{Role: roleAssistant, Body: "real reply"},
			{Role: roleUser, Body: "/review", Echo: true}, // slash echo
			{Role: roleUser, Body: "! ls", Echo: true},    // bash echo
			{Role: roleUser, Body: "current"},
		}, []historyMessage{
			{Role: "user", Content: "real ask"},
			{Role: "assistant", Content: `{"content":"real reply","type":"text"}`},
		}},
		{"empty rows are left out", []chatMessage{
			{Role: roleUser, Body: "real ask"},
			{Role: roleAssistant, Body: ""},
			{Role: roleAssistant, Body: "real reply"},
			{Role: roleUser, Body: "current"},
		}, []historyMessage{
			{Role: "user", Content: "real ask"},
			{Role: "assistant", Content: `{"content":"real reply","type":"text"}`},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newTUIModel("http://test")
			m.chat = c.chat
			if got := m.buildChatHistory(); !reflect.DeepEqual(got, c.want) {
				t.Errorf("buildChatHistory = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestBuildChatHistoryCapsAt40(t *testing.T) {
	m := newTUIModel("http://test")
	// 30 user/assistant pairs = 60 rows, plus the just-sent user row.
	for i := 0; i < 30; i++ {
		m.chat = append(m.chat,
			chatMessage{Role: roleUser, Body: "u"},
			chatMessage{Role: roleAssistant, Body: "a"})
	}
	m.chat = append(m.chat, chatMessage{Role: roleUser, Body: "current"})
	got := m.buildChatHistory()
	if len(got) != 40 {
		t.Errorf("buildChatHistory cap = %d, want 40", len(got))
	}
	// Cap keeps the most recent rows, not the oldest. Last row is an
	// assistant — re-wrapped in the JSON envelope.
	wantLast := `{"content":"a","type":"text"}`
	if got[len(got)-1].Content != wantLast {
		t.Errorf("last history row = %q, want %q (most-recent assistant, wrapped)", got[len(got)-1].Content, wantLast)
	}
}

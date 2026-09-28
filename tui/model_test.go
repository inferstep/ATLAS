// Tests for the Bubbletea model — drive Update directly with synthetic
// messages and assert on the resulting state. Skips teatest's harness
// because direct Update calls give the same coverage with less plumbing.

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
	case "ctrl+r":
		return tea.KeyMsg(tea.Key{Type: tea.KeyCtrlR})
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

func TestConfiguredContextTokensUsesPerSlotRuntimeConfig(t *testing.T) {
	t.Setenv("ATLAS_CTX_SIZE", "131072")
	t.Setenv("ATLAS_PARALLEL_SLOTS", "4")
	if got := configuredContextTokens(); got != 32768 {
		t.Fatalf("configuredContextTokens() = %d, want 32768", got)
	}
}

func TestConfiguredContextTokensHasModelNeutralFallback(t *testing.T) {
	t.Setenv("ATLAS_CTX_SIZE", "")
	t.Setenv("ATLAS_PARALLEL_SLOTS", "")
	if got := configuredContextTokens(); got != 32768 {
		t.Fatalf("configuredContextTokens() = %d, want neutral fallback 32768", got)
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
	payload, _ := json.Marshal(map[string]string{"content": "hello world"})
	updated, _ := m.Update(chatStreamMsg{ev: chatEvent{
		Type: "text", Data: payload,
	}})
	m = updated.(tuiModel)
	if len(m.chat) != 1 || m.chat[0].Role != roleAssistant {
		t.Fatalf("chat = %+v, want one assistant row", m.chat)
	}
	if m.chat[0].Body != "hello world" {
		t.Errorf("body = %q, want %q", m.chat[0].Body, "hello world")
	}
}

func TestChatStreamMsgToolResultMarksSuccess(t *testing.T) {
	m := sized(80, 30)
	payload, _ := json.Marshal(map[string]interface{}{
		"tool":    "read_file",
		"success": true,
		"data":    json.RawMessage(`{"content": "42 lines"}`),
	})
	updated, _ := m.Update(chatStreamMsg{ev: chatEvent{
		Type: "tool_result", Data: payload,
	}})
	m = updated.(tuiModel)
	if len(m.chat) != 1 || !m.chat[0].Success {
		t.Errorf("expected one successful tool row; got %+v", m.chat)
	}
}

func TestTurnDoneSentinelClearsActive(t *testing.T) {
	m := sized(80, 30)
	m.turnActive = true
	payload, _ := json.Marshal(map[string]string{"err": ""})
	updated, _ := m.Update(chatStreamMsg{ev: chatEvent{
		Type: "__turn_done__", Data: payload,
	}})
	m = updated.(tuiModel)
	if m.turnActive {
		t.Errorf("turnActive should be false after __turn_done__")
	}
}

func TestTurnDoneWithErrorAppendsSystemMsg(t *testing.T) {
	m := sized(80, 30)
	m.turnActive = true
	payload, _ := json.Marshal(map[string]string{"err": "boom"})
	updated, _ := m.Update(chatStreamMsg{ev: chatEvent{
		Type: "__turn_done__", Data: payload,
	}})
	m = updated.(tuiModel)
	if len(m.chat) != 1 || m.chat[0].Role != roleSystem {
		t.Fatalf("expected one system error row; got %+v", m.chat)
	}
	if m.chat[0].Body != "boom" {
		t.Errorf("body = %q", m.chat[0].Body)
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

func TestViewContainsPaneTitlesWhenSized(t *testing.T) {
	m := sized(120, 40)
	out := m.View()
	for _, want := range []string{"Pipeline", "Chat", "Events", "Message"} {
		if !strings.Contains(out, want) {
			t.Errorf("view missing pane title %q", want)
		}
	}
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

// The rendered frame must occupy exactly the terminal height: one row
// over pushes the top of the alt screen out of view.
func TestViewHeightMatchesTerminal(t *testing.T) {
	for _, stages := range []int{0, 1, 2, 4} {
		m := withStages(t, 100, 30, stages)
		if got := lipgloss.Height(m.View()); got != 30 {
			t.Errorf("100x30 stages=%d: view height = %d, want 30", stages, got)
		}
	}
	for _, tc := range []struct{ w, h int }{{80, 24}, {120, 40}, {90, 26}} {
		m := withStages(t, tc.w, tc.h, 2)
		if got := lipgloss.Height(m.View()); got != tc.h {
			t.Errorf("%dx%d: view height = %d, want %d", tc.w, tc.h, got, tc.h)
		}
	}
}

// The "?" help hint is clipped to its layout budget, so help mode still
// renders a frame exactly as tall as the terminal.
func TestHelpModeViewHeightMatchesTerminal(t *testing.T) {
	m := withStages(t, 100, 30, 1)
	m.inputMode = "help"
	out := m.View()
	if got := lipgloss.Height(out); got != 30 {
		t.Errorf("help view height = %d, want 30", got)
	}
	if !strings.Contains(out, "/help for the full list") {
		t.Errorf("clipped help should point at /help for the rest")
	}
}

// bash/slash modes reserve one extra row for their hint banner.
func TestHintModeViewHeightMatchesTerminal(t *testing.T) {
	for _, mode := range []string{"bash", "slash"} {
		m := withStages(t, 100, 30, 1)
		m.inputMode = mode
		if mode == "slash" {
			m.input.SetValue("/")
		}
		if got := lipgloss.Height(m.View()); got != 30 {
			t.Errorf("%s view height = %d, want 30", mode, got)
		}
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

func TestWrapPlainKeepsMultiByteRunesIntact(t *testing.T) {
	s := strings.Repeat("─", 30) + " — " + strings.Repeat("é", 20)
	for _, line := range wrapPlain(s, 12) {
		if !utf8.ValidString(line) {
			t.Fatalf("wrapPlain produced invalid UTF-8: %q", line)
		}
		if n := len([]rune(line)); n > 12 {
			t.Errorf("line rune count = %d, want <= 12 (%q)", n, line)
		}
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
	if mu.turnActive != true {
		t.Errorf("mid-turn enter must not end the turn")
	}
}

func TestViewBeforeWindowSizeRendersWithSafeDefaults(t *testing.T) {
	// Some terminals don't reliably emit an initial WindowSizeMsg —
	// View must render the actual UI with safe defaults so the user
	// isn't stuck on a placeholder screen.
	m := newTUIModel("http://test")
	out := m.View()
	for _, want := range []string{"Pipeline", "Chat", "Message"} {
		if !strings.Contains(out, want) {
			t.Errorf("pre-size view missing %q; got %q", want, out)
		}
	}
}

// Proxy stream-health events each render one muted system row.
func TestChatStreamRendersProxyInterventionEvents(t *testing.T) {
	cases := []struct {
		typ     string
		payload interface{}
		want    string
	}{
		{"agent_reasoning_intervention",
			map[string]interface{}{"turn": 3, "consecutive": 2, "reason": "Stop repeating."},
			"REASONING REPEAT at turn 3"},
		{"content_loop_cut",
			map[string]interface{}{"chars": 1234},
			"content loop detected — stream cut after 1234 chars"},
		{"reasoning_budget_cut",
			map[string]interface{}{"reasoning_chars": 9000},
			"reasoning budget exceeded (9000 chars"},
		{"symbol_index_injected",
			map[string]interface{}{"matched": []string{"foo", "bar"}, "n_files": 4, "skipped": 1},
			"injected 2 symbol snippet(s) from 4 project file(s)"},
	}
	for _, tc := range cases {
		t.Run(tc.typ, func(t *testing.T) {
			m := sized(80, 30)
			data, _ := json.Marshal(tc.payload)
			updated, _ := m.Update(chatStreamMsg{ev: chatEvent{Type: tc.typ, Data: data}})
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

func TestBuildChatHistoryEmpty(t *testing.T) {
	m := newTUIModel("http://test")
	if got := m.buildChatHistory(); got != nil {
		t.Errorf("buildChatHistory on empty chat = %v, want nil", got)
	}
}

func TestBuildChatHistoryExcludesLastUserAndNonTextRoles(t *testing.T) {
	m := newTUIModel("http://test")
	m.chat = []chatMessage{
		{Role: roleUser, Body: "first ask"},
		{Role: roleAssistant, Body: "first reply"},
		{Role: roleTool, Body: "list_directory result", Meta: "list_directory"},
		{Role: roleSystem, Body: "spinner update", Meta: "llm"},
		{Role: roleUser, Body: "current message — being sent now"},
	}
	got := m.buildChatHistory()
	// Assistant bodies are re-wrapped in the JSON envelope shape so the
	// model keeps emitting JSON next turn (raw text in history teaches
	// the model that text-only is OK and breaks the envelope contract).
	want := []historyMessage{
		{Role: "user", Content: "first ask"},
		{Role: "assistant", Content: `{"content":"first reply","type":"text"}`},
	}
	if len(got) != len(want) {
		t.Fatalf("buildChatHistory len = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("buildChatHistory[%d] = %+v, want %+v", i, got[i], w)
		}
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

func TestBuildChatHistorySkipsEchoRows(t *testing.T) {
	m := newTUIModel("http://test")
	m.chat = []chatMessage{
		{Role: roleUser, Body: "real ask"},
		{Role: roleAssistant, Body: "real reply"},
		{Role: roleUser, Body: "/review", Echo: true}, // slash echo
		{Role: roleUser, Body: "! ls", Echo: true},    // bash echo
		{Role: roleUser, Body: "current"},
	}
	got := m.buildChatHistory()
	if len(got) != 2 {
		t.Fatalf("buildChatHistory len = %d, want 2 (echo rows excluded): %v", len(got), got)
	}
	for _, h := range got {
		if strings.HasPrefix(h.Content, "/") || strings.HasPrefix(h.Content, "!") {
			t.Errorf("echo row leaked into agent history: %+v", h)
		}
	}
}

func TestBuildChatHistorySkipsEmptyBodies(t *testing.T) {
	m := newTUIModel("http://test")
	m.chat = []chatMessage{
		{Role: roleUser, Body: "real ask"},
		{Role: roleAssistant, Body: ""}, // empty assistant — skip
		{Role: roleAssistant, Body: "real reply"},
		{Role: roleUser, Body: "current"},
	}
	got := m.buildChatHistory()
	if len(got) != 2 {
		t.Fatalf("buildChatHistory len = %d, want 2 (got=%v)", len(got), got)
	}
}

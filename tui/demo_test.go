package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
)

func TestFormatDemoModelLabel(t *testing.T) {
	tests := map[string]string{
		"orion-code-10b-it-Q4_K_M":                "Orion code 10B",
		"nova-7B-Q6_K":                            "Nova 7B",
		"/models/atlas-test-3.1-8B-Instruct.gguf": "Atlas test 3.1 8B",
		"": demoModelFallback,
	}
	for input, want := range tests {
		t.Run(input, func(t *testing.T) {
			if got := formatDemoModelLabel(input); got != want {
				t.Fatalf("formatDemoModelLabel(%q) = %q, want %q", input, got, want)
			}
		})
	}
}

func TestFetchDemoModelIdentity(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		wantID string
		want   string
	}{
		{"configured model", http.StatusOK, `{"object":"list","data":[{"id":"orion-code-10b-it-Q4_K_M"}]}`, "orion-code-10b-it-Q4_K_M", "Orion code 10B"},
		{"empty list", http.StatusOK, `{"object":"list","data":[]}`, "", demoModelFallback},
		{"missing id", http.StatusOK, `{"object":"list","data":[{"id":""}]}`, "", demoModelFallback},
		{"malformed", http.StatusOK, `{`, "", demoModelFallback},
		{"upstream failure", http.StatusServiceUnavailable, `unavailable`, "", demoModelFallback},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					t.Errorf("path = %q, want /v1/models", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			id, label := fetchDemoModelIdentity(srv.URL)
			if id != tc.wantID || label != tc.want {
				t.Fatalf("identity = (%q, %q), want (%q, %q)", id, label, tc.wantID, tc.want)
			}
		})
	}
}

func TestProxySupportsRawDemoRequiresCapability(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"current proxy", `{"capabilities":["demo_raw_completion_v1"]}`, true},
		{"old proxy", `{"status":"ok"}`, false},
		{"malformed", `{`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/health" {
					t.Fatalf("path = %q, want /health", r.URL.Path)
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			if got := proxySupportsRawDemo(srv.URL); got != tc.want {
				t.Fatalf("proxySupportsRawDemo = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRunDemoRejectsOldProxyBeforeLaunching(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	err := runDemo(srv.URL, t.TempDir(), "short")
	if err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("runDemo error = %v, want stale-proxy rejection", err)
	}
}

func TestDemoRawStreamNeverEntersAgentEndpoint(t *testing.T) {
	rawCalls, agentCalls := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			rawCalls++
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"raw answer\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		case "/v1/agent":
			agentCalls++
			http.Error(w, "raw side entered agent", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	m := &demoModel{
		proxyURL: srv.URL,
		modelID:  "configured-model",
		prompt:   demoPrompt{Prompt: "build it"},
		events:   make(chan demoEvent, 16),
		ctx:      context.Background(),
	}
	m.runStream("raw")
	if rawCalls != 1 || agentCalls != 0 {
		t.Fatalf("raw calls = %d, agent calls = %d", rawCalls, agentCalls)
	}
}

func TestDemoLabelsRawCompletionAsModelNotAgent(t *testing.T) {
	rawChild := newTUIModel("http://unused")
	v3Child := newTUIModel("http://unused")
	payload, _ := json.Marshal(map[string]string{"content": "raw answer"})
	m := &demoModel{
		rawChild: &rawChild,
		v3Child:  &v3Child,
		events:   make(chan demoEvent, 1),
		ctx:      context.Background(),
	}
	_, _ = m.Update(demoBatchMsg{stream: []demoStreamMsg{{
		side: "raw",
		evt:  chatEvent{Type: "text", Data: payload},
	}}})
	if len(m.rawChild.chat) != 1 || m.rawChild.chat[0].Meta != "raw model" {
		t.Fatalf("raw chat rows = %#v", m.rawChild.chat)
	}
}

func TestDemoTitlesDescribeActualComparison(t *testing.T) {
	m := &demoModel{modelLabel: "Orion code 10B"}
	left := m.rawTitle()
	right := m.atlasTitle()
	for _, title := range []string{left, right} {
		if !strings.Contains(title, "Orion code 10B") {
			t.Fatalf("title %q does not identify configured model", title)
		}
		if strings.Contains(title, "RAW 9B") || strings.Contains(title, "no V3 orchestration") {
			t.Fatalf("title retains misleading legacy wording: %q", title)
		}
	}
	if !strings.Contains(left, "RAW MODEL") || !strings.Contains(left, "NO ORCHESTRATION") {
		t.Fatalf("baseline title is not explicit about comparison: %q", left)
	}
	if !strings.Contains(right, "ATLAS V3") {
		t.Fatalf("V3 title is not explicit: %q", right)
	}
}

func TestDemoTitleFallsBackWithoutMetadata(t *testing.T) {
	m := &demoModel{}
	if got := m.rawTitle(); !strings.HasPrefix(got, demoModelFallback) {
		t.Fatalf("baseline title = %q, want neutral fallback", got)
	}
}

func TestDemoLiveAndOutputViewsUseResolvedTitles(t *testing.T) {
	rawChild := newTUIModel("http://unused")
	v3Child := newTUIModel("http://unused")
	m := &demoModel{
		modelLabel:  "Orion code 10B",
		prompt:      demoPrompt{Prompt: "build it"},
		promptShown: len("build it"),
		width:       180,
		height:      36,
		rawChild:    &rawChild,
		v3Child:     &v3Child,
		v3Sandbox:   ".demo-v3-test",
		activePane:  "v3",
	}

	assertTitles := func(name, view string) {
		t.Helper()
		for _, want := range []string{"Orion code 10B", "RAW MODEL", "NO ORCHESTRATION", "ATLAS V3"} {
			if !strings.Contains(view, want) {
				t.Fatalf("%s view missing %q", name, want)
			}
		}
		for _, stale := range []string{"RAW 9B", "no V3 orchestration"} {
			if strings.Contains(view, stale) {
				t.Fatalf("%s view contains stale label %q", name, stale)
			}
		}
	}

	assertTitles("live", m.View())
	m.outputMode = true
	m.rawChild.chat = append(m.rawChild.chat, chatMessage{
		Role: roleAssistant, Meta: "raw model", Body: "raw response survives",
	})
	assertTitles("output", m.View())
	if output := m.View(); !strings.Contains(output, "raw response") || !strings.Contains(output, "survives") {
		t.Fatal("output view discarded the raw model response")
	}
	if output := m.View(); !strings.Contains(output, "raw model") {
		t.Fatal("raw response is still labeled as an agent response")
	}
}

// The done marker must trail every buffered stream event — it's sent
// by the forwarding loop only after the stream channel closes.
func TestDemoStreamDoneArrivesAfterAllEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 20; i++ {
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"tok%d\"}}]}\n\n", i)
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	m := &demoModel{
		proxyURL: srv.URL,
		modelID:  "m",
		prompt:   demoPrompt{Prompt: "p"},
		events:   make(chan demoEvent, 256),
		ctx:      context.Background(),
	}
	m.runStream("raw")

	var seen []demoEvent
drain:
	for {
		select {
		case ev := <-m.events:
			seen = append(seen, ev)
		default:
			break drain
		}
	}
	if len(seen) == 0 {
		t.Fatal("no events forwarded")
	}
	for i, ev := range seen[:len(seen)-1] {
		if ev.done != nil {
			t.Fatalf("done marker at position %d/%d, before the stream drained", i, len(seen))
		}
	}
	if seen[len(seen)-1].done == nil {
		t.Fatalf("last event is not the done marker: %#v", seen[len(seen)-1])
	}
}

// newDemoModel creates a sandbox for the V3 side only — the raw lane
// is a direct completion with no filesystem tools, so no .demo-raw-*
// dirs accumulate in the workspace.
func TestNewDemoModelCreatesOnlyV3Sandbox(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m"}]}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	m, err := newDemoModel(srv.URL, dir, "short")
	if err != nil {
		t.Fatalf("newDemoModel: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("workspace entries = %d, want only the v3 sandbox", len(entries))
	}
	if name := entries[0].Name(); !strings.HasPrefix(name, ".demo-v3-") || name != m.v3Sandbox {
		t.Fatalf("sandbox dir = %q, want %q (.demo-v3-*)", name, m.v3Sandbox)
	}
}

// The prompt type-out advances per rune so multi-byte characters are
// never split into invalid UTF-8 mid-animation.
func TestDemoPromptTypeOutIsRuneSafe(t *testing.T) {
	rawChild := newTUIModel("http://unused")
	v3Child := newTUIModel("http://unused")
	m := &demoModel{
		prompt:     demoPrompt{Prompt: "fix — draw ─ boxes"},
		width:      120,
		height:     30,
		rawChild:   &rawChild,
		v3Child:    &v3Child,
		activePane: "v3",
		// streamsFired keeps the tick handler from launching real requests.
		streamsFired: true,
	}
	promptLen := len([]rune(m.prompt.Prompt))
	for i := 0; i < promptLen+2; i++ {
		if view := m.View(); !utf8.ValidString(view) {
			t.Fatalf("view contains invalid UTF-8 at promptShown=%d", m.promptShown)
		}
		_, _ = m.Update(demoTickMsg(time.Now()))
	}
	if m.promptShown != promptLen {
		t.Fatalf("promptShown = %d, want rune count %d", m.promptShown, promptLen)
	}
}

func TestDemoPromptStatusDoesNotInventZeroPercentProgress(t *testing.T) {
	child := &tuiModel{
		promptTotal:     3000,
		promptEvalStart: time.Now(),
	}
	if got := streamStatus(child, false, false, nil); got != "processing prompt…" {
		t.Fatalf("status = %q, want indeterminate prompt progress", got)
	}
}

func TestDemoPromptStatusUsesRealSlotProgress(t *testing.T) {
	child := &tuiModel{
		promptProcessed: 750,
		promptTotal:     3000,
		promptPct:       0.25,
		promptEvalStart: time.Now(),
	}
	if got := streamStatus(child, false, false, nil); got != "processing prompt 25%" {
		t.Fatalf("status = %q, want current wire-format percentage", got)
	}
}

// A new terminal size gives each pane half the width less the borders, and the
// height less the title rows.
func TestDemoWindowSizeSizesBothPanes(t *testing.T) {
	rawChild := newTUIModel("http://unused")
	v3Child := newTUIModel("http://unused")
	m := &demoModel{rawChild: &rawChild, v3Child: &v3Child}
	_, cmd := m.Update(tea.WindowSizeMsg{Width: 124, Height: 40})
	if m.rawChild.width != 60 {
		t.Errorf("raw pane width = %d, want 60", m.rawChild.width)
	}
	if m.v3Child.height != 36 {
		t.Errorf("V3 pane height = %d, want 36", m.v3Child.height)
	}
	if cmd != nil {
		t.Error("a new size returned a command")
	}
}

// Keys and the wheel act on the focused pane: Tab moves the focus, Up scrolls
// one row in the output review only, the wheel focuses the side under the
// cursor and scrolls it, and q quits.
func TestDemoKeysAndWheelMoveTheFocusedPane(t *testing.T) {
	cases := []struct {
		name       string
		outputMode bool
		pane       string // the focused pane before the message
		msg        tea.Msg
		wantPane   string
		wantRaw    int // rows the raw pane is scrolled up
		wantV3     int
		wantQuit   bool
	}{
		{"tab moves the focus", false, "v3", tea.KeyMsg{Type: tea.KeyTab}, "raw", 0, 0, false},
		{"up outside the review does nothing", false, "v3", tea.KeyMsg{Type: tea.KeyUp}, "v3", 0, 0, false},
		{"up in the review scrolls a row", true, "raw", tea.KeyMsg{Type: tea.KeyUp}, "raw", 1, 0, false},
		{"the wheel on the left half scrolls the raw pane", false, "v3",
			tea.MouseMsg{X: 10, Y: 5, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp}, "raw", 3, 0, false},
		{"q quits", false, "v3", keyMsg("q"), "v3", 0, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m := &demoModel{width: 120, outputMode: c.outputMode, activePane: c.pane,
				rawTotal: 100, v3Total: 100, ctx: ctx, cancel: cancel}
			_, cmd := m.Update(c.msg)
			if m.activePane != c.wantPane {
				t.Errorf("focused pane = %q, want %q", m.activePane, c.wantPane)
			}
			if m.rawScroll != c.wantRaw {
				t.Errorf("raw pane scroll = %d, want %d", m.rawScroll, c.wantRaw)
			}
			if m.v3Scroll != c.wantV3 {
				t.Errorf("V3 pane scroll = %d, want %d", m.v3Scroll, c.wantV3)
			}
			if (cmd != nil) != c.wantQuit {
				t.Errorf("a command came back: %v, want it only for quit (%v)", cmd != nil, c.wantQuit)
			}
		})
	}
}

func TestDemoScrollActiveTargetsFocusedPane(t *testing.T) {
	m := &demoModel{activePane: "v3", rawTotal: 100, v3Total: 100}
	m.scrollActive(10)
	if m.v3Scroll != 10 || m.rawScroll != 0 {
		t.Fatalf("v3Scroll=%d rawScroll=%d, want 10/0", m.v3Scroll, m.rawScroll)
	}
	m.activePane = "raw"
	m.scrollActive(10)
	m.scrollActive(-3)
	if m.rawScroll != 7 {
		t.Fatalf("rawScroll=%d, want 7", m.rawScroll)
	}
	// Clamped to the last-rendered total on over-scroll, floor at zero.
	m.scrollActive(1 << 30)
	if m.rawScroll != 100 {
		t.Fatalf("rawScroll=%d, want clamp at total 100", m.rawScroll)
	}
	m.scrollActive(-1 << 30)
	if m.rawScroll != 0 {
		t.Fatalf("rawScroll=%d, want floor 0", m.rawScroll)
	}
}

func TestDemoScrollActiveFileBodyInOutputMode(t *testing.T) {
	m := &demoModel{activePane: "v3", outputMode: true, fileTotal: 50}
	// File body is top-anchored: pgup (positive delta) moves toward the
	// start, so from 0 it stays clamped at 0.
	m.scrollActive(10)
	if m.fileScroll != 0 {
		t.Fatalf("fileScroll=%d, want 0 after pgup at top", m.fileScroll)
	}
	m.scrollActive(-10)
	if m.fileScroll != 10 {
		t.Fatalf("fileScroll=%d, want 10 after pgdn", m.fileScroll)
	}
	m.scrollActiveToEnd()
	if m.fileScroll != 1<<30 {
		t.Fatalf("fileScroll=%d, want end sentinel", m.fileScroll)
	}
	// Chat panes still scroll when the raw side is focused.
	m.activePane = "raw"
	m.rawTotal = 40
	m.scrollActive(5)
	if m.rawScroll != 5 {
		t.Fatalf("rawScroll=%d, want 5", m.rawScroll)
	}
}

func TestReadFileForDisplayWindowsAndCounts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	var b strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&b, "line-%02d\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	// Window at the top: no above-note, below-note present.
	body, total := readFileForDisplay(path, 10, 80, 0)
	if total != 40 {
		t.Fatalf("total=%d, want 40", total)
	}
	if !strings.Contains(body, "line-01") || strings.Contains(body, "lines above") {
		t.Fatalf("top window wrong:\n%s", body)
	}
	if !strings.Contains(body, "lines below") {
		t.Fatalf("top window missing below-note:\n%s", body)
	}

	// Mid-file offset shows both notes and the offset line.
	body, _ = readFileForDisplay(path, 10, 80, 15)
	if !strings.Contains(body, "line-16") ||
		!strings.Contains(body, "lines above") ||
		!strings.Contains(body, "lines below") {
		t.Fatalf("mid window wrong:\n%s", body)
	}

	// Over-scroll clamps to the last window and reaches the final line.
	body, _ = readFileForDisplay(path, 10, 80, 9999)
	if !strings.Contains(body, "line-40") || strings.Contains(body, "lines below") {
		t.Fatalf("tail window wrong:\n%s", body)
	}
}

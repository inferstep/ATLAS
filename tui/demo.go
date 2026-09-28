// Demo mode — split-pane recording subprogram. Two concurrent requests
// against the same configured model: the left pane is one direct raw chat
// completion, while the right pane enters the ATLAS V3 agent.
//
// Implementation note: each pane uses a real `tuiModel` as its chat
// state holder. We forward every chatEvent into that model's
// appendChatEvent, then call renderChatPane to draw. That way the
// V3 pane formats EXACTLY like a normal atlas-tui session — every
// tool call, V3 stage, token stream, lens score row — with no
// reimplementation. The raw pane translates standard OpenAI SSE deltas into
// the same rendering events, without entering the agent or tool protocols.
//
// Reliability without scripting: prompt comes from a curated bank in
// docs/demo/demo_prompts.json — each entry is hand-validated to expose
// the V3 difference. Inference itself is real.

package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
)

//go:embed demo_prompts_fallback.json
var demoPromptsFallback embed.FS

// demoPrompt is one entry from the curated bank. The expected_* fields
// document what the baseline may miss and what V3 is expected to repair —
// they're not enforced at runtime, but they let
// future contributors know what each prompt is *for*. Difficulty is the
// recording-budget bucket: "short" = single-file, fast (good for the
// 30-60s cut); "medium" = 2 files, ~3-5 min; "long" = multi-file
// Flask/FastAPI, can take 30+ min. /demo <length> filters the bank
// before random pick so the cut doesn't blow its budget.
type demoPrompt struct {
	Prompt                  string `json:"prompt"`
	Difficulty              string `json:"difficulty,omitempty"` // "short" | "medium" | "long"
	ExpectedBaselineFailure string `json:"expected_baseline_failure"`
	ExpectedV3Repair        string `json:"expected_v3_repair"`
	Notes                   string `json:"notes,omitempty"`
}

// demoStreamMsg delivers a chatEvent from one of the two streaming
// goroutines into the Bubbletea loop. Side picks which child to mutate.
type demoStreamMsg struct {
	side string // "raw" | "v3"
	evt  chatEvent
}

// demoBatchMsg carries multiple events drained in one shot. Heavy token
// streams produce dozens of events/sec; without batching, the
// one-event-per-Update + two-pane glamour render becomes a backpressure
// bottleneck (~10 events/sec ceiling) and the TUI visibly stutters.
// Batching lets View() run once per drain instead of once per event.
type demoBatchMsg struct {
	stream []demoStreamMsg
	dones  []demoStreamDone
}

// demoStreamDone signals one of the two streams has finished (or failed).
type demoStreamDone struct {
	side string
	err  error
}

// demoTickMsg drives the prompt type-out animation and the post-finish
// hold timer.
type demoTickMsg time.Time

type demoModel struct {
	proxyURL   string
	workingDir string
	length     string // "short" (30s) | "medium" (60s) | "long" (3-5m)
	prompt     demoPrompt
	modelID    string // exact identifier returned by /v1/models
	modelLabel string // resolved from the proxy's OpenAI-compatible model list

	// The V3 side writes into an isolated sandbox under workingDir. The raw
	// side has no filesystem tools; its one model response is retained in the
	// left pane during output review.
	v3Sandbox string

	width, height int

	// Per-pane state. Each child tuiModel is constructed via newTUIModel
	// so its chatRenderer (glamour) and pipeline-state struct are wired,
	// but we never call its Init() — no goroutines, no /events SSE, no
	// keyboard input. We're just using it as a state container for chat
	// rendering. All event ingestion flows through child.appendChatEvent
	// and rendering through renderChatPane.
	rawChild *tuiModel
	v3Child  *tuiModel

	rawDone, v3Done bool
	rawErr, v3Err   error

	// Sticky "ever generated a token" flag per side. tuiModel's
	// streamingLLM/streamingV3 flags clear on llm_call_end (turn
	// boundary), so a multi-turn agent loop flips through
	// processing-prompt → streaming → processing-prompt-again. Tracking
	// it here at the demo level instead keeps the status header at
	// "streaming…" once the model starts producing tokens.
	rawEverStreamed, v3EverStreamed bool

	// Prompt type-out: chars revealed left of the user-input line at the
	// top of the screen. While typing, streams haven't fired yet — the
	// real raw-model and /v1/agent requests go out when typing completes, not
	// on Init, so the viewer sees prompt → reaction in the right order.
	promptShown  int
	streamsFired bool

	// Spinner frame index. Ticks on demoTickMsg; consumed in
	// streamStatus so each pane shows a moving glyph while in flight.
	// Without this the V3 side's long PlanSearch / repair windows look
	// dead to a viewer who can't see the proxy logs.
	spinnerFrame int

	// Output review mode: when both sides finish, the V3 pane switches
	// from streaming chat to a file-tree + selected-file-contents view
	// of its sandbox, while the raw pane keeps showing its one model
	// response. Tab/space cycles files within the V3 pane; 1-9 jumps.
	// outputMode is set once both rawDone and v3Done are true.
	outputMode    bool
	v3Files       []string // relative paths inside v3Sandbox, sorted
	v3SelectedIdx int
	activePane    string // "raw" | "v3" — which side keys apply to

	// Per-pane scrollback. Chat panes scroll in rows up from the bottom
	// (renderChatPane's convention; 0 = follow live output). The V3
	// output-review file body scrolls in rows down from the top. The
	// *Total fields capture line totals from the last View so key/wheel
	// input can clamp without re-rendering.
	rawScroll, v3Scroll int
	rawTotal, v3Total   int
	fileScroll          int
	fileTotal           int

	events chan demoEvent

	ctx    context.Context
	cancel context.CancelFunc

	startedAt  time.Time
	finishedAt time.Time
}

// demoEvent is the channel payload from goroutines to the Bubbletea
// loop. Exactly one of {stream, done} is meaningful.
type demoEvent struct {
	stream demoStreamMsg
	done   *demoStreamDone
}

// pickPrompt loads docs/demo/demo_prompts.json (looked up under the
// session's working dir, then the process cwd, then the embedded
// fallback) and returns
// a random prompt whose difficulty fits the requested length. Bucket
// rule: `short` picks only difficulty=short; `medium` picks short or
// medium (so the bank stays useful for 60s cuts even with no `medium`
// entries); `long` picks anything. If no prompt matches, falls back to
// the full bank so /demo always returns SOMETHING rather than
// erroring on a misconfigured length.
func pickPrompt(workingDir, length string) (demoPrompt, error) {
	candidates := []string{
		filepath.Join(workingDir, "docs", "demo", "demo_prompts.json"),
		"docs/demo/demo_prompts.json",
	}
	var raw []byte
	for _, p := range candidates {
		if b, err := os.ReadFile(p); err == nil {
			raw = b
			break
		}
	}
	if raw == nil {
		b, err := demoPromptsFallback.ReadFile("demo_prompts_fallback.json")
		if err != nil {
			return demoPrompt{}, fmt.Errorf("demo prompts not found: %w", err)
		}
		raw = b
	}
	var bank []demoPrompt
	if err := json.Unmarshal(raw, &bank); err != nil {
		return demoPrompt{}, fmt.Errorf("parse demo prompts: %w", err)
	}
	if len(bank) == 0 {
		return demoPrompt{}, fmt.Errorf("demo prompt bank empty")
	}
	pool := filterByDifficulty(bank, length)
	if len(pool) == 0 {
		pool = bank
	}
	return pool[rand.Intn(len(pool))], nil
}

// filterByDifficulty returns the subset of prompts that fit the
// requested length. Untagged prompts (Difficulty == "") are treated as
// "medium" so old banks keep working without back-fill.
func filterByDifficulty(bank []demoPrompt, length string) []demoPrompt {
	var out []demoPrompt
	for _, p := range bank {
		d := p.Difficulty
		if d == "" {
			d = "medium"
		}
		switch length {
		case "short":
			if d == "short" {
				out = append(out, p)
			}
		case "medium":
			if d == "short" || d == "medium" {
				out = append(out, p)
			}
		default: // "long" and anything unknown
			out = append(out, p)
		}
	}
	return out
}

func newDemoModel(proxyURL, workingDir, length string) (*demoModel, error) {
	p, err := pickPrompt(workingDir, length)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())

	rawChild := newTUIModel(proxyURL)
	v3Child := newTUIModel(proxyURL)

	// Per-run V3 sandbox subdir. The timestamp makes recordings
	// reproducible (you know which dir came from which take) and stops
	// collisions between rapid re-runs. Created here so the demo can
	// show the "files written" view after both sides finish. The raw
	// side is a direct completion with no filesystem tools, so it gets
	// no sandbox.
	v3Sandbox := fmt.Sprintf(".demo-v3-%d", time.Now().Unix())
	if err := os.MkdirAll(filepath.Join(workingDir, v3Sandbox), 0o755); err != nil {
		cancel()
		return nil, fmt.Errorf("create sandbox %s: %w", v3Sandbox, err)
	}

	modelID, modelLabel := fetchDemoModelIdentity(proxyURL)
	return &demoModel{
		proxyURL:   proxyURL,
		workingDir: workingDir,
		length:     length,
		prompt:     p,
		modelID:    modelID,
		modelLabel: modelLabel,
		events:     make(chan demoEvent, 1024),
		ctx:        ctx,
		cancel:     cancel,
		rawChild:   &rawChild,
		v3Child:    &v3Child,
		v3Sandbox:  v3Sandbox,
		activePane: "v3",
	}, nil
}

const demoModelFallback = "MODEL"
const demoRawCapability = "demo_raw_completion_v1"

// fetchDemoModelIdentity resolves the model actually configured in the proxy.
// Metadata is presentation-only: if the endpoint is unavailable or malformed,
// the demo still launches with a neutral label instead of guessing a family or
// parameter count.
func fetchDemoModelIdentity(proxyURL string) (string, string) {
	req, err := http.NewRequest(http.MethodGet,
		strings.TrimRight(proxyURL, "/")+"/v1/models", nil)
	if err != nil {
		return "", demoModelFallback
	}
	setProxyAuth(req)
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return "", demoModelFallback
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", demoModelFallback
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil || len(payload.Data) == 0 {
		return "", demoModelFallback
	}
	id := strings.TrimSpace(payload.Data[0].ID)
	return id, formatDemoModelLabel(id)
}

// proxySupportsRawDemo prevents a new TUI from silently talking to a proxy
// whose demo contract predates the direct raw-completion comparison.
func proxySupportsRawDemo(proxyURL string) bool {
	req, err := http.NewRequest(http.MethodGet,
		strings.TrimRight(proxyURL, "/")+"/health", nil)
	if err != nil {
		return false
	}
	setProxyAuth(req)
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var payload struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return false
	}
	for _, capability := range payload.Capabilities {
		if capability == demoRawCapability {
			return true
		}
	}
	return false
}

// formatDemoModelLabel turns a registry identifier such as
// orion-code-10b-it-Q4_K_M into a compact title (Orion code 10B). The exact ID
// remains available from /v1/models; the demo title favors readability.
func formatDemoModelLabel(id string) string {
	id = strings.TrimSpace(filepath.Base(id))
	if strings.HasSuffix(strings.ToLower(id), ".gguf") {
		id = id[:len(id)-len(".gguf")]
	}
	if id == "" || id == "." {
		return demoModelFallback
	}
	parts := strings.FieldsFunc(id, func(r rune) bool { return r == '-' || r == '_' })
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		lower := strings.ToLower(part)
		if len(lower) > 1 && lower[0] == 'q' && lower[1] >= '0' && lower[1] <= '9' {
			break // quantization suffix (Q4_K_M, Q6_K, ...)
		}
		if lower == "it" || lower == "instruct" || lower == "chat" {
			continue
		}
		if len(lower) > 1 && lower[len(lower)-1] == 'b' {
			allDigits := true
			for _, r := range lower[:len(lower)-1] {
				if r < '0' || r > '9' {
					allDigits = false
					break
				}
			}
			if allDigits {
				part = strings.ToUpper(part)
			}
		}
		kept = append(kept, part)
	}
	if len(kept) == 0 {
		return demoModelFallback
	}
	if first := kept[0]; first != "" {
		kept[0] = strings.ToUpper(first[:1]) + first[1:]
	}
	return strings.Join(kept, " ")
}

func (m *demoModel) displayModelLabel() string {
	if strings.TrimSpace(m.modelLabel) == "" {
		return demoModelFallback
	}
	return m.modelLabel
}

func (m *demoModel) rawTitle() string {
	return m.displayModelLabel() + "  ·  RAW MODEL  ·  NO ORCHESTRATION"
}

func (m *demoModel) atlasTitle() string {
	return m.displayModelLabel() + "  ·  ATLAS V3"
}

func (m *demoModel) Init() tea.Cmd {
	m.startedAt = time.Now()
	// Streams are NOT fired here — they fire from the tick handler the
	// instant the prompt animation completes. This keeps the visual
	// timeline honest: viewer sees the prompt being typed, *then* both
	// sides react. Without this, V3's "candidate 1/3" stage label
	// renders before the prompt is fully visible.
	return tea.Batch(
		m.drainEvents(),
		demoTick(80*time.Millisecond),
	)
}

// startStreams fires a direct raw model request and an ATLAS agent request in
// parallel. They share the same proxy and configured model, but only the V3
// side enters /v1/agent.
// disable_fresh_slot=true on the V3 request so PC-045 doesn't wipe its prefix
// cache. The raw request bypasses the agent slot-reset path entirely.
func (m *demoModel) startStreams() tea.Cmd {
	return func() tea.Msg {
		go m.runStream("raw")
		go m.runStream("v3")
		return nil
	}
}

func (m *demoModel) runStream(side string) {
	out := make(chan chatEvent, 128)
	errCh := make(chan error, 1)
	go func() {
		var err error
		if side == "raw" {
			err = sendRawChat(m.ctx, m.proxyURL, m.modelID, m.prompt.Prompt, out)
		} else {
			sid := fmt.Sprintf("demo-%s-%d", side, time.Now().UnixNano())
			err = sendChatOpts(m.ctx, m.proxyURL, m.prompt.Prompt, m.workingDir,
				"yolo", sid, nil, demoOpts{
					disableFreshSlot: true,
					sandboxSubdir:    m.v3Sandbox,
				}, out)
		}
		errCh <- err
		close(out)
	}()
	// Forward every stream event, then the done marker — ranging over
	// the closed channel drains the buffer first, so done can never
	// overtake events still queued on `out`.
	for evt := range out {
		m.events <- demoEvent{stream: demoStreamMsg{side: side, evt: evt}}
	}
	m.events <- demoEvent{done: &demoStreamDone{side: side, err: <-errCh}}
}

// drainEvents pulls events from the shared channel and batches every
// ready event into a single demoBatchMsg. Blocks on the first event so
// idle ticks don't spin, then non-blockingly drains up to maxBatch more
// before returning. View() runs once per batch instead of once per
// event — the key win during heavy token streams.
const maxDemoBatch = 128

func (m *demoModel) drainEvents() tea.Cmd {
	return func() tea.Msg {
		var batch demoBatchMsg
		// First event: block until something arrives or context dies.
		select {
		case ev, ok := <-m.events:
			if !ok {
				return nil
			}
			absorb(&batch, ev)
		case <-m.ctx.Done():
			return nil
		}
		// Drain the rest of what's already queued, non-blocking.
		for i := 1; i < maxDemoBatch; i++ {
			select {
			case ev, ok := <-m.events:
				if !ok {
					return batch
				}
				absorb(&batch, ev)
			default:
				return batch
			}
		}
		return batch
	}
}

func absorb(b *demoBatchMsg, ev demoEvent) {
	if ev.done != nil {
		b.dones = append(b.dones, *ev.done)
		return
	}
	b.stream = append(b.stream, ev.stream)
}

func demoTick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return demoTickMsg(t) })
}

func (m *demoModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		widthChanged := msg.Width != m.width
		m.width = msg.Width
		m.height = msg.Height
		// Forward a half-width sizing to each child so glamour wraps to
		// the per-pane column. The chat-pane wrapper takes ~4 columns
		// for borders + padding.
		colW := (msg.Width - 4) / 2
		m.rawChild.width = colW
		m.rawChild.height = msg.Height - 4
		m.v3Child.width = colW
		m.v3Child.height = msg.Height - 4
		if widthChanged {
			// Rebuild each child's glamour renderer at the pane's inner
			// content width (renderChatPane is called with colW-4) so
			// markdown wraps to the actual column, not the default.
			style := os.Getenv("GLAMOUR_STYLE")
			if style == "" {
				style = "dark"
			}
			wrap := colW - 4
			if wrap < 20 {
				wrap = 20
			}
			for _, child := range []*tuiModel{m.rawChild, m.v3Child} {
				if r, err := glamour.NewTermRenderer(
					glamour.WithStandardStyle(style),
					glamour.WithWordWrap(wrap),
				); err == nil {
					child.chatRenderer = r
				}
			}
		}
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.cancel()
			return m, tea.Quit
		case "tab", "shift+tab":
			// Which pane scroll keys target; in output review this is
			// also the side file-cycling keys apply to.
			if m.activePane == "raw" {
				m.activePane = "v3"
			} else {
				m.activePane = "raw"
			}
			return m, nil
		case "pgup":
			m.scrollActive(10)
			return m, nil
		case "pgdown":
			m.scrollActive(-10)
			return m, nil
		case "ctrl+home":
			m.scrollActive(1 << 30)
			return m, nil
		case "ctrl+end":
			m.scrollActiveToEnd()
			return m, nil
		case "up", "down":
			// Line-step scrolling only in output review — during
			// streaming the panes follow the live output.
			if m.outputMode {
				if msg.String() == "up" {
					m.scrollActive(1)
				} else {
					m.scrollActive(-1)
				}
				return m, nil
			}
		}
		if m.outputMode {
			m.handleOutputKey(msg.String())
			return m, nil
		}

	case tea.MouseMsg:
		// Wheel scrolls the pane under the cursor; focus follows so
		// subsequent keys target the same side.
		if msg.Action == tea.MouseActionPress {
			var delta int
			switch msg.Button {
			case tea.MouseButtonWheelUp:
				delta = 3
			case tea.MouseButtonWheelDown:
				delta = -3
			default:
				return m, nil
			}
			if msg.X < m.width/2 {
				m.activePane = "raw"
			} else {
				m.activePane = "v3"
			}
			m.scrollActive(delta)
		}
		return m, nil

	case demoBatchMsg:
		// Apply every event in this batch before returning so View only
		// renders once. Stream events first, then done flags.
		for _, s := range msg.stream {
			// Sticky generation flag: any token-bearing event flips
			// this side's "ever streamed" bit so the header doesn't
			// flop back to "processing prompt" on subsequent turns.
			switch s.evt.Type {
			case "llm_token", "reasoning_token", "v3_token", "v3_reasoning_token":
				if s.side == "raw" {
					m.rawEverStreamed = true
				} else {
					m.v3EverStreamed = true
				}
			}
			if s.side == "raw" {
				if s.evt.Type == "text" {
					var payload struct {
						Content string `json:"content"`
					}
					if json.Unmarshal(s.evt.Data, &payload) == nil && payload.Content != "" {
						m.rawChild.chat = append(m.rawChild.chat, chatMessage{
							Role: roleAssistant, Meta: "raw model", Body: payload.Content,
						})
					}
				} else {
					m.rawChild.appendChatEvent(s.evt)
				}
			} else {
				m.v3Child.appendChatEvent(s.evt)
			}
		}
		for _, d := range msg.dones {
			switch d.side {
			case "raw":
				m.rawDone = true
				m.rawErr = d.err
			case "v3":
				m.v3Done = true
				m.v3Err = d.err
			}
		}
		if m.rawDone && m.v3Done && m.finishedAt.IsZero() {
			m.finishedAt = time.Now()
			m.enterOutputMode()
		}
		return m, m.drainEvents()

	case demoTickMsg:
		m.spinnerFrame++
		// promptShown counts runes, not bytes, so a multi-byte character
		// (em dash, box drawing) is revealed whole instead of split
		// mid-sequence.
		promptLen := len([]rune(m.prompt.Prompt))
		if !m.outputMode && m.promptShown < promptLen {
			m.promptShown++
		}
		// Fire the streams exactly once, the tick after the prompt
		// animation finishes. The trailing tick on promptLen == promptShown
		// gives one frame of "complete prompt with no caret" before the
		// status flips to processing-prompt — small thing but reads
		// noticeably cleaner on camera.
		if !m.streamsFired && !m.outputMode && m.promptShown >= promptLen {
			m.streamsFired = true
			return m, tea.Batch(
				m.startStreams(),
				demoTick(80*time.Millisecond),
			)
		}
		// In output mode the user explores at their own pace — no auto-quit.
		// Pre-output mode the tick is just driving the prompt-typing animation.
		return m, demoTick(80 * time.Millisecond)
	}

	return m, nil
}

var (
	demoRawTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Padding(0, 1).
				Background(lipgloss.Color("88")). // muted red
				Foreground(lipgloss.Color("231"))

	demoV3TitleStyle = lipgloss.NewStyle().
				Bold(true).
				Padding(0, 1).
				Background(lipgloss.Color("28")). // muted green
				Foreground(lipgloss.Color("231"))

	demoStatusStyle = lipgloss.NewStyle().
			Faint(true).
			Padding(0, 1)

	demoPaneStyle = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)

	demoPromptStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("11"))

	demoSelectedFileStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("11"))
)

func (m *demoModel) View() string {
	if m.width == 0 || m.height == 0 {
		return "loading demo…"
	}

	// Top row: typed prompt animation. Caret blinks via promptShown
	// (a rune count — slicing by rune keeps multi-byte characters
	// intact). In output mode the prompt header becomes a compact
	// reminder of what was asked so the file diff has context.
	prompt := m.prompt.Prompt
	if promptRunes := []rune(prompt); !m.outputMode && m.promptShown < len(promptRunes) {
		shown := m.promptShown
		if shown < 0 {
			shown = 0
		}
		prompt = string(promptRunes[:shown]) + "▌"
	}
	// Wrap to terminal width — the real-world prompts in the bank are
	// 200+ chars and would otherwise overflow off the right edge.
	// Subtract 2 for the "> " prefix on the first visible line.
	headerLines := wrapPlain(prompt, m.width-2)
	headerStyled := make([]string, len(headerLines))
	for i, line := range headerLines {
		if i == 0 {
			headerStyled[i] = demoPromptStyle.Render("> " + line)
		} else {
			// Continuation lines get a 2-space indent so the eye tracks
			// them as a continuation of the prompt rather than separate text.
			headerStyled[i] = demoPromptStyle.Render("  " + line)
		}
	}
	header := strings.Join(headerStyled, "\n")

	// Column width accounting:
	//   m.width = total terminal width
	//   subtract 4 for the two outer borders + padding columns
	//   divide by 2 for two equal panes
	//   per-pane inner content width = colW - 4 (border + padding inside pane)
	colW := (m.width - 4) / 2
	// Body height shrinks by the wrapped header's line count. Without
	// this, a long prompt steals rows from the chat panes and chat
	// content gets clipped at the bottom.
	bodyH := m.height - len(headerLines) - 3 // header + footer + breathing

	var row, footer string
	if m.outputMode {
		rawPane := m.renderOutputPane("raw", colW, bodyH)
		v3Pane := m.renderOutputPane("v3", colW, bodyH)
		row = lipgloss.JoinHorizontal(lipgloss.Top, rawPane, v3Pane)
		footer = demoStatusStyle.Render(
			"output review  ·  tab: switch side  ·  n/p (or ←/→): cycle file  ·  1-9: jump  ·  ↑/↓ pgup/pgdn/wheel: scroll  ·  q: quit  ·  active: " + m.activePane)
	} else {
		rawTitle := demoRawTitleStyle.Render(m.rawTitle()) + "  " +
			demoStatusStyle.Render(streamStatus(m.rawChild, m.rawDone, m.rawEverStreamed, m.rawErr))
		v3Title := demoV3TitleStyle.Render(m.atlasTitle()) + "  " +
			demoStatusStyle.Render(streamStatus(m.v3Child, m.v3Done, m.v3EverStreamed, m.v3Err))

		// Reserve one row at the bottom of each pane for the thinking
		// spinner (matches the main ATLAS TUI's pattern). chat content
		// loses one row in flight; final frame on done has no spinner
		// so the chat reclaims that row.
		rawThink := thinkingRow(m.rawDone, m.spinnerFrame)
		v3Think := thinkingRow(m.v3Done, m.spinnerFrame+5) // phase offset

		rawChatH := bodyH - 2
		v3ChatH := bodyH - 2
		if rawThink != "" {
			rawChatH--
		}
		if v3Think != "" {
			v3ChatH--
		}

		rawChat, _, _, rawTotal, _, _ := renderChatPane(m.rawChild.chat, m.rawChild.chatRenderer,
			rawChatH, colW-4, m.rawScroll)
		v3Chat, _, _, v3Total, _, _ := renderChatPane(m.v3Child.chat, m.v3Child.chatRenderer,
			v3ChatH, colW-4, m.v3Scroll)
		m.rawTotal, m.v3Total = rawTotal, v3Total

		rawBody := rawTitle + "\n\n" + rawChat
		if rawThink != "" {
			rawBody += "\n" + rawThink
		}
		v3Body := v3Title + "\n\n" + v3Chat
		if v3Think != "" {
			v3Body += "\n" + v3Think
		}
		rawPane := demoPaneStyle.Width(colW).Height(bodyH).Render(rawBody)
		v3Pane := demoPaneStyle.Width(colW).Height(bodyH).Render(v3Body)

		row = lipgloss.JoinHorizontal(lipgloss.Top, rawPane, v3Pane)
		footer = demoStatusStyle.Render(
			"recording demo  ·  pgup/pgdn/wheel: scroll (tab switches side)  ·  ctrl+c to abort")
	}

	return header + "\n" + row + "\n" + footer
}

// streamStatus produces the title-bar status text WITHOUT a spinner —
// the moving spinner lives at the bottom of each chat pane now (see
// thinkingRow), mirroring the main TUI's pattern. The title bar just
// states "processing prompt N%" or "streaming…" or "✓ done".
//
// `everStreamed` is the demo's sticky bit — once this side has produced
// any generation token, the header stays at "streaming…" until done.
// Without it, multi-turn agent loops flip back to "processing prompt"
// on every fresh LLM call, which reads as a glitch on camera.
func streamStatus(child *tuiModel, done, everStreamed bool, err error) string {
	if done {
		if err != nil {
			return "✗ error"
		}
		return "✓ done"
	}
	if everStreamed {
		return "streaming…"
	}
	// total is always pre-filled from the chars/4 estimate. Only show a
	// percentage when llama.cpp also exposes a non-zero processed count;
	// current builds omit those /slots counters, and "0%" for an entire
	// Metal prompt encode is confidently wrong rather than useful progress.
	if child.promptProcessed > 0 && child.promptTotal > 0 &&
		child.promptPct > 0 && child.promptPct < 1 {
		return fmt.Sprintf("processing prompt %.0f%%", child.promptPct*100)
	}
	if !child.promptEvalStart.IsZero() {
		return "processing prompt…"
	}
	return "waiting…"
}

// thinkingRow is the bottom-of-pane orange spinner row, matching the
// main TUI's pattern (panes.go:752-759). Returns empty string when the
// side is done — last frame freezes on the chat content rather than a
// dangling spinner.
func thinkingRow(done bool, spinnerFrame int) string {
	if done {
		return ""
	}
	mark := spinnerFrames[spinnerFrame%len(spinnerFrames)]
	verb := thinkingVerbs[(spinnerFrame/20)%len(thinkingVerbs)]
	return runStyle.Render(fmt.Sprintf("  %s %s…", mark, verb))
}

// enterOutputMode scans the V3 sandbox for files and flips the demo
// into review mode so the recorder can pan across the two outputs
// after generation finishes. We walk the tree once (caching the list)
// and read file contents lazily on selection so a large sandbox
// doesn't stall the transition. The raw side never writes files —
// its pane keeps showing the model response.
func (m *demoModel) enterOutputMode() {
	m.outputMode = true
	m.v3Files = scanSandbox(filepath.Join(m.workingDir, m.v3Sandbox))
}

// scanSandbox walks a sandbox dir and returns relative paths to every
// regular file, sorted. Empty or unreadable trees return nil — the
// caller renders a "(no files written)" placeholder.
func scanSandbox(root string) []string {
	var files []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	sort.Strings(files)
	return files
}

// handleOutputKey routes navigation in output-review mode. Tab cycles
// the active pane between sides; n/p (or arrow keys) cycles files
// within the active pane; 1-9 jumps.
// scrollActive adjusts the focused pane's scroll position by delta rows
// (positive = toward older/earlier content). Chat panes are
// bottom-anchored (rows up from the live tail); the output-review file
// body is top-anchored, so the sign flips there.
func (m *demoModel) scrollActive(delta int) {
	clamp := func(v, max int) int {
		if max < 0 {
			max = 0
		}
		if v > max {
			v = max
		}
		if v < 0 {
			v = 0
		}
		return v
	}
	if m.outputMode && m.activePane == "v3" {
		m.fileScroll = clamp(m.fileScroll-delta, m.fileTotal)
		return
	}
	if m.activePane == "raw" {
		m.rawScroll = clamp(m.rawScroll+delta, m.rawTotal)
	} else {
		m.v3Scroll = clamp(m.v3Scroll+delta, m.v3Total)
	}
}

// scrollActiveToEnd jumps the focused pane to its natural "end": the
// live tail for chat panes, the last window for the file body.
func (m *demoModel) scrollActiveToEnd() {
	if m.outputMode && m.activePane == "v3" {
		m.fileScroll = 1 << 30 // clamped to the last window at render
		return
	}
	if m.activePane == "raw" {
		m.rawScroll = 0
	} else {
		m.v3Scroll = 0
	}
}

func (m *demoModel) handleOutputKey(key string) {
	switch key {
	case "right", "l", "n", " ":
		m.cycleActiveFile(+1)
	case "left", "h", "p":
		m.cycleActiveFile(-1)
	default:
		if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
			idx := int(key[0] - '1')
			files := m.activeFiles()
			if idx < len(files) {
				m.setActiveIdx(idx)
			}
		}
	}
}

func (m *demoModel) activeFiles() []string {
	if m.activePane == "raw" {
		return nil // raw lane is a direct completion — no files written
	}
	return m.v3Files
}

func (m *demoModel) cycleActiveFile(delta int) {
	files := m.activeFiles()
	if len(files) == 0 {
		return
	}
	m.v3SelectedIdx = (m.v3SelectedIdx + delta + len(files)) % len(files)
	m.fileScroll = 0
}

func (m *demoModel) setActiveIdx(i int) {
	if m.activePane == "v3" {
		m.v3SelectedIdx = i
		m.fileScroll = 0
	}
}

// renderOutputPane builds the post-generation review view for one side.
// The raw lane has no filesystem tools, so its pane shows the model
// response; the V3 pane shows the sandbox file tree + selected-file
// contents. The active pane gets a brighter border so the viewer knows
// which side keys apply to.
func (m *demoModel) renderOutputPane(side string, w, h int) string {
	if side == "raw" {
		chat, _, _, total, _, _ := renderChatPane(
			m.rawChild.chat, m.rawChild.chatRenderer, h-3, w-4, m.rawScroll,
		)
		m.rawTotal = total
		body := demoRawTitleStyle.Render(m.rawTitle()+"  ·  RESPONSE") + "\n\n" + chat
		border := lipgloss.Color("240")
		if m.activePane == side {
			border = lipgloss.Color("11")
		}
		return demoPaneStyle.BorderForeground(border).Width(w).Height(h).Render(body)
	}
	sandbox := m.v3Sandbox
	files := m.v3Files
	selected := m.v3SelectedIdx
	title := m.atlasTitle() + "  ·  " + sandbox
	titleStyle := demoV3TitleStyle

	// File list. Trim if too tall — the body needs space too.
	treeHeight := h / 3
	if treeHeight < 3 {
		treeHeight = 3
	}
	tree := []string{titleStyle.Render(title), ""}
	if len(files) == 0 {
		tree = append(tree, demoStatusStyle.Render("(no files written)"))
	} else {
		for i, f := range files {
			marker := "  "
			if i == selected {
				marker = "▸ "
			}
			line := marker + f
			if i == selected {
				line = demoSelectedFileStyle.Render(line)
			}
			tree = append(tree, line)
			if len(tree) >= treeHeight {
				tree = append(tree, demoStatusStyle.Render(fmt.Sprintf("  … +%d more", len(files)-i-1)))
				break
			}
		}
	}

	// File contents. Read on demand to keep the transition cheap.
	bodyHeight := h - len(tree) - 2
	if bodyHeight < 1 {
		bodyHeight = 1
	}
	body := ""
	if len(files) > 0 && selected < len(files) {
		fpath := filepath.Join(m.workingDir, sandbox, files[selected])
		body, m.fileTotal = readFileForDisplay(fpath, bodyHeight, w-4, m.fileScroll)
	}

	border := lipgloss.Color("240")
	if m.activePane == side {
		border = lipgloss.Color("11") // bright yellow on the focused side
	}
	pane := demoPaneStyle.
		BorderForeground(border).
		Width(w).
		Height(h).
		Render(strings.Join(tree, "\n") + "\n\n" + body)
	return pane
}

// readFileForDisplay returns a maxLines window of the file starting
// `offset` lines from the top (clamped to the file), lines trimmed to
// maxCols, plus the file's total line count for scroll clamping. Rows
// above/below the window are noted so it's obvious there's more to
// scroll. Binary files are flagged; reading caps at maxScanLines so a
// generated monster file can't stall the render loop.
func readFileForDisplay(path string, maxLines, maxCols, offset int) (string, int) {
	const sniffBytes = 512
	const maxScanLines = 5000
	f, err := os.Open(path)
	if err != nil {
		return demoStatusStyle.Render(fmt.Sprintf("(cannot read: %v)", err)), 0
	}
	defer f.Close()
	sniff := make([]byte, sniffBytes)
	n, _ := f.Read(sniff)
	for _, b := range sniff[:n] {
		if b == 0 {
			return demoStatusStyle.Render("(binary file)"), 0
		}
	}
	_, _ = f.Seek(0, 0)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var lines []string
	for scanner.Scan() && len(lines) < maxScanLines {
		line := scanner.Text()
		if maxCols > 0 && len(line) > maxCols {
			line = line[:maxCols-1] + "…"
		}
		lines = append(lines, line)
	}
	total := len(lines)

	if maxLines < 1 {
		maxLines = 1
	}
	// Largest start whose window still reaches EOF: +1 reserves the
	// above-note row any non-zero start displays.
	maxStart := total - maxLines + 1
	if maxStart < 0 || total <= maxLines {
		maxStart = 0
	}
	if offset > maxStart {
		offset = maxStart
	}
	if offset < 0 {
		offset = 0
	}
	// The above/below notes each take a display row from the window.
	avail := maxLines
	if offset > 0 && avail > 1 {
		avail--
	}
	end := offset + avail
	if end > total {
		end = total
	}
	if end < total && avail > 1 {
		avail--
		end = offset + avail
	}
	var parts []string
	if offset > 0 {
		parts = append(parts, demoStatusStyle.Render(
			fmt.Sprintf("… +%d lines above (pgup)", offset)))
	}
	parts = append(parts, lines[offset:end]...)
	if end < total {
		parts = append(parts, demoStatusStyle.Render(
			fmt.Sprintf("… +%d lines below (pgdn)", total-end)))
	}
	return strings.Join(parts, "\n"), total
}

// runDemo launches the demo subprogram in the same terminal session.
// Called from main.go after the primary TUI exits with launchDemoMode
// set, or from a `--demo` flag on cold start.
func runDemo(proxyURL, workingDir, length string) error {
	if !proxySupportsRawDemo(proxyURL) {
		return fmt.Errorf(
			"active atlas-proxy is too old for an honest split demo; " +
				"restart ATLAS from the current checkout so the proxy is rebuilt",
		)
	}
	model, err := newDemoModel(proxyURL, workingDir, length)
	if err != nil {
		return err
	}
	// Mouse gating mirrors the primary TUI (main.go): wheel-scroll of
	// the demo panes needs cell-motion reporting, opt out with
	// ATLAS_TUI_MOUSE=off to keep the terminal's native selection.
	opts := []tea.ProgramOption{tea.WithAltScreen()}
	if strings.ToLower(envOr("ATLAS_TUI_MOUSE", "on")) != "off" {
		opts = append(opts, tea.WithMouseCellMotion())
	}
	prog := tea.NewProgram(model, opts...)
	_, err = prog.Run()
	return err
}

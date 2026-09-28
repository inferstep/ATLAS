// PC-062: pane renderers — pure functions from state → string.
//
// Panes:
//   pipelinePane — stage table with status icons + durations
//   eventsPane   — scrolling event log (raw envelope stream)
//   chatPane     — chat history (user + assistant + tool calls)
//   statsPane    — one-line counter strip
//   inputPane    — textarea (rendered by Bubbles, this file just frames it)

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// thinkingVerbs cycles through quirky phrases while the model is
// generating, so a long turn doesn't look stuck on a single word.
// Refreshed every ~3s based on spinnerFrame (which ticks at 150ms).
var thinkingVerbs = []string{
	"Thinking", "Pondering", "Cogitating", "Brewing", "Concocting",
	"Conjuring", "Synthesizing", "Mulling", "Reasoning", "Plotting",
	"Distilling", "Crafting", "Weaving", "Marinating", "Percolating",
	"Crystallizing", "Untangling", "Hypothesizing", "Sleuthing",
	"Decoding", "Composing", "Sculpting", "Architecting", "Brainstorming",
	"Wrangling", "Spelunking", "Unraveling", "Deliberating",
}

var (
	bordStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240"))

	bordStyleFocused = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("117"))

	// Bash mode: leading "!" routes input to a shell command. Red
	// border signals "this WILL execute on your machine".
	bordStyleBash = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("196"))

	// Slash mode: leading "/" is a TUI command (no agent / shell).
	// Purple border + completion hint above the box.
	bordStyleSlash = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("141"))

	titleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("117")).
			Bold(true)

	dimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))

	okStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	failStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	runStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))

	chatUserStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("117")).
			Bold(true)

	chatAssistantStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("231"))

	chatToolStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("214"))

	chatSystemStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("245")).
			Italic(true)

	chatTurnStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("141")).
			Bold(true)

	// chatLLMStyle is intentionally dim — these rows are "machine
	// internals" (encoding/decoding/streaming JSON) and should sit
	// behind the brighter assistant/tool output rows.
	chatLLMStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("242")).
			Italic(true)

	chatV3Style = lipgloss.NewStyle().
			Foreground(lipgloss.Color("99"))
)

// renderPipelinePane returns the pipeline pane content (no border).
// Caller wraps in a bordered box at the right size.
//
// `height` is the usable inside height. `scroll` is rows from the
// bottom (0 = follow latest stage). Returns (rendered, allLines,
// viewStart) so the caller can snapshot the full content for the
// highlight-to-copy / mouse-wheel routing.
func renderPipelinePane(p *pipelineState, height, width, scroll int) (string, []string, int) {
	stages := p.stages()
	if len(stages) == 0 {
		empty := dimStyle.Render("waiting for events…")
		return empty, []string{empty}, 0
	}
	all := make([]string, 0, len(stages))
	for _, s := range stages {
		all = append(all, renderPipelineRow(s, width))
	}
	return windowLines(all, height, scroll)
}

func renderPipelineRow(s *stageStatus, width int) string {
	icon, style := stageIcon(s)
	name := lipgloss.NewStyle().Width(16).Render(truncate(s.Name, 16))
	status := style.Width(8).Render(stageStatusLabel(s))
	// Truncate to the cell width — content wider than the Width(8) cell
	// would soft-wrap and push the whole row onto a second line.
	dur := dimStyle.Width(8).Render(truncate(formatDuration(s.Duration()), 8))
	detail := dimStyle.Render(truncate(s.Detail, max(0, width-40)))
	return fmt.Sprintf("%s  %s %s %s %s", icon, name, status, dur, detail)
}

func stageIcon(s *stageStatus) (string, lipgloss.Style) {
	if s.Running() {
		return runStyle.Render("⚙"), runStyle
	}
	if s.Success {
		return okStyle.Render("✓"), okStyle
	}
	return failStyle.Render("✗"), failStyle
}

func stageStatusLabel(s *stageStatus) string {
	if s.Running() {
		return "RUN"
	}
	if s.Success {
		return "OK"
	}
	return "FAIL"
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// renderEventsPane returns the event log pane content. `height` is the
// usable inside height (caller has already accounted for the border).
// `scroll` is rows from the bottom (0 = follow). Returns (rendered,
// allLines, viewStart).
func renderEventsPane(events []Envelope, height, width, scroll int) (string, []string, int) {
	if height <= 0 {
		return "", nil, 0
	}
	all := make([]string, 0, len(events))
	for _, ev := range events {
		all = append(all, formatEventLine(ev, width))
	}
	return windowLines(all, height, scroll)
}

// selectionStyle is the inverse-video style applied to lines under
// an in-flight drag-highlight selection. Solid block so the user can
// see exactly which rows will be copied on release.
var selectionStyle = lipgloss.NewStyle().Reverse(true)

// applySelectionOverlay re-styles the rows of `rendered` (one pane's
// already-windowed content, "\n"-joined) that fall within the user's
// drag selection. paneTopY is the screen Y of the FIRST visible row;
// paneRows is how many lines `rendered` contains. Returns the rendered
// string with selected rows wrapped in inverse-video styling.
//
// Called by layoutFullScreen after each pane's renderXxxPane, when
// the active sel.pane matches the pane being processed. Stripping
// ANSI from the targeted lines before re-styling is intentional —
// preserving the original styling alongside reverse video produces
// inconsistent results across terminals (some terminals XOR the
// styles, others let the foreground bleed through).
func applySelectionOverlay(rendered, paneName string, sel selectionState,
	paneTopY, paneRows int) string {
	if sel.pane == "" || sel.pane != paneName || paneRows <= 0 {
		return rendered
	}
	lo, hi := sel.startY, sel.endY
	if lo > hi {
		lo, hi = hi, lo
	}
	paneBottomY := paneTopY + paneRows - 1
	if hi < paneTopY || lo > paneBottomY {
		return rendered
	}
	if lo < paneTopY {
		lo = paneTopY
	}
	if hi > paneBottomY {
		hi = paneBottomY
	}
	startRow := lo - paneTopY
	endRow := hi - paneTopY
	lines := strings.Split(rendered, "\n")
	for i := startRow; i <= endRow && i < len(lines); i++ {
		lines[i] = selectionStyle.Render(stripPaneANSI(lines[i]))
	}
	return strings.Join(lines, "\n")
}

// stripPaneANSI is the panes-level shim around model.go's stripANSI.
// Defined here so the overlay code can stay in panes.go without
// pulling in model.go's helpers.
func stripPaneANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		if c == 0x1b && i+1 < len(s) {
			next := s[i+1]
			if next == '[' {
				j := i + 2
				for j < len(s) {
					if s[j] >= 0x40 && s[j] <= 0x7e {
						j++
						break
					}
					j++
				}
				i = j
				continue
			}
			i += 2
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

// windowLines slices a flat lines list to a (height)-tall window
// shifted up by `scroll` rows, padding short content with blanks at
// the top so the newest entry stays anchored at the bottom. Returns
// (rendered_joined, all_lines, viewStart) where viewStart is the
// absolute index of the first VISIBLE non-pad line.
func windowLines(all []string, height, scroll int) (string, []string, int) {
	total := len(all)
	if height <= 0 {
		return "", all, 0
	}
	if total == 0 {
		out := make([]string, height)
		return strings.Join(out, "\n"), all, 0
	}
	maxScroll := total - height
	if maxScroll < 0 {
		maxScroll = 0
	}
	if scroll > maxScroll {
		scroll = maxScroll
	}
	if scroll < 0 {
		scroll = 0
	}
	end := total - scroll
	start := end - height
	if start < 0 {
		start = 0
	}
	out := append([]string(nil), all[start:end]...)
	for len(out) < height {
		out = append([]string{""}, out...)
	}
	return strings.Join(out, "\n"), all, start
}

// renderChatPane returns the chat history rendered for `height` rows.
// scroll is the number of rows scrolled UP from the bottom; 0 means
// "follow the latest message". The value is clamped against the total
// line count so over-scrolling is harmless.
//
// Returns (rendered, atTop, atBottom, totalLines, viewStart, allLines)
// so the caller can show a scroll indicator in the chat title and
// build a paneSnapshot for highlight-to-copy.
func renderChatPane(chat []chatMessage, renderer *glamour.TermRenderer,
	height, width, scroll int) (string, bool, bool, int, int, []string) {
	if height <= 0 {
		return "", true, true, 0, 0, nil
	}
	if len(chat) == 0 {
		empty := dimStyle.Render(
			"Type a message and press Enter to send it to the agent.")
		return empty, true, true, 0, 0, []string{empty}
	}

	// Flatten every message into a list of display lines with blank
	// separators. We then take a (height)-tall window from the bottom
	// shifted up by `scroll` rows.
	allLines := []string{}
	for i, msg := range chat {
		if i > 0 {
			allLines = append(allLines, "")
		}
		allLines = append(allLines, renderChatMessage(msg, renderer, width)...)
	}
	// Defense in depth: hard-truncate any line wider than `width` using
	// ANSI-aware truncation. Glamour and other renderers should respect
	// our wrap settings, but a pathological line (long URL, code block
	// that didn't break, runaway styling) would otherwise expand the
	// chat box past the right column and cover the sidebar. lipgloss
	// Width() is a *minimum* — without this clamp, the box grows.
	for i, ln := range allLines {
		if lipgloss.Width(ln) > width {
			allLines[i] = ansi.Truncate(ln, width, "")
		}
	}
	total := len(allLines)

	// Clamp scroll so the user can't drift off either end.
	maxScroll := total - height
	if maxScroll < 0 {
		maxScroll = 0
	}
	if scroll > maxScroll {
		scroll = maxScroll
	}
	if scroll < 0 {
		scroll = 0
	}

	end := total - scroll
	start := end - height
	if start < 0 {
		start = 0
	}
	// Snapshot lives in the caller now (paneSnaps) — return allLines
	// + viewStart so layoutFullScreen can build a paneSnapshot with
	// the correct bounds.
	out := append([]string(nil), allLines[start:end]...)
	// Pad short content to height (newest stays at bottom). When we
	// pad ABOVE, the visual row → flat-line index relation shifts:
	// the first padded row maps to line 0 (start = 0), but the user's
	// mouse Y still indexes from the chat pane's top. The handler
	// guards against negative line indices, so this is OK.
	for len(out) < height {
		out = append([]string{""}, out...)
	}
	return strings.Join(out, "\n"), start == 0, scroll == 0, total, start, allLines
}

// renderChatMessage formats one chat row into a list of display lines.
func renderChatMessage(m chatMessage, renderer *glamour.TermRenderer,
	width int) []string {
	switch m.Role {
	case roleUser:
		header := chatUserStyle.Render("you")
		body := wrapPlain(m.Body, width-2)
		return prependPrefix(header, body)

	case roleAssistant:
		label := m.Meta
		if label == "" {
			label = "agent"
		}
		header := chatAssistantStyle.Bold(true).Render(label)
		body := renderMarkdown(m.Body, renderer)
		return prependPrefix(header, body)

	case roleTool:
		// Meta convention from appendChatEvent:
		//   "→ <tool>" — pending call, no mark
		//   "← <tool>" — returned, render ✓ or ✗ based on m.Success
		var header string
		if strings.HasPrefix(m.Meta, "→") {
			header = chatToolStyle.Render("tool · " + m.Meta)
		} else {
			mark := okStyle.Render("✓")
			if !m.Success {
				mark = failStyle.Render("✗")
			}
			header = chatToolStyle.Render(fmt.Sprintf("%s tool · %s", mark, m.Meta))
		}
		body := wrapPlain(m.Body, width-2)
		return prependPrefix(header, body)

	case roleSystem:
		tag := m.Meta
		if tag == "" {
			tag = "system"
		}
		// Per-tag styling so the user can scan the chat at a glance:
		// turn separators in purple, LLM call markers in cyan italic,
		// V3 progress in violet, everything else in dim grey italic.
		var header string
		switch m.Meta {
		case "turn":
			header = chatTurnStyle.Render(fmt.Sprintf("── %s ──", m.Body))
			return []string{header}
		case "llm", "v3-llm":
			// Multi-line streaming bodies get a header line + indented
			// dim body. Single-line stats fit on one row. v3-llm uses
			// the same layout but inherits the V3 violet tint via the
			// switch below — pick the style first.
			lineStyle := chatLLMStyle
			if m.Meta == "v3-llm" {
				lineStyle = chatV3Style.Italic(true)
				tag = "v3"
			}
			if !strings.Contains(m.Body, "\n") {
				return []string{lineStyle.Render(fmt.Sprintf("· %s · %s", tag, m.Body))}
			}
			parts := strings.SplitN(m.Body, "\n", 2)
			out := []string{lineStyle.Render(fmt.Sprintf("· %s · %s", tag, parts[0]))}
			for _, ln := range wrapPlain(parts[1], width-4) {
				out = append(out, lineStyle.Render("  "+ln))
			}
			return out
		case "V3":
			header = chatV3Style.Render(fmt.Sprintf("· %s · %s", tag, m.Body))
			return []string{header}
		}
		header = chatSystemStyle.Render(fmt.Sprintf("· %s", tag))
		body := wrapPlain(m.Body, width-2)
		return prependPrefix(header, body)
	}
	return []string{m.Body}
}

func renderMarkdown(body string, r *glamour.TermRenderer) []string {
	if r == nil {
		return wrapPlain(body, 80)
	}
	out, err := r.Render(body)
	if err != nil {
		return wrapPlain(body, 80)
	}
	out = strings.TrimRight(out, "\n")
	return strings.Split(out, "\n")
}

// wrapPlain hard-wraps long lines at the given width. Nothing fancy —
// just splits at whitespace when possible. Operates on runes so a
// multi-byte character (em dash, box drawing, CJK) is never split
// mid-sequence and width counts characters, not bytes.
func wrapPlain(s string, width int) []string {
	if width < 8 {
		width = 8
	}
	out := []string{}
	for _, line := range strings.Split(s, "\n") {
		runes := []rune(line)
		for len(runes) > width {
			cut := width
			for i := width - 1; i > width/2; i-- {
				if runes[i] == ' ' {
					cut = i
					break
				}
			}
			out = append(out, string(runes[:cut]))
			runes = runes[cut:]
			for len(runes) > 0 && runes[0] == ' ' {
				runes = runes[1:]
			}
		}
		out = append(out, string(runes))
	}
	return out
}

func prependPrefix(header string, body []string) []string {
	out := make([]string, 0, len(body)+1)
	out = append(out, header)
	for _, line := range body {
		out = append(out, "  "+line)
	}
	return out
}

// renderStatsPane is a one-line summary of pipeline counters + active
// stage. Plain text — no border (rendered between chat and input).
func renderStatsPane(p *pipelineState, width int,
	lastTurnTokens, totalTokens, maxTokens int) string {
	parts := []string{}
	if active := p.activeStage(); active != nil {
		parts = append(parts, runStyle.Render(fmt.Sprintf("● %s", active.Name)))
	}
	if p.currentTurn > 0 {
		parts = append(parts, fmt.Sprintf("turn:%d", p.currentTurn))
	}
	// Token usage / context-window pressure. lastTurnTokens reflects the
	// most recent llm_call_end's total (llama-server reports prompt+completion
	// for the whole call). When that approaches maxTokens, the next turn
	// will start truncating history — flag in red over 80% utilization.
	if lastTurnTokens > 0 && maxTokens > 0 {
		pct := float64(lastTurnTokens) / float64(maxTokens) * 100
		ctxStr := fmt.Sprintf("ctx:%s/%s (%.0f%%)",
			formatTokens(lastTurnTokens), formatTokens(maxTokens), pct)
		switch {
		case pct >= 80:
			parts = append(parts, failStyle.Render(ctxStr))
		case pct >= 50:
			parts = append(parts, runStyle.Render(ctxStr))
		default:
			parts = append(parts, ctxStr)
		}
	}
	if totalTokens > 0 {
		parts = append(parts, fmt.Sprintf("session:%s",
			formatTokens(totalTokens)))
	}
	parts = append(parts,
		fmt.Sprintf("tools:%d✓/%d✗", p.toolSuccesses, p.toolFailures),
		fmt.Sprintf("events:%d", p.totalEvents))
	if p.errors > 0 {
		parts = append(parts, failStyle.Render(fmt.Sprintf("errors:%d", p.errors)))
	}
	if p.done {
		mark := okStyle.Render("✓ done")
		if !p.doneSuccess {
			mark = failStyle.Render("✗ done")
		}
		parts = append(parts, mark, fmt.Sprintf("total:%dms", p.totalMS))
	}
	line := dimStyle.Render(strings.Join(parts, "  "))
	if lipgloss.Width(line) > width {
		line = ansi.Truncate(line, width, "")
	}
	return line
}

// formatTokens renders a token count in a compact form (e.g. 12k, 1.4k).
// Used by the stats line so a 32768-token max doesn't blow the width
// budget on narrow terminals.
// slashCommandList drives the slash-mode autocomplete hint. Order is
// the order users see; keep frequent commands first.
var slashCommandList = []string{
	"/help", "/review", "/redo",
	"/clear", "/compact",
	"/add", "/drop", "/context",
	"/diff", "/commit", "/undo",
	"/run", "/hide", "/show",
	"/mouse", "/copy", "/demo", "/quit",
}

// renderSlashHint shows a one-line list of slash commands matching
// the current prefix. Renders empty when the prefix matches nothing
// (e.g. "/foobar" — let the unknown-command error explain itself).
func renderSlashHint(value string, width int) string {
	prefix := strings.TrimSpace(value)
	matches := []string{}
	for _, c := range slashCommandList {
		if strings.HasPrefix(c, prefix) {
			matches = append(matches, c)
		}
	}
	if len(matches) == 0 {
		return ""
	}
	header := lipgloss.NewStyle().
		Foreground(lipgloss.Color("141")).
		Bold(true).
		Render("commands:")
	body := dimStyle.Render(strings.Join(matches, "  "))
	line := header + " " + body
	if lipgloss.Width(line) > width {
		line = ansi.Truncate(line, width, "")
	}
	return line
}

// renderHelpHint shows the slashCommandHelp body above the input box
// while the user is in "?" mode. Live hint, no Enter required —
// matches how /slash shows its command dropdown. Pressing Enter still
// routes through /help (so the help also lands in chat scrollback).
// maxLines is the row budget the layout reserved for the hint block;
// when the help body doesn't fit, the tail is replaced with a pointer
// to /help so the block never bleeds past its budget.
func renderHelpHint(width, maxLines int) string {
	header := lipgloss.NewStyle().
		Foreground(lipgloss.Color("220")).
		Bold(true).
		Render("help · press Enter to dismiss, or just keep typing")
	body := dimStyle.Render(slashCommandHelp)
	out := header + "\n" + body
	// Width-clamp each rendered line so a wide help body doesn't blow
	// past the input column on narrow terminals.
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if lipgloss.Width(line) > width {
			lines[i] = ansi.Truncate(line, width, "")
		}
	}
	if maxLines > 0 && len(lines) > maxLines {
		if maxLines == 1 {
			lines = lines[:1]
		} else {
			lines = append(lines[:maxLines-1],
				dimStyle.Render("… (/help for the full list)"))
		}
	}
	return strings.Join(lines, "\n")
}

// permBorderStyle frames the interactive permission modal. Amber border so it
// reads as an action-required prompt, distinct from the pane boxes.
var permBorderStyle = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(lipgloss.Color("214"))

// The permission modal shows the request whole. Approval is the only
// per-command control, and a box fixed at one row per line cut a command at
// the terminal width: the tail of a chain (`... && rm -rf src`) was never
// seen by the person approving it.

// permPromptLines are the modal's content rows at innerW: title, tool, the
// message wrapped to its end, and the legend. A message longer than maxRows
// allows keeps its first lines and its last two, and says how many are not
// shown between them, so the end of a command is always on screen.
func permPromptLines(pp *permPrompt, innerW, maxRows int) []string {
	title := lipgloss.NewStyle().
		Foreground(lipgloss.Color("214")).Bold(true).
		Render("⚠ Permission required")
	tool := chatToolStyle.Render("tool · " + pp.toolName)
	msg := pp.message
	if strings.TrimSpace(msg) == "" {
		msg = "Allow " + pp.toolName + "?"
	}
	var body []string
	for _, ln := range strings.Split(msg, "\n") {
		body = append(body, strings.Split(ansi.Hardwrap(ln, innerW, true), "\n")...)
	}
	room := maxRows - 2 - 3 // borders; title, tool and legend
	if room < 3 {
		room = 3
	}
	if len(body) > room {
		head, tail := body[:room-3], body[len(body)-2:]
		hidden := len(body) - len(head) - len(tail)
		body = append(append(append([]string{}, head...),
			dimStyle.Render(fmt.Sprintf("… %d lines not shown …", hidden))), tail...)
	}
	legendText := "[y] allow once   [a] allow for session   [n] deny"
	if pp.oneTimeOnly {
		legendText = "[y] allow this one   [n] deny"
	}
	legend := lipgloss.NewStyle().Foreground(lipgloss.Color("117")).Render(legendText)

	lines := []string{title, tool}
	for _, ln := range body {
		lines = append(lines, chatAssistantStyle.Render(ln))
	}
	lines = append(lines, legend)
	for i, ln := range lines {
		if lipgloss.Width(ln) > innerW {
			lines[i] = ansi.Truncate(ln, innerW, "")
		}
	}
	return lines
}

// permPromptHeight is the rendered height of the modal, borders included, so
// the layout can reserve it before the box is drawn. 0 without a request.
func permPromptHeight(pp *permPrompt, innerW, maxRows int) int {
	if pp == nil {
		return 0
	}
	return len(permPromptLines(pp, innerW, maxRows)) + 2
}

// renderPermPrompt renders the permission approval modal as a bordered box
// `innerW` wide (matching the input box's content width). Returns "" when
// there is no pending request. The box is exactly permPromptHeight tall.
func renderPermPrompt(pp *permPrompt, innerW, maxRows int) string {
	if pp == nil {
		return ""
	}
	if innerW < 10 {
		innerW = 10
	}
	return permBorderStyle.Width(innerW).Render(strings.Join(permPromptLines(pp, innerW, maxRows), "\n"))
}

// renderBashHint shows a single warning row above the bash input box.
// Red text; brief enough to fit a single row at any reasonable width.
func renderBashHint(_ string, width int) string {
	header := lipgloss.NewStyle().
		Foreground(lipgloss.Color("196")).
		Bold(true).
		Render("bash:")
	body := dimStyle.Render(
		"Enter runs as a shell command in the working dir.")
	line := header + " " + body
	if lipgloss.Width(line) > width {
		line = ansi.Truncate(line, width, "")
	}
	return line
}

// formatTokens renders a token count compactly. Below 10,000 we keep
// the raw integer so the user sees per-token movement during
// streaming (1234 → 1235 → 1236…). At 10k+ the count changes too
// quickly for raw digits to be readable, so we abbreviate.
func formatTokens(n int) string {
	switch {
	case n >= 100000:
		return fmt.Sprintf("%dk", n/1000)
	case n >= 10000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// layoutFullScreen stitches header + sidebar + pipeline + chat +
// events + stats + input into a full-screen view.
//
// Horizontal budget:
//
//	files    sidebar — 28 cols when terminal ≥ 110 cols, hidden below
//	right    everything else
//
// Vertical budget (per row, including borders), right column:
//
//	header   1
//	pipeline up to 10  (title + up to 7 rows + 2 border, capped)
//	chat     fills (≥5)
//	events   5  (title + 2 rows + 2 border)
//	stats    1  (no border)
//	input    6  (title + 3 rows + 2 border)
//
// selectionState carries the user's drag-in-progress info into
// layoutFullScreen so the overlay can highlight the active range.
// Empty pane name = no selection in flight.
type selectionState struct {
	pane         string
	startY, endY int
	startX, endX int
}

func layoutFullScreen(p *pipelineState, events []Envelope, chat []chatMessage,
	inputView, inputValue, inputMode string,
	renderer *glamour.TermRenderer, header string,
	turnActive bool, spinnerFrame int,
	chatScroll, eventsScroll, pipelineScroll int,
	files []fileEntry, modified map[string]bool, fileScroll int, fileRoot string,
	lastTurnTokens, totalTokens, maxTokens int,
	hideFiles, hidePipeline, hideEvents bool,
	sel selectionState,
	calibrationBadge string,
	perm *permPrompt,
	width, height int) (string, int) {

	if width <= 0 || height <= 0 {
		return "", 0
	}

	const (
		headerH = 1
		statsH  = 1
	)
	// Sidebar gets a fixed 26 cols when there's enough room. Dropped
	// on truly narrow terminals (<90 cols) so the chat/events panes
	// don't get squeezed unreadably small. The previous 110-col
	// threshold meant the safe-default render (width=100, used before
	// the first WindowSizeMsg) hid the sidebar entirely — making it
	// look like the feature was broken at startup.
	sidebarW := 0
	if width >= 90 && !hideFiles {
		sidebarW = 26
	}
	rightW := width - sidebarW
	if rightW < 20 {
		// Pathological narrow case — a 10-col terminal still has to
		// produce *some* output. Force a minimum so lipgloss doesn't
		// see negative widths.
		rightW = 20
		sidebarW = 0
	}
	innerW := rightW - 2 // border consumes 2 cols on each box
	if innerW < 10 {
		innerW = 10
	}

	// Input box: 6 rows by default (title + 3 textarea rows + 2 border).
	// bash/slash mode adds a one-row hint banner above, so reserve one
	// extra row. help mode shows the multi-line slashCommandHelp body —
	// count its lines so the box doesn't bleed into the chat pane.
	inputH := 6
	switch inputMode {
	case "bash", "slash":
		inputH = 7
	case "help":
		// 6-row box + 1 hint header + N body lines. Cap at half the
		// terminal height (floor 8) so the chat pane stays visible on
		// small terminals — renderHelpHint truncates the body to fit.
		helpLines := strings.Count(slashCommandHelp, "\n") + 1
		inputH = 6 + 1 + helpLines
		cap := height / 2
		if cap < 8 {
			cap = 8
		}
		if inputH > cap {
			inputH = cap
		}
	}
	eventsH := 5 // title + 2 rows + 2 border
	if hideEvents {
		eventsH = 0
	}
	pipelineH := 0
	if !hidePipeline {
		pipelineRows := len(p.stages())
		if pipelineRows < 1 {
			pipelineRows = 1
		}
		pipelineH = pipelineRows + 3 // title + borders
		if pipelineH > 10 {
			pipelineH = 10
		}
	}
	// Reserve rows for the permission modal (rendered above the input box)
	// so nothing clips and View() height stays == terminal height. It gets
	// what the other panes leave at their minimum: a 4-row pipeline and a
	// 4-row chat.
	permMax := height - headerH - statsH - inputH - eventsH - 4
	if !hidePipeline {
		permMax -= 4
	}
	permH := permPromptHeight(perm, innerW, permMax)
	chatH := height - headerH - pipelineH - eventsH - statsH - inputH - permH
	if chatH < 5 {
		// Squeeze the pipeline (down to its 4-row minimum: title + one
		// content row + borders) before letting chat go below 5.
		shortfall := 5 - chatH
		if give := pipelineH - 4; give > 0 {
			if give > shortfall {
				give = shortfall
			}
			pipelineH -= give
			chatH += give
		}
		if chatH < 4 {
			chatH = 4
		}
	}

	// Reset pane snapshots — populated by each pane's render below.
	paneSnaps = paneSnaps[:0]

	pipelineBox := ""
	var pipelineAll []string
	pipelineViewStart := 0
	pipelineTopY := headerH + 2 // top border + title (only valid if shown)
	if !hidePipeline {
		pipelineContentH := pipelineH - 3 // borders + title
		if pipelineContentH < 1 {
			pipelineContentH = 1
		}
		var pipelinePane string
		pipelinePane, pipelineAll, pipelineViewStart = renderPipelinePane(
			p, pipelineContentH, innerW, pipelineScroll)
		pipelinePane = applySelectionOverlay(pipelinePane, "pipeline",
			sel, pipelineTopY, pipelineContentH)
		// PC-059: append the Lens/ASA calibration badge to the right
		// of the "Pipeline" title so users see compat verdict at a
		// glance. The badge is pre-rendered by the caller (model.go's
		// View) from m.calibration so this function stays UI-only.
		pipelineTitle := titleStyle.Render(" Pipeline ")
		if calibrationBadge != "" {
			pipelineTitle += calibrationBadge
		}
		pipelineBox = bordStyle.Width(innerW).Render(
			pipelineTitle + "\n" + pipelinePane)
	}

	// Chat: render history into the available rows, then append a
	// "thinking…" footer row below the content (still inside the box)
	// when a turn is in flight. Footer-anchored so the user's eye
	// follows naturally from the latest message down to the indicator,
	// instead of jumping back to the title at the top.
	chatContentH := chatH - 3 // -3 = title row + 2 border rows
	thinkingRow := ""
	if turnActive && chatContentH > 1 {
		chatContentH -= 1 // reserve one row for the indicator
		mark := spinnerFrames[spinnerFrame%len(spinnerFrames)]
		// Verb cycles every ~3s. spinnerFrame ticks at 150ms, so divide
		// by 20 for a 3s rotation. Adds a sense of progress on long turns.
		verb := thinkingVerbs[(spinnerFrame/20)%len(thinkingVerbs)]
		thinkingRow = "\n" + runStyle.Render(
			fmt.Sprintf("  %s %s…  (Ctrl+C to cancel)", mark, verb))
	}
	chatPane, atTop, atBottom, totalLines, viewStart, chatAll := renderChatPane(
		chat, renderer, chatContentH, innerW-2, chatScroll)
	// Compute chat pane top Y now (depends only on header + pipeline)
	// so the selection overlay can highlight rows BEFORE chatBox wraps.
	chatBoxTopY := headerH
	if !hidePipeline {
		chatBoxTopY += pipelineH
	}
	chatTopY := chatBoxTopY + 2 // top border + title
	chatPane = applySelectionOverlay(chatPane, "chat", sel,
		chatTopY, chatContentH)
	// Chat title shows scroll state. When following, just " Chat ".
	// When scrolled, show the position so the user knows where they
	// are and how to get back to live.
	chatTitle := " Chat "
	if !atBottom {
		chatTitle = fmt.Sprintf(" Chat — ↑ %d/%d (PgDn / Ctrl+End to follow) ",
			viewStart, totalLines)
		if atTop {
			chatTitle = fmt.Sprintf(" Chat — top %d/%d (PgDn to scroll down) ",
				viewStart, totalLines)
		}
	}
	chatBox := bordStyle.Width(innerW).Render(
		titleStyle.Render(chatTitle) + "\n" +
			chatPane +
			thinkingRow)

	// Compute screen-coord bounds for each pane and snapshot them.
	// Right column starts at screen X 0, files sidebar (if shown) at X
	// rightW. Each box has a top border (1 row) + title (1 row) +
	// content rows + bottom border (1 row); content X range inside the
	// box is [boxLeft+1, boxLeft+innerW-2] (we use innerW-2 as a safety
	// margin to avoid right-edge ANSI artifacts).
	rightLeftX := 1
	rightRightX := innerW - 2
	yCursor := headerH
	if !hidePipeline {
		paneSnaps = append(paneSnaps, paneSnapshot{
			name:      "pipeline",
			topY:      yCursor + 2, // top border + title
			bottomY:   yCursor + 2 + (pipelineH - 3) - 1,
			leftX:     rightLeftX,
			rightX:    rightRightX,
			viewStart: pipelineViewStart,
			lines:     pipelineAll,
		})
		yCursor += pipelineH
	}
	// chatTopY computed above. Snapshot for the mouse handler.
	paneSnaps = append(paneSnaps, paneSnapshot{
		name:      "chat",
		topY:      chatTopY,
		bottomY:   chatTopY + chatContentH - 1,
		leftX:     rightLeftX,
		rightX:    rightRightX,
		viewStart: viewStart,
		lines:     chatAll,
	})
	eventsTopY := yCursor + chatH + 2
	// (yCursor isn't bumped past chat — same reason as the events block
	// below: no downstream pane reads it. stats + input are positioned
	// from the bottom, not yCursor.)

	eventsBox := ""
	if !hideEvents {
		eventsContentH := eventsH - 3
		if eventsContentH < 1 {
			eventsContentH = 1
		}
		eventsPane, eventsAll, eventsViewStart := renderEventsPane(
			events, eventsContentH, innerW, eventsScroll)
		eventsPane = applySelectionOverlay(eventsPane, "events", sel,
			eventsTopY, eventsContentH)
		eventsBox = bordStyle.Width(innerW).Render(
			titleStyle.Render(" Events ") + "\n" + eventsPane)
		paneSnaps = append(paneSnaps, paneSnapshot{
			name:      "events",
			topY:      eventsTopY,
			bottomY:   eventsTopY + eventsContentH - 1,
			leftX:     rightLeftX,
			rightX:    rightRightX,
			viewStart: eventsViewStart,
			lines:     eventsAll,
		})
		// (Don't bump yCursor here — no pane below events references it,
		// stats + input are statically positioned relative to the bottom.)
	}

	statsLine := renderStatsPane(p, width, lastTurnTokens, totalTokens, maxTokens)

	// Mode-driven input box: red for bash, purple for slash, cyan for
	// the default chat mode. The title also flips so the user has two
	// signals (color + label) for the active mode.
	style := bordStyleFocused
	title := " Message "
	hint := ""
	switch inputMode {
	case "bash":
		style = bordStyleBash
		title = " Bash · executes on your machine "
		hint = renderBashHint(inputValue, innerW)
	case "slash":
		style = bordStyleSlash
		title = " Command "
		hint = renderSlashHint(inputValue, innerW)
	case "help":
		style = bordStyleSlash
		title = " ? · help "
		hint = renderHelpHint(innerW, inputH-6)
	}
	inputBox := style.Width(innerW).Render(
		titleStyle.Render(title) + "\n" + inputView)
	if hint != "" {
		inputBox = hint + "\n" + inputBox
	}

	// Build the right column out of only the panes that are still
	// enabled — JoinVertical is fine with an empty string but the
	// concise list reads better and keeps lipgloss off the empty rows.
	rightParts := []string{}
	if pipelineBox != "" {
		rightParts = append(rightParts, pipelineBox)
	}
	rightParts = append(rightParts, chatBox)
	if eventsBox != "" {
		rightParts = append(rightParts, eventsBox)
	}
	rightParts = append(rightParts, statsLine)
	// Permission modal sits just above the input box so the y/a/n legend is
	// next to where the user types. Reserved in the height budget above.
	if perm != nil {
		rightParts = append(rightParts, renderPermPrompt(perm, innerW, permMax))
	}
	rightParts = append(rightParts, inputBox)
	rightCol := lipgloss.JoinVertical(lipgloss.Left, rightParts...)

	// No sidebar on narrow terminals — return the right column at full
	// width with the header on top.
	if sidebarW == 0 {
		return lipgloss.JoinVertical(lipgloss.Left, header, rightCol), totalLines
	}

	// Sidebar runs the full body height (everything below the header).
	// Total box rows = bodyH (matches right column). Inside the box:
	// 1 title row + filesPane rows + 2 border rows = bodyH, so
	// filesPane gets bodyH - 3 rows of content.
	bodyH := height - headerH
	sidebarInnerW := sidebarW - 2
	filesContentH := bodyH - 3
	if filesContentH < 1 {
		filesContentH = 1
	}
	filesPane, filesAll, filesViewStart := renderFilesPane(files, modified, fileRoot,
		filesContentH, sidebarInnerW, fileScroll)
	filesPane = applySelectionOverlay(filesPane, "files", sel,
		headerH+2, filesContentH)
	filesBox := bordStyle.Width(sidebarInnerW).Height(bodyH - 2).Render(
		titleStyle.Render(" Files ") + "\n" + filesPane)
	// Snapshot files pane bounds for highlight-to-copy / wheel scroll.
	// Sidebar is on the right, after rightCol, so its X starts at rightW.
	rightColWidth := width - sidebarW
	filesLeftX := rightColWidth + 1
	filesRightX := rightColWidth + sidebarW - 2
	filesTopY := headerH + 2
	filesBottomY := filesTopY + filesContentH - 1
	paneSnaps = append(paneSnaps, paneSnapshot{
		name:      "files",
		topY:      filesTopY,
		bottomY:   filesBottomY,
		leftX:     filesLeftX,
		rightX:    filesRightX,
		viewStart: filesViewStart,
		lines:     filesAll,
		padBottom: true, // renderFilesPane pads below the content
	})

	// Sidebar on the right (after rightCol) per user preference —
	// keeps the chat anchored on the left where the eye starts.
	body := lipgloss.JoinHorizontal(lipgloss.Top, rightCol, filesBox)
	return lipgloss.JoinVertical(lipgloss.Left, header, body), totalLines
}

// PC-062 follow-up: file-tree sidebar.
//
// Renders the workspace root as a left sidebar so devs can see what's
// in the project at a glance without bouncing to a terminal pane. Two
// levels deep, sorted (dirs first, then files alphabetically), with
// recently-modified files highlighted (the agent just wrote them).

// fileEntry is one row in the sidebar tree.
type fileEntry struct {
	RelPath string // path relative to scan root, e.g. "src/main.go"
	IsDir   bool
	Depth   int
}

// scanIgnored returns true for directory names we never recurse into.
// Mostly noise that bloats the tree (build artifacts, VCS internals,
// language caches). Keeping this list small and explicit — better to
// show a few extra dirs than hide something the user cared about.
var scanIgnored = map[string]bool{
	".git":          true,
	"node_modules":  true,
	"__pycache__":   true,
	".venv":         true,
	"venv":          true,
	"dist":          true,
	"build":         true,
	".idea":         true,
	".vscode":       true,
	"target":        true,
	".next":         true,
	".nuxt":         true,
	".cache":        true,
	".pytest_cache": true,
	".mypy_cache":   true,
	".ruff_cache":   true,
	"__MACOSX":      true,
}

// scanFiles walks `root` to `maxDepth` directory levels and returns a
// flattened, ordered list of entries suitable for sidebar rendering.
// Caps at `maxEntries` total to keep huge monorepos from blowing up.
func scanFiles(root string, maxDepth, maxEntries int) []fileEntry {
	if root == "" {
		return nil
	}
	out := make([]fileEntry, 0, 64)
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if len(out) >= maxEntries {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		// Sort so dirs come before files at each level, alphabetical
		// within each group. Stable per-tick scans keep the sidebar
		// from jittering as files are added/removed.
		sort.SliceStable(entries, func(i, j int) bool {
			if entries[i].IsDir() != entries[j].IsDir() {
				return entries[i].IsDir()
			}
			return strings.ToLower(entries[i].Name()) <
				strings.ToLower(entries[j].Name())
		})
		for _, e := range entries {
			if len(out) >= maxEntries {
				return
			}
			name := e.Name()
			if e.IsDir() && scanIgnored[name] {
				continue
			}
			full := filepath.Join(dir, name)
			rel, err := filepath.Rel(root, full)
			if err != nil {
				continue
			}
			out = append(out, fileEntry{
				RelPath: rel,
				IsDir:   e.IsDir(),
				Depth:   depth,
			})
			if e.IsDir() && depth < maxDepth {
				walk(full, depth+1)
			}
		}
	}
	walk(root, 0)
	return out
}

var (
	fileDirStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("117")).
			Bold(true)

	fileFileStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("251"))

	fileModifiedStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("214")).
				Bold(true)
)

// renderFilesPane returns the sidebar contents (no border — caller
// wraps in a bordered box at the right size). `modified` is the set
// of relative paths that were touched by recent tool calls; those rows
// render in orange/bold so the user can see what just changed.
//
// Returns the rendered string, the full pre-window list of lines (for
// pane snapshot / highlight-to-copy), and the absolute viewStart index.
func renderFilesPane(entries []fileEntry, modified map[string]bool,
	root string, height, width, scroll int) (string, []string, int) {
	if height <= 0 {
		return "", nil, 0
	}
	if len(entries) == 0 {
		empty := dimStyle.Render("(empty workspace)")
		return empty, []string{empty}, 0
	}

	// Header row showing the scan root (basename only — we don't have
	// width to spare). Helps the user confirm "yes, this is where the
	// agent is operating".
	rootDisplay := filepath.Base(root)
	if rootDisplay == "" || rootDisplay == "/" {
		rootDisplay = root
	}
	header := dimStyle.Render(truncate("● "+rootDisplay, width))

	rows := []string{header}
	for _, e := range entries {
		indent := strings.Repeat("  ", e.Depth)
		name := filepath.Base(e.RelPath)
		var marker string
		var styled string
		switch {
		case e.IsDir:
			marker = "▸ "
			styled = fileDirStyle.Render(indent + marker + name + "/")
		case modified[e.RelPath]:
			marker = "● "
			styled = fileModifiedStyle.Render(indent + marker + name)
		default:
			marker = "  "
			styled = fileFileStyle.Render(indent + marker + name)
		}
		// Truncate the rendered string by visible width — lipgloss
		// strings include ANSI codes that don't count toward width.
		// Quick approximation: truncate raw, re-render with same style.
		raw := indent + marker + name
		if e.IsDir {
			raw += "/"
		}
		if lipgloss.Width(styled) > width {
			cut := truncate(raw, width)
			switch {
			case e.IsDir:
				styled = fileDirStyle.Render(cut)
			case modified[e.RelPath]:
				styled = fileModifiedStyle.Render(cut)
			default:
				styled = fileFileStyle.Render(cut)
			}
		}
		rows = append(rows, styled)
	}

	// Scroll: clamp `scroll` against total length, take a window.
	total := len(rows)
	maxScroll := total - height
	if maxScroll < 0 {
		maxScroll = 0
	}
	if scroll > maxScroll {
		scroll = maxScroll
	}
	if scroll < 0 {
		scroll = 0
	}
	end := total - scroll
	if end > total {
		end = total
	}
	start := end - height
	if start < 0 {
		start = 0
	}
	out := append([]string(nil), rows[start:end]...)
	for len(out) < height {
		out = append(out, "")
	}
	if total > height {
		// Quiet hint at the bottom that there's more.
		more := fmt.Sprintf("(+%d more)", total-height-scroll)
		if scroll > 0 {
			more = fmt.Sprintf("(↑%d / +%d more)", scroll, total-height-scroll)
		}
		if len(out) > 0 {
			out[len(out)-1] = dimStyle.Render(truncate(more, width))
		}
	}
	return strings.Join(out, "\n"), rows, start
}

// extractWritePath pulls the file path out of a tool call's args JSON
// for tools that touch a single file. Used to highlight modified files
// in the sidebar. Returns "" if the args don't fit the pattern.
func extractWritePath(args []byte) string {
	// We only need the "path" field — keep it cheap.
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return ""
	}
	return p.Path
}

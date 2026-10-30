package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/glamour"
)

// windowTitle is a harmless sequence that would set the terminal window's title.
const windowTitle = "\x1b]0;atlas test\x07"

// markedInTheOutput are the characters that must not reach the terminal: the
// end of windowTitle and the direction marks. "ESC [" is not among them,
// because the TUI's own styles use it.
const markedInTheOutput = "\x07\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069\u200e\u200f\u061c"

// Text from the proxy reaches the terminal with its control characters and
// direction marks replaced by a visible mark, on each path that renders it.
// `shown` is what must be on the screen in their place.
func TestTextFromTheProxyReachesTheTerminalWithItsControlsMarked(t *testing.T) {
	mark := string(controlMark)
	markdown, err := glamour.NewTermRenderer(glamour.WithStandardStyle("dark"), glamour.WithWordWrap(60))
	if err != nil {
		t.Fatal(err)
	}
	pipeline := newPipelineState()
	pipeline.apply(Envelope{Type: EvtStageStart, Stage: "plan" + windowTitle})
	for _, c := range []struct {
		name   string
		render func() string
		shown  string
	}{
		{"the model's text", func() string {
			return strings.Join(renderChatMessage(chatMessage{Role: roleAssistant, Body: windowTitle}, nil, 80), "\n")
		}, mark},
		{"a character reference in the model's markdown", func() string {
			return strings.Join(renderChatMessage(chatMessage{Role: roleAssistant, Body: "a &#27;&#91;1mword b"}, markdown, 80), "\n")
		}, mark + "[1mword"},
		{"a character reference in a fenced code block", func() string {
			return strings.Join(renderChatMessage(chatMessage{Role: roleAssistant, Body: "```\n&#169;\n```"}, markdown, 80), "\n")
		}, "&#169;"},
		{"the markdown renderer's output", func() string {
			return visibleControlsKeepingStyles("\x1b[1mbold\x1b[0m" + windowTitle)
		}, "\x1b[1mbold\x1b[0m" + mark},
		{"a tool result", func() string {
			return strings.Join(renderChatMessage(chatMessage{Role: roleTool, Meta: "← run_command", Body: windowTitle}, nil, 80), "\n")
		}, mark},
		{"the status of a done row", func() string {
			return strings.Join(renderChatMessage(chatMessage{Role: roleSystem, Meta: "done", Status: windowTitle}, nil, 80), "\n")
		}, mark},
		{"the approval prompt's message", func() string {
			return strings.Join(permPromptLines(&permPrompt{toolName: "write_file", message: windowTitle}, 60, 20), "\n")
		}, mark},
		{"direction marks in the approval prompt's message", func() string {
			return strings.Join(permPromptLines(&permPrompt{toolName: "write_file", message: "\u202eword\u202c \u200fword"}, 60, 20), "\n")
		}, mark + "word" + mark + " " + mark + "word"},
		{"a stage of the pipeline", func() string { return renderPipelineRow(pipeline.activeStage(), 80) }, mark},
		{"a stage name cut after a letter of two bytes", func() string {
			return renderPipelineRow(&stageStatus{Name: "abcdefghijklmn\xc3\xa9xyz"}, 80)
		}, "abcdefghijklmn\xc3\xa9"},
		{"the stats line", func() string { return renderStatsPane(&pipeline, 120, 0, 0, 0) }, mark},
		{"a line of the events pane", func() string {
			return formatEventLine(Envelope{Type: EvtError, Payload: map[string]interface{}{"message": windowTitle}}, 120)
		}, mark},
		{"a line of a file in the demo's file viewer", func() string {
			path := filepath.Join(t.TempDir(), "notes.txt")
			if err := os.WriteFile(path, []byte("notes"+windowTitle+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			shown, _ := readFileForDisplay(path, 10, 80, 0)
			return shown
		}, mark},
		{"a file name in the workspace", func() string {
			rendered, _, _ := renderFilesPane([]fileEntry{{RelPath: "notes" + windowTitle + ".txt"}}, nil, "/workspace", 10, 80, 0)
			return rendered
		}, mark},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := c.render()
			if strings.Contains(got, "\x1b]") || strings.ContainsAny(got, markedInTheOutput) || !utf8.ValidString(got) ||
				!strings.Contains(got, c.shown) {
				t.Errorf("rendered %q; want %q in place of the controls, and none of them", got, c.shown)
			}
		})
	}
}

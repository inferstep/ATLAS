// The events pane's lines: one line per /events envelope, with its time, its
// type in color, its stage and a short summary of its payload.

package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/itigges22/atlas-tui/internal/proxyclient"
)

func formatEventLine(ev proxyclient.Envelope, width int) string {
	ts := time.Unix(0, int64(ev.Timestamp*1e9)).Format("15:04:05")
	color := typeColor(ev.Type)
	typeCell := lipgloss.NewStyle().Foreground(color).Width(13).Render(visibleControls(ev.Type))
	stageCell := lipgloss.NewStyle().Foreground(lipgloss.Color("251")).
		Width(14).Render(truncate(visibleControls(ev.Stage), 14))
	detail := visibleControls(summarizePayload(ev))

	line := fmt.Sprintf("%s  %s %s %s", ts, typeCell, stageCell, detail)
	line = strings.ReplaceAll(line, "\n", " ")
	if lipgloss.Width(line) > width {
		line = ansi.Truncate(line, width, "")
	}
	return line
}

func typeColor(t string) lipgloss.Color {
	switch t {
	case proxyclient.EvtStageStart:
		return lipgloss.Color("33")
	case proxyclient.EvtStageEnd:
		return lipgloss.Color("42")
	case proxyclient.EvtToolCall:
		return lipgloss.Color("214")
	case proxyclient.EvtToolResult:
		return lipgloss.Color("70")
	case proxyclient.EvtMetric:
		return lipgloss.Color("99")
	case proxyclient.EvtError:
		return lipgloss.Color("196")
	case proxyclient.EvtDone:
		return lipgloss.Color("226")
	}
	return lipgloss.Color("245")
}

func summarizePayload(ev proxyclient.Envelope) string {
	switch ev.Type {
	case proxyclient.EvtToolCall:
		return fmt.Sprintf("%v  %v",
			ev.Payload["name"], truncateAny(ev.Payload["args_summary"], 60))
	case proxyclient.EvtToolResult:
		ok := ev.Payload["success"] == true
		mark := "✓"
		if !ok {
			mark = "✗"
		}
		dur := ""
		if ev.DurationMS > 0 {
			dur = fmt.Sprintf(" %dms", ev.DurationMS)
		}
		return fmt.Sprintf("%s  %v%s",
			mark, ev.Payload["name"], dur)
	case proxyclient.EvtMetric:
		return fmt.Sprintf("%v = %v",
			ev.Payload["name"], ev.Payload["value"])
	case proxyclient.EvtError:
		return truncateAny(ev.Payload["message"], 80)
	case proxyclient.EvtStageEnd:
		ok := ev.Payload["success"] == true
		mark := "✓"
		if !ok {
			mark = "✗"
		}
		dur := ""
		if ev.DurationMS > 0 {
			dur = fmt.Sprintf(" %dms", ev.DurationMS)
		}
		return mark + dur
	case proxyclient.EvtDone:
		ok := ev.Payload["success"] == true
		mark := "✓"
		if !ok {
			mark = "✗"
		}
		return fmt.Sprintf("%s  total %vms",
			mark, ev.Payload["total_duration_ms"])
	}
	if d, ok := ev.Payload["detail"].(string); ok {
		return truncate(d, 80)
	}
	return ""
}

func truncateAny(v interface{}, n int) string {
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	return truncate(s, n)
}

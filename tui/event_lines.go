// The events pane's lines: one line per /events envelope, with its time, its
// type in color, its stage and a short summary of its payload.

package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func formatEventLine(ev Envelope, width int) string {
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
	case EvtStageStart:
		return lipgloss.Color("33")
	case EvtStageEnd:
		return lipgloss.Color("42")
	case EvtToolCall:
		return lipgloss.Color("214")
	case EvtToolResult:
		return lipgloss.Color("70")
	case EvtMetric:
		return lipgloss.Color("99")
	case EvtError:
		return lipgloss.Color("196")
	case EvtDone:
		return lipgloss.Color("226")
	}
	return lipgloss.Color("245")
}

func summarizePayload(ev Envelope) string {
	switch ev.Type {
	case EvtToolCall:
		return fmt.Sprintf("%v  %v",
			ev.Payload["name"], truncateAny(ev.Payload["args_summary"], 60))
	case EvtToolResult:
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
	case EvtMetric:
		return fmt.Sprintf("%v = %v",
			ev.Payload["name"], ev.Payload["value"])
	case EvtError:
		return truncateAny(ev.Payload["message"], 80)
	case EvtStageEnd:
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
	case EvtDone:
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

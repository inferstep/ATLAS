package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/itigges22/atlas-tui/internal/proxyclient"
	"github.com/itigges22/atlas-tui/internal/sessionstore"
)

// The done row shows the run's status and reason, not only its summary
// (#236), and a missing status reads as incomplete (docs/reference/API.md).
func TestTheDoneRowShowsStatusAndReason(t *testing.T) {
	m := &tuiModel{}
	m.appendChatEvent(mkChatEvent("done", map[string]string{
		"summary": "Stopped: mod.py still does not parse.",
		"status":  "incomplete", "reason": "repair_unfinished"}))
	m.appendChatEvent(mkChatEvent("done", map[string]string{"status": "completed", "reason": "text_reply"}))
	m.appendChatEvent(mkChatEvent("done", map[string]string{"summary": "an older proxy"}))

	if len(m.chat) != 3 {
		t.Fatalf("%d rows, want one per done event (even with no summary)", len(m.chat))
	}
	for i, want := range []struct{ status, reason string }{
		{"incomplete", "repair_unfinished"}, {"completed", "text_reply"}, {"incomplete", ""},
	} {
		if r := m.chat[i]; r.Status != want.status || r.Reason != want.reason {
			t.Errorf("row %d: %q/%q, want %q/%q", i, r.Status, r.Reason, want.status, want.reason)
		}
	}
	lines := renderChatMessage(m.chat[0], nil, 80)
	if !strings.Contains(lines[0], "done · incomplete · repair_unfinished") {
		t.Errorf("header %q lacks the status and reason", lines[0])
	}
	if !strings.Contains(strings.Join(lines[1:], "\n"), "mod.py still does not parse") {
		t.Errorf("the summary is not under the header: %q", lines)
	}
	if got := renderChatMessage(sessionstore.ChatMessage{Role: sessionstore.RoleSystem, Meta: "done", Status: "timed_out",
		Reason: "work_deadline"}, nil, 80); !strings.Contains(got[0], "done · timed out · work_deadline") {
		t.Errorf("timed_out renders as %q", got[0])
	}
}

// Each status has its own color, so a stopped or failed run cannot pass for
// a completed one at a glance.
func TestEachDoneStatusLooksDifferent(t *testing.T) {
	seen := map[string]string{}
	for _, s := range []string{"completed", "incomplete", "stopped", "timed_out", "failed"} {
		fg := fmt.Sprint(doneStatusStyle(s).GetForeground())
		if other, dup := seen[fg]; dup {
			t.Errorf("%s and %s share the color %s", s, other, fg)
		}
		seen[fg] = s
	}
	if fmt.Sprint(doneStatusStyle("some_new_status").GetForeground()) !=
		fmt.Sprint(doneStatusStyle("incomplete").GetForeground()) {
		t.Error("an unknown status does not read like incomplete")
	}
}

func mkChatEvent(typ string, data interface{}) proxyclient.ChatEvent {
	b, _ := json.Marshal(data)
	return proxyclient.ChatEvent{Type: typ, Data: b}
}

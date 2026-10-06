package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// holdRefused makes the object hold fail for one test, as it does on a system
// that has none.
func holdRefused(t *testing.T) {
	t.Helper()
	previous := pinObjectFn
	pinObjectFn = func(string) (*objectHandle, error) { return nil, errObjectIdentityUnavailable }
	t.Cleanup(func() { pinObjectFn = previous })
}

// deleteThenDone is a model that asks for the deletion of a.py and then stops.
func deleteThenDone(turn int) map[string]interface{} {
	if turn == 0 {
		return dlDel("a.py")
	}
	return map[string]interface{}{"type": "done", "summary": "deleted a.py"}
}

// eventsOf returns the data of each stream event of one kind.
func (r *delLoop) eventsOf(kind string) []string {
	var out []string
	for _, event := range r.events {
		if event.kind == kind {
			out = append(out, event.data)
		}
	}
	return out
}

// toolMessages returns what the model read back for each call of one tool.
func (r *delLoop) toolMessages(tool string) []string {
	var out []string
	for _, message := range r.ctx.Messages {
		if message.Role == "tool" && message.ToolName == tool {
			out = append(out, message.Content)
		}
	}
	return out
}

func TestADeletionRefusedForTheMissingHoldSaysSoToTheUserAndTheModel(t *testing.T) {
	holdRefused(t)
	r := delLoopFixture(t, map[string]string{"a.py": delSeed}, "Delete a.py.", deleteThenDone)
	reason := errObjectIdentityUnavailable.Error()

	if asked := r.census["permission_request"]; asked != 0 {
		t.Errorf("%d prompt(s) were shown for a deletion the proxy refuses before asking", asked)
	}
	read := map[string][]string{
		"the permission_denied event": r.eventsOf("permission_denied"),
		"the tool_result event":       r.eventsOf("tool_result"),
		"the message to the model":    r.toolMessages("delete_file"),
	}
	for where, texts := range read {
		if len(texts) != 1 {
			t.Errorf("%s: %d found, want 1: %q", where, len(texts), texts)
			continue
		}
		if !strings.Contains(texts[0], reason) {
			t.Errorf("%s does not give the reason %q: %s", where, reason, texts[0])
		}
		if strings.Contains(texts[0], userDenied) {
			t.Errorf("%s says the user denied a deletion nobody was asked about: %s", where, texts[0])
		}
	}
	if _, err := os.Stat(filepath.Join(r.dir, "a.py")); err != nil {
		t.Errorf("the file is gone after a refused deletion: %v", err)
	}
}

func TestADeletionTheUserDeniedReadsAsItDidBefore(t *testing.T) {
	needsObjectHold(t)
	r := delLoopFixtureApprove(t, map[string]string{"a.py": delSeed}, "Delete a.py.", deleteThenDone, false)

	if asked := r.census["permission_request"]; asked != 1 {
		t.Errorf("%d prompt(s) were shown, want 1", asked)
	}
	if got := r.eventsOf("permission_denied"); len(got) != 1 || got[0] != `{"tool":"delete_file"}` {
		t.Errorf("permission_denied events = %q, want one with the tool name only", got)
	}
	if got := r.eventsOf("tool_result"); len(got) != 1 || !strings.Contains(got[0], `"error":"`+userDenied+`"`) {
		t.Errorf("tool_result events = %q, want one with the error %q", got, userDenied)
	}
	if got := r.toolMessages("delete_file"); len(got) != 1 || got[0] != `{"success":false,"error":"permission denied by user"}` {
		t.Errorf("the model read %q, want the same two-key message as before", got)
	}
}

func TestOnlyTheMissingHoldChangesWhatADeniedCallReads(t *testing.T) {
	ctx := &AgentContext{PassID: "refusal-reason"}
	if got := permissionDenial(ctx, "call_none"); got != userDenied {
		t.Errorf("a call with no recorded refusal reads %q, want %q", got, userDenied)
	}
	noteMissingHold(ctx, "call_other", "delete_file: file not found: gone.py")
	if got := permissionDenial(ctx, "call_other"); got != userDenied {
		t.Errorf("another refusal reads %q, want %q for now", got, userDenied)
	}
	noteMissingHold(ctx, "call_hold", "delete_file: "+errObjectIdentityUnavailable.Error())
	first, second := permissionDenial(ctx, "call_hold"), permissionDenial(ctx, "call_hold")
	if !strings.Contains(first, errObjectIdentityUnavailable.Error()) || !strings.Contains(first, "Nobody was asked") {
		t.Errorf("the missing hold reads %q, want the reason and that nobody was asked", first)
	}
	if second != userDenied {
		t.Errorf("the reason was read twice: %q", second)
	}
	if got := deniedToolMessage(userDenied); got != `{"success":false,"error":"permission denied by user"}` {
		t.Errorf("deniedToolMessage(%q) = %s", userDenied, got)
	}
	if got := permissionDeniedEvent("delete_file", userDenied); len(got) != 1 || got["tool"] != "delete_file" {
		t.Errorf("the event of a user denial = %v, want the tool name only", got)
	}
}

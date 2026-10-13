package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// holdStandIn lets a test reach the refusals that come after the hold on a
// system that has none. Where the hold exists the real one is used.
func holdStandIn(t *testing.T) {
	t.Helper()
	if objectHoldSupported {
		return
	}
	previous := pinObjectFn
	pinObjectFn = func(string) (*objectHandle, error) { return &objectHandle{}, nil }
	t.Cleanup(func() { pinObjectFn = previous })
}

// notAllowed runs the approval for one call and returns what the call then
// reads as, and how many prompts were shown.
func notAllowed(t *testing.T, ctx *AgentContext, tool, args string) (string, int) {
	t.Helper()
	prompts := 0
	ctx.StreamFn = func(kind string, _ interface{}) {
		if kind == "permission_request" {
			prompts++
		}
	}
	if awaitPermission(ctx, tool, "call_reason", json.RawMessage(args)) {
		t.Fatalf("%s %s was allowed", tool, args)
	}
	return permissionDenial(ctx, "call_reason"), prompts
}

func TestADeletionRefusedBeforeAskingReadsAsItsOwnReason(t *testing.T) {
	holdStandIn(t)
	dir := t.TempDir()
	writeInWorkspace(t, dir, "pkg/mod.py", "A = 1\n")
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Fatalf("make a file of a type that is not supported: %v", err)
	}
	for _, c := range []struct{ name, args, reason string }{
		{"a path outside the workspace", `{"path":"../outside.py"}`, "is outside the workspace"},
		{"a target on the deny list", `{"path":".env"}`, "blocked by safety rule: writing .env"},
		{"a missing file", `{"path":"gone.py"}`, "file not found: gone.py"},
		{"a directory that is not empty", `{"path":"pkg"}`, "directory not empty: pkg (1 entries)"},
		{"a file type that is not supported", `{"path":"pipe"}`, "unsupported target type"},
		{"an empty path", `{"path":""}`, "path cannot be empty"},
		{"arguments that cannot be read", `{"path":123}`, "arguments are not usable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := deletePermCtx(t, "sess-reason", dir)
			defer cancel()
			denial, prompts := notAllowed(t, ctx, "delete_file", c.args)
			if prompts != 0 {
				t.Errorf("%d prompt(s) were shown for a deletion the proxy refuses before asking", prompts)
			}
			if !strings.Contains(denial, c.reason) || !strings.Contains(denial, "Nobody was asked, and nothing was deleted.") {
				t.Errorf("the call reads %q, want the reason %q and that nobody was asked", denial, c.reason)
			}
			if strings.Contains(denial, userDenied) {
				t.Errorf("the call reads as denied by the user: %q", denial)
			}
			if again := permissionDenial(ctx, "call_reason"); again != userDenied {
				t.Errorf("the reason was kept for a later call: %q", again)
			}
		})
	}
}

func TestACallNobodyCouldBeAskedAboutSaysSo(t *testing.T) {
	ctx, cancel := permCtx("")
	defer cancel()
	denial, prompts := notAllowed(t, ctx, "run_command", `{"command":"ls"}`)
	if prompts != 0 {
		t.Errorf("%d prompt(s) were shown in a request with no session", prompts)
	}
	for _, want := range []string{"run_command needs the user's approval", "no session id", "nobody could be asked"} {
		if !strings.Contains(denial, want) {
			t.Errorf("the call reads %q, want %q in it", denial, want)
		}
	}
	other, cancelOther := permCtx("")
	defer cancelOther()
	if got := permissionDenial(other, "call_reason"); got != userDenied {
		t.Errorf("another request with no session read this one's reason: %q", got)
	}
}

func TestAPromptNobodyAnsweredInTimeSaysSo(t *testing.T) {
	t.Setenv("ATLAS_PERMISSION_TIMEOUT_SEC", "1")
	ctx, cancel := permCtx("sess-reason-timeout")
	defer cancel()
	denial, prompts := notAllowed(t, ctx, "run_command", `{"command":"ls"}`)
	if prompts != 1 {
		t.Errorf("%d prompt(s) were shown, want 1", prompts)
	}
	for _, want := range []string{"nobody answered the approval prompt for run_command", "within 1s", "Do not send the same call again"} {
		if !strings.Contains(denial, want) {
			t.Errorf("the call reads %q, want %q in it", denial, want)
		}
	}
}

func TestARequestThatEndedBeforeTheAnswerSaysSo(t *testing.T) {
	ctx, cancel := permCtx("sess-reason-ended")
	go func() {
		waitForPending(t, "sess-reason-ended", "call_reason")
		cancel()
	}()
	denial, _ := notAllowed(t, ctx, "run_command", `{"command":"ls"}`)
	if !strings.Contains(denial, "the request ended before the approval prompt for run_command was answered") {
		t.Errorf("the call reads %q, want that the request ended first", denial)
	}
}

func TestADenialByTheUserStillReadsAsADenial(t *testing.T) {
	ctx, cancel := permCtx("sess-reason-denied")
	defer cancel()
	go func() {
		waitForPending(t, "sess-reason-denied", "call_reason")
		postDecision(t, `{"session_id":"sess-reason-denied","tool_call_id":"call_reason","decision":"deny"}`)
	}()
	if denial, _ := notAllowed(t, ctx, "run_command", `{"command":"ls"}`); denial != userDenied {
		t.Errorf("a denial by the user reads %q, want %q", denial, userDenied)
	}
	if got := deniedToolMessage(userDenied); got != `{"success":false,"error":"permission denied by user"}` {
		t.Errorf("deniedToolMessage(%q) = %s", userDenied, got)
	}
	if got := permissionDeniedEvent("delete_file", userDenied); len(got) != 1 || got["tool"] != "delete_file" {
		t.Errorf("the event of a user denial = %v, want the tool name only", got)
	}
}

func TestAMissingFileReachesTheUserAndTheModelAsItsReason(t *testing.T) {
	r := delLoopFixture(t, map[string]string{"a.py": delSeed}, "Delete gone.py.", func(turn int) map[string]interface{} {
		if turn == 0 {
			return dlDel("gone.py")
		}
		return map[string]interface{}{"type": "done", "summary": "gone.py is not there"}
	})
	for where, texts := range map[string][]string{
		"the permission_denied event": r.eventsOf("permission_denied"),
		"the tool_result event":       r.eventsOf("tool_result"),
		"the message to the model":    r.toolMessages("delete_file"),
	} {
		if len(texts) != 1 || !strings.Contains(texts[0], "file not found: gone.py") || strings.Contains(texts[0], userDenied) {
			t.Errorf("%s = %q, want one text with the reason and no denial by the user", where, texts)
		}
	}
}

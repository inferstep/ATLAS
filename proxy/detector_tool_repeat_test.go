package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The repeat detector counts identical call signatures in a short window and
// fires on the third. A write_file signature keys on the path and the content,
// with trailing whitespace ignored; a structural_edit signature keys on the
// path and the selector, not the body.

func TestRepeatDetectorFiresOnIdenticalCalls(t *testing.T) {
	ctx := &AgentContext{}
	args := json.RawMessage(`{"path":"app.py","offset":0,"limit":100}`)
	for i := 0; i < 2; i++ {
		if _, _, repeating := recordToolCall(ctx, "read_file", args); repeating {
			t.Fatalf("fired at call %d, want threshold 3", i+1)
		}
	}
	msg, obs, repeating := recordToolCall(ctx, "read_file", args)
	if !repeating {
		t.Fatal("identical call 3x must fire")
	}
	if !strings.Contains(msg, "read_file") {
		t.Fatalf("corrective doesn't name the tool: %q", msg)
	}
	// The observation carries the streak the detector just erased.
	if obs.Count != toolRepeatThreshold {
		t.Errorf("observation count = %d, want %d", obs.Count, toolRepeatThreshold)
	}
	// The detector owns the reset: the window is empty on return, so the
	// same streak can't fire a second corrective.
	if len(ctx.RecentToolCalls) != 0 {
		t.Errorf("firing must clear the window, got %d entries", len(ctx.RecentToolCalls))
	}
	if _, _, again := recordToolCall(ctx, "read_file", args); again {
		t.Error("the call right after a fire must start a fresh streak")
	}
}

// Calls that differ only in ways the signature ignores are one call repeated.
func TestRepeatDetectorFiresOnTheThirdSameCall(t *testing.T) {
	for _, tc := range []struct {
		name          string
		tool          string
		calls         [3]string
		wantInMessage []string
	}{
		{
			name: "run_command with keys reordered and spaced",
			tool: "run_command",
			calls: [3]string{
				`{"command":"pytest","timeout":30}`,
				`{"timeout":30,"command":"pytest"}`,
				`{ "command" : "pytest", "timeout" : 30 }`,
			},
			wantInMessage: []string{"run_command"},
		},
		{
			// The model rewrites the same draft from memory: same code, same
			// path, only trailing spaces differ.
			name: "write_file of the same code with trailing spaces",
			tool: "write_file",
			calls: [3]string{
				`{"path":"app.py","content":"from flask import Flask\napp = Flask(__name__)"}`,
				`{"path":"app.py","content":"from flask import Flask\napp = Flask(__name__) "}`,
				`{"path":"app.py","content":"from flask import Flask\napp = Flask(__name__)"}`,
			},
			wantInMessage: []string{"app.py", "rewritten"},
		},
		{
			name: "write_file of the same code with trailing spaces and CR line ends",
			tool: "write_file",
			calls: [3]string{
				`{"path":"app.py","content":"def f():\n    return 1"}`,
				`{"path":"app.py","content":"def f():\n    return 1  "}`,
				`{"path":"app.py","content":"def f():\r\n    return 1\r"}`,
			},
			wantInMessage: []string{"app.py"},
		},
		{
			// Each attempt carries a different body, but the selector is what
			// fails: the node does not hold the code being changed. The message
			// must name the selector as the cause, or the model rewrites the
			// body a fourth time.
			name: "structural_edit on one selector with different bodies",
			tool: "structural_edit",
			calls: [3]string{
				`{"path":"app.py","selector":"function:index","content":"first attempt"}`,
				`{"path":"app.py","selector":"function:index","content":"second, rewritten"}`,
				`{"path":"app.py","selector":"function:index","content":"third, different again"}`,
			},
			wantInMessage: []string{"function:index", "SELECTOR", "edit_file", "insert_after"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &AgentContext{}
			for i, call := range tc.calls[:2] {
				if _, _, fired := recordToolCall(ctx, tc.tool, json.RawMessage(call)); fired {
					t.Fatalf("fired on call %d, want the third", i+1)
				}
			}
			msg, _, fired := recordToolCall(ctx, tc.tool, json.RawMessage(tc.calls[2]))
			if !fired {
				t.Fatal("the third same call did not fire")
			}
			for _, want := range tc.wantInMessage {
				if !strings.Contains(msg, want) {
					t.Errorf("message lacks %q:\n%s", want, msg)
				}
			}
		})
	}
}

// Calls that differ in a way that matters are not a loop, however many there
// are.
func TestRepeatDetectorStaysSilentOnDifferentCalls(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tool  string
		calls []string
	}{
		{
			name: "write_file to different paths",
			tool: "write_file",
			calls: []string{
				`{"path":"app.py","content":"x"}`,
				`{"path":"static/game.js","content":"x"}`,
				`{"path":"templates/index.html","content":"x"}`,
			},
		},
		{
			name: "distinct edit_file edits to one path",
			tool: "edit_file",
			calls: []string{
				`{"path":"app.py","old_str":"v0","new_str":"v1"}`,
				`{"path":"app.py","old_str":"v1","new_str":"v2"}`,
				`{"path":"app.py","old_str":"v2","new_str":"v3"}`,
				`{"path":"app.py","old_str":"v3","new_str":"v4"}`,
			},
		},
		{
			// Fixing successive compiler errors rewrites one file with new
			// content each time.
			name: "write_file of materially different content",
			tool: "write_file",
			calls: []string{
				`{"path":"main.py.c","content":"int main(){ return 0; }"}`,
				`{"path":"main.py.c","content":"int main(){ printf(\"x\"); return 0; }"}`,
				`{"path":"main.py.c","content":"#include <stdio.h>\nint main(){ printf(\"x\"); return 0; }"}`,
			},
		},
		{
			// Leading indentation is code in Python, so it is not collapsed.
			name: "write_file with changed indentation",
			tool: "write_file",
			calls: []string{
				`{"path":"m.py","content":"def f():\nreturn 1"}`,
				`{"path":"m.py","content":"def f():\n    return 1"}`,
				`{"path":"m.py","content":"def f():\n        return 1"}`,
			},
		},
		{
			name: "structural_edit on different selectors",
			tool: "structural_edit",
			calls: []string{
				`{"path":"app.py","selector":"function:a","content":"x"}`,
				`{"path":"app.py","selector":"function:b","content":"x"}`,
				`{"path":"app.py","selector":"function:c","content":"x"}`,
				`{"path":"app.py","selector":"class:D","content":"x"}`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &AgentContext{}
			for i, call := range tc.calls {
				if _, _, fired := recordToolCall(ctx, tc.tool, json.RawMessage(call)); fired {
					t.Fatalf("call %d was treated as a repeat", i+1)
				}
			}
		})
	}
}

func TestWriteFileRepeatOutsideWindowDoesNotFire(t *testing.T) {
	ctx := &AgentContext{}
	wf := json.RawMessage(`{"path":"app.py","content":"draft"}`)
	recordToolCall(ctx, "write_file", wf)
	// Eight unrelated calls push the first write out of the window.
	for i := 0; i < toolRepeatWindow; i++ {
		recordToolCall(ctx, "read_file",
			json.RawMessage(fmt.Sprintf(`{"path":"f%d.py"}`, i)))
	}
	recordToolCall(ctx, "write_file", wf)
	if _, _, repeating := recordToolCall(ctx, "write_file", wf); repeating {
		t.Fatal("two in-window writes must not fire (threshold 3)")
	}
}

func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// The repeat detector needs three calls and steers only the next turn, so an
// identical pair of failing calls never reaches it. The retry refusal stops
// the second one before it runs, and hands back the first rejection.
func TestAnIdenticalResendOfARejectedCallIsRefused(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	args := json.RawMessage(`{"path":"app.py","start_line":201,"end_line":201,"content":"x"}`)

	if refusal := identicalRetryRefusal(ctx, "replace_lines", args); refusal != "" {
		t.Fatalf("a first attempt must run: %s", refusal)
	}
	recordFailedToolCall(ctx, "replace_lines", args, "stops a render loop: `draw` now runs once")

	refusal := identicalRetryRefusal(ctx, "replace_lines", args)
	if refusal == "" {
		t.Fatal("the identical re-send must be refused before it executes")
	}
	// The original rejection has to come back with it: the model needs the
	// reason, not just the fact that it repeated itself.
	if !strings.Contains(refusal, "stops a render loop") {
		t.Errorf("refusal drops the original reason:\n%s", refusal)
	}
	if !strings.Contains(refusal, "read_file") {
		t.Errorf("refusal gives no way forward:\n%s", refusal)
	}
}

// The refusal holds only for the same call that failed, with no success in
// between. Any success drops every remembered rejection, so re-running a test
// after a fix, or re-reading after an edit, is never refused.
func TestIdenticalRetryRefusalAppliesOnlyToAnUnchangedFailedCall(t *testing.T) {
	const edit = `{"path":"t.py","old_str":"@Pytest.fixture","new_str":"@pytest.fixture"}`
	for _, tc := range []struct {
		name        string
		before      func(ctx *AgentContext)
		tool        string
		args        string
		wantRefused bool
	}{
		{
			name:   "a repeated call that never failed",
			before: func(ctx *AgentContext) {},
			tool:   "read_file", args: `{"path":"app.py"}`,
		},
		{
			name: "a different call to the same tool",
			before: func(ctx *AgentContext) {
				recordFailedToolCall(ctx, "replace_lines",
					json.RawMessage(`{"path":"a.py","start_line":1,"end_line":1,"content":"x"}`), "boom")
			},
			tool: "replace_lines", args: `{"path":"a.py","start_line":2,"end_line":2,"content":"x"}`,
		},
		{
			name: "a call whose own success cleared its failure",
			before: func(ctx *AgentContext) {
				args := json.RawMessage(`{"path":"a.py","start_line":1,"end_line":1,"content":"x"}`)
				recordFailedToolCall(ctx, "replace_lines", args, "stale range")
				clearFailedToolCall(ctx, "replace_lines", args)
			},
			tool: "replace_lines", args: `{"path":"a.py","start_line":1,"end_line":1,"content":"x"}`,
		},
		{
			name: "a test command after an edit succeeded",
			before: func(ctx *AgentContext) {
				recordFailedToolCall(ctx, "run_command",
					json.RawMessage(`{"command":"pytest test_store.py","timeout":30}`), "1 error in 0.12s")
				clearFailedToolCall(ctx, "edit_file", json.RawMessage(`{"path":"store.py"}`))
			},
			tool: "run_command", args: `{"command":"pytest test_store.py","timeout":30}`,
		},
		{
			name: "a read after an edit succeeded",
			before: func(ctx *AgentContext) {
				recordFailedToolCall(ctx, "read_file", json.RawMessage(`{"path":"a.py"}`), "nope")
				clearFailedToolCall(ctx, "edit_file", json.RawMessage(`{"path":"a.py"}`))
			},
			tool: "read_file", args: `{"path":"a.py"}`,
		},
		{
			// Polling a background job is meant to repeat byte for byte.
			name: "a poll of a background job",
			before: func(ctx *AgentContext) {
				recordFailedToolCall(ctx, "tail_background", json.RawMessage(`{"id":"job-1"}`), "no output yet")
			},
			tool: "tail_background", args: `{"id":"job-1"}`,
		},
		{
			name: "an edit after the read it was waiting for",
			before: func(ctx *AgentContext) {
				recordFailedToolCall(ctx, "edit_file", json.RawMessage(edit), "file not read yet — use read_file first")
				clearFailedToolCall(ctx, "read_file", json.RawMessage(`{"path":"t.py"}`))
			},
			tool: "edit_file", args: edit,
		},
		{
			name: "an edit re-sent before the read it was waiting for",
			before: func(ctx *AgentContext) {
				recordFailedToolCall(ctx, "edit_file", json.RawMessage(edit), "file not read yet — use read_file first")
			},
			tool: "edit_file", args: edit, wantRefused: true,
		},
		{
			name: "an identical edit with nothing in between",
			before: func(ctx *AgentContext) {
				recordFailedToolCall(ctx, "edit_file", json.RawMessage(edit), "old_str not found")
			},
			tool: "edit_file", args: edit, wantRefused: true,
		},
		{
			// A python3 -c line with mismatched quotes, re-sent unchanged.
			name: "an identical failing command with nothing in between",
			before: func(ctx *AgentContext) {
				recordFailedToolCall(ctx, "run_command",
					json.RawMessage(`{"command":"python3 -c \"print(f'x: {x}\")\"","timeout":30}`),
					"bash: -c: line 1: syntax error near unexpected token `)'")
			},
			tool: "run_command", args: `{"command":"python3 -c \"print(f'x: {x}\")\"","timeout":30}`,
			wantRefused: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := NewAgentContext(t.TempDir(), Tier2Medium)
			tc.before(ctx)
			refusal := identicalRetryRefusal(ctx, tc.tool, json.RawMessage(tc.args))
			if (refusal != "") != tc.wantRefused {
				t.Errorf("refused=%v, want %v: %s", refusal != "", tc.wantRefused, refusal)
			}
		})
	}
}

// A refused call skips the failure branch where the stopping rules live, so a
// model could be refused cheaply and forever. Three failures on one named
// file is the shared condition that stops it.
func TestStuckOnOnePathIsTheSharedBreakerCondition(t *testing.T) {
	if !stuckOnOnePath([]string{"todo.py", "todo.py", "todo.py"}) {
		t.Error("three failures on one file must count as stuck")
	}
	for _, paths := range [][]string{
		{"a.py", "b.py", "c.py"}, // grinding through multi-file work
		{"a.py", "a.py"},         // not yet three
		{"", "", ""},             // unnamed target proves nothing
		{"a.py", "a.py", "b.py"},
	} {
		if stuckOnOnePath(paths) {
			t.Errorf("wrongly reported stuck: %v", paths)
		}
	}
}

func TestRepeatedRefusalTellsTheUserRetryingWontHelp(t *testing.T) {
	msg := repeatedRefusalSummary("structural_edit", "todo.py", false)
	for _, want := range []string{"re-sent after being refused", "todo.py", "Nothing was written", "same wall"} {
		if !strings.Contains(msg, want) {
			t.Errorf("summary missing %q:\n%s", want, msg)
		}
	}
	if wrote := repeatedRefusalSummary("edit_file", "", true); !strings.Contains(wrote, "did land on disk") {
		t.Errorf("a run that wrote must say so:\n%s", wrote)
	}
}

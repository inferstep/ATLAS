package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

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

func TestRepeatDetectorCanonicalizesJSONFormatting(t *testing.T) {
	ctx := &AgentContext{}
	recordToolCall(ctx, "run_command", json.RawMessage(`{"command":"pytest","timeout":30}`))
	recordToolCall(ctx, "run_command", json.RawMessage(`{"timeout":30,"command":"pytest"}`))
	_, _, repeating := recordToolCall(ctx, "run_command", json.RawMessage(`{ "command" : "pytest", "timeout" : 30 }`))
	if !repeating {
		t.Fatal("key order / whitespace variations of the same call must match")
	}
}

func TestWriteFileReassertionKeyedOnPathAndContent(t *testing.T) {
	// The 2026-07-18 loop: the model reasserted the SAME app.py draft
	// while V3 wrote the verified expansion. Reassertion = same logical
	// content (whitespace/formatting aside) rewritten to the same path;
	// it must still fire at the threshold. (Materially different content
	// is iteration — TestWriteFileIterationNotRepeat — and must NOT fire.)
	ctx := &AgentContext{}
	for i := 0; i < 2; i++ {
		// Same code — only TRAILING whitespace/CR differs, which is noise and
		// must still collide. (Leading indentation is semantic in Python and
		// is deliberately NOT collapsed — TestWriteFileIndentationChangeIsIteration.)
		args := json.RawMessage(fmt.Sprintf(
			`{"path":"app.py","content":"from flask import Flask\napp = Flask(__name__)%s"}`,
			strings.Repeat(" ", i)))
		if _, _, repeating := recordToolCall(ctx, "write_file", args); repeating {
			t.Fatalf("fired at write %d, want threshold 3", i+1)
		}
	}
	msg, _, repeating := recordToolCall(ctx, "write_file",
		json.RawMessage(`{"path":"app.py","content":"from flask import Flask\napp = Flask(__name__)"}`))
	if !repeating {
		t.Fatal("reassertion of the same logical content must fire")
	}
	if !strings.Contains(msg, "app.py") || !strings.Contains(msg, "rewritten") {
		t.Fatalf("write-loop corrective should name the path and the rewrite pattern: %q", msg)
	}
}

func TestWriteFileDifferentPathsDoNotFire(t *testing.T) {
	ctx := &AgentContext{}
	for i, p := range []string{"app.py", "static/game.js", "templates/index.html"} {
		args := json.RawMessage(fmt.Sprintf(`{"path":"%s","content":"x"}`, p))
		if _, _, repeating := recordToolCall(ctx, "write_file", args); repeating {
			t.Fatalf("multi-file scaffolding flagged as a loop at write %d (%s)", i+1, p)
		}
	}
}

func TestEditFileKeepsFullArgsSignature(t *testing.T) {
	// Distinct surgical edits to one file in close succession are
	// legitimate iteration — only identical edits are a loop.
	ctx := &AgentContext{}
	for i := 0; i < 4; i++ {
		args := json.RawMessage(fmt.Sprintf(
			`{"path":"app.py","old_str":"v%d","new_str":"v%d"}`, i, i+1))
		if _, _, repeating := recordToolCall(ctx, "edit_file", args); repeating {
			t.Fatalf("distinct edits to one path flagged as a loop at edit %d", i+1)
		}
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

// Iteration must NOT be flagged as repetition: rewriting the same file
// with materially different content (fixing successive compiler errors)
// produces different signatures, so the detector stays silent. Regression
// for 2026-07-19 (a polyglot task killed mid-fix by the path-only key).
func TestWriteFileIterationNotRepeat(t *testing.T) {
	ctx := &AgentContext{}
	versions := []string{
		`{"path":"main.py.c","content":"int main(){ return 0; }"}`,
		`{"path":"main.py.c","content":"int main(){ printf(\"x\"); return 0; }"}`,
		`{"path":"main.py.c","content":"#include <stdio.h>\nint main(){ printf(\"x\"); return 0; }"}`,
	}
	for i, v := range versions {
		_, _, repeating := recordToolCall(ctx, "write_file", json.RawMessage(v))
		if repeating {
			t.Errorf("version %d: iteration flagged as repetition", i)
		}
	}
}

// Reassertion IS still caught: rewriting the same file with identical
// code — only trailing whitespace / line-ending noise differs — collides
// on the fingerprint and fires at the threshold. Protects the 2026-07-18 case.
func TestWriteFileReassertionStillCaught(t *testing.T) {
	ctx := &AgentContext{}
	// Same code + same leading indentation; only trailing whitespace/CR varies.
	versions := []string{
		`{"path":"app.py","content":"def f():\n    return 1"}`,
		`{"path":"app.py","content":"def f():\n    return 1  "}`,
		`{"path":"app.py","content":"def f():\r\n    return 1\r"}`,
	}
	fired := false
	for _, v := range versions {
		if _, _, r := recordToolCall(ctx, "write_file", json.RawMessage(v)); r {
			fired = true
		}
	}
	if !fired {
		t.Error("reassertion of the same logical content was not caught")
	}
}

// An indentation-only change is a REAL change in Python (iteration), so it
// must NOT collide as reassertion (#147 review finding #13).
func TestWriteFileIndentationChangeIsIteration(t *testing.T) {
	ctx := &AgentContext{}
	// A common fix: correcting a wrongly-indented body line. Different
	// leading indentation -> different fingerprint -> not flagged.
	versions := []string{
		`{"path":"m.py","content":"def f():\nreturn 1"}`,         // broken indent
		`{"path":"m.py","content":"def f():\n    return 1"}`,     // fixed (4)
		`{"path":"m.py","content":"def f():\n        return 1"}`, // 8-space
	}
	for i, v := range versions {
		if _, _, r := recordToolCall(ctx, "write_file", json.RawMessage(v)); r {
			t.Fatalf("indentation change at write %d flagged as reassertion", i+1)
		}
	}
}

// May 10 2026 BiasBusters #30 — locks the reasoning-repetition
// detector against regression. Prefix-match similarity over normalized
// reasoning openings; ≥2 consecutive identical openings triggers
// intervention. Single-turn repeats and prose-free turns must NOT fire.

func TestRecordReasoningTriggersOnConsecutiveRepeat(t *testing.T) {
	ctx := &AgentContext{}
	// Turn 1: first reasoning. No intervention.
	if msg, _, fired := recordReasoning(ctx, "Now I need to read the file to understand the structure."); fired || msg != "" {
		t.Fatalf("turn 1: expected no fire, got fired=%v msg=%q", fired, msg)
	}
	// Turn 2: same opening prefix. count=1 (not yet at threshold of 2).
	if msg, _, fired := recordReasoning(ctx, "Now I need to read the file to understand the structure."); fired || msg != "" {
		t.Fatalf("turn 2: expected no fire (count=1, threshold=2), got fired=%v msg=%q", fired, msg)
	}
	// Turn 3: same opening prefix again. count=2. FIRES.
	msg, obs, fired := recordReasoning(ctx, "Now I need to read the file to understand the structure.")
	if !fired {
		t.Fatalf("turn 3: expected intervention, got no fire")
	}
	if !strings.Contains(msg, "Reasoning repetition") {
		t.Errorf("intervention message missing canonical prefix: %s", msg)
	}
	if !strings.Contains(msg, "3 consecutive turns") {
		t.Errorf("intervention should report 3 consecutive turns, got: %s", msg)
	}
	// The observation is what the caller renders its log line and its
	// agent_reasoning_intervention payload from, so it must carry the
	// same count the message reports and the snippet that repeated.
	if obs.Count != 3 {
		t.Errorf("observation count = %d, want 3 consecutive turns", obs.Count)
	}
	if obs.Snippet != normalizeReasoningSnippet("Now I need to read the file to understand the structure.") {
		t.Errorf("observation snippet = %q, want the normalized repeated opening", obs.Snippet)
	}
	// The detector owns the reset. Reading the streak back off ctx now
	// sees the cleared values — which is exactly why the observation is
	// returned instead.
	if ctx.ConsecutiveReasoningRepeats != 0 || ctx.LastReasoningSnippet != "" {
		t.Errorf("firing must clear the streak; got repeats=%d snippet=%q",
			ctx.ConsecutiveReasoningRepeats, ctx.LastReasoningSnippet)
	}
	// A fourth identical turn starts over rather than re-firing the
	// same loop immediately.
	if _, _, again := recordReasoning(ctx, "Now I need to read the file to understand the structure."); again {
		t.Error("the turn right after a fire must start a fresh streak")
	}
}

func TestRecordReasoningResetOnDivergence(t *testing.T) {
	ctx := &AgentContext{}
	recordReasoning(ctx, "Now I need to read the file.")
	recordReasoning(ctx, "Now I need to read the file.")
	// Turn 3: model commits to a different thought — counter resets.
	if _, _, fired := recordReasoning(ctx, "I have the file content. Now let me write the new version."); fired {
		t.Error("divergent reasoning should reset the counter, no intervention expected")
	}
	if ctx.ConsecutiveReasoningRepeats != 0 {
		t.Errorf("counter should reset to 0 after divergence, got %d", ctx.ConsecutiveReasoningRepeats)
	}
	// Turn 4: similar to turn 3 (the new pattern). count=1.
	if _, _, fired := recordReasoning(ctx, "I have the file content. Now let me write the new version."); fired {
		t.Error("turn 4 should be count=1 (one repeat), no fire yet")
	}
	// Turn 5: third identical → FIRES.
	if _, _, fired := recordReasoning(ctx, "I have the file content. Now let me write the new version."); !fired {
		t.Error("turn 5 should fire (count=2 of new pattern)")
	}
}

func TestRecordReasoningIgnoresEmptyTurns(t *testing.T) {
	ctx := &AgentContext{}
	recordReasoning(ctx, "Now I need to read the file.")
	// Turn 2: empty reasoning (model committed straight to action). Counter resets.
	if _, _, fired := recordReasoning(ctx, ""); fired {
		t.Error("empty reasoning should not fire")
	}
	if ctx.ConsecutiveReasoningRepeats != 0 || ctx.LastReasoningSnippet != "" {
		t.Errorf("empty reasoning should reset state; got repeats=%d snippet=%q",
			ctx.ConsecutiveReasoningRepeats, ctx.LastReasoningSnippet)
	}
	// Turn 3: same prose as turn 1, but the empty turn 2 broke the streak.
	// Should be treated as a fresh start, not count=2.
	recordReasoning(ctx, "Now I need to read the file.")
	if ctx.ConsecutiveReasoningRepeats != 0 {
		t.Errorf("post-empty: counter should be 0 (fresh start), got %d", ctx.ConsecutiveReasoningRepeats)
	}
}

func TestRecordReasoningNormalizesWhitespace(t *testing.T) {
	ctx := &AgentContext{}
	recordReasoning(ctx, "  Now I  need\nto    read the file.\n")
	recordReasoning(ctx, "now i need to read the file.")
	// Both should normalize to the same prefix → count=1.
	if ctx.ConsecutiveReasoningRepeats != 1 {
		t.Errorf("normalized whitespace+case should match; got count=%d, snippet=%q",
			ctx.ConsecutiveReasoningRepeats, ctx.LastReasoningSnippet)
	}
}

func TestRecordReasoningRespectsPrefixLength(t *testing.T) {
	// Two reasonings that share the first 80 chars but diverge later
	// should still match — that's the design (we want the OPENING to
	// be the signal). Let me confirm the prefix-match behavior.
	a := "Looking at the existing dashboard.html, I see the basic Flask template that needs to be transformed into a metrics view."
	b := "Looking at the existing dashboard.html, I see the basic Flask template that needs to be expanded with three KPI cards."
	ctx := &AgentContext{}
	recordReasoning(ctx, a)
	recordReasoning(ctx, b)
	// First 80 chars match → count should advance.
	if ctx.ConsecutiveReasoningRepeats == 0 {
		t.Errorf("expected prefix match to advance counter; got count=0, snippet=%q",
			ctx.LastReasoningSnippet)
	}
}

func TestRecordReasoningDoesNotFireOnSingleRepeat(t *testing.T) {
	ctx := &AgentContext{}
	recordReasoning(ctx, "Looking at the file...")
	if _, _, fired := recordReasoning(ctx, "Looking at the file..."); fired {
		t.Error("single repeat (turn 2 = turn 1) should not fire — needs 2 consecutive repeats")
	}
}

func TestNormalizeReasoningSnippet(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"   \n  ", ""},
		{"Hello World", "hello world"},
		{"  HELLO\n\tWORLD  ", "hello world"},
		{strings.Repeat("a", 200), strings.Repeat("a", 80)},
	}
	for _, tc := range cases {
		if got := normalizeReasoningSnippet(tc.in); got != tc.want {
			t.Errorf("normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The uninstalled-dependency loop: `python3 -m flask run` fails because flask
// isn't in the sandbox. The steer must name the package and tell it to install.
func TestMissingModuleSteerPythonDashM(t *testing.T) {
	dir := t.TempDir()
	ctx := &AgentContext{WorkingDir: dir}
	out := "/usr/local/bin/python3: No module named flask\n"
	steer := missingModuleSteer(ctx, out)
	if !strings.Contains(steer, "flask") || !strings.Contains(steer, "pip install") {
		t.Errorf("expected install steer naming flask, got: %q", steer)
	}
}

// ModuleNotFoundError (import form), and a top-level package is extracted from
// a dotted submodule.
func TestMissingModuleSteerImportForm(t *testing.T) {
	dir := t.TempDir()
	ctx := &AgentContext{WorkingDir: dir}
	out := "ModuleNotFoundError: No module named 'flask.cli'\n"
	steer := missingModuleSteer(ctx, out)
	if !strings.Contains(steer, "pip install flask") {
		t.Errorf("expected `pip install flask` (top-level pkg), got: %q", steer)
	}
}

// When a requirements.txt exists, prefer installing the whole manifest.
func TestMissingModuleSteerPrefersRequirements(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("flask\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ctx := &AgentContext{WorkingDir: dir}
	out := "No module named flask\n"
	steer := missingModuleSteer(ctx, out)
	if !strings.Contains(steer, "pip install -r requirements.txt") {
		t.Errorf("expected requirements.txt steer, got: %q", steer)
	}
}

func TestMissingModuleSteerNoModuleError(t *testing.T) {
	ctx := &AgentContext{WorkingDir: t.TempDir()}
	if s := missingModuleSteer(ctx, "Total: 42\n"); s != "" {
		t.Errorf("expected empty steer for unrelated output, got: %q", s)
	}
}

// The case-typo loop: ran `pip install -r Requirements.txt` while the real
// file is `requirements.txt`. The steer must name the actual file.
func TestMissingFileSteerCaseMismatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("flask\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ctx := &AgentContext{WorkingDir: dir}
	out := "ERROR: Could not open requirements file: [Errno 2] No such file or directory: 'Requirements.txt'\n"
	steer := missingFileSteer(ctx, out)
	if !strings.Contains(steer, "requirements.txt") || !strings.Contains(steer, "case") {
		t.Errorf("expected case-mismatch steer naming requirements.txt, got: %q", steer)
	}
}

// A genuinely absent file (no case-variant) must NOT produce a steer — we
// never invent an anchor for a file that doesn't exist.
func TestMissingFileSteerNoVariant(t *testing.T) {
	dir := t.TempDir()
	ctx := &AgentContext{WorkingDir: dir}
	out := "cat: nope.txt: No such file or directory\n"
	if s := missingFileSteer(ctx, out); s != "" {
		t.Errorf("expected no steer when no case-variant exists, got: %q", s)
	}
}

// Shell-style error (filename before the colon) is also recognized.
func TestMissingFileSteerShellShape(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print(1)\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ctx := &AgentContext{WorkingDir: dir}
	out := "python: Main.py: No such file or directory\n"
	steer := missingFileSteer(ctx, out)
	if !strings.Contains(steer, "main.py") {
		t.Errorf("expected steer naming main.py, got: %q", steer)
	}
}

func TestTracebackSteerNamesFixSite(t *testing.T) {
	ctx := &AgentContext{WorkingDir: "/workspace"}
	out := "Traceback (most recent call last):\n" +
		"  File \"/workspace/_agenttest/app.py\", line 14, in get_item\n" +
		"    return jsonify(items[item_id + 1])\n" +
		"IndexError: list index out of range\n"
	steer := tracebackSteer(ctx, out)
	for _, want := range []string{"get_item", "line 14", "IndexError", "function:get_item"} {
		if !strings.Contains(steer, want) {
			t.Errorf("steer missing %q:\n%s", want, steer)
		}
	}
}

func TestTracebackSteerNoTraceback(t *testing.T) {
	ctx := &AgentContext{WorkingDir: "/workspace"}
	if s := tracebackSteer(ctx, "Total inventory value: $237\n"); s != "" {
		t.Errorf("expected empty steer for non-traceback output, got: %s", s)
	}
}

// Environment errors (missing package) aren't code-localization targets —
// steering/banning would loop on an unfixable import.
func TestTracebackSteerSkipsModuleNotFound(t *testing.T) {
	ctx := &AgentContext{WorkingDir: "/workspace"}
	out := "Traceback (most recent call last):\n" +
		"  File \"/workspace/snake_game.py\", line 1, in <module>\n" +
		"    import pygame\n" +
		"ModuleNotFoundError: No module named 'pygame'\n"
	if s := tracebackSteer(ctx, out); s != "" {
		t.Errorf("should not steer on ModuleNotFoundError, got: %s", s)
	}
}

// The deepest frame is usually stdlib; the fix site is the deepest PROJECT
// frame (the user line that called into the library).
func TestTracebackSteerSkipsStdlib(t *testing.T) {
	ctx := &AgentContext{WorkingDir: "/workspace"}
	out := "Traceback (most recent call last):\n" +
		"  File \"/workspace/app.py\", line 5, in main\n" +
		"    data = json.loads(raw)\n" +
		"  File \"/usr/lib/python3.9/json/__init__.py\", line 346, in loads\n" +
		"    return _default_decoder.decode(s)\n" +
		"ValueError: Expecting value\n"
	steer := tracebackSteer(ctx, out)
	if !strings.Contains(steer, "app.py") || !strings.Contains(steer, "function:main") {
		t.Errorf("should pick project frame app.py:main, got: %s", steer)
	}
	if strings.Contains(steer, "json/__init__") {
		t.Errorf("should NOT point at stdlib, got: %s", steer)
	}
}

// The missing-binary loop (observed 2026-07-18): `git clone ...` in a
// sandbox without git. The steer must name the binary, state that
// apt-get can't work (non-root, read-only), and point at alternatives.
func TestMissingCommandSteerBashForm(t *testing.T) {
	out := "bash: line 1: git: command not found\n"
	steer := missingCommandSteer(out)
	if !strings.Contains(steer, "`git`") || !strings.Contains(steer, "CANNOT be installed") {
		t.Errorf("expected missing-command steer naming git, got: %q", steer)
	}
	if strings.Contains(steer, "apt-get install") {
		t.Errorf("steer must not suggest apt-get install (impossible in sandbox): %q", steer)
	}
}

// dash/sh abbreviates: "sh: 1: sqlite3: not found".
func TestMissingCommandSteerShForm(t *testing.T) {
	out := "sh: 1: sqlite3: not found\n"
	steer := missingCommandSteer(out)
	if !strings.Contains(steer, "`sqlite3`") {
		t.Errorf("expected steer naming sqlite3, got: %q", steer)
	}
}

// A full path is reduced to its basename.
func TestMissingCommandSteerPathBasename(t *testing.T) {
	out := "bash: line 3: /usr/local/bin/terraform: command not found\n"
	steer := missingCommandSteer(out)
	if !strings.Contains(steer, "`terraform`") {
		t.Errorf("expected basename terraform, got: %q", steer)
	}
}

// Bare "<name>: not found" without an sh prefix must NOT fire — program
// output legitimately prints "config.yaml: not found" shapes.
func TestMissingCommandSteerNoFalsePositive(t *testing.T) {
	if s := missingCommandSteer("config.yaml: not found\n"); s != "" {
		t.Errorf("expected no steer for non-shell not-found line, got: %q", s)
	}
	if s := missingCommandSteer("all tests passed\n"); s != "" {
		t.Errorf("expected no steer for clean output, got: %q", s)
	}
}

// The broken-verification-command loop (observed 2026-07-19, regex-chess): the
// model verifies with `python3 -c "...; def f(): ..."` — a multi-statement
// script that can't parse on a -c line — and the SyntaxError is in the
// command, not the solution. Steer must move the test to a file.
func TestBrokenInlineScriptSteerFires(t *testing.T) {
	cmd := `python3 -c "import json, re; def all_legal_next_positions(fen): return []"`
	out := "  File \"<string>\", line 1\n    import json, re; def all_legal\n                     ^\nSyntaxError: invalid syntax"
	steer := brokenInlineScriptSteer(cmd, out)
	if steer == "" || !strings.Contains(steer, "inline `-c`") || !strings.Contains(steer, ".py") {
		t.Errorf("expected broken-inline-script steer, got: %q", steer)
	}
}

// A syntax error in a REAL file (not a -c one-liner) must NOT match — that's
// a solution bug tracebackSteer localizes, not a broken verify command.
func TestBrokenInlineScriptSteerIgnoresRealFile(t *testing.T) {
	cmd := `python3 solution.py`
	out := "  File \"solution.py\", line 12\n    def f(\n         ^\nSyntaxError: invalid syntax"
	if s := brokenInlineScriptSteer(cmd, out); s != "" {
		t.Errorf("expected no steer for a real-file syntax error, got: %q", s)
	}
}

// No SyntaxError at all → no steer.
func TestBrokenInlineScriptSteerNoSyntaxError(t *testing.T) {
	if s := brokenInlineScriptSteer(`python3 -c "print(1)"`, "1\n"); s != "" {
		t.Errorf("expected no steer on clean output, got: %q", s)
	}
}

// Truncation robustness: the sandbox clipped the output before the
// "SyntaxError:" line, leaving only the "<string>" frame. The steer must
// still fire (2026-07-19 regression — the keyword gate missed this).
func TestBrokenInlineScriptSteerTruncatedOutput(t *testing.T) {
	cmd := `python3 -c "import json, re; def all_legal(fen): return []"`
	out := `  File "<string>", line 1` + "\n" + `    import json, re; def all_legal(fen): return []`
	if s := brokenInlineScriptSteer(cmd, out); s == "" {
		t.Error("steer must fire on a <string> frame even when SyntaxError is truncated away")
	}
}

// #147 review #9: the bash command-not-found steer must require a real bash
// diagnostic prefix, not fire on the phrase in ordinary program output.
func TestMissingCommandSteerRequiresShellPrefix(t *testing.T) {
	if s := missingCommandSteer("bash: line 1: git: command not found\n"); s == "" {
		t.Error("real bash diagnostic must fire")
	}
	if s := missingCommandSteer("bash: git: command not found\n"); s == "" {
		t.Error("bash diagnostic without line-number must fire")
	}
	// Program output that merely prints the phrase must NOT fire.
	if s := missingCommandSteer(`print("mytool: command not found")` + "\nmytool: command not found\n"); s != "" {
		t.Errorf("must not fire on program output: %q", s)
	}
}

// #147 review #11: don't misfire on `python -c "exec(open('f').read())"` —
// the SyntaxError is in file f, not the one-liner.
func TestBrokenInlineScriptSteerSkipsExec(t *testing.T) {
	cmd := `python3 -c "exec(open('solution.py').read())"`
	out := "  File \"<string>\", line 1\n    def broken(\nSyntaxError: invalid syntax"
	if s := brokenInlineScriptSteer(cmd, out); s != "" {
		t.Errorf("must not fire when -c execs external code: %q", s)
	}
}

// The live failure: three structural_edit calls on function:index in a Flask
// app whose HTML/JS template is a module-level constant. index() is two lines
// and holds none of the JavaScript being fixed, so every attempt was doomed —
// but each carried a different body, so a whole-args signature saw three
// distinct calls and the repeat detector stayed silent until the failure cap
// killed the turn.
func TestStructuralEditRepeatsKeyOnSelectorNotContent(t *testing.T) {
	ctx := &AgentContext{}
	args := func(content string) json.RawMessage {
		return json.RawMessage(`{"path":"app.py","selector":"function:index","content":` +
			mustJSONString(content) + `}`)
	}

	var msg string
	var fired bool
	for i, body := range []string{"first attempt", "second, rewritten", "third, different again"} {
		msg, _, fired = recordToolCall(ctx, "structural_edit", args(body))
		if fired {
			if i == 0 {
				t.Fatal("fired on the first call — that is not a repeat")
			}
			break
		}
	}
	if !fired {
		t.Fatal("three attempts at the same selector never registered as repetition")
	}
	// The message has to name the selector as the cause, or the model reads it
	// as "try again differently" and rewrites the body a fourth time.
	for _, want := range []string{"function:index", "SELECTOR", "edit_file", "insert_after"} {
		if !strings.Contains(msg, want) {
			t.Errorf("escalation missing %q:\n%s", want, msg)
		}
	}
}

// A different selector is a different attempt and must not be conflated.
func TestStructuralEditDifferentSelectorsAreNotRepeats(t *testing.T) {
	ctx := &AgentContext{}
	for _, sel := range []string{"function:a", "function:b", "function:c", "class:D"} {
		args := json.RawMessage(`{"path":"app.py","selector":"` + sel + `","content":"x"}`)
		if _, _, fired := recordToolCall(ctx, "structural_edit", args); fired {
			t.Fatalf("distinct selector %q was treated as a repeat", sel)
		}
	}
}

func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// Run 9 (2026-08-02) emitted the same replace_lines call on turns 2 and 3
// against a rejection that named the file, the line, the cause and two
// concrete fixes. The repetition detector needs three occurrences in its
// window and only steers the NEXT turn, so an identical pair never reached
// it and the run died on the three-strike breaker with the file untouched.
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
	// The original rejection has to come back with it — the model needs the
	// reason, not just the fact that it repeated itself.
	if !strings.Contains(refusal, "stops a render loop") {
		t.Errorf("refusal drops the original reason:\n%s", refusal)
	}
	if !strings.Contains(refusal, "read_file") {
		t.Errorf("refusal gives no way forward:\n%s", refusal)
	}
}

func TestTheRefusalIsScopedToCallsThatFailed(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	read := json.RawMessage(`{"path":"app.py"}`)

	// Re-reading a file after editing it is byte-identical and correct.
	if refusal := identicalRetryRefusal(ctx, "read_file", read); refusal != "" {
		t.Errorf("a repeated successful call was refused: %s", refusal)
	}
	// A different call to the same tool is not the same call.
	args := json.RawMessage(`{"path":"a.py","start_line":1,"end_line":1,"content":"x"}`)
	recordFailedToolCall(ctx, "replace_lines", args, "boom")
	other := json.RawMessage(`{"path":"a.py","start_line":2,"end_line":2,"content":"x"}`)
	if refusal := identicalRetryRefusal(ctx, "replace_lines", other); refusal != "" {
		t.Errorf("a different call was refused: %s", refusal)
	}
}

func TestASucceedingCallClearsItsOwnRejection(t *testing.T) {
	// An edit rejected for a stale range works after a re-read. The memory
	// must not outlive the condition that caused it.
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	args := json.RawMessage(`{"path":"a.py","start_line":1,"end_line":1,"content":"x"}`)
	recordFailedToolCall(ctx, "replace_lines", args, "stale range")
	clearFailedToolCall(ctx, "replace_lines", args)
	if refusal := identicalRetryRefusal(ctx, "replace_lines", args); refusal != "" {
		t.Errorf("rejection outlived the failure: %s", refusal)
	}
}

// The refusal path incremented consecutiveErrors and appended the failure
// path, then returned — while both stopping rules that read those live inside
// the post-execution failure branch it skips. Observed: four consecutive
// refusals of the same structural_edit in one run, no breaker, no ceiling; the
// model was refused cheaply and forever.
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

// Measured on multifile_cli rep 2. Two ways the refusal blocked correct work,
// both because "nothing about the workspace has changed since" was false.
func TestTheRetryRefusalDoesNotBlockLegitimateRetries(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)

	// 1. Re-running a verification command after fixing the code IS the
	// loop. The fix succeeds, which drops every remembered rejection.
	cmd := json.RawMessage(`{"command":"pytest test_store.py","timeout":30}`)
	recordFailedToolCall(ctx, "run_command", cmd, "1 error in 0.12s")
	clearFailedToolCall(ctx, "edit_file", json.RawMessage(`{"path":"store.py"}`))
	if r := identicalRetryRefusal(ctx, "run_command", cmd); r != "" {
		t.Errorf("re-running a test after a fix was refused:\n%s", r)
	}
	// Re-reading after an edit is correct for the same reason.
	rd := json.RawMessage(`{"path":"a.py"}`)
	recordFailedToolCall(ctx, "read_file", rd, "nope")
	clearFailedToolCall(ctx, "edit_file", json.RawMessage(`{"path":"a.py"}`))
	if r := identicalRetryRefusal(ctx, "read_file", rd); r != "" {
		t.Errorf("re-reading was refused:\n%s", r)
	}

	// But a command that failed and changed nothing must not be re-sent.
	// Measured on multiturn_stats: one `python3 -c` with mismatched
	// quotes, re-sent seven times across six turns.
	ctxCmd := NewAgentContext(t.TempDir(), Tier2Medium)
	broken := json.RawMessage(`{"command":"python3 -c \"print(f'x: {x}\")\"","timeout":30}`)
	recordFailedToolCall(ctxCmd, "run_command", broken,
		"bash: -c: line 1: syntax error near unexpected token `)'")
	if identicalRetryRefusal(ctxCmd, "run_command", broken) == "" {
		t.Error("an identical failing command with nothing in between was allowed")
	}

	// Polling a background job is meant to repeat byte-for-byte.
	ctxPoll := NewAgentContext(t.TempDir(), Tier2Medium)
	poll := json.RawMessage(`{"id":"job-1"}`)
	recordFailedToolCall(ctxPoll, "tail_background", poll, "no output yet")
	if r := identicalRetryRefusal(ctxPoll, "tail_background", poll); r != "" {
		t.Errorf("polling a background job was refused:\n%s", r)
	}

	// 2. A precondition failure resolved by another call must not linger.
	ctx2 := NewAgentContext(t.TempDir(), Tier2Medium)
	edit := json.RawMessage(`{"path":"t.py","old_str":"@Pytest.fixture","new_str":"@pytest.fixture"}`)
	recordFailedToolCall(ctx2, "edit_file", edit, "file not read yet — use read_file first")
	if identicalRetryRefusal(ctx2, "edit_file", edit) == "" {
		t.Fatal("the immediate re-send should still be refused")
	}
	// The model reads the file — the right response — which satisfies it.
	clearFailedToolCall(ctx2, "read_file", json.RawMessage(`{"path":"t.py"}`))
	if r := identicalRetryRefusal(ctx2, "edit_file", edit); r != "" {
		t.Errorf("the edit was still refused after its precondition was met:\n%s", r)
	}

	// A genuinely repeated edit with nothing in between is still refused.
	ctx3 := NewAgentContext(t.TempDir(), Tier2Medium)
	recordFailedToolCall(ctx3, "edit_file", edit, "old_str not found")
	if identicalRetryRefusal(ctx3, "edit_file", edit) == "" {
		t.Error("an identical re-send with no intervening success was allowed")
	}
}

// A productive change earlier in the run does not mean the deliverable is
// good now. Measured on the seed-20260901 confirmation, task debounce5: an
// accepted write set madeProductiveChange, the model then repeated a failing
// verification command, and the repeat detector terminated with "Made your
// change ... the change is on disk; run it yourself to confirm." The bytes on
// disk were a SyntaxError, and the final write never executed. One false
// success in 50 sessions, and the only terminal in the run that misreported
// its own outcome.
//
// madeProductiveChange is a progress hint. It may describe what happened; it
// may never authorize a completion claim.
func TestProductiveChangeCannotAuthorizeCompletionOverInvalidBytes(t *testing.T) {
	dir := t.TempDir()
	deliverable := filepath.Join(dir, "solve.py")
	if err := os.WriteFile(deliverable, []byte("def solve():\n    return [1, 2]]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier2Medium)

	summary := repeatTerminalSummary(ctx, []string{"solve.py"}, true, nil)
	if strings.Contains(summary, "Made your change") {
		t.Errorf("a syntax-invalid deliverable must not be reported as a completed change:\n%s", summary)
	}
	if !strings.HasPrefix(summary, "Stopped:") {
		t.Errorf("terminal must be an honest stop, got:\n%s", summary)
	}
}

func TestProductiveChangeStillReportsAStopWhenValidityIsUnknown(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	// No deliverable on disk at all: validity is not demonstrated, so the
	// terminal may not claim the change landed.
	summary := repeatTerminalSummary(ctx, []string{"missing.py"}, true, nil)
	if !strings.HasPrefix(summary, "Stopped:") {
		t.Errorf("unknown validity must stop honestly, got:\n%s", summary)
	}
	if strings.Contains(summary, "Made your change") {
		t.Errorf("existence is not validity:\n%s", summary)
	}
}

func TestNoDeclaredDeliverableCannotAuthorizeCompletion(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	if s := repeatTerminalSummary(ctx, nil, true, nil); !strings.HasPrefix(s, "Stopped:") {
		t.Errorf("with nothing declared, validity is undemonstrated:\n%s", s)
	}
}

// syntaxStub answers the whole-file syntax check the terminal consults.
// Without it every observation is not_run, which is fail-closed and correct
// but cannot exercise the passed branch.
func syntaxStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/syntax-check") {
			http.NotFound(w, r)
			return
		}
		var in struct{ Code string }
		json.NewDecoder(r.Body).Decode(&in)
		valid := !strings.Contains(in.Code, "[1,") && !strings.Contains(in.Code, "]]")
		out := map[string]interface{}{"valid": valid}
		if !valid {
			out["errors"] = []string{"SyntaxError: invalid syntax"}
		}
		json.NewEncoder(w).Encode(out)
	}))
}

// Syntax is not task completion. A repeat-breaker is an operational failure
// however good the bytes look, so a demonstrably valid deliverable changes
// what the terminal DISCLOSES and never whether it claims success.
func TestValidBytesStillTerminateAsStopped(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "solve.py"),
		[]byte("def solve():\n    return 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := syntaxStub(t)
	defer srv.Close()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.SandboxURL = srv.URL
	summary := repeatTerminalSummary(ctx, []string{"solve.py"}, true, nil)
	if !strings.HasPrefix(summary, "Stopped:") {
		t.Errorf("a repeat-breaker is an operational failure even with valid "+
			"bytes:\n%s", summary)
	}
	if strings.Contains(summary, "Made your change") {
		t.Errorf("no branch may claim completion:\n%s", summary)
	}
	if !strings.Contains(summary, "parses") ||
		!strings.Contains(summary, "verification did not complete") {
		t.Errorf("validity should change the disclosure:\n%s", summary)
	}
}

// Disclosure differs by validation status; completion never appears.
func TestEveryTerminalDisclosureRefusesCompletion(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "ok.py")
	os.WriteFile(valid, []byte("x = 1\n"), 0o644)
	invalid := filepath.Join(dir, "bad.py")
	os.WriteFile(invalid, []byte("x = [1,\n"), 0o644)
	srv := syntaxStub(t)
	defer srv.Close()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.SandboxURL = srv.URL

	for _, tc := range []struct {
		name     string
		expected []string
		wrote    bool
		want     string
	}{
		{"passed", []string{"ok.py"}, true, "parses"},
		{"failed", []string{"bad.py"}, true, "not shown to be valid"},
		{"unreadable", []string{"gone.py"}, true, "not shown to be valid"},
		{"none declared", nil, true, "not shown to be valid"},
		{"nothing written", []string{"ok.py"}, false, "nothing was written"},
	} {
		got := repeatTerminalSummary(ctx, tc.expected, tc.wrote, nil)
		if !strings.HasPrefix(got, "Stopped:") {
			t.Errorf("%s: not an honest stop:\n%s", tc.name, got)
		}
		if strings.Contains(got, "Made your change") {
			t.Errorf("%s: completion claim:\n%s", tc.name, got)
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: missing disclosure %q:\n%s", tc.name, tc.want, got)
		}
	}
}

// The model's own `done` is untouched: it is not a breaker terminal and this
// change must not alter ordinary clean completion.
func TestModelIssuedDoneIsUnchanged(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	// The model-issued done path streams the model's own summary verbatim.
	if !strings.Contains(body, `case "done":`) {
		t.Fatal("model-issued done branch is gone")
	}
	// repeatTerminalSummary is reachable from exactly one call site: the
	// repeat detector's terminal. It must not have leaked into the ordinary
	// completion path.
	if n := strings.Count(body, "repeatTerminalSummary("); n != 1 {
		t.Errorf("repeatTerminalSummary has %d call sites, want exactly 1", n)
	}
}

// --- production-path reproduction of debounce5 ------------------------------
//
// Reconstructed from the retained raw events of the seed-20260901 run, not
// guessed. The recorded shape: every write_file SUCCEEDS (1181, 1180, 1181,
// 1182, 1183, 1181, 1183 bytes — the model rewrites the same file with
// slightly different content each turn), the runaway-write backstop fires
// twice with "you have fully rewritten solve.py N times", and the second
// detection reaches the terminal. Nothing is ever refused, which is why the
// refusal-ban terminal never engages — an earlier fixture built on rejected
// calls reached that wrong branch and had to be rebuilt from the trace.
//
// The deliverable stays syntactically invalid throughout: the first write
// creates it (a new file has no healthy prior state, so it lands with a parse
// warning) and every later write is broken->broken, which the healthy->broken
// policy allows as repair-in-progress.
//
// Deliberately free of production symbols added by the fix, so the same
// fixture runs against the parent commit.

const debounce5Broken = "def solve():\n    return [1, 2]]\n"

func debounce5Stubs(t *testing.T, dir, rel string, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v3/") || strings.HasPrefix(r.URL.Path, "/internal/") {
			http.Error(w, "v3 unavailable in this test", http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/syntax-check") {
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			valid := !strings.Contains(in.Code, "]]")
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{"SyntaxError: unmatched ']' (line 2)"}
			}
			json.NewEncoder(w).Encode(out)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/execute") {
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "stderr": "", "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "stderr": "", "exit_code": 0})
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.NotFound(w, r)
			return
		}
		i := *calls
		*calls++
		// write_file signatures carry a CONTENT fingerprint, so novel content
		// every turn is never "repetition". The recorded run alternated a
		// small set of near-identical rewrites -- byte counts 1181, 1180,
		// 1181, 1182, 1183, 1181, 1183, with 1181 recurring three times --
		// which is the model re-emitting the same broken file from memory.
		// Two variants reproduce that: one recurs three times inside the
		// eight-call window and the detector fires, twice.
		variant := "# a\n" + debounce5Broken
		if i%2 == 1 {
			variant = "# b\n" + debounce5Broken
		}
		body, _ := json.Marshal(map[string]interface{}{
			"type": "tool_call", "name": "write_file",
			"args": map[string]string{"path": rel, "content": variant},
		})
		w.Header().Set("Content-Type", "text/event-stream")
		delta, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(body)}}},
		})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", delta)
	}))
}

func TestDebounce5ReachesTheRepeatDetectorTerminal(t *testing.T) {
	dir := t.TempDir()
	rel := "solve.py"
	calls := 0
	srv := debounce5Stubs(t, dir, rel, &calls)
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.V3URL = srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.MaxTurns = 40

	var summary string
	interventions, acceptedWrites, banEntries, rescues := 0, 0, 0, 0
	ctx.StreamFn = func(eventType string, data interface{}) {
		b, _ := json.Marshal(data)
		switch eventType {
		case "agent_repeat_intervention":
			interventions++
		case "tool_result":
			if strings.Contains(string(b), `"success":true`) &&
				strings.Contains(string(b), "bytes_written") {
				acceptedWrites++
			}
		case "gate":
			if strings.Contains(string(b), "no longer available") {
				banEntries++
			}
		case "done":
			summary = string(b)
		}
		if strings.Contains(string(b), "named deliverable") ||
			strings.Contains(string(b), "was never created") {
			rescues++
		}
	}
	if err := runAgentLoop(ctx, "Create solve.py that solves the task."); err != nil {
		t.Fatalf("agent loop error: %v", err)
	}

	// --- routing premises: this run reached the repeat-DETECTOR terminal ---
	if interventions < 2 {
		t.Fatalf("second-detection condition not reached: %d repeat interventions", interventions)
	}
	if acceptedWrites == 0 {
		t.Fatal("no write landed, so the productive-change hint was never set")
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("expected output missing, so output-rescue would have fired: %v", err)
	}
	if rescues > 0 {
		t.Fatalf("output-rescue fired (%d); this is not the terminal under test", rescues)
	}
	if banEntries > 0 {
		t.Fatalf("refusal-ban fired (%d); wrong terminal", banEntries)
	}
	if strings.Contains(summary, "re-sent after being refused") {
		t.Fatalf("terminal came from the refusal ban, not the repeat detector:\n%s", summary)
	}
	if !strings.Contains(summary, "kept repeating") && !strings.Contains(summary, "Made your change") {
		t.Fatalf("terminal did not come from the repeat-detector call site:\n%s", summary)
	}

	// --- the behaviour under test ---
	if strings.Contains(summary, "Made your change") {
		t.Errorf("completion claimed over invalid bytes:\n%s", summary)
	}
	if !strings.Contains(summary, "Stopped:") {
		t.Errorf("terminal must be an honest stop:\n%s", summary)
	}
	after, _ := os.ReadFile(filepath.Join(dir, rel))
	if string(after) != string(onDisk) {
		t.Errorf("termination must not rewrite disk")
	}
	if !strings.Contains(string(after), "]]") {
		t.Errorf("fixture no longer leaves an invalid deliverable: %q", string(after))
	}
	t.Logf("interventions=%d accepted_writes=%d summary=%s", interventions, acceptedWrites, summary)
}

// --- Phase 3B: the canonical restoration scenario ---------------------------
//
// The shape the ledger was built for. A version of the deliverable is written
// and shown to parse; a shell command then rewrites those exact bytes into
// something that does not; the model loops on a verification that keeps
// failing and the repeat detector stops the run. The parent leaves the broken
// bytes on disk and says so; the current build puts the last version shown to
// be valid back and still stops.
//
// Deliberately free of production symbols added by Phase 3B, so the same
// fixture runs against the parent commit.

const restoreGood = "def solve():\n    return [1, 2]\n"

func restoreScenarioStubs(t *testing.T, dir, rel string, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v3/") || strings.HasPrefix(r.URL.Path, "/internal/") {
			http.Error(w, "v3 unavailable in this test", http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/syntax-check") {
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			valid := !strings.Contains(in.Code, "]]")
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{"SyntaxError: unmatched ']' (line 2)"}
			}
			json.NewEncoder(w).Encode(out)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/execute") {
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "stderr": "", "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "stderr": "", "exit_code": 0})
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.NotFound(w, r)
			return
		}
		i := *calls
		*calls++

		var body []byte
		switch i {
		case 0:
			body, _ = json.Marshal(map[string]interface{}{
				"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": rel, "content": restoreGood}})
		case 1:
			// The corruption comes from OUTSIDE the edit tools, which is the
			// case no gate can catch: a command rewrites the file after it
			// was shown to parse.
			body, _ = json.Marshal(map[string]interface{}{
				"type": "tool_call", "name": "run_command",
				"args": map[string]string{
					"command": "printf 'def solve():\\n    return [1, 2]]\\n' > " + rel}})
		default:
			// The model then loops on the same verification, which is what
			// stops the run.
			body, _ = json.Marshal(map[string]interface{}{
				"type": "tool_call", "name": "run_command",
				"args": map[string]string{"command": "test -s " + rel}})
		}
		w.Header().Set("Content-Type", "text/event-stream")
		delta, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(body)}}},
		})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", delta)
	}))
}

func TestCorruptedDeliverableIsRestoredAtTheRepeatTerminal(t *testing.T) {
	dir := t.TempDir()
	rel := "solve.py"
	calls := 0
	srv := restoreScenarioStubs(t, dir, rel, &calls)
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.V3URL = srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.MaxTurns = 40

	var summary string
	interventions := 0
	census := map[string]int{}
	ctx.StreamFn = func(eventType string, data interface{}) {
		b, _ := json.Marshal(data)
		census[eventType]++
		switch eventType {
		case "agent_repeat_intervention":
			interventions++
		case "done":
			summary = string(b)
		}
	}
	if err := runAgentLoop(ctx, "Create solve.py that solves the task."); err != nil {
		t.Fatalf("agent loop error: %v", err)
	}

	// --- routing premises -------------------------------------------------
	if interventions < 2 {
		t.Fatalf("second-detection condition not reached: %d repeat interventions", interventions)
	}
	if !strings.Contains(summary, "kept repeating") {
		t.Fatalf("terminal did not come from the repeat detector:\n%s", summary)
	}

	// --- the behaviour under test ----------------------------------------
	onDisk, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("deliverable missing: %v", err)
	}
	t.Logf("final bytes: %q", string(onDisk))
	t.Logf("summary: %s", summary)
	if string(onDisk) != restoreGood {
		t.Errorf("the demonstrated-broken bytes were left on disk:\n%q", string(onDisk))
	}
	if !strings.Contains(summary, "Put back the last version shown to be valid") {
		t.Errorf("recovery was not disclosed:\n%s", summary)
	}
	if !strings.Contains(summary, rel) {
		t.Errorf("the disclosure does not name the file it recovered:\n%s", summary)
	}
	// Recovery is not completion, in the prose and in the contract.
	var term struct{ Summary, Status, Reason string }
	if err := json.Unmarshal([]byte(summary), &term); err != nil {
		t.Fatalf("terminal payload is not decodable: %v", err)
	}
	if !strings.HasPrefix(term.Summary, "Stopped:") {
		t.Errorf("terminal stopped being a stop:\n%s", term.Summary)
	}
	if term.Status != "stopped" {
		t.Errorf("a restored run reported status %q", term.Status)
	}
	if term.Reason != "repeat_detector" {
		t.Errorf("terminal reason = %q", term.Reason)
	}
	for _, claim := range []string{"Made your change", "the change is on disk"} {
		if strings.Contains(summary, claim) {
			t.Errorf("a restored run claimed %q:\n%s", claim, summary)
		}
	}
	// The completion clause must still be the REFUSAL of completion. After a
	// restore the file does parse again, so this is the branch that is most
	// tempting to misread as success.
	if !strings.Contains(summary, "cannot say the task is done") {
		t.Errorf("the terminal stopped refusing completion:\n%s", summary)
	}
	// Never presented as a transaction.
	if strings.Contains(summary, "rolled back the workspace") {
		t.Errorf("recovery implied transactionality:\n%s", summary)
	}
	// Recovery is not a tool call. The event census is logged so the same
	// fixture on the parent commit can be compared number for number: the
	// only intended difference is the text of the terminal disclosure.
	keys := make([]string, 0, len(census))
	for k := range census {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, census[k]))
	}
	t.Logf("event census: %s", strings.Join(parts, " "))
	if census["tool_call"] != census["tool_result"] {
		t.Errorf("recovery broke the call/result invariant: %d vs %d",
			census["tool_call"], census["tool_result"])
	}
}

// Phase 3B is scoped to ONE terminal. Twelve other done emitters exist, and
// silently attaching recovery to them would turn an evidence-bound action
// into a routine one.
func TestRestorationIsWiredToExactlyOneTerminal(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	// Two sites, both deliberate: the repeat detector's terminal (Phase 3B)
	// and the work-deadline finaliser (Phase 2B), which reuses the same
	// eligibility rules rather than relaxing them for a timeout.
	if n := strings.Count(body, "restoreSaferDeliverables("); n != 2 {
		t.Errorf("restoration has %d call sites, want exactly 2", n)
	}
	i := strings.Index(body, "restoreSaferDeliverables(")
	if i < 0 {
		t.Fatal("restoration call site not found")
	}
	if !strings.Contains(body[i:min(len(body), i+400)], "repeatTerminalSummary(") {
		t.Error("restoration is no longer adjacent to the repeat-detector terminal")
	}
	j := strings.LastIndex(body, "restoreSaferDeliverables(")
	if !strings.Contains(body[max(0, j-900):j], "finalizeOnWorkDeadline") {
		t.Error("the second restoration site is not the work-deadline finaliser")
	}
	// The other terminal producers must not have gained it. They all route
	// through the one emitter now, so the count to hold is theirs.
	if n := strings.Count(body, "emitTerminal("); n < 12 {
		t.Errorf("found %d emitTerminal call sites, expected the full set of "+
			"producers; if one was removed, re-check this scope deliberately", n)
	}
	if n := strings.Count(body, `Stream("done"`); n != 1 {
		t.Errorf("found %d direct done payloads, want exactly 1 (the emitter)", n)
	}
}

// --- Phase 2B commit A: the atomic terminal contract ------------------------

// Every producer routes through the one emitter, and the emitter is the only
// place a done payload is built. A half-migrated producer would emit a
// terminal with no status, which consumers must read as incomplete -- so the
// defect would be invisible in behaviour and visible only here.
func TestEveryTerminalProducerGoesThroughTheOneEmitter(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if n := strings.Count(body, `Stream("done"`); n != 1 {
		t.Errorf("%d direct done payloads outside the emitter, want 0", n-1)
	}
	// 12 producers: 8 direct plus the 4 that share endStream. It was 13 until
	// the delete pre-empt was removed -- delete_file no longer ends the
	// session on the spot, so file_operation_no_task_intent has no producer.
	producers := strings.Count(body, "emitTerminal(") - 1 // the definition
	if producers < 12 {
		t.Errorf("found %d terminal producers, want at least 12; a producer "+
			"that stopped emitting is a session that ends in silence", producers)
	}
	for _, reason := range []string{
		"workspace_misaligned", "inference_failed", "text_instead_of_work",
		"unusable_model_output",
		"failure_ceiling", "same_target_failures", "turn_budget_exhausted",
		"oversized_tool_content", "repeated_refusal", "repeat_detector",
	} {
		if !strings.Contains(body, `"`+reason+`"`) {
			t.Errorf("terminal reason %q is gone; reasons are a stable "+
				"machine-readable contract", reason)
		}
	}
}

func TestUnclassifiedStatusFailsClosedAtTheEmitter(t *testing.T) {
	for _, raw := range []string{"", "COMPLETED", "done", "success", "ok", "finished"} {
		if NormalizeTerminalStatus(raw).Completed() {
			t.Errorf("consumer read %q as completed", raw)
		}
		if NormalizeTerminalStatus(raw) != TerminalIncomplete {
			t.Errorf("consumer read %q as %q, want incomplete",
				raw, NormalizeTerminalStatus(raw))
		}
	}
	if !NormalizeTerminalStatus("completed").Completed() {
		t.Error("a real completion stopped being one")
	}

	// A producer that names nothing must not imply an outcome.
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	var got map[string]string
	ctx.StreamFn = func(eventType string, data interface{}) {
		if eventType == "done" {
			b, _ := json.Marshal(data)
			json.Unmarshal(b, &got)
		}
	}
	emitTerminal(ctx, nil, TerminalStatus("nonsense"), "made_up", "whatever")
	if got["status"] != string(TerminalIncomplete) {
		t.Errorf("emitter accepted an unclassified status: %v", got)
	}
	if got["reason"] != "unclassified_producer" {
		t.Errorf("reason = %q, want the producer defect named", got["reason"])
	}
}

// Exactly one terminal per session, whatever races.
func TestOnlyOneTerminalEventPerSession(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	var mu sync.Mutex
	var terminals []map[string]string
	ctx.StreamFn = func(eventType string, data interface{}) {
		if eventType != "done" {
			return
		}
		b, _ := json.Marshal(data)
		var m map[string]string
		json.Unmarshal(b, &m)
		mu.Lock()
		terminals = append(terminals, m)
		mu.Unlock()
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				emitTerminal(ctx, nil, TerminalTimedOut, "work_deadline", "timed out")
			} else {
				emitTerminal(ctx, nil, TerminalCompleted, "deliverables_demonstrated", "done")
			}
		}(i)
	}
	wg.Wait()
	if len(terminals) != 1 {
		t.Fatalf("%d terminal events for one session", len(terminals))
	}
	// Whichever won, the recorded outcome and the emitted one agree.
	if terminals[0]["status"] != string(ctx.TerminalStatus) {
		t.Errorf("emitted %q but recorded %q", terminals[0]["status"], ctx.TerminalStatus)
	}
}

// The legacy key keeps its exact meaning and position, so a consumer that
// never learned about status reads what it always read.
func TestLegacyConsumersStillSeeSummaryOnly(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	var raw string
	ctx.StreamFn = func(eventType string, data interface{}) {
		if eventType == "done" {
			b, _ := json.Marshal(data)
			raw = string(b)
		}
	}
	emitTerminal(ctx, nil, TerminalStopped, "repeat_detector", "Stopped: because.")

	var legacy struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(raw), &legacy); err != nil {
		t.Fatalf("a legacy decoder cannot read the payload: %v", err)
	}
	if legacy.Summary != "Stopped: because." {
		t.Errorf("summary changed for legacy readers: %q", legacy.Summary)
	}
}

// The completion rule, stated as the outcomes it must produce.
func TestCompletionRequiresADemonstratedObligation(t *testing.T) {
	newCtx := func(t *testing.T, syntaxValid bool) (*AgentContext, string) {
		dir := t.TempDir()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/syntax-check") {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": syntaxValid})
		}))
		t.Cleanup(srv.Close)
		ctx := NewAgentContext(dir, Tier2Medium)
		ctx.SandboxURL = srv.URL
		ctx.PermissionMode = PermissionYolo
		ctx.StreamFn = func(string, interface{}) {}
		return ctx, dir
	}

	t.Run("nothing declared and nothing written", func(t *testing.T) {
		ctx, _ := newCtx(t, true)
		ok, reason := terminalCompletionAllowed(ctx, nil)
		if !ok || reason != "no_file_obligation" {
			t.Errorf("ok=%v reason=%q", ok, reason)
		}
	})

	t.Run("declared deliverable that parses", func(t *testing.T) {
		ctx, dir := newCtx(t, true)
		os.WriteFile(filepath.Join(dir, "solve.py"), []byte("A = 1\n"), 0o644)
		if ok, _ := terminalCompletionAllowed(ctx, []string{"solve.py"}); !ok {
			t.Error("a demonstrated deliverable was refused")
		}
	})

	t.Run("declared deliverable that does not parse", func(t *testing.T) {
		ctx, dir := newCtx(t, false)
		os.WriteFile(filepath.Join(dir, "solve.py"), []byte("def f(\n"), 0o644)
		ok, reason := terminalCompletionAllowed(ctx, []string{"solve.py"})
		if ok {
			t.Error("invalid bytes authorized completion")
		}
		if reason != "deliverables_not_demonstrated" {
			t.Errorf("reason = %q", reason)
		}
	})

	t.Run("declared deliverable that is not there", func(t *testing.T) {
		ctx, _ := newCtx(t, true)
		if ok, _ := terminalCompletionAllowed(ctx, []string{"missing.py"}); ok {
			t.Error("a missing deliverable authorized completion")
		}
	})

	t.Run("validation unavailable", func(t *testing.T) {
		ctx, dir := newCtx(t, true)
		ctx.SandboxURL = "http://127.0.0.1:1"
		os.WriteFile(filepath.Join(dir, "solve.py"), []byte("A = 1\n"), 0o644)
		if ok, _ := terminalCompletionAllowed(ctx, []string{"solve.py"}); ok {
			t.Error("an unknown verdict authorized completion")
		}
	})

	t.Run("a file the session wrote but never declared", func(t *testing.T) {
		ctx, _ := newCtx(t, false)
		args, _ := json.Marshal(map[string]string{"path": "side.py", "content": "def f(\n"})
		executeToolCall("write_file", args, ctx)
		if ok, _ := terminalCompletionAllowed(ctx, nil); ok {
			t.Error("an undeclared broken file the run wrote authorized completion")
		}
	})

	t.Run("deleting the deliverable cannot authorize completion", func(t *testing.T) {
		ctx, _ := newCtx(t, true)
		w, _ := json.Marshal(map[string]string{"path": "solve.py", "content": "A = 1\n"})
		executeToolCall("write_file", w, ctx)
		d, _ := json.Marshal(map[string]string{"path": "solve.py"})
		executeToolCall("delete_file", d, ctx)
		ok, reason := terminalCompletionAllowed(ctx, []string{"solve.py"})
		if ok {
			t.Fatal("removing the deliverable authorized completion — pair-1 defect")
		}
		if reason != "delete_intent_unestablished" {
			t.Errorf("reason = %q, want the delete intent named", reason)
		}
	})
}

// --- Phase 4A: the summary a non-completed run may carry --------------------
//
// Four of the fifty Stage-1 sessions ended with the model's own prose over an
// artifact nothing had verified, and three ended with no summary at all.
// Phase 2B made `status` honest and left `summary` — the only field a client
// written before that existed can read — saying the opposite.

// stage1FalseSuccessSummaries are the retained terminals, verbatim.
var stage1FalseSuccessSummaries = map[string]string{
	"debounce5": "Made your change. The follow-up verification command kept repeating and " +
		"failing (often a typo in the command, not the edit) — the change is on disk; run it " +
		"yourself to confirm.",
	"ledger2": "I have verified the contents of input.txt and confirmed that the logic in " +
		"solve.py correctly processes the data. The script identifies 4 settle events and " +
		"determines that the final balance is correct.",
	"overlay3": "I have successfully implemented the interval priority logic in `solve.py`. " +
		"The script correctly processes `input.txt`, identifies the highest priority at each " +
		"point, and calculates the total.",
	"ring5": "I wrote solve.py which implements a ring buffer of capacity 6. It correctly " +
		"handles 'push' (with overwrites), 'pop', 'rot' (rotating the oldest K items to the " +
		"end), and prints the final state.",
}

func TestRetainedFalseSuccessSummariesNeverShipOnANonCompletedRun(t *testing.T) {
	for task, prose := range stage1FalseSuccessSummaries {
		t.Run(task, func(t *testing.T) {
			// The claim is detected as a claim.
			if claim := completionClaimIn(prose); claim == "" {
				t.Fatalf("no completion claim detected in the retained summary:\n%s", prose)
			}
			ctx := NewAgentContext(t.TempDir(), Tier2Medium)
			st := &runState{madeProductiveChange: true}
			got := honestTerminalSummary(ctx, st, TerminalIncomplete, "deliverables_not_demonstrated", prose)
			if completionClaimIn(got) != "" {
				t.Errorf("a completion claim survived:\n%s", got)
			}
			if !hasHonestMarker(got) {
				t.Errorf("the replacement does not say the run did not finish:\n%s", got)
			}
			if strings.Contains(got, prose) {
				t.Error("the model's account was reproduced verbatim")
			}
		})
	}
}

// Negated language is a report of failure, not a claim, and must survive.
func TestHonestReportsAreNotMistakenForClaims(t *testing.T) {
	for _, s := range []string{
		"Nothing was written to disk in this run, and no verification command completed successfully.",
		"Changes were written to disk, but NOTHING in this run verified them.",
		"Stopped: the same tool call kept repeating without making progress. Your work is on disk " +
			"and parses, but the verification did not complete, so this run cannot say the task is done.",
		"Stopped: the session ran out of time before the work finished, and nothing was written to disk.",
		"I ran out of turns for this request before finishing. Nothing was written to disk.",
		"Stopped after 3 tool failures on the same target with no successful changes.",
	} {
		if claim := completionClaimIn(s); claim != "" {
			t.Errorf("honest report flagged as the claim %q:\n%s", claim, s)
		}
	}
}

func TestEveryNonCompletedTerminalIsHonest(t *testing.T) {
	// One row per terminal producer, with the status and reason it emits.
	producers := []struct {
		reason  string
		status  TerminalStatus
		summary string
	}{
		{"workspace_misaligned", TerminalFailed, "proxy and sandbox workspaces are not aligned"},
		{"inference_failed", TerminalFailed, "Stopped: the model call failed, so the run could not continue."},
		{"text_instead_of_work", TerminalIncomplete, "The reply was cut short — it had begun repeating itself."},
		{"unusable_model_output", TerminalStopped, "Stopped after 3 unparseable responses."},
		{"deliverables_not_demonstrated", TerminalIncomplete, ""},
		{"delete_intent_unestablished", TerminalIncomplete, ""},
		{"text_reply", TerminalIncomplete, ""},
		{"failure_ceiling", TerminalStopped, "Stopped after 9 failed tool calls with nothing landing on disk."},
		{"same_target_failures", TerminalStopped, "Wrote your changes to disk; couldn't verify them automatically."},
		{"turn_budget_exhausted", TerminalIncomplete, "I ran out of turns for this request before finishing."},
		{"oversized_tool_content", TerminalStopped, "Stopped: content too large for tool calls."},
		{"repeated_refusal", TerminalStopped, "Stopped: the same `write_file` call was re-sent after being refused."},
		{"repeat_detector", TerminalStopped, "Stopped: the same tool call kept repeating without making progress."},
		{"work_deadline", TerminalTimedOut, "Stopped: the session ran out of time before the work finished."},
		{"cancelled", TerminalIncomplete, "Stopped: the run was cancelled before the work finished."},
		{"unclassified_producer", TerminalIncomplete, ""},
		{"clarification_requested", TerminalIncomplete, ""},
		{"investigation_handed_back", TerminalIncomplete, ""},
	}
	if len(producers) < 12 {
		t.Fatalf("only %d producers covered; there are 12", len(producers))
	}
	for _, p := range producers {
		for _, wrote := range []bool{false, true} {
			name := p.reason
			if wrote {
				name += "/wrote"
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				ctx := NewAgentContext(dir, Tier2Medium)
				st := &runState{madeProductiveChange: wrote}
				got := honestTerminalSummary(ctx, st, p.status, p.reason, p.summary)
				if strings.TrimSpace(got) == "" {
					t.Fatal("a terminal shipped with no summary at all")
				}
				if claim := completionClaimIn(got); claim != "" {
					t.Errorf("completion claim %q on a %s terminal:\n%s", claim, p.status, got)
				}
				if !hasHonestMarker(got) {
					t.Errorf("summary never says the run did not finish:\n%s", got)
				}
			})
		}
	}
}

// The fallback reports the artifact state it can actually establish.
func TestFallbackDescribesTheArtifactItCanSee(t *testing.T) {
	newCtx := func(t *testing.T, valid bool) (*AgentContext, string) {
		dir := t.TempDir()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/syntax-check") {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": valid})
		}))
		t.Cleanup(srv.Close)
		ctx := NewAgentContext(dir, Tier2Medium)
		ctx.SandboxURL = srv.URL
		ctx.PermissionMode = PermissionYolo
		ctx.StreamFn = func(string, interface{}) {}
		return ctx, dir
	}

	t.Run("timed out with a valid artifact", func(t *testing.T) {
		ctx, dir := newCtx(t, true)
		os.WriteFile(filepath.Join(dir, "solve.py"), []byte("A = 1\n"), 0o644)
		st := &runState{madeProductiveChange: true, expectedOutputs: []string{"solve.py"}}
		got := honestTerminalSummary(ctx, st, TerminalTimedOut, "work_deadline", "")
		if !strings.Contains(got, "parses") {
			t.Errorf("valid bytes not disclosed: %s", got)
		}
		if completionClaimIn(got) != "" || !hasHonestMarker(got) {
			t.Errorf("not honest: %s", got)
		}
	})

	t.Run("timed out with unverified bytes", func(t *testing.T) {
		ctx, dir := newCtx(t, false)
		os.WriteFile(filepath.Join(dir, "solve.py"), []byte("def f(\n"), 0o644)
		st := &runState{madeProductiveChange: true, expectedOutputs: []string{"solve.py"}}
		got := honestTerminalSummary(ctx, st, TerminalTimedOut, "work_deadline", "")
		if !strings.Contains(got, "unverified") {
			t.Errorf("invalid bytes not disclosed as unverified: %s", got)
		}
	})

	t.Run("stopped with nothing on disk", func(t *testing.T) {
		ctx, _ := newCtx(t, true)
		st := &runState{}
		got := honestTerminalSummary(ctx, st, TerminalStopped, "repeat_detector", "")
		if !strings.Contains(got, "Nothing was written") {
			t.Errorf("empty workspace not disclosed: %s", got)
		}
	})
}

// A completed run keeps its account, because the gate already agreed with it.
func TestCompletedRunKeepsItsSummary(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	st := &runState{madeProductiveChange: true}
	prose := "I have successfully implemented the interval priority logic in solve.py."
	got := honestTerminalSummary(ctx, st, TerminalCompleted, "deliverables_demonstrated", prose)
	if got != prose {
		t.Errorf("a verified completion had its summary rewritten:\n%s", got)
	}
	if modelProseIfAuthorized(TerminalCompleted, prose) != prose {
		t.Error("authorized prose was withheld")
	}
	if modelProseIfAuthorized(TerminalIncomplete, prose) != "" {
		t.Error("unauthorized prose passed through")
	}
}

// The two kinds of client must not disagree about failure. A legacy client
// reads only `summary`; a structured client reads `status`.
//
// The property is one-directional on purpose. A legacy reader seeing a success
// claim while the status says otherwise is the defect this phase exists to
// remove. A legacy reader failing to recognise a genuine completion is not:
// prose is not a protocol, and under-claiming is safe. So the assertion is
// that a claim NEVER appears on a non-completed status, and that an authorised
// completion's claim is still allowed through.
func TestLegacyReaderNeverSeesSuccessOnANonCompletedRun(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	cases := []struct {
		status  TerminalStatus
		reason  string
		summary string
	}{
		{TerminalCompleted, "deliverables_demonstrated", "I have successfully implemented solve.py and all tests pass."},
		{TerminalIncomplete, "deliverables_not_demonstrated", stage1FalseSuccessSummaries["overlay3"]},
		{TerminalIncomplete, "delete_intent_unestablished", ""},
		{TerminalStopped, "repeat_detector", "Stopped: the same tool call kept repeating."},
		{TerminalTimedOut, "work_deadline", ""},
		{TerminalFailed, "inference_failed", ""},
	}
	for _, c := range cases {
		t.Run(c.reason+"/"+string(c.status), func(t *testing.T) {
			st := &runState{madeProductiveChange: true}
			summary := honestTerminalSummary(ctx, st, c.status, c.reason, c.summary)

			// A legacy client has one signal: does the prose claim success?
			legacySeesClaim := completionClaimIn(summary) != ""
			structuredSaysDone := NormalizeTerminalStatus(string(c.status)).Completed()
			if legacySeesClaim && !structuredSaysDone {
				t.Errorf("legacy reader sees a success claim while the status is %q:\n%s",
					c.status, summary)
			}
			if structuredSaysDone && !legacySeesClaim {
				t.Errorf("an authorised completion had its claim stripped:\n%s", summary)
			}
		})
	}
}

// The production shape, through the real loop: the model declares done with a
// success claim over a file that does not parse. Free of Phase 4A symbols, so
// the same fixture runs on the parent tree.
func TestModelSuccessClaimOverAnInvalidArtifact(t *testing.T) {
	dir := t.TempDir()
	const broken = "def solve():\n    return [1, 2]]\n"
	const claim = "I have successfully implemented solve.py. The script correctly processes input.txt."

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			valid := !strings.Contains(in.Code, "]]")
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{"SyntaxError: unmatched ']'"}
			}
			json.NewEncoder(w).Encode(out)
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		i := calls
		calls++
		var payload map[string]interface{}
		switch i {
		case 0:
			// A write that lands with a parse warning, as write_file does for
			// a new file that does not compile.
			payload = map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "solve.py", "content": broken}}
		case 1:
			// A command that EXITS ZERO. This is the ledger2 / ring5 shape:
			// the run has a passing verification on the record, so the
			// verification gate is satisfied, and the model's confident prose
			// reaches the summary untouched — over a file that does not parse.
			payload = map[string]interface{}{"type": "tool_call", "name": "run_command",
				"args": map[string]string{"command": "echo checked"}}
		default:
			payload = map[string]interface{}{"type": "done", "summary": claim}
		}
		body, _ := json.Marshal(payload)
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(body)}}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.MaxTurns = 12

	var terminal map[string]string
	census := map[string]int{}
	ctx.StreamFn = func(eventType string, data interface{}) {
		census[eventType]++
		if eventType == "done" {
			b, _ := json.Marshal(data)
			json.Unmarshal(b, &terminal)
		}
	}
	if err := runAgentLoop(ctx, "Write solve.py."); err != nil {
		t.Fatalf("agent loop error: %v", err)
	}

	onDisk, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
	t.Logf("summary: %s", terminal["summary"])
	t.Logf("status=%q reason=%q tool_calls=%d disk=%q",
		terminal["status"], terminal["reason"], census["tool_call"], string(onDisk))

	// The behaviour under test: the model's claim does not reach the client.
	for _, phrase := range []string{"successfully implemented", "correctly processes"} {
		if strings.Contains(terminal["summary"], phrase) {
			t.Errorf("the model's claim %q shipped over an invalid artifact:\n%s",
				phrase, terminal["summary"])
		}
	}
	if terminal["status"] == "completed" {
		t.Errorf("invalid bytes reported as completed")
	}
	if strings.TrimSpace(terminal["summary"]) == "" {
		t.Error("the terminal shipped with no summary")
	}
	// Everything else is unchanged: the bytes the model wrote are still there,
	// and the run still made the same calls.
	if string(onDisk) != broken {
		t.Errorf("disk changed: %q", onDisk)
	}
	if census["tool_call"] != census["tool_result"] {
		t.Errorf("call/result invariant broken: %d vs %d",
			census["tool_call"], census["tool_result"])
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
}

// --- Terminal authorization: the evidence the status decision already had ---
//
// Both completion paths authorized success before consulting predicates the
// same function evaluates twelve lines later for the summary. The measured
// result was one terminal event contradicting itself: status "completed",
// reason "no_file_obligation", summary "Nothing was written — no file was
// created or changed in this run."

// termFixture drives the real loop with MaxTurns=0 and a server-side ceiling,
// so nothing sleeps and an unbounded loop fails immediately.
func termFixture(t *testing.T, dir, request string, ceiling int,
	plan func(i int, prompt string) map[string]interface{}) (*AgentContext, *int, map[string]int, map[string]string) {
	t.Helper()
	turns := 0
	census := map[string]int{}
	terminal := map[string]string{}
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			valid := !strings.Contains(in.Code, "]]")
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{"SyntaxError: unmatched ']'"}
			}
			json.NewEncoder(w).Encode(out)
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(string(raw), "single fenced block") {
			// No fenced block ever arrives: the resolution fails fast.
			d, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"delta": map[string]string{"content": "Sure, here it is."}}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
			return
		}
		mu.Lock()
		i := turns
		turns++
		mu.Unlock()
		if i >= ceiling {
			http.Error(w, "turn ceiling exceeded", http.StatusInsufficientStorage)
			return
		}
		call, _ := json.Marshal(plan(i, string(raw)))
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(call)}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	t.Cleanup(srv.Close)

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.MaxTurns = 0
	ctx.StreamFn = func(et string, data interface{}) {
		b, _ := json.Marshal(data)
		mu.Lock()
		defer mu.Unlock()
		census[et]++
		if et == "done" {
			var m map[string]string
			json.Unmarshal(b, &m)
			for k, v := range m {
				terminal[k] = v
			}
		}
	}
	return ctx, &turns, census, terminal
}

const termCeiling = 30

// 1. The measured four-path defect.
func TestUnnamedDeliverablesNeverCompleteWithNothingWritten(t *testing.T) {
	dir := t.TempDir()
	paths := []string{"a.py", "b.py", "c.py", "d.py"}
	ctx, turns, census, terminal := termFixture(t, dir, "Write four files.", termCeiling,
		func(i int, _ string) map[string]interface{} {
			if i >= len(paths) {
				return map[string]interface{}{"type": "done",
					"summary": "I have successfully written all four files."}
			}
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": paths[i], "content": "@fenced"}}
		})
	if err := runAgentLoop(ctx, "Write four files."); err != nil {
		t.Fatalf("loop error: %v", err)
	}
	t.Logf("turns=%d status=%q reason=%q summary=%.100s",
		*turns, terminal["status"], terminal["reason"], terminal["summary"])

	if terminal["status"] != string(TerminalIncomplete) {
		t.Fatalf("status = %q, want incomplete", terminal["status"])
	}
	if terminal["reason"] != "action_demanded_unmet" {
		t.Errorf("reason = %q, want action_demanded_unmet", terminal["reason"])
	}
	if completionClaimIn(terminal["summary"]) != "" {
		t.Errorf("the model's claim reached the summary:\n%s", terminal["summary"])
	}
	if !hasHonestMarker(terminal["summary"]) {
		t.Errorf("summary is not honest:\n%s", terminal["summary"])
	}
	if len(ctx.Ledger) != 0 {
		t.Errorf("a path the session never wrote entered the ledger: %v", ctx.Ledger)
	}
	var found []string
	filepath.Walk(dir, func(p string, i os.FileInfo, e error) error {
		if e == nil && i != nil && !i.IsDir() && !strings.Contains(p, "mount-probe") {
			found = append(found, p)
		}
		return nil
	})
	if len(found) != 0 {
		t.Errorf("files on disk: %v", found)
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
	if census["tool_call"] != census["tool_result"] {
		t.Errorf("call/result balance: %d vs %d", census["tool_call"], census["tool_result"])
	}
}

// 2. The text exit, which starts at completed and only downgrades.
func TestTextExitCannotCompleteAnUnmetActionRequest(t *testing.T) {
	dir := t.TempDir()
	ctx, _, census, terminal := termFixture(t, dir, "Write four files.", termCeiling,
		func(i int, _ string) map[string]interface{} {
			return map[string]interface{}{"type": "text",
				"content": "I have successfully implemented all four files as requested."}
		})
	if err := runAgentLoop(ctx, "Write four files."); err != nil {
		t.Fatal(err)
	}
	t.Logf("text exit: status=%q reason=%q summary=%.100s",
		terminal["status"], terminal["reason"], terminal["summary"])
	if terminal["status"] != string(TerminalIncomplete) {
		t.Fatalf("status = %q, want incomplete", terminal["status"])
	}
	if terminal["reason"] != "action_demanded_unmet" {
		t.Errorf("reason = %q", terminal["reason"])
	}
	if completionClaimIn(terminal["summary"]) != "" {
		t.Errorf("the model's completion prose reached the summary:\n%s", terminal["summary"])
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
}

// 3. Verification demanded and unmet, and the precedence when both are unmet.
func TestUnmetVerificationDowngradesAndActionWins(t *testing.T) {
	t.Run("verification unmet after a real write", func(t *testing.T) {
		dir := t.TempDir()
		const body = "def solve():\n    return 1\n"
		ctx, _, _, terminal := termFixture(t, dir,
			"Create solve.py and run the tests to verify it.", termCeiling,
			func(i int, _ string) map[string]interface{} {
				if i == 0 {
					return map[string]interface{}{"type": "tool_call", "name": "write_file",
						"args": map[string]string{"path": "solve.py", "content": body}}
				}
				return map[string]interface{}{"type": "done", "summary": "wrote it"}
			})
		if err := runAgentLoop(ctx, "Create solve.py and run the tests to verify it."); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
		t.Logf("verify-unmet: status=%q reason=%q disk=%q",
			terminal["status"], terminal["reason"], string(got))
		if terminal["status"] == string(TerminalCompleted) {
			t.Errorf("completed with verification demanded and unmet")
		}
		if terminal["reason"] != "verification_demanded_unmet" {
			t.Errorf("reason = %q, want verification_demanded_unmet", terminal["reason"])
		}
	})

	t.Run("both unmet: action wins", func(t *testing.T) {
		dir := t.TempDir()
		ctx, _, _, terminal := termFixture(t, dir,
			"Create some files and run the tests to verify them.", termCeiling,
			func(i int, _ string) map[string]interface{} {
				return map[string]interface{}{"type": "done", "summary": "all set"}
			})
		if err := runAgentLoop(ctx, "Create some files and run the tests to verify them."); err != nil {
			t.Fatal(err)
		}
		t.Logf("both-unmet: status=%q reason=%q", terminal["status"], terminal["reason"])
		if terminal["status"] != string(TerminalIncomplete) {
			t.Fatalf("status = %q", terminal["status"])
		}
		if terminal["reason"] != "action_demanded_unmet" {
			t.Errorf("reason = %q, want action to take precedence", terminal["reason"])
		}
	})
}

// 4. A genuine question still completes, through both exits.
func TestReadOnlyRequestsStillComplete(t *testing.T) {
	for _, exit := range []string{"done", "text"} {
		t.Run(exit, func(t *testing.T) {
			dir := t.TempDir()
			const q = "What does this project do?"
			ctx, _, _, terminal := termFixture(t, dir, q, termCeiling,
				func(i int, _ string) map[string]interface{} {
					if exit == "text" {
						return map[string]interface{}{"type": "text",
							"content": "It is a small solver: solve.py reads input.txt and prints a total."}
					}
					return map[string]interface{}{"type": "done",
						"summary": "It is a small solver that reads input.txt and prints a total."}
				})
			if err := runAgentLoop(ctx, q); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s exit: status=%q reason=%q", exit, terminal["status"], terminal["reason"])
			if terminal["status"] != string(TerminalCompleted) {
				t.Errorf("a read-only question no longer completes: status=%q reason=%q",
					terminal["status"], terminal["reason"])
			}
		})
	}
}

// 5. The successful paths are unchanged.
func TestSuccessfulCompletionsAreUnchanged(t *testing.T) {
	const body = "def solve():\n    return 1\n\nprint(solve())\n"
	run := func(t *testing.T, request string) (map[string]string, *AgentContext, string) {
		dir := t.TempDir()
		ctx, _, _, terminal := termFixture(t, dir, request, termCeiling,
			func(i int, _ string) map[string]interface{} {
				switch i {
				case 0:
					return map[string]interface{}{"type": "tool_call", "name": "write_file",
						"args": map[string]string{"path": "solve.py", "content": body}}
				case 1:
					return map[string]interface{}{"type": "tool_call", "name": "run_command",
						"args": map[string]string{"command": "python3 solve.py"}}
				default:
					return map[string]interface{}{"type": "done",
						"summary": "I have successfully created solve.py and ran it."}
				}
			})
		if err := runAgentLoop(ctx, request); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
		return terminal, ctx, string(got)
	}

	t.Run("named deliverable", func(t *testing.T) {
		terminal, ctx, got := run(t, "Create solve.py that prints 1, then run it.")
		t.Logf("named: status=%q reason=%q", terminal["status"], terminal["reason"])
		if terminal["status"] != string(TerminalCompleted) {
			t.Fatalf("status=%q reason=%q", terminal["status"], terminal["reason"])
		}
		d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
		if d == nil || d.CurrentHash != hashBytes([]byte(got)) {
			t.Error("ledger does not describe the final bytes")
		}
		if k, s := d.CurrentValidation(); s != ValidationPassed || k != ValidationKindSyntax {
			t.Errorf("completion over %v/%v", k, s)
		}
		// A genuinely completed run keeps the model's account.
		if completionClaimIn(terminal["summary"]) == "" {
			t.Errorf("an authorised completion had its claim stripped:\n%s", terminal["summary"])
		}
	})

	t.Run("unnamed deliverable actually written", func(t *testing.T) {
		terminal, _, _ := run(t, "Write a small script and run it.")
		t.Logf("unnamed: status=%q reason=%q", terminal["status"], terminal["reason"])
		if terminal["status"] != string(TerminalCompleted) {
			t.Errorf("a run that wrote and verified did not complete: status=%q reason=%q",
				terminal["status"], terminal["reason"])
		}
	})

	t.Run("deliverables_not_demonstrated still wins when it applies", func(t *testing.T) {
		dir := t.TempDir()
		const broken = "def solve():\n    return [1, 2]]\n"
		ctx, _, _, terminal := termFixture(t, dir,
			"Create solve.py that prints the list.", termCeiling,
			func(i int, _ string) map[string]interface{} {
				if i == 0 {
					return map[string]interface{}{"type": "tool_call", "name": "write_file",
						"args": map[string]string{"path": "solve.py", "content": broken}}
				}
				if i == 1 {
					return map[string]interface{}{"type": "tool_call", "name": "run_command",
						"args": map[string]string{"command": "echo checked"}}
				}
				return map[string]interface{}{"type": "done", "summary": "done"}
			})
		if err := runAgentLoop(ctx, "Create solve.py that prints the list."); err != nil {
			t.Fatal(err)
		}
		t.Logf("broken-artifact: status=%q reason=%q", terminal["status"], terminal["reason"])
		if terminal["status"] == string(TerminalCompleted) {
			t.Fatal("invalid bytes completed")
		}
		if terminal["reason"] != "deliverables_not_demonstrated" {
			t.Errorf("reason = %q — a more specific existing failure was replaced",
				terminal["reason"])
		}
	})
}

// 6. Refused unsafe mutations cannot become success by leaving no trace.
func TestRefusedUnsafeMutationsCannotComplete(t *testing.T) {
	for _, c := range []struct{ name, path string }{
		{"deny-listed", ".env"},
		{"path escape", "../outside.py"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, ".env"), []byte("S=1\n"), 0o644)
			ctx, _, _, terminal := termFixture(t, dir, "Write the config file.", termCeiling,
				func(i int, _ string) map[string]interface{} {
					if i < 2 {
						return map[string]interface{}{"type": "tool_call", "name": "write_file",
							"args": map[string]string{"path": c.path, "content": "SECRET=2\n"}}
					}
					return map[string]interface{}{"type": "done", "summary": "wrote the config"}
				})
			if err := runAgentLoop(ctx, "Write the config file."); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: status=%q reason=%q", c.name, terminal["status"], terminal["reason"])
			if terminal["status"] == string(TerminalCompleted) {
				t.Errorf("a refused unsafe mutation completed: reason=%q", terminal["reason"])
			}
			if got, _ := os.ReadFile(filepath.Join(dir, ".env")); string(got) != "S=1\n" {
				t.Errorf(".env changed: %q", got)
			}
			if len(ctx.Ledger) != 0 {
				t.Errorf("a refused path entered the ledger: %v", ctx.Ledger)
			}
		})
	}
}

// Structural guard: both exits must reach the SAME finalizer, so a future
// producer cannot consult unmet-action evidence only for prose.
func TestBothCompletionPathsShareOneDecision(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if n := strings.Count(body, "finalizeCompletion("); n < 3 {
		t.Errorf("finalizeCompletion has %d references (definition + 2 exits expected); "+
			"both completion paths must share one decision", n)
	}
	// terminalCompletionAllowed is the finalizer's input, not a producer's.
	if n := strings.Count(body, "terminalCompletionAllowed("); n != 2 {
		t.Errorf("terminalCompletionAllowed has %d references, want 2 (definition + "+
			"the single call inside finalizeCompletion); a producer calling it "+
			"directly bypasses the unmet-action evidence", n)
	}
}

// --- Unresolved mutation debt -----------------------------------------------
//
// Completion had three inputs: user-named outputs, the deliverable ledger, and
// one session-wide madeProductiveChange bool. A valid mutation intent that
// never landed left no trace in any of them, so a success on an unrelated path
// retired it. Measured: a.py fails before dispatch, b.py lands and validates,
// terminal completed / deliverables_demonstrated.
//
// Debt is deliberately NOT in the deliverable ledger: that records what the
// session owns on disk, and an intent that never landed owns nothing.

// debtFixture scripts the loop with MaxTurns=0 and a server-side ceiling.
// Fenced sub-calls never return a block, so a "@fenced" write fails before
// dispatch — the case that creates debt without a ledger entry.
func debtFixture(t *testing.T, dir, request string, ceiling int,
	plan func(i int, prompt string) map[string]interface{}) (*AgentContext, *int, map[string]int, map[string]string, *[]string) {
	t.Helper()
	turns := 0
	census := map[string]int{}
	terminal := map[string]string{}
	var bounces []string
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			valid := !strings.Contains(in.Code, "]]")
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{"SyntaxError: unmatched ']'"}
			}
			json.NewEncoder(w).Encode(out)
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(string(raw), "single fenced block") {
			d, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"delta": map[string]string{"content": "Sure, here it is."}}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
			return
		}
		mu.Lock()
		i := turns
		turns++
		mu.Unlock()
		if i >= ceiling {
			http.Error(w, "turn ceiling exceeded", http.StatusInsufficientStorage)
			return
		}
		call, _ := json.Marshal(plan(i, string(raw)))
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(call)}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	t.Cleanup(srv.Close)

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.MaxTurns = 0
	ctx.StreamFn = func(et string, data interface{}) {
		b, _ := json.Marshal(data)
		mu.Lock()
		defer mu.Unlock()
		census[et]++
		if et == "gate" || et == "tool_result" {
			bounces = append(bounces, et+"|"+string(b))
		}
		if et == "done" {
			var m map[string]string
			json.Unmarshal(b, &m)
			for k, v := range m {
				terminal[k] = v
			}
		}
	}
	return ctx, &turns, census, terminal, &bounces
}

const debtCeiling = 30
const debtGoodBody = "def helper():\n    return 2\n\nprint(helper())\n"

// 1. THE DEFECT: an unrelated success must not retire a failed intent.
func TestUnrelatedSuccessDoesNotRetireAFailedIntent(t *testing.T) {
	dir := t.TempDir()
	const req = "Write a couple of small scripts."
	ctx, _, census, terminal, _ := debtFixture(t, dir, req, debtCeiling,
		func(i int, _ string) map[string]interface{} {
			switch i {
			case 0, 1:
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "a.py", "content": "@fenced"}}
			case 2:
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "b.py", "content": debtGoodBody}}
			case 3:
				return map[string]interface{}{"type": "tool_call", "name": "run_command",
					"args": map[string]string{"command": "python3 b.py"}}
			default:
				return map[string]interface{}{"type": "done", "summary": "wrote what I could"}
			}
		})
	if err := runAgentLoop(ctx, req); err != nil {
		t.Fatalf("loop error: %v", err)
	}
	t.Logf("status=%q reason=%q summary=%.140s",
		terminal["status"], terminal["reason"], terminal["summary"])

	if terminal["status"] == string(TerminalCompleted) {
		t.Fatalf("an unrelated success retired the failed intent: reason=%q", terminal["reason"])
	}
	if terminal["reason"] != "unresolved_mutation_debt" {
		t.Errorf("reason = %q, want unresolved_mutation_debt", terminal["reason"])
	}
	if !strings.Contains(terminal["summary"], "a.py") {
		t.Errorf("the summary does not name the unresolved path:\n%s", terminal["summary"])
	}
	for _, leak := range []string{"debt", "ledger", "hash", "canonical"} {
		if strings.Contains(strings.ToLower(terminal["summary"]), leak) {
			t.Errorf("the summary exposes the internal term %q:\n%s", leak, terminal["summary"])
		}
	}
	// b.py really did land and validate: debt is not a blanket block.
	if got, _ := os.ReadFile(filepath.Join(dir, "b.py")); string(got) != debtGoodBody {
		t.Errorf("b.py = %q", got)
	}
	// a.py never entered the deliverable ledger.
	if _, ok := ctx.Ledger[ledgerKey(ctx, "a.py")]; ok {
		t.Error("a path that never landed entered the deliverable ledger")
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
	if census["tool_call"] != census["tool_result"] {
		t.Errorf("call/result balance: %d vs %d", census["tool_call"], census["tool_result"])
	}
}

// 2 + 3. Same-path validated success clears, including through an alias.
func TestSamePathValidatedSuccessClearsExactlyOneDebt(t *testing.T) {
	for _, c := range []struct{ name, failPath, fixPath string }{
		{"same spelling", "a.py", "a.py"},
		{"alias spelling", "./a.py", "a.py"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			const req = "Write a small script."
			ctx, _, _, terminal, _ := debtFixture(t, dir, req, debtCeiling,
				func(i int, _ string) map[string]interface{} {
					switch i {
					case 0, 1:
						return map[string]interface{}{"type": "tool_call", "name": "write_file",
							"args": map[string]string{"path": c.failPath, "content": "@fenced"}}
					case 2:
						return map[string]interface{}{"type": "tool_call", "name": "write_file",
							"args": map[string]string{"path": c.fixPath, "content": debtGoodBody}}
					case 3:
						return map[string]interface{}{"type": "tool_call", "name": "run_command",
							"args": map[string]string{"command": "python3 " + c.fixPath}}
					default:
						return map[string]interface{}{"type": "done", "summary": "wrote it"}
					}
				})
			if err := runAgentLoop(ctx, req); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: status=%q reason=%q", c.name, terminal["status"], terminal["reason"])
			if terminal["status"] != string(TerminalCompleted) {
				t.Errorf("a resolved path did not complete: status=%q reason=%q",
					terminal["status"], terminal["reason"])
			}
		})
	}
}

// 4. Bytes on disk are not enough: the validation must be current and passed.
func TestUnvalidatedBytesDoNotClearDebt(t *testing.T) {
	dir := t.TempDir()
	const req = "Write a small script."
	const broken = "def solve():\n    return [1, 2]]\n"
	ctx, _, _, terminal, _ := debtFixture(t, dir, req, debtCeiling,
		func(i int, _ string) map[string]interface{} {
			switch i {
			case 0, 1:
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "a.py", "content": "@fenced"}}
			case 2:
				// Lands with a parse warning: applied, but validation failed.
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "a.py", "content": broken}}
			default:
				return map[string]interface{}{"type": "done", "summary": "wrote it"}
			}
		})
	if err := runAgentLoop(ctx, req); err != nil {
		t.Fatal(err)
	}
	t.Logf("unvalidated: status=%q reason=%q", terminal["status"], terminal["reason"])
	if terminal["status"] == string(TerminalCompleted) {
		t.Errorf("bytes that failed validation cleared the debt")
	}
}

// 5. A file with no applicable checker resolves on not_applicable.
func TestNonCodeNotApplicableClearsDebt(t *testing.T) {
	dir := t.TempDir()
	const req = "Write the notes file."
	ctx, _, _, terminal, _ := debtFixture(t, dir, req, debtCeiling,
		func(i int, _ string) map[string]interface{} {
			switch i {
			case 0, 1:
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "notes.txt", "content": "@fenced"}}
			case 2:
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "notes.txt", "content": "hello\n"}}
			default:
				return map[string]interface{}{"type": "done", "summary": "wrote the notes"}
			}
		})
	if err := runAgentLoop(ctx, req); err != nil {
		t.Fatal(err)
	}
	t.Logf("not_applicable: status=%q reason=%q", terminal["status"], terminal["reason"])
	// The DEBT must clear on not_applicable. The terminal is separately
	// blocked by deliverablesDemonstrablyValid, which requires a syntax PASS
	// and so can never demonstrate a non-code deliverable — a pre-existing
	// limitation of the deliverable rule, not of debt, and out of scope here.
	if terminal["reason"] == "unresolved_mutation_debt" {
		t.Errorf("a non-code file with no applicable checker did not clear its debt: %s",
			terminal["summary"])
	}
}

// 7. Unsafe or malformed attempts create no debt and no ledger entry; they are
// still covered by the unmet-action evidence.
func TestUnsafeAttemptsCreateNoPathDebt(t *testing.T) {
	for _, c := range []struct{ name, path, content string }{
		{"deny-listed", ".env", "SECRET=2\n"},
		{"path escape", "../outside.py", "x = 1\n"},
		{"blank path", "   ", "x = 1\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			const req = "Write the config and a script."
			ctx, _, _, terminal, _ := debtFixture(t, dir, req, debtCeiling,
				func(i int, _ string) map[string]interface{} {
					switch i {
					case 0:
						return map[string]interface{}{"type": "tool_call", "name": "write_file",
							"args": map[string]string{"path": c.path, "content": c.content}}
					case 1:
						return map[string]interface{}{"type": "tool_call", "name": "write_file",
							"args": map[string]string{"path": "ok.py", "content": debtGoodBody}}
					case 2:
						return map[string]interface{}{"type": "tool_call", "name": "run_command",
							"args": map[string]string{"command": "python3 ok.py"}}
					default:
						return map[string]interface{}{"type": "done", "summary": "did the work"}
					}
				})
			if err := runAgentLoop(ctx, req); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: status=%q reason=%q", c.name, terminal["status"], terminal["reason"])
			// The unsafe path must not be the reason, and must not appear as
			// an unresolved deliverable in the summary.
			if terminal["reason"] == "unresolved_mutation_debt" &&
				strings.Contains(terminal["summary"], strings.TrimSpace(c.path)) {
				t.Errorf("an unsafe/invalid path became tracked work:\n%s", terminal["summary"])
			}
			if _, ok := ctx.Ledger[ledgerKey(ctx, c.path)]; ok {
				t.Error("an unsafe path entered the deliverable ledger")
			}
		})
	}
}

// 11. Read-only tasks are untouched.
func TestReadOnlyTaskUnaffectedByDebt(t *testing.T) {
	dir := t.TempDir()
	const q = "What does this project do?"
	ctx, _, _, terminal, _ := debtFixture(t, dir, q, debtCeiling,
		func(i int, _ string) map[string]interface{} {
			return map[string]interface{}{"type": "done", "summary": "It is a small solver."}
		})
	if err := runAgentLoop(ctx, q); err != nil {
		t.Fatal(err)
	}
	t.Logf("read-only: status=%q reason=%q", terminal["status"], terminal["reason"])
	if terminal["status"] != string(TerminalCompleted) {
		t.Errorf("a question stopped completing: status=%q reason=%q",
			terminal["status"], terminal["reason"])
	}
}

// 8, 9, 10. Delete and move debt resolve only on demonstrated absence and
// demonstrated destination bytes, and separate paths stay independent.
func TestDeleteMoveAndIndependenceOfDebt(t *testing.T) {
	t.Run("successful delete resolves on confirmed absence", func(t *testing.T) {
		ctx := NewAgentContext(t.TempDir(), Tier2Medium)
		ctx.PermissionMode, ctx.StreamFn = PermissionYolo, func(string, interface{}) {}
		st := &runState{}
		w, _ := json.Marshal(map[string]string{"path": "gone.py", "content": "A = 1\n"})
		executeToolCall("write_file", w, ctx)
		d, _ := json.Marshal(map[string]string{"path": "gone.py"})
		noteMutationIntent(ctx, st, "delete_file", d)
		if !hasUnresolvedDebt(st) {
			t.Fatal("delete intent created no debt")
		}
		executeToolCall("delete_file", d, ctx)
		settleMutationDebt(ctx, st)
		if hasUnresolvedDebt(st) {
			paths, _ := unresolvedDebtPaths(st, 5)
			t.Errorf("a demonstrated delete did not resolve: %v", paths)
		}
	})

	t.Run("failed delete of a file that is still there stays blocking", func(t *testing.T) {
		dir := t.TempDir()
		ctx := NewAgentContext(dir, Tier2Medium)
		ctx.PermissionMode, ctx.StreamFn = PermissionYolo, func(string, interface{}) {}
		st := &runState{}
		sub := filepath.Join(dir, "locked")
		os.MkdirAll(sub, 0o755)
		os.WriteFile(filepath.Join(sub, "keep.py"), []byte("A = 1\n"), 0o644)
		if err := os.Chmod(sub, 0o555); err != nil {
			t.Skip("cannot make a directory read-only here")
		}
		t.Cleanup(func() { os.Chmod(sub, 0o755) })

		d, _ := json.Marshal(map[string]string{"path": "locked/keep.py"})
		noteMutationIntent(ctx, st, "delete_file", d)
		executeToolCall("delete_file", d, ctx) // the file survives
		settleMutationDebt(ctx, st)
		if !hasUnresolvedDebt(st) {
			t.Error("a delete that left the file in place resolved its debt")
		}
	})

	t.Run("abandoning a path that never landed settles it", func(t *testing.T) {
		ctx := NewAgentContext(t.TempDir(), Tier2Medium)
		ctx.PermissionMode, ctx.StreamFn = PermissionYolo, func(string, interface{}) {}
		st := &runState{}
		// The fenced case: content debt on a path that was never produced.
		w, _ := json.Marshal(map[string]string{"path": "ghost.py", "content": "@fenced"})
		noteMutationIntent(ctx, st, "write_file", w)
		d, _ := json.Marshal(map[string]string{"path": "ghost.py"})
		noteMutationIntent(ctx, st, "delete_file", d) // converts to delete debt
		executeToolCall("delete_file", d, ctx)        // fails: it was never there
		settleMutationDebt(ctx, st)
		if hasUnresolvedDebt(st) {
			t.Error("an explicitly abandoned path that is demonstrably absent stayed blocking")
		}
	})

	t.Run("successful move needs source absence and destination bytes", func(t *testing.T) {
		dir := t.TempDir()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/syntax-check") {
				json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
				return
			}
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		ctx := NewAgentContext(dir, Tier2Medium)
		ctx.SandboxURL = srv.URL
		ctx.PermissionMode, ctx.StreamFn = PermissionYolo, func(string, interface{}) {}
		st := &runState{}
		w, _ := json.Marshal(map[string]string{"path": "old.py", "content": "A = 1\n"})
		executeToolCall("write_file", w, ctx)
		m, _ := json.Marshal(map[string]string{"source": "old.py", "destination": "new.py"})
		noteMutationIntent(ctx, st, "move_file", m)
		if !hasUnresolvedDebt(st) {
			t.Fatal("move intent created no debt")
		}
		executeToolCall("move_file", m, ctx)
		settleMutationDebt(ctx, st)
		// The destination is observed with an unknown verdict by design — a
		// rename earns no evidence under the new name — so the demonstration
		// is read from disk through the same contract every deliverable uses.
		// These bytes pass it, so the move is settled without the model having
		// to rewrite a file it only moved.
		if hasUnresolvedDebt(st) {
			paths, _ := unresolvedDebtPaths(st, 5)
			t.Errorf("a demonstrated move did not resolve: %v", paths)
		}
		// And a destination that cannot be demonstrated leaves it owed.
		bad, _ := json.Marshal(map[string]string{"source": "new.py", "destination": "gone.py"})
		noteMutationIntent(ctx, st, "move_file", bad)
		executeToolCall("move_file", bad, ctx)
		os.Remove(filepath.Join(dir, "gone.py"))
		settleMutationDebt(ctx, st)
		if !hasUnresolvedDebt(st) {
			t.Error("a move whose destination is not there resolved anyway")
		}
	})

	t.Run("separate paths are independent", func(t *testing.T) {
		dir := t.TempDir()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/syntax-check") {
				json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
				return
			}
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		ctx := NewAgentContext(dir, Tier2Medium)
		ctx.SandboxURL = srv.URL
		ctx.PermissionMode, ctx.StreamFn = PermissionYolo, func(string, interface{}) {}
		st := &runState{}
		for _, p := range []string{"a.py", "b.py"} {
			args, _ := json.Marshal(map[string]string{"path": p, "content": "@fenced"})
			noteMutationIntent(ctx, st, "write_file", args)
		}
		if n := len(st.mutationDebt); n != 2 {
			t.Fatalf("%d debts for two paths", n)
		}
		w, _ := json.Marshal(map[string]string{"path": "a.py", "content": "A = 1\n"})
		executeToolCall("write_file", w, ctx)
		settleMutationDebt(ctx, st)
		paths, _ := unresolvedDebtPaths(st, 5)
		if len(paths) != 1 || paths[0] != "b.py" {
			t.Errorf("unresolved = %v, want only b.py", paths)
		}
	})
}

// The map is bounded, and past the ceiling it fails closed: no naming, still
// blocking.
func TestMutationDebtCeilingFailsClosed(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	st := &runState{}
	for i := 0; i < maxTrackedMutationDebt+25; i++ {
		args, _ := json.Marshal(map[string]string{
			"path": fmt.Sprintf("f%03d.py", i), "content": "@fenced"})
		noteMutationIntent(ctx, st, "write_file", args)
	}
	if len(st.mutationDebt) > maxTrackedMutationDebt {
		t.Errorf("map grew to %d, ceiling is %d", len(st.mutationDebt), maxTrackedMutationDebt)
	}
	if !st.debtOverflow {
		t.Error("the ceiling was reached without recording that it was")
	}
	if !hasUnresolvedDebt(st) {
		t.Fatal("overflow stopped blocking completion")
	}
	paths, more := unresolvedDebtPaths(st, 5)
	if len(paths) != 5 || !more {
		t.Errorf("disclosure = %d paths, more=%v; want a bounded list flagged as partial",
			len(paths), more)
	}
	if status, reason := finalizeCompletion(ctx, st, "Write the files.", ""); status.Completed() {
		t.Errorf("overflow allowed completion: reason=%q", reason)
	}
	if s := unresolvedDebtSummary(st); !strings.Contains(s, "Other files are in the same state") {
		t.Errorf("overflow is not disclosed:\n%s", s)
	}
}

// run_command and run_background keep their unobserved semantics and must not
// be approximated as one-path debt.
func TestCommandToolsCreateNoPathDebt(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	st := &runState{}
	for _, c := range []struct{ name, args string }{
		{"run_command", `{"command":"rm -rf build"}`},
		{"run_background", `{"command":"python app.py"}`},
		{"read_file", `{"path":"a.py"}`},
		{"search_files", `{"pattern":"def "}`},
	} {
		noteMutationIntent(ctx, st, c.name, json.RawMessage(c.args))
	}
	if hasUnresolvedDebt(st) {
		paths, _ := unresolvedDebtPaths(st, 5)
		t.Errorf("non-path-targeted tools created debt: %v", paths)
	}
}

// --- Bounded recovery for unresolved work -----------------------------------

const debtRecoveryMark = "never reached a state this run could check"

// THE CAUSAL FIXTURE. The scripted model retires the mistaken path only after
// it is told what is outstanding; on the parent it is never told.
func TestDebtRecoveryRetiresAMistakenPathAndCompletes(t *testing.T) {
	dir := t.TempDir()
	const req = "Write a couple of small scripts."
	var recoveries, step int
	var mu sync.Mutex

	ctx, _, census, terminal, _ := debtFixture(t, dir, req, debtCeiling,
		func(i int, prompt string) map[string]interface{} {
			sawRecovery := strings.Contains(prompt, debtRecoveryMark)
			switch {
			case i == 0 || i == 1:
				// a.py: the mistaken path, never lands.
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "a.py", "content": "@fenced"}}
			case i == 2:
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "b.py", "content": debtGoodBody}}
			case i == 3:
				return map[string]interface{}{"type": "tool_call", "name": "run_command",
					"args": map[string]string{"command": "python3 b.py"}}
			case !sawRecovery:
				return map[string]interface{}{"type": "done", "summary": "wrote what I could"}
			default:
				// Steps after the recovery are scripted, because the delete of
				// a path that never landed FAILS and leaves no marker in the
				// prompt to key on. The conditional part -- that the model
				// only abandons the path once it has been told -- is above.
				mu.Lock()
				step++
				n := step
				mu.Unlock()
				if n == 1 {
					return map[string]interface{}{"type": "tool_call", "name": "delete_file",
						"args": map[string]string{"path": "a.py"}}
				}
				return map[string]interface{}{"type": "done", "summary": "b.py is written and runs"}
			}
		})
	inner := ctx.StreamFn
	ctx.StreamFn = func(et string, data interface{}) {
		if et == "gate" {
			if b, _ := json.Marshal(data); strings.Contains(string(b), debtRecoveryMark) {
				mu.Lock()
				recoveries++
				mu.Unlock()
			}
		}
		inner(et, data)
	}
	if err := runAgentLoop(ctx, req); err != nil {
		t.Fatalf("loop error: %v", err)
	}
	bBytes, _ := os.ReadFile(filepath.Join(dir, "b.py"))
	_, aErr := os.Stat(filepath.Join(dir, "a.py"))
	t.Logf("recoveries=%d status=%q reason=%q a.py_absent=%v",
		recoveries, terminal["status"], terminal["reason"], os.IsNotExist(aErr))

	if recoveries != 1 {
		t.Fatalf("recovery fired %d times, want exactly 1", recoveries)
	}
	if terminal["status"] != string(TerminalCompleted) {
		t.Fatalf("structured retirement did not complete: status=%q reason=%q summary=%.140s",
			terminal["status"], terminal["reason"], terminal["summary"])
	}
	if !os.IsNotExist(aErr) {
		t.Error("a.py was not confirmed absent")
	}
	if string(bBytes) != debtGoodBody {
		t.Errorf("b.py = %q", bBytes)
	}
	d := ctx.Ledger[ledgerKey(ctx, "b.py")]
	if d == nil || d.CurrentHash != hashBytes(bBytes) {
		t.Error("b.py's ledger hash does not match disk")
	}
	if k, s := d.CurrentValidation(); s != ValidationPassed || k != ValidationKindSyntax {
		t.Errorf("completion over %v/%v", k, s)
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
	if census["tool_call"] != census["tool_result"] {
		t.Errorf("call/result balance: %d vs %d", census["tool_call"], census["tool_result"])
	}
}

// A user-required path stays required even when the model deletes it.
func TestDeletingAUserRequiredPathDoesNotComplete(t *testing.T) {
	dir := t.TempDir()
	const req = "Write a.py and b.py."
	ctx, _, _, terminal, _ := debtFixture(t, dir, req, debtCeiling,
		func(i int, prompt string) map[string]interface{} {
			sawRecovery := strings.Contains(prompt, debtRecoveryMark)
			retired := strings.Contains(prompt, `"deleted":true`)
			switch {
			case i == 0 || i == 1:
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "a.py", "content": "@fenced"}}
			case i == 2:
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "b.py", "content": debtGoodBody}}
			case !sawRecovery:
				return map[string]interface{}{"type": "done", "summary": "did what I could"}
			case !retired:
				return map[string]interface{}{"type": "tool_call", "name": "delete_file",
					"args": map[string]string{"path": "a.py"}}
			default:
				return map[string]interface{}{"type": "done", "summary": "b.py is written"}
			}
		})
	if err := runAgentLoop(ctx, req); err != nil {
		t.Fatal(err)
	}
	t.Logf("required-path deletion: status=%q reason=%q", terminal["status"], terminal["reason"])
	if terminal["status"] == string(TerminalCompleted) {
		t.Fatal("deleting a user-required output completed the run")
	}
	if terminal["reason"] != "deliverables_not_demonstrated" {
		t.Errorf("reason = %q, want the prompt obligation to block first", terminal["reason"])
	}
}

// Ignoring the recovery, budget, boundedness, and prose.
func TestDebtRecoveryBoundsAndRefusals(t *testing.T) {
	// b.py lands so the unmet-action clause is satisfied and debt is the only
	// thing left blocking: that isolates what this test is about.
	ignore := func(i int, prompt string) map[string]interface{} {
		switch i {
		case 0, 1:
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "a.py", "content": "@fenced"}}
		case 2:
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "b.py", "content": debtGoodBody}}
		}
		return map[string]interface{}{"type": "done",
			"summary": "I no longer need a.py, so the work is complete."}
	}

	t.Run("ignored recovery stops honestly", func(t *testing.T) {
		dir := t.TempDir()
		ctx, _, census, terminal, _ := debtFixture(t, dir, "Write a couple of scripts.", debtCeiling, ignore)
		if err := runAgentLoop(ctx, "Write a couple of scripts."); err != nil {
			t.Fatal(err)
		}
		t.Logf("ignored: status=%q reason=%q", terminal["status"], terminal["reason"])
		if terminal["status"] == string(TerminalCompleted) {
			t.Fatal("prose retired the work")
		}
		if terminal["reason"] != "unresolved_mutation_debt" {
			t.Errorf("reason = %q", terminal["reason"])
		}
		if completionClaimIn(terminal["summary"]) != "" {
			t.Errorf("the terminal claimed success:\n%s", terminal["summary"])
		}
		if census["done"] != 1 {
			t.Errorf("%d terminal events", census["done"])
		}
		// Nothing was run or mutated on the model's behalf.
		if _, err := os.Stat(filepath.Join(dir, "a.py")); err == nil {
			t.Error("recovery created the file itself")
		}
	})

	t.Run("offered once per generation and bounded overall", func(t *testing.T) {
		ctx := NewAgentContext(t.TempDir(), Tier2Medium)
		st := &runState{}
		args, _ := json.Marshal(map[string]string{"path": "a.py", "content": "@fenced"})
		noteMutationIntent(ctx, st, "write_file", args)
		if offerDebtRecovery(ctx, st) == "" {
			t.Fatal("no first offer")
		}
		if offerDebtRecovery(ctx, st) != "" {
			t.Error("a second offer in the same generation")
		}
		// New unresolved work opens the next generation.
		args2, _ := json.Marshal(map[string]string{"path": "b.py", "content": "@fenced"})
		noteMutationIntent(ctx, st, "write_file", args2)
		if offerDebtRecovery(ctx, st) == "" {
			t.Error("new unresolved work earned no offer")
		}
		// ...but not without bound.
		args3, _ := json.Marshal(map[string]string{"path": "c.py", "content": "@fenced"})
		noteMutationIntent(ctx, st, "write_file", args3)
		if offerDebtRecovery(ctx, st) != "" {
			t.Errorf("offers exceeded the cap of %d", maxDebtRecoveries)
		}
	})

	t.Run("low budget skips recovery", func(t *testing.T) {
		ctx := NewAgentContext(t.TempDir(), Tier2Medium)
		workCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		ctx.Ctx = workCtx
		st := &runState{}
		args, _ := json.Marshal(map[string]string{"path": "a.py", "content": "@fenced"})
		noteMutationIntent(ctx, st, "write_file", args)
		if offerDebtRecovery(ctx, st) != "" {
			t.Error("recovery ran with less budget than it needs to be acted on")
		}
		if status, _ := finalizeCompletion(ctx, st, "Write a script.", ""); status.Completed() {
			t.Error("skipping recovery allowed completion")
		}
	})

	t.Run("same-path correction settles without deletion", func(t *testing.T) {
		dir := t.TempDir()
		var step2 int
		var stepMu sync.Mutex
		const req = "Write a couple of small scripts."
		ctx, _, _, terminal, _ := debtFixture(t, dir, req, debtCeiling,
			func(i int, prompt string) map[string]interface{} {
				sawRecovery := strings.Contains(prompt, debtRecoveryMark)
				switch {
				case i == 0 || i == 1:
					return map[string]interface{}{"type": "tool_call", "name": "write_file",
						"args": map[string]string{"path": "a.py", "content": "@fenced"}}
				case i == 2:
					// Something has to land, or the action gate owns the loop
					// and the debt recovery is never reached — that gate is
					// the more specific failure and takes precedence.
					return map[string]interface{}{"type": "tool_call", "name": "write_file",
						"args": map[string]string{"path": "b.py", "content": debtGoodBody}}
				case !sawRecovery:
					return map[string]interface{}{"type": "done", "summary": "tried"}
				default:
					// Scripted after the recovery, for the same reason as the
					// causal fixture above.
					stepMu.Lock()
					step2++
					n := step2
					stepMu.Unlock()
					if n == 1 {
						return map[string]interface{}{"type": "tool_call", "name": "write_file",
							"args": map[string]string{"path": "a.py", "content": debtGoodBody}}
					}
					return map[string]interface{}{"type": "done", "summary": "a.py is written"}
				}
			})
		if err := runAgentLoop(ctx, req); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(filepath.Join(dir, "a.py"))
		t.Logf("corrected: status=%q reason=%q a.py=%q",
			terminal["status"], terminal["reason"], string(got))
		if terminal["status"] != string(TerminalCompleted) {
			t.Errorf("a corrected path did not complete: status=%q reason=%q",
				terminal["status"], terminal["reason"])
		}
	})
}

// --- Live workspace hazards at completion ------------------------------------
//
// run_background raises the hazard and only a confirmed exit lowers it, but the
// completion decision never asked. A server started mid-run could keep
// rewriting a tracked deliverable while the run reported completed over a hash
// taken at one instant.

// bgSandbox stands in for the sandbox's job endpoints. `state` decides what
// /jobs/{id}/output reports, and `onStop` records reaping.
type bgSandbox struct {
	mu       sync.Mutex
	running  bool
	started  int
	exitCode *int
	tailed   int
	stopped  []string
	// mutate rewrites the deliverable when the job is observed as exited,
	// standing in for a process that changed the file on its way out.
	mutate func()
}

func newBgSandbox(t *testing.T, dir string, bg *bgSandbox) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			valid := !strings.Contains(in.Code, "]]")
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{"SyntaxError: unmatched ']'"}
			}
			json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(r.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
		case strings.HasSuffix(r.URL.Path, "/jobs/start"):
			// Unique per start, as the real sandbox does: the hazard is
			// raised per start and lowered per reaped job, so a stub reusing
			// one id would manufacture a mismatch production does not have.
			bg.mu.Lock()
			bg.started++
			id := fmt.Sprintf("job%d", bg.started)
			bg.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]interface{}{"job_id": id, "pid": 4242})
		case strings.Contains(r.URL.Path, "/output"):
			bg.mu.Lock()
			bg.tailed++
			running, code, mutate := bg.running, bg.exitCode, bg.mutate
			bg.mu.Unlock()
			if !running && code != nil && mutate != nil {
				mutate() // the process changed the file on its way out
			}
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/jobs/"), "/output")
			out := map[string]interface{}{"job_id": id, "running": running,
				"stdout": []string{}, "stderr": []string{}, "elapsed_sec": 1.0,
				"command": "python app.py"}
			if code != nil {
				out["exit_code"] = *code
			}
			json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(r.URL.Path, "/stop"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/jobs/"), "/stop")
			bg.mu.Lock()
			bg.stopped = append(bg.stopped, id)
			code := bg.exitCode
			bg.mu.Unlock()
			out := map[string]interface{}{"job_id": id, "killed": true,
				"stdout": []string{}, "stderr": []string{}}
			if code != nil {
				out["exit_code"] = *code
			}
			json.NewEncoder(w).Encode(out)
		default:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func bgCtx(t *testing.T, dir, url string) *AgentContext {
	t.Helper()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.SandboxURL = url
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(string, interface{}) {}
	return ctx
}

func TestLiveBackgroundWorkBlocksCompletion(t *testing.T) {
	const good = "def solve():\n    return 1\n"
	zero := 0

	t.Run("live job blocks", func(t *testing.T) {
		dir := t.TempDir()
		bg := &bgSandbox{running: true}
		srv := newBgSandbox(t, dir, bg)
		ctx := bgCtx(t, dir, srv.URL)
		w, _ := json.Marshal(map[string]string{"path": "solve.py", "content": good})
		executeToolCall("write_file", w, ctx)
		start, _ := json.Marshal(map[string]string{"command": "python app.py"})
		executeToolCall("run_background", start, ctx)
		if !workspaceHazardous(ctx) {
			t.Fatal("run_background did not raise the hazard")
		}
		st := &runState{madeProductiveChange: true, expectedOutputs: []string{"solve.py"}}
		status, reason := finalizeCompletion(ctx, st, "Create solve.py.", "")
		t.Logf("live: status=%q reason=%q tails=%d stops=%v",
			status, reason, bg.tailed, bg.stopped)
		if status.Completed() {
			t.Fatalf("completed with a live background job: reason=%q", reason)
		}
		if reason != "background_work_unresolved" {
			t.Errorf("reason = %q", reason)
		}
		// Nothing was killed to make a decision.
		if len(bg.stopped) != 0 {
			t.Errorf("a live job was stopped during ordinary completion: %v", bg.stopped)
		}
		if len(ctx.BackgroundJobs) != 1 {
			t.Errorf("the live job stopped being tracked")
		}
	})

	t.Run("exited and unchanged: reaped, may complete", func(t *testing.T) {
		dir := t.TempDir()
		// The job must be LIVE when it starts, or run_background treats it as
		// a job that died at startup and never tracks it.
		bg := &bgSandbox{running: true}
		srv := newBgSandbox(t, dir, bg)
		ctx := bgCtx(t, dir, srv.URL)
		w, _ := json.Marshal(map[string]string{"path": "solve.py", "content": good})
		executeToolCall("write_file", w, ctx)
		start, _ := json.Marshal(map[string]string{"command": "python app.py"})
		executeToolCall("run_background", start, ctx)
		bg.mu.Lock()
		bg.running, bg.exitCode = false, &zero // it has since exited
		bg.mu.Unlock()

		t.Logf("jobs tracked before: %v hazard=%v", ctx.BackgroundJobs, workspaceHazardous(ctx))
		st := &runState{madeProductiveChange: true, expectedOutputs: []string{"solve.py"}}
		status, reason := finalizeCompletion(ctx, st, "Create solve.py.", "")
		t.Logf("exited: status=%q reason=%q stops=%v tails=%d jobs=%v hazard=%v",
			status, reason, bg.stopped, bg.tailed, ctx.BackgroundJobs, workspaceHazardous(ctx))
		if !status.Completed() {
			t.Fatalf("an exited, reaped job blocked completion: reason=%q", reason)
		}
		if len(bg.stopped) != 1 {
			t.Errorf("the exited job was not reaped: %v", bg.stopped)
		}
		if workspaceHazardous(ctx) {
			t.Error("the hazard survived a confirmed exit")
		}
		d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
		if _, s := d.CurrentValidation(); s != ValidationPassed {
			t.Errorf("an untouched file lost its verdict: %v", s)
		}
	})

	t.Run("exited after changing the file: old verdict cannot authorize", func(t *testing.T) {
		dir := t.TempDir()
		bg := &bgSandbox{running: true}
		srv := newBgSandbox(t, dir, bg)
		ctx := bgCtx(t, dir, srv.URL)
		w, _ := json.Marshal(map[string]string{"path": "solve.py", "content": good})
		executeToolCall("write_file", w, ctx)
		before := ctx.Ledger[ledgerKey(ctx, "solve.py")].CurrentHash
		start, _ := json.Marshal(map[string]string{"command": "python app.py"})
		executeToolCall("run_background", start, ctx)
		// It has since exited, and rewrote the file on its way out.
		bg.mu.Lock()
		bg.running, bg.exitCode = false, &zero
		bg.mutate = func() {
			os.WriteFile(filepath.Join(dir, "solve.py"), []byte("def solve():\n    return [1]]\n"), 0o644)
		}
		bg.mu.Unlock()

		st := &runState{madeProductiveChange: true, expectedOutputs: []string{"solve.py"}}
		status, reason := finalizeCompletion(ctx, st, "Create solve.py.", "")
		d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
		_, s := d.CurrentValidation()
		t.Logf("changed-on-exit: status=%q reason=%q hash_moved=%v current=%v",
			status, reason, d.CurrentHash != before, s)
		if d.CurrentHash == before {
			t.Fatal("the rehash did not notice the change")
		}
		if s == ValidationPassed {
			t.Error("a verdict about the old bytes survived")
		}
		if status.Completed() {
			t.Errorf("completed over bytes a background job changed: reason=%q", reason)
		}
	})

	t.Run("unconfirmed exit stays blocking", func(t *testing.T) {
		dir := t.TempDir()
		bg := &bgSandbox{running: true}
		srv := newBgSandbox(t, dir, bg)
		ctx := bgCtx(t, dir, srv.URL)
		w, _ := json.Marshal(map[string]string{"path": "solve.py", "content": good})
		executeToolCall("write_file", w, ctx)
		start, _ := json.Marshal(map[string]string{"command": "python app.py"})
		executeToolCall("run_background", start, ctx)
		// Signalled but never reaped: no exit code to confirm it is gone.
		bg.mu.Lock()
		bg.running, bg.exitCode = false, nil
		bg.mu.Unlock()
		st := &runState{madeProductiveChange: true, expectedOutputs: []string{"solve.py"}}
		status, reason := finalizeCompletion(ctx, st, "Create solve.py.", "")
		t.Logf("unconfirmed: status=%q reason=%q", status, reason)
		if status.Completed() {
			t.Errorf("an unconfirmed exit completed: reason=%q", reason)
		}
		if reason != "background_work_unresolved" {
			t.Errorf("reason = %q", reason)
		}
	})

	t.Run("unobservable job stays blocking", func(t *testing.T) {
		dir := t.TempDir()
		ctx := bgCtx(t, dir, "http://127.0.0.1:1")
		ctx.BackgroundJobs = map[string]string{"job1": "python app.py"}
		raiseWorkspaceHazard(ctx, "job1")
		st := &runState{madeProductiveChange: true}
		status, reason := finalizeCompletion(ctx, st, "Create solve.py.", "")
		if status.Completed() || reason != "background_work_unresolved" {
			t.Errorf("status=%q reason=%q", status, reason)
		}
	})

	t.Run("sessions do not share hazards", func(t *testing.T) {
		dir := t.TempDir()
		bg := &bgSandbox{running: true}
		srv := newBgSandbox(t, dir, bg)
		busy := bgCtx(t, dir, srv.URL)
		start, _ := json.Marshal(map[string]string{"command": "python app.py"})
		executeToolCall("run_background", start, busy)
		if !workspaceHazardous(busy) {
			t.Fatal("no hazard on the busy session")
		}
		other := bgCtx(t, t.TempDir(), srv.URL)
		if workspaceHazardous(other) {
			t.Error("a second session inherited the hazard")
		}
		if live := settleBackgroundHazard(other); len(live) != 0 {
			t.Errorf("a session with no jobs of its own saw %v", live)
		}
	})

	t.Run("foreground-only work is unchanged", func(t *testing.T) {
		dir := t.TempDir()
		bg := &bgSandbox{}
		srv := newBgSandbox(t, dir, bg)
		ctx := bgCtx(t, dir, srv.URL)
		w, _ := json.Marshal(map[string]string{"path": "solve.py", "content": good})
		executeToolCall("write_file", w, ctx)
		st := &runState{madeProductiveChange: true, expectedOutputs: []string{"solve.py"}}
		status, reason := finalizeCompletion(ctx, st, "Create solve.py.", "")
		if !status.Completed() {
			t.Errorf("a run with no background work stopped completing: reason=%q", reason)
		}
		if bg.tailed != 0 {
			t.Errorf("the sandbox was polled for jobs that do not exist (%d)", bg.tailed)
		}
	})
}

// The hazard counter and the job map must never disagree about whether work is
// outstanding. Both are mutated only from the agent-loop goroutine -- the one
// `go func` in the loop is the prompt-progress poller and touches neither --
// so this exercises the production sequence rather than manufacturing a
// concurrency pattern production does not have. Run under -race.
func TestHazardAndJobReapingStayConsistent(t *testing.T) {
	zero := 0
	dir := t.TempDir()
	bg := &bgSandbox{running: true}
	srv := newBgSandbox(t, dir, bg)
	ctx := bgCtx(t, dir, srv.URL)
	w, _ := json.Marshal(map[string]string{"path": "solve.py", "content": "def s():\n    return 1\n"})
	executeToolCall("write_file", w, ctx)

	start, _ := json.Marshal(map[string]string{"command": "python app.py"})
	for i := 0; i < 3; i++ {
		executeToolCall("run_background", start, ctx)
	}
	st := &runState{madeProductiveChange: true, expectedOutputs: []string{"solve.py"}}

	// While anything is live, the two views agree that work is outstanding.
	for i := 0; i < 3; i++ {
		live := settleBackgroundHazard(ctx)
		if len(live) == 0 || !workspaceHazardous(ctx) {
			t.Fatalf("live=%v hazardous=%v — the two views disagree", live, workspaceHazardous(ctx))
		}
		if status, _ := finalizeCompletion(ctx, st, "Create solve.py.", ""); status.Completed() {
			t.Fatal("completed while a job was live")
		}
	}
	// Once the job is confirmed gone, both views agree it is settled, and
	// repeating the settle does not double-count.
	bg.mu.Lock()
	bg.running, bg.exitCode = false, &zero
	bg.mu.Unlock()
	for i := 0; i < 3; i++ {
		if live := settleBackgroundHazard(ctx); len(live) != 0 {
			t.Errorf("settle %d still reports %v", i, live)
		}
	}
	if workspaceHazardous(ctx) {
		t.Error("the hazard outlived every confirmed exit")
	}
	if len(ctx.BackgroundJobs) != 0 {
		t.Errorf("%d jobs still tracked", len(ctx.BackgroundJobs))
	}
	if status, reason := finalizeCompletion(ctx, st, "Create solve.py.", ""); !status.Completed() {
		t.Errorf("a fully reaped session did not complete: reason=%q", reason)
	}
}

// --- Non-code deliverables ---------------------------------------------------
//
// deliverablesDemonstrablyValid required a syntax PASS universally, so a valid
// notes.txt with exact current bytes could never complete. The naive fix --
// treating not_applicable as valid -- is unsafe, because an unsupported
// language reports not_applicable for an entirely different reason. The
// discriminator is the document set stripOneFenceLayer has always used.

func TestDocumentAssetClassification(t *testing.T) {
	for _, c := range []struct {
		path string
		want bool
		why  string
	}{
		{"notes.txt", true, "ordinary text"},
		{"README.md", true, "markdown document"},
		{"guide.markdown", true, "markdown document"},
		{"spec.rst", true, "restructured text"},
		{"solve.py", false, "recognized source"},
		{"lib.rs", false, "unsupported source, not prose"},
		{"main.c", false, "unsupported source"},
		{"app.jinja", false, "template with executable content"},
		{"data.json", false, "structured, has a parser"},
		{"config.yaml", false, "structured, has a parser"},
		{"page.html", false, "may carry embedded scripts"},
		{"weird.zzz", false, "unknown extension"},
		{"Makefile", false, "no extension, not prose"},
	} {
		if got := isDocumentAsset(c.path); got != c.want {
			t.Errorf("isDocumentAsset(%q) = %v, want %v (%s)", c.path, got, c.want, c.why)
		}
	}
}

func TestNonCodeDeliverableDemonstration(t *testing.T) {
	newCtx := func(t *testing.T) (*AgentContext, string) {
		dir := t.TempDir()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/syntax-check") {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			var in struct{ Code, Language string }
			json.NewDecoder(r.Body).Decode(&in)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"valid": !strings.Contains(in.Code, "]]")})
		}))
		t.Cleanup(srv.Close)
		ctx := NewAgentContext(dir, Tier2Medium)
		ctx.SandboxURL = srv.URL
		ctx.PermissionMode = PermissionYolo
		ctx.StreamFn = func(string, interface{}) {}
		return ctx, dir
	}

	write := func(t *testing.T, ctx *AgentContext, path, body string) *ToolResult {
		t.Helper()
		args, _ := json.Marshal(map[string]string{"path": path, "content": body})
		return executeToolCall("write_file", args, ctx)
	}

	t.Run("text file may demonstrate", func(t *testing.T) {
		ctx, _ := newCtx(t)
		res := write(t, ctx, "notes.txt", "ordinary notes\n")
		if res.ValidationKind != ValidationKindNone || res.ValidationStatus != ValidationNotApplicable {
			t.Fatalf("expected none/not_applicable, got %s/%s",
				res.ValidationKind, res.ValidationStatus)
		}
		if !deliverablesDemonstrablyValid(ctx, []string{"notes.txt"}) {
			t.Error("a current, ordinary text deliverable could not demonstrate")
		}
		// The internal record is untouched: no fake syntax pass.
		d := ctx.Ledger[ledgerKey(ctx, "notes.txt")]
		k, s := d.CurrentValidation()
		if k != ValidationKindNone || s != ValidationNotApplicable {
			t.Errorf("the ledger was relabelled: %v/%v", k, s)
		}
	})

	t.Run("markdown may demonstrate", func(t *testing.T) {
		ctx, _ := newCtx(t)
		write(t, ctx, "README.md", "# Title\n\nSome prose.\n")
		if !deliverablesDemonstrablyValid(ctx, []string{"README.md"}) {
			t.Error("a markdown document could not demonstrate")
		}
	})

	t.Run("unsupported code cannot", func(t *testing.T) {
		for _, p := range []string{"lib.rs", "main.c", "weird.zzz", "app.jinja"} {
			ctx, _ := newCtx(t)
			write(t, ctx, p, "fn main() { let x = 1; }\n")
			if deliverablesDemonstrablyValid(ctx, []string{p}) {
				t.Errorf("%s demonstrated completion with no applicable check", p)
			}
		}
	})

	t.Run("recognized code with the checker unavailable cannot", func(t *testing.T) {
		ctx, _ := newCtx(t)
		ctx.SandboxURL = "http://127.0.0.1:1"
		write(t, ctx, "solve.py", "A = 1\n")
		if deliverablesDemonstrablyValid(ctx, []string{"solve.py"}) {
			t.Error("an unchecked .py demonstrated completion")
		}
	})

	t.Run("structured format uses its parser", func(t *testing.T) {
		ctx, _ := newCtx(t)
		write(t, ctx, "data.json", "{\"a\": 1}\n")
		if !deliverablesDemonstrablyValid(ctx, []string{"data.json"}) {
			t.Error("valid json did not pass through its own parser")
		}
		ctx2, _ := newCtx(t)
		write(t, ctx2, "bad.json", "{\"a\": [1, 2]]}\n")
		if deliverablesDemonstrablyValid(ctx2, []string{"bad.json"}) {
			t.Error("invalid json demonstrated completion")
		}
	})

	t.Run("bytes changed after observation cannot", func(t *testing.T) {
		ctx, dir := newCtx(t)
		write(t, ctx, "notes.txt", "ordinary notes\n")
		os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("changed behind us\n"), 0o644)
		if deliverablesDemonstrablyValid(ctx, []string{"notes.txt"}) {
			t.Error("a stale record demonstrated completion")
		}
	})

	t.Run("a path the session never wrote cannot", func(t *testing.T) {
		ctx, dir := newCtx(t)
		os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("pre-existing\n"), 0o644)
		if deliverablesDemonstrablyValid(ctx, []string{"notes.txt"}) {
			t.Error("a file the session never owned demonstrated completion")
		}
	})

	t.Run("unmet verification and outstanding work still block", func(t *testing.T) {
		ctx, _ := newCtx(t)
		write(t, ctx, "notes.txt", "ordinary notes\n")
		st := &runState{madeProductiveChange: true, expectedOutputs: []string{"notes.txt"}}
		// Requested verification, never satisfied.
		st.userWantsVerification = true
		if status, reason := finalizeCompletion(ctx, st, "Write notes.txt and verify it.", ""); status.Completed() {
			t.Errorf("completed with verification unmet: reason=%q", reason)
		}
		// Outstanding work elsewhere.
		st2 := &runState{madeProductiveChange: true, expectedOutputs: []string{"notes.txt"}}
		args, _ := json.Marshal(map[string]string{"path": "other.py", "content": "@fenced"})
		noteMutationIntent(ctx, st2, "write_file", args)
		if status, reason := finalizeCompletion(ctx, st2, "Write the files.", ""); status.Completed() {
			t.Errorf("completed with unresolved work: reason=%q", reason)
		}
	})
}

// The production shape, through the real loop.
func TestNonCodeDeliverableCompletesThroughTheLoop(t *testing.T) {
	for _, c := range []struct {
		name, path, body string
		wantCompleted    bool
	}{
		{"ordinary text", "notes.txt", "the notes you asked for\n", true},
		{"unsupported code", "lib.rs", "fn main() { let x = 1; }\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			const req = "Write the file."
			ctx, _, _, terminal, _ := debtFixture(t, dir, req, debtCeiling,
				func(i int, _ string) map[string]interface{} {
					if i == 0 {
						return map[string]interface{}{"type": "tool_call", "name": "write_file",
							"args": map[string]string{"path": c.path, "content": c.body}}
					}
					return map[string]interface{}{"type": "done", "summary": "wrote it"}
				})
			if err := runAgentLoop(ctx, req); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(filepath.Join(dir, c.path))
			d := ctx.Ledger[ledgerKey(ctx, c.path)]
			t.Logf("%s: status=%q reason=%q hash_matches=%v",
				c.name, terminal["status"], terminal["reason"],
				d != nil && d.CurrentHash == hashBytes(got))
			completed := terminal["status"] == string(TerminalCompleted)
			if completed != c.wantCompleted {
				t.Errorf("completed=%v, want %v (reason=%q)", completed, c.wantCompleted, terminal["reason"])
			}
			if c.wantCompleted {
				if d == nil || d.CurrentHash != hashBytes(got) {
					t.Error("the ledger hash does not match disk")
				}
				if k, s := d.CurrentValidation(); k != ValidationKindNone || s != ValidationNotApplicable {
					t.Errorf("the internal record was relabelled: %v/%v", k, s)
				}
			}
		})
	}
}

// --- Background hazard lifecycle ---------------------------------------------
//
// The hazard rose per start ATTEMPT and fell only when a registered job was
// reaped, so a start that registered no job raised one nothing could lower.
// Once completion began consulting it, that session could never finish.
// Hazards are owned by job identity now.

// bgStartSandbox scripts the job endpoints deterministically: `startFails`
// makes /jobs/start error, `startRunning` decides what the settle-window tail
// reports, and `jobID` lets a stub return a duplicate id on purpose.
type bgStartSandbox struct {
	mu           sync.Mutex
	startFails   bool
	startRunning bool
	jobID        string
	started      int
	stopped      []string
	mutate       func()
}

func newBgStartSandbox(t *testing.T, dir string, bg *bgStartSandbox) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"valid": !strings.Contains(in.Code, "]]")})
		case strings.HasSuffix(r.URL.Path, "/jobs/start"):
			bg.mu.Lock()
			fails, id := bg.startFails, bg.jobID
			bg.started++
			if id == "" {
				id = fmt.Sprintf("job%d", bg.started)
			}
			bg.mu.Unlock()
			if fails {
				http.Error(w, "no slots", http.StatusServiceUnavailable)
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"job_id": id, "pid": 4242})
		case strings.Contains(r.URL.Path, "/output"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/jobs/"), "/output")
			bg.mu.Lock()
			running, mutate := bg.startRunning, bg.mutate
			bg.mu.Unlock()
			if !running && mutate != nil {
				mutate()
			}
			out := map[string]interface{}{"job_id": id, "running": running,
				"stdout": []string{}, "stderr": []string{}, "elapsed_sec": 0.1,
				"command": "python app.py"}
			if !running {
				zero := 0
				out["exit_code"] = zero
			}
			json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(r.URL.Path, "/stop"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/jobs/"), "/stop")
			bg.mu.Lock()
			bg.stopped = append(bg.stopped, id)
			bg.mu.Unlock()
			zero := 0
			json.NewEncoder(w).Encode(map[string]interface{}{"job_id": id,
				"killed": true, "exit_code": zero, "stdout": []string{}, "stderr": []string{}})
		default:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func bgStartCtx(t *testing.T, dir, url string) *AgentContext {
	t.Helper()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.SandboxURL = url
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.StreamFn = func(string, interface{}) {}
	return ctx
}

func TestBackgroundHazardLifecycle(t *testing.T) {
	const good = "def solve():\n    return 1\n"
	seed := func(t *testing.T, ctx *AgentContext) {
		t.Helper()
		w, _ := json.Marshal(map[string]string{"path": "solve.py", "content": good})
		if res := executeToolCall("write_file", w, ctx); res.ValidationStatus != ValidationPassed {
			t.Fatalf("seed did not validate: %s", res.ValidationStatus)
		}
	}
	start := func(ctx *AgentContext) *ToolResult {
		args, _ := json.Marshal(map[string]string{"command": "python app.py"})
		return executeToolCall("run_background", args, ctx)
	}
	st := func() *runState {
		return &runState{madeProductiveChange: true, expectedOutputs: []string{"solve.py"}}
	}

	// A. Definitively no job: a refusal the tool makes before dispatch.
	t.Run("A definitive failed start leaves no hazard", func(t *testing.T) {
		dir := t.TempDir()
		bg := &bgStartSandbox{}
		srv := newBgStartSandbox(t, dir, bg)
		ctx := bgStartCtx(t, dir, srv.URL)
		seed(t, ctx)
		// An empty command is refused locally: nothing is dispatched.
		args, _ := json.Marshal(map[string]string{"command": "   "})
		if res := executeToolCall("run_background", args, ctx); res.Success {
			t.Fatal("an empty command was accepted")
		}
		if workspaceHazardous(ctx) {
			t.Fatal("a refusal that never dispatched left a hazard")
		}
		if len(ctx.BackgroundJobs) != 0 {
			t.Error("a job was invented")
		}
		if len(bg.stopped) != 0 {
			t.Error("something was reaped")
		}
		status, reason := finalizeCompletion(ctx, st(), "Create solve.py.", "")
		if !status.Completed() {
			t.Errorf("a valid deliverable could not complete: reason=%q", reason)
		}
	})

	// B. A failed attempt beside a live job changes nothing about the live one.
	t.Run("B failed attempt neither adds nor removes", func(t *testing.T) {
		dir := t.TempDir()
		bg := &bgStartSandbox{startRunning: true}
		srv := newBgStartSandbox(t, dir, bg)
		ctx := bgStartCtx(t, dir, srv.URL)
		seed(t, ctx)
		start(ctx) // job1, live
		if len(ctx.WorkspaceHazards) != 1 {
			t.Fatalf("hazards after one live start: %v", ctx.WorkspaceHazards)
		}
		args, _ := json.Marshal(map[string]string{"command": ""})
		executeToolCall("run_background", args, ctx)
		if len(ctx.WorkspaceHazards) != 1 {
			t.Errorf("a refused attempt changed the hazard set: %v", ctx.WorkspaceHazards)
		}
		if status, reason := finalizeCompletion(ctx, st(), "Create solve.py.", ""); status.Completed() {
			t.Fatalf("completed with job1 live: reason=%q", reason)
		}
		// A settles; nothing else is outstanding.
		bg.mu.Lock()
		bg.startRunning = false
		bg.mu.Unlock()
		if status, reason := finalizeCompletion(ctx, st(), "Create solve.py.", ""); !status.Completed() {
			t.Errorf("after A exited and settled: reason=%q", reason)
		}
	})

	// C. Dispatched and already gone by the settle window.
	t.Run("C immediate exit without mutation settles at once", func(t *testing.T) {
		dir := t.TempDir()
		bg := &bgStartSandbox{startRunning: false}
		srv := newBgStartSandbox(t, dir, bg)
		ctx := bgStartCtx(t, dir, srv.URL)
		seed(t, ctx)
		before := ctx.Ledger[ledgerKey(ctx, "solve.py")].CurrentHash
		start(ctx)
		if workspaceHazardous(ctx) {
			t.Fatalf("an already-exited job left a lasting hazard: %v", ctx.WorkspaceHazards)
		}
		d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
		if d.CurrentHash != before {
			t.Error("an untouched file was re-recorded")
		}
		if _, s := d.CurrentValidation(); s != ValidationPassed {
			t.Errorf("an untouched file lost its verdict: %v", s)
		}
		if status, reason := finalizeCompletion(ctx, st(), "Create solve.py.", ""); !status.Completed() {
			t.Errorf("could not complete after an immediate exit: reason=%q", reason)
		}
	})

	// D. Same, but it changed the file on its way out.
	t.Run("D immediate exit after mutation invalidates", func(t *testing.T) {
		dir := t.TempDir()
		bg := &bgStartSandbox{startRunning: false}
		srv := newBgStartSandbox(t, dir, bg)
		ctx := bgStartCtx(t, dir, srv.URL)
		seed(t, ctx)
		before := ctx.Ledger[ledgerKey(ctx, "solve.py")].CurrentHash
		bg.mu.Lock()
		bg.mutate = func() {
			os.WriteFile(filepath.Join(dir, "solve.py"), []byte("def solve():\n    return [1]]\n"), 0o644)
		}
		bg.mu.Unlock()
		start(ctx)
		d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
		if d.CurrentHash == before {
			t.Fatal("the rehash did not notice the change")
		}
		if _, s := d.CurrentValidation(); s == ValidationPassed {
			t.Error("a verdict about the old bytes survived")
		}
		if status, reason := finalizeCompletion(ctx, st(), "Create solve.py.", ""); status.Completed() {
			t.Errorf("completed over changed bytes: reason=%q", reason)
		}
	})

	// E. Two live jobs, two owners.
	t.Run("E multiple jobs are independently owned", func(t *testing.T) {
		dir := t.TempDir()
		bg := &bgStartSandbox{startRunning: true}
		srv := newBgStartSandbox(t, dir, bg)
		ctx := bgStartCtx(t, dir, srv.URL)
		seed(t, ctx)
		start(ctx)
		start(ctx)
		if len(ctx.WorkspaceHazards) != 2 {
			t.Fatalf("hazards: %v", ctx.WorkspaceHazards)
		}
		stop, _ := json.Marshal(map[string]string{"job_id": "job1"})
		executeToolCall("stop_background", stop, ctx)
		if len(ctx.WorkspaceHazards) != 1 {
			t.Errorf("reaping one cleared %v", ctx.WorkspaceHazards)
		}
		if !workspaceHazardous(ctx) {
			t.Error("the second job stopped blocking")
		}
		// Idempotent.
		executeToolCall("stop_background", stop, ctx)
		if len(ctx.WorkspaceHazards) != 1 {
			t.Errorf("a repeated reap changed the set: %v", ctx.WorkspaceHazards)
		}
		stop2, _ := json.Marshal(map[string]string{"job_id": "job2"})
		executeToolCall("stop_background", stop2, ctx)
		if workspaceHazardous(ctx) {
			t.Errorf("hazards after reaping both: %v", ctx.WorkspaceHazards)
		}
	})

	// F. A duplicate id is one job, however often it is seen.
	t.Run("F duplicate id cannot double-raise", func(t *testing.T) {
		dir := t.TempDir()
		bg := &bgStartSandbox{startRunning: true, jobID: "same"}
		srv := newBgStartSandbox(t, dir, bg)
		ctx := bgStartCtx(t, dir, srv.URL)
		seed(t, ctx)
		start(ctx)
		start(ctx)
		start(ctx)
		if len(ctx.WorkspaceHazards) != 1 {
			t.Fatalf("a duplicate id raised %v", ctx.WorkspaceHazards)
		}
		stop, _ := json.Marshal(map[string]string{"job_id": "same"})
		executeToolCall("stop_background", stop, ctx)
		if workspaceHazardous(ctx) {
			t.Errorf("one reap did not settle the duplicate: %v", ctx.WorkspaceHazards)
		}
	})

	// G. Dispatch may have happened and cannot be named.
	t.Run("G ambiguous dispatch fails closed", func(t *testing.T) {
		dir := t.TempDir()
		bg := &bgStartSandbox{startFails: true}
		srv := newBgStartSandbox(t, dir, bg)
		ctx := bgStartCtx(t, dir, srv.URL)
		seed(t, ctx)
		if res := start(ctx); res.Success {
			t.Fatal("the start unexpectedly succeeded")
		}
		if !workspaceHazardous(ctx) {
			t.Fatal("a possibly-dispatched start left no hazard")
		}
		if !ctx.WorkspaceHazards[hazardUnidentifiedJob] {
			t.Errorf("the hazard is not the unidentified one: %v", ctx.WorkspaceHazards)
		}
		// Nothing can reap it, and it keeps blocking.
		reapSessionBackgroundJobs(ctx)
		settleBackgroundHazard(ctx)
		if !workspaceHazardous(ctx) {
			t.Error("reaping nothing cleared an unidentified job")
		}
		status, reason := finalizeCompletion(ctx, st(), "Create solve.py.", "")
		if status.Completed() {
			t.Fatal("completed with a possibly-live unidentified process")
		}
		if reason != "background_work_unresolved" {
			t.Errorf("reason = %q", reason)
		}
	})

	// I. Sessions that never start anything do no job work at all.
	t.Run("I no background work, no job traffic", func(t *testing.T) {
		dir := t.TempDir()
		bg := &bgStartSandbox{}
		srv := newBgStartSandbox(t, dir, bg)
		ctx := bgStartCtx(t, dir, srv.URL)
		seed(t, ctx)
		status, reason := finalizeCompletion(ctx, st(), "Create solve.py.", "")
		if !status.Completed() {
			t.Errorf("reason=%q", reason)
		}
		bg.mu.Lock()
		defer bg.mu.Unlock()
		if bg.started != 0 || len(bg.stopped) != 0 {
			t.Errorf("job endpoints were touched: started=%d stopped=%v", bg.started, bg.stopped)
		}
	})
}

// --- Workspace-boundary refusals must be bounded -----------------------------
//
// The retained C1 session ran host-side against a workspace root that did not
// exist, so every byte-identical find_file was refused by the boundary check
// before dispatch. That branch bounced, incremented consecutiveErrors, and
// continued -- past the counter's only reader, past the retry ban and past the
// repeat detector -- so 60 identical calls produced no intervention, no log
// line and no terminal of its own. This is the fourth early-refusal branch
// with that shape; the per-path ban, the retry ban and the fenced bounce were
// the first three.

const wsCeiling = 30

// wsRefusalFixture drives the real loop with MaxTurns=0 against a workspace
// root that cannot be opened. The ceiling lives in the server, so nothing
// sleeps and an unbounded loop fails immediately.
func wsRefusalFixture(t *testing.T, tool string, args map[string]interface{}) (
	*AgentContext, *int, map[string]int, map[string]string, string) {
	t.Helper()
	parent := t.TempDir()
	dir := filepath.Join(parent, "missing-root") // never created
	turns := 0
	census := map[string]int{}
	terminal := map[string]string{}
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		i := turns
		turns++
		mu.Unlock()
		if i >= wsCeiling {
			http.Error(w, "turn ceiling exceeded", http.StatusInsufficientStorage)
			return
		}
		call, _ := json.Marshal(map[string]interface{}{
			"type": "tool_call", "name": tool, "args": args})
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(call)}}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	t.Cleanup(srv.Close)

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.MaxTurns = 0
	firstRefusal := ""
	ctx.StreamFn = func(et string, data interface{}) {
		b, _ := json.Marshal(data)
		mu.Lock()
		defer mu.Unlock()
		census[et]++
		if et == "tool_result" && firstRefusal == "" {
			firstRefusal = string(b)
		}
		if et == "done" {
			var m map[string]string
			json.Unmarshal(b, &m)
			for k, v := range m {
				terminal[k] = v
			}
		}
	}
	runAgentLoop(ctx, "Add a /health route to app.py.")
	return ctx, &turns, census, terminal, firstRefusal
}

// The retained C1 shape, exactly.
func TestRepeatedWorkspaceRefusalIsBounded(t *testing.T) {
	ctx, turns, census, terminal, refusal := wsRefusalFixture(t, "find_file",
		map[string]interface{}{"path": "app.py"})

	t.Logf("turns=%d tool_call=%d tool_result=%d status=%q reason=%q",
		*turns, census["tool_call"], census["tool_result"],
		terminal["status"], terminal["reason"])
	t.Logf("refusal: %.140s", refusal)

	if *turns >= wsCeiling {
		t.Fatalf("the loop consumed %d main-loop turns without a terminal; production "+
			"runs uncapped and spent the whole session budget here", *turns)
	}
	if census["done"] != 1 {
		t.Fatalf("%d terminal events", census["done"])
	}
	st := NormalizeTerminalStatus(terminal["status"])
	if st.Completed() {
		t.Fatalf("a run that never dispatched anything reported %q", terminal["status"])
	}
	if terminal["reason"] != "repeated_refusal" {
		t.Errorf("reason = %q, want repeated_refusal from the existing failure policy",
			terminal["reason"])
	}
	// The refusal the model reads is unchanged.
	if !strings.Contains(refusal, "workspace root") {
		t.Errorf("the boundary diagnostic changed: %s", refusal)
	}
	if census["tool_call"] != census["tool_result"] {
		t.Errorf("call/result balance: %d vs %d", census["tool_call"], census["tool_result"])
	}
	// Nothing was dispatched, created, or read.
	if _, err := os.Stat(ctx.WorkingDir); err == nil {
		t.Error("the missing workspace root was created")
	}
	if len(ctx.Ledger) != 0 {
		t.Errorf("a refused path entered the ledger: %v", ctx.Ledger)
	}
}

// Every tool the validator guards must be bounded the same way. The tool list
// is ENUMERATED from workspacePathFields -- the registry the validator itself
// keys on -- so a tool added there without a bounded refusal fails here rather
// than silently gaining the old unaccounted behaviour.
func TestEveryWorkspaceGuardedToolRefusesBoundedly(t *testing.T) {
	// Non-path arguments a tool needs before its path is even looked at.
	// Anything enumerated below must appear here, which is what makes a new
	// tool fail loudly instead of being skipped.
	extra := map[string]map[string]interface{}{
		"read_file":       {},
		"outline_file":    {},
		"write_file":      {"content": "x = 1\n"},
		"edit_file":       {"old_str": "a", "new_str": "b"},
		"structural_edit": {"selector": "function:f", "content": "def f():\n    pass\n"},
		"insert_after":    {"line": 1, "content": "x = 1"},
		"replace_lines":   {"start_line": 1, "end_line": 1, "expected_first_line": "a", "expected_last_line": "a", "content": "b"},
		"delete_file":     {},
		"move_file":       {},
		"search_files":    {"pattern": "def "},
		"find_file":       {"pattern": "app"},
		"list_directory":  {},
		"run_command":     {"command": "ls"},
		"run_background":  {"command": "python app.py"},
	}
	if len(workspacePathFields) != 13 {
		t.Logf("validator now guards %d tools (was 13)", len(workspacePathFields))
	}
	names := make([]string, 0, len(workspacePathFields))
	for name := range workspacePathFields {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			shape, known := extra[name]
			if !known {
				t.Fatalf("%s is guarded by the validator but this table has no argument "+
					"shape for it; give it one so its refusal is proven bounded", name)
			}
			args := map[string]interface{}{}
			for k, v := range shape {
				args[k] = v
			}
			// Every path field the validator keys on, pointed at the same
			// target so repeated refusals share one failure identity.
			for _, field := range workspacePathFields[name] {
				args[field] = "app.py"
			}
			ctx, turns, census, terminal, refusal := wsRefusalFixture(t, name, args)

			t.Logf("%-16s turns=%d status=%q reason=%q", name, *turns,
				terminal["status"], terminal["reason"])
			if *turns >= wsCeiling {
				t.Fatalf("%s: %d turns without a terminal", name, *turns)
			}
			if census["done"] != 1 {
				t.Errorf("%d terminal events", census["done"])
			}
			if NormalizeTerminalStatus(terminal["status"]).Completed() {
				t.Errorf("a session that only ever refused reported %q", terminal["status"])
			}
			if census["tool_call"] != census["tool_result"] {
				t.Errorf("call/result balance: %d vs %d",
					census["tool_call"], census["tool_result"])
			}
			// Refused before dispatch: nothing ran, nothing landed, and the
			// missing root was not created.
			if _, err := os.Stat(ctx.WorkingDir); err == nil {
				t.Error("the missing workspace root was created")
			}
			if len(ctx.Ledger) != 0 {
				t.Errorf("a refused path entered the ledger: %v", ctx.Ledger)
			}
			if refusal == "" {
				t.Error("no refusal reached the model")
			}
		})
	}
}

// Healthy-workspace controls: the aligned route is untouched.
func TestAlignedWorkspaceRoutesAreUnchanged(t *testing.T) {
	base := func(t *testing.T, plan func(i int, prompt string) map[string]interface{}) (
		*AgentContext, *int, map[string]int, map[string]string, string) {
		t.Helper()
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "app.py"), []byte("A = 1\n"), 0o644)
		turns := 0
		census := map[string]int{}
		terminal := map[string]string{}
		first := ""
		var mu sync.Mutex
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			case strings.HasSuffix(r.URL.Path, "/syntax-check"):
				json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
				return
			case strings.HasSuffix(r.URL.Path, "/execute"):
				var in struct{ Code string }
				json.NewDecoder(r.Body).Decode(&in)
				if strings.Contains(in.Code, ".atlas-mount-probe") {
					b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
					json.NewEncoder(w).Encode(map[string]interface{}{
						"success": true, "stdout": string(b), "exit_code": 0})
					return
				}
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": "", "exit_code": 0})
				return
			case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
				http.NotFound(w, r)
				return
			}
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			i := turns
			turns++
			mu.Unlock()
			if i >= wsCeiling {
				http.Error(w, "ceiling", http.StatusInsufficientStorage)
				return
			}
			call, _ := json.Marshal(plan(i, string(raw)))
			d, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"delta": map[string]string{"content": string(call)}}}})
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
		}))
		t.Cleanup(srv.Close)
		ctx := NewAgentContext(dir, Tier2Medium)
		ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
		ctx.PermissionMode = PermissionYolo
		ctx.TrustMode = trustFullyTrusted
		ctx.VerifyOnHost = true
		ctx.MaxTurns = 0
		ctx.StreamFn = func(et string, data interface{}) {
			b, _ := json.Marshal(data)
			mu.Lock()
			defer mu.Unlock()
			census[et]++
			if et == "tool_result" && first == "" {
				first = string(b)
			}
			if et == "done" {
				var m map[string]string
				json.Unmarshal(b, &m)
				for k, v := range m {
					terminal[k] = v
				}
			}
		}
		runAgentLoop(ctx, "Look at app.py.")
		return ctx, &turns, census, terminal, first
	}

	t.Run("aligned find_file dispatches and succeeds", func(t *testing.T) {
		_, turns, _, terminal, first := base(t, func(i int, _ string) map[string]interface{} {
			if i == 0 {
				return map[string]interface{}{"type": "tool_call", "name": "find_file",
					"args": map[string]string{"pattern": "app"}}
			}
			return map[string]interface{}{"type": "done", "summary": "found app.py"}
		})
		t.Logf("aligned success: turns=%d first=%.90s", *turns, first)
		if !strings.Contains(first, `"success":true`) {
			t.Errorf("a well-formed find_file did not dispatch: %s", first)
		}
		if strings.Contains(first, "workspace root") {
			t.Error("an aligned call took the boundary-refusal path")
		}
		_ = terminal
	})

	t.Run("repeated aligned find_file uses the retry policy, not the boundary path", func(t *testing.T) {
		_, turns, _, terminal, first := base(t, func(i int, _ string) map[string]interface{} {
			return map[string]interface{}{"type": "tool_call", "name": "find_file",
				"args": map[string]string{"path": "app.py"}}
		})
		t.Logf("aligned repeat: turns=%d status=%q reason=%q", *turns,
			terminal["status"], terminal["reason"])
		if strings.Contains(first, "workspace root") {
			t.Fatal("an aligned repeat was refused by the boundary check")
		}
		if !strings.Contains(first, "not a directory") {
			t.Errorf("the aligned diagnostic changed: %.140s", first)
		}
		if terminal["reason"] != "repeated_refusal" {
			t.Errorf("reason = %q", terminal["reason"])
		}
	})

	t.Run("path escape and deny-list diagnostics are verbatim", func(t *testing.T) {
		for _, c := range []struct{ name, path, want string }{
			{"escape", "../outside.py", "outside the workspace"},
			{"deny-list", ".env", "refused"},
		} {
			_, _, _, _, first := base(t, func(i int, _ string) map[string]interface{} {
				if i == 0 {
					return map[string]interface{}{"type": "tool_call", "name": "write_file",
						"args": map[string]string{"path": c.path, "content": "x = 1\n"}}
				}
				return map[string]interface{}{"type": "done", "summary": "done"}
			})
			t.Logf("%s: %.120s", c.name, first)
			if !strings.Contains(strings.ToLower(first), c.want) {
				t.Errorf("%s diagnostic changed: %s", c.name, first)
			}
		}
	})
}

// Structural guard against a fifth instance. Every pre-execution bounce in the
// tool-call branch must either be accounted (recorded and measured against the
// stopping rule) or bounded by its own budget. Anything else is listed here
// explicitly, with what is known about it, so a NEW unaccounted refusal fails
// this test rather than shipping as another unbounded loop.
//
// Keyed on the rejection's own wording rather than line numbers, so ordinary
// edits above it do not break the guard.
func TestEveryPreExecutionBounceIsAccountedOrBounded(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(src), "\n")
	start, ban := -1, -1
	for i, l := range lines {
		if start < 0 && strings.TrimSpace(l) == `case "tool_call":` && i > 1100 {
			start = i
		}
		if start >= 0 && ban < 0 && strings.Contains(l, "identicalRetryRefusal(") {
			ban = i
		}
	}
	if start < 0 || ban < 0 {
		t.Fatal("could not locate the tool-call branch and its accounting boundary")
	}

	// Bounces that are deliberately NOT failure-accounted, each with the
	// mechanism that bounds them instead. A new entry needs a deliberate
	// decision; an unlisted one fails.
	known := map[string]string{
		// Anchors are text visible in the SOURCE around the bounce, not the
		// runtime message, because several rejections are composed elsewhere.
		"foregroundServerRejectionWithSource(": "unmeasured; reported",
		"rejecting run_command":                "unmeasured; reported",
		"rejecting run_background":             "unmeasured; reported",
		"Your output was truncated":            "bounded by its own consecutiveErrors>=3 stop",
		"fencedRunFirstRecovery(":              "bounded: one recovery per canonical path",
		"fencedChannelRecovery(":               "bounded: one offer per canonical path",
		"no fenced block followed":             "accounted since the fenced-bounce fix",
		"toolBanNote(":                         "accounted by the per-path ban branch",
		"identicalRetryRefusal(":               "accounted by the retry-ban branch",
	}

	var unlisted []string
	for i := start; i < ban; i++ {
		if !strings.Contains(lines[i], "st.bounceToolCall(") {
			continue
		}
		// The rejection text is on this line or the few around it.
		window := strings.Join(lines[max(start, i-16):min(len(lines), i+6)], "\n")
		// Accounted right here?
		after := strings.Join(lines[i:min(len(lines), i+30)], "\n")
		// accountRefusedCall is the shared reader every pre-dispatch refusal
		// goes through; the two inline forms predate it and still exist.
		if strings.Contains(after, "accountRefusedCall(") ||
			strings.Contains(after, "shouldStopForFailures(") ||
			strings.Contains(after, "recordFailedToolCall(") {
			continue
		}
		if strings.Contains(window, "chargeBounce(") {
			continue
		}
		matched := false
		for phrase := range known {
			if strings.Contains(window, phrase) {
				matched = true
				break
			}
		}
		if !matched {
			unlisted = append(unlisted, fmt.Sprintf("agent.go:%d", i+1))
		}
	}
	if len(unlisted) > 0 {
		t.Errorf("pre-execution bounces that are neither accounted nor bounded nor "+
			"listed: %v — a refusal the model can repeat forever is how the "+
			"workspace-boundary loop happened; account for it or record why it "+
			"is bounded", unlisted)
	}
	t.Logf("audited pre-execution bounces between agent.go:%d and the retry ban at agent.go:%d",
		start+1, ban+1)
}

// --- Ignored write_file steering ---------------------------------------------
//
// Two branches refuse a write_file over an existing file and steer the model
// elsewhere: "read it first" when the session has never read it, and "use
// edit_file" once it has. Both bounced and continued before any convergence
// accounting, so a model that ignored the steer could repeat it forever --
// measured at 31 turns in a HEALTHY workspace with no terminal of ATLAS's own.
//
// They are steering, not ordinary failures: a model that FOLLOWS the steer must
// be completely unaffected.

// steerFixture drives the real loop with MaxTurns=0 over a seeded existing
// file. plan sees the prompt so a scripted model can react to what it was told.
func steerFixture(t *testing.T, seed map[string]string,
	plan func(i int, prompt string) map[string]interface{}) (
	*AgentContext, string, *int, map[string]int, map[string]string, *[]string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range seed {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
	turns := 0
	census := map[string]int{}
	terminal := map[string]string{}
	var bounces []string
	var mu sync.Mutex
	fenced := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"valid": !strings.Contains(in.Code, "]]")})
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(string(raw), "single fenced block") {
			mu.Lock()
			fenced++
			mu.Unlock()
			d, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"delta": map[string]string{"content": "no block here"}}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
			return
		}
		mu.Lock()
		i := turns
		turns++
		mu.Unlock()
		if i >= wsCeiling {
			http.Error(w, "turn ceiling exceeded", http.StatusInsufficientStorage)
			return
		}
		call, _ := json.Marshal(plan(i, string(raw)))
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(call)}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	t.Cleanup(srv.Close)

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.MaxTurns = 0
	ctx.StreamFn = func(et string, data interface{}) {
		b, _ := json.Marshal(data)
		mu.Lock()
		defer mu.Unlock()
		census[et]++
		census["_fenced"] = fenced
		if et == "gate" || et == "tool_result" {
			bounces = append(bounces, et+"|"+string(b))
		}
		if et == "done" {
			var m map[string]string
			json.Unmarshal(b, &m)
			for k, v := range m {
				terminal[k] = v
			}
		}
	}
	runAgentLoop(ctx, "Rewrite app.py so it prints 7.")
	return ctx, dir, &turns, census, terminal, &bounces
}

// Over the five-line carve-out, so the existing-file steer applies: below it
// there is no edit-vs-rewrite distinction and a full write is allowed.
const steerSeed = "def solve():\n    total = 0\n    for i in range(3):\n        total += i\n    return 1\n\n\nprint(solve())\n"

func TestIgnoredWriteSteeringIsBounded(t *testing.T) {
	for _, c := range []struct {
		name, marker string
		preRead      bool
	}{
		{"never read", "has not read it", false},
		{"already read", "write_file is for creating new files", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, dir, turns, census, terminal, bounces := steerFixture(t,
				map[string]string{"app.py": steerSeed},
				func(i int, _ string) map[string]interface{} {
					if c.preRead && i == 0 {
						return map[string]interface{}{"type": "tool_call", "name": "read_file",
							"args": map[string]string{"path": "app.py"}}
					}
					return map[string]interface{}{"type": "tool_call", "name": "write_file",
						"args": map[string]string{"path": "app.py", "content": "print(7)\n"}}
				})
			t.Logf("%s: turns=%d status=%q reason=%q", c.name, *turns,
				terminal["status"], terminal["reason"])

			steered := false
			for _, b := range *bounces {
				if strings.Contains(b, c.marker) {
					steered = true
				}
			}
			if !steered {
				t.Fatalf("the fixture never reached the %q steer", c.name)
			}
			if *turns >= wsCeiling {
				t.Fatalf("%d turns without a terminal", *turns)
			}
			if census["done"] != 1 {
				t.Errorf("%d terminal events", census["done"])
			}
			if NormalizeTerminalStatus(terminal["status"]).Completed() {
				t.Errorf("a run that only ever repeated a refused write reported %q",
					terminal["status"])
			}
			if census["tool_call"] != census["tool_result"] {
				t.Errorf("call/result balance: %d vs %d",
					census["tool_call"], census["tool_result"])
			}
			// The file is untouched and unowned.
			const wfPath = "app.py"
			got, _ := os.ReadFile(filepath.Join(dir, wfPath))
			if string(got) != steerSeed {
				t.Errorf("the refused write reached disk: %q", got)
			}
			// The bounded recovery does show the file once, on purpose --
			// see TestUnreadSteerRecoveryShowsTheFile. What it must never do
			// is hand the model ownership of a file it did not write.
			if ctx.SessionWrites[wfPath] {
				t.Error("a refused write manufactured session ownership")
			}
		})
	}
}

// The control that matters most: a model that FOLLOWS the steer is unaffected.
func TestFollowingTheFirstSteerStillCompletes(t *testing.T) {
	ctx, dir, turns, census, terminal, _ := steerFixture(t,
		map[string]string{"app.py": steerSeed},
		func(i int, prompt string) map[string]interface{} {
			switch {
			case i == 0:
				// Refused: existing and unread.
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "app.py", "content": "print(7)\n"}}
			case i == 1:
				return map[string]interface{}{"type": "tool_call", "name": "read_file",
					"args": map[string]string{"path": "app.py"}}
			case i == 2:
				return map[string]interface{}{"type": "tool_call", "name": "edit_file",
					"args": map[string]string{"path": "app.py",
						"old_str": "return 1", "new_str": "return 7"}}
			case i == 3:
				return map[string]interface{}{"type": "tool_call", "name": "run_command",
					"args": map[string]string{"command": "python3 app.py"}}
			default:
				return map[string]interface{}{"type": "done", "summary": "app.py now returns 7"}
			}
		})
	got, _ := os.ReadFile(filepath.Join(dir, "app.py"))
	t.Logf("followed: turns=%d status=%q reason=%q disk=%q",
		*turns, terminal["status"], terminal["reason"], string(got))

	if terminal["status"] != string(TerminalCompleted) {
		t.Fatalf("following the steer did not complete: status=%q reason=%q",
			terminal["status"], terminal["reason"])
	}
	if !strings.Contains(string(got), "return 7") {
		t.Errorf("the edit did not land: %q", got)
	}
	d := ctx.Ledger[ledgerKey(ctx, "app.py")]
	if d == nil || d.CurrentHash != hashBytes(got) {
		t.Error("the ledger does not describe the final bytes")
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
}

// Path identity under ignored steering: aliases converge on one target, and
// distinct paths stay independent of each other.
func TestIgnoredSteeringPathIdentity(t *testing.T) {
	write := func(path string) map[string]interface{} {
		return map[string]interface{}{"type": "tool_call", "name": "write_file",
			"args": map[string]string{"path": path, "content": "print(7)\n"}}
	}
	for _, c := range []struct {
		name       string
		second     string
		sameTarget bool
	}{
		{"aliases are one target", "./app.py", true},
		{"distinct paths are independent", "util.py", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, dir, turns, _, terminal, _ := steerFixture(t,
				map[string]string{"app.py": steerSeed, "util.py": steerSeed},
				func(i int, _ string) map[string]interface{} {
					if i%2 == 0 {
						return write("app.py")
					}
					return write(c.second)
				})
			t.Logf("%s: turns=%d status=%q reason=%q",
				c.name, *turns, terminal["status"], terminal["reason"])

			// Either way the run is bounded and honest.
			if *turns >= wsCeiling {
				t.Fatalf("%d turns without a terminal", *turns)
			}
			if terminal["reason"] != "repeated_refusal" {
				t.Fatalf("reason=%q, want repeated_refusal", terminal["reason"])
			}
			// The difference is WHEN. Alternating aliases is one path
			// repeating, so the consecutive-same-path rule ends it at three.
			// Two real paths is not a stuck loop, so it survives past three
			// and is bounded by the total-failure ceiling instead.
			if c.sameTarget && *turns > 3 {
				t.Errorf("aliases did not converge on one target: %d turns", *turns)
			}
			if !c.sameTarget && *turns <= 3 {
				t.Errorf("distinct paths were treated as one stuck path: %d turns", *turns)
			}
			for _, f := range []string{"app.py", "util.py"} {
				if got, _ := os.ReadFile(filepath.Join(dir, f)); string(got) != steerSeed {
					t.Errorf("%s was overwritten by a refused write: %q", f, got)
				}
			}
		})
	}
}

// A success on the path clears the remembered rejection, under the same
// semantics every other refusal branch already relies on: a later identical
// write is refused fresh by the gate rather than banned as a repeat.
func TestSuccessClearsSteeringState(t *testing.T) {
	ctx, _, turns, _, terminal, bounces := steerFixture(t,
		map[string]string{"app.py": steerSeed},
		func(i int, _ string) map[string]interface{} {
			switch i {
			case 1:
				return map[string]interface{}{"type": "tool_call", "name": "read_file",
					"args": map[string]string{"path": "app.py"}}
			case 2:
				return map[string]interface{}{"type": "tool_call", "name": "edit_file",
					"args": map[string]string{"path": "app.py",
						"old_str": "return 1", "new_str": "return 7"}}
			case 0, 3: // the same refused write, before and after the edit
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "app.py", "content": "print(7)\n"}}
			default:
				return map[string]interface{}{"type": "done", "summary": "app.py returns 7"}
			}
		})
	t.Logf("clear-on-success: turns=%d failed_calls=%d status=%q reason=%q",
		*turns, len(ctx.FailedToolCalls), terminal["status"], terminal["reason"])

	steers := 0
	for _, b := range *bounces {
		if strings.HasPrefix(b, "gate|") && strings.Contains(b, "already exists") {
			steers++
		}
	}
	if steers != 2 {
		t.Errorf("%d steering refusals, want one before and one after the edit", steers)
	}
	// The post-edit refusal is the gate speaking again, not a stale ban: the
	// edit wiped the remembered rejections, so only that one is held.
	if len(ctx.FailedToolCalls) != 1 {
		t.Errorf("%d remembered rejections after the successful edit, want 1",
			len(ctx.FailedToolCalls))
	}
}

// --- One bounded recovery for ignored write_file steering --------------------

// The unread steer is ignored once, so the second refusal shows the file the
// model kept trying to destroy — through the same reader read_file uses, and
// recording exactly what was shown. It is a read, not ownership.
func TestUnreadSteerRecoveryShowsTheFile(t *testing.T) {
	ctx, dir, turns, _, terminal, bounces := steerFixture(t,
		map[string]string{"app.py": steerSeed},
		func(i int, _ string) map[string]interface{} {
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "app.py", "content": "print(7)\n"}}
		})
	t.Logf("unread recovery: turns=%d status=%q reason=%q",
		*turns, terminal["status"], terminal["reason"])

	// Gate events carry a truncated reason for display; the tool result the
	// model actually receives carries the whole thing.
	var offers, shown int
	for _, b := range *bounces {
		if strings.HasPrefix(b, "gate|") && strings.Contains(b, "here it is") {
			offers++
		}
		if strings.HasPrefix(b, "tool_result|") && strings.Contains(b, "here it is") {
			if strings.Contains(b, "range(3)") && strings.Contains(b, "print(solve())") {
				shown++
			}
		}
	}
	if offers != 1 {
		t.Errorf("%d recovery offers, want exactly one", offers)
	}
	if shown != 1 {
		t.Errorf("the recovery claimed to show the file but the model received %d bodies", shown)
	}
	// The model has genuinely seen the body now, and owns nothing more.
	if !ctx.WasFileRead(filepath.Join(dir, "app.py")) {
		t.Error("the shown file was not recorded as read")
	}
	if ctx.SessionWrites["app.py"] {
		t.Error("the recovery manufactured session ownership of the file")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "app.py")); string(got) != steerSeed {
		t.Errorf("the refused write reached disk: %q", got)
	}
	// The recovery buys a better turn, not an extra one.
	if *turns > 3 {
		t.Errorf("the recovery loosened the bound: %d turns", *turns)
	}
	if terminal["reason"] != "repeated_refusal" {
		t.Errorf("reason=%q, want repeated_refusal", terminal["reason"])
	}
}

// The already-read steer fails for a different reason, so it gets a different
// recovery: one reminder, no reread, both edit tools still on the table.
func TestAlreadyReadSteerRecoveryRemindsWithoutRereading(t *testing.T) {
	_, _, turns, _, terminal, bounces := steerFixture(t,
		map[string]string{"app.py": steerSeed},
		func(i int, _ string) map[string]interface{} {
			if i == 0 {
				return map[string]interface{}{"type": "tool_call", "name": "read_file",
					"args": map[string]string{"path": "app.py"}}
			}
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "app.py", "content": "print(7)\n"}}
		})
	t.Logf("already-read recovery: turns=%d status=%q reason=%q",
		*turns, terminal["status"], terminal["reason"])

	var offers int
	for _, b := range *bounces {
		if !strings.Contains(b, "You do not need to read it again") {
			continue
		}
		if strings.HasPrefix(b, "gate|") {
			offers++
			continue
		}
		if !strings.HasPrefix(b, "tool_result|") {
			continue
		}
		if strings.Contains(b, "range(3)") {
			t.Error("the reminder re-showed a file the model had already read")
		}
		// Both edit tools stay on the table; neither is imposed.
		for _, tool := range []string{"edit_file", "structural_edit"} {
			if !strings.Contains(b, tool) {
				t.Errorf("the reminder does not leave %s available", tool)
			}
		}
	}
	if offers != 1 {
		t.Errorf("%d reminders, want exactly one", offers)
	}
}

// The recovery is per path and released only by a materially different action
// on that same path.
func TestSteerRecoveryIsPerPathAndReleasedByProgress(t *testing.T) {
	write := func(path string) map[string]interface{} {
		return map[string]interface{}{"type": "tool_call", "name": "write_file",
			"args": map[string]string{"path": path, "content": "print(7)\n"}}
	}
	ctx, _, _, _, _, bounces := steerFixture(t,
		map[string]string{"app.py": steerSeed, "util.py": steerSeed},
		func(i int, _ string) map[string]interface{} {
			switch i {
			case 0, 1: // app.py: steer, then the ignored repeat -> recovery
				return write("app.py")
			case 2: // a materially different action on app.py, which succeeds
				return map[string]interface{}{"type": "tool_call", "name": "read_file",
					"args": map[string]string{"path": "util.py"}}
			default:
				return map[string]interface{}{"type": "done", "summary": "done"}
			}
		})
	appOffers := 0
	utilOffers := 0
	for _, b := range *bounces {
		if !strings.HasPrefix(b, "gate|") {
			continue
		}
		if strings.Contains(b, "here it is") || strings.Contains(b, "You do not need to read it again") {
			if strings.Contains(b, "util.py") {
				utilOffers++
			} else {
				appOffers++
			}
		}
	}
	t.Logf("per-path recovery: app=%d util=%d", appOffers, utilOffers)
	if appOffers != 1 {
		t.Errorf("%d recoveries for app.py, want one", appOffers)
	}
	if utilOffers != 0 {
		t.Errorf("app.py's recovery spent util.py's: %d", utilOffers)
	}
	// Reading util.py is not progress on app.py, so app.py's state stands.
	if !ctx.WasFileRead(ledgerKey(ctx, "app.py")) {
		t.Error("the recovery did not leave app.py read")
	}
}

// Causal: the recovery is what makes the difference. The model only leaves the
// loop after it has actually been shown the file — before that it repeats the
// same refused write, exactly as it did in production.
func TestSteerRecoveryCausesConvergence(t *testing.T) {
	edited := false
	ctx, dir, turns, census, terminal, _ := steerFixture(t,
		map[string]string{"app.py": steerSeed},
		func(i int, prompt string) map[string]interface{} {
			// Nothing but the recovery moves it.
			if !strings.Contains(prompt, "so here it is") {
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "app.py", "content": "print(7)\n"}}
			}
			switch {
			case !edited:
				edited = true
				// It edits the text the recovery put in front of it.
				return map[string]interface{}{"type": "tool_call", "name": "edit_file",
					"args": map[string]string{"path": "app.py",
						"old_str": "return 1", "new_str": "return 7"}}
			case i < 5:
				return map[string]interface{}{"type": "tool_call", "name": "run_command",
					"args": map[string]string{"command": "python3 app.py"}}
			default:
				return map[string]interface{}{"type": "done", "summary": "app.py returns 7"}
			}
		})
	got, _ := os.ReadFile(filepath.Join(dir, "app.py"))
	t.Logf("causal: turns=%d status=%q reason=%q disk=%q",
		*turns, terminal["status"], terminal["reason"], string(got))

	if terminal["status"] != string(TerminalCompleted) {
		t.Fatalf("the recovery did not convert the loop: status=%q reason=%q",
			terminal["status"], terminal["reason"])
	}
	if !strings.Contains(string(got), "return 7") {
		t.Errorf("the edit did not land: %q", got)
	}
	// It edited the real file rather than replacing it with the model's guess.
	if !strings.Contains(string(got), "range(3)") {
		t.Errorf("the original content was destroyed: %q", got)
	}
	d := ctx.Ledger[ledgerKey(ctx, "app.py")]
	if d == nil || d.CurrentHash != hashBytes(got) {
		t.Error("the ledger does not describe the final bytes")
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
}

// The recovery is released by a materially different action on the SAME path,
// and by nothing else, so a path that gets genuinely unstuck and later sticks
// again is not left without one.
func TestSteerRecoveryReleasedBySamePathProgress(t *testing.T) {
	write := map[string]interface{}{"type": "tool_call", "name": "write_file",
		"args": map[string]string{"path": "app.py", "content": "print(7)\n"}}
	_, _, turns, _, _, bounces := steerFixture(t,
		map[string]string{"app.py": steerSeed},
		func(i int, _ string) map[string]interface{} {
			if i == 2 {
				// Materially different, same path, and it succeeds.
				return map[string]interface{}{"type": "tool_call", "name": "edit_file",
					"args": map[string]string{"path": "app.py",
						"old_str": "return 1", "new_str": "return 7"}}
			}
			return write
		})
	offers := 0
	for _, b := range *bounces {
		if strings.HasPrefix(b, "gate|") &&
			(strings.Contains(b, "so here it is") ||
				strings.Contains(b, "You do not need to read it again")) {
			offers++
		}
	}
	t.Logf("released: turns=%d offers=%d", *turns, offers)
	if offers != 2 {
		t.Errorf("%d recoveries across a release, want one before and one after", offers)
	}
}

// --- C3: the no-op edit over a demonstrably broken artifact ------------------
//
// Retained shape, seen twice in Stage 1: write_file lands a file that does not
// parse and says so; verification reports the concrete failure; edit_file
// demands a read; the model reads; then it submits an edit whose old_str and
// new_str are identical, over and over, until repetition protection ends the
// run with the broken file still on disk and no valid version to fall back to.
//
// The class is already bounded. What is missing is a productive answer: the
// model is copying a span it cannot reproduce with a change applied, and being
// told again that the two sides match gives it nothing new.

// brokenArtifactFixture runs the real loop with a genuine Python syntax check,
// so the failure diagnostic in the ledger is a real one and not a fixture
// invention. plan sees the prompt, so a scripted model can react to what it
// was actually told rather than to the turn number.
func brokenArtifactFixture(t *testing.T, plan func(i int, prompt string) map[string]interface{}) (
	*AgentContext, string, *int, map[string]int, map[string]string, *[]string) {
	t.Helper()
	dir := t.TempDir()
	turns := 0
	census := map[string]int{}
	terminal := map[string]string{}
	var bounces []string
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			out := map[string]interface{}{"valid": true}
			if diag := pythonSyntaxDiagnostic(in.Code); diag != "" {
				out = map[string]interface{}{"valid": false, "errors": []string{diag}}
			}
			json.NewEncoder(w).Encode(out)
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(string(raw), "single fenced block") {
			d, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"delta": map[string]string{"content": "no block"}}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
			return
		}
		mu.Lock()
		i := turns
		turns++
		mu.Unlock()
		if i >= wsCeiling {
			http.Error(w, "turn ceiling exceeded", http.StatusInsufficientStorage)
			return
		}
		call, _ := json.Marshal(plan(i, string(raw)))
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(call)}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	t.Cleanup(srv.Close)

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.MaxTurns = 0
	ctx.StreamFn = func(et string, data interface{}) {
		b, _ := json.Marshal(data)
		mu.Lock()
		defer mu.Unlock()
		census[et]++
		if et == "gate" || et == "tool_result" {
			bounces = append(bounces, et+"|"+string(b))
		}
		if et == "done" {
			var m map[string]string
			json.Unmarshal(b, &m)
			for k, v := range m {
				terminal[k] = v
			}
		}
	}
	runAgentLoop(ctx, "Write solve.py so it prints 7, then run it.")
	return ctx, dir, &turns, census, terminal, &bounces
}

// pythonSyntaxDiagnostic returns the real interpreter's message, or "" when the
// source parses. The fixture must not invent the diagnostic the ledger holds.
func pythonSyntaxDiagnostic(code string) string {
	cmd := exec.Command("python3", "-c",
		"import ast,sys;\ntry:\n ast.parse(sys.stdin.read())\nexcept SyntaxError as e:\n sys.stdout.write('line %d: %s' % (e.lineno or 0, e.msg))")
	cmd.Stdin = strings.NewReader(code)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

const (
	c3Broken  = "def solve()\n    return 7\n\n\nprint(solve())\n"
	c3NoopStr = "def solve()"
	c3Fixed   = "def solve():"
)

// c3Plan is the retained sequence: create the broken file, read it, then repeat
// the identical no-op edit. It corrects itself ONLY after the recovery context
// arrives — never merely because turns passed.
func c3Plan(marker string) func(int, string) map[string]interface{} {
	corrected := false
	verified := false
	return func(i int, prompt string) map[string]interface{} {
		switch {
		case i == 0:
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "solve.py", "content": c3Broken}}
		case i == 1:
			return map[string]interface{}{"type": "tool_call", "name": "read_file",
				"args": map[string]string{"path": "solve.py"}}
		case marker != "" && strings.Contains(prompt, marker) && !corrected:
			corrected = true
			return map[string]interface{}{"type": "tool_call", "name": "edit_file",
				"args": map[string]string{"path": "solve.py",
					"old_str": c3NoopStr, "new_str": c3Fixed}}
		case corrected && !verified:
			verified = true
			return map[string]interface{}{"type": "tool_call", "name": "run_command",
				"args": map[string]string{"command": "python3 solve.py"}}
		case corrected:
			return map[string]interface{}{"type": "done", "summary": "solve.py prints 7"}
		default:
			return map[string]interface{}{"type": "tool_call", "name": "edit_file",
				"args": map[string]string{"path": "solve.py",
					"old_str": c3NoopStr, "new_str": c3NoopStr}}
		}
	}
}

// The C3 marker: the one sentence the recovery must say, and the phrase the
// scripted model keys on.
const c3Marker = "changes nothing"

func TestNoopEditOverBrokenArtifactRecovers(t *testing.T) {
	ctx, dir, turns, census, terminal, bounces := brokenArtifactFixture(t, c3Plan(c3Marker))
	got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
	t.Logf("c3: turns=%d status=%q reason=%q disk=%q",
		*turns, terminal["status"], terminal["reason"], string(got))

	offers := 0
	for _, b := range *bounces {
		if strings.HasPrefix(b, "gate|") && strings.Contains(b, c3Marker) {
			offers++
		}
	}
	if offers != 1 {
		t.Fatalf("%d recovery offers, want exactly one", offers)
	}
	// The correction is the model's, and it is materially different.
	if !strings.Contains(string(got), c3Fixed) {
		t.Errorf("the corrected bytes did not land: %q", got)
	}
	if !strings.Contains(string(got), "print(solve())") {
		t.Errorf("the surrounding structure was not preserved: %q", got)
	}
	// Ledger describes the bytes on disk, and they are demonstrably valid.
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
	if d == nil || d.CurrentHash != hashBytes(got) {
		t.Fatal("the ledger does not describe the bytes on disk")
	}
	kind, status := d.CurrentValidation()
	if status != ValidationPassed {
		t.Errorf("validation on the current hash is %s/%s, want passed", kind, status)
	}
	if terminal["status"] != string(TerminalCompleted) ||
		terminal["reason"] != "deliverables_demonstrated" {
		t.Errorf("terminal %q/%q, want completed/deliverables_demonstrated",
			terminal["status"], terminal["reason"])
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
	if census["tool_call"] != census["tool_result"] {
		t.Errorf("call/result balance: %d vs %d", census["tool_call"], census["tool_result"])
	}
}

// c3Ledger builds the one state C3 reads: a session-written file on disk whose
// exact current bytes carry a demonstrated syntax failure, with the model
// having actually seen them. Each case then breaks exactly one clause.
func c3Ledger(t *testing.T, mutate func(*AgentContext, *DeliverableState, string)) (*AgentContext, *runState) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "solve.py")
	os.WriteFile(path, []byte(c3Broken), 0o644)
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.RecordFileRead(path, c3Broken)
	ctx.RecordBodySeen(path)
	h := hashBytes([]byte(c3Broken))
	d := &DeliverableState{
		Path: "solve.py", CurrentHash: h, CurrentSize: len(c3Broken), Generation: 1,
		ValidationKind: ValidationKindSyntax, ValidationStatus: ValidationFailed,
		ValidationDetail: "line 1: expected ':'", ValidatedHash: h,
	}
	if ctx.Ledger == nil {
		ctx.Ledger = map[string]*DeliverableState{}
	}
	ctx.Ledger[ledgerKey(ctx, "solve.py")] = d
	if mutate != nil {
		mutate(ctx, d, path)
	}
	return ctx, &runState{}
}

// Entry is a conjunction of evidence clauses. Each case removes one and must
// produce nothing — no source, no diagnostic, no state written.
func TestBrokenArtifactRecoveryEntryPredicates(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(*AgentContext, *DeliverableState, string)
	}{
		{"validation unknown", func(_ *AgentContext, d *DeliverableState, _ string) {
			d.ValidationStatus, d.ValidationKind = ValidationUnknown, ValidationKindUnknown
		}},
		{"validation not_run", func(_ *AgentContext, d *DeliverableState, _ string) {
			d.ValidationStatus = ValidationNotRun
		}},
		{"validation not_applicable", func(_ *AgentContext, d *DeliverableState, _ string) {
			d.ValidationStatus = ValidationNotApplicable
		}},
		{"current bytes are valid", func(_ *AgentContext, d *DeliverableState, _ string) {
			d.ValidationStatus = ValidationPassed
		}},
		{"failure describes older bytes", func(_ *AgentContext, d *DeliverableState, _ string) {
			d.ValidatedHash = hashBytes([]byte("something else"))
		}},
		{"disk moved under the ledger", func(_ *AgentContext, _ *DeliverableState, path string) {
			os.WriteFile(path, []byte("def solve():\n    return 7\n"), 0o644)
		}},
		{"file was never read", func(ctx *AgentContext, _ *DeliverableState, path string) {
			ctx.BodySeen = map[string]bool{}
		}},
		{"not written by this session", func(_ *AgentContext, d *DeliverableState, _ string) {
			d.Generation = 0
		}},
		{"a safer checkpoint exists", func(_ *AgentContext, d *DeliverableState, _ string) {
			good := []byte("def solve():\n    return 7\n")
			d.CheckpointBytes, d.CheckpointHash = good, hashBytes(good)
			d.CheckpointKind = ValidationKindSyntax
		}},
		{"file is gone", func(_ *AgentContext, _ *DeliverableState, path string) {
			os.Remove(path)
		}},
		{"no budget to act on it", func(ctx *AgentContext, _ *DeliverableState, _ string) {
			c, cancel := context.WithDeadline(context.Background(), time.Now().Add(5*time.Second))
			t.Cleanup(cancel)
			ctx.Ctx = c
		}},
		{"work context already ended", func(ctx *AgentContext, _ *DeliverableState, _ string) {
			c, cancel := context.WithCancel(context.Background())
			cancel()
			ctx.Ctx = c
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, st := c3Ledger(t, c.mutate)
			// Twice: the recurrence is what would arm it if it were eligible.
			for i := 0; i < 2; i++ {
				if msg := brokenArtifactRecovery(ctx, st, "solve.py"); msg != "" {
					t.Fatalf("recovered on %q: %.120s", c.name, msg)
				}
			}
			if len(st.brokenArtifactRecovered) != 0 {
				t.Errorf("recovery state was written anyway: %v", st.brokenArtifactRecovered)
			}
		})
	}
}

// The positive case, and the shape of what it says.
func TestBrokenArtifactRecoveryArmsOnTheRecurrence(t *testing.T) {
	ctx, st := c3Ledger(t, nil)
	if msg := brokenArtifactRecovery(ctx, st, "solve.py"); msg != "" {
		t.Fatalf("one accidental no-op recovered: %.120s", msg)
	}
	msg := brokenArtifactRecovery(ctx, st, "solve.py")
	if msg == "" {
		t.Fatal("the recurrence did not recover")
	}
	for _, want := range []string{
		"solve.py",                // canonical path
		"return 7",                // bounded current source
		"line 1: expected ':'",    // the diagnostic bound to these exact bytes
		"old_str and new_str are", // why the edit changes nothing
		"actually differs",        // what the next mutation has to do
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the recovery does not supply %q:\n%s", want, msg)
		}
	}
	// Once per evidence generation, and an alias is the same generation.
	for _, spelling := range []string{"solve.py", "./solve.py"} {
		if again := brokenArtifactRecovery(ctx, st, spelling); again != "" {
			t.Errorf("%s bought a second recovery", spelling)
		}
	}
	// A different path is a different problem.
	other := filepath.Join(ctx.WorkingDir, "other.py")
	os.WriteFile(other, []byte(c3Broken), 0o644)
	ctx.RecordBodySeen(other)
	h := hashBytes([]byte(c3Broken))
	ctx.Ledger[ledgerKey(ctx, "other.py")] = &DeliverableState{
		Path: "other.py", CurrentHash: h, Generation: 1,
		ValidationKind: ValidationKindSyntax, ValidationStatus: ValidationFailed,
		ValidationDetail: "line 1: expected ':'", ValidatedHash: h,
	}
	if first := brokenArtifactRecovery(ctx, st, "other.py"); first != "" {
		t.Error("another path inherited solve.py's recurrence")
	}
	if second := brokenArtifactRecovery(ctx, st, "other.py"); second == "" {
		t.Error("another path was denied its own recovery")
	}
}

// New bytes are a new evidence generation: the recovery re-arms, and it cannot
// carry the old diagnostic across.
func TestBrokenArtifactRecoveryRearmsOnNewEvidence(t *testing.T) {
	ctx, st := c3Ledger(t, nil)
	brokenArtifactRecovery(ctx, st, "solve.py")
	if brokenArtifactRecovery(ctx, st, "solve.py") == "" {
		t.Fatal("the first generation did not recover")
	}
	// The model changes the file and it is still broken, differently.
	next := "def solve()\n    return 8\n"
	os.WriteFile(filepath.Join(ctx.WorkingDir, "solve.py"), []byte(next), 0o644)
	h := hashBytes([]byte(next))
	key := ledgerKey(ctx, "solve.py")
	// Stale evidence first: the ledger still describes the old bytes.
	if msg := brokenArtifactRecovery(ctx, st, "solve.py"); msg != "" {
		t.Errorf("stale failure detail was reused on new bytes: %.120s", msg)
	}
	ctx.Ledger[key].CurrentHash, ctx.Ledger[key].ValidatedHash = h, h
	ctx.Ledger[key].ValidationDetail = "line 1: still expected ':'"
	ctx.RecordBodySeen(filepath.Join(ctx.WorkingDir, "solve.py"))
	if msg := brokenArtifactRecovery(ctx, st, "solve.py"); msg != "" {
		t.Error("the new generation recovered on one accidental no-op")
	}
	msg := brokenArtifactRecovery(ctx, st, "solve.py")
	if msg == "" {
		t.Fatal("the new evidence generation did not re-arm")
	}
	if strings.Contains(msg, "line 1: expected ':'") || !strings.Contains(msg, "still expected") {
		t.Errorf("the recovery carried the previous generation's diagnostic:\n%s", msg)
	}
	if !strings.Contains(msg, "return 8") {
		t.Errorf("the recovery showed stale source:\n%s", msg)
	}
}

// State is released by a materially different mutation on the SAME path, and
// by nothing else.
func TestBrokenArtifactStateReleaseIsNarrow(t *testing.T) {
	arm := func(t *testing.T) (*AgentContext, *runState) {
		ctx, st := c3Ledger(t, nil)
		brokenArtifactRecovery(ctx, st, "solve.py")
		if brokenArtifactRecovery(ctx, st, "solve.py") == "" {
			t.Fatal("setup did not arm")
		}
		return ctx, st
	}
	held := func(st *runState) bool { return len(st.brokenArtifactRecovered) > 0 }

	for _, c := range []struct {
		name    string
		tool    string
		args    string
		release bool
	}{
		{"another no-op on the same path", "edit_file",
			`{"path":"solve.py","old_str":"x","new_str":"x"}`, false},
		{"a read of the same path", "read_file", `{"path":"solve.py"}`, false},
		{"success on an unrelated path", "write_file",
			`{"path":"other.py","content":"print(1)\n"}`, false},
		{"a real edit of the same path", "edit_file",
			`{"path":"solve.py","old_str":"a","new_str":"b"}`, true},
		{"a real edit spelled as an alias", "edit_file",
			`{"path":"./solve.py","old_str":"a","new_str":"b"}`, true},
		{"a rewrite of the same path", "write_file",
			`{"path":"solve.py","content":"print(7)\n"}`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, st := arm(t)
			clearBrokenArtifactState(ctx, st, c.tool, json.RawMessage(c.args))
			if held(st) == c.release {
				t.Errorf("release=%v, want %v", !held(st), c.release)
			}
		})
	}
}

// Loop-level controls: what the run does around the recovery.
func TestNoopEditRecoveryLoopControls(t *testing.T) {
	t.Run("one accidental no-op gets only the tool's answer", func(t *testing.T) {
		// It corrects itself off edit_file's OWN refusal, which is unchanged.
		ctx, dir, turns, census, terminal, bounces := brokenArtifactFixture(t,
			c3Plan("would change nothing"))
		got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
		t.Logf("accidental: turns=%d status=%q reason=%q", *turns,
			terminal["status"], terminal["reason"])
		for _, b := range *bounces {
			if strings.Contains(b, c3Marker) {
				t.Fatal("a single no-op drew the recovery")
			}
		}
		if terminal["status"] != string(TerminalCompleted) {
			t.Errorf("following the tool's own refusal did not complete: %q/%q",
				terminal["status"], terminal["reason"])
		}
		if !strings.Contains(string(got), c3Fixed) {
			t.Errorf("the correction did not land: %q", got)
		}
		d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
		if _, status := d.CurrentValidation(); status != ValidationPassed {
			t.Errorf("validation on the current hash is %s, want passed", status)
		}
		if census["done"] != 1 {
			t.Errorf("%d terminal events", census["done"])
		}
	})

	t.Run("a model that ignores the recovery stops honestly", func(t *testing.T) {
		// marker "" — nothing makes it correct itself.
		ctx, dir, turns, census, terminal, bounces := brokenArtifactFixture(t, c3Plan(""))
		got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
		t.Logf("ignored: turns=%d status=%q reason=%q", *turns,
			terminal["status"], terminal["reason"])

		offers := 0
		for _, b := range *bounces {
			if strings.HasPrefix(b, "gate|") && strings.Contains(b, c3Marker) {
				offers++
			}
		}
		if offers != 1 {
			t.Errorf("%d recovery offers, want exactly one even when ignored", offers)
		}
		if *turns >= wsCeiling {
			t.Fatalf("%d turns without a terminal", *turns)
		}
		if NormalizeTerminalStatus(terminal["status"]).Completed() {
			t.Errorf("an unresolved broken artifact reported %q", terminal["status"])
		}
		// Nothing was mutated or run on the model's behalf.
		if string(got) != c3Broken {
			t.Errorf("the file changed without a model mutation: %q", got)
		}
		d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
		if _, status := d.CurrentValidation(); status != ValidationFailed {
			t.Errorf("the ledger lost the failure on the current bytes: %s", status)
		}
		// Nothing safer ever existed to fall back to, which is why this class
		// needs a recovery rather than the Phase 3B restoration path.
		ok, why := checkpointRestorable(d, hashBytes(got), false)
		t.Logf("checkpoint: restorable=%v (%s)", ok, why)
		if ok {
			t.Error("an eligible checkpoint existed and should have owned this")
		}
		if d.CheckpointHash != "" {
			t.Errorf("a checkpoint was held for a file never shown valid: %q", d.CheckpointHash)
		}
		if census["done"] != 1 {
			t.Errorf("%d terminal events", census["done"])
		}
		if census["tool_call"] != census["tool_result"] {
			t.Errorf("call/result balance: %d vs %d",
				census["tool_call"], census["tool_result"])
		}
	})
}

// The recovery turn is counted exactly once: it never reaches the tool, so no
// post-execution accounting runs for it, and the resend ban below it is
// skipped. One no-op edit, one increment.
func TestNoopEditRecoveryCountsTheRefusalOnce(t *testing.T) {
	ctx, st := c3Ledger(t, nil)
	ctx.RecentFailurePaths = nil
	before := len(ctx.RecentFailurePaths)
	if brokenArtifactRecovery(ctx, st, "solve.py") != "" {
		t.Fatal("armed on the first no-op")
	}
	// The helper itself accounts for nothing; the loop does, once per turn.
	if len(ctx.RecentFailurePaths) != before {
		t.Errorf("the recovery helper wrote failure accounting of its own: %v",
			ctx.RecentFailurePaths)
	}
	if msg := brokenArtifactRecovery(ctx, st, "solve.py"); msg == "" {
		t.Fatal("the recurrence did not arm")
	}
	if len(ctx.RecentFailurePaths) != before {
		t.Errorf("the recovery helper wrote failure accounting of its own: %v",
			ctx.RecentFailurePaths)
	}
	// And it launches nothing: no mutation, no command, no read state invented.
	if len(ctx.SessionWrites) != 0 {
		t.Errorf("the recovery wrote something: %v", ctx.SessionWrites)
	}
}

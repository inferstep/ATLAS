package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// P-agent-1/INTEGRITY#3: once an exit gate's three bounces were spent, the
// fourth exit fell straight through to "completed", with nothing in the
// status, the reason or the summary. The audit reproduced it with an unread
// citation (completed/text_reply) and a stdin-only verification
// (completed/deliverables_demonstrated).

// exitUntilSpent drives the exit the way the loop does: each bounce is a
// refused exit, and the exit after the last one is judged.
func exitUntilSpent(t *testing.T, st *runState, ctx *AgentContext, prompt, claim, gate string) {
	t.Helper()
	for i := 0; i < maxGateBounces; i++ {
		if got, _ := st.exitGates(ctx, prompt, claim); got != gate {
			t.Fatalf("exit %d: gate %q, want %q", i+1, got, gate)
		}
	}
	if got, _ := st.exitGates(ctx, prompt, claim); got != "" {
		t.Fatalf("exit %d: gate %q after %s was spent", maxGateBounces+1, got, gate)
	}
}

// withSyntaxSandbox answers the deliverable re-check the finalizer makes.
func withSyntaxSandbox(t *testing.T, ctx *AgentContext) {
	t.Helper()
	srv := fakeSyntaxSandbox(t, "")
	t.Cleanup(srv.Close)
	ctx.SandboxURL = srv.URL
}

// doneEvent captures the terminal event the client receives.
func doneEvent(ctx *AgentContext) *map[string]string {
	var done map[string]string
	ctx.StreamFn = func(eventType string, data interface{}) {
		if m, ok := data.(map[string]string); ok && eventType == "done" {
			done = m
		}
	}
	return &done
}

func TestASpentEvidenceGateEndsIncomplete(t *testing.T) {
	ctx, dir := sepCtx(t, &TaskContract{TaskMode: TaskModeQuestion})
	if err := os.WriteFile(filepath.Join(dir, "orders.py"), []byte("def total(items):\n    return 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := &runState{toolsRun: 1}
	prompt, claim := "Why is the order total wrong?", "The bug is in orders.py: total() returns a constant."
	exitUntilSpent(t, st, ctx, prompt, claim, "evidence_gate")

	status, reason := finalizeCompletion(ctx, st, prompt, "text_reply")
	if status != TerminalIncomplete || reason != "unread_citation" {
		t.Fatalf("terminal = %s/%s, want incomplete/unread_citation", status, reason)
	}
	done := doneEvent(ctx)
	emitTerminal(ctx, st, status, reason, "")
	if !strings.Contains((*done)["summary"], "orders.py") || (*done)["unresolved"] != "evidence_gate" {
		t.Fatalf("done event does not say what stopped the run: %v", *done)
	}
}

func TestASpentClaimCheckEndsIncomplete(t *testing.T) {
	ctx, dir := sepCtx(t, nil)
	app := "from flask import Flask, render_template\napp = Flask(__name__)\n\n" +
		"@app.route('/')\ndef index():\n    return render_template('index.html')\n"
	sepWrite(t, ctx, dir, "app.py", app)
	withSyntaxSandbox(t, ctx)
	st := &runState{madeProductiveChange: true, productiveChanges: 1, toolsRun: 2}
	prompt, claim := "Build the Flask app.", "Everything is fully functional."
	exitUntilSpent(t, st, ctx, prompt, claim, "claim_check")

	status, reason := finalizeCompletion(ctx, st, prompt, "")
	if status != TerminalIncomplete || reason != "claim_check_unresolved" {
		t.Fatalf("terminal = %s/%s, want incomplete/claim_check_unresolved", status, reason)
	}
	summary := honestTerminalSummary(ctx, st, status, reason, "")
	if !strings.Contains(summary, "index.html") || strings.Contains(summary, "Your `done` summary") {
		t.Fatalf("summary does not name the gap in the user's terms: %q", summary)
	}
}

// A heuristic gate may let the run finish, and the summary and the event say
// what it still found. The stdin-only case is a caveat, not a failure: for a
// program whose real interface is stdin, piping is the right way to run it.
func TestASpentRedirectGateCompletesWithACaveat(t *testing.T) {
	ctx, dir := sepCtx(t, sepContract([]string{"solve.py"}, nil))
	sepWrite(t, ctx, dir, "solve.py", "import sys\nprint(sys.stdin.read().strip())\n")
	withSyntaxSandbox(t, ctx)
	if err := os.WriteFile(filepath.Join(dir, "input.txt"), []byte("42\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := &runState{madeProductiveChange: true, productiveChanges: 1, toolsRun: 2}
	st.observeVerification(ctx, "", 1, "python3 solve.py < input.txt", ranClean("42\n"))
	prompt, claim := "Write solve.py.", "Finished."
	exitUntilSpent(t, st, ctx, prompt, claim, "contract_gate")

	status, reason := finalizeCompletion(ctx, st, prompt, "")
	if status != TerminalCompleted {
		t.Fatalf("terminal = %s/%s, want completed with a caveat", status, reason)
	}
	done := doneEvent(ctx)
	emitTerminal(ctx, st, status, reason, claim)
	summary := (*done)["summary"]
	if !strings.HasPrefix(summary, claim) || !strings.Contains(summary, "Not confirmed by this run") ||
		!strings.Contains(summary, "input.txt") {
		t.Fatalf("summary does not carry the caveat: %q", summary)
	}
	if (*done)["unresolved"] != "contract_gate" {
		t.Fatalf("done event does not name the spent gate: %v", *done)
	}
	// The /events broker's done envelope reads the same field.
	if ctx.TerminalUnresolved != "contract_gate" {
		t.Fatalf("TerminalUnresolved = %q, want contract_gate", ctx.TerminalUnresolved)
	}
}

// Bytes that changed after the run that verified them are unverified whether
// or not the artifact gate still has a bounce. Where verification was
// demanded, that ends the run incomplete; where it was not, it is a caveat.
func TestASpentArtifactGateLeavesTheRunUnverified(t *testing.T) {
	for _, demanded := range []bool{false, true} {
		ctx, dir := sepCtx(t, nil)
		sepWrite(t, ctx, dir, "app.py", "print('ok')\n")
		withSyntaxSandbox(t, ctx)
		st := &runState{madeProductiveChange: true, productiveChanges: 1, toolsRun: 2}
		st.observeVerification(ctx, "", 1, "python3 app.py", ranClean("ok\n"))
		// A shell write after the verifying run: nothing re-ran it.
		if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("print('changed')\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		st.userWantsVerification = demanded
		prompt := "Fix app.py."
		for i := 0; i < 2*maxGateBounces+1; i++ {
			if gate, _ := st.exitGates(ctx, prompt, "Finished."); gate == "" {
				break
			}
		}
		status, reason := finalizeCompletion(ctx, st, prompt, "")
		if demanded {
			if status != TerminalIncomplete || reason != "verification_demanded_unmet" {
				t.Fatalf("demanded: terminal = %s/%s, want incomplete/verification_demanded_unmet", status, reason)
			}
			continue
		}
		if status != TerminalCompleted {
			t.Fatalf("not demanded: terminal = %s/%s, want completed with a caveat", status, reason)
		}
		if s := honestTerminalSummary(ctx, st, status, reason, "Finished."); !strings.Contains(s, "app.py changed after the run that verified it") {
			t.Fatalf("summary does not carry the drift caveat: %q", s)
		}
	}
}

// A run that clears every gate reports no unresolved gates at all.
func TestACleanExitReportsNothingUnresolved(t *testing.T) {
	ctx, dir := sepCtx(t, sepContract([]string{"app.py"}, nil))
	sepWrite(t, ctx, dir, "app.py", "print('ok')\n")
	withSyntaxSandbox(t, ctx)
	st := &runState{madeProductiveChange: true, productiveChanges: 1, toolsRun: 2}
	st.observeVerification(ctx, "", 1, "python3 app.py", ranClean("ok\n"))
	if gate, msg := st.exitGates(ctx, "Write app.py.", "Finished."); gate != "" {
		t.Fatalf("a verified run was bounced by %s: %s", gate, msg)
	}
	status, reason := finalizeCompletion(ctx, st, "Write app.py.", "")
	done := doneEvent(ctx)
	emitTerminal(ctx, st, status, reason, "Finished.")
	if status != TerminalCompleted || (*done)["summary"] != "Finished." {
		t.Fatalf("terminal = %s/%s summary %q", status, reason, (*done)["summary"])
	}
	if _, ok := (*done)["unresolved"]; ok {
		t.Fatalf("a clean exit reported unresolved gates: %v", *done)
	}
}

// P-agent-3/INTEGRITY#1: a completion that rests on a parse said
// "deliverables_demonstrated", and the model's "All tests pass and everything
// works" was shown word for word over code nothing had run.
func TestAParseOnlyCompletionSaysSo(t *testing.T) {
	cases := []struct {
		name, file, body, command, prose string
		wantReason                       string
		wantPrefix, wantContains         string
	}{
		{"claims over unrun code", "solve.py", "print(1)\n", "",
			"All tests pass and everything works.", "deliverables_parse_only",
			"solve.py parses, but nothing in this run ran it.", "which nothing in this run checked"},
		{"a plain account over unrun code", "solve.py", "print(1)\n", "",
			"Wrote solve.py.", "deliverables_parse_only",
			"Wrote solve.py.", "solve.py parses, but nothing in this run ran it."},
		{"a negated claim is not a claim", "solve.py", "print(1)\n", "",
			"Wrote solve.py; I did not verify it works correctly.", "deliverables_parse_only",
			"Wrote solve.py;", "solve.py parses, but nothing in this run ran it."},
		{"a page nothing loaded", "index.html", "<!DOCTYPE html><html><body><canvas></canvas></body></html>\n", "",
			"I built a fully working snake game.", "deliverables_parse_only",
			"index.html parses, but nothing in this run ran it.", "which nothing in this run checked"},
		{"code that ran", "solve.py", "print(1)\n", "python3 solve.py",
			"All tests pass and everything works.", "deliverables_demonstrated",
			"All tests pass and everything works.", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, dir := sepCtx(t, nil)
			sepWrite(t, ctx, dir, c.file, c.body)
			withSyntaxSandbox(t, ctx)
			st := &runState{madeProductiveChange: true, productiveChanges: 1, toolsRun: 1}
			if c.command != "" {
				st.observeVerification(ctx, "", 1, c.command, ranClean("1\n"))
			}
			status, reason := finalizeCompletion(ctx, st, "Write "+c.file+".", "")
			if status != TerminalCompleted || reason != c.wantReason {
				t.Fatalf("terminal = %s/%s, want completed/%s", status, reason, c.wantReason)
			}
			summary := honestTerminalSummary(ctx, st, status, reason, c.prose)
			if !strings.HasPrefix(summary, c.wantPrefix) || !strings.Contains(summary, c.wantContains) {
				t.Fatalf("summary = %q", summary)
			}
			if c.command != "" && summary != c.prose {
				t.Fatalf("a demonstrated completion's account was changed: %q", summary)
			}
		})
	}
}

// GB-4#5 part 1: a run whose deliverable no check applies to cannot
// complete, and the summary said "the run ended without finishing the task".
// It now says which file has no check.
func TestAFileWithNoCheckIsNamed(t *testing.T) {
	ctx, dir := sepCtx(t, nil)
	withSyntaxSandbox(t, ctx)
	sepWrite(t, ctx, dir, "index.html", "<!DOCTYPE html><html><body>hi</body></html>\n")
	if err := os.WriteFile(filepath.Join(dir, "style.css"), []byte("body { color: red; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	observeDeliverable(ctx, "style.css", []byte("body { color: red; }\n"), ValidationKindNone, ValidationNotApplicable, "")
	st := &runState{madeProductiveChange: true}
	status, reason := finalizeCompletion(ctx, st, "Build the page.", "")
	if status != TerminalIncomplete || reason != "deliverables_not_demonstrated" {
		t.Fatalf("terminal = %s/%s", status, reason)
	}
	summary := honestTerminalSummary(ctx, st, status, reason, "")
	if !strings.Contains(summary, "no check for style.css") || strings.Contains(summary, "ended without finishing") {
		t.Fatalf("summary does not name the unchecked file: %q", summary)
	}
}

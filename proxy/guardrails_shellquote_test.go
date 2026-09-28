package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The exact command measured on multiturn_stats, which the model re-sent 12
// times: the f-string opens on a single quote and closes on a double, so it is
// invalid Python as well as unparseable shell. Nothing the harness could
// re-quote would save it; the shape has to change.
const brokenOneLiner = `python3 -c "from stats import mean, median; print(f'mean([1, 2, 3]): {mean([1, 2, 3])}"); print(f'median([1, 2, 3]): {median([1, 2, 3])}")"`

const bashParseErr = "bash: -c: line 1: syntax error near unexpected token `)'"

func TestAnUnparseableOneLinerGetsAnActionableReason(t *testing.T) {
	hint := shellQuotingHint(brokenOneLiner, bashParseErr)
	if hint == "" {
		t.Fatal("the measured failure must produce a hint")
	}
	for _, want := range []string{"write_file", "nothing ran", "fails identically"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint should contain %q: %s", want, hint)
		}
	}
}

func TestACommandThatRanAndFailedIsNotAQuotingProblem(t *testing.T) {
	// pytest exiting non-zero is a real result, not a parse failure.
	if h := shellQuotingHint("pytest -q", "1 failed, 3 passed"); h != "" {
		t.Errorf("a real test failure must not be relabelled as quoting: %s", h)
	}
}

func TestAParseErrorInAScriptFileIsNotRedirected(t *testing.T) {
	// Already running from a file — the advice would be to do what it did.
	if h := shellQuotingHint("python3 check.py", bashParseErr); h != "" {
		t.Errorf("no redirect when the program is already a file: %s", h)
	}
}

func TestOtherInlineInterpretersAreCovered(t *testing.T) {
	for _, cmd := range []string{`node -e "console.log('x)"`, `perl -e 'print "x'`} {
		if shellQuotingHint(cmd, bashParseErr) == "" {
			t.Errorf("no hint for %s", cmd)
		}
	}
}

func TestEmptyInputsAreSafe(t *testing.T) {
	if shellQuotingHint("", bashParseErr) != "" || shellQuotingHint(brokenOneLiner, "") != "" {
		t.Error("empty command or error must not produce a hint")
	}
}

// A file whose tail is swallowed by a drifting comment still parses, runs and
// exits 0 with no output. Measured: a session recorded that as verification
// and reported success on a program that provably printed no answer.
func TestASilentRunIsNotVerificationWhenOutputWasPromised(t *testing.T) {
	ctx := &AgentContext{}
	prompt := "Write solve.py that reads input.txt and prints the answer on a single line."
	data := json.RawMessage(`{"stdout":"","stderr":"","exit_code":0}`)
	if !silentRunWhenOutputPromised(ctx, prompt, "python3 solve.py", data) {
		t.Error("empty stdout on a print-demanding task must not verify")
	}
	withOut := json.RawMessage(`{"stdout":"42 7","exit_code":0}`)
	if silentRunWhenOutputPromised(ctx, prompt, "python3 solve.py", withOut) {
		t.Error("real output verifies")
	}
	if silentRunWhenOutputPromised(ctx, "fix the bug in app.py", "python3 app.py", data) {
		t.Error("no printed-output promise, no requirement")
	}
	if silentRunWhenOutputPromised(ctx, prompt, "go build ./...", data) {
		t.Error("build steps legitimately print nothing")
	}
}

// Past the streak threshold the advice flips from edit-the-fix to rewrite —
// the no-tool retry baseline's whole advantage is the fresh sheet.
func TestARedStreakFlipsToRewriteAdvice(t *testing.T) {
	msg := verificationRejectionWithStreak(true, false, "", 3)
	if !strings.Contains(msg, "Rewrite the file from scratch") {
		t.Errorf("streak 3 must advise a rewrite: %s", msg)
	}
	msg = verificationRejectionWithStreak(true, false, "", 1)
	if strings.Contains(msg, "from scratch") {
		t.Errorf("streak 1 keeps the edit advice: %s", msg)
	}
}

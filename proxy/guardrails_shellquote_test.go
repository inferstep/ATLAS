package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// A command a model re-sent many times: the f-string opens on a single quote
// and closes on a double, so it is invalid Python as well as unparseable
// shell. Nothing the harness could re-quote would save it; the shape has to
// change.
const brokenOneLiner = `python3 -c "from stats import mean, median; print(f'mean([1, 2, 3]): {mean([1, 2, 3])}"); print(f'median([1, 2, 3]): {median([1, 2, 3])}")"`

const bashParseErr = "bash: -c: line 1: syntax error near unexpected token `)'"

func TestAnUnparseableOneLinerGetsAnActionableReason(t *testing.T) {
	hint := shellQuotingHint(brokenOneLiner, bashParseErr)
	for _, want := range []string{"write_file", "nothing ran", "fails identically"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint should contain %q: %q", want, hint)
		}
	}
}

// The hint is only for an inline program the shell could not parse.
func TestTheQuotingHintIsOnlyForAnInlineProgramThatDidNotParse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cmd      string
		errText  string
		wantHint bool
	}{
		{"python one-liner", brokenOneLiner, bashParseErr, true},
		{"node one-liner", `node -e "console.log('x)"`, bashParseErr, true},
		{"perl one-liner", `perl -e 'print "x'`, bashParseErr, true},
		// pytest exiting non-zero is a real result, not a parse failure.
		{"a command that ran and failed", "pytest -q", "1 failed, 3 passed", false},
		// Already running from a file: the advice would be to do what it did.
		{"a script file", "python3 check.py", bashParseErr, false},
		{"empty command", "", bashParseErr, false},
		{"empty error", brokenOneLiner, "", false},
	} {
		if got := shellQuotingHint(tc.cmd, tc.errText) != ""; got != tc.wantHint {
			t.Errorf("%s: hint given = %v, want %v", tc.name, got, tc.wantHint)
		}
	}
}

// A file whose tail is swallowed by a drifting comment still parses, runs and
// exits 0 with no output. Counting that as verification reports success on a
// program that printed no answer.
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

// Past the streak threshold the advice flips from edit-the-fix to rewrite: a
// fresh start is what a stuck fix lacks.
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

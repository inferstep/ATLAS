package main

import (
	"strings"
	"testing"
)

// An unclosed brace in the MIDDLE of a file is a bug, not a truncated write.
//
// Measured on multifile_cli: test_store.py failed with "'{' was never closed
// (line 44)" and the rejection said the call looked truncated and to resend
// the complete content. The model resent it, identically, twice, until the
// repetition breaker ended the session. The task went 0/2 across two runs.
//
// Truncation puts the failure at the END of the content, which is now
// checkable because the syntax check carries the line number.

func midFileUnclosed() string {
	var b strings.Builder
	b.WriteString("import json\n")
	for i := 0; i < 40; i++ {
		b.WriteString("# filler\n")
	}
	b.WriteString("d = {'a': 1\n") // line 42: the real bug
	for i := 0; i < 20; i++ {
		b.WriteString("# more\n")
	}
	return b.String()
}

func TestAnUnclosedBraceMidFileIsNotCalledTruncation(t *testing.T) {
	msg := fallbackSyntaxRejection("test_store.py", midFileUnclosed(),
		"SyntaxError: '{' was never closed (line 42)")
	if strings.Contains(msg, "truncated tool call") {
		t.Errorf("a failure 20 lines from the end is not truncation: %s", msg)
	}
	if !strings.Contains(msg, "The offending line 42") {
		t.Errorf("must quote the offending line: %s", msg)
	}
	if !strings.Contains(msg, "d = {'a': 1") {
		t.Errorf("must show the actual source: %s", msg)
	}
}

func TestAFailureAtTheEndStillReadsAsTruncation(t *testing.T) {
	content := "def f():\n    return {\n"
	msg := fallbackSyntaxRejection("a.py", content,
		"SyntaxError: '{' was never closed (line 2)")
	if !strings.Contains(msg, "truncated tool call") {
		t.Errorf("a failure on the last line is the truncation shape: %s", msg)
	}
	// Even then, showing the line costs nothing and grounds the retry.
	if !strings.Contains(msg, "The offending line 2") {
		t.Errorf("truncation advice should still quote the line: %s", msg)
	}
}

func TestNoLineNumberKeepsTheOldBehaviour(t *testing.T) {
	msg := fallbackSyntaxRejection("a.py", "x = (\n",
		"SyntaxError: unexpected EOF while parsing")
	if !strings.Contains(msg, "truncated tool call") {
		t.Errorf("without a location the truncation guess is all we have: %s", msg)
	}
}

func TestLastContentLineIgnoresTrailingBlanks(t *testing.T) {
	if got := lastContentLine("a\nb\n\n\n"); got != 2 {
		t.Errorf("want 2, got %d", got)
	}
	if got := lastContentLine(""); got != 0 {
		t.Errorf("want 0, got %d", got)
	}
}

func TestLocateSyntaxLineRejectsOutOfRange(t *testing.T) {
	if n, q := locateSyntaxLine("a\nb\n", "SyntaxError: bad (line 99)"); n != 0 || q != "" {
		t.Errorf("line 99 of a 3-line file is not locatable: %d %q", n, q)
	}
}

// One mis-escaped newline traps the next statement inside a comment. The file
// may still parse, or dies with an IndentationError whose reported line is a
// downstream casualty: measured, the rejection quoted line 45 (blank) while
// the fused comment sat at line 40, and the model re-sent identical content
// until the repetition breaker ended the session.
func TestAFusedCommentLineIsNamed(t *testing.T) {
	content := "x = 1\n" +
		"# If the value changes, it's a transition\\n        if a != b:\n" +
		"    y = 2\n"
	hint := fusedLineHint(content)
	if hint == "" {
		t.Fatal("the fused line must be named")
	}
	if !strings.Contains(hint, "line 2") {
		t.Errorf("must point at the comment line, got: %s", hint)
	}
	if !strings.Contains(hint, "trapped INSIDE the comment") {
		t.Errorf("must say what happened: %s", hint)
	}
}

func TestAnOrdinaryCommentMentioningBackslashNIsLeftAlone(t *testing.T) {
	for _, content := range []string{
		"# split the input on \\n characters\nx = 1\n",
		"s = 'a\\nb'\n",
		"# plain comment\nif a != b:\n    pass\n",
	} {
		if hint := fusedLineHint(content); hint != "" {
			t.Errorf("false positive on %q: %s", content, hint)
		}
	}
}

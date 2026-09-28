package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// replace_lines refused a wrong range with "The numbers you used are stale"
// every time. Smoke run 2026-09-27 (smallrung_toml): the model read lines 1-214
// and 1401-1569 of a file that had not changed, and asked for line 169 with
// text that is only on lines 1418-1548. The refusal was right; the cause it
// named was not. It now claims a cause only when the evidence shows it, and
// says where the expected text is.

// checkerFile has `result = _run_check(` on three lines (4, 8, 11).
const checkerFile = "import os\n\ndef a():\n    result = _run_check(\n    return result\n\ndef b():\n" +
	"    result = _run_check(\n    return result\n\n    result = _run_check(\n"

func replaceWrongLine(t *testing.T, ctx *AgentContext) string {
	t.Helper()
	res := exactEditCall(t, ctx, "replace_lines", map[string]interface{}{
		"path": "checker.py", "start_line": 1, "end_line": 1,
		"expected_first_line": "result = _run_check(", "expected_last_line": "result = _run_check(",
		"content": "import sys",
	})
	if res.Success {
		t.Fatal("a range whose first line does not match was applied")
	}
	return res.Error
}

func TestAnUnchangedFileIsNotCalledStale(t *testing.T) {
	ctx, _ := exactEditWorld(t, "checker.py", checkerFile)
	msg := replaceWrongLine(t, ctx)
	if strings.Contains(msg, "stale") {
		t.Errorf("the file never changed, yet the numbers are called stale:\n%s", msg)
	}
	if !strings.Contains(msg, "has not changed since you read it") {
		t.Errorf("the refusal does not say the numbers never matched:\n%s", msg)
	}
}

func TestAChangedFileIsCalledStale(t *testing.T) {
	ctx, path := exactEditWorld(t, "checker.py", checkerFile)
	// Something else rewrote the file after the read.
	if err := os.WriteFile(path, []byte("# header\n"+checkerFile), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if msg := replaceWrongLine(t, ctx); !strings.Contains(msg, "changed since you last read it") {
		t.Errorf("a file changed after the read is not called stale:\n%s", msg)
	}

	ctx, _ = exactEditWorld(t, "checker.py", checkerFile)
	ctx.SessionWrites["checker.py"] = true // the session's own edit
	if msg := replaceWrongLine(t, ctx); !strings.Contains(msg, "your own edits move line numbers") {
		t.Errorf("a file the session wrote is not called stale:\n%s", msg)
	}
}

func TestTheRefusalSaysWhereTheTextIs(t *testing.T) {
	ctx, _ := exactEditWorld(t, "checker.py", checkerFile)
	if msg := replaceWrongLine(t, ctx); !strings.Contains(msg, "That text is on lines 4, 8, 11: send the one you mean.") {
		t.Errorf("the refusal does not list the lines holding the text:\n%s", msg)
	}
	for want, lines := range map[string][]string{
		"That text is not in the file.": {"a", "b"},
		"That text is at line 2.":       {"a", "  x = 1", "b"},
	} {
		if got := whereTextIs(lines, "x = 1"); got != want {
			t.Errorf("whereTextIs(%q) = %q, want %q", lines, got, want)
		}
	}
}

// With no evidence either way, the refusal names no cause.
func TestNoCauseIsClaimedWithoutEvidence(t *testing.T) {
	ctx, path := exactEditWorld(t, "checker.py", checkerFile)
	ctx.OriginalContent[path] = "only the head\n" // first seen through a truncated read
	msg := replaceWrongLine(t, ctx)
	if strings.Contains(msg, "stale") || strings.Contains(msg, "has not changed") {
		t.Errorf("a cause was claimed without evidence:\n%s", msg)
	}
	if !strings.Contains(msg, "Those line numbers do not match the file.") {
		t.Errorf("the neutral wording is missing:\n%s", msg)
	}
}

package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// A replace_lines range whose first line does not match is refused. The
// refusal claims a cause only when the evidence shows it, and says where the
// expected text is. A model can read two parts of an unchanged file and ask
// for the wrong line: calling its numbers "stale" then names a cause that is
// not true.

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

func TestAReplaceLinesMissNamesOnlyTheCauseItCanShow(t *testing.T) {
	for _, tc := range []struct {
		name string
		// Changes the world after the read; nil leaves it as read.
		setup     func(t *testing.T, ctx *AgentContext, path string)
		want      string
		forbidden []string
	}{
		{
			name: "file unchanged since the read",
			want: "has not changed since you read it", forbidden: []string{"stale"},
		},
		{
			name: "file rewritten by something else after the read",
			setup: func(t *testing.T, ctx *AgentContext, path string) {
				if err := os.WriteFile(path, []byte("# header\n"+checkerFile), 0o644); err != nil {
					t.Fatal(err)
				}
				later := time.Now().Add(2 * time.Second)
				if err := os.Chtimes(path, later, later); err != nil {
					t.Fatal(err)
				}
			},
			want: "changed since you last read it",
		},
		{
			name: "file the session wrote",
			setup: func(t *testing.T, ctx *AgentContext, path string) {
				ctx.SessionWrites["checker.py"] = true
			},
			want: "your own edits move line numbers",
		},
		{
			// First seen through a truncated read: no evidence either way.
			name: "no evidence either way",
			setup: func(t *testing.T, ctx *AgentContext, path string) {
				ctx.OriginalContent[path] = "only the head\n"
			},
			want: "Those line numbers do not match the file.", forbidden: []string{"stale", "has not changed"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, path := exactEditWorld(t, "checker.py", checkerFile)
			if tc.setup != nil {
				tc.setup(t, ctx, path)
			}
			msg := replaceWrongLine(t, ctx)
			if !strings.Contains(msg, tc.want) {
				t.Errorf("the refusal does not say %q:\n%s", tc.want, msg)
			}
			for _, f := range tc.forbidden {
				if strings.Contains(msg, f) {
					t.Errorf("the refusal claims %q without evidence:\n%s", f, msg)
				}
			}
		})
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

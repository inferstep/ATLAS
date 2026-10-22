package main

import (
	"strings"
	"testing"
)

// An unclosed brace in the middle of a file is a bug, not a truncated write.
// Told that its call looked truncated, a model resends the same content until
// the repetition breaker ends the session. Truncation puts the failure at the
// end of the content, and the syntax check's line number shows where it is.

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

func TestFallbackSyntaxRejection(t *testing.T) {
	for _, c := range []struct {
		name, path, content, checkerError string
		truncation                        bool
		quotes                            []string
	}{
		{"unclosed brace mid-file", "test_store.py", midFileUnclosed(),
			"SyntaxError: '{' was never closed (line 42)",
			false, []string{"The offending line 42", "d = {'a': 1"}},
		// Even then, showing the line costs nothing and grounds the retry.
		{"failure on the last line", "a.py", "def f():\n    return {\n",
			"SyntaxError: '{' was never closed (line 2)",
			true, []string{"The offending line 2"}},
		// Without a location, truncation is the only guess.
		{"no line number", "a.py", "x = (\n",
			"SyntaxError: unexpected EOF while parsing",
			true, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			msg := fallbackSyntaxRejection(c.path, c.content, c.checkerError)
			if got := strings.Contains(msg, "truncated tool call"); got != c.truncation {
				t.Errorf("reads as truncation: %v, want %v: %s", got, c.truncation, msg)
			}
			for _, want := range c.quotes {
				if !strings.Contains(msg, want) {
					t.Errorf("must quote %q: %s", want, msg)
				}
			}
		})
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
// may still parse, or die with an IndentationError whose reported line is a
// later casualty, so the hint names the fused comment line itself. A comment
// or string that only mentions \n is left alone.
func TestFusedLineHint(t *testing.T) {
	for _, c := range []struct {
		name, content string
		quotes        []string // nil: no hint
	}{
		{"fused comment",
			"x = 1\n" + "# If the value changes, it's a transition\\n        if a != b:\n" + "    y = 2\n",
			[]string{"line 2", "trapped INSIDE the comment"}},
		{"comment mentioning \\n", "# split the input on \\n characters\nx = 1\n", nil},
		{"string holding \\n", "s = 'a\\nb'\n", nil},
		{"plain comment", "# plain comment\nif a != b:\n    pass\n", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			hint := fusedLineHint(c.content)
			if (hint != "") != (c.quotes != nil) {
				t.Fatalf("hint = %q, want a hint: %v", hint, c.quotes != nil)
			}
			for _, want := range c.quotes {
				if !strings.Contains(hint, want) {
					t.Errorf("hint must contain %q: %s", want, hint)
				}
			}
		})
	}
}

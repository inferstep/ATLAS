package main

import (
	"strings"
	"testing"
)

// The refusal of a write that does not parse. Which advice it gives decides
// what the model does next: told that complete content was truncated, or that
// an f-string's quotes are at fault when a bracket is, it resends the same
// content until a breaker ends the session. Truncation is claimed only when the
// failure is at the end of the content, where a cut would put it.
func TestFallbackSyntaxRejection(t *testing.T) {
	for _, c := range []struct {
		name, path, content, checkerError string
		opening                           string          // the message starts with this
		says                              map[string]bool // a phrase, and whether the message says it
	}{
		{"an unclosed brace mid-file is a bug, and its line is quoted", "test_store.py",
			"import json\n" + strings.Repeat("# filler\n", 40) + "d = {'a': 1\n" + strings.Repeat("# more\n", 20),
			"SyntaxError: '{' was never closed (line 42)",
			"Your content for test_store.py has a syntax error",
			map[string]bool{"is NOT truncated": true, "The offending line 42 is:\nd = {'a': 1\n": true, "Do NOT resend": true}},
		{"an unclosed brace on the last line is a truncated write", "a.py",
			"def f():\n    return {\n",
			"SyntaxError: '{' was never closed (line 2)",
			"Your content for a.py does not parse",
			map[string]bool{"truncated tool call": true, "The offending line 2 is:\n    return {\n": true, "with the COMPLETE file content": true}},
		{"with no line number, truncation is the only guess", "a.py",
			"x = (\n",
			"SyntaxError: unexpected EOF while parsing",
			"Your content for a.py does not parse",
			map[string]bool{"truncated tool call": true}},
		// The sandbox runs Python 3.13, where nesting the same quote is valid,
		// so an f-string error is a problem inside the braces.
		{"an f-string error points inside the braces", "todo.py",
			"print(f\"{i}: {item[}\")\n",
			"SyntaxError: f-string: closing parenthesis '}' does not match opening parenthesis '[' (todo.py, line 1)",
			"Your content for todo.py has an f-string error",
			map[string]bool{"does not match opening parenthesis": true, "The offending line 1 is:\nprint(f\"{i}: {item[}\")\n": true,
				"Look inside the braces": true, "NOT written": true, "Do NOT resend": true,
				"3.12": false, "older Python": false, "the other quote": false}},
		{"a stray backslash is named, with where a backslash is legal", "todo.py",
			"print(\"hi\") \\ x\n",
			"SyntaxError: unexpected character after line continuation character (line 1)",
			"Your content for todo.py has a stray backslash",
			map[string]bool{"LAST character on its line": true}},
		{"any other syntax error gets the general advice", "a.py",
			"def f(:\n    pass\n",
			"SyntaxError: invalid syntax (line 1)",
			"Your content for a.py has a syntax error",
			map[string]bool{"is NOT truncated": true, "The offending line 1 is:\ndef f(:\n": true,
				"f-string": false, "stray backslash": false}},
	} {
		t.Run(c.name, func(t *testing.T) {
			msg := fallbackSyntaxRejection(c.path, c.content, c.checkerError)
			if !strings.HasPrefix(msg, c.opening) {
				t.Errorf("must open with %q:\n%s", c.opening, msg)
			}
			for phrase, said := range c.says {
				if strings.Contains(msg, phrase) != said {
					t.Errorf("says %q: %v, want %v:\n%s", phrase, !said, said, msg)
				}
			}
		})
	}
}

// An embedded-script finding arrives worded for the model. The advice above is
// about the host file and would be wrong for JavaScript inside a string, so the
// finding is handed back as it is, without its marker.
func TestFallbackSyntaxRejectionHandsBackAnEmbeddedFinding(t *testing.T) {
	finding := "Your content for app.py has a JavaScript syntax error in the <script> block"
	if got := fallbackSyntaxRejection("app.py", "x = 1\n", embeddedScriptErrPrefix+finding); got != finding {
		t.Errorf("got %q, want the finding as it is: %q", got, finding)
	}
}

// The refusal of a write whose embedded JavaScript or CSS is broken. Its first
// line blames the submission, not the file (the file on disk is unchanged); the
// rest names the line and the fix, and says why running the app did not show
// the defect.
func TestFormatEmbeddedScriptRejection(t *testing.T) {
	const where = "the <script> block inside the Python string HTML_TEMPLATE"
	for _, c := range []struct {
		name, path string
		finding    embeddedScriptFinding
		firstLine  string
		says       map[string]bool // a phrase, and whether the message says it
	}{
		{"a JavaScript syntax error", "app.py",
			embeddedScriptFinding{Line: 7, Column: 86, Kind: "javascript", Where: where,
				Message: "unexpected `)`",
				Hint:    "Nothing opened a `(` for it to close — delete the stray `)`, or add the `(` it was meant to close.",
				Text:    "nextDirection = 'DOWN');"},
			"Your content for app.py has a JavaScript syntax error in " + where + " — it was NOT written, and app.py on disk is unchanged.",
			map[string]bool{"line 7: unexpected `)`": true, "  7 | nextDirection = 'DOWN');": true, "delete the stray `)`": true,
				"the Python syntax is valid": true, "still returns 200": true, "do NOT resend it unchanged": true,
				"never closed": false}},
		{"a CSS syntax error speaks of the stylesheet", "templates/index.html",
			embeddedScriptFinding{Line: 4, Kind: "css", Where: "the <style> block",
				Message: "a `{` that is never closed", Hint: "Close the rule with `}`.", Text: "body { margin: 0;"},
			"Your content for templates/index.html has a CSS syntax error in the <style> block — it was NOT written, and templates/index.html on disk is unchanged.",
			map[string]bool{"the HTML syntax is valid": true, "the page loads unstyled": true, "inside the <style> block": true}},
		// The parser reports a missing `}` where it gave up, past the block
		// that needs it; a session once tried to fix that untouched line twice.
		{"a missing closer points at the block that opens it", "app.py",
			embeddedScriptFinding{Line: 202, Kind: "javascript", Where: where,
				Message: "a `}` is missing", Text: "setInterval(draw, 100);",
				OpenedLine: 143, OpenedText: "function draw() {"},
			"Your content for app.py has a JavaScript syntax error in " + where + " — it was NOT written, and app.py on disk is unchanged.",
			map[string]bool{
				"143 | function draw() {   <- this block is never closed":    true,
				"202 | setInterval(draw, 100);   <- the parser gave up here": true,
				"belongs at the end of the block that opens on line 143":     true,
				"not where it goes": true,
			}},
		{"a render loop the edit stopped is not a syntax error", "app.py",
			embeddedScriptFinding{Line: 42, Column: 8, Kind: "javascript", Defect: "stopped_loop", Where: where,
				Message: "`draw` used to run on a repeating timer and now runs once",
				Hint:    "Nothing schedules the next call, so the loop stops after one frame.",
				Text:    "setTimeout(draw, delay);"},
			"Your content for app.py stops a render loop in " + where + " — it was NOT written, and app.py on disk is unchanged.",
			map[string]bool{"line 42: `draw` used to run": true, "still returns 200": true, "freezes after one frame": true,
				"syntax error": false}},
		{"a name declared twice: the browser refuses the whole script", "app.py",
			embeddedScriptFinding{Line: 207, Column: 4, Kind: "javascript", Defect: "redeclaration", Where: where,
				Message: "`score` is declared twice in the same scope",
				Hint:    "Drop this declaration and use the existing `score`, or rename one of them.",
				Text:    "let score = 0;"},
			"Your content for app.py declares the same name twice in " + where + " — it was NOT written, and app.py on disk is unchanged.",
			map[string]bool{"line 207: `score` is declared twice": true, "refuses the script": true, "still returns 200": true,
				"syntax error": false}},
	} {
		t.Run(c.name, func(t *testing.T) {
			msg := formatEmbeddedScriptRejection(c.path, c.finding)
			if first, _, _ := strings.Cut(msg, "\n"); first != c.firstLine {
				t.Errorf("first line:\n%s\nwant:\n%s", first, c.firstLine)
			}
			for phrase, said := range c.says {
				if strings.Contains(msg, phrase) != said {
					t.Errorf("says %q: %v, want %v:\n%s", phrase, !said, said, msg)
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
// later casualty, so the hint names the fused comment line itself.
func TestFusedLineHint(t *testing.T) {
	content := "x = 1\n" + "# If the value changes, it's a transition\\n        if a != b:\n" + "    y = 2\n"
	hint := fusedLineHint(content)
	for _, want := range []string{"line 2", "trapped INSIDE the comment"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint must contain %q: %q", want, hint)
		}
	}
}

// A comment or string that only mentions \n gets no hint.
func TestFusedLineHintLeavesAMentionOfBackslashNAlone(t *testing.T) {
	for _, c := range []struct {
		name, content string
	}{
		{"comment mentioning \\n", "# split the input on \\n characters\nx = 1\n"},
		{"string holding \\n", "s = 'a\\nb'\n"},
		{"plain comment", "# plain comment\nif a != b:\n    pass\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if hint := fusedLineHint(c.content); hint != "" {
				t.Errorf("hint = %q, want none", hint)
			}
		})
	}
}

package main

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// find_file with the filename in `path` and an empty pattern: the hint says
// which argument the name belongs in. A directory, or nothing, gets no hint.
func TestFindFileArgSwapIsNamed(t *testing.T) {
	for _, c := range []struct {
		path  string
		wants []string // nil: no hint
	}{
		// The doubled backslash is how the model writes a literal backslash
		// in the JSON it sends.
		{"app.py", []string{"app.py", "pattern", "path", `app\\.py$`}},
		{"", nil},
		{"src", nil},
		{"a/b.py", nil},
	} {
		t.Run(c.path, func(t *testing.T) {
			hint := findFileArgSwapHint(c.path)
			if (hint != "") != (c.wants != nil) {
				t.Fatalf("hint %q, want a hint: %v", hint, c.wants != nil)
			}
			for _, want := range c.wants {
				if !strings.Contains(hint, want) {
					t.Errorf("hint lacks %q:\n%s", want, hint)
				}
			}
		})
	}
}

// The pattern the hint suggests has to be a regex that matches the file and
// nothing else, or the retry fails too.
func TestFindFileArgSwapSuggestsAWorkingPattern(t *testing.T) {
	hint := findFileArgSwapHint("snake_app.py")
	start := strings.Index(hint, `{"pattern":"`)
	if start < 0 {
		t.Fatalf("no concrete retry in hint: %s", hint)
	}
	// The hint quotes the pattern as the model must paste it into its tool
	// arguments; unquoted, it is the regex the proxy will receive.
	raw := hint[start+len(`{"pattern":`):]
	raw = raw[:strings.LastIndex(raw, `}`)]
	suggested, err := strconv.Unquote(raw)
	if err != nil {
		t.Fatalf("suggested pattern %q is not a valid quoted string: %v", raw, err)
	}
	re, err := regexp.Compile(suggested)
	if err != nil {
		t.Fatalf("suggested pattern %q does not compile: %v", suggested, err)
	}
	if !re.MatchString("snake_app.py") {
		t.Errorf("suggested pattern %q does not match the filename it came from", suggested)
	}
	if re.MatchString("snakeXapp.py") {
		t.Errorf("suggested pattern %q left the dot unescaped", suggested)
	}
}

// An old_str that fails on one corrupted character looks right in a diff. A
// non-ASCII character the file holds nowhere is named, with its codepoint; an
// ASCII mis-copy is left to the closest-line hint.
func TestCorruptedCharacterIsNamed(t *testing.T) {
	file := "        if(headX === food.x && headY === food.y) {\n            score++;\n"
	for _, c := range []struct {
		name, oldStr, file string
		want               []rune
	}{
		{"a decode artefact in place of &&", "if(headX === food.x ℘ headY ===food.y) {", file, []rune{'℘'}},
		{"an em dash the file does not hold", "x = 1 — y", "x = 1 - y\n", []rune{'—'}},
		{"an ASCII mis-copy", "if(headX == food.x && headY === food.y) {", file, nil},
		{"non-ASCII the file holds", "label = 'café ☕'", "label = 'café ☕'\n", nil},
		{"an empty old_str", "", file, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := foreignRunes(c.oldStr, c.file); !slices.Equal(got, c.want) {
				t.Errorf("foreignRunes = %q, want %q", string(got), string(c.want))
			}
		})
	}
	if desc := describeForeignRunes([]rune{'℘'}); !strings.Contains(desc, "U+2118") {
		t.Errorf("the description lacks the codepoint: %q", desc)
	}
}

// A tool the model could not use on a file is withdrawn for that file, stated
// as a fact. The note names what is left: never the banned tool, and
// structural_edit only for the files it can edit (.py, .html).
func TestToolBanNoteNamesTheRemainingTools(t *testing.T) {
	for _, c := range []struct {
		tool, path string
		left       []string
	}{
		{"edit_file", "app.py", []string{"structural_edit", "replace_lines", "write_file"}},
		{"edit_file", "game.js", []string{"replace_lines", "write_file"}},
		{"structural_edit", "pricing.py", []string{"replace_lines", "write_file"}},
		{"replace_lines", "pricing.py", []string{"structural_edit", "write_file"}},
		{"replace_lines", "data.json", []string{"write_file"}},
		{"write_file", "pricing.py", []string{"structural_edit", "replace_lines"}},
	} {
		t.Run(c.tool+" on "+c.path, func(t *testing.T) {
			note := toolBanNote(c.tool, c.path)
			if !strings.Contains(note, "no longer available") {
				t.Errorf("the note does not say the tool is gone: %s", note)
			}
			_, advice, _ := strings.Cut(note, "Use ")
			for _, tool := range []string{"structural_edit", "replace_lines", "write_file", "edit_file"} {
				if strings.Contains(advice, "`"+tool+"`") != slices.Contains(c.left, tool) {
					t.Errorf("advice %q, want it to name exactly %q", advice, c.left)
				}
			}
		})
	}
}

// rejectionClass is in detectors.go; the error-loop breaker that bans a tool
// counts rejections by it. Three different rejections are a model closing in;
// the same rejection on other lines of other files is the loop.
func TestRejectionClassSeparatesConvergingFromLooping(t *testing.T) {
	selector := rejectionClass("`draw` exists in app.py, but NOT as a node any selector can reach: it is javascript at lines 77-202")
	tooLarge := rejectionClass("replace_lines: 126 lines is too large a range (limit 60).")
	stale := rejectionClass("replace_lines: line 202 of app.py is not what you expected, so the range is wrong")
	for _, pair := range [][2]string{{selector, tooLarge}, {tooLarge, stale}, {selector, stale}} {
		if pair[0] == pair[1] {
			t.Errorf("distinct rejections collapsed into one class:\n%q\n%q", pair[0], pair[1])
		}
	}
	other := rejectionClass("replace_lines: line 87 of main.py is not what you expected, so the range is wrong")
	if stale != other {
		t.Errorf("the same failure was split by its variable parts:\n%q\n%q", stale, other)
	}
	if rejectionClass("") != "" {
		t.Error("an empty error must not produce a class")
	}
}

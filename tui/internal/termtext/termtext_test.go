package termtext

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Wrapping cuts between runes, never inside a multi-byte one, keeps each line
// within the width, and loses no text (only the spaces at a cut).
func TestWrapPlainKeepsMultiByteRunesIntact(t *testing.T) {
	s := strings.Repeat("─", 30) + " — " + strings.Repeat("é", 20)
	lines := WrapPlain(s, 12)
	for _, line := range lines {
		if !utf8.ValidString(line) {
			t.Fatalf("WrapPlain produced invalid UTF-8: %q", line)
		}
		if n := len([]rune(line)); n > 12 {
			t.Errorf("line rune count = %d, want <= 12 (%q)", n, line)
		}
	}
	if strings.ReplaceAll(strings.Join(lines, ""), " ", "") != strings.ReplaceAll(s, " ", "") {
		t.Errorf("the lines %q lost text of %q", lines, s)
	}
}

// StripANSI keeps the text and drops each escape sequence: a CSI sequence up
// to its final byte, an OSC sequence up to BEL or ESC \, and any other escape
// with the byte after it.
func TestStripANSIDropsEachEscapeSequence(t *testing.T) {
	for _, c := range []struct {
		name, in, want string
	}{
		{"plain text", "plain text", "plain text"},
		{"a style", "a \x1b[1;31mbold\x1b[0m b", "a bold b"},
		{"an unfinished style runs to the end", "a\x1b[1;3", "a"},
		{"a window title ending in BEL", "a\x1b]0;title\x07b", "ab"},
		{"a link ending in ESC \\", "a\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\b", "alinkb"},
		{"another escape and the byte after it", "a\x1b7b", "ab"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := StripANSI(c.in); got != c.want {
				t.Errorf("StripANSI(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

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

package main

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// controlMark stands in for a control character in text from the proxy.
const controlMark = '\uFFFD'

// visibleControls returns text from the proxy as the terminal should show it:
// each control character other than a newline or a tab becomes controlMark,
// so the text cannot move the cursor, change the window or start a terminal
// sequence; so does each direction mark, so the text cannot change the order
// in which a terminal shows it. A carriage return before a newline is dropped
// with no mark, because a Windows line ending is ordinary text.
func visibleControls(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		if isMarked(r) {
			return controlMark
		}
		return r
	}, s)
}

// isMarked reports a character that text from the proxy may not bring to the
// terminal: a control character other than a newline or a tab, or a
// direction mark.
func isMarked(r rune) bool {
	return r != '\n' && r != '\t' && (unicode.IsControl(r) || isDirectionMark(r))
}

// isDirectionMark reports the Unicode marks that change the order of text on
// a terminal that shows right-to-left text: U+202A to U+202E, U+2066 to
// U+2069, and the single marks U+200E, U+200F and U+061C. Other format
// characters, such as the joiner inside an emoji, stay.
func isDirectionMark(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) ||
		r == 0x200E || r == 0x200F || r == 0x061C
}

// characterReference is a numeric character reference, such as "&#27;" or
// "&#x1b;", which the markdown renderer turns into its character.
var characterReference = regexp.MustCompile(`&#(?:[0-9]+|[xX][0-9a-fA-F]+);`)

// withMarkedReferences returns markdown with each numeric character reference
// to a marked character replaced by the reference to controlMark, so that the
// renderer shows the mark. Every other reference stays as written.
func withMarkedReferences(markdown string) string {
	return characterReference.ReplaceAllStringFunc(markdown, func(reference string) string {
		digits, base := reference[2:len(reference)-1], 10
		if digits[0] == 'x' || digits[0] == 'X' {
			digits, base = digits[1:], 16
		}
		code, err := strconv.ParseInt(digits, base, 32)
		if err != nil || !isMarked(rune(code)) {
			return reference
		}
		return "&#xFFFD;"
	})
}

// styleSequence is a style (SGR) sequence, the one kind of terminal sequence
// the markdown renderer writes.
var styleSequence = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// visibleControlsKeepingStyles is visibleControls for the markdown renderer's
// output: the renderer's style sequences stay, and each other control
// character or direction mark is marked, whatever way it reached the output.
func visibleControlsKeepingStyles(s string) string {
	var b strings.Builder
	last := 0
	for _, style := range styleSequence.FindAllStringIndex(s, -1) {
		b.WriteString(visibleControls(s[last:style[0]]))
		b.WriteString(s[style[0]:style[1]])
		last = style[1]
	}
	b.WriteString(visibleControls(s[last:]))
	return b.String()
}

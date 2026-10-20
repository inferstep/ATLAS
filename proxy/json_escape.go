package main

import (
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// writeJSONEscape writes the character that the JSON escape at the start of s
// stands for, and returns how many bytes of s it took. s starts with a
// backslash. A \uXXXX escape is decoded, and two that form a UTF-16 surrogate
// pair are one character; a surrogate alone is U+FFFD, as encoding/json reads
// it. An escape it does not know stands for its letter.
func writeJSONEscape(sb *strings.Builder, s string) int {
	switch s[1] {
	case 'n':
		sb.WriteByte('\n')
	case 't':
		sb.WriteByte('\t')
	case 'r':
		sb.WriteByte('\r')
	case 'u':
		if r, size := unicodeEscape(s); size > 0 {
			sb.WriteRune(r)
			return size
		}
		sb.WriteByte('u')
	default: // a quote, a backslash, a slash, or a letter it does not know
		sb.WriteByte(s[1])
	}
	return 2
}

// unicodeEscape reads the \uXXXX escape at the start of s, and a second one
// when the first is half of a surrogate pair. size is 0 when s does not start
// with a whole \uXXXX.
func unicodeEscape(s string) (r rune, size int) {
	first, ok := hex4(s)
	if !ok {
		return 0, 0
	}
	if !utf16.IsSurrogate(first) {
		return first, 6
	}
	if second, ok := hex4(s[6:]); ok {
		if pair := utf16.DecodeRune(first, second); pair != utf8.RuneError {
			return pair, 12
		}
	}
	return utf8.RuneError, 6
}

// hex4 reads a \uXXXX escape at the start of s.
func hex4(s string) (rune, bool) {
	if len(s) < 6 || s[0] != '\\' || s[1] != 'u' {
		return 0, false
	}
	n, err := strconv.ParseUint(s[2:6], 16, 16)
	return rune(n), err == nil
}

// Package termtext is text cut, wrapped and made visible for the terminal.
// Truncate, ClipColumns and WrapPlain fit text to a width and StripANSI takes
// its styles out; visible_controls.go marks the control characters in text
// from the proxy.
package termtext

import "strings"

// Truncate cuts s to n characters, the last of them "…". It cuts between
// characters, never inside one: a cut inside a character would leave a broken
// byte after the text was marked (VisibleControls).
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 1 {
		return string(runes[:n])
	}
	return string(runes[:n-1]) + "…"
}

// ClipColumns returns s[lo:hi] in rune positions, clamped to the
// string's actual length. Used to apply the column-precision clip on
// the first and last lines of a multi-line drag.
func ClipColumns(s string, lo, hi int) string {
	r := []rune(s)
	if lo < 0 {
		lo = 0
	}
	if hi < lo {
		hi = lo
	}
	if hi > len(r) {
		hi = len(r)
	}
	if lo > len(r) {
		return ""
	}
	return string(r[lo:hi])
}

// WrapPlain hard-wraps long lines at the given width. Nothing fancy —
// just splits at whitespace when possible. Operates on runes so a
// multi-byte character (em dash, box drawing, CJK) is never split
// mid-sequence and width counts characters, not bytes.
func WrapPlain(s string, width int) []string {
	if width < 8 {
		width = 8
	}
	out := []string{}
	for _, line := range strings.Split(s, "\n") {
		runes := []rune(line)
		for len(runes) > width {
			cut := width
			for i := width - 1; i > width/2; i-- {
				if runes[i] == ' ' {
					cut = i
					break
				}
			}
			out = append(out, string(runes[:cut]))
			runes = runes[cut:]
			for len(runes) > 0 && runes[0] == ' ' {
				runes = runes[1:]
			}
		}
		out = append(out, string(runes))
	}
	return out
}

// StripANSI removes ANSI CSI / OSC sequences from s. Bubbletea/lipgloss
// embed lots of styling in chat lines; the clipboard only wants the
// human-readable characters.
func StripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		if c == 0x1b && i+1 < len(s) {
			switch s[i+1] {
			case '[':
				i = csiEnd(s, i+2)
			case ']':
				i = oscEnd(s, i+2)
			default:
				i += 2
			}
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

// csiEnd returns the index just past a CSI sequence whose body starts at
// s[j]. CSI: ESC [ ... <final byte 0x40-0x7e>
func csiEnd(s string, j int) int {
	for j < len(s) {
		if s[j] >= 0x40 && s[j] <= 0x7e {
			j++
			break
		}
		j++
	}
	return j
}

// oscEnd returns the index just past an OSC sequence whose body starts at
// s[j]. OSC: ESC ] ... BEL or ESC \
func oscEnd(s string, j int) int {
	for j < len(s) && s[j] != 0x07 {
		if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
			j += 2
			break
		}
		j++
	}
	if j < len(s) && s[j] == 0x07 {
		j++
	}
	return j
}

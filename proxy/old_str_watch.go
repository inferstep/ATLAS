package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// An edit_file old_str that has stopped matching its file, and keeps going.
//
// old_str is text copied from the target file, so it can be checked while it
// streams: once no completion of what has arrived can match the file, the
// call cannot succeed, and every further token is spent on a call edit_file
// will refuse. The content-loop cut catches such a runaway only when its tail
// repeats verbatim, and one that carries a changing number never does.
// Measured in the fc8321d smoke runs (smallrung_toml run a, rep 1, turn 3): an
// old_str cycling five comment lines, each with a read_file line number from
// 1596 to 1882, ran 25,333 characters and 322 s to the token cap. In the
// ccc71fc runs, 9 of 20 loop cuts were an edit_file old_str.
//
// "Can still match" is the edit tool's own tolerance, never less, so a long
// old_str that does match is never cut:
//   - exact bytes, or with curly and straight quotes treated alike
//     (findActualString);
//   - the same with read_file's line-number prefixes removed, which edit_file
//     refuses with the replace_lines call that makes the change, so that
//     refusal still happens;
//   - each line equal to a file line once leading and trailing whitespace is
//     ignored (findFuzzyLineMatch).
//
// A line that is only whitespace so far can still become any line under the
// last rule, so a run of tabs or spaces is never cut here; the content-loop
// cut handles those. And an old_str that stopped matching is given
// oldStrRunOn more characters before the cut: a short mis-copy closes on its
// own and gets edit_file's detailed refusal, as it did before.

// oldStrRunOn is how far an old_str may run past the last point where it could
// still match before the stream is cut.
const oldStrRunOn = 256

// oldStrKeyRe finds where an old_str value opens in a streaming tool call.
var oldStrKeyRe = regexp.MustCompile(`"old_str"\s*:\s*"`)

// oldStrWatch follows one streaming response. It loads the target file once.
type oldStrWatch struct {
	ctx        *AgentContext
	path       string
	content    string
	lines      []string // the file's lines, each trimmed
	loaded     bool
	unreadable bool
	goodLen    int // longest old_str prefix, in bytes, known to still match

	// Set when the watch cut the stream, for the model's feedback.
	Cut          bool
	Path         string
	MatchedLines int    // complete old_str lines that still matched
	FirstBadLine string // the old_str line where matching stopped
	Chars        int    // old_str bytes received at the cut
}

func newOldStrWatch(ctx *AgentContext) *oldStrWatch {
	return &oldStrWatch{ctx: ctx}
}

// runaway reports whether the streamed response is an edit_file call whose
// old_str stopped matching its file at least oldStrRunOn bytes ago.
func (w *oldStrWatch) runaway(stream string) bool {
	if w == nil || w.Cut || w.unreadable {
		return false
	}
	rc := recoverCutCall(stream)
	if rc.Tool != "edit_file" || rc.CutField != "old_str" || rc.Args["path"] == "" {
		return false
	}
	partial, ok := openOldStr(stream)
	partial = trimIncompleteRune(partial)
	if !ok || partial == "" {
		return false
	}
	if !w.load(rc.Args["path"]) {
		return false
	}
	if oldStrCanMatch(w.content, w.lines, partial) {
		w.goodLen = len(partial)
		return false
	}
	// Monotone: once a prefix cannot match, no longer one can. Search between
	// the last length that could and this one for where matching stopped.
	lo, hi := w.goodLen, len(partial)
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		for mid > lo && !utf8.RuneStart(partial[mid]) {
			mid--
		}
		if mid == lo {
			break
		}
		if oldStrCanMatch(w.content, w.lines, partial[:mid]) {
			lo = mid
		} else {
			hi = mid
		}
	}
	w.goodLen = lo
	if len(partial)-lo < oldStrRunOn {
		return false
	}
	w.Cut, w.Path, w.Chars = true, w.path, len(partial)
	w.MatchedLines = strings.Count(partial[:lo], "\n")
	lineStart := strings.LastIndex(partial[:lo], "\n") + 1
	lineEnd := strings.Index(partial[lo:], "\n")
	if lineEnd < 0 {
		lineEnd = len(partial)
	} else {
		lineEnd += lo
	}
	w.FirstBadLine = strings.TrimSpace(partial[lineStart:lineEnd])
	return true
}

func (w *oldStrWatch) load(path string) bool {
	if w.loaded {
		return w.path == path
	}
	w.loaded, w.path = true, path
	if w.ctx == nil {
		w.unreadable = true
		return false
	}
	data, _, err := readWorkspaceFile(w.ctx, path)
	if err != nil {
		w.unreadable = true
		return false
	}
	w.content = string(data)
	for _, l := range strings.Split(w.content, "\n") {
		w.lines = append(w.lines, strings.TrimSpace(l))
	}
	return true
}

// openOldStr decodes the value of an old_str that is still open at the end of
// the stream. It returns false when the value has closed, or none opened. An
// escape cut in the middle is dropped: it has not arrived.
func openOldStr(stream string) (string, bool) {
	loc := oldStrKeyRe.FindAllStringIndex(stream, -1)
	if len(loc) == 0 {
		return "", false
	}
	s := stream[loc[len(loc)-1][1]:]
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			return "", false // the value closed
		case c != '\\':
			sb.WriteByte(c)
			continue
		}
		if i+1 >= len(s) {
			break
		}
		i++
		switch s[i] {
		case 'n':
			sb.WriteByte('\n')
		case 't':
			sb.WriteByte('\t')
		case 'r':
			sb.WriteByte('\r')
		case 'b':
			sb.WriteByte('\b')
		case 'f':
			sb.WriteByte('\f')
		case 'u':
			if i+4 >= len(s) {
				return sb.String(), true
			}
			n, err := strconv.ParseUint(s[i+1:i+5], 16, 16)
			if err != nil {
				return sb.String(), true
			}
			sb.WriteRune(rune(n))
			i += 4
		default: // \" \\ \/
			sb.WriteByte(s[i])
		}
	}
	return sb.String(), true
}

// trimIncompleteRune drops a UTF-8 sequence cut at the end: a check can land
// inside a multi-byte character (a curly quote), and half of one matches
// nothing.
func trimIncompleteRune(s string) string {
	for back := 1; back <= utf8.UTFMax && back <= len(s); back++ {
		i := len(s) - back
		if !utf8.RuneStart(s[i]) {
			continue
		}
		if !utf8.FullRuneInString(s[i:]) {
			return s[:i]
		}
		return s
	}
	return s
}

// oldStrCanMatch reports whether some completion of partial could be matched
// in content by edit_file, under the tolerances described above.
func oldStrCanMatch(content string, trimmedLines []string, partial string) bool {
	if partial == "" || foundUpToQuotes(content, partial) {
		return true
	}
	if stripped := stripPartialLineNumbers(partial); stripped != partial && foundUpToQuotes(content, stripped) {
		return true
	}
	return fuzzyLinesCanMatch(trimmedLines, partial)
}

func foundUpToQuotes(content, s string) bool {
	return strings.Contains(content, s) || strings.Contains(content, normalizeQuotes(s)) ||
		strings.Contains(content, denormalizeQuotes(s))
}

// digitsOnlyRe is a line that may be a read_file line-number prefix still
// arriving: blanks and digits, the tab not yet sent.
var digitsOnlyRe = regexp.MustCompile(`^[ \t]*\d*$`)

// stripPartialLineNumbers removes read_file's line-number prefixes, as
// stripLineNumberPrefixes does, from a value that may end mid-prefix.
func stripPartialLineNumbers(s string) string {
	head, last := "", s
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		head, last = s[:i+1], s[i+1:]
	}
	if digitsOnlyRe.MatchString(last) {
		last = ""
	}
	return stripLineNumberPrefixes(head) + stripLineNumberPrefixes(last)
}

// fuzzyLinesCanMatch is findFuzzyLineMatch's rule for a value still arriving:
// its complete lines equal consecutive file lines once trimmed, and its last,
// partial line can still grow into the next one. Uniqueness is not asked: a
// later line may settle it.
func fuzzyLinesCanMatch(trimmedLines []string, partial string) bool {
	lines := strings.Split(partial, "\n")
	complete, last := lines[:len(lines)-1], lines[len(lines)-1]
	lastLeft := strings.TrimLeft(last, " \t\r")
	need := len(complete)
	if lastLeft != "" {
		need++
	}
	for i := 0; i+need <= len(trimmedLines); i++ {
		ok := true
		for j, l := range complete {
			if trimmedLines[i+j] != strings.TrimSpace(l) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		if lastLeft == "" {
			return true
		}
		next := trimmedLines[i+len(complete)]
		if strings.HasPrefix(next, lastLeft) || next == strings.TrimSpace(last) {
			return true
		}
	}
	return false
}

// oldStrCutFeedback is what the model is told after the cut.
func oldStrCutFeedback(w *oldStrWatch) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Your edit_file call on %s was stopped while old_str was still arriving: ", w.Path)
	if w.MatchedLines > 0 {
		fmt.Fprintf(&sb, "its first %d line(s) are text in %s, but from the line `%s` on, old_str matches "+
			"nothing in the file, and it kept going. ", w.MatchedLines, w.Path, truncateStr(w.FirstBadLine, 80))
	} else {
		fmt.Fprintf(&sb, "from its first line, `%s`, old_str matches nothing in %s, and it kept going. ",
			truncateStr(w.FirstBadLine, 80), w.Path)
	}
	sb.WriteString("Nothing was executed and the file is unchanged. old_str must be text copied from the file, " +
		"and one short line that appears once in it is enough to place an edit. Pick that line from what " +
		"read_file showed you, without its line number, or use replace_lines with the line numbers " +
		"read_file showed.")
	return sb.String()
}

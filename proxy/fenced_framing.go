package main

import (
	"fmt"
	"regexp"
	"strings"
)

// Fenced-payload framing.
//
// "@fenced" routes a file body around the JSON channel, and the parent then
// has to answer one question about whatever came back: is this a whole file?
//
// It used to answer that from a single signal -- an opening fence with no
// closing fence -- and treat everything else as complete. "Everything else"
// includes a bare body with no fence at all, which proves nothing in either
// direction. The sealed Stage-A acquisition inlined 42 bodies and NOT ONE of
// them opened a fence, so the guard could not fire on any of them; one had
// stopped mid-emission and 1106 unframed bytes were written, parsed cleanly,
// and did nothing. A syntax gate cannot catch that, because a body cut in the
// middle of a comment is valid in most languages.
//
// So framing is the only evidence used here. A payload is a file when its
// protocol framing says the emission finished: an opening fence and a closing
// fence. Nothing below inspects the body -- not its language, syntax, final
// line, comments, indentation, prose, or length. A completed emission of
// something that looks unfinished is a file; an unterminated emission of
// something that looks finished is not.
type fenceFraming int

const (
	// fenceFramingComplete: opening and closing fences both present. The
	// body between them is the file.
	fenceFramingComplete fenceFraming = iota
	// fenceFramingUnterminated: fence markers are present but do not close a
	// block. The emission was cut, or never framed what followed.
	fenceFramingUnterminated
	// fenceFramingAbsent: no fence markers at all. There is no framing
	// evidence, so completion cannot be established either way.
	fenceFramingAbsent
	// fenceFramingAmbiguous: an interior line would itself close the outer
	// fence, so the reply is two blocks (a file plus a "run it with" snippet)
	// or a file whose own ``` lines sit inside a ``` wrapper. Which bytes are
	// the file cannot be known; guessing wrote the second block as the file.
	fenceFramingAmbiguous
)

func (f fenceFraming) String() string {
	switch f {
	case fenceFramingComplete:
		return "complete"
	case fenceFramingUnterminated:
		return "fence opened, never closed"
	case fenceFramingAmbiguous:
		return "more than one fence closes the block"
	default:
		return "no fence at all"
	}
}

// classifyFencedPayload is the ONE framing decision. The inline path and the
// sub-call path both route through it, so the two cannot drift into
// disagreeing about what counts as a finished emission.
//
// A body is returned only for fenceFramingComplete. Every other outcome
// returns "" so that no caller can accidentally use bytes whose framing did
// not prove they are whole.
func classifyFencedPayload(payload string) (fenceFraming, string) {
	return parseFencedReply(payload)
}

// fenceOpenRe / fenceCloseRe follow CommonMark fence lines: up to three spaces
// of indentation, a run of at least three backticks, and for an opener an
// optional info string (no backticks). A closer carries no info string. The
// trailing \r? keeps CRLF replies working without touching body bytes.
var (
	fenceOpenRe  = regexp.MustCompile("^ {0,3}(`{3,})[^`\\r\\n]*\\r?$")
	fenceCloseRe = regexp.MustCompile("^ {0,3}(`{3,})[ \\t]*\\r?$")
)

// parseFencedReply finds the file body in a fenced reply by fence LINES, not a
// lazy or greedy regex over the whole reply.
//
// The body is every line between the first opening fence and the LAST line
// that closes it (at least as many backticks), joined with the newline that
// ended each line, so its bytes -- CRLF included -- are exactly what the model
// wrote, ending in the line break before the closing fence. Prose before the
// opener or after the closer is not part of the file.
//
// Measured on c5927b3, the regex form did three wrong things with valid
// replies: trailing prose after the closing fence made it fall back to a lazy
// match that cut the file at its first interior ``` (a docstring example, a
// Markdown code block, a YAML block scalar); a four-backtick wrapper, the
// Markdown way to carry ``` inside a block, never matched at all; and a reply
// holding the file plus a separate "run it with" block was captured from the
// first opener to the last closer, so the bash snippet was written as a.py.
//
// A line inside the block that would itself close it is the one case where the
// file cannot be told apart from a second block. That is refused as ambiguous,
// with a retry path (wrap the file in four backticks), never guessed.
func parseFencedReply(reply string) (fenceFraming, string) {
	lines := strings.Split(reply, "\n")
	open, width := -1, 0
	for i, l := range lines {
		if m := fenceOpenRe.FindStringSubmatch(l); m != nil {
			open, width = i, len(m[1])
			break
		}
	}
	if open < 0 {
		if strings.Contains(reply, "```") {
			return fenceFramingUnterminated, ""
		}
		return fenceFramingAbsent, ""
	}
	closeAt := -1
	for i := len(lines) - 1; i > open; i-- {
		if m := fenceCloseRe.FindStringSubmatch(lines[i]); m != nil && len(m[1]) >= width {
			closeAt = i
			break
		}
	}
	if closeAt < 0 {
		return fenceFramingUnterminated, ""
	}
	for i := open + 1; i < closeAt; i++ {
		if m := fenceCloseRe.FindStringSubmatch(lines[i]); m != nil && len(m[1]) >= width {
			return fenceFramingAmbiguous, ""
		}
	}
	body := strings.Join(lines[open+1:closeAt], "\n")
	if strings.TrimSpace(body) == "" {
		return fenceFramingUnterminated, ""
	}
	return fenceFramingComplete, body + "\n"
}

// extractFencedContent returns the file body of a fenced reply, or "" unless
// the framing proved a single complete block.
func extractFencedContent(reply string) string {
	if framing, body := parseFencedReply(reply); framing == fenceFramingComplete {
		return body
	}
	return ""
}

// fencedRetryNote is what the sub-call is told after a reply that did not
// yield a file, naming the actual framing problem.
func fencedRetryNote(framing fenceFraming, tag string) string {
	if framing == fenceFramingAmbiguous {
		return fmt.Sprintf("[system note]: That reply had more than one fence closing the block, so "+
			"which lines are the file is ambiguous. Reply with exactly ONE fenced block containing the "+
			"complete file and nothing else. If the file itself contains ``` lines, open and close the "+
			"block with four backticks (````%s ... ````).", tag)
	}
	return fmt.Sprintf("[system note]: That had no fenced block. Reply with ONE ```%s fenced block "+
		"containing the complete file, nothing else.", tag)
}

// resolveInlineFencedBody decides whether the bytes the model inlined after
// the "@fenced" sentinel may be used as the file.
//
// ok is true only when framing proved the emission finished. Otherwise the
// caller falls back to the sub-call -- the channel built to carry a file
// body -- and why names the framing truthfully for the log.
func resolveInlineFencedBody(inline string) (body string, ok bool, why fenceFraming) {
	framing, resolved := classifyFencedPayload(inline)
	if framing == fenceFramingComplete {
		return resolved, true, framing
	}
	return "", false, framing
}

// isFencedSentinel reports whether write_file content is the "@fenced"
// sentinel: exactly "@fenced", or "@fenced" followed by a line break that
// introduces an inline fenced body. Content that merely begins with those
// characters is file content -- measured: `@fenced_route("/x")`, the first line
// of a Python module, opened a fenced sub-call and the file was never written.
func isFencedSentinel(content string) bool {
	t := strings.TrimSpace(content)
	if !strings.HasPrefix(t, "@fenced") {
		return false
	}
	rest := strings.TrimLeft(t[len("@fenced"):], " \t")
	return rest == "" || strings.HasPrefix(rest, "\n") || strings.HasPrefix(rest, "\r\n")
}

// fenceBlockGrammar restricts a reply to exactly what the sub-call asks for:
// an opening fence with the requested tag, any number of lines that cannot
// close the fence early, and the closing fence. It is a GBNF grammar, the
// same llama-server interface the main loop already uses, and says nothing
// about any model or chat template.
//
// The outer fence is FOUR backticks. With three, no body line could start
// with ```, so the first ``` line a file needed -- a Markdown code block, a
// docstring example -- was the only thing the grammar allowed there: the
// closer. Generation had to stop, the reply parsed as a complete block, and
// the truncated file was written (audit P-safety/INTEGRITY#5). Body lines may
// now start with up to three backticks; parseFencedReply matches the closer
// by width, so they stay inside the file. A file that itself has a column-0
// ```` line is sent without the grammar (fencedGrammarFits).
// fenceGrammarRoot is how every grammar fenceBlockGrammar builds begins, so
// the stream reader can tell a fenced sub-call from an ordinary turn by its
// grammar (isFenceBlockGrammar).
const fenceGrammarRoot = "root ::= \"````"

// isFenceBlockGrammar reports whether grammar is one fenceBlockGrammar built.
func isFenceBlockGrammar(grammar string) bool {
	return strings.HasPrefix(grammar, fenceGrammarRoot)
}

//
// For a code file the block may also end on a line of exactly three
// backticks, the way the model closes a block. Ending there is allowed, never
// forced: such a line may still be a body line, and the model decides. Without
// it the model's closer was a body line, the grammar refused to end, and the
// attempt ran on (writing its next tool calls into the file) until the idle
// watchdog cut it: in the smoke run on 4403ae8 (2026-09-28), 14 of 29 fenced
// writes waited that way, median 53 s. Markdown and untyped files keep the
// four-backtick closer, because a column-0 ``` line can be their content.
func fenceBlockGrammar(tag string) string {
	safe := make([]rune, 0, len(tag))
	for _, r := range tag {
		if r == '-' || r == '+' || r == '#' || r == '.' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			safe = append(safe, r)
		}
	}
	closer := "\"````\""
	if fenceShortCloserAllowed(string(safe)) {
		closer = "( \"````\" | \"```\" \"\\n\"? )"
	}
	return fmt.Sprintf(fenceGrammarRoot+"%s\\n\" line* %s\n"+
		"line ::= ( [^`\\n] [^\\n]* | \"`\" ( [^`\\n] [^\\n]* )? | \"``\" ( [^`\\n] [^\\n]* )? | "+
		"\"```\" ( [^`\\n] [^\\n]* )? )? \"\\n\"\n",
		string(safe), closer)
}

// fenceShortCloserAllowed reports whether a fenced block for this language tag
// may end on three backticks: a known code tag. Markdown and the bare fence
// (an untyped file) may hold a column-0 ``` line as content.
func fenceShortCloserAllowed(tag string) bool {
	return tag != "" && tag != "markdown"
}

// closeShortFence makes the three-backtick closer the grammar accepted into
// the four-backtick one parseFencedReply matches the opener with. It changes
// only a reply whose opener is four backticks, that has no line closing it,
// and whose last non-blank line is exactly three backticks; any other reply
// comes back as it was. Interior ``` lines stay in the body.
func closeShortFence(reply string) string {
	lines := strings.Split(reply, "\n")
	open, width := -1, 0
	for i, l := range lines {
		if m := fenceOpenRe.FindStringSubmatch(l); m != nil {
			open, width = i, len(m[1])
			break
		}
	}
	if open < 0 || width < 4 {
		return reply
	}
	last := len(lines) - 1
	for last > open && strings.TrimSpace(lines[last]) == "" {
		last--
	}
	for i := open + 1; i <= last; i++ {
		if m := fenceCloseRe.FindStringSubmatch(lines[i]); m != nil && len(m[1]) >= width {
			return reply
		}
	}
	if last <= open || strings.TrimRight(lines[last], " \t\r") != "```" {
		return reply
	}
	lines[last] = strings.Repeat("`", width)
	return strings.Join(lines, "\n")
}

// fencedGrammarFits reports whether a file's current text can be carried
// inside the four-backtick fence: none of its lines starts with four
// backticks at column 0, which the grammar reserves for the closer.
func fencedGrammarFits(current string) bool {
	for _, line := range strings.Split(current, "\n") {
		if strings.HasPrefix(line, "````") {
			return false
		}
	}
	return true
}

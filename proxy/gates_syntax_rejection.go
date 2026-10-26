// Syntax rejections: the text handed back to the model when a write is
// refused because its content does not parse, or because the JavaScript or
// CSS embedded in it does not. Each one names the line and what to do.
// stoppedLoopRejection is here too, for JavaScript that parses but stops
// its render loop.

package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// embeddedScriptRejectionFor unwraps a checkFallbackSyntax error that turned
// out to be an embedded-script finding. (message, true) when it is one — the
// message is already model-ready — else ("", false).
func embeddedScriptRejectionFor(syntaxErr string) (string, bool) {
	if !strings.HasPrefix(syntaxErr, embeddedScriptErrPrefix) {
		return "", false
	}
	return strings.TrimPrefix(syntaxErr, embeddedScriptErrPrefix), true
}

// formatEmbeddedScriptRejection names the file, the line, the offending
// construct and the fix — and spells out WHY the model's usual verification
// missed it, because "I ran it and curled the page" is exactly the evidence
// that made the broken snake game look done.
func formatEmbeddedScriptRejection(path string, f embeddedScriptFinding) string {
	lang, block, breakage := "JavaScript", "<script>", "the browser stops running the script at that point, so the page loads but nothing on it responds"
	if f.Kind == "css" {
		lang, block, breakage = "CSS", "<style>", "the browser drops the rest of the stylesheet, so the page loads unstyled"
	}
	host := "HTML"
	if strings.ToLower(filepath.Ext(path)) == ".py" {
		host = "Python"
	}
	if f.Defect == "redeclaration" {
		return redeclarationRejection(path, f)
	}
	if f.Defect == "stopped_loop" {
		return stoppedLoopRejection(path, f)
	}
	var sb strings.Builder
	// Blame the SUBMISSION, not the file. The write was refused, so the file
	// on disk is clean — and "app.py has a JavaScript syntax error" sends the
	// model hunting a bug in app.py that is not there. Measured as an H2
	// false rejection on flask_pause: the gate was right, the wording was not.
	fmt.Fprintf(&sb, "Your content for %s has a %s syntax error in %s — it was NOT written, "+
		"and %s on disk is unchanged.\n", path, lang, f.Where, path)
	fmt.Fprintf(&sb, "line %d: %s\n", f.Line, f.Message)
	if f.OpenedLine > 0 {
		// Point at the block, not at the line the parser stopped on. Given
		// only the latter, an observed session tried to "add a `}`" to an
		// untouched `setInterval(...)` line, twice, and never converged.
		fmt.Fprintf(&sb, "  %d | %s   <- this block is never closed\n", f.OpenedLine, f.OpenedText)
		fmt.Fprintf(&sb, "  %d | %s   <- the parser gave up here\n", f.Line, f.Text)
		fmt.Fprintf(&sb, "The `}` belongs at the end of the block that opens on line %d. "+
			"Line %d is where the missing brace was noticed, not where it goes — "+
			"re-check the block you just rewrote, not that line.\n", f.OpenedLine, f.Line)
	} else {
		if f.Text != "" {
			fmt.Fprintf(&sb, "  %d | %s\n", f.Line, f.Text)
		}
		if f.Hint != "" {
			fmt.Fprintf(&sb, "%s\n", f.Hint)
		}
	}
	fmt.Fprintf(&sb,
		"Running the file will NOT surface this: the %s syntax is valid, the server "+
			"still starts and the page still returns 200 — but %s. Fix line %d inside "+
			"the %s block and re-send the corrected content; do NOT resend it unchanged.",
		host, breakage, f.Line, block)
	return sb.String()
}

// redeclarationRejection is the rejection for a script that declares the same
// name twice: it parses, and the browser still refuses all of it.
func redeclarationRejection(path string, f embeddedScriptFinding) string {
	var sb strings.Builder
	// Parses, but never runs: a repeated let/const is an early
	// SyntaxError, so the engine throws out the whole script before
	// executing a line of it. Every handler on the page is dead, which
	// is worse than the stray-paren case and looks identical from the
	// server side.
	fmt.Fprintf(&sb, "Your content for %s declares the same name twice in %s — it was NOT written, and %s on disk is unchanged.\n", path, f.Where, path)
	fmt.Fprintf(&sb, "line %d: %s\n", f.Line, f.Message)
	if f.Text != "" {
		fmt.Fprintf(&sb, "  %d | %s\n", f.Line, f.Text)
	}
	if f.Hint != "" {
		fmt.Fprintf(&sb, "%s\n", f.Hint)
	}
	sb.WriteString("Running the file will NOT surface this: the Python compiles, the server " +
		"still starts and the page still returns 200 — the browser refuses the script " +
		"outright, so nothing on the page responds. Re-send the edit without the second " +
		"declaration.")
	return sb.String()
}

// stoppedLoopRejection is the rejection for an edit that leaves a render loop
// scheduled once and never again, so the page freezes after one frame.
func stoppedLoopRejection(path string, f embeddedScriptFinding) string {
	var sb strings.Builder
	// Not a syntax error: the JavaScript parses. The edit left a render
	// loop scheduled exactly once, so the page draws one frame and
	// freezes. Nothing downstream can see it — the file compiles, the
	// server starts, the page returns 200.
	fmt.Fprintf(&sb, "Your content for %s stops a render loop in %s — it was NOT written, and %s on disk is unchanged.\n", path, f.Where, path)
	fmt.Fprintf(&sb, "line %d: %s\n", f.Line, f.Message)
	if f.Text != "" {
		fmt.Fprintf(&sb, "  %d | %s\n", f.Line, f.Text)
	}
	if f.Hint != "" {
		fmt.Fprintf(&sb, "%s\n", f.Hint)
	}
	sb.WriteString("Running the file will NOT surface this: the JavaScript is valid, " +
		"the server still starts and the page still returns 200 — it just freezes " +
		"after one frame. Re-send the edit with the loop rescheduling itself.")
	return sb.String()
}

// reSyntaxLineNo pulls a 1-based line number out of a Python syntax error
// message ("... (file, line 13)" or "at line 13"), when present.
var reSyntaxLineNo = regexp.MustCompile(`line (\d+)`)

// fusedLineRe finds a literal backslash-n inside a comment with code-shaped
// text after it: `# ...it's a transition\n        if current_run[0][1] != ...`.
// One mis-escaped newline in the model's JSON traps the next statement inside
// the comment. The file often still parses (comments swallow anything), or
// dies later with an IndentationError whose reported line is a downstream
// casualty — measured: a rejection quoted "line 45", which was blank, while
// the fused comment sat at line 40; the model re-sent the identical content
// until the repetition breaker ended the session. The pattern is the
// two-character sequence backslash-n, not a newline.
var fusedLineRe = regexp.MustCompile(`#[^\n]*?\\n\s*(?:if |for |while |return |def |class |[A-Za-z_][A-Za-z0-9_]*\s*[=(+])`)

// fusedLineHint names the first comment-trapped statement, or "".
func fusedLineHint(content string) string {
	for i, line := range strings.Split(content, "\n") {
		if fusedLineRe.MatchString(line) {
			return fmt.Sprintf(
				" Before anything else, look at line %d: its comment contains a "+
					"literal \\n followed by code, so the statement after it is "+
					"trapped INSIDE the comment. You escaped a newline that should "+
					"be real — split that into two lines.", i+1)
		}
	}
	return ""
}

// locateSyntaxLine resolves the 1-based line a syntax error points at, and
// renders that line for quoting back. Returns (0, "") when the error carries
// no location or the location is outside the content.
//
// The model cannot fix what it cannot see. Handing back its own broken line
// is worth more than any guess at the cause: a session was told the likely
// problem was nested quotes in an f-string when it had actually dropped
// `self` from a method signature, and it re-sent the same signature four
// times.
func locateSyntaxLine(content, syntaxErr string) (int, string) {
	m := reSyntaxLineNo.FindStringSubmatch(syntaxErr)
	if m == nil {
		return 0, ""
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 {
		return 0, ""
	}
	lines := strings.Split(content, "\n")
	if n > len(lines) {
		return 0, ""
	}
	return n, fmt.Sprintf(" The offending line %d is:\n%s\n",
		n, strings.TrimRight(lines[n-1], " \t"))
}

// lastContentLine is the 1-based index of the last non-blank line, which is
// where a genuinely truncated write stops. Trailing newlines would otherwise
// make every file look like it ends several lines after its real content.
func lastContentLine(content string) int {
	lines := strings.Split(content, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return i + 1
		}
	}
	return 0
}

// fallbackSyntaxRejection builds the tool error handed back to the model
// when the gate blocks a write. It DISTINGUISHES the two failure shapes,
// because the old one-size message ("truncated — resend complete content")
// is actively wrong for a genuine syntax bug in COMPLETE content and made
// the model reassert the same broken text (observed 2026-07-20 on a
// pytorch-model-recovery task: an f-string with nested quotes resent 5×):
//   - truncation shape (unterminated string / unexpected EOF / "never
//     closed") → the content really is cut off; resend it complete.
//   - a mid-content syntax bug → point at the offending line (quoted from
//     `content` when the error carries a line number) and tell the model to
//     FIX that line, explicitly forbidding an identical resend.
func fallbackSyntaxRejection(path, content, syntaxErr string) string {
	// An embedded-script finding arrives pre-formatted (it already names the
	// line and the fix); the truncation/syntax-bug fork below is about the
	// host file and would give wrong advice for JavaScript inside a string.
	if msg, isEmbedded := embeddedScriptRejectionFor(syntaxErr); isEmbedded {
		return msg
	}
	low := strings.ToLower(syntaxErr)
	truncationShape := strings.Contains(low, "unexpected eof") ||
		strings.Contains(low, "was never closed") ||
		strings.Contains(low, "unterminated") ||
		strings.Contains(low, "expected an indented block")

	// Locate the offending line before choosing the advice: it decides which
	// advice is even true, and it is the one concrete thing to hand back
	// either way. The syntax check now carries "(line N)" for Python, so this
	// resolves where it used to come up empty.
	lineNo, quoted := locateSyntaxLine(content, syntaxErr)
	quoted += fusedLineHint(content)

	// These error shapes are ambiguous. An unclosed brace on line 44 of a
	// 60-line file is a real bug; the same message on the LAST line means the
	// content really was cut off. Telling the model it was truncated when it
	// was not sends it to resend the identical file, which is precisely what
	// it did: three write_file calls, then the same call byte for byte until
	// the repetition breaker ended the session. Only claim truncation when
	// the failure is at the end, where truncation would put it.
	if truncationShape && (lineNo == 0 || lineNo >= lastContentLine(content)-1) {
		return fmt.Sprintf(
			"Your content for %s does not parse (%s) — this looks like a "+
				"truncated tool call (content cut off mid-way).%s Retry write_file "+
				"with the COMPLETE file content; if it is long, write it in full, "+
				"not in fragments.", path, truncateStr(syntaxErr, 200), quoted)
	}
	// An f-string error names a real problem inside the braces. The sandbox
	// that checks the file runs Python 3.13, where nesting the same quote
	// (`f"{d["k"]}"`, PEP 701) is valid, so the old advice -- "this
	// environment runs an older Python" -- was false, and it sent the model
	// after quotes when the error was a bracket. Point at the braces instead,
	// on the quoted line. Observed live: a session hit the wall clock
	// re-sending the same content because the advice sat in a parenthetical
	// after two other sentences, so it still leads.
	// "unexpected character after line continuation character" is Python
	// telling you a backslash is followed by something other than a newline.
	// The wording names the mechanism, not the mistake, and a model that has
	// started emitting stray backslashes cannot act on it — observed three
	// times in one session, all on the same file, until the wall clock ran
	// out. Name the character and where it is.
	lowerErr := strings.ToLower(syntaxErr)
	if strings.Contains(lowerErr, "line continuation character") {
		return fmt.Sprintf(
			"Your content for %s has a stray backslash (%s) — it was NOT "+
				"written.%s A backslash only means line-continuation when it is "+
				"the LAST character on its line; anywhere else Python rejects "+
				"the file. Remove it, or write \\\\ if you meant a literal "+
				"backslash. Do NOT resend the same content unchanged.",
			path, truncateStr(syntaxErr, 200), quoted)
	}
	if strings.Contains(lowerErr, "f-string") {
		return fmt.Sprintf(
			"Your content for %s has an f-string error (%s) — it was NOT "+
				"written.%s Look inside the braces on that line: a bracket or "+
				"brace that is never closed, an empty {}, or a conversion other "+
				"than !r, !s or !a. If the expression is long, pull it into a "+
				"variable first. Do NOT resend the same content unchanged.",
			path, truncateStr(syntaxErr, 200), quoted)
	}
	return fmt.Sprintf(
		"Your content for %s has a syntax error (%s) — it was NOT written. The "+
			"content is NOT truncated; it is complete but INVALID.%s Fix THAT "+
			"specific error on the line quoted. Do NOT resend the same content "+
			"unchanged; it will fail identically.", path, truncateStr(syntaxErr, 200), quoted)
}

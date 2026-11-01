// Edit requests: checks on what the model sent to an edit or write tool,
// made before the tool changes the file, and the texts that say why a
// request was refused. Among them: a write that only repeats the file on
// disk, an old_str that still carries read_file's line numbers, a
// structural_edit selector or content that names no single node, a splice
// that adds a second `if __name__ == "__main__":`, an insert_after that moves
// existing Python code into another block, and an edit made from a view of
// the file that is no longer current.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// isUnreadOverwrite reports whether a write_file would replace an existing
// file this session has never read and did not itself create.
//
// Separate from the ">5 lines" rule, which asks whether a surgical edit is
// cheaper than a rewrite. This asks whether the file should be replaced at
// all, and the answer is no when nobody has looked at it: edit_file and
// structural_edit already require a read first, and this was the one write
// path that did not.
func isUnreadOverwrite(ctx *AgentContext, resolvedPath string, corrupted, sessionOwned bool) bool {
	if ctx == nil || corrupted || sessionOwned {
		return false
	}
	return !ctx.WasFileRead(resolvedPath)
}

// strayCarriageReturns counts CRs that are not part of a CRLF pair.
//
// A CRLF file is normal and must not be flagged, but bare or repeated CRs in
// an old_str are a reliable signature of a model that degenerated partway
// through copying a block — observed across three sessions, one of which
// emitted a literal `\rVert` (a LaTeX fragment) in the middle of JavaScript.
func strayCarriageReturns(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '\r' {
			continue
		}
		if i+1 < len(s) && s[i+1] == '\n' {
			i++ // a well-formed CRLF, skip its LF
			continue
		}
		n++
	}
	return n
}

// reLineNumberPrefix matches read_file's display prefix: "N<tab>".
var reLineNumberPrefix = regexp.MustCompile(`(?m)^[ \t]*\d+\t`)

// lineNumberPrefixedLines counts lines in s that still carry read_file's
// "N<tab>" prefix.
//
// read_file already says the numbers are presentation and not in the file, but
// that notice is at read time and the mistake happens at edit time, often many
// turns later. Observed live: a model read a file, then sent
// old_str="76\t const ctx = canvas.getContext('2d');" — the real line with the
// prefix still attached. Nothing matched, and the generic "not found" plus a
// closest-line hint showed it the correct text without ever naming what it had
// actually done wrong, so its next attempt kept the prefix and corrupted the
// rest of the line.
func lineNumberPrefixedLines(s string) int {
	return len(reLineNumberPrefix.FindAllString(s, -1))
}

// stripLineNumberPrefixes removes read_file's "N<tab>" display prefix.
//
// The prefix is this harness's own addition, so taking it back off is a
// mechanical undo rather than a guess about intent. Callers accept the result
// only when it then matches the file, so a strip that was not wanted cannot
// change an edit.
func stripLineNumberPrefixes(s string) string {
	return reLineNumberPrefix.ReplaceAllString(s, "")
}

// selectorGuidance says which structural_edit selectors exist in a file, read
// from the file itself, or how to find out. It never offers a placeholder: a
// refusal that suggested "a selector (`function:NAME` or `class:NAME`)" was
// answered with the literal selector `function:NAME` and a correct whole-file
// fix as content, which was then refused too (stabilization cycle 1, W1
// aoc_slope rep1). Returns "" for a language structural_edit does not handle.
func selectorGuidance(path, source string) string {
	if !structuralLanguage(path) {
		return ""
	}
	name := filepath.Base(path)
	sels := selectorsInFile(path, source)
	if len(sels) == 0 {
		return fmt.Sprintf("%s has no top-level function, class or element a selector can name; "+
			"run outline_file on %s to see its structure", name, name)
	}
	more := ""
	if len(sels) > 8 {
		more = fmt.Sprintf(" and %d more (outline_file lists them)", len(sels)-8)
		sels = sels[:8]
	}
	return fmt.Sprintf("selectors that exist in %s: `%s`%s", name, strings.Join(sels, "`, `"), more)
}

func structuralLanguage(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".py", ".html", ".htm", ".go", ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs":
		return true
	}
	return false
}

// selectorsInFile lists selectors that name exactly one top-level node in the
// source: column-0 definitions for code, and elements that occur once for
// HTML. A name defined twice is left out, since its selector would be
// ambiguous.
func selectorsInFile(path, source string) []string {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".html" || ext == ".htm" {
		return htmlSelectors(source)
	}
	count := map[string]int{}
	var order []string
	for _, sym := range outlineByRegex(path, source) {
		prefix := sym.Kind
		if prefix == "type" && ext != ".go" {
			continue
		}
		sel := prefix + ":" + sym.Name
		if count[sel] == 0 {
			order = append(order, sel)
		}
		count[sel]++
	}
	var out []string
	for _, sel := range order {
		if count[sel] == 1 {
			out = append(out, sel)
		}
	}
	return out
}

// htmlSelectors lists the element selectors of an HTML file: each of the page's
// main elements that occurs exactly once.
func htmlSelectors(source string) []string {
	var out []string
	low := strings.ToLower(source)
	for _, tag := range []string{"html", "head", "body", "main", "header", "nav", "footer", "script", "style"} {
		n := 0
		for _, sep := range []string{"<" + tag + ">", "<" + tag + " ", "<" + tag + "\n"} {
			n += strings.Count(low, sep)
		}
		if n == 1 {
			out = append(out, "<"+tag+">")
		}
	}
	return out
}

// moduleLevelContentNote explains a structural_edit refusal whose content is
// not one node at all: Python content with module-level code (an import, a
// `if __name__` block, an assignment at column 0) reads as a whole file, and
// structural_edit replaces a single function or class. The note names the
// line that shows it and the tool that replaces a whole file, or edits outside
// a node. Python only, where column-0 code outside a def/class is unambiguous.
func moduleLevelContentNote(ctx *AgentContext, path, content string) string {
	if strings.ToLower(filepath.Ext(path)) != ".py" {
		return ""
	}
	for i, line := range strings.Split(content, "\n") {
		t := strings.TrimRight(line, " \t\r")
		if t == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		if strings.HasPrefix(t, "#") || strings.HasPrefix(t, "@") || strings.HasPrefix(t, "def ") ||
			strings.HasPrefix(t, "async def ") || strings.HasPrefix(t, "class ") ||
			t == ")" || t == "]" || t == "}" || strings.HasPrefix(t, strings.Repeat("\"", 3)) || strings.HasPrefix(t, strings.Repeat("'", 3)) {
			continue
		}
		name := filepath.Base(path)
		how := "use replace_lines with the line numbers read_file shows, or edit_file anchored on one unique line"
		if ctx != nil && ctx.SessionWrites[path] {
			how = fmt.Sprintf("write_file with the complete file is the tool that replaces all of %s", name)
		}
		return fmt.Sprintf(" Your content is not a single function or class: line %d (`%s`) is module-level code, "+
			"so it reads as a whole file, and structural_edit replaces one named node. To change the whole file, %s.",
			i+1, truncateStr(t, 60), how)
	}
	return ""
}

// reMainGuard matches a top-level `if __name__ == "__main__":` (either quote
// style, any spacing). Anchored at column 0 so a nested one inside a function
// is not counted.
var reMainGuard = regexp.MustCompile(`(?m)^if\s+__name__\s*==\s*['"]__main__['"]\s*:`)

// duplicateMainGuard reports whether an edit left a .py file with more than
// one module entrypoint when it had at most one before.
//
// Observed live: structural_edit on a 3-line `index()` was handed content
// that carried an `if __name__ == "__main__": app.run(...)` block along with
// it, so the splice appended a second one and the file went from 209 lines to
// 388. Nothing caught it — the file still parses and still runs, because the
// first app.run() blocks and the second is simply dead code sitting under it.
// It is the signature of a whole-file blob smuggled through a node selector.
//
// Healthy->broken like the other write gates: a file that already had two is
// left alone. Returns "" for anything that is not Python.
func duplicateMainGuard(path, original, edited string) string {
	if strings.ToLower(filepath.Ext(path)) != ".py" {
		return ""
	}
	after := len(reMainGuard.FindAllString(edited, -1))
	if after < 2 || after <= len(reMainGuard.FindAllString(original, -1)) {
		return ""
	}
	return fmt.Sprintf(
		"%s would end up with %d `if __name__ == \"__main__\":` blocks — it was NOT written.\n"+
			"Your replacement carried the module's entrypoint along with it, so splicing it in "+
			"added a second copy. Only the first one ever runs; the rest is dead code.\n"+
			"Send ONLY the node you are replacing — the function or class body itself, with no "+
			"surrounding module-level code. If you meant to change the entrypoint, edit that "+
			"block directly with replace_lines or edit_file.",
		path, after)
}

// echoesExistingFile reports a write whose content is the file that is
// already on disk, or a truncated prefix of it.
//
// Reproducing data the session already has is never the task, and it is how
// the worst failure in the measured runs starts: asked to solve an Advent of
// Code puzzle, the model called write_file on input.txt — the fixture — and
// tried to retype 2000 lines of numbers from memory. It degenerated into
// repeating one line ~50 times, the content-loop detector cut the stream
// mid-JSON at 601 chars, the parse failed, and three identical retries ended
// the run. A sibling session got further and corrupted the fixture outright.
//
// Refusing the echo removes the whole chain, and it is cheap to recognise:
// the model has no reason to send back bytes it just read.
//
// Prefix rather than equality because the collapse truncates: by the time it
// is cut, the content is a partial copy. A short prefix proves nothing, so
// there is a floor.
func echoesExistingFile(existing, incoming string) bool {
	const minEcho = 200
	if len(existing) < minEcho || len(incoming) < minEcho {
		return false
	}
	a, b := strings.TrimSpace(existing), strings.TrimSpace(incoming)
	if a == b {
		return true
	}
	// A truncated retype: everything that arrived matches the head of the
	// file, and it is a real portion of it rather than a couple of lines.
	if len(b) < len(a) && strings.HasPrefix(a, b) {
		return true
	}
	return false
}

// resendsOwnWrite reports a write whose content is already what the file holds,
// at any size. It is asked only about a file this session wrote: re-sending
// those bytes changes nothing, and the next step is to use them.
//
// echoesExistingFile keeps its 200-byte floor, which exists for the prefix
// case (a short prefix proves nothing) and for files the session did not
// write. For the session's own file the floor let small test files through.
// In the ccc71fc smoke runs (2026-09-29), offbyone and add_function wrote a
// 76-byte test_chunk.py (and a small test_stats.py), re-sent it five or six
// times without running it, and passing work ended stopped/repeat_detector
// in 4 of 84 sessions.
func resendsOwnWrite(existing, incoming string) bool {
	a, b := strings.TrimSpace(existing), strings.TrimSpace(incoming)
	return b != "" && a == b
}

// echoedWriteRejection tells the model why copying a file back is refused.
//
// For a file this session wrote, the reason is different: the model already
// wrote exactly these bytes, and the next step is to use them. Smoke run
// 2026-09-28 (add_function rep 2): the test file landed, the model re-sent the
// same write three times and was told each time that it did not need to
// reproduce input data; the test never ran, and passing work ended "stopped".
func echoedWriteRejection(path string, sessionWrote bool) string {
	if sessionWrote {
		return fmt.Sprintf(
			"write_file on %s would not change it: %s already holds exactly this content. "+
				"You wrote it earlier in this session, and it is on disk. There is nothing to "+
				"write. The next step is to use it: run it (run_command), or run the tests that "+
				"exercise it, and read the output.",
			path, path)
	}
	return fmt.Sprintf(
		"write_file on %s would rewrite it with the contents it already has. You do not "+
			"need to reproduce a file to work with it — read_file has already shown it to "+
			"you, and code you write can read it at runtime.\n\n"+
			"If this is input or fixture data, leave it alone and write the program that "+
			"processes it. If you meant to change part of it, use replace_lines or "+
			"edit_file on the lines that differ. Retyping a large file from memory does "+
			"not work: the response degenerates into repetition and gets cut off.",
		path)
}

// editViewIsCurrent reports whether the model's view of `path` is current
// enough to edit it without a fresh read_file.
//
// A read makes the view current. So does a write THIS SESSION made -- but only
// while the bytes on disk are still the bytes we wrote. Session ownership on
// its own is deliberately NOT sufficient: a shell command this run issued, or
// anything else touching the workspace, can change the file underneath, and
// then the model's belief is stale exactly as if it had never read it. The test
// is identity, not ownership, and it is the same one the restoration path
// already applies (Generation > 0 && CurrentHash == diskHash), so the two
// cannot disagree about what "we still know this file" means.
//
// Measured on the 38eaa0a benchmark (aoc_slope): the session wrote solve.py,
// the syntax gate warned that it did not parse and quoted the offending line,
// and the model's correct one-line edit_file repair was refused with "file not
// read yet". Having no accepted way to change one line, it re-sent the whole
// file, tripped the identical-call detector, and the run ended without ever
// repairing the line it had been told about. A read would have been a pure
// round trip: the content it would return is the content this session just
// wrote.
func editViewIsCurrent(ctx *AgentContext, path string) bool {
	if ctx == nil {
		return false
	}
	if ctx.WasFileRead(path) {
		return true
	}
	data, err := readRegularFile(path)
	if err != nil {
		// Unreadable or absent: nothing is known about it.
		return false
	}
	return diskIsOwnWrite(ctx, path, data)
}

// diskIsOwnWrite reports whether data -- the bytes now on disk at path -- are
// exactly the bytes this session last wrote there. Identity, not ownership:
// Generation > 0 && CurrentHash == hash(disk), the same test the restoration
// path applies.
func diskIsOwnWrite(ctx *AgentContext, path string, data []byte) bool {
	if ctx == nil {
		return false
	}
	disk := hashBytes(data)
	key := ledgerKey(ctx, path)
	ctx.LedgerMu.Lock()
	d := ctx.Ledger[key]
	own := d != nil && !d.Tombstoned && d.Generation > 0 && d.CurrentHash == disk
	ctx.LedgerMu.Unlock()
	return own
}

// modifiedSinceSessionView reports whether path changed after the session last
// established what is in it -- by reading it, or by writing it. data is the
// content the caller just read from disk.
//
// This was an mtime-versus-read-time comparison inline in edit_file and
// structural_edit, and write_file is the one mutating tool that never records a
// read time. So after ANY write_file, the file's mtime is later than a read
// time that is either older or absent (zero), and a follow-up edit was refused
// "file modified since last read" although nothing but the session had touched
// it. editViewIsCurrent had already been taught identity for exactly this
// scenario; the check immediately after it had not, so the fix moved the
// refusal to a different message instead of removing it -- its test exercised
// the helper, never the tool.
//
// Measured across the saved benchmark and acceptance events: 9 refusals in 5
// sessions, and in all 9 the last thing to touch the path was the session's own
// write_file (4 directly, 5 with only a run_command such as `python3 solve.py`
// between). They include the four refusals in the multifile_cli-rep2 deadlock
// and aoc_sonar-rep1, where the model ran its broken solve.py as instructed,
// read the real traceback, and had its one-line repair refused.
//
// A genuine change still refuses: if a command or anything else altered the
// bytes, they no longer equal the session's own write, and the mtime rule
// applies exactly as before.
func modifiedSinceSessionView(ctx *AgentContext, path string, data []byte) bool {
	ctx.mu.Lock()
	lastRead := ctx.FileReadTimes[path]
	ctx.mu.Unlock()
	info, err := os.Stat(path)
	if err != nil || !info.ModTime().After(lastRead) {
		return false
	}
	return !diskIsOwnWrite(ctx, path, data)
}

// insertionReparentsPython reports an insert_after that would move EXISTING
// Python statements into a different enclosing block, and describes the first
// one moved. insert_after only adds lines; it never re-indents the lines after
// the insertion point. So if an existing statement's enclosing block changes,
// the insertion has restructured code the model did not ask to touch, and the
// file can still parse -- which is why no syntax gate sees it.
//
// Measured on 84296fd smallrung_toml rep2: `elif lang == "toml":` inserted
// after line 1359 of executor_server.py, between `stderr = result.get(...)`
// and `for line in stderr.splitlines():`. The file parsed. The java branch's
// loop that turns compiler output into errors now belonged to the new toml
// branch's `except`, so java compile errors were no longer reported, and the
// toml branch raised on invalid TOML. The task scored PASS under a static
// check and was later corrected to fail.
//
// This is a structural invariant for an insertion, not a claim about
// behaviour: an insertion that leaves every existing statement in its block
// can still be wrong. Indentation is read from logical line starts, skipping
// continuation lines, triple-quoted strings and bracketed spans. If the
// original file cannot be scanned consistently the check is skipped rather
// than risk refusing a sound edit.
func insertionReparentsPython(path string, lines []string, at int, insert []string) (string, bool) {
	if strings.ToLower(filepath.Ext(path)) != ".py" || len(insert) == 0 || at < 0 || at > len(lines) {
		return "", false
	}
	orig, ok := pythonBlockParents(lines)
	if !ok {
		return "", false
	}
	merged := make([]string, 0, len(lines)+len(insert))
	merged = append(merged, lines[:at]...)
	merged = append(merged, insert...)
	merged = append(merged, lines[at:]...)
	upd, ok := pythonBlockParents(merged)
	if !ok {
		return "", false
	}
	toOrig := func(u int) int {
		switch {
		case u < 0:
			return u
		case u < at:
			return u
		case u < at+len(insert):
			return inserted
		default:
			return u - len(insert)
		}
	}
	for j := at; j < len(lines); j++ {
		u := j + len(insert)
		if (orig[j] == notLogicalStart) != (upd[u] == notLogicalStart) {
			return fmt.Sprintf(
				"insert_after at line %d would change how existing line %d (`%s`) is read: the inserted text leaves "+
					"a string or bracket open, so that line would no longer be its own statement. Insert complete "+
					"statements only.", at, j+1, strings.TrimSpace(lines[j])), true
		}
		if orig[j] == notLogicalStart {
			continue
		}
		if was, now := orig[j], toOrig(upd[u]); was != now {
			from, to := reparentEnds(lines, merged, was, now, upd[u])
			return fmt.Sprintf(
				"insert_after at line %d would move existing code into a different block: line %d `%s` belongs to %s "+
					"and would now belong to %s. The file would still parse, but that existing code would silently "+
					"change behaviour. Nothing was written. Pick an insertion point where the next existing line is "+
					"not indented deeper than the code you insert -- usually after the LAST line of the block you are "+
					"adding next to, not in the middle of it.",
				at, j+1, strings.TrimSpace(lines[j]), from, to), true
		}
	}
	return "", false
}

// reparentEnds names the two blocks an existing line would move between: the
// one it belongs to (was) and the one it would belong to (now). Each is named
// by the line that opens it, or is the top level of the file. mergedParent is
// now's index in the file with the insertion made.
func reparentEnds(lines, merged []string, was, now, mergedParent int) (string, string) {
	from := "the top level of the file"
	if was >= 0 {
		from = fmt.Sprintf("`%s` (line %d)", strings.TrimSpace(lines[was]), was+1)
	}
	to := "the top level of the file"
	if now == inserted {
		to = fmt.Sprintf("your inserted `%s`", strings.TrimSpace(merged[mergedParent]))
	} else if now >= 0 {
		to = fmt.Sprintf("`%s` (line %d)", strings.TrimSpace(lines[now]), now+1)
	}
	return from, to
}

const notLogicalStart = -2

// inserted marks, in insertionReparentsPython, an enclosing line that is one
// of the inserted lines rather than a line of the file.
const inserted = -3

// pythonBlockParents returns, for each line, the index of the logical line
// that encloses it by indentation (-1 for module level), or notLogicalStart
// for blank lines, comment-only lines and lines that continue a previous
// statement. ok is false when the scan ends inside a string or bracket, i.e.
// the scanner cannot vouch for the structure it found.
func pythonBlockParents(lines []string) ([]int, bool) {
	parents := make([]int, len(lines))
	var stack []pythonFrame
	var triple string // `"""` or `'''` while inside a triple-quoted string
	depth := 0
	continued := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		start := triple == "" && depth == 0 && !continued && trimmed != "" && !strings.HasPrefix(trimmed, "#")
		if start {
			indent := pythonIndent(line)
			parents[i], stack = openPythonBlock(stack, indent, i)
		} else {
			parents[i] = notLogicalStart
		}
		// Advance string/bracket state across this line.
		continued = false
		triple, depth = advancePythonLine(line, triple, depth)
		if triple == "" && depth == 0 && strings.HasSuffix(strings.TrimRight(stripPythonComment(line), " \t"), "\\") {
			continued = true
		}
	}
	return parents, triple == "" && depth == 0 && !continued
}

// pythonFrame is a block that is open while pythonBlockParents scans: the
// indentation of the logical line that opened it, and that line's index.
type pythonFrame struct{ indent, index int }

// openPythonBlock closes the open blocks that a logical line at indent ends
// and opens the line's own. It returns the index of the line that encloses
// line i (-1 at module level) and the stack with line i's block on top.
func openPythonBlock(stack []pythonFrame, indent, i int) (int, []pythonFrame) {
	var parent int
	for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
		stack = stack[:len(stack)-1]
	}
	if len(stack) == 0 {
		parent = -1
	} else {
		parent = stack[len(stack)-1].index
	}
	stack = append(stack, pythonFrame{indent, i})
	return parent, stack
}

// advancePythonLine carries the scan's string and bracket state across one
// line: the triple quote still open at its end ("" when none), and the
// depth of open brackets.
func advancePythonLine(line, triple string, depth int) (string, int) {
	for k := 0; k < len(line); k++ {
		c := line[k]
		if triple != "" {
			if strings.HasPrefix(line[k:], triple) {
				k += 2
				triple = ""
			} else if c == '\\' {
				k++
			}
			continue
		}
		switch c {
		case '#':
			k = len(line)
		case '"', '\'':
			q := string(c)
			if strings.HasPrefix(line[k:], q+q+q) {
				triple = q + q + q
				k += 2
				continue
			}
			k = skipPythonString(line, k, c)
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		}
	}
	return triple, depth
}

// skipPythonString moves k from the opening quote c of a single-line string
// to its closing quote, or past the end of the line when there is none.
func skipPythonString(line string, k int, c byte) int {
	// single-line string: skip to its closing quote
	for k++; k < len(line); k++ {
		if line[k] == '\\' {
			k++
			continue
		}
		if line[k] == c {
			break
		}
	}
	return k
}

func pythonIndent(line string) int {
	n := 0
	for _, r := range line {
		switch r {
		case ' ':
			n++
		case '\t':
			n += 8 - n%8
		default:
			return n
		}
	}
	return n
}

// stripPythonComment removes a trailing comment that is outside any string on
// a single line; used only to see whether a line ends in a continuation.
func stripPythonComment(line string) string {
	var q byte
	for k := 0; k < len(line); k++ {
		c := line[k]
		if q != 0 {
			if c == '\\' {
				k++
			} else if c == q {
				q = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			q = c
		} else if c == '#' {
			return line[:k]
		}
	}
	return line
}

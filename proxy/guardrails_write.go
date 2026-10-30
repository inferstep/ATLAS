// Guardrails on the content a write or an edit sends: the whole-file fence
// wrapper that is removed, the shapes the JSON channel refuses before
// execution (a line break that lost its backslash, doubled escapes, a fence
// with text around it), the suspiciously small replacement, the duplicated
// doctype, the new file that ignores its siblings, the file left corrupted on
// disk, and the literal-content contracts a request carries. The
// placeholder-stub check, looksLikeStub, is in guardrails.go.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// sanitizeFileContent is the one content change the write path makes. The
// local model sometimes sends a file wrapped in a markdown fence:
//
//	```html
//	<!DOCTYPE html>
//	...
//	```
//
// Written verbatim, the fence lines are a syntax error in every language.
// The function returns (cleaned, modified); callers that write report a
// modification to the model (wholeFileWrapperNote) rather than doing it
// silently. Nested exact wrappers are removed to a fixpoint, so the result
// never depends on how many times content passes through (V3 re-sanitizes).
func sanitizeFileContent(filePath, content string) (string, bool) {
	modified := false
	for {
		stripped, ok := stripWholeFileWrapper(filePath, content)
		if !ok {
			return content, modified
		}
		content, modified = stripped, true
	}
}

// stripWholeFileWrapper removes a markdown fence that wraps the ENTIRE content
// and nothing else: the first non-blank line opens a fence, the last non-blank
// line closes it with at least as many backticks, and no line in between would
// close it. Only then are the bytes unambiguous -- the lines inside are the
// file -- and only then is anything removed. Documents (.md, .rst, .txt) are
// never touched, since fences are their content.
//
// This used to search for a fence behind up to five lines of prose, strip an
// opener that never closed, and then "repair" the result: decode a one-line
// body with literal \n escapes, and turn a form feed or backspace before a
// letter into a line break. Each of those rewrote valid content. Measured on
// c5927b3 through the real write path: a YAML file whose block scalar held a
// ```bash example was reduced to "  ls -la\n"; a Python page break (form feed
// before `def`) became "\nfdef"; one-line minified JSON had its \n escapes
// decoded. Content that is ambiguous is now left to the syntax gate or, when
// it arrives through the JSON channel in a shape that is usually an escaping
// slip, refused before execution (jsonChannelContentFeedback).
func stripWholeFileWrapper(filePath, content string) (string, bool) {
	if isDocumentAsset(filePath) {
		return content, false
	}
	lines := strings.Split(content, "\n")
	first, last := nonBlankBounds(lines)
	if first < 0 || first == last {
		return content, false
	}
	open := fenceOpenRe.FindStringSubmatch(lines[first])
	closing := fenceCloseRe.FindStringSubmatch(lines[last])
	if open == nil || closing == nil || len(closing[1]) < len(open[1]) {
		return content, false
	}
	for i := first + 1; i < last; i++ {
		if m := fenceCloseRe.FindStringSubmatch(lines[i]); m != nil && len(m[1]) >= len(open[1]) {
			return content, false
		}
	}
	inner := strings.Join(lines[first+1:last], "\n")
	if strings.TrimSpace(inner) == "" {
		return content, false
	}
	return inner + "\n", true
}

// nonBlankBounds returns the indexes of the first and the last line that is
// not blank, or -1, -1 when every line is blank.
func nonBlankBounds(lines []string) (int, int) {
	first, last := -1, -1
	for i, l := range lines {
		if strings.TrimSpace(l) != "" {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	return first, last
}

// wholeFileWrapperNote is what a write result says when a wrapper was removed.
const wholeFileWrapperNote = "The content you sent was wrapped in a single ``` fenced block; " +
	"only the lines inside the fence were written."

// isDocumentAsset reports whether a path is prose rather than code.
//
// This is the set stripOneFenceLayer has always used to decide that a file's
// fences are its CONTENT, not a wrapper around it -- a markdown document full
// of code blocks is still a document. Naming it lets the completion decision
// ask the same question, and the answer has to be an allowlist: an unknown
// extension holding source-like logic, or an unsupported language like .rs,
// must fall outside it, because "no checker ran" means something entirely
// different for those than it does for a text file.
func isDocumentAsset(filePath string) bool {
	switch strings.ToLower(filepath.Ext(filePath)) {
	case ".md", ".markdown", ".rst", ".txt":
		return true
	}
	return false
}

// stripOneFenceLayer removes a single whole-file markdown wrapper. Callers
// want sanitizeFileContent, which drives this to a fixpoint.
func stripOneFenceLayer(filePath, content string) (string, bool) {
	if isDocumentAsset(filePath) {
		return content, false
	}

	lines := strings.Split(content, "\n")

	// Locate the opening fence within the preamble allowance. More than
	// a few non-empty lines before the first fence — or any line that
	// opens a docstring/comment block — means the fence is interior
	// content, not a wrapper.
	openIdx := openingFenceIndex(lines)
	if openIdx < 0 {
		return content, false
	}

	closeIdx := -1
	for i := len(lines) - 1; i > openIdx; i-- {
		if strings.TrimSpace(lines[i]) == "```" {
			closeIdx = i
			break
		}
	}

	// Same whole-file requirement on the way out: after the closing
	// fence only a short prose trailer ("This file: 1. ... 2. ...") is
	// allowed. Substantial content or docstring/comment markers after
	// the fence mean the pair is interior — pass through unchanged.
	if trailerMakesFenceInterior(lines, openIdx, closeIdx) {
		return content, false
	}

	var extracted []string
	if closeIdx > openIdx {
		extracted = lines[openIdx+1 : closeIdx]
	} else {
		// Unmatched closing fence — model probably truncated. Take
		// everything after the opener; better than discarding the
		// whole file or keeping the prose preamble.
		extracted = lines[openIdx+1:]
	}

	cleaned := strings.Join(extracted, "\n")
	// Preserve a single trailing newline if the original had one — POSIX
	// text files conventionally end with \n.
	if strings.HasSuffix(content, "\n") && !strings.HasSuffix(cleaned, "\n") {
		cleaned += "\n"
	}
	return cleaned, true
}

// openingFenceIndex is the line of the fence that may open a whole-file
// wrapper, or -1 when there is none: no fence at all, or more prose before it
// than a wrapper's preamble allows, or a docstring or comment block there.
func openingFenceIndex(lines []string) int {
	const maxWrapperProseLines = 5
	openIdx := -1
	preambleProse := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			openIdx = i
			break
		}
		if trimmed == "" {
			continue
		}
		preambleProse++
		if preambleProse > maxWrapperProseLines || lineSignalsRealContent(trimmed) {
			return -1
		}
	}
	return openIdx
}

// trailerMakesFenceInterior reports whether what follows the closing fence is
// more than a short prose trailer, so the fence pair is interior content and
// not a wrapper.
func trailerMakesFenceInterior(lines []string, openIdx, closeIdx int) bool {
	if closeIdx > openIdx {
		const maxTrailerProseLines = 8
		trailerProse := 0
		for _, line := range lines[closeIdx+1:] {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			trailerProse++
			if trailerProse > maxTrailerProseLines || lineSignalsRealContent(trimmed) {
				return true
			}
		}
	}
	return false
}

// docstringDelimiters mark a Python/multiline string. When one appears
// anywhere on a preamble or trailer line, the line is real string content
// (e.g. `DOC = """usage:`) and a fence around it is legitimate — so the
// content is not a whole-file wrapper. These are matched with Contains
// because code commonly precedes the delimiter on the opening line.
var docstringDelimiters = []string{`"""`, "'''"}

// commentBlockOpeners mark a comment block. These are matched by prefix so
// that model prose merely mentioning a marker (e.g. "the /* config */
// block:") does not disqualify a genuine whole-file wrapper, while a line
// that actually opens a comment block does.
var commentBlockOpeners = []string{"/*", "*/", "<!--", "-->"}

// lineSignalsRealContent reports whether a trimmed line indicates the text
// around a fence is real file content (a docstring or comment block) rather
// than model prose wrapping the file.
func lineSignalsRealContent(trimmed string) bool {
	for _, d := range docstringDelimiters {
		if strings.Contains(trimmed, d) {
			return true
		}
	}
	for _, m := range commentBlockOpeners {
		if strings.HasPrefix(trimmed, m) {
			return true
		}
	}
	return false
}

// validateNotSuspiciouslyShrunk rejects writes that replace a
// substantial original with a tiny new payload. May 9 2026 structural_edit
// failure: model emitted only `<!DOCTYPE html>\n` (16B) for an entire
// <html>-element rewrite of a 120B file; the on-disk result was a
// destroyed file passed off as a successful "done". The model usually
// produces this shape when its response stops mid-output (json_object
// grammar + length bias converging on minimal valid
// JSON) — the parser sees a syntactically clean tool_call with empty
// content, no truncation marker fires, the recovery path doesn't
// engage, and the destructive write lands.
//
// Heuristic: skip the check when the original was already small
// (line-level edits often legitimately shrink), reject when the new
// payload is clearly a stub. Threshold history:
//
//	v1 (May 9 2026): newSize < 32 — model slipped a 32B stub past it
//	v2 (May 10 morning): bumped to 128 — false-rejected legit
//	  "5KB function refactored to 80B one-liner" case
//	v3 (current): 64 — catches today's 32B destructive stubs and any
//	  "doctype-only" outputs while leaving room for real one-liner
//	  refactors. Subtler cases (legitimate-shape but bad code) are
//	  V3's job now that structural_edit always routes through it.
func validateNotSuspiciouslyShrunk(toolName, path string, oldSize, newSize int) string {
	if oldSize < 100 {
		return ""
	}
	if newSize >= 64 {
		return ""
	}
	return fmt.Sprintf(
		"%s refused: replacement is suspiciously small (%dB) for an existing %dB target at %s. The model usually emits this shape when its response was cut off mid-output or stopped after only the doctype/scaffolding. Re-emit %s with the FULL replacement body — don't ship a stub for a real rewrite.",
		toolName, newSize, oldSize, path, toolName)
}

// leadingDoctypeRe matches an HTML5 <!DOCTYPE ...> declaration at the
// very start of a string (allowing whitespace before it). Case-insensitive
// per spec.
var leadingDoctypeRe = regexp.MustCompile(`(?i)^\s*<!DOCTYPE[^>]*>\s*\n?`)

// stripLeadingDoctype removes a leading <!DOCTYPE> declaration from
// content. Returns the stripped content and true if a doctype was
// present, the original content and false otherwise. Used by structural_edit
// when the selector is <html> to prevent duplicated doctypes (the
// element selector replaces only <html>...</html>, not the preceding
// doctype).
func stripLeadingDoctype(content string) (string, bool) {
	if loc := leadingDoctypeRe.FindStringIndex(content); loc != nil {
		return content[loc[1]:], true
	}
	return content, false
}

// isNewWrite returns true when the resolved path doesn't yet exist on
// disk. Used by stub-detection / pattern-reflex gates to scope their
// rejection logic to genuinely new files — modifying an existing file
// is a different shape and the V3 / surgical-edit gate handles those.
func isNewWrite(resolvedPath string) bool {
	_, err := os.Stat(resolvedPath)
	return os.IsNotExist(err)
}

// patternMatchHint returns a non-empty rejection string when the model
// is creating a NEW file in a directory that already contains files of
// the same extension AND it hasn't read any of those siblings in this
// session. Forces the "model from existing patterns" reflex
// instead of generating from scratch — a NEW route handler should
// match the project's existing route handlers, a new test should match
// the existing test conventions, etc.
//
// Only fires when:
//   - The target path doesn't exist (genuinely new file, not an edit)
//   - The parent directory contains ≥1 sibling with the same extension
//   - ctx.FilesRead doesn't include any of those siblings
//
// Soft-coupled to AgentContext via the FilesRead snapshot we pass in
// (ctx.SnapshotFilesRead() at the call site); keeps the helper testable
// without dragging the whole context type in.
func patternMatchHint(resolvedPath string, filesRead map[string]string) string {
	if !isNewWrite(resolvedPath) {
		return ""
	}
	dir := filepath.Dir(resolvedPath)
	ext := strings.ToLower(filepath.Ext(resolvedPath))
	if ext == "" {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var siblings []string
	for _, e := range entries {
		if e.IsDir() || strings.ToLower(filepath.Ext(e.Name())) != ext {
			continue
		}
		full := filepath.Join(dir, e.Name())
		if full == resolvedPath {
			continue
		}
		siblings = append(siblings, e.Name())
	}
	// Need a meaningful neighborhood — single-sibling dirs are too noisy
	// (one-off configs, isolated entry points). Two or more is enough
	// to call it a "pattern."
	if len(siblings) < 2 {
		return ""
	}
	for _, s := range siblings {
		if _, ok := filesRead[filepath.Join(dir, s)]; ok {
			return ""
		}
	}
	preview := siblings
	if len(preview) > 3 {
		preview = preview[:3]
	}
	return fmt.Sprintf(
		"write_file deferred: you're creating a new %s file in %s, which already contains %d sibling %s files (e.g. %s). Read at least one of those first so this new file follows the project's existing conventions (style, imports, structure). Then re-issue the write_file call.",
		ext, dir, len(siblings), ext, strings.Join(preview, ", "))
}

// looksCorruptedOnDisk returns true when the file at displayPath has the
// markdown-fence-with-prose corruption left by earlier writes: prose preamble
// ("Looking at the task, I need to create..."), a ```html fence, real HTML, a
// closing fence and trailing commentary. Such a file is unparseable, but the
// surgical-edit gate would block write_file from cleaning it up; this tells the
// agent loop "the file is broken, let write_file overwrite it."
//
// It uses the tolerant single-layer search (stripOneFenceLayer). A clean file
// with no fence always returns false.
func looksCorruptedOnDisk(displayPath, existing string) bool {
	// Detection only: nothing is rewritten from this answer. The fuzzy
	// preamble/trailer search belongs here, where a false positive merely lets
	// write_file replace a file, and not on the write path, where it destroyed
	// valid content.
	cleaned, sanitized := stripOneFenceLayer(displayPath, existing)
	return sanitized && cleaned != existing
}

// ---------------------------------------------------------------------------
// Literal-content contracts: the model plans, the harness copies.
//
// A quantized model cannot be trusted to transcribe bytes it was given —
// measured live and deterministic: told to write exactly `BANNER = "ready"`,
// it emits `BANNER = " ready"` under greedy AND default sampling, because the
// space-prefixed BPE token for the word outranks the bare one after a quote
// (the leading-whitespace artifact; arXiv:2502.14969). The literature's
// remedy for the whole class is to treat the LLM as a planner and use a
// deterministic channel for exact emission (arXiv:2601.03640, 2604.18170).
//
// Here that channel is the user's own message: when the request carries the
// intended bytes explicitly, they are recorded as contracts, and a landed
// write is verified against them. A near-miss whose only divergence is
// whitespace is repaired mechanically — the user's bytes are definitionally
// the correct rendering, so substituting them cannot be wrong. Anything
// beyond whitespace is left alone: a bolder repair could mask a legitimate
// transformation the model was asked to make.

// literalMinBytes is the smallest contract worth tracking. Below this,
// prose fragments ("x", "42") would false-positive all over the artifact.
const literalMinBytes = 8

// literalExactlyRe captures the single line following an "exactly ...:"
// marker: `containing exactly one line:\nBANNER = "ready"`. Deliberately
// only ONE line — multi-line literals in prose are what fenced blocks are
// for, and guessing where a prose literal ends is how false positives start.
var literalExactlyRe = regexp.MustCompile(`(?i)(?:exactly|verbatim|precisely)[^:\n]*:[ \t]*\n([^\n]+)`)

// literalFenceRe captures every fenced block in the task text.
var literalFenceRe = regexp.MustCompile("(?s)```(?:[a-zA-Z0-9+#._-]+)?[ \\t]*\\r?\\n(.*?)```")

// extractLiteralBlocks pulls the byte-exact content contracts out of a human
// request. Only explicit forms count: fenced blocks, and the single line
// after an "exactly:"-style marker.
func extractLiteralBlocks(task string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.Trim(s, "\r\n")
		if len(strings.TrimSpace(s)) < literalMinBytes || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, m := range literalFenceRe.FindAllStringSubmatch(task, -1) {
		add(m[1])
	}
	for _, m := range literalExactlyRe.FindAllStringSubmatch(task, -1) {
		add(m[1])
	}
	return out
}

// stripAllWhitespace is the near-miss equivalence: two renderings whose
// non-whitespace bytes agree differ only in spacing, and the user's literal
// is by definition the correct spacing.
func stripAllWhitespace(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r != ' ' && r != '\t' && r != '\r' && r != '\n' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// repairLiteralDrift returns content with every absent literal whose
// rendering differs from it only in spacing WITHIN lines replaced by the
// literal's exact bytes. The bool reports whether anything changed.
//
// Each line of the window must have exactly the literal line's leading
// whitespace. Indentation is structure, not spacing: a user's unindented
// snippet placed inside a function used to match whitespace-insensitively and
// was re-indented to column 0 after the write -- silently, and into a Python
// syntax error.
func repairLiteralDrift(content string, literals []string) (string, []string, bool) {
	var repaired []string
	for _, lit := range literals {
		if strings.Contains(content, lit) {
			continue // contract already satisfied byte-exact
		}
		litLines := strings.Split(lit, "\n")
		litKey := stripAllWhitespace(lit)
		lines := strings.Split(content, "\n")
		for i := 0; i+len(litLines) <= len(lines); i++ {
			window := strings.Join(lines[i:i+len(litLines)], "\n")
			if stripAllWhitespace(window) != litKey || !sameIndentation(lines[i:i+len(litLines)], litLines) {
				continue
			}
			lines = append(lines[:i], append(litLines, lines[i+len(litLines):]...)...)
			content = strings.Join(lines, "\n")
			repaired = append(repaired, lit)
			break
		}
	}
	return content, repaired, len(repaired) > 0
}

func sameIndentation(a, b []string) bool {
	indent := func(l string) string { return l[:len(l)-len(strings.TrimLeft(l, " \t"))] }
	for k := range a {
		if indent(a[k]) != indent(b[k]) {
			return false
		}
	}
	return true
}

// jsonChannelContentFeedback refuses, before execution, content that arrived
// through the JSON tool-call channel in a shape that is usually an escaping
// slip -- and that used to be silently "repaired" by guessing the intended
// bytes. Refusal names the shape and gives two retry paths: escape the line
// break once, or send the file with @fenced, whose bytes never pass through
// JSON decoding (so a genuine control character or one-line body is preserved
// there).
//
//   - A form feed (\f), backspace (\b) or bare carriage return (\r) directly
//     before a letter: what a "\n" that lost its backslash decodes to
//     ("\function" -> form feed + "unction"). It can also be real content, so
//     it is refused rather than rewritten.
//   - A body of 120+ characters with no real line break and three or more
//     literal \n sequences: a line-structured file whose escapes were doubled.
//     Data formats where a single long line is normal are exempt.
//   - A markdown fence with other text around it ("Here is the file:" then
//     ```python ... ```), or a fence that never closes, in content the write
//     path would otherwise store verbatim. Only an exact whole-content wrapper
//     is removed (sanitizeFileContent); anything looser was a guess about
//     which lines are the file.
//
// Scoped to the free-text fields of write_file (not the @fenced sentinel),
// edit_file new_str, insert_after / replace_lines content and structural_edit
// content; the fence check to the three tools that strip wrappers.
func jsonChannelContentFeedback(toolName string, args json.RawMessage) (string, bool) {
	var in map[string]interface{}
	if json.Unmarshal(args, &in) != nil {
		return "", false
	}
	path, _ := in["path"].(string)
	fields := map[string][]string{
		"write_file":      {"content"},
		"edit_file":       {"new_str"},
		"insert_after":    {"content"},
		"replace_lines":   {"content"},
		"structural_edit": {"content"},
	}[toolName]
	for _, f := range fields {
		v, _ := in[f].(string)
		if v == "" || (toolName == "write_file" && isFencedSentinel(v)) {
			continue
		}
		if name, line, ok := controlBeforeLetter(v); ok {
			return fmt.Sprintf("Your %s call was NOT performed — nothing was written. The %s for %s contains a %s "+
				"directly before a letter on line %d. That is what a line break whose \\n lost its backslash "+
				"decodes to (\\function becomes a form feed followed by \"unction\"). If you meant a line break, "+
				"resend with it written as \\n. If the character is really part of the file, send the file with "+
				"\"content\": \"@fenced\" instead.", toolName, f, path, name, line), true
		}
		if feedback, refused := fenceWithTextFeedback(toolName, f, path, v); refused {
			return feedback, true
		}
		if doubledEscapeBody(path, v) {
			return fmt.Sprintf("Your %s call was NOT performed — nothing was written. The %s for %s is a single "+
				"%d-character line whose only line breaks are the two characters \\n, so the escapes were "+
				"doubled. Resend with each line break escaped once (\\n), or send the file with "+
				"\"content\": \"@fenced\".", toolName, f, path, len(v)), true
		}
	}
	return "", false
}

// fenceWithTextFeedback is jsonChannelContentFeedback's check for a markdown
// fence with other text around it, or one that never closes, in the three
// tools that strip wrappers.
func fenceWithTextFeedback(toolName, f, path, v string) (string, bool) {
	if toolName == "write_file" || toolName == "edit_file" || toolName == "structural_edit" {
		if _, wrapped := stripOneFenceLayer(path, v); wrapped {
			if _, exact := stripWholeFileWrapper(path, v); !exact {
				return fmt.Sprintf("Your %s call was NOT performed — nothing was written. The %s for %s "+
					"contains a ``` markdown fence with other text around it (or a fence that never closes), "+
					"and ATLAS does not guess which lines belong in the file. Resend with only the file's own "+
					"lines — no explanation before or after, no fence. If the fence lines really are part of "+
					"the file, send it with \"content\": \"@fenced\".", toolName, f, path), true
			}
		}
	}
	return "", false
}

// controlBeforeLetter finds a form feed, backspace or bare carriage return
// (in content that otherwise uses LF) immediately followed by a letter.
func controlBeforeLetter(v string) (name string, line int, ok bool) {
	if !strings.ContainsAny(v, "\x0c\x08\r") {
		return "", 0, false
	}
	hasLF := strings.Contains(v, "\n")
	isWord := func(b byte) bool {
		return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
	}
	line = 1
	for i := 0; i+1 < len(v); i++ {
		c := v[i]
		if c == '\n' {
			line++
			continue
		}
		if !isWord(v[i+1]) {
			continue
		}
		switch {
		case c == '\x0c':
			return "form feed (\\f)", line, true
		case c == '\x08':
			return "backspace (\\b)", line, true
		case c == '\r' && hasLF:
			return "carriage return (\\r)", line, true
		}
	}
	return "", 0, false
}

// doubledEscapeBody reports a long single-line body of literal \n text for a
// line-structured file.
func doubledEscapeBody(path, v string) bool {
	if strings.ContainsAny(v, "\n\r") || len(v) < 120 || strings.Count(v, `\n`) < 3 {
		return false
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json", ".jsonl", ".ndjson", ".csv", ".tsv", ".map":
		return false
	}
	return true
}

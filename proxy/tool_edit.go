// The tools that change part of a file by its text or lines: edit_file, insert_after and replace_lines.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// edit_file — old_str/new_str with uniqueness validation
// ---------------------------------------------------------------------------

func editFileTool() *ToolDef {
	return &ToolDef{
		Name:   "edit_file",
		Effect: ToolEffectDirectMutation,
		Description: "SURGICAL inline string replacement, ONLY. Use ONLY when changing a few lines inside a function (a None check, a regex, a constant). " +
			"DO NOT use for whole-function rewrites, whole-class rewrites, whole-file replacements, or any change >10 lines — for those, use structural_edit (named node) or write_file (new file). " +
			"old_str must match exactly once (or replace_all=true). Always read_file before editing. " +
			"Heuristic: if you're tempted to copy a whole function/class/HTML element into old_str, you have the wrong tool — switch to structural_edit.",
		InputSchema: EditFileInput{},
		ReadOnly:    false,
		Destructive: false,
		Execute:     executeEditFile,
	}
}

// executeEditFile runs one edit_file call: it finds old_str in the file, checks
// the edit, sends it down the pipeline every content edit shares, and writes
// what comes back.
func executeEditFile(rawInput json.RawMessage, ctx *AgentContext) (res *ToolResult, execErr error) {
	var input EditFileInput
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return nil, errNoMutation(fmt.Errorf("invalid input: %w", err))
	}
	if refusal := editInputRefusal(input, rawInput); refusal != nil {
		return refusal, nil
	}

	path := resolveAgentPath(ctx, input.Path)
	content, err := readFileToEdit(ctx, path, input.Path)
	if err != nil {
		return nil, err
	}
	actualOldStr, matchNote, err := locateOldStr(content, input)
	if err != nil {
		return nil, err
	}
	if err := matchedEditError(content, actualOldStr, input); err != nil {
		return nil, err
	}

	// Sanitise the replacement string before splicing it in. The
	// model occasionally fences the new_str ("```python\n...\n```")
	// even though it's a fragment, not a whole file. If we let
	// that slip through, every line of the edit would have a
	// stray ``` at the top and bottom.
	if cleanedNew, sanitized := sanitizeFileContent(input.Path, input.NewStr); sanitized {
		log.Printf("[edit_file] removed a whole-content fence wrapper from new_str of %s", input.Path)
		input.NewStr = cleanedNew
		defer func() { noteContentChange(res, wholeFileWrapperNote) }()
	}

	if matchNote != "" {
		note := matchNote
		defer func() { noteContentChange(res, note) }()
	}

	newContent := applyEdit(content, actualOldStr, input)
	editKey := input.Path + "\x00" + input.OldStr + "\x00" + input.NewStr
	if refusal := composedEditRefusal(ctx, input, content, newContent, actualOldStr, editKey); refusal != nil {
		return refusal, nil
	}

	// Same pipeline entry every content edit uses; see
	// runEditPipeline for why it is shared rather than inlined.
	route := runEditPipeline(ctx, "edit_file", path, input.Path, content, newContent)
	if route.Cancelled != nil {
		return route.Cancelled, nil
	}
	if route.Delivered {
		// The candidate landed through the protected chain. Writing the
		// caller's own edit now would undo a delivery that settlement
		// and restoration already decided about.
		return attachV3(route.Result, route.Meta), nil
	}
	newContent = route.Content

	// Syntax gate on the composed result. A truncated new_str (or a
	// string-level edit that broke the file) must not land when V3
	// verification didn't run or failed — same class as the
	// write_file fallback gate (mini-bench t06/t09). Semantics:
	// an edit must not INTRODUCE breakage — when the ORIGINAL
	// content already fails to parse, a still-failing result is a
	// permitted repair-in-progress (fixing one error at a time),
	// so the gate only blocks healthy→broken transitions.
	observed, refusal := editSyntaxObservation(ctx, "edit_file", path, input.Path,
		content, newContent, editFileSyntaxMessage(input.Path))
	if refusal != nil {
		return refusal, nil
	}
	if refusal := editGateRefusal(ctx, path, input.Path, content, newContent); refusal != nil {
		return refusal, nil
	}
	logMandatoryChecks(ctx, "edit_file", input.Path, "")

	if err := writeEditedFile(path, input.Path, newContent, observed); err != nil {
		return nil, err
	}

	// Update cached state with whatever was actually written
	ctx.RecordFileRead(path, newContent)
	ctx.RecordBodySeen(path)

	out := editFileOutput(input, content, newContent, actualOldStr)
	outBytes, _ := json.Marshal(out)
	// Record it so an identical re-application is refused above.
	if ctx.AppliedEdits != nil {
		ctx.AppliedEdits[editKey] = true
	}
	result := &ToolResult{Success: true, Data: outBytes,
		MutationStatus: MutationApplied}
	// The observation describes exactly the bytes just written: the tool's
	// own edit. A delivered candidate returned above.
	overlayValidation(result, observed)
	return result, nil
}

// editInputRefusal refuses a call whose arguments cannot name an edit.
func editInputRefusal(input EditFileInput, rawInput json.RawMessage) *ToolResult {
	// Reject empty path — same reasoning as read_file.
	if strings.TrimSpace(input.Path) == "" {
		return noMutation("edit_file: path cannot be empty. Use read_file first on the target, then edit_file with the same path.")
	}

	// Malformed-args check, BEFORE the read-staleness check: a call
	// with an empty old_str, a key literally named "", and write_file's
	// `content` field needs to hear that, not "file not read yet", which
	// is true and says nothing about what is wrong with the call.
	if strings.TrimSpace(input.OldStr) == "" {
		hint := ""
		if bytes.Contains(rawInput, []byte(`"content"`)) {
			hint = " You sent `content`, which is write_file's field — edit_file does not take it."
		}
		return noMutation("edit_file: old_str is required and cannot be empty." + hint +
			" The call is edit_file {\"path\":..., \"old_str\":<one unique line copied from the file>, " +
			"\"new_str\":<what replaces it>}. To ADD lines rather than replace any, use " +
			"insert_after {\"path\":..., \"line\":<number from read_file>, \"content\":...} — that one does " +
			"take `content`.")
	}
	return nil
}

// readFileToEdit reads the file an edit names, if the model has seen what is
// there now.
func readFileToEdit(ctx *AgentContext, path, relPath string) (string, error) {
	// Require file was read first (staleness protection)
	if !editViewIsCurrent(ctx, path) {
		return "", errNoMutation(fmt.Errorf("file not read yet — use read_file first before editing: %s", relPath))
	}

	// Read current content
	data, err := readRegularFile(path)
	if err != nil {
		return "", errNoMutation(fmt.Errorf("cannot read %s: %w", relPath, err))
	}

	// Check for staleness
	if modifiedSinceSessionView(ctx, path, data) {
		return "", errNoMutation(fmt.Errorf("file modified since last read — read it again before editing: %s", relPath))
	}
	return string(data), nil
}

// locateOldStr finds the text of the file that old_str names, and a note for
// the model when the match needed any tolerance.
func locateOldStr(content string, input EditFileInput) (string, string, error) {
	// Find old_str. Exact bytes first; a match that needed quote-style
	// or whitespace tolerance is allowed but reported, so the model
	// knows which text of the file its new_str replaced.
	actualOldStr := findActualString(content, input.OldStr)
	if actualOldStr == input.OldStr {
		return actualOldStr, "", nil
	}
	if actualOldStr != "" {
		return actualOldStr, fmt.Sprintf("old_str matched %s only after treating curly and straight quotes as the "+
			"same; the file's own text there was replaced by new_str exactly as sent.",
			lineSpanOf(content, actualOldStr)), nil
	}
	// An old_str that matches only after decoding HTML entities is NOT
	// accepted by decoding new_str to match: the replacement may hold
	// entities on purpose (&amp; in markup, or in a Python string), and
	// decoding it would write different bytes from the ones sent. The
	// targeted refusal below names the
	// entities and the retry.
	// read_file prints "12<tab>" before each line for reference
	// and the model pastes back what it was shown. Stripping the
	// prefix would be an unannounced rewrite of both arguments, so
	// old_str and new_str are applied exactly as sent; text that
	// really begins "12<tab>" (tab-separated data) still edits,
	// because it matched above. When only the prefix-free form
	// matches, nothing is changed and the refusal gives the exact
	// replace_lines call that makes this change without
	// reproducing the text: the lines where it matched, their
	// real contents, and a note if new_str carries the prefix too.
	if stripped := stripLineNumberPrefixes(input.OldStr); stripped != input.OldStr {
		if maybeMatch := findActualString(content, stripped); maybeMatch != "" {
			log.Printf("[edit_file] old_str on %s matches only without read_file's line-number prefixes — refused", input.Path)
			return "", "", errNoMutation(fmt.Errorf("%s", prefixedEditRefusal(input.Path, content, maybeMatch,
				input.OldStr, input.NewStr)))
		}
	}
	// Last resort before failing: whitespace-tolerant line match.
	// Small models can't reproduce a code block byte-for-byte —
	// indentation width, tabs-vs-spaces, and trailing spaces drift
	// constantly — so exact old_str matching is the #1 reason
	// edit_file fails for them, which strands the whole V3/lens
	// stack (it only engages after a successful edit). This
	// matches old_str against the file ignoring per-line leading/
	// trailing whitespace, and ONLY accepts a UNIQUE match — an
	// ambiguous or content-different old_str still fails, so it
	// can never silently edit the wrong place. Exact-matching
	// (frontier) models never reach this path.
	if fuzzy, ok := findFuzzyLineMatch(content, input.OldStr); ok {
		log.Printf("[edit_file] exact old_str missed on %s; unique whitespace-tolerant match found — proceeding (small-model indentation drift)", input.Path)
		return fuzzy, fmt.Sprintf("old_str matched %s only after ignoring leading and trailing whitespace "+
			"on each line; those lines were replaced by new_str exactly as sent.", lineSpanOf(content, fuzzy)), nil
	}
	// Degenerate old_str: not drift the fuzzy matcher can rescue,
	// but corrupted output: runs of bare \r, or a stray `\rVert`
	// (a LaTeX fragment), in the middle of copied code. "Must match
	// byte-for-byte" is useless advice for that. Name it, and ask for
	// the short anchor that is far less likely to degenerate.
	// A long multi-line anchor is a transcription burden, and a model
	// can corrupt a character under it. One line
	// is enough to locate an edit, and both the tool description
	// and the structural_edit steer already say so — repeat it
	// here, where the failure actually happened.
	// Checked before everything: one corrupted character is
	// invisible next to a long old_str, and every other hint here
	// sends the model to re-copy text it already copied right.
	if err := corruptedOldStrError(content, input); err != nil {
		return "", "", err
	}
	return "", "", oldStrMismatchError(content, input)
}

// corruptedOldStrError names what is wrong with an old_str that was not copied
// faithfully, or returns nil.
func corruptedOldStrError(content string, input EditFileInput) error {
	if bad := foreignRunes(input.OldStr, content); len(bad) > 0 {
		return errNoMutation(fmt.Errorf("string to replace not found in file. Your `old_str` contains %s, "+
			"which appears nowhere in %s. That is a corrupted character, not a mis-copy — most often "+
			"an operator that decoded wrong (`&&`, `||`, `>=`, `->`). Re-read the line from the file "+
			"and re-emit `old_str` with plain ASCII operators.\nSearched for: %s",
			describeForeignRunes(bad), input.Path, truncateStr(input.OldStr, 200)))
	}
	// Checked next because it is the most specific remaining: the
	// text is otherwise correct and only carries read_file's
	// display prefix. Saying "not found" here sends the model back to
	// re-copy a line it already copied right.
	if n := lineNumberPrefixedLines(input.OldStr); n > 0 {
		return errNoMutation(fmt.Errorf("string to replace not found in file. Your `old_str` still has "+
			"read_file's line-number prefix on %d line(s) — the \"12<tab>\" at the start is "+
			"added for reference and is NOT in the file. Send the line text only, starting at "+
			"the first real character. If you are ADDING lines rather than changing one, "+
			"insert_after takes that number directly and needs no old_str at all.\nSearched for: %s",
			n, truncateStr(input.OldStr, 200)))
	}
	if lines := strings.Count(input.OldStr, "\n") + 1; lines >= 5 {
		return errNoMutation(fmt.Errorf("string to replace not found in file. Your `old_str` is %d lines "+
			"long — reproducing that much text byte-for-byte is where these edits go wrong. "+
			"Use `replace_lines` instead: give the start and end line numbers read_file "+
			"printed and assert only the FIRST and LAST line of that range, so there is no "+
			"multi-line block to reproduce. Or anchor edit_file on ONE short line that "+
			"appears exactly once and put the whole replacement in `new_str`. If you are "+
			"ADDING code rather than replacing it, use insert_after with the line number "+
			"read_file printed — it needs no old_str at all.\nSearched for: %s",
			lines, truncateStr(input.OldStr, 200)))
	}
	if n := strayCarriageReturns(input.OldStr); n >= 3 {
		return errNoMutation(fmt.Errorf("string to replace not found in file. Your `old_str` "+
			"contains %d stray carriage returns and looks corrupted rather than copied "+
			"— long blocks tend to come out this way. Use `replace_lines` on the line "+
			"range instead, or re-emit `old_str` as ONE short unique line taken from the "+
			"file (the single line you are changing), not a multi-line block", n))
	}
	return nil
}

// oldStrMismatchError is the targeted refusal for an old_str the file does not
// hold.
func oldStrMismatchError(content string, input EditFileInput) error {
	// Mismatch persists — return targeted error.
	hasEntities := strings.Contains(input.OldStr, "&lt;") ||
		strings.Contains(input.OldStr, "&gt;") ||
		strings.Contains(input.OldStr, "&amp;")
	literalsOnDisk := strings.ContainsAny(content, "<>&")
	if hasEntities && literalsOnDisk {
		alt := ""
		if guide := selectorGuidance(input.Path, content); guide != "" {
			alt = " For whole-element rewrites, structural_edit is the cleaner option — it takes a selector and the new content body, no old_str needed; " + guide + "."
		}
		return errNoMutation(fmt.Errorf("string to replace not found in file. Your `old_str` contains HTML-entity-encoded characters (`&lt;` / `&gt;` / `&amp;`) but the file on disk has literal `<` / `>` / `&`. Re-emit `old_str` with literal angle brackets — JSON strings should contain literal `<` not `&lt;`.%s\nSearched for: %s",
			alt, truncateStr(input.OldStr, 200)))
	}
	// Generic mismatch — the model's old_str doesn't byte-match
	// the file (whitespace, quotes, or paraphrase drift, which
	// smaller models do constantly). For structured files,
	// structural_edit sidesteps the whole problem: it selects the node
	// by name, no old_str to reproduce exactly. Steer there.
	astAlt := ""
	if guide := selectorGuidance(input.Path, content); guide != "" {
		astAlt = " To replace a whole function/class/element without " +
			"matching exact text, use structural_edit with a selector and the " +
			"new content — no old_str needed, so a near-miss can't fail it; " +
			guide + "."
	}
	// Ground the retry in the file's REAL content. A small model
	// frequently writes old_str from its memory of the file
	// rather than the file itself. Without an anchor it
	// gives up on the surgical edit and rewrites the whole node
	// from the same faulty memory. Quoting the closest actual
	// line gives it real bytes to copy into the next attempt.
	hint := closestLineHint(content, input.OldStr)
	return errNoMutation(fmt.Errorf("string to replace not found in file. old_str must match the file byte-for-byte (whitespace and quotes included).%s%s\nSearched for: %s", astAlt, hint, truncateStr(input.OldStr, 200)))
}

// matchedEditError refuses an edit whose old_str was found but cannot be applied
// as sent.
func matchedEditError(content, actualOldStr string, input EditFileInput) error {
	// Check uniqueness
	count := strings.Count(content, actualOldStr)
	if count > 1 && !input.ReplaceAll {
		return errNoMutation(fmt.Errorf("found %d matches of the string to replace. Set replace_all=true to replace all, or provide more context to uniquely identify the instance", count))
	}

	// No-op check. The bare diagnosis is a dead end: the model
	// copies old_str into new_str precisely because it cannot
	// reproduce a span with a change applied, so telling it only
	// that the two match gives it nothing to do differently and it
	// re-sends the identical call until the loop breaker kills the
	// session. Name the tool that removes the
	// requirement it just failed.
	if input.OldStr == input.NewStr {
		alt := "`replace_lines` with the line numbers read_file printed — you assert only the FIRST and LAST line of the range, so there is no span to reproduce"
		if ext := strings.ToLower(filepath.Ext(input.Path)); ext == ".py" || ext == ".html" || ext == ".htm" {
			alt = "`structural_edit` with a selector (e.g. `function:update`) and the new body — it needs no old_str at all, so there is nothing to copy"
		}
		// The alternative it is being sent to needs line numbers, and
		// "the line numbers read_file printed" may be many turns and
		// several successful edits old. Where those bytes sit RIGHT NOW
		// is something this call already knows, so it is said here.
		return errNoMutation(fmt.Errorf(
			"old_str and new_str are identical, so this edit would change nothing. "+
				"Re-sending it will not help. You are being asked to reproduce a span "+
				"verbatim AND change it, which is what just failed — use %s.%s "+
				"If you meant to REPLACE the whole file, use write_file with the "+
				"complete new contents", alt, currentSpanNote(input.Path, content, actualOldStr)))
	}
	return nil
}

// applyEdit is the file with the edit applied.
func applyEdit(content, actualOldStr string, input EditFileInput) string {
	if input.ReplaceAll {
		return strings.ReplaceAll(content, actualOldStr, input.NewStr)
	}
	return strings.Replace(content, actualOldStr, input.NewStr, 1)
}

// composedEditRefusal refuses a composed edit that should not go further.
func composedEditRefusal(ctx *AgentContext, input EditFileInput, content, newContent, actualOldStr,
	editKey string) *ToolResult {
	// Shrinkage guard — same shape as structural_edit's. Catches the
	// "model emitted a stub for old_str" failure where new_str
	// is implausibly tiny for a substantial old_str. We compare
	// the local replacement footprint (old_str → new_str), not
	// whole-file size, so refactors that genuinely shrink the
	// file aren't false-rejected.
	if rejection := validateNotSuspiciouslyShrunk("edit_file", input.Path, len(actualOldStr), len(input.NewStr)); rejection != "" {
		log.Printf("[edit_file] rejecting suspicious shrinkage: %s old_str=%dB new_str=%dB",
			input.Path, len(actualOldStr), len(input.NewStr))
		// The shrinkage heuristic declining bytes that were formed.
		return refusedNoCheck(rejection)
	}

	// No-op guard — same rationale as structural_edit's. new_str identical
	// to old_str (or a replacement that leaves the file unchanged)
	// must not report success: the model believes the fix landed
	// and moves on while the bug is still on disk.
	if newContent == content {
		log.Printf("[edit_file] no-op edit rejected for %s — file content unchanged", input.Path)
		return noMutation("edit_file: new_str is identical to old_str — nothing was changed and the bug is still there. " +
			"Look at the current code again and emit a new_str that actually differs from the existing code.")
	}

	// Already-applied guard. The check above only catches an edit that
	// leaves the file byte-identical; it cannot see an edit that
	// applied once and is now being applied AGAIN. A whitespace-only
	// change is the dangerous shape: after " x" replaces "x", the
	// original old_str still matches as a substring, so the same call
	// keeps succeeding and keeps adding whitespace. The repeat
	// detector does not help — it needs three occurrences, and two
	// runs of a 75-second edit is already two minutes gone.
	if ctx.AppliedEdits[editKey] {
		log.Printf("[edit_file] duplicate edit rejected for %s — already applied this session", input.Path)
		return refusedNoCheck("edit_file: this exact edit already succeeded earlier in this " +
			"session, so applying it again would just repeat it — and when the change is whitespace-only, the " +
			"old_str still matches afterwards, so it can repeat forever. The file already has this change. " +
			"Read the file to see its current state, then either make a DIFFERENT edit or declare done.")
	}
	return nil
}

// editFileSyntaxMessage is edit_file's refusal for a result that does not parse.
func editFileSyntaxMessage(relPath string) func(string) string {
	return func(detail string) string {
		// An embedded-script finding is already model-ready and names
		// its own fix; the generic wrapper's "check old_str/new_str"
		// advice is wrong for JavaScript inside a string.
		if msg, isEmbedded := embeddedScriptRejectionFor(detail); isEmbedded {
			return msg
		}
		return fmt.Sprintf(
			"edit_file result for %s does not parse (%s). The file was NOT modified — check that old_str/new_str are complete and re-issue the edit.",
			relPath, truncateStr(detail, 200))
	}
}

// editGateRefusal runs edit_file's gates after the syntax gate.
func editGateRefusal(ctx *AgentContext, path, relPath, content, newContent string) *ToolResult {
	// Structural gate: a parse-clean edit can still introduce
	// an unresolved direct call (render_template with only
	// render_template_string imported) that 500s at runtime. Block a
	// write that NEWLY makes a name unresolved; a pre-existing one
	// (mid-repair) is allowed, mirroring the syntax gate above.
	if introduced := editIntroducesUnresolved(ctx, path, content, newContent); len(introduced) > 0 {
		log.Printf("[edit_file] edit introduces unresolved call(s) %v in %s — rejecting", logPaths(introduced), logPath(relPath))
		return structuralRefusal(structuralRejection(relPath, introduced))
	}
	// checkFallbackSyntax parses the embedded script but has no pre-edit
	// file to compare against, so the comparative findings — a render
	// loop that stopped repeating, a lexical binding declared twice —
	// need this gate, as on every other edit path.
	if msg := embeddedScriptGate(ctx, path, content, newContent); msg != "" {
		log.Printf("[edit_file] edit breaks an embedded script in %s — rejecting", logPath(relPath))
		return structuralRefusal(msg)
	}
	if msg := duplicateMainGuard(path, content, newContent); msg != "" {
		log.Printf("[edit_file] edit duplicates the module entrypoint in %s — rejecting", logPath(relPath))
		return structuralRefusal(msg)
	}
	return nil
}

// writeEditedFile replaces the file atomically.
func writeEditedFile(path, relPath, newContent string, observed checkOutcome) error {
	tmpPath := path + atomicTempSuffix
	if err := os.WriteFile(tmpPath, []byte(newContent), 0644); err != nil {
		return editWriteFailure(path,
			fmt.Errorf("cannot write %s: %w", relPath, err), observed)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return editWriteFailure(path,
			fmt.Errorf("cannot rename temp file: %w", err), observed)
	}
	return nil
}

// editFileOutput is what edit_file tells the model about the change.
func editFileOutput(input EditFileInput, content, newContent, actualOldStr string) EditFileOutput {
	// Build diff preview against the original on-disk content
	oldLines := strings.Count(input.OldStr, "\n") + 1
	newLines := strings.Count(input.NewStr, "\n") + 1
	preview := buildDiffPreview(content, newContent, actualOldStr, input.NewStr)

	out := EditFileOutput{
		OK:           true,
		DiffPreview:  preview,
		LinesAdded:   newLines - oldLines,
		LinesRemoved: 0,
	}
	if newLines < oldLines {
		out.LinesRemoved = oldLines - newLines
		out.LinesAdded = 0
	}
	return out
}

// closestLineHint finds the file line most similar to the first
// non-blank line of a missed old_str and formats it (with its line
// number) for inclusion in the edit_file mismatch error. Similarity is
// shared-token count — crude, but enough to map a from-memory paraphrase
// like `item = items[id + 1]` onto the real
// `return jsonify(items[item_id + 1])`. Returns "" when nothing clears a
// minimal overlap bar (so unrelated guesses don't get a misleading
// anchor).
func closestLineHint(content, oldStr string) string {
	probeToks := identifierTokens(firstNonBlankLine(oldStr))
	lines := strings.Split(content, "\n")
	bestIdx, bestScore := lineSharingMostTokens(lines, probeToks)
	// Require at least 2 shared identifiers and half the probe's tokens —
	// below that the "closest" line is likely unrelated.
	if bestIdx < 0 || bestScore < 2 || bestScore*2 < len(probeToks) {
		return ""
	}
	return fmt.Sprintf("\nClosest actual line in the file (line %d): %s\nCopy real lines from the file into old_str — do not write them from memory.",
		bestIdx+1, truncateStr(strings.TrimSpace(lines[bestIdx]), 160))
}

func firstNonBlankLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			return line
		}
	}
	return ""
}

func identifierTokens(line string) map[string]bool {
	tokens := map[string]bool{}
	for _, token := range strings.FieldsFunc(line, func(r rune) bool {
		return !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	}) {
		tokens[token] = true
	}
	return tokens
}

// lineSharingMostTokens returns the first line with the highest count of
// tokens in common with probeToks, or -1 when no line shares one.
func lineSharingMostTokens(lines []string, probeToks map[string]bool) (bestIdx, bestScore int) {
	bestIdx = -1
	for i, line := range lines {
		score := 0
		for token := range identifierTokens(line) {
			if probeToks[token] {
				score++
			}
		}
		if score > bestScore {
			bestScore, bestIdx = score, i
		}
	}
	return bestIdx, bestScore
}

// findFuzzyLineMatch rescues an old_str whose only error is per-line
// whitespace drift (indentation width, tabs vs spaces, trailing spaces) —
// the dominant edit_file failure mode for small models. It compares old_str
// against the file line-by-line with each line whitespace-stripped, and
// returns the EXACT span from the file (original indentation preserved) so
// the caller's existing uniqueness-count + replace logic works unchanged.
//
// Safety: it requires exactly ONE matching window. Zero or multiple matches
// return ("", false) so the caller fails cleanly rather than editing a
// guessed location. It also refuses to match when old_str has no
// non-whitespace content (would match any blank run). Content differences
// beyond whitespace (renamed tokens, changed quotes) do NOT match — those
// are semantic and must fail.
func findFuzzyLineMatch(content, oldStr string) (string, bool) {
	fileLines := strings.Split(content, "\n")
	oldLines := strings.Split(oldStr, "\n")
	// Drop a single trailing empty line from a trailing newline in old_str.
	if len(oldLines) > 1 && strings.TrimSpace(oldLines[len(oldLines)-1]) == "" {
		oldLines = oldLines[:len(oldLines)-1]
	}
	want := trimmedLines(oldLines)
	if want == nil {
		return "", false
	}
	n := len(want)
	matchStart := -1
	for i := 0; i+n <= len(fileLines); i++ {
		if !trimmedLinesMatchAt(fileLines, i, want) {
			continue
		}
		if matchStart >= 0 {
			return "", false // ambiguous — fail safe
		}
		matchStart = i
	}
	if matchStart < 0 {
		return "", false
	}
	return strings.Join(fileLines[matchStart:matchStart+n], "\n"), true
}

// trimmedLines trims each line. It returns nil when every line is blank, so an
// all-whitespace target is refused.
func trimmedLines(lines []string) []string {
	trimmed := make([]string, len(lines))
	nonEmpty := false
	for i, line := range lines {
		trimmed[i] = strings.TrimSpace(line)
		if trimmed[i] != "" {
			nonEmpty = true
		}
	}
	if !nonEmpty {
		return nil
	}
	return trimmed
}

func trimmedLinesMatchAt(fileLines []string, start int, want []string) bool {
	for offset, line := range want {
		if strings.TrimSpace(fileLines[start+offset]) != line {
			return false
		}
	}
	return true
}

// findActualString searches for oldStr in content, handling quote normalization.
// Returns the actual string found in content (may differ in quote style).
func findActualString(content, oldStr string) string {
	// Direct match first
	if strings.Contains(content, oldStr) {
		return oldStr
	}

	// Quote normalization: try replacing curly quotes with straight and vice versa
	normalized := normalizeQuotes(oldStr)
	if normalized != oldStr && strings.Contains(content, normalized) {
		return normalized
	}

	// Try the reverse direction
	denormalized := denormalizeQuotes(oldStr)
	if denormalized != oldStr && strings.Contains(content, denormalized) {
		return denormalized
	}

	return ""
}

// normalizeQuotes replaces curly quotes with straight quotes.
func normalizeQuotes(s string) string {
	r := strings.NewReplacer(
		"\u201c", "\"", // left double
		"\u201d", "\"", // right double
		"\u2018", "'", // left single
		"\u2019", "'", // right single
	)
	return r.Replace(s)
}

// denormalizeQuotes replaces straight quotes with curly quotes (best-effort).
func denormalizeQuotes(s string) string {
	r := strings.NewReplacer(
		"\"", "\u201c", // straight double → left double (approximate)
		"'", "\u2019", // straight single → right single (approximate)
	)
	return r.Replace(s)
}

// buildDiffPreview creates a unified-diff-style preview of the edit.
func buildDiffPreview(oldContent, newContent, oldStr, newStr string) string {
	// Find the line number where the change starts
	idx := strings.Index(oldContent, oldStr)
	if idx < 0 {
		return ""
	}
	lineNum := strings.Count(oldContent[:idx], "\n") + 1

	var sb strings.Builder
	fmt.Fprintf(&sb, "@@ line %d @@\n", lineNum)

	// Show removed lines
	for _, line := range strings.Split(oldStr, "\n") {
		fmt.Fprintf(&sb, "- %s\n", line)
	}
	// Show added lines
	for _, line := range strings.Split(newStr, "\n") {
		fmt.Fprintf(&sb, "+ %s\n", line)
	}

	return sb.String()
}

// ---------------------------------------------------------------------------
// insert_after and replace_lines
// ---------------------------------------------------------------------------

// insert_after adds lines at a location the model NAMES instead of one it
// reproduces.
//
// edit_file needs an anchor copied byte-for-byte; structural_edit needs the
// whole node re-emitted. Both put a large verbatim-output burden on the model,
// and that is the step that measurably fails: asked to reproduce a ten-line
// anchor it produced "safe_load_aller" for "safe_load_all". read_file already
// returns "N<tab>content", so a line number is something the model can cite
// rather than transcribe, and only the NEW text has to be generated.
//
// Same gates as every other write: workspace boundary, syntax (healthy->broken
// so a file already failing is not blocked), and the structural check for
// newly unresolved calls.
func insertAfterTool() *ToolDef {
	return &ToolDef{
		Name:   "insert_after",
		Effect: ToolEffectDirectMutation,
		Description: "Insert new lines into a file AFTER a given line number, without touching anything else. " +
			"Use this to ADD code — a new branch, function, import, or case — when you are not changing existing lines. " +
			"`line` is the 1-based number shown by read_file (0 inserts at the top of the file); `content` is only the new text. " +
			"Prefer this over edit_file when adding rather than replacing: there is no old_str to reproduce, so a long or awkward anchor cannot go wrong. " +
			"To CHANGE an existing line use edit_file; to replace a whole function use structural_edit.",
		InputSchema: InsertAfterInput{},
		Destructive: false,
		Execute: func(input json.RawMessage, ctx *AgentContext) (*ToolResult, error) {
			var in InsertAfterInput
			if err := json.Unmarshal(input, &in); err != nil {
				return nil, errNoMutation(fmt.Errorf("invalid input: %w", err))
			}
			if strings.TrimSpace(in.Path) == "" {
				// Naming only the missing field sends the model back with a
				// different field missing.
				return noMutation("insert_after: path is required. The call is " +
					"insert_after {\"path\":\"app.py\", \"line\":<1-based number from read_file, 0 for top of file>, " +
					"\"content\":<the new lines>}."), nil
			}
			if in.Content == "" {
				return noMutation("insert_after: content is empty — nothing would be inserted"), nil
			}
			path := resolveAgentPath(ctx, in.Path)
			if !editViewIsCurrent(ctx, path) {
				return nil, errNoMutation(fmt.Errorf("file not read yet — use read_file first so the line numbers are current: %s", in.Path))
			}
			data, err := readRegularFile(path)
			if err != nil {
				return noMutation(fmt.Sprintf("cannot read %s: %v", in.Path, err)), nil
			}
			original := string(data)
			return insertLinesAfter(ctx, in, path, original)
		},
	}
}

// insertLinesAfter splices the new lines in after in.Line and sends the result
// through the edit pipeline. A line out of range, and an insertion that moves
// statements into another block, are refused.
func insertLinesAfter(ctx *AgentContext, in InsertAfterInput, path, original string) (*ToolResult, error) {
	lines := strings.Split(original, "\n")
	// A trailing newline yields a final empty element; inserting after
	// it would append past the end, so treat it as the boundary.
	limit := len(lines)
	if limit > 0 && lines[limit-1] == "" {
		limit--
	}
	if in.Line < 0 || in.Line > limit {
		return noMutation(fmt.Sprintf(
			"insert_after: line %d is out of range for %s, which has %d lines. Use the numbers read_file showed you.",
			in.Line, in.Path, limit)), nil
	}
	insert := strings.Split(strings.TrimSuffix(in.Content, "\n"), "\n")
	// Before any pipeline work: an insertion that moves existing
	// statements into another block is refused outright, however well
	// the result parses. See insertionReparentsPython.
	if msg, moved := insertionReparentsPython(path, lines, in.Line, insert); moved {
		return structuralRefusal(msg), nil
	}
	merged := append([]string{}, lines[:in.Line]...)
	merged = append(merged, insert...)
	merged = append(merged, lines[in.Line:]...)
	updated := strings.Join(merged, "\n")

	// Healthy->broken only: a file already failing the checker stays
	// editable, which is what makes repair-in-progress possible.
	// Every content edit goes through the pipeline — these two were
	// producing one greedy sample with no candidate generation and no
	// lens scoring, which is exactly what the tier system exists to
	// prevent.
	route := runEditPipeline(ctx, "insert_after", path, in.Path, original, updated)
	if route.Cancelled != nil {
		return route.Cancelled, nil
	}
	if route.Delivered {
		// The candidate landed through the protected chain. Writing the
		// caller's own edit now would undo a delivery that settlement
		// and restoration already decided about.
		return attachV3(route.Result, route.Meta), nil
	}
	v3Out := route.Meta
	updated = route.Content

	return writeInsertion(ctx, in, path, original, updated, insert, v3Out)
}

// writeInsertion runs the gates every content edit shares over the bytes about
// to land, writes them, and reports the insertion.
func writeInsertion(ctx *AgentContext, in InsertAfterInput, path, original, updated string,
	insert []string, v3Out V3EditMetadata) (*ToolResult, error) {
	observed, refusal := editSyntaxObservation(ctx, "insert_after", in.Path, in.Path,
		original, updated, func(detail string) string {
			return fallbackSyntaxRejection(in.Path, updated, detail)
		})
	if refusal != nil {
		return refusal, nil
	}
	if introduced := editIntroducesUnresolved(ctx, path, original, updated); len(introduced) > 0 {
		return structuralRefusal(structuralRejection(in.Path, introduced)), nil
	}
	if msg := embeddedScriptGate(ctx, path, original, updated); msg != "" {
		return structuralRefusal(msg), nil
	}
	if msg := duplicateMainGuard(path, original, updated); msg != "" {
		return structuralRefusal(msg), nil
	}
	logMandatoryChecks(ctx, "insert_after", in.Path, "")

	if err := os.WriteFile(path, []byte(updated), 0644); err != nil {
		return nil, editWriteFailure(path,
			fmt.Errorf("cannot write %s: %w", in.Path, err), observed)
	}
	ctx.SessionWrites[in.Path] = true
	ctx.RecordFileRead(path, updated)
	ctx.RecordBodySeen(path)
	log.Printf("[insert_after] %s +%d lines after line %d", logPath(in.Path), len(insert), in.Line)
	out, _ := json.Marshal(EditFileOutput{
		OK:          true,
		DiffPreview: fmt.Sprintf("+%d lines after line %d", len(insert), in.Line),
	})
	result := &ToolResult{Success: true, Data: out,
		MutationStatus: MutationApplied}
	// The observation describes exactly the bytes just written --
	// the tool's own edit, an authorized V3 candidate, or a baseline
	// restored after revocation, whichever runEditPipeline returned.
	// Provenance is a separate fact, attached by attachV3.
	overlayValidation(result, observed)
	return attachV3(result, v3Out), nil
}

// replaceLinesMaxSpan bounds a single replace_lines call. Above this the model
// is re-authoring rather than editing, and a whole-node rewrite is both more
// reliable and easier to verify.
//
// 60 because the unit of work is a whole function, and a JavaScript function
// inside a Flask template runs 40-50 lines. A lower cap sends the model to
// structural_edit, which cannot reach into a Python string literal, and its
// refusal sends it back here. The size guard is also not what makes this
// tool safe: expected_first_line / expected_last_line already fail a stale
// range, and they cost the same two lines whether the span is 5 or 50.
const replaceLinesMaxSpan = 60

// relocateStaleRange finds where a stale replace_lines range moved to.
//
// The model's line numbers go stale the moment an earlier edit changes the
// file's length, and it re-sends the same numbers because from its side
// nothing looks wrong.
//
// The assertions the call already carries are enough to fix it. If the
// expected first and last lines both appear, at the same offset, exactly
// once each, the range simply moved — apply it there instead of refusing.
// Ambiguity (either line appearing more than once, or the two disagreeing
// about the shift) returns 0 and leaves the refusal in place: relocating an
// edit to the wrong place is far worse than asking for a re-read.
func relocateStaleRange(fileLines []string, limit int,
	expectedFirst, expectedLast string, span int) int {
	first := strings.TrimSpace(expectedFirst)
	last := strings.TrimSpace(expectedLast)
	if first == "" || last == "" || span < 1 {
		return 0
	}
	findUnique := func(want string) int {
		found := 0
		for i := 0; i < limit; i++ {
			if strings.TrimSpace(fileLines[i]) == want {
				if found != 0 {
					return -1 // ambiguous
				}
				found = i + 1
			}
		}
		return found
	}
	firstAt := findUnique(first)
	if firstAt <= 0 {
		return 0
	}
	lastAt := findUnique(last)
	if lastAt <= 0 {
		return 0
	}
	if lastAt-firstAt+1 != span {
		return 0 // the block changed shape, not just position
	}
	return firstAt
}

// lineAssertionMismatch compares an expected line against what is actually
// there, whitespace-insensitively, and renders the correction when they differ.
//
// Whitespace-insensitive on purpose: the model reliably reproduces the TEXT of
// one line and unreliably reproduces its indentation, and indentation is not
// what the assertion is for. It exists to catch a wrong line NUMBER.
//
// The error carries a numbered window around the range, and says where the
// expected text really is. history is what is known about why the numbers are
// wrong (lineNumberHistory); without it the error claims no cause, because
// numbers can be wrong in a file that has not changed since the model read it.
func lineAssertionMismatch(expected, actual string, lineNum int, path string, fileLines []string, history string) string {
	if strings.TrimSpace(expected) == strings.TrimSpace(actual) {
		return ""
	}
	if strings.TrimSpace(expected) == "" {
		return fmt.Sprintf("replace_lines: expected text for line %d is empty. Send the text of that line "+
			"(without the \"N<tab>\" prefix) so an off-by-one cannot apply silently.", lineNum)
	}
	var window strings.Builder
	lo, hi := lineNum-3, lineNum+3
	if lo < 1 {
		lo = 1
	}
	if hi > len(fileLines) {
		hi = len(fileLines)
	}
	for i := lo; i <= hi; i++ {
		marker := " "
		if i == lineNum {
			marker = ">"
		}
		window.WriteString(fmt.Sprintf("%s %d\t%s\n", marker, i, fileLines[i-1]))
	}
	if history == "" {
		history = "Those line numbers do not match the file."
	}
	return fmt.Sprintf("replace_lines: line %d of %s is not what you expected, so the range is wrong and was NOT applied.\n"+
		"  you said: %s\n  actually: %s\n%s %s Current lines around %d:\n%s"+
		"Re-read the file if you need more context, then send the range that matches.",
		lineNum, path, truncateStr(strings.TrimSpace(expected), 120), truncateStr(strings.TrimSpace(actual), 120),
		history, whereTextIs(fileLines, expected), lineNum, window.String())
}

// whereTextIs says which lines hold want, whitespace-insensitively: none, one,
// or several to choose from.
func whereTextIs(fileLines []string, want string) string {
	want = strings.TrimSpace(want)
	var at []string
	for i, l := range fileLines {
		if strings.TrimSpace(l) == want {
			at = append(at, strconv.Itoa(i+1))
		}
	}
	switch {
	case len(at) == 0:
		return "That text is not in the file."
	case len(at) == 1:
		return "That text is at line " + at[0] + "."
	case len(at) <= 8:
		return "That text is on lines " + strings.Join(at, ", ") + ": send the one you mean."
	default:
		return fmt.Sprintf("That text is on %d lines (%s, ...): send the one you mean.",
			len(at), strings.Join(at[:8], ", "))
	}
}

// lineNumberHistory says why the model's line numbers are wrong, but only when
// the evidence shows it: the session wrote the file, or it changed after the
// last read (stale); or it still equals what the first full read showed (the
// numbers never matched). Otherwise it returns "", and no cause is claimed.
func lineNumberHistory(ctx *AgentContext, path, relPath, current string) string {
	if ctx == nil {
		return ""
	}
	ctx.mu.Lock()
	wrote := ctx.SessionWrites[relPath]
	lastRead, read := ctx.FileReadTimes[path]
	original, seen := ctx.OriginalContent[path]
	ctx.mu.Unlock()
	if wrote {
		return "The file changed since you read it (your own edits move line numbers), so the numbers you used are stale."
	}
	if read {
		if info, err := os.Stat(path); err == nil && info.ModTime().After(lastRead) {
			return "The file changed since you last read it, so the numbers you used are stale."
		}
	}
	if seen && original == current {
		return "The file has not changed since you read it, so these numbers never matched this text."
	}
	return ""
}

// replaceLinesTool is insert_after's rationale extended to REPLACEMENT.
//
// edit_file addresses a region by reproducing its text; on a 9-13 line span
// this model corrupts one token somewhere in it essentially every time
// (food.y -> hood.y, scoreElement -> scorerElement, unshift(( ). read_file
// already prints the line numbers, so the address can be cited instead.
//
// The expected first/last line assertion is not optional. A line range fails
// differently from an anchor: a wrong anchor simply does not match, while a
// wrong line number still splices cleanly and produces plausible corruption.
// One line each is the length regime this model is reliable in.
func replaceLinesTool() *ToolDef {
	return &ToolDef{
		Name:   "replace_lines",
		Effect: ToolEffectDirectMutation,
		Description: "Replace a RANGE OF LINES with new content, addressed by the line numbers read_file printed. " +
			"Use this to CHANGE existing code spanning more than a line or two: cite `start_line` and `end_line` instead of " +
			"reproducing the old text, so a long span cannot go wrong in transcription. Both are 1-based and INCLUSIVE. " +
			"`expected_first_line` / `expected_last_line` are the text of those two lines WITHOUT the \"N<tab>\" prefix; they are " +
			"compared whitespace-insensitively so an off-by-one is caught instead of silently applied. `content` is only the new text. " +
			"Use edit_file for a one-line change, insert_after to ADD without replacing, structural_edit for a whole function or class.",
		InputSchema: ReplaceLinesInput{},
		ReadOnly:    false,
		Destructive: false,
		Execute: func(input json.RawMessage, ctx *AgentContext) (*ToolResult, error) {
			var in ReplaceLinesInput
			if err := json.Unmarshal(input, &in); err != nil {
				return nil, errNoMutation(fmt.Errorf("invalid input: %w", err))
			}
			if strings.TrimSpace(in.Path) == "" {
				return noMutation("replace_lines: path is required. The call is " +
					"replace_lines {\"path\":\"app.py\", \"start_line\":N, \"end_line\":M, " +
					"\"expected_first_line\":<text of line N>, \"expected_last_line\":<text of line M>, " +
					"\"content\":<the new lines>}."), nil
			}
			path := resolveAgentPath(ctx, in.Path)
			if !editViewIsCurrent(ctx, path) {
				return nil, errNoMutation(fmt.Errorf("file not read yet — use read_file first so the line numbers are current: %s", in.Path))
			}
			data, err := readRegularFile(path)
			if err != nil {
				return noMutation(fmt.Sprintf("cannot read %s: %v", in.Path, err)), nil
			}
			original := string(data)
			return replaceLineRange(ctx, in, path, original)
		},
	}
}

// replaceLineRange replaces in.StartLine to in.EndLine with the new lines and
// sends the result through the edit pipeline. A range that moved is applied
// where its lines are now; a range out of bounds, too long, or whose first or
// last line is not what the call says, is refused.
func replaceLineRange(ctx *AgentContext, in ReplaceLinesInput, path, original string) (*ToolResult, error) {
	fileLines := strings.Split(original, "\n")
	limit := len(fileLines)
	if limit > 0 && fileLines[limit-1] == "" {
		limit--
	}
	if in.StartLine < 1 || in.EndLine < in.StartLine || in.EndLine > limit {
		return noMutation(fmt.Sprintf(
			"replace_lines: range %d-%d is invalid for %s, which has %d lines. Both bounds are 1-based and "+
				"inclusive, and start_line must not exceed end_line. Use the numbers read_file showed you.",
			in.StartLine, in.EndLine, in.Path, limit)), nil
	}
	if span := in.EndLine - in.StartLine + 1; span > replaceLinesMaxSpan {
		// A size policy declining a replacement it never formed.
		return noMutation(fmt.Sprintf(
			"replace_lines: %d lines is too large a range (limit %d). A replacement that size is a rewrite rather "+
				"than an edit. Split it into consecutive replace_lines calls of at most %d lines, working from "+
				"the BOTTOM of the file upward so the earlier line numbers stay valid. For a whole function "+
				"or class, structural_edit replaces the node in one call (%s) — but only for real nodes: code "+
				"inside a string literal (a <script> block in an HTML template, say) is one string to the "+
				"host grammar and no selector reaches into it, so there the split is the way.",
			span, replaceLinesMaxSpan, replaceLinesMaxSpan, selectorGuidanceOrOutline(in.Path, original))), nil
	}
	// A stale range that simply moved is relocated rather than
	// refused: the numbers go stale as soon as an earlier edit
	// changes the file's length, and the model cannot see that.
	if strings.TrimSpace(in.ExpectedFirstLine) != "" &&
		strings.TrimSpace(fileLines[in.StartLine-1]) != strings.TrimSpace(in.ExpectedFirstLine) {
		span := in.EndLine - in.StartLine + 1
		if moved := relocateStaleRange(fileLines, limit,
			in.ExpectedFirstLine, in.ExpectedLastLine, span); moved > 0 {
			log.Printf("[replace_lines] %s: range %d-%d is stale; the same block is at %d-%d — relocating",
				in.Path, in.StartLine, in.EndLine, moved, moved+span-1)
			in.StartLine, in.EndLine = moved, moved+span-1
		}
	}
	// The anchors did not match, so no replacement was ever formed.
	history := lineNumberHistory(ctx, path, in.Path, original)
	if msg := lineAssertionMismatch(in.ExpectedFirstLine, fileLines[in.StartLine-1], in.StartLine, in.Path, fileLines, history); msg != "" {
		return noMutation(msg), nil
	}
	if msg := lineAssertionMismatch(in.ExpectedLastLine, fileLines[in.EndLine-1], in.EndLine, in.Path, fileLines, history); msg != "" {
		return noMutation(msg), nil
	}

	replacement := strings.Split(strings.TrimSuffix(in.Content, "\n"), "\n")
	merged := append([]string{}, fileLines[:in.StartLine-1]...)
	merged = append(merged, replacement...)
	merged = append(merged, fileLines[in.EndLine:]...)
	updated := strings.Join(merged, "\n")

	if updated == original {
		return noMutation("replace_lines: the replacement is identical to what is already on " +
			"those lines — nothing changed and the bug is still there."), nil
	}

	// Same gate chain as insert_after and edit_file, healthy->broken
	// only so a file mid-repair stays editable.
	// Every content edit goes through the pipeline — these two were
	// producing one greedy sample with no candidate generation and no
	// lens scoring, which is exactly what the tier system exists to
	// prevent.
	route := runEditPipeline(ctx, "replace_lines", path, in.Path, original, updated)
	if route.Cancelled != nil {
		return route.Cancelled, nil
	}
	if route.Delivered {
		// The candidate landed through the protected chain. Writing the
		// caller's own edit now would undo a delivery that settlement
		// and restoration already decided about.
		return attachV3(route.Result, route.Meta), nil
	}
	v3Out := route.Meta
	updated = route.Content

	return writeLineReplacement(ctx, in, path, original, updated, replacement, v3Out)
}

// writeLineReplacement runs the gates every content edit shares over the bytes
// about to land, writes them, and reports the lines it replaced.
func writeLineReplacement(ctx *AgentContext, in ReplaceLinesInput, path, original, updated string,
	replacement []string, v3Out V3EditMetadata) (*ToolResult, error) {
	observed, refusal := editSyntaxObservation(ctx, "replace_lines", in.Path, in.Path,
		original, updated, func(detail string) string {
			return fallbackSyntaxRejection(in.Path, updated, detail)
		})
	if refusal != nil {
		return refusal, nil
	}
	if introduced := editIntroducesUnresolved(ctx, path, original, updated); len(introduced) > 0 {
		return structuralRefusal(structuralRejection(in.Path, introduced)), nil
	}
	if msg := embeddedScriptGate(ctx, path, original, updated); msg != "" {
		return structuralRefusal(msg), nil
	}
	if msg := duplicateMainGuard(path, original, updated); msg != "" {
		return structuralRefusal(msg), nil
	}
	logMandatoryChecks(ctx, "replace_lines", in.Path, "")

	if err := os.WriteFile(path, []byte(updated), 0644); err != nil {
		return nil, editWriteFailure(path,
			fmt.Errorf("cannot write %s: %w", in.Path, err), observed)
	}
	ctx.SessionWrites[in.Path] = true
	ctx.RecordFileRead(path, updated)
	ctx.RecordBodySeen(path)
	replaced := in.EndLine - in.StartLine + 1
	log.Printf("[replace_lines] %s lines %d-%d: %d -> %d lines", logPath(in.Path), in.StartLine, in.EndLine, replaced, len(replacement))
	out, _ := json.Marshal(EditFileOutput{
		OK:          true,
		DiffPreview: fmt.Sprintf("lines %d-%d replaced (%d -> %d lines)", in.StartLine, in.EndLine, replaced, len(replacement)),
	})
	result := &ToolResult{Success: true, Data: out,
		MutationStatus: MutationApplied}
	// The observation describes exactly the bytes just written --
	// the tool's own edit, an authorized V3 candidate, or a baseline
	// restored after revocation, whichever runEditPipeline returned.
	// Provenance is a separate fact, attached by attachV3.
	overlayValidation(result, observed)
	return attachV3(result, v3Out), nil
}

// editSyntaxObservation is the healthy->broken syntax gate the content-edit
// tools share, as one structured observation.
//
// The bytes about to be written are evaluated ONCE, and the original only when
// those bytes demonstrably fail: an edit may not INTRODUCE breakage, but a file
// already failing the checker stays editable, which is what makes
// repair-in-progress possible. Each tool supplies only its own model-facing
// wording.
//
// Returns the observation for the proposed bytes, and a classified refusal when
// the rule declines the edit. The observation describes the exact bytes the
// caller is about to write, so the caller attaches it to whichever outcome the
// filesystem produces rather than re-deriving it.
func editSyntaxObservation(ctx *AgentContext, tool, checkPath, relPath, original, edited string,
	rejection func(detail string) string) (checkOutcome, *ToolResult) {
	proposal := fallbackSyntaxOutcomeFor(ctx, checkPath, edited).aggregate()
	if proposal.Status != ValidationFailed {
		return proposal, nil
	}
	baseline := fallbackSyntaxOutcomeFor(ctx, checkPath, original).aggregate()
	if baselineAllowsRepair(baseline) {
		log.Printf("[%s] %s still unparsable after the edit (was already broken) — allowing repair-in-progress",
			tool, logPath(relPath))
		return proposal, nil
	}
	log.Printf("[%s] edited content for %s failed the syntax gate: %s",
		tool, logPath(relPath), truncateStr(proposal.Detail, 120))
	return proposal, &ToolResult{
		Success:          false,
		Error:            rejection(proposal.Detail),
		MutationStatus:   MutationRefused,
		ValidationKind:   ValidationKindSyntax,
		ValidationStatus: ValidationFailed,
		ValidationDetail: proposal.Detail,
	}
}

// structuralRefusal is the shared shape of the three later gates' verdicts:
// the content parses, and something about its structure does not. Syntax ran
// first and did not fail on these bytes, so the structural failure is the
// decisive one and recording it as syntax/failed would assert the opposite.
// fencedCallIsExecutable answers one question before any generation is spent:
// would this call survive the checks it has to survive anyway?
//
// The fenced channel exists to carry a file body around the JSON encoding, and
// it costs a full unconstrained generation per attempt. Starting one for a
// call that cannot execute -- no path, a path outside the workspace, a
// deny-listed target -- spends a minute of the session budget to produce
// content that is then thrown away, and a model that keeps re-sending the
// malformed call spends the whole session that way.
//
// It runs the SAME checks executeToolCall runs, by calling them, so the set of
// calls refused here is a subset of the set refused there. That is what lets
// the caller simply decline to resolve and hand the call to the tool, whose
// own producer owns the refusal, its wording and its MutationNone.
func fencedCallIsExecutable(name string, args json.RawMessage, ctx *AgentContext) (bool, string) {
	trimmed := strings.TrimSpace(string(args))
	if trimmed == "" || trimmed == "null" {
		return false, "the call carried no arguments"
	}
	var in WriteFileInput
	if err := json.Unmarshal(args, &in); err != nil {
		return false, "the arguments are not the shape " + name + " takes"
	}
	if strings.TrimSpace(in.Path) == "" {
		return false, "the call names no path"
	}
	if reason := validateToolWorkspacePaths(name, args, ctx); reason != "" {
		return false, reason
	}
	if denied, reason := shouldDenyToolCall(name, args); denied {
		return false, reason
	}
	return true, ""
}

func structuralRefusal(msg string) *ToolResult {
	return &ToolResult{
		Success:          false,
		Error:            msg,
		MutationStatus:   MutationRefused,
		ValidationKind:   ValidationKindStructural,
		ValidationStatus: ValidationFailed,
		ValidationDetail: msg,
	}
}

// editWriteFailure keeps an edit tool's real error while stating both facts:
// the mutation did not establish the intended state, and the observation made
// on those exact bytes still holds.
func editWriteFailure(path string, err error, observed checkOutcome) error {
	return overlayValidationOnError(failedMutation(path, err), observed)
}

// lineSpanOf names the lines of content where text first occurs, as
// "line N" or "lines N-M".
func lineSpanOf(content, text string) string {
	i := strings.Index(content, text)
	if i < 0 {
		return "the file"
	}
	first := strings.Count(content[:i], "\n") + 1
	last := first + strings.Count(strings.TrimSuffix(text, "\n"), "\n")
	if strings.HasPrefix(text, "\n") {
		first++
	}
	if last <= first {
		return fmt.Sprintf("line %d", first)
	}
	return fmt.Sprintf("lines %d-%d", first, last)
}

// prefixedEditRefusal explains an edit_file whose old_str matches the file only
// once read_file's "N<tab>" display prefixes are removed, and gives a
// replace_lines call that makes the change without reproducing the text. It
// computes, never applies: nothing in it is executed.
func prefixedEditRefusal(path, content, match, oldStr, newStr string) string {
	i := strings.Index(content, match)
	lines := strings.Split(content, "\n")
	body := strings.Trim(match, "\n")
	first := strings.Count(content[:i], "\n") + 1
	if strings.HasPrefix(match, "\n") {
		first++
	}
	last := first + strings.Count(body, "\n")
	var sb strings.Builder
	fmt.Fprintf(&sb, "edit_file was NOT applied — %s is unchanged. Your old_str carries read_file's line-number "+
		"prefix (\"12<tab>\") on %d line(s); that prefix is display only and is not in the file, and edits apply "+
		"old_str and new_str exactly as sent.", path, lineNumberPrefixedLines(oldStr))
	if n := strings.Count(content, match); n > 1 {
		fmt.Fprintf(&sb, " Without the prefixes it matches %d places, so it does not identify one; anchor on a line "+
			"that appears once.", n)
		return sb.String()
	}
	fmt.Fprintf(&sb, " Without the prefixes it matches %s of %s.", lineSpanOf(content, match), path)
	if m := lineNumberPrefixedLines(newStr); m > 0 {
		fmt.Fprintf(&sb, " Your new_str also has the prefix on %d line(s), and it would be written into the file "+
			"literally.", m)
	}
	if first >= 1 && last <= len(lines) {
		fmt.Fprintf(&sb, " To make this change, either resend edit_file with the prefixes removed from old_str and "+
			"new_str, or call replace_lines with path %q, start_line %d, end_line %d, expected_first_line %q, "+
			"expected_last_line %q, and content set to the replacement lines without prefixes.",
			path, first, last, strings.TrimSpace(lines[first-1]), strings.TrimSpace(lines[last-1]))
	}
	return sb.String()
}

// currentSpanNote says where a span sits in the file as it is now, so a retry
// that needs line numbers can be written without re-reading first. It reports
// only what the file contains; when the text is not there at all, it says that
// instead of inventing a location.
func currentSpanNote(path, content, text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	i := strings.Index(content, text)
	if i < 0 {
		return fmt.Sprintf(" That text is not in %s as it stands now, so any line numbers you are holding are "+
			"stale — read it again before the next edit.", filepath.Base(path))
	}
	first := strings.Count(content[:i], "\n") + 1
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	last := first + len(lines) - 1
	note := fmt.Sprintf(" In %s as it stands now that text is %s", filepath.Base(path),
		lineSpanOf(content, text))
	if len(lines) > 0 {
		note += fmt.Sprintf(", so the replacement call is replace_lines with start_line %d, end_line %d, "+
			"expected_first_line %q and expected_last_line %q.",
			first, last, strings.TrimSpace(lines[0]), strings.TrimSpace(lines[len(lines)-1]))
	} else {
		note += "."
	}
	return note
}

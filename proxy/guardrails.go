// Guardrails for the agent loop. Centralises the checks that bounce
// model output before it touches disk or the host filesystem.
//
// Why a separate file: the rules accumulate (output sanitisation,
// shell-op blocking, protected paths) and live downstream of multiple
// tool handlers. Keeping them together makes the policy auditable —
// reviewers don't have to chase three call sites to know what we
// reject.
//
// Background: ATLAS runs against compact local coding models that are
// weaker than the API frontier models. Claude-Code-style "trust the
// model + permission prompts" doesn't hold for us; the model will
// reliably emit markdown-fenced code with prose preamble and reach
// for shell `mv`/`rm` against source files mid-task. Server-side
// gates are how we keep the workspace usable.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	first, last := -1, -1
	for i, l := range lines {
		if strings.TrimSpace(l) != "" {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
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
			return content, false
		}
	}
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
				return content, false
			}
		}
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

// run_command executes inside the sandbox container, which is already a
// project-folder jail: read-only rootfs, no-new-privileges, ONLY the project
// dir bind-mounted writable at /workspace, and the /shell endpoint forces cwd
// under /workspace. So the model cannot touch the host — the blast radius of
// any shell command is the project folder (recoverable via git). Given that,
// the old "block every mutating verb" policy was overbroad: it made the model
// reinvent mv/cp/rm as bespoke tools and loop when it couldn't (e.g. "mv
// index.html templates/" refused → mkdir loop → stuck). Policy now (2026-06):
// allow shell to manage files freely; block ONLY the few commands that are
// catastrophic even inside the jail — wiping the whole project, fork-bombing
// the sandbox, or destroying a block device. Content edits are still nudged
// toward write_file/edit_file by the system prompt (that's where V3 + the lens
// add value), but they are no longer hard-refused at the shell.

// shellFindDeleteRe catches `find ... -delete` / `find ... -exec rm` — a
// recursive delete whose target is usually `.` (the project root), so its
// blast radius is the whole workspace. Kept blocked; targeted deletes use
// `rm <file>` or delete_file.
var shellFindDeleteRe = regexp.MustCompile(
	`\bfind\b.*?(-delete\b|-exec\s+rm\b)`)

// shellForkBombRe matches the classic fork bomb and close variants: a function
// whose body pipes to itself and backgrounds (`| … &`) then invokes itself.
// The `&` (background spawn) inside the braces is the signature that separates
// a bomb from a benign `f() { ls | grep x; }`.
var shellForkBombRe = regexp.MustCompile(`\(\)\s*\{[^}]*\|[^}]*&[^}]*\}\s*;`)

// shellDeviceWriteRe matches a redirect straight onto a block device. The
// commands that format or overwrite a device (mkfs, wipefs, dd of=/dev/...)
// are matched at a command position instead (catastrophicCommand), so a
// command that only mentions one, like `grep mkfs notes.txt`, is allowed.
var shellDeviceWriteRe = regexp.MustCompile(`(^|\s)>\s*/dev/(sd|nvme|mmcblk|vd|hd|xvd)`)

// validateShellCommand rejects catastrophic commands (whole-project wipe,
// fork bomb, device destruction) and execution wrappers that cannot be inspected
// within the quoting and depth limits. Everything else — mv, cp, mkdir, rm of specific
// files, chmod, sed -i, > redirects, build/test/run — is allowed.
//
// It is the one command policy: every tool that runs a command goes through
// it, before dispatch (shouldDenyToolCall) and in the tool itself. There were
// two, and they disagreed: run_command's deny-list saw through `env rm -rf /`
// and `(rm -rf /)` and run_background's check did not.
func validateShellCommand(cmd string) string {
	return inspectShellCommand(cmd)
}

// commandSegments splits a command line where a new command can start: at
// `;`, `|`, `&`, newlines, a subshell's parentheses, and command
// substitution. Single quotes hide everything; double quotes hide the
// operators but not `$(...)` or backticks, which the shell still runs.
func commandSegments(cmd string) []string {
	var out []string
	var cur strings.Builder
	cut := func() {
		out = append(out, cur.String())
		cur.Reset()
	}
	inSingle, inDouble := false, false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case c == '\\' && !inSingle && i+1 < len(cmd):
			cur.WriteByte(c)
			cur.WriteByte(cmd[i+1])
			i++
			continue
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case inSingle:
		case c == '`' || c == ')' || (c == '$' && i+1 < len(cmd) && cmd[i+1] == '('):
			cut()
			if c == '$' {
				i++ // the `(` of `$(`
			}
			continue
		case inDouble:
		case c == ';' || c == '|' || c == '&' || c == '\n' || c == '(':
			cut()
			continue
		}
		cur.WriteByte(c)
	}
	cut()
	return out
}

// catastrophicCommand checks the command one segment runs.
func catastrophicCommand(seg string) string {
	fields := strings.Fields(seg)
	i := commandPosition(fields)
	if i >= len(fields) {
		return ""
	}
	head, args := filepath.Base(fields[i]), fields[i+1:]
	switch {
	case head == "rm":
		return catastrophicRm(args)
	case head == "wipefs" || head == "mkfs" || strings.HasPrefix(head, "mkfs."):
		return "refused: formatting or wiping a filesystem (mkfs/wipefs) is blocked. Work with files under the project directory instead."
	case head == "dd":
		for _, a := range args {
			if strings.HasPrefix(a, "of=/dev/") {
				return "refused: dd onto a device is blocked. Work with files under the project directory instead."
			}
		}
	case head == "find" && shellFindDeleteRe.MatchString(seg):
		return "refused: `find ... -delete` / `-exec rm` recursively deletes from the search root (usually the whole project). Delete specific files with `rm <file>` or the delete_file tool."
	}
	return ""
}

// catastrophicRm flags a recursive `rm` whose target would wipe the whole
// project (or root / home). A targeted recursive delete of a subdirectory
// (`rm -rf __pycache__`, `rm -rf node_modules`, `rm -rf build`) is allowed —
// only roots and glob-everything targets are catastrophic.
func catastrophicRm(args []string) string {
	recursive := false
	var targets []string
	for _, f := range args {
		if strings.HasPrefix(f, "--") {
			if f == "--recursive" {
				recursive = true
			}
			continue
		}
		if strings.HasPrefix(f, "-") {
			if strings.ContainsAny(f, "rR") {
				recursive = true
			}
			continue
		}
		targets = append(targets, f)
	}
	if !recursive {
		return "" // `rm file` / `rm -f file` is fine; only recursive wipes are gated
	}
	for _, t := range targets {
		if isCatastrophicDeleteTarget(t) {
			return "refused: `rm -r` of " + t + " would wipe the whole project (or root). Delete a specific subdirectory by name instead (e.g. `rm -rf build`), or use delete_file."
		}
	}
	return ""
}

// isCatastrophicDeleteTarget reports whether a recursive-rm target is a root /
// home / project-root / glob-everything path.
func isCatastrophicDeleteTarget(t string) bool {
	t = strings.Trim(t, `"'`)
	switch t {
	case "/", "/*", "~", "~/", "~/*", "$HOME", "${HOME}", "$HOME/*", "${HOME}/*",
		".", "./", "./*", "*", "..", "../", "../*",
		"/workspace", "/workspace/", "/workspace/*":
		return true
	}
	return false
}

// workspaceRefRe matches `/workspace` as a path component (preceded by
// non-word char or line start, followed by /, whitespace, end, or
// non-word char). Avoids false matches inside e.g. `/home/foo_workspace`.
var workspaceRefRe = regexp.MustCompile(`(^|[^a-zA-Z0-9_])/workspace(/|\s|$|[^a-zA-Z0-9_])`)

// validateWorkingDirReference rejects shell commands that reference
// `/workspace` when /workspace is not the project's working directory.
//
// Coding models often have a training-data prior toward `/workspace` as a
// generic project sandbox path — coding-assistant fine-tunes use it
// heavily. The system prompt explicitly warns against absolute paths
// but the prior leaks through under conversation pressure. May 8 2026
// flask test: model emitted a correct `cd /home/isaac/snake && python
// app.py` at turn 7, then drifted at turn 9 to `cd /workspace && python
// app.py` and burned three turns retrying that wrong path. This guard
// catches the drift one turn earlier with a rejection that names the
// actual workingDir, so the model can self-correct in one round-trip.
//
// Returns "" if (a) workingDir is empty, (b) cmd doesn't reference
// /workspace, (c) the actual project IS at /workspace (no false reject),
// or (d) the /workspace mention is a substring of an unrelated path
// (`/home/foo_workspace`). Otherwise returns a rejection string.
func validateWorkingDirReference(cmd, workingDir string) string {
	if workingDir == "" {
		return ""
	}
	if !strings.Contains(cmd, "/workspace") {
		return ""
	}
	if workingDir == "/workspace" || strings.HasPrefix(workingDir, "/workspace/") {
		return ""
	}
	if !workspaceRefRe.MatchString(cmd) {
		return ""
	}
	return fmt.Sprintf(
		"command refused: references /workspace, which is not your project root. Working directory is %s — `cd %s && ...` for shell commands, or use relative paths from there. /workspace is a generic training-data prior, not this project's path.",
		workingDir, workingDir)
}

// validateRunCommand chains the shell-mutation gate and the workingDir
// gate. Used by both run_command and run_background paths in the agent
// loop. Empty return = command is allowed.
//
// It does not refuse a server start. run_command gets that check in every
// mode, source-aware, before this one (foregroundServerRejectionWithSource),
// and run_background is where a server belongs: refusing it here, in the
// default and accept-edits modes, told the model to use run_background and
// then refused run_background with the same instruction.
func validateRunCommand(cmd, workingDir string) string {
	if r := validateShellCommand(cmd); r != "" {
		return r
	}
	if r := validateWorkingDirReference(cmd, workingDir); r != "" {
		return r
	}
	return ""
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

// fixIntentWords tracks vocabulary that signals "the user wants
// something repaired or verified." Reused by the verification gate
// to decide when "done" needs a build/test/run before it passes.
// Kept in sync with classifyAgentTier's fix-intent list.
var fixIntentWords = []string{
	"fix", "broken", "doesn't work", "doesn't", "does not work", "does not",
	"not working", "isn't working", "isn't", "is not", "aren't", "wasn't",
	"didn't", "won't", "can't", "bug", "issue", "problem", "error",
	"failed", "fails", "failing", "incorrect", "wrong", "verify",
	"render", "renders", "rendering", "load", "loads", "loading",
}

// isFixIntentMessage returns true when the user prompt looks like a
// repair/verification request. The verification gate uses this to
// decide whether `done` requires a real verification step. Pure
// feature requests ("add a logout button") don't trip the gate —
// adding code doesn't always need a curl/test to declare done.
// promisesMoreContent reports an answer that ends by promising content it
// never delivers — "I will now provide the specific location", "let me give
// you the exact comparison".
//
// Distinct from announcesImminentToolUse: that one catches announcing a TOOL
// call before any work has happened. This catches a reply that has done the
// work, then signs off promising the actual answer. Observed on a bug-find
// task: the model named the file, described the symptom, and ended with "I
// will now provide the specific location and the incorrect comparison as
// requested" — and the turn ended there, leaving the user a half-answer.
//
// Requires the promise to be at the END, because "I'll explain why below"
// followed by the explanation is fine.
func promisesMoreContent(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	for _, phrase := range []string{
		"i will now provide", "i'll now provide", "i will now give",
		"i'll now give", "i will provide the", "i'll provide the",
		"let me provide the", "let me give you the", "i will now show",
		"i'll now show", "here is what i will", "i will now list",
	} {
		at := strings.LastIndex(lower, phrase)
		if at < 0 {
			continue
		}
		// A promise that is FOLLOWED by the thing promised is fine —
		// "I'll provide the details: line 314 uses > where it should use <"
		// delivers in the same breath. What is broken is a promise with
		// nothing concrete after it. Digits, operators and backticks are the
		// cheap signal for "concrete", and the remaining prose after an
		// undelivered promise ("...as requested.") has none of them.
		rest := lower[at+len(phrase):]
		return !strings.ContainsAny(rest, "0123456789`=<>+*/(){}[]")
	}
	return false
}

// announcesImminentToolUse reports first-person narration of a tool call the
// model is about to make — "I need to read X", "let me look at Y", "I'll
// start by outlining".
//
// A `text` reply ends the turn, so a model that announces instead of acting
// stops with the right intent and no action. Deliberately narrow: it needs a
// first-person subject AND an action verb aimed at inspecting the workspace,
// so an ANSWER that merely mentions reading ("this function reads the file")
// does not match.
func announcesImminentToolUse(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}
	subjects := []string{"i need to ", "i'll ", "i will ", "let me ", "i am going to ",
		"i'm going to ", "i should ", "first, i ", "next, i "}
	// "look into" was missing and "look at" was not enough: observed on a
	// fresh workspace, "How does the contact form work?" was answered with
	// "I'll look into the contact form's implementation..." and nothing else.
	// `text` is a terminal exit, so an announcement that slips this check ends
	// the turn and the user gets a promise instead of an answer.
	verbs := []string{"read", "look at", "look into", "look through", "look over",
		"open", "inspect", "examine", "outline", "check", "search", "list",
		"start by", "investigate", "dig into", "trace through", "review the",
		"take a look"}
	for _, sub := range subjects {
		at := strings.Index(lower, sub)
		if at < 0 {
			continue
		}
		// Look only just past the subject: "I need to read" matches, while
		// "I need to explain why the code reads a file" does not.
		window := lower[at:]
		if len(window) > 80 {
			window = window[:80]
		}
		for _, v := range verbs {
			if strings.Contains(window, v) {
				return true
			}
		}
	}
	return false
}

// replyClosing returns the part of a reply that decides whether it is an
// answer: its last paragraph, capped to the final ~320 characters, with any
// sentence that offers optional follow-up to the USER removed.
//
// The closing, not the whole reply, because "I'll start by describing the
// flow" followed by the description is an answer, while the same words as
// the last thing said are a promise. Offers are removed because "If you'd
// like, I can fix it" and "let me know if you want me to look at scoring.py"
// hand the next step to the user; they do not leave the agent's own work
// undone.
func replyClosing(text string) string {
	t := strings.TrimSpace(text)
	if i := strings.LastIndex(t, "\n\n"); i >= 0 && len(strings.TrimSpace(t[i:])) > 0 {
		t = strings.TrimSpace(t[i:])
	}
	if len(t) > 320 {
		t = t[len(t)-320:]
	}
	var kept []string
	for _, sentence := range replySentenceRe.FindAllString(t, -1) {
		if replyOfferRe.MatchString(sentence) {
			continue
		}
		kept = append(kept, sentence)
	}
	return strings.ToLower(strings.Join(kept, " "))
}

var (
	replySentenceRe = regexp.MustCompile(`[^.!?\n]+(?:[.!?]+|$)`)
	replyOfferRe    = regexp.MustCompile(`(?i)\blet me know\b|\bif you(?:'d| would)? (?:like|want|prefer)\b|\bwould you like\b|\bwant me to\b|\bshall i\b|\bhappy to\b|\bi can also\b`)
)

// replyDefersWork reports a reply whose closing hands off to work the agent
// has not done: an announced next action ("I will now check the
// generate_plan function"), a result it says is still pending ("Please wait
// while I verify the exact line"), or promised content never given.
//
// A `text` reply ends the session, so a reply that closes this way is not an
// answer -- the work it names will never happen. Measured: every
// bugfind_tiebreak session across four benchmark runs (8 of 8) ended on such
// a closing and was reported completed. announcesImminentToolUse already
// caught the first shape, but only before any tool had run, which is exactly
// when a model that has read one file and means to read another is not.
func replyDefersWork(text string) bool {
	closing := replyClosing(text)
	if closing == "" {
		return false
	}
	for _, wait := range []string{"please wait", "one moment", "hold on", "stand by", "bear with me"} {
		if strings.Contains(closing, wait) {
			return true
		}
	}
	for _, pending := range []string{"once i verify", "once i confirm", "once i check",
		"after i verify", "after i confirm", "after i check", "to confirm the exact",
		"to verify the exact"} {
		if strings.Contains(closing, pending) {
			return true
		}
	}
	subjects := []string{"i will ", "i'll ", "let me ", "i need to ", "i'm going to ",
		"i am going to ", "next, i ", "now i ", "i am now ", "i'm now ", "which i will "}
	verbs := []string{"read", "look at", "look into", "open", "inspect", "examine", "outline",
		"check", "search", "investigate", "dig into", "trace", "review the", "take a look",
		"verify", "confirm", "run ", "test", "locate", "find ", "determine", "analyze",
		"analyse", "compare", "debug", "reproduce"}
	for _, sub := range subjects {
		for from := 0; ; {
			at := strings.Index(closing[from:], sub)
			if at < 0 {
				break
			}
			at += from
			window := closing[at+len(sub):]
			if len(window) > 60 {
				window = window[:60]
			}
			window = trimReplyLeadIns(window) // "i will try to read" defers as "i will read" does
			for _, v := range verbs {
				if strings.HasPrefix(window, v) {
					return true
				}
			}
			from = at + len(sub)
		}
	}
	return promisesMoreContent(text)
}

// replyAsksUser reports a reply whose last sentence asks the USER to supply
// something -- a fact, a choice, a file, where to look -- rather than offering
// optional follow-up or checking in. It says nothing about whether asking was
// right; exitGates decides that from what the run could inspect.
//
// Only the final sentence that is not an offer or a courtesy counts: "Which
// approach is safer? The second, because..." answers its own question, while
// "Could you tell me which files handle the pricing cache?" leaves the turn
// with the user. Measured on c5927b3 acceptance family S: that reply, with no
// tool call in a workspace of five source files, was reported completed.
func replyAsksUser(text string) bool {
	t := strings.TrimSpace(text)
	if i := strings.LastIndex(t, "\n\n"); i >= 0 && len(strings.TrimSpace(t[i:])) > 0 {
		t = strings.TrimSpace(t[i:])
	}
	if len(t) > 400 {
		t = t[len(t)-400:]
	}
	sentences := replySentenceRe.FindAllString(t, -1)
	for k := len(sentences) - 1; k >= 0; k-- {
		sen := strings.ToLower(strings.TrimSpace(sentences[k]))
		if sen == "" {
			continue
		}
		if userRequestRe.MatchString(sen) {
			return true
		}
		if replyOfferRe.MatchString(sen) || replyCourtesyRe.MatchString(sen) {
			continue
		}
		return strings.HasSuffix(sen, "?") && userQuestionRe.MatchString(sen)
	}
	return false
}

var (
	// Asking the user to provide or identify something, in any sentence form.
	userRequestRe = regexp.MustCompile(`\b(?:could|can|would|will) you(?: please)? (?:tell|share|provide|paste|send|upload|show|point|specify|clarify|confirm|describe|give|list|identify|indicate|let me know)\b|\bplease (?:tell|share|provide|paste|send|upload|show|point|specify|clarify|confirm|describe|give|list|identify|indicate)\b|\blet me know (?:which|what|where|how|whether|the)\b`)
	// A direct question about the user's situation or preference.
	userQuestionRe = regexp.MustCompile(`(?:^|[:;,]\s*)(?:which|what|where|how|when|who|do you|did you|have you|are you|were you|is it|is this|is that|should the|should it|can you|could you|would you)\b`)
	// Checking in after an answer: not a request for anything.
	replyCourtesyRe = regexp.MustCompile(`\b(?:does (?:that|this) (?:help|answer|make sense)|makes? sense\?|any (?:other|more|further) questions|anything else|is (?:that|this) what you)\b`)
)

// replyDeclaresInability reports a reply whose closing says, in the first
// person, that the agent could not accomplish what was asked -- "I could not
// determine the cause from these files". That is an honest outcome, and it is
// not a completed answer: the session ends incomplete instead of reporting
// success. Deliberately narrow: an environment limitation stated alongside an
// answer ("I couldn't run the tests, but the fix is ...") names no outcome
// verb and does not match, and third-person descriptions of code ("the
// function is unable to handle an empty list") have no first-person subject.
func replyDeclaresInability(text string) bool {
	closing := replyClosing(text)
	if closing == "" {
		return false
	}
	subjects := []string{"i could not ", "i couldn't ", "i was unable to ", "i am unable to ",
		"i'm unable to ", "i cannot ", "i can't ", "i was not able to ", "i wasn't able to ",
		"i am not able to ", "i'm not able to "}
	outcomes := []string{"determine", "find", "identify", "locate", "complete", "finish",
		"answer", "figure out", "work out", "pin down", "resolve", "tell"}
	for _, sub := range subjects {
		at := strings.Index(closing, sub)
		if at < 0 {
			continue
		}
		window := closing[at+len(sub):]
		if len(window) > 40 {
			window = window[:40]
		}
		for _, o := range outcomes {
			if strings.HasPrefix(window, o) || strings.HasPrefix(window, "fully "+o) ||
				strings.HasPrefix(window, "reliably "+o) {
				return true
			}
		}
	}
	return false
}

// inlineProgramFlagRe matches an interpreter invoked with a program passed
// inline on the command line rather than as a file.
var inlineProgramFlagRe = regexp.MustCompile(`\b(python3?|node|perl|ruby)\s+-(c|e)\b`)

// shellParseFailureSignatures are the messages bash emits when it cannot parse
// the command at all, so nothing ran. Distinct from a command that ran and
// failed: there is no output to reason about and no partial effect to undo.
var shellParseFailureSignatures = []string{
	"syntax error near unexpected token",
	"unexpected EOF while looking for matching",
	"syntax error: unexpected end of file",
}

// shellQuotingHint explains a command the shell could not parse, when the
// cause is a program quoted inline after -c or -e.
//
// Measured across runs 14-16: run_command is the least reliable tool in the
// harness at 86 failures in 156 calls, and 53 of those trace to this one
// shape. Eight commands failed to parse and the model re-sent them 45 times.
// The raw bash error names a token and a column, which says nothing the model
// can act on, so it changed nothing and sent the identical line again. One
// session did this twelve times before writing the code to a file instead,
// which worked first try.
//
// Nesting is why the shape fails rather than any one typo: the outer shell
// quotes, the quotes inside the program, and f-string braces all have to
// agree. The observed commands were not merely mis-shell-quoted, they were
// also invalid Python (`print(f'...{...}")` opens on a quote and closes on a
// double quote), so no amount of re-quoting by the harness could rescue them.
// The fix is to stop nesting: put the program in a file.
func shellQuotingHint(command, errMsg string) string {
	if command == "" || errMsg == "" {
		return ""
	}
	parseFailed := false
	for _, sig := range shellParseFailureSignatures {
		if strings.Contains(errMsg, sig) {
			parseFailed = true
			break
		}
	}
	if !parseFailed || !inlineProgramFlagRe.MatchString(command) {
		return ""
	}
	return "\n\nThe shell could not parse this, so nothing ran and re-sending it " +
		"unchanged fails identically. Quoting a program inline after -c is fragile: " +
		"the shell's quotes, the quotes inside your code, and any braces all have to " +
		"nest correctly. Write the snippet to a file with write_file (say check.py) " +
		"and run it with `python3 check.py`. Then the quoting is only the language's " +
		"problem, and whatever comes back is a real error in the code."
}

// fileSHA256 hashes the current on-disk bytes of a session path, "" when
// the file can't be read.
func fileSHA256(ctx *AgentContext, path string) string {
	data, err := os.ReadFile(resolveAgentPath(ctx, path))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// sessionWriteHashes snapshots the sha256 of every file this session wrote,
// keyed by the path as the model sent it. Taken at the moment a verifying
// run succeeds, it records WHICH bytes that run vouched for.
// changedPathsForCoverage is every path a passing run may cover: the raw
// paths write_file registered as session writes, and the canonical paths of
// every code deliverable the ledger recorded this session -- which is where an
// edit tool's landing is written down.
//
// The verification demand (decideVerificationDemand -> codeDeliverablesFor)
// already asks about the ledger's deliverables. The coverage that answered it
// read only the session-write map, which edit_file and structural_edit never
// wrote to and no edit tool wrote to for a delivered candidate, so a run that
// named an edited file covered nothing and every edit task under task_mode
// work ended verification_demanded_unmet. Reading both sides from the ledger's
// canonical identity closes that asymmetry without touching what the
// session-write map means to its other readers: the overwrite guard's notion
// of the model's own draft, the debug fast path, the manifest note and the V3
// project context are unchanged.
//
// Coverage states only that a run named a path whose current bytes hash to
// this value. It proves nothing about correctness, and the ledger's own
// validation still settles mutation debt.
func changedPathsForCoverage(ctx *AgentContext) []string {
	if ctx == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for p := range ctx.SessionWrites {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, p := range codeDeliverablesFor(ctx, nil) {
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func sessionWriteHashes(ctx *AgentContext) map[string]string {
	if ctx == nil || len(ctx.SessionWrites) == 0 {
		return nil
	}
	out := make(map[string]string, len(ctx.SessionWrites))
	for p := range ctx.SessionWrites {
		if h := fileSHA256(ctx, p); h != "" {
			out[p] = h
		}
	}
	return out
}

// commandNamesPath reports whether the command line names the file as a
// whole token (path, basename, or */basename) in any shell segment. Within
// a command already classified as verification, a named file participated
// in what was verified — passed to an interpreter, a test runner, or a
// grader as an argument. Substring matching is deliberately avoided:
// "solve.py" must not match "solve.py.bak".
//
// Three common shapes named their files without a plain token, so a run that
// verified them bound nothing: a subshell, `(cd app && python main.py)`, whose
// token keeps the parenthesis; a glob, `javac *.java`; and a Java class run,
// `java Main` or `java com.example.Main`, which names the class, not
// Main.java. Shell punctuation is trimmed from tokens, a glob token matches
// the files it would expand to, and after `java` a class name matches its
// source file.
func commandNamesPath(command, path string) bool {
	base := filepath.Base(path)
	class := ""
	if strings.HasSuffix(base, ".java") {
		class = strings.TrimSuffix(base, ".java")
	}
	for _, segment := range splitShellSegments(command) {
		fields := strings.Fields(segment)
		javaRun := false
		for i, tok := range fields {
			tok = strings.Trim(tok, "\"'()`{};")
			if tok == "" {
				continue
			}
			if tok == path || tok == base || strings.HasSuffix(tok, "/"+base) {
				return true
			}
			if strings.ContainsAny(tok, "*?[") && globNamesPath(tok, path) {
				return true
			}
			if i == 0 || (!javaRun && filepath.Base(tok) == "java") {
				javaRun = filepath.Base(tok) == "java"
				continue
			}
			if javaRun && class != "" && !strings.HasPrefix(tok, "-") &&
				(tok == class || strings.HasSuffix(tok, "."+class)) {
				return true
			}
		}
	}
	return false
}

// globNamesPath reports whether a shell glob from a command line would expand
// to path. A changed path is often absolute (the resolved form sits beside
// the one the model sent), and a relative glob is relative to where the
// command ran, so the glob is matched against as many trailing components of
// the path as it has: `src/*.java` against `.../src/Util.java`.
func globNamesPath(glob, path string) bool {
	glob = strings.TrimPrefix(glob, "./")
	if filepath.IsAbs(glob) {
		ok, _ := filepath.Match(glob, path)
		return ok
	}
	parts := strings.Split(filepath.ToSlash(path), "/")
	n := strings.Count(glob, "/") + 1
	if n > len(parts) {
		return false
	}
	ok, _ := filepath.Match(glob, strings.Join(parts[len(parts)-n:], "/"))
	return ok
}

// coverageFromLiveServer credits a probe with the files whose code is actually
// answering it.
//
// The contradiction this resolves, measured in the cycle-6 regression
// (flask_pause rep 2, R3): ATLAS tells a run to start a server with
// run_background and probe it with curl — and then asks for verification
// evidence that NAMES the deliverable. A probe names no file, so the run
// probed a live app, was credited with nothing, and finished
// verification_demanded_unmet. No command can both start a blocking server and
// return, so the obligation was unreachable by the route ATLAS prescribes.
//
// A probe is credited with the files named by a background job that is still
// running AND was started after those files were last written: that process is
// running the bytes on disk, and it answered. A job that predates the last
// edit is serving older code and is credited with nothing — which is the same
// rule the artifact gate applies to files that change after a verification.
func coverageFromLiveServer(ctx *AgentContext, command string) map[string]string {
	covered := map[string]string{}
	if ctx == nil || len(ctx.BackgroundJobs) == 0 || !isHTTPProbe(command) {
		return covered
	}
	for id, jobCmd := range ctx.BackgroundJobs {
		started, ok := ctx.BackgroundJobStarted[id]
		if !ok {
			continue
		}
		for _, p := range changedPathsForCoverage(ctx) {
			if _, already := covered[p]; already || !commandNamesPath(jobCmd, p) {
				continue
			}
			info, err := os.Stat(p)
			if err != nil || info.ModTime().After(started) {
				continue // the file changed after this process started
			}
			if h := fileSHA256(ctx, p); h != "" {
				covered[p] = h
			}
		}
	}
	return covered
}

// isHTTPProbe matches a request made against a local service — the shape the
// verification rejection prescribes for a server.
func isHTTPProbe(command string) bool {
	c := strings.ToLower(strings.TrimSpace(command))
	if !strings.Contains(c, "curl") && !strings.Contains(c, "wget") && !strings.Contains(c, "http") {
		return false
	}
	return strings.Contains(c, "localhost") || strings.Contains(c, "127.0.0.1") || strings.Contains(c, "0.0.0.0")
}

// staleServingJob names a running job that was started before a file the run
// has since changed, with that file: probing it would exercise older code.
func staleServingJob(ctx *AgentContext) (jobID, path string) {
	if ctx == nil {
		return "", ""
	}
	for id, jobCmd := range ctx.BackgroundJobs {
		started, ok := ctx.BackgroundJobStarted[id]
		if !ok {
			continue
		}
		for _, p := range changedPathsForCoverage(ctx) {
			if !commandNamesPath(jobCmd, p) {
				continue
			}
			if info, err := os.Stat(p); err == nil && info.ModTime().After(started) {
				return id, filepath.Base(p)
			}
		}
	}
	return "", ""
}

// coverageForGreenCommand binds a green run to the files it exercised.
//
// Direct naming is the floor: a command that typed a path ran that path. But
// the standard way to verify a change is to run something that USES it -- a
// test, a driver, a reproduction script -- and coverage that stops at the
// command line cannot see through that. Measured: three tasks wrote a test
// beside the fix and ran it green (`python3 test_stats.py` printing
// "median() passed"), then were told nothing in the run verified the code,
// because the command named the test and not stats.py. Those runs had done
// exactly the right thing and were failed for it.
//
// So coverage follows the entry point into the workspace files it references.
// The inference is the same strength as the direct case, not weaker: if
// test_stats.py imports stats and the process exited 0, then stats.py was
// located, parsed and its module body executed -- which is precisely, and
// only, what `python3 stats.py` proves. Neither shows a particular function
// is correct; that was never this gate's claim. Code that nothing which ran
// refers to stays uncovered. A red run binds nothing: its record, marked
// Failed, only takes back an earlier pass over the same bytes.
func coverageForGreenCommand(ctx *AgentContext, command string) map[string]string {
	covered := coverageFromLiveServer(ctx, command)
	candidates := changedPathsForCoverage(ctx)
	var entries []string
	for _, p := range candidates {
		if commandNamesPath(command, p) {
			if h := fileSHA256(ctx, p); h != "" {
				covered[p] = h
				entries = append(entries, p)
			}
		}
	}
	// A runner that names no file still ran the ones it discovers: a bare
	// `pytest` imports every test file, and `go test ./...` builds and tests
	// every package with tests. Without this, a work request verified only by
	// a bare `pytest` could never meet its contract: nothing named the module
	// or its tests.
	if ctx != nil {
		for _, p := range runnerEntries(command, ctx.WorkingDir, candidates) {
			if _, already := covered[p]; already {
				continue
			}
			if h := fileSHA256(ctx, p); h != "" {
				covered[p] = h
				entries = append(entries, p)
			}
		}
	}
	// Breadth-first, bounded by the candidate set: each file is admitted at
	// most once, so an import cycle terminates and nothing outside the set of
	// files this session already tracks can be pulled in.
	for i := 0; i < len(entries); i++ {
		src, err := os.ReadFile(entries[i])
		if err != nil {
			continue
		}
		for _, q := range candidates {
			if _, already := covered[q]; already {
				continue
			}
			if !sourceReferencesPath(string(src), q) {
				continue
			}
			if h := fileSHA256(ctx, q); h != "" {
				covered[q] = h
				entries = append(entries, q)
			}
		}
	}
	return covered
}

// sourceReferencesPath reports whether running src would load q: it names the
// file outright, or imports it by module name. Import context is required for
// the bare module name so that a file merely mentioning the word "stats" does
// not claim to have exercised stats.py.
func sourceReferencesPath(src, q string) bool {
	base := filepath.Base(q)
	if base == "" || base == "." {
		return false
	}
	if strings.Contains(src, base) {
		return true
	}
	module := strings.TrimSuffix(base, filepath.Ext(base))
	if module == "" || module == base {
		return false
	}
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "import ") && !strings.HasPrefix(trimmed, "from ") &&
			!strings.Contains(trimmed, "require(") && !strings.Contains(trimmed, "import(") &&
			!strings.Contains(trimmed, "import_module") {
			continue
		}
		if referencesModuleWord(trimmed, module) {
			return true
		}
	}
	return false
}

// referencesModuleWord finds module as a whole identifier, so "stats" does not
// match "statsd" or "my_stats".
func referencesModuleWord(line, module string) bool {
	for i := 0; ; {
		j := strings.Index(line[i:], module)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(module)
		beforeOK := start == 0 || !isIdentByte(line[start-1])
		afterOK := end == len(line) || !isIdentByte(line[end])
		if beforeOK && afterOK {
			return true
		}
		i = start + 1
	}
}

func isIdentByte(b byte) bool {
	return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// --- work-contract verification demand ---------------------------------------
//
// verificationDemandedAndUnmet asks one session-wide question: did anything
// pass. It cannot say WHAT was verified or at WHICH bytes, so `echo ok` clears
// it and a rewrite after a green run does not re-arm it. The evidence that can
// answer already exists -- ctx.VerificationEvidence records, per green command,
// the sha256 of each file the command actually NAMED -- and until now its only
// consumer was lens labelling. A completion decision is exactly the place that
// evidence was built for.
//
// Scope is deliberately narrow: a client that declared task_mode work. Nothing
// here touches contractless callers, questions, documents, deletion, moves,
// debt, hazards, tombstones, permission or timeouts.

type verificationDemand struct {
	Required bool
	Met      bool
	// Missing names the first deliverable that has no current, relevant,
	// green evidence, or the first declared command that did not run against
	// the final bytes. Empty when Met.
	Missing string
	// MissingCommand says Missing is a declared command rather than a path.
	MissingCommand bool
}

// codeDeliverablesFor is the set this demand covers: paths the client declared
// plus paths the session wrote, restricted to extensions the syntax gate knows.
// Documents keep the existing exact-current-hash rule and are not included.
func codeDeliverablesFor(ctx *AgentContext, expected []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(rel string) {
		if rel == "" {
			return
		}
		resolved := resolveAgentPath(ctx, rel)
		// Executability, not registry membership: a static or declarative
		// artifact has a checker but nothing to run, and demanding an
		// execution that names it is an obligation nothing can discharge.
		// Its bytes are still held to the existing validation contract.
		if meta, gated := syntaxGateLanguages[strings.ToLower(filepath.Ext(resolved))]; !gated ||
			!meta.Executable {
			return
		}
		if seen[resolved] {
			return
		}
		seen[resolved] = true
		out = append(out, resolved)
	}
	for _, rel := range expected {
		add(rel)
	}
	if ctx != nil {
		ctx.LedgerMu.Lock()
		for key, d := range ctx.Ledger {
			if d.Tombstoned || d.Generation == 0 {
				continue
			}
			add(key)
		}
		ctx.LedgerMu.Unlock()
	}
	sort.Strings(out)
	return out
}

// evidenceIsCurrent reports whether a green record still describes the bytes on
// disk for every path it covered. A later mutation to any covered path makes
// the record stale, which is the verify-then-modify hole stated as a rule.
// The record is keyed by the path the model wrote, the deliverable set by the
// path the client declared or the ledger owns. Both are put through
// resolveAgentPath -- the one canonicalisation rule -- so "solve.py" and
// "./solve.py" are the same file without a second normalizer.
func evidenceIsCurrent(ctx *AgentContext, rec VerificationRecord) (map[string]string, bool) {
	if len(rec.Covered) == 0 {
		return nil, false
	}
	out := make(map[string]string, len(rec.Covered))
	for p, h := range rec.Covered {
		if fileSHA256(ctx, p) != h {
			return nil, false
		}
		out[resolveAgentPath(ctx, p)] = h
	}
	return out, true
}

// contractRequiresCommand reports whether the client declared this exact
// command. isVerificationCommand recognises builds, tests, probes and runners
// by shape, and a client's own requirement may be none of those -- a linter, a
// schema check, a project script. Without this, a declared command could be
// impossible to satisfy: the demand would require it and nothing would ever
// record it running.
//
// It changes what gets RECORDED, never how strongly the record counts. The
// evidence is the same execution, bound to the same hashes; declaring a command
// says who required it, not that its passing proves more.
func contractRequiresCommand(ctx *AgentContext, command string) bool {
	if ctx == nil || ctx.TaskContract == nil {
		return false
	}
	for _, want := range ctx.TaskContract.VerificationCommands() {
		if want == command {
			return true
		}
	}
	return false
}

// decideVerificationDemand is the single owner of the work-contract demand.
//
// Fails closed everywhere: no evidence, stale evidence, evidence that names a
// different path, or a declared command that never ran against the final bytes
// all leave the demand unmet. A successful command that names nothing covers
// nothing, so `true` and `echo ok` cannot satisfy it -- not because they are
// recognised, but because they carry no binding.
func decideVerificationDemand(ctx *AgentContext, tc *TaskContract, expected []string) verificationDemand {
	if ctx == nil || tc == nil || tc.TaskMode != TaskModeWork {
		return verificationDemand{}
	}
	// Which commands this run owes, and on whose authority. A caller that
	// declared its verification is the authority on that list: the declared
	// commands are the obligation, and the legacy reading of the same field
	// applies only when nothing was declared.
	obligation := resolveVerificationObligation(ctx)
	// What the run actually wrote demands its own evidence. That demand is
	// read off the workspace rather than off the request, so declaring
	// verification knowledge -- of any shape, including none -- narrows the
	// command list and never switches this off. Code the run wrote and never
	// showed to run stays unverified whatever the caller declared.
	paths := codeDeliverablesFor(ctx, expected)
	// A declared command is a requirement in its own right. It survives when
	// the run produced nothing executable -- a client that asked for
	// `htmlhint index.html` asked for it regardless of what the registry
	// thinks can be run.
	if len(paths) == 0 && len(obligation.Items) == 0 {
		return verificationDemand{}
	}
	// Two demand sets, evaluated independently.
	//
	// Path coverage asks which artifact bytes were exercised, and only an
	// evidence record that NAMED the path can answer it. The exact-command
	// demand asks whether the client's own command ran, and a command that
	// names no file answers that perfectly well while covering nothing. They
	// were one loop over one list, so `pytest` created an obligation nothing
	// could discharge and the session could never reach settlement.
	for _, p := range paths {
		h := fileSHA256(ctx, p)
		if h == "" {
			return verificationDemand{Required: true, Missing: p}
		}
		if !pathCoverageSatisfied(ctx, p, h) {
			return verificationDemand{Required: true, Missing: p}
		}
	}
	for _, want := range obligation.Items {
		if !commandObligationSatisfied(ctx, want) {
			return verificationDemand{Required: true, Missing: want, MissingCommand: true}
		}
	}
	return verificationDemand{Required: true, Met: true}
}

// pathCoverageSatisfied answers the artifact question and only that one.
//
// Coverage comes from Covered and from nowhere else: a record that did not
// name the path cannot vouch for its bytes, however green it was.
func pathCoverageSatisfied(ctx *AgentContext, path, hash string) bool {
	if _, ok := coverageRecord(ctx, path, hash); ok {
		return true
	}
	return stagedCoverageSatisfied(ctx, path, hash)
}

// coverageRecord replays the evidence over one path's bytes, in order, and
// returns the record that vouches for them now. A run that showed the program
// working binds the bytes it covered; a later failed run over the same bytes
// takes that back, because the latest result on those bytes is the one that
// describes them. A static check, or a declared command that runs nothing,
// neither binds nor unbinds.
func coverageRecord(ctx *AgentContext, path, hash string) (VerificationRecord, bool) {
	var bound VerificationRecord
	ok := false
	if ctx == nil {
		return bound, false
	}
	for _, rec := range ctx.VerificationEvidence {
		if covered, current := evidenceIsCurrent(ctx, rec); current && covered[path] == hash {
			switch {
			case rec.Failed:
				bound, ok = VerificationRecord{}, false
			case rec.showsWorking():
				bound, ok = rec, true
			}
		}
	}
	return bound, ok
}

// commandObligationSatisfied answers the command question and only that one.
//
// Exact identity, byte for byte: no normalisation, no equivalence, no shell
// parsing. "python3  solve.py" is not "python3 solve.py".
func commandObligationSatisfied(ctx *AgentContext, want string) bool {
	// The latest current run of the command decides: a pass followed by a
	// failure on the same workspace is a failure.
	satisfied := false
	for _, rec := range ctx.VerificationEvidence {
		if rec.Command == want && commandEvidenceCurrent(ctx, rec) {
			satisfied = !rec.Failed
		}
	}
	if satisfied {
		return true
	}
	return stagedCommandSatisfied(ctx, want)
}

// commandEvidenceCurrent reports whether a direct execution still describes
// the workspace as it is now.
//
// A record that named paths is current on its coverage, exactly as before. A
// record that named none is current on the workspace identity it was stamped
// with -- which is conservative by construction: any material mutation to a
// tracked artifact moves the identity and stales every pathless record at
// once, including ones the mutation had nothing to do with.
func commandEvidenceCurrent(ctx *AgentContext, rec VerificationRecord) bool {
	if len(rec.Covered) > 0 {
		_, ok := evidenceIsCurrent(ctx, rec)
		return ok
	}
	if rec.WorkspaceStateHash == "" {
		// A record from before the stamp existed says nothing about when it
		// ran, and an unanswerable question is not a satisfied one.
		return false
	}
	generation, state := workspaceIdentity(ctx)
	return rec.WorkspaceGeneration == generation && rec.WorkspaceStateHash == state
}

// driftedSinceVerification names the first session-written file whose bytes
// no longer match the verified snapshot, or "" when everything still does.
// A file written AFTER the snapshot (absent from it) is drift by definition:
// it has never been executed.
func driftedSinceVerification(ctx *AgentContext, verified map[string]string) string {
	if ctx == nil {
		return ""
	}
	for p := range ctx.SessionWrites {
		want, seen := verified[p]
		if !seen {
			return p
		}
		data, err := os.ReadFile(resolveAgentPath(ctx, p))
		if err != nil {
			return p
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != want {
			return p
		}
	}
	return ""
}

// silentRunWhenOutputPromised reports a verification run that exited 0 while
// printing nothing, on a task whose prompt demands printed output.
//
// A file whose tail has been swallowed by a comment (one drifting "#" line
// eating the solve() call) still parses, runs, and exits 0. Empty stdout is
// then indistinguishable from success unless someone asks whether output was
// promised. Only run_commands that execute a program are held to it —
// build/compile steps legitimately print nothing.
func silentRunWhenOutputPromised(ctx *AgentContext, userMessage, command string, data json.RawMessage) bool {
	lower := strings.ToLower(userMessage)
	if !strings.Contains(lower, "print") && !strings.Contains(lower, "output") {
		return false
	}
	cmd := strings.TrimSpace(command)
	runsProgram := strings.Contains(cmd, "python") || strings.Contains(cmd, "node ") ||
		strings.Contains(cmd, "go run") || strings.HasPrefix(cmd, "./")
	if !runsProgram {
		return false
	}
	var out struct {
		Stdout string `json:"stdout"`
	}
	if json.Unmarshal(data, &out) != nil {
		return false
	}
	return strings.TrimSpace(out.Stdout) == ""
}

// stdinRedirectRe matches a shell stdin redirect from a plain filename
// anywhere in a segment: `python3 solve.py < input.txt`, including with
// trailing redirections after it (`< input.txt > out.txt`) — the
// trailing-only anchor missed those and the contract gate stayed silent
// (audit finding). `<<` heredocs and `<(...)` process substitution are not
// the shape this is about and are excluded by the caller.
var stdinRedirectRe = regexp.MustCompile(`(?:^|[^<>])<\s*([A-Za-z0-9_./-]+)`)

// pipeCatRe matches the pipe idiom that feeds a file to the next command's
// stdin: `cat input.txt | prog`. Same contract as `prog < input.txt`.
var pipeCatRe = regexp.MustCompile(`(?:^|&&|\|\||;)\s*cat\s+([A-Za-z0-9_./-]+)\s*\|[^|]`)

// stdinRedirectSource names the file a command pipes into a program's stdin,
// or "" when it does not.
//
// A program run as `prog < data` is being verified under a contract the
// caller may never use. Measured on the AoC tasks, whose prompt says the
// program must read input.txt: 7 of 10 failures wrote a program that reads
// stdin, ran it as `python3 solve.py < input.txt`, and got a successful
// result — so the model had every reason to believe it was done. The checker
// then ran `python solve.py` with no redirect and got 0. None of the sessions
// that verified this way passed.
//
// The same model with no shell never does this: it writes code that opens the
// file, because piping is not available to it. The tool is what makes the
// wrong shape reachable, so the harness is what has to notice.
func stdinRedirectSource(command string) string {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return ""
	}
	// `cat file | prog` feeds prog's stdin exactly like `prog < file`.
	if m := pipeCatRe.FindStringSubmatch(cmd); m != nil {
		return m[1]
	}
	for _, seg := range splitShellSegments(cmd) {
		if strings.Contains(seg, "<<") || strings.Contains(seg, "<(") {
			continue
		}
		if m := stdinRedirectRe.FindStringSubmatch(seg); m != nil {
			return m[1]
		}
	}
	return ""
}

// redirectOnlyVerificationMessage tells the model to run the artifact the way
// its caller will.
func redirectOnlyVerificationMessage(source string) string {
	return fmt.Sprintf(
		"Every time you ran the program you piped a file into it: `< %s`. That "+
			"verifies it as a filter reading stdin, which is not how it will be "+
			"run. %s is sitting in the working directory, so run it standalone "+
			"— `python3 <yourfile>` with no `<` — and make it open %s itself. "+
			"If it prints nothing or 0 that way, it is reading stdin and needs "+
			"to read the file instead.",
		source, source, source)
}

// fileCitationRe matches a filename as it appears in prose: `scoring.py`,
// planning.py, src/app/main.go. The extension must start with a letter and run
// 1-5 characters, so version strings ("V3.2") and decimals ("0.34") are not
// read as paths. Anything it over-matches is discarded by the existence check
// in unreadFileCitations.
var fileCitationRe = regexp.MustCompile(`[A-Za-z0-9_][A-Za-z0-9_./-]*\.[A-Za-z][A-Za-z0-9]{0,4}`)

// maxCitedPaths caps how many unread files one rejection names. Listing every
// one turns the correction into a chore; the model needs the shape and a
// couple of concrete targets.
const maxCitedPaths = 3

// unreadFileCitations returns files in the workspace that the reply makes a
// claim about without the run ever having been shown their contents.
//
// A reply that names a file it never opened is guessing, and the guess reads
// exactly like knowledge. Measured on a diagnostic question across three
// modules: 12 of 12 sessions ran list_directory, outlined ONE file, and
// answered. Which file they guessed decided the outcome — scoring.py wrong
// 11/11, planning.py right 1/1 — because the prompt said "scored" and the
// filename matched. One session cited "lines 134-142" of a file whose body it
// had never seen.
//
// The predicate is existence plus absence of evidence, not a judgement about
// the claim: the file has to be real, and the run has to have never read it.
// A file the model wrote is evidence enough, since it authored the contents,
// and so is one it moved or deleted: naming it reports the operation, not a
// guess about code it never saw.
func unreadFileCitations(ctx *AgentContext, text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range fileCitationRe.FindAllString(text, -1) {
		name := strings.Trim(m, "./-")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		resolved, info, err := statWorkspaceFile(ctx, name) // "x/../../y" is never looked up
		if err != nil || info.IsDir() {
			continue
		}
		if ctx.WasBodySeen(resolved) || ledgerTracks(ctx, resolved) {
			continue
		}
		out = append(out, name)
		if len(out) == maxCitedPaths {
			break
		}
	}
	return out
}

// unreadCitationMessage tells the model to look before it answers.
//
// It names outline_file explicitly because that is the tool the failure runs
// through: an outline lists signatures and line ranges with no bodies, which
// is enough scaffolding to state a confident, specific, wrong claim about what
// those lines contain.
func unreadCitationMessage(paths []string) string {
	var b strings.Builder
	b.WriteString("Your reply makes a claim about ")
	for i, p := range paths {
		switch {
		case i == 0:
		case i == len(paths)-1:
			b.WriteString(" and ")
		default:
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "`%s`", p)
	}
	subject, object := "that file", "it"
	if len(paths) > 1 {
		subject, object = "those files", "each of them"
	}
	fmt.Fprintf(&b, ", but this session has never seen the contents of %s. ", subject)
	fmt.Fprintf(&b, "outline_file lists signatures and line ranges only — it shows you no code, "+
		"so anything you say about what those lines do is a guess. "+
		"Call read_file on %s now, and on any other file you are about to name as the cause, "+
		"then answer from what the code actually says. ", object)
	b.WriteString(
		"If you are not sure which file holds the problem, search_files for the relevant symbol " +
			"across the whole directory rather than picking the file whose name matches the question.")
	return b.String()
}

// codeRefRe matches a code reference in backticks: `name`, `name()`,
// `module.name`.
var codeRefRe = regexp.MustCompile("`([A-Za-z_][A-Za-z0-9_]*(?:\\.[A-Za-z_][A-Za-z0-9_]*)*)(?:\\(\\))?`")

// fileExtensionWords are the last parts of a backticked file name
// (`config.json`), which name a file, not code.
var fileExtensionWords = map[string]bool{
	"json": true, "yaml": true, "yml": true, "toml": true, "html": true, "txt": true,
	"java": true, "tsx": true, "jsx": true, "cpp": true, "php": true, "css": true,
}

// unshownSymbol is code a reply names whose definition no read showed.
type unshownSymbol struct {
	Name  string
	File  string // as the workspace names it
	Line  int    // the definition's line
	Spans [][2]int
}

// unshownSymbolCitations returns the code a reply names whose definition the
// run never showed. unreadFileCitations works per file: any read of a file
// counts there as seeing all of it, but a truncated or ranged read shows only
// part. Smoke run 2026-09-27 (bugfind_tiebreak): both reads stopped near line
// 190, the reply named `_score_plan`, which it said lay "past the provided
// snippet", and the run ended completed.
//
// The predicate matches the file-level one: existence plus absence of
// evidence. The name must be defined in a file the run read only in part, at a
// line no read showed, and nowhere a read did show it.
func unshownSymbolCitations(ctx *AgentContext, text string) []unshownSymbol {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	paths := ctx.bodySeenPaths()
	partial := false
	for _, p := range paths {
		if ctx.ShownSpans(p) != nil {
			partial = true
			break
		}
	}
	if !partial {
		return nil
	}
	sources := map[string]string{}
	var out []unshownSymbol
	seen := map[string]bool{}
	for _, m := range codeRefRe.FindAllStringSubmatch(text, -1) {
		name := m[1]
		if i := strings.LastIndexByte(name, '.'); i >= 0 {
			name = name[i+1:]
		}
		if len(name) < 3 || seen[name] || fileExtensionWords[strings.ToLower(name)] {
			continue
		}
		seen[name] = true
		var gap *unshownSymbol
		shown := false
		for _, p := range paths {
			src, ok := sources[p]
			if !ok {
				data, err := os.ReadFile(p)
				if err != nil {
					continue
				}
				src = string(data)
				sources[p] = src
			}
			for _, line := range definitionLines(src, name) {
				if ctx.LineWasShown(p, line) {
					shown = true
					break
				}
				if gap == nil {
					gap = &unshownSymbol{Name: name, File: workspaceName(ctx, p), Line: line,
						Spans: ctx.ShownSpans(p)}
				}
			}
			if shown {
				break
			}
		}
		if gap != nil && !shown {
			out = append(out, *gap)
			if len(out) == maxCitedPaths {
				break
			}
		}
	}
	return out
}

// definitionLines returns the 1-based lines of source that define name: a
// def, class, func, function, fn or type line, at any indent.
func definitionLines(source, name string) []int {
	re := regexp.MustCompile(`^\s*(?:export\s+)?(?:(?:async|static|public|private|protected|pub)\s+)*` +
		`(?:def|class|function|fn|type|func(?:\s*\([^)]*\))?)\s+` + regexp.QuoteMeta(name) + `\b`)
	var out []int
	for i, ln := range strings.Split(source, "\n") {
		if re.MatchString(ln) {
			out = append(out, i+1)
		}
	}
	return out
}

// workspaceName is how the model names a file: relative to the workspace.
func workspaceName(ctx *AgentContext, path string) string {
	if rel, err := filepath.Rel(ctx.WorkingDir, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return filepath.Base(path)
}

// unshownSymbolMessage tells the model which code it described without being
// shown it, and where that code is.
func unshownSymbolMessage(gaps []unshownSymbol) string {
	var b strings.Builder
	b.WriteString("Your reply describes code this session has not shown you. ")
	for _, g := range gaps {
		fmt.Fprintf(&b, "`%s` is defined at line %d of %s, and the reads so far showed only %s of that file. ",
			g.Name, g.Line, g.File, lineSpanText(g.Spans))
	}
	b.WriteString("A truncated or ranged read shows only part of a file, so what you say about the rest " +
		"is a guess. Read the lines you are describing (read_file with offset and limit), then answer " +
		"from what the code actually says.")
	return b.String()
}

// symbolNames lists the names in gaps, for logs and the unresolved note.
func symbolNames(gaps []unshownSymbol) string {
	names := make([]string, len(gaps))
	for i, g := range gaps {
		names[i] = "`" + g.Name + "`"
	}
	return strings.Join(names, ", ")
}

// lineSpanText renders line spans as "lines 1-189 and 400-420".
func lineSpanText(spans [][2]int) string {
	if len(spans) == 0 {
		return "no lines"
	}
	parts := make([]string, len(spans))
	for i, s := range spans {
		if s[0] == s[1] {
			parts[i] = strconv.Itoa(s[0])
		} else {
			parts[i] = fmt.Sprintf("%d-%d", s[0], s[1])
		}
	}
	if len(parts) == 1 {
		return "lines " + parts[0]
	}
	return "lines " + strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// isExplainOnlyMessage reports an explicit "tell me, do not touch it"
// instruction: an explain/describe request paired with a no-edit directive.
//
// Position-based negation cannot catch this. In "…whether it is actually a
// bug. Do not change the code." the intent word comes BEFORE the directive,
// so a backward scan finds nothing — yet the instruction plainly governs the
// whole message. Measured: that prompt classified T2, ran the V3 pipeline,
// and wrote to files the user had just asked it to leave alone.
//
// Both halves are required. "fix the bug but don't change the public API" has
// the directive and is still real work; without the explain half it stays
// action intent.
func isExplainOnlyMessage(lower string) bool {
	explain := false
	for _, w := range []string{"explain", "describe", "walk me through",
		"what does", "what is", "how does", "why does", "tell me"} {
		if strings.Contains(lower, w) {
			explain = true
			break
		}
	}
	if !explain {
		return false
	}
	for _, d := range []string{
		"do not change", "don't change", "dont change",
		"do not edit", "don't edit", "dont edit",
		"do not modify", "don't modify", "dont modify",
		"do not write", "don't write",
		// "fix" completes the set: the list already carries change/edit/
		// modify/write, and a user who says "tell me what is wrong -- do not
		// fix it yet" has forbidden mutation exactly as plainly. Measured: a
		// benchmark question carrying this phrasing was classified as work and
		// edited the fixture it was told to leave alone, twice.
		"do not fix", "don't fix", "dont fix",
		"without changing", "without editing", "without modifying",
		"no code changes", "just explain", "only explain", "explain only",
	} {
		if strings.Contains(lower, d) {
			return true
		}
	}
	return false
}

func isFixIntentMessage(msg string) bool {
	lower := strings.ToLower(msg)
	if isExplainOnlyMessage(lower) {
		return false
	}
	for _, w := range fixIntentWords {
		idx := 0
		for {
			i := strings.Index(lower[idx:], w)
			if i < 0 {
				break
			}
			at := idx + i
			// Same negation rule as isActionIntentMessage: "explain whether
			// it is a bug, do not change the code" is a question about a
			// defect, not a request to repair one, and reading it as repair
			// intent handed it the write pipeline.
			if !negatedAt(lower, at) {
				return true
			}
			idx = at + len(w)
		}
	}
	return false
}

// actionIntentWords tracks verbs that signal "the user wants something
// CREATED, MODIFIED, or REPLACED on disk." Distinct from
// fixIntentWords (which is about repair/verification) — these match
// feature-build prompts where the model must emit a write_file /
// edit_file / structural_edit / delete_file before `done` is honest.
//
// May 10 2026 false-success case that motivated this: prompt was
// "Rewrite templates/dashboard.html to display a clean SaaS-style
// metrics dashboard..." Model spent 6 turns starting servers and
// curling the placeholder, never edited anything, declared `done`.
// The fix-intent gate didn't fire because "rewrite" isn't a
// fix-intent word — but it IS clearly an action-intent word that
// should have required a productive write.
var actionIntentWords = []string{
	"rewrite", "rewriting", "rewritten",
	"create", "creates", "creating", "created",
	"add", "adds", "adding", "added",
	"implement", "implements", "implementing", "implemented",
	"build", "builds", "building", "built",
	"write", "writes", "writing", "wrote",
	"refactor", "refactors", "refactoring", "refactored",
	"replace", "replaces", "replacing", "replaced",
	"update", "updates", "updating", "updated",
	"modify", "modifies", "modifying", "modified",
	"change", "changes", "changing", "changed",
	"make a", "make the", "make it",
	"convert", "converts", "converting", "converted",
	"redesign", "redesigning", "redesigned",
}

// reOutputFilenameTok matches a filename-looking token: an optional
// leading path, then name.ext (1-6 char extension). Captures group 1.
var reOutputFilenameTok = regexp.MustCompile("[`\"']?((?:[~./]|\\.\\./)?[\\w./-]*\\.[A-Za-z][A-Za-z0-9]{0,5})[`\"']?")

// reOutputWriteVerb matches the stems of verbs that mean "produce this
// file" — used to tell a prompt's OUTPUT file from an INPUT file. `read`
// is deliberately absent (it names an input).
var reOutputWriteVerb = regexp.MustCompile(`(?i)\b(sav|writ|creat|output|generat|stor|produc|recover|dump)`)

// reMustProduce matches the "<file> must exist / must contain" requirement
// phrasing (a merge-diff prompt: "the file algo.py must exist in the
// merged result"), which names a deliverable without a write verb.
// Checked in a window AFTER the filename.
var reMustProduce = regexp.MustCompile(`(?i)^\s*(must (exist|contain|include|be (creat|writt|present|generat)))`)

// reReadsTheFile matches a read verb that directly governs the filename after
// it ("reads input.txt", "loads the file config.yaml"). Such a file is the
// task's input even when a write verb sits within the window before it:
// "Write solve.py that reads input.txt" asks for solve.py alone. Only a
// determiner or "file"/"data"/"contents" may stand between the verb and the
// name, so "Write the parse results to out.json" still names an output.
var reReadsTheFile = regexp.MustCompile(`(?i)\b(read|reads|reading|load|loads|loading|parse|parses|parsing|open|opens|opening)\s+((the|a|an|its|file|data|contents?|of)\s+){0,3}$`)

// expectedOutputPaths extracts the file(s) a task prompt explicitly asks
// the model to produce: a filename token preceded within ~70 chars by a
// write/save/create/output verb. Grounded in the task text (many bench and
// real prompts say "save your solution in X", "write the output to Y",
// "create a JSON file Z"), so it can be checked against disk at the end.
// Bounded to the first 2 to avoid over-steering on a chatty prompt.
func expectedOutputPaths(msg string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range reOutputFilenameTok.FindAllStringSubmatchIndex(msg, -1) {
		path := msg[m[2]:m[3]]
		if path == "" || strings.Count(path, ".") == len(path) {
			continue
		}
		start := m[0] - 70
		if start < 0 {
			start = 0
		}
		afterEnd := m[1] + 40
		if afterEnd > len(msg) {
			afterEnd = len(msg)
		}
		// Output signal: a write verb within ~70 chars before the filename,
		// OR "must exist/contain" requirement phrasing right after it.
		if (!reOutputWriteVerb.MatchString(msg[start:m[0]]) || reReadsTheFile.MatchString(msg[start:m[0]])) &&
			!reMustProduce.MatchString(msg[m[1]:afterEnd]) {
			continue // input/incidental filename
		}
		if !seen[path] {
			seen[path] = true
			out = append(out, path)
			if len(out) >= 2 {
				break
			}
		}
	}
	return out
}

// reInvestigationScope matches a request whose subject is more than one file:
// "these files", "across the modules", "the codebase", "this directory". It
// reads the user's own words and nothing else -- no plan, no task name, no
// knowledge of any particular project.
var reInvestigationScope = regexp.MustCompile(`(?i)\b(?:these|those)\s+(?:\w+\s+){0,2}(?:files|modules|scripts|services|packages)\b` +
	`|\bacross\b[^.]{0,40}?\b(?:files|modules|codebase|project|services|packages)\b` +
	`|\bthe\s+(?:codebase|repo|repository)\b` +
	`|\bthis\s+(?:directory|repo|repository)\b` +
	`|\b(?:each|every|all)\s+(?:of\s+the\s+)?(?:files|modules)\b`)

// investigationScopeUnmet answers one question about a read-only run: the user
// asked how several files behave, so does the answer account for more than one
// of the files this run actually opened?
//
// Measured (stabilization cycle 3, family S): asked to walk through how the
// pricing cache works ACROSS five modules and why the hit rate is low, the run
// read three of them and answered with one function pair in one file. Every
// existing exit gate passed -- nothing was written, nothing was deferred, no
// question was asked, no file was cited unread -- so the reply was finalized
// completed. Half an investigation and a whole one were indistinguishable.
//
// The evidence is the ordinary request and the run's own reads. Nothing here
// judges whether the answer is correct, or good: that is not decidable from
// the workspace, and a rule that pretended otherwise would be a guess with a
// status attached.
func investigationScopeUnmet(ctx *AgentContext, userMessage, claimText string,
	readOnly bool, toolsRun int) (read, cited []string, unmet bool) {
	if ctx == nil || !readOnly || toolsRun == 0 || strings.TrimSpace(claimText) == "" {
		return nil, nil, false
	}
	if !reInvestigationScope.MatchString(userMessage) {
		return nil, nil, false
	}
	read = bodiesSeenInRun(ctx)
	if len(read) < 2 {
		// One file opened cannot span anything; the answer is judged by the
		// gates that already exist.
		return read, nil, false
	}
	for _, name := range read {
		if strings.Contains(claimText, name) || answerNamesSymbolFrom(ctx, name, claimText) {
			cited = append(cited, name)
		}
	}
	return read, cited, len(cited) < 2
}

// bodiesSeenInRun lists the base names of files whose contents this run was
// actually shown, sorted, so a message about them reads the same way twice.
func bodiesSeenInRun(ctx *AgentContext) []string {
	seen := map[string]bool{}
	ctx.mu.Lock()
	for path, ok := range ctx.BodySeen {
		if ok {
			seen[filepath.Base(path)] = true
		}
	}
	ctx.mu.Unlock()
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// answerNamesSymbolFrom reports whether the answer accounts for a file by
// naming something defined in it.
//
// Measured (stabilization cycle 5, audit of this guard): an answer that traced
// the fault correctly through read_key and write_key, naming one file, was
// sent back as a partial investigation. Explaining a relationship through the
// functions involved is how such an answer is normally written, and the
// symbols come from the bytes THIS run read — not from any task knowledge.
//
// It makes the guard more permissive, which is the intended direction: the
// guard is a floor against answering for one file out of several, not a test
// of whether an answer is correct.
func answerNamesSymbolFrom(ctx *AgentContext, base, claimText string) bool {
	ctx.mu.Lock()
	var source, path string
	for p, content := range ctx.FilesRead {
		if filepath.Base(p) == base {
			source, path = content, p
			break
		}
	}
	ctx.mu.Unlock()
	if source == "" {
		return false
	}
	for _, sym := range outlineByRegex(path, source) {
		// Short names ("get", "id") appear in ordinary prose; requiring four
		// characters keeps the match to something the file actually declares.
		// Whole-word only: "price" must not be credited to the word "pricing".
		if len(sym.Name) >= 4 && mentionsWord(claimText, sym.Name) {
			return true
		}
	}
	return false
}

// mentionsWord reports whether text contains name as a whole identifier —
// neither a prefix of a longer word nor a suffix of one.
func mentionsWord(text, name string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], name)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(name)
		beforeOK := start == 0 || !isIdentRune(rune(text[start-1]))
		afterOK := end == len(text) || !isIdentRune(rune(text[end]))
		if beforeOK && afterOK {
			return true
		}
		i = start + 1
	}
}

func isIdentRune(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// investigationScopeMessage asks for the coverage the request asked for. It
// names the files the run opened and says nothing about what is in them.
func investigationScopeMessage(read, cited []string) string {
	var sb strings.Builder
	sb.WriteString("The request asks about how several files behave together. ")
	switch len(cited) {
	case 0:
		fmt.Fprintf(&sb, "Your reply does not mention any of the files you read (%s). ", strings.Join(read, ", "))
	default:
		fmt.Fprintf(&sb, "Your reply accounts for %s, but you read %s. ", strings.Join(cited, ", "), strings.Join(read, ", "))
	}
	sb.WriteString("Answer for the files you examined: say what each one contributes and how they connect, " +
		"in a single `text` reply that stands on its own. If a file you opened turned out to be irrelevant, " +
		"say so and why. Do not change any files.")
	return sb.String()
}

// missingExpectedOutputs returns the expected output files that do not
// exist on disk. Checks the resolved path with os.Stat so it counts a
// file created by ANY means (write_file OR a run_command that
// redirected/generated it), not just write_file. Stat probes are
// contained to known roots — the workspace, plus the system temp dir
// (host-verify tasks legitimately name /tmp outputs). A path outside
// both is skipped: the gate only enforces deliverables it can check
// without probing arbitrary prompt-derived paths.
func missingExpectedOutputs(ctx *AgentContext, expected []string) []string {
	var missing []string
	roots := []string{filepath.Clean(ctx.WorkingDir), filepath.Clean(os.TempDir())}
	for _, p := range expected {
		resolved := resolveAgentPath(ctx, p)
		for _, root := range roots {
			rel, err := filepath.Rel(root, resolved)
			if err != nil || !filepath.IsLocal(rel) {
				continue
			}
			if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
				missing = append(missing, p)
			}
			break // first containing root decides
		}
	}
	return missing
}

// logPath escapes CR/LF in a request-derived value so a crafted name
// can't forge additional log lines; logPaths is the slice form.
func logPath(p string) string {
	p = strings.ReplaceAll(p, "\n", `\n`)
	return strings.ReplaceAll(p, "\r", `\r`)
}

func logPaths(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = logPath(p)
	}
	return out
}

// isActionIntentMessage returns true when the prompt clearly asks
// for a state change on disk (create/rewrite/refactor/etc.). The
// done-without-action gate uses this to bounce a `done` that wasn't
// preceded by any productive write — which would otherwise pass
// through silently because the fix-intent gate ignores feature work.
func isActionIntentMessage(msg string) bool {
	lower := strings.ToLower(msg)
	if isExplainOnlyMessage(lower) {
		return false
	}
	for _, w := range actionIntentWords {
		idx := 0
		for {
			i := strings.Index(lower[idx:], w)
			if i < 0 {
				break
			}
			at := idx + i
			if !negatedAt(lower, at) {
				return true
			}
			idx = at + len(w)
		}
	}
	return false
}

// negatedAt reports whether the action word at `at` is inside a negation —
// "do not change any code", "without editing", "no need to fix it".
//
// A plain substring scan reads "do not change any code" as a request to
// change code, so a question carrying that clause was classified T2 and got
// the whole write pipeline. Measured: the identical question scored T0
// without the clause and T2 with it, and the T2 run edited files the user had
// explicitly asked it to leave alone. Telling ATLAS not to touch anything
// made it more likely to.
//
// Scans a short window back rather than parsing: the negation always sits
// within a few words in the phrasings people actually use, and a wider window
// would start swallowing unrelated clauses ("I fixed the parser, now don't
// worry about X" must still read as action intent).
func negatedAt(lower string, at int) bool {
	const window = 24
	start := at - window
	if start < 0 {
		start = 0
	}
	before := lower[start:at]
	for _, neg := range []string{
		"do not ", "don't ", "dont ", "never ", "without ",
		"no need to ", "rather than ", "instead of ", "avoid ",
	} {
		if strings.Contains(before, neg) {
			return true
		}
	}
	return false
}

// expectedOutputMissingMessage tells the model the task's named output
// file doesn't exist yet — the deliverable, not just "some change." Names
// the file(s) so the steer is concrete and grounded in the task text.
func expectedOutputMissingMessage(missing []string) string {
	quoted := make([]string, len(missing))
	for i, p := range missing {
		quoted[i] = "`" + p + "`"
	}
	return "Before you finish — the task names " +
		strings.Join(quoted, " and ") +
		" as a deliverable, but it does not exist on disk yet. If your code PRODUCES it when run, run your code now to generate it (do NOT hand-write a fabricated stand-in). If it is a file you author directly, write your solution to it. If you have genuinely already produced it elsewhere or it is not actually required, you may proceed."
}

// actionWithoutProductiveChangeMessage tells the model to actually do
// the work the user asked for before declaring done. Concrete and
// directive — points at the missing tool call, not abstract "you
// haven't done enough." Mirror of verificationRejectionMessage's
// shape.
func actionWithoutProductiveChangeMessage(userMsg string) string {
	return "Cannot declare `done` yet — the user asked you to make a change on disk (rewrite/create/add/implement/refactor/etc.) and you haven't emitted any successful write_file / edit_file / structural_edit / delete_file in this loop. Verification (running the server, curling the page) is NOT the task — it's how you confirm AFTER the change. Re-read the user's request, identify what file needs to change, and emit the appropriate edit tool. Then verify, then done."
}

// isHeadOnlyProbe reports a probe that fetches headers and no body. Such a
// probe is not verification (see isVerificationCommand) — this says so out
// loud instead of declining in silence.
//
// Measured (stabilization cycle 6, both flask_pause sessions): the run
// installed the missing dependency, started the app, and probed it with
// `curl -I`, which answered HTTP 200. The probe was declined, nothing said
// why, the run repeated it, and both sessions ended
// verification_demanded_unmet with working code on disk.
func isHeadOnlyProbe(cmd string) bool {
	segs, _, _ := splitTopLevelShell(cmd)
	for _, seg := range segs {
		words := programWords(shellFields(strings.TrimSpace(seg)))
		if len(words) == 0 {
			continue
		}
		switch filepath.Base(words[0]) {
		case "curl", "wget":
			if headOnlyProbeRe.MatchString(seg) {
				return true
			}
		}
	}
	return false
}

// headOnlyProbeNote explains the decline in one sentence, without prescribing
// a command to paste.
func headOnlyProbeNote(cmd string) string {
	return fmt.Sprintf("Your probe `%s` asked for headers only. A HEAD response shows the server answered; "+
		"it cannot show that the page or endpoint does the right thing, so it does not count as a "+
		"verification. Request the body instead and check what comes back.", truncateStr(strings.TrimSpace(cmd), 60))
}

// isVerificationCommand reports whether a command, when it exits green,
// shows the program working: it ran the program or its tests, or fetched a
// response body, and the line reports that part's exit status. A parse, a
// lint, a build, a `--version`, a headers-only probe, and a test whose status
// `| tail` or `|| true` replaced are not verification. A probe that never
// retrieves a body proves the SERVER is up, not that the artifact works:
// measured on a "build me a snake game" session, `curl -I` was recorded as
// the verification and `done` shipped an index.html with no HTML in it.
func isVerificationCommand(cmd string) bool {
	return classifyCommandEvidence(cmd).Kind.verifies()
}

// headOnlyProbeRe matches curl/wget invocations that fetch headers only:
// `curl -I`, `curl --head`, `wget --spider`. Long-form and clustered short
// flags (-sI) both count.
var headOnlyProbeRe = regexp.MustCompile(`(?i)\b(?:curl\b[^|;&]*?\s-{1,2}(?:I\b|head\b)|curl\b[^|;&]*?\s-[a-zA-Z]*I[a-zA-Z]*\b|wget\b[^|;&]*?--spider\b)`)

// actionDemandSource names why an action-demand decision came out as it did.
// It exists so evidence can say which authority answered, not merely what the
// answer was.
type actionDemandSource string

const (
	actionDemandContractWork     actionDemandSource = "contract_work"
	actionDemandContractQuestion actionDemandSource = "contract_question"
	actionDemandLegacy           actionDemandSource = "legacy"
	// contract_invalid_failed_closed cannot be produced by a validated
	// request; it exists so an internally malformed mode fails toward
	// requiring work rather than silently reading as a question.
	actionDemandContractInvalid actionDemandSource = "contract_invalid_failed_closed"
)

// actionDemand is one decision about whether a request demands a state change,
// together with the authority that made it and the legacy heuristic's own
// answer for comparison.
type actionDemand struct {
	Required bool
	Source   actionDemandSource
	// Legacy is what wantsStateChange said. It is always reported and is
	// authoritative ONLY when no contract is present.
	Legacy bool
}

// decideActionDemand is the single owner of "does this request demand a state
// change". Both live action-demand sites consume it and nothing else calls the
// heuristic for a live decision.
//
// Where the client declared a task mode, that mode decides. Step 3B measured
// the alternative on a frozen 105-case corpus: 25 of 101 evaluable requests
// disagreed with the client's own declaration, 19 of them work that the wording
// alone read as a question, and 110 of 115 gate-bearing requests were decided
// with inspectedWorkspace false -- i.e. on phrasing. A client that states what
// it asked for is better evidence than a guess about its English.
//
// Where no contract was sent, nothing changes: the heuristic decides exactly as
// before. That corpus says nothing about contractless clients, so it authorises
// nothing for them.
//
// The mode establishes an OBLIGATION only. It never authorises completion,
// mutation, deletion, permission or verification -- those keep their own
// evidence, and a question contract does not erase debt, hazards or broken
// deliverables.
//
// Pure: no model output, no workspace state beyond the inspection flag it is
// handed, no shadow state, and the same answer whether or not capture is on.
func decideActionDemand(tc *TaskContract, userMessage string, tier Tier,
	inspectedWorkspace bool) actionDemand {
	// A change the user explicitly forbade cannot also be required. The
	// read-only boundary in executeToolCallInner will REFUSE every mutating
	// tool for this request, so demanding one produces a run that is told to
	// do the one thing it is not allowed to do, bounces its own exit until the
	// budget runs out, and reports incomplete work it was never permitted to
	// perform. Measured: a client declared task_mode=work on prompts whose
	// text says "do not change any code", and all six such sessions ended
	// action_demanded_unmet while the guard correctly refused their edits.
	//
	// This outranks the contract deliberately. A caller's task_mode describes
	// what it believes it is asking for; the user's own words are the request.
	// When they disagree about whether the workspace may change, the safe and
	// coherent reading is the prohibition — and the run still completes, on
	// its answer, because a run that writes nothing owes no file obligation.
	if isExplainOnlyMessage(strings.ToLower(userMessage)) {
		return actionDemand{Required: false, Source: actionDemandLegacy, Legacy: false}
	}
	// Evaluated exactly once, here, for every path. Reporting it costs
	// nothing because the heuristic is pure, and having it always present
	// keeps the shadow record identical whether or not capture is enabled.
	legacy := wantsStateChange(userMessage, tier, inspectedWorkspace)
	if tc == nil {
		return actionDemand{Required: legacy, Source: actionDemandLegacy, Legacy: legacy}
	}
	switch tc.TaskMode {
	case TaskModeWork:
		return actionDemand{Required: true, Source: actionDemandContractWork, Legacy: legacy}
	case TaskModeQuestion:
		return actionDemand{Required: false, Source: actionDemandContractQuestion, Legacy: legacy}
	default:
		return actionDemand{Required: true, Source: actionDemandContractInvalid, Legacy: legacy}
	}
}

// wantsStateChange reports whether `done` should be blocked when no write,
// edit, or delete succeeded in this run.
//
// actionIntentWords alone was the test, and it is an open vocabulary that
// cannot be completed: it lists "create"/"add"/"make" but not
// "remove"/"delete", so "remove the debug logging from app.py" armed no
// gate and the model could close the turn having deleted nothing. Adding
// those two words leaves the next verb missing.
//
// The second signal is observed instead of guessed: a read-only tool
// succeeded, so the model opened the project rather than answering from the
// message alone. That covers any phrasing, including verbs no list has.
//
// It needs the tier to stay honest, because reading files is also how a
// question gets answered. "why does the game store direction as a string"
// opens the file and correctly writes nothing; classifyAgentTier calls that
// conversational, and conversational messages are never gated. What remains
// is the case worth blocking: a non-conversational message, the model went
// into the project, and nothing changed on disk.
func wantsStateChange(userMessage string, tier Tier, inspectedWorkspace bool) bool {
	if isActionIntentMessage(userMessage) {
		return true
	}
	// A request to LOOK is satisfied by looking. inspectedWorkspace turns on
	// as soon as a read-only tool succeeds, so without this "list the files"
	// bounced its own `done`: the read that answered the question was the same
	// read the gate treated as evidence that work had started. Observed live —
	// the model listed the directory, was refused `done`, and told the user it
	// was "unable to complete the 'done' state as per the system's
	// requirements", a rule nothing in the prompt states.
	if isReadOnlyRequest(userMessage) {
		return false
	}
	return inspectedWorkspace && tier != Tier0Conversational
}

// isReadOnlyRequest matches asks that are ANSWERED by reading: list, show,
// find, print. Deliberately narrow — it only decides whether a completed read
// is allowed to end the turn, so a false positive lets a real edit request
// finish without editing.
//
// Callers check isActionIntentMessage first, which covers "add a list to the
// page". The fix-intent exclusion below covers the other overlap: "find and
// fix the bug" opens with a read verb but is not read-only work.
func isReadOnlyRequest(msg string) bool {
	lower := strings.ToLower(msg)
	if isFixIntentMessage(msg) || isActionIntentMessage(msg) {
		return false
	}
	for _, w := range []string{
		"list the file", "list files", "list all", "list the director",
		"show me", "show the", "what files", "which files", "what's in",
		"whats in", "what is in", "find the file", "where is", "where are",
		"print the", "display the", "read the",
	} {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

// gateTrigger names why the verification gate fired, for the log line. A
// red command outranks message shape: it is the concrete signal, and when
// both hold it is the one that describes what actually happened.
func gateTrigger(userWantsVerification, sawFailedVerification bool) string {
	switch {
	case sawFailedVerification:
		return "failed-verification"
	case userWantsVerification:
		return "fix-intent"
	default:
		return "none"
	}
}

// verificationRejectionMessage tells the model exactly what's
// missing and what to run. We prefer concrete suggestions over
// abstract "verify your work" prompts — the model is more likely to
// pick a sensible command when given a category.
//
// sawFailedVerification distinguishes the two ways this gate fires. When a
// verification command has actually gone red in this loop, the run holds
// concrete evidence of breakage, so the message says that rather than
// describing the request — the model has already seen the failure and needs
// to act on it, not be told what verification is.
// blockedServerStart reports whether a failed verification command failed
// because it is a long-running process rather than because the code is
// broken: it never exited (the sandbox timeout fired) or it could not bind
// because something is already serving that port.
//
// The distinction decides what the verification gate says next. Treating it
// as a red test tells the model to fix its code and re-run the command, and
// re-running a blocking server start can never exit clean — an observed
// session started the server correctly with run_background, was told to
// "re-run the same command and confirm it exits clean", and spent its three
// remaining bounces re-sending `done` because nothing it could do satisfied
// that.
func blockedServerStart(output string) bool {
	low := strings.ToLower(output)
	return strings.Contains(low, "execution timed out") ||
		strings.Contains(low, "address already in use") ||
		strings.Contains(low, "is in use by another program")
}

func verificationRejectionMessage(sawFailedVerification bool) string {
	return verificationRejection(sawFailedVerification, false, "")
}

// verificationRejection is verificationRejectionMessage with the two facts
// that change the advice: whether the red command was a blocking server
// start, and the job id if one is already running in the background.
// rewriteThreshold is how many consecutive red verifications exhaust the
// benefit of incremental edits. The no-tool retry baseline regenerates from
// scratch on every failure and scores 78% where incremental nibbling
// plateaued at 66-68: past this streak the advice flips from "apply the fix"
// to "rewrite the file from a clean sheet".
const rewriteThreshold = 2

// executorNames are commands that run a file handed to them as an argument.
// Wrappers like timeout/env don't qualify on their own — the real interpreter
// still has to appear between them and the file.
var executorNames = map[string]bool{
	"python": true, "python3": true, "python2": true, "py": true,
	"node": true, "nodejs": true, "deno": true, "bun": true,
	"ruby": true, "perl": true, "php": true, "lua": true,
	"bash": true, "sh": true, "zsh": true, "dash": true,
	"pytest": true, "go": true, "cargo": true, "java": true, "dotnet": true,
}

// executionAttempt reports whether the command actually attempts to RUN the
// file at path — an interpreter invocation (`python3 solve.py`, with or
// without wrapper/flag tokens in between) or direct execution (`./solve.py`).
// Merely naming the file proves nothing about its runtime behavior: `cat`,
// `grep`, `ls`, `wc` all name it and were all discharging the warned-run
// mark under the old substring rule (audit finding).
func executionAttempt(command, path string) bool {
	base := filepath.Base(path)
	for _, segment := range splitShellSegments(command) {
		// A parse, lint or compile names the file without running it:
		// `python -m py_compile app.py` is not an execution of app.py.
		if segmentEvidenceKind(segment) != evidenceExecution {
			continue
		}
		toks := strings.Fields(segment)
		fileAt := -1
		for i, tok := range toks {
			tok = strings.Trim(tok, `"'`)
			if tok == path || tok == base || strings.HasSuffix(tok, "/"+base) {
				if strings.HasPrefix(tok, "./") {
					return true // direct execution
				}
				fileAt = i
				break
			}
		}
		if fileAt <= 0 {
			continue // absent, or the file itself is the first token without ./
		}
		for _, prev := range toks[:fileAt] {
			prev = strings.Trim(prev, `"'`)
			if executorNames[filepath.Base(prev)] {
				return true
			}
		}
	}
	// An import IS an execution. `python3 -c "import store; store.add(...)"`
	// loads store.py and raises its SyntaxError -- exactly the traceback the
	// run-first gate exists to make the model read -- but the command never
	// spells "store.py", so the token scan above cannot see it.
	//
	// Measured on multifile_cli rep2: the model wrote a broken store.py, ran
	// it this way, got the traceback, and then had every edit_file refused as
	// "has never been run", while re-sending the run tripped the
	// identical-call detector. It was told to do the one thing it had already
	// done and was no longer permitted to repeat, and the session ended
	// `repeated_refusal` with the file still broken. A deadlock, not a model
	// failure: for a file that does not parse there is no run that succeeds,
	// and the only exit is the edit the gate was refusing.
	return inlineCodeExecutesPath(command, path)
}

// inlineCodeExecutesPath reports whether an executor was handed inline code
// that loads `path`. Narrow on purpose: an executor must actually be invoked,
// and the code it was given has to reference the file the way an import does.
func inlineCodeExecutesPath(command, path string) bool {
	for _, segment := range splitShellSegments(command) {
		code, ok := inlineCodePayload(segment)
		if !ok {
			continue
		}
		// `;` separates statements the way a newline does, and
		// sourceReferencesPath reads lines. Same reference rule the
		// verification coverage uses, so the two cannot disagree about what
		// counts as loading a file.
		if sourceReferencesPath(strings.ReplaceAll(code, ";", "\n"), path) {
			return true
		}
	}
	return false
}

// inlineCodePayload returns the code an executor was asked to run inline, and
// whether this segment is such an invocation at all. `python3 -m pytest x.py`
// is not inline code -- it names its file, which the token scan already sees.
func inlineCodePayload(segment string) (string, bool) {
	toks := strings.Fields(segment)
	if len(toks) == 0 || !executorNames[filepath.Base(strings.Trim(toks[0], `"'`))] {
		return "", false
	}
	for i, tok := range toks[1:] {
		if strings.Trim(tok, `"'`) == "-c" {
			return strings.Trim(strings.Join(toks[i+2:], " "), `"'`), true
		}
	}
	// `python3 - <<'EOF'` runs the here-document on the lines after the
	// command. `python3 app.py <<EOF` feeds app.py data, which is not code.
	nl := strings.IndexByte(segment, '\n')
	if nl < 0 {
		return "", false
	}
	for _, tok := range strings.Fields(segment[:nl])[1:] {
		t := strings.Trim(tok, `"'`)
		if t == "-" || strings.HasPrefix(t, "<<") {
			return segment[nl+1:], true
		}
		if !strings.HasPrefix(t, "-") {
			return "", false
		}
	}
	return "", false
}

// workspaceFileReader reads a workspace-relative path for the guardrails
// that inspect a script before deciding how it should be run.
func workspaceFileReader(ctx *AgentContext) func(string) (string, bool) {
	return func(rel string) (string, bool) {
		data, _, err := readWorkspaceFile(ctx, rel) // rel is parsed from a command string
		if err != nil {
			return "", false
		}
		return string(data), true
	}
}

// runCommandFor is the command that runs one file of a syntax-gated language
// on its own, with the toolchains the sandbox image installs. Python is the
// historical default the gate always quoted; it was also what the gate quoted
// for Go, Java and Kotlin files, which python3 cannot run.
func runCommandFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".js", ".mjs", ".cjs":
		return "node " + path
	case ".ts":
		return "npx tsx " + path
	case ".sh", ".bash":
		return "bash " + path
	case ".rb":
		return "ruby " + path
	case ".php":
		return "php " + path
	case ".go":
		return "go run " + path
	case ".java":
		return "java " + path // JDK 11+ runs a single source file directly
	case ".kt":
		jar := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)) + ".jar"
		return "kotlinc " + path + " -include-runtime -d " + jar + " && java -jar " + jar
	}
	return "python3 " + path
}

// runFirstInstruction is the one instruction every run-first gate quotes:
// the call that executes the warned file, phrased for the tool that will
// accept it. A script that serves until killed is refused by the
// foreground-server redirect when sent through run_command, so demanding
// `run_command python3 app.py` for such a file is a contradiction the model
// cannot resolve. Observed 2026-09-14: the gate demanded exactly that, the
// redirect refused it, and the session ended with the broken file on disk.
func runFirstInstruction(ctx *AgentContext, path string) string {
	cmd := runCommandFor(path)
	if foregroundServerRejectionWithSource(cmd, workspaceFileReader(ctx)) != "" {
		return fmt.Sprintf("start it with run_background {\"command\": %q} and read the traceback it returns (tail_background shows more), then stop_background", cmd)
	}
	return fmt.Sprintf("run it with run_command {\"command\": %q} and read the real error", cmd)
}

// freshRewriteAdvice is the start-over guidance shared by the done-gate
// rejection and the immediate mid-loop corrective. One source of truth so
// the model hears the same instruction at the crossing and at the gate.
func freshRewriteAdvice(redStreak int) string {
	return fmt.Sprintf(
		"the verification command has now failed %d times in a row and your incremental edits are not converging. Stop patching. Rewrite the file from scratch with write_file: re-read the task statement, take a fresh approach, and keep it simple. A clean rewrite finds bugs that ten small edits walk past.",
		redStreak)
}

// verificationRejectionWithStreak is verificationRejection plus the red-run
// streak that decides between edit-the-fix and start-over advice.
func verificationRejectionWithStreak(sawFailedVerification, serverBlocked bool, bgJobID string, redStreak int) string {
	return verificationRejectionFor(sawFailedVerification, serverBlocked, bgJobID, redStreak, "", "", "")
}

// verificationRejectionFor is the rejection with one more fact when the run has
// one: the probe it already made that could not count, and why.
func verificationRejectionFor(sawFailedVerification, serverBlocked bool, bgJobID string,
	redStreak int, headOnlyProbe, staleJob, staleFile string) string {
	base := ""
	if !serverBlocked && sawFailedVerification && redStreak > rewriteThreshold {
		base = "Cannot declare `done` — " + freshRewriteAdvice(redStreak)
	} else {
		base = verificationRejection(sawFailedVerification, serverBlocked, bgJobID)
	}
	if headOnlyProbe != "" {
		base += "\n\n" + headOnlyProbeNote(headOnlyProbe)
	}
	if !serverBlocked && bgJobID != "" {
		base += fmt.Sprintf("\n\nBackground job %s is running right now — probe it and read what comes back "+
			"(the body, not just the headers). Verify it before stopping it.", bgJobID)
	}
	if staleJob != "" && staleFile != "" {
		base += fmt.Sprintf("\n\nNote: job %s has been running since before your last change to %s, so it is "+
			"still serving the older code — probing it would not show your change. Stop it and start it again "+
			"first.", staleJob, staleFile)
	}
	return base
}

func verificationRejection(sawFailedVerification, serverBlocked bool, bgJobID string) string {
	if serverBlocked {
		probe := "Start it with `run_background` (it returns a job_id), then probe it with " +
			"`run_command(\"curl -sf http://localhost:<port>/\")`."
		if bgJobID != "" {
			probe = "It is ALREADY running as background job " + bgJobID +
				" — do not start another copy. Probe it now with " +
				"`run_command(\"curl -sf http://localhost:<port>/\")`, which is the command that verifies it."
		}
		return "Cannot declare `done` yet — nothing has verified this change. The command that " +
			"failed is a long-running server: it did not exit because servers do not exit, so " +
			"re-running it in the foreground can never succeed and its failure says nothing about " +
			"your code. " + probe + " A clean curl is the verification this gate wants."
	}
	if sawFailedVerification {
		return "Cannot declare `done` — a test or build command you ran in this session FAILED and nothing has passed since. You have already seen the failure output. Apply the fix with `edit_file`, `structural_edit`, or `write_file`, then re-run the same command and confirm it exits clean. Describing the fix is not applying it: if you know what the problem is, make the edit now. Declaring done over a red test reports a broken result as a working one."
	}
	return "Cannot declare `done` yet — this is a fix/repair request and you haven't verified the change works. Before emitting `done`, run a verification command and confirm it succeeded. Examples: `python3 app.py` to run a script, `pytest tests/` to run tests, `npm test` for Node, `go test ./...` for Go; for a server, start it with run_background and probe it with `curl -sf http://localhost:<port>/`. A syntax check or a linter does not count: it shows the code is well formed, not that it works. \"Done\" without a clean verification exit is a guess, not a fix."
}

// splitShellSegments splits a command line on `&&`, `||`, `;`, `|`
// while ignoring those characters when they appear inside single
// or double quotes. Best-effort, not a real shell parser — but enough
// for the model-emitted commands we want to gate.
func splitShellSegments(cmd string) []string {
	var out []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch c {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		}
		if !inSingle && !inDouble {
			if c == '&' && i+1 < len(cmd) && cmd[i+1] == '&' {
				out = append(out, cur.String())
				cur.Reset()
				i++
				continue
			}
			if c == '|' && i+1 < len(cmd) && cmd[i+1] == '|' {
				out = append(out, cur.String())
				cur.Reset()
				i++
				continue
			}
			if c == ';' || c == '|' {
				out = append(out, cur.String())
				cur.Reset()
				continue
			}
		}
		cur.WriteByte(c)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// isNewWrite returns true when the resolved path doesn't yet exist on
// disk. Used by stub-detection / pattern-reflex gates to scope their
// rejection logic to genuinely new files — modifying an existing file
// is a different shape and the V3 / surgical-edit gate handles those.
func isNewWrite(resolvedPath string) bool {
	_, err := os.Stat(resolvedPath)
	return os.IsNotExist(err)
}

// stubHTMLRe catches `<h1>Foo Page</h1>` / `<h1>Bar Section</h1>` —
// the exact shape the model emits when it gives up and ships a
// placeholder. Matches inside <body>, allows whitespace.
var stubHTMLRe = regexp.MustCompile(
	`(?is)<h\d>\s*[A-Za-z]+\s+(page|section|title|content|view)\s*</h\d>`)

// looksLikeStub returns a non-empty rejection string when the content
// looks like a placeholder/stub. The model's lazy-completion
// failure mode is to ship 8-line skeletons that pass syntactic gates
// but ship the absolute minimum content to claim "done." Catches the
// most egregious shapes per file type; deliberately conservative —
// short content that has REAL substance (one-liner shell scripts,
// minimal Dockerfiles, single-import test files) passes through.
//
// The fix is to either model the file from a sibling (templates/index.html
// usually has the right scaffold) or — if the user really did ask for
// a placeholder — say so in the response so the user knows.
func looksLikeStub(displayPath, content string) string {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return "write_file refused: content is empty. If you mean to create an empty file, write a meaningful starting structure or `touch` it via run_command."
	}

	ext := strings.ToLower(filepath.Ext(displayPath))
	lineCount := strings.Count(trimmed, "\n") + 1

	switch ext {
	case ".html", ".htm":
		// 200 chars is the cliff — full pages don't fit under that.
		if len(trimmed) < 200 && stubHTMLRe.MatchString(trimmed) {
			return stubRejectionMessage(displayPath,
				"the body is just `<h1>X Page</h1>` with no real content")
		}
	case ".py":
		// Functions whose body is `pass` or a single TODO comment.
		if lineCount <= 5 && (regexp.MustCompile(`(?m)^\s*pass\s*$`).MatchString(trimmed) ||
			regexp.MustCompile(`(?im)^\s*#\s*TODO\b.*$`).MatchString(trimmed)) {
			if !strings.Contains(trimmed, "import ") && !strings.Contains(trimmed, "def ") && !strings.Contains(trimmed, "class ") {
				return stubRejectionMessage(displayPath,
					"the file body is just `pass` / `# TODO` with no real implementation")
			}
		}
	case ".md", ".markdown":
		if len(trimmed) < 100 && (strings.Contains(strings.ToLower(trimmed), "todo") ||
			strings.Contains(strings.ToLower(trimmed), "placeholder")) {
			return stubRejectionMessage(displayPath,
				"the document is just a TODO/placeholder marker")
		}
	case ".js", ".ts", ".tsx", ".jsx":
		// React component / module that's just an empty fragment or
		// a `<div>Page</div>` placeholder.
		if len(trimmed) < 200 && regexp.MustCompile(`(?is)return\s*\(?\s*<[a-z0-9]+>\s*[A-Za-z]+\s+(page|section|view)\s*</[a-z0-9]+>\s*\)?`).MatchString(trimmed) {
			return stubRejectionMessage(displayPath,
				"the component just returns `<X>Foo Page</X>` with no real markup")
		}
	}
	return ""
}

func stubRejectionMessage(path, why string) string {
	return fmt.Sprintf(
		"write_file refused: %s looks like a placeholder stub — %s. Either (a) read a sibling file in the same directory to model the structure (the project's other %s files almost certainly have the right scaffold), or (b) if the user explicitly asked for an empty placeholder, acknowledge that in your response so they know the file needs to be filled in. Don't ship stubs and call the task done.",
		path, why, strings.TrimPrefix(filepath.Ext(path), "."))
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

// findFileArgSwapHint catches find_file called with the filename in `path`
// and `pattern` empty, and returns the corrected call.
//
// Observed live: the model sent {"path":"app.py"}, got "pattern cannot be
// empty", and sent {"path":".*app\\.py.*"} — moving the regex into `path`
// rather than into `pattern`, because the error named the field it had left
// blank and not the one it had filled. A `path` that carries an extension and
// no separator is a filename, so say which argument it belongs in.
func findFileArgSwapHint(path string) string {
	p := strings.TrimSpace(path)
	if p == "" || strings.ContainsAny(p, `/\`) {
		return "" // a real directory, or nothing to go on
	}
	ext := filepath.Ext(p)
	if ext == "" || len(ext) > 6 {
		return ""
	}
	// Escape it into the regex the caller meant.
	quoted := regexp.QuoteMeta(p)
	return fmt.Sprintf(
		"find_file: `pattern` is empty and `path` is %q, which is a filename, not a directory. "+
			"`path` is WHERE to search (a directory, default the project root) and `pattern` is a "+
			"regex matching the FILENAME. Retry as: find_file {\"pattern\":%q}",
		p, quoted+"$")
}

// foreignRunes reports characters in old_str that appear NOWHERE in the file,
// restricted to non-ASCII. A mismatch caused by one corrupted character is
// invisible in a diff of two long strings, and every other hint sends the model
// to re-copy text it already copied correctly.
//
// Observed live: the model sent `if(headX === food.x ℘ headY ===food.y)` for a
// line reading `&&`. U+2118 SCRIPT CAPITAL P is not a typo, it is a decode
// artefact, and the rejection it got ("your old_str is 9 lines long") was true
// and useless — the length was not why it failed.
//
// ASCII is excluded deliberately: a wrong ASCII character is an ordinary
// mis-copy the closest-line hint already handles. A non-ASCII rune that the
// file does not contain anywhere is nearly always corruption.
func foreignRunes(oldStr, fileContent string) []rune {
	var out []rune
	seen := map[rune]bool{}
	for _, r := range oldStr {
		if r < 0x80 || seen[r] {
			continue
		}
		seen[r] = true
		if !strings.ContainsRune(fileContent, r) {
			out = append(out, r)
		}
	}
	return out
}

// describeForeignRunes renders foreignRunes for the model: the character, its
// codepoint, and where it sits, so the fix is mechanical rather than a re-copy.
func describeForeignRunes(runes []rune) string {
	parts := make([]string, 0, len(runes))
	for _, r := range runes {
		parts = append(parts, fmt.Sprintf("%q (U+%04X)", r, r))
	}
	return strings.Join(parts, ", ")
}

// unverifiedSummary replaces a completion claim the run's own evidence cannot
// support.
//
// The verification gate bounces a `done` three times and then, out of
// bounces, lets it through — so the model's summary reaches the user
// unchanged. Observed 2026-08-02 across three runs: "I updated the snake game
// logic... I also verified that the page loads", over a file whose only
// @app.route had been deleted. The gate had done its job and the claim
// shipped anyway.
//
// Rewriting the summary is mechanical and needs nothing from the model, which
// is why it is done here rather than by asking the model to be more careful.
// Making `done` ungrammatical would be stronger, but that needs strict
// schema-GBNF and Gemma-family models require the loose grammar (a strict
// schema makes them emit `done` instead of calling tools at all).
//
// The model's own words are kept, labelled, because they usually do describe
// the intended change accurately — it is the verification claim inside them
// that is unsupported.
func unverifiedSummary(wrote bool, claim string) string {
	var sb strings.Builder
	if wrote {
		sb.WriteString("Changes were written to disk, but NOTHING in this run verified them — " +
			"no build, test, or probe command completed successfully. Run it yourself before " +
			"relying on it.")
	} else {
		sb.WriteString("Nothing was written to disk in this run, and no verification command " +
			"completed successfully.")
	}
	if c := strings.TrimSpace(claim); c != "" {
		sb.WriteString("\n\nThe agent's own account, which is UNVERIFIED and may describe work " +
			"that did not land:\n")
		sb.WriteString(truncateStr(c, 1200))
	}
	return sb.String()
}

// planIncompleteMessage names the plan steps that never got a matching tool
// call, or "" to let `done` through.
//
// The plan is generated up front, PlanStepsSatisfied tracks which steps have
// been hit, and buildPlanReminder shows the model its progress every turn —
// but nothing checked it at the exit. Run 4 built the variable-delay loop it
// was asked for, never added the per-food decrement, and emitted `done`: two
// required edits, one delivered. The reminder is an instruction and was
// ignored; this is the same fact used as a gate.
//
// Gated on plan quality, because a bad plan blocking a finished task is worse
// than no plan at all. A low-scoring plan, or one whose steps the matcher
// could not track, is not evidence of anything and is skipped.
func planIncompleteMessage(ctx *AgentContext) string {
	if ctx == nil || ctx.Plan == nil || len(ctx.Plan.Steps) < 2 {
		return ""
	}
	if ctx.Plan.WinningScore < planGateMinScore {
		return ""
	}
	if len(ctx.PlanStepsSatisfied) != len(ctx.Plan.Steps) {
		return ""
	}
	// Nothing matched at all means the matcher is not tracking this task, not
	// that the model did nothing.
	if countTrue(ctx.PlanStepsSatisfied) == 0 {
		return ""
	}
	var missing []string
	for i, step := range ctx.Plan.Steps {
		if !ctx.PlanStepsSatisfied[i] {
			missing = append(missing, fmt.Sprintf("  %s: %s", step.ID, step.Action))
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"Cannot declare `done` yet — %d of %d planned steps have landed, and these have not:\n%s\n"+
			"Each one needs its own tool call. Do the next one now. If a step is genuinely "+
			"unnecessary or already satisfied by an edit you made under a different step, say "+
			"which in your next `done` summary rather than leaving it silent.",
		countTrue(ctx.PlanStepsSatisfied), len(ctx.Plan.Steps), strings.Join(missing, "\n"))
}

// planOwesRun reports whether the plan gate would send this exit back for a
// step that runs a command. Such a step may need a job the run started: a
// probe of a server needs the server.
func planOwesRun(ctx *AgentContext) bool {
	if planIncompleteMessage(ctx) == "" {
		return false
	}
	for i, step := range ctx.Plan.Steps {
		if !ctx.PlanStepsSatisfied[i] && (step.Action == "run_command" || step.Action == "run_background") {
			return true
		}
	}
	return false
}

// reForegroundServer matches commands that serve until killed. Deliberately
// narrow: only forms that cannot be anything else. `python app.py` is
// excluded because it is just as likely to be a script that exits, and
// refusing it would block a legitimate verification.
var reForegroundServer = regexp.MustCompile(`(?i)(^|\s|&&|;)\s*(` +
	`python3?\s+-m\s+http\.server` +
	`|php\s+-S\b` +
	`|(python3?\s+-m\s+)?(uvicorn|gunicorn|waitress-serve)\b` +
	`|flask\s+run\b` +
	`|(npm|yarn|pnpm)\s+(start|run\s+(dev|serve|start|preview))\b` +
	`|(npx\s+)?(vite|next\s+dev|http-server|serve)\b` +
	`|rails\s+s(erver)?\b` +
	`|jekyll\s+serve\b` +
	`)`)

// foregroundServerRejection redirects a server start from run_command to
// run_background before it executes.
//
// Observed on the first-contact path — an empty workspace, "create a simple
// portfolio website": the model wrote three files, then ran
// `python3 -m http.server 8000` in the foreground, waited out the full 30s
// sandbox timeout, and only then reached for run_background. That is 30
// seconds of a 3m39s run, every time, on the most common way anyone will
// first try ATLAS.
//
// The guidance already says to use run_background for servers and the model
// still does this, which is the usual result for an instruction. The harness
// can see the command before it runs, so it stops being a suggestion.
// A script the user wrote is only recognisable as a server from its
// contents. These calls do not return until the process is killed, so
// finding one in the file the command runs is evidence rather than a guess
// from the command text — `python app.py` and `python solve.py` are
// indistinguishable otherwise, and refusing on the name would block
// ordinary verification.
var serverLoopMarkers = []string{
	"app.run(", ".serve_forever(", "uvicorn.run(", "socketserver.",
	"httpd.serve", "app.listen(", "server.listen(", "web.run_app(",
}

// reServerScript pulls the workspace file out of an interpreter invocation:
// `python app.py`, `python3 -u srv/app.py`, `node server.js`.
var reServerScript = regexp.MustCompile(
	`(?i)\b(?:python3?|node|ruby|php)\b[^|;&]*?\s([\w./-]+\.(?:py|js|mjs|rb|php))\b`)

// reInterpreterModuleOrInline matches an interpreter invoked with -m (run a
// module) or -c (run inline code): any .py that follows is an argument to
// that program, not the script being run.
var reInterpreterModuleOrInline = regexp.MustCompile(
	`(?i)\b(?:python3?|node)\b(?:\s+-[a-zA-Z]+)*\s+-(?:m|c|e)\b`)

// runsAServerLoop reports whether the file this command executes blocks
// forever. readFile returns workspace contents; a miss says no, since
// refusing on a filename alone is the guess this avoids.
func runsAServerLoop(cmd string, readFile func(string) (string, bool)) bool {
	if readFile == nil {
		return false
	}
	m := reServerScript.FindStringSubmatch(cmd)
	if m == nil {
		return false
	}
	// `python3 -m py_compile app.py` and `python3 -c "import app"` hand the
	// file to a module or an inline program; they never execute its server
	// loop. Observed 2026-09-14: the compile check the model reached for
	// after a SyntaxError was refused as a server start, and the session
	// ended without ever fixing the file.
	//
	// Scoped to the invocation that RUNS the script (m[0]), not the whole
	// command. reServerScript's [^|;&] class already stops m[0] at the
	// segment boundary, so a chained `python3 -m pip install ... && python3
	// app.py` matches only the second segment here — checking the whole
	// command instead let the pip step's `-m` exempt the real server start,
	// and app.py then ran in the foreground until the sandbox timeout
	// (observed 2026-09-14).
	if reInterpreterModuleOrInline.MatchString(m[0]) {
		return false
	}
	src, ok := readFile(m[1])
	if !ok {
		return false
	}
	low := strings.ToLower(src)
	for _, marker := range serverLoopMarkers {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

func foregroundServerRejection(cmd string) string {
	return foregroundServerRejectionWithSource(cmd, nil)
}

// foregroundServerRejectionWithSource is foregroundServerRejection plus the
// workspace reader, so a script that serves is caught alongside the
// launchers that always do.
//
// The pattern list covers the well-known launchers (`http.server`,
// `npm run dev`, `flask run`). It cannot cover a file the user wrote:
// measured 2026-08-04 on flask_pause, `python app.py` sat through the full
// 30s sandbox timeout before the verification gate said anything, and that
// is the shape a real project takes.
func foregroundServerRejectionWithSource(cmd string,
	readFile func(string) (string, bool)) string {
	// A trailing & already detaches, so the call returns immediately and
	// there is nothing to redirect. (`nohup` alone does not: it only
	// ignores SIGHUP, and the command still holds the foreground.)
	if strings.HasSuffix(strings.TrimSpace(cmd), "&") {
		return ""
	}
	if !reForegroundServer.MatchString(cmd) && !runsAServerLoop(cmd, readFile) {
		return ""
	}
	return fmt.Sprintf(
		"`%s` serves until it is killed, so run_command would sit on it until the timeout "+
			"and report a failure that says nothing about your code. Start it with "+
			"run_background instead — it returns a job_id immediately:\n"+
			"  run_background {\"command\": %q}\n"+
			"Then probe it with run_command (`curl -sf http://localhost:<port>/`), and "+
			"stop_background when you are done.",
		truncateStr(cmd, 80), cmd)
}

// outOfTurnsSummary is what the user reads when the loop hits its turn cap.
//
// The cap used to end the turn with an `error` event and nothing else, so a
// question whose recon ran long came back blank. A blank reply is the worst
// outcome the harness can produce: the user cannot tell whether ATLAS is
// broken, still thinking, or ignoring them. Say what happened, say what was
// learned, and name the next move.
func outOfTurnsSummary(ctx *AgentContext, wrote bool) string {
	var sb strings.Builder
	sb.WriteString("I ran out of turns for this request before finishing.")
	if wrote {
		sb.WriteString(" Changes were written to disk — check them before relying on them.")
	} else {
		sb.WriteString(" Nothing was written to disk.")
	}
	if files := ctx.SnapshotFilesRead(); len(files) > 0 {
		names := make([]string, 0, len(files))
		for p := range files {
			if rel, err := filepath.Rel(ctx.WorkingDir, p); err == nil && rel != "" {
				names = append(names, rel)
			} else {
				names = append(names, filepath.Base(p))
			}
		}
		sort.Strings(names)
		fmt.Fprintf(&sb, "\n\nI did get to look at: %s.", strings.Join(names, ", "))
	}
	sb.WriteString("\n\nAsk again and point me at the specific file or function you care " +
		"about — a narrower request finishes inside the budget.")
	return sb.String()
}

// repeatedRefusalSummary ends a run that kept re-sending one rejected call.
//
// The user needs to know the tool call was refused rather than attempted, and
// that re-running the same prompt will do the same thing — otherwise the
// obvious response is to try again verbatim.
func repeatedRefusalSummary(tool, path string, wrote bool) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Stopped: the same `%s` call was re-sent after being refused, and kept being "+
		"re-sent without changing.", tool)
	if path != "" {
		fmt.Fprintf(&sb, " Target: %s.", path)
	}
	if wrote {
		sb.WriteString(" Earlier changes in this run did land on disk — check them.")
	} else {
		sb.WriteString(" Nothing was written to disk.")
	}
	sb.WriteString("\n\nThe refusal reason is in the per-turn errors above. Re-running this " +
		"prompt unchanged will hit the same wall; say specifically what to change and where, " +
		"or point at a different file.")
	return sb.String()
}

// repeatTerminalSummary is the terminal for the repeat detector, and the one
// place that decides whether a run may say its change landed.
//
// `wrote` (madeProductiveChange) is a PROGRESS HINT. It records that some
// mutation succeeded earlier in the run; it says nothing about the bytes on
// disk now, and it may never authorize a completion claim. Measured on the
// seed-20260901 confirmation, task debounce5: an accepted write set the hint,
// the model then repeated a failing verification, and the terminal read "Made
// your change ... the change is on disk" over a file containing a
// SyntaxError. One false success in 50 sessions, and the only terminal in
// that run which misreported its own outcome.
//
// Completion is therefore derived from a FRESH observation of the declared
// deliverable's CURRENT bytes, through the same syntax contract the write
// path uses. Existence is not validity, an earlier success is not this
// content, and anything short of an explicit pass -- not_run, not_applicable,
// unknown, unreadable, or nothing declared -- is undemonstrated and stops.
// recovered carries the per-path outcomes of Phase 3B restoration, which the
// caller performs before composing this summary. It is disclosed as its own
// clause: recovery changes what is on disk, never whether the run finished.
func repeatTerminalSummary(ctx *AgentContext, expected []string, wrote bool,
	recovered []restoreDecision) string {
	var sb strings.Builder
	sb.WriteString("Stopped: the same tool call kept repeating without making progress")
	// Validation status alters DISCLOSURE only. A repeat-breaker is an
	// operational failure whatever the bytes look like: syntax is not task
	// completion, and the run stopped without finishing its verification.
	// Neither branch may read as a completion claim.
	switch {
	case !wrote:
		sb.WriteString(", and nothing was written to disk")
	case deliverablesDemonstrablyValid(ctx, expected):
		sb.WriteString(". Your work is on disk and parses, but the " +
			"verification did not complete, so this run cannot say the task is done")
	default:
		sb.WriteString(". Earlier changes did land on disk, but the current " +
			"contents were not shown to be valid — treat them as unverified")
	}
	sb.WriteString(". Try a more specific instruction (e.g. name the file and " +
		"the exact change).")
	// Appended last, and never in place of the stop: a restored file is a
	// safer starting point, not a completed task.
	sb.WriteString(restorationDisclosure(recovered))
	return sb.String()
}

// deliverablesDemonstrablyValid answers the completion question and nothing
// else: does every declared deliverable, as it exists RIGHT NOW, pass the
// syntax contract? With nothing declared there is nothing demonstrated, so
// the answer is no.
func deliverablesDemonstrablyValid(ctx *AgentContext, expected []string) bool {
	if len(expected) == 0 {
		return false
	}
	for _, rel := range expected {
		// A deliverable that resolves outside the workspace demonstrates nothing.
		content, resolved, err := readWorkspaceFile(ctx, rel)
		if err != nil {
			return false
		}
		checked := fallbackSyntaxOutcomeFor(ctx, resolved, string(content))
		// A script the file embeds that the harness found broken is a
		// demonstrated failure, whatever the whole-file parse says: a Flask
		// app whose HTML_TEMPLATE has a stray `)` in its <script> compiles,
		// serves, and breaks in the browser. Only a demonstrated one: an
		// embedded check that could not run is no finding (the documented
		// fail-soft).
		if checked.Embedded.Status == ValidationFailed {
			return false
		}
		status := checked.WholeFile.Status
		if status == ValidationPassed {
			continue
		}
		// A document has no syntax to pass. Requiring one meant a valid
		// notes.txt could never demonstrate anything, however current its
		// bytes were. What stands in for the pass is existence plus currency:
		// the checker reports that nothing applies, the path is prose rather
		// than unsupported code, and the ledger's record still describes the
		// bytes that are there. This is not a syntax pass and is never
		// relabelled as one.
		if status == ValidationNotApplicable && documentDeliverableCurrent(ctx, resolved, content) {
			continue
		}
		return false
	}
	return true
}

// documentDeliverableCurrent is the existence-and-currency evidence a non-code
// deliverable can offer in place of a syntax pass.
//
// Every clause is required. Prose only, so an unsupported language or an
// unknown extension holding logic is excluded; the ledger must already own the
// path, so nothing the session never wrote qualifies; its recorded verdict
// must be exactly "checked nothing because nothing applies"; and that record
// has to describe the bytes on disk right now.
func documentDeliverableCurrent(ctx *AgentContext, resolved string, content []byte) bool {
	if !isDocumentAsset(resolved) {
		return false
	}
	key := ledgerKey(ctx, resolved)
	ctx.LedgerMu.Lock()
	d := ctx.Ledger[key]
	var kind ValidationKind
	var status ValidationStatus
	var tombstoned bool
	var current string
	if d != nil {
		kind, status = d.CurrentValidation()
		tombstoned, current = d.Tombstoned, d.CurrentHash
	}
	ctx.LedgerMu.Unlock()
	if d == nil || tombstoned {
		return false
	}
	if kind != ValidationKindNone || status != ValidationNotApplicable {
		return false
	}
	return current == hashBytes(content)
}

// inferenceFailureSummary is what the user reads when the model call itself
// fails — the stream cannot continue, so this is the last thing they get.
//
// The context-size 400 is called out by name because it is actionable and
// because it was the deterministic killer: `aoc_sonar` hit it in both reps at
// turn 3, and the run ended with an `error` event and no outcome at all.
func inferenceFailureSummary(err error, wrote bool) string {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	var sb strings.Builder
	if strings.Contains(msg, "exceed_context_size") || strings.Contains(msg, "exceeds the available context size") {
		sb.WriteString("Stopped: the conversation outgrew the model's context window, so the " +
			"request was refused before it ran. This usually means a large file was read " +
			"into the session. Start a fresh request naming just the file and change you " +
			"want, or raise the server's context size.")
	} else {
		sb.WriteString("Stopped: the model call failed, so the run could not continue.")
		if msg != "" {
			fmt.Fprintf(&sb, "\n\n%s", truncateStr(msg, 300))
		}
	}
	if wrote {
		sb.WriteString("\n\nChanges made earlier in this run are on disk — check them before re-running.")
	} else {
		sb.WriteString("\n\nNothing was written to disk.")
	}
	return sb.String()
}

// nothingWrittenSummary states that a run which was asked to change
// something finished without changing anything.
//
// The action gate bounces this while it can, then stops — its bounces are
// capped so an exhausted gate cannot loop. Past the cap the exit is
// unremarked, and the worst version is a summary that looks like success:
// observed on smallrung_toml, a refused structural_edit, the model giving
// up on tools and emitting the replacement as chat text, and that code
// arriving as the run's summary. A user reading it has no way to tell the
// change was never applied.
//
// Prefixed rather than replacing: whatever the model said may still be
// useful, it just cannot stand as a completion claim.
func nothingWrittenSummary(original string) string {
	const lead = "Nothing was written — no file was created or changed in this run. " +
		"Any code below is a proposal, not something on disk."
	if strings.TrimSpace(original) == "" {
		return lead
	}
	return lead + "\n\n" + original
}

// noChangeNote is what a completed run that looked into the project and
// changed nothing says about it. Nothing demanded a change, or the stronger
// nothingWrittenSummary would apply; but a request misread as a question
// ended completed with the model's "Updated calc.py" as its whole summary,
// and nothing told the user no file had changed.
const noChangeNote = "No file was created or changed in this run."

// withNoChangeNote appends noChangeNote to a completed run's summary when the
// run inspected the workspace and is known to have changed nothing. A reply
// that never looked at the project (a greeting, a general question) is left
// as it is.
func withNoChangeNote(ctx *AgentContext, st *runState, status TerminalStatus, summary string) string {
	if !status.Completed() || st == nil || !st.inspectedWorkspace || !runChangedNothing(ctx, st) {
		return summary
	}
	if strings.TrimSpace(summary) == "" {
		return noChangeNote
	}
	return summary + "\n\n" + noChangeNote
}

// runChangedNothing reports whether this run is known to have changed no
// file: no tool write, nothing the shell wrote or removed, and no part of the
// workspace the shell touched left unobserved. A shell-written file sets no
// tool flag, so the ledger is what says whether anything changed.
func runChangedNothing(ctx *AgentContext, st *runState) bool {
	if ctx == nil || st == nil || st.madeProductiveChange || ctx.ShellEffectsUnobserved {
		return false
	}
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	for _, d := range ctx.Ledger {
		if d.Tombstoned || d.Generation > 0 {
			return false
		}
	}
	return true
}

// verifiedPhase reports whether a V3 phase_solved value means a candidate
// actually passed verification.
//
// The field is initialised to "none" and only overwritten when something
// passes, so "not empty" is not the same question as "verified" — "none" is
// a perfectly good non-empty string. Listing the phases that mean success is
// the check that cannot drift: a new phase added upstream is unverified here
// until it is named, which is the safe direction.
func verifiedPhase(phase string) bool {
	switch phase {
	case "probe", "phase1", "pr_cot", "refinement", "budget":
		return true
	}
	return false
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

// toolBanNote tells the model a tool is gone for a file and names what is
// left. Written as a fact rather than a suggestion, because the suggestion
// form was measured to be ignored: an explicit "re-sending will not help,
// use structural_edit" was followed by the identical call on the next turn.
func toolBanNote(tool, path string) string {
	// Never name the tool that was just banned. The alternatives used to be
	// chosen by file extension alone, so banning structural_edit on a .py
	// file produced "structural_edit is no longer available for pricing.py
	// ... Use `structural_edit`" -- the one instruction the model cannot
	// follow. Measured on acceptance task L: the session was handed exactly
	// that and ended repeated_refusal with nothing written to disk.
	structuralFits := false
	if ext := strings.ToLower(filepath.Ext(path)); ext == ".py" || ext == ".html" || ext == ".htm" {
		structuralFits = true
	}
	candidates := []struct{ name, how string }{
		{"structural_edit", "`structural_edit` (the selector of the node you are changing plus its new body — no old_str to reproduce)"},
		{"replace_lines", "`replace_lines` (assert only the first and last line of the range)"},
		{"write_file", "`write_file` with the complete new contents"},
	}
	var alts []string
	for _, c := range candidates {
		if c.name == tool {
			continue
		}
		if c.name == "structural_edit" && !structuralFits {
			continue
		}
		alts = append(alts, c.how)
	}
	return fmt.Sprintf(
		"%s is no longer available for %s in this session: it was sent and rejected unchanged, so it is not a path to a working edit here. Use %s.",
		tool, path, strings.Join(alts, " or "))
}

// toolBanNoteFor is toolBanNote plus the selectors that exist in the file
// when structural_edit is one of the alternatives it names, so the suggestion
// can be acted on without inventing a name.
func toolBanNoteFor(ctx *AgentContext, tool, path string) string {
	note := toolBanNote(tool, path)
	if tool == "structural_edit" || !strings.Contains(note, "`structural_edit`") {
		return note
	}
	b, err := os.ReadFile(resolveAgentPath(ctx, path))
	if err != nil {
		return note
	}
	if g := selectorGuidance(path, string(b)); g != "" {
		note += " For structural_edit, " + g + "."
	}
	return note
}

// --- explicit read-only requests --------------------------------------------

// mutationForbidden reports whether the USER forbade changing the workspace on
// this request.
//
// The signal is an explicit prohibition in the user's own words ("just explain
// — do not change any code"), not a question shape and not the task contract's
// `question` mode. Those two mean a state change is not REQUIRED, which is a
// different claim from "a state change is not PERMITTED"; treating them as a
// prohibition would refuse legitimate work on any request phrased as a
// question. Requiring the prohibition keeps the guarantee narrow enough to be
// safe and specific enough to be worth enforcing.
// Reads ctx.HumanTask -- the human's actual instruction, captured before the
// loop appends correctives, manifests or re-injected content, so a system note
// can never be mistaken for the user forbidding a change.
func mutationForbidden(ctx *AgentContext) bool {
	if ctx == nil {
		return false
	}
	return isExplainOnlyMessage(strings.ToLower(ctx.HumanTask))
}

// readOnlyRequestRefusal returns the refusal text when a mutation-capable tool
// is called under an explicit read-only request, or "" to allow the call.
//
// Keyed on the tool's declared effect, so it holds for direct mutators and for
// arbitrary shell alike. A read-only tool is always allowed: the model still
// needs to read the code to answer, and answering IS the deliverable here --
// a run that writes nothing has no file obligation to demonstrate and can
// complete on its answer.
func readOnlyRequestRefusal(tool *ToolDef, ctx *AgentContext) string {
	if tool == nil || !mutationForbidden(ctx) {
		return ""
	}
	if tool.Effect == ToolEffectReadOnly {
		return ""
	}
	what := "change the workspace"
	if tool.Effect == ToolEffectCommandUnobserved {
		what = "run a command that could change the workspace"
	}
	return fmt.Sprintf(
		"`%s` was not run: this request explicitly asked you NOT to change anything, "+
			"and %s would %s.\n\n"+
			"Answer the question instead. Read what you need (read_file, outline_file, "+
			"search_files, list_directory, find_file) and reply with "+
			"{\"type\":\"text\",\"content\":\"<your answer>\"} — an answer is the "+
			"deliverable for this request, so no file needs to change for it to be complete.",
		tool.Name, tool.Name, what)
}

// verificationNeverRan reports that a verification command failed before it
// could exercise anything: the runner could not find what it was pointed at,
// or collected nothing to run.
//
// That is not evidence the artifact is broken, so it must not latch the
// verification gate. It is not evidence the artifact works either, so it never
// clears the gate. Strictly neutral.
//
// Measured on smallrung_toml rep1: the model had ALREADY verified green
// ("Successfully parsed: {'title': 'Test'}"), then invented
// `python3 -m pytest tests/test_syntax_check.py`. Its own next call,
// find_file on tests/, returned 0 matches -- the directory never existed.
// pytest collected nothing, the gate latched on that as a red test, and the
// session spent the rest of its 570 s budget chasing a directory that was not
// there, ending work_deadline with a correct edit already on disk.
//
// Deliberately keyed ONLY on messages a runner emits about its own arguments.
// A generic "no such file or directory" is excluded on purpose: a test that
// fails because the program under test did not create a file says exactly
// that, and neutralising it would let a genuine failure through.
func verificationNeverRan(result *ToolResult) bool {
	if result == nil {
		return false
	}
	hay := strings.ToLower(result.Error + " " + string(result.Data))
	for _, runnerSaysItFoundNothing := range []string{
		"no tests ran",
		"no tests collected",
		"error: file or directory not found",
		"can't open file",
	} {
		if strings.Contains(hay, runnerSaysItFoundNothing) {
			return true
		}
	}
	return false
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
		if doubledEscapeBody(path, v) {
			return fmt.Sprintf("Your %s call was NOT performed — nothing was written. The %s for %s is a single "+
				"%d-character line whose only line breaks are the two characters \\n, so the escapes were "+
				"doubled. Resend with each line break escaped once (\\n), or send the file with "+
				"\"content\": \"@fenced\".", toolName, f, path, len(v)), true
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

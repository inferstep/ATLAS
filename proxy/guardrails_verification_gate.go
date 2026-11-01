// The verification gate and the run-first gate. This file holds which runs
// count as verification and which do not (a headers-only probe, a program fed
// through a stdin redirect, a silent run where the task promised output, a
// runner that found nothing to run, a V3 phase that is not a pass), whether a
// command ran a given file and how to run one, and what the gate says when it
// refuses `done`. The work-contract verification demand itself is in
// guardrails.go, and so is driftedSinceVerification, which finds a file that
// changed after the run that verified it.

package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

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

// isHeadOnlyProbe reports a probe that fetches headers and no body. Such a
// probe is not verification (see commandEvidenceKind.verifies) — this says so out
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

// headOnlyProbeRe matches curl/wget invocations that fetch headers only:
// `curl -I`, `curl --head`, `wget --spider`. Long-form and clustered short
// flags (-sI) both count.
var headOnlyProbeRe = regexp.MustCompile(`(?i)\b(?:curl\b[^|;&]*?\s-{1,2}(?:I\b|head\b)|curl\b[^|;&]*?\s-[a-zA-Z]*I[a-zA-Z]*\b|wget\b[^|;&]*?--spider\b)`)

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
		if segmentRunsFile(segment, path, base) {
			return true
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

// segmentRunsFile reports whether one execution segment of a command runs the
// file: `./solve.py` directly, or an executor named before the file's token.
func segmentRunsFile(segment, path, base string) bool {
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
		return false // absent, or the file itself is the first token without ./
	}
	for _, prev := range toks[:fileAt] {
		prev = strings.Trim(prev, `"'`)
		if executorNames[filepath.Base(prev)] {
			return true
		}
	}
	return false
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

// verificationRejectionFor is the rejection with the red-run streak that decides
// between edit-the-fix and start-over advice, and one more fact when the run has
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

// verificationRejection tells the model exactly what's missing and what to run.
// We prefer concrete suggestions over abstract "verify your work" prompts — the
// model is more likely to pick a sensible command when given a category.
//
// sawFailedVerification distinguishes the two ways this gate fires. When a
// verification command has actually gone red in this loop, the run holds
// concrete evidence of breakage, so the message says that rather than
// describing the request — the model has already seen the failure and needs
// to act on it, not be told what verification is.
//
// serverBlocked and bgJobID are the two facts that change the advice: whether
// the red command was a blocking server start, and the job id if one is already
// running in the background.
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

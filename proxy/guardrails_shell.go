// Guardrails on shell commands for run_command and run_background: the one
// command policy (it refuses only the catastrophic commands), the drift to a
// /workspace path that is not the project, the hint for a command the shell
// could not parse, the split of a command line into segments, and the
// redirect of a foreground server start to run_background. The list of
// delete targets that would wipe the project, isCatastrophicDeleteTarget, is
// in guardrails.go.

package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

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
		inSingle, inDouble = shellQuoteState(c, inSingle, inDouble)
		if !inSingle && !inDouble {
			if (c == '&' || c == '|') && i+1 < len(cmd) && cmd[i+1] == c {
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

// shellQuoteState is the quoting after one more character of a command line:
// a single quote opens or closes single quotes when it is not inside double
// quotes, and a double quote does the same for double quotes.
func shellQuoteState(c byte, inSingle, inDouble bool) (bool, bool) {
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
	return inSingle, inDouble
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
	// loop, so a compile check after a SyntaxError is not a server start.
	//
	// Scoped to the invocation that RUNS the script (m[0]), not the whole
	// command. reServerScript's [^|;&] class already stops m[0] at the
	// segment boundary, so a chained `python3 -m pip install ... && python3
	// app.py` matches only the second segment here — checking the whole
	// command instead would let the pip step's `-m` exempt the real server start.
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

// foregroundServerRejectionWithSource redirects a server start from run_command
// to run_background before it executes.
//
// A model told to use run_background for servers can still run one in the
// foreground and wait out the full sandbox timeout. The harness can see the
// command before it runs, so it stops being a suggestion.
//
// The workspace reader catches a script that serves, alongside the launchers
// that always do.
//
// The pattern list covers the well-known launchers (`http.server`,
// `npm run dev`, `flask run`). It cannot cover a file the user wrote, and
// `python app.py` is the shape a real project takes.
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

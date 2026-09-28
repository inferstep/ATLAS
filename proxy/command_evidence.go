package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// What a shell command demonstrates when it exits.
//
// A green exit proves only what the command that produced it did. Recording
// every green line whose first word was `python`, `mypy` or `curl` as a
// verification let a parse (`python -m py_compile app.py`), a linter, a
// `--version`, or a test whose failure was hidden by `| tail` discharge the
// verification gate and the work contract, and settle the ledger as
// "executed clean". The program never ran, and the run still ended
// `completed`.
//
// classifyCommandEvidence answers two separate questions about one command
// line: what kind of evidence its verifying part produces, and whether that
// part's exit status actually reaches the status the line reports.

// commandEvidenceKind orders what a successful command can show, weakest
// first.
type commandEvidenceKind int

const (
	// evidenceNone: recon or setup (ls, cat, echo, cd, pip install). Proves
	// nothing about the artifact.
	evidenceNone commandEvidenceKind = iota
	// evidenceStatic: the code was parsed, linted, type-checked, formatted or
	// compiled, or a tool printed its version or help. It says the code is
	// well formed, not that it does anything.
	evidenceStatic
	// evidenceProbe: a response body was fetched from a running service.
	evidenceProbe
	// evidenceExecution: the program, or its tests, actually ran.
	evidenceExecution
)

func (k commandEvidenceKind) String() string {
	switch k {
	case evidenceStatic:
		return "static"
	case evidenceProbe:
		return "probe"
	case evidenceExecution:
		return "execution"
	}
	return "none"
}

// verifies reports whether the kind can stand as verification that the
// program works. Static checks cannot.
func (k commandEvidenceKind) verifies() bool {
	return k == evidenceProbe || k == evidenceExecution
}

// commandEvidence is the classification of one command line.
type commandEvidence struct {
	// Kind is the strongest kind among the segments whose exit status reaches
	// the line's own status. A segment whose status is hidden contributes
	// nothing here.
	Kind commandEvidenceKind
	// Honest holds those segments, in order, each with the here-document it
	// reads, if any. A failed line is charged to all of them.
	Honest []string
	// Covering is the part of Honest that a passing line binds files from: the
	// segments that ran or probed something, and a compile step whose output a
	// later segment ran (`javac App.java && java App`), each preceded by the
	// `cd` that set where it ran. A parse or a lint in the same chain names a
	// file it never ran, and binds nothing.
	Covering []string
	// Masked is set when a verifying segment's result cannot reach the line's
	// status: `pytest | tail`, `pytest || true`, `python app.py; echo`,
	// `python app.py &`, or a curl that exits 0 on an error page. MaskNote
	// says what hid it and what to run instead, for the model.
	Masked   bool
	MaskNote string
}

// classifyCommandEvidence splits a command at its top-level shell operators
// and classifies each segment.
//
// A segment's failure reaches the line's exit status only if every operator
// after it is `&&`: the first failure stops the chain and the non-zero status
// survives to the end. Anything else hides it. `|` reports the last command's
// status (the sandbox runs `bash -c` without pipefail), `;` and a newline
// report whatever ran last, `||` replaces a failure with the fallback's
// result, and `&` backgrounds the command so its status is never observed.
func classifyCommandEvidence(command string) commandEvidence {
	segs, ops, bodies := splitTopLevelShell(command)
	// `bash -c "pytest | tail"` is the command inside the quotes.
	if len(segs) == 1 && bodies[0] == "" {
		if script, ok := shellScript(segs[0]); ok {
			return classifyCommandEvidence(script)
		}
	}
	// `set -e` makes `;` stop at the first failure, and `set -o pipefail`
	// makes a pipeline report its first failure, so neither hides one.
	errexit, pipefail := shellOptions(segs)
	var ev commandEvidence
	var bare, cdBefore []string
	var kinds []commandEvidenceKind
	lastCd := ""
	for i, seg := range segs {
		kind := segmentEvidenceKind(seg)
		if kind == evidenceNone {
			if w := shellFields(strings.TrimSpace(seg)); len(w) > 0 && w[0] == "cd" {
				lastCd = seg
			}
			continue
		}
		hiddenBy, asserted := "", false
		for j := i; j < len(ops) && hiddenBy == ""; j++ {
			op := ops[j]
			if j+1 >= len(segs) {
				// A trailing operator with nothing after it: `pytest;` still
				// reports pytest. A trailing `&` backgrounds it.
				if op == "&" {
					hiddenBy = "&"
				}
				break
			}
			switch {
			case op == "|" && j == i && kind == evidenceProbe && pipeAssertion(segs[j+1]):
				// `curl -s localhost:5000/ | grep -q Welcome`: the pipeline's
				// status is a check on the body the probe fetched.
				asserted = true
			case op == "&&", op == ";" && errexit, op == "|" && pipefail:
			case op == "||" && failurePropagates(segs[j+1]):
				// `pytest || exit 1`: the fallback re-raises the failure.
			case op == "&":
				hiddenBy = "&"
			default:
				hiddenBy = strings.TrimSpace(op + " " + firstWord(segs[j+1]))
			}
		}
		note := ""
		switch {
		case hiddenBy == "&":
			note = fmt.Sprintf("`&` runs `%s` in the background, so its exit status is never seen. Run it in "+
				"the foreground, or start a server with run_background and probe it.", programName(seg))
		case hiddenBy != "":
			note = fmt.Sprintf("`%s` replaces the exit status of `%s`, so a failure would still have looked "+
				"like success. Run it on its own; long output is truncated for you anyway.", hiddenBy, programName(seg))
		case kind == evidenceProbe && !asserted && probeStatusBlind(seg):
			note = fmt.Sprintf("`%s` exits 0 even when the server answers with an error page, so a broken "+
				"page would still have looked like success. Use `curl -sf <url>`, or pipe the body into "+
				"`grep` for what the page must contain.", programName(seg))
		}
		if note != "" {
			if !ev.Masked {
				ev.Masked, ev.MaskNote = true, note
			}
			continue
		}
		honest := seg
		if bodies[i] != "" {
			honest += "\n" + bodies[i]
		}
		ev.Honest = append(ev.Honest, honest)
		bare = append(bare, seg)
		cdBefore = append(cdBefore, lastCd)
		kinds = append(kinds, kind)
		if kind > ev.Kind {
			ev.Kind = kind
		}
	}
	lastRun := -1
	for i, k := range kinds {
		if k == evidenceExecution {
			lastRun = i
		}
	}
	placed := ""
	for i, k := range kinds {
		if k.verifies() || (i < lastRun && compileStep(bare[i])) {
			// Where it ran decides what a runner discovers (runnerEntries).
			if cdBefore[i] != "" && cdBefore[i] != placed {
				ev.Covering = append(ev.Covering, cdBefore[i])
				placed = cdBefore[i]
			}
			ev.Covering = append(ev.Covering, ev.Honest[i])
		}
	}
	return ev
}

// shellOptions reads `set -e` / `set -o errexit` and `set -o pipefail` from
// the segments that set shell options.
func shellOptions(segs []string) (errexit, pipefail bool) {
	for _, seg := range segs {
		f := shellFields(strings.TrimSpace(seg))
		if len(f) == 0 || f[0] != "set" {
			continue
		}
		for i, a := range f[1:] {
			switch {
			case a == "-o" && i+2 < len(f):
				switch f[i+2] {
				case "errexit":
					errexit = true
				case "pipefail":
					pipefail = true
				}
			case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--"):
				if strings.Contains(a, "e") {
					errexit = true
				}
				if strings.Contains(a, "o") && i+2 < len(f) && f[i+2] == "pipefail" {
					pipefail = true
				}
			}
		}
	}
	return errexit, pipefail
}

// pipeAssertion reports a pipeline stage that checks what it receives rather
// than passing it through: `grep` (not -v, -c or -L), `diff`, `cmp`, `jq -e`.
func pipeAssertion(seg string) bool {
	f := programWords(shellFields(strings.TrimSpace(seg)))
	if len(f) == 0 {
		return false
	}
	switch filepath.Base(f[0]) {
	case "grep", "egrep", "fgrep", "rg":
		for _, a := range f[1:] {
			if a == "-v" || a == "-c" || a == "-L" || a == "--invert-match" || a == "--count" ||
				(strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsAny(a, "vcL")) {
				return false
			}
		}
		return true
	case "diff", "cmp":
		return true
	case "jq":
		for _, a := range f[1:] {
			if a == "-e" || a == "--exit-status" {
				return true
			}
		}
	}
	return false
}

// probeStatusBlind reports an HTTP client that exits 0 whatever status the
// server answers with: curl without -f, httpie without --check-status. wget
// fails on an HTTP error by default.
func probeStatusBlind(seg string) bool {
	words := programWords(shellFields(strings.TrimSpace(seg)))
	if len(words) == 0 {
		return false
	}
	switch filepath.Base(words[0]) {
	case "curl":
		for _, a := range words[1:] {
			if a == "--fail" || a == "--fail-with-body" ||
				(len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.Contains(a, "f")) {
				return false
			}
		}
		return true
	case "http", "https", "httpie":
		for _, a := range words[1:] {
			if a == "--check-status" {
				return false
			}
		}
		return true
	}
	return false
}

// failurePropagates reports a `||` fallback that fails again: `exit 1`,
// `exit $?`, `false`.
func failurePropagates(seg string) bool {
	f := strings.Fields(strings.TrimSpace(seg))
	if len(f) == 0 {
		return false
	}
	switch f[0] {
	case "false":
		return true
	case "exit":
		return len(f) > 1 && f[1] != "0"
	}
	return false
}

// firstWord is the word a segment starts with, for messages.
func firstWord(seg string) string {
	f := shellFields(seg)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// programName is the program a segment runs, past wrappers like timeout.
func programName(seg string) string {
	if w := programWords(shellFields(strings.TrimSpace(seg))); len(w) > 0 {
		return w[0]
	}
	return firstWord(seg)
}

// hereDoc is a pending here-document: its delimiter, whether `<<-` strips
// leading tabs, and the segment that reads it.
type hereDoc struct {
	delim     string
	stripTabs bool
	seg       int
}

// splitTopLevelShell splits a command line into segments and the operators
// between them: ops[i] separates segs[i] and segs[i+1]. Quotes, backslash
// escapes, parentheses, $( ) and backticks are respected, so an operator
// inside a quoted string or a subshell is not a separator. `2>&1`, `&>` and
// `>&` are redirections, not the background operator. A here-document body is
// not shell text: it is returned in bodies[i] for the segment that reads it,
// and never split. Best effort, like splitShellSegments, which it does not
// replace: that one drops the operators, and the operators are the point
// here.
func splitTopLevelShell(cmd string) (segs, ops, bodies []string) {
	var cur strings.Builder
	inSingle, inDouble, inBacktick := false, false, false
	depth := 0
	var pending []hereDoc
	bodyAt := map[int]*strings.Builder{}
	flush := func(op string) {
		segs = append(segs, cur.String())
		ops = append(ops, op)
		cur.Reset()
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if c == '\\' && !inSingle && i+1 < len(cmd) {
			cur.WriteByte(c)
			cur.WriteByte(cmd[i+1])
			i++
			continue
		}
		switch {
		case c == '\'' && !inDouble && !inBacktick:
			inSingle = !inSingle
		case c == '"' && !inSingle && !inBacktick:
			inDouble = !inDouble
		case c == '`' && !inSingle:
			inBacktick = !inBacktick
		}
		quoted := inSingle || inDouble || inBacktick
		if !quoted {
			if c == '(' {
				depth++
			} else if c == ')' && depth > 0 {
				depth--
			}
		}
		if quoted || depth > 0 {
			cur.WriteByte(c)
			continue
		}
		next := byte(0)
		if i+1 < len(cmd) {
			next = cmd[i+1]
		}
		prev := byte(0)
		if i > 0 {
			prev = cmd[i-1]
		}
		if c == '<' && next == '<' {
			if i+2 < len(cmd) && cmd[i+2] == '<' {
				cur.WriteString("<<<") // a here-string, not a here-document
				i += 2
				continue
			}
			end, doc := readHereDocOperator(cmd, i)
			cur.WriteString(cmd[i:end])
			if doc.delim != "" {
				doc.seg = len(segs)
				pending = append(pending, doc)
			}
			i = end - 1
			continue
		}
		switch {
		case c == '&' && next == '&':
			flush("&&")
			i++
			continue
		case c == '|' && next == '|':
			flush("||")
			i++
			continue
		case c == '|' && next == '&':
			flush("|")
			i++
			continue
		case c == '|':
			flush("|")
			continue
		case c == '\n' && len(pending) > 0:
			// The bodies start on the next line. The newline itself still
			// ends the command line.
			i = readHereDocBodies(cmd, i+1, pending, bodyAt)
			pending = nil
			flush(";")
			continue
		case c == ';' || c == '\n':
			flush(";")
			continue
		case c == '&' && prev != '>' && prev != '<' && next != '>':
			flush("&")
			continue
		}
		cur.WriteByte(c)
	}
	segs = append(segs, cur.String())
	// Drop empty trailing segments ("pytest;" or "cmd &") but keep the
	// operator that preceded them: a trailing `&` still backgrounds the
	// command before it.
	for len(segs) > 0 && strings.TrimSpace(segs[len(segs)-1]) == "" && len(segs) > len(ops) {
		segs = segs[:len(segs)-1]
	}
	bodies = make([]string, len(segs))
	for idx, b := range bodyAt {
		if idx < len(bodies) {
			bodies[idx] = b.String()
		}
	}
	return segs, ops, bodies
}

// readHereDocOperator reads `<<WORD`, `<<-WORD`, `<<'WORD'` or `<<"WORD"`
// starting at i, and returns the index just past it with the delimiter it
// names.
func readHereDocOperator(cmd string, i int) (int, hereDoc) {
	var doc hereDoc
	j := i + 2
	if j < len(cmd) && cmd[j] == '-' {
		doc.stripTabs = true
		j++
	}
	for j < len(cmd) && (cmd[j] == ' ' || cmd[j] == '\t') {
		j++
	}
	var delim strings.Builder
	for j < len(cmd) {
		d := cmd[j]
		if d == '\'' || d == '"' {
			end := strings.IndexByte(cmd[j+1:], d)
			if end < 0 {
				j = len(cmd)
				break
			}
			delim.WriteString(cmd[j+1 : j+1+end])
			j += end + 2
			continue
		}
		if d == '\\' && j+1 < len(cmd) {
			delim.WriteByte(cmd[j+1])
			j += 2
			continue
		}
		if strings.IndexByte(" \t\r\n;&|<>()", d) >= 0 {
			break
		}
		delim.WriteByte(d)
		j++
	}
	doc.delim = delim.String()
	return j, doc
}

// readHereDocBodies consumes, in order, the bodies of the pending
// here-documents that start at p, and returns the index of the last byte
// consumed. An unterminated body runs to the end of the command.
func readHereDocBodies(cmd string, p int, docs []hereDoc, bodyAt map[int]*strings.Builder) int {
	for _, doc := range docs {
		b := bodyAt[doc.seg]
		if b == nil {
			b = &strings.Builder{}
			bodyAt[doc.seg] = b
		}
		for p < len(cmd) {
			line, next := cmd[p:], len(cmd)
			if end := strings.IndexByte(line, '\n'); end >= 0 {
				line, next = line[:end], p+end+1
			}
			p = next
			check := strings.TrimRight(line, "\r")
			if doc.stripTabs {
				check = strings.TrimLeft(check, "\t")
			}
			if check == doc.delim {
				break
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(line)
		}
	}
	return p - 1
}

// shellFields splits one segment into words, honouring quotes and dropping
// them. Redirections stay as their own words.
func shellFields(seg string) []string {
	var out []string
	var cur strings.Builder
	inSingle, inDouble, have := false, false, false
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		switch {
		case c == '\\' && !inSingle && i+1 < len(seg):
			cur.WriteByte(seg[i+1])
			i++
			have = true
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			have = true
		case c == '"' && !inSingle:
			inDouble = !inDouble
			have = true
		case (c == ' ' || c == '\t' || c == '\n' || c == '\r') && !inSingle && !inDouble:
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteByte(c)
			have = true
		}
	}
	if have {
		out = append(out, cur.String())
	}
	return out
}

var envAssignmentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// runnerValueFlags are `uv run` / `poetry run` options that take a value.
var runnerValueFlags = map[string]bool{
	"--with": true, "--with-requirements": true, "--python": true, "-p": true,
	"--env-file": true, "--directory": true, "--project": true, "--group": true,
	"--extra": true, "--package": true, "--index": true,
}

// programWords strips what runs a program without being it: leading
// environment assignments, the wrappers timeout, env, nice, nohup, time,
// command, exec and stdbuf, and environment runners (`uv run`, `poetry run`,
// `bundle exec`). `timeout 30 python app.py` is a run of app.py.
func programWords(words []string) []string {
	for len(words) > 0 {
		w := words[0]
		switch {
		case envAssignmentRe.MatchString(w):
			words = words[1:]
		case w == "time" || w == "command" || w == "exec" || w == "nohup":
			words = words[1:]
		case w == "env":
			words = words[1:]
			for len(words) > 0 && (strings.HasPrefix(words[0], "-") || envAssignmentRe.MatchString(words[0])) {
				words = words[1:]
			}
		case w == "nice":
			words = words[1:]
			if len(words) > 1 && words[0] == "-n" {
				words = words[2:]
			}
		case w == "stdbuf":
			words = words[1:]
			for len(words) > 0 && strings.HasPrefix(words[0], "-") {
				words = words[1:]
			}
		case w == "timeout":
			words = words[1:]
			for len(words) > 0 && strings.HasPrefix(words[0], "-") {
				if (words[0] == "-s" || words[0] == "-k" || words[0] == "--signal" || words[0] == "--kill-after") && len(words) > 1 {
					words = words[1:]
				}
				words = words[1:]
			}
			if len(words) > 0 {
				words = words[1:] // the duration
			}
		case (w == "uv" || w == "poetry" || w == "pipenv" || w == "pdm" || w == "rye") && len(words) > 1 && words[1] == "run":
			words = words[2:]
			for len(words) > 0 && strings.HasPrefix(words[0], "-") {
				if runnerValueFlags[words[0]] && len(words) > 1 {
					words = words[1:]
				}
				words = words[1:]
			}
			if len(words) > 0 && strings.HasSuffix(words[0], ".py") {
				return append([]string{"python"}, words...) // `uv run app.py`
			}
		case w == "bundle" && len(words) > 1 && words[1] == "exec":
			words = words[2:]
		default:
			return words
		}
	}
	return words
}

// staticTools are programs whose green exit can only mean the code is well
// formed: linters, type-checkers, formatters and compilers.
var staticTools = map[string]bool{
	"mypy": true, "ruff": true, "pylint": true, "flake8": true, "pyflakes": true,
	"pycodestyle": true, "pydocstyle": true, "black": true, "isort": true,
	"autopep8": true, "yapf": true, "bandit": true, "pyright": true,
	"tsc": true, "eslint": true, "prettier": true, "stylelint": true,
	"markdownlint": true, "htmlhint": true, "jsonlint": true, "yamllint": true,
	"shellcheck": true, "hadolint": true, "rubocop": true, "golangci-lint": true,
	"gofmt": true, "goimports": true, "clang-format": true, "clang-tidy": true,
	"cppcheck": true, "javac": true, "kotlinc": true, "rustfmt": true,
	"gcc": true, "g++": true, "cc": true, "c++": true, "clang": true, "clang++": true,
	"rustc": true, "swiftc": true, "ktlint": true, "phpstan": true, "psalm": true,
}

// compilers are the static tools whose output a later command can run.
var compilers = map[string]bool{
	"javac": true, "kotlinc": true, "gcc": true, "g++": true, "cc": true, "c++": true,
	"clang": true, "clang++": true, "rustc": true, "swiftc": true, "tsc": true,
}

// testRunners run a test suite; a green exit means its tests passed.
var testRunners = map[string]bool{
	"pytest": true, "py.test": true, "nose": true, "nosetests": true, "tox": true,
	"jest": true, "vitest": true, "mocha": true, "ava": true, "rspec": true,
	"phpunit": true, "behave": true, "ctest": true,
}

// staticPythonModules are `python -m` targets that only inspect code.
var staticPythonModules = map[string]bool{
	"py_compile": true, "compileall": true, "pyflakes": true, "pylint": true,
	"flake8": true, "mypy": true, "ruff": true, "black": true, "isort": true,
	"pycodestyle": true, "pydocstyle": true, "tabnanny": true, "pyright": true,
	"bandit": true, "json.tool": true,
}

// setupPythonModules are `python -m` targets that install, serve files or
// report on the interpreter. None of them runs the project's code.
var setupPythonModules = map[string]bool{
	"pip": true, "venv": true, "virtualenv": true, "ensurepip": true, "pipx": true,
	"http.server": true, "site": true, "sysconfig": true, "platform": true, "pydoc": true,
}

// staticScriptNames are package scripts that build or check rather than run:
// `npm run build`, `yarn lint:fix`, `pnpm check-types`.
var staticScriptNames = regexp.MustCompile(`^(build|compile|lint|format|fmt|check|typecheck|type-check|tsc|prettier|eslint|stylelint)([:-].*)?$`)

// buildScriptNames are the static scripts that produce something runnable.
var buildScriptNames = regexp.MustCompile(`^(build|compile|tsc)([:-].*)?$`)

// parseOnlyInlineRe matches inline Python whose only work is parsing:
// `python -c "import ast; ast.parse(open('app.py').read())"`.
var parseOnlyInlineRe = regexp.MustCompile(`^\s*(import\s+(?:(?:ast|py_compile|compileall|sys|tokenize)\s*,?\s*)+[;\n]\s*)*(ast\.parse|py_compile\.compile|compileall\.compile_\w+|compile)\s*\(`)

func onlyVersionOrHelp(args []string) bool {
	if len(args) == 0 {
		return false
	}
	for _, a := range args {
		switch a {
		case "--version", "-V", "--help", "-h", "-help", "-version", "version":
		default:
			return false
		}
	}
	return true
}

// segmentEvidenceKind classifies one segment by the program it runs.
func segmentEvidenceKind(seg string) commandEvidenceKind {
	t := strings.TrimSpace(seg)
	// `(cd app && pytest)`: a subshell reports what ran last inside it.
	if inner, ok := leadingSubshell(t); ok {
		return classifyCommandEvidence(inner).Kind
	}
	words := programWords(shellFields(t))
	if len(words) == 0 {
		return evidenceNone
	}
	prog := filepath.Base(words[0])
	args := words[1:]

	if isShellProgram(prog) {
		if script, ok := shellScript(t); ok {
			return classifyCommandEvidence(script).Kind
		}
		return shellArgsKind(args)
	}

	switch {
	case staticTools[prog]:
		return evidenceStatic
	case testRunners[prog]:
		return testRunnerKind(args)
	case prog == "curl" || prog == "wget" || prog == "http" || prog == "https" || prog == "httpie":
		if headOnlyProbeRe.MatchString(t) {
			return evidenceNone
		}
		return evidenceProbe
	case isPythonProgram(prog):
		return pythonEvidenceKind(args)
	case prog == "node" || prog == "nodejs":
		return scriptInterpreterKind(args, []string{"--check", "-c"})
	case prog == "ruby":
		return scriptInterpreterKind(args, []string{"-c"})
	case prog == "perl":
		return scriptInterpreterKind(args, []string{"-c"})
	case prog == "php":
		return scriptInterpreterKind(args, []string{"-l"})
	case prog == "lua" || prog == "luajit" || prog == "Rscript" || prog == "julia" || prog == "elixir":
		return scriptInterpreterKind(args, nil)
	case prog == "deno":
		return subcommandKind(args, []string{"run", "test", "eval", "serve", "task", "bench"},
			[]string{"check", "lint", "fmt", "doc", "compile"})
	case prog == "bun":
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") && !isKnownSubcommand(args[0], "test", "run", "x", "build", "install", "add", "remove", "init", "create", "upgrade") {
			return evidenceExecution // bun app.ts
		}
		return subcommandKind(args, []string{"test", "run", "x"}, []string{"build"})
	case prog == "go":
		return subcommandKind(args, []string{"test", "run"}, []string{"build", "vet", "fmt"})
	case prog == "cargo":
		return subcommandKind(args, []string{"test", "run", "bench"}, []string{"build", "check", "clippy", "fmt", "doc"})
	case prog == "dotnet":
		return subcommandKind(args, []string{"test", "run"}, []string{"build", "format"})
	case prog == "swift":
		if len(args) > 0 && strings.HasSuffix(args[0], ".swift") {
			return evidenceExecution
		}
		return subcommandKind(args, []string{"test", "run"}, []string{"build"})
	case prog == "mvn" || prog == "mvnw" || prog == "gradle" || prog == "gradlew":
		return subcommandKind(args, []string{"test", "verify", "run", "bootRun"}, []string{"compile", "build", "package", "check", "assemble"})
	case prog == "java" || prog == "kotlin" || prog == "scala":
		if onlyVersionOrHelp(args) {
			return evidenceStatic
		}
		if len(args) > 0 {
			return evidenceExecution
		}
		return evidenceNone
	case prog == "npm" || prog == "pnpm" || prog == "yarn":
		return packageScriptKind(prog, args)
	case prog == "npx" || prog == "pnpx" || prog == "bunx":
		for len(args) > 0 && strings.HasPrefix(args[0], "-") {
			args = args[1:] // npx --yes jest
		}
		if len(args) == 0 {
			return evidenceNone
		}
		tool := filepath.Base(args[0])
		switch {
		case staticTools[tool]:
			return evidenceStatic
		case testRunners[tool]:
			return testRunnerKind(args[1:])
		}
		return evidenceExecution
	case prog == "make" || prog == "gmake" || prog == "just" || prog == "task":
		return makeTargetKind(args)
	}
	// A path to a program or script: ./solve.py, bin/app. Checked after the
	// named programs, so /usr/bin/python3 -m py_compile is still a parse.
	if strings.Contains(words[0], "/") && !strings.HasSuffix(words[0], "/") {
		if onlyVersionOrHelp(args) {
			return evidenceStatic
		}
		return evidenceExecution
	}
	return evidenceNone
}

// leadingSubshell returns what a segment runs in a `( … )` subshell, when
// nothing but redirections follows the closing parenthesis. `((` is
// arithmetic, not a subshell.
func leadingSubshell(t string) (string, bool) {
	if !strings.HasPrefix(t, "(") || strings.HasPrefix(t, "((") {
		return "", false
	}
	depth := 0
	inSingle, inDouble := false, false
	for i := 0; i < len(t); i++ {
		c := t[i]
		switch {
		case c == '\\' && !inSingle:
			i++
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case inSingle || inDouble:
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				rest := strings.TrimLeft(strings.TrimSpace(t[i+1:]), "0123456789&")
				if rest == "" || strings.HasPrefix(rest, ">") || strings.HasPrefix(rest, "<") {
					return t[1:i], true
				}
				return "", false
			}
		}
	}
	return "", false
}

// shellScript returns the script a shell segment runs inline — `bash -c
// SCRIPT`, flags combined or not (`bash -lc`, `sh -ec`) — with `set -e` or
// `set -o …` prepended for the options the shell was started with.
func shellScript(seg string) (string, bool) {
	words := programWords(shellFields(strings.TrimSpace(seg)))
	if len(words) == 0 || !isShellProgram(filepath.Base(words[0])) {
		return "", false
	}
	args := words[1:]
	pre := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case (a == "-o" || a == "+o") && i+1 < len(args):
			if a == "-o" {
				pre += "set -o " + args[i+1] + "\n"
			}
			i++
			continue
		case strings.HasPrefix(a, "--"):
			continue
		case !strings.HasPrefix(a, "-") || a == "-":
			return "", false
		}
		flags := a[1:]
		if strings.Contains(flags, "n") {
			return "", false // parse only; shellArgsKind says so
		}
		if strings.Contains(flags, "e") {
			pre += "set -e\n"
		}
		if strings.HasSuffix(flags, "o") && i+1 < len(args) {
			pre += "set -o " + args[i+1] + "\n" // bash -euo pipefail -c …
			i++
			continue
		}
		if strings.Contains(flags, "c") {
			if i+1 >= len(args) {
				return "", false
			}
			return pre + args[i+1], true
		}
	}
	return "", false
}

// shellArgsKind classifies a shell started without inline code: `bash -n
// script.sh` parses, `bash script.sh` runs, a bare shell reads its stdin.
func shellArgsKind(args []string) commandEvidenceKind {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case (a == "-o" || a == "+o") && i+1 < len(args):
			i++
		case strings.HasPrefix(a, "--"):
		case strings.HasPrefix(a, "-") && a != "-":
			if strings.Contains(a[1:], "n") {
				return evidenceStatic
			}
			if strings.HasSuffix(a, "o") && i+1 < len(args) {
				i++ // bash -eo pipefail script.sh
			}
		default:
			return evidenceExecution // bash script.sh
		}
	}
	return evidenceNone
}

func isShellProgram(p string) bool {
	switch p {
	case "bash", "sh", "zsh", "dash", "ksh":
		return true
	}
	return false
}

func isPythonProgram(p string) bool {
	switch p {
	case "python", "python3", "python2", "py", "pypy", "pypy3":
		return true
	}
	return strings.HasPrefix(p, "python3.")
}

// pythonEvidenceKind: `-m py_compile` and friends are static, `-m pip` and
// `-m venv` are setup, `-m pytest` runs tests, inline code runs unless all it
// does is parse, and a script runs unless the only arguments ask for its
// version or help.
func pythonEvidenceKind(args []string) commandEvidenceKind {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-m" && i+1 < len(args):
			return pythonModuleKind(args[i+1], args[i+2:])
		case a == "-c" && i+1 < len(args):
			if parseOnlyInlineRe.MatchString(args[i+1]) {
				return evidenceStatic
			}
			return evidenceExecution
		case a == "--version" || a == "-V" || a == "--help" || a == "-h":
			return evidenceStatic
		case strings.HasPrefix(a, "-") && a != "-":
			continue // interpreter flags: -u, -B, -W, -X ...
		default:
			if onlyVersionOrHelp(args[i+1:]) {
				return evidenceStatic
			}
			return evidenceExecution // python solve.py ..., or `python -` reading a script
		}
	}
	return evidenceNone // bare interpreter
}

// pythonModuleKind classifies `python -m MODULE ARGS`.
func pythonModuleKind(mod string, args []string) commandEvidenceKind {
	switch {
	case staticPythonModules[mod]:
		return evidenceStatic
	case setupPythonModules[mod]:
		return evidenceNone
	case testRunners[mod] || mod == "unittest":
		return testRunnerKind(args)
	}
	return evidenceExecution // python -m app
}

// testRunnerKind: a runner asked only to list, collect or describe its tests
// runs none of them.
func testRunnerKind(args []string) commandEvidenceKind {
	if onlyVersionOrHelp(args) {
		return evidenceStatic
	}
	for _, a := range args {
		switch a {
		case "--collect-only", "--co", "--fixtures", "--markers", "--listTests", "--list-tests",
			"--dry-run", "--help", "-h", "--version":
			return evidenceStatic
		}
	}
	return evidenceExecution
}

// scriptInterpreterKind: a check flag makes it static, a version or help
// flag too, and so does a script asked only for its version or help.
// Anything else that names code runs it.
func scriptInterpreterKind(args []string, checkFlags []string) commandEvidenceKind {
	for _, a := range args {
		for _, f := range checkFlags {
			if a == f {
				return evidenceStatic
			}
		}
	}
	if len(args) == 0 {
		return evidenceNone
	}
	if onlyVersionOrHelp(args) || args[0] == "-v" {
		return evidenceStatic
	}
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			if onlyVersionOrHelp(args[i+1:]) {
				return evidenceStatic // node app.js --version
			}
			break
		}
	}
	return evidenceExecution
}

func isKnownSubcommand(a string, names ...string) bool {
	for _, n := range names {
		if a == n {
			return true
		}
	}
	return false
}

// subcommandKind classifies `tool <subcommand>` by the subcommand.
func subcommandKind(args []string, runs []string, checks []string) commandEvidenceKind {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if isKnownSubcommand(a, runs...) {
			return evidenceExecution
		}
		if isKnownSubcommand(a, checks...) {
			return evidenceStatic
		}
		return evidenceNone
	}
	return evidenceNone
}

// packageScriptKind: `npm test` and `npm start` run; `npm run <script>` runs
// unless the script name says it builds or checks; installs prove nothing.
func packageScriptKind(prog string, args []string) commandEvidenceKind {
	if len(args) == 0 {
		return evidenceNone
	}
	sub := args[0]
	switch sub {
	case "test", "t", "start":
		return evidenceExecution
	case "run", "run-script":
		if len(args) > 1 {
			if staticScriptNames.MatchString(args[1]) {
				return evidenceStatic
			}
			return evidenceExecution
		}
		return evidenceNone
	case "install", "i", "ci", "add", "remove", "uninstall", "update", "init", "audit", "outdated", "ls", "list", "view", "info":
		return evidenceNone
	}
	if prog == "yarn" || prog == "pnpm" {
		// `yarn build`, `pnpm dev`: a bare script name.
		if staticScriptNames.MatchString(sub) {
			return evidenceStatic
		}
		return evidenceExecution
	}
	return evidenceNone
}

// makeTargetKind classifies `make TARGET` by the target's name. Lint and
// format targets check; test, check and run targets execute; setup targets
// prove nothing. Any other target, and a bare `make`, builds.
func makeTargetKind(args []string) commandEvidenceKind {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-C" || a == "-f" || a == "--file" || a == "--directory" || a == "-I" || a == "-o" || a == "-W":
			i++
			continue
		case (a == "-j" || a == "-l") && i+1 < len(args) && strings.Trim(args[i+1], "0123456789") == "":
			i++
			continue
		case strings.HasPrefix(a, "-") || strings.Contains(a, "="):
			continue
		}
		t := strings.ToLower(a)
		switch {
		case strings.Contains(t, "lint") || strings.Contains(t, "fmt") || strings.Contains(t, "format") ||
			strings.Contains(t, "typecheck") || strings.Contains(t, "type-check") || t == "vet" || t == "tidy":
			return evidenceStatic
		case strings.Contains(t, "test") || strings.Contains(t, "check") || strings.Contains(t, "spec") ||
			strings.Contains(t, "e2e") || strings.Contains(t, "smoke") || strings.Contains(t, "verify") ||
			t == "run" || t == "start" || t == "serve" || t == "demo" || t == "dev" ||
			strings.HasPrefix(t, "run-") || strings.HasPrefix(t, "run_"):
			return evidenceExecution
		case t == "install" || t == "uninstall" || t == "clean" || t == "distclean" || t == "deps" ||
			t == "setup" || t == "bootstrap" || t == "venv" || t == "init":
			return evidenceNone
		}
		return evidenceStatic
	}
	return evidenceStatic // the default target, which conventionally builds
}

// runnerEntries returns the candidate files a test runner or package run
// executes without naming them. A bare `pytest`, or `pytest tests/`, imports
// every test file it discovers; `python -m unittest` does the same for
// test*.py; `go run .` builds and runs a package, and `go test ./...` the
// packages that have tests (without tests it only compiles); `npm test`,
// `jest` and `vitest` run the files their default patterns match. Named files
// are commandNamesPath's job and are left to it. Only candidates are
// returned, so nothing outside what this session changed is claimed, and the
// caller follows their imports as it does for a named entry point.
func runnerEntries(command, workDir string, candidates []string) []string {
	segs, _, _ := splitTopLevelShell(command)
	dir := workDir
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, seg := range segs {
		words := programWords(shellFields(strings.TrimSpace(seg)))
		if len(words) == 0 {
			continue
		}
		prog, args := filepath.Base(words[0]), words[1:]
		if prog == "cd" && len(args) > 0 {
			dir = joinDir(dir, args[0])
			continue
		}
		var roots []string
		var match func(path string) bool
		recursive := true
		switch {
		case prog == "pytest" || prog == "py.test" || (isPythonProgram(prog) && pythonModule(args) == "pytest"):
			roots, match = runnerRoots(dir, pathArgs(args, "pytest")), isPytestFile
		case isPythonProgram(prog) && pythonModule(args) == "unittest":
			roots, match = []string{dir}, func(p string) bool {
				b := filepath.Base(p)
				return strings.HasPrefix(b, "test") && strings.HasSuffix(b, ".py")
			}
		case prog == "go" && len(args) > 0 && (args[0] == "test" || args[0] == "run"):
			for _, pkg := range goPackages(args[1:]) {
				pkgDir, all := joinDir(dir, strings.TrimSuffix(pkg, "/...")), strings.HasSuffix(pkg, "...")
				for _, c := range candidates {
					if !strings.HasSuffix(c, ".go") || !underDir(c, pkgDir, workDir, all) {
						continue
					}
					if args[0] == "test" && !dirHasGoTests(filepath.Dir(absUnder(c, workDir))) {
						continue // compiled, and nothing ran
					}
					add(c)
				}
			}
			continue
		case (prog == "npm" || prog == "yarn" || prog == "pnpm") && len(args) > 0 && (args[0] == "test" || args[0] == "t"),
			prog == "jest" || prog == "vitest" || prog == "mocha",
			(prog == "npx" || prog == "pnpx" || prog == "bunx") && len(args) > 0 && (args[0] == "jest" || args[0] == "vitest" || args[0] == "mocha"):
			roots, match = []string{dir}, isJSTestFile
		default:
			continue
		}
		for _, root := range roots {
			for _, c := range candidates {
				if match(c) && underDir(c, root, workDir, recursive) {
					add(c)
				}
			}
		}
	}
	return out
}

// pythonModule is the module `python -m MODULE` runs, or "".
func pythonModule(args []string) string {
	for i, a := range args {
		if a == "-m" && i+1 < len(args) {
			return args[i+1]
		}
		if !strings.HasPrefix(a, "-") {
			return ""
		}
	}
	return ""
}

// pytestValueFlags are pytest options whose value is the next word.
var pytestValueFlags = map[string]bool{
	"-k": true, "-m": true, "-p": true, "-c": true, "-o": true, "--maxfail": true,
	"--deselect": true, "--ignore": true, "--rootdir": true, "-n": true, "--tb": true,
	"--junitxml": true, "--basetemp": true, "--confcutdir": true,
}

// pathArgs are a runner's directory arguments: its non-flag words that do not
// name a file. `pytest -k slow tests/` → [tests/].
func pathArgs(args []string, runner string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-m" && i+1 < len(args) && args[i+1] == runner {
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			if pytestValueFlags[a] {
				i++
			}
			continue
		}
		if filepath.Ext(strings.SplitN(a, "::", 2)[0]) == "" {
			out = append(out, a)
		}
	}
	return out
}

// runnerRoots is where discovery starts: the runner's directory arguments,
// or the directory it ran in.
func runnerRoots(dir string, args []string) []string {
	if len(args) == 0 {
		return []string{dir}
	}
	roots := make([]string, 0, len(args))
	for _, a := range args {
		roots = append(roots, joinDir(dir, a))
	}
	return roots
}

// goPackages are the package patterns of `go test` / `go run`, "." when none.
// A .go file argument is a file, which commandNamesPath already covers.
func goPackages(args []string) []string {
	var out []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") || strings.HasSuffix(a, ".go") {
			continue
		}
		if a == "." || a == "./..." || strings.HasPrefix(a, "./") || strings.HasPrefix(a, "../") {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		out = []string{"."}
	}
	return out
}

func isPytestFile(p string) bool {
	b := filepath.Base(p)
	return strings.HasSuffix(b, ".py") && (strings.HasPrefix(b, "test_") || strings.HasSuffix(b, "_test.py"))
}

var jsTestFileRe = regexp.MustCompile(`\.(test|spec)\.[cm]?[jt]sx?$`)

func isJSTestFile(p string) bool {
	return jsTestFileRe.MatchString(filepath.Base(p)) ||
		strings.Contains(filepath.ToSlash(p), "/__tests__/")
}

// joinDir resolves a `cd` target or a runner argument against dir.
func joinDir(dir, rel string) string {
	if filepath.IsAbs(rel) {
		return filepath.Clean(rel)
	}
	return filepath.Join(dir, rel)
}

// absUnder makes a candidate path absolute against the workspace.
func absUnder(p, workDir string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(workDir, p)
}

// underDir reports whether candidate p sits in dir, or anywhere below it when
// recursive.
func underDir(p, dir, workDir string, recursive bool) bool {
	abs := absUnder(p, workDir)
	if !recursive {
		return filepath.Dir(abs) == filepath.Clean(dir)
	}
	rel, err := filepath.Rel(dir, abs)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// dirHasGoTests reports whether a Go package directory holds a test file.
func dirHasGoTests(dir string) bool {
	matches, _ := filepath.Glob(filepath.Join(dir, "*_test.go"))
	return len(matches) > 0
}

// compileStep reports a segment that compiles source into something a later
// segment can run: `javac App.java`, `gcc main.c -o main`, `go build`.
func compileStep(seg string) bool {
	words := programWords(shellFields(strings.TrimSpace(seg)))
	if len(words) == 0 {
		return false
	}
	prog, args := filepath.Base(words[0]), words[1:]
	switch {
	case compilers[prog]:
		return true
	case prog == "go" || prog == "cargo" || prog == "dotnet" || prog == "swift":
		return subcommandKind(args, nil, []string{"build"}) == evidenceStatic
	case prog == "mvn" || prog == "mvnw" || prog == "gradle" || prog == "gradlew":
		return subcommandKind(args, nil, []string{"compile", "build", "package", "assemble"}) == evidenceStatic
	case prog == "npm" || prog == "pnpm" || prog == "yarn":
		name := ""
		if len(args) > 1 && (args[0] == "run" || args[0] == "run-script") {
			name = args[1]
		} else if len(args) > 0 && prog != "npm" {
			name = args[0]
		}
		return name != "" && buildScriptNames.MatchString(name)
	case prog == "make" || prog == "gmake":
		return makeTargetKind(args) == evidenceStatic
	}
	return false
}

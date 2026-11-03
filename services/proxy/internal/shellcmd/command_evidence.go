package shellcmd

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
// ClassifyCommandEvidence answers two separate questions about one command
// line: what kind of evidence its verifying part produces, and whether that
// part's exit status actually reaches the status the line reports.

// CommandEvidenceKind orders what a successful command can show, weakest
// first.
type CommandEvidenceKind int

const (
	// EvidenceNone: recon or setup (ls, cat, echo, cd, pip install). Proves
	// nothing about the artifact.
	EvidenceNone CommandEvidenceKind = iota
	// EvidenceStatic: the code was parsed, linted, type-checked, formatted or
	// compiled, or a tool printed its version or help. It says the code is
	// well formed, not that it does anything.
	EvidenceStatic
	// evidenceProbe: a response body was fetched from a running service.
	evidenceProbe
	// EvidenceExecution: the program, or its tests, actually ran.
	EvidenceExecution
)

func (k CommandEvidenceKind) String() string {
	switch k {
	case EvidenceStatic:
		return "static"
	case evidenceProbe:
		return "probe"
	case EvidenceExecution:
		return "execution"
	}
	return "none"
}

// Verifies reports whether the kind can stand as verification that the program
// works: the command ran the program or its tests, or fetched a response body,
// and the line reports that part's exit status. A parse, a lint, a build, a
// `--version`, a headers-only probe, and a test whose status `| tail` or
// `|| true` replaced are not verification. A probe that never retrieves a body
// proves the SERVER is up, not that the artifact works: measured on a "build me
// a snake game" session, `curl -I` was recorded as the verification and `done`
// shipped an index.html with no HTML in it.
func (k CommandEvidenceKind) Verifies() bool {
	return k == evidenceProbe || k == EvidenceExecution
}

// CommandEvidence is the classification of one command line.
type CommandEvidence struct {
	// Kind is the strongest kind among the segments whose exit status reaches
	// the line's own status. A segment whose status is hidden contributes
	// nothing here.
	Kind CommandEvidenceKind
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

// ClassifyCommandEvidence splits a command at its top-level shell operators
// and classifies each segment.
//
// A segment's failure reaches the line's exit status only if every operator
// after it is `&&`: the first failure stops the chain and the non-zero status
// survives to the end. Anything else hides it. `|` reports the last command's
// status (the sandbox runs `bash -c` without pipefail), `;` and a newline
// report whatever ran last, `||` replaces a failure with the fallback's
// result, and `&` backgrounds the command so its status is never observed.
func ClassifyCommandEvidence(command string) CommandEvidence {
	segs, ops, bodies := SplitTopLevelShell(command)
	// `bash -c "pytest | tail"` is the command inside the quotes.
	if script, ok := soleShellScript(segs, bodies); ok {
		return ClassifyCommandEvidence(script)
	}
	// `set -e` makes `;` stop at the first failure, and `set -o pipefail`
	// makes a pipeline report its first failure, so neither hides one.
	errexit, pipefail := shellOptions(segs)
	var ev CommandEvidence
	var bare, cdBefore []string
	var kinds []CommandEvidenceKind
	lastCd := ""
	for i, seg := range segs {
		kind := SegmentEvidenceKind(seg)
		if kind == EvidenceNone {
			if w := ShellFields(strings.TrimSpace(seg)); len(w) > 0 && w[0] == "cd" {
				lastCd = seg
			}
			continue
		}
		hiddenBy, asserted := hiddenStatus(i, kind, segs, ops, errexit, pipefail)
		if note := maskNote(seg, kind, hiddenBy, asserted); note != "" {
			ev.mask(note)
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
		ev.Kind = max(ev.Kind, kind)
	}
	ev.Covering = coveringSegments(ev.Honest, bare, cdBefore, kinds)
	return ev
}

// soleShellScript returns the inline script of a line that is one shell
// started with a script (`bash -c SCRIPT`) and reads no here-document.
func soleShellScript(segs, bodies []string) (string, bool) {
	if len(segs) != 1 || bodies[0] != "" {
		return "", false
	}
	return shellScript(segs[0])
}

// hiddenStatus reports what keeps segment i's exit status from reaching the
// line's: the operator and the word after it, "&" for the background
// operator, or "" when nothing does. asserted is set when segment i is a
// probe piped into a check on the body it fetched.
func hiddenStatus(i int, kind CommandEvidenceKind, segs, ops []string, errexit, pipefail bool) (hiddenBy string, asserted bool) {
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
	return hiddenBy, asserted
}

// maskNote says, for the model, what hid a verifying segment's result and
// what to run instead, or "" when the line reports that result.
func maskNote(seg string, kind CommandEvidenceKind, hiddenBy string, asserted bool) string {
	switch {
	case hiddenBy == "&":
		return fmt.Sprintf("`&` runs `%s` in the background, so its exit status is never seen. Run it in "+
			"the foreground, or start a server with run_background and probe it.", programName(seg))
	case hiddenBy != "":
		return fmt.Sprintf("`%s` replaces the exit status of `%s`, so a failure would still have looked "+
			"like success. Run it on its own; long output is truncated for you anyway.", hiddenBy, programName(seg))
	case kind == evidenceProbe && !asserted && probeStatusBlind(seg):
		return fmt.Sprintf("`%s` exits 0 even when the server answers with an error page, so a broken "+
			"page would still have looked like success. Use `curl -sf <url>`, or pipe the body into "+
			"`grep` for what the page must contain.", programName(seg))
	}
	return ""
}

// mask records that a verifying segment's result was hidden. The first note
// is the one the model sees.
func (ev *CommandEvidence) mask(note string) {
	if !ev.Masked {
		ev.Masked, ev.MaskNote = true, note
	}
}

// coveringSegments picks the honest segments a passing line binds files from:
// each that ran or probed something, and each compile step whose output a
// later segment ran, preceded by the `cd` that set where it ran.
func coveringSegments(honest, bare, cdBefore []string, kinds []CommandEvidenceKind) []string {
	lastRun := -1
	for i, k := range kinds {
		if k == EvidenceExecution {
			lastRun = i
		}
	}
	var covering []string
	placed := ""
	for i, k := range kinds {
		if k.Verifies() || (i < lastRun && compileStep(bare[i])) {
			// Where it ran decides what a runner discovers (RunnerEntries).
			if cdBefore[i] != "" && cdBefore[i] != placed {
				covering = append(covering, cdBefore[i])
				placed = cdBefore[i]
			}
			covering = append(covering, honest[i])
		}
	}
	return covering
}

// shellOptions reads `set -e` / `set -o errexit` and `set -o pipefail` from
// the segments that set shell options.
func shellOptions(segs []string) (errexit, pipefail bool) {
	for _, seg := range segs {
		f := ShellFields(strings.TrimSpace(seg))
		if len(f) == 0 || f[0] != "set" {
			continue
		}
		e, p := setOptions(f)
		if e {
			errexit = true
		}
		if p {
			pipefail = true
		}
	}
	return errexit, pipefail
}

// setOptions reads errexit and pipefail from the words of one `set` command.
func setOptions(f []string) (errexit, pipefail bool) {
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
	return errexit, pipefail
}

// pipeAssertion reports a pipeline stage that checks what it receives rather
// than passing it through: `grep` (not -v, -c or -L), `diff`, `cmp`, `jq -e`.
func pipeAssertion(seg string) bool {
	f := ProgramWords(ShellFields(strings.TrimSpace(seg)))
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
	words := ProgramWords(ShellFields(strings.TrimSpace(seg)))
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
	f := ShellFields(seg)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// programName is the program a segment runs, past wrappers like timeout.
func programName(seg string) string {
	if w := ProgramWords(ShellFields(strings.TrimSpace(seg))); len(w) > 0 {
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

// SplitTopLevelShell splits a command line into segments and the operators
// between them: ops[i] separates segs[i] and segs[i+1]. Quotes, backslash
// escapes, parentheses, $( ) and backticks are respected, so an operator
// inside a quoted string or a subshell is not a separator. `2>&1`, `&>` and
// `>&` are redirections, not the background operator. A here-document body is
// not shell text: it is returned in bodies[i] for the segment that reads it,
// and never split. Best effort, like splitShellSegments, which it does not
// replace: that one drops the operators, and the operators are the point
// here.
func SplitTopLevelShell(cmd string) (segs, ops, bodies []string) {
	var cur strings.Builder
	var nest shellNesting
	var pending []hereDoc
	bodyAt := map[int]*strings.Builder{}
	flush := func(op string) {
		segs = append(segs, cur.String())
		ops = append(ops, op)
		cur.Reset()
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if c == '\\' && !nest.inSingle && i+1 < len(cmd) {
			cur.WriteByte(c)
			cur.WriteByte(cmd[i+1])
			i++
			continue
		}
		if !nest.topLevel(c) {
			cur.WriteByte(c)
			continue
		}
		op, width := shellOperatorAt(cmd, i)
		switch {
		case strings.HasPrefix(cmd[i:], "<<<"):
			cur.WriteString("<<<") // a here-string, not a here-document
			i += 2
		case strings.HasPrefix(cmd[i:], "<<"):
			end, doc := readHereDocOperator(cmd, i)
			cur.WriteString(cmd[i:end])
			if doc.delim != "" {
				doc.seg = len(segs)
				pending = append(pending, doc)
			}
			i = end - 1
		case c == '\n' && len(pending) > 0:
			// The bodies start on the next line. The newline itself still
			// ends the command line.
			i = readHereDocBodies(cmd, i+1, pending, bodyAt)
			pending = nil
			flush(";")
		case op != "":
			flush(op)
			i += width - 1
		default:
			cur.WriteByte(c)
		}
	}
	segs = append(segs, cur.String())
	segs = dropEmptyTrailingSegments(segs, ops)
	bodies = hereDocBodiesBySegment(segs, bodyAt)
	return segs, ops, bodies
}

// shellNesting tracks the quotes and parentheses a command line is inside.
type shellNesting struct {
	inSingle, inDouble, inBacktick bool
	depth                          int
}

// topLevel reads byte c and reports whether it sits outside every quote and
// parenthesis, where an operator separates commands. An opening quote or
// parenthesis is inside; the one that closes the last of them is outside.
func (n *shellNesting) topLevel(c byte) bool {
	switch {
	case c == '\'' && !n.inDouble && !n.inBacktick:
		n.inSingle = !n.inSingle
	case c == '"' && !n.inSingle && !n.inBacktick:
		n.inDouble = !n.inDouble
	case c == '`' && !n.inSingle:
		n.inBacktick = !n.inBacktick
	}
	if n.inSingle || n.inDouble || n.inBacktick {
		return false
	}
	if c == '(' {
		n.depth++
	} else if c == ')' && n.depth > 0 {
		n.depth--
	}
	return n.depth == 0
}

// shellOperatorAt returns the operator that starts at cmd[i] and its length
// in bytes, or "" when none does. `|&` is recorded as `|` and a newline as
// `;`. The `&` of `2>&1`, `&>` and `>&` is part of a redirection, not the
// background operator.
func shellOperatorAt(cmd string, i int) (op string, width int) {
	c := cmd[i]
	prev, next := byte(0), byte(0)
	if i > 0 {
		prev = cmd[i-1]
	}
	if i+1 < len(cmd) {
		next = cmd[i+1]
	}
	switch {
	case c == '&' && next == '&':
		return "&&", 2
	case c == '|' && next == '|':
		return "||", 2
	case c == '|' && next == '&':
		return "|", 2
	case c == '|':
		return "|", 1
	case c == ';' || c == '\n':
		return ";", 1
	case c == '&' && prev != '>' && prev != '<' && next != '>':
		return "&", 1
	}
	return "", 0
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
	doc.delim, j = readHereDocDelim(cmd, j)
	return j, doc
}

// readHereDocDelim reads the delimiter word of a here-document starting at j,
// with its quotes and backslashes removed, and returns the index just past it.
func readHereDocDelim(cmd string, j int) (string, int) {
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
	return delim.String(), j
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
		p = readHereDocBody(cmd, p, doc, b)
	}
	return p - 1
}

// readHereDocBody adds to b the lines of one here-document body that starts at
// p, up to and with its delimiter line, and returns the index after it.
func readHereDocBody(cmd string, p int, doc hereDoc, b *strings.Builder) int {
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
	return p
}

// dropEmptyTrailingSegments removes the empty segments at the end of a split.
func dropEmptyTrailingSegments(segs, ops []string) []string {
	// Drop empty trailing segments ("pytest;" or "cmd &") but keep the
	// operator that preceded them: a trailing `&` still backgrounds the
	// command before it.
	for len(segs) > 0 && strings.TrimSpace(segs[len(segs)-1]) == "" && len(segs) > len(ops) {
		segs = segs[:len(segs)-1]
	}
	return segs
}

// hereDocBodiesBySegment lists the here-document bodies by segment: one
// entry for each segment, empty for a segment that reads none.
func hereDocBodiesBySegment(segs []string, bodyAt map[int]*strings.Builder) (bodies []string) {
	bodies = make([]string, len(segs))
	for idx, b := range bodyAt {
		if idx < len(bodies) {
			bodies[idx] = b.String()
		}
	}
	return bodies
}

// ShellFields splits one segment into words, honouring quotes and dropping
// them. Redirections stay as their own words.
func ShellFields(seg string) []string {
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

// timeoutValueFlags are timeout options that take a value.
var timeoutValueFlags = map[string]bool{
	"-s": true, "-k": true, "--signal": true, "--kill-after": true,
}

// ProgramWords strips what runs a program without being it: leading
// environment assignments, the wrappers timeout, env, nice, nohup, time,
// command, exec and stdbuf, and environment runners (`uv run`, `poetry run`,
// `bundle exec`). `timeout 30 python app.py` is a run of app.py.
func ProgramWords(words []string) []string {
	for len(words) > 0 {
		w := words[0]
		switch {
		case envAssignmentRe.MatchString(w):
			words = words[1:]
		case w == "time" || w == "command" || w == "exec" || w == "nohup":
			words = words[1:]
		case w == "env":
			words = dropEnvOptions(words[1:])
		case w == "nice":
			words = words[1:]
			if len(words) > 1 && words[0] == "-n" {
				words = words[2:]
			}
		case w == "stdbuf":
			words = dropOptions(words[1:], nil)
		case w == "timeout":
			words = dropOptions(words[1:], timeoutValueFlags)
			if len(words) > 0 {
				words = words[1:] // the duration
			}
		case (w == "uv" || w == "poetry" || w == "pipenv" || w == "pdm" || w == "rye") && len(words) > 1 && words[1] == "run":
			words = runnerProgramWords(words[2:])
		case w == "bundle" && len(words) > 1 && words[1] == "exec":
			words = words[2:]
		default:
			return words
		}
	}
	return words
}

// dropOptions removes the options words start with, and the value that
// follows each option named in withValue.
func dropOptions(words []string, withValue map[string]bool) []string {
	for len(words) > 0 && strings.HasPrefix(words[0], "-") {
		if withValue[words[0]] && len(words) > 1 {
			words = words[1:]
		}
		words = words[1:]
	}
	return words
}

// dropEnvOptions removes the options and variable assignments env reads
// before the command it runs.
func dropEnvOptions(words []string) []string {
	for len(words) > 0 && (strings.HasPrefix(words[0], "-") || envAssignmentRe.MatchString(words[0])) {
		words = words[1:]
	}
	return words
}

// runnerProgramWords are the words of what an environment runner runs, past
// its options. A Python script it is given runs under python.
func runnerProgramWords(words []string) []string {
	words = dropOptions(words, runnerValueFlags)
	if len(words) > 0 && strings.HasSuffix(words[0], ".py") {
		return append([]string{"python"}, words...) // `uv run app.py`
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

// helpFlag and versionFlag are the long flags that ask a tool for its help or
// its version, which run nothing.
const (
	helpFlag    = "--help"
	versionFlag = "--version"
)

func onlyVersionOrHelp(args []string) bool {
	if len(args) == 0 {
		return false
	}
	for _, a := range args {
		switch a {
		case versionFlag, "-V", helpFlag, "-h", "-help", "-version", "version":
		default:
			return false
		}
	}
	return true
}

// SegmentEvidenceKind classifies one segment by the program it runs.
func SegmentEvidenceKind(seg string) CommandEvidenceKind {
	t := strings.TrimSpace(seg)
	// `(cd app && pytest)`: a subshell reports what ran last inside it.
	if inner, ok := leadingSubshell(t); ok {
		return ClassifyCommandEvidence(inner).Kind
	}
	words := ProgramWords(ShellFields(t))
	if len(words) == 0 {
		return EvidenceNone
	}
	prog := filepath.Base(words[0])
	args := words[1:]

	switch {
	case isShellProgram(prog):
		return shellProgramKind(t, args)
	case staticTools[prog]:
		return EvidenceStatic
	case testRunners[prog]:
		return testRunnerKind(args)
	case isPythonProgram(prog):
		return pythonEvidenceKind(args)
	}

	switch prog {
	case "curl", "wget", "http", "https", "httpie":
		if headOnlyProbeRe.MatchString(t) {
			return EvidenceNone
		}
		return evidenceProbe
	case "node", "nodejs":
		return scriptInterpreterKind(args, []string{"--check", "-c"})
	case "ruby":
		return scriptInterpreterKind(args, []string{"-c"})
	case "perl":
		return scriptInterpreterKind(args, []string{"-c"})
	case "php":
		return scriptInterpreterKind(args, []string{"-l"})
	case "lua", "luajit", "Rscript", "julia", "elixir":
		return scriptInterpreterKind(args, nil)
	case "deno":
		return subcommandKind(args, []string{"run", "test", "eval", "serve", "task", "bench"},
			[]string{"check", "lint", "fmt", "doc", "compile"})
	case "bun":
		return bunKind(args)
	case "go":
		return subcommandKind(args, []string{"test", "run"}, []string{"build", "vet", "fmt"})
	case "cargo":
		return subcommandKind(args, []string{"test", "run", "bench"}, []string{"build", "check", "clippy", "fmt", "doc"})
	case "dotnet":
		return subcommandKind(args, []string{"test", "run"}, []string{"build", "format"})
	case "swift":
		if len(args) > 0 && strings.HasSuffix(args[0], ".swift") {
			return EvidenceExecution
		}
		return subcommandKind(args, []string{"test", "run"}, []string{"build"})
	case "mvn", "mvnw", "gradle", "gradlew":
		return subcommandKind(args, []string{"test", "verify", "run", "bootRun"}, []string{"compile", "build", "package", "check", "assemble"})
	case "java", "kotlin", "scala":
		return jvmLauncherKind(args)
	case "npm", "pnpm", "yarn":
		return packageScriptKind(prog, args)
	case "npx", "pnpx", "bunx":
		return npxToolKind(args)
	case "make", "gmake", "just", "task":
		return makeTargetKind(args)
	}
	// A path to a program or script: ./solve.py, bin/app. Checked after the
	// named programs, so /usr/bin/python3 -m py_compile is still a parse.
	if strings.Contains(words[0], "/") && !strings.HasSuffix(words[0], "/") {
		if onlyVersionOrHelp(args) {
			return EvidenceStatic
		}
		return EvidenceExecution
	}
	return EvidenceNone
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
				return subshellBody(t, i)
			}
		}
	}
	return "", false
}

// subshellBody is what the subshell that closes at i runs, when nothing but
// redirections follows its closing parenthesis.
func subshellBody(t string, i int) (string, bool) {
	rest := strings.TrimLeft(strings.TrimSpace(t[i+1:]), "0123456789&")
	if rest == "" || strings.HasPrefix(rest, ">") || strings.HasPrefix(rest, "<") {
		return t[1:i], true
	}
	return "", false
}

// shellScript returns the script a shell segment runs inline — `bash -c
// SCRIPT`, flags combined or not (`bash -lc`, `sh -ec`) — with `set -e` or
// `set -o …` prepended for the options the shell was started with.
func shellScript(seg string) (string, bool) {
	words := ProgramWords(ShellFields(strings.TrimSpace(seg)))
	if len(words) == 0 || !isShellProgram(filepath.Base(words[0])) {
		return "", false
	}
	return inlineScript(words[1:])
}

// inlineScript reads a shell's arguments up to the -c flag and returns the
// script after it, with the options read on the way as its first lines.
func inlineScript(args []string) (string, bool) {
	pre := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case (a == "-o" || a == "+o") && i+1 < len(args):
			pre += optionLine(a, args[i+1])
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
			pre += optionLine("-o", args[i+1]) // bash -euo pipefail -c …
			i++
			continue
		}
		if strings.Contains(flags, "c") {
			return commandOperand(pre, args, i)
		}
	}
	return "", false
}

// optionLine is the `set -o NAME` line for `-o NAME`; `+o NAME` adds none.
func optionLine(flag, name string) string {
	if flag == "-o" {
		return "set -o " + name + "\n"
	}
	return ""
}

// commandOperand is the script that the -c flag at i takes, after the
// options' lines; false when the script is missing.
func commandOperand(pre string, args []string, i int) (string, bool) {
	if i+1 >= len(args) {
		return "", false
	}
	return pre + args[i+1], true
}

// shellProgramKind: a shell started with inline code is judged by that code,
// and one started without it by its arguments.
func shellProgramKind(seg string, args []string) CommandEvidenceKind {
	if script, ok := shellScript(seg); ok {
		return ClassifyCommandEvidence(script).Kind
	}
	return shellArgsKind(args)
}

// shellArgsKind classifies a shell started without inline code: `bash -n
// script.sh` parses, `bash script.sh` runs, a bare shell reads its stdin.
func shellArgsKind(args []string) CommandEvidenceKind {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case (a == "-o" || a == "+o") && i+1 < len(args):
			i++
		case strings.HasPrefix(a, "--"):
		case strings.HasPrefix(a, "-") && a != "-":
			if strings.Contains(a[1:], "n") {
				return EvidenceStatic
			}
			if strings.HasSuffix(a, "o") && i+1 < len(args) {
				i++ // bash -eo pipefail script.sh
			}
		default:
			return EvidenceExecution // bash script.sh
		}
	}
	return EvidenceNone
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
func pythonEvidenceKind(args []string) CommandEvidenceKind {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-m" && i+1 < len(args):
			return pythonModuleKind(args[i+1], args[i+2:])
		case a == "-c" && i+1 < len(args):
			if parseOnlyInlineRe.MatchString(args[i+1]) {
				return EvidenceStatic
			}
			return EvidenceExecution
		case a == versionFlag || a == "-V" || a == helpFlag || a == "-h":
			return EvidenceStatic
		case strings.HasPrefix(a, "-") && a != "-":
			continue // interpreter flags: -u, -B, -W, -X ...
		default:
			if onlyVersionOrHelp(args[i+1:]) {
				return EvidenceStatic
			}
			return EvidenceExecution // python solve.py ..., or `python -` reading a script
		}
	}
	return EvidenceNone // bare interpreter
}

// pythonModuleKind classifies `python -m MODULE ARGS`.
func pythonModuleKind(mod string, args []string) CommandEvidenceKind {
	switch {
	case staticPythonModules[mod]:
		return EvidenceStatic
	case setupPythonModules[mod]:
		return EvidenceNone
	case testRunners[mod] || mod == "unittest":
		return testRunnerKind(args)
	}
	return EvidenceExecution // python -m app
}

// testRunnerKind: a runner asked only to list, collect or describe its tests
// runs none of them.
func testRunnerKind(args []string) CommandEvidenceKind {
	if onlyVersionOrHelp(args) {
		return EvidenceStatic
	}
	for _, a := range args {
		switch a {
		case "--collect-only", "--co", "--fixtures", "--markers", "--listTests", "--list-tests",
			"--dry-run", helpFlag, "-h", versionFlag:
			return EvidenceStatic
		}
	}
	return EvidenceExecution
}

// scriptInterpreterKind: a check flag makes it static, a version or help
// flag too, and so does a script asked only for its version or help.
// Anything else that names code runs it.
func scriptInterpreterKind(args, checkFlags []string) CommandEvidenceKind {
	for _, a := range args {
		for _, f := range checkFlags {
			if a == f {
				return EvidenceStatic
			}
		}
	}
	if len(args) == 0 {
		return EvidenceNone
	}
	if onlyVersionOrHelp(args) || args[0] == "-v" {
		return EvidenceStatic
	}
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			if onlyVersionOrHelp(args[i+1:]) {
				return EvidenceStatic // node app.js --version
			}
			break
		}
	}
	return EvidenceExecution
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
func subcommandKind(args, runs, checks []string) CommandEvidenceKind {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if isKnownSubcommand(a, runs...) {
			return EvidenceExecution
		}
		if isKnownSubcommand(a, checks...) {
			return EvidenceStatic
		}
		return EvidenceNone
	}
	return EvidenceNone
}

// bunKind: `bun app.ts` runs the file it names; any other bun command is
// judged by its subcommand.
func bunKind(args []string) CommandEvidenceKind {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") && !isKnownSubcommand(args[0], "test", "run", "x", "build", "install", "add", "remove", "init", "create", "upgrade") {
		return EvidenceExecution // bun app.ts
	}
	return subcommandKind(args, []string{"test", "run", "x"}, []string{"build"})
}

// jvmLauncherKind: java, kotlin and scala run whatever they are given, unless
// they are asked only for their version or help.
func jvmLauncherKind(args []string) CommandEvidenceKind {
	if onlyVersionOrHelp(args) {
		return EvidenceStatic
	}
	if len(args) > 0 {
		return EvidenceExecution
	}
	return EvidenceNone
}

// packageScriptKind: `npm test` and `npm start` run; `npm run <script>` runs
// unless the script name says it builds or checks; installs prove nothing.
func packageScriptKind(prog string, args []string) CommandEvidenceKind {
	if len(args) == 0 {
		return EvidenceNone
	}
	sub := args[0]
	switch sub {
	case "test", "t", "start":
		return EvidenceExecution
	case "run", "run-script":
		if len(args) > 1 {
			if staticScriptNames.MatchString(args[1]) {
				return EvidenceStatic
			}
			return EvidenceExecution
		}
		return EvidenceNone
	case "install", "i", "ci", "add", "remove", "uninstall", "update", "init", "audit", "outdated", "ls", "list", "view", "info":
		return EvidenceNone
	}
	if prog == "yarn" || prog == "pnpm" {
		// `yarn build`, `pnpm dev`: a bare script name.
		if staticScriptNames.MatchString(sub) {
			return EvidenceStatic
		}
		return EvidenceExecution
	}
	return EvidenceNone
}

// npxToolKind classifies `npx TOOL`, `pnpx TOOL` and `bunx TOOL` by the tool
// they run: a static tool checks, a test runner runs tests, any other tool
// runs.
func npxToolKind(args []string) CommandEvidenceKind {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		args = args[1:] // npx --yes jest
	}
	if len(args) == 0 {
		return EvidenceNone
	}
	tool := filepath.Base(args[0])
	switch {
	case staticTools[tool]:
		return EvidenceStatic
	case testRunners[tool]:
		return testRunnerKind(args[1:])
	}
	return EvidenceExecution
}

// makeTargetKind classifies `make TARGET` by the target's name. Lint and
// format targets check; test, check and run targets execute; setup targets
// prove nothing. Any other target, and a bare `make`, builds.
func makeTargetKind(args []string) CommandEvidenceKind {
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
			return EvidenceStatic
		case strings.Contains(t, "test") || strings.Contains(t, "check") || strings.Contains(t, "spec") ||
			strings.Contains(t, "e2e") || strings.Contains(t, "smoke") || strings.Contains(t, "verify") ||
			t == "run" || t == "start" || t == "serve" || t == "demo" || t == "dev" ||
			strings.HasPrefix(t, "run-") || strings.HasPrefix(t, "run_"):
			return EvidenceExecution
		case t == "install" || t == "uninstall" || t == "clean" || t == "distclean" || t == "deps" ||
			t == "setup" || t == "bootstrap" || t == "venv" || t == "init":
			return EvidenceNone
		}
		return EvidenceStatic
	}
	return EvidenceStatic // the default target, which conventionally builds
}

// RunnerEntries returns the candidate files a test runner or package run
// executes without naming them. A bare `pytest`, or `pytest tests/`, imports
// every test file it discovers; `python -m unittest` does the same for
// test*.py; `go run .` builds and runs a package, and `go test ./...` the
// packages that have tests (without tests it only compiles); `npm test`,
// `jest` and `vitest` run the files their default patterns match. Named files
// are commandNamesPath's job and are left to it. Only candidates are
// returned, so nothing outside what this session changed is claimed, and the
// caller follows their imports as it does for a named entry point.
func RunnerEntries(command, workDir string, candidates []string) []string {
	segs, _, _ := SplitTopLevelShell(command)
	dir := workDir
	var out []string
	seen := map[string]bool{}
	for _, seg := range segs {
		words := ProgramWords(ShellFields(strings.TrimSpace(seg)))
		if len(words) == 0 {
			continue
		}
		prog, args := filepath.Base(words[0]), words[1:]
		if prog == "cd" && len(args) > 0 {
			dir = joinDir(dir, args[0])
			continue
		}
		for _, p := range segmentRunnerEntries(prog, args, dir, workDir, candidates) {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// segmentRunnerEntries returns the candidates that one segment's runner
// discovers when it runs in dir, and none for a segment that runs no runner.
func segmentRunnerEntries(prog string, args []string, dir, workDir string, candidates []string) []string {
	switch {
	case prog == "pytest" || prog == "py.test" || (isPythonProgram(prog) && pythonModule(args) == "pytest"):
		return candidatesUnder(runnerRoots(dir, pathArgs(args, "pytest")), isPytestFile, workDir, candidates)
	case isPythonProgram(prog) && pythonModule(args) == "unittest":
		return candidatesUnder([]string{dir}, isUnittestFile, workDir, candidates)
	case prog == "go" && len(args) > 0 && (args[0] == "test" || args[0] == "run"):
		return goPackageEntries(args, dir, workDir, candidates)
	case (prog == "npm" || prog == "yarn" || prog == "pnpm") && len(args) > 0 && (args[0] == "test" || args[0] == "t"),
		prog == "jest" || prog == "vitest" || prog == "mocha",
		(prog == "npx" || prog == "pnpx" || prog == "bunx") && len(args) > 0 && (args[0] == "jest" || args[0] == "vitest" || args[0] == "mocha"):
		return candidatesUnder([]string{dir}, isJSTestFile, workDir, candidates)
	}
	return nil
}

// candidatesUnder returns, root by root, the candidates that match and sit
// anywhere below that root.
func candidatesUnder(roots []string, match func(path string) bool, workDir string, candidates []string) []string {
	var out []string
	for _, root := range roots {
		for _, c := range candidates {
			if match(c) && underDir(c, root, workDir, true) {
				out = append(out, c)
			}
		}
	}
	return out
}

// goPackageEntries returns the .go candidates in the packages that `go test`
// or `go run` names. `go test` only compiles a package without test files, so
// such a package adds nothing to a test run.
func goPackageEntries(args []string, dir, workDir string, candidates []string) []string {
	var out []string
	for _, pkg := range goPackages(args[1:]) {
		pkgDir, all := joinDir(dir, strings.TrimSuffix(pkg, "/...")), strings.HasSuffix(pkg, "...")
		for _, c := range candidates {
			if !strings.HasSuffix(c, ".go") || !underDir(c, pkgDir, workDir, all) {
				continue
			}
			if args[0] == "test" && !dirHasGoTests(filepath.Dir(absUnder(c, workDir))) {
				continue // compiled, and nothing ran
			}
			out = append(out, c)
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

// isUnittestFile matches the files `python -m unittest` discovers: test*.py.
func isUnittestFile(p string) bool {
	b := filepath.Base(p)
	return strings.HasPrefix(b, "test") && strings.HasSuffix(b, ".py")
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
	words := ProgramWords(ShellFields(strings.TrimSpace(seg)))
	if len(words) == 0 {
		return false
	}
	prog, args := filepath.Base(words[0]), words[1:]
	switch {
	case compilers[prog]:
		return true
	case prog == "go" || prog == "cargo" || prog == "dotnet" || prog == "swift":
		return subcommandKind(args, nil, []string{"build"}) == EvidenceStatic
	case prog == "mvn" || prog == "mvnw" || prog == "gradle" || prog == "gradlew":
		return subcommandKind(args, nil, []string{"compile", "build", "package", "assemble"}) == EvidenceStatic
	case prog == "npm" || prog == "pnpm" || prog == "yarn":
		name := ""
		if len(args) > 1 && (args[0] == "run" || args[0] == "run-script") {
			name = args[1]
		} else if len(args) > 0 && prog != "npm" {
			name = args[0]
		}
		return name != "" && buildScriptNames.MatchString(name)
	case prog == "make" || prog == "gmake":
		return makeTargetKind(args) == EvidenceStatic
	}
	return false
}

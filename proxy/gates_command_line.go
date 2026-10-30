// Command lines: what a shell command line runs. The program it exists to
// run is what the plan's verify step is matched by, and the code given to an
// interpreter on the line itself is what the verification gate judges.

package main

import (
	"path/filepath"
	"strings"
	"unicode"
)

// setupCommands start a line without being what it runs.
var setupCommands = map[string]bool{"cd": true, "export": true, "source": true, ".": true,
	"set": true, "pushd": true}

// commandProgram is the program a command line exists to run: the first
// segment that is not setup (`cd x &&`), past assignments and prefix words,
// as a base name, with an interpreter's version dropped (python3.12 is
// python) and `python -m X` read as X.
func commandProgram(command string) string {
	for _, seg := range commandSegments(command) {
		fields := strings.Fields(seg)
		i := commandPosition(fields)
		if i >= len(fields) {
			continue
		}
		head := filepath.Base(fields[i])
		if setupCommands[head] {
			continue
		}
		if strings.HasPrefix(head, "python") {
			head = "python"
			if i+2 < len(fields) && fields[i+1] == "-m" {
				return fields[i+2]
			}
		}
		return head
	}
	return ""
}

// inlineInterpreterFlags are the flags that make an interpreter run code
// given on its command line rather than a file.
var inlineInterpreterFlags = map[string][]string{
	"python": {"-c"},
	"node":   {"-e", "--eval", "-p", "--print"},
	"ruby":   {"-e"},
	"perl":   {"-e", "-E"},
	"php":    {"-r"},
}

// inlineCode returns the code an interpreter segment runs from its command
// line (`python -c "..."`), and whether the segment is such a run. The code
// is cut from the raw segment, so its own line breaks survive.
func inlineCode(segment string) (string, bool) {
	fields := strings.Fields(segment)
	i := commandPosition(fields)
	if i >= len(fields) {
		return "", false
	}
	prog := filepath.Base(fields[i])
	if strings.HasPrefix(prog, "python") {
		prog = "python"
	}
	for j := i + 1; j < len(fields); j++ {
		// Past the script, the module or stdin, a flag is the program's own
		// argument: `node app.js -e` runs app.js.
		if tok := strings.Trim(fields[j], `"'`); tok == "-" || tok == "-m" ||
			(!strings.HasPrefix(tok, "-") && filepath.Ext(tok) != "") {
			return "", false
		}
		for _, f := range inlineInterpreterFlags[prog] {
			if fields[j] != f {
				continue
			}
			code := strings.Join(fields[j+1:], " ")
			if k := flagOffset(segment, f); k >= 0 {
				code = segment[k+len(f):]
			}
			return strings.Trim(strings.TrimSpace(code), `"'`), true
		}
	}
	return "", false
}

// flagOffset is the byte offset of flag as a whole word in segment, or -1.
func flagOffset(segment, flag string) int {
	for from := 0; ; {
		k := strings.Index(segment[from:], flag)
		if k < 0 {
			return -1
		}
		k += from
		end := k + len(flag)
		if (k == 0 || unicode.IsSpace(rune(segment[k-1]))) &&
			(end == len(segment) || unicode.IsSpace(rune(segment[end]))) {
			return k
		}
		from = end
	}
}

// inlineCodeTouchesNothing reports whether every segment a passing command
// binds files from runs inline code that neither names nor imports a file
// this session changed. Such a run exercised nothing the work is made of. A
// `cd` among the segments only places the run.
func inlineCodeTouchesNothing(ctx *AgentContext, ev commandEvidence) bool {
	changed := changedPathsForCoverage(ctx)
	ran := false
	for _, seg := range ev.Covering {
		fields := strings.Fields(seg)
		if k := commandPosition(fields); k < len(fields) && fields[k] == "cd" {
			continue
		}
		code, ok := inlineCode(seg)
		if !ok {
			return false
		}
		ran = true
		// The code runs to the end of the segment, so a file passed to it as
		// an argument is in it too.
		for _, p := range changed {
			for _, stmt := range strings.Split(code, ";") {
				if sourceReferencesPath(strings.TrimSpace(stmt), p) {
					return false
				}
			}
		}
	}
	return ran
}

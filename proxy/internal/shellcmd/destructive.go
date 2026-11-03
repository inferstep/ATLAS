// Package shellcmd reads a shell command: what it runs, what it demonstrates,
// and whether its form is one no command may use. It holds no run state.
package shellcmd

import (
	"path/filepath"
	"regexp"
	"strings"
)

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
	i := CommandPosition(fields)
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

// headOnlyProbeRe matches curl/wget invocations that fetch headers only:
// `curl -I`, `curl --head`, `wget --spider`. Long-form and clustered short
// flags (-sI) both count.
var headOnlyProbeRe = regexp.MustCompile(`(?i)\b(?:curl\b[^|;&]*?\s-{1,2}(?:I\b|head\b)|curl\b[^|;&]*?\s-[a-zA-Z]*I[a-zA-Z]*\b|wget\b[^|;&]*?--spider\b)`)

// IsHeadOnlyProbe reports a probe that fetches headers and no body. Such a
// probe is not verification (see CommandEvidenceKind.Verifies) — this says so out
// loud instead of declining in silence.
//
// Measured (stabilization cycle 6, both flask_pause sessions): the run
// installed the missing dependency, started the app, and probed it with
// `curl -I`, which answered HTTP 200. The probe was declined, nothing said
// why, the run repeated it, and both sessions ended
// verification_demanded_unmet with working code on disk.
func IsHeadOnlyProbe(cmd string) bool {
	segs, _, _ := SplitTopLevelShell(cmd)
	for _, seg := range segs {
		words := ProgramWords(ShellFields(strings.TrimSpace(seg)))
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

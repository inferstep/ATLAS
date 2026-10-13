package main

import (
	"path/filepath"
	"regexp"
	"strings"
)

const (
	maxShellWrapperDepth       = 16
	shellWrapperQuotingRefusal = "refused: the command's quoting could not be checked; simplify it."
	shellWrapperMissingRefusal = "refused: the execution wrapper is incomplete; simplify it."
	shellWrapperDepthRefusal   = "refused: the command exceeds 16 execution wrapper layers; simplify it."
)

type shellInspection struct {
	command string
	depth   int
}

// A refusal is distinct from finding no wrapper, even when no code was extracted.
type shellWrapperResult struct {
	command   string
	depth     int
	unwrapped bool
	refusal   string
}

func inspectShellCommand(cmd string) string {
	pending := []shellInspection{{command: strings.TrimSpace(cmd)}}
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		seg := strings.TrimSpace(current.command)
		if shellForkBombRe.MatchString(seg) {
			return "refused: that is a fork bomb — it would exhaust the sandbox's process table. If you need to spawn processes, run them one at a time."
		}
		if shellDeviceWriteRe.MatchString(seg) {
			return "refused: writing to a block device is blocked. Work with files under the project directory instead."
		}
		segments := commandSegments(seg)
		if len(segments) > 1 {
			for _, part := range segments {
				pending = append(pending, shellInspection{command: part, depth: current.depth})
			}
			continue
		}
		if msg := catastrophicCommand(seg); msg != "" {
			return msg
		}
		result := unwrapShellWrapper(seg, current.depth)
		if result.refusal != "" {
			return result.refusal
		}
		if result.unwrapped {
			pending = append(pending, shellInspection{command: result.command, depth: result.depth})
		}
	}
	return ""
}

// shellCommandWords splits only the shell words needed to inspect wrappers.
// Completed words and the partial final word survive a parsing failure so the
// caller can identify an execution layer and refuse it rather than overlook it.
func shellCommandWords(seg string) ([]string, bool) {
	var words []string
	for i := 0; i < len(seg); {
		for i < len(seg) && strings.ContainsRune(" \t\r\n", rune(seg[i])) {
			i++
		}
		if i == len(seg) {
			break
		}
		var word strings.Builder
		quote := byte(0)
		for i < len(seg) {
			c := seg[i]
			if quote == 0 && strings.ContainsRune(" \t\r\n", rune(c)) {
				break
			}
			switch {
			case c == '$' && quote == 0 && i+1 < len(seg) && (seg[i+1] == '\'' || seg[i+1] == '"'):
				// ANSI-C and locale quotes need expansions this lexer does not model.
				return append(words, word.String()), false
			case c == '\'' && quote != '"':
				if quote == '\'' {
					quote = 0
				} else {
					quote = '\''
				}
			case c == '"' && quote != '\'':
				if quote == '"' {
					quote = 0
				} else {
					quote = '"'
				}
			case c == '\\' && quote != '\'':
				if i+1 == len(seg) {
					return append(words, word.String()), false
				}
				i++
				next := seg[i]
				if quote == '"' && !strings.ContainsRune("$`\"\\\n", rune(next)) {
					word.WriteByte('\\')
				}
				if next != '\n' {
					word.WriteByte(next)
				}
			default:
				word.WriteByte(c)
			}
			i++
		}
		words = append(words, word.String())
		if quote != 0 {
			return words, false
		}
	}
	return words, true
}

// unwrapShellWrapper locates execution layers after supported command prefixes.
// Every env split and shell/eval extraction shares the current path's budget.
func unwrapShellWrapper(seg string, depth int) shellWrapperResult {
	words, valid := shellCommandWords(seg)
	if !valid && len(words) == 1 &&
		(strings.HasPrefix(seg, "$'") || strings.HasPrefix(seg, `$"`)) {
		// Refuse unsupported encoded heads, not opaque fragments from splitting.
		return shellWrapperResult{refusal: shellWrapperQuotingRefusal}
	}
	env := expandEnvSplitStrings(words, valid, depth)
	if env.refusal != "" {
		return shellWrapperResult{refusal: env.refusal}
	}
	i := commandPosition(env.words)
	if i < len(env.words) {
		command, wrapped, refusal := extractExecutionWrapper(env.words[i:], valid)
		if refusal != "" {
			return shellWrapperResult{refusal: refusal}
		}
		if wrapped {
			if env.depth >= maxShellWrapperDepth {
				return shellWrapperResult{refusal: shellWrapperDepthRefusal}
			}
			// The extracted code is another shell input, not an opaque argument.
			if _, ok := shellCommandWords(command); !ok {
				return shellWrapperResult{refusal: shellWrapperQuotingRefusal}
			}
			return shellWrapperResult{command: command, depth: env.depth + 1, unwrapped: true}
		}
	}
	if env.expanded {
		return shellWrapperResult{command: quoteShellWords(env.words[i:]), depth: env.depth, unwrapped: true}
	}
	return shellWrapperResult{}
}

type envSplitExpansion struct {
	words    []string
	depth    int
	expanded bool
	refusal  string
}

func expandEnvSplitStrings(words []string, valid bool, depth int) envSplitExpansion {
	result := envSplitExpansion{words: words, depth: depth}
	prefixed := false
	for i := 0; i < len(result.words); {
		head := filepath.Base(result.words[i])
		switch {
		case commandPrefixWords[head]:
			prefixed = true
			if head == "env" {
				result = expandEnvOptions(result, i, valid)
				if result.refusal != "" {
					return result
				}
			}
			i = skipCommandPrefix(result.words, i)
		case strings.Contains(result.words[i], "=") && !strings.HasPrefix(result.words[i], "-"):
			i++
		default:
			if prefixed && !valid && i == len(result.words)-1 {
				result.refusal = shellWrapperQuotingRefusal
			}
			return result
		}
	}
	if prefixed && !valid {
		result.refusal = shellWrapperQuotingRefusal
	}
	return result
}

func expandEnvOptions(result envSplitExpansion, index int, valid bool) envSplitExpansion {
	for k := index + 1; k < len(result.words); {
		option := normalizeEnvSplitOption(result.words[k])
		text, end, split := envSplitString(result.words, k, option)
		if split {
			if !valid {
				result.refusal = shellWrapperQuotingRefusal
				return result
			}
			if end > len(result.words) {
				result.refusal = shellWrapperMissingRefusal
				return result
			}
			if result.depth >= maxShellWrapperDepth {
				result.refusal = shellWrapperDepthRefusal
				return result
			}
			words, ok := shellCommandWords(text)
			if !ok {
				result.refusal = shellWrapperQuotingRefusal
				return result
			}
			result.words = append(append(append([]string{}, result.words[:k]...), words...), result.words[end:]...)
			result.depth++
			result.expanded = true
			continue // A split may expose another -S at this same position.
		}
		if option == "--" {
			break
		}
		if strings.HasPrefix(option, "-") {
			k++
			if wrapperOptionTakesValue("env", option) {
				k++
			}
			continue
		}
		if strings.Contains(option, "=") {
			k++
			continue
		}
		break
	}
	return result
}

// Normalize GNU env's combined flags and unambiguous split-string abbreviations.
func normalizeEnvSplitOption(option string) string {
	if strings.HasPrefix(option, "-") && !strings.HasPrefix(option, "--") {
		short := strings.TrimLeft(option[1:], "i0v")
		if strings.HasPrefix(short, "S") {
			option = "-" + short
		}
	}
	if name, value, attached := strings.Cut(option, "="); len(name) > 2 &&
		strings.HasPrefix(name, "--") && strings.HasPrefix("--split-string", name) {
		option = "--split-string"
		if attached {
			option += "=" + value
		}
	}
	return option
}

func envSplitString(words []string, index int, option string) (string, int, bool) {
	switch {
	case option == "-S" || option == "--split-string":
		if index+1 >= len(words) {
			return "", index + 2, true
		}
		return words[index+1], index + 2, true
	case strings.HasPrefix(option, "--split-string="):
		return strings.TrimPrefix(option, "--split-string="), index + 1, true
	case strings.HasPrefix(option, "-S"):
		return option[2:], index + 1, true
	}
	return "", index + 1, false
}

func extractExecutionWrapper(words []string, valid bool) (string, bool, string) {
	switch filepath.Base(words[0]) {
	case "bash", "sh", "zsh", "dash", "ksh":
		index, commandString := shellCommandOperand(words)
		// Unparseable options or a first operand can hide -c from this lexer.
		if !valid && (commandString || index >= len(words)-1) {
			return "", false, shellWrapperQuotingRefusal
		}
		if !commandString {
			return "", false, ""
		}
		if index >= len(words) {
			return "", false, shellWrapperMissingRefusal
		}
		return words[index], true, ""
	case "eval":
		if !valid {
			return "", false, shellWrapperQuotingRefusal
		}
		return extractEvalCommand(words), true, ""
	}
	return "", false, ""
}

// -c selects the first operand after all shell options, including combined flags.
func shellCommandOperand(words []string) (int, bool) {
	j, commandString := 1, false
	for j < len(words) {
		option := words[j]
		if option == "--" || option == "-" {
			j++
			break
		}
		if len(option) < 2 || (option[0] != '-' && option[0] != '+') {
			break
		}
		if option == "--rcfile" || option == "--init-file" {
			j += 2
			continue
		}
		if !strings.HasPrefix(option, "--") {
			if option[0] == '-' && strings.Contains(option[1:], "c") {
				commandString = true
			}
			if strings.ContainsAny(option[1:], "oO") {
				j += 2
				continue
			}
		}
		j++
	}
	return j, commandString
}

func extractEvalCommand(words []string) string {
	i := 1
	if i < len(words) && words[i] == "--" {
		i++
	}
	return strings.Join(words[i:], " ")
}

// Preserve literal punctuation in arguments revealed by env -S.
func quoteShellWords(words []string) string {
	quoted := make([]string, len(words))
	for i, word := range words {
		if word == "" || strings.ContainsAny(word, " \t\r\n;|&()<>$`\"'\\*?[]{}~#") {
			word = "'" + strings.ReplaceAll(word, "'", "'\\''") + "'"
		}
		quoted[i] = word
	}
	return strings.Join(quoted, " ")
}

// commandPrefixWords run the command that follows them.
var commandPrefixWords = map[string]bool{
	"sudo": true, "doas": true, "env": true, "command": true, "nice": true,
	"nohup": true, "time": true, "exec": true, "builtin": true, "timeout": true,
	"stdbuf": true,
}

// wrapperArgRe matches a prefix word's own options and values: `nice -n 10`,
// `timeout 5s`, `env -i`.
var wrapperArgRe = regexp.MustCompile(`^(-.*|\d+(\.\d+)?[smhd]?)$`)

// Some prefix options take a separate value. Skipping only the option would
// mistake that value for the executable (for example `sudo -u root bash -c`).
func wrapperOptionTakesValue(wrapper, option string) bool {
	switch wrapper {
	case "sudo":
		switch option {
		case "-u", "--user", "-g", "--group", "-h", "--host", "-C", "--close-from", "-p", "--prompt", "-r", "--role", "-t", "--type":
			return true
		}
	case "doas":
		return option == "-u"
	case "env":
		switch option {
		case "-u", "--unset", "-C", "--chdir", "-S", "--split-string":
			return true
		}
	case "nice":
		return option == "-n" || option == "--adjustment"
	case "timeout":
		switch option {
		case "-s", "--signal", "-k", "--kill-after":
			return true
		}
	case "stdbuf":
		return option == "-i" || option == "-o" || option == "-e"
	case "time":
		return option == "-f" || option == "--format" || option == "-o" || option == "--output"
	}
	return false
}

func skipCommandPrefix(words []string, index int) int {
	wrapper := filepath.Base(words[index])
	index++
	for index < len(words) && wrapperArgRe.MatchString(words[index]) {
		option := words[index]
		index++
		if strings.HasPrefix(option, "-") && wrapperOptionTakesValue(wrapper, option) && index < len(words) {
			index++
		}
	}
	return index
}

// commandPosition returns the index of the word a segment actually runs,
// past variable assignments and prefix words with their arguments.
func commandPosition(fields []string) int {
	i := 0
	for i < len(fields) {
		switch f := fields[i]; {
		case commandPrefixWords[filepath.Base(f)]:
			i = skipCommandPrefix(fields, i)
		case strings.Contains(f, "=") && !strings.HasPrefix(f, "-"):
			i++ // VAR=value
		default:
			return i
		}
	}
	return i
}

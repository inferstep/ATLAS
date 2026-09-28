package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// A tool call that arrives cut off -- the stream was stopped because the output
// began repeating, or it hit the token cap -- is never reconstructed or
// executed: its bytes are unknowable from a prefix. But the prefix is not
// nothing. Fields that arrived as complete JSON tokens (the tool name, the
// path, a selector) are known exactly, and the file they name is on disk.
//
// The refusal used to discard all of that. Measured in stabilization cycle 2
// (flask_pause, both repetitions, byte-identical requests): a structural_edit
// selecting `function:index` with the whole HTML template as content was cut
// by the repetition detector. c5927b3 reconstructed and executed it, and the
// shrinkage guard refused it with guidance grounded in the file -- the bulk of
// app.py is the HTML_TEMPLATE string, which no selector reaches; use
// replace_lines, edit_file on one line or insert_after -- and both sessions
// recovered. The candidate refused the cut call with a generic message about
// retyping data, and both sessions went on to fail.
//
// cutCallDiagnostic says what the prefix does establish, from complete tokens
// and the verified workspace only, and names supported ways to make the
// change. It never authorises execution and never supplies missing content.

// attemptEcho is the model's own unparseable output, put back into the
// conversation as the assistant turn it was, so the next turn does not start
// from the same prefix.
//
// Measured (stabilization cycle 3, probes K and L, one captured context,
// greedy decoding). The candidate dropped the attempt and appended advice: the
// conversation up to that advice was byte-identical to the prefix that had
// produced the cut call, and 9 of 10 generations regenerated a call long
// enough to be cut again. With the attempt present as an assistant turn and
// the same grounded refusal after it, 10 of 10 generations made a valid small
// edit. The baseline reached that state only by EXECUTING the reconstructed
// call first, which is the thing that must not happen.
//
// Only bytes that arrived are echoed. A long attempt is elided, and the
// elision says so.
const cutAttemptKeepRunes = 400

func attemptEcho(raw string, cut bool) string {
	r := []rune(strings.TrimRight(raw, "\n"))
	if len(r) <= cutAttemptKeepRunes {
		return string(r)
	}
	tail := "\n… [the rest of this reply is not repeated here; nothing was executed]"
	if cut {
		tail = "\n… [cut here — the rest of this call never arrived and nothing was executed]"
	}
	return string(r[:cutAttemptKeepRunes]) + tail
}

// recoveredCall holds the fields of a cut tool call that arrived complete.
type recoveredCall struct {
	Tool     string
	Args     map[string]string // complete string values only
	CutField string            // the argument whose value was cut, if known
	CutLines int               // lines of that value received before the cut
}

// recoverCutCall reads a cut tool-call response token by token and keeps only
// values that arrived whole. A string still open at the cut is not a token and
// is never returned.
func recoverCutCall(raw string) recoveredCall {
	rc := recoveredCall{Args: map[string]string{}}
	dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(raw)))
	depth := 0
	var key string
	inArgs := false
	expectKey := map[int]bool{}
	lastArgKey := ""
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{':
				depth++
				expectKey[depth] = true
				if depth == 2 && key == "args" {
					inArgs = true
				}
			case '}':
				if depth == 2 {
					inArgs = false
				}
				depth--
				if depth >= 1 {
					expectKey[depth] = true
				}
			case '[':
				depth++
			case ']':
				depth--
				if depth >= 1 {
					expectKey[depth] = true
				}
			}
			continue
		case string:
			if expectKey[depth] {
				key = v
				expectKey[depth] = false
				if inArgs && depth == 2 {
					lastArgKey = v
				}
				continue
			}
			if depth == 1 && key == "name" {
				rc.Tool = v
			}
			if inArgs && depth == 2 {
				rc.Args[key] = v
			}
		default:
			if inArgs && depth == 2 && key != "" {
				rc.Args[key] = fmt.Sprint(v)
			}
		}
		expectKey[depth] = true
	}
	if lastArgKey != "" {
		if _, done := rc.Args[lastArgKey]; !done {
			rc.CutField = lastArgKey
			marker := `"` + lastArgKey + `":"`
			if i := strings.LastIndex(raw, marker); i >= 0 {
				rc.CutLines = strings.Count(raw[i+len(marker):], `\n`) + 1
			}
		}
	}
	return rc
}

var writeToolNames = map[string]bool{"write_file": true, "edit_file": true, "structural_edit": true,
	"insert_after": true, "replace_lines": true}

// cutCallDiagnostic is the grounded part of the refusal for a cut tool call,
// or "" when the prefix names no write tool.
func cutCallDiagnostic(ctx *AgentContext, raw string) string {
	rc := recoverCutCall(raw)
	if !writeToolNames[rc.Tool] {
		return ""
	}
	var sb strings.Builder
	path := rc.Args["path"]
	source, readable := "", false
	if path != "" && ctx != nil {
		args, _ := json.Marshal(map[string]string{"path": path})
		if reason := validateToolWorkspacePaths(rc.Tool, args, ctx); reason != "" {
			fmt.Fprintf(&sb, "What arrived was an unfinished %s call whose path is outside the workspace; nothing was executed.", rc.Tool)
			return sb.String()
		}
		if b, err := os.ReadFile(resolveAgentPath(ctx, path)); err == nil {
			source, readable = string(b), true
		}
	}
	switch {
	case path == "":
		fmt.Fprintf(&sb, "What arrived was an unfinished %s call, cut before its path was complete, so which file it "+
			"targeted cannot be said. Nothing was executed.", rc.Tool)
	case readable:
		fmt.Fprintf(&sb, "What arrived was an unfinished %s call on %s. Nothing was executed and %s is unchanged.",
			rc.Tool, path, path)
	default:
		fmt.Fprintf(&sb, "What arrived was an unfinished %s call for %s. Nothing was executed.", rc.Tool, path)
	}
	if rc.CutField != "" && rc.CutLines > 1 {
		fmt.Fprintf(&sb, " It was cut inside `%s` after about %d lines.", rc.CutField, rc.CutLines)
	}
	if !readable {
		if rc.Tool == "write_file" && path != "" {
			sb.WriteString(" To write it without one long call: ")
			if fencedChannelDisabledForSession(ctx) {
				sb.WriteString("write the first part with write_file and add the rest with insert_after in parts.")
			} else {
				fmt.Fprintf(&sb, "send write_file for %s with \"content\": \"@fenced\" so the file arrives as one "+
					"fenced block, or write it in parts with write_file and insert_after.", path)
			}
		}
		return sb.String()
	}
	name := filepath.Base(path)
	constants := largeStringConstants(path, source)
	selector := rc.Args["selector"]
	var regions []EmbeddedRegion
	if strings.HasSuffix(path, ".py") {
		_, _, regions = outlineViaV3(ctx, path, source)
	}
	if rc.Tool == "structural_edit" && selector != "" {
		if span, ok := selectorSpan(ctx, path, source, selector); ok {
			fmt.Fprintf(&sb, " In %s, `%s` is %s.", name, selector, span.describe())
			if rc.CutField == "content" && rc.CutLines >= 5*span.lines() && rc.CutLines-span.lines() >= 30 {
				sb.WriteString(" The replacement was already many times that size, so it was not that node's code.")
			}
		} else {
			fmt.Fprintf(&sb, " %s has no node `%s` (%s).", name, selector, selectorGuidanceOrOutline(path, source))
		}
	}
	if len(constants) > 0 {
		fmt.Fprintf(&sb, " Most of %s is %s, a module-level string; no selector reaches inside a string literal.",
			name, strings.Join(constants, " and "))
	}
	for _, r := range regions {
		if len(r.Symbols) == 0 {
			continue
		}
		fmt.Fprintf(&sb, " The %s functions %s are at lines %d-%d, in %s; they are not selectable either.",
			r.Kind, strings.Join(r.Symbols, ", "), r.StartLine, r.EndLine, r.Where)
	}
	sb.WriteString(" To change this code without one long call: ")
	if len(constants) > 0 || rc.Tool != "write_file" {
		sb.WriteString("use replace_lines with the line numbers read_file printed (up to 60 lines per call; for more, " +
			"several calls from the bottom of the range up), edit_file with old_str set to one unique line, or " +
			"insert_after to add lines at a line number.")
	} else if fencedChannelDisabledForSession(ctx) {
		sb.WriteString("write the first part with write_file and add the rest with insert_after in parts.")
	} else {
		fmt.Fprintf(&sb, "send write_file for %s with \"content\": \"@fenced\" so the file arrives as one fenced block, "+
			"or write it in parts with write_file and insert_after.", path)
	}
	return sb.String()
}

type lineSpan struct{ first, last int }

func (s lineSpan) lines() int { return s.last - s.first + 1 }

func (s lineSpan) describe() string {
	if s.first == s.last {
		return fmt.Sprintf("line %d (1 line)", s.first)
	}
	return fmt.Sprintf("lines %d-%d (%d lines)", s.first, s.last, s.lines())
}

// selectorSpan finds the lines a structural_edit selector names, using the same
// outline the outline_file tool shows.
func selectorSpan(ctx *AgentContext, path, source, selector string) (lineSpan, bool) {
	kind, name, ok := strings.Cut(selector, ":")
	if !ok {
		return lineSpan{}, false
	}
	var syms []OutlineSymbol
	if strings.HasSuffix(path, ".py") {
		if v3, ok, _ := outlineViaV3(ctx, path, source); ok {
			syms = v3
		}
	}
	if syms == nil {
		syms = outlineByRegex(path, source)
	}
	for _, s := range syms {
		if s.Name == name && (s.Kind == kind || (kind == "function" && strings.Contains(s.Kind, "function"))) {
			return lineSpan{s.StartLine, s.EndLine}, true
		}
	}
	return lineSpan{}, false
}

var pyStringConstantOpen = regexp.MustCompile(`^([A-Za-z_]\w*)\s*=\s*[rRbBuUfF]{0,2}("""|''')`)

// largeStringConstants names module-level triple-quoted string constants of
// 20+ lines in a Python file: where a template, and the code inside it, lives.
func largeStringConstants(path, source string) []string {
	if !strings.HasSuffix(path, ".py") {
		return nil
	}
	lines := strings.Split(source, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		m := pyStringConstantOpen.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		delim := m[2]
		rest := lines[i][strings.Index(lines[i], delim)+3:]
		if strings.Contains(rest, delim) {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if strings.Contains(lines[j], delim) {
				if j-i+1 >= 20 {
					out = append(out, fmt.Sprintf("`%s` (lines %d-%d)", m[1], i+1, j+1))
				}
				i = j
				break
			}
		}
		if len(out) == 2 {
			break
		}
	}
	return out
}

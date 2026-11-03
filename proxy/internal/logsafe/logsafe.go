package logsafe

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// Diagnostic summaries for operator logs.
//
// An agent trace line used to carry the tool call's arguments truncated to a
// character budget. A character budget is not a policy: it decides how much
// model-authored source reaches the log, never whether any does. The sealed
// Stage-A acquisition wrote 291 such lines carrying a content or command
// argument, and container stdout is captured into the acquisition's evidence
// archive, so those fragments became evidence-resident and were sealed.
//
// The owner of that decision is here, and it works on the PARSED arguments
// rather than on the serialized string: knowing which field a value came from
// is what makes redaction decidable. A regex over the finished line cannot
// tell a path from a body, and a list of forbidden words grows forever.
//
// The rule is an allowlist per tool. Anything not declared safe is summarized,
// so a new tool, or a new field on an existing tool, is redacted until someone
// decides otherwise -- the failure mode is a less useful log line, never a
// leak. filterPrivateValues stays installed on the logger underneath this as
// defence in depth.
//
// A summary may keep: the tool name, the canonical path (already permitted in
// these lines), the body's byte length, a stable content hash, and selector or
// range metadata. It may not keep source, commands, credentials, prompts, or
// any model-authored body text. The hash is an operator affordance for
// correlating two log lines; it is never offered to the model or to an
// external client.

// toolSafeArgFields declares, per registered tool, the argument fields whose
// values may appear verbatim in a log line. Every other field is summarized.
//
// Paths and path-shaped fields are already permitted in these lines. Numbers,
// booleans and job ids carry no authored text. Selectors are structural
// coordinates the model chose from the file's own symbol names, and they are
// what makes an edit trace readable.
//
// Deliberately absent, and why:
//
//	content, old_str, new_str  model-authored source
//	expected_first_line, expected_last_line  quoted source lines
//	command                    shell text, the most likely place for a secret
//	pattern                    model-authored text that can embed one
var toolSafeArgFields = map[string]map[string]bool{
	"read_file":       {"path": true, "offset": true, "limit": true},
	"outline_file":    {"path": true},
	"search_files":    {"path": true, "glob": true},
	"list_directory":  {"path": true},
	"find_file":       {"path": true},
	"write_file":      {"path": true},
	"edit_file":       {"path": true, "replace_all": true},
	"insert_after":    {"path": true, "line": true},
	"replace_lines":   {"path": true, "start_line": true, "end_line": true},
	"structural_edit": {"path": true, "selector": true},
	"delete_file":     {"path": true},
	"move_file":       {"source": true, "destination": true},
	"run_command":     {"cwd": true, "timeout": true},
	"run_background":  {"cwd": true, "settle_ms": true},
	"tail_background": {"job_id": true, "lines": true},
	"stop_background": {"job_id": true},
}

// redactedValue is what a summarized field becomes: enough to correlate two
// lines and size a payload, nothing to read.
func redactedValue(raw json.RawMessage) string {
	var s string
	body := []byte(raw)
	if json.Unmarshal(raw, &s) == nil {
		body = []byte(s)
	}
	return fmt.Sprintf("<redacted %dB sha256:%s>", len(body), sha256Hex(body)[:12])
}

// ArgsSummary renders a tool call's arguments for an operator log.
//
// Fails closed at every step: unknown tool, unknown field, unparseable
// arguments and non-object arguments all summarize rather than fall through
// to the raw bytes.
func ArgsSummary(tool string, args json.RawMessage) string {
	trimmed := strings.TrimSpace(string(args))
	if trimmed == "" || trimmed == "null" {
		return "<no args>"
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil {
		// Not an object -- a list, a scalar, or malformed. Nothing here can
		// be attributed to a field, so none of it is shown.
		return redactedValue(args)
	}
	safe := toolSafeArgFields[tool] // nil for an unregistered tool: nothing is safe
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys) // one line shape for the same call, every time
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if safe[k] {
			parts = append(parts, fmt.Sprintf("%q:%s", k, string(fields[k])))
			continue
		}
		parts = append(parts, fmt.Sprintf("%q:%q", k, redactedValue(fields[k])))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// TextSummary describes model-authored text without reproducing it. Used
// where a raw reply or response was previously quoted into the log.
func TextSummary(s string) string {
	return fmt.Sprintf("<%d chars sha256:%s>", len(s), sha256Hex([]byte(s))[:12])
}

// DiagnosticSummary keeps a checker's classification and drops the source
// it quotes. A syntax error's first line names the error; everything after it
// is the offending source and a caret.
//
// The classification is what a reader acts on ("SyntaxError: unmatched ']'"),
// and it is generated by the checker rather than authored by the model.
func DiagnosticSummary(detail string) string {
	head := detail
	if i := strings.IndexByte(head, '\n'); i >= 0 {
		head = head[:i]
	}
	head = strings.TrimSpace(head)
	if len(head) > 120 {
		head = head[:120]
	}
	if len(detail) == len(head) {
		return head
	}
	return fmt.Sprintf("%s <+%d chars elided sha256:%s>",
		head, len(detail)-len(head), sha256Hex([]byte(detail))[:12])
}

// escapeControls prints every control character but tab, and the Unicode line
// and paragraph separators, as text, so no logged value can start a line of its
// own or reach a terminal as a control code. A byte that is not UTF-8 becomes
// the replacement character. The \uXXXX form stays valid inside a JSON
// string: logEvent's finished records pass through here, and json.Marshal
// leaves DEL and the C1 controls raw.
func escapeControls(s string) string {
	var out strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			out.WriteString(`\n`)
		case r == '\r':
			out.WriteString(`\r`)
		case r != '\t' && unicode.IsControl(r), unicode.In(r, unicode.Zl, unicode.Zp):
			fmt.Fprintf(&out, `\u%04x`, r)
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

// Classified reports whether the logging rule names a tool's safe fields.
func Classified(tool string) bool {
	_, ok := toolSafeArgFields[tool]
	return ok
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

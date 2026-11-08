package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/itigges22/atlas-proxy/internal/wire"
)

// canonicalToolArgs is the single reading of a tool call's arguments. It decodes
// them once, judges each workspace-path argument, and returns a canonical
// re-encoding of exactly what it judged; a handler decoding the returned bytes
// cannot see a value the check did not. The second return value is a refusal
// saying what was refused, why, and what to send, or "" when the call may
// proceed. On a refusal the returned bytes are nil and the caller leaves the
// call's arguments unchanged.
//
// Arguments that are not a JSON object are not a call any handler acts on: the
// original bytes are returned so the handler reports its own decode error, and a
// top-level null is the no-arguments case, returned the same way. Neither
// carries a path, so neither can reach a file.
func canonicalToolArgs(name string, args json.RawMessage, ctx *AgentContext) (json.RawMessage, string) {
	if getTool(name) == nil {
		// No handler runs for an unknown tool, so nothing reads these
		// arguments; the dispatcher answers "unknown tool" whatever they are.
		return args, ""
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil || fields == nil {
		return args, ""
	}
	if dup := repeatedArgName(args); dup != "" {
		return nil, fmt.Sprintf("%s: %s is given more than once; give it once", name, dup)
	}
	if k := nonCanonicalArgName(fields); k != "" {
		return nil, fmt.Sprintf("%s: argument names are lowercase, as the tool lists them; %q is not", name, k)
	}
	for _, key := range workspacePathFields[name] {
		raw, ok := fields[key]
		if rejection := judgeWorkspacePathArg(ctx, name, key, raw, ok); rejection != "" {
			return nil, rejection
		}
	}
	canonical, err := encodeCanonicalArgs(fields)
	if err != nil {
		return nil, fmt.Sprintf("%s: arguments could not be read as one value", name)
	}
	return canonical, ""
}

// enforceWorkspacePaths runs the check on a model tool call and, when the call is
// allowed, replaces its arguments with the single reading the check judged, so
// every later reader of the call works from that one form. It returns a refusal,
// or "" to proceed; on a refusal the call's arguments are left unchanged.
func enforceWorkspacePaths(parsed *wire.ModelResponse, ctx *AgentContext) string {
	canonical, rejection := canonicalToolArgs(parsed.Name, parsed.Args, ctx)
	if rejection != "" {
		return rejection
	}
	parsed.Args = canonical
	return ""
}

// judgeWorkspacePathArg judges one path argument. An absent argument, an explicit
// null and an empty value are passed over, as the handler reads them as empty. A
// value that is present but not a string is refused, where the check used to skip
// it. A padded value is refused, because it would be checked in one form and
// recorded in another. Anything outside the workspace is refused.
func judgeWorkspacePathArg(ctx *AgentContext, name, key string, raw json.RawMessage, present bool) string {
	if !present || string(raw) == "null" {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return fmt.Sprintf("%s: %s must be a string, as the tool lists it", name, key)
	}
	if strings.TrimSpace(value) == "" {
		return ""
	}
	if value != strings.TrimSpace(value) {
		return fmt.Sprintf("%s: %s %q starts or ends with whitespace", name, key, value)
	}
	if _, err := resolveWorkspacePath(ctx, value); err != nil {
		return fmt.Sprintf("%s: %v", name, err)
	}
	return ""
}

// repeatedArgName returns a top-level argument name given more than once, or "".
// encoding/json keeps only the last value for a repeated name, so the check and
// a handler could otherwise read one call two ways. Only top-level names are
// judged, which are the only ones the check reads.
func repeatedArgName(args json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(args))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return ""
	}
	seen := make(map[string]struct{})
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		key, ok := tok.(string)
		if !ok {
			return ""
		}
		if _, repeated := seen[key]; repeated {
			return key
		}
		seen[key] = struct{}{}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return ""
		}
	}
	return ""
}

// encodeCanonicalArgs re-encodes the judged arguments. Each value keeps its raw
// bytes, so a large number or a nested value is carried exactly and never
// widened into a float, and HTML escaping is off so markup in a value is not
// rewritten. The map holds one entry per name, so the result names each argument
// once.
func encodeCanonicalArgs(fields map[string]json.RawMessage) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(fields); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

type toolCheckOutput struct{ result *wire.ToolResult }

func (output *toolCheckOutput) accept(category checkCategory, result *wire.ToolResult) checkResult {
	output.result = result
	if result == nil {
		return checkResult{checkPass, turnContinues}
	}
	return checkResult{category, turnEnds}
}

func fencedCallIsExecutable(name string, args json.RawMessage, ctx *AgentContext) (bool, string) {
	input := fencedCallCheckInput{name: name, args: args, ctx: ctx}
	result := runCheckPhase(proxyChecks.fencedCall, &input)
	return result.outcome == turnContinues, input.reason
}

type fencedCallCheckInput struct {
	name   string
	args   json.RawMessage
	ctx    *AgentContext
	input  wire.WriteFileInput
	reason string
}

func (input *fencedCallCheckInput) refuse(category checkCategory, reason string) checkResult {
	input.reason = reason
	return checkResult{category, turnEnds}
}

func (input *fencedCallCheckInput) arguments() checkResult {
	trimmed := strings.TrimSpace(string(input.args))
	if trimmed == "" || trimmed == "null" {
		return input.refuse(checkModelError, "the call carried no arguments")
	}
	return checkResult{checkPass, turnContinues}
}

func (input *fencedCallCheckInput) decode() checkResult {
	if err := json.Unmarshal(input.args, &input.input); err != nil {
		return input.refuse(checkModelError, "the arguments are not the shape "+input.name+" takes")
	}
	return checkResult{checkPass, turnContinues}
}

func (input *fencedCallCheckInput) path() checkResult {
	if strings.TrimSpace(input.input.Path) == "" {
		return input.refuse(checkModelError, "the call names no path")
	}
	return checkResult{checkPass, turnContinues}
}

func (input *fencedCallCheckInput) workspace() checkResult {
	if reason := validateToolWorkspacePaths(input.name, input.args, input.ctx); reason != "" {
		return input.refuse(checkGuardRefusal, reason)
	}
	return checkResult{checkPass, turnContinues}
}

func (input *fencedCallCheckInput) denyList() checkResult {
	if denied, reason := shouldDenyToolCall(input.name, input.args); denied {
		return input.refuse(checkGuardRefusal, reason)
	}
	return checkResult{checkPass, turnContinues}
}

type lineReplacementCheckInput struct {
	toolCheckOutput
	ctx         *AgentContext
	in          wire.ReplaceLinesInput
	path        string
	original    string
	fileLines   []string
	limit       int
	history     string
	updated     string
	replacement []string
}

func (check *lineReplacementCheckInput) boundsCheck() checkResult {
	in := check.in
	limit := check.limit
	if in.StartLine < 1 || in.EndLine < in.StartLine || in.EndLine > limit {
		return check.accept(checkModelError, noMutation(fmt.Sprintf(
			"replace_lines: range %d-%d is invalid for %s, which has %d lines. Both bounds are 1-based and "+
				"inclusive, and start_line must not exceed end_line. Use the numbers read_file showed you.",
			in.StartLine, in.EndLine, in.Path, limit)))
	}
	return checkResult{checkPass, turnContinues}
}

func (check *lineReplacementCheckInput) spanCheck() checkResult {
	in := check.in
	original := check.original
	if span := in.EndLine - in.StartLine + 1; span > replaceLinesMaxSpan {
		// A size policy declining a replacement it never formed.
		return check.accept(checkGuardRefusal, noMutation(fmt.Sprintf(
			"replace_lines: %d lines is too large a range (limit %d). A replacement that size is a rewrite rather "+
				"than an edit. Split it into consecutive replace_lines calls of at most %d lines, working from "+
				"the BOTTOM of the file upward so the earlier line numbers stay valid. For a whole function "+
				"or class, structural_edit replaces the node in one call (%s) — but only for real nodes: code "+
				"inside a string literal (a <script> block in an HTML template, say) is one string to the "+
				"host grammar and no selector reaches into it, so there the split is the way.",
			span, replaceLinesMaxSpan, replaceLinesMaxSpan, selectorGuidanceOrOutline(in.Path, original))))
	}
	return checkResult{checkPass, turnContinues}
}

func (check *lineReplacementCheckInput) relocateCheck() checkResult {
	ctx := check.ctx
	in := check.in
	path := check.path
	original := check.original
	fileLines := check.fileLines
	limit := check.limit
	// A stale range that simply moved is relocated rather than
	// refused: the numbers go stale as soon as an earlier edit
	// changes the file's length, and the model cannot see that.
	if strings.TrimSpace(in.ExpectedFirstLine) != "" &&
		strings.TrimSpace(fileLines[in.StartLine-1]) != strings.TrimSpace(in.ExpectedFirstLine) {
		span := in.EndLine - in.StartLine + 1
		if moved := relocateStaleRange(fileLines, limit,
			in.ExpectedFirstLine, in.ExpectedLastLine, span); moved > 0 {
			log.Printf("[replace_lines] %s: range %d-%d is stale; the same block is at %d-%d — relocating",
				in.Path, in.StartLine, in.EndLine, moved, moved+span-1)
			in.StartLine, in.EndLine = moved, moved+span-1
		}
	}
	// The anchors did not match, so no replacement was ever formed.
	check.history = lineNumberHistory(ctx, path, in.Path, original)
	check.in = in
	return checkResult{checkPass, turnContinues}
}

func (check *lineReplacementCheckInput) firstLineCheck() checkResult {
	in := check.in
	fileLines := check.fileLines
	history := check.history
	if msg := lineAssertionMismatch(in.ExpectedFirstLine, fileLines[in.StartLine-1], in.StartLine, in.Path, fileLines, history); msg != "" {
		return check.accept(checkGuardRefusal, noMutation(msg))
	}
	return checkResult{checkPass, turnContinues}
}

func (check *lineReplacementCheckInput) lastLineCheck() checkResult {
	in := check.in
	fileLines := check.fileLines
	history := check.history
	if msg := lineAssertionMismatch(in.ExpectedLastLine, fileLines[in.EndLine-1], in.EndLine, in.Path, fileLines, history); msg != "" {
		return check.accept(checkGuardRefusal, noMutation(msg))
	}
	return checkResult{checkPass, turnContinues}
}

func (check *lineReplacementCheckInput) replacementCheck() checkResult {
	in := check.in
	original := check.original
	fileLines := check.fileLines
	replacement := strings.Split(strings.TrimSuffix(in.Content, "\n"), "\n")
	merged := append([]string{}, fileLines[:in.StartLine-1]...)
	merged = append(merged, replacement...)
	merged = append(merged, fileLines[in.EndLine:]...)
	updated := strings.Join(merged, "\n")

	if updated == original {
		return check.accept(checkGuardRefusal, noMutation("replace_lines: the replacement is identical to what is already on "+
			"those lines — nothing changed and the bug is still there."))
	}
	check.updated, check.replacement = updated, replacement
	return checkResult{checkPass, turnContinues}
}

type insertInputCheckInput struct {
	toolCheckOutput
	in wire.InsertAfterInput
}

func (check *insertInputCheckInput) path() checkResult {
	in := check.in
	if strings.TrimSpace(in.Path) == "" {
		// Naming only the missing field sends the model back with a
		// different field missing.
		return check.accept(checkModelError, noMutation("insert_after: path is required. The call is "+
			"insert_after {\"path\":\"app.py\", \"line\":<1-based number from read_file, 0 for top of file>, "+
			"\"content\":<the new lines>}."))
	}
	return checkResult{checkPass, turnContinues}
}
func (check *insertInputCheckInput) content() checkResult {
	in := check.in
	if in.Content == "" {
		return check.accept(checkModelError, noMutation("insert_after: content is empty — nothing would be inserted"))
	}
	return checkResult{checkPass, turnContinues}
}

type replaceInputCheckInput struct {
	toolCheckOutput
	in wire.ReplaceLinesInput
}

func (check *replaceInputCheckInput) path() checkResult {
	in := check.in
	if strings.TrimSpace(in.Path) == "" {
		return check.accept(checkModelError, noMutation("replace_lines: path is required. The call is "+
			"replace_lines {\"path\":\"app.py\", \"start_line\":N, \"end_line\":M, "+
			"\"expected_first_line\":<text of line N>, \"expected_last_line\":<text of line M>, "+
			"\"content\":<the new lines>}."))
	}
	return checkResult{checkPass, turnContinues}
}

type insertionCheckInput struct {
	toolCheckOutput
	in            wire.InsertAfterInput
	path          string
	lines, insert []string
	limit         int
}

func (check *insertionCheckInput) bounds() checkResult {
	in, limit := check.in, check.limit

	if in.Line < 0 || in.Line > limit {
		return check.accept(checkModelError, noMutation(fmt.Sprintf(
			"insert_after: line %d is out of range for %s, which has %d lines. Use the numbers read_file showed you.",
			in.Line, in.Path, limit)))
	}
	return checkResult{checkPass, turnContinues}
}
func (check *insertionCheckInput) reparenting() checkResult {
	in, path, lines := check.in, check.path, check.lines

	insert := strings.Split(strings.TrimSuffix(in.Content, "\n"), "\n")
	// Before any pipeline work: an insertion that moves existing
	// statements into another block is refused outright, however well
	// the result parses. See insertionReparentsPython.
	if msg, moved := insertionReparentsPython(path, lines, in.Line, insert); moved {
		return check.accept(checkGuardRefusal, structuralRefusal(msg))
	}
	check.insert = insert

	return checkResult{checkPass, turnContinues}
}

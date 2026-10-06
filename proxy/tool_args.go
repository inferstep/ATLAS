package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
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
func enforceWorkspacePaths(parsed *ModelResponse, ctx *AgentContext) string {
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
// it. Anything outside the workspace is refused.
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
// bytes, so a large number or a nested value is carried exactly and never widened
// into a float, and HTML escaping is off so markup in a value is not rewritten.
// The map holds one entry per name, so the result names each argument once.
func encodeCanonicalArgs(fields map[string]json.RawMessage) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(fields); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

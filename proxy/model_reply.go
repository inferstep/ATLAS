// What the model sent back: its reasoning, its JSON, and what to tell it when that does not parse.

package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
)

// ------------------------------------------------------------------------
// Trust model (load-bearing — read before "fixing" go/path-injection CodeQL
// alerts on this file or its helpers in the tool files, context.go, gates.go).
//
// ATLAS is a single-tenant local-install agent. The proxy runs inside a
// container whose /workspace mount IS the user's project directory: the
// agent's whole purpose is to read, edit, and verify files there on
// behalf of the user who owns that directory. The "user-controlled" value
// CodeQL flags in agent path joins is almost always ctx.WorkingDir, which
// is set ONCE at agent startup from the bind mount — not from a per-
// request input. The few cases where the model emits a path
// (resolveAgentPath callers in the tool files) go through that resolver, which
// translates host-absolute paths into the container view + cleans `..`
// segments before any os call.
//
// What we deliberately do NOT do: enforce a strict in-workspace check
// that rejects paths resolving outside /workspace. The agent legitimately
// reads outside it (e.g. /etc/os-release for tier detection, /tmp for
// scratch). The container is the isolation boundary — host-side files
// not bind-mounted in are simply not reachable.
//
// If ATLAS is ever deployed multi-tenant (e.g. shared proxy with many
// users' workspaces colocated), every site flagged by go/path-injection
// here would need a real fix. Until then they're dismissed as false
// positives with this rationale.
// ------------------------------------------------------------------------

// stripThinkTags removes common <think>...</think> reasoning markers from a
// response string. Used as a defensive cleanup when
// reasoning_content gets surfaced as content fallback — the raw
// reasoning text sometimes still has the tags wrapping it.
var thinkTagRE = regexp.MustCompile(`(?s)<think>.*?</think>`)

func stripThinkTags(s string) string {
	return strings.TrimSpace(thinkTagRE.ReplaceAllString(s, ""))
}

// recoverStructuredReasoning accepts a complete agent envelope that a chat
// template routed to reasoning_content instead of content. Parse the JSON
// rather than matching serialized substrings: whitespace and key order are
// insignificant, and both `text` and `done` are valid terminal responses.
func recoverStructuredReasoning(s string) (string, bool) {
	recovered := stripThinkTags(s)
	if recovered == "" {
		return "", false
	}
	parsed, err := extractModelResponse(recovered)
	if err != nil {
		return "", false
	}
	switch parsed.Type {
	case "tool_call":
		return recovered, parsed.Name != "" && len(parsed.Args) > 0 &&
			string(parsed.Args) != "null"
	case "text":
		return recovered, parsed.Content != ""
	case "done":
		return recovered, parsed.Summary != ""
	default:
		return "", false
	}
}

// classifyParseFailure names the shape of an unparseable response and returns a
// short stable category for the docker log (so `docker logs atlas-proxy` reads
// "what kind of broken" at a glance) and a targeted corrective for the model.
// The model can't see why parsing failed, so a generic "respond in JSON"
// message lets it loop on the same pattern; naming the failure breaks the
// cycle.
//
// `streamCut` is why the PROXY ended the generation, or "" when the model
// stopped on its own. It comes first because it is the only fact here that is
// known rather than inferred: everything below reads the wreckage and guesses.
// A loop cut mid-JSON looks truncated, and "you hit the token cap, make the
// call smaller" is then the wrong diagnosis and the wrong instruction.
//
// Categories:
//
//	loop_cut        — the proxy cut a response that was repeating itself
//	old_str_cut     — the proxy cut an old_str that stopped matching the file
//	reasoning_cut   — the reasoning budget ran out before a tool call
//	empty           — response was whitespace
//	html_entities   — tool_call contains &lt; / &gt; / &amp; in string args
//	prose           — response does not start with '{' (model narration leaking)
//	non_json        — starts with '{' but is not a tool_call
//	truncated_tool  — JSON tool_call envelope cut off mid-args (max_tokens)
//	malformed_tool  — tool_call envelope present but JSON malformed
func classifyParseFailure(raw, streamCut string) (category, feedback string) {
	switch streamCut {
	case "content_loop":
		return "loop_cut", "Your response was cut off because it had started repeating " +
			"itself — the same line over and over — so what arrived was an unfinished " +
			"tool call. The response was NOT too long for the token cap, and re-sending " +
			"a smaller version of the same call will not help.\n\nThis happens when you " +
			"try to reproduce a large block of data you already have. You do not need to " +
			"copy a file's contents to work with it: read_file already showed you the " +
			"file, and input or fixture data should be read at runtime by the code you " +
			"write, never retyped into a tool call. Write the CODE that processes the " +
			"data, not the data."
	case "old_str_unmatched":
		return "old_str_cut", "Your edit_file call was stopped while old_str was still arriving: " +
			"it had stopped matching any text in the file and kept going. Nothing was executed " +
			"and the file is unchanged. old_str must be text copied from the file; one short " +
			"line that appears once in it is enough to place an edit, or use replace_lines " +
			"with the line numbers read_file showed."
	case "reasoning_budget":
		return "reasoning_cut", "Your response was cut off: it spent the whole per-turn " +
			"budget on reasoning without emitting a tool call. Skip the deliberation and " +
			"respond with the single JSON action you want to take next."
	}
	stripped := strings.TrimSpace(raw)
	if stripped == "" {
		return "empty", "Your response was empty. Respond with ONLY a single JSON object — {\"type\":\"tool_call\",...} or {\"type\":\"text\",\"content\":\"...\"} or {\"type\":\"done\",\"summary\":\"...\"}."
	}
	// HTML-entity encoding detection: some models encode <, >, & inside
	// tool-call string args (`&lt;!DOCTYPE...&gt;`) instead of emitting
	// them literally. JSON parses fine if the whole envelope arrives,
	// but those entities then appear verbatim in old_str and do not
	// match the file. Catch and redirect whether or not the response
	// has a prose prefix: the looksLikeToolCall check below needs the
	// response to start with `{`, so a prose prefix would leave the
	// targeted corrective unfired. Checked FIRST: the entity bug is a
	// stronger signal than "this is narration", so it wins over
	// prose/non_json/malformed.
	htmlEntities := strings.Contains(stripped, "&lt;") ||
		strings.Contains(stripped, "&gt;") ||
		strings.Contains(stripped, "&amp;")
	embeddedToolCall := strings.Contains(stripped, `"type":"tool_call"`) ||
		strings.Contains(stripped, `"type": "tool_call"`)
	if htmlEntities && embeddedToolCall {
		return "html_entities", "Your tool call has HTML-entity-encoded angle brackets (`&lt;` / `&gt;` / `&amp;`) inside the JSON string args. JSON strings should contain literal `<` and `>` — don't HTML-escape them. The file content goes verbatim onto disk; entities like `&lt;!DOCTYPE&gt;` would write the literal text `&lt;!DOCTYPE&gt;` into the file, not `<!DOCTYPE>`. Re-emit with literal angle brackets. For HTML rewrites, structural_edit is also a good alternative — it takes `selector: \"<body>\"` and the content body, no old_str needed. Also: respond with ONLY the JSON object — no prose preamble."
	}
	// Truncated tool_call detection: response starts with the tool-call
	// preamble but doesn't have a properly closed args object. We look
	// for the opening shape and the absence of a clean trailing `}}` —
	// if both, treat it as truncation.
	looksLikeToolCall := strings.HasPrefix(stripped, `{"type":"tool_call"`) ||
		strings.HasPrefix(stripped, `{ "type": "tool_call"`) ||
		strings.HasPrefix(stripped, `{"type": "tool_call"`)
	if !looksLikeToolCall {
		// Could be prose narration (model thinking leaked into content)
		// or some other non-tool_call shape.
		feedback := "Your response was not valid JSON. Respond with ONLY a JSON object, no other text. Example: {\"type\":\"tool_call\",\"name\":\"write_file\",\"args\":{\"path\":\"file.py\",\"content\":\"code\"}}"
		if !strings.HasPrefix(stripped, "{") {
			return "prose", feedback
		}
		return "non_json", feedback
	}
	// Crude truncation heuristic — if the response doesn't end with
	// at least one closing brace it's almost certainly cut off
	// mid-args. (A complete tool_call ends `...}}`.)
	truncated := !strings.HasSuffix(stripped, "}}") &&
		!strings.HasSuffix(stripped, "}") &&
		!strings.HasSuffix(stripped, "]")
	if truncated {
		return "truncated_tool", truncatedCallFeedback(stripped)
	}
	return "malformed_tool", "Your tool_call JSON was malformed. Re-emit it as a single valid JSON object: {\"type\":\"tool_call\",\"name\":\"<tool>\",\"args\":{...}}. No prose, no markdown fences, no trailing commas."
}

// truncatedCallFeedback tells the model how to shrink a call the token cap cut.
func truncatedCallFeedback(stripped string) string {
	hasEditOrWrite := strings.Contains(stripped, `"edit_file"`) ||
		strings.Contains(stripped, `"write_file"`)
	if !hasEditOrWrite {
		return "Your tool call was truncated mid-args. Make a smaller call — keep `content`, `old_str`, and `new_str` short (under ~30 lines). Respond now with the corrected, smaller call."
	}
	// When truncation hits on a whole-file replacement, structural_edit is the
	// right tool: it takes a structural selector (function:NAME, <tag>) instead
	// of literal old_str, so the JSON envelope stays small.
	structuralHint := ""
	if strings.Contains(stripped, `&lt;`) || strings.Contains(stripped, `&gt;`) ||
		strings.Contains(stripped, `<body>`) || strings.Contains(stripped, `<head>`) ||
		strings.Contains(stripped, `def `) || strings.Contains(stripped, `class `) {
		structuralHint = " For whole-function or whole-element replacements, use `structural_edit` instead — it takes the selector of the node you are changing and drops `old_str` entirely, so it doesn't truncate."
	}
	return "Your last tool call was TRUNCATED — the response hit the token cap mid-args. The fix is to shrink old_str/new_str: edit ONE function or block per call, not the whole file. If you need to change multiple routes/functions, do them in separate edit_file calls (one per turn). Common offenders: pasting all of app.py into old_str, embedding 5+ @app.route handlers in a single replacement." + structuralHint + " Respond now with a smaller edit_file or a structural_edit call."
}

// parseFailureFeedback is classifyParseFailure grounded in the call that was
// cut: when the prefix names a write tool, the generic advice is replaced by
// what the complete tokens and the file on disk establish (cutCallDiagnostic).
// Nothing in the cut call is executed either way.
func parseFailureFeedback(ctx *AgentContext, raw, streamCut string) (string, string) {
	category, feedback := classifyParseFailure(raw, streamCut)
	if category == "old_str_cut" && ctx != nil && ctx.LastOldStrCut != nil && ctx.LastOldStrCut.Cut {
		// The watch knows the file and the line where old_str left it.
		return category, oldStrCutFeedback(ctx.LastOldStrCut)
	}
	if category != "loop_cut" && category != "truncated_tool" {
		return category, feedback
	}
	diag := cutCallDiagnostic(ctx, raw)
	if diag == "" {
		return category, feedback
	}
	if category == "loop_cut" {
		return category, "Your response was cut off because it had started repeating itself, so what arrived " +
			"was an unfinished tool call; sending the same call again repeats the same way. " + diag
	}
	return category, "Your tool call was cut off at the per-turn token cap. " + diag
}

func extractModelResponse(raw string) (ModelResponse, error) {
	raw = strings.TrimSpace(raw)

	// Try direct parse first. Capture the error so we can surface it
	// to the caller's log if every other path fails — without this,
	// real diagnostics ("invalid character '\\n' in string literal",
	// "unexpected end of JSON input") were silently swallowed and the
	// agent loop just got "could not parse JSON" with no clue why.
	var resp ModelResponse
	directErr := json.Unmarshal([]byte(raw), &resp)
	if directErr == nil {
		liftMissingArgs(&resp, raw)
		return resp, nil
	}

	start := strings.Index(raw, "{")
	if start < 0 {
		return resp, fmt.Errorf("no JSON object found in response")
	}

	end := balancedObjectEnd(raw, start)

	var balancedErr error
	if end > start {
		jsonStr := raw[start:end]
		balancedErr = json.Unmarshal([]byte(jsonStr), &resp)
		if balancedErr == nil {
			liftMissingArgs(&resp, jsonStr)
			return resp, nil
		}
	}

	// A tool call whose JSON was cut off (max_tokens, a stream cut) or is
	// otherwise malformed is NOT reconstructed. Recovery used to rebuild
	// write_file / edit_file / structural_edit args from whatever prefix
	// arrived and execute them: a write cut at `return [{"user": 1` landed on
	// disk as that fragment, and an invalid escape was "unescaped" by guesswork,
	// all reported as a clean success and never told to the model. The bytes
	// intended are unknowable from a prefix, so the call is refused through the
	// parse-failure path, which tells the model its call was cut or malformed
	// and that nothing was executed.

	// Surface the most informative error available. directErr fires
	// when the response had garbage outside the JSON envelope (prose
	// preamble) — usually less useful. balancedErr fires when the
	// brace-balanced substring still failed to Unmarshal — that's the
	// actual JSON-content bug, e.g. literal LF inside a string,
	// unescaped backslash, malformed escape sequence. Prefer it.
	if balancedErr != nil {
		return resp, fmt.Errorf("could not parse JSON from response: %w", balancedErr)
	}
	return resp, fmt.Errorf("could not parse JSON from response: %w", directErr)
}

// balancedObjectEnd finds the brace that closes the one at start by counting
// nesting, and returns the index just past it, or -1 when none closes it.
func balancedObjectEnd(raw string, start int) int {
	depth, inString, escaped := 0, false, false
	for i := start; i < len(raw); i++ {
		c := raw[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inString:
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
			// A brace inside a string does not count.
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// liftMissingArgs handles models that emit tool calls in shapes other than
// the prescribed {"type":"tool_call","name":"X","args":{...}} envelope.
//
// Common alternative shapes:
//   - OpenAI-style: {"type":"tool_call","name":"X","arguments":{...}}
//   - Anthropic-style: {"type":"tool_call","name":"X","parameters":{...}}
//   - Inlined: {"type":"tool_call","name":"X","path":"...","offset":0,...}
//   - Type-is-tool-name: {"type":"read_file","path":"..."} — model
//     put the tool name in the type field instead of using "tool_call".
//
// When `args` is missing on a tool_call, re-decode the raw JSON into a
// generic map and either pull `arguments`/`parameters` over to args, or
// lift every non-envelope top-level field into a synthetic args object.
// This is purely a recovery path; the system prompt still teaches the
// canonical shape.
func liftMissingArgs(resp *ModelResponse, raw string) {
	// If Type is a known tool name, treat it as a tool_call with
	// that tool. The model emitted {"type":"read_file","path":"..."}
	// instead of {"type":"tool_call","name":"read_file","args":{...}}.
	// Without this fix the agent loop's switch hits the `default` arm
	// and burns a turn telling the model "Unknown response type".
	if resp.Type != "tool_call" && resp.Type != "text" && resp.Type != "done" && getTool(resp.Type) != nil {
		resp.Name = resp.Type
		resp.Type = "tool_call"
	}

	if resp.Type != "tool_call" || resp.Name == "" {
		return
	}
	if len(resp.Args) > 0 && string(resp.Args) != "null" {
		return
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		return
	}

	// Prefer explicit alt-key wrappers when present.
	for _, key := range []string{"arguments", "parameters", "params", "input"} {
		if v, ok := top[key]; ok && len(v) > 0 && string(v) != "null" {
			resp.Args = v
			return
		}
	}

	// Otherwise lift every non-envelope key into a synthetic args object.
	envelope := map[string]struct{}{
		"type": {}, "name": {}, "content": {}, "summary": {}, "args": {},
	}
	lifted := make(map[string]json.RawMessage)
	for k, v := range top {
		if _, isEnvelope := envelope[k]; isEnvelope {
			continue
		}
		lifted[k] = v
	}
	if len(lifted) == 0 {
		return
	}
	if buf, err := json.Marshal(lifted); err == nil {
		resp.Args = buf
	}
}

// knownArgKeys is the set of JSON argument names a tool legitimately accepts,
// read off its InputSchema struct's json tags. Derived by reflection rather
// than hand-listed so it can never drift from the tool's real signature. nil
// when the tool is unknown or its schema is not a struct (fail open — the
// caller then makes no claim about the shape).
func knownArgKeys(toolName string) map[string]bool {
	td := getTool(toolName)
	if td == nil || td.InputSchema == nil {
		return nil
	}
	t := reflect.TypeOf(td.InputSchema)
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	keys := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		if name := strings.Split(tag, ",")[0]; name != "" {
			keys[name] = true
		}
	}
	return keys
}

// swallowedContentFeedback catches a tool_call that parsed cleanly into the
// WRONG shape because a file body terminated its JSON string early. The
// mechanism: a doubled quote in the file body (for example inside a <script>)
// closes the content string of the write_file JSON envelope, and
// json.Unmarshal reads the rest of the file as a sibling KEY. Decoding into
// the typed WriteFileInput silently discards that key, so a truncated file
// lands and is reported as a clean, complete write; the model sees success and
// never repairs it, and V3 can take the truncated bytes as its baseline.
//
// extractModelResponse cannot catch this: the envelope is valid JSON, so the
// direct parse succeeds and the parse-failure path (which only runs on a parse
// ERROR) never fires. The syntax gates cannot either: html.parser is lenient
// about an unclosed <script>, and embedded_script_check suppresses findings
// when <script>/</script> counts disagree, which a truncated file guarantees.
// Both fail open on exactly this shape.
//
// Detection is the presence of an args key outside the tool's real signature
// that carries the fingerprint of leaked file content: it is long, or holds
// code/markup punctuation a genuine argument name never would. Scoped to tools
// with a long free-text field, since those are the only ones this can strike.
func swallowedContentFeedback(toolName string, args json.RawMessage) (string, bool) {
	known := knownArgKeys(toolName)
	if known == nil {
		return "", false
	}
	// Only tools whose args include a long free-text body can be cut this way.
	if !known["content"] && !known["old_str"] && !known["new_str"] {
		return "", false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return "", false
	}
	for k := range m {
		if known[k] {
			continue
		}
		// A real argument name is short and identifier-shaped. Swallowed file
		// remainder is long or carries structural characters.
		if len(k) > 40 || strings.ContainsAny(k, "\n\r\t;{}()<>") {
			field := "content"
			if !known["content"] {
				field = "old_str/new_str"
			}
			feedback := fmt.Sprintf(
				"Your %s call was NOT performed — nothing was written to disk. The JSON is mis-formed: your %s string ended EARLY because the file body contains an unescaped double-quote (\"). Everything after that quote — the rest of your file — was dropped, so the file would have landed truncated (no closing tags, no trailing code).\n\n"+
					"Inside a JSON string, every \" that belongs in the file must be written as \\\". Re-send the call with the inner quotes escaped. For a large HTML/JS file this is error-prone by hand: prefer structural_edit (a selector plus the block, no long JSON string to escape), or write the file in smaller pieces. Do NOT resend the same bytes unchanged.",
				toolName, field)
			return feedback, true
		}
	}
	return "", false
}

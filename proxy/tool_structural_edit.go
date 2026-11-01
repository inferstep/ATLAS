// The structural_edit tool: a change to one named function, class or method.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// structural_edit — friendly-selector named-node replacement
// ---------------------------------------------------------------------------

func structuralEditTool() *ToolDef {
	return &ToolDef{
		Name:   "structural_edit",
		Effect: ToolEffectDirectMutation,
		Description: "REQUIRED tool for whole-function, whole-class, or whole-HTML-element rewrites in existing files. " +
			"ALWAYS prefer over edit_file when replacing a named node or changing more than ~10 lines — edit_file is the WRONG tool for those cases (it forces you to copy the entire existing block as old_str, wasting tokens and frequently truncating). " +
			"Selectors v1: python `function:NAME` or `class:NAME` (decorators included automatically); html `<tag>` (top-level element). " +
			"Selector must match exactly one node; failures return actionable errors. " +
			"Decision rule: existing file + named-node change (any size) ⇒ structural_edit. New file ⇒ write_file. ≤10 lines inside a function ⇒ edit_file.",
		InputSchema: StructuralEditInput{},
		ReadOnly:    false,
		Destructive: false,
		Execute:     executeStructuralEdit,
	}
}

// executeStructuralEdit runs one structural_edit call: it checks the call, has
// the service splice the node in, sends the result down the protected route
// when that applies, and writes what comes back.
func executeStructuralEdit(rawInput json.RawMessage, ctx *AgentContext) (res *ToolResult, execErr error) {
	var input StructuralEditInput
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return nil, errNoMutation(fmt.Errorf("invalid input: %w", err))
	}
	if refusal := structuralInputRefusal(input); refusal != nil {
		return refusal, nil
	}

	path := resolveAgentPath(ctx, input.Path)
	if !editViewIsCurrent(ctx, path) {
		return nil, errNoMutation(fmt.Errorf("file not read yet — use read_file first before structural_edit: %s", input.Path))
	}

	data, err := readRegularFile(path)
	if err != nil {
		return nil, errNoMutation(fmt.Errorf("cannot read %s: %w", input.Path, err))
	}
	source := string(data)
	if refusal := structuralContentRefusal(input, source); refusal != nil {
		return refusal, nil
	}

	if modifiedSinceSessionView(ctx, path, data) {
		return nil, errNoMutation(fmt.Errorf("file modified since last read — read it again before structural_edit: %s", input.Path))
	}

	// Sanitise replacement content the same way edit_file does — the
	// model occasionally fences fragments with ```python or ```html.
	if cleaned, sanitized := sanitizeFileContent(input.Path, input.Content); sanitized {
		log.Printf("[structural_edit] removed a whole-content fence wrapper from content of %s", input.Path)
		input.Content = cleaned
		defer func() { noteContentChange(res, wholeFileWrapperNote) }()
	}

	input.Content = withoutRepeatedDoctype(input)

	astResp, err := spliceStructuralEdit(ctx, input, source)
	if err != nil {
		return nil, err
	}
	if refusal := spliceRefusal(ctx, input, source, astResp); refusal != nil {
		return refusal, nil
	}
	finalContent, routed := routeStructuralEdit(ctx, input, path, source, astResp.NewContent)
	if routed != nil {
		return routed, nil
	}

	// The healthy->broken rule the other three edit tools share.
	// The splice service only shows it could re-parse its own
	// output, and a splice can still leave a file unparseable. A
	// file that already failed stays editable, which is what makes
	// repair-in-progress possible, and the check fails open when it
	// cannot run.
	observed, refusal := editSyntaxObservation(ctx, "structural_edit", input.Path, input.Path,
		source, finalContent, structuralSyntaxMessage(input, finalContent))
	if refusal != nil {
		return refusal, nil
	}
	if refusal := structuralGateRefusal(ctx, path, input.Path, source, finalContent); refusal != nil {
		return refusal, nil
	}
	logMandatoryChecks(ctx, "structural_edit", input.Path, "")

	// Atomic write — same pattern as edit_file/write_file.
	tmpPath := path + atomicTempSuffix
	if err := os.WriteFile(tmpPath, []byte(finalContent), 0644); err != nil {
		return nil, errFailedMutationSyntaxUnrun(fmt.Errorf("cannot write %s: %w", input.Path, err))
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return nil, errFailedMutationSyntaxUnrun(fmt.Errorf("cannot rename temp file: %w", err))
	}
	ctx.RecordFileRead(path, finalContent)
	ctx.RecordBodySeen(path)

	log.Printf("[structural_edit] %s %s selector=%q lang=%s old=%dB new=%dB",
		input.Path, input.Selector, input.Selector, astResp.Language, astResp.OldSize, len(finalContent))

	outBytes, _ := json.Marshal(structuralEditOutput(input, astResp, finalContent, observed))
	result := &ToolResult{Success: true, Data: outBytes,
		MutationStatus:   MutationApplied,
		ValidationKind:   ValidationKindSyntax,
		ValidationStatus: ValidationNotRun}
	overlayValidation(result, observed)
	return result, nil
}

// structuralInputRefusal refuses a call that names no file or no node.
func structuralInputRefusal(input StructuralEditInput) *ToolResult {
	if strings.TrimSpace(input.Path) == "" {
		return noMutation("structural_edit: path cannot be empty. Read the file first then structural_edit with the same path.")
	}
	if strings.TrimSpace(input.Selector) == "" {
		return noMutation("structural_edit: selector cannot be empty. Examples: function:dashboard, class:UserModel, <body>")
	}
	return nil
}

// structuralContentRefusal refuses a replacement that cannot be one node's body.
func structuralContentRefusal(input StructuralEditInput, source string) *ToolResult {
	// Empty-content guard. Replacing a node with nothing is a
	// deletion, not an edit: an omitted `content` field would splice
	// an empty string over the node and silently delete it. That
	// passes the syntax gate (the file still parses) and the no-op
	// guard (the content did change), so nothing else catches it.
	// Refuse it and steer: an edit needs a replacement body; an
	// intentional removal is delete_file's job.
	if strings.TrimSpace(input.Content) == "" {
		log.Printf("[structural_edit] rejected empty content for %s selector=%q — would delete the node", input.Path, input.Selector)
		return noMutation(fmt.Sprintf(
			"structural_edit: content is empty — that would DELETE `%s`, not fix it. "+
				"Provide the full replacement body of the node (e.g. the corrected function definition). "+
				"If you truly mean to remove code, use delete_file on the whole file instead.",
			input.Selector))
	}

	// Runaway-content guard. structural_edit replaces ONE node, so the
	// replacement should be roughly node-sized. A reasoning-heavy
	// model sometimes leaks its entire chain-of-thought into the
	// content field instead of emitting just the new body, and that
	// blob then lands on disk and, being huge, trips downstream size
	// heuristics. Reject when the
	// replacement is implausibly larger than the whole file: >4× the
	// file and over an 8KB floor (so legit edits that grow a small
	// file stay clear, and large-function edits in large files aren't
	// touched). Steer the model to emit only the replacement node.
	if len(input.Content) > 8000 && len(input.Content) > len(source)*4 {
		log.Printf("[structural_edit] rejected runaway content for %s selector=%q: %d chars vs %d-byte file",
			input.Path, input.Selector, len(input.Content), len(source))
		return noMutation(fmt.Sprintf(
			"structural_edit: replacement content is %d characters — far larger than the entire %d-byte file. "+
				"You only need to provide the new body of the single node `%s` (just the function/class/element itself), "+
				"not the whole file and not your reasoning. Re-emit structural_edit with content set to ONLY the replacement node.",
			len(input.Content), len(source), input.Selector))
	}
	return nil
}

// withoutRepeatedDoctype is the replacement, without a doctype the file keeps.
func withoutRepeatedDoctype(input StructuralEditInput) string {
	// HTML <html>-selector quirk. structural_edit replaces only the
	// <html>...</html> element, not the <!DOCTYPE> declaration that
	// conventionally precedes it. The model often emits a leading
	// <!DOCTYPE html> at the top of `content` when the selector is
	// <html>, which would leave a duplicated doctype on disk. Detect
	// that shape and strip the leading doctype line so on-disk
	// output matches intent.
	ext := strings.ToLower(filepath.Ext(input.Path))
	isHTML := ext == ".html" || ext == ".htm"
	if isHTML && strings.EqualFold(strings.TrimSpace(input.Selector), "<html>") {
		if stripped, ok := stripLeadingDoctype(input.Content); ok {
			log.Printf("[structural_edit] stripped leading <!DOCTYPE> from content of %s — selector <html> only replaces the html element, not the preceding doctype", input.Path)
			input.Content = stripped
		}
	}
	return input.Content
}

// structuralSplice is v3-service's answer to /internal/structural_edit.
type structuralSplice struct {
	Success    bool   `json:"success"`
	Error      string `json:"error,omitempty"`
	Language   string `json:"language,omitempty"`
	NewContent string `json:"new_content,omitempty"`
	ByteRange  []int  `json:"byte_range,omitempty"`
	OldSize    int    `json:"old_size,omitempty"`
	NewSize    int    `json:"new_size,omitempty"`
}

// spliceStructuralEdit has v3-service splice the replacement into the file.
func spliceStructuralEdit(ctx *AgentContext, input StructuralEditInput, source string) (structuralSplice, error) {
	// Call v3-service /internal/structural_edit. Stateless transform:
	// proxy reads + writes (preserving lens-score-before-write),
	// v3-service is the tree-sitter authority.
	reqBody, _ := json.Marshal(map[string]interface{}{
		"path":     input.Path, // for language detection + error messages
		"source":   source,
		"selector": input.Selector,
		"content":  input.Content,
	})
	v3URL := ctx.V3URL
	if v3URL == "" {
		return structuralSplice{}, errNoMutation(fmt.Errorf("structural_edit unavailable: V3 service URL not configured"))
	}
	req, err := http.NewRequestWithContext(ctx.Ctx, "POST", v3URL+"/internal/structural_edit", bytes.NewReader(reqBody))
	if err != nil {
		return structuralSplice{}, errNoMutation(fmt.Errorf("structural_edit: build request: %w", err))
	}
	req.Header.Set(headerContentType, contentTypeJSON)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return structuralSplice{}, errNoMutation(fmt.Errorf("structural_edit: v3-service unreachable: %w", err))
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(serviceAnswer(resp.Body))
	if err != nil {
		return structuralSplice{}, errNoMutation(fmt.Errorf("structural_edit: read v3 response: %w", err))
	}
	var astResp structuralSplice
	if err := json.Unmarshal(respBytes, &astResp); err != nil {
		return structuralSplice{}, errNoMutation(fmt.Errorf("structural_edit: parse v3 response: %w (body=%s)", err, truncateStr(string(respBytes), 200)))
	}
	return astResp, nil
}

// spliceRefusal refuses a splice that failed or would not help.
func spliceRefusal(ctx *AgentContext, input StructuralEditInput, source string, astResp structuralSplice) *ToolResult {
	if !astResp.Success {
		// The selector matched nothing, or the splice would not parse:
		// either way no replacement content came back to write.
		return noMutation(astResp.Error + moduleLevelContentNote(ctx, input.Path, input.Content))
	}

	// Shrinkage guard: catch a destructive stub, such as only
	// "<!DOCTYPE html>" emitted for a whole <html>-element
	// rewrite. astResp.OldSize is the original node's bytes;
	// astResp.NewSize is the replacement bytes. If the replacement
	// is suspiciously small for the original, reject the write and
	// tell the model to re-emit with the full body.
	if rejection := validateNotSuspiciouslyShrunk("structural_edit", input.Path, astResp.OldSize, astResp.NewSize); rejection != "" {
		log.Printf("[structural_edit] rejecting suspicious shrinkage: %s old=%dB new=%dB selector=%q",
			input.Path, astResp.OldSize, astResp.NewSize, input.Selector)
		return refusedNoCheck(rejection)
	}

	// No-op guard. A weak model frequently "fixes" a bug by
	// re-emitting the node's existing (broken) code verbatim.
	// Reporting success on a no-op tells the model the fix landed
	// when nothing changed; it then moves on to verification, fails,
	// and can't work out why. Fail loudly instead so the model
	// re-derives the edit.
	if astResp.NewContent == source {
		log.Printf("[structural_edit] no-op edit rejected for %s selector=%q — replacement identical to existing code", input.Path, input.Selector)
		return noMutation(fmt.Sprintf(
			"structural_edit: your replacement for `%s` is IDENTICAL to the code already in the file — nothing was changed and the bug is still there. "+
				"Look at the current code again and emit a replacement that actually differs (for a swapped-operator bug, the operator itself must change).",
			input.Selector))
	}
	return nil
}

// routeStructuralEdit sends the spliced file down the protected route when it
// warrants V3. It returns the content to write, or a result that ends the call.
//
// V3 routing. The tier is the higher of the pre-edit and post-edit
// content, so a destructive edit that shrinks a T2+ file into a
// stub still reaches V3: those are the edits that most need
// checking. structural_edit is already surgical (the model named
// the exact node, and the replacement is its own tree-sitter
// transform), so running PlanSearch on every one-line edit is
// mostly cost: minutes per edit, which looks like a hang. V3 runs
// only when editGenerationBypass allows it: a T2+ file complex
// enough for editWarrantsV3, outside an active debug loop.
//
// The baseline candidate is the structurally edited full file; a V3
// alternative lands only through the protected route below,
// otherwise the structurally edited content passes through
// unchanged.
func routeStructuralEdit(ctx *AgentContext, input StructuralEditInput, path, source,
	finalContent string) (string, *ToolResult) {
	oldTier := classifyFileTier(input.Path, source) // pre-edit content
	newTier := classifyFileTier(input.Path, finalContent)
	fileTier := oldTier
	if newTier > fileTier {
		fileTier = newTier
	}
	cc, ccOK := cyclomaticComplexity(ctx, input.Path, finalContent)
	if ccOK {
		if refined := refineTierWithCC(fileTier, cc); refined != fileTier {
			log.Printf("[structural_edit] %s tier %s→%s via cc=%d", input.Path, fileTier, refined, cc)
			fileTier = refined
		}
	}
	// A session iterating on its own file after a failed run is in
	// the write-run-fix loop; V3's toll there only delays the next
	// execution, on the edit path as on the write path.
	structuralBypass := editGenerationBypass(ctx, fileTier,
		editWarrantsV3(finalContent, cc, ccOK),
		isActiveDebugIteration(ctx, input.Path))
	recordCandidateGenerationBypass(ctx, "structural_edit", structuralBypass,
		fileTier, strings.Count(finalContent, "\n")+1)
	logBudgetBypass("structural_edit", input.Path, structuralBypass)
	if structuralBypass != bypassNone {
		return finalContent, nil
	}
	log.Printf("[structural_edit] V3 pipeline activating for %s (oldTier=%d newTier=%d max=%d, req_tier=%d, cc=%d) post-structural-edit", input.Path, oldTier, newTier, fileTier, ctx.Tier, cc)
	// THE protected edit route, same owner as the other three edit
	// tools: a service proposal reaches disk only through the
	// evidence, authorization, one-time grant, exact-byte delivery,
	// ledger, validation and settlement chain, and otherwise the
	// caller's own structurally edited content is what stays.
	route := deliverEditCandidate(ctx, "structural_edit", path,
		input.Path, source, finalContent)
	if route.Cancelled != nil {
		return "", noMutation("structural_edit cancelled — no content was written")
	}
	if route.Delivered {
		// The candidate landed through the protected chain.
		// Writing the caller's own content now would undo a
		// delivery settlement and restoration already decided.
		return "", attachV3(route.Result, route.Meta)
	}
	return route.Content, nil
}

// structuralSyntaxMessage is structural_edit's refusal for a result that does
// not parse.
func structuralSyntaxMessage(input StructuralEditInput, finalContent string) func(string) string {
	return func(detail string) string {
		if msg, isEmbedded := embeddedScriptRejectionFor(detail); isEmbedded {
			return msg
		}
		return fmt.Sprintf(
			"structural_edit result for %s does not parse (%s). The file was NOT modified.%s\n"+
				"Check that the replacement for `%s` is complete and re-issue the edit.",
			input.Path, truncateStr(detail, 200), offendingLineNote(finalContent, detail), input.Selector)
	}
}

// structuralGateRefusal runs structural_edit's gates after the syntax gate.
func structuralGateRefusal(ctx *AgentContext, path, relPath, source, finalContent string) *ToolResult {
	// Structural gate: the structural splice guarantees the result
	// parses, but not that its calls resolve, so a new call with no
	// matching import would land as verified and fail at runtime.
	// Block a write that NEWLY makes a direct call unresolved
	// (healthy->broken); a pre-existing one is left alone for
	// repair-in-progress. Fail-open when the check can't run.
	if introduced := editIntroducesUnresolved(ctx, path, source, finalContent); len(introduced) > 0 {
		log.Printf("[structural_edit] edit introduces unresolved call(s) %v in %s — rejecting", logPaths(introduced), logPath(relPath))
		return structuralRefusal(structuralRejection(relPath, introduced))
	}

	// Embedded-script gate: the post-splice check in v3-service
	// proves the .py/.html parses, not that the JavaScript inside a
	// <script> block does — and `<script>` is a first-class selector
	// here, so this tool is the likeliest way to land broken JS.
	// Healthy->broken, fail-soft.
	if msg := embeddedScriptGate(ctx, path, source, finalContent); msg != "" {
		log.Printf("[structural_edit] edit breaks an embedded script in %s — rejecting", logPath(relPath))
		return structuralRefusal(msg)
	}
	if msg := duplicateMainGuard(path, source, finalContent); msg != "" {
		log.Printf("[structural_edit] edit duplicates the module entrypoint in %s — rejecting", logPath(relPath))
		return structuralRefusal(msg)
	}
	return nil
}

// structuralEditOutput is what structural_edit tells the model about the change.
func structuralEditOutput(input StructuralEditInput, astResp structuralSplice, finalContent string,
	observed checkOutcome) StructuralEditOutput {
	out := StructuralEditOutput{
		OK:       true,
		Selector: input.Selector,
		Language: astResp.Language,
		BytesOld: astResp.OldSize,
		BytesNew: len(finalContent),
	}
	// The splice landed, so run the syntax check on the bytes
	// written. The tree-sitter transform only proves v3-service
	// could re-parse ITS output, and those bytes may be a V3
	// replacement rather than the splice, so the service's ok
	// boolean is not a syntax pass.
	//
	// not_run is not a neutral answer at the exit: a path can only
	// retire its mutation debt on a verdict about its CURRENT bytes.
	//
	// The verdict is real, so a clean splice settles and a broken one
	// is recorded as broken instead of as unknown. It is the check the
	// healthy->broken rule already ran on exactly these bytes: a splice
	// that lands failing was a repair of a file that already failed.
	if observed.Status == ValidationFailed {
		log.Printf("[structural_edit] %s landed but does not parse (%s)",
			logPath(input.Path), safeDiagnosticSummary(observed.Detail))
		out.Warning = fmt.Sprintf(
			"the edit landed, but %s does not parse now (%s). Run it and "+
				"read the real traceback, then fix that line.",
			input.Path, truncateStr(observed.Detail, 160))
	}
	return out
}

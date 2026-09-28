// The agent's tool surface: what the model may call, what happens when it
// does, and how the model is told the tools exist at all.
//
// In file order:
//
//	Registry and dispatch — a name→ToolDef map populated in init(), and
//	  executeToolCall, the one entry point the agent loop uses.
//	Eleven tool definitions: read_file, outline_file, search_files,
//	  list_directory, write_file, edit_file, structural_edit, delete_file,
//	  move_file, find_file, run_command. Each is a constructor returning its
//	  ToolDef — schema, model-facing description, executor — so a tool's
//	  three faces are edited in one place. The V3 and sandbox calls a tool
//	  makes (candidate generation for a T2 write, tree-sitter outline,
//	  the run client) sit with the tool that makes them rather than
//	  in a shared client block.
//	Tier classification — whether a given write is boilerplate the proxy
//	  writes straight to disk (T0/T1) or logic worth routing through the V3
//	  pipeline (T2/T3), refined by cyclomatic complexity from v3-service.
//	Helpers — path resolution across the three coordinate systems the model,
//	  host and sandbox each use, the redundant-read short circuit, and the
//	  V3-stage → SSE-event name map.
//	Background commands — run_background, tail_background, stop_background:
//	  the remaining three of the fourteen, kept as a set with their sandbox
//	  /jobs client because none of them is usable alone.
//	Output constraint — the JSON Schema and the GBNF grammar that hold the
//	  model to one tool-call shape, both generated from the registry.
//	Prompt rendering — the same registry turned into the "## Available
//	  Tools" section of the system prompt, with a variant that omits named
//	  tools for the per-decision nudge.
//
// One file because there is one source of truth. Add a tool to the registry
// and its schema, its grammar alternative and its prompt entry all follow
// from the same ToolDef. Splitting the definitions away from the generators
// is exactly what would let the model's instructions drift from what the
// grammar permits and what the executor actually does.

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// Tool registry
// ---------------------------------------------------------------------------

var toolRegistry = map[string]*ToolDef{}

func init() {
	registerTool(readFileTool())
	registerTool(outlineFileTool())
	registerTool(writeFileTool())
	registerTool(editFileTool())
	registerTool(structuralEditTool())
	registerTool(insertAfterTool())
	registerTool(replaceLinesTool())
	registerTool(deleteFileTool())
	registerTool(moveFileTool())
	registerTool(runCommandTool())
	registerTool(searchFilesTool())
	registerTool(findFileTool())
	registerTool(listDirectoryTool())
	registerTool(runBackgroundTool())
	registerTool(tailBackgroundTool())
	registerTool(stopBackgroundTool())
}

func registerTool(t *ToolDef) {
	toolRegistry[t.Name] = t
}

func getTool(name string) *ToolDef {
	return toolRegistry[name]
}

// allTools returns every registered tool in one stable order, sorted by name.
//
// This is the ordering authority for everything the model sees: the sequence of
// the "### <tool>" documentation blocks in the system prompt, the tool-name enum
// in response_format, and the alternation in the GBNF tool-name production all
// come from here. Ranging toolRegistry directly gave each of them a fresh Go map
// order on every request, so two identical requests produced different bytes --
// which reprocesses the cached prompt prefix on the server and varies the tool
// ordering the model sees, for no reason anyone chose. Name order is arbitrary
// but fixed, which is the property that matters.
//
// The registry stays a map: lookup by name is what getTool needs, and the order
// belongs to the readers, not to the storage.
func allTools() []*ToolDef {
	tools := make([]*ToolDef, 0, len(toolRegistry))
	for _, t := range toolRegistry {
		tools = append(tools, t)
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools
}

// executeTool dispatches a tool call to its executor.
// executeToolCall dispatches a tool and then classifies the result at the
// shared boundary, for the tools whose classification FOLLOWS FROM THEIR
// EFFECT CLASS. Direct mutators are deliberately excluded: only the branch
// that performed the mutation knows whether bytes landed, so filling that in
// here would conceal a missed local producer.
//
// Nothing here reads Success. Classification is never inferred from it.
func executeToolCall(name string, args json.RawMessage, ctx *AgentContext) *ToolResult {
	tool := getTool(name)
	// What a shell command changes is observed around it (applyShellChanges):
	// the ledger otherwise sees only paths it already tracked. A background
	// job's later writes are measured from the walk after the call that
	// started it (noteBackgroundBaseline).
	var before workspaceSnapshot
	observeShell := (name == "run_command" || name == "run_background" || name == "stop_background") &&
		ctx != nil && ctx.WorkingDir != ""
	if observeShell {
		before = snapshotWorkspace(ctx.WorkingDir)
	}
	result := executeToolCallInner(name, args, ctx)
	if result == nil || tool == nil {
		return result
	}
	// Only fill what the effect class alone can prove, and only where a local
	// producer has not already spoken.
	if tool.Effect.BoundaryClassifiable() && result.MutationStatus == MutationUnknown {
		switch tool.Effect {
		case ToolEffectReadOnly:
			// Cannot mutate by construction, on success or failure alike.
			result.MutationStatus = MutationNone
		case ToolEffectCommandUnobserved:
			// Conservative: once dispatch reached the handler, a subprocess or
			// background job may have started and written before failing,
			// timing out, or being killed. Only a branch that proves no
			// execution began may claim MutationNone, and it does so locally.
			result.MutationStatus = MutationUnobserved
		}
	}
	if result.ValidationKind == ValidationKindUnknown && tool.Effect.BoundaryClassifiable() {
		// Neither class performs content validation. Saying so explicitly is
		// different from leaving it unknown.
		result.ValidationKind = ValidationKindNone
		result.ValidationStatus = ValidationNotApplicable
	}
	// Observational: records what the call did to the session's deliverables.
	// Runs after classification so it sees the final evidence, and returns the
	// same result either way — nothing downstream can observe that it ran.
	recordLedgerEffect(name, args, ctx, result)
	if observeShell && result != nil && result.MutationStatus != MutationNone &&
		result.MutationStatus != MutationRefused {
		after := snapshotWorkspace(ctx.WorkingDir)
		applyShellChanges(ctx, before, after, name)
		if name == "run_background" {
			noteBackgroundBaseline(ctx, after)
		}
	}
	if name == "stop_background" {
		// A confirmed exit may have lowered the last hazard.
		settleBackgroundEffects(ctx)
	}
	return result
}

func executeToolCallInner(name string, args json.RawMessage, ctx *AgentContext) *ToolResult {
	tool := getTool(name)
	if tool == nil {
		return &ToolResult{
			Success: false,
			Error:   fmt.Sprintf("unknown tool: %s", name),
			// Refused before dispatch: no handler ran, so nothing could have
			// started or mutated. This is provable, not conservative.
			MutationStatus: MutationNone,
			ValidationKind: ValidationKindNone, ValidationStatus: ValidationNotApplicable,
		}
	}

	// Distinguish "no args field at all" from "malformed args".
	// The model occasionally emits {"type":"tool_call","name":"read_file"}
	// with no "args" key, which lands here as nil/empty bytes. Calling
	// json.Unmarshal on that returns "unexpected end of JSON input" — the
	// same string a *truncated* response produces — and the old remap
	// branch below would then tell the model "your output was truncated,
	// use smaller edit_file calls" which is not just unhelpful, it
	// actively steers the model away from the read_file/list_directory
	// it was trying to make. Catch the empty case here and return a
	// per-tool hint that tells the model exactly what shape to send.
	trimmed := strings.TrimSpace(string(args))
	if trimmed == "" || trimmed == "null" {
		return &ToolResult{
			Success: false,
			Error:   missingArgsHint(name),
			// Pre-dispatch: no handler ran.
			MutationStatus: MutationNone,
			ValidationKind: ValidationKindNone, ValidationStatus: ValidationNotApplicable,
		}
	}
	if reason := validateToolWorkspacePaths(name, args, ctx); reason != "" {
		return &ToolResult{Success: false, Error: reason,
			MutationStatus: MutationNone,
			ValidationKind: ValidationKindNone, ValidationStatus: ValidationNotApplicable}
	}
	// Safety deny-list — sensitive targets (.env, *.pem, *credentials*,
	// destructive shell patterns) are refused in every permission mode.
	if denied, reason := shouldDenyToolCall(name, args); denied {
		return &ToolResult{Success: false, Error: fmt.Sprintf("%s refused: %s", name, reason),
			MutationStatus: MutationNone,
			ValidationKind: ValidationKindNone, ValidationStatus: ValidationNotApplicable}
	}
	// The user forbade changing anything. Enforced HERE, at the one dispatch
	// every tool passes through, and keyed on the tool's DECLARED effect --
	// not on a tool-name list (which has gone stale repeatedly) and not on the
	// system prompt, which a model can simply not follow. That makes the
	// guarantee hold for arbitrary shell and background jobs
	// (ToolEffectCommandUnobserved: `sed -i`, `rm`, a `>` redirect) exactly as
	// it does for write_file. Measured on the 38eaa0a benchmark: 6 sessions
	// edited a fixture after the user wrote "do not change any code".
	if reason := readOnlyRequestRefusal(tool, ctx); reason != "" {
		log.Printf("[tools] %s refused: the request forbids changing the workspace", name)
		return &ToolResult{Success: false, Error: reason,
			MutationStatus: MutationNone,
			ValidationKind: ValidationKindNone, ValidationStatus: ValidationNotApplicable}
	}

	result, err := tool.Execute(args, ctx)
	if err != nil {
		errMsg := err.Error()
		// Only treat "unexpected end of JSON" as truncation when the
		// args payload is large enough that truncation is plausible.
		// Short payloads with that error are malformed JSON, not
		// truncated output, and the model needs the real parser error
		// to correct itself.
		if len(args) > 200 && strings.Contains(errMsg, "unexpected end of JSON") {
			errMsg = "Tool call was truncated (output too long for context window). Use smaller, targeted edit_file calls instead of full write_file rewrites."
		}
		failed := &ToolResult{
			Success: false,
			Error:   errMsg,
		}
		// A producer that returned a classified error owns those facts, so
		// copy them onto the synthesised result. Untyped errors stay Unknown:
		// nothing is inferred from Success or from parsing error text, and a
		// read-only tool's generic failure must never become MutationFailed.
		var ce *classifiedError
		if errors.As(err, &ce) {
			failed.MutationStatus = ce.mutationStatus
			failed.ValidationKind = ce.validationKind
			failed.ValidationStatus = ce.validationStatus
		}
		return failed
	}
	return result
}

// missingArgsHint returns a tool-specific message instructing the model
// what argument shape to send when it omits the args field entirely.
func missingArgsHint(name string) string {
	switch name {
	case "read_file":
		return `read_file: no arguments provided. Call with {"path":"<file>"}. Use list_directory {"path":"."} first if you need to discover what files exist.`
	case "write_file":
		return `write_file: no arguments provided. Call with {"path":"<file>","content":"<full file contents>"}.`
	case "edit_file":
		return `edit_file: no arguments provided. Call with {"path":"<file>","old_str":"<exact text to replace>","new_str":"<replacement>"}.`
	case "delete_file":
		return `delete_file: no arguments provided. Call with {"path":"<file>"}.`
	case "move_file":
		return `move_file: no arguments provided. Call with {"source":"<current path>","destination":"<new path or dir>"}.`
	case "list_directory":
		return `list_directory: no arguments provided. Call with {"path":"."} for the working directory or {"path":"<subdir>"}.`
	case "search_files":
		return `search_files: no arguments provided. Call with {"pattern":"<regex>"} and optionally {"path":"<dir>","include":"*.py"}.`
	case "find_file":
		return `find_file: no arguments provided. Call with {"pattern":"<name regex>"} (e.g. {"pattern":"snake_game\\.py"}).`
	case "run_command":
		return `run_command: no arguments provided. Call with {"command":"<shell command>"}.`
	default:
		return fmt.Sprintf("%s: no arguments provided. Inspect the tool schema and resend with the required fields.", name)
	}
}

// ---------------------------------------------------------------------------
// read_file
// ---------------------------------------------------------------------------

func readFileTool() *ToolDef {
	return &ToolDef{
		Name:        "read_file",
		Effect:      ToolEffectReadOnly,
		Description: "Read the contents of a file. Returns numbered lines. Use offset and limit for large files.",
		InputSchema: ReadFileInput{},
		ReadOnly:    true,
		Destructive: false,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (*ToolResult, error) {
			var input ReadFileInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}

			// Empty path → resolves to the working dir, which is a
			// directory, which fails with a confusing error the model
			// can't recover from. Reject early with a hint at how to
			// discover the file.
			if strings.TrimSpace(input.Path) == "" {
				return &ToolResult{
					Success: false,
					Error:   "read_file: path cannot be empty. Call list_directory with path \".\" to see what files exist, or find_file with a name regex (e.g. \"snake_game\\.py\").",
				}, nil
			}

			path := resolveAgentPath(ctx, input.Path)

			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("cannot read %s: %w", input.Path, err)
			}

			// Binary-file guard: reading a compiled binary / image /
			// archive as text returns garbage the model can't use, after
			// which it gives up or loops (observed 2026-07-19,
			// extract-elf: read the ELF as text, never reached for the
			// analysis tools that were installed). Point it at the right
			// tools instead of handing back the raw bytes.
			if isBinaryContent(data) {
				return &ToolResult{
					Success: false,
					Error: fmt.Sprintf("read_file: %q is a binary file, not text — reading it as text is not useful. Inspect it with run_command instead: `strings %q` (printable strings), `readelf -a %q` or `objdump -d %q` (ELF headers / disassembly), `nm %q` (symbols), `file %q` (type), or `xxd %q | head` (hex dump).",
						input.Path, input.Path, input.Path, input.Path, input.Path, input.Path, input.Path),
				}, nil
			}

			lines := strings.Split(string(data), "\n")
			totalLines := len(lines)

			start := 0
			if input.Offset != nil {
				start = *input.Offset
				if start < 0 {
					start = 0
				}
				if start > totalLines {
					start = totalLines
				}
			}

			end := totalLines
			if input.Limit != nil {
				end = start + *input.Limit
				if end > totalLines {
					end = totalLines
				}
			}

			// Build numbered output (matches Claude Code's cat -n format).
			//
			// The "N<tab>" prefix is presentation, and nothing in the payload
			// said so, so a model reasonably concluded the file itself is
			// tab-delimited. Observed live: an otherwise correct solution to a
			// grid puzzle parsed every line as `line.split('\t')[1]`, found no
			// tabs in the real file, built an empty grid and printed 0. Say it
			// once, up front, for any read a program might be written against.
			var sb strings.Builder
			sb.WriteString("(Line numbers and the tab after them are added by " +
				"read_file for reference. They are NOT in the file — code that " +
				"reads this file must not parse them.)\n")
			for i := start; i < end; i++ {
				fmt.Fprintf(&sb, "%d\t%s\n", i+1, lines[i])
			}

			content := sb.String()
			// Cap the returned BYTES so one huge read can't blow the
			// model's context window. A model that gunzips a data file
			// and read_files it (observed 2026-07-19, gcode-to-text: a
			// decompressed G-code file read with limit:100000 -> 2.26M tokens
			// -> hard 400 exceed_context_size, which the force-trim retry
			// can't fix because the single message alone overflows). The cap
			// is UNCONDITIONAL: a line `limit` does not bound bytes (100k
			// lines of G-code is enormous), so capping only unbounded reads
			// left the hole open. Truncate at a line boundary and tell the
			// model to narrow the range or process the file with a command.
			truncated := false
			shownEnd := end // 1-past the last line actually returned
			if len(content) > maxReadFileBytes {
				cut := maxReadFileBytes
				if nl := strings.LastIndexByte(content[:cut], '\n'); nl > 0 {
					cut = nl + 1
				}
				// content opens with the one-line "line numbers are added"
				// header, so its newline count is one MORE than the number of
				// numbered rows actually returned. Counting the header made
				// the window label overshoot by a line and made shownEnd
				// record a line the model never saw.
				shown := strings.Count(content[:cut], "\n") - 1
				if shown < 0 {
					shown = 0
				}
				content = content[:cut] + readFileTruncationNotice(
					start, shown, totalLines, len(data), fileIsSourceCode(input.Path))
				truncated = true
				shownEnd = start + shown
				if shownEnd > totalLines {
					shownEnd = totalLines
				}
			}
			// #147 review finding #10: on a truncated read the model only
			// saw the head, so record ONLY that — otherwise the redundant-read
			// dedup later asserts the whole file is in context and short-
			// circuits a real re-read. Untruncated reads record the full bytes.
			recorded := string(data)
			if truncated {
				recorded = strings.Join(lines[start:shownEnd], "\n")
			}
			ctx.RecordFileRead(path, recorded)
			ctx.RecordBodyRead(path, start+1, shownEnd, totalLines)

			// Call-graph footer (issue #39). The model reads a
			// file far more often than it outlines one, so attach the
			// intra-file call edges to a .py read where the localization
			// decision happens. Fire on any read that starts at the top of
			// the file (start == 0) — that covers both a whole-file read and
			// the model's common "offset:0, limit:N" first look; it computes
			// the graph from the full file on disk regardless of the page
			// shown, and skips mid-file pages so a model scrolling a big file
			// doesn't get the footer repeated.
			if start == 0 && strings.HasSuffix(input.Path, ".py") {
				if footer := callGraphFooter(ctx, input.Path, string(data)); footer != "" {
					content += footer
				}
			}

			out := ReadFileOutput{
				Content:    content,
				TotalLines: totalLines,
				StartLine:  start + 1,
				EndLine:    shownEnd, // #147 review #15: actual last line returned, not the pre-truncation end
			}
			outBytes, _ := json.Marshal(out)
			return &ToolResult{Success: true, Data: outBytes}, nil
		},
	}
}

// ---------------------------------------------------------------------------
// outline_file — cheap structural index of a file (names + line ranges, no
// bodies). The surgical-read entry point: outline to see what's in a file
// for a few hundred bytes, then read_file with offset/limit to pull just
// the one function you need. Saves the model from dumping a whole file into
// context (and re-reading it) just to find one bug. GH #39.
// ---------------------------------------------------------------------------

func outlineFileTool() *ToolDef {
	return &ToolDef{
		Name:   "outline_file",
		Effect: ToolEffectReadOnly,
		Description: "List a file's top-level functions and classes with their " +
			"line ranges. Returns NO code: an outline tells you WHERE something is " +
			"defined and never what it does, so you cannot answer a question, " +
			"diagnose a bug, or judge whether code is correct from one. " +
			"Use it to locate a target inside a file too large to read whole: " +
			"outline_file to find the function, then read_file with offset/limit " +
			"for its lines, then structural_edit (selector function:NAME / " +
			"class:NAME) or edit_file to change it. For a file you can simply " +
			"read, go straight to read_file. " +
			"Python is parsed precisely (tree-sitter, decorator-aware); other " +
			"languages get a best-effort definition scan.",
		InputSchema: OutlineInput{},
		ReadOnly:    true,
		Destructive: false,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (*ToolResult, error) {
			var input OutlineInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}
			if strings.TrimSpace(input.Path) == "" {
				return &ToolResult{Success: false,
					Error: "outline_file: path cannot be empty. Pass {\"path\":\"<file>\"}."}, nil
			}
			path := resolveAgentPath(ctx, input.Path)
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("cannot read %s: %w", input.Path, err)
			}
			src := string(data)
			totalLines := strings.Count(src, "\n") + 1

			// Prefer the v3 tree-sitter outline for .py (accurate, matches
			// structural_edit selectors). Fall back to a language-agnostic regex
			// scan for everything else and whenever v3 is unavailable.
			var syms []OutlineSymbol
			var regions []EmbeddedRegion
			if strings.HasSuffix(input.Path, ".py") {
				if v3, ok, emb := outlineViaV3(ctx, input.Path, src); ok {
					syms, regions = v3, emb
				}
			}
			engine := "tree-sitter"
			if syms == nil {
				syms = outlineByRegex(input.Path, src)
				engine = "scan"
			}

			ctx.RecordFileRead(path, src)

			var sb strings.Builder
			fmt.Fprintf(&sb, "%s — %d lines, %d symbols (%s)\n",
				input.Path, totalLines, len(syms), engine)
			if len(syms) == 0 {
				sb.WriteString("(no top-level functions/classes found — read_file to view it directly)\n")
			}
			hasGraph := false
			for _, s := range syms {
				fmt.Fprintf(&sb, "L%d-%d\t%s %s\n", s.StartLine, s.EndLine, s.Kind, s.Name)
				if len(s.Calls) > 0 {
					fmt.Fprintf(&sb, "\tcalls: %s\n", strings.Join(s.Calls, ", "))
					hasGraph = true
				}
				if len(s.CalledBy) > 0 {
					fmt.Fprintf(&sb, "\tcalled by: %s\n", strings.Join(s.CalledBy, ", "))
					hasGraph = true
				}
			}
			if hasGraph {
				// Steer the model to use the structure for localization — the
				// #39 point: a wrong value a function returns may come from a
				// function it calls, not from the function itself.
				sb.WriteString("\nNote: if a function returns a wrong value, the bug may be in a function it `calls`, not in the function itself — follow the call edges to the root cause before editing.\n")
			}
			sb.WriteString(embeddedRegionNote(regions))
			out := OutlineOutput{Symbols: syms, Supported: len(syms) > 0,
				EmbeddedRegions: regions, Outline: sb.String()}
			outBytes, _ := json.Marshal(out)
			return &ToolResult{Success: true, Data: outBytes}, nil
		},
	}
}

// callGraphFooter renders a compact intra-file call-graph summary for a
// whole-file read, reusing the same v3 outline (which carries calls/called_by).
// Returns "" when there are no edges, so a file with no internal calls doesn't
// get a noisy empty section.
func callGraphFooter(ctx *AgentContext, path, source string) string {
	syms, ok, _ := outlineViaV3(ctx, path, source)
	if !ok {
		return ""
	}
	var sb strings.Builder
	any := false
	for _, s := range syms {
		if len(s.Calls) == 0 && len(s.CalledBy) == 0 {
			continue
		}
		if !any {
			// The footer is concatenated onto the file content the model
			// reads, so without an explicit boundary it looks like the last
			// lines of the file. Observed live: a model anchored edit_file's
			// old_str on "## Call graph (within this file)\n- mean calls: ..."
			// and the edit could never match, because that text is not on
			// disk. Say where the file ends and that what follows is not it.
			fmt.Fprintf(&sb, "\n\n--- end of %s ---\n", path)
			sb.WriteString("The lines below are ATLAS analysis, NOT part of the file. " +
				"Never copy them into old_str.\n")
			sb.WriteString("## Call graph (within this file)\n")
			any = true
		}
		sb.WriteString("- " + s.Name)
		if len(s.Calls) > 0 {
			sb.WriteString(" calls: " + strings.Join(s.Calls, ", "))
		}
		if len(s.CalledBy) > 0 {
			sb.WriteString("; called by: " + strings.Join(s.CalledBy, ", "))
		}
		sb.WriteString("\n")
	}
	if any {
		sb.WriteString("If a function returns a wrong value, the bug may be in a function it calls — follow the edges to the root cause before editing.\n")
	}
	return sb.String()
}

// outlineViaV3 asks v3-service for a tree-sitter outline. Returns (nil,false)
// on any failure so the caller can fall back to the regex scan.
func outlineViaV3(ctx *AgentContext, path, source string) ([]OutlineSymbol, bool, []EmbeddedRegion) {
	if ctx.V3URL == "" {
		return nil, false, nil
	}
	body, _ := json.Marshal(map[string]string{"path": path, "source": source})
	req, err := http.NewRequestWithContext(ctx.Ctx, "POST",
		ctx.V3URL+"/internal/outline", bytes.NewReader(body))
	if err != nil {
		return nil, false, nil
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, false, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, false, nil
	}
	var out OutlineOutput
	if json.NewDecoder(resp.Body).Decode(&out) != nil || !out.Supported {
		return nil, false, nil
	}
	return out.Symbols, true, out.EmbeddedRegions
}

// embeddedRegionNote renders the foreign-language regions of a file for the
// outline the model reads.
//
// It names the symbols AND says they are unreachable by selector, because
// naming them alone would invite exactly the call it is meant to prevent.
func embeddedRegionNote(regions []EmbeddedRegion) string {
	if len(regions) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\nEmbedded code (a different language, inside this file):\n")
	for _, r := range regions {
		fmt.Fprintf(&sb, "L%d-%d\t%s in %s\n", r.StartLine, r.EndLine, r.Kind, r.Where)
		if len(r.Symbols) > 0 {
			fmt.Fprintf(&sb, "\tdefines: %s\n", strings.Join(r.Symbols, ", "))
		}
	}
	sb.WriteString("These are NOT selectable. To the host grammar the whole block is one " +
		"string literal or one raw text node, so `structural_edit` cannot address anything " +
		"listed above — a selector naming any of them reports that the symbol does not exist. Change " +
		"this code with replace_lines (the line numbers above), edit_file on one unique line, " +
		"or insert_after.\n")
	return sb.String()
}

// outlineByRegex is the language-agnostic fallback: any line starting (at
// column 0, allowing common keywords) with a definition. Good enough to give
// the model line anchors to read from; not as precise as tree-sitter.
func outlineByRegex(path, source string) []OutlineSymbol {
	type pat struct {
		re   *regexp.Regexp
		kind string
	}
	// Ordered, language-agnostic-ish. Name is capture group 1.
	pats := []pat{
		{regexp.MustCompile(`^(?:async\s+)?def\s+([A-Za-z_]\w*)`), "function"},
		{regexp.MustCompile(`^class\s+([A-Za-z_]\w*)`), "class"},
		{regexp.MustCompile(`^func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)`), "function"},
		{regexp.MustCompile(`^type\s+([A-Za-z_]\w*)`), "type"},
		{regexp.MustCompile(`^(?:export\s+)?(?:async\s+)?function\s+([A-Za-z_$][\w$]*)`), "function"},
		{regexp.MustCompile(`^(?:export\s+)?class\s+([A-Za-z_$][\w$]*)`), "class"},
	}
	lines := strings.Split(source, "\n")
	var out []OutlineSymbol
	for i, ln := range lines {
		for _, p := range pats {
			if m := p.re.FindStringSubmatch(ln); m != nil {
				out = append(out, OutlineSymbol{
					Name: m[1], Kind: p.kind,
					StartLine: i + 1, EndLine: i + 1, // single-line anchor; body follows
				})
				break
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// search_files
// ---------------------------------------------------------------------------

func searchFilesTool() *ToolDef {
	return &ToolDef{
		Name:        "search_files",
		Effect:      ToolEffectReadOnly,
		Description: "Search for a regex pattern inside file CONTENTS. Returns matching lines with file paths and line numbers. Use glob to filter by filename pattern. To find a file by its name (not contents), use find_file or list_directory instead.",
		InputSchema: SearchFilesInput{},
		ReadOnly:    true,
		Destructive: false,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (*ToolResult, error) {
			var input SearchFilesInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}

			// Reject empty pattern: same reasoning as find_file. An empty
			// regex matches every line in every file.
			if strings.TrimSpace(input.Pattern) == "" {
				return &ToolResult{
					Success: false,
					Error:   "search_files: pattern cannot be empty. Provide a regex to grep file contents for (e.g. \"def main\" or \"TODO\\(.*\\)\").",
				}, nil
			}

			searchPath := ctx.WorkingDir
			if input.Path != "" {
				searchPath = resolveAgentPath(ctx, input.Path)
			}

			re, err := regexp.Compile(input.Pattern)
			if err != nil {
				return nil, fmt.Errorf("invalid regex: %w", err)
			}

			var matches []SearchMatch
			maxMatches := 200
			skippedCredentials := 0

			err = filepath.WalkDir(searchPath, func(path string, d fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return nil // skip unreadable dirs
				}
				if d.IsDir() {
					base := d.Name()
					if base == ".git" || base == "node_modules" || base == "__pycache__" || base == ".next" || base == "target" {
						return filepath.SkipDir
					}
					return nil
				}
				// A symlink can point outside the workspace. read_file refuses
				// to follow one, and so does search.
				if d.Type()&fs.ModeSymlink != 0 {
					return nil
				}

				// Apply glob filter
				if input.Glob != "" {
					matched, _ := filepath.Match(input.Glob, d.Name())
					if !matched {
						return nil
					}
				}

				// Skip binary/large files
				info, err := d.Info()
				if err != nil || info.Size() > 1<<20 { // 1MB max
					return nil
				}

				relPath, _ := filepath.Rel(ctx.WorkingDir, path)
				if relPath == "" {
					relPath = path
				}
				// Credential files stay out of model context whichever tool
				// asks. search_files read them while read_file refused.
				if denyReadPathReason(relPath) != "" {
					skippedCredentials++
					return nil
				}

				data, err := os.ReadFile(path)
				if err != nil {
					return nil
				}

				scanner := bufio.NewScanner(strings.NewReader(string(data)))
				lineNum := 0
				for scanner.Scan() {
					lineNum++
					line := scanner.Text()
					if re.MatchString(line) {
						matches = append(matches, SearchMatch{
							File:    relPath,
							Line:    lineNum,
							Content: truncateStr(line, 200),
						})
						if len(matches) >= maxMatches {
							break
						}
					}
				}

				if len(matches) >= maxMatches {
					return filepath.SkipAll
				}
				return nil
			})

			if err != nil && len(matches) == 0 {
				return nil, fmt.Errorf("search error: %w", err)
			}

			out := SearchFilesOutput{
				Matches:                matches,
				TotalCount:             len(matches),
				Truncated:              len(matches) >= maxMatches,
				SkippedCredentialFiles: skippedCredentials,
			}
			outBytes, _ := json.Marshal(out)
			return &ToolResult{Success: true, Data: outBytes}, nil
		},
	}
}

// ---------------------------------------------------------------------------
// list_directory
// ---------------------------------------------------------------------------

func listDirectoryTool() *ToolDef {
	return &ToolDef{
		Name:        "list_directory",
		Effect:      ToolEffectReadOnly,
		Description: "List the contents of a directory. Returns file names, types (file/dir/symlink), and sizes.",
		InputSchema: ListDirectoryInput{},
		ReadOnly:    true,
		Destructive: false,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (*ToolResult, error) {
			var input ListDirectoryInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}

			dirPath := resolveAgentPath(ctx, input.Path)

			entries, err := os.ReadDir(dirPath)
			if err != nil {
				return nil, fmt.Errorf("cannot list %s: %w", input.Path, err)
			}

			var dirEntries []DirEntry
			for _, e := range entries {
				entryType := "file"
				if e.IsDir() {
					entryType = "dir"
				} else if e.Type()&os.ModeSymlink != 0 {
					entryType = "symlink"
				}

				var size int64
				if info, err := e.Info(); err == nil {
					size = info.Size()
				}

				dirEntries = append(dirEntries, DirEntry{
					Name: e.Name(),
					Type: entryType,
					Size: size,
				})
			}

			out := ListDirectoryOutput{
				Entries: dirEntries,
				Path:    dirPath,
			}
			outBytes, _ := json.Marshal(out)
			return &ToolResult{Success: true, Data: outBytes}, nil
		},
	}
}

// ---------------------------------------------------------------------------
// write_file — T0/T1 direct, T2/T3 routes through V3 pipeline
// ---------------------------------------------------------------------------

func writeFileTool() *ToolDef {
	return &ToolDef{
		Name:   "write_file",
		Effect: ToolEffectDirectMutation,
		Description: "Create a NEW file from scratch. Creates parent directories if needed. " +
			"For any file longer than a few lines, set content to \"@fenced\" and provide " +
			"the file as a plain fenced code block when asked — code embedded in a JSON " +
			"string loses parens and newlines to escaping. " +
			"DO NOT use to overwrite existing files — for existing files use structural_edit (whole function/class/element rewrite) or edit_file (≤10-line surgical change). " +
			"If a write_file call is rejected because the path already exists, switch to structural_edit (whole-block rewrite) or edit_file (surgical change). DO NOT retry with edit_file simply because the file is large.",
		InputSchema: WriteFileInput{},
		ReadOnly:    false,
		Destructive: true,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (res *ToolResult, execErr error) {
			var input WriteFileInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, errNoMutation(fmt.Errorf("invalid input: %w", err))
			}

			// Reject empty path — same reasoning as read_file.
			if strings.TrimSpace(input.Path) == "" {
				return noMutation("write_file: path cannot be empty. Provide a relative path like \"snake_game.py\" or \"src/main.py\"."), nil
			}

			path := resolveAgentPath(ctx, input.Path)

			// Sanitise model output before anything else touches it.
			// Otherwise a markdown-fenced response with a prose preamble
			// ("Looking at the task..." / ```html / actual code / ```)
			// lands on disk verbatim and the file becomes unparseable.
			cleaned, sanitized := sanitizeFileContent(input.Path, input.Content)
			if sanitized {
				log.Printf("[write_file] removed a whole-file fence wrapper from %s (was %d chars, now %d)",
					input.Path, len(input.Content), len(cleaned))
				input.Content = cleaned
				defer func() { noteContentChange(res, wholeFileWrapperNote) }()
			}

			// Pattern-matching reflex. When the model creates a
			// NEW file in a non-empty directory of similar files (HTML
			// alongside HTML, route handler alongside route handlers),
			// nudge it to read a sibling first instead of generating
			// content from scratch. Only fires for genuinely-new files
			// to avoid breaking edits-via-write_file. Soft hint via
			// tool result, not a hard reject — the model can ignore it
			// if the content is clearly intentional.
			if hint := patternMatchHint(path, ctx.SnapshotFilesRead()); hint != "" {
				return refusedNoCheck(hint), nil
			}

			// Echoed write: the content is the file that is already there.
			// Reproducing data the session already has is never the task,
			// and it is where the worst measured failure starts — the model
			// retyping a 2000-line fixture from memory, degenerating into
			// repetition, and getting its stream cut mid-JSON.
			if existing, err := os.ReadFile(path); err == nil {
				if echoesExistingFile(string(existing), input.Content) {
					log.Printf("[write_file] refusing an echoed write of %s (%d bytes on disk, %d incoming)",
						logPath(input.Path), len(existing), len(input.Content))
					return refusedNoCheck(echoedWriteRejection(input.Path)), nil
				}
			}

			// Stub detection. Reject "<h1>X Page</h1>" / "TODO"
			// placeholder writes that pass syntactic gates but ship the
			// minimum content humanly possible. The model's lazy-completion
			// failure mode is to write 8-line stubs and call it done; this
			// gate forces it to either commit real content or acknowledge
			// the stub explicitly. New files only — edits to existing
			// files might legitimately shrink to a stub via refactor.
			if isNewWrite(path) {
				if reason := looksLikeStub(input.Path, input.Content); reason != "" {
					return refusedNoCheck(reason), nil
				}
			}

			// Per-file tier classification — determines V3 pipeline activation
			fileTier := classifyFileTier(input.Path, input.Content)
			// GH #39 point 2: real cyclomatic complexity from tree-sitter
			// can escalate the regex classifier's verdict. Never downgrades.
			if cc, ok := cyclomaticComplexity(ctx, input.Path, input.Content); ok {
				if refined := refineTierWithCC(fileTier, cc); refined != fileTier {
					log.Printf("[write_file] %s tier %s→%s via cc=%d", input.Path, fileTier, refined, cc)
					fileTier = refined
				} else {
					log.Printf("[write_file] %s cc=%d (tier %s unchanged)", input.Path, cc, fileTier)
				}
			}
			log.Printf("[write_file] %s → %s (%d lines)", input.Path, fileTier, strings.Count(input.Content, "\n")+1)

			// V3 pipeline fires on T2+ files when V3 service is available.
			// V3 takes the model's content as baseline candidate, generates diverse
			// alternatives via PlanSearch/DivSampling, build-verifies each, and
			// selects the best. This is the intelligence layer.
			//
			// EXCEPT during an active edit-test-fix loop: once the model has
			// written this file and just saw it fail a run, the next write is
			// a targeted fix, and V3's full pipeline (minutes per call, and on
			// a mid-debug file frequently "completes without result" and falls
			// back anyway) throttles iteration to a handful of cycles.
			// Observed 2026-07-19: a polyglot task got ~5 writes in 25 min,
			// ~4 min of V3 stall each, when it needed 10-15 fast cycles.
			// Fast-path (direct, still syntax-gated below) so the model can
			// iterate at run speed. V3 still owns the FIRST write of each
			// file (the baseline generation where it adds value).
			iterating := isActiveDebugIteration(ctx, input.Path)

			// Regression protection for existing code, independent of tier
			// and of whether V3 is configured. The healthy->broken rule
			// exists to protect WORKING code, but it used to live only
			// inside branches gated on `fileTier >= Tier2Medium &&
			// ctx.V3URL != ""` or on an active debug iteration. A small
			// file, or any file in a session without V3, reached neither and
			// had valid bytes replaced by invalid ones with no check at all.
			// Measured black-box through the agent loop: a T1 overwrite
			// destroyed good bytes whether or not V3 was configured, while
			// the T2+V3 cell refused the same rewrite.
			//
			// Scope is deliberately narrow, so the three intended policies
			// survive: a NEW file still lands with a warning (nothing on
			// disk to protect), an already-broken file still accepts a
			// repair attempt (the baseline demonstrably fails), and a
			// non-gated language is unaffected (the checker reports
			// not_applicable off syntaxGateLanguages).
			//
			// The observations are KEPT rather than collapsed to a boolean.
			// The active-debug branch below asks the same question of the same
			// bytes, and re-asking the checker there costs the fast path a
			// second sandbox round trip and loses the distinction between
			// passed, not_run and not_applicable that a route needs in order to
			// classify its own result. Each side is evaluated at most once per
			// dispatch; ValidationUnknown means "not evaluated here".
			proposalCheck := checkOutcome{Status: ValidationUnknown}
			baselineCheck := checkOutcome{Status: ValidationUnknown}
			if _, statErr := os.Stat(path); statErr == nil {
				proposalCheck = fallbackSyntaxOutcomeFor(ctx, input.Path, input.Content).aggregate()
				if proposalCheck.Status == ValidationFailed {
					synErr := proposalCheck.Detail
					if prior, priorOK := readOriginalForGate(path); priorOK {
						baselineCheck = fallbackSyntaxOutcomeFor(ctx, input.Path, prior).aggregate()
						if !baselineAllowsRepair(baselineCheck) {
							log.Printf("[write_file] %s: refusing to regress valid content to invalid (%s)",
								logPath(input.Path), safeDiagnosticSummary(synErr))
							// The check examined input.Content, which is exactly
							// the content that would have been written, and the
							// refusal happens before any byte reaches disk.
							return &ToolResult{
								Success:          false,
								Error:            fallbackSyntaxRejection(input.Path, input.Content, synErr),
								MutationStatus:   MutationRefused,
								ValidationKind:   ValidationKindSyntax,
								ValidationStatus: ValidationFailed,
								ValidationDetail: synErr,
							}, nil
						}
					}
				}
			}
			// Content that does not parse gets the error now, not after the
			// V3 timeout. V3 improves a working candidate; it does not exist
			// to guess what a malformed one meant, and the post-V3 fallback
			// rejects an unparseable baseline anyway — so spending the budget
			// first only delays the same answer. The fast-path above cannot
			// cover this: it keys off a SUCCESSFUL write of the file, and a
			// model failing the syntax gate never records one, so it paid the
			// full timeout on every attempt. Observed live: a degenerating
			// model emitted markdown bold inside code (`data = [1, 2, **3**]`)
			// four times, each costing 180s before it was told anything.
			// One owner for whether the producer is consulted, and for why
			// it was not. Identical to the condition it replaces; what is new
			// is that the skip says which threshold turned it away.
			// The typed predicate, named at the site that reaches the
			// producer. The owner below decides WHICH reason applies and in
			// what order; this is the dependency being visible where the
			// dispatch happens, so a reader looking at the call that reaches
			// generation can see that disabling generation reaches it.
			writeBypass := writeGenerationBypass(ctx, fileTier, iterating)
			recordCandidateGenerationBypass(ctx, "write_file", writeBypass,
				fileTier, strings.Count(input.Content, "\n")+1)
			logBudgetBypass("write_file", input.Path, writeBypass)
			if writeBypass == bypassNone {
				// The regression gate above evaluated these exact bytes
				// whenever the destination exists, so the evaluation here is
				// for the case it skipped: a file that does not exist yet,
				// which is this gate's main case.
				if proposalCheck.Status == ValidationUnknown {
					proposalCheck = fallbackSyntaxOutcomeFor(ctx, input.Path, input.Content).aggregate()
				}
				if proposalCheck.Status == ValidationFailed {
					synErr := proposalCheck.Detail
					// Feeding V3 broken content wastes its whole budget, so
					// V3 is skipped either way. What happens to the bytes
					// depends on what is at the path: clobbering an existing
					// file with garbage is still refused, but a NEW file
					// lands with a warning so the model can run it and read
					// the real traceback. See writeNewFileWithWarning.
					if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
						log.Printf("[write_file] new file %s does not parse — writing with a warning, skipping V3 (%s)",
							logPath(input.Path), safeDiagnosticSummary(synErr))
						recordCandidateGenerationBypass(ctx, "write_file",
							bypassProposalFailedSyntaxGuard, fileTier,
							strings.Count(input.Content, "\n")+1)
						return preflightWarnedWrite(path, input.Path, input.Content, proposalCheck, ctx)
					}
					// The strictness on existing files protects WORKING code.
					// When what is on disk is itself unparseable, there is
					// nothing to protect, and rejecting an imperfect fix
					// guarantees the broken version survives. Same
					// healthy->broken rule the fast-path below already uses.
					// Measured twice on the novel benchmark: a broken first
					// draft landed, the corrective write carried a new syntax
					// slip, the rejection fed the model a line it could not
					// act on, and it re-sent byte-identical content until the
					// repetition breaker ended the session with the ORIGINAL
					// broken file still on disk.
					if prior, priorOK := readOriginalForGate(path); priorOK {
						if baselineCheck.Status == ValidationUnknown {
							baselineCheck = fallbackSyntaxOutcomeFor(ctx, input.Path, prior).aggregate()
						}
						if baselineAllowsRepair(baselineCheck) {
							log.Printf("[write_file] %s already broken on disk — landing the repair attempt with a warning (%s)",
								logPath(input.Path), safeDiagnosticSummary(synErr))
							recordCandidateGenerationBypass(ctx, "write_file",
								bypassProposalFailedSyntaxGuard, fileTier,
								strings.Count(input.Content, "\n")+1)
							return preflightWarnedWrite(path, input.Path, input.Content, proposalCheck, ctx)
						}
					}
					log.Printf("[write_file] %s does not parse — rejecting before V3 (%s)",
						logPath(input.Path), safeDiagnosticSummary(synErr))
					recordCandidateGenerationBypass(ctx, "write_file",
						bypassProposalFailedSyntaxGuard, fileTier,
						strings.Count(input.Content, "\n")+1)
					// Refused before any byte reached disk, on exactly the
					// content that would have been written. An unreadable
					// baseline arrives here too: without a baseline there is
					// no healthy->broken comparison to make, and the
					// conservative answer is the refusal.
					return &ToolResult{
						Success:          false,
						Error:            fallbackSyntaxRejection(input.Path, input.Content, synErr),
						MutationStatus:   MutationRefused,
						ValidationKind:   ValidationKindSyntax,
						ValidationStatus: ValidationFailed,
						ValidationDetail: synErr,
					}, nil
				}

				// proposalCheck now describes input.Content: the BASELINE
				// handed to the pipeline, not the bytes it may come back with.
				// A winning candidate is different content and needs its own
				// observation, and the fallback write inside writeFileWithV3
				// re-checks the baseline itself. Threading this observation in
				// there is a separate change: it would start a partial
				// migration of that function's own state machine, so the
				// handoff is left explicitly pending rather than stored on the
				// context or in a package-level value.
				log.Printf("[write_file] V3 pipeline activating for %s", input.Path)
				res, err := writeFileWithV3(path, input.Content, ctx)
				if err == nil && res != nil && res.Success {
					ctx.SessionWrites[input.Path] = true
				}
				return res, err
			}
			if iterating {
				log.Printf("[write_file] active edit-test-fix loop on %s — fast-path direct write (V3 skipped)", input.Path)
				// The file already exists this session; read its on-disk
				// content once and run both gates against it with the
				// healthy->broken rule edit_file uses — block only a NEWLY
				// introduced defect, allow a repair-in-progress on
				// already-broken content. Without the healthy->broken guard
				// the syntax gate would hard-block content the strict checker
				// rejects both before AND after (a multi-doc YAML or JSONC
				// config being iterated), which is exactly why the T0/T1
				// direct path below carries no syntax gate.
				original, origOK := readOriginalForGate(path)
				// The proposal was already evaluated above whenever the
				// destination exists, which is the normal state of this route:
				// it fires on a file the session wrote and just watched fail.
				// Evaluate here only for the residual case where it was not --
				// the file has been deleted since that write.
				if proposalCheck.Status == ValidationUnknown {
					proposalCheck = fallbackSyntaxOutcomeFor(ctx, input.Path, input.Content).aggregate()
				}
				if proposalCheck.Status == ValidationFailed {
					// Reaching here with a demonstrably failing proposal means
					// the gate above did not decide: either the baseline
					// demonstrably failed (repair-in-progress), or there was
					// no baseline to read -- the file is gone or unreadable.
					// That second case is why this gate stays.
					// readOriginalForGate yields "" for it, which the checker
					// finds healthy, so the regression is still refused rather
					// than landing unexamined.
					if baselineCheck.Status == ValidationUnknown {
						baselineCheck = fallbackSyntaxOutcomeFor(ctx, input.Path, original).aggregate()
					}
					if !baselineAllowsRepair(baselineCheck) {
						synErr := proposalCheck.Detail
						// The check examined input.Content, the exact bytes
						// that would have been written, and nothing has
						// reached disk at this point.
						return &ToolResult{
							Success:          false,
							Error:            fallbackSyntaxRejection(input.Path, input.Content, synErr),
							MutationStatus:   MutationRefused,
							ValidationKind:   ValidationKindSyntax,
							ValidationStatus: ValidationFailed,
							ValidationDetail: synErr,
						}, nil
					}
					log.Printf("[write_file] %s still unparsable after fast-path write (was already broken) — allowing repair-in-progress", input.Path)
				}
				// #147 review finding #1: the fast-path skips V3 (and its
				// structural veto), so it needs the same structural gate
				// edit_file/structural_edit have — otherwise a fast-path rewrite that
				// introduces render_template lands as verified and 500s. A new
				// unresolved call vs the on-disk state blocks; an unreadable
				// original skips the gate (fail open).
				if origOK {
					if introduced := editIntroducesUnresolved(ctx, path, original, input.Content); len(introduced) > 0 {
						log.Printf("[write_file] fast-path write introduces unresolved call(s) %v in %s — rejecting", logPaths(introduced), logPath(input.Path))
						// Syntax ran first and PASSED on these exact bytes; the
						// structural check is what refused them, so structural
						// is the decisive outcome.
						return &ToolResult{Success: false,
							Error:            structuralWriteRejection(input.Path, introduced),
							MutationStatus:   MutationRefused,
							ValidationKind:   ValidationKindStructural,
							ValidationStatus: ValidationFailed,
							ValidationDetail: structuralWriteRejection(input.Path, introduced),
						}, nil
					}
				}
			}

			// #147: the T0/T1 direct path skipped the structural gate — a
			// sub-10-line .py calling an unimported name landed as verified,
			// the same NameError class the edit path blocks. Structural gate
			// ONLY (.py-scoped, healthy->broken, fail-open): a syntax gate
			// here would hard-block legitimate non-parsing T1 content that
			// this branch exists to handle (JSONC, multi-doc and templated
			// YAML, scaffold .py templates). An unreadable existing original
			// skips the gate (fail open) — treating it as empty would count
			// every pre-existing call as introduced.
			if !iterating {
				if original, ok := readOriginalForGate(path); ok {
					if introduced := editIntroducesUnresolved(ctx, path, original, input.Content); len(introduced) > 0 {
						log.Printf("[write_file] direct write introduces unresolved call(s) %v in %s — rejecting", logPaths(introduced), logPath(input.Path))
						// Structural validation failed before any mutation.
						// Syntax does NOT run on this route (see the comment
						// above), so no syntax verdict is implied in either
						// direction -- the decisive demonstrated fact is the
						// structural failure.
						//
						// editIntroducesUnresolved -> checkStructuralUnresolved
						// posts to /internal/structural_check (NOT
						// /internal/symbol_index, a different path in
						// context.go) and returns
						// {"ok": bool, "unresolved": []string}. It FAILS OPEN
						// when ok is absent or false, on transport error,
						// non-200, parse error or missing tree-sitter, and
						// needs BOTH the edited- and original-side calls to
						// succeed before it can refuse. Observing an HTTP
						// request therefore does not prove the gate ran; only
						// the refusal does.
						rejection := structuralWriteRejection(input.Path, introduced)
						return &ToolResult{
							Success:          false,
							Error:            rejection,
							MutationStatus:   MutationRefused,
							ValidationKind:   ValidationKindStructural,
							ValidationStatus: ValidationFailed,
							ValidationDetail: rejection,
						}, nil
					}
				}
			}

			// A NEW file that does not parse is unambiguously wrong, so the
			// direct path gates it. The healthy->broken rule the gate uses
			// elsewhere needs an "before" state; a file being created has
			// none, which is exactly why this case is decidable.
			//
			// This path was ungated because the sandbox's YAML checker
			// rejected multi-document files — valid YAML, and the shape every
			// Kubernetes and Compose manifest uses. That checker is fixed
			// (safe_load_all), so the one reason to skip the gate here is
			// gone. Observed: a 4-line test_discount.py with an unterminated
			// string reached disk through this path.
			// os.Stat, not readOriginalForGate: that helper returns ("", true)
			// for a MISSING file — its bool means "usable as a baseline", not
			// "exists" — so testing it here silently skipped the gate.
			// Destination absence is determined ONCE, and when the file is
			// new the structured checker is evaluated ONCE. This route owns a
			// real observation, so it overlays it onto the result afterwards
			// rather than leaving writeFileDirect's conservative default.
			_, statErr := os.Stat(path)
			isNew := os.IsNotExist(statErr)
			newFileCheck := checkOutcome{Status: ValidationUnknown}
			if isNew {
				newFileCheck = fallbackSyntaxOutcomeFor(ctx, input.Path, input.Content).aggregate()
				if newFileCheck.Status == ValidationFailed {
					log.Printf("[write_file] new file %s does not parse — writing with a warning (%s)",
						logPath(input.Path), truncateStr(newFileCheck.Detail, 80))
					return writeNewFileWithWarning(path, input.Path, input.Content, newFileCheck.Detail, ctx)
				}
			}

			// The destination exists and these exact bytes were already
			// observed not to parse. A healthy baseline was refused outright
			// above, so reaching here means what is on disk is itself broken
			// or unreadable — the same case the non-bypassed route lands with
			// a warning. Land it the same way, and say so.
			//
			// Only the SAYING is new. The observation was already overlaid
			// onto the result below, so the ledger and the completion gate
			// knew; the model did not. It was handed {"bytes_written":1243}
			// and nothing else, which reads as a clean write. Measured on
			// multifile_cli rep2: the model rewrote test_store.py containing
			// `store.add("Buy milk"")`, was told the write succeeded, ran
			// pytest, could not reconcile the failure with a write it had
			// been told was fine, and resent content until the repetition
			// breaker closed the path — leaving the file unparseable at the
			// end of the session. The sandbox had answered `valid:false` for
			// those bytes the whole time.
			if proposalCheck.Status == ValidationFailed {
				log.Printf("[write_file] %s: landing an unparseable rewrite with a warning (%s)",
					logPath(input.Path), safeDiagnosticSummary(proposalCheck.Detail))
				return preflightWarnedWrite(path, input.Path, input.Content, proposalCheck, ctx)
			}

			// The two comparative gates the V3 route applies to the bytes it
			// writes (writeFileWithV3). They are checks on the write, not part
			// of candidate generation, so a write that does not generate --
			// small, mid-iteration, or with nothing deliverable -- must not skip
			// them. Healthy->broken and fail-soft, as there.
			if original, ok := readOriginalForGate(path); ok {
				if msg := embeddedScriptGate(ctx, path, original, input.Content); msg != "" {
					log.Printf("[write_file] direct write breaks an embedded script in %s — rejecting", logPath(input.Path))
					return &ToolResult{Success: false, Error: msg,
						MutationStatus:   MutationRefused,
						ValidationKind:   ValidationKindStructural,
						ValidationStatus: ValidationFailed,
						ValidationDetail: msg}, nil
				}
				if msg := duplicateMainGuard(path, original, input.Content); msg != "" {
					log.Printf("[write_file] direct write duplicates the module entrypoint in %s — rejecting", logPath(input.Path))
					return &ToolResult{Success: false, Error: msg,
						MutationStatus:   MutationRefused,
						ValidationKind:   ValidationKindStructural,
						ValidationStatus: ValidationFailed,
						ValidationDetail: msg}, nil
				}
			}

			logMandatoryChecks(ctx, "write_file", input.Path, "direct")
			// T1: Direct write — config, data, boilerplate
			res, err := writeFileRecorded(path, input.Content, ctx)
			if err == nil && res != nil && res.Success {
				ctx.SessionWrites[input.Path] = true
			}
			// Mutation facts stay as the writer reported them; only the
			// validation observation is overlaid, and only by a route that
			// actually made one. Unknown is preserved.
			switch {
			case isNew:
				res, err = applyRouteObservation(res, err, newFileCheck)
			case proposalCheck.Status != ValidationUnknown:
				// Every remaining route through this write -- the active-debug
				// fast path and the ordinary direct write over an existing
				// file -- already holds an observation of these exact bytes.
				// The shared regression gate makes it whenever the destination
				// exists, and the active-debug branch fills it in for the one
				// case where the destination is gone, so non-Unknown IS the
				// ownership test. Restating it with a second checker call
				// would spend a round trip to risk a different answer about
				// bytes that have not changed since.
				res, err = applyRouteObservation(res, err, proposalCheck)
			}
			return res, err
		},
	}
}

// isActiveDebugIteration reports whether the model is in a tight edit-
// test-fix loop on `path`: it has already written this file this session
// AND the most recent tool action was a run_command/run_background that
// FAILED with output referencing this file. In that state the next write
// is a targeted fix for an error the model just saw, so the full V3
// pipeline only adds latency (and on a mid-debug file often "completes
// without result"). Fast-path those writes so iteration runs at test
// speed. The FIRST write of a file (SessionWrites false) still gets V3 —
// that is where V3's generation adds value.
func isActiveDebugIteration(ctx *AgentContext, path string) bool {
	if ctx == nil || !ctx.SessionWrites[path] {
		return false
	}
	base := filepath.Base(path)
	for i := len(ctx.Messages) - 1; i >= 0; i-- {
		m := ctx.Messages[i]
		if m.Role != "tool" {
			continue
		}
		// Only the MOST RECENT tool action counts: if the model's last
		// step was a read or an edit rather than a failing run, it isn't
		// mid-test-fix on this file.
		if m.ToolName != "run_command" && m.ToolName != "run_background" {
			return false
		}
		return strings.Contains(m.Content, `"success":false`) &&
			mentionsFilename(m.Content, base)
	}
	return false
}

// mentionsFilename reports whether `base` appears in text as a whole
// filename token, not as a substring of a longer name (#147 review finding
// #12: a.py must not match data.py, main.py must not match domain.py). The
// character on each side of a real occurrence must not be a filename
// character (letter, digit, _, ., -), so `webapp.py` and `app.python` don't
// count while `./app.py`, `"app.py"`, and ` app.py:12` do.
func mentionsFilename(text, base string) bool {
	if base == "" {
		return false
	}
	isNameChar := func(b byte) bool {
		return b == '_' || b == '.' || b == '-' ||
			(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
	}
	for i := 0; ; {
		j := strings.Index(text[i:], base)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(base)
		leftOK := start == 0 || !isNameChar(text[start-1])
		rightOK := end == len(text) || !isNameChar(text[end])
		if leftOK && rightOK {
			return true
		}
		i = start + 1
	}
}

// readFileByteCap returns the byte cap for a single read_file result.
// Derived from the per-slot context so one read can't overflow it — and
// sized for the WORST-CASE tokenization of ~1 token/char (dense content:
// G-code, minified JS, base64), not the chars/4 average. A fixed 120 KB
// cap was safe for prose but not dense content: a gcode-to-text task read a
// capped 120 KB G-code page that still tokenized to 120k tokens and 400'd
// the 32k slot. Half the per-slot context in bytes guarantees even
// 1-token/char content uses at most half the window, leaving room for the
// system prompt, tools, and reply; the force-trim retry then handles
// any residual pressure since no single message is huge. Clamped
// to [2 KB, 200 KB]. Tunable via ATLAS_MAX_READ_BYTES.
func readFileByteCap() int {
	if v := envOr("ATLAS_MAX_READ_BYTES", ""); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return n
		}
	}
	perSlot := 32768
	if b := conversationTokenBudget(); b > 0 {
		// budget already reserves reply + margin; treat it as a
		// worst-case char cap (1 token/char) and take half for a
		// single read so other context still fits.
		perSlot = b
	}
	// Half the per-slot budget: a single read uses at most half the window
	// even at ~1 token/char. The lower clamp must stay AT OR BELOW that
	// half — a fixed 16 KB floor could exceed a small-context slot's whole
	// budget and re-introduce the overflow this cap exists to prevent
	// (#147 review finding #6). 2 KB is a sane minimum and, since the token
	// budget floors at 4000, never actually raises the derived value.
	cap := perSlot / 2
	if cap < 2_000 {
		cap = 2_000
	}
	if cap > 200_000 {
		cap = 200_000
	}
	return cap
}

var maxReadFileBytes = readFileByteCap()

// isBinaryContent reports whether data looks like a binary (non-text)
// file. A NUL byte in the head is the reliable signal — text encodings
// (UTF-8/ASCII) never contain NUL, while compiled binaries, images, and
// archives are full of them. Scans a bounded head so a large file is cheap.
func isBinaryContent(data []byte) bool {
	// UTF-16/UTF-32 text legitimately contains NUL bytes; a Unicode BOM
	// identifies it as text, not binary (#147 review finding #7).
	if len(data) >= 2 {
		b0, b1 := data[0], data[1]
		if (b0 == 0xFF && b1 == 0xFE) || (b0 == 0xFE && b1 == 0xFF) { // UTF-16 LE/BE
			return false
		}
	}
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF { // UTF-8 BOM
		return false
	}
	n := len(data)
	if n > 8000 {
		n = 8000
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

// readFileTruncationNotice explains a read the byte cap cut short.
//
// The old wording ended "process it with run_command (grep/awk/sed/head, or a
// python script) instead of loading it all into context". That is right for
// inspecting a large SOURCE file and wrong for a DATA file the program is
// meant to open at runtime: it points the model at shell pipelines and stdin,
// and the caller then runs `python solve.py` with no stdin and gets 0.
//
// Measured on the AoC tasks, whose answer is computed from input.txt:
//
//	shoal   1 line,     600 B    never truncated   92%
//	slope   400 lines,  12800 B  truncated         50%
//	course  1200 lines, 9707 B   truncated         27%
//	sonar   2000 lines, 8707 B   truncated         27%
//
// The score tracks the notice rather than the problem. The same model
// prompted directly never reads the file at all and scored 83-100% on every
// one of them.
// It also said "the first N" whatever offset was asked for, and told the model
// the head was enough whatever kind of file it was. Both are wrong for source.
//
// Measured on smallrung_toml, which failed 6 of 6 attempts. executor_server.py
// is 2026 lines with two routines that dispatch on `lang`: normalize_language
// at line 208, which only maps aliases, and _syntax_check_impl at line 1266,
// which is the one the request describes. A default read returns the first 214
// lines, so the decoy is visible and the target is not. All six sessions edited
// the decoy. Two paginated correctly with offset=214 and were told "showing the
// first 225 of 2026 lines" -- a false label that makes pagination look broken.
// No session ever called search_files or outline_file, either of which finds
// _syntax_check_impl immediately, while this notice was telling them the rest
// was unnecessary.
//
// The data-file advice stays for data files: it was measured to help on the AoC
// tasks, where the right move IS to have the program open the file at runtime.
// Source code gets the advice that fits source code -- where the window is and
// how to find a symbol.
func readFileTruncationNotice(start, shown, totalLines, totalBytes int, isSource bool) string {
	where := fmt.Sprintf("lines %d-%d of %d (%d bytes total)",
		start+1, start+shown, totalLines, totalBytes)
	if isSource {
		return fmt.Sprintf(
			"\n... [read_file truncated: showing %s. The rest of this file is NOT "+
				"shown, and what you are looking for may be in it. To find a symbol "+
				"use `search_files` or `outline_file` \u2014 do not assume the part you "+
				"can see is the part the request is about. To read a different range, "+
				"read again with offset/limit.]",
			where)
	}
	return fmt.Sprintf(
		"\n... [read_file truncated: showing %s. If you are writing code that reads "+
			"this file when it runs, you do not need the rest here \u2014 have your "+
			"program open it. To look at a different part of the file, read it again "+
			"with offset/limit.]",
		where)
}

// fileIsSourceCode reports whether a path holds code a request might ask to
// change, as opposed to data or markup a program consumes. Reuses the
// registry's own Executable flag so this cannot drift from it.
func fileIsSourceCode(path string) bool {
	meta, known := syntaxGateLanguages[strings.ToLower(filepath.Ext(path))]
	return known && meta.Executable
}

// writeNewFileWithWarning lands syntactically broken content in a file that
// does not exist yet, and says so, instead of rejecting the write.
//
// The rejection was the harness substituting its judgement for execution's.
// On an EXISTING file it earns its keep: it stops working code being
// clobbered with garbage. On a new file there is nothing to protect, and the
// rejection forbids the one loop that measurably works — write it, run it,
// read the real traceback, fix it. The no-tool baseline arm, whose only
// feedback IS the traceback, resolves its own syntax errors at 85-100%.
// Under the rejection the model retried blind against our error message,
// resent byte-identical content, and the repetition breaker ended the
// session: three AoC sessions and a novel-arm session all died as
// "solve.py was never created" — code on hand, nothing on disk.
func writeNewFileWithWarning(path, inputPath, content, synErr string, ctx *AgentContext) (*ToolResult, error) {
	logMandatoryChecks(ctx, "write_file", inputPath, "syntax failed, landing with a warning")
	res, err := writeFileRecorded(path, content, ctx)
	if err != nil || res == nil || !res.Success {
		return res, err
	}
	// Keyed on the INPUT path: every SessionWrites reader and writer uses the
	// path as the model sent it (input.Path / wfInput.Path). This helper
	// briefly keyed on the resolved path, so the session-owned carveout
	// missed and the model's own CORRECTION of a warned write was rejected
	// as an overwrite of an unread existing file — fail-forward landed the
	// broken file and then forbade fixing it. Measured: every early novel-arm
	// session hit "rejecting write_file over unread existing solve.py"
	// immediately after a warned write.
	ctx.SessionWrites[inputPath] = true
	out := WriteFileOutput{
		BytesWritten: len(content),
		Warning: fmt.Sprintf(
			"written, but it does not parse (%s).%s Run it now and read the "+
				"real traceback, then fix that line and write it again.",
			truncateStr(synErr, 160), offendingLineNote(content, synErr)),
	}
	outBytes, _ := json.Marshal(out)
	res.Data = outBytes
	// The syntax check ran on exactly `content`, and writeFileDirect wrote
	// exactly `content`, so the failure describes the bytes on disk. Applied
	// and failed are orthogonal here on purpose: the file landed AND it does
	// not parse, which is the documented debugging policy.
	res.MutationStatus = MutationApplied
	res.ValidationKind = ValidationKindSyntax
	res.ValidationStatus = ValidationFailed
	res.ValidationDetail = synErr
	return res, nil
}

// preflightWarnedWrite lands a warned write for the V3 preflight and keeps the
// observation that justified the warning attached to whichever outcome the
// filesystem produced.
//
// writeNewFileWithWarning already states applied + syntax/failed when the bytes
// land. When they do not, the writer reports not_run, which is honest for a
// layer that ran no check of its own -- but this route did run one, on exactly
// these bytes, and a failed mutation does not unmake that observation.
func preflightWarnedWrite(path, inputPath, content string, o checkOutcome, ctx *AgentContext) (*ToolResult, error) {
	res, err := writeNewFileWithWarning(path, inputPath, content, o.Detail, ctx)
	return applyRouteObservation(res, err, o)
}

// writeFileRecorded is writeFileDirect plus the body-seen record.
//
// Content the model authored is content it has seen, and that has to hold
// whichever write path ran. Recording it only on the T1 direct write left
// every V3-path write unmarked, so the evidence gate fired on files the
// model had just created: measured on run 18, 11 of 12 gate firings were
// solve.py or test_stats.py, each written through writeFileWithV3 moments
// earlier. Wrapping the single function all four call sites funnel through
// is what keeps a fifth from reintroducing it.
func writeFileRecorded(path, content string, ctx *AgentContext) (*ToolResult, error) {
	res, err := writeFileDirect(path, content)
	if err == nil && res != nil && res.Success && ctx != nil {
		ctx.RecordBodySeen(path)
	}
	return res, err
}

// writeFileDirect writes content to disk atomically (write tmp + rename).
// The proxy is the only thing downstream that touches the filesystem —
// the TUI is read-only at the workspace level — so this is where any
// write_file tool call ultimately lands. Without this the file would
// vanish into the void ("agent says it wrote the file but it isn't
// there" bug).
// classifiedError carries mutation/validation facts alongside a real error,
// so the site that OWNS the evidence can state them without changing the
// (nil, err) contract callers branch on. The V3 route and every other caller
// still receive a non-nil error and take their existing error paths; only the
// outward ToolResult synthesised at the boundary is enriched.
type classifiedError struct {
	err              error
	mutationStatus   MutationStatus
	validationKind   ValidationKind
	validationStatus ValidationStatus
}

func (c *classifiedError) Error() string { return c.err.Error() }
func (c *classifiedError) Unwrap() error { return c.err }

// failedMutation tags a filesystem failure: a mutation was attempted and did
// not establish the intended state. Validation reports honestly that no check
// passed -- not_run for recognised code, not_applicable otherwise -- because
// writeFileDirect runs no check of its own.
func failedMutation(path string, err error) error {
	kind, status := ValidationKindNone, ValidationNotApplicable
	if _, gated := syntaxGateLanguages[strings.ToLower(filepath.Ext(path))]; gated {
		kind, status = ValidationKindSyntax, ValidationNotRun
	}
	return &classifiedError{err: err, mutationStatus: MutationFailed,
		validationKind: kind, validationStatus: status}
}

// overlayValidation replaces ONLY the validation fields of a result with an
// observation the caller obtained upstream. writeFileDirect's conservative
// default (syntax/not_run for recognised code) is right when nobody checked;
// a route that DID check owns the stronger answer and overlays it here.
//
// Mutation facts and Success are never touched, and nothing is inferred from
// Success, from the extension, or from a wrapper's ok boolean. Unknown is
// preserved rather than normalised, so the result stays visibly unclassified
// and a sentinel can catch the producer defect.
func overlayValidation(res *ToolResult, o checkOutcome) {
	if res == nil {
		return
	}
	res.ValidationStatus = o.Status
	res.ValidationDetail = o.Detail
	switch o.Status {
	case ValidationNotApplicable:
		res.ValidationKind = ValidationKindNone
	case ValidationUnknown:
		res.ValidationKind = ValidationKindUnknown
	default:
		res.ValidationKind = ValidationKindSyntax
	}
}

// overlayValidationOnError does the same for a classified filesystem error:
// the mutation still failed and the error is still returned non-nil, but the
// validation observation made on those exact proposed bytes is preserved.
// Mutation failure and validation success are orthogonal -- checked bytes can
// still fail to land. An untyped error is returned unchanged, so the overlay
// never manufactures a classification no producer made.
func overlayValidationOnError(err error, o checkOutcome) error {
	var ce *classifiedError
	if !errors.As(err, &ce) {
		return err
	}
	ce.validationStatus = o.Status
	switch o.Status {
	case ValidationNotApplicable:
		ce.validationKind = ValidationKindNone
	case ValidationUnknown:
		ce.validationKind = ValidationKindUnknown
	default:
		ce.validationKind = ValidationKindSyntax
	}
	return err
}

// applyRouteObservation is how a route that owns an observation attaches it,
// on both outcomes of the mutation attempt. Success and failure take the same
// observation because they are orthogonal facts: checked bytes can still fail
// to land, and bytes that landed can still be the ones that do not parse.
// Keeping it in one place is what makes two routes provably identical here.
func applyRouteObservation(res *ToolResult, err error, o checkOutcome) (*ToolResult, error) {
	if err != nil {
		return res, overlayValidationOnError(err, o)
	}
	overlayValidation(res, o)
	return res, nil
}

// atomicReplaceFile is the write-then-rename the mutating tools have always
// used, as one primitive. A reader of the target either sees the old bytes or
// the new ones, never a partial file, and a failed rename leaves the target
// untouched with the temp file cleaned up.
func atomicReplaceFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("cannot create parent dir for %s: %w", path, err)
	}
	tmpPath := path + ".atlas.tmp"
	if err := os.WriteFile(tmpPath, content, 0644); err != nil {
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("cannot rename temp file: %w", err)
	}
	return nil
}

func writeFileDirect(path, content string) (*ToolResult, error) {
	if err := atomicReplaceFile(path, []byte(content)); err != nil {
		return nil, failedMutation(path, err)
	}
	out := WriteFileOutput{BytesWritten: len(content)}
	outBytes, _ := json.Marshal(out)
	// Slice 1 classification. The bytes are on disk after the rename, so the
	// mutation is demonstrated rather than intended.
	//
	// Validation is deliberately NOT claimed here. This layer performs no
	// syntax check, so for recognized code the honest answer is not_run: a
	// caller that did validate these exact bytes upgrades it, and one that
	// did not leaves the truth visible. Claiming passed because a write
	// succeeded is the conflation this whole contract exists to remove.
	kind, status := ValidationKindNone, ValidationNotApplicable
	if _, gated := syntaxGateLanguages[strings.ToLower(filepath.Ext(path))]; gated {
		kind, status = ValidationKindSyntax, ValidationNotRun
	}
	return &ToolResult{
		Success: true, Data: outBytes,
		MutationStatus: MutationApplied,
		ValidationKind: kind, ValidationStatus: status,
	}, nil
}

// v3CandidatesTested unwraps a possibly-nil V3 response so the
// stage_end envelope can carry a count even on error paths.
func v3CandidatesTested(r *V3GenerateResponse) int {
	if r == nil {
		return 0
	}
	return r.CandidatesTested
}

// latestUserMessage is the HUMAN's request, for handing the V3 pipeline the
// requirement it is generating against. ctx.HumanTask is authoritative: the
// loop stores the request verbatim before appending anything, because the
// conversation itself cannot answer this question — correctives, manifests
// and re-injected file content all ride user-role messages for chat-template
// compatibility, and "last user turn" was observed resolving to "[system
// note]: run the program standalone" mid-session, sending V3 off to generate
// against harness feedback instead of the task (third-party audit finding).
// The scan below is a fallback for contexts built without the loop (tests,
// direct bridge calls) and skips synthetic notes.
func latestUserMessage(ctx *AgentContext) string {
	if ctx == nil {
		return ""
	}
	if ctx.HumanTask != "" {
		return ctx.HumanTask
	}
	for i := len(ctx.Messages) - 1; i >= 0; i-- {
		if ctx.Messages[i].Role != "user" {
			continue
		}
		if strings.HasPrefix(ctx.Messages[i].Content, "[system note]:") {
			continue
		}
		return ctx.Messages[i].Content
	}
	return ""
}

// writeFileWithV3 routes through the V3 pipeline for T2/T3 tasks.
// Model's content becomes baseline candidate #0; V3 generates diverse
// alternatives, tests all, selects the best.
func writeFileWithV3(path, baselineContent string, ctx *AgentContext) (*ToolResult, error) {
	// Build V3 request with project context
	req := V3GenerateRequest{
		FilePath:     path,
		BaselineCode: baselineContent,
		Tier:         int(ctx.Tier),
		WorkingDir:   ctx.WorkingDir,
		UserMessage:  latestUserMessage(ctx),
	}

	// Add project context from files read during this session. The target
	// file's own PRE-EDIT snapshot is excluded: its current symbols come
	// from the baseline, and stale content would let the in-pipeline veto
	// credit a def this write deletes (#147 review finding #2 — same rule
	// as checkStructuralUnresolved).
	cleanTarget := filepath.Clean(path)
	if filesRead := ctx.SnapshotFilesRead(); len(filesRead) > 0 {
		req.ProjectContext = make(map[string]string)
		for p, content := range filesRead {
			if filepath.Clean(p) == cleanTarget {
				continue
			}
			relPath, _ := filepath.Rel(ctx.WorkingDir, p)
			if relPath == "" {
				relPath = p
			}
			// Truncate large files in context to save tokens
			if len(content) > 4000 {
				content = content[:4000] + "\n... (truncated)"
			}
			req.ProjectContext[relPath] = content
		}
	}

	// Files this session WROTE are project context too — previously only
	// files the model had read were included, so written-but-never-read
	// siblings were invisible to candidate generation (2026-07-18: V3
	// generated an app.py blind to the session's own templates/index.html
	// and static/game.js, and the winner inlined its page, orphaning
	// both). Disk content wins over any stale read snapshot: the on-disk
	// version is what verification produced.
	for rel := range ctx.SessionWrites {
		if rel == "" {
			continue
		}
		abs := resolveAgentPath(ctx, rel)
		if abs == path {
			continue // the file being generated is the baseline, not context
		}
		data, rerr := os.ReadFile(abs)
		if rerr != nil {
			continue
		}
		content := string(data)
		if len(content) > 4000 {
			content = content[:4000] + "\n... (truncated)"
		}
		if req.ProjectContext == nil {
			req.ProjectContext = make(map[string]string)
		}
		req.ProjectContext[rel] = content
	}

	// Add project info if available
	if ctx.Project != nil {
		req.Framework = ctx.Project.Framework
		req.BuildCommand = ctx.Project.BuildCommand
	}

	// THE production call path for invocation feasibility. It asks, before a
	// single candidate is generated, whether this task could close at all --
	// the question the sealed Stage-A run answered 103 times, once per
	// candidate, after generating each.
	//
	// It sits ABOVE the "V3 is taking over" message on purpose: under enforce
	// a skipped invocation must not first tell the user a pipeline is running.
	// Under observe the answer is recorded and generation proceeds exactly as
	// it did at e8fefe8, whatever it concludes.
	// One entry of the candidate-generation route, named before anything is
	// decided about it. Every record this attempt produces carries it, so a
	// retry's work can never be mistaken for this one's.
	entry := mintRouteEntry(ctx)
	// One owner for how this entry ends, finalised exactly once. The deferred
	// default means a branch that forgets to speak still records the
	// fail-closed member instead of leaving an entry that never ended.
	lifecycle := newRouteLifecycle(entry)
	// The attribution is deferred first so it runs last, over the recorded
	// ending.
	defer lifecycle.recordAttribution(ctx)
	defer lifecycle.finalizeDefault(ctx)
	// Recorded, never enforced: generation proceeds whatever it says.
	observeInvocationFeasibility(ctx, entry)

	// Tell the user V3 is taking over so they don't think the file
	// vanished. write_file with V3 holds the disk write until V3 picks
	// a winner — without this message the chat goes silent for the 1–3
	// minute V3 cycle and looks broken.
	if ctx.StreamFn != nil {
		ctx.StreamFn("v3_progress", map[string]string{
			"message": fmt.Sprintf("V3 pipeline starting for %s — generating diverse candidates and build-verifying each.", filepath.Base(path)),
		})
	}
	Emit(NewEnvelope(EvtStageStart, "v3", map[string]interface{}{
		"detail": fmt.Sprintf("file=%s", filepath.Base(path)),
		// Additive: an existing consumer sees the field it always saw. This
		// one names which entry of the route generated, so an observer can
		// pair a generation with its own feasibility decision instead of
		// counting events.
		"route_entry": entry.ID,
	}))
	v3Start := time.Now()

	// Call V3 service with streaming progress. Each stage callback also
	// fires a typed envelope so the pipeline pane shows V3 progress.
	// Three categories of progress events:
	//   token       — per-LLM-token delta from V3's streaming generator
	//   llm_start   — V3 is starting an LLM call (candidate gen, scoring…)
	//   llm_end     — V3's LLM call finished (with token/timing summary)
	//   <other>     — pipeline stage marker (probe, plansearch, sandbox…)
	currentV3Stage := ""
	v3Result, err := callV3GenerateStreaming(ctx.Ctx, ctx.V3URL, req, func(stage, detail string, data map[string]interface{}) {
		// Token deltas: forward to the TUI on a separate SSE event so
		// it can render them as a streaming dim row, mirroring how the
		// agent's own LLM tokens are shown. No envelope (would bloat
		// /events with thousands of metric events for a single call).
		if stage == "token" {
			if ctx.StreamFn != nil {
				ctx.StreamFn("v3_token", map[string]string{"text": detail})
			}
			return
		}
		// Reasoning deltas during V3 LLM calls (<think>
		// stream). Forwarded as v3_reasoning_token (NOT plain
		// reasoning_token) because the agent-loop's reasoning_token
		// handler in the TUI targets the agent's LLM row — V3 calls
		// run in a different lifecycle and need their own pipe into
		// the V3 streaming row. Gives the /demo viewer something to
		// watch during long PlanSearch / repair phases.
		if stage == "reasoning_token" {
			if ctx.StreamFn != nil {
				ctx.StreamFn("v3_reasoning_token", map[string]string{"text": detail})
			}
			return
		}
		// LLM-call boundary markers. Match the chat protocol's
		// llm_call_start/end shapes so the TUI can reuse handlers.
		if stage == "llm_start" {
			if ctx.StreamFn != nil {
				payload := map[string]interface{}{"detail": detail}
				for k, v := range data {
					payload[k] = v
				}
				ctx.StreamFn("v3_llm_start", payload)
			}
			return
		}
		if stage == "llm_end" {
			if ctx.StreamFn != nil {
				payload := map[string]interface{}{"detail": detail}
				for k, v := range data {
					payload[k] = v
				}
				ctx.StreamFn("v3_llm_end", payload)
			}
			return
		}

		// Dedicated structured events for the pipeline pane. The TUI
		// renders each as its own row instead of a generic v3_progress
		// string. data is the structured payload from V3's _emit; we
		// pass it through verbatim with `stage` and `detail` for
		// fallback rendering.
		if ctx.StreamFn != nil {
			eventName := v3StageToEvent(stage)
			if eventName == "v3_progress" {
				// Unknown / unmapped stage — emit the legacy text line
				// only. Keeps third-party clients that haven't migrated
				// to typed events working.
				ctx.StreamFn("v3_progress", map[string]string{
					"message": fmt.Sprintf("  \u2502 [%s] %s", stage, detail),
				})
			} else {
				payload := map[string]interface{}{
					"stage":  stage,
					"detail": detail,
				}
				for k, v := range data {
					payload[k] = v
				}
				ctx.StreamFn(eventName, payload)
			}
		}
		// Stage transitions emit start/end envelopes for the pipeline
		// pane — close the previous stage when we see a new name.
		if stage != currentV3Stage {
			if currentV3Stage != "" {
				Emit(Envelope{
					EventID:   NewEventID(),
					Timestamp: float64(time.Now().UnixNano()) / 1e9,
					Type:      EvtStageEnd,
					Stage:     "v3:" + currentV3Stage,
					Payload: map[string]interface{}{
						"success": true,
					},
				})
			}
			payload := map[string]interface{}{"detail": detail}
			for k, v := range data {
				payload[k] = v
			}
			Emit(NewEnvelope(EvtStageStart, "v3:"+stage, payload))
			currentV3Stage = stage
		} else {
			Emit(NewEnvelope(EvtMetric, "v3:"+stage,
				map[string]interface{}{"name": "progress", "value": detail}))
		}
	})
	if currentV3Stage != "" {
		Emit(Envelope{
			EventID:   NewEventID(),
			Timestamp: float64(time.Now().UnixNano()) / 1e9,
			Type:      EvtStageEnd,
			Stage:     "v3:" + currentV3Stage,
			Payload:   map[string]interface{}{"success": err == nil},
		})
	}
	// The structured evidence rides the telemetry envelope, whole. It is
	// deliberately NOT put on the ToolResult: that is projected to the model
	// and to the guarded tool-result SSE, where an unconsumed research field
	// would become an accidental interface. Telemetry is where a durable
	// record belongs while nothing decides from it yet.
	v3StagePayload := map[string]interface{}{
		"success":           err == nil,
		"candidates_tested": v3CandidatesTested(v3Result),
	}
	if v3Result != nil {
		v3StagePayload["evidence"] = evidenceTelemetry(
			v3Result.Evidence, v3Result.EvidenceUnavailableReason)
		// The authorization decision and its reason, recorded where a durable
		// record belongs. It is deliberately not put on the ToolResult, which
		// is projected to the model and to the guarded tool-result SSE.
		authorized, why := v3DeliveryAuthorized(v3Result, v3Result.Code)
		auth := map[string]interface{}{"authorized": authorized}
		if why != "" {
			auth["reason"] = why
		}
		v3StagePayload["authorization"] = auth
	}
	Emit(Envelope{
		EventID:    NewEventID(),
		Timestamp:  float64(time.Now().UnixNano()) / 1e9,
		Type:       EvtStageEnd,
		Stage:      "v3",
		DurationMS: time.Since(v3Start).Milliseconds(),
		Payload:    v3StagePayload,
	})
	if err != nil {
		// User cancellation is not a fallback case — the turn was aborted,
		// so nothing should land on disk.
		if errors.Is(err, context.Canceled) || (ctx.Ctx != nil && ctx.Ctx.Err() != nil) {
			log.Printf("[write_file] V3 aborted by cancellation — not writing %s", path)
			lifecycle.finish(ctx, routingCancelled, "", "")
			return &ToolResult{
				Success: false,
				Error:   "write_file cancelled — no content was written",
			}, nil
		}
		// The lens stopped scoring inside V3. The lens is required, so this
		// is not a fallback case either: nothing is written, and the agent
		// loop ends the run with the reason (lens_required.go).
		if why, down := lensUnavailable(err); down {
			log.Printf("[write_file] the lens cannot score (%s) — not writing %s", why, logPath(path))
			ctx.noteLensDown(why)
			lifecycle.finish(ctx, routingProducerUnavailable, "", "")
			return noMutation("write_file not applied: the geometric lens stopped answering (" + why + ")"), nil
		}
		// Fallback to direct write if V3 service unavailable — but never
		// land content the sandbox confirms is broken: a truncated tool
		// call writing a SyntaxError to disk with success=true is how the
		// mini-bench got its two broken files (t06/t09).
		log.Printf("[write_file] V3 failed: %s — falling back to direct write", err)
		// The final summary names this file while these bytes are on disk.
		ctx.noteV3Unchecked(path, v3FailureReason(err), baselineContent)
		if errors.Is(err, context.DeadlineExceeded) {
			lifecycle.finish(ctx, routingProducerTimedOut, "", "")
		} else {
			lifecycle.finish(ctx, routingProducerUnavailable, "", "")
		}
		msg := "  \u2514\u2500 V3 unavailable, writing directly"
		if errors.Is(err, context.DeadlineExceeded) {
			// Not the configured ceiling: the session's remaining time can
			// shorten this call's cap, and naming the wrong number tells the
			// operator to raise a setting that was not the limit they hit.
			// The exact cap and its source are in the error and the log.
			msg = "  \u2514\u2500 V3 ran out of its time budget, writing your version"
		}
		return writeWithoutCandidate(ctx, path, baselineContent, msg)
	}
	// V3 answered for this write: an earlier unchecked write of the file is
	// no longer the one to name.
	ctx.clearV3Unchecked(path)

	// Write the winning candidate (or baseline if V3 didn't improve).
	//
	// The authorization to replace the caller's content is `Passed`, not the
	// presence of `Code`. Today Python only fills Code when a candidate
	// passed, so this held by construction — but that is an invariant on the
	// far side of a JSON boundary, and the evidence work deliberately
	// introduces a "best_record" that is the strongest available candidate
	// while NOT being closure-eligible. Returning its code for diagnostics
	// would have silently made it the delivered artifact. An unverified
	// alternative must never displace the baseline.
	// The structured intent of THIS call: write_file, this canonical target,
	// the caller's own bytes as the proposal. Derived before anything is
	// decided, from the call rather than from the prose, and consulted only to
	// narrow what a candidate may touch.
	scope, scopeOK := deriveMutationScope(ctx, entry, "write_file", path,
		originalForScope(path, ctx), baselineContent)

	code, proposed := proposedV3Candidate(v3Result, baselineContent)
	// Declared here, with the bytes it describes. It used to be declared
	// below the language-swap gate, which is precisely why that gate could
	// restore the baseline without withdrawing provenance.
	fellBack := !proposed

	// Language-swap gate. V3 generates candidates for the TASK, and on a
	// multi-file job the task is not the file: "build me a snake game"
	// produced JavaScript for index.html, replacing a correct 18-line
	// document with 149 lines of JS and no tags. The in-pipeline smoke
	// check could not see it because for .html it runs an HTML parser, and
	// an HTML parser accepts any text at all.
	languageOrBoundaryViolation := false
	if swapped := v3SwappedTheLanguage(path, baselineContent, code); swapped != "" {
		languageOrBoundaryViolation = true
		code, proposed, fellBack = revokeV3(baselineContent, swapped, path)
	}

	// Sanitise V3 output. The pipeline's underlying LLM response
	// occasionally arrives with markdown fences and prose preamble
	// intact; if we don't strip them, every V3-rewritten file ships
	// with a "Looking at the task..." header on disk.
	if cleaned, sanitized := sanitizeFileContent(path, code); sanitized {
		log.Printf("[write_file] sanitised V3 output for %s", path)
		code = cleaned
	}

	// Sanitisation can turn a proposal back into the caller's own content. That
	// is not a candidate, and carrying it forward as one would mint a licence
	// for a delivery that is not going to happen.
	if proposed && code == baselineContent {
		code, proposed, fellBack = revokeV3(baselineContent,
			"sanitisation left the caller's own content", path)
	}

	// FINAL-BYTE OBSERVATION. The bytes are now chosen, and every artifact this
	// machine delivers must carry a structured observation of exactly the bytes
	// that land. It sits here, after sanitisation, because sanitisation rewrites
	// the candidate AFTER V3 earned its verdict, and because V3 verifies by
	// RUNNING code -- a different question from whether these exact bytes parse,
	// answered about text that no longer exists in that form.
	//
	// deliveredCheck is only ever assigned beside the bytes it describes, and
	// checkedFor records which bytes those were, so the pairing is checkable
	// rather than a discipline each branch below has to remember.
	deliveredCheck := fallbackSyntaxOutcomeFor(ctx, path, code).aggregate()
	checkedFor := code

	// THE production call path for the proxy-owned syntax producer. It sits
	// here because this is the one moment the bytes are fixed and the gate has
	// just reported on exactly those bytes: the verdict is handed over, never
	// recomputed, so no second sandbox call and no second opinion. What it
	// produces goes to private telemetry and reaches no decision -- the
	// authorization immediately below is the same one that ran at e8fefe8.
	// THE live authorization owner. For a request that declared structured
	// obligations the typed answer is binding: no candidate lands without a
	// one-time grant, and a refusal keeps the caller's own content rather than
	// falling through to a candidate the envelope happened to like. For a
	// request that declared nothing there is no typed answer to give, so
	// `authorizedV3` stands exactly as it did.
	// A RETAINED BASELINE IS NOT A CANDIDATE.
	//
	// When nothing materially different came back, `code` is the caller's own
	// content. Staging it, taking an authorization decision over it and minting
	// a one-time licence for it describes a delivery that is not going to
	// happen: the fallback below writes those same bytes directly and the
	// licence retires unused.
	//
	// What a candidate does NOT have to be is service-certified. The service
	// ranks; the evidence this machine produces about these exact bytes is what
	// authorizes, and it cannot be produced without staging the candidate.
	candidateProposed := proposed
	// The proposal's own identity, taken here rather than at the ending.
	// Every gate below that withdraws a candidate restores `code` to the
	// caller's bytes, so a hash read at the end names the BASELINE -- which is
	// how a retained baseline came to be recorded under `candidate_hash`, with
	// the model's own content attributed to the pipeline. The pilot has one:
	// route entry 1 of pilot_go_duration_label carries the sha256 of the
	// main.go the model wrote itself.
	proposalIdentity := ""
	if candidateProposed {
		proposalIdentity = contentSHA256(code)
	}

	var observed []proxyEvidence
	var evID candidateEvidenceIdentity
	var unmet map[string]AuthorizationReason
	stagingMutatedAssets := false
	if ev, id, seen := observeDeliveredCandidateSyntax(ctx, entry, path, code, deliveredCheck); seen && candidateProposed {
		observed, evID = []proxyEvidence{ev}, id
	}
	if candidateProposed && evID.InvocationID == "" {
		// No producer spoke -- a request that declared no outputs owes no
		// syntax obligation, and a class the syntax gate does not check owes
		// none either -- but the candidate is still this route entry's and
		// these exact bytes, and the authorization owner binds to that.
		evID = proposedCandidateIdentity(ctx, entry, code)
	}
	if candidateProposed {
		// THE production call path for the client-declared verification
		// producer. It stages these exact bytes in a workspace that is not the
		// caller's and runs the commands the client declared there, so the
		// behavioral question has an answer rather than a blocker. It runs
		// commands only for a request that declared them, and the staging
		// budget bounds what it may spend on one. It no longer waits for a
		// syntax obligation: a declared main.rs has none, so its declared
		// command was owed and never run.
		behavioral, why, mutatedAssets := observeCandidateVerification(ctx, path, code, evID, scope)
		observed, unmet = append(observed, behavioral...), why
		stagingMutatedAssets = mutatedAssets
	}
	selected := ""
	if v3Result != nil && v3Result.Evidence != nil {
		selected = v3Result.Evidence.Identity.CandidateContentHash
	}
	// Unconditionally, and deliberately so. The producers decline for a target
	// the client never declared and for a class the structural gate does not
	// govern, and gating the AUTHORIZATION on a producer having spoken would
	// mean exactly those two cases fell through to the legacy decision -- a
	// candidate landing on a structured request because nothing could speak
	// for it. The owner handles an empty evidence set: it refuses.
	scopeAdmits, scopeRefusal := false, scopeRefusedNoScope
	if scopeOK {
		scopeAdmits, scopeRefusal = scopeAdmitsCandidate(ctx, scope, code)
	}
	if candidateProposed {
		recordMutationScope(ctx, scope, scopeAdmits, scopeRefusal)
	}
	// ONE veto list, two readers. The authorization owner needs the
	// disqualifying facts before it can mint an automatic grant, and the
	// policy owner needs them to reach its decision. The authorization owner
	// computes it, after its decision, and both read that list.
	vetoInput := advisoryInput{
		Observed:                    deliveredCheck,
		TargetDeclared:              outputKnowledgeDeclared(ctx),
		TargetAuthorized:            targetIsAuthorized(requestObligations(ctx), resolveAgentPath(ctx, path)),
		LanguageOrBoundaryViolation: languageOrBoundaryViolation,
		Unmet:                       unmet,
		Evidence:                    observed,
		Cancelled:                   ctx.Ctx != nil && ctx.Ctx.Err() != nil,
		MutatedProtectedAssets:      stagingMutatedAssets,
		ScopeAdmits:                 scopeAdmits,
		ScopeRefusal:                scopeRefusal,
	}
	vetoInput.Envelope = envelopeOf(v3Result)
	// Nil without a candidate: the policy owner then computes the list itself,
	// from the same input.
	var vetoes []string
	var delivery deliveryAuthorization
	if candidateProposed {
		delivery = authorizeCandidateDelivery(ctx, entry, path, code, evID,
			v3Result.Evidence, observed, selected, unmet, deliveredCheck, scope,
			automaticIntent{VetoInput: vetoInput})
		vetoes = delivery.Vetoes
		vetoInput.Unmet = delivery.Unmet
		lifecycle.noteAuthorization(delivery, evID, contentSHA256(code), vetoes)
	}
	// THE policy owner. It reads the typed answer, the trusted observations and
	// the disqualifying facts, and says which of the honest decisions this
	// candidate earned. Only the strict authorization delivers in this build.
	policyInput := vetoInput
	policyInput.Decision = delivery.Decision
	policyInput.CaptureOnlySuppressed = delivery.CaptureOnly
	policyInput.AutomaticEligible = delivery.holds(grantBasisAutomaticV3)
	// The vetoes the authorization owner computed, verbatim. Recomputing them
	// here is exactly the duplication that lets two answers drift apart.
	policyInput.Vetoes = vetoes
	// The would-have, not the outcome: an acquisition control takes the licence
	// and leaves the answer, so the policy is asked the question it would have
	// been asked without one.
	// Strict means strict. An automatic licence also makes mayDeliver true, and
	// reporting it as a strict authorization would name the wrong rule in the
	// record a calibration is computed from.
	policy := decideCandidatePolicy(ctx, policyInput,
		candidateProposed && delivery.holds(grantBasisStrict))
	if candidateProposed {
		recordCandidatePolicyDecision(ctx, entry, contentSHA256(code), policy)
		if delivery.CaptureOnly {
			recordCaptureOnlyDisposition(ctx, entry, contentSHA256(code), policy, true)
		}
	}

	// Whether the proposal becomes the delivered artifact. One owner, for every
	// request: the proxy's policy, over evidence the proxy produced about these
	// exact bytes.
	//
	// A request that declared nothing has no trusted verification to satisfy,
	// so under strict it retains the baseline -- the model's own bytes land,
	// through the model's own path, exactly as if the pipeline had not run.
	// The service's closure verdict does not decide it. A machine that let the
	// producer of a candidate certify that candidate has no authorization
	// story at all, and "it was like that before" is not one either.
	authorizedV3 := candidateProposed && policy.mayDeliverUnderPolicy()

	// A refusal withdraws the candidate. The caller's own content is the
	// alternative, and it is checked before being restored -- the same rule
	// every other gate on this path follows.
	if candidateProposed && !authorizedV3 {
		lifecycle.finish(ctx, routingAuthorizationRefused, contentSHA256(code),
			AuthorizationReason(delivery.Refusal))
		baseCheck := fallbackSyntaxOutcomeFor(ctx, path, baselineContent).aggregate()
		if baseCheck.Status == ValidationFailed {
			return &ToolResult{Success: false,
				Error:            fallbackSyntaxRejection(path, baselineContent, baseCheck.Detail),
				MutationStatus:   MutationRefused,
				ValidationKind:   ValidationKindSyntax,
				ValidationStatus: ValidationFailed,
				ValidationDetail: baseCheck.Detail}, nil
		}
		code, authorizedV3, fellBack = revokeV3(
			baselineContent, "the policy refused it ("+string(policy.Decision)+")", path)
		deliveredCheck, checkedFor = baseCheck, code
	}
	if deliveredCheck.Status == ValidationFailed {
		if !authorizedV3 {
			// The caller's own content, and it does not parse. Nothing here
			// authored it and there is no alternative to fall back to.
			log.Printf("[write_file] fallback content for %s does not parse: %s",
				logPath(path), safeDiagnosticSummary(deliveredCheck.Detail))
			return &ToolResult{Success: false,
				Error:            fallbackSyntaxRejection(path, code, deliveredCheck.Detail),
				MutationStatus:   MutationRefused,
				ValidationKind:   ValidationKindSyntax,
				ValidationStatus: ValidationFailed,
				ValidationDetail: deliveredCheck.Detail}, nil
		}
		// A candidate that does not parse is not a candidate. The model's own
		// content is the alternative, and it is checked before being restored:
		// falling back to bytes nobody looked at is how the harness would
		// replace one broken artifact with another.
		baseCheck := fallbackSyntaxOutcomeFor(ctx, path, baselineContent).aggregate()
		if baseCheck.Status == ValidationFailed {
			log.Printf("[write_file] V3 winner and baseline both fail the syntax gate for %s",
				logPath(path))
			return &ToolResult{Success: false,
				Error:            fallbackSyntaxRejection(path, baselineContent, baseCheck.Detail),
				MutationStatus:   MutationRefused,
				ValidationKind:   ValidationKindSyntax,
				ValidationStatus: ValidationFailed,
				ValidationDetail: baseCheck.Detail}, nil
		}
		log.Printf("[write_file] V3 winner for %s does not parse — writing your version instead (%s)",
			logPath(path), safeDiagnosticSummary(deliveredCheck.Detail))
		code, authorizedV3, fellBack = revokeV3(
			baselineContent, "the winner does not parse", path)
		deliveredCheck, checkedFor = baseCheck, code
	}

	// #147: authoritative structural gate on whatever is about to land —
	// the in-pipeline veto only prunes phase-1 sandbox-passing candidates,
	// so probe/repair returns, the energy fallback, and the baseline
	// resurrection above can all deliver content that calls a name the
	// file never binds. Same rule as the edit paths: original is the
	// on-disk content (empty for a first write), only NEWLY unresolved
	// calls block, and an unreadable existing original skips the gate.
	// When the WINNER fails but the model's own baseline passes, write
	// the baseline instead of rejecting: the offending call is V3-
	// authored, and a rejection would blame the model for content it
	// never wrote and cost a full pipeline retry per resend.
	if original, origOK := readOriginalForGate(path); origOK {
		if introduced := editIntroducesUnresolved(ctx, path, original, code); len(introduced) > 0 {
			if code != baselineContent {
				// The baseline is about to become the delivered artifact, so it
				// is observed before it is restored -- and the verdict is kept,
				// not spent on the allow/refuse decision and discarded.
				baseCheck := fallbackSyntaxOutcomeFor(ctx, path, baselineContent).aggregate()
				if baseCheck.Status == ValidationFailed {
					return &ToolResult{Success: false,
						Error:            fallbackSyntaxRejection(path, baselineContent, baseCheck.Detail),
						MutationStatus:   MutationRefused,
						ValidationKind:   ValidationKindSyntax,
						ValidationStatus: ValidationFailed,
						ValidationDetail: baseCheck.Detail}, nil
				}
				if intrBase := editIntroducesUnresolved(ctx, path, original, baselineContent); len(intrBase) == 0 {
					log.Printf("[write_file] V3 winner introduces unresolved call(s) %v in %s — writing gate-passing baseline instead", logPaths(introduced), logPath(path))
					if ctx.StreamFn != nil {
						ctx.StreamFn("v3_progress", map[string]string{
							"message": "  └─ V3 winner failed the structural gate — writing your version",
						})
					}
					code, authorizedV3, fellBack = revokeV3(
						baselineContent, "winner failed the structural gate", path)
					deliveredCheck, checkedFor = baseCheck, code
				} else {
					introduced = intrBase // name what the MODEL can act on
				}
			}
			if !fellBack {
				log.Printf("[write_file] V3 result introduces unresolved call(s) %v in %s — rejecting", logPaths(introduced), logPath(path))
				// Syntax already passed on these exact bytes at the final-byte
				// check; the structural failure is what refused them, so it is
				// the decisive outcome. The earlier pass is not carried here --
				// it would describe a check that did not make this decision.
				rejection := structuralWriteRejection(path, introduced)
				return &ToolResult{Success: false,
					Error:            rejection,
					MutationStatus:   MutationRefused,
					ValidationKind:   ValidationKindStructural,
					ValidationStatus: ValidationFailed,
					ValidationDetail: rejection}, nil
			}
		}
	}

	// Embedded-script gate: V3 verifies a candidate by RUNNING it — the
	// server starts and the page returns 200 — which is exactly the evidence a
	// broken <script> block inside the rendered HTML survives. Nothing else on
	// this path parses that JavaScript. Same healthy->broken rule; and when the
	// WINNER is the one carrying the break, write the model's gate-passing
	// baseline rather than blaming it for V3-authored content.
	if original, origOK := readOriginalForGate(path); origOK {
		if msg := embeddedScriptGate(ctx, path, original, code); msg != "" {
			if code != baselineContent && embeddedScriptGate(ctx, path, original, baselineContent) == "" {
				// Same rule as the structural revocation: the baseline is
				// observed before it is restored, and the verdict travels with
				// it to the write.
				baseCheck := fallbackSyntaxOutcomeFor(ctx, path, baselineContent).aggregate()
				if baseCheck.Status == ValidationFailed {
					return &ToolResult{Success: false,
						Error:            fallbackSyntaxRejection(path, baselineContent, baseCheck.Detail),
						MutationStatus:   MutationRefused,
						ValidationKind:   ValidationKindSyntax,
						ValidationStatus: ValidationFailed,
						ValidationDetail: baseCheck.Detail}, nil
				}
				log.Printf("[write_file] V3 winner breaks an embedded script in %s — writing gate-passing baseline instead", logPath(path))
				code, authorizedV3, fellBack = revokeV3(
					baselineContent, "winner breaks an embedded script", path)
				deliveredCheck, checkedFor = baseCheck, code
			} else {
				log.Printf("[write_file] embedded-script gate rejected content for %s", logPath(path))
				// Structural, not syntax. A standalone embedded parse failure
				// is already owned by the final-byte check, which asks without
				// a `previous` and would have refused before this gate ran.
				// What survives to here is what only a before/after comparison
				// can see -- a render loop the change stopped driving -- which
				// is a structural regression in code that parses.
				return &ToolResult{Success: false,
					Error:            msg,
					MutationStatus:   MutationRefused,
					ValidationKind:   ValidationKindStructural,
					ValidationStatus: ValidationFailed,
					ValidationDetail: msg}, nil
			}
		}
	}

	// Same entrypoint check every edit path runs. write_file on an existing
	// file is a whole-file replacement, which is exactly how a second
	// `if __name__` block gets appended.
	if original, origOK := readOriginalForGate(path); origOK {
		if msg := duplicateMainGuard(path, original, code); msg != "" {
			log.Printf("[write_file] write duplicates the module entrypoint in %s — rejecting", logPath(path))
			// The file parses; what is wrong is its module-level structure, so
			// this is a structural refusal like the two above.
			return &ToolResult{Success: false,
				Error:            msg,
				MutationStatus:   MutationRefused,
				ValidationKind:   ValidationKindStructural,
				ValidationStatus: ValidationFailed,
				ValidationDetail: msg}, nil
		}
	}

	// The structural gate made HTTP calls; a user cancel during that window
	// must not land content on disk, mirroring the main call's abort path above.
	if ctx.Ctx != nil && ctx.Ctx.Err() != nil {
		log.Printf("[write_file] cancelled during structural gate — not writing %s", path)
		// MutationNone, not Refused: no gate declined anything and no write was
		// attempted -- the turn ended. The observation earned on the current
		// bytes is still true and is carried, but nothing new is evaluated
		// here: cancellation is exactly the point at which no further work may
		// be done. It is overlaid only while it still describes those bytes.
		cancelled := &ToolResult{
			Success:        false,
			Error:          "write_file cancelled — no content was written",
			MutationStatus: MutationNone,
		}
		if checkedFor == code {
			overlayValidation(cancelled, deliveredCheck)
		}
		return cancelled, nil
	}

	// A gate fallback wrote the model's own baseline, which was NOT
	// V3-sandbox-verified (only syntax- and structural-checked). Report it
	// as a plain direct write with no V3 metadata — the same honest
	// reporting as the V3-unavailable fallback — so the vetoed winner's
	// score/phase/evidence don't attach to unverified content and the
	// "V3 verified this edit" completion nudge (agent.go) doesn't fire.
	// The observation that travels to the write must describe the bytes being
	// written. Every transition above re-observes what it changed, so this
	// normally returns what it was handed; it re-evaluates only if a future
	// branch alters the bytes without saying what it found about them, which is
	// the one way this invariant could rot.
	logMandatoryChecks(ctx, "write_file", path, "candidate route")
	final := deliveredCheck
	if checkedFor != code {
		log.Printf("[write_file] final bytes for %s are not the observed ones — re-checking",
			logPath(path))
		final = fallbackSyntaxOutcomeFor(ctx, path, code).aggregate()
	}

	if fellBack || !authorizedV3 {
		// The caller's own bytes are what lands. Any grant minted over them was
		// never owed a delivery, and says so through its own lifecycle.
		markBaselineRetainedGrant(ctx, delivery)
		// WHICH of the two things happened is a different fact, and the
		// vocabulary already has both words for it. Nothing materially
		// different came back, or a gate above withdrew what did -- and only
		// the second had a candidate to name. Recording both as a retained
		// baseline is what made this ending unreadable: an analysis cannot
		// tell a producer that agreed with the caller from one whose winner
		// was revoked, and the hash it joined on belonged to neither.
		ending := routingBaselineRetained
		if candidateProposed {
			ending = routingRevokedByGate
		}
		lifecycle.finish(ctx, ending, proposalIdentity,
			AuthorizationReason(delivery.Refusal))
		// Authorization governs METADATA as well as bytes: reporting
		// V3Used/score/phase/evidence over content V3 did not author is the
		// same false claim in a different field, and it fires the agent's
		// "V3 verified this edit" completion nudge.
		fbRes, fbErr := writeFileRecorded(path, code, ctx)
		out, outErr := applyRouteObservation(fbRes, fbErr, final)
		emitDeliveryProvenance(ctx, path, DeliveryFromModelProposal, policy)
		return withDeliveryProvenance(out, DeliveryFromModelProposal), outErr
	}

	// Stream V3 completion summary — after the gate, so a rejected write
	// doesn't present as a successfully completed V3 stage.
	if ctx.StreamFn != nil {
		ctx.StreamFn("v3_progress", map[string]string{
			"message": fmt.Sprintf("  └──── V3 complete: %s, %d candidates", v3Result.PhaseSolved, v3Result.CandidatesTested),
		})
	}

	// The caller's own bytes, checked before they land, for a delivery that
	// cannot go ahead after the policy said it could.
	writeCallersContent := func() (*ToolResult, error) {
		baseCheck := fallbackSyntaxOutcomeFor(ctx, path, baselineContent).aggregate()
		if baseCheck.Status == ValidationFailed {
			return &ToolResult{Success: false,
				Error:            fallbackSyntaxRejection(path, baselineContent, baseCheck.Detail),
				MutationStatus:   MutationRefused,
				ValidationKind:   ValidationKindSyntax,
				ValidationStatus: ValidationFailed,
				ValidationDetail: baseCheck.Detail}, nil
		}
		fbRes, fbErr := writeFileRecorded(path, baselineContent, ctx)
		out, outErr := applyRouteObservation(fbRes, fbErr, baseCheck)
		emitDeliveryProvenance(ctx, path, DeliveryFromModelProposal, policy)
		return withDeliveryProvenance(out, DeliveryFromModelProposal), outErr
	}

	// THE consumer of the authorization grant, and the only mutation on this
	// route that a typed request can reach. Everything it needs is re-read
	// from disk inside it: the grant froze a moment and this is a later one.
	if delivery.Typed {
		lifecycle.finish(ctx, routingCandidateAuthorized, contentSHA256(code), "")
		result, outcome, err := deliverAuthorizedCandidate(ctx, path, code,
			delivery.Grant, final, delivery.MetCommands, delivery.BaselinePreserved)
		lifecycle.noteDelivery(outcome)
		if err != nil {
			if result == nil && errors.Is(err, errDeliveryUnauthorized) {
				// Refused before any byte moved. The caller's own content is
				// still the alternative, exactly as on the edit route and on
				// every other refusal here. Refusing the whole write told the
				// model its content was kept when nothing had been written.
				log.Printf("[write_file] the candidate was not spendable (%s) — writing the caller's content",
					outcome.Reason)
				return writeCallersContent()
			}
			if result == nil {
				// The write itself failed. The licence was spent on bytes that
				// never reached disk: nothing moved and nothing is claimed.
				return &ToolResult{Success: false,
					Error:            "the file could not be written (" + outcome.Reason + ") — nothing was changed",
					MutationStatus:   MutationRefused,
					ValidationKind:   final.kind(),
					ValidationStatus: final.Status,
					ValidationDetail: final.Detail}, nil
			}
			return result, err
		}
		if !outcome.Delivered {
			// The write happened and the result is not what was authorized.
			// No V3 metadata is attached: reporting a verified delivery over
			// bytes that did not settle is the same false claim in a
			// different field.
			return result, nil
		}
		provenance := deliveryProvenanceFor(policy)
		emitDeliveryProvenance(ctx, path, provenance, policy)
		return withDeliveryProvenance(v3DeliveredResult(ctx, result, v3Result, code),
			provenance), nil
	}

	// Unreachable by an authorized candidate: authorization now requires a
	// grant, and a grant requires declared output knowledge. Kept as the
	// fail-closed floor rather than deleted, because a future policy that
	// authorizes without minting would otherwise write bytes through a path
	// nobody reviewed.
	log.Printf("[write_file] authorized delivery reached the ungranted path for %s — writing the caller's content",
		logPath(path))
	return writeCallersContent()
}

// originalForScope is the artifact as it stands before this call, or "" when
// there is nothing there. Read through the gate's own reader so a file the
// process cannot read is treated as absent rather than as empty.
func originalForScope(path string, ctx *AgentContext) string {
	if original, ok := readOriginalForGate(path); ok {
		return original
	}
	return ""
}

// withDeliveryProvenance labels a result with where its bytes came from.
func withDeliveryProvenance(result *ToolResult, provenance string) *ToolResult {
	if result == nil || !deliveryProvenanceValues[provenance] {
		return result
	}
	result.DeliveryProvenance = provenance
	return result
}

// v3DeliveredResult attaches the pipeline's metadata to a delivery that
// actually happened. One owner for both routes, so the typed and untyped
// deliveries are provably identical in what they report.
func v3DeliveredResult(ctx *AgentContext, result *ToolResult,
	v3Result *V3GenerateResponse, code string) *ToolResult {
	if result == nil || v3Result == nil {
		return result
	}
	out := WriteFileOutput{
		BytesWritten:         len(code),
		V3Used:               true,
		CandidatesTested:     v3Result.CandidatesTested,
		WinningScore:         v3Result.WinningScore,
		PhaseSolved:          v3Result.PhaseSolved,
		VerificationEvidence: v3Result.VerificationEvidence,
	}
	outBytes, _ := json.Marshal(out)
	result.Data = outBytes
	result.V3Used = true
	result.CandidatesTested = v3Result.CandidatesTested
	result.WinningScore = v3Result.WinningScore
	result.PhaseSolved = v3Result.PhaseSolved
	result.VerificationEvidence = v3Result.VerificationEvidence
	return result
}

// ---------------------------------------------------------------------------
// edit_file — old_str/new_str with uniqueness validation
// ---------------------------------------------------------------------------

func editFileTool() *ToolDef {
	return &ToolDef{
		Name:   "edit_file",
		Effect: ToolEffectDirectMutation,
		Description: "SURGICAL inline string replacement, ONLY. Use ONLY when changing a few lines inside a function (a None check, a regex, a constant). " +
			"DO NOT use for whole-function rewrites, whole-class rewrites, whole-file replacements, or any change >10 lines — for those, use structural_edit (named node) or write_file (new file). " +
			"old_str must match exactly once (or replace_all=true). Always read_file before editing. " +
			"Heuristic: if you're tempted to copy a whole function/class/HTML element into old_str, you have the wrong tool — switch to structural_edit.",
		InputSchema: EditFileInput{},
		ReadOnly:    false,
		Destructive: false,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (res *ToolResult, execErr error) {
			var input EditFileInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, errNoMutation(fmt.Errorf("invalid input: %w", err))
			}

			// Reject empty path — same reasoning as read_file.
			if strings.TrimSpace(input.Path) == "" {
				return noMutation("edit_file: path cannot be empty. Use read_file first on the target, then edit_file with the same path."), nil
			}

			// Malformed-args check, BEFORE the read-staleness check. Observed
			// live: the model sent
			//   {"path":"app.py","old_str":"","":"new_str","content":"# hello world"}
			// — an empty old_str, a key literally named "", and write_file's
			// `content` field instead of new_str. It got "file not read yet",
			// which was true and said nothing about the four things wrong with
			// the call, so the next attempt fumbled a different field.
			if strings.TrimSpace(input.OldStr) == "" {
				hint := ""
				if bytes.Contains(rawInput, []byte(`"content"`)) {
					hint = " You sent `content`, which is write_file's field — edit_file does not take it."
				}
				return noMutation("edit_file: old_str is required and cannot be empty." + hint +
					" The call is edit_file {\"path\":..., \"old_str\":<one unique line copied from the file>, " +
					"\"new_str\":<what replaces it>}. To ADD lines rather than replace any, use " +
					"insert_after {\"path\":..., \"line\":<number from read_file>, \"content\":...} — that one does " +
					"take `content`."), nil
			}

			path := resolveAgentPath(ctx, input.Path)

			// Require file was read first (staleness protection)
			if !editViewIsCurrent(ctx, path) {
				return nil, errNoMutation(fmt.Errorf("file not read yet — use read_file first before editing: %s", input.Path))
			}

			// Read current content
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, errNoMutation(fmt.Errorf("cannot read %s: %w", input.Path, err))
			}
			content := string(data)

			// Check for staleness
			if modifiedSinceSessionView(ctx, path, data) {
				return nil, errNoMutation(fmt.Errorf("file modified since last read — read it again before editing: %s", input.Path))
			}

			// Find old_str. Exact bytes first; a match that needed quote-style
			// or whitespace tolerance is allowed but reported, so the model
			// knows which text of the file its new_str replaced.
			actualOldStr := findActualString(content, input.OldStr)
			matchNote := ""
			if actualOldStr != "" && actualOldStr != input.OldStr {
				matchNote = fmt.Sprintf("old_str matched %s only after treating curly and straight quotes as the "+
					"same; the file's own text there was replaced by new_str exactly as sent.",
					lineSpanOf(content, actualOldStr))
			}
			// An old_str that matches only after decoding HTML entities is NOT
			// accepted by decoding new_str to match (GH #39's old shortcut):
			// the replacement may hold entities on purpose (&amp; in markup,
			// or in a Python string), and decoding it wrote different bytes
			// from the ones sent. The targeted refusal below names the
			// entities and the retry.
			if actualOldStr == "" {
				// read_file prints "12<tab>" before each line for reference
				// and the model pastes back what it was shown. The edit used
				// to strip the prefix from old_str and new_str and apply the
				// result, reporting nothing: an unannounced rewrite of both
				// arguments (stabilization cycle 2, smallrung_toml rep 2).
				// old_str and new_str are applied exactly as sent; text that
				// really begins "12<tab>" (tab-separated data) still edits,
				// because it matched above. When only the prefix-free form
				// matches, nothing is changed and the refusal gives the exact
				// replace_lines call that makes this change without
				// reproducing the text: the lines where it matched, their
				// real contents, and a note if new_str carries the prefix too.
				if stripped := stripLineNumberPrefixes(input.OldStr); stripped != input.OldStr {
					if maybeMatch := findActualString(content, stripped); maybeMatch != "" {
						log.Printf("[edit_file] old_str on %s matches only without read_file's line-number prefixes — refused", input.Path)
						return nil, errNoMutation(fmt.Errorf("%s", prefixedEditRefusal(input.Path, content, maybeMatch,
							input.OldStr, input.NewStr)))
					}
				}
			}
			if actualOldStr == "" {
				// Last resort before failing: whitespace-tolerant line match.
				// Small models can't reproduce a code block byte-for-byte —
				// indentation width, tabs-vs-spaces, and trailing spaces drift
				// constantly — so exact old_str matching is the #1 reason
				// edit_file fails for them, which strands the whole V3/lens
				// stack (it only engages after a successful edit). This
				// matches old_str against the file ignoring per-line leading/
				// trailing whitespace, and ONLY accepts a UNIQUE match — an
				// ambiguous or content-different old_str still fails, so it
				// can never silently edit the wrong place. Exact-matching
				// (frontier) models never reach this path. See GH #39.
				if fuzzy, ok := findFuzzyLineMatch(content, input.OldStr); ok {
					log.Printf("[edit_file] exact old_str missed on %s; unique whitespace-tolerant match found — proceeding (small-model indentation drift)", input.Path)
					actualOldStr = fuzzy
					matchNote = fmt.Sprintf("old_str matched %s only after ignoring leading and trailing whitespace "+
						"on each line; those lines were replaced by new_str exactly as sent.", lineSpanOf(content, fuzzy))
				}
			}
			if actualOldStr == "" {
				// Degenerate old_str: not drift the fuzzy matcher can rescue,
				// but corrupted output. Observed across three sessions —
				// runs of bare \r and a stray `\rVert` (a LaTeX fragment) in
				// the middle of copied code. "Must match byte-for-byte" is
				// useless advice for that, and the model re-sent an equally
				// corrupted block each time. Name it, and ask for the short
				// anchor that is far less likely to degenerate.
				// A long multi-line anchor is a transcription burden, and this
				// model corrupts under it: asked to reproduce a ten-line block
				// it emitted "safe_load_aller" for "safe_load_all". One line
				// is enough to locate an edit, and both the tool description
				// and the structural_edit steer already say so — repeat it
				// here, where the failure actually happened.
				// Checked before everything: one corrupted character is
				// invisible next to a long old_str, and every other hint here
				// sends the model to re-copy text it already copied right.
				if bad := foreignRunes(input.OldStr, content); len(bad) > 0 {
					return nil, errNoMutation(fmt.Errorf("string to replace not found in file. Your `old_str` contains %s, "+
						"which appears nowhere in %s. That is a corrupted character, not a mis-copy — most often "+
						"an operator that decoded wrong (`&&`, `||`, `>=`, `->`). Re-read the line from the file "+
						"and re-emit `old_str` with plain ASCII operators.\nSearched for: %s",
						describeForeignRunes(bad), input.Path, truncateStr(input.OldStr, 200)))
				}
				// Checked next because it is the most specific remaining: the
				// text is otherwise correct and only carries read_file's
				// display prefix. Saying "not found" here sends the model back to
				// re-copy a line it already copied right.
				if n := lineNumberPrefixedLines(input.OldStr); n > 0 {
					return nil, errNoMutation(fmt.Errorf("string to replace not found in file. Your `old_str` still has "+
						"read_file's line-number prefix on %d line(s) — the \"12<tab>\" at the start is "+
						"added for reference and is NOT in the file. Send the line text only, starting at "+
						"the first real character. If you are ADDING lines rather than changing one, "+
						"insert_after takes that number directly and needs no old_str at all.\nSearched for: %s",
						n, truncateStr(input.OldStr, 200)))
				}
				if lines := strings.Count(input.OldStr, "\n") + 1; lines >= 5 {
					return nil, errNoMutation(fmt.Errorf("string to replace not found in file. Your `old_str` is %d lines "+
						"long — reproducing that much text byte-for-byte is where these edits go wrong. "+
						"Use `replace_lines` instead: give the start and end line numbers read_file "+
						"printed and assert only the FIRST and LAST line of that range, so there is no "+
						"multi-line block to reproduce. Or anchor edit_file on ONE short line that "+
						"appears exactly once and put the whole replacement in `new_str`. If you are "+
						"ADDING code rather than replacing it, use insert_after with the line number "+
						"read_file printed — it needs no old_str at all.\nSearched for: %s",
						lines, truncateStr(input.OldStr, 200)))
				}
				if n := strayCarriageReturns(input.OldStr); n >= 3 {
					return nil, errNoMutation(fmt.Errorf("string to replace not found in file. Your `old_str` "+
						"contains %d stray carriage returns and looks corrupted rather than copied "+
						"— long blocks tend to come out this way. Use `replace_lines` on the line "+
						"range instead, or re-emit `old_str` as ONE short unique line taken from the "+
						"file (the single line you are changing), not a multi-line block", n))
				}
				// Mismatch persists — return targeted error.
				hasEntities := strings.Contains(input.OldStr, "&lt;") ||
					strings.Contains(input.OldStr, "&gt;") ||
					strings.Contains(input.OldStr, "&amp;")
				literalsOnDisk := strings.ContainsAny(content, "<>&")
				if hasEntities && literalsOnDisk {
					alt := ""
					if guide := selectorGuidance(input.Path, content); guide != "" {
						alt = " For whole-element rewrites, structural_edit is the cleaner option — it takes a selector and the new content body, no old_str needed; " + guide + "."
					}
					return nil, errNoMutation(fmt.Errorf("string to replace not found in file. Your `old_str` contains HTML-entity-encoded characters (`&lt;` / `&gt;` / `&amp;`) but the file on disk has literal `<` / `>` / `&`. Re-emit `old_str` with literal angle brackets — JSON strings should contain literal `<` not `&lt;`.%s\nSearched for: %s",
						alt, truncateStr(input.OldStr, 200)))
				}
				// Generic mismatch — the model's old_str doesn't byte-match
				// the file (whitespace, quotes, or paraphrase drift, which
				// smaller models do constantly). For structured files,
				// structural_edit sidesteps the whole problem: it selects the node
				// by name, no old_str to reproduce exactly. Steer there.
				astAlt := ""
				if guide := selectorGuidance(input.Path, content); guide != "" {
					astAlt = " To replace a whole function/class/element without " +
						"matching exact text, use structural_edit with a selector and the " +
						"new content — no old_str needed, so a near-miss can't fail it; " +
						guide + "."
				}
				// Ground the retry in the file's REAL content. A small model
				// frequently writes old_str from its memory of the file
				// rather than the file itself (observed: old_str
				// `item = items[id + 1]` for a file whose actual line is
				// `return jsonify(items[item_id + 1])`). Without an anchor it
				// gives up on the surgical edit and rewrites the whole node
				// from the same faulty memory. Quoting the closest actual
				// line gives it real bytes to copy into the next attempt.
				hint := closestLineHint(content, input.OldStr)
				return nil, errNoMutation(fmt.Errorf("string to replace not found in file. old_str must match the file byte-for-byte (whitespace and quotes included).%s%s\nSearched for: %s", astAlt, hint, truncateStr(input.OldStr, 200)))
			}

			// Check uniqueness
			count := strings.Count(content, actualOldStr)
			if count > 1 && !input.ReplaceAll {
				return nil, errNoMutation(fmt.Errorf("found %d matches of the string to replace. Set replace_all=true to replace all, or provide more context to uniquely identify the instance", count))
			}

			// No-op check. The bare diagnosis was a dead end: the model
			// copies old_str into new_str precisely because it cannot
			// reproduce a span with a change applied, so telling it only
			// that the two match gives it nothing to do differently and it
			// re-sends the identical call until the loop breaker kills the
			// session. Measured on a "build me a snake game" run: refused at
			// turn 11, re-sent at 12 and 13, session dead at 757s with
			// game.js half-written. Name the tool that removes the
			// requirement it just failed.
			if input.OldStr == input.NewStr {
				alt := "`replace_lines` with the line numbers read_file printed — you assert only the FIRST and LAST line of the range, so there is no span to reproduce"
				if ext := strings.ToLower(filepath.Ext(input.Path)); ext == ".py" || ext == ".html" || ext == ".htm" {
					alt = "`structural_edit` with a selector (e.g. `function:update`) and the new body — it needs no old_str at all, so there is nothing to copy"
				}
				// The alternative it is being sent to needs line numbers, and
				// "the line numbers read_file printed" may be many turns and
				// several successful edits old. Measured (cycle 7 audit,
				// family O): refused at turns 11, 14 and 16 against a file
				// last read at turn 5, alternating with a structural_edit
				// that changed nothing, until the run stopped on repeated
				// refusals. Where those bytes sit RIGHT NOW is something this
				// call already knows, so it is said here.
				return nil, errNoMutation(fmt.Errorf(
					"old_str and new_str are identical, so this edit would change nothing. "+
						"Re-sending it will not help. You are being asked to reproduce a span "+
						"verbatim AND change it, which is what just failed — use %s.%s "+
						"If you meant to REPLACE the whole file, use write_file with the "+
						"complete new contents", alt, currentSpanNote(input.Path, content, actualOldStr)))
			}

			// Sanitise the replacement string before splicing it in. The
			// model occasionally fences the new_str ("```python\n...\n```")
			// even though it's a fragment, not a whole file. If we let
			// that slip through, every line of the edit would have a
			// stray ``` at the top and bottom.
			if cleanedNew, sanitized := sanitizeFileContent(input.Path, input.NewStr); sanitized {
				log.Printf("[edit_file] removed a whole-content fence wrapper from new_str of %s", input.Path)
				input.NewStr = cleanedNew
				defer func() { noteContentChange(res, wholeFileWrapperNote) }()
			}

			if matchNote != "" {
				note := matchNote
				defer func() { noteContentChange(res, note) }()
			}

			var newContent string
			if input.ReplaceAll {
				newContent = strings.ReplaceAll(content, actualOldStr, input.NewStr)
			} else {
				newContent = strings.Replace(content, actualOldStr, input.NewStr, 1)
			}

			// Shrinkage guard — same shape as structural_edit's. Catches the
			// "model emitted a stub for old_str" failure where new_str
			// is implausibly tiny for a substantial old_str. We compare
			// the local replacement footprint (old_str → new_str), not
			// whole-file size, so refactors that genuinely shrink the
			// file aren't false-rejected.
			if rejection := validateNotSuspiciouslyShrunk("edit_file", input.Path, len(actualOldStr), len(input.NewStr)); rejection != "" {
				log.Printf("[edit_file] rejecting suspicious shrinkage: %s old_str=%dB new_str=%dB",
					input.Path, len(actualOldStr), len(input.NewStr))
				// The shrinkage heuristic declining bytes that were formed.
				return refusedNoCheck(rejection), nil
			}

			// No-op guard — same rationale as structural_edit's. new_str identical
			// to old_str (or a replacement that leaves the file unchanged)
			// must not report success: the model believes the fix landed
			// and moves on while the bug is still on disk.
			if newContent == content {
				log.Printf("[edit_file] no-op edit rejected for %s — file content unchanged", input.Path)
				return noMutation("edit_file: new_str is identical to old_str — nothing was changed and the bug is still there. " +
					"Look at the current code again and emit a new_str that actually differs from the existing code."), nil
			}

			// Already-applied guard. The check above only catches an edit that
			// leaves the file byte-identical; it cannot see an edit that
			// applied once and is now being applied AGAIN. A whitespace-only
			// change is the dangerous shape: after " x" replaces "x", the
			// original old_str still matches as a substring, so the same call
			// keeps succeeding and keeps adding whitespace. The repeat
			// detector does not help — it needs three occurrences, and two
			// runs of a 75-second edit is already two minutes gone.
			editKey := input.Path + "\x00" + input.OldStr + "\x00" + input.NewStr
			if ctx.AppliedEdits[editKey] {
				log.Printf("[edit_file] duplicate edit rejected for %s — already applied this session", input.Path)
				return refusedNoCheck("edit_file: this exact edit already succeeded earlier in this " +
					"session, so applying it again would just repeat it — and when the change is whitespace-only, the " +
					"old_str still matches afterwards, so it can repeat forever. The file already has this change. " +
					"Read the file to see its current state, then either make a DIFFERENT edit or declare done."), nil
			}

			// Route through V3 pipeline when the file warrants it. The
			// gate now mirrors write_file (file-tier only, no request-tier
			// AND-gate) — having two separate tier checks meant V3 only
			// Same pipeline entry every content edit uses; see
			// runEditPipeline for why it is shared rather than inlined.
			route := runEditPipeline(ctx, "edit_file", path, input.Path, content, newContent)
			if route.Cancelled != nil {
				return route.Cancelled, nil
			}
			if route.Delivered {
				// The candidate landed through the protected chain. Writing the
				// caller's own edit now would undo a delivery that settlement
				// and restoration already decided about.
				return attachV3(route.Result, route.Meta), nil
			}
			v3Out := route.Meta
			newContent = route.Content

			// Syntax gate on the composed result. A truncated new_str (or a
			// string-level edit that broke the file) must not land when V3
			// verification didn't run or failed — same class as the
			// write_file fallback gate (mini-bench t06/t09). Semantics:
			// an edit must not INTRODUCE breakage — when the ORIGINAL
			// content already fails to parse, a still-failing result is a
			// permitted repair-in-progress (fixing one error at a time),
			// so the gate only blocks healthy→broken transitions.
			observed, refusal := editSyntaxObservation(ctx, "edit_file", path, input.Path,
				content, newContent, func(detail string) string {
					// An embedded-script finding is already model-ready and names
					// its own fix; the generic wrapper's "check old_str/new_str"
					// advice is wrong for JavaScript inside a string.
					if msg, isEmbedded := embeddedScriptRejectionFor(detail); isEmbedded {
						return msg
					}
					return fmt.Sprintf(
						"edit_file result for %s does not parse (%s). The file was NOT modified — check that old_str/new_str are complete and re-issue the edit.",
						input.Path, truncateStr(detail, 200))
				})
			if refusal != nil {
				return refusal, nil
			}

			// Structural gate (#147): a parse-clean edit can still introduce
			// an unresolved direct call (render_template with only
			// render_template_string imported) that 500s at runtime. Block a
			// write that NEWLY makes a name unresolved; a pre-existing one
			// (mid-repair) is allowed, mirroring the syntax gate above.
			if introduced := editIntroducesUnresolved(ctx, path, content, newContent); len(introduced) > 0 {
				log.Printf("[edit_file] edit introduces unresolved call(s) %v in %s — rejecting", logPaths(introduced), logPath(input.Path))
				return structuralRefusal(structuralRejection(input.Path, introduced)), nil
			}
			// edit_file ran checkFallbackSyntax, which parses the embedded
			// script but has no pre-edit file to compare against, so the
			// comparative findings — a render loop that stopped repeating, a
			// lexical binding declared twice — were never checked here. Every
			// other edit path had them. Observed: the same one-shot
			// setTimeout the gate refuses under replace_lines landed through
			// edit_file, and the page returned 200 with a dead game.
			if msg := embeddedScriptGate(ctx, path, content, newContent); msg != "" {
				log.Printf("[edit_file] edit breaks an embedded script in %s — rejecting", logPath(input.Path))
				return structuralRefusal(msg), nil
			}
			if msg := duplicateMainGuard(path, content, newContent); msg != "" {
				log.Printf("[edit_file] edit duplicates the module entrypoint in %s — rejecting", logPath(input.Path))
				return structuralRefusal(msg), nil
			}
			logMandatoryChecks(ctx, "edit_file", input.Path, "")

			// Atomic write
			tmpPath := path + ".atlas.tmp"
			if err := os.WriteFile(tmpPath, []byte(newContent), 0644); err != nil {
				return nil, editWriteFailure(path,
					fmt.Errorf("cannot write %s: %w", input.Path, err), observed)
			}
			if err := os.Rename(tmpPath, path); err != nil {
				os.Remove(tmpPath)
				return nil, editWriteFailure(path,
					fmt.Errorf("cannot rename temp file: %w", err), observed)
			}

			// Update cached state with whatever was actually written
			ctx.RecordFileRead(path, newContent)
			ctx.RecordBodySeen(path)

			// Build diff preview against the original on-disk content
			oldLines := strings.Count(input.OldStr, "\n") + 1
			newLines := strings.Count(input.NewStr, "\n") + 1
			preview := buildDiffPreview(content, newContent, actualOldStr, input.NewStr)

			out := EditFileOutput{
				OK:           true,
				DiffPreview:  preview,
				LinesAdded:   newLines - oldLines,
				LinesRemoved: 0,
			}
			if newLines < oldLines {
				out.LinesRemoved = oldLines - newLines
				out.LinesAdded = 0
			}

			outBytes, _ := json.Marshal(out)
			// Record it so an identical re-application is refused above.
			if ctx.AppliedEdits != nil {
				ctx.AppliedEdits[editKey] = true
			}
			result := &ToolResult{Success: true, Data: outBytes,
				MutationStatus: MutationApplied}
			// The observation describes exactly the bytes just written --
			// the tool's own edit, an authorized V3 candidate, or a baseline
			// restored after revocation, whichever runEditPipeline returned.
			// Provenance is a separate fact and is attached below.
			overlayValidation(result, observed)
			if v3Out.Used {
				result.V3Used = true
				result.CandidatesTested = v3Out.CandidatesTested
				result.WinningScore = v3Out.WinningScore
				result.PhaseSolved = v3Out.PhaseSolved
				result.VerificationEvidence = v3Out.VerificationEvidence
			}
			return result, nil
		},
	}
}

// ---------------------------------------------------------------------------
// structural_edit — GH #39 v1: friendly-selector named-node replacement
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
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (res *ToolResult, execErr error) {
			var input StructuralEditInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, errNoMutation(fmt.Errorf("invalid input: %w", err))
			}
			if strings.TrimSpace(input.Path) == "" {
				return noMutation("structural_edit: path cannot be empty. Read the file first then structural_edit with the same path."), nil
			}
			if strings.TrimSpace(input.Selector) == "" {
				return noMutation("structural_edit: selector cannot be empty. Examples: function:dashboard, class:UserModel, <body>"), nil
			}

			path := resolveAgentPath(ctx, input.Path)
			if !editViewIsCurrent(ctx, path) {
				return nil, errNoMutation(fmt.Errorf("file not read yet — use read_file first before structural_edit: %s", input.Path))
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return nil, errNoMutation(fmt.Errorf("cannot read %s: %w", input.Path, err))
			}
			source := string(data)

			// Empty-content guard. Replacing a node with nothing is a
			// deletion, not an edit — observed live: a model called structural_edit
			// with the `content` field omitted entirely, which spliced an
			// empty string over `function:add` and silently deleted it
			// (calc.py lost both functions while __main__ still called
			// them). It passes the syntax gate (the file still parses) and
			// the no-op guard (the content did change), so nothing else
			// catches it. Refuse it and steer: an edit needs a replacement
			// body; an intentional removal is delete_file's job.
			if strings.TrimSpace(input.Content) == "" {
				log.Printf("[structural_edit] rejected empty content for %s selector=%q — would delete the node", input.Path, input.Selector)
				return noMutation(fmt.Sprintf(
					"structural_edit: content is empty — that would DELETE `%s`, not fix it. "+
						"Provide the full replacement body of the node (e.g. the corrected function definition). "+
						"If you truly mean to remove code, use delete_file on the whole file instead.",
					input.Selector)), nil
			}

			// Runaway-content guard. structural_edit replaces ONE node, so the
			// replacement should be roughly node-sized. A reasoning-heavy
			// model sometimes leaks its entire chain-of-thought into the
			// content field instead of emitting just the new body — observed
			// live: a 69KB "content" for a 3-line function, full of
			// "# Wait, the user said..." commentary. That blob then ships to
			// disk and, being huge, trips downstream size heuristics (it once
			// re-triggered a 23-min V3 PlanSearch). Reject when the
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
					len(input.Content), len(source), input.Selector)), nil
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

			// HTML <html>-selector quirk. structural_edit replaces only the
			// <html>...</html> element, NOT the preceding <!DOCTYPE>
			// declaration that conventionally precedes it. The model
			// frequently emits a leading <!DOCTYPE html> at the top of
			// `content` when selector is <html>, which produces a duplicated
			// doctype on disk (May 8 2026 flask test: dashboard.html
			// ended up with two consecutive <!DOCTYPE html> lines after
			// a successful structural_edit). Detect that shape and strip the
			// leading doctype line so on-disk output matches intent.
			ext := strings.ToLower(filepath.Ext(input.Path))
			isHTML := ext == ".html" || ext == ".htm"
			if isHTML && strings.EqualFold(strings.TrimSpace(input.Selector), "<html>") {
				if stripped, ok := stripLeadingDoctype(input.Content); ok {
					log.Printf("[structural_edit] stripped leading <!DOCTYPE> from content of %s — selector <html> only replaces the html element, not the preceding doctype", input.Path)
					input.Content = stripped
				}
			}

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
				return nil, errNoMutation(fmt.Errorf("structural_edit unavailable: V3 service URL not configured"))
			}
			req, err := http.NewRequestWithContext(ctx.Ctx, "POST", v3URL+"/internal/structural_edit", bytes.NewReader(reqBody))
			if err != nil {
				return nil, errNoMutation(fmt.Errorf("structural_edit: build request: %w", err))
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return nil, errNoMutation(fmt.Errorf("structural_edit: v3-service unreachable: %w", err))
			}
			defer resp.Body.Close()
			respBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				return nil, errNoMutation(fmt.Errorf("structural_edit: read v3 response: %w", err))
			}
			var astResp struct {
				Success    bool   `json:"success"`
				Error      string `json:"error,omitempty"`
				Language   string `json:"language,omitempty"`
				NewContent string `json:"new_content,omitempty"`
				ByteRange  []int  `json:"byte_range,omitempty"`
				OldSize    int    `json:"old_size,omitempty"`
				NewSize    int    `json:"new_size,omitempty"`
			}
			if err := json.Unmarshal(respBytes, &astResp); err != nil {
				return nil, errNoMutation(fmt.Errorf("structural_edit: parse v3 response: %w (body=%s)", err, truncateStr(string(respBytes), 200)))
			}
			if !astResp.Success {
				// The selector matched nothing, or the splice would not parse:
				// either way no replacement content came back to write.
				return noMutation(astResp.Error + moduleLevelContentNote(ctx, input.Path, input.Content)), nil
			}

			// Shrinkage guard — catch the May 9 2026 destructive-stub bug
			// where the model emits only "<!DOCTYPE html>\n" for an entire
			// <html>-element rewrite. astResp.OldSize is the original
			// node's bytes; astResp.NewSize is the replacement bytes. If
			// the replacement is suspiciously small for the original,
			// reject the write and tell the model to re-emit with the
			// full body.
			if rejection := validateNotSuspiciouslyShrunk("structural_edit", input.Path, astResp.OldSize, astResp.NewSize); rejection != "" {
				log.Printf("[structural_edit] rejecting suspicious shrinkage: %s old=%dB new=%dB selector=%q",
					input.Path, astResp.OldSize, astResp.NewSize, input.Selector)
				return refusedNoCheck(rejection), nil
			}

			// V3 quality-gate routing. History:
			//   (a) May 10: tier classified on post-edit content only, so a
			//       destructive structural_edit that shrank a T2+ file into a stub
			//       classified T1 and skipped V3 — the edits that most need
			//       checking. Fixed by classifying on max(oldTier, newTier).
			//   (b) May 10: floor dropped entirely so V3 fired on every
			//       structural_edit.
			//   (c) Jun 8: floor restored to Tier2Medium. With (b), every
			//       one-line structural_edit ran the full PlanSearch pipeline —
			//       minutes per edit on a reasoning-heavy model, blocking the
			//       single-threaded v3-service and looking like a hang. But
			//       structural_edit is ALREADY surgical: the model named the exact
			//       node and the replacement is its own tree-sitter
			//       transform. PlanSearch-improving a precise node swap is
			//       mostly cost. Gate it to T2+ files (same as edit_file /
			//       write_file): trivial edits apply instantly, V3 still
			//       engages where the file is genuinely complex. max-tier
			//       from (a) is preserved, so a destructive edit to a T2+
			//       original still triggers V3.
			//
			// Baseline candidate is the structurally edited full file. V3's
			// alternatives compete against it; if one build-verifies
			// better, V3 wins; otherwise the structurally edited content passes
			// through unchanged. Either way the answer is build-verified.
			finalContent := astResp.NewContent

			// No-op guard. A weak model frequently "fixes" a bug by
			// re-emitting the node's existing (broken) code verbatim —
			// observed live: structural_edit function:add with content identical
			// to the buggy body, twice in one batch. Reporting success on
			// a no-op tells the model the fix landed when nothing changed;
			// it then moves on to verification, fails, and can't work out
			// why. Fail loudly instead so the model re-derives the edit.
			if finalContent == source {
				log.Printf("[structural_edit] no-op edit rejected for %s selector=%q — replacement identical to existing code", input.Path, input.Selector)
				return noMutation(fmt.Sprintf(
					"structural_edit: your replacement for `%s` is IDENTICAL to the code already in the file — nothing was changed and the bug is still there. "+
						"Look at the current code again and emit a replacement that actually differs (for a swapped-operator bug, the operator itself must change).",
					input.Selector)), nil
			}
			v3Out := V3EditMetadata{}
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
			// execution. The write path has had this fast-track since PC-190;
			// the edit path never consulted it. Measured: three 900s timeout
			// deaths whose sessions each paid one full V3 run and then
			// V3-improve on every corrective edit until the clock died.
			structuralBypass := editGenerationBypass(ctx, fileTier,
				editWarrantsV3(finalContent, cc, ccOK),
				isActiveDebugIteration(ctx, input.Path))
			recordCandidateGenerationBypass(ctx, "structural_edit", structuralBypass,
				fileTier, strings.Count(finalContent, "\n")+1)
			logBudgetBypass("structural_edit", input.Path, structuralBypass)
			if structuralBypass == bypassNone {
				log.Printf("[structural_edit] V3 pipeline activating for %s (oldTier=%d newTier=%d max=%d, req_tier=%d, cc=%d) post-structural-edit", input.Path, oldTier, newTier, fileTier, ctx.Tier, cc)
				// THE protected edit route, same owner as the other three edit
				// tools: a service proposal reaches disk only through the
				// evidence, authorization, one-time grant, exact-byte delivery,
				// ledger, validation and settlement chain, and otherwise the
				// caller's own structurally edited content is what stays.
				route := deliverEditCandidate(ctx, "structural_edit", path,
					input.Path, source, finalContent)
				if route.Cancelled != nil {
					return noMutation("structural_edit cancelled — no content was written"), nil
				}
				if route.Delivered {
					// The candidate landed through the protected chain.
					// Writing the caller's own content now would undo a
					// delivery settlement and restoration already decided.
					return attachV3(route.Result, route.Meta), nil
				}
				finalContent = route.Content
				v3Out = route.Meta
			}

			// Structural gate (#147): the structural splice guarantees the result
			// parses, but not that its calls resolve — the observed failure
			// was a structural_edit that introduced a render_template call with only
			// render_template_string imported, which landed as verified and
			// 500'd every request. Block a write that NEWLY makes a direct
			// call unresolved (healthy->broken); a pre-existing one is left
			// alone for repair-in-progress. Fail-open when the check can't run.
			if introduced := editIntroducesUnresolved(ctx, path, source, finalContent); len(introduced) > 0 {
				log.Printf("[structural_edit] edit introduces unresolved call(s) %v in %s — rejecting", logPaths(introduced), logPath(input.Path))
				return structuralRefusal(structuralRejection(input.Path, introduced)), nil
			}

			// Embedded-script gate: the post-splice check in v3-service
			// proves the .py/.html parses, not that the JavaScript inside a
			// <script> block does — and `<script>` is a first-class selector
			// here, so this tool is the likeliest way to land broken JS.
			// Healthy->broken, fail-soft.
			if msg := embeddedScriptGate(ctx, path, source, finalContent); msg != "" {
				log.Printf("[structural_edit] edit breaks an embedded script in %s — rejecting", logPath(input.Path))
				return structuralRefusal(msg), nil
			}
			if msg := duplicateMainGuard(path, source, finalContent); msg != "" {
				log.Printf("[structural_edit] edit duplicates the module entrypoint in %s — rejecting", logPath(input.Path))
				return structuralRefusal(msg), nil
			}
			logMandatoryChecks(ctx, "structural_edit", input.Path, "")

			// Atomic write — same pattern as edit_file/write_file.
			tmpPath := path + ".atlas.tmp"
			if err := os.WriteFile(tmpPath, []byte(finalContent), 0644); err != nil {
				return nil, errFailedMutationSyntaxUnrun(fmt.Errorf("cannot write %s: %w", input.Path, err))
			}
			if err := os.Rename(tmpPath, path); err != nil {
				os.Remove(tmpPath)
				return nil, errFailedMutationSyntaxUnrun(fmt.Errorf("cannot rename temp file: %w", err))
			}
			ctx.RecordFileRead(path, finalContent)
			ctx.RecordBodySeen(path)

			log.Printf("[structural_edit] %s %s selector=%q lang=%s old=%dB new=%dB v3=%v",
				input.Path, input.Selector, input.Selector, astResp.Language, astResp.OldSize, len(finalContent), v3Out.Used)

			out := StructuralEditOutput{
				OK:       true,
				Selector: input.Selector,
				Language: astResp.Language,
				BytesOld: astResp.OldSize,
				BytesNew: len(finalContent),
			}
			// The splice landed. Validation used to stay not_run, on sound
			// reasoning: the tree-sitter transform only proves v3-service
			// could re-parse ITS output, this tool ran no check of its own on
			// the bytes it writes -- which may be a V3 replacement rather
			// than the splice -- and reading the service's ok boolean as a
			// syntax pass is exactly the inference overlayValidation exists
			// to prevent.
			//
			// The conclusion was right and the premise was fixable: run the
			// check. not_run is not a neutral answer at the exit. A path can
			// only retire its mutation debt on a verdict about its CURRENT
			// bytes, so every file edited this way carried debt that nothing
			// could ever discharge. Measured on offbyone: the run reproduced
			// the bug, fixed chunks() with structural_edit, re-ran its
			// reproduction and printed the corrected output -- and still
			// ended `unresolved_mutation_debt`, told that chunk.py "was never
			// written in a state this run could check". It had been.
			//
			// The bytes still land either way; this tool has never refused
			// after the rename and does not start now. What changes is that
			// the verdict is real, so a clean splice settles and a broken one
			// is recorded as broken instead of as unknown.
			spliceCheck := fallbackSyntaxOutcomeFor(ctx, input.Path, finalContent).aggregate()
			if spliceCheck.Status == ValidationFailed {
				log.Printf("[structural_edit] %s landed but does not parse (%s)",
					logPath(input.Path), safeDiagnosticSummary(spliceCheck.Detail))
				out.Warning = fmt.Sprintf(
					"the edit landed, but %s does not parse now (%s). Run it and "+
						"read the real traceback, then fix that line.",
					input.Path, truncateStr(spliceCheck.Detail, 160))
			}
			outBytes, _ := json.Marshal(out)
			result := &ToolResult{Success: true, Data: outBytes,
				MutationStatus:   MutationApplied,
				ValidationKind:   ValidationKindSyntax,
				ValidationStatus: ValidationNotRun}
			overlayValidation(result, spliceCheck)
			if v3Out.Used {
				result.V3Used = true
				result.CandidatesTested = v3Out.CandidatesTested
				result.WinningScore = v3Out.WinningScore
				result.PhaseSolved = v3Out.PhaseSolved
				result.VerificationEvidence = v3Out.VerificationEvidence
			}
			return result, nil
		},
	}
}

// V3EditMetadata captures what V3 did to an edit_file request, so the
// edit_file result can carry the same v3_used / candidates_tested fields
// write_file does.
type V3EditMetadata struct {
	Used                 bool
	CandidatesTested     int
	WinningScore         float64
	PhaseSolved          string
	VerificationEvidence []V3VerificationEvidence
	// Envelope is the service's own evidence record for the proposal, carried
	// so the route that decides can read its ranking signals. Advisory only:
	// nothing in it authorizes a delivery on any route.
	Envelope *V3EvidenceEnvelope
}

// improveContentWithV3 sends content through the V3 pipeline and returns
// V3's chosen code (baseline candidate or a better-scoring alternative).
// On error, returns "" + zero metadata; the caller should fall back to
// writing the original content.

// proposedV3Candidate is the PROPOSAL boundary: what the service offered, and
// whether it is materially different from what the caller wrote.
//
// It answers one question and deliberately not the other. Whether a candidate
// may LAND is a decision this machine makes, from evidence it produced itself,
// against the policy the client or the operator declared -- and it cannot make
// that decision about bytes it never received. Collapsing an uncertified
// proposal to the baseline here is what put the trusted producer on the far
// side of a gate that needed its output: staging runs the client's declared
// command against the candidate, so a candidate discarded before staging can
// never be the thing that command is run against.
//
// The service's own verdict travels with the bytes as advisory metadata. It
// ranks; it does not authorize. Nothing downstream reads it as permission, and
// no field of it mints a grant.
//
// A proposal is still refused outright for the things that make bytes unusable
// rather than unproven: no response, no code, or code identical to the
// caller's own, which is not a proposal at all.
func proposedV3Candidate(result *V3GenerateResponse, baseline string) (string, bool) {
	if result == nil || strings.TrimSpace(result.Code) == "" {
		// No bytes, or nothing but whitespace. Not a proposal: unusable is a
		// different thing from unproven, and only the second is worth staging.
		return baseline, false
	}
	if result.Code == baseline {
		// The service agreed with the caller. There is nothing to decide, no
		// licence to mint, and no delivery to describe.
		return baseline, false
	}
	return result.Code, true
}

// revokeV3 restores the caller's baseline AND withdraws V3 provenance in one
// step.
//
// These moved independently before: the language-swap gate reset the bytes
// to baseline but left the proposal live and fellBack false, so the final
// check attached V3Used, phase, score and verification evidence to content
// V3 had not authored -- and fired the "V3 verified this edit" nudge over it.
// Provenance describes the FINAL bytes, not the initial response, so it is
// revocable and every restoring gate must revoke it. Returning all three
// values together makes it impossible for a future gate to update one
// without the others.
func revokeV3(baseline, reason, path string) (string, bool, bool) {
	log.Printf("[write_file] V3 provenance withdrawn for %s — %s", logPath(path), reason)
	return baseline, false, true // content, live proposal, fellBack
}

// envelopeOf is the service's evidence record, or nil. Read for advisory
// signals only; nothing downstream may treat it as permission.
func envelopeOf(result *V3GenerateResponse) *V3EvidenceEnvelope {
	if result == nil {
		return nil
	}
	return result.Evidence
}

// There is no service-certification path, and its absence is the point.
//
// A contractless request used to deliver on the service's own closure verdict,
// which is the producer of a candidate certifying that candidate. It read as a
// compatibility rule and it was a self-certification bypass: no target the
// client named, no obligation, no floor, and an authority that came from the
// side being checked. Nothing replaced it, because nothing should. Such a
// request retains the model's own bytes under strict, and the structured
// mutation scope its tool call defines is what a calibrated advisory decision
// would later bound a licence to -- WHERE, never whether.

func improveContentWithV3(path, content string, ctx *AgentContext) (string, V3EditMetadata, error) {
	req := V3GenerateRequest{
		FilePath:     path,
		BaselineCode: content,
		Tier:         int(ctx.Tier),
		WorkingDir:   ctx.WorkingDir,
		UserMessage:  latestUserMessage(ctx),
	}
	// Exclude the target's own pre-edit snapshot from project context —
	// same rule (and reason) as writeFileWithV3 / checkStructuralUnresolved.
	cleanTarget := filepath.Clean(path)
	if filesRead := ctx.SnapshotFilesRead(); len(filesRead) > 0 {
		req.ProjectContext = make(map[string]string)
		for p, c := range filesRead {
			if filepath.Clean(p) == cleanTarget {
				continue
			}
			rel, _ := filepath.Rel(ctx.WorkingDir, p)
			if rel == "" {
				rel = p
			}
			if len(c) > 4000 {
				c = c[:4000] + "\n... (truncated)"
			}
			req.ProjectContext[rel] = c
		}
	}
	if ctx.Project != nil {
		req.Framework = ctx.Project.Framework
		req.BuildCommand = ctx.Project.BuildCommand
	}

	// Same callback logic as the write_file V3 path: tokens forward to
	// the dedicated v3_token SSE event so the TUI updates one streaming
	// row instead of spawning a chat row per token; LLM-call boundaries
	// match the chat protocol's start/end shapes; structured stages
	// emit typed events (v3_phase, v3_sandbox, etc.); only truly
	// unknown stages fall back to the v3_progress text line. Without
	// this branching, edit_file with V3 floods the chat pane with
	// thousands of "[token] X" rows during a single candidate generation.
	v3Result, err := callV3GenerateStreaming(ctx.Ctx, ctx.V3URL, req, func(stage, detail string, data map[string]interface{}) {
		if ctx.StreamFn == nil {
			return
		}
		if stage == "token" {
			ctx.StreamFn("v3_token", map[string]string{"text": detail})
			return
		}
		// Reasoning deltas from V3's LLM calls (see write_file path's
		// matching branch). Same purpose: visible thinking stream
		// during long PlanSearch / repair phases. v3_reasoning_token,
		// not reasoning_token, so it targets the V3 row not the agent row.
		if stage == "reasoning_token" {
			ctx.StreamFn("v3_reasoning_token", map[string]string{"text": detail})
			return
		}
		if stage == "llm_start" {
			payload := map[string]interface{}{"detail": detail}
			for k, v := range data {
				payload[k] = v
			}
			ctx.StreamFn("v3_llm_start", payload)
			return
		}
		if stage == "llm_end" {
			payload := map[string]interface{}{"detail": detail}
			for k, v := range data {
				payload[k] = v
			}
			ctx.StreamFn("v3_llm_end", payload)
			return
		}
		eventName := v3StageToEvent(stage)
		if eventName == "v3_progress" {
			ctx.StreamFn("v3_progress", map[string]string{
				"message": fmt.Sprintf("  │ [%s] %s", stage, detail),
			})
			return
		}
		payload := map[string]interface{}{
			"stage":  stage,
			"detail": detail,
		}
		for k, v := range data {
			payload[k] = v
		}
		ctx.StreamFn(eventName, payload)
	})
	if err != nil {
		return "", V3EditMetadata{}, err
	}

	if ctx.StreamFn != nil {
		ctx.StreamFn("v3_progress", map[string]string{
			"message": fmt.Sprintf("  └──── V3 complete: %s, %d candidates", v3Result.PhaseSolved, v3Result.CandidatesTested),
		})
	}

	chosen, proposed := proposedV3Candidate(v3Result, content)
	if !proposed {
		// Nothing materially different came back. The caller's content stands,
		// and no V3 metadata is attached to it.
		return content, V3EditMetadata{}, nil
	}
	// V3 sometimes returns code wrapped in markdown fences (the underlying
	// llama-server response had a preamble it didn't strip). Strip it here,
	// at the boundary, so the regression check below judges the code rather
	// than the wrapper — a fenced candidate is not a broken one — and so
	// neither caller ships a "Looking at the task..." header to disk.
	if cleaned, sanitized := sanitizeFileContent(path, chosen); sanitized {
		log.Printf("[v3] sanitised candidate for %s", logPath(path))
		chosen = cleaned
		// Stripping a wrapper can leave the caller's own content, which is
		// not a proposal. Everything else about the sanitised bytes is
		// decided by the route that receives them: it stages them, produces
		// evidence about them, and applies the policy.
		if chosen == content {
			log.Printf("[v3] sanitised candidate for %s is the caller's own content", logPath(path))
			return content, V3EditMetadata{}, nil
		}
	}
	// A candidate only counts as an improvement if it does not break what it
	// was handed. V3 regenerates the whole file, so it can reintroduce a
	// defect the model had already repaired; the write gates downstream would
	// then reject the edit and quote a line number from a candidate the model
	// never saw, sending it to hunt a bug that is not in the file (observed
	// 2026-07-31: an edit rejected for a stray paren V3 reintroduced, then
	// three dead turns searching a clean file). Drop the candidate here so the
	// caller keeps the model's own content and the gates below only ever judge
	// content the model actually wrote.
	if chosen != content {
		if reason := v3CandidateRegression(ctx, path, content, chosen); reason != "" {
			log.Printf("[v3] discarding candidate for %s — %s; keeping the caller's content", logPath(path), reason)
			return content, V3EditMetadata{}, nil
		}
	}
	return chosen, V3EditMetadata{
		Used:                 true,
		CandidatesTested:     v3Result.CandidatesTested,
		WinningScore:         v3Result.WinningScore,
		PhaseSolved:          v3Result.PhaseSolved,
		VerificationEvidence: v3Result.VerificationEvidence,
		Envelope:             envelopeOf(v3Result),
	}, nil
}

// findActualString searches for oldStr in content, handling quote normalization.
// Returns the actual string found in content (may differ in quote style).
// findFuzzyLineMatch rescues an old_str whose only error is per-line
// whitespace drift (indentation width, tabs vs spaces, trailing spaces) —
// the dominant edit_file failure mode for small models. It compares old_str
// against the file line-by-line with each line whitespace-stripped, and
// returns the EXACT span from the file (original indentation preserved) so
// the caller's existing uniqueness-count + replace logic works unchanged.
//
// Safety: it requires exactly ONE matching window. Zero or multiple matches
// return ("", false) so the caller fails cleanly rather than editing a
// guessed location. It also refuses to match when old_str has no
// non-whitespace content (would match any blank run). Content differences
// beyond whitespace (renamed tokens, changed quotes) do NOT match — those
// are semantic and must fail.
// closestLineHint finds the file line most similar to the first
// non-blank line of a missed old_str and formats it (with its line
// number) for inclusion in the edit_file mismatch error. Similarity is
// shared-token count — crude, but enough to map a from-memory paraphrase
// like `item = items[id + 1]` onto the real
// `return jsonify(items[item_id + 1])`. Returns "" when nothing clears a
// minimal overlap bar (so unrelated guesses don't get a misleading
// anchor).
func closestLineHint(content, oldStr string) string {
	var probe string
	for _, l := range strings.Split(oldStr, "\n") {
		if strings.TrimSpace(l) != "" {
			probe = l
			break
		}
	}
	if probe == "" {
		return ""
	}
	tokenize := func(s string) map[string]bool {
		out := map[string]bool{}
		for _, t := range strings.FieldsFunc(s, func(r rune) bool {
			return !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
		}) {
			out[t] = true
		}
		return out
	}
	probeToks := tokenize(probe)
	if len(probeToks) == 0 {
		return ""
	}
	bestScore, bestIdx := 0, -1
	lines := strings.Split(content, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		score := 0
		for t := range tokenize(l) {
			if probeToks[t] {
				score++
			}
		}
		if score > bestScore {
			bestScore, bestIdx = score, i
		}
	}
	// Require at least 2 shared identifiers and half the probe's tokens —
	// below that the "closest" line is likely unrelated.
	if bestIdx < 0 || bestScore < 2 || bestScore*2 < len(probeToks) {
		return ""
	}
	return fmt.Sprintf("\nClosest actual line in the file (line %d): %s\nCopy real lines from the file into old_str — do not write them from memory.",
		bestIdx+1, truncateStr(strings.TrimSpace(lines[bestIdx]), 160))
}

func findFuzzyLineMatch(content, oldStr string) (string, bool) {
	fileLines := strings.Split(content, "\n")
	oldLines := strings.Split(oldStr, "\n")
	// Drop a single trailing empty line from a trailing newline in old_str.
	if len(oldLines) > 1 && strings.TrimSpace(oldLines[len(oldLines)-1]) == "" {
		oldLines = oldLines[:len(oldLines)-1]
	}
	if len(oldLines) == 0 {
		return "", false
	}
	strip := func(ls []string) []string {
		out := make([]string, len(ls))
		nonEmpty := false
		for i, l := range ls {
			out[i] = strings.TrimSpace(l)
			if out[i] != "" {
				nonEmpty = true
			}
		}
		if !nonEmpty {
			return nil // all-whitespace target: refuse
		}
		return out
	}
	want := strip(oldLines)
	if want == nil {
		return "", false
	}
	n := len(want)
	matchStart := -1
	matches := 0
	for i := 0; i+n <= len(fileLines); i++ {
		ok := true
		for j := 0; j < n; j++ {
			if strings.TrimSpace(fileLines[i+j]) != want[j] {
				ok = false
				break
			}
		}
		if ok {
			matches++
			matchStart = i
			if matches > 1 {
				return "", false // ambiguous — fail safe
			}
		}
	}
	if matches != 1 {
		return "", false
	}
	return strings.Join(fileLines[matchStart:matchStart+n], "\n"), true
}

func findActualString(content, oldStr string) string {
	// Direct match first
	if strings.Contains(content, oldStr) {
		return oldStr
	}

	// Quote normalization: try replacing curly quotes with straight and vice versa
	normalized := normalizeQuotes(oldStr)
	if normalized != oldStr && strings.Contains(content, normalized) {
		return normalized
	}

	// Try the reverse direction
	denormalized := denormalizeQuotes(oldStr)
	if denormalized != oldStr && strings.Contains(content, denormalized) {
		return denormalized
	}

	return ""
}

// normalizeQuotes replaces curly quotes with straight quotes.
func normalizeQuotes(s string) string {
	r := strings.NewReplacer(
		"\u201c", "\"", // left double
		"\u201d", "\"", // right double
		"\u2018", "'", // left single
		"\u2019", "'", // right single
	)
	return r.Replace(s)
}

// denormalizeQuotes replaces straight quotes with curly quotes (best-effort).
func denormalizeQuotes(s string) string {
	r := strings.NewReplacer(
		"\"", "\u201c", // straight double → left double (approximate)
		"'", "\u2019", // straight single → right single (approximate)
	)
	return r.Replace(s)
}

// buildDiffPreview creates a unified-diff-style preview of the edit.
func buildDiffPreview(oldContent, newContent, oldStr, newStr string) string {
	// Find the line number where the change starts
	idx := strings.Index(oldContent, oldStr)
	if idx < 0 {
		return ""
	}
	lineNum := strings.Count(oldContent[:idx], "\n") + 1

	var sb strings.Builder
	fmt.Fprintf(&sb, "@@ line %d @@\n", lineNum)

	// Show removed lines
	for _, line := range strings.Split(oldStr, "\n") {
		fmt.Fprintf(&sb, "- %s\n", line)
	}
	// Show added lines
	for _, line := range strings.Split(newStr, "\n") {
		fmt.Fprintf(&sb, "+ %s\n", line)
	}

	return sb.String()
}

// ---------------------------------------------------------------------------
// delete_file
// ---------------------------------------------------------------------------

// insert_after adds lines at a location the model NAMES instead of one it
// reproduces.
//
// edit_file needs an anchor copied byte-for-byte; structural_edit needs the
// whole node re-emitted. Both put a large verbatim-output burden on the model,
// and that is the step that measurably fails: asked to reproduce a ten-line
// anchor it produced "safe_load_aller" for "safe_load_all". read_file already
// returns "N<tab>content", so a line number is something the model can cite
// rather than transcribe, and only the NEW text has to be generated.
//
// Same gates as every other write: workspace boundary, syntax (healthy->broken
// so a file already failing is not blocked), and the structural check for
// newly unresolved calls.
func insertAfterTool() *ToolDef {
	return &ToolDef{
		Name:   "insert_after",
		Effect: ToolEffectDirectMutation,
		Description: "Insert new lines into a file AFTER a given line number, without touching anything else. " +
			"Use this to ADD code — a new branch, function, import, or case — when you are not changing existing lines. " +
			"`line` is the 1-based number shown by read_file (0 inserts at the top of the file); `content` is only the new text. " +
			"Prefer this over edit_file when adding rather than replacing: there is no old_str to reproduce, so a long or awkward anchor cannot go wrong. " +
			"To CHANGE an existing line use edit_file; to replace a whole function use structural_edit.",
		InputSchema: InsertAfterInput{},
		Destructive: false,
		Execute: func(input json.RawMessage, ctx *AgentContext) (*ToolResult, error) {
			var in InsertAfterInput
			if err := json.Unmarshal(input, &in); err != nil {
				return nil, errNoMutation(fmt.Errorf("invalid input: %w", err))
			}
			if strings.TrimSpace(in.Path) == "" {
				// Naming only the missing field sends the model back with a
				// different field missing. Observed live: it sent
				// {"line":0,"content":"..."} with no path, right after an
				// edit_file that had also been malformed.
				return noMutation("insert_after: path is required. The call is " +
					"insert_after {\"path\":\"app.py\", \"line\":<1-based number from read_file, 0 for top of file>, " +
					"\"content\":<the new lines>}."), nil
			}
			if in.Content == "" {
				return noMutation("insert_after: content is empty — nothing would be inserted"), nil
			}
			path := resolveAgentPath(ctx, in.Path)
			if !editViewIsCurrent(ctx, path) {
				return nil, errNoMutation(fmt.Errorf("file not read yet — use read_file first so the line numbers are current: %s", in.Path))
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return noMutation(fmt.Sprintf("cannot read %s: %v", in.Path, err)), nil
			}
			original := string(data)
			lines := strings.Split(original, "\n")
			// A trailing newline yields a final empty element; inserting after
			// it would append past the end, so treat it as the boundary.
			limit := len(lines)
			if limit > 0 && lines[limit-1] == "" {
				limit--
			}
			if in.Line < 0 || in.Line > limit {
				return noMutation(fmt.Sprintf(
					"insert_after: line %d is out of range for %s, which has %d lines. Use the numbers read_file showed you.",
					in.Line, in.Path, limit)), nil
			}
			insert := strings.Split(strings.TrimSuffix(in.Content, "\n"), "\n")
			// Before any pipeline work: an insertion that moves existing
			// statements into another block is refused outright, however well
			// the result parses. See insertionReparentsPython.
			if msg, moved := insertionReparentsPython(path, lines, in.Line, insert); moved {
				return structuralRefusal(msg), nil
			}
			merged := append([]string{}, lines[:in.Line]...)
			merged = append(merged, insert...)
			merged = append(merged, lines[in.Line:]...)
			updated := strings.Join(merged, "\n")

			// Healthy->broken only: a file already failing the checker stays
			// editable, which is what makes repair-in-progress possible.
			// Every content edit goes through the pipeline — these two were
			// producing one greedy sample with no candidate generation and no
			// lens scoring, which is exactly what the tier system exists to
			// prevent.
			route := runEditPipeline(ctx, "insert_after", path, in.Path, original, updated)
			if route.Cancelled != nil {
				return route.Cancelled, nil
			}
			if route.Delivered {
				// The candidate landed through the protected chain. Writing the
				// caller's own edit now would undo a delivery that settlement
				// and restoration already decided about.
				return attachV3(route.Result, route.Meta), nil
			}
			v3Out := route.Meta
			updated = route.Content

			observed, refusal := editSyntaxObservation(ctx, "insert_after", in.Path, in.Path,
				original, updated, func(detail string) string {
					return fallbackSyntaxRejection(in.Path, updated, detail)
				})
			if refusal != nil {
				return refusal, nil
			}
			if introduced := editIntroducesUnresolved(ctx, path, original, updated); len(introduced) > 0 {
				return structuralRefusal(structuralRejection(in.Path, introduced)), nil
			}
			if msg := embeddedScriptGate(ctx, path, original, updated); msg != "" {
				return structuralRefusal(msg), nil
			}
			if msg := duplicateMainGuard(path, original, updated); msg != "" {
				return structuralRefusal(msg), nil
			}
			logMandatoryChecks(ctx, "insert_after", in.Path, "")

			if err := os.WriteFile(path, []byte(updated), 0644); err != nil {
				return nil, editWriteFailure(path,
					fmt.Errorf("cannot write %s: %w", in.Path, err), observed)
			}
			ctx.SessionWrites[in.Path] = true
			ctx.RecordFileRead(path, updated)
			ctx.RecordBodySeen(path)
			log.Printf("[insert_after] %s +%d lines after line %d", logPath(in.Path), len(insert), in.Line)
			out, _ := json.Marshal(EditFileOutput{
				OK:          true,
				DiffPreview: fmt.Sprintf("+%d lines after line %d", len(insert), in.Line),
			})
			result := &ToolResult{Success: true, Data: out,
				MutationStatus: MutationApplied}
			// The observation describes exactly the bytes just written --
			// the tool's own edit, an authorized V3 candidate, or a baseline
			// restored after revocation, whichever runEditPipeline returned.
			// Provenance is a separate fact, attached by attachV3.
			overlayValidation(result, observed)
			return attachV3(result, v3Out), nil
		},
	}
}

// replaceLinesMaxSpan bounds a single replace_lines call. Above this the model
// is re-authoring rather than editing, and a whole-node rewrite is both more
// reliable and easier to verify. The cliff is not gradual: a 14B model applies
// ~0.87 of its edit blocks on 100-500 line files and 0.00 above 500.
//
// 60, not the 20 this shipped with. The unit of work that kept hitting the
// cap is a whole function, and a JavaScript function inside a Flask template
// runs 40-50 lines. At 20 the refusal sent the model to structural_edit,
// which cannot reach into a Python string literal, and its refusal sent it
// back here — an observed session burned all three of its strikes on that
// loop with the file untouched. The size guard is also not what makes this
// tool safe: expected_first_line / expected_last_line already fail a stale
// range, and they cost the same two lines whether the span is 5 or 50.
const replaceLinesMaxSpan = 60

// relocateStaleRange finds where a stale replace_lines range moved to.
//
// The model's line numbers go stale the moment an earlier edit changes the
// file's length, and it re-sends the same numbers because from its side
// nothing looks wrong. Measured across 168 sessions: 23 hit an anchor
// refusal and 11 of those lost the task, usually to a re-send loop after
// "line N is not what you expected".
//
// The assertions the call already carries are enough to fix it. If the
// expected first and last lines both appear, at the same offset, exactly
// once each, the range simply moved — apply it there instead of refusing.
// Ambiguity (either line appearing more than once, or the two disagreeing
// about the shift) returns 0 and leaves the refusal in place: relocating an
// edit to the wrong place is far worse than asking for a re-read.
//
// This is the "shifted" case from grok-build's hashline anchors, using the
// text ATLAS already receives rather than hashing every line of read_file
// output — the same recovery without spending context on every read.
func relocateStaleRange(fileLines []string, limit int,
	expectedFirst, expectedLast string, span int) int {
	first := strings.TrimSpace(expectedFirst)
	last := strings.TrimSpace(expectedLast)
	if first == "" || last == "" || span < 1 {
		return 0
	}
	findUnique := func(want string) int {
		found := 0
		for i := 0; i < limit; i++ {
			if strings.TrimSpace(fileLines[i]) == want {
				if found != 0 {
					return -1 // ambiguous
				}
				found = i + 1
			}
		}
		return found
	}
	firstAt := findUnique(first)
	if firstAt <= 0 {
		return 0
	}
	lastAt := findUnique(last)
	if lastAt <= 0 {
		return 0
	}
	if lastAt-firstAt+1 != span {
		return 0 // the block changed shape, not just position
	}
	return firstAt
}

// lineAssertionMismatch compares an expected line against what is actually
// there, whitespace-insensitively, and renders the correction when they differ.
//
// Whitespace-insensitive on purpose: the model reliably reproduces the TEXT of
// one line and unreliably reproduces its indentation, and indentation is not
// what the assertion is for. It exists to catch a wrong line NUMBER.
//
// The error carries a numbered window around the range, and says where the
// expected text really is. history is what is known about why the numbers are
// wrong (lineNumberHistory); without it the error claims no cause. It used to
// say "The numbers you used are stale" every time. Smoke run 2026-09-27
// (smallrung_toml): the file had not changed since the model read it; the
// model had used line 169 for text that is only on lines 1418-1548.
func lineAssertionMismatch(expected, actual string, lineNum int, path string, fileLines []string, history string) string {
	if strings.TrimSpace(expected) == strings.TrimSpace(actual) {
		return ""
	}
	if strings.TrimSpace(expected) == "" {
		return fmt.Sprintf("replace_lines: expected text for line %d is empty. Send the text of that line "+
			"(without the \"N<tab>\" prefix) so an off-by-one cannot apply silently.", lineNum)
	}
	var window strings.Builder
	lo, hi := lineNum-3, lineNum+3
	if lo < 1 {
		lo = 1
	}
	if hi > len(fileLines) {
		hi = len(fileLines)
	}
	for i := lo; i <= hi; i++ {
		marker := " "
		if i == lineNum {
			marker = ">"
		}
		window.WriteString(fmt.Sprintf("%s %d\t%s\n", marker, i, fileLines[i-1]))
	}
	if history == "" {
		history = "Those line numbers do not match the file."
	}
	return fmt.Sprintf("replace_lines: line %d of %s is not what you expected, so the range is wrong and was NOT applied.\n"+
		"  you said: %s\n  actually: %s\n%s %s Current lines around %d:\n%s"+
		"Re-read the file if you need more context, then send the range that matches.",
		lineNum, path, truncateStr(strings.TrimSpace(expected), 120), truncateStr(strings.TrimSpace(actual), 120),
		history, whereTextIs(fileLines, expected), lineNum, window.String())
}

// whereTextIs says which lines hold want, whitespace-insensitively: none, one,
// or several to choose from.
func whereTextIs(fileLines []string, want string) string {
	want = strings.TrimSpace(want)
	var at []string
	for i, l := range fileLines {
		if strings.TrimSpace(l) == want {
			at = append(at, strconv.Itoa(i+1))
		}
	}
	switch {
	case len(at) == 0:
		return "That text is not in the file."
	case len(at) == 1:
		return "That text is at line " + at[0] + "."
	case len(at) <= 8:
		return "That text is on lines " + strings.Join(at, ", ") + ": send the one you mean."
	default:
		return fmt.Sprintf("That text is on %d lines (%s, ...): send the one you mean.",
			len(at), strings.Join(at[:8], ", "))
	}
}

// lineNumberHistory says why the model's line numbers are wrong, but only when
// the evidence shows it: the session wrote the file, or it changed after the
// last read (stale); or it still equals what the first full read showed (the
// numbers never matched). Otherwise it returns "", and no cause is claimed.
func lineNumberHistory(ctx *AgentContext, path, relPath, current string) string {
	if ctx == nil {
		return ""
	}
	ctx.mu.Lock()
	wrote := ctx.SessionWrites[relPath]
	lastRead, read := ctx.FileReadTimes[path]
	original, seen := ctx.OriginalContent[path]
	ctx.mu.Unlock()
	if wrote {
		return "The file changed since you read it (your own edits move line numbers), so the numbers you used are stale."
	}
	if read {
		if info, err := os.Stat(path); err == nil && info.ModTime().After(lastRead) {
			return "The file changed since you last read it, so the numbers you used are stale."
		}
	}
	if seen && original == current {
		return "The file has not changed since you read it, so these numbers never matched this text."
	}
	return ""
}

// replaceLinesTool is insert_after's rationale extended to REPLACEMENT.
//
// edit_file addresses a region by reproducing its text; on a 9-13 line span
// this model corrupts one token somewhere in it essentially every time
// (food.y -> hood.y, scoreElement -> scorerElement, unshift(( ). read_file
// already prints the line numbers, so the address can be cited instead.
//
// The expected first/last line assertion is not optional. A line range fails
// differently from an anchor: a wrong anchor simply does not match, while a
// wrong line number still splices cleanly and produces plausible corruption.
// One line each is the length regime this model is reliable in.
func replaceLinesTool() *ToolDef {
	return &ToolDef{
		Name:   "replace_lines",
		Effect: ToolEffectDirectMutation,
		Description: "Replace a RANGE OF LINES with new content, addressed by the line numbers read_file printed. " +
			"Use this to CHANGE existing code spanning more than a line or two: cite `start_line` and `end_line` instead of " +
			"reproducing the old text, so a long span cannot go wrong in transcription. Both are 1-based and INCLUSIVE. " +
			"`expected_first_line` / `expected_last_line` are the text of those two lines WITHOUT the \"N<tab>\" prefix; they are " +
			"compared whitespace-insensitively so an off-by-one is caught instead of silently applied. `content` is only the new text. " +
			"Use edit_file for a one-line change, insert_after to ADD without replacing, structural_edit for a whole function or class.",
		InputSchema: ReplaceLinesInput{},
		ReadOnly:    false,
		Destructive: false,
		Execute: func(input json.RawMessage, ctx *AgentContext) (*ToolResult, error) {
			var in ReplaceLinesInput
			if err := json.Unmarshal(input, &in); err != nil {
				return nil, errNoMutation(fmt.Errorf("invalid input: %w", err))
			}
			if strings.TrimSpace(in.Path) == "" {
				return noMutation("replace_lines: path is required. The call is " +
					"replace_lines {\"path\":\"app.py\", \"start_line\":N, \"end_line\":M, " +
					"\"expected_first_line\":<text of line N>, \"expected_last_line\":<text of line M>, " +
					"\"content\":<the new lines>}."), nil
			}
			path := resolveAgentPath(ctx, in.Path)
			if !editViewIsCurrent(ctx, path) {
				return nil, errNoMutation(fmt.Errorf("file not read yet — use read_file first so the line numbers are current: %s", in.Path))
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return noMutation(fmt.Sprintf("cannot read %s: %v", in.Path, err)), nil
			}
			original := string(data)
			fileLines := strings.Split(original, "\n")
			limit := len(fileLines)
			if limit > 0 && fileLines[limit-1] == "" {
				limit--
			}
			if in.StartLine < 1 || in.EndLine < in.StartLine || in.EndLine > limit {
				return noMutation(fmt.Sprintf(
					"replace_lines: range %d-%d is invalid for %s, which has %d lines. Both bounds are 1-based and "+
						"inclusive, and start_line must not exceed end_line. Use the numbers read_file showed you.",
					in.StartLine, in.EndLine, in.Path, limit)), nil
			}
			if span := in.EndLine - in.StartLine + 1; span > replaceLinesMaxSpan {
				// A size policy declining a replacement it never formed.
				return noMutation(fmt.Sprintf(
					"replace_lines: %d lines is too large a range (limit %d). A replacement that size is a rewrite rather "+
						"than an edit. Split it into consecutive replace_lines calls of at most %d lines, working from "+
						"the BOTTOM of the file upward so the earlier line numbers stay valid. For a whole function "+
						"or class, structural_edit replaces the node in one call (%s) — but only for real nodes: code "+
						"inside a string literal (a <script> block in an HTML template, say) is one string to the "+
						"host grammar and no selector reaches into it, so there the split is the way.",
					span, replaceLinesMaxSpan, replaceLinesMaxSpan, selectorGuidanceOrOutline(in.Path, original))), nil
			}
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
			history := lineNumberHistory(ctx, path, in.Path, original)
			if msg := lineAssertionMismatch(in.ExpectedFirstLine, fileLines[in.StartLine-1], in.StartLine, in.Path, fileLines, history); msg != "" {
				return noMutation(msg), nil
			}
			if msg := lineAssertionMismatch(in.ExpectedLastLine, fileLines[in.EndLine-1], in.EndLine, in.Path, fileLines, history); msg != "" {
				return noMutation(msg), nil
			}

			replacement := strings.Split(strings.TrimSuffix(in.Content, "\n"), "\n")
			merged := append([]string{}, fileLines[:in.StartLine-1]...)
			merged = append(merged, replacement...)
			merged = append(merged, fileLines[in.EndLine:]...)
			updated := strings.Join(merged, "\n")

			if updated == original {
				return noMutation("replace_lines: the replacement is identical to what is already on " +
					"those lines — nothing changed and the bug is still there."), nil
			}

			// Same gate chain as insert_after and edit_file, healthy->broken
			// only so a file mid-repair stays editable.
			// Every content edit goes through the pipeline — these two were
			// producing one greedy sample with no candidate generation and no
			// lens scoring, which is exactly what the tier system exists to
			// prevent.
			route := runEditPipeline(ctx, "replace_lines", path, in.Path, original, updated)
			if route.Cancelled != nil {
				return route.Cancelled, nil
			}
			if route.Delivered {
				// The candidate landed through the protected chain. Writing the
				// caller's own edit now would undo a delivery that settlement
				// and restoration already decided about.
				return attachV3(route.Result, route.Meta), nil
			}
			v3Out := route.Meta
			updated = route.Content

			observed, refusal := editSyntaxObservation(ctx, "replace_lines", in.Path, in.Path,
				original, updated, func(detail string) string {
					return fallbackSyntaxRejection(in.Path, updated, detail)
				})
			if refusal != nil {
				return refusal, nil
			}
			if introduced := editIntroducesUnresolved(ctx, path, original, updated); len(introduced) > 0 {
				return structuralRefusal(structuralRejection(in.Path, introduced)), nil
			}
			if msg := embeddedScriptGate(ctx, path, original, updated); msg != "" {
				return structuralRefusal(msg), nil
			}
			if msg := duplicateMainGuard(path, original, updated); msg != "" {
				return structuralRefusal(msg), nil
			}
			logMandatoryChecks(ctx, "replace_lines", in.Path, "")

			if err := os.WriteFile(path, []byte(updated), 0644); err != nil {
				return nil, editWriteFailure(path,
					fmt.Errorf("cannot write %s: %w", in.Path, err), observed)
			}
			ctx.SessionWrites[in.Path] = true
			ctx.RecordFileRead(path, updated)
			ctx.RecordBodySeen(path)
			replaced := in.EndLine - in.StartLine + 1
			log.Printf("[replace_lines] %s lines %d-%d: %d -> %d lines", logPath(in.Path), in.StartLine, in.EndLine, replaced, len(replacement))
			out, _ := json.Marshal(EditFileOutput{
				OK:          true,
				DiffPreview: fmt.Sprintf("lines %d-%d replaced (%d -> %d lines)", in.StartLine, in.EndLine, replaced, len(replacement)),
			})
			result := &ToolResult{Success: true, Data: out,
				MutationStatus: MutationApplied}
			// The observation describes exactly the bytes just written --
			// the tool's own edit, an authorized V3 candidate, or a baseline
			// restored after revocation, whichever runEditPipeline returned.
			// Provenance is a separate fact, attached by attachV3.
			overlayValidation(result, observed)
			return attachV3(result, v3Out), nil
		},
	}
}

func deleteFileTool() *ToolDef {
	return &ToolDef{
		Name:        "delete_file",
		Effect:      ToolEffectDirectMutation,
		Description: "Delete a file or empty directory. Use for removing files that are no longer needed.",
		InputSchema: DeleteFileInput{},
		ReadOnly:    false,
		Destructive: true,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (*ToolResult, error) {
			var input DeleteFileInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, errNoMutation(fmt.Errorf("invalid input: %w", err))
			}

			// Reject empty path — same reasoning as read_file.
			if strings.TrimSpace(input.Path) == "" {
				return noMutation("delete_file: path cannot be empty. Provide the path of the file you want to delete."), nil
			}

			path := resolveAgentPath(ctx, input.Path)
			// The approval is spent by this attempt whatever happens next --
			// a preflight refusal included -- so one answer can never carry
			// to a later call, and the reference it holds lives exactly as
			// long as the attempt. `approved` is only ever non-empty when the
			// user answered this exact call through the permission endpoint.
			approved, userApproved := takeDeleteApproval(ctx, path)
			if userApproved {
				defer approved.release()
			}
			info, err := os.Stat(path)
			if err != nil {
				return nil, errNoMutation(fmt.Errorf("file not found: %s", input.Path))
			}
			if info.IsDir() {
				entries, _ := os.ReadDir(path)
				if len(entries) > 0 {
					// A guard declining a target it could otherwise remove.
					return refusedNoCheck(fmt.Sprintf("directory not empty: %s (%d entries) — delete_file only removes files or empty directories", input.Path, len(entries))), nil
				}
			}
			// The approval was granted against an inspected object. Re-inspect
			// now, next to the removal, and compare: bytes, type, link text,
			// emptiness. If any of it moved while the prompt was on screen,
			// the user's answer was about a different thing and is spent
			// rather than reused. A fresh tool call can ask again.
			//
			// This narrows the window between check and remove; it does not
			// close it. There is no portable way to remove a directory entry
			// conditional on the inode behind it, so a change landing inside
			// that window is still possible. What is ruled out is honouring an
			// approval for bytes that were already different when the user
			// answered.
			// The approved route.
			if userApproved {
				// The re-inspection's own reference is needed only for the
				// comparison and is released with the attempt.
				now, refusal := inspectDeleteTarget(ctx, rawInput)
				defer now.release()
				if refusal != "" || !approved.identityMatches(now) {
					log.Printf("[delete_file] approval is stale for %s — not deleting", logPath(input.Path))
					return refusedNoCheck(fmt.Sprintf(
						"%s changed after you were asked about it, so the approval no longer "+
							"describes what is on disk and nothing was deleted. Ask again if "+
							"you still want it removed.", input.Path)), nil
				}
				// Room to account for it, reserved before anything is
				// removed: a destructive mutation this session could not
				// track is worse than one it declines.
				if !reserveDeletionSlot(ctx, path) {
					log.Printf("[delete_file] no room to track %s — refusing", logPath(input.Path))
					return refusedNoCheck(fmt.Sprintf(
						"this session is already tracking as many removals as it can account "+
							"for, so %s was not deleted.", input.Path)), nil
				}
			}
			if rmErr := os.Remove(path); rmErr != nil {
				// The removal was attempted and the entry may or may not
				// still be there, so the producer says failed, not none.
				return &ToolResult{
					Success:          false,
					Error:            fmt.Sprintf("delete_file: %v", rmErr),
					MutationStatus:   MutationFailed,
					ValidationKind:   ValidationKindNone,
					ValidationStatus: ValidationNotApplicable,
				}, nil
			}

			out := DeleteFileOutput{Deleted: true}
			outBytes, _ := json.Marshal(out)
			// The approval, the revalidation and the successful removal are
			// all true at this instant, so they are recorded together rather
			// than reconstructed later from separate booleans. Absence and the
			// tombstone are the ledger's to confirm, and it promotes this.
			if userApproved {
				noteDeletionAttempt(ctx, permCallIDFor(ctx), approved)
			}

			// Removal demonstrated. A delete validates nothing: there are no
			// bytes left to have an opinion about.
			//
			// This used to hand the loop a sentinel that ended the session on
			// the spot, so the model could not narrate after a destructive
			// operation. It also ended the TASK. Asked to delete a.py and
			// write report.py, a run that happened to delete first stopped
			// before report.py was ever attempted, and the outcome turned on
			// nothing but which tool the model reached for first.
			//
			// The delete now returns like any other call. It still cannot
			// authorise a completion -- the tombstone rule at the real
			// terminal is unchanged and still fail-closed -- but the run gets
			// to finish what it was asked to do and be judged on all of it.
			return &ToolResult{Success: true, Data: outBytes,
				MutationStatus:   MutationApplied,
				ValidationKind:   ValidationKindNone,
				ValidationStatus: ValidationNotApplicable}, nil
		},
	}
}

// ---------------------------------------------------------------------------
// move_file — relocate / rename a file within the workspace.
//
// Added to close the "reorganize the files" gap (observed: a flask task asked
// to move index.html into templates/; `mv` is refused, there is no move tool,
// so the model looped on mkdir until the repetition breaker fired). A pure
// move is not a content change, so it bypasses the V3 / surgical-edit gate —
// content is preserved verbatim. Refuses to clobber an existing destination
// file so a relocation can't silently destroy data.
// ---------------------------------------------------------------------------

func moveFileTool() *ToolDef {
	return &ToolDef{
		Name:        "move_file",
		Effect:      ToolEffectDirectMutation,
		Description: "Move or rename a file within the project (e.g. move index.html into templates/, or rename old.py to new.py). Use this to reorganize files — shell `mv`/`cp` are refused. If destination is an existing directory, the file is moved into it keeping its name. Content is preserved exactly.",
		InputSchema: MoveFileInput{},
		ReadOnly:    false,
		Destructive: false,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (*ToolResult, error) {
			var input MoveFileInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, errNoMutation(fmt.Errorf("invalid input: %w", err))
			}
			if strings.TrimSpace(input.Source) == "" || strings.TrimSpace(input.Destination) == "" {
				return noMutation(`move_file: both source and destination are required. Call with {"source":"<current path>","destination":"<new path>"}.`), nil
			}

			src := resolveAgentPath(ctx, input.Source)
			srcInfo, err := os.Stat(src)
			if err != nil {
				return noMutation(fmt.Sprintf("move_file: source %s not found. Use list_directory or find_file to confirm the path before moving.", input.Source)), nil
			}

			// Resolve destination. If it names an existing directory (or ends
			// with a separator), move INTO it keeping the source basename —
			// mirrors `mv file dir/`. The relative dest is what we report back
			// so the model's mental model stays in project-relative terms.
			relDest := input.Destination
			dst := resolveAgentPath(ctx, input.Destination)
			if info, err := os.Stat(dst); err == nil && info.IsDir() {
				dst = filepath.Join(dst, filepath.Base(src))
				relDest = filepath.Join(input.Destination, filepath.Base(src))
			} else if strings.HasSuffix(input.Destination, "/") {
				dst = filepath.Join(dst, filepath.Base(src))
				relDest = filepath.Join(input.Destination, filepath.Base(src))
			}

			if src == dst {
				return noMutation("move_file: source and destination are the same path — nothing to do."), nil
			}

			// Never clobber an existing destination file: a relocation must not
			// silently destroy data. Tell the model to pick another name or
			// delete_file the destination first if the overwrite is intended.
			if _, err := os.Stat(dst); err == nil {
				// The clobber guard declining a move it could otherwise make.
				return refusedNoCheck(fmt.Sprintf("move_file: destination %s already exists. Pick a different name, or delete_file the destination first if you mean to replace it.", relDest)), nil
			}

			if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
				// The source is untouched, but MkdirAll can leave part of the
				// tree behind, so this is an attempted mutation that errored.
				return nil, errFailedMutation(
					fmt.Errorf("move_file: cannot create destination dir: %w", err),
					ValidationKindNone, ValidationNotApplicable)
			}

			// os.Rename is atomic on the same filesystem; fall back to
			// copy+remove across devices (bind mounts can straddle filesystems).
			if err := os.Rename(src, dst); err != nil {
				// Rename is all-or-nothing: a failure leaves the source where
				// it was and the destination absent.
				if srcInfo.IsDir() {
					return nil, errNoMutation(fmt.Errorf("move_file: cannot move directory across filesystems: %w", err))
				}
				data, rerr := os.ReadFile(src)
				if rerr != nil {
					return nil, errNoMutation(fmt.Errorf("move_file: cannot read source: %w", rerr))
				}
				if werr := os.WriteFile(dst, data, srcInfo.Mode().Perm()); werr != nil {
					// The destination may hold a partial copy.
					return nil, errFailedMutation(
						fmt.Errorf("move_file: cannot write destination: %w", werr),
						ValidationKindNone, ValidationNotApplicable)
				}
				// A failure here leaves BOTH copies on disk. The move still
				// happened, so it is applied; the ledger sees the surviving
				// source and declines to tombstone it.
				os.Remove(src)
			}
			log.Printf("[move_file] %s → %s", input.Source, relDest)

			// Keep agent bookkeeping consistent: the file the model just read
			// now lives at the new path. Re-point the recorded read and the
			// session-write set so a follow-up edit isn't bounced as blind and
			// dedup logic tracks the right path.
			if content, ok := ctx.GetFileRead(src); ok {
				ctx.RecordFileRead(dst, content)
				ctx.ForgetFileRead(src)
			}
			if ctx.SessionWrites != nil {
				if ctx.SessionWrites[input.Source] {
					delete(ctx.SessionWrites, input.Source)
				}
				ctx.SessionWrites[relDest] = true
			}

			out := MoveFileOutput{Moved: true, Source: input.Source, Destination: relDest}
			outBytes, _ := json.Marshal(out)
			// Relocation demonstrated. A move preserves content exactly, so
			// this tool checks nothing: any verdict the bytes had was earned
			// under the old name and does not travel with them.
			return &ToolResult{Success: true, Data: outBytes,
				MutationStatus:   MutationApplied,
				ValidationKind:   ValidationKindNone,
				ValidationStatus: ValidationNotApplicable}, nil
		},
	}
}

// ---------------------------------------------------------------------------
// find_file — locate files by NAME (vs search_files which greps contents).
// Added because the model would search_files for a filename,
// get zero matches (because contents don't contain the literal filename),
// and conclude the file didn't exist.
// ---------------------------------------------------------------------------

func findFileTool() *ToolDef {
	return &ToolDef{
		Name:        "find_file",
		Effect:      ToolEffectReadOnly,
		Description: "Find files by NAME using a regex against the filename or relative path. Use this to check whether a file exists or to locate it. For searching inside file contents, use search_files instead.",
		InputSchema: FindFileInput{},
		ReadOnly:    true,
		Destructive: false,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (*ToolResult, error) {
			var input FindFileInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}

			// Reject empty pattern: it matches every filename, returns the
			// 200-match cap full of unrelated files, and confuses the model
			// into thinking it found nothing useful.
			if strings.TrimSpace(input.Pattern) == "" {
				// Observed live: the model put the filename in `path` and left
				// `pattern` empty, twice in a row. The generic "pattern cannot
				// be empty" is correct and did not land, because it never
				// mentioned the argument the model actually filled in. When
				// `path` looks like a filename rather than a directory, hand
				// back the exact call.
				if hint := findFileArgSwapHint(input.Path); hint != "" {
					return &ToolResult{Success: false, Error: hint}, nil
				}
				return &ToolResult{
					Success: false,
					Error:   "find_file: pattern cannot be empty. Provide a regex matching the filename you want to locate (e.g. \"snake_game\\.py\" or \"^main\\.\").",
				}, nil
			}

			searchPath := ctx.WorkingDir
			if input.Path != "" {
				searchPath = resolveAgentPath(ctx, input.Path)
			}

			re, err := regexp.Compile(input.Pattern)
			if err != nil {
				return nil, fmt.Errorf("invalid regex: %w", err)
			}

			var matches []FindFileMatch
			maxMatches := 200

			err = filepath.WalkDir(searchPath, func(path string, d fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return nil
				}
				if d.IsDir() {
					base := d.Name()
					if base == ".git" || base == "node_modules" || base == "__pycache__" || base == ".next" || base == "target" {
						return filepath.SkipDir
					}
					return nil
				}
				relPath, _ := filepath.Rel(ctx.WorkingDir, path)
				if relPath == "" {
					relPath = path
				}
				if re.MatchString(d.Name()) || re.MatchString(relPath) {
					matches = append(matches, FindFileMatch{Path: relPath, Name: d.Name()})
					if len(matches) >= maxMatches {
						return filepath.SkipAll
					}
				}
				return nil
			})

			if err != nil && len(matches) == 0 {
				return nil, fmt.Errorf("find error: %w", err)
			}

			out := FindFileOutput{
				Matches:    matches,
				TotalCount: len(matches),
				Truncated:  len(matches) >= maxMatches,
			}
			outBytes, _ := json.Marshal(out)
			return &ToolResult{Success: true, Data: outBytes}, nil
		},
	}
}

// ---------------------------------------------------------------------------
// run_command
// ---------------------------------------------------------------------------

func runCommandTool() *ToolDef {
	return &ToolDef{
		Name:        "run_command",
		Effect:      ToolEffectCommandUnobserved,
		Description: "Execute a shell command and WAIT for it to exit. Returns stdout, stderr, and exit code. Use for building, testing, and verifying code: `pytest`, `npm test`, `go build`, `curl`, `ls`. NOT for anything that doesn't exit on its own — a server, watcher, or `--watch` build blocks here until the timeout kills it, and the port it bound may still be held when you retry, so the identical command fails again with \"address already in use\". Use `run_background` for those.",
		InputSchema: RunCommandInput{},
		ReadOnly:    false,
		Destructive: true,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (*ToolResult, error) {
			var input RunCommandInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}

			// Trust gate: untrusted mode refuses command execution;
			// host execution is honored only under fully-trusted (else
			// downgraded to the sandbox below).
			if !ctx.TrustMode.commandsAllowed() {
				return &ToolResult{Success: false, Error: untrustedRefusal}, nil
			}

			timeoutSec := 30
			if input.Timeout != nil && *input.Timeout > 0 {
				timeoutSec = *input.Timeout
			}
			if timeoutSec > 300 {
				timeoutSec = 300
			}

			cwd := ctx.WorkingDir
			if input.Cwd != "" {
				cwd = resolveAgentPath(ctx, input.Cwd)
			}

			// Route shell execution through the sandbox container.
			// The proxy is a slim Go binary with no python/pip/node, so
			// running locally meant every "verify" command failed with
			// "command not found". The sandbox has the language matrix
			// pre-installed AND has /workspace bind-mounted at the same
			// path the proxy sees, so paths the agent learned via
			// read_file / list_directory still work. validateShellCommand
			// upstream is the gate; this is the executor.
			//
			// When ctx.VerifyOnHost is set (ATLAS_VERIFY_IN=host
			// or per-project config), we BYPASS the sandbox and execute
			// on the host directly. This is the right call for working
			// codebases that depend on host-side state — the user's
			// installed venv binaries, system tools, env vars,
			// running databases, etc. — that the sandbox can't see.
			// The shell-op safety gate (validateShellCommand) still
			// fired upstream regardless of target. cwd is translated
			// to the host path so the command lands in the right dir.
			// The declared envelope, checked before anything runs. A
			// deployment whose configured maxima exceed the host is one where
			// every process can be inside its own limit at the moment the
			// kernel picks a victim, which is exactly how the inference server
			// died. Refusing here rather than at startup keeps the diagnosis
			// readable: reading a file was never the unsafe part.
			if refusal := executionEnvelopeRefusal(envelopeFromEnv()); refusal != "" {
				log.Printf("[run_command] %s", refusal)
				return &ToolResult{
					Success: false,
					Error: "commands cannot be run in this deployment: its memory " +
						"limits add up to more than the machine has, so a command " +
						"that used too much could take down another service. Ask " +
						"the operator to fix the configured limits.",
				}, nil
			}

			var out RunCommandOutput
			var err error
			// Host execution requires fully-trusted; otherwise a
			// host request is downgraded to sandbox so the trust
			// level can't be silently escalated by ATLAS_VERIFY_IN.
			useHost := ctx.VerifyOnHost && ctx.TrustMode.hostExecutionAllowed()
			if useHost {
				hostCwd := cwd
				if ctx.HostWorkingDir != "" && strings.HasPrefix(cwd, ctx.WorkingDir) {
					hostCwd = ctx.HostWorkingDir + strings.TrimPrefix(cwd, ctx.WorkingDir)
				}
				out = runLocally(input.Command, hostCwd, time.Duration(timeoutSec)*time.Second)
			} else {
				out, err = runViaSandbox(ctx, input.Command, cwd, timeoutSec)
				if err != nil {
					log.Printf("[run_command] sandbox unavailable: %v", err)
					out = RunCommandOutput{
						Stderr:   fmt.Sprintf("sandbox unavailable: %v", err),
						ExitCode: 1,
					}
				}
			}

			outBytes, _ := json.Marshal(out)
			var errMsg string
			if out.TimedOut && out.ExitCode == 0 {
				// A kill that left a zero status behind. Saying nothing here
				// would hand the model a silent failure.
				errMsg = "the command was killed on its timeout"
			}
			// A command stopped at a resource ceiling is told apart from one
			// that failed, in the words the model reads. Left as an exit code
			// and a MemoryError on stderr, "your test suite is broken" and
			// "your test suite never finished" are the same message, and only
			// one of them is actionable.
			if resourceMsg := executionOutcomeMessage(out.Outcome); resourceMsg != "" &&
				executionStoppedByResource(out.Outcome) {
				errMsg = resourceMsg
			} else if out.ExitCode != 0 {
				errMsg = strings.TrimSpace(out.Stderr)
				if errMsg == "" {
					if s := strings.TrimSpace(out.Stdout); s != "" {
						lines := strings.Split(s, "\n")
						errMsg = lines[len(lines)-1]
					}
				}
				if errMsg == "" {
					errMsg = fmt.Sprintf("exit %d (no output)", out.ExitCode)
				}
				errMsg = truncateStr(errMsg, 400)
				errMsg += ownBackgroundJobHint(ctx, errMsg)
				errMsg += shellQuotingHint(input.Command, errMsg)
			}
			if errMsg == "" && !executionCompleted(out.Outcome) {
				// Exit zero from a command that did not reach its own end.
				// Silence here would read as success.
				if m := executionOutcomeMessage(out.Outcome); m != "" {
					errMsg = m
				}
			}
			return &ToolResult{
				Success: runCommandVerifiable(out),
				Data:    outBytes,
				Error:   errMsg,
			}, nil
		},
	}
}

// runViaSandbox POSTs the command to the sandbox /shell endpoint.
// Returns a populated RunCommandOutput on success, or an error if the
// sandbox is unreachable / returned a non-2xx (caller falls back to
// local exec). Timeout is in seconds and is enforced server-side; we
// add a generous client-side margin so the HTTP call doesn't kill
// long-running commands prematurely.
func runViaSandbox(ctx *AgentContext, command, cwd string, timeoutSec int) (RunCommandOutput, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"command": command,
		"cwd":     cwd,
		"timeout": timeoutSec,
	})
	endpoint := ctx.SandboxURL + "/shell"
	// Bind the agent's request context so a user cancel aborts the
	// in-flight sandbox call instead of waiting out the client timeout.
	reqCtx := ctx.Ctx
	if reqCtx == nil {
		reqCtx = context.Background()
	}
	httpReq, err := http.NewRequestWithContext(reqCtx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return RunCommandOutput{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: time.Duration(timeoutSec+30) * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return RunCommandOutput{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		// 4xx is usually a validation error (bad cwd, etc.) — propagate
		// as a regular failure, not a sandbox-unreachable signal. Read
		// the FastAPI detail so the model sees what went wrong.
		var errBody struct {
			Detail string `json:"detail"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		return RunCommandOutput{
			Stderr:   fmt.Sprintf("sandbox /shell %d: %s", resp.StatusCode, errBody.Detail),
			ExitCode: 1,
		}, nil
	}
	var out RunCommandOutput
	if err := decodeShellResponse(resp.Body, &out); err != nil {
		return RunCommandOutput{}, err
	}
	return out, nil
}

// decodeShellResponse reads the executor's answer into the shape the rest of
// the proxy uses.
//
// timed_out is decoded, not inferred. The executor sets it structurally when
// it kills a command, precisely so nothing downstream has to recognise
// "Execution timed out after 5s" in a stderr string to reach a decision.
func decodeShellResponse(body io.Reader, out *RunCommandOutput) error {
	var sr struct {
		Success         bool   `json:"success"`
		Stdout          string `json:"stdout"`
		Stderr          string `json:"stderr"`
		ExitCode        int    `json:"exit_code"`
		ElapsedMS       int    `json:"elapsed_ms"`
		TimedOut        bool   `json:"timed_out"`
		Outcome         string `json:"outcome"`
		PeakMemoryBytes int    `json:"peak_memory_bytes"`
	}
	if err := json.NewDecoder(body).Decode(&sr); err != nil {
		return fmt.Errorf("decode sandbox response: %w", err)
	}
	stdout := truncateStr(sr.Stdout, 8000)
	stderr := truncateStr(sr.Stderr, 4000)
	*out = RunCommandOutput{
		Stdout: stdout, Stderr: stderr, ExitCode: sr.ExitCode,
		TimedOut:        sr.TimedOut,
		OutputTruncated: len(stdout) < len(sr.Stdout) || len(stderr) < len(sr.Stderr),
		// Canonicalised at the boundary, so an executor that predates the
		// vocabulary -- or one that grows a member this build has not been
		// taught -- arrives as unclassified rather than as a completion.
		Outcome:         canonicalExecutionOutcome(sr.Outcome),
		PeakMemoryBytes: sr.PeakMemoryBytes,
	}
	return nil
}

// runCommandVerifiable reports whether an execution is sound enough to stand
// as verification of anything.
//
// One predicate, over structural facts only: the executor's exit status and
// its own timeout flag. Refusal, cancellation and an unreachable sandbox all
// arrive here as a nonzero status set by the caller that observed them, so
// none of them can pass. No string is examined.
func runCommandVerifiable(out RunCommandOutput) bool {
	// Exit zero is not enough, and never was. A command stopped at a memory
	// ceiling exits non-zero, but a command stopped at one after its last
	// assertion passed could exit zero -- and either way it did not run to its
	// own end, so it demonstrates nothing.
	//
	// Known-incomplete rather than not-completed, because this predicate also
	// governs debt retirement and what the model is told: an executor too old
	// to speak the vocabulary cannot report a resource kill, and refusing
	// everything it says would disable verification for that deployment
	// without making it safer. Candidate authorization asks the stricter
	// question, in stagingApplyObservation, because that is where evidence
	// mints a licence.
	return out.ExitCode == 0 && !out.TimedOut && !executionKnownIncomplete(out.Outcome)
}

// runLocally executes a command only when the operator explicitly selects
// host verification (ATLAS_VERIFY_IN=host). Sandbox outages never route
// here implicitly. Host mode removes the container backstop entirely:
// the only thing between model output and the host shell is
// validateShellCommand's catastrophe-only blocklist.
func runLocally(command, cwd string, timeout time.Duration) RunCommandOutput {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// The same address-space ceiling the sandbox installs, from the same
	// operator value, applied by the shell before it runs anything: `ulimit
	// -v` sets RLIMIT_AS, which is inherited across fork and exec, so it
	// covers whatever the command spawns. Host mode removes the container
	// backstop by design; it does not get to remove the memory one too.
	//
	// What it cannot do is TELL the two apart afterwards. There is no sampler
	// here, so a command that dies of MemoryError exits 1 like a failing test,
	// and this route reports unclassified rather than claiming a completion it
	// cannot demonstrate -- which is why host execution can run commands and
	// cannot produce verification evidence.
	cmd := exec.CommandContext(ctx, "bash", "-c",
		fmt.Sprintf("ulimit -v %d 2>/dev/null; %s", hostAddressSpaceKiB(), command))
	cmd.Dir = cwd
	// Its own process group, so the kill below reaches what it started.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	defer func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	var exitCode int
	timedOut := false
	if ctx.Err() == context.DeadlineExceeded {
		exitCode = 124
		timedOut = true
		stderr.WriteString(fmt.Sprintf("\nCommand timed out after %s", timeout))
	} else if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			stderr.WriteString(err.Error())
			exitCode = 1
		}
	}

	so, se := stdout.String(), stderr.String()
	outStr, errStr := truncateStr(so, 8000), truncateStr(se, 4000)
	return RunCommandOutput{
		Stdout: outStr, Stderr: errStr, ExitCode: exitCode,
		// Structural on this path too: the deadline is what this side observed,
		// not something a reader has to find in the message it appended.
		TimedOut:        timedOut,
		OutputTruncated: len(outStr) < len(so) || len(errStr) < len(se),
	}
}

// ---------------------------------------------------------------------------
// Per-file tier classification for V3 pipeline activation
// ---------------------------------------------------------------------------

// classifyFileTier determines whether a specific write_file call should
// route through the V3 pipeline (T2) or write directly (T1).
//
// T1 (direct write): config files, data files, boilerplate, CSS variables,
// JSON data, simple scripts under 30 lines with no complex logic.
//
// T2 (V3 pipeline): files with application logic, multiple functional
// requirements, framework-specific patterns, function definitions,
// event handlers, API logic, state management, conditional branching.
func classifyFileTier(filePath, content string) Tier {
	ext := strings.ToLower(filepath.Ext(filePath))
	base := strings.ToLower(filepath.Base(filePath))
	lines := strings.Count(content, "\n") + 1

	// Always T1: config files by name
	configFiles := []string{
		"package.json", "tsconfig.json", "next.config.js", "next.config.ts",
		"next.config.mjs", "tailwind.config.ts", "tailwind.config.js",
		"postcss.config.js", "postcss.config.mjs", "vite.config.ts",
		"vite.config.js", ".eslintrc.json", ".prettierrc", "jest.config.ts",
		"jest.config.js", "cargo.toml", "go.mod", "go.sum", "makefile",
		"cmakelists.txt", "pyproject.toml", "setup.py", "setup.cfg",
		"requirements.txt", "pipfile", ".editorconfig", ".gitignore",
		"dockerfile", "docker-compose.yml", "docker-compose.yaml",
	}
	for _, cf := range configFiles {
		if base == cf {
			return Tier1Simple
		}
	}

	// Always T1: data files
	dataExts := []string{".json", ".yaml", ".yml", ".toml", ".csv", ".xml", ".env"}
	for _, de := range dataExts {
		if ext == de {
			return Tier1Simple
		}
	}

	// Always T1: CSS/style files
	if ext == ".css" || ext == ".scss" || ext == ".less" {
		return Tier1Simple
	}

	// Always T1: markdown, text
	if ext == ".md" || ext == ".txt" || ext == ".rst" {
		return Tier1Simple
	}

	// Always T1: shell scripts (usually boilerplate)
	if ext == ".sh" || ext == ".bash" {
		return Tier1Simple
	}

	// Trivially tiny files → T1 always. Below 10 lines there's nothing
	// for V3 to meaningfully diversify on (the prior 50-line floor was
	// too conservative — flask app.py with 7 routes is 33 lines and is
	// exactly the kind of file V3 should help with).
	if lines < 10 {
		return Tier1Simple
	}

	// Code files with any application logic → T2. Lower threshold than
	// before to catch small-but-routed files (flask blueprints, express
	// routers, etc.) that the previous 3-indicator rule missed.
	if hasLogicIndicators(content) {
		return Tier2Medium
	}

	// Source-code and markup extensions get the benefit of the doubt
	// at T2 even without obvious logic-pattern matches — naming a file
	// foo.py / foo.go / foo.html is itself a strong signal that V3's
	// diverse candidate generation is worth the cost. HTML / JSX
	// templates used to require ≥150 lines to clear the markup branch,
	// which made V3 silent on every typical flask/express template
	// (usually 30–120 lines). Now any file at ≥10 lines with a
	// recognized code/markup extension goes T2.
	codeExts := map[string]bool{
		".py": true, ".go": true, ".rs": true,
		".ts": true, ".tsx": true, ".js": true, ".jsx": true,
		".c": true, ".cpp": true, ".cc": true, ".h": true, ".hpp": true,
		".java": true, ".kt": true, ".swift": true,
		".rb": true, ".php": true,
		".vue": true, ".svelte": true,
		".html": true, ".htm": true,
	}
	if codeExts[ext] {
		return Tier2Medium
	}

	// Default: T1 for unknown extensions / pure markup we're not sure about.
	return Tier1Simple
}

// cyclomaticComplexity calls v3-service /internal/cyclomatic_complexity.
// Returns (cc, true) when the service computed a real number, (0, false)
// for any failure mode (unsupported language, parse error, network down,
// timeout). Fail-soft is intentional — the existing regex-based
// hasLogicIndicators stays the floor; CC only adds signal when available.
//
// GH #39 point 2. v1 supports Python only; HTML/JSON/etc. fall through
// to false here and the proxy uses the regex classifier.
func cyclomaticComplexity(ctx *AgentContext, path, source string) (int, bool) {
	if ctx == nil || ctx.V3URL == "" {
		return 0, false
	}
	body, err := json.Marshal(map[string]interface{}{"path": path, "source": source})
	if err != nil {
		return 0, false
	}
	reqCtx, cancel := context.WithTimeout(ctx.Ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "POST",
		ctx.V3URL+"/internal/cyclomatic_complexity", bytes.NewReader(body))
	if err != nil {
		return 0, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, false
	}
	var r struct {
		OK bool `json:"ok"`
		CC int  `json:"cyclomatic_complexity"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || !r.OK {
		return 0, false
	}
	return r.CC, true
}

// refineTierWithCC bumps an existing tier upward when McCabe CC reveals
// more branching than the regex classifier could see. Never downgrades —
// the regex classifier is the floor, CC only escalates.
//
// Thresholds:
//
//	CC ≥ 16 → Tier3Hard  — definitely needs full V3 + best-of-K
//	CC ≥  8 → Tier2Medium — moderate branching, V3 likely helps
//	CC <  8 → leave base tier unchanged
//
// Calibrated against the snake/app.py family: a flask file with 8 routes
// runs at CC≈9 (one branch per route) and the regex already classifies
// it T2; a control-flow-heavy parser with nested ifs at CC≈18 should
// jump to T3 even if the regex landed it at T2.
func refineTierWithCC(base Tier, cc int) Tier {
	if cc >= 16 && base < Tier3Hard {
		return Tier3Hard
	}
	if cc >= 8 && base < Tier2Medium {
		return Tier2Medium
	}
	return base
}

// editWarrantsV3 decides whether a successful, surgical edit should be run
// back through the V3 whole-file pipeline. edit_file and structural_edit both
// produce content the model specified precisely — an exact old→new string
// swap, or a named tree-sitter node replacement — so the result is already
// what was asked for. V3's PlanSearch generates and build-verifies whole-
// file alternatives: worthwhile when a file is large or genuinely complex,
// but on a small, low-complexity file it spends minutes (single-threaded
// v3-service, reasoning-heavy models) only to reproduce the same edit. The
// file tier alone can't distinguish these — a 9-line calc.py classifies
// Tier2 like a 400-line module — so gate additionally on the resulting
// file's complexity and size. Trivial edits then apply instantly while V3
// still engages where a file is substantial enough to benefit. Keys off
// the code, not the model, so it stays model-agnostic.
//
// ccOK is false when complexity couldn't be measured (non-code or parser
// miss); fall back to a line-count bar so we neither always-run nor
// never-run on unmeasurable files.
func editWarrantsV3(content string, cc int, ccOK bool) bool {
	if ccOK && cc >= 8 {
		return true
	}
	lines := strings.Count(content, "\n") + 1
	return lines >= 80
}

// hasLogicIndicators checks if content contains signs of real application logic
// that would benefit from V3 pipeline's diverse candidate generation.
func hasLogicIndicators(content string) bool {
	// Count logic indicators
	indicators := 0
	logicPatterns := []string{
		// Function/method definitions
		"def ", "func ", "function ", "fn ", "async ",
		// Control flow
		"if ", "else ", "switch ", "match ", "for ", "while ",
		// Error handling
		"try ", "catch ", "except ", "throw ", "raise ",
		// Flask / FastAPI / Django routing — was missing before, which
		// caused a 33-line app.py with 7 @app.route handlers to register
		// only one indicator ("def ") and fall through to T1.
		"@app.route", "@app.get", "@app.post", "@app.put", "@app.delete",
		"@blueprint", "render_template", "url_for", "request.method",
		"flask.", "from flask",
		// Express / Node API patterns
		"export default", "export async", "module.exports",
		"app.get", "app.post", "app.put", "app.delete",
		"router.", "handler",
		"NextResponse", "Response(", "Request",
		// State/data management
		"useState", "useEffect", "useRef", "useCallback",
		"setState", "dispatch", "reducer",
		// Validation
		"validate", "schema", "parse", "zod.",
		// Database
		"query(", "insert(", ".select(", ".update(",
		// JSX / React component patterns
		"return (", "return <",
		"className=", "onClick", "onChange", "onSubmit",
		".map(", ".filter(", ".reduce(",
		// Multiple imports (sign of real component)
		"import {",
	}

	for _, p := range logicPatterns {
		if strings.Contains(content, p) {
			indicators++
		}
	}

	// 2+ logic indicators → has real application logic. Lowered from 3
	// because the original threshold was tuned for large files and
	// caused small-but-real apps (e.g. a flask routing module) to slip
	// through to T1 even though V3 would have helped.
	return indicators >= 2
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// resolvePath resolves a relative path against the working directory.
//
// Absolute paths pass through unchanged — but the model frequently
// emits the host-side absolute path it saw in the user's prompt
// (e.g. "/home/isaac/snake/app.py") which doesn't exist inside the
// proxy container. Use resolveAgentPath when you have an
// AgentContext available — it translates host paths to container
// paths via ctx.HostWorkingDir. resolvePath is the lower-level
// primitive kept for sites that don't have a context (e.g. V3
// adapter helpers).
func resolvePath(path, workingDir string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(workingDir, path))
}

// resolveAgentPath is the path resolver every tool handler should
// use. It first translates host-side absolute paths into the
// container path (when HostWorkingDir is set and the input falls
// inside that prefix), then resolves the result against
// ctx.WorkingDir. This is what makes the agent forgiving when the
// user pastes "/home/isaac/snake/app.py" into a prompt — the model
// copies the absolute path, the proxy rewrites it to /workspace/app.py,
// and read_file actually finds the file.
// redundantReadShortCircuit returns a compact synthetic result when the
// model asks to read a file it has ALREADY read this session and the
// content on disk is unchanged. A weak model frequently re-reads the same
// file several times before acting; each re-read re-injects the full file
// into the conversation and — on sliding-window-attention models that
// can't reuse llama.cpp's KV cache — forces a full re-encode of the whole
// prompt, costing tens of seconds for zero new information. Serving a
// short "already in context, act now" pointer keeps the working set small
// and steers toward the edit.
//
// Only whole-file reads short-circuit (a paged read with offset/limit is
// the model deliberately fetching a different span, so let it through),
// and only when the on-disk content byte-matches what was already shown
// (so a re-read after an edit still returns the new content). Returns nil
// to mean "no short-circuit — execute the read normally." Model-agnostic:
// keys off whether this exact content was already served, not the model.
func redundantReadShortCircuit(name string, args json.RawMessage, ctx *AgentContext) *ToolResult {
	if name != "read_file" {
		return nil
	}
	if envOr("ATLAS_DEDUP_READS", "1") == "0" {
		return nil
	}
	var input ReadFileInput
	if err := json.Unmarshal(args, &input); err != nil {
		return nil
	}
	if input.Offset != nil || input.Limit != nil || strings.TrimSpace(input.Path) == "" {
		return nil
	}
	path, err := resolveWorkspacePath(ctx, input.Path)
	if err != nil {
		return &ToolResult{Success: false, Error: "read_file: " + err.Error()}
	}
	ctx.mu.Lock()
	prev, ok := ctx.FilesRead[path]
	cacheEntries := len(ctx.FilesRead)
	ctx.mu.Unlock()
	if !ok {
		// Diagnostic: a re-read that SHOULD have been cached but wasn't.
		// Logged only when some other entry exists (first read of a file
		// is the normal case and not worth a line).
		if cacheEntries > 0 {
			log.Printf("[read-dedup] no cache entry for %q (have %d entries) — serving real read", truncateStr(path, 240), cacheEntries)
		}
		return nil
	}
	data, _, err := readWorkspaceFile(ctx, input.Path)
	if err != nil || string(data) != prev {
		if err == nil {
			log.Printf("[read-dedup] %q changed on disk since last read (%dB -> %dB) — serving real read", truncateStr(path, 240), len(prev), len(data))
		}
		return nil // changed or unreadable — let the real read run
	}
	// CRITICAL: only short-circuit to a pointer if the content is STILL in
	// the live conversation. After conversation trimming drops the original
	// read, "its content is above" becomes a lie — the model has nothing,
	// edits blind, and (if weak) hallucinates symbols/lines that aren't in
	// the file. When the content has been trimmed out, return nil so the
	// real read re-serves the full file. Verified failure mode: a model editing
	// function:count_items / old_str="return len(items)" against a file
	// containing neither, reasoning "I don't see the file content."
	if !fileContentInContext(ctx, prev) {
		log.Printf("[read-dedup] %q content was trimmed from context — re-serving full read", truncateStr(path, 240))
		return nil
	}
	out := ReadFileOutput{
		Content:    fmt.Sprintf("(You already read %s earlier in this session and it has not changed — its full content is above in the conversation. Do not read it again. Make your edit now with structural_edit or edit_file.)", input.Path),
		TotalLines: strings.Count(prev, "\n") + 1,
		StartLine:  1,
		EndLine:    strings.Count(prev, "\n") + 1,
	}
	b, _ := json.Marshal(out)
	return &ToolResult{Success: true, Data: b}
}

// fileContentInContext reports whether a file's content is still present in
// the live (post-trim) conversation. Conservative: an empty/short-content file
// probes as "present" (nothing to lose).
//
// The probe must survive JSON escaping. Tool results are stored in the
// conversation via ToolResult.MarshalText (json.Marshal), so the file content
// lives in ctx.Messages with `"`→`\"`, newlines→`\n`, tabs→`\t`. The old probe
// (the longest raw LINE) failed for any file whose longest line contained a
// double quote — e.g. a flask app's embedded HTML/JS — producing a false
// "trimmed" verdict that made the dedup re-serve the whole file every read and
// the model loop on read_file. Instead, probe with the longest maximal run of
// characters JSON does NOT escape (no `"`, `\`, or \n/\r/\t); that run is byte-
// identical in both the raw file and the escaped conversation copy.
func fileContentInContext(ctx *AgentContext, content string) bool {
	probe := ""
	for _, run := range strings.FieldsFunc(content, func(r rune) bool {
		return r == '"' || r == '\\' || r == '\n' || r == '\r' || r == '\t'
	}) {
		t := strings.TrimSpace(run)
		if len(t) > len(probe) {
			probe = t
		}
	}
	if len(probe) < 12 {
		return true // too short to probe reliably; don't churn re-reads
	}
	for _, m := range ctx.Messages {
		if strings.Contains(m.Content, probe) {
			return true
		}
	}
	return false
}

func resolveAgentPath(ctx *AgentContext, path string) string {
	// Defensive prefix strip. The local model frequently
	// emits `workspace/app.py` (no leading slash) when it means the
	// project root. Without this, resolvePath joins it onto cwd and
	// produces `/workspace/workspace/app.py`, which 404s. Strip the
	// `workspace/` prefix when WorkingDir is exactly `/workspace`.
	// Also handles a bare `workspace` (no trailing slash) for
	// list_directory.
	if ctx.WorkingDir == "/workspace" {
		switch {
		case path == "workspace":
			path = "."
		case strings.HasPrefix(path, "workspace/"):
			path = strings.TrimPrefix(path, "workspace/")
		case strings.HasPrefix(path, "./workspace/"):
			path = strings.TrimPrefix(path, "./workspace/")
		}
	}
	if filepath.IsAbs(path) && ctx.HostWorkingDir != "" {
		clean := filepath.Clean(path)
		host := filepath.Clean(ctx.HostWorkingDir)
		if clean == host {
			return filepath.Clean(ctx.WorkingDir)
		}
		// Match `host` as a directory prefix — require the next character
		// to be a separator so "/home/isaac/snakebar" doesn't match
		// "/home/isaac/snake".
		if strings.HasPrefix(clean, host+string(filepath.Separator)) {
			rel := strings.TrimPrefix(clean, host+string(filepath.Separator))
			translated := filepath.Join(ctx.WorkingDir, rel)
			return filepath.Clean(translated)
		}
	}
	return resolvePath(path, ctx.WorkingDir)
}

// v3StageToEvent maps a V3 pipeline stage name to the TUI event type
// it should fire. Stages cluster by phase: PlanSearch / DivSampling /
// Sandbox / selection / Phase 3 each get a dedicated event type so the
// TUI can render specialized rows (counters, per-test results, strategy
// choice) instead of a generic "v3_progress" string. Unknown stages fall
// back to v3_progress.
//
// Names are intentionally short — they cross the SSE wire on every
// pipeline stage transition (a typical T2 run emits 15–30 of them).
func v3StageToEvent(stage string) string {
	switch stage {
	case "phase1", "phase2", "phase2_allocated":
		return "v3_phase"
	case "plansearch", "plansearch_done", "plansearch_error":
		return "v3_plansearch"
	case "divsampling", "divsampling_done", "divsampling_error",
		"divsampling_stop":
		return "v3_divsampling"
	case "sandbox_test", "sandbox_pass", "sandbox_fail", "sandbox_done":
		return "v3_sandbox"
	case "selected":
		return "v3_select"
	case "phase3", "pr_cot", "pr_cot_pass", "pr_cot_failed", "pr_cot_error",
		"refinement", "refinement_pass", "refinement_failed", "refinement_error",
		"refinement_skip", "fallback", "fallback_all_vetoed", "fallback_unverified",
		"budget_exhausted", "budget_no_verified_candidate":
		return "v3_repair"
	case "probe", "probe_light", "probe_retry", "probe_failed",
		"probe_scored", "probe_sandbox", "probe_pass", "probe_error":
		return "v3_probe"
	case "self_test_gen", "self_test_done", "self_test_error",
		"self_test_skip", "self_test_verify":
		return "v3_self_test"
	case "plan_start", "plan_candidate", "plan_candidate_unparseable",
		"plan_candidate_error", "plan_candidate_scored", "plan_selected",
		"plan_failed":
		// All plan-pipeline stages collapse to one TUI event family. The
		// TUI reads the stage name off the payload to decide between
		// "scoring..." spinner, "winner: plan #N" summary, etc.
		return "v3_plan"
	case "lens_per_step":
		// Per-token lens scoring of each V3 candidate. TUI
		// surfaces first_off_rails_idx + gx_score_min so the user can see
		// WHERE a candidate's quality cratered. Without this case the
		// event flattens to v3_progress and the structured payload is lost.
		return "v3_lens_per_step"
	case "lens_veto":
		// V3 hard-rejected a sandbox-passing candidate
		// because the lens flagged it as a stub (gx_min < severe threshold).
		// Surfaced as its own event so the user can see "sandbox said pass
		// but lens vetoed" rather than burying it in v3_progress.
		return "v3_lens_veto"
	case "structural_veto":
		// GH #39 point 1: V3 hard-rejected a sandbox-passing candidate
		// because tree-sitter found unresolved direct-identifier calls.
		// Sandbox passes for code with try/except ImportError fallbacks
		// or dead branches; structural verification doesn't care whether
		// the unresolved call executes, only that it can't resolve.
		return "v3_structural_veto"
	case "call_chain_context":
		// GH #39 point 3: V3 phase-3 repair built a call-chain context
		// block for the failing function before invoking PR-CoT /
		// refinement. Informational — not a veto, just shows the user
		// that the repair phase has structural context the bare stderr
		// doesn't include.
		return "v3_call_chain_context"
	}
	return "v3_progress"
}

// truncateStr limits a string to maxLen characters.
// (truncateStr() already exists in main.go for backward compat)
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// ---------------------------------------------------------------------------
// Background commands
// ---------------------------------------------------------------------------
//
// Three tools wrap the sandbox /jobs/* endpoints. The pattern the model
// learns is: run_background(server) → tail_background or curl via
// run_command → stop_background. Without these, foreground servers
// (flask, npm start, cargo run) can't be verified — they don't exit
// and the model invents `timeout 5 ... || true` workarounds that tear
// the server down before any probe can hit it.
//
// All three tools require ATLAS_VERIFY_IN=sandbox (the default). Host
// mode bypasses the sandbox entirely; running long-lived processes on
// the host without any reaping is a foot-gun we don't want to ship,
// so we surface a clear error instead.

func runBackgroundTool() *ToolDef {
	return &ToolDef{
		Name:        "run_background",
		Effect:      ToolEffectCommandUnobserved,
		Description: "Start a long-running command (server, watcher, etc.) in the background and return a job_id. Use for `python app.py`, `npm start`, `cargo run`, `flask run` — anything that doesn't exit. Returns initial stdout/stderr captured during a brief settle window so you can confirm startup. Pair with run_command/curl to probe the running service, then stop_background to clean up.",
		InputSchema: RunBackgroundInput{},
		ReadOnly:    false,
		Destructive: false,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (*ToolResult, error) {
			var input RunBackgroundInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}

			// Trust gate: same contract as run_command — untrusted mode
			// refuses all command execution, foreground or background.
			if !ctx.TrustMode.commandsAllowed() {
				return &ToolResult{Success: false, Error: untrustedRefusal}, nil
			}

			if strings.TrimSpace(input.Command) == "" {
				return &ToolResult{Success: false, Error: "run_background: command cannot be empty"}, nil
			}
			if reason := validateShellCommand(input.Command); reason != "" {
				return &ToolResult{Success: false, Error: reason}, nil
			}
			if ctx.VerifyOnHost {
				return &ToolResult{
					Success: false,
					Error:   "run_background is only available in sandbox mode (ATLAS_VERIFY_IN=sandbox). On the host, use `run_command` with `nohup ... &` and track the PID yourself.",
				}, nil
			}
			cwd := ctx.WorkingDir
			if input.Cwd != "" {
				cwd = resolveAgentPath(ctx, input.Cwd)
			}
			settleMs := 1500
			if input.SettleMs != nil {
				settleMs = *input.SettleMs
				if settleMs < 0 {
					settleMs = 0
				} else if settleMs > 10000 {
					settleMs = 10000
				}
			}
			jobID, pid, err := sandboxStartBackground(ctx, input.Command, cwd)
			if err != nil {
				return &ToolResult{Success: false, Error: fmt.Sprintf("sandbox start failed: %v", err)}, nil
			}
			// Settle window — give the process time to bind a port, fail
			// to import, etc., before we hand back to the model.
			time.Sleep(time.Duration(settleMs) * time.Millisecond)
			tail, _ := sandboxTailBackground(ctx, jobID, 50)
			out := RunBackgroundOutput{
				JobID:   jobID,
				PID:     pid,
				Stdout:  tail.Stdout,
				Stderr:  tail.Stderr,
				Running: tail.Running,
			}
			if !tail.Running {
				out.ExitCode = tail.ExitCode
			}
			if ctx.BackgroundJobs == nil {
				ctx.BackgroundJobs = make(map[string]string)
			}
			if tail.Running {
				ctx.BackgroundJobs[jobID] = input.Command
				if ctx.BackgroundJobStarted == nil {
					ctx.BackgroundJobStarted = map[string]time.Time{}
				}
				ctx.BackgroundJobStarted[jobID] = time.Now()
			}
			outBytes, _ := json.Marshal(out)
			// A job that has already exited non-zero did not start. Reporting
			// that as a success is how the failure got lost: measured on
			// flask_pause (cycles 3-5), `python app.py` exited 1 inside the
			// settle window with ModuleNotFoundError in its stderr, the result
			// came back success=true with no error, and the run went on to
			// probe a port nothing was listening on -- three times, then gave
			// up. run_command reports a failing command as a failure; so does
			// this now.
			if !tail.Running && tail.ExitCode != nil && *tail.ExitCode != 0 {
				return &ToolResult{Success: false, Data: outBytes,
					Error: immediateExitMessage(ctx, input.Command, out)}, nil
			}
			return &ToolResult{Success: true, Data: outBytes}, nil
		},
	}
}

func tailBackgroundTool() *ToolDef {
	return &ToolDef{
		Name:        "tail_background",
		Effect:      ToolEffectReadOnly,
		Description: "Read the recent stdout/stderr of a background job started via run_background. Returns the last N lines of each stream (default 50), the run state (running/exited), and the exit code if applicable. Use to check whether a server is still up, watch test runner output, or read the failure traceback after a crash.",
		InputSchema: TailBackgroundInput{},
		ReadOnly:    true,
		Destructive: false,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (*ToolResult, error) {
			var input TailBackgroundInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}
			if strings.TrimSpace(input.JobID) == "" {
				return &ToolResult{Success: false, Error: "tail_background: job_id required"}, nil
			}
			lines := 50
			if input.Lines != nil {
				lines = *input.Lines
				if lines < 1 {
					lines = 1
				} else if lines > 500 {
					lines = 500
				}
			}
			out, err := sandboxTailBackground(ctx, input.JobID, lines)
			if err != nil {
				return &ToolResult{Success: false, Error: err.Error()}, nil
			}
			outBytes, _ := json.Marshal(out)
			return &ToolResult{Success: true, Data: outBytes}, nil
		},
	}
}

func stopBackgroundTool() *ToolDef {
	return &ToolDef{
		Name:        "stop_background",
		Effect:      ToolEffectCommandUnobserved,
		Description: "Stop a background job started via run_background. Sends SIGTERM, waits briefly, then SIGKILL if needed. Returns the final stdout/stderr buffer. Always call this when you're done with a background job — leaving them running blocks future job slots.",
		InputSchema: StopBackgroundInput{},
		ReadOnly:    false,
		Destructive: true,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (*ToolResult, error) {
			var input StopBackgroundInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}
			if strings.TrimSpace(input.JobID) == "" {
				return &ToolResult{Success: false, Error: "stop_background: job_id required"}, nil
			}
			out, err := sandboxStopBackground(ctx, input.JobID)
			if err != nil {
				return &ToolResult{Success: false, Error: err.Error()}, nil
			}
			delete(ctx.BackgroundJobs, input.JobID)
			outBytes, _ := json.Marshal(out)
			return &ToolResult{Success: true, Data: outBytes}, nil
		},
	}
}

// sandboxStartBackground POSTs to /jobs/start. Returns (job_id, pid, err).
func sandboxStartBackground(ctx *AgentContext, command, cwd string) (string, int, error) {
	if ctx.SandboxURL == "" {
		return "", 0, fmt.Errorf("ATLAS_SANDBOX_URL not configured")
	}
	body, _ := json.Marshal(map[string]interface{}{"command": command, "cwd": cwd})
	reqCtx := ctx.Ctx
	if reqCtx == nil {
		reqCtx = context.Background()
	}
	req, err := http.NewRequestWithContext(reqCtx, "POST", ctx.SandboxURL+"/jobs/start", bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var d struct {
			Detail string `json:"detail"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&d)
		if d.Detail != "" {
			return "", 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, d.Detail)
		}
		return "", 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out struct {
		JobID string `json:"job_id"`
		PID   int    `json:"pid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", 0, err
	}
	return out.JobID, out.PID, nil
}

func sandboxTailBackground(ctx *AgentContext, jobID string, lines int) (TailBackgroundOutput, error) {
	if ctx.SandboxURL == "" {
		return TailBackgroundOutput{}, fmt.Errorf("ATLAS_SANDBOX_URL not configured")
	}
	url := fmt.Sprintf("%s/jobs/%s/output?lines=%d", ctx.SandboxURL, jobID, lines)
	reqCtx := ctx.Ctx
	if reqCtx == nil {
		reqCtx = context.Background()
	}
	req, err := http.NewRequestWithContext(reqCtx, "GET", url, nil)
	if err != nil {
		return TailBackgroundOutput{}, err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return TailBackgroundOutput{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return TailBackgroundOutput{}, fmt.Errorf("unknown job_id %q (already cleaned up?)", jobID)
	}
	if resp.StatusCode != 200 {
		return TailBackgroundOutput{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out TailBackgroundOutput
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return TailBackgroundOutput{}, err
	}
	return out, nil
}

func sandboxStopBackground(ctx *AgentContext, jobID string) (StopBackgroundOutput, error) {
	if ctx.SandboxURL == "" {
		return StopBackgroundOutput{}, fmt.Errorf("ATLAS_SANDBOX_URL not configured")
	}
	url := fmt.Sprintf("%s/jobs/%s/stop", ctx.SandboxURL, jobID)
	reqCtx := ctx.Ctx
	if reqCtx == nil {
		reqCtx = context.Background()
	}
	req, err := http.NewRequestWithContext(reqCtx, "POST", url, nil)
	if err != nil {
		return StopBackgroundOutput{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return StopBackgroundOutput{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return StopBackgroundOutput{}, fmt.Errorf("unknown job_id %q", jobID)
	}
	if resp.StatusCode != 200 {
		return StopBackgroundOutput{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out StopBackgroundOutput
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return StopBackgroundOutput{}, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// JSON Schema generation for constrained output
// ---------------------------------------------------------------------------

// buildToolCallSchema generates the JSON Schema that describes the valid output
// format: exactly one of tool_call, text, or done.
//
// The actual constraint is enforced by response_format: json_object in the
// LLM request. This schema is available for reference but not directly
// passed to llama-server.
func buildToolCallSchema() map[string]interface{} {
	return buildToolCallSchemaForTools(nil)
}

// buildToolCallSchemaForTools is the same as buildToolCallSchema, but with
// the named tools removed from the tool-name enum. Used by callLLMConstrained
// to ban edit_file/write_file for a single decision step after a write_file
// rejection on .py/.html (BiasBusters #3, May 2026 research synthesis).
func buildToolCallSchemaForTools(excluded []string) map[string]interface{} {
	excludeSet := make(map[string]struct{}, len(excluded))
	for _, name := range excluded {
		excludeSet[name] = struct{}{}
	}
	// allTools() order, so the enum is the same sequence every request.
	toolNames := make([]interface{}, 0, len(toolRegistry))
	for _, tool := range allTools() {
		if _, skip := excludeSet[tool.Name]; skip {
			continue
		}
		toolNames = append(toolNames, tool.Name)
	}

	return map[string]interface{}{
		"oneOf": []interface{}{
			// Tool call variant
			map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"type": map[string]interface{}{
						"type": "string",
						"enum": []string{"tool_call"},
					},
					"name": map[string]interface{}{
						"type": "string",
						"enum": toolNames,
					},
					"args": map[string]interface{}{
						"type": "object",
					},
				},
				"required":             []string{"type", "name", "args"},
				"additionalProperties": false,
			},
			// Text variant
			map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"type": map[string]interface{}{
						"type": "string",
						"enum": []string{"text"},
					},
					"content": map[string]interface{}{
						"type": "string",
					},
				},
				"required":             []string{"type", "content"},
				"additionalProperties": false,
			},
			// Done variant
			map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"type": map[string]interface{}{
						"type": "string",
						"enum": []string{"done"},
					},
					"summary": map[string]interface{}{
						"type": "string",
					},
				},
				"required":             []string{"type", "summary"},
				"additionalProperties": false,
			},
		},
	}
}

// buildResponseFormat picks the response_format payload to send to
// llama-server based on ATLAS_GRAMMAR_MODE (#33).
//
//	"strict" (default): {"type":"json_object","schema":<full schema>}.
//	  llama-server converts the schema to internal GBNF at the C side
//	  so the token sampler can ONLY emit our tool_call/text/done union.
//	  Previously the model could emit any valid JSON and we'd reject
//	  + retry post-hoc, burning tokens; the schema-constrained path
//	  eliminates that whole class of waste.
//
//	"loose": {"type":"json_object"} — old behavior, "valid JSON only,
//	  shape not enforced." Kept as an escape hatch in case a model
//	  handles the schema-to-GBNF conversion poorly (rare, but a
//	  one-env-var rollback beats a code revert).
//
// Returns an interface{} because the strict case nests a map (the
// schema), which doesn't fit map[string]string.
//
// effectiveGrammarMode is the mode this process applies: "loose" when
// ATLAS_GRAMMAR_MODE says so, "strict" otherwise. /version reports it, so a
// measurement can record the configuration it ran against.
func effectiveGrammarMode() string {
	if os.Getenv("ATLAS_GRAMMAR_MODE") == "loose" {
		return "loose"
	}
	return "strict"
}

func buildResponseFormat() interface{} {
	if effectiveGrammarMode() == "loose" {
		return map[string]string{"type": "json_object"}
	}
	return map[string]interface{}{
		"type":   "json_object",
		"schema": buildToolCallSchema(),
	}
}

// ---------------------------------------------------------------------------
// GBNF Grammar fallback
// ---------------------------------------------------------------------------

// buildGBNFGrammarForTools generates a GBNF grammar string constraining
// output to the tool_call/text/done union, with the listed tools
// removed from the tool-name production. May 2026 BiasBusters #2: when
// the next step must NOT use edit_file (e.g. write_file just got rejected
// on a .py/.html file >5 lines), llama-server enforces the restriction
// at the token-decode level via the `grammar` parameter — descriptions
// and system prompt rules can be ignored by the model, but the grammar
// physically cannot emit the banned tool name.
func buildGBNFGrammarForTools(excluded []string) string {
	excludeSet := make(map[string]struct{}, len(excluded))
	for _, name := range excluded {
		excludeSet[name] = struct{}{}
	}

	var sb strings.Builder

	// Root: one of the three response types
	sb.WriteString("root ::= tool-call | text-response | done-response\n\n")

	// Tool call
	// allTools() order, so the alternation is the same sequence every request.
	// The set of alternatives is unchanged; only their order is now fixed.
	toolNames := make([]string, 0, len(toolRegistry))
	for _, tool := range allTools() {
		if _, skip := excludeSet[tool.Name]; skip {
			continue
		}
		toolNames = append(toolNames, fmt.Sprintf(`"\"%s\""`, tool.Name))
	}

	sb.WriteString("tool-call ::= \"{\" ws ")
	sb.WriteString(`"\"type\"" ws ":" ws "\"tool_call\"" ws "," ws `)
	sb.WriteString(`"\"name\"" ws ":" ws tool-name ws "," ws `)
	sb.WriteString(`"\"args\"" ws ":" ws json-object ws `)
	sb.WriteString("\"}\"\n\n")

	// Tool name enum
	sb.WriteString("tool-name ::= ")
	sb.WriteString(strings.Join(toolNames, " | "))
	sb.WriteString("\n\n")

	// Text response
	sb.WriteString("text-response ::= \"{\" ws ")
	sb.WriteString(`"\"type\"" ws ":" ws "\"text\"" ws "," ws `)
	sb.WriteString(`"\"content\"" ws ":" ws json-string ws `)
	sb.WriteString("\"}\"\n\n")

	// Done response
	sb.WriteString("done-response ::= \"{\" ws ")
	sb.WriteString(`"\"type\"" ws ":" ws "\"done\"" ws "," ws `)
	sb.WriteString(`"\"summary\"" ws ":" ws json-string ws `)
	sb.WriteString("\"}\"\n\n")

	// JSON primitives
	sb.WriteString("json-object ::= \"{\" ws (json-pair (\",\" ws json-pair)*)? ws \"}\"\n")
	sb.WriteString("json-pair ::= json-string ws \":\" ws json-value\n")
	sb.WriteString("json-array ::= \"[\" ws (json-value (\",\" ws json-value)*)? ws \"]\"\n")
	sb.WriteString("json-value ::= json-string | json-number | json-object | json-array | \"true\" | \"false\" | \"null\"\n")
	sb.WriteString(`json-string ::= "\"" json-char* "\""` + "\n")
	sb.WriteString(`json-char ::= [^"\\] | "\\" ["\\/bfnrt] | "\\u" [0-9a-fA-F] [0-9a-fA-F] [0-9a-fA-F] [0-9a-fA-F]` + "\n")
	// Exponent sign is an optional + or -. The class must be [-+] (literal
	// minus, no escape) — the previous [\"+\\-\"] emitted `["+\-"]`, whose
	// `\-` is an illegal GBNF escape and crashed grammar parsing whenever this
	// rule was included (surfaced when banning run tools left a number-bearing
	// tool in the enum). GBNF needs no quotes inside a character class.
	sb.WriteString("json-number ::= \"-\"? [0-9]+ (\".\" [0-9]+)? ([eE] [-+]? [0-9]+)?\n")
	sb.WriteString("ws ::= [ \\t\\n]*\n")

	return sb.String()
}

// ---------------------------------------------------------------------------
// System prompt: tool descriptions for the model
// ---------------------------------------------------------------------------

// buildToolDescriptionsExcluding generates the tool documentation section
// with the listed tools removed entirely. Used for the per-step nudge
// note when edit_file/write_file must be banned for a single decision.
// The system prompt is built once per session and we don't rebuild it,
// but this is reused inside buildStepAdvisoryNote() to remind the model
// of the available palette without the banned tool present.
func buildToolDescriptionsExcluding(excluded []string) string {
	excludeSet := make(map[string]struct{}, len(excluded))
	for _, name := range excluded {
		excludeSet[name] = struct{}{}
	}
	var sb strings.Builder
	sb.WriteString("## Available Tools\n\n")
	sb.WriteString("You must respond with a JSON object in one of these formats:\n\n")
	sb.WriteString("**Tool call:** `{\"type\":\"tool_call\",\"name\":\"<tool>\",\"args\":{...}}`\n")
	sb.WriteString("**Text message:** `{\"type\":\"text\",\"content\":\"<message>\"}`\n")
	sb.WriteString("**Task complete:** `{\"type\":\"done\",\"summary\":\"<what you did>\"}`\n\n")

	for _, tool := range allTools() {
		if _, skip := excludeSet[tool.Name]; skip {
			continue
		}
		sb.WriteString(fmt.Sprintf("### %s\n", tool.Name))
		sb.WriteString(fmt.Sprintf("%s\n\n", tool.Description))
		sb.WriteString("**Input:**\n```json\n")

		// Generate example from input schema struct
		schemaJSON := generateInputExample(tool.Name)
		sb.WriteString(schemaJSON)
		sb.WriteString("\n```\n\n")
	}

	return sb.String()
}

// generateInputExample creates an example JSON for a tool's input.
func generateInputExample(toolName string) string {
	switch toolName {
	case "read_file":
		return `{"path": "src/main.py", "offset": 0, "limit": 100}`
	case "outline_file":
		return `{"path": "src/main.py"}`
	case "write_file":
		return `{"path": "src/main.py", "content": "#!/usr/bin/env python3\n..."}`
	// Both fell through to "{}" — the system prompt rendered the description
	// and an empty example for exactly the two tools that exist to be reached
	// for instead of edit_file.
	case "insert_after":
		return `{"path": "app.py", "line": 42, "content": "    log.info('added')"}`
	case "replace_lines":
		return `{"path": "app.py", "start_line": 42, "end_line": 47, "expected_first_line": "def handle(req):", "expected_last_line": "    return None", "content": "def handle(req):\n    ..."}`
	case "edit_file":
		// Real fix-style snippet — adding a None check, the most common
		// kind of small targeted edit. Models cargo-cult the example
		// shape, so a "rename foo to bar" placeholder steered them
		// toward purely cosmetic edits instead of real bug-fix shapes.
		return `{"path": "src/main.py", "old_str": "if x == 0:\n        return None", "new_str": "if x is None or x == 0:\n        return None", "replace_all": false}`
	case "structural_edit":
		// Whole-function rewrite — the case where edit_file would force
		// the model to copy the entire existing function as old_str and
		// blow through max_tokens. Selector grammar is intentionally
		// narrow in v1 (function:NAME, class:NAME, <tag>) to avoid the
		// raw-tree-sitter hallucination problem (GH #39 measurement).
		return `{"path": "src/main.py", "selector": "function:dashboard", "content": "@app.route('/dashboard')\ndef dashboard():\n    return render_template('dashboard.html')"}`
	case "delete_file":
		return `{"path": "old_file.py"}`
	case "run_command":
		return `{"command": "python3 src/main.py", "timeout": 30}`
	case "search_files":
		return `{"pattern": "def main", "path": "src/", "glob": "*.py"}`
	case "list_directory":
		return `{"path": "."}`
	default:
		return `{}`
	}
}

// editSyntaxObservation is the healthy->broken syntax gate the three
// content-edit tools share, as one structured observation.
//
// The bytes about to be written are evaluated ONCE, and the original only when
// those bytes demonstrably fail -- an edit may not INTRODUCE breakage, but a
// file already failing the checker stays editable, which is what makes
// repair-in-progress possible. The three tools had three copies of that rule;
// they now have one, and each supplies only its own model-facing wording.
//
// Returns the observation for the proposed bytes, and a classified refusal when
// the rule declines the edit. The observation describes the exact bytes the
// caller is about to write, so the caller attaches it to whichever outcome the
// filesystem produces rather than re-deriving it.
func editSyntaxObservation(ctx *AgentContext, tool, checkPath, relPath, original, edited string,
	rejection func(detail string) string) (checkOutcome, *ToolResult) {
	proposal := fallbackSyntaxOutcomeFor(ctx, checkPath, edited).aggregate()
	if proposal.Status != ValidationFailed {
		return proposal, nil
	}
	baseline := fallbackSyntaxOutcomeFor(ctx, checkPath, original).aggregate()
	if baselineAllowsRepair(baseline) {
		log.Printf("[%s] %s still unparsable after the edit (was already broken) — allowing repair-in-progress",
			tool, logPath(relPath))
		return proposal, nil
	}
	log.Printf("[%s] edited content for %s failed the syntax gate: %s",
		tool, logPath(relPath), truncateStr(proposal.Detail, 120))
	return proposal, &ToolResult{
		Success:          false,
		Error:            rejection(proposal.Detail),
		MutationStatus:   MutationRefused,
		ValidationKind:   ValidationKindSyntax,
		ValidationStatus: ValidationFailed,
		ValidationDetail: proposal.Detail,
	}
}

// structuralRefusal is the shared shape of the three later gates' verdicts:
// the content parses, and something about its structure does not. Syntax ran
// first and did not fail on these bytes, so the structural failure is the
// decisive one and recording it as syntax/failed would assert the opposite.
// fencedCallIsExecutable answers one question before any generation is spent:
// would this call survive the checks it has to survive anyway?
//
// The fenced channel exists to carry a file body around the JSON encoding, and
// it costs a full unconstrained generation per attempt. Starting one for a
// call that cannot execute -- no path, a path outside the workspace, a
// deny-listed target -- spends a minute of the session budget to produce
// content that is then thrown away, and a model that keeps re-sending the
// malformed call spends the whole session that way. Measured live: a 300s
// canary reached turn 36 having written nothing, every turn a write_file with
// `content: "@fenced"` and no path at all.
//
// It runs the SAME checks executeToolCall runs, by calling them, so the set of
// calls refused here is a subset of the set refused there. That is what lets
// the caller simply decline to resolve and hand the call to the tool, whose
// own producer owns the refusal, its wording and its MutationNone.
func fencedCallIsExecutable(name string, args json.RawMessage, ctx *AgentContext) (bool, string) {
	trimmed := strings.TrimSpace(string(args))
	if trimmed == "" || trimmed == "null" {
		return false, "the call carried no arguments"
	}
	var in WriteFileInput
	if err := json.Unmarshal(args, &in); err != nil {
		return false, "the arguments are not the shape " + name + " takes"
	}
	if strings.TrimSpace(in.Path) == "" {
		return false, "the call names no path"
	}
	if reason := validateToolWorkspacePaths(name, args, ctx); reason != "" {
		return false, reason
	}
	if denied, reason := shouldDenyToolCall(name, args); denied {
		return false, reason
	}
	return true, ""
}

// noMutation classifies a branch that ran, formed no bytes to write, and
// returned -- a malformed argument, an unmet precondition, an anchor that
// matched nothing, a replacement identical to what is already there. Disk is
// provably untouched and nothing was checked.
//
// Deliberately distinct from MutationRefused, which is a gate DECLINING bytes
// that were fully formed and could have landed. The difference is what a
// later policy needs: refused means a candidate existed and was judged;
// none means there was never anything to write.
func noMutation(msg string) *ToolResult {
	return &ToolResult{
		Success:          false,
		Error:            msg,
		MutationStatus:   MutationNone,
		ValidationKind:   ValidationKindNone,
		ValidationStatus: ValidationNotApplicable,
	}
}

// errNoMutation carries the same fact on the (nil, error) return shape, which
// the boundary turns into a failed ToolResult while preserving the producer's
// classification.
func errNoMutation(err error) error {
	return &classifiedError{err: err, mutationStatus: MutationNone,
		validationKind: ValidationKindNone, validationStatus: ValidationNotApplicable}
}

// refusedNoCheck classifies a guard that declined fully-formed bytes on a
// policy the guard can decide without reading them as code -- a shrinkage
// heuristic, a duplicate-edit guard. Nothing was validated, so the validation
// fields say so rather than borrowing the refusal as a verdict.
func refusedNoCheck(msg string) *ToolResult {
	return &ToolResult{
		Success:          false,
		Error:            msg,
		MutationStatus:   MutationRefused,
		ValidationKind:   ValidationKindNone,
		ValidationStatus: ValidationNotApplicable,
	}
}

// errFailedMutationSyntaxUnrun is the post-write failure of a tool whose file
// types all have a syntax checker that this tool never runs. The target may
// hold the new bytes, the old bytes, or a temp file, so the mutation is
// failed and no verdict is offered.
func errFailedMutationSyntaxUnrun(err error) error {
	return errFailedMutation(err, ValidationKindSyntax, ValidationNotRun)
}

// errFailedMutation is failedMutation without the extension lookup, for a
// producer that knows a write was attempted and left the target in an
// indeterminate state.
func errFailedMutation(err error, kind ValidationKind, status ValidationStatus) error {
	return &classifiedError{err: err, mutationStatus: MutationFailed,
		validationKind: kind, validationStatus: status}
}

func structuralRefusal(msg string) *ToolResult {
	return &ToolResult{
		Success:          false,
		Error:            msg,
		MutationStatus:   MutationRefused,
		ValidationKind:   ValidationKindStructural,
		ValidationStatus: ValidationFailed,
		ValidationDetail: msg,
	}
}

// editWriteFailure keeps an edit tool's real error while stating both facts:
// the mutation did not establish the intended state, and the observation made
// on those exact bytes still holds.
func editWriteFailure(path string, err error, observed checkOutcome) error {
	return overlayValidationOnError(failedMutation(path, err), observed)
}

// runEditPipeline is the V3 entry every content edit goes through: classify
// the file, and when the tier warrants it hand the composed result to the
// candidate pipeline, keeping the caller's version if the winner drifts
// outside the edit.
//
// It exists because it was inlined in edit_file and simply absent from
// insert_after and replace_lines, so the two line-addressed tools produced a
// single greedy sample with no candidate generation and no lens scoring — and
// those are the tools the guidance now steers toward. Adding an edit tool
// must not mean re-deciding whether the pipeline applies to it.
//
// Returns the content to write, the V3 metadata to report, and a non-nil
// ToolResult only when the turn was cancelled mid-pipeline (nothing may land
// on disk in that case).
func runEditPipeline(ctx *AgentContext, tool, path, relPath, original,
	edited string) editRouteOutcome {
	keep := editRouteOutcome{Content: edited}
	// Classify on max(old, new): a destructive edit that shrinks a T2+ file
	// into a T1 stub is exactly the edit that most needs checking, and
	// classifying on the result alone let it bypass the pipeline.
	fileTier := classifyFileTier(relPath, original)
	if newTier := classifyFileTier(relPath, edited); newTier > fileTier {
		fileTier = newTier
	}
	cc, ccOK := cyclomaticComplexity(ctx, relPath, edited)
	if ccOK {
		if refined := refineTierWithCC(fileTier, cc); refined != fileTier {
			log.Printf("[%s] %s tier %s→%s via cc=%d", tool, relPath, fileTier, refined, cc)
			fileTier = refined
		}
	}
	// One owner for whether the producer is consulted, and for why it was
	// not. Identical to the conditions it replaces -- including the debug
	// fast-track, which keeps the session's clock on executions rather than
	// candidates -- and now the skip says which threshold turned it away.
	bypass := editGenerationBypass(ctx, fileTier, editWarrantsV3(edited, cc, ccOK),
		isActiveDebugIteration(ctx, relPath))
	recordCandidateGenerationBypass(ctx, tool, bypass, fileTier,
		strings.Count(edited, "\n")+1)
	logBudgetBypass(tool, relPath, bypass)
	if bypass == bypassActiveDebugIteration {
		log.Printf("[%s] %s mid-debug iteration — skipping V3, execution is the feedback", tool, relPath)
	}
	if bypass != bypassNone {
		return keep
	}

	log.Printf("[%s] V3 pipeline activating for %s (file_tier=%d, req_tier=%d)", tool, relPath, fileTier, ctx.Tier)
	// THE protected edit route. A service proposal reaches disk only through
	// the same evidence, authorization, one-time grant, exact-byte delivery,
	// ledger, validation and settlement chain the new-file route uses; when it
	// does not earn that, the caller's own edit is what stays.
	return deliverEditCandidate(ctx, tool, path, relPath, original, edited)
}

// attachV3 copies pipeline metadata onto a successful tool result, so a
// candidate-verified edit reports as one wherever it came from.
func attachV3(result *ToolResult, meta V3EditMetadata) *ToolResult {
	if meta.Used {
		result.V3Used = true
		result.CandidatesTested = meta.CandidatesTested
		result.WinningScore = meta.WinningScore
		result.PhaseSolved = meta.PhaseSolved
		result.VerificationEvidence = meta.VerificationEvidence
	}
	return result
}

// sandboxJobState is one entry of the sandbox's /jobs listing. Only the
// fields this side reads are declared.
type sandboxJobState struct {
	JobID   string `json:"job_id"`
	Command string `json:"command"`
	Running bool   `json:"running"`
}

// sandboxListBackground reports every job the sandbox is holding.
//
// /jobs carries `running` per job, so completion is observable without
// reading a job's output — which matters because reading it is what
// tail_background does, and consuming output the model has not asked for
// would change what it sees next.
func sandboxListBackground(ctx *AgentContext) ([]sandboxJobState, error) {
	if ctx.SandboxURL == "" {
		return nil, fmt.Errorf("ATLAS_SANDBOX_URL not configured")
	}
	reqCtx := ctx.Ctx
	if reqCtx == nil {
		reqCtx = context.Background()
	}
	req, err := http.NewRequestWithContext(reqCtx, "GET", ctx.SandboxURL+"/jobs", nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out struct {
		Jobs []sandboxJobState `json:"jobs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Jobs, nil
}

// finishedBackgroundNote reports jobs this run started that have since
// exited, once each, and stops tracking them.
//
// A background job's outcome is invisible unless the model calls
// tail_background, so a server that died on startup looks identical to one
// serving happily: the run continues, the next probe fails for a reason
// nothing explains, and a session can finish claiming work it never
// verified. The foreground-server redirect pushes more work down this path,
// which makes the silence cost more.
//
// Only jobs this run started are reported — the sandbox registry is
// process-wide and outlives sessions, so another session's leftovers are
// not this run's news. Errors return "": a job listing that cannot be
// fetched must not interrupt the loop.
func finishedBackgroundNote(ctx *AgentContext) string {
	if ctx == nil || len(ctx.BackgroundJobs) == 0 {
		return ""
	}
	jobs, err := sandboxListBackground(ctx)
	if err != nil {
		return ""
	}
	state := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		state[j.JobID] = j.Running
	}
	var done []string
	for id := range ctx.BackgroundJobs {
		running, known := state[id]
		if known && !running {
			done = append(done, id)
		}
	}
	if len(done) == 0 {
		return ""
	}
	sort.Strings(done)
	var sb strings.Builder
	for _, id := range done {
		cmd := ctx.BackgroundJobs[id]
		// Reported once: drop it from tracking so the next turn does not
		// repeat the same news.
		delete(ctx.BackgroundJobs, id)
		fmt.Fprintf(&sb, "\nBackground job %s has exited — `%s` is no longer running. "+
			"Read its output with tail_background(%q) before relying on it; a server "+
			"that exited on startup and one that served correctly look the same from here.",
			id, truncateStr(cmd, 80), id)
	}
	return strings.TrimLeft(sb.String(), "\n")
}

// writeWithoutCandidate is the direct path: the caller's own bytes, checked
// and written, with no candidate anywhere in the story.
//
// Two callers reach it and they mean different things -- a V3 outage, and an
// invocation enforcement found no closure path for -- so each supplies its own
// message. What they share is everything after: the same fresh syntax check on
// exactly the bytes about to land, the same structural gate, the same write,
// and no V3 provenance, because nothing here was generated, verified or scored.
func writeWithoutCandidate(ctx *AgentContext, path, content, message string) (*ToolResult, error) {
	// A FRESH check, deliberately: the decision above can take time, and this
	// revalidates the exact content about to be written immediately before
	// writing it. The observation therefore describes the bytes that land,
	// with no window between the two.
	check := fallbackSyntaxOutcomeFor(ctx, path, content).aggregate()
	if check.Status == ValidationFailed {
		synErr := check.Detail
		log.Printf("[write_file] fallback content for %s failed syntax gate: %s",
			logPath(path), safeDiagnosticSummary(synErr))
		// Refused before any byte reached disk, on exactly the content that
		// would have been written.
		return &ToolResult{Success: false,
			Error:            fallbackSyntaxRejection(path, content, synErr),
			MutationStatus:   MutationRefused,
			ValidationKind:   ValidationKindSyntax,
			ValidationStatus: ValidationFailed,
			ValidationDetail: synErr}, nil
	}
	// #147: structural gate on the fallback too. It matters on the
	// DeadlineExceeded case -- /generate timed out but the service is up, so
	// /internal/structural_check (own 5s timeout) still answers; when the
	// service is genuinely down the gate fails open.
	if original, ok := readOriginalForGate(path); ok {
		if introduced := editIntroducesUnresolved(ctx, path, original, content); len(introduced) > 0 {
			log.Printf("[write_file] fallback content introduces unresolved call(s) %v in %s — rejecting",
				logPaths(introduced), logPath(path))
			// Syntax ran first and did not fail on these exact bytes, so the
			// structural failure is the decisive one. Recording it as
			// syntax/failed would assert the opposite of what happened.
			rejection := structuralWriteRejection(path, introduced)
			return &ToolResult{Success: false, Error: rejection,
				MutationStatus:   MutationRefused,
				ValidationKind:   ValidationKindStructural,
				ValidationStatus: ValidationFailed,
				ValidationDetail: rejection}, nil
		}
	}
	// The comparative gates every other write route applies. The producer
	// failing or timing out is no reason to skip them.
	if original, ok := readOriginalForGate(path); ok {
		if msg := embeddedScriptGate(ctx, path, original, content); msg != "" {
			log.Printf("[write_file] fallback content breaks an embedded script in %s — rejecting", logPath(path))
			return &ToolResult{Success: false, Error: msg,
				MutationStatus:   MutationRefused,
				ValidationKind:   ValidationKindStructural,
				ValidationStatus: ValidationFailed,
				ValidationDetail: msg}, nil
		}
		if msg := duplicateMainGuard(path, original, content); msg != "" {
			log.Printf("[write_file] fallback content duplicates the module entrypoint in %s — rejecting", logPath(path))
			return &ToolResult{Success: false, Error: msg,
				MutationStatus:   MutationRefused,
				ValidationKind:   ValidationKindStructural,
				ValidationStatus: ValidationFailed,
				ValidationDetail: msg}, nil
		}
	}
	logMandatoryChecks(ctx, "write_file", path, "producer fallback")
	if message != "" {
		ctx.Stream("text", map[string]string{"content": message})
	}
	res, err := writeFileRecorded(path, content, ctx)
	return applyRouteObservation(res, err, check)
}

// noteContentChange tells the model, in a successful result, about a change
// ATLAS made to the content it sent, so no such change is silent. It is a
// separate key from "warning", which marks a landed file as pending execution.
func noteContentChange(res *ToolResult, note string) {
	if res == nil || !res.Success || note == "" {
		return
	}
	var out map[string]interface{}
	if json.Unmarshal(res.Data, &out) != nil || out == nil {
		out = map[string]interface{}{}
	}
	out["content_note"] = note
	if b, err := json.Marshal(out); err == nil {
		res.Data = b
	}
}

// logBudgetBypass names, in the proxy log, a write whose optional candidate
// generation was skipped by the budget-ownership rules, so a run's log shows
// how many writes each rule turned away.
func logBudgetBypass(tool, relPath string, reason candidateBypassReason) {
	if reason == bypassCandidateUndeliverable || reason == bypassWorkAllowance {
		log.Printf("[%s] V3 skipped for %s: %s", tool, logPath(relPath), reason)
	}
}

// logMandatoryChecks records, just before a write lands, that the checks every
// write owes (syntax with healthy->broken, unresolved names, embedded script,
// duplicate entrypoint) were applied on this route. One line per write, so a
// run's log can be counted against its writes.
func logMandatoryChecks(ctx *AgentContext, tool, path, route string) {
	if route != "" {
		route = " (" + route + ")"
	}
	unavailable := ""
	if n := ctx.checkServiceFailureSummary(); n != "" {
		unavailable = "; could not run this session: " + n
	}
	log.Printf("[gates] %s %s: mandatory checks applied%s%s", tool, logPath(path), route, unavailable)
}

// selectorGuidanceOrOutline is selectorGuidance, or a pointer to outline_file
// for a language structural_edit does not handle.
func selectorGuidanceOrOutline(path, source string) string {
	if g := selectorGuidance(path, source); g != "" {
		return g
	}
	return "run outline_file on " + filepath.Base(path) + " to see what a selector can name"
}

// lineSpanOf names the lines of content where text first occurs, as
// "line N" or "lines N-M".
func lineSpanOf(content, text string) string {
	i := strings.Index(content, text)
	if i < 0 {
		return "the file"
	}
	first := strings.Count(content[:i], "\n") + 1
	last := first + strings.Count(strings.TrimSuffix(text, "\n"), "\n")
	if strings.HasPrefix(text, "\n") {
		first++
	}
	if last <= first {
		return fmt.Sprintf("line %d", first)
	}
	return fmt.Sprintf("lines %d-%d", first, last)
}

// prefixedEditRefusal explains an edit_file whose old_str matches the file only
// once read_file's "N<tab>" display prefixes are removed, and gives a
// replace_lines call that makes the change without reproducing the text. It
// computes, never applies: nothing in it is executed.
func prefixedEditRefusal(path, content, match, oldStr, newStr string) string {
	i := strings.Index(content, match)
	lines := strings.Split(content, "\n")
	body := strings.Trim(match, "\n")
	first := strings.Count(content[:i], "\n") + 1
	if strings.HasPrefix(match, "\n") {
		first++
	}
	last := first + strings.Count(body, "\n")
	var sb strings.Builder
	fmt.Fprintf(&sb, "edit_file was NOT applied — %s is unchanged. Your old_str carries read_file's line-number "+
		"prefix (\"12<tab>\") on %d line(s); that prefix is display only and is not in the file, and edits apply "+
		"old_str and new_str exactly as sent.", path, lineNumberPrefixedLines(oldStr))
	if n := strings.Count(content, match); n > 1 {
		fmt.Fprintf(&sb, " Without the prefixes it matches %d places, so it does not identify one; anchor on a line "+
			"that appears once.", n)
		return sb.String()
	}
	fmt.Fprintf(&sb, " Without the prefixes it matches %s of %s.", lineSpanOf(content, match), path)
	if m := lineNumberPrefixedLines(newStr); m > 0 {
		fmt.Fprintf(&sb, " Your new_str also has the prefix on %d line(s), and it would be written into the file "+
			"literally.", m)
	}
	if first >= 1 && last <= len(lines) {
		fmt.Fprintf(&sb, " To make this change, either resend edit_file with the prefixes removed from old_str and "+
			"new_str, or call replace_lines with path %q, start_line %d, end_line %d, expected_first_line %q, "+
			"expected_last_line %q, and content set to the replacement lines without prefixes.",
			path, first, last, strings.TrimSpace(lines[first-1]), strings.TrimSpace(lines[last-1]))
	}
	return sb.String()
}

// missingModulePatterns recognise "this environment does not have X" across the
// runtimes the sandbox carries. The name is echoed from the program's own
// output; nothing here installs anything or builds a command from it.
var missingModulePatterns = []*regexp.Regexp{
	regexp.MustCompile(`ModuleNotFoundError: No module named '([^']+)'`),
	regexp.MustCompile(`ImportError: cannot import name '([^']+)'`),
	regexp.MustCompile(`Error: Cannot find module '([^']+)'`),
	regexp.MustCompile(`cannot find package "([^"]+)"`),
	regexp.MustCompile(`LoadError: cannot load such file -- (\S+)`),
}

// dependencyManifests are the ordinary places a project says what it needs.
var dependencyManifests = []string{"requirements.txt", "pyproject.toml", "Pipfile", "setup.py",
	"package.json", "go.mod", "Gemfile", "Cargo.toml"}

// immediateExitMessage reports a job that exited before it could serve
// anything, in the words of its own output.
func immediateExitMessage(ctx *AgentContext, command string, out RunBackgroundOutput) string {
	var sb strings.Builder
	code := 0
	if out.ExitCode != nil {
		code = *out.ExitCode
	}
	fmt.Fprintf(&sb, "`%s` exited immediately with status %d — nothing is running, so there is nothing to probe.",
		truncateStr(command, 80), code)
	if tail := strings.TrimSpace(strings.Join(lastLines(out.Stderr, 4), "\n")); tail != "" {
		fmt.Fprintf(&sb, "\n\nIts last output:\n%s", tail)
	} else if tail := strings.TrimSpace(strings.Join(lastLines(out.Stdout, 4), "\n")); tail != "" {
		fmt.Fprintf(&sb, "\n\nIts last output:\n%s", tail)
	}
	if missing := missingModuleFrom(out.Stderr); missing != "" {
		fmt.Fprintf(&sb, "\n\nNothing named %q is importable in this environment.", missing)
		if declared := declaredDependencyFiles(ctx); len(declared) > 0 {
			fmt.Fprintf(&sb, " This project declares its dependencies in %s — installing from that file is "+
				"a supported step here.", strings.Join(declared, " and "))
		} else {
			sb.WriteString(" No file in this workspace declares it, so either the code should not import it, " +
				"or it has to be installed before the program can run. Installing is a supported step here; " +
				"decide which is right for this task and say what you did.")
		}
	}
	return sb.String()
}

func missingModuleFrom(lines []string) string {
	joined := strings.Join(lines, "\n")
	for _, re := range missingModulePatterns {
		if m := re.FindStringSubmatch(joined); m != nil {
			return m[1]
		}
	}
	return ""
}

func declaredDependencyFiles(ctx *AgentContext) []string {
	if ctx == nil || ctx.WorkingDir == "" {
		return nil
	}
	var found []string
	for _, name := range dependencyManifests {
		if st, err := os.Stat(filepath.Join(ctx.WorkingDir, name)); err == nil && !st.IsDir() {
			found = append(found, name)
		}
	}
	return found
}

func lastLines(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}

// currentSpanNote says where a span sits in the file as it is now, so a retry
// that needs line numbers can be written without re-reading first. It reports
// only what the file contains; when the text is not there at all, it says that
// instead of inventing a location.
func currentSpanNote(path, content, text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	i := strings.Index(content, text)
	if i < 0 {
		return fmt.Sprintf(" That text is not in %s as it stands now, so any line numbers you are holding are "+
			"stale — read it again before the next edit.", filepath.Base(path))
	}
	first := strings.Count(content[:i], "\n") + 1
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	last := first + len(lines) - 1
	note := fmt.Sprintf(" In %s as it stands now that text is %s", filepath.Base(path),
		lineSpanOf(content, text))
	if len(lines) > 0 {
		note += fmt.Sprintf(", so the replacement call is replace_lines with start_line %d, end_line %d, "+
			"expected_first_line %q and expected_last_line %q.",
			first, last, strings.TrimSpace(lines[0]), strings.TrimSpace(lines[len(lines)-1]))
	} else {
		note += "."
	}
	return note
}

// syntaxErrorLineRe finds the line number a parser reported, in the shapes the
// sandbox checkers produce ("(line 33)", "line 33", "app.py:33:").
var syntaxErrorLineRe = regexp.MustCompile(`(?:\bline\s+(\d+)|:(\d+):)`)

// offendingLineNote quotes the line a parse failure names.
//
// Measured (family P, cycle 9): the warning said `unmatched ')' (line 33)` and
// the run rewrote the whole file six times over 530 s, each version carrying
// the same error at the same line, without ever running it. The line it needed
// to look at was `@app.route('/items', methods=['GET']))` — one paren too
// many, visible at a glance and never shown. The number alone asks the model
// to find the line in a file it is reproducing from memory.
//
// Information only: nothing is rejected, retried or repaired here, and the
// write still lands with its warning as before.
func offendingLineNote(content, synErr string) string {
	m := syntaxErrorLineRe.FindStringSubmatch(synErr)
	if m == nil {
		return ""
	}
	num := m[1]
	if num == "" {
		num = m[2]
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return ""
	}
	lines := strings.Split(content, "\n")
	if n > len(lines) {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(" That line is:\n")
	for i := n - 1; i <= n+1 && i <= len(lines); i++ {
		if i < 1 {
			continue
		}
		marker := "  "
		if i == n {
			marker = "> "
		}
		fmt.Fprintf(&sb, "%s%d\t%s\n", marker, i, truncateStr(lines[i-1], 160))
	}
	return strings.TrimRight(sb.String(), "\n")
}

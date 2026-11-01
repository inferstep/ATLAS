// The agent's tool surface: what the model may call, what happens when it
// does, and how the model is told the tools exist at all.
//
// This file holds:
//
//	Registry and dispatch — a name→ToolDef map populated in init(), and
//	  executeToolCall, the one entry point the agent loop uses.
//	run_command. Each tool is a constructor returning its ToolDef — schema,
//	  model-facing description, executor — so a tool's three faces are edited
//	  in one place. The run client sits with the tool that uses it rather than
//	  in a shared client block.
//	Tier classification — whether a given write is boilerplate the proxy
//	  writes straight to disk (T0/T1) or logic worth routing through the V3
//	  pipeline (T2/T3), refined by cyclomatic complexity from v3-service.
//	Helpers — path resolution across the three coordinate systems the model,
//	  host and sandbox each use, and the V3-stage → SSE-event name map.
//
// tool_read.go holds the read-only tools, tool_write.go write_file,
// tool_write_v3.go the V3 route of a write or edit, tool_edit.go edit_file,
// insert_after and replace_lines, tool_structural_edit.go structural_edit,
// tool_files.go delete_file and move_file, tool_background.go the background
// commands, and tool_schema.go the JSON Schema, the GBNF grammar and the prompt
// section.
//
// There is one source of truth, the registry. Add a tool to it and its schema,
// its grammar alternative and its prompt entry all follow from the same
// ToolDef, so the model's instructions cannot drift from what the grammar
// permits and what the executor actually does.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

const (
	headerContentType = "Content-Type"
	contentTypeJSON   = "application/json"
)

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
	canonical, rejection := canonicalToolArgs(name, args, ctx) // one reading for every reader below
	if rejection != "" {
		return &ToolResult{Success: false, Error: rejection, MutationStatus: MutationNone, ValidationKind: ValidationKindNone, ValidationStatus: ValidationNotApplicable}
	}
	args = canonical
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
	classifyByEffect(tool.Effect, result)
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

// classifyByEffect fills only what the effect class alone can prove, and only
// where a local producer has not already spoken.
func classifyByEffect(effect ToolEffect, result *ToolResult) {
	if !effect.BoundaryClassifiable() {
		return
	}
	if result.MutationStatus == MutationUnknown {
		switch effect {
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
	if result.ValidationKind == ValidationKindUnknown {
		// Neither class performs content validation. Saying so explicitly is
		// different from leaving it unknown.
		result.ValidationKind = ValidationKindNone
		result.ValidationStatus = ValidationNotApplicable
	}
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

	// Distinguish "no args field at all" from "malformed args". The model
	// sometimes emits a tool_call with no args key, which lands here as
	// nil or empty bytes, and json.Unmarshal on that returns "unexpected
	// end of JSON input", the same error a truncated response produces,
	// which says nothing about the shape the call needs. Catch the empty
	// case here and return a per-tool hint that tells the model exactly
	// what shape to send.
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
	// Safety deny-list — sensitive targets (.env, *.pem, *credentials*,
	// destructive shell patterns) are refused in every permission mode.
	if denied, reason := shouldDenyToolCall(name, args); denied {
		return &ToolResult{Success: false, Error: fmt.Sprintf("%s refused: %s", name, reason),
			MutationStatus: MutationNone,
			ValidationKind: ValidationKindNone, ValidationStatus: ValidationNotApplicable}
	}
	// The user forbade changing anything. Enforced here, at the one dispatch
	// every tool passes through, and keyed on the tool's declared effect, not
	// on a tool-name list (which goes stale) and not on the system prompt,
	// which a model can simply not follow. That makes the guarantee hold for
	// arbitrary shell and background jobs (ToolEffectCommandUnobserved:
	// `sed -i`, `rm`, a `>` redirect) exactly as it does for write_file.
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

			out := runCommandOutput(ctx, input, cwd, timeoutSec)

			outBytes, _ := json.Marshal(out)
			errMsg := runCommandErrorMessage(ctx, input, out)
			return &ToolResult{
				Success: runCommandVerifiable(out),
				Data:    outBytes,
				Error:   errMsg,
			}, nil
		},
	}
}

// runCommandOutput runs the command where the session may run it: on the host
// when it is fully trusted and asked for that, and otherwise in the sandbox,
// whose absence is reported as the command's own failure.
func runCommandOutput(ctx *AgentContext, input RunCommandInput, cwd string, timeoutSec int) RunCommandOutput {
	// Route shell execution through the sandbox container.
	// The proxy is a slim Go binary with no python/pip/node, so
	// running locally meant every "verify" command failed with
	// "command not found". The sandbox has the language matrix
	// pre-installed AND has /workspace bind-mounted at the same
	// path the proxy sees, so paths the agent learned via
	// read_file / list_directory still work. validateShellCommand
	// upstream is the gate; this is the executor.
	//
	// When ctx.VerifyOnHost is set (ATLAS_VERIFY_IN=host with
	// ATLAS_TRUST_MODE=fully-trusted), we BYPASS the sandbox and execute
	// on the host directly. This is the right call for working
	// codebases that depend on host-side state — the user's
	// installed venv binaries, system tools, env vars,
	// running databases, etc. — that the sandbox can't see.
	// The shell-op safety gate (validateShellCommand) still
	// fired upstream regardless of target. cwd is translated
	// to the host path so the command lands in the right dir.
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
	return out
}

// runCommandErrorMessage is what the model is told went wrong with a command:
// a kill on the timeout, a stop at a resource ceiling, a failure with its last
// words, or an end the command did not reach. "" when nothing did.
func runCommandErrorMessage(ctx *AgentContext, input RunCommandInput, out RunCommandOutput) string {
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
		errMsg = nonZeroExitMessage(ctx, input, out)
	}
	if errMsg == "" && !executionCompleted(out.Outcome) {
		// Exit zero from a command that did not reach its own end.
		// Silence here would read as success.
		if m := executionOutcomeMessage(out.Outcome); m != "" {
			errMsg = m
		}
	}
	return errMsg
}

// nonZeroExitMessage is a failed command's last words: its stderr, else the
// last line of its stdout, else its exit code, with the hints that apply.
func nonZeroExitMessage(ctx *AgentContext, input RunCommandInput, out RunCommandOutput) string {
	errMsg := strings.TrimSpace(out.Stderr)
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
	return errMsg
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
	httpReq.Header.Set(headerContentType, contentTypeJSON)
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
		_ = json.NewDecoder(serviceAnswer(resp.Body)).Decode(&errBody)
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
	if err := json.NewDecoder(serviceAnswer(body)).Decode(&sr); err != nil {
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
	// diverse candidate generation is worth the cost. HTML / JSX templates
	// are usually short, so any file at ≥10 lines with a recognized
	// code/markup extension goes T2.
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
// Python only; HTML/JSON/etc. fall through
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
	req.Header.Set(headerContentType, contentTypeJSON)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(serviceAnswer(resp.Body))
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
		// V3 hard-rejected a sandbox-passing candidate
		// because tree-sitter found unresolved direct-identifier calls.
		// Sandbox passes for code with try/except ImportError fallbacks
		// or dead branches; structural verification doesn't care whether
		// the unresolved call executes, only that it can't resolve.
		return "v3_structural_veto"
	case "call_chain_context":
		// V3 phase-3 repair built a call-chain context
		// block for the failing function before invoking PR-CoT /
		// refinement. Informational — not a veto, just shows the user
		// that the repair phase has structural context the bare stderr
		// doesn't include.
		return "v3_call_chain_context"
	}
	return "v3_progress"
}

// truncateStr limits a string to maxLen bytes and appends "..." when it
// cuts.
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
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

// syntaxErrorLineRe finds the line number a parser reported, in the shapes the
// sandbox checkers produce ("(line 33)", "line 33", "app.py:33:").
var syntaxErrorLineRe = regexp.MustCompile(`(?:\bline\s+(\d+)|:(\d+):)`)

// offendingLineNote quotes the line a parse failure names.
//
// The number alone asks the model to find the line in a file it is reproducing
// from memory, and it can rewrite the whole file again and again with the same
// error at the same line without ever running it.
//
// Information only: nothing is rejected, retried or repaired here, and the
// write still lands with its warning.
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

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ------------------------------------------------------------------------
// Trust model (load-bearing — read before "fixing" go/path-injection CodeQL
// alerts on this file or its helpers in tools.go, context.go, gates.go).
//
// ATLAS is a single-tenant local-install agent. The proxy runs inside a
// container whose /workspace mount IS the user's project directory: the
// agent's whole purpose is to read, edit, and verify files there on
// behalf of the user who owns that directory. The "user-controlled" value
// CodeQL flags in agent path joins is almost always ctx.WorkingDir, which
// is set ONCE at agent startup from the bind mount — not from a per-
// request input. The few cases where the model emits a path
// (resolveAgentPath callers in tools.go) go through that resolver, which
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

// activeSessions tracks in-flight /v1/agent turns by session_id so
// /cancel can abort them. Map value is a *sessionCancel wrapping the
// context.CancelFunc from the per-request context.WithCancel wrapper.
//
// The pointer doubles as a per-turn identity token: cleanup uses
// CompareAndDelete so a finishing turn only removes its own entry. If a
// second /v1/agent request reuses the same session_id, the first turn's
// deferred cleanup no longer deletes the second turn's cancel func.
//
// Defense-in-depth: cancellation also flows naturally through TCP
// disconnect (handleAgent already binds ctx to r.Context()), but a
// reverse proxy may buffer the disconnect. /cancel gives the TUI a
// reliable, explicit kill switch.
var activeSessions sync.Map

// sessionCancel is the activeSessions map value — a comparable wrapper
// around the per-turn cancel func.
type sessionCancel struct {
	cancel context.CancelFunc
}

// ---------------------------------------------------------------------------
// Agent loop — iterative tool-calling loop between model and executors
// ---------------------------------------------------------------------------

// runAgentLoop runs the agent loop for a single user request.
// The model emits tool calls (constrained by grammar), the proxy executes them,
// and returns results. Continues until the model emits "done" or max turns hit.
// maxGateBounces caps EACH of the verification, done-without-action,
// expected-output, and claim-check gates independently. Mirrors the
// parse-error cap: a gate that has bounced the same `done` three times is
// in a stuck loop, so its fourth is accepted rather than bounced forever.
// The other gates keep their own budgets — see runState.gateBounces.
const maxGateBounces = 3

// runState is the per-run evidence the completion-honesty gates decide
// on, plus their bounce budgets. One struct so the gates see the
// same facts on the done and text exits instead of two hand-copied
// gate blocks (which is exactly how the text exit shipped ungated once).
type runState struct {
	turn     int    // current loop turn, for tool-call IDs and logs
	response string // raw model output this turn, echoed on a bounce

	// Tool calls executed this run, of any kind. The intent gate uses it to
	// tell "announced a tool call and stopped" from ordinary narration after
	// work has already happened.
	toolsRun int
	// Set when a write/edit/structural_edit/delete landed in this run.
	madeProductiveChange bool
	// Set when a read-only tool succeeds — the model opened the project
	// to answer this message. Distinguishes a request the model treated
	// as work from one it answered conversationally, without consulting
	// a vocabulary list. See wantsStateChange.
	inspectedWorkspace bool
	// Files the prompt explicitly asks the model to produce
	// ("save your solution in X"). Checked against disk before `done` is
	// allowed — a model can satisfy the generic action gate with a
	// PARTIAL artifact or by exploring without ever committing the named
	// output (observed 2026-07-19). Computed once from the prompt.
	expectedOutputs []string
	// The expected-output gate fires at most ONCE per session: a named
	// deliverable might be PRODUCED AT RUNTIME by the model's code (not
	// authored), so repeatedly bouncing a correct done would steer the
	// model to fabricate a stand-in (#147 review finding #8).
	outputGateUsed bool
	// Set when a verification command (pytest, curl, go test, ...)
	// completed successfully in any turn of this run. One success per
	// loop is enough — the model can iterate without re-verifying every
	// turn. Also softens the consecutive-errors exit: post-write
	// run_command failures are usually verification noise, not a
	// genuinely stuck loop.
	verifiedThisLoop bool
	// Set when a verification command RAN AND FAILED and none has
	// succeeded since. Observed session state, not a guess about the
	// request: once a test has gone red in this loop, declaring done is
	// dishonest regardless of how the user phrased the ask. Closes the
	// case the message-shape check cannot see (2026-07-21 dogfooding:
	// the model watched pytest fail 5/5 three times, diagnosed the fix
	// in prose, and exited through a bare text narration).
	sawFailedVerification bool
	// Whether the user prompt is a repair/fix request. Computed once —
	// the user message doesn't change mid-loop.
	userWantsVerification bool
	// Bounces spent per gate this run, keyed by gate name.
	//
	// Per-gate rather than one shared counter: the gates are evaluated in
	// a fixed order, so a single counter let whichever fired first spend
	// the whole allowance and silence the rest. An observed session put
	// all three bounces on the verification gate, so the
	// done-without-action gate never ran and the model exited having
	// changed nothing while claiming the work was already present. Each
	// gate reports a DIFFERENT problem, and a gate that has said its
	// piece three times must stop without muting the others.
	gateBounces map[string]int

	// correctives queued by the loop-health detectors this turn, drained
	// after the tool result so the next LLM call sees them in order:
	// assistant(tool_call) → tool(result) → user([system note]: …).
	//
	// Role MUST be "user": some Jinja chat templates enforce "system
	// message must be at the beginning" and 500 on a system role appended
	// mid-conversation. The "[system note]:" prefix is how the model
	// tells loop machinery from an actual user instruction.
	//
	// Several detectors firing on one turn is intentional — the model
	// gets each signal, since they observe the same stuckness from
	// different angles (identical args vs rehashed reasoning vs a lens
	// quality crash).
	correctives []string
}

// queueCorrective adds a loop-health corrective for this turn.
func (s *runState) queueCorrective(msg string) {
	if msg != "" {
		s.correctives = append(s.correctives, msg)
	}
}

// drainCorrectives appends every queued corrective to the conversation
// and clears the queue. Called once per turn, after the tool result.
func (s *runState) drainCorrectives(ctx *AgentContext) {
	for _, msg := range s.correctives {
		ctx.Messages = append(ctx.Messages, AgentMessage{
			Role:    "user",
			Content: "[system note]: " + msg,
		})
	}
	s.correctives = nil
}

// bounce echoes the model's output plus a synthetic tool rejection into
// the conversation, so the next LLM call sees exactly why the attempt
// was refused. The one shape every gate and guard refusal shares.
func (s *runState) bounce(ctx *AgentContext, toolName, rejection string) {
	ctx.Messages = append(ctx.Messages, AgentMessage{Role: "assistant", Content: s.response})
	ctx.Messages = append(ctx.Messages, AgentMessage{
		Role:       "tool",
		Content:    fmt.Sprintf(`{"success":false,"error":%q}`, rejection),
		ToolCallID: fmt.Sprintf("call_%d", s.turn),
		ToolName:   toolName,
	})
}

// bounceToolCall is bounce for a rejection that lands AFTER the tool_call
// event has already gone out. Without a matching tool_result the consumer
// sees a call that never resolves: the TUI prints the call row and nothing
// after it, so the user is never told the tool was refused and why.
//
// Observed live: a model tried to overwrite a fixture input file, the
// surgical-edit gate correctly refused, and the refusal reached the model
// (through ctx.Messages) but never the event stream — the session's tool_call
// and tool_result counts disagreed by one.
func (s *runState) bounceToolCall(ctx *AgentContext, toolName, rejection string) {
	s.bounce(ctx, toolName, rejection)
	ctx.Stream("tool_result", map[string]interface{}{
		"tool":    toolName,
		"success": false,
		"error":   rejection,
	})
}

// exitGates runs the completion-honesty gates a done or text exit must
// clear, in order: verification, done-without-action, expected-output,
// claim-check. claimText is the completion claim to check structurally
// (the done summary, or the text narration — on a text exit the
// narration IS the claim). Returns the failing gate's tool name and
// rejection, or "" to let the exit pass. Gates run in EVERY permission
// mode: yolo means "don't ask permission for destructive calls", not
// "skip completion checks". Bounces stay capped by maxGateBounces, so
// unattended runs cannot loop on a gate.
func (s *runState) exitGates(ctx *AgentContext, userMessage, claimText string) (string, string) {
	// Announcing a tool call is not making one. Observed on a question about
	// code: the model replied "I need to read orders.py — I'll start by
	// outlining the file to locate the function" and the turn ended there,
	// because text is a terminal event. It had the right intent and never
	// acted on it. Only fires before any tool has run, so it cannot interrupt
	// work already in progress.
	// A reply that signs off promising the actual answer leaves the user with
	// half of one, whether or not tools ran. Checked before the zero-tools
	// case below, since this one applies after the work is done.
	if promisesMoreContent(claimText) && s.chargeBounce("intent_gate") {
		log.Printf("[agent] intent gate: bouncing a reply that promised content it did not deliver (bounce %d/%d)",
			s.gateBounces["intent_gate"], maxGateBounces)
		return "intent_gate", "You ended by saying you would provide the answer, but the reply stops there and the turn ends with it — the user sees only the promise. Give the actual content now, in full, in a single `text` reply."
	}
	if s.toolsRun == 0 && announcesImminentToolUse(claimText) && s.chargeBounce("intent_gate") {
		log.Printf("[agent] intent gate: bouncing a text exit that announced a tool call without making one (bounce %d/%d)",
			s.gateBounces["intent_gate"], maxGateBounces)
		return "intent_gate", "You described the tool call you were about to make instead of making it, and a `text` reply ends the turn. Emit the tool_call itself now — read the file, then answer in a single `text` reply once you have its contents."
	}
	if (s.userWantsVerification || s.sawFailedVerification) && !s.verifiedThisLoop && s.chargeBounce("verification_gate") {
		log.Printf("[agent] verification gate: bouncing exit at turn %d (trigger=%s, no successful verification command this loop, bounce %d/%d)",
			s.turn, gateTrigger(s.userWantsVerification, s.sawFailedVerification), s.gateBounces["verification_gate"], maxGateBounces)
		return "verification_gate", verificationRejectionMessage(s.sawFailedVerification)
	}
	if wantsStateChange(userMessage, ctx.Tier, s.inspectedWorkspace) && !s.madeProductiveChange && s.chargeBounce("action_gate") {
		log.Printf("[agent] done-without-action gate: bouncing exit at turn %d (user prompt %q wants a state change, no successful write/edit/structural_edit this loop, bounce %d/%d)",
			s.turn, truncateStr(userMessage, 60), s.gateBounces["action_gate"], maxGateBounces)
		return "action_gate", actionWithoutProductiveChangeMessage(userMessage)
	}
	if missing := missingExpectedOutputs(ctx, s.expectedOutputs); len(missing) > 0 && !s.outputGateUsed && s.chargeBounce("output_gate") {
		s.outputGateUsed = true // fire once — see field doc
		log.Printf("[agent] expected-output gate: bouncing exit at turn %d — named deliverable(s) %v not on disk (bounce %d/%d)",
			s.turn, logPaths(missing), s.gateBounces["output_gate"], maxGateBounces)
		return "output_gate", expectedOutputMissingMessage(missing)
	}
	if claimsUniversal(claimText) || promptIsMultiIssue(userMessage) {
		if gap := verifyCompletionClaims(ctx.WorkingDir); gap != "" && s.chargeBounce("claim_check") {
			log.Printf("[agent] claim-check gate: bouncing exit at turn %d (bounce %d/%d) — %q",
				s.turn, s.gateBounces["claim_check"], maxGateBounces, truncateStr(gap, 200))
			return "claim_check", gap
		}
	}
	return "", ""
}

// chargeBounce spends one of gate's bounces and reports whether it had one
// left. A gate whose budget is gone returns false so exitGates falls through
// to the next gate rather than returning early: an exhausted gate must stop
// repeating itself, not mute the gates behind it.
func (s *runState) chargeBounce(gate string) bool {
	if s.gateBounces[gate] >= maxGateBounces {
		return false
	}
	if s.gateBounces == nil {
		s.gateBounces = make(map[string]int, 4)
	}
	s.gateBounces[gate]++
	return true
}

// fetchPatternContext asks the lens pattern-cache reader
// (/internal/patterns/context) for lessons from previous sessions whose
// pattern type matches the user message, and formats them as one
// "[system note]:" block (≤3 patterns, one "- [type] summary" line each,
// hard-capped at 600 chars). Strictly fail-soft: any error, timeout, or
// empty result returns ("", nil) and the agent loop proceeds without the
// block — the lens being down must never cost a turn or spam the log.
func fetchPatternContext(ctx *AgentContext, userMessage string) (string, []string) {
	if ctx.LensURL == "" || strings.TrimSpace(userMessage) == "" {
		return "", nil
	}
	body, err := json.Marshal(map[string]interface{}{
		"task": userMessage, "top_k": 3,
	})
	if err != nil {
		return "", nil
	}
	reqCtx, cancel := context.WithTimeout(ctx.Ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "POST",
		ctx.LensURL+"/internal/patterns/context", bytes.NewReader(body))
	if err != nil {
		return "", nil
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil
	}
	var r struct {
		Patterns []struct {
			Summary string `json:"summary"`
			Type    string `json:"type"`
		} `json:"patterns"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil || len(r.Patterns) == 0 {
		return "", nil
	}
	const blockCap = 600
	b := "[system note]: lessons from previous ATLAS sessions on similar tasks:"
	types := make([]string, 0, 3)
	for i, p := range r.Patterns {
		if i >= 3 {
			break
		}
		line := "\n- [" + p.Type + "] " + truncateStr(p.Summary, 160)
		if len(b)+len(line) > blockCap {
			break
		}
		b += line
		types = append(types, p.Type)
	}
	if len(types) == 0 {
		return "", nil
	}
	return b, types
}

func runAgentLoop(ctx *AgentContext, userMessage string) error {
	// Emit a stage_start envelope so the TUI's pipeline pane shows
	// the agent is working. Mirrors the typed-event broker.
	loopStart := time.Now()
	Emit(NewEnvelope(EvtStageStart, "agent", map[string]interface{}{
		"detail": fmt.Sprintf("tier=%s msg=%q", ctx.Tier,
			truncateStr(userMessage, 80)),
	}))
	defer func() {
		// Close the "agent" stage so the pipeline pane stops showing it
		// running. Without this, the TUI's pipelineState.apply only ever
		// sees EvtDone (overall finish) and the agent row is stuck in
		// Running() forever — visually misleading after the turn ended.
		dur := time.Since(loopStart).Milliseconds()
		Emit(Envelope{
			EventID:    NewEventID(),
			Timestamp:  float64(time.Now().UnixNano()) / 1e9,
			Type:       EvtStageEnd,
			Stage:      "agent",
			DurationMS: dur,
			Payload: map[string]interface{}{
				"success":      true,
				"total_tokens": ctx.TotalTokens,
			},
		})
		Emit(Envelope{
			EventID:    NewEventID(),
			Timestamp:  float64(time.Now().UnixNano()) / 1e9,
			Type:       EvtDone,
			Stage:      "agent",
			DurationMS: dur,
			Payload: map[string]interface{}{
				"success":           true,
				"total_duration_ms": dur,
				"total_tokens":      ctx.TotalTokens,
			},
		})
	}()

	// Pre-flight plan generation. Runs BEFORE buildSystemPrompt so
	// the system prompt can reference the planned steps — the model
	// gets explicit guidance on what to do first instead of having
	// to infer it from the user message alone. Skipped for trivial
	// chat / acks where the ~5-15s cost isn't worth it. Failures
	// degrade silently — the loop runs without adherence gating.
	if shouldGeneratePlan(ctx, userMessage) {
		if plan := generatePlan(ctx, userMessage); plan != nil {
			ctx.Plan = plan
			log.Printf("[agent] plan: %d steps, verify=%s, score=%.2f",
				len(plan.Steps), plan.VerifyStep, plan.WinningScore)
		}
	}

	// Build system prompt with tool descriptions, project context,
	// and (when present) the planned steps.
	systemPrompt := buildSystemPrompt(ctx)

	// Initialize messages: system prompt, then any prior-turn history
	// the TUI shipped, then the new user message. PriorHistory is
	// already filtered to role=user|assistant text turns (no tool
	// calls/results, no system spam) on the TUI side. Without this,
	// every user message starts a fresh agent loop and the model can't
	// answer follow-ups like "what did you just delete?".
	ctx.Messages = make([]AgentMessage, 0, 3+len(ctx.PriorHistory))
	ctx.Messages = append(ctx.Messages, AgentMessage{Role: "system", Content: systemPrompt})
	ctx.Messages = append(ctx.Messages, ctx.PriorHistory...)

	// GH #39 point 4: auto-inject reachability slice. If the user
	// message names project symbols (`dashboard`, "the foo function",
	// foo.bar.baz), pre-load their definitions so the model doesn't
	// burn agent turns on read_file/list_directory recon. Fail-soft —
	// no v3-service / no symbols / no project files / network error
	// all degrade silently to the original message-only flow.
	if symbols := extractCandidateSymbols(userMessage); len(symbols) > 0 {
		fileMap := walkPythonFiles(ctx.WorkingDir)
		if len(fileMap) > 0 {
			if idx, ok := resolveProjectSymbols(ctx, fileMap, symbols); ok && len(idx.Matched) > 0 {
				body := formatProjectContextMessage(idx.Matched)
				// #39 Phase 3: append the call-graph neighborhood when v3-service
				// returned it (ATLAS_CALL_GRAPH on). Empty string when absent, so
				// flag-off behavior is unchanged.
				body += formatGraphNeighborhood(idx.Graph)
				if body != "" {
					// Role MUST be "user" with a "[system note]:" prefix —
					// Some Jinja chat templates enforce "System message
					// must be at the beginning" and 500s on any system
					// role appended mid-conversation. Same convention
					// the lens-intervention path uses (commit b79b31d).
					ctx.Messages = append(ctx.Messages, AgentMessage{
						Role:    "user",
						Content: "[system note]: " + body,
					})
					names := make([]string, 0, len(idx.Matched))
					for _, m := range idx.Matched {
						names = append(names, m.Name)
					}
					log.Printf("[symbol_index] injected %d snippet(s) for [%s] from %d project files",
						len(idx.Matched), strings.Join(names, ", "), len(fileMap))
					ctx.Stream("symbol_index_injected", map[string]interface{}{
						"matched": names,
						"n_files": len(fileMap),
						"skipped": len(idx.Skipped),
					})
				}
			}
		}
	}

	// Pattern-cache context: lessons from previous sessions whose pattern
	// type matches this task, served by the lens reader. Same user-role
	// "[system note]:" convention as the symbol injection above. Fail-soft:
	// an empty block means no message and no event.
	if block, types := fetchPatternContext(ctx, userMessage); block != "" {
		ctx.Messages = append(ctx.Messages, AgentMessage{
			Role:    "user",
			Content: block,
		})
		log.Printf("[pattern_context] injected %d pattern(s) [%s]",
			len(types), strings.Join(types, ", "))
		ctx.Stream("pattern_context_injected", map[string]interface{}{
			"count": len(types),
			"types": types,
		})
	}

	ctx.Messages = append(ctx.Messages, AgentMessage{Role: "user", Content: userMessage})

	// Per-session cache scope. llama.cpp's KV slot persists between
	// requests by default — that's what keepLlamaWarm relies on. But the
	// slot also persists *across user sessions*, so context from a previous
	// session's conversation can bias the next session (the
	// `show_greeting.py` hallucination from the 2026-04-30 snake test was
	// likely an example). Erase slot 0 at the start of each agent loop call.
	// llama.cpp re-encodes the system prompt from scratch (~1-2s on a
	// warm GPU); the per-turn cache benefit within the session is preserved.
	// Disable with ATLAS_FRESH_SLOT_PER_SESSION=0.
	if envOr("ATLAS_FRESH_SLOT_PER_SESSION", "1") != "0" && !ctx.DisableFreshSlot {
		eraseLlamaSlot(ctx)
	}

	consecutiveReads := 0  // Track consecutive read-only calls
	consecutiveErrors := 0 // Track consecutive tool failures to break error loops
	// edit_file old_str-mismatch failures per path. A successful read_file
	// between attempts resets consecutiveErrors/RecentFailurePaths, which
	// masks the classic read→edit-miss→read loop (smaller models can't
	// reproduce old_str byte-for-byte). This counter survives interleaved
	// reads so we can force the structural_edit steer after the second miss.
	editMissByPath := map[string]int{}
	repeatDetections := 0 // hard-stop after the 2nd repeated-identical-call detection
	// Runaway backstop for content-varying write loops (#147 review finding
	// #14): the content-fingerprint repeat detector, by design, does not
	// catch a model that rewrites one file with materially different content
	// every time and never converges. This counts total writes per path and
	// escalates to the repeat corrective only at a threshold far above any
	// realistic iteration (polyglot's healthiest run was ~10), so it stops a
	// true runaway without regressing legitimate iteration.
	writeCountByPath := map[string]int{}
	const runawayWriteThreshold = 20
	// Exit-gate evidence + the shared bounce shape live on runState (see
	// its field docs); the remaining counters are loop-local.
	st := &runState{
		expectedOutputs:       expectedOutputPaths(userMessage),
		userWantsVerification: isFixIntentMessage(userMessage),
	}
	// One-shot: when a loop-stop is about to fire but the task's named
	// deliverable was never written, steer toward it once instead of
	// stopping (many hard tasks loop on run_command exploration and
	// hard-stop without ever reaching the done/text exit where the
	// expected-output gate lives — observed on sqlite and merge-diff).
	outputRescueUsed := false

	// Flag whether we've already injected the approaching-budget hint,
	// so we don't fire it every turn after crossing the threshold.
	budgetHintFired := false

	for turn := 0; ctx.MaxTurns <= 0 || turn < ctx.MaxTurns; turn++ {
		st.turn = turn
		// Budget hint — only relevant when there IS a turn cap.
		// May 10 2026: T1/T2/T3 default to uncapped (ctx.MaxTurns == 0),
		// so the hint is mostly dormant unless an operator explicitly
		// sets ATLAS_MAX_TURNS. T0 still hits a hint at turn 4 if a
		// conversational request unexpectedly tries to loop.
		if !budgetHintFired && ctx.MaxTurns > 0 && turn > 0 && turn*5 >= ctx.MaxTurns*4 {
			budgetHintFired = true
			ctx.Messages = append(ctx.Messages, AgentMessage{
				Role: "system",
				Content: fmt.Sprintf(
					"Turn budget notice: you're at turn %d of %d. If significant work remains, prioritize finishing the highest-impact items and verifying them — do not start new exploration. If you can finish in the remaining turns, keep going. If you cannot, summarize what's done and what's not in your `done` summary so the user knows what to follow up on.",
					turn, ctx.MaxTurns),
			})
		}

		// Bail out fast if the upstream request was cancelled (the client closed the
		// connection, user hit Ctrl-C, terminal exited). Without this check the
		// loop would keep grinding LLM calls and tool work for a client that's
		// already gone, burning GPU.
		if ctx.Ctx != nil {
			select {
			case <-ctx.Ctx.Done():
				log.Printf("[agent] cancelled at turn %d: %v", turn, ctx.Ctx.Err())
				return ctx.Ctx.Err()
			default:
			}
		}

		// Trim conversation history if it gets too long (prevent context overflow).
		// Keep system + most-recent-user-instruction + last 8 messages.
		//
		// Pinning the most recent user message is critical: long agent loops
		// (5+ tool calls) push the user's task beyond the trim window, and
		// the next LLM call sees only system + tool exchanges. Model has no
		// instruction to work from and goes generic ("Hi! I'm ATLAS...").
		// Hardcoding ctx.Messages[1] as the user msg used to work, but
		// PriorHistory makes that index a prior-turn message instead — so
		// scan backwards for the actual current-turn user role.
		// Trim by TOKEN BUDGET, not a blind message count. The
		// old `> 12 messages → keep 8` rule dropped a just-read file after
		// a couple of turns even when the prompt was a fraction of the
		// context window — the model would then re-read in a loop, saying
		// "I don't see the output in the history". keepLast is now derived
		// from how many recent messages actually fit the per-slot budget,
		// floored at 8 so we never trim more aggressively than before.
		trimmed := false
		if keep := budgetedKeepLast(ctx.Messages); keep < len(ctx.Messages)-1 {
			ctx.Messages = trimMessages(ctx.Messages, keep)
			trimmed = true
			log.Printf("[agent] trimmed conversation to %d messages (token-budget)", len(ctx.Messages))
		}

		// Per-turn streaming visibility: announce the start of the turn,
		// then the LLM call boundaries. Without these the TUI sees a 10-30s
		// gap between tool_result and the next tool_call while the model
		// is generating — looks like a hang.
		ctx.Stream("turn_start", map[string]interface{}{
			"turn":     turn,
			"messages": len(ctx.Messages),
			"trimmed":  trimmed,
		})
		// Estimate prompt tokens up front (chars/4 — works for English
		// + code, off by maybe 10–20%) so the TUI can pre-fill its
		// context-utilization gauge while llama-server is still doing
		// prompt eval. Authoritative count arrives in llm_call_end.
		promptTokenEst := 0
		for _, mm := range ctx.Messages {
			promptTokenEst += len(mm.Content) / 4
		}
		ctx.Stream("llm_call_start", map[string]interface{}{
			"turn":          turn,
			"messages":      len(ctx.Messages),
			"prompt_tokens": promptTokenEst,
		})
		Emit(NewEnvelope(EvtStageStart, "llm",
			map[string]interface{}{"turn": turn, "messages": len(ctx.Messages)}))
		llmStart := time.Now()

		// Call LLM with grammar constraint
		response, tokens, err := callLLMConstrained(ctx)
		llmElapsed := time.Since(llmStart)
		if err != nil {
			ctx.Stream("llm_call_end", map[string]interface{}{
				"turn":         turn,
				"tokens":       0,
				"total_tokens": ctx.TotalTokens,
				"ms":           llmElapsed.Milliseconds(),
				"error":        err.Error(),
			})
			Emit(Envelope{
				EventID:    NewEventID(),
				Timestamp:  float64(time.Now().UnixNano()) / 1e9,
				Type:       EvtStageEnd,
				Stage:      "llm",
				DurationMS: llmElapsed.Milliseconds(),
				Payload: map[string]interface{}{
					"success": false, "error": err.Error(),
				},
			})
			Emit(NewEnvelope(EvtError, "llm",
				map[string]interface{}{"message": err.Error()}))
			ctx.Stream("error", map[string]string{"error": err.Error()})
			return fmt.Errorf("LLM call failed on turn %d: %w", turn, err)
		}
		ctx.TotalTokens += tokens
		st.response = response
		ctx.Stream("llm_call_end", map[string]interface{}{
			"turn":         turn,
			"tokens":       tokens,
			"total_tokens": ctx.TotalTokens,
			"ms":           llmElapsed.Milliseconds(),
			"chars":        len(response),
		})
		Emit(Envelope{
			EventID:    NewEventID(),
			Timestamp:  float64(time.Now().UnixNano()) / 1e9,
			Type:       EvtStageEnd,
			Stage:      "llm",
			DurationMS: llmElapsed.Milliseconds(),
			Payload: map[string]interface{}{
				"success":      true,
				"tokens":       tokens,
				"total_tokens": ctx.TotalTokens,
			},
		})
		Emit(NewEnvelope(EvtMetric, "llm", map[string]interface{}{
			"name": "total_tokens", "value": ctx.TotalTokens,
		}))

		// Parse the response — extract JSON even if model added surrounding text
		parsed, parseErr := extractModelResponse(response)
		if parseErr != nil {
			// Classify the failure shape once: a category for the log so
			// docker logs reads "what kind of broken" at a glance, and
			// targeted feedback for the model — generic "your response
			// wasn't JSON" led to the May 2026 user-session bug where the
			// model retried the same 1100-char edit_file with a giant
			// old_str 5 times in a row. The response was being truncated
			// at the llama-server token cap; the model couldn't see that
			// and kept emitting the same too-big payload.
			category, feedback := classifyParseFailure(response)
			log.Printf("[agent] parse error: %v | category=%s raw_len=%d | raw: %q",
				parseErr, category, len(response), truncateStr(response, 500))
			ctx.Stream("error", map[string]string{
				"error":    "failed to parse model response",
				"category": category,
			})
			ctx.Messages = append(ctx.Messages, AgentMessage{
				Role:    "user",
				Content: feedback,
			})
			// Cap parse failures the same way we cap tool failures.
			// Five identical parse errors in a row is a stuck loop;
			// bailing keeps us from burning 6 more LLM round-trips.
			consecutiveErrors++
			if consecutiveErrors >= 3 {
				log.Printf("[agent] breaking parse-error loop at turn %d (%d consecutive)", turn, consecutiveErrors)
				ctx.Stream("done", map[string]string{
					"summary": "Stopped after 3 unparseable responses — the model's tool calls keep getting truncated. Try a more targeted request (e.g. 'edit just the @app.route(\"/product\") handler in app.py') so the response stays under the token cap.",
				})
				return nil
			}
			continue
		}

		// Log the args truncated — enables diagnosing failures like
		// "all 3 tool calls returned Success=false" without having to add
		// breakpoints.
		logEvent("info",
			fmt.Sprintf("[agent] turn=%d type=%s name=%s args=%s",
				turn, parsed.Type, parsed.Name, truncateStr(string(parsed.Args), 200)),
			requestIDFromContext(ctx.Ctx), nil)

		// When a tool_call still has no args after liftMissingArgs,
		// log the raw model output so we can see exactly what shape was
		// emitted — helps catch new alt-shapes the lift logic missed.
		if parsed.Type == "tool_call" && (len(parsed.Args) == 0 || string(parsed.Args) == "null") {
			log.Printf("[agent] turn=%d EMPTY ARGS — raw model output: %q", turn, truncateStr(response, 500))
		}

		switch parsed.Type {
		case "done":
			// The four honesty gates (see runState.exitGates): a done that
			// the run's own evidence contradicts is bounced, capped.
			if gate, rejection := st.exitGates(ctx, userMessage, parsed.Summary); gate != "" {
				st.bounce(ctx, gate, rejection)
				continue
			}
			ctx.Stream("done", map[string]string{
				"summary": parsed.Summary + liveBackgroundJobNote(ctx),
			})
			return nil

		case "text":
			// `text` is the agent's user-facing chat answer. End the turn
			// here — the user gets one reply per message they send, and can
			// follow up to continue. Looping after text caused two failures
			// in earlier revisions: a trailing role=assistant tripped
			// llama-server's "prefill incompatible with enable_thinking"
			// 400, and with a "continue" nudge the model would rabbit-hole
			// into nonsense tool_calls on conversational input.
			//
			// text is otherwise an UNGATED exit, and on action-intent
			// prompts models abandon work through it ("I will now proceed
			// to sanitize the credentials" — then session over, zero
			// edits). So the same gates as done run here,
			// with the narration as the completion claim. Chat replies
			// still exit cleanly: wantsStateChange requires action-intent
			// wording or an opened project, and a chat reply has neither.
			if gate, rejection := st.exitGates(ctx, userMessage, parsed.Content); gate != "" {
				st.bounce(ctx, gate, rejection)
				continue
			}
			ctx.Stream("text", map[string]string{"content": parsed.Content})
			ctx.Stream("done", map[string]string{"summary": ""})
			return nil

		case "tool_call":
			st.toolsRun++
			ctx.Stream("tool_call", map[string]interface{}{
				"name": parsed.Name,
				"args": json.RawMessage(parsed.Args),
				"turn": turn,
			})
			Emit(NewEnvelope(EvtToolCall, "tool", map[string]interface{}{
				"name":         parsed.Name,
				"args_summary": truncateStr(string(parsed.Args), 80),
				"turn":         turn,
			}))

			// Check permissions. In default and accept-edits modes a
			// destructive tool pauses the loop until the client approves or
			// denies it (via POST /v1/permission). Yolo mode and pre-approved
			// tools short-circuit needsPermission and never reach here. The
			// legacy PermissionFn is still honored for non-interactive callers.
			if needsPermission(ctx, parsed.Name, parsed.Args) {
				allowed := true
				if ctx.PermissionFn != nil {
					allowed = ctx.PermissionFn(parsed.Name, parsed.Args)
				} else {
					allowed = awaitPermission(ctx, parsed.Name, permCallID(turn), parsed.Args)
				}
				if !allowed {
					ctx.Stream("permission_denied", map[string]string{
						"tool": parsed.Name,
					})
					// Bespoke bounce: the permission flow keys its tool-call
					// ID via permCallID so the TUI can match the decision.
					ctx.Messages = append(ctx.Messages, AgentMessage{
						Role:    "assistant",
						Content: response,
					})
					ctx.Messages = append(ctx.Messages, AgentMessage{
						Role:       "tool",
						Content:    `{"success":false,"error":"permission denied by user"}`,
						ToolCallID: permCallID(turn),
						ToolName:   parsed.Name,
					})
					continue
				}
			}

			// Fix C: Detect truncated args BEFORE execution.
			// If the args JSON doesn't parse, don't attempt execution —
			// tell the model to use smaller edits instead.
			if parsed.Name == "write_file" || parsed.Name == "edit_file" || parsed.Name == "run_command" {
				var testParse map[string]interface{}
				if err := json.Unmarshal(parsed.Args, &testParse); err != nil {
					log.Printf("[agent] truncated args detected for %s at turn %d", parsed.Name, turn)
					st.bounceToolCall(ctx, parsed.Name, "Your output was truncated — the content is too long for a single tool call. For existing files, use edit_file with small targeted changes (replace specific functions or sections). For new files, keep them under 100 lines per write_file call.")
					consecutiveErrors++
					if consecutiveErrors >= 3 {
						ctx.Stream("done", map[string]string{"summary": "Stopped: content too large for tool calls. Try requesting smaller, targeted changes."})
						return nil
					}
					continue
				}
			}

			// Enforce the workspace boundary before any pre-execution gate reads
			// a path. executeToolCall repeats this check for parallel dispatch.
			if rejection := validateToolWorkspacePaths(parsed.Name, parsed.Args, ctx); rejection != "" {
				st.bounceToolCall(ctx, parsed.Name, rejection)
				consecutiveErrors++
				continue
			}

			// Surgical-edit gate: reject write_file on existing files
			// outright. write_file is for *creating* files; edits to an
			// existing file must use edit_file with old_str/new_str.
			//
			// The gate originally only blocked near-rewrites
			// (>= 70% line overlap) or >100-line writes. That left a
			// hole: a *complete* rewrite of a 90-line template (low
			// overlap, under the size cap) would slip through and
			// destroy the original. Hardened to reject every write
			// against an existing path. Trivially-small files (<= 5
			// lines, e.g. a single-line config) are still allowed
			// because there's no edit-vs-rewrite distinction at that
			// size — anything below that is faster to overwrite than
			// to surgically edit.
			if parsed.Name == "write_file" {
				var wfInput WriteFileInput
				if json.Unmarshal(parsed.Args, &wfInput) == nil {
					existingPath := resolveAgentPath(ctx, wfInput.Path)
					if existing, err := os.ReadFile(existingPath); err == nil {
						existingLines := strings.Count(string(existing), "\n") + 1
						// Exempt corrupted files. If the existing file
						// looks like it has prose preamble or stray
						// markdown fences (sanitizeFileContent would change
						// it), the only way to clean it up is full
						// replacement. edit_file can't express "remove
						// these specific corrupted lines" cleanly; the
						// model proved this by emitting old_str = new_str
						// for 53 wall-minutes (May 6 18:30 → 19:23).
						// Allow write_file in that case and log the
						// self-heal.
						// Self-iteration carveout: if this session wrote the file
						// itself (it's not the user's code, it's the agent's
						// own draft), allow overwriting. Otherwise the agent
						// can't correct its own first-pass mistakes — the
						// May 12 multi-file failure mode where V3 wrote a
						// stub app.py, realized it needed render-module
						// wiring, and got blocked from fixing it.
						sessionOwned := ctx.SessionWrites[wfInput.Path]
						corrupted := looksCorruptedOnDisk(existingPath, string(existing))
						// Existing, never read, not ours: refuse regardless of
						// size. The >5-line rule below is about "is a surgical
						// edit cheaper than a rewrite", which is a different
						// question from "should this be replaced at all" — and
						// you cannot know a file should be replaced when you
						// have never looked at it. edit_file and
						// structural_edit already demand a read first; this
						// closes the one path that did not.
						//
						// Observed twice: given a 1-line puzzle input, the
						// model recognised the puzzle from training, wrote the
						// canonical textbook example over the real input
						// without reading it, and solved the wrong data while
						// honestly reporting "created input.txt with sample
						// data".
						if isUnreadOverwrite(ctx, existingPath, corrupted, sessionOwned) {
							rejection := fmt.Sprintf(
								"%s already exists and this session has not read it. Use read_file first: if it holds input or configuration you were given, you need its real contents, not a replacement. If you have read it and still mean to replace the whole file, use edit_file or structural_edit.",
								wfInput.Path)
							log.Printf("[agent] rejecting write_file over unread existing %q (%d lines)", wfInput.Path, existingLines)
							st.bounceToolCall(ctx, "write_file", rejection)
							continue
						}
						if existingLines > 5 && !corrupted && !sessionOwned {
							// GH #39: when the existing file is .py or .html
							// and the model is replacing the whole thing,
							// structural_edit is the right tool — selector-based
							// node replacement, no old_str literal, no
							// truncation risk on long content. Surface
							// the option in the rejection text. edit_file
							// stays the recommendation for surgical
							// string-level changes (other file types,
							// inline tweaks).
							ext := strings.ToLower(filepath.Ext(wfInput.Path))
							structuralHint := ""
							if ext == ".py" || ext == ".html" || ext == ".htm" {
								structuralHint = " For whole-function or whole-element rewrites, prefer `structural_edit` — it takes a structural selector (e.g. `function:dashboard`, `<body>`) and the new content body, no `old_str` needed. structural_edit doesn't truncate the way edit_file can on long replacement strings."
							}
							rejection := fmt.Sprintf(
								"File %s already exists (%d lines). write_file is for creating new files, not modifying existing ones. Use edit_file with old_str/new_str to make targeted changes (read the file first if you need to confirm the exact text to replace).%s",
								wfInput.Path, existingLines, structuralHint)
							// %q quotes + escapes the path (go/log-injection).
							log.Printf("[agent] rejecting write_file for existing %q (%d lines)", wfInput.Path, existingLines)
							st.bounceToolCall(ctx, "write_file", rejection)
							continue
						}
						if existingLines > 5 {
							// Name the actual carveout — the corrupted-file
							// message on a session-owned overwrite sent a
							// loop diagnosis down the wrong path (2026-07-18).
							if corrupted {
								log.Printf("[agent] allowing write_file on corrupted %s (%d lines, sanitizer would clean it)", wfInput.Path, existingLines)
							} else {
								log.Printf("[agent] allowing write_file on session-owned %s (%d lines, self-iteration carveout)", wfInput.Path, existingLines)
							}
						}
					}
				}
			}

			// Shell-op guardrail: bounce destructive filesystem verbs in
			// run_command. The native edit_file/write_file/delete_file
			// tools are the supported mutation path — they go through
			// V3, the surgical-edit gate, and audit logging. Shell `mv`,
			// `rm`, `cp`, `find -delete` bypass all of that and led to
			// today's "agent moved templates into venv mid-task" disaster.
			// Yolo mode opts out of this for users who want the model to
			// have free rein.
			if parsed.Name == "run_command" && !ctx.YoloMode {
				var rc RunCommandInput
				if json.Unmarshal(parsed.Args, &rc) == nil {
					if rejection := validateRunCommand(rc.Command, ctx.WorkingDir); rejection != "" {
						// %q on rejection too: validateRunCommand may embed
						// fragments of the user's command verbatim in its
						// reason string (go/log-injection).
						log.Printf("[agent] rejecting run_command %q: %q",
							truncateStr(rc.Command, 80), rejection)
						st.bounceToolCall(ctx, "run_command", rejection)
						continue
					}
				}
			}

			// Same shell-validation + working-dir gate for run_background.
			// Without this, the May 8 2026 phantom-/workspace drift went
			// unblocked: the surgical-edit gate covered run_command but
			// run_background sailed through, so `run_background "cd
			// /workspace && python app.py"` looped for 3 turns before the
			// repeat detector caught it. validateRunCommand chains both
			// gates so destructive shell verbs and /workspace drift get
			// the same treatment regardless of which run_* tool the model
			// picks.
			if parsed.Name == "run_background" && !ctx.YoloMode {
				var rb RunBackgroundInput
				if json.Unmarshal(parsed.Args, &rb) == nil {
					if rejection := validateRunCommand(rb.Command, ctx.WorkingDir); rejection != "" {
						log.Printf("[agent] rejecting run_background %q: %q",
							truncateStr(rb.Command, 80), rejection)
						st.bounceToolCall(ctx, "run_background", rejection)
						continue
					}
				}
			}

			// Tool-call repetition detector. Catches the structural-loop
			// case the lens scoring doesn't see: same exact (tool, args)
			// emitted N times in close succession. Lens covers semantic
			// repetition (model produced the same low-quality content);
			// this covers structural repetition (model emitted the same
			// call to read_file or run_command). Fires before tool
			// execution so the corrective lands in the same iteration
			// as the lens corrective if both trigger.
			pendingRepeatCorrective := ""
			// Runaway backstop (#147 review #14): count writes per path and
			// force the repeat path once a single file is rewritten far more
			// than any real iteration would.
			runawayWrite := false
			if parsed.Name == "write_file" {
				if wp := writeFilePath(parsed.Args); wp != "" {
					writeCountByPath[wp]++
					if writeCountByPath[wp] == runawayWriteThreshold {
						runawayWrite = true
						log.Printf("[agent] runaway write backstop: %q rewritten %d times — escalating", wp, writeCountByPath[wp])
					}
				}
			}
			if msg, _, repeating := recordToolCall(ctx, parsed.Name, parsed.Args); repeating || runawayWrite {
				if runawayWrite && !repeating {
					msg = "You have rewritten this file an unusually large number of times without converging. Stop rewriting the whole file — read the current on-disk version, make ONE targeted change with edit_file/structural_edit, or step back and reconsider the approach; if the task is satisfied, respond with done."
					// The detector clears its window only when IT fires.
					// The backstop is a separate trigger for the same
					// corrective, so clear it here too.
					resetToolRepeatWindow(ctx)
				}
				log.Printf("[agent] tool-call repetition at turn %d on %s — queuing corrective for next turn", turn, parsed.Name)
				ctx.Stream("agent_repeat_intervention", map[string]interface{}{
					"turn":   turn,
					"tool":   parsed.Name,
					"reason": msg,
				})
				pendingRepeatCorrective = msg
				repeatDetections++
				// Steer-before-kill ladder. On the FIRST detection, fall
				// through: pendingRepeatCorrective is injected below and the
				// loop continues, so the model gets an explicit nudge to
				// change approach before we ever terminate. Only hard-stop
				// on the SECOND detection — the model saw the steer and
				// repeated anyway (genuinely stuck).
				//
				// The old code hard-stopped on the FIRST detection whenever
				// st.madeProductiveChange was set ("work landed, model spinning
				// on verification"). That mistook legitimate iteration for a
				// loop: 2026-07-19 showed models one nudge from finishing
				// — regex-chess repeating a verify command that itself had a
				// syntax error, polyglot mid-fix — killed with their solution
				// on disk but unverified. A nudge first is the whole point:
				// the broken-verify steer (below) can turn exactly these into
				// completions.
				if repeatDetections >= 2 {
					// Output-rescue: if the task named a deliverable and it
					// isn't on disk, the model is looping WITHOUT having
					// committed its answer — steer toward the file once and
					// keep going rather than hard-stopping empty-handed.
					if missing := missingExpectedOutputs(ctx, st.expectedOutputs); len(missing) > 0 && !outputRescueUsed {
						outputRescueUsed = true
						repeatDetections = 0
						pendingRepeatCorrective = expectedOutputMissingMessage(missing)
						log.Printf("[agent] repeat loop at turn %d but named deliverable(s) %v not on disk — output-rescue steer instead of stopping", turn, logPaths(missing))
					} else {
						if st.madeProductiveChange {
							log.Printf("[agent] second repetition after a productive change at turn %d — stopping (nudge ignored; work is on disk)", turn)
							ctx.Stream("done", map[string]string{"summary": "Made your change. The follow-up verification command kept repeating and failing (often a typo in the command, not the edit) — the change is on disk; run it yourself to confirm."})
						} else {
							log.Printf("[agent] second repetition detection at turn %d — breaking stuck loop", turn)
							ctx.Stream("done", map[string]string{"summary": "Stopped: the same tool call kept repeating without making progress. Try a more specific instruction (e.g. name the file and the exact change)."})
						}
						return nil
					}
				}
			}

			// Reasoning-repetition detector (BiasBusters #30). The
			// model's reasoning_content stream is captured per-turn in
			// ctx.LastTurnReasoning by callLLMOnce. recordReasoning
			// compares the normalized opening prefix against the prior
			// turn's snippet; ≥2 consecutive identical openings fires
			// the intervention. Sibling to the structural repeat
			// detector (above) and the lens regression detector (below)
			// — three different angles on "model is stuck", catching
			// different shapes of stuck-ness.
			pendingReasoningCorrective := ""
			if msg, obs, repeating := recordReasoning(ctx, ctx.LastTurnReasoning); repeating {
				log.Printf("[agent] reasoning repetition at turn %d (consecutive=%d) — queuing corrective", turn, obs.Count)
				ctx.Stream("agent_reasoning_intervention", map[string]interface{}{
					"turn":        turn,
					"consecutive": obs.Count,
					"reason":      msg,
					"snippet":     obs.Snippet,
				})
				pendingReasoningCorrective = msg
			}

			// Score write_file/edit_file content with the geometric lens
			// BEFORE executing. The score reflects what the model produced
			// (independent of whether the tool succeeds). On a quality-crash pattern (N consecutive
			// low scores) we queue a corrective system message that gets
			// appended AFTER the tool result so the next LLM call sees:
			// assistant(tool_call) → tool(result) → system(lens warning).
			// This is the direct fix for the May 6 templates/resources.html
			// stub-loop case where the stub gate kept rejecting but the model
			// kept retrying the same stub.
			pendingLensCorrective := ""
			if scorable, ok := extractScorableContent(parsed.Name, parsed.Args); ok {
				// Capture the model's write for deferred lens-training labeling
				// (a later /feedback call turns it into a weighted sample). Same
				// content the lens scores below, so a sample mirrors its score.
				ctx.RecordPassWrite(parsed.Name, extractFailurePath(parsed.Name, parsed.Args), scorable)
				if score, scored := scoreContentForAgent(ctx.Ctx, ctx.LensURL, scorable); scored {
					ctx.LensScoreHistory = append(ctx.LensScoreHistory, score.Aggregate.GxScoreMin)
					log.Printf("[agent] lens turn=%d tool=%s gx_min=%.3f gx_mean=%.3f off_rails=%d n_tok=%d latency=%.0fms history=%s",
						turn, parsed.Name,
						score.Aggregate.GxScoreMin, score.Aggregate.GxScoreMean,
						score.Aggregate.FirstOffRailsIdx, score.NTokens,
						score.LatencyMS, formatScoreSlice(ctx.LensScoreHistory))
					ctx.Stream("agent_lens_score", map[string]interface{}{
						"tool":                parsed.Name,
						"turn":                turn,
						"n_tokens":            score.NTokens,
						"first_off_rails_idx": score.Aggregate.FirstOffRailsIdx,
						"gx_score_min":        score.Aggregate.GxScoreMin,
						"gx_score_mean":       score.Aggregate.GxScoreMean,
						"latency_ms":          score.LatencyMS,
					})
					if low, severe, calibrated := score.calibratedThresholds(); calibrated {
						if msg, intervene := agentLensRegression(ctx.LensScoreHistory, low, severe); intervene {
							log.Printf("[agent] lens regression at turn %d on %s — queuing corrective for next turn", turn, parsed.Name)
							ctx.Stream("agent_lens_intervention", map[string]interface{}{
								"turn":   turn,
								"tool":   parsed.Name,
								"reason": msg,
							})
							pendingLensCorrective = msg
							// Reset history so we don't re-fire on the same crash.
							ctx.LensScoreHistory = nil
						}
					}
				}
			}

			// Execute tool. A re-read of an unchanged file already in
			// context is served from a compact pointer instead of
			// re-injecting + re-encoding the whole file (see
			// redundantReadShortCircuit).
			startTime := time.Now()
			result := redundantReadShortCircuit(parsed.Name, parsed.Args, ctx)
			if result != nil {
				log.Printf("[agent] turn=%d short-circuited redundant read (already in context, unchanged)", turn)
			}
			if result == nil && (parsed.Name == "run_command" || parsed.Name == "run_background") {
				if blk := runBlockAfterTraceback(ctx); blk != nil {
					result = blk
					log.Printf("[agent] turn=%d blocked re-run after traceback — forcing an edit first", turn)
				}
			}
			if result == nil {
				result = executeToolCall(parsed.Name, parsed.Args, ctx)
			}
			elapsed := time.Since(startTime)

			// On failure, log the error so it shows up in `docker compose
			// logs atlas-proxy` without having to attach a debugger.
			if !result.Success {
				log.Printf("[agent] turn=%d tool=%q FAIL: %q", turn,
					truncateStr(parsed.Name, 64), truncateStr(result.Error, 240))
			}

			// Force-stop after destructive operations that shouldn't have
			// follow-up. The sentinel is internal control flow — strip it
			// before any event is emitted so it never reaches the client.
			forceDone := result.Error == "__FORCE_DONE__"
			if forceDone {
				result.Error = ""
			}

			ctx.Stream("tool_result", map[string]interface{}{
				"tool":    parsed.Name,
				"success": result.Success,
				"data":    json.RawMessage(result.Data),
				"error":   result.Error,
				"elapsed": elapsed.String(),
			})
			Emit(Envelope{
				EventID:    NewEventID(),
				Timestamp:  float64(time.Now().UnixNano()) / 1e9,
				Type:       EvtToolResult,
				Stage:      "tool",
				DurationMS: elapsed.Milliseconds(),
				Payload: map[string]interface{}{
					"name":    parsed.Name,
					"success": result.Success,
					"error":   truncateStr(result.Error, 120),
				},
			})

			if forceDone {
				// Don't stream a follow-up message — the file deletion already
				// happened on disk and any trailing text would just be noise
				// for the TUI to render after a destructive op.
				return nil
			}

			// Track productive state changes — write/edit/delete that landed.
			// Used below to soften the error-loop exit when work was completed
			// AND by the done-without-action gate so a feature prompt
			// ("rewrite X", "add Y") can't declare done without any actual
			// edit on disk. structural_edit was missing from this list pre-May-10,
			// which let a structural_edit-only success path slip past the
			// productive-change tracking too.
			if result.Success && (parsed.Name == "write_file" || parsed.Name == "edit_file" ||
				parsed.Name == "structural_edit" || parsed.Name == "delete_file") {
				st.madeProductiveChange = true
			}

			// Track verification — a successful run_command of a build /
			// test / probe / runner. Recon (ls, cat, grep) doesn't count.
			// Once any verification succeeds in this loop, the fix-intent
			// gate stops blocking `done`.
			if parsed.Name == "run_command" {
				var rc RunCommandInput
				if json.Unmarshal(parsed.Args, &rc) == nil && isVerificationCommand(rc.Command) {
					if result.Success {
						st.verifiedThisLoop = true
						st.sawFailedVerification = false
						log.Printf("[agent] verification recorded: turn=%d cmd=%q",
							turn, truncateStr(rc.Command, 60))
					} else {
						// Red test/build. Latches the verification gate on
						// for this loop until something verifies green.
						st.sawFailedVerification = true
						log.Printf("[agent] verification FAILED: turn=%d cmd=%q — done is gated until it passes",
							turn, truncateStr(rc.Command, 60))
					}
				}
			}

			// Plan-adherence accounting. Records whether this tool
			// call satisfied an unsatisfied step on ctx.Plan (if any),
			// updates the off-streak counter, and asks us to revise
			// the plan if the streak crossed the threshold. Advisory
			// — never blocks the call. recordPlanAdherence is a no-op
			// when ctx.Plan is nil (T0 / planner failure).
			if shouldRevise := recordPlanAdherence(ctx, parsed.Name, parsed.Args, result.Success); shouldRevise {
				revisePlan(ctx, userMessage,
					fmt.Sprintf("agent went off-plan for %d consecutive tool calls (last: %s)",
						ctx.PlanOffStreak, parsed.Name))
			}

			// Break error loops: if 3 tool calls fail in a row, stop.
			// When the agent has already written/edited a file
			// and is now failing on `run_command` (verification noise — no
			// TTY for curses, missing toolchain, etc.), a different exit
			// message is appropriate so the user isn't told "the file may
			// be too large to modify" when their file is, in fact, on disk.
			// edit_file old_str miss: count per path independently of the
			// consecutiveErrors reset an interleaved read causes. On the
			// second miss for the same structured file, force the structural_edit
			// steer as a [system note] (the inline tool-error hint alone
			// doesn't reliably move a small model off edit_file).
			if !result.Success && parsed.Name == "edit_file" &&
				strings.Contains(result.Error, "string to replace not found") {
				mp := extractFailurePath(parsed.Name, parsed.Args)
				editMissByPath[mp]++
				ext := strings.ToLower(filepath.Ext(mp))
				// Force the structural_edit steer on the FIRST miss for structured
				// files — small models bail to run_command after a single
				// edit_file miss rather than retrying, so waiting for a
				// second miss never fires (observed: 1 edit_file all session,
				// then 9 run_command re-runs).
				if editMissByPath[mp] >= 1 && (ext == ".py" || ext == ".html" || ext == ".htm") {
					pendingRepeatCorrective = "edit_file's old_str did not match " +
						mp + " (small drift in whitespace/quotes is enough to miss). " +
						"Do NOT re-read or run the file — switch to structural_edit, which " +
						"needs no old_str: {\"type\":\"tool_call\",\"name\":\"structural_edit\"," +
						"\"args\":{\"path\":\"" + mp + "\",\"selector\":\"function:NAME\" " +
						"(or class:NAME, or <tag> for HTML),\"content\":\"<the full " +
						"replacement function/class/element>\"}}."
					log.Printf("[agent] edit_file miss on %q — forcing structural_edit steer", mp)
				}
			}

			if !result.Success {
				consecutiveErrors++
				// May 10 2026: path-aware breaker. Track which file each
				// failure was on; only escalate when 3 consecutive failures
				// share the same path (= truly stuck on one file). 3 fails
				// across DIFFERENT files = grinding through multi-file work,
				// keep going.
				failPath := extractFailurePath(parsed.Name, parsed.Args)
				ctx.RecentFailurePaths = append(ctx.RecentFailurePaths, failPath)
				if len(ctx.RecentFailurePaths) > 3 {
					ctx.RecentFailurePaths = ctx.RecentFailurePaths[len(ctx.RecentFailurePaths)-3:]
				}
				if consecutiveErrors >= 3 {
					samePath := len(ctx.RecentFailurePaths) == 3 &&
						ctx.RecentFailurePaths[0] != "" &&
						ctx.RecentFailurePaths[0] == ctx.RecentFailurePaths[1] &&
						ctx.RecentFailurePaths[1] == ctx.RecentFailurePaths[2]
					if !samePath {
						log.Printf("[agent] path-aware breaker: %d consecutive failures across different paths (%v) — continuing, not a stuck loop", consecutiveErrors, ctx.RecentFailurePaths)
						// Reset consecutiveErrors so the multi-file grind
						// can keep going. The recent-paths list stays as
						// a rolling window so if subsequent fails DO
						// collapse onto one path, we still catch it.
						consecutiveErrors = 0
					} else if missing := missingExpectedOutputs(ctx, st.expectedOutputs); len(missing) > 0 && !outputRescueUsed {
						// Output-rescue (same as the repeat breaker): looping
						// on failures without ever committing the named
						// deliverable — steer toward it once before stopping.
						outputRescueUsed = true
						consecutiveErrors = 0
						ctx.RecentFailurePaths = nil
						ctx.Messages = append(ctx.Messages, AgentMessage{Role: "user", Content: expectedOutputMissingMessage(missing)})
						log.Printf("[agent] error loop at turn %d but named deliverable(s) %v not on disk — output-rescue steer instead of stopping", turn, logPaths(missing))
					} else {
						log.Printf("[agent] breaking error loop: %d consecutive failures on the same path %q at turn %d (productive=%v)",
							consecutiveErrors, ctx.RecentFailurePaths[0], turn, st.madeProductiveChange)
						if st.madeProductiveChange {
							ctx.Stream("done", map[string]string{"summary": "Wrote your changes to disk; couldn't verify them automatically (the verification commands failed). Run them yourself to confirm — they're on disk."})
						} else {
							ctx.Stream("done", map[string]string{"summary": "Stopped after 3 tool failures on the same target with no successful changes. Common causes: the file you referenced isn't in the workspace, an empty path argument was passed, or a regex was malformed. Check the per-turn errors above, then try a more specific request (e.g. \"fix snake_game.py at line 95 — the curses bounds are wrong\")."})
						}
						return nil
					}
				}
			} else {
				consecutiveErrors = 0
				// Successful tool call resets the path window — the model
				// is clearly making progress somewhere.
				ctx.RecentFailurePaths = nil
			}

			// Track consecutive read-only calls to detect exploration loops.
			// outline_file/find_file MUST be here too — otherwise an
			// interleaved outline resets the counter and the model
			// read→outline→read→outline forever without the breaker firing
			// (observed live with a compact reasoning model). Every navigation-only tool counts.
			isReadOnly := parsed.Name == "read_file" ||
				parsed.Name == "outline_file" ||
				parsed.Name == "list_directory" ||
				parsed.Name == "search_files" ||
				parsed.Name == "find_file"
			if isReadOnly {
				consecutiveReads++
				if result.Success {
					// The model went looking at the project, so it read the
					// message as work rather than conversation. Used by the
					// done-without-action gate below to tell "remove the
					// debug logging" (opens the file, writes nothing) apart
					// from "thanks, that looks great" (no tool calls at all).
					st.inspectedWorkspace = true
				}
			} else {
				consecutiveReads = 0
			}

			// Add assistant message (the tool call) and tool result to conversation
			ctx.Messages = append(ctx.Messages, AgentMessage{
				Role:    "assistant",
				Content: response,
			})
			ctx.Messages = append(ctx.Messages, AgentMessage{
				Role:       "tool",
				Content:    result.MarshalText(),
				ToolCallID: fmt.Sprintf("call_%d", turn),
				ToolName:   parsed.Name,
			})

			// Lens intervention: if the lens flagged a
			// regression earlier in this iteration, append the corrective
			// NOW so the next LLM call sees it after the tool result.
			// Role MUST be "user" — some Jinja chat templates enforce
			// "System message must be at the beginning" and rejects any
			// system role appended mid-conversation, which previously
			// crashed the next LLM call with a 500. The "[system note]:"
			// prefix is how the model knows it's loop-machinery feedback,
			// not an actual user instruction.
			// Loop-health correctives, queued in signal order (lens
			// quality crash, repeated call, rehashed reasoning) and drained
			// through one path. Each slot holds at most one message: the
			// repeat slot is deliberately overwritable, so the specific
			// edit_file -> structural_edit steer above replaces the generic
			// repeat warning instead of stacking with it.
			st.queueCorrective(pendingLensCorrective)
			st.queueCorrective(pendingRepeatCorrective)
			st.queueCorrective(pendingReasoningCorrective)
			st.drainCorrectives(ctx)

			// Option 3 (issue #39): traceback → directed edit. When a
			// run_command surfaced a Python traceback, mechanically extract
			// the fix site and hand the model a directed instruction ("fix
			// function X here") instead of leaving it to localize — the step
			// a weak model fails by hallucinating symbols / editing the wrong
			// function. The stack frame IS the localization; no LLM reasoning
			// needed to read it.
			if !result.Success && (parsed.Name == "run_command" || parsed.Name == "run_background") {
				// Scan the RAW stdout/stderr, not result.MarshalText() — the
				// marshaled JSON escapes the quotes in `File "..."` frames, so
				// the traceback regex wouldn't match.
				var rc struct {
					Stdout string `json:"stdout"`
					Stderr string `json:"stderr"`
				}
				_ = json.Unmarshal(result.Data, &rc)
				scan := rc.Stderr + "\n" + rc.Stdout
				if scan == "\n" {
					scan = result.Error
				}
				// The command text — needed by the broken-inline-script
				// steer to tell a malformed verify one-liner (SyntaxError in
				// the `-c` argument) apart from a real code bug.
				var runArgs struct {
					Command string `json:"command"`
				}
				_ = json.Unmarshal(parsed.Args, &runArgs)
				if steer := brokenInlineScriptSteer(runArgs.Command, scan); steer != "" {
					// Broken verification command: the SyntaxError is in the
					// model's own inline `-c` test, not the solution. Steer it
					// to move the test into a file instead of re-running the
					// unparseable one-liner into the repetition breaker.
					ctx.Messages = append(ctx.Messages, AgentMessage{Role: "user", Content: steer})
					log.Printf("[agent] broken-inline-script steer: verify command won't parse, directed to a test file")
				} else if steer := tracebackSteer(ctx, scan); steer != "" {
					ctx.Messages = append(ctx.Messages, AgentMessage{Role: "user", Content: steer})
					log.Printf("[agent] traceback localization: steered to fix site")
				} else if steer := missingModuleSteer(ctx, scan); steer != "" {
					// Uninstalled-dependency recovery: the run failed with "No
					// module named X". Tell the model to pip install it instead
					// of re-running the identical failing command into the
					// repetition breaker.
					ctx.Messages = append(ctx.Messages, AgentMessage{Role: "user", Content: steer})
					log.Printf("[agent] missing-module steer: directed to install dependency")
				} else if steer := missingCommandSteer(scan); steer != "" {
					// Missing-binary recovery: the sandbox image lacks the
					// command and can't apt-install it (non-root, read-only).
					// Say so and point at the escape hatches, instead of the
					// model re-running into the breaker or giving up.
					ctx.Messages = append(ctx.Messages, AgentMessage{Role: "user", Content: steer})
					log.Printf("[agent] missing-command steer: named the unavailable binary")
				} else if steer := missingFileSteer(ctx, scan); steer != "" {
					// Case-typo recovery: command referenced a file whose name
					// differs only in case from a real workspace file. Name the
					// correct file so the model stops re-running the wrong name.
					ctx.Messages = append(ctx.Messages, AgentMessage{Role: "user", Content: steer})
					log.Printf("[agent] missing-file localization: steered to correct case")
				}
			}

			// Cross-file coherence signals after a successful mutation:
			// the session file manifest (so later files reference earlier
			// ones instead of re-creating them) and the asset-graph lint
			// (orphaned templates/static files, dangling refs). Both are
			// advisory [system note]s — never blockers.
			if result.Success &&
				(parsed.Name == "write_file" || parsed.Name == "edit_file" ||
					parsed.Name == "structural_edit" || parsed.Name == "move_file" ||
					parsed.Name == "delete_file") {
				if note := sessionManifestNote(ctx); note != "" {
					ctx.Messages = append(ctx.Messages, AgentMessage{
						Role:    "user",
						Content: "[system note]: " + note,
					})
					log.Printf("[agent] session manifest announced (%d files)", len(ctx.SessionWrites))
				}
				if note := assetLintNote(ctx); note != "" {
					ctx.Messages = append(ctx.Messages, AgentMessage{
						Role:    "user",
						Content: "[system note]: " + note,
					})
					ctx.Stream("asset_lint", map[string]interface{}{
						"turn":   turn,
						"detail": note,
					})
					log.Printf("[agent] asset lint: %s", truncateStr(note, 160))
				}
			}

			// Trust V3-verified edits — strongly nudge toward done.
			// When V3 ran the edit through its sandbox/probe pipeline and
			// the result came back successful (V3Used && PhaseSolved
			// non-empty), the edit is build-verified. Compact models can otherwise
			// keeps grinding: re-reads the file, edits unrelated functions,
			// runs another V3 cycle (~110s each). Inject an explicit
			// "you're done unless you have a specific reason" message.
			if result.Success && result.V3Used && result.PhaseSolved != "" &&
				(parsed.Name == "write_file" || parsed.Name == "edit_file") {
				ctx.Messages = append(ctx.Messages, AgentMessage{
					Role: "user",
					Content: fmt.Sprintf(
						"V3 verified this edit passed its %s pipeline (%d candidates, score=%.2f). The fix is on disk and build-checked. If this resolves the user's original request, respond NOW with {\"type\":\"done\",\"summary\":\"<one sentence describing the fix>\"}. Only continue if you have a specific, concrete additional change to make — do not re-read the file to double-check, and do not edit unrelated code.",
						result.PhaseSolved, result.CandidatesTested, result.WinningScore,
					),
				})
				log.Printf("[agent] V3-verified %s on %s — nudging toward done", parsed.Name, truncateStr(string(parsed.Args), 80))
			}

			// Exploration budget: after 4 consecutive read-only calls,
			// inject nudge. After 5, escalate the nudge. The read above
			// already executed and its result is in context — the nudge
			// steers the NEXT turn toward a write.
			// FUTURE (L6 reliability): Compact models can over-explore when adding
			// features to existing projects (~67% pass rate). Better prompting,
			// larger model, or V3-guided exploration would improve this.
			if consecutiveReads == 4 {
				ctx.Messages = append(ctx.Messages, AgentMessage{
					Role:    "user",
					Content: "You have full project context in the system prompt. Do not read more files. Emit a write_file or edit_file tool call now.",
				})
				log.Printf("[agent] exploration budget: warning at turn %d", turn)
			} else if consecutiveReads >= 5 {
				ctx.Messages = append(ctx.Messages, AgentMessage{
					Role:    "user",
					Content: "You already have this information in context — reading more files will not help. Write your changes now. Use write_file or edit_file.",
				})
				consecutiveReads = 2 // Keep at warning level, don't reset
				log.Printf("[agent] exploration budget: escalated nudge at turn %d", turn)
			}

		default:
			// Unknown type — grammar should prevent this
			ctx.Messages = append(ctx.Messages, AgentMessage{
				Role:    "user",
				Content: fmt.Sprintf("Unknown response type '%s'. Use tool_call, text, or done.", parsed.Type),
			})
		}
	}

	ctx.Stream("error", map[string]string{
		"error": fmt.Sprintf("max turns (%d) exceeded for %s task", ctx.MaxTurns, ctx.Tier),
	})
	return fmt.Errorf("max turns exceeded (%d)", ctx.MaxTurns)
}

// ---------------------------------------------------------------------------
// LLM call with grammar constraint
// ---------------------------------------------------------------------------

// isContextOverflow reports whether an LLM-call error is llama-server's
// exceed_context_size_error 400 (prompt tokens > per-slot n_ctx). Matched
// on the error body text — model-agnostic, keyed to llama.cpp's stable
// error type string with the human message as fallback.
func isContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "exceed_context_size") ||
		strings.Contains(s, "exceeds the available context size")
}

// callLLMConstrained calls the LLM with json_schema or grammar constraint.
// Returns the raw response text and token count.
//
// When the model emits zero tokens (raw_len=0) — usually after a
// tool result message under a constrained JSON grammar — we retry
// inline once with a bumped temperature and a transient "continue"
// nudge appended to the messages. This avoids burning a full agent-loop
// turn (~30s + tokens) on the parse-error retry path. The nudge is
// scoped to the retry call only; ctx.Messages is not mutated.
//
// May 2026 BiasBusters #2/#3 — per-step tool restriction. If the previous
// turn ended in a write_file rejection on a .py/.html file >5 lines, the
// model is biased toward retrying with edit_file (lexically closer to
// write_file than structural_edit, despite structural_edit being correct for the case).
// We respond by (a) dropping edit_file and write_file from the GBNF
// tool-name production for this single decision and (b) injecting an
// ephemeral [system note] reminding the model that structural_edit is the only
// available structural-edit tool for this step. ctx.Messages is not
// mutated; the nudge and grammar restriction are scoped to this call.
func callLLMConstrained(ctx *AgentContext) (string, int, error) {
	messages, grammar := buildStepRequest(ctx)

	content, tokens, err := callLLMOnceWithGrammar(ctx, messages, 0.3, grammar)
	if isContextOverflow(err) {
		// The real prompt exceeded the per-slot context despite the
		// budget estimate (dense content under-counts at chars/4).
		// Recover instead of hard-killing the session: force-trim the
		// conversation to the minimum window (system + pins + 8-tail)
		// and retry once. The trim persists on ctx.Messages — the
		// conversation genuinely no longer fits, so shrinking it is
		// the correct durable state, not just a retry hack.
		log.Printf("[agent] context overflow from llama-server — force-trimming to minimum window and retrying")
		ctx.Messages = trimMessages(ctx.Messages, 8)
		messages, grammar = buildStepRequest(ctx)
		content, tokens, err = callLLMOnceWithGrammar(ctx, messages, 0.3, grammar)
	}
	if err != nil {
		return "", tokens, err
	}
	if strings.TrimSpace(content) != "" {
		return content, tokens, nil
	}

	// Empty response — retry once with a transient continuation nudge
	// and a higher temperature. The nudge gives the model an explicit
	// next-action prompt; the temperature bump escapes the EOS-local
	// minimum that the json_object grammar can wedge the model into.
	log.Printf("[agent] empty LLM response, retrying with temp=0.7 + continuation nudge")
	nudged := append(append([]AgentMessage(nil), messages...), AgentMessage{
		Role:    "user",
		Content: `Continue. Respond with one JSON object: {"type":"tool_call","name":"<tool>","args":{...}} for the next action, or {"type":"done","summary":"..."} if the task is complete. Do not emit empty content.`,
	})
	content2, tokens2, err := callLLMOnceWithGrammar(ctx, nudged, 0.7, grammar)
	if err != nil {
		// Return whatever we have from the original call; caller
		// handles empty via parse-error retry.
		return content, tokens, nil
	}
	return content2, tokens + tokens2, nil
}

// buildStepRequest assembles the messages and grammar for the next LLM
// call. In the common case it returns ctx.Messages and "" (no grammar
// override). When the previous turn ended in a write_file rejection on a
// .py/.html file, it returns ctx.Messages plus an ephemeral [system note]
// user message AND a restricted GBNF grammar that excludes edit_file
// and write_file from the tool-name production. See callLLMConstrained
// docstring for the BiasBusters context.
func buildStepRequest(ctx *AgentContext) ([]AgentMessage, string) {
	// Plan-progress reminder. Always rendered when ctx.Plan exists;
	// not persisted to ctx.Messages so it doesn't accumulate. Lands
	// AT THE TAIL of the messages slice so the model sees it as the
	// most-recent user-role input right before its next decision.
	// May 10 2026 follow-up — long multi-file tasks were losing plan
	// context after trim; per-turn injection makes the progress
	// surface persistent without bloating history.
	planReminder := buildPlanReminder(ctx)

	// Traceback step-restriction (issue #39 / option 3): after a crash, ban the
	// run tools so the model can't loop on re-running and is forced to edit the
	// fix site the traceback names. Takes precedence over the write_file case.
	if tbExcluded, tbNote := tracebackExclusion(ctx); len(tbExcluded) > 0 {
		messages := append([]AgentMessage(nil), ctx.Messages...)
		if planReminder != "" {
			messages = append(messages, AgentMessage{Role: "user", Content: planReminder})
		}
		messages = append(messages, AgentMessage{Role: "user", Content: tbNote})
		log.Printf("[agent] traceback step-restriction: banning run tools, forcing an edit")
		return messages, buildGBNFGrammarForTools(tbExcluded)
	}

	excluded, ext := stepExclusions(ctx)
	if len(excluded) == 0 {
		if planReminder == "" {
			return ctx.Messages, ""
		}
		messages := append([]AgentMessage(nil), ctx.Messages...)
		messages = append(messages, AgentMessage{Role: "user", Content: planReminder})
		return messages, ""
	}

	selectors := structuralSelectorHint(ext)
	if selectors == "" {
		selectors = "`function:NAME` or `class:NAME`"
	}
	note := fmt.Sprintf(
		"[system note]: For this single decision, %s is unavailable. The previous write_file was rejected because the target is an existing %s file >5 lines. Use structural_edit with a structural selector (%s) to rewrite the named node. structural_edit doesn't need old_str so it doesn't truncate on long content. Emit exactly one JSON object: {\"type\":\"tool_call\",\"name\":\"structural_edit\",\"args\":{\"path\":\"...\",\"selector\":\"...\",\"content\":\"...\"}}.",
		strings.Join(excluded, " and "),
		strings.TrimPrefix(ext, "."),
		selectors,
	)
	messages := append([]AgentMessage(nil), ctx.Messages...)
	if planReminder != "" {
		messages = append(messages, AgentMessage{Role: "user", Content: planReminder})
	}
	messages = append(messages, AgentMessage{Role: "user", Content: note})

	grammar := buildGBNFGrammarForTools(excluded)
	log.Printf("[agent] step-restriction active: banning %v from tool-name enum (ext=%q) — BiasBusters #2/#3", excluded, ext)
	return messages, grammar
}

// stepExclusions inspects the tail of ctx.Messages and returns the list
// of tool names that must be banned for the next decision, plus the
// triggering file extension. Returns nil/"" in the common case.
//
// Trigger: most recent tool-result message is from write_file with a
// success=false body whose error mentions "already exists", and the
// path being targeted has extension .py / .html / .htm. The window
// scanned is the last 6 messages (assistant call + tool result + a few
// recent siblings).
func stepExclusions(ctx *AgentContext) ([]string, string) {
	n := len(ctx.Messages)
	if n == 0 {
		return nil, ""
	}
	// Walk backwards over the recent tail. We only fire when the LAST
	// tool message is a write_file rejection on .py/.html. If a fresh
	// assistant turn has already happened (the model corrected itself),
	// the tail will end in something other than that tool result and we
	// return nil — the restriction expires after a single decision.
	startIdx := n - 1
	if startIdx > 6 {
		startIdx = 6
	}
	for i := n - 1; i >= n-1-startIdx && i >= 0; i-- {
		msg := ctx.Messages[i]
		if msg.Role != "tool" {
			// First non-tool message we encounter while walking back —
			// stop. We don't want a stale rejection from 4 turns ago to
			// keep firing.
			if msg.Role == "user" && strings.HasPrefix(strings.TrimSpace(msg.Content), "[system note]:") {
				continue
			}
			break
		}
		if msg.ToolName != "write_file" {
			continue
		}
		if !strings.Contains(msg.Content, "already exists") {
			continue
		}
		// Pull the path from the rejection text so we can sniff the ext.
		// The rejection format (see surgical-edit gate) is:
		//   "File <path> already exists (<n> lines). ..."
		const pfx = "File "
		s := msg.Content
		idx := strings.Index(s, pfx)
		if idx < 0 {
			continue
		}
		s = s[idx+len(pfx):]
		spaceIdx := strings.Index(s, " ")
		if spaceIdx < 0 {
			continue
		}
		path := s[:spaceIdx]
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".py" && ext != ".html" && ext != ".htm" {
			return nil, ""
		}
		// Ban write_file (just got rejected) and edit_file (the wrong
		// shortcut the model is biased toward). Leave structural_edit and the
		// read/run/etc tools available.
		return []string{"edit_file", "write_file"}, ext
	}
	return nil, ""
}

// eraseLlamaSlot clears llama.cpp's KV slots to give the next chat
// completion a fresh prefix. Errors are logged and
// swallowed — slot erase is a best-effort isolation step, not a
// correctness requirement.
//
// All slots are erased, not just slot 0. With --parallel > 1 and prompt
// caching on, llama-server picks a slot per request by prefix match /
// LRU, so a new session can land on slot 1..N-1. If only slot 0 were
// cleared, those other slots would still hold a prior session's KV and
// reuse it — the exact cross-session bleed this prevents.
func eraseLlamaSlot(ctx *AgentContext) {
	llamaURL := envOr("ATLAS_LLAMA_URL", ctx.InferenceURL)

	reqCtx := ctx.Ctx
	if reqCtx == nil {
		reqCtx = context.Background()
	}
	client := &http.Client{Timeout: 5 * time.Second}

	erased := 0
	slots := parallelSlots()
	for id := 0; id < slots; id++ {
		endpoint := fmt.Sprintf("%s/slots/%d?action=erase", llamaURL, id)
		req, err := http.NewRequestWithContext(reqCtx, "POST", endpoint, nil)
		if err != nil {
			log.Printf("[agent] erase slot %d: build request failed: %v", id, err)
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[agent] erase slot %d: request failed: %v (continuing — slot is stale, will re-encode)", id, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			log.Printf("[agent] erase slot %d: status %d (continuing — first turn re-encodes prefix)", id, resp.StatusCode)
			continue
		}
		erased++
	}
	log.Printf("[agent] erased %d/%d llama slots — fresh KV cache for this session", erased, slots)
}

// pollPromptProgress emits llm_prompt_progress events at 100ms cadence
// while llama-server is in the prompt-eval phase of a streaming chat
// completion. Without these events the TUI freezes on "encoding prompt…"
// for the 30–90s prompt-eval window on long histories.
//
// Always emits elapsed_ms so the TUI can show a live timer ("encoding
// prompt · 12.3s"). Additionally tries to extract processed/total/pct
// from llama.cpp's /slots endpoint — those fields are only present in
// some llama.cpp builds (n_prompt_tokens_processed / n_prompt_tokens).
// When absent, the TUI renders a spinner-with-timer rather than a bar.
//
// Stops when stop is closed (the caller closes it on first-token
// arrival, on function return, or on context cancel).
//
// totalEst is the chars/4 prompt-token estimate; passed through to the
// TUI as `total_est` so even without /slots data the user sees the
// rough magnitude of what's being encoded.
func pollPromptProgress(ctx *AgentContext, llamaURL string, stop <-chan struct{}, totalEst int) {
	// Defense in depth: if anything panics inside this goroutine
	// (e.g. a write to a closed flusher) don't take the whole proxy
	// down with it. The WaitGroup in callLLMOnce should prevent the
	// race that makes this possible, but a recover here is cheap.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] pollPromptProgress recovered: %v", r)
		}
	}()
	startedAt := time.Now()
	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	// Once /slots returns 404/501 we stop probing it but keep emitting
	// elapsed-time progress events — the timer is the useful signal,
	// the bar is the bonus.
	slotsAvailable := true
	for {
		select {
		case <-stop:
			return
		case <-ctx.Ctx.Done():
			return
		case <-ticker.C:
		}
		elapsed := time.Since(startedAt).Milliseconds()
		processed, total := 0, 0
		if slotsAvailable {
			processed, total, slotsAvailable = probeSlot(ctx.Ctx, client, llamaURL)
		}
		if total == 0 {
			total = totalEst
		}
		pct := 0.0
		if processed > 0 && total > 0 {
			pct = float64(processed) / float64(total)
			if pct > 1 {
				pct = 1
			}
		}
		ctx.Stream("llm_prompt_progress", map[string]interface{}{
			"processed":  processed,
			"total":      total,
			"pct":        pct,
			"elapsed_ms": elapsed,
		})
	}
}

// probeSlot does one /slots GET and pulls out prompt-eval counters when
// llama.cpp exposes them. Returns (processed, total, stillAvailable);
// stillAvailable goes false on 404/501 so the caller can stop probing.
func probeSlot(ctx context.Context, client *http.Client, llamaURL string) (int, int, bool) {
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "GET", llamaURL+"/slots", nil)
	if err != nil {
		return 0, 0, true
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, true // transient — try again next tick
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented {
		return 0, 0, false // /slots disabled — give up
	}
	if resp.StatusCode != http.StatusOK {
		return 0, 0, true
	}
	var slots []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&slots); err != nil {
		return 0, 0, true
	}
	for _, s := range slots {
		if isProc, ok := s["is_processing"].(bool); ok && !isProc {
			continue
		}
		var processed, total int
		for _, k := range []string{"n_prompt_tokens_processed", "prompt_n", "n_past"} {
			if v, ok := s[k].(float64); ok && v > 0 {
				processed = int(v)
				break
			}
		}
		for _, k := range []string{"n_prompt_tokens", "n_prompt"} {
			if v, ok := s[k].(float64); ok && v > 0 {
				total = int(v)
				break
			}
		}
		return processed, total, true
	}
	return 0, 0, true
}

// llmStreamClient is a long-lived HTTP client for streaming LLM calls.
// Streaming responses can run for many minutes (a 4k-token write_file
// generation at ~30 tok/s is ~2min, longer for big content). The old
// 3-minute total Client.Timeout aborted those mid-decode with
// "context deadline exceeded while awaiting headers". Streaming mode
// also makes the total-timeout meaningless: we instead bound only the
// dial + header phases and rely on ctx.Ctx for user-initiated cancel.
//
// ResponseHeaderTimeout note: llama.cpp doesn't flush HTTP response
// headers until the FIRST decoded token arrives — i.e., header time
// = prompt eval time. With a long conversation history (e.g. a 767-line
// HTML file the assistant just wrote, ~8500 tokens) prompt eval can
// take ~60s on the GPU. A tight ResponseHeaderTimeout would cancel
// these legitimate calls. Bumped to 10 min: still bounds a truly hung
// llama-server, but tolerates large prompts. User Ctrl+C still works
// via the request context for any in-flight call.
// May 10 2026: ResponseHeaderTimeout removed. V3 pipelines that fire
// on T2+ edits routinely take 5-15 minutes between when the proxy
// posts and when llama-server flushes the first response header (it
// flushes on first decoded token, but prompt eval after a long V3
// run can take ages on cold KV state). The 10-minute cap fired on
// turn 5 of a real session and killed an otherwise-working chain.
// User instruction was to remove the timeout stuff completely.
// Dial timeout stays — connection refused / DNS failure should still
// fail fast; that's a different failure mode from "server is working
// but slow." Request-context cancellation via ctx.Ctx still works,
// so user-initiated cancels still propagate.
var llmStreamClient = &http.Client{
	Transport: &http.Transport{
		DialContext:     (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		IdleConnTimeout: 90 * time.Second,
	},
}

// callLLMOnce is one round-trip to llama-server's /v1/chat/completions.
// Extracted from callLLMConstrained so the empty-response retry can
// reuse the same plumbing with a different temperature + message list.
//
// Uses SSE streaming so the proxy can forward per-token deltas to the
// TUI as `llm_token` events. The first delta also fires `llm_first_token`
// with the prompt-eval duration — that gap (request sent → first token)
// is llama-server doing prompt processing, which the user couldn't see
// before. Streaming mode also removes the 3-minute total-request timeout
// that was killing long generations on a single write_file with
// substantial content (HTML mockups, code with imports, etc.).
func callLLMOnce(ctx *AgentContext, messages []AgentMessage, temperature float64) (string, int, error) {
	return callLLMOnceWithGrammar(ctx, messages, temperature, "")
}

// callLLMOnceWithGrammar is callLLMOnce with an optional GBNF grammar
// override. When grammar != "", llama-server enforces it at the
// token-decode level (BiasBusters #2 — banning edit_file/write_file from
// the tool-name production for a single decision). The json_object
// response_format is dropped in that case because GBNF is the more
// specific constraint and supersedes it.
// toWireMessages converts the agent's internal messages to the role/content
// pairs sent on /v1/chat/completions.
//
// Tool results are rendered as a USER turn. Some chat templates have no `tool`
// role and silently drop role:"tool" messages — the model never sees the
// result (verified: the prompt carries only the user/assistant turns and the
// model reasons "the tool output is not visible"), so it re-issues the same
// tool call forever until the repetition breaker fires. This was the real
// cause behind every "it can't see what it's reading / it just loops" report.
// Every chat template handles the user role, so converting here is
// model-agnostic; the `[tool result]` marker
// tells the model this is tool output, not a fresh user instruction.
// ctx.Messages keeps the semantic "tool" role so trim-pinning and the
// step/traceback exclusions that key off ToolName still work.
func toWireMessages(messages []AgentMessage) []map[string]string {
	wire := make([]map[string]string, len(messages))
	for i, msg := range messages {
		role := msg.Role
		content := msg.Content
		if role == "tool" {
			role = "user"
			content = "[tool result] " + content
		}
		wire[i] = map[string]string{"role": role, "content": content}
	}
	return wire
}

// applyRepetitionSampling sets the repetition-control sampler fields on an
// outgoing llama-server request.
//
// llama-server ships every repetition control off: querying /props on a
// running instance reports repeat_penalty=1.0, dry_multiplier=0.0,
// frequency_penalty=0.0, presence_penalty=0.0. Nothing bounded how long a
// generation could repeat itself, which is what the stream-level
// isLoopingTail cut in callLLMOnceWithGrammar exists to catch. That cut is
// a backstop; it does not stop the model entering the loop.
//
// DRY rather than repeat_penalty. repeat_penalty scores individual token
// reoccurrence, which is wrong for code: indentation, `return`, `self.`,
// and closing braces legitimately repeat many times in one file. DRY scores
// repeated *sequences*, and llama.cpp treats "\n" as a sequence breaker by
// default, so per-line repetition across lines is not penalized at all.
// dry_allowed_length is raised above llama.cpp's default of 2 for the same
// reason — 3-token runs are ordinary in source.
//
// The defaults here reduce how often the tail loop is entered; they have not
// been A/B'd against a benchmark run. ATLAS_DRY_MULTIPLIER=0 disables DRY
// outright. ATLAS_REPEAT_PENALTY is available for the pure-repeated-newline
// degeneration that DRY's newline sequence-breaker cannot see, and defaults
// off precisely because of the code-repetition cost above.
func applyRepetitionSampling(reqBody map[string]interface{}) {
	dryMultiplier := envFloatOr("ATLAS_DRY_MULTIPLIER", 0.8)
	if dryMultiplier > 0 {
		reqBody["dry_multiplier"] = dryMultiplier
		reqBody["dry_base"] = envFloatOr("ATLAS_DRY_BASE", 1.75)
		reqBody["dry_allowed_length"] = envIntOr("ATLAS_DRY_ALLOWED_LENGTH", 6)
		// Bound the lookback so DRY scans the current generation rather
		// than the whole 32k window; -1 (llama.cpp's default) would make
		// every prior turn's text a repetition source.
		reqBody["dry_penalty_last_n"] = envIntOr("ATLAS_DRY_PENALTY_LAST_N", 2048)
	}
	if rp := envFloatOr("ATLAS_REPEAT_PENALTY", 1.0); rp != 1.0 {
		reqBody["repeat_penalty"] = rp
		reqBody["repeat_last_n"] = envIntOr("ATLAS_REPEAT_LAST_N", 64)
	}
}

// envFloatOr reads a float tunable from the environment, falling back to def
// when unset or unparseable.
func envFloatOr(key string, def float64) float64 {
	if v := envOr(key, ""); v != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	}
	return def
}

// envIntOr reads an int tunable from the environment, falling back to def
// when unset or unparseable.
func envIntOr(key string, def int) int {
	if v := envOr(key, ""); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func callLLMOnceWithGrammar(ctx *AgentContext, messages []AgentMessage, temperature float64, grammar string) (string, int, error) {
	wireMessages := toWireMessages(messages)

	llamaURL := envOr("ATLAS_LLAMA_URL", ctx.InferenceURL)

	// Per-turn hard ceiling (agentMaxTokens, default 8192). 32768 let a
	// rambling content blob run the full window (~18 min at the GPU's capped
	// decode rate) — the runaway nothing else caught, since the reasoning
	// budget only fires on reasoning-WITHOUT-content. An agent turn is a tool
	// call (small) or a whole-file write_file (a few thousand tokens); 8192
	// covers a ~600-line generation. Truncation recovery backstops the rare
	// legit overflow. conversationTokenBudget reserves this same value.
	reqBody := map[string]interface{}{
		"model":       modelName,
		"messages":    wireMessages,
		"temperature": temperature,
		"max_tokens":  agentMaxTokens(),
		"stream":      true,
		// Without include_usage, the final SSE chunk before [DONE] has no
		// usage block, so we can't report total_tokens to the TUI.
		"stream_options": map[string]bool{"include_usage": true},
		// Some reasoning-capable chat templates default to thinking, but the
		// agent loop relies on grammar-constrained JSON output — thinking
		// blocks would just bloat tokens and llama-server rejects the
		// combination outright once a trailing assistant message looks
		// like a "response prefill" (400: "Assistant response prefill is
		// incompatible with enable_thinking"). Disable explicitly.
		"chat_template_kwargs": map[string]bool{"enable_thinking": false},
	}
	applyRepetitionSampling(reqBody)
	if grammar != "" {
		// Token-level restriction wins over response_format. llama-server
		// rejects requests that pass both response_format=json_object and
		// a non-trivial grammar; pass only the grammar in restricted mode.
		reqBody["grammar"] = grammar
	} else {
		reqBody["response_format"] = buildResponseFormat()
	}
	body, _ := json.Marshal(reqBody)
	endpoint := llamaURL + "/v1/chat/completions"

	// Carry the agent's request context into the HTTP request so client
	// disconnects propagate down to llama-server.
	reqCtx := ctx.Ctx
	if reqCtx == nil {
		reqCtx = context.Background()
	}
	httpReq, err := http.NewRequestWithContext(reqCtx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	// Don't reuse the TCP connection across turns. We were seeing
	// `Post ".../v1/chat/completions": EOF` failures in 0ms between
	// back-to-back turns: the previous streaming response left the
	// connection in a state llama-server (--parallel 1) closed at its
	// end, then the next turn's POST reused the dead idle connection
	// from Go's pool and got EOF on first read. Setting Close=true
	// forces a fresh dial per call. The dial overhead is negligible
	// next to a 5k-token prompt eval, and the reliability win is huge.
	httpReq.Close = true

	sentAt := time.Now()

	// Estimate total prompt tokens (chars/4 — works for English + code
	// within ~10–20%) so the prompt-progress poller has a baseline even
	// when /slots doesn't expose n_prompt_tokens directly.
	promptTokenEst := 0
	for _, m := range messages {
		promptTokenEst += len(m.Content) / 4
	}
	// pollPromptProgress runs as a sibling goroutine while the LLM call is
	// in flight; it streams elapsed_ms ticks back to the TUI. We MUST
	// guarantee it has fully exited before callLLMOnce returns — otherwise
	// it can call ctx.Stream (which writes to handleAgent's flusher) AFTER
	// handleAgent has returned and the response writer is invalid, causing
	// a SIGSEGV inside bufio.(*Writer).Flush. The defers run LIFO: stop
	// the channel first, then wait on the WaitGroup until the goroutine
	// exits.
	stopProgress := make(chan struct{})
	var stopOnce sync.Once
	stopProgressFn := func() { stopOnce.Do(func() { close(stopProgress) }) }
	var pollWG sync.WaitGroup
	pollWG.Add(1)
	go func() {
		defer pollWG.Done()
		pollPromptProgress(ctx, llamaURL, stopProgress, promptTokenEst)
	}()
	defer pollWG.Wait()
	defer stopProgressFn()

	resp, err := llmStreamClient.Do(httpReq)
	if err != nil {
		return "", 0, fmt.Errorf("LLM request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", 0, fmt.Errorf("LLM returned %d: %s",
			resp.StatusCode, truncateStr(string(respBody), 500))
	}

	var (
		contentBuf strings.Builder
		// PC-?: capture reasoning_content separately so we can fall
		// back to it when contentBuf is empty. Some models occasionally
		// engages thinking mode despite enable_thinking=false (most
		// reproducibly on retries with bumped temperature) — when it
		// does, ALL output streams into delta.reasoning_content. The
		// previous version threw it away and returned an empty string,
		// which fired the empty-response retry uselessly. Now we
		// surface the reasoning as content (with <think> tags stripped)
		// so the agent loop has SOMETHING to parse.
		reasoningBuf   strings.Builder
		totalTokens    int
		firstTokenSent bool
		reasoningCut   bool
		contentLoopCut bool
		lastLoopCheck  int
	)

	// Per-turn reasoning budget. A reasoning-heavy model can spiral for
	// tens of thousands of tokens inside ONE generation (observed: a
	// 14-minute, ~17K-token deliberation over a 24-line file that ended
	// with no tool call) — max_tokens (32768) is the only bound and it
	// allows ~25 minutes of silence. When accumulated reasoning passes
	// the budget we stop reading; closing the response body cancels the
	// slot server-side. The post-loop recovery path then either extracts
	// a tool_call already present in the reasoning, or returns empty so
	// the caller's standard re-prompt ("emit your tool call now") fires.
	// Token-estimate at 4 chars/token; ATLAS_REASONING_BUDGET (tokens)
	// overrides, 0 disables. Keyed off stream state, not model identity.
	reasoningBudgetChars := 6144 * 4
	if v := envOr("ATLAS_REASONING_BUDGET", ""); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			reasoningBudgetChars = n * 4
		}
	}

	scanner := bufio.NewScanner(resp.Body)
	// Default scanner buffer is 64KB which is fine per line, but bump
	// the max in case llama-server emits a fat usage payload at the end.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				TotalTokens      int `json:"total_tokens"`
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, c := range chunk.Choices {
			if c.Delta.ReasoningContent != "" {
				// First output of ANY kind means prompt eval is done — for
				// reasoning models (some stream their whole chain as
				// reasoning_content, often with no content tokens until the
				// final JSON) the first delta is reasoning, not content.
				// Stop the prompt-eval poller and fire llm_first_token here
				// too; otherwise the poller keeps emitting prompt_progress
				// for the entire generation, the TUI keeps painting
				// "encoding", and it fights the streaming reasoning for the
				// row — the encode timer never stops and the screen flickers.
				if !firstTokenSent {
					stopProgressFn()
					ctx.Stream("llm_first_token", map[string]interface{}{
						"prompt_ms": time.Since(sentAt).Milliseconds(),
					})
					firstTokenSent = true
				}
				// Accumulate for the empty-content fallback below AND
				// stream to the TUI as a separate `reasoning_token` event
				// so users can see the model's thought process. The TUI
				// subscribes to reasoning_token distinctly from llm_token
				// so it can render thinking dimmed without mixing it into
				// the content stream destined for parse.
				reasoningBuf.WriteString(c.Delta.ReasoningContent)
				ctx.Stream("reasoning_token", map[string]interface{}{
					"text": c.Delta.ReasoningContent,
				})
				if reasoningBudgetChars > 0 && reasoningBuf.Len() > reasoningBudgetChars && contentBuf.Len() == 0 {
					reasoningCut = true
				}
			}
			if c.Delta.Content == "" {
				continue
			}
			if !firstTokenSent {
				stopProgressFn() // prompt eval done — kill the poller
				ctx.Stream("llm_first_token", map[string]interface{}{
					"prompt_ms": time.Since(sentAt).Milliseconds(),
				})
				firstTokenSent = true
			}
			contentBuf.WriteString(c.Delta.Content)
			ctx.Stream("llm_token", map[string]interface{}{
				"text": c.Delta.Content,
			})
			// Content-loop cut. Some models state the right answer then
			// spirals on self-doubt in the CONTENT stream ("...the first line
			// is X. Wait, I can't see the output. I'll just say X. Wait, I
			// can't..." repeating) — the reasoning budget doesn't catch it
			// (that's content, not reasoning_content), so it ran to max_tokens.
			// Detect a verbatim repeating tail and cut. Checked periodically
			// to keep it O(n) overall.
			if !contentLoopCut && contentBuf.Len() > 600 && contentBuf.Len()-lastLoopCheck > 200 {
				lastLoopCheck = contentBuf.Len()
				if isLoopingTail(contentBuf.String()) {
					contentLoopCut = true
				}
			}
		}
		if chunk.Usage != nil && chunk.Usage.TotalTokens > 0 {
			totalTokens = chunk.Usage.TotalTokens
		}
		if reasoningCut {
			log.Printf("[agent] reasoning budget exceeded (%d chars, ~%d tokens) with no content emitted — cutting the stream and re-prompting",
				reasoningBuf.Len(), reasoningBuf.Len()/4)
			ctx.Stream("reasoning_budget_cut", map[string]interface{}{
				"reasoning_chars": reasoningBuf.Len(),
			})
			break
		}
		if contentLoopCut {
			log.Printf("[agent] content loop detected (%d chars) — model repeating itself; cutting the stream", contentBuf.Len())
			ctx.Stream("content_loop_cut", map[string]interface{}{"chars": contentBuf.Len()})
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return contentBuf.String(), totalTokens,
			fmt.Errorf("read LLM stream: %w", err)
	}

	// Stash the reasoning content on ctx so the agent loop's per-turn
	// reasoning-repetition detector can compare it against prior turns.
	// We capture regardless of whether contentBuf was non-empty — the
	// model may emit BOTH content (the JSON tool call) AND reasoning
	// (the prose narration), and we want to detect rehashed reasoning
	// even when a tool call was successfully emitted.
	ctx.LastTurnReasoning = reasoningBuf.String()

	if contentBuf.Len() == 0 {
		// No content deltas — check reasoning_content. Two distinct cases:
		//
		//   (a) Model dumped its actual response into the thinking
		//       stream despite template-level reasoning being disabled).
		//       reasoning_content contains a JSON
		//       tool_call; we recover it and parse normally.
		//
		//   (b) Model emitted ONLY thinking ("Now I need to read...")
		//       and terminated without producing a response. The
		//       reasoning_content is pure prose narration — there's no
		//       tool call to recover. Earlier we returned this prose
		//       as the "response" and it parse-errored every time,
		//       wasting a turn. Pre-May-8 behavior had the agent
		//       loop's classifyParseFailure scolding the model with
		//       "respond in JSON only" — but the corrective is
		//       useless when the response was truly empty (model
		//       wasn't disobeying the format, it just stopped mid-flow).
		//
		// New behavior: only return recovered reasoning when it
		// CONTAINS a tool_call envelope. For pure prose, return empty
		// + log so the caller's retry path can re-prompt with a
		// "you produced thinking but no response — emit your tool
		// call now" message instead of treating prose as a failed
		// response.
		if reasoningBuf.Len() > 0 {
			if recovered, ok := recoverStructuredReasoning(reasoningBuf.String()); ok {
				log.Printf("[agent] empty content but %d chars of reasoning_content contained a structured agent response — recovering",
					reasoningBuf.Len())
				return recovered, totalTokens, nil
			}
			// Pure prose narration in reasoning_content with no tool
			// call. Don't return it — let the caller retry. Logged so
			// the failure mode stays visible.
			log.Printf("[agent] %d chars of reasoning_content had no valid agent envelope — discarding so caller can re-prompt",
				reasoningBuf.Len())
		}
		// Truly nothing (or only narration). Caller's empty-response
		// retry path (callLLMConstrained) will handle.
		return "", totalTokens, nil
	}
	return contentBuf.String(), totalTokens, nil
}

// ---------------------------------------------------------------------------
// Permission checking
// ---------------------------------------------------------------------------

// needsPermission returns true if the tool call requires user confirmation.
func needsPermission(ctx *AgentContext, toolName string, args json.RawMessage) bool {
	if ctx.YoloMode || ctx.PermissionMode == PermissionYolo {
		return false
	}

	tool := getTool(toolName)
	if tool == nil {
		return true // unknown tool always requires permission
	}

	// Read-only tools never need permission
	if tool.ReadOnly {
		return false
	}

	// Tools the client pre-approved for the session (or the user approved
	// with session scope earlier this turn) skip the prompt.
	if ctx.isToolAllowed(toolName) {
		return false
	}

	// In accept-edits mode, file writes and edits are auto-approved;
	// run_command and delete_file still prompt.
	if ctx.PermissionMode == PermissionAcceptEdits {
		if toolName == "write_file" || toolName == "edit_file" || toolName == "structural_edit" || toolName == "move_file" {
			return false
		}
	}

	// Anything that runs a command asks, whatever the tool is called:
	// run_background started any command without a prompt while run_command
	// asked for the same one.
	if commandTools[toolName] {
		return true
	}
	// Destructive tools need permission in default mode
	return tool.Destructive
}

// ---------------------------------------------------------------------------
// System prompt construction
// ---------------------------------------------------------------------------

func buildSystemPrompt(ctx *AgentContext) string {
	var sb strings.Builder

	sb.WriteString("You are ATLAS, a coding assistant that creates and modifies code by calling tools. ")
	sb.WriteString("You have access to the filesystem and can run commands to verify your work.\n")
	sb.WriteString("You MUST respond with ONLY a single valid JSON object, no other text.\n\n")

	// Pick-the-right-shape guidance — this is what keeps "hi" out of the
	// tool-call rabbit hole. Without it the model treats every input as a
	// task and starts read_file'ing random paths.
	sb.WriteString("## Choosing your response shape\n\n")
	sb.WriteString("- **Conversational input** (greetings, small talk, questions about YOU, status checks): emit `{\"type\":\"text\",\"content\":\"...\"}` — the turn ends after one text reply, and the user can follow up. Do NOT call tools to answer \"hi\" or \"what can you do\".\n")
	sb.WriteString("- **Questions about the CODE** (\"what does X do\", \"why is this slow\", \"is this a bug\") are NOT that case: read the file first, then answer in one `text` reply. You have read_file and outline_file — use them. Never ask the user to paste a file that is already in the workspace, and never answer from a guess about code you have not opened. Reading to answer a question is not \"starting work\": make no edits unless the user asked for one.\n")
	sb.WriteString("- **Coding tasks** (\"fix the bug\", \"add a feature\", \"refactor X\"): emit `{\"type\":\"tool_call\",...}` to make progress, repeat as needed, then emit `{\"type\":\"done\",\"summary\":\"...\"}` when finished.\n")
	sb.WriteString("- **Don't use `text` mid-task.** Roll narration into the done.summary at the end, or skip it entirely. Mid-task `text` ends the turn early.\n")
	sb.WriteString("- **When unsure** whether the user wants chat or work: ask in a single `text` reply. Don't speculatively start tool-calling — but reading a file the user named is never speculative.\n\n")

	// Tool descriptions.
	sb.WriteString(buildToolDescriptionsExcluding(nil))

	// Rules
	sb.WriteString("## Rules\n\n")
	sb.WriteString("- To work on an EXISTING file, navigate it cheaply first: call `outline_file` to list its functions/classes with line ranges, then `read_file` with `offset`/`limit` to read just the part you need (e.g. the buggy function). Don't dump a whole large file into context — and never re-read the same file in a loop; if a read's content is already in the conversation, act on it.\n")
	sb.WriteString("- Always read the relevant code before editing it (outline_file → read_file, then edit_file/structural_edit).\n")
	sb.WriteString("- MANDATORY: Use `edit_file` (targeted old_str/new_str) for any change to a file that already exists, no matter how small. `write_file` is ONLY for creating brand-new files. The agent layer rejects every `write_file` call against an existing file >5 lines — your call won't execute and you'll get a tool error directing you to edit_file. Don't re-emit a whole file to change a few lines.\n")
	sb.WriteString("  Example — to add a None check to one branch, use:\n")
	sb.WriteString("    edit_file {\"path\":\"src/foo.py\",\"old_str\":\"if x == 0:\\n        return None\",\"new_str\":\"if x is None or x == 0:\\n        return None\"}\n")
	sb.WriteString("  NOT write_file with the entire file's new contents.\n")
	sb.WriteString("- For WHOLE-FUNCTION or WHOLE-ELEMENT rewrites, prefer `structural_edit` over `edit_file`. structural_edit takes a structural selector (`function:NAME`, `class:NAME`, `<tag>` for HTML) and replaces that one whole named block — no need to copy the existing function as old_str. Selector must match exactly one node; ambiguous selectors return an error so you can be more specific. Decorators are included automatically when selecting a Python function. Available v1 only on `.py` and `.html`/`.htm` files.\n")
	sb.WriteString("    structural_edit {\"path\":\"app.py\",\"selector\":\"function:dashboard\",\"content\":\"@app.route('/dashboard')\\ndef dashboard():\\n    return render_template('dashboard.html')\"}\n")
	sb.WriteString("    structural_edit {\"path\":\"templates/index.html\",\"selector\":\"<body>\",\"content\":\"<body>\\n  <h1>Welcome</h1>\\n  ...\\n</body>\"}\n")
	sb.WriteString("- WHEN write_file IS REJECTED for an existing file: if the file is `.py`, `.html`, or `.htm` and you're replacing the whole thing (e.g. swapping the entire body, replacing the dashboard function), use `structural_edit` next, not edit_file. structural_edit doesn't need `old_str` so it doesn't hit the max_tokens truncation that kills long edit_file calls. Use edit_file ONLY for surgical inline string changes (one line, one expression). This rule applies even when conversation trimming has dropped the original rejection message — re-derive the intent from the file extension and the size of your replacement.\n")
	sb.WriteString("- JSON strings in tool args contain LITERAL characters: write `<` not `&lt;`, `>` not `&gt;`, `&` not `&amp;`. The file content goes verbatim onto disk — `&lt;!DOCTYPE&gt;` would write the literal text `&lt;!DOCTYPE&gt;` instead of `<!DOCTYPE>`. NEVER HTML-encode angle brackets inside `content`, `old_str`, or `new_str`.\n")
	sb.WriteString("- The `content` you put in write_file / edit_file goes verbatim onto disk. **No markdown fences. No prose preamble (\"Looking at the task...\", \"Here's the file:\"). No trailing explanation.** Just the raw file contents. The agent layer strips fenced wrappers before writing, but the right move is to never emit them in the first place.\n")
	sb.WriteString("- For CONTENT changes, prefer the dedicated tools — `edit_file` (targeted), `write_file` (new files), `structural_edit` (whole node) — they go through the validation pipeline. For moving / renaming / reorganizing files you may use either `move_file` or shell `mv`/`cp` via run_command; both work. `run_command` runs a real shell (in an isolated sandbox confined to this project), so ordinary file operations (mv, cp, mkdir, rm of a specific file, chmod) are fine. Only catastrophic commands are blocked: wiping the whole project (`rm -rf /`, `rm -rf .`, `rm -rf *`), fork bombs, and device/filesystem destruction.\n")
	sb.WriteString("- Use run_command to verify your changes (build, test, lint, curl). For \"fix\"/\"isn't working\" prompts, verify before `done`.\n")
	sb.WriteString("- For LONG-RUNNING commands (servers): `run_background(cmd)` → `run_command(\"curl ...\")` → `stop_background(job_id)`. Don't use `timeout 5 ... || true` — server dies before probe hits.\n")
	sb.WriteString("- When creating a project from scratch: create config/build files FIRST, verify they work (e.g., npm install, cargo check), THEN create feature code\n")
	sb.WriteString("- Respond with {\"type\":\"done\",\"summary\":\"...\"} when the task is complete\n")
	sb.WriteString("- If a command fails, read the error output, fix the issue, and try again\n")
	sb.WriteString("- Do not guess at file contents — read first, then edit\n")
	sb.WriteString("- ALWAYS use relative file paths (`app.py`, `src/main.rs`), NEVER absolute paths and NEVER prefix with `workspace/` — that's the parent dir, not your project root.\n")
	sb.WriteString("- When adding features to an existing project, read at most 2-3 files to understand the structure, then immediately write your changes. Do not explore the entire directory tree. Prioritize writing code over reading code.\n\n")

	// Project context
	if ctx.Project != nil {
		sb.WriteString("## Project Context\n\n")
		sb.WriteString(fmt.Sprintf("Language: %s\n", ctx.Project.Language))
		if ctx.Project.Framework != "" {
			sb.WriteString(fmt.Sprintf("Framework: %s\n", ctx.Project.Framework))
		}
		if ctx.Project.BuildCommand != "" {
			sb.WriteString(fmt.Sprintf("Build command: %s\n", ctx.Project.BuildCommand))
		}
		if ctx.Project.DevCommand != "" {
			sb.WriteString(fmt.Sprintf("Dev command: %s\n", ctx.Project.DevCommand))
		}
		if len(ctx.Project.ConfigFiles) > 0 {
			sb.WriteString(fmt.Sprintf("Config files: %s\n", strings.Join(ctx.Project.ConfigFiles, ", ")))
		}
		sb.WriteString("\n")
	}

	// Working directory
	sb.WriteString(fmt.Sprintf("Working directory: %s\n\n", ctx.WorkingDir))

	// Toolchain hints. Detect every recognized language manifest in
	// the project and surface the runners + install commands so the
	// model picks the right tool per file edit. Polyglot projects
	// (React + Django + deploy scripts) get one entry per ecosystem.
	// Covers every toolchain, not just Python's venv. Probe-first
	// hints — whether the deps are already importable — are added
	// per-toolchain when the evidence is on disk.
	if tcs := detectProjectToolchains(ctx.WorkingDir); len(tcs) > 0 {
		sb.WriteString("## Toolchains\n")
		for _, tc := range tcs {
			line := fmt.Sprintf("- **%s** — runner `%s`", tc.Name, displayRelativeRunner(tc.Runner, ctx.WorkingDir))
			if tc.InstallCommand != "" {
				line += fmt.Sprintf(", install `%s`", tc.InstallCommand)
			}
			if tc.TestCommand != "" {
				line += fmt.Sprintf(", tests `%s`", tc.TestCommand)
			}
			if probe := probeToolchainReady(ctx.WorkingDir, tc); probe != "" {
				line += " [" + probe + "]"
			}
			sb.WriteString(line + "\n")
		}
		sb.WriteString("Skip install when status is `ready`; install only what's missing.\n\n")
	}

	if ctx.VerifyOnHost {
		sb.WriteString("`run_command` targets the host (not sandbox). Sees host env/services/paths.\n\n")
	}

	// Show which files are in the project (names only, not full content).
	// Full content is available via read_file if needed.
	// This avoids consuming context window with pre-injected file dumps.
	if filesRead := ctx.SnapshotFilesRead(); len(filesRead) > 0 {
		sb.WriteString("## Project Files Available\n")
		for path := range filesRead {
			sb.WriteString(fmt.Sprintf("- %s\n", path))
		}
		sb.WriteString("\nUse read_file to inspect these files if needed. To MODIFY any of them, use edit_file — write_file against an existing file (>5 lines) is rejected at the agent layer.\n\n")
	}

	// Plan section. When the planner returned a plan, surface it so
	// the model has explicit step guidance instead of having to infer
	// the right shape from the user message alone. Plans are advisory
	// (the agent layer doesn't hard-block off-plan calls), but having
	// them in the system prompt visibly improves first-call accuracy.
	if ctx.Plan != nil && len(ctx.Plan.Steps) > 0 {
		sb.WriteString("## Plan\n\n")
		sb.WriteString("A planner has proposed these steps for the user's request. ")
		sb.WriteString("Follow them in order when sensible. ")
		sb.WriteString("Deviate only if a step's premise is wrong (file doesn't exist, command unavailable, etc.) — the agent layer notices repeated off-plan calls and will silently revise the plan with what you've discovered.\n\n")
		for i, step := range ctx.Plan.Steps {
			marker := " "
			if step.ID == ctx.Plan.VerifyStep {
				marker = "✓" // verify step
			}
			sb.WriteString(fmt.Sprintf("%d. [%s] **%s** %s — %s\n",
				i+1, marker, step.Action, step.Target, step.Why))
		}
		if ctx.Plan.Rationale != "" {
			sb.WriteString(fmt.Sprintf("\n_%s_\n", ctx.Plan.Rationale))
		}
		if ctx.Plan.VerifyStep != "" {
			sb.WriteString(fmt.Sprintf("\nThe verify step (%s) is your evidence the fix worked — don't emit `done` until it has run successfully.\n", ctx.Plan.VerifyStep))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// estTokens is a cheap, model-agnostic token estimate: ~4 chars/token plus
// a small per-message framing overhead. Good enough for budgeting; we leave
// generous headroom so the estimate never has to be exact.
func estTokens(content string) int {
	return len(content)/4 + 8
}

// conversationTokenBudget is how many prompt tokens the agent loop will let
// the conversation grow to before trimming. Derived from the deployment's
// per-slot context (ATLAS_CTX_SIZE / ATLAS_PARALLEL_SLOTS), reserving ~35%
// for the response. Model-agnostic: keys off the context the deploy gives,
// not the model identity. Falls back to a safe default when env is absent.
func conversationTokenBudget() int {
	ctxSize := 131072
	if v := envOr("ATLAS_CTX_SIZE", ""); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			ctxSize = n
		}
	}
	perSlot := ctxSize / parallelSlots()
	// Sliding window sized to the actual slot: reserve room for the model's
	// reply (max_tokens) plus a margin for system-prompt growth and tokenizer
	// slack, and give the REST of the slot to the conversation. The previous
	// flat 14k cap was too aggressive — on a 32k slot it left ~10k unused AND
	// dropped the file the model was editing, so weak models hallucinated
	// symbols/lines they could no longer see. The active file is additionally
	// pinned in trimMessages so it survives the window regardless. The
	// model-agnostic re-encode cost (SWA models re-process the prompt each
	// turn) is bounded by the slot itself; deploys that need it smaller can
	// still set ATLAS_AGENT_HISTORY_BUDGET.
	// Reserve: the model's reply (max_tokens), a fixed margin for
	// system-prompt growth, and a proportional tokenizer-slack margin.
	// estTokens is chars/4, which UNDER-counts dense content (code,
	// JSON-escaped tool results run closer to 3 chars/token) — without
	// the proportional slack the estimate can pass while the real
	// prompt exceeds the slot (observed: 32844 real vs 32768 slot).
	budget := perSlot - agentMaxTokens() - 2048 - perSlot/8
	if budget < 4000 {
		budget = 4000 // floor: tiny-context deploys still keep a usable window
	}
	// Optional hard ceiling — unset by default. Only set
	// ATLAS_AGENT_HISTORY_BUDGET to bound per-turn re-encode cost below the
	// slot capacity (trades retained context for faster turns on SWA models).
	if v := envOr("ATLAS_AGENT_HISTORY_BUDGET", ""); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 && n < budget {
			budget = n
		}
	}
	return budget
}

// isLoopingTail reports whether the content stream has degenerated into a
// verbatim repeating phrase — the signature of a model spiraling on the same
// sentence ("...the first line is X. Wait, I can't see the output. I'll just
// say X. Wait, I can't see..."). Takes a chunk from the tail and counts its
// occurrences; 3+ verbatim repeats is a loop a real response never produces.
func isLoopingTail(s string) bool {
	const probe = 48
	if len(s) < probe*3 {
		return false
	}
	tail := s[len(s)-probe:]
	if strings.TrimSpace(tail) == "" {
		return false
	}
	return strings.Count(s, tail) >= 3
}

// agentMaxTokens is the per-turn generation ceiling (ATLAS_MAX_TOKENS,
// default 8192). Shared by the LLM request and conversationTokenBudget so the
// window and the reply reservation stay consistent.
func agentMaxTokens() int {
	maxTokens := 8192
	if v := envOr("ATLAS_MAX_TOKENS", ""); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			maxTokens = n
		}
	}
	return maxTokens
}

// parallelSlots returns the llama-server --parallel slot count for this
// deployment (ATLAS_PARALLEL_SLOTS), defaulting to 4 to match the
// entrypoint. Used both for KV-slot isolation and per-slot context math.
func parallelSlots() int {
	slots := 4
	if v := envOr("ATLAS_PARALLEL_SLOTS", ""); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			slots = n
		}
	}
	return slots
}

// pinnedIndices returns the two message indices trimMessages will keep
// regardless of the tail window: the most recent user message (the task)
// and the most recent file-content tool result (the file being edited).
// Either is -1 when absent. Shared by trimMessages (which re-injects
// them) and budgetedKeepLast (which must COUNT them — see below).
func pinnedIndices(msgs []AgentMessage) (pinIdx, filePinIdx int) {
	pinIdx, filePinIdx = -1, -1
	for i := len(msgs) - 1; i >= 1; i-- {
		if msgs[i].Role == "user" {
			pinIdx = i
			break
		}
	}
	for i := len(msgs) - 1; i >= 1; i-- {
		if msgs[i].Role == "tool" && (msgs[i].ToolName == "read_file" || msgs[i].ToolName == "outline_file") &&
			!strings.Contains(msgs[i].Content, "You already read") { // skip dedup pointers — they carry no content
			filePinIdx = i
			break
		}
	}
	return pinIdx, filePinIdx
}

// budgetedKeepLast returns how many trailing messages trimMessages should
// keep so the kept set (system + pinned user + pinned file + tail) fits the
// token budget. Floored at 8 (never trim more aggressively than the old
// fixed rule); when the whole conversation fits, returns len(msgs) so
// nothing is trimmed.
//
// The pinned messages MUST be pre-counted here: trimMessages re-injects
// them even when they fall outside the tail window, so a budget that
// ignored them under-counted the real prompt by the size of the pinned
// read_file — observed live as a llama-server 400 exceed_context_size
// (32844 > 32768 per-slot) hard-killing a bench session.
func budgetedKeepLast(msgs []AgentMessage) int {
	if len(msgs) == 0 {
		return 0
	}
	budget := conversationTokenBudget()
	used := estTokens(msgs[0].Content) // system prompt is always kept
	pinIdx, filePinIdx := pinnedIndices(msgs)
	if pinIdx >= 1 {
		used += estTokens(msgs[pinIdx].Content)
	}
	if filePinIdx >= 1 && filePinIdx != pinIdx {
		used += estTokens(msgs[filePinIdx].Content)
	}
	keep := 0
	for i := len(msgs) - 1; i >= 1; i-- {
		t := 0
		if i != pinIdx && i != filePinIdx { // already counted above
			t = estTokens(msgs[i].Content)
		}
		if used+t > budget && keep >= 8 {
			break
		}
		used += t
		keep++
	}
	if keep > len(msgs)-1 {
		keep = len(msgs) - 1
	}
	return keep
}

// trimMessages caps a conversation at roughly 1 (system) + 1 (pinned user) +
// keepLast tail messages, dropping the middle. The pin is the most recent
// role=="user" message — the user's current task. Without the pin, long agent
// loops (5+ tool calls) push the user's instruction off the end of the
// keepLast window, the model loses the task, and replies generically
// ("Hi! I'm ATLAS..."). If the pinned message already lives inside the tail
// window we don't duplicate it.
//
// Assumes msgs[0] is the system prompt.
func trimMessages(msgs []AgentMessage, keepLast int) []AgentMessage {
	if len(msgs) <= keepLast+1 {
		return msgs
	}

	// Pins (shared scan with budgetedKeepLast, which counts them): the
	// most-recent user message — the task — and the most-recent
	// file-content tool result (read_file / outline_file), so the file the
	// model is working on never gets trimmed out from under it. Without
	// the file pin, a long agent loop drops the file content, the model
	// edits BLIND, and a weak model then hallucinates symbols and old_str
	// that aren't in the file (observed live: structural_edit
	// function:count_items and edit_file old_str="return len(items)"
	// against a file containing neither, with the model literally
	// reasoning "I don't see the file content"). The exploration-budget
	// breaker compounds it by telling the model it "has full project
	// context" when the content was already trimmed.
	pinIdx, filePinIdx := pinnedIndices(msgs)

	tailStart := len(msgs) - keepLast
	out := make([]AgentMessage, 0, keepLast+3)
	out = append(out, msgs[0])
	if pinIdx >= 1 && pinIdx < tailStart {
		out = append(out, msgs[pinIdx])
	}
	// Re-inject the pinned file content (as a user-role note so it survives
	// templates that reject orphan tool messages) when it falls outside the
	// kept tail.
	if filePinIdx >= 1 && filePinIdx < tailStart && filePinIdx != pinIdx {
		out = append(out, AgentMessage{
			Role:    "user",
			Content: "[system note]: current contents of the file you are editing (do not invent symbols or lines not shown here):\n" + msgs[filePinIdx].Content,
		})
	}
	out = append(out, msgs[tailStart:]...)
	return out
}

// ---------------------------------------------------------------------------
// HTTP handler for /v1/agent endpoint
// ---------------------------------------------------------------------------

// handleAgent is the HTTP handler for the new agent endpoint.
func handleAgent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, ErrUnsupported, "method not allowed")
		return
	}

	type historyMsg struct {
		Role    string `json:"role"` // "user" or "assistant"
		Content string `json:"content"`
	}
	var req struct {
		Message    string       `json:"message"`
		WorkingDir string       `json:"working_dir"`
		Mode       string       `json:"mode"`       // "default", "accept-edits", "yolo"
		SessionID  string       `json:"session_id"` // optional — required for /cancel
		History    []historyMsg `json:"history,omitempty"`
		// Tools the client has approved for the whole session so the proxy
		// skips the interactive prompt for them (see /v1/permission).
		SessionAllowedTools []string `json:"session_allowed_tools,omitempty"`
		// /demo split-pane flags — tags match tui/chat.go's agentRequest.
		BypassV3         bool   `json:"bypass_v3,omitempty"`          // baseline pane: disable V3 orchestration
		DisableFreshSlot bool   `json:"disable_fresh_slot,omitempty"` // keep the pre-warmed KV prefix
		SandboxSubdir    string `json:"sandbox_subdir,omitempty"`     // confine writes to this workspace subdir
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ErrInvalidInput, "invalid request body")
		return
	}

	if req.Message == "" {
		writeError(w, http.StatusBadRequest, ErrInvalidInput, "message is required")
		return
	}

	// Path translation: the TUI sends its host cwd (e.g. /home/isaac/snake)
	// as working_dir, but the proxy runs in a container where that path
	// doesn't exist — only /workspace (the bind-mount target) does. The
	// startup wrapper (atlas/runtime.py:_align_workspace) already aligns
	// the bind mount to the user's cwd, so /workspace IS the user's cwd
	// from the proxy's perspective. Use ATLAS_WORKSPACE_DIR (set in
	// docker-compose.yml) as the canonical write target. The original
	// host path is kept on HostWorkingDir for path translation (below).
	hostDir := req.WorkingDir
	if hostDir == "" {
		hostDir = "."
	}
	workingDir := envOr("ATLAS_WORKSPACE_DIR", hostDir)

	// /demo: each pane works inside its own workspace subdir. A non-empty
	// sandbox_subdir must be one allowed bare name, else HTTP 400 and no turn.
	if sub := req.SandboxSubdir; sub != "" {
		if !validSandboxSubdir(sub) {
			writeError(w, http.StatusBadRequest, ErrInvalidInput, sandboxSubdirDetail)
			return
		}
		workingDir = filepath.Join(workingDir, sub)
		if hostDir != "" && hostDir != "." {
			hostDir = filepath.Join(hostDir, sub)
		}
	}

	// Classify tier from message
	tier := classifyAgentTier(req.Message)

	// Create agent context
	ctx := NewAgentContext(workingDir, tier)
	ctx.BypassV3 = req.BypassV3
	ctx.DisableFreshSlot = req.DisableFreshSlot
	// Stash the host path so resolveAgentPath can translate absolute
	// host paths the model receives in user prompts (e.g. "fix
	// /home/isaac/snake/app.py") into the container path. Without this
	// the model copies the user's host path verbatim into read_file
	// and the open() fails because that path doesn't exist inside the
	// proxy container — only /workspace does.
	if hostDir != "" && hostDir != "." {
		ctx.HostWorkingDir = filepath.Clean(hostDir)
	}
	ctx.InferenceURL = inferenceURL
	ctx.SandboxURL = sandboxURL
	ctx.LensURL = lensURL
	ctx.V3URL = envOr("ATLAS_V3_URL", "http://localhost:8070")

	// Opt-in host execution for run_command. Per-project config
	// (.atlas/config.toml: [execution] target = "host") wins over the
	// global env var so users can flip behaviour without touching the
	// proxy environment. Either source can downgrade to "sandbox"
	// explicitly. Default stays sandbox.
	ctx.VerifyOnHost = resolveVerifyTarget(workingDir) == "host"
	ctx.TrustMode = resolveTrustMode()

	// Seed prior-turn transcript from the request body. The TUI ships
	// user/assistant text rows from its local chat history so the agent
	// can answer follow-ups; without it, every /v1/agent call starts
	// fresh. Cap defensively at 40 messages here too — the proxy's own
	// trim logic in runAgentLoop handles further overflow.
	if n := len(req.History); n > 0 {
		if n > 40 {
			req.History = req.History[n-40:]
		}
		ctx.PriorHistory = make([]AgentMessage, 0, len(req.History))
		for _, h := range req.History {
			// Only accept the two roles that make sense as conversation
			// history; anything else is skipped silently rather than
			// passed through to the LLM as an unknown role.
			if h.Role != "user" && h.Role != "assistant" {
				continue
			}
			if h.Content == "" {
				continue
			}
			ctx.PriorHistory = append(ctx.PriorHistory, AgentMessage{
				Role:    h.Role,
				Content: h.Content,
			})
		}
	}
	// Carry the upstream cancellation through so disconnects abort the loop
	// and llama-server's in-flight generation.
	//
	// Also wrap in a cancellable context so POST /cancel can
	// abort even when the TCP disconnect is buffered upstream.
	reqCtx, cancel := context.WithCancel(r.Context())
	defer cancel()
	ctx.Ctx = reqCtx
	ctx.PassID = req.SessionID
	if req.SessionID != "" {
		entry := &sessionCancel{cancel: cancel}
		activeSessions.Store(req.SessionID, entry)
		defer activeSessions.CompareAndDelete(req.SessionID, entry)
	}

	// Set permission mode
	switch req.Mode {
	case "accept-edits":
		ctx.PermissionMode = PermissionAcceptEdits
	case "yolo":
		ctx.PermissionMode = PermissionYolo
		ctx.YoloMode = true
	default:
		ctx.PermissionMode = PermissionDefault
	}

	// Seed session-approved tools so pre-approved destructive tools skip the
	// interactive prompt (the client re-sends this list each turn).
	if len(req.SessionAllowedTools) > 0 {
		ctx.AllowedTools = make(map[string]bool, len(req.SessionAllowedTools))
		for _, t := range req.SessionAllowedTools {
			ctx.AllowedTools[t] = true
		}
	}

	// Detect project (implemented in context.go)
	ctx.Project = detectProjectInfo(workingDir)

	// Set up SSE streaming
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, ErrInternal, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Flush headers immediately so the client sees the response as
	// "established" before the first LLM call returns. Without this
	// sentinel, net/http waits to flush headers until the first body
	// write, which is the first ctx.Stream() call — and that doesn't
	// happen until the agent loop emits its first event, which can
	// take 10-60s for the first LLM round-trip. Clients with a
	// reasonable ResponseHeaderTimeout (e.g. 30s) would time out
	// before getting any data.
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	// http.ResponseWriter is NOT goroutine-safe. StreamFn fires from at
	// least two concurrent goroutines during a single agent turn:
	//   - main agent loop (tool dispatch, LLM SSE forwarding)
	//   - pollPromptProgress (250ms ticker emitting llm_prompt_progress)
	// May 10 2026: adding reasoning_token doubled the event rate from
	// the SSE-decode loop, surfacing a long-latent race where
	// concurrent Write+Flush calls produced interleaved bytes that
	// corrupted the chunked-encoding framing — clients then errored
	// with "chunked line ends with bare LF" and dropped, which the
	// proxy saw as `context canceled` mid-prompt-eval. Serialize the
	// writes with a per-handler mutex so chunk framing stays
	// well-formed regardless of how fast or how concurrently events
	// fire.
	var streamMu sync.Mutex
	ctx.StreamFn = func(eventType string, data interface{}) {
		event := SSEEvent{Type: eventType, Data: data}
		eventJSON, _ := json.Marshal(event)
		streamMu.Lock()
		defer streamMu.Unlock()
		fmt.Fprintf(w, "data: %s\n\n", eventJSON)
		flusher.Flush()
	}

	// For yolo mode, auto-approve all permissions
	if ctx.YoloMode {
		ctx.PermissionFn = func(string, json.RawMessage) bool { return true }
	}

	// Run agent loop
	if err := runAgentLoop(ctx, req.Message); err != nil {
		// %q quotes the error string so user-influenced fragments
		// embedded in err.Error() can't fake additional log entries.
		log.Printf("[agent] error: %q", err.Error())
	}

	// Stash this pass's writes for deferred /feedback labeling (lens training
	// data). Keyed by session id; a later thumbs / per-file verdict turns them
	// into weighted samples. No-op when the pass wrote nothing or has no id.
	stashPendingPass(req.SessionID, modelName, ctx.PassWrites)

	// Send final done event
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// ---------------------------------------------------------------------------
// /cancel — abort an in-flight /v1/agent turn by session_id
// ---------------------------------------------------------------------------

// handleCancel POSTs cancel an in-flight agent turn. Body:
//
//	{"session_id": "..."}
//
// Returns 200 with `{"cancelled": true}` if the session was found and
// cancelled, 404 with `{"cancelled": false}` if no such session is
// active. Idempotent: a second cancel for the same session returns 404.
//
// On success, the agent loop exits via context.Canceled, the SSE
// stream emits its trailing `[DONE]`, and the client connection
// closes cleanly. The TUI surfaces a "turn cancelled" system message.
func handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, ErrUnsupported, "method not allowed")
		return
	}
	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ErrInvalidInput, "invalid request body")
		return
	}
	if req.SessionID == "" {
		writeError(w, http.StatusBadRequest, ErrInvalidInput, "session_id required")
		return
	}
	v, ok := activeSessions.LoadAndDelete(req.SessionID)
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]bool{"cancelled": false})
		return
	}
	entry, ok := v.(*sessionCancel)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "bad session entry"})
		return
	}
	entry.cancel()
	log.Printf("[agent] cancelled session %q via /cancel", req.SessionID)
	_ = json.NewEncoder(w).Encode(map[string]bool{"cancelled": true})
}

// classifyParseFailure walks the raw response shape once and returns
// both a short stable category for the docker log (so `docker logs
// atlas-proxy` reads "what kind of broken" at a glance) and a targeted
// feedback message for the model. The model can't see why parsing
// failed, so a generic "respond in JSON" message lets it loop forever
// on the same pattern:
//
//   - starts with `{"type":"tool_call",...,"name":"<edit_file|write_file>",...}` and looks
//     truncated → it tried a too-big edit; tell it to shrink old_str/new_str
//   - non-JSON prose → standard "respond JSON only" reminder
//   - empty or whitespace → continuation nudge
//
// Categories:
//
//	empty           — response was whitespace
//	prose           — response is non-JSON text (model narration leaking)
//	truncated_tool  — JSON tool_call envelope cut off mid-args (max_tokens)
//	html_entities   — tool_call contains &lt; / &gt; / &amp; in string args
//	malformed_tool  — tool_call envelope present but JSON malformed
//	non_json        — response begins with text other than '{'
//
// The bug this addresses: in May 2026 a user fix-intent prompt put the
// model in a loop emitting the same 1100-char edit_file with all 5
// flask routes embedded in old_str. Llama-server's response cap cut it
// mid-string, parse failed, we didn't tell the model why, it retried
// identically. classifyParseFailure breaks the cycle by naming the
// failure mode.
func classifyParseFailure(raw string) (category, feedback string) {
	stripped := strings.TrimSpace(raw)
	if stripped == "" {
		return "empty", "Your response was empty. Respond with ONLY a single JSON object — {\"type\":\"tool_call\",...} or {\"type\":\"text\",\"content\":\"...\"} or {\"type\":\"done\",\"summary\":\"...\"}."
	}
	// HTML-entity encoding detection — some models encode <, >, &
	// inside tool-call string args (`&lt;!DOCTYPE...&gt;`) instead of
	// emitting them literally. JSON parses fine if the whole envelope
	// arrives, but those entities then appear verbatim in old_str and
	// don't match the actual file content. Catch and redirect — works
	// regardless of whether the response has a prose prefix (May 8
	// dashboard.html session: model emitted "Now I can see..." then a
	// JSON tool_call with HTML-entity-encoded old_str; the
	// looksLikeToolCall check below missed it because the response
	// didn't start with `{`, leaving the targeted corrective unfired).
	// Checked FIRST — the entity bug is a stronger signal than "this
	// is narration," so it wins over prose/non_json/malformed.
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
		hasEditOrWrite := strings.Contains(stripped, `"edit_file"`) ||
			strings.Contains(stripped, `"write_file"`)
		if hasEditOrWrite {
			// GH #39: when truncation hits on a whole-file replacement,
			// structural_edit is the right tool — it takes a structural
			// selector (function:NAME, <tag>) instead of literal
			// old_str, so the JSON envelope stays small. Steer the
			// model toward it explicitly.
			structuralHint := ""
			if strings.Contains(stripped, `&lt;`) || strings.Contains(stripped, `&gt;`) ||
				strings.Contains(stripped, `<body>`) || strings.Contains(stripped, `<head>`) ||
				strings.Contains(stripped, `def `) || strings.Contains(stripped, `class `) {
				structuralHint = " For whole-function or whole-element replacements, use `structural_edit` instead — it takes a selector (e.g. `function:dashboard`, `<body>`) and drops `old_str` entirely, so it doesn't truncate."
			}
			return "truncated_tool", "Your last tool call was TRUNCATED — the response hit the token cap mid-args. The fix is to shrink old_str/new_str: edit ONE function or block per call, not the whole file. If you need to change multiple routes/functions, do them in separate edit_file calls (one per turn). Common offenders: pasting all of app.py into old_str, embedding 5+ @app.route handlers in a single replacement." + structuralHint + " Respond now with a smaller edit_file or a structural_edit call."
		}
		return "truncated_tool", "Your tool call was truncated mid-args. Make a smaller call — keep `content`, `old_str`, and `new_str` short (under ~30 lines). Respond now with the corrected, smaller call."
	}
	return "malformed_tool", "Your tool_call JSON was malformed. Re-emit it as a single valid JSON object: {\"type\":\"tool_call\",\"name\":\"<tool>\",\"args\":{...}}. No prose, no markdown fences, no trailing commas."
}

// extractModelResponse extracts a ModelResponse from the LLM output,
// handling cases where the model adds text before/after the JSON or
// where the JSON is truncated.
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

	// Find the first '{' and try to parse from there
	start := strings.Index(raw, "{")
	if start < 0 {
		return resp, fmt.Errorf("no JSON object found in response")
	}

	// Find matching closing brace by counting nesting
	depth := 0
	inString := false
	escaped := false
	end := -1
	for i := start; i < len(raw); i++ {
		c := raw[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && inString {
			escaped = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if c == '{' {
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 {
				end = i + 1
				break
			}
		}
	}

	var balancedErr error
	if end > start {
		jsonStr := raw[start:end]
		balancedErr = json.Unmarshal([]byte(jsonStr), &resp)
		if balancedErr == nil {
			liftMissingArgs(&resp, jsonStr)
			return resp, nil
		}
	}

	// JSON was truncated (max_tokens hit mid-content) or otherwise
	// malformed — try a generalized tool_call recovery for write_file,
	// edit_file, and structural_edit. Identical shape (path + payload field),
	// just different field names. If recovery succeeds, return it; if
	// not, fall through to the diagnostic error below.
	if recovered, ok := recoverTruncatedToolCall(raw[start:]); ok {
		return recovered, nil
	}

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
	if resp.Type != "" && resp.Type != "tool_call" && resp.Type != "text" && resp.Type != "done" {
		if getTool(resp.Type) != nil {
			resp.Name = resp.Type
			resp.Type = "tool_call"
		}
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

// recoverTruncatedToolCall is the generalized counterpart to
// recoverTruncatedWriteFile. May 9 2026: under BiasBusters mitigations
// the model now reaches for structural_edit and edit_file too, and either can
// land malformed JSON (truncated content, stray escape) the same way
// write_file used to. Old code only recovered write_file; everything
// else just died with "could not parse JSON". Now we sniff the tool
// name from the partial bytes and dispatch to a tool-specific recovery
// when one exists. Returns (response, true) on successful recovery,
// (zero, false) when no recovery is available so the caller falls
// through to the diagnostic error.
func recoverTruncatedToolCall(partial string) (ModelResponse, bool) {
	switch {
	case strings.Contains(partial, `"name":"write_file"`) || strings.Contains(partial, `"name": "write_file"`):
		if r, err := recoverTruncatedWriteFile(partial); err == nil {
			return r, true
		}
	case strings.Contains(partial, `"name":"structural_edit"`) || strings.Contains(partial, `"name": "structural_edit"`):
		if r, err := recoverTruncatedStructuralEdit(partial); err == nil {
			return r, true
		}
	case strings.Contains(partial, `"name":"edit_file"`) || strings.Contains(partial, `"name": "edit_file"`):
		if r, err := recoverTruncatedEditFile(partial); err == nil {
			return r, true
		}
	}
	return ModelResponse{}, false
}

// looksDegenerate reports whether a recovered field value is the model's
// own degenerate output rather than real content.
//
// Truncation recovery exists for one case: a well-formed tool call whose
// JSON was cut off by max_tokens. It reconstructs args from whatever
// extractStringField can read, which is a purely structural operation — a
// run of repeated newlines parses exactly as well as a real function body.
// Without this check, a generation that degenerated into a repeating tail
// (the same condition isLoopingTail cuts the stream on) is "successfully
// recovered" into an edit_file or write_file call and executed against the
// user's file. The stream cut prevents the tokens from being generated; it
// does nothing about the bytes already buffered when recovery runs.
//
// Two shapes, both observed: a value that is almost entirely whitespace,
// and one whose tail repeats. Short values are exempt — a legitimately
// small new_str has no room to look degenerate, and the length floor keeps
// ordinary edits out of the check entirely.
func looksDegenerate(s string) bool {
	const minJudgeable = 64
	if len(s) < minJudgeable {
		return false
	}
	var ws int
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			ws++
		}
	}
	if float64(ws)/float64(len(s)) > 0.9 {
		return true
	}
	// Repetition alone is not degeneracy — real files repeat boilerplate,
	// and isLoopingTail's "tail occurs 3+ times" fires on a long file with
	// a handful of similar lines. Rejecting those would break recovery for
	// exactly the truncated writes it exists to salvage. Require instead
	// that the repeated tail account for most of the value, which
	// separates a repeating generation from a file that happens to repeat.
	const probe = 48
	if len(s) < probe*3 {
		return false
	}
	tail := s[len(s)-probe:]
	if strings.TrimSpace(tail) == "" {
		return false
	}
	occurrences := strings.Count(s, tail)
	return float64(occurrences*probe)/float64(len(s)) > 0.5
}

// extractStringField pulls a JSON-string field value out of a partial
// (possibly truncated) tool-call payload. Returns the unescaped value
// and true on success. The end is determined by the next unescaped `"`
// — for the trailing field of a truncated payload, the value runs to
// end-of-input and is closed by the caller.
func extractStringField(partial, field string) (string, bool) {
	for _, marker := range []string{`"` + field + `":"`, `"` + field + `": "`} {
		idx := strings.Index(partial, marker)
		if idx < 0 {
			continue
		}
		valueStart := idx + len(marker)
		// Walk until unescaped closing quote.
		escaped := false
		for i := valueStart; i < len(partial); i++ {
			c := partial[i]
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				raw := partial[valueStart:i]
				var unescaped string
				if err := json.Unmarshal([]byte(`"`+raw+`"`), &unescaped); err == nil {
					return unescaped, true
				}
				return raw, true
			}
		}
		// Hit end-of-input without finding closing quote — payload was
		// truncated mid-string. Return what we have, best-effort
		// unescaping; trailing backslash is dropped to avoid invalid
		// escape sequences.
		raw := strings.TrimRight(partial[valueStart:], "\\")
		var unescaped string
		if err := json.Unmarshal([]byte(`"`+raw+`"`), &unescaped); err == nil {
			return unescaped, true
		}
		// Manual fallback for the common escapes when Unmarshal rejected
		// a partial string (rarely happens but cheap insurance).
		manual := strings.ReplaceAll(raw, `\n`, "\n")
		manual = strings.ReplaceAll(manual, `\t`, "\t")
		manual = strings.ReplaceAll(manual, `\"`, `"`)
		manual = strings.ReplaceAll(manual, `\\`, `\`)
		return manual, true
	}
	return "", false
}

// recoverTruncatedStructuralEdit recovers a structural_edit tool call whose JSON
// envelope didn't survive the parser. structural_edit's args are
// {path, selector, content} — same shape as write_file but with an
// additional selector field that's always short (function:NAME,
// class:NAME, <tag>) so it lands intact even on truncation. The
// content is the long field that gets cut.
func recoverTruncatedStructuralEdit(partial string) (ModelResponse, error) {
	path, ok := extractStringField(partial, "path")
	if !ok || path == "" {
		return ModelResponse{}, fmt.Errorf("structural_edit recovery: missing path")
	}
	selector, ok := extractStringField(partial, "selector")
	if !ok || selector == "" {
		return ModelResponse{}, fmt.Errorf("structural_edit recovery: missing selector")
	}
	content, ok := extractStringField(partial, "content")
	if !ok {
		return ModelResponse{}, fmt.Errorf("structural_edit recovery: missing content")
	}
	if looksDegenerate(content) {
		return ModelResponse{}, fmt.Errorf("structural_edit recovery: content is degenerate output, not a real edit")
	}
	args, _ := json.Marshal(StructuralEditInput{Path: path, Selector: selector, Content: content})
	log.Printf("[agent] recovered truncated structural_edit: path=%s selector=%q content=%d chars",
		path, selector, len(content))
	return ModelResponse{Type: "tool_call", Name: "structural_edit", Args: args}, nil
}

// recoverTruncatedEditFile recovers an edit_file tool call. Args are
// {path, old_str, new_str, replace_all?}. Either old_str or new_str
// can be the truncation point; recover whichever one terminated
// cleanly and warn-log when one didn't, so the agent loop sees the
// failure category instead of a generic parse error.
func recoverTruncatedEditFile(partial string) (ModelResponse, error) {
	path, ok := extractStringField(partial, "path")
	if !ok || path == "" {
		return ModelResponse{}, fmt.Errorf("edit_file recovery: missing path")
	}
	oldStr, oldOK := extractStringField(partial, "old_str")
	newStr, newOK := extractStringField(partial, "new_str")
	if !oldOK && !newOK {
		return ModelResponse{}, fmt.Errorf("edit_file recovery: missing both old_str and new_str")
	}
	if looksDegenerate(oldStr) || looksDegenerate(newStr) {
		return ModelResponse{}, fmt.Errorf("edit_file recovery: old_str/new_str is degenerate output, not a real edit")
	}
	replaceAll := strings.Contains(partial, `"replace_all":true`) ||
		strings.Contains(partial, `"replace_all": true`)
	args, _ := json.Marshal(EditFileInput{
		Path:       path,
		OldStr:     oldStr,
		NewStr:     newStr,
		ReplaceAll: replaceAll,
	})
	log.Printf("[agent] recovered truncated edit_file: path=%s old_str=%dch new_str=%dch", path, len(oldStr), len(newStr))
	return ModelResponse{Type: "tool_call", Name: "edit_file", Args: args}, nil
}

// recoverTruncatedWriteFile attempts to recover a write_file tool call
// where the content was truncated by max_tokens.
func recoverTruncatedWriteFile(partial string) (ModelResponse, error) {
	// The pattern is: {"type":"tool_call","name":"write_file","args":{"path":"...","content":"...
	// We need to close the content string and the JSON objects

	// Find the "content":" part
	idx := strings.Index(partial, `"content":"`)
	if idx < 0 {
		idx = strings.Index(partial, `"content": "`)
	}
	if idx < 0 {
		return ModelResponse{}, fmt.Errorf("cannot find content field in truncated write_file")
	}

	// Find the "path" value
	pathIdx := strings.Index(partial, `"path":"`)
	pathEnd := -1
	path := ""
	if pathIdx >= 0 {
		pathStart := pathIdx + len(`"path":"`)
		pathEnd = strings.Index(partial[pathStart:], `"`)
		if pathEnd >= 0 {
			path = partial[pathStart : pathStart+pathEnd]
		}
	}

	// Extract content: everything after "content":" until the end
	contentStart := idx + len(`"content":"`)
	if strings.Contains(partial[idx:idx+15], `: "`) {
		contentStart = idx + len(`"content": "`)
	}
	content := partial[contentStart:]

	// Unescape the content string (it's JSON-escaped)
	// Remove trailing incomplete escape sequences
	content = strings.TrimRight(content, "\\")
	// Close the string
	content = strings.TrimSuffix(content, `"`)
	content = strings.TrimSuffix(content, `"}`)
	content = strings.TrimSuffix(content, `"}}`)

	// Unescape JSON string escapes
	var unescaped string
	err := json.Unmarshal([]byte(`"`+content+`"`), &unescaped)
	if err != nil {
		// Fallback: manual unescape of common sequences
		unescaped = strings.ReplaceAll(content, `\n`, "\n")
		unescaped = strings.ReplaceAll(unescaped, `\t`, "\t")
		unescaped = strings.ReplaceAll(unescaped, `\"`, "\"")
		unescaped = strings.ReplaceAll(unescaped, `\\`, "\\")
	}

	if path == "" {
		return ModelResponse{}, fmt.Errorf("could not extract path from truncated write_file")
	}
	if looksDegenerate(unescaped) {
		return ModelResponse{}, fmt.Errorf("write_file recovery: content is degenerate output, not a real file")
	}

	// Build the args JSON
	args, _ := json.Marshal(WriteFileInput{Path: path, Content: unescaped})

	log.Printf("[agent] recovered truncated write_file: path=%s content=%d chars", path, len(unescaped))

	return ModelResponse{
		Type: "tool_call",
		Name: "write_file",
		Args: args,
	}, nil
}

// classifyAgentTier decides whether a request is conversational.
//
// The message tier has exactly two behaviours, despite the four-value Tier
// type. TierMaxTurns caps T0 at 5 turns and leaves T1/T2/T3 uncapped
// alike; shouldGeneratePlan tests only Tier0Conversational; and the tier
// travels to v3-service where it is read into a log line and never branched
// on. V3 activation is driven by classifyFileTier, which scores the file
// being edited — a different function that does use T1/T2/T3 meaningfully.
// So the only question here is conversational or not, and the returned
// non-T0 value is Tier2Medium because that is what every consumer treats
// every non-T0 value as.
//
// The costs are asymmetric, which sets the direction of the default.
// Misreading chat as a task wastes one planner call on a message the model
// answers and closes in a single turn. Misreading a task as chat caps it at
// 5 turns and skips planning, and a capped task fails: "the snake is still
// moving way too fast, please slow it down significantly" was classified
// conversational during 2026-07-21 dogfooding and returned a zero-tool-call
// non-answer instead of an edit.
//
// So T0 requires positive evidence, and the absence of a recognized task
// word is not evidence. Describing desired software behaviour is open
// vocabulary with no closed list to match against, while greetings are
// short and questions are a closed grammatical class. Both of those are
// things a message can be shown to BE, rather than shown not to be.
func classifyAgentTier(message string) Tier {
	trimmed := strings.TrimSpace(message)

	// Task language wins outright, at any length and in any shape. "can
	// you fix the login bug?" is a question and a task; the task reading
	// is the one whose failure mode is expensive.
	if isActionIntentMessage(trimmed) || isFixIntentMessage(trimmed) {
		return Tier2Medium
	}

	// An explicit "explain this, do not edit anything" is conversational by
	// definition, whether or not it is phrased as a question. Without this,
	// "Explain how the retry logic works, without editing anything." carries
	// no question mark and no question-word opener, so it fell through to a
	// work tier and got the write pipeline.
	if isExplainOnlyMessage(strings.ToLower(trimmed)) {
		return Tier0Conversational
	}

	// Greeting or acknowledgement. The floor matches shouldGeneratePlan's
	// own, so the two agree on what is too short to plan for.
	if len(trimmed) < 12 {
		return Tier0Conversational
	}

	if isQuestionMessage(trimmed) {
		return Tier0Conversational
	}

	return Tier2Medium
}

// questionStarters is the set of words an English interrogative can open
// with. Unlike task vocabulary, which is unbounded, this is a closed
// grammatical class, which is what makes matching against it sound where
// matching against a list of task verbs would not be.
var questionStarters = []string{
	"why", "what", "when", "where", "who", "which", "how",
	"is ", "are ", "does ", "do ", "did ", "can ", "could ",
	"would ", "should ", "will ", "won't", "isn't", "aren't",
}

// isQuestionMessage reports whether a message is shaped as a question:
// a trailing "?", which catches any phrasing, or one of the interrogative
// openers above for questions written without one.
func isQuestionMessage(message string) bool {
	trimmed := strings.TrimSpace(message)
	// A question mark ANYWHERE, not only at the end. People ask and then
	// qualify — "what does find_duplicates do, and what is its complexity?
	// Just explain." ends in a period, so a suffix-only check read it as
	// not-a-question and it was handed the full write pipeline. Safe to
	// widen: classifyAgentTier checks action and fix intent first, so
	// "fix the bug in foo.py? or bar.py?" still classifies as work.
	if strings.Contains(trimmed, "?") {
		return true
	}
	lower := strings.ToLower(trimmed)
	for _, w := range questionStarters {
		if strings.HasPrefix(lower, w) {
			return true
		}
		// Or opening a clause: "In orders.py, what does X do" carries no
		// question mark at all but is plainly a question.
		if strings.Contains(lower, ", "+w) || strings.Contains(lower, ". "+w) {
			return true
		}
	}
	return false
}

// Toolchain describes one language ecosystem detected in the project.
// The fields are surfaced into the system prompt so the model knows
// which runner to invoke and how to install deps if needed.
//
// Detection is manifest-driven: presence of pyproject.toml means
// Python, package.json means Node, Cargo.toml means Rust, etc. A
// polyglot project (React frontend + Django backend + deploy scripts)
// returns multiple Toolchains so the model can pick the right one
// per file edit.
type Toolchain struct {
	Name           string   // canonical key: "python", "node", "rust", "go", "ruby", "java-maven", "java-gradle", "php", "dotnet", "dart"
	Manifests      []string // manifest files found relative to workingDir (e.g. ["pyproject.toml", "requirements.txt"])
	Runner         string   // command to run the project's main entry (e.g. "/workspace/venv/bin/python", "node", "cargo run", "go run .")
	PackageManager string   // detected pkg manager when ambiguous (npm vs pnpm vs yarn vs bun for Node)
	InstallCommand string   // command to install deps from lockfile (e.g. "npm ci", "pip install -r requirements.txt")
	TestCommand    string   // best-guess test runner ("pytest", "npm test", "cargo test", ...)
}

// detectProjectToolchains scans workingDir for language manifests and
// returns one Toolchain per detected ecosystem. Polyglot projects
// (e.g. React + Django) produce multiple entries. Empty slice means
// no recognized manifest was found at the root.
//
// We deliberately only look ONE level deep at the root — most
// monorepos have manifests in subdirs (apps/web/package.json,
// services/api/pyproject.toml) but probing deeper here would be
// expensive and noisy. The model can still discover deep manifests
// via list_directory / read_file when it needs to.
func detectProjectToolchains(workingDir string) []Toolchain {
	if workingDir == "" {
		return nil
	}
	var out []Toolchain

	// Python — venv-aware so the runner points at the project's
	// pinned interpreter when one exists.
	pyManifests := pickExisting(workingDir, "pyproject.toml", "requirements.txt", "setup.py", "Pipfile", "poetry.lock")
	if len(pyManifests) > 0 || detectProjectVenvPython(workingDir) != "" {
		runner := detectProjectVenvPython(workingDir)
		if runner == "" {
			runner = "python"
		}
		install := "pip install -r requirements.txt"
		if hasFile(workingDir, "poetry.lock") {
			install = "poetry install"
		} else if hasFile(workingDir, "Pipfile.lock") {
			install = "pipenv install"
		} else if hasFile(workingDir, "pyproject.toml") && !hasFile(workingDir, "requirements.txt") {
			install = "pip install -e ."
		}
		out = append(out, Toolchain{
			Name: "python", Manifests: pyManifests,
			Runner: runner, InstallCommand: install,
			TestCommand: "pytest",
		})
	}

	// Node / TypeScript — pkg manager picked from lockfile.
	if hasFile(workingDir, "package.json") {
		pm, install := "npm", "npm install"
		switch {
		case hasFile(workingDir, "pnpm-lock.yaml"):
			pm, install = "pnpm", "pnpm install --frozen-lockfile"
		case hasFile(workingDir, "yarn.lock"):
			pm, install = "yarn", "yarn install --frozen-lockfile"
		case hasFile(workingDir, "bun.lockb"):
			pm, install = "bun", "bun install --frozen-lockfile"
		case hasFile(workingDir, "package-lock.json"):
			pm, install = "npm", "npm ci"
		}
		runner := "node"
		if hasFile(workingDir, "tsconfig.json") {
			runner = "tsx" // ts/jsx-aware launcher; falls back to node for plain .js
		}
		out = append(out, Toolchain{
			Name: "node", Manifests: pickExisting(workingDir, "package.json", "tsconfig.json"),
			Runner: runner, PackageManager: pm, InstallCommand: install,
			TestCommand: pm + " test",
		})
	}

	// Rust
	if hasFile(workingDir, "Cargo.toml") {
		out = append(out, Toolchain{
			Name: "rust", Manifests: pickExisting(workingDir, "Cargo.toml", "Cargo.lock"),
			Runner: "cargo run", InstallCommand: "cargo fetch",
			TestCommand: "cargo test",
		})
	}

	// Go
	if hasFile(workingDir, "go.mod") {
		out = append(out, Toolchain{
			Name: "go", Manifests: pickExisting(workingDir, "go.mod", "go.sum"),
			Runner: "go run .", InstallCommand: "go mod download",
			TestCommand: "go test ./...",
		})
	}

	// Ruby
	if hasFile(workingDir, "Gemfile") {
		out = append(out, Toolchain{
			Name: "ruby", Manifests: pickExisting(workingDir, "Gemfile", "Gemfile.lock"),
			Runner: "bundle exec ruby", InstallCommand: "bundle install",
			TestCommand: "bundle exec rspec",
		})
	}

	// Java — Maven
	if hasFile(workingDir, "pom.xml") {
		out = append(out, Toolchain{
			Name: "java-maven", Manifests: []string{"pom.xml"},
			Runner: "mvn exec:java", InstallCommand: "mvn install -DskipTests",
			TestCommand: "mvn test",
		})
	}

	// Java/Kotlin — Gradle (prefer wrapper if present)
	if hasFile(workingDir, "build.gradle") || hasFile(workingDir, "build.gradle.kts") {
		runner := "gradle run"
		install := "gradle build -x test"
		test := "gradle test"
		if hasFile(workingDir, "gradlew") {
			runner = "./gradlew run"
			install = "./gradlew build -x test"
			test = "./gradlew test"
		}
		out = append(out, Toolchain{
			Name: "java-gradle", Manifests: pickExisting(workingDir, "build.gradle", "build.gradle.kts", "settings.gradle", "gradlew"),
			Runner: runner, InstallCommand: install, TestCommand: test,
		})
	}

	// PHP / Composer
	if hasFile(workingDir, "composer.json") {
		out = append(out, Toolchain{
			Name: "php", Manifests: pickExisting(workingDir, "composer.json", "composer.lock"),
			Runner: "php", InstallCommand: "composer install",
			TestCommand: "vendor/bin/phpunit",
		})
	}

	// .NET — pick the first project file we find
	if csproj := firstMatchingGlob(workingDir, "*.csproj", "*.fsproj", "*.sln"); csproj != "" {
		out = append(out, Toolchain{
			Name: "dotnet", Manifests: []string{csproj},
			Runner: "dotnet run", InstallCommand: "dotnet restore",
			TestCommand: "dotnet test",
		})
	}

	// Dart / Flutter
	if hasFile(workingDir, "pubspec.yaml") {
		runner, install := "dart run", "dart pub get"
		if hasFile(workingDir, ".flutter-plugins") || hasFile(workingDir, "flutter.yaml") {
			runner, install = "flutter run", "flutter pub get"
		}
		out = append(out, Toolchain{
			Name: "dart", Manifests: pickExisting(workingDir, "pubspec.yaml", "pubspec.lock"),
			Runner: runner, InstallCommand: install,
			TestCommand: "dart test",
		})
	}

	return out
}

// probeToolchainReady returns a short status string for a Toolchain
// that's safe to run from buildSystemPrompt — meaning: it MUST be
// purely filesystem-based (no shelling out, no network). The model
// uses this to decide whether to install deps or skip straight to
// verification.
//
// We can't actually invoke `python -c "import flask"` here without
// running a subprocess in the sandbox, which is too expensive for
// every system-prompt build. Instead we look for filesystem evidence
// that deps are installed: venv with site-packages populated,
// node_modules present, target/debug/ for Rust, vendor/ for Ruby/Go,
// etc. False positives are fine ("looks installed but isn't" — the
// model will discover that on first verify and install). False
// negatives are bad — they push the model toward unnecessary
// reinstalls. Bias toward "ready" when the evidence is ambiguous.
func probeToolchainReady(workingDir string, tc Toolchain) string {
	switch tc.Name {
	case "python":
		for _, vd := range []string{"venv", ".venv", "env", ".env-py"} {
			sp := filepath.Join(workingDir, vd, "lib")
			if entries, err := os.ReadDir(sp); err == nil {
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), "python") && e.IsDir() {
						if hasUserPackages(filepath.Join(sp, e.Name(), "site-packages")) {
							return "ready"
						}
					}
				}
			}
		}
		if hasFile(workingDir, "requirements.txt") || hasFile(workingDir, "pyproject.toml") {
			return "needs install"
		}
		return "no manifest"

	case "node":
		if entries, err := os.ReadDir(filepath.Join(workingDir, "node_modules")); err == nil && len(entries) > 0 {
			return "ready"
		}
		return "needs install"

	case "rust":
		if info, err := os.Stat(filepath.Join(workingDir, "target")); err == nil && info.IsDir() {
			return "warm"
		}
		return "cold"

	case "go":
		if info, err := os.Stat(filepath.Join(workingDir, "vendor")); err == nil && info.IsDir() {
			return "vendored"
		}
		if hasFile(workingDir, "go.sum") {
			return "ready"
		}
		return "needs `go mod tidy`"

	case "ruby":
		if info, err := os.Stat(filepath.Join(workingDir, "vendor", "bundle")); err == nil && info.IsDir() {
			return "ready"
		}
		return "needs install"

	case "java-maven", "java-gradle":
		dir := "target"
		if tc.Name == "java-gradle" {
			dir = "build"
		}
		if info, err := os.Stat(filepath.Join(workingDir, dir)); err == nil && info.IsDir() {
			return "warm"
		}
		return "cold"

	case "php":
		if info, err := os.Stat(filepath.Join(workingDir, "vendor")); err == nil && info.IsDir() {
			return "ready"
		}
		return "needs install"

	case "dotnet":
		if info, err := os.Stat(filepath.Join(workingDir, "bin")); err == nil && info.IsDir() {
			return "warm"
		}
		return "cold"

	case "dart":
		if info, err := os.Stat(filepath.Join(workingDir, ".dart_tool")); err == nil && info.IsDir() {
			return "ready"
		}
		return "needs install"
	}
	return ""
}

// displayRelativeRunner converts an absolute runner path to its
// project-relative form when it lives under workingDir. Compresses
// `/workspace/venv/bin/python` to `venv/bin/python` in prompt output —
// matches the existing "use relative paths" rule and stops the model
// confusing itself into emitting `workspace/app.py`.
func displayRelativeRunner(runner, workingDir string) string {
	if !filepath.IsAbs(runner) {
		return runner
	}
	if rel, err := filepath.Rel(workingDir, runner); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return runner
}

// hasUserPackages returns true when site-packages contains anything
// beyond pip/setuptools/wheel — i.e. the user has installed real
// project deps. Empty / pip-only venvs return false.
func hasUserPackages(sitePackages string) bool {
	entries, err := os.ReadDir(sitePackages)
	if err != nil {
		return false
	}
	skip := map[string]bool{
		"pip": true, "setuptools": true, "wheel": true,
		"pkg_resources": true, "_distutils_hack": true,
		"__pycache__": true,
	}
	for _, e := range entries {
		name := e.Name()
		// Strip dist-info / egg-info suffixes for the skip check.
		if i := strings.Index(name, "-"); i > 0 {
			name = name[:i]
		}
		if strings.HasSuffix(e.Name(), ".dist-info") || strings.HasSuffix(e.Name(), ".egg-info") {
			continue
		}
		if !skip[name] && !strings.HasPrefix(name, "_") {
			return true
		}
	}
	return false
}

// hasFile returns true when workingDir/name exists as a file.
func hasFile(workingDir, name string) bool {
	info, err := os.Stat(filepath.Join(workingDir, name))
	return err == nil && !info.IsDir()
}

// pickExisting returns the subset of names that exist as files in workingDir.
func pickExisting(workingDir string, names ...string) []string {
	var out []string
	for _, n := range names {
		if hasFile(workingDir, n) {
			out = append(out, n)
		}
	}
	return out
}

// firstMatchingGlob returns the first filename matching any of the
// glob patterns at the workingDir root, or "" if none match.
func firstMatchingGlob(workingDir string, patterns ...string) string {
	for _, p := range patterns {
		matches, _ := filepath.Glob(filepath.Join(workingDir, p))
		if len(matches) > 0 {
			return filepath.Base(matches[0])
		}
	}
	return ""
}

// detectProjectVenvPython returns the container-side path to the
// project's venv python (e.g. "/workspace/venv/bin/python") if the
// working directory has a recognisable Python virtual environment.
// Returns "" when no venv is found.
//
// The agent's working_dir is the container-internal /workspace, so
// we resolve against that. Common venv directory names: venv, .venv,
// env, .env-py — we probe in priority order and stop at the first hit.
// Inside each, look for bin/python, bin/python3, or Scripts/python.exe
// (Windows-emitted venvs occasionally end up bind-mounted on Linux).
//
// Caller passes workingDir from ctx.WorkingDir; the returned path is
// what the model should literally invoke via run_command — e.g.
// "/workspace/venv/bin/python app.py" — and what gets surfaced in the
// system prompt's venv hint.
func detectProjectVenvPython(workingDir string) string {
	if workingDir == "" {
		return ""
	}
	venvDirs := []string{"venv", ".venv", "env", ".env-py"}
	pythonRels := []string{"bin/python", "bin/python3", "Scripts/python.exe"}
	for _, vd := range venvDirs {
		for _, py := range pythonRels {
			abs := filepath.Join(workingDir, vd, py)
			if info, err := os.Stat(abs); err == nil && !info.IsDir() {
				// Return container-relative path (workingDir is already
				// the container-side /workspace), so caller can paste
				// it into a run_command argument unchanged.
				return abs
			}
		}
	}
	return ""
}

// samplePlanContext walks ctx.WorkingDir and reads a handful of files
// the planner is most likely to need: source files, templates,
// manifests. Limited to maxFiles per call, each truncated to maxBytes.
//
// The planner runs *before* any tool calls have happened in the loop,
// so ctx.FilesRead is empty — without this, plans for "fix the flask
// app" would have no signal about what's in app.py and would generate
// generic 5-step recipes. We pay one fs walk + a few small reads up
// front; the budget is small (~5 files × 2KB) and the planning quality
// jump is large.
func samplePlanContext(workingDir string, maxFiles, maxBytes int) map[string]string {
	if workingDir == "" {
		return nil
	}
	out := map[string]string{}
	// Files we always inline if present — most projects have at least
	// one of these and they describe shape (deps, entry point).
	priority := []string{
		"app.py", "main.py", "manage.py", "wsgi.py",
		"index.html", "templates/index.html", "templates/base.html",
		"package.json", "tsconfig.json", "vite.config.ts", "vite.config.js",
		"go.mod", "main.go",
		"Cargo.toml", "src/main.rs", "src/lib.rs",
		"requirements.txt", "pyproject.toml", "setup.py",
		"README.md",
	}
	for _, rel := range priority {
		if len(out) >= maxFiles {
			break
		}
		full := filepath.Join(workingDir, rel)
		info, err := os.Stat(full)
		if err != nil || info.IsDir() {
			continue
		}
		// Skip oversized files — the planner doesn't need a 50KB README.
		if info.Size() > int64(maxBytes)*4 {
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		s := string(data)
		if len(s) > maxBytes {
			s = s[:maxBytes] + "\n... (truncated)"
		}
		out[rel] = s
	}
	// If priority files yielded nothing at the workspace root, the
	// project may live one level down — common when the user's
	// `atlas tui` cwd was the parent dir (e.g. /workspace) but the
	// flask app is at /workspace/snake/. Walk one level looking for
	// the SAME priority filenames inside subdirectories. Without
	// this, the May 2026 user-session planner saw zero context and
	// the agent wasted 3 turns finding `snake/app.py`.
	if len(out) == 0 {
		entries, err := os.ReadDir(workingDir)
		if err != nil {
			return nil
		}
		// First pass: peek into subdirectories for priority files.
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			// Skip caches, vendors, dot-dirs — these aren't projects.
			if strings.HasPrefix(name, ".") || name == "node_modules" ||
				name == "venv" || name == "__pycache__" ||
				name == "dist" || name == "build" || name == "target" ||
				name == "vendor" {
				continue
			}
			for _, rel := range priority {
				if len(out) >= maxFiles {
					break
				}
				full := filepath.Join(workingDir, name, rel)
				info, err := os.Stat(full)
				if err != nil || info.IsDir() {
					continue
				}
				if info.Size() > int64(maxBytes)*4 {
					continue
				}
				data, err := os.ReadFile(full)
				if err != nil {
					continue
				}
				s := string(data)
				if len(s) > maxBytes {
					s = s[:maxBytes] + "\n... (truncated)"
				}
				// Key uses subdir/filename so the planner sees the
				// path the agent will need to use in tool calls.
				out[filepath.Join(name, rel)] = s
			}
			if len(out) >= maxFiles {
				break
			}
		}
		// Second pass: shallow walk of the workspace root for any
		// source-looking files (uncommon repo layout, no priority
		// hits anywhere).
		if len(out) == 0 {
			for _, e := range entries {
				if len(out) >= maxFiles {
					break
				}
				if e.IsDir() {
					continue
				}
				name := e.Name()
				ext := strings.ToLower(filepath.Ext(name))
				switch ext {
				case ".py", ".go", ".js", ".ts", ".tsx", ".jsx",
					".html", ".rs", ".rb", ".java", ".kt", ".swift":
					// pass
				default:
					continue
				}
				info, err := e.Info()
				if err != nil || info.Size() > int64(maxBytes)*4 {
					continue
				}
				data, err := os.ReadFile(filepath.Join(workingDir, name))
				if err != nil {
					continue
				}
				s := string(data)
				if len(s) > maxBytes {
					s = s[:maxBytes] + "\n... (truncated)"
				}
				out[name] = s
			}
		}
	}
	return out
}

// shouldGeneratePlan decides whether a turn warrants the ~5-15s plan
// pipeline cost. We skip plans for:
//   - T0 (trivial chat — "hi", "thanks") where a plan is wasted budget
//   - explicit follow-up / clarification requests that depend on the
//     prior turn's plan, which we'd just regenerate identically
//
// Everything else gets a plan — we'd rather plan and have the model
// ignore it than not plan and let the model thrash.
func shouldGeneratePlan(ctx *AgentContext, message string) bool {
	// A V3-bypassed demo request is the baseline side of the comparison.
	// Running the V3 planner here made that pane visibly orchestrated even
	// though its file writes bypassed V3 later in the turn.
	if ctx != nil && ctx.BypassV3 {
		return false
	}
	if ctx.Tier == Tier0Conversational {
		return false
	}
	// Single-line ack-style messages where the user is just steering
	// the existing direction ("yes do that", "looks good", "try again")
	// — already-running plan is still relevant; a fresh one would just
	// re-derive it.
	trimmed := strings.ToLower(strings.TrimSpace(message))
	return len(trimmed) >= 12
}

// generatePlan hits /v3/plan with a sampled project context and the
// user's message, streaming plan_* stage events out to the TUI as
// `v3_plan` events. Returns the winning Plan or nil if the planner
// errored — callers should treat nil as "no plan, proceed without
// adherence gating".
func generatePlan(ctx *AgentContext, userMessage string) *Plan {
	if ctx.V3URL == "" {
		return nil
	}
	pctx := samplePlanContext(ctx.WorkingDir, 6, 2000)
	req := V3PlanRequest{
		UserMessage:    userMessage,
		WorkingDir:     ctx.WorkingDir,
		ProjectContext: pctx,
		NCandidates:    3,
	}

	planStart := time.Now()
	Emit(NewEnvelope(EvtStageStart, "v3:plan", map[string]interface{}{
		"detail":     fmt.Sprintf("planning: %s", truncateStr(userMessage, 60)),
		"context_n":  len(pctx),
		"candidates": req.NCandidates,
	}))

	plan, err := callV3PlanStreaming(ctx.Ctx, ctx.V3URL, req, func(stage, detail string, data map[string]interface{}) {
		// Filter out per-token events — the LLM emits ~150 token deltas
		// per candidate × 3 candidates = ~450 streamed events. Forwarding
		// every one to the TUI as a separate v3_plan row clogs the
		// pipeline pane (same regression as the v3-generation token
		// spam we already fixed). The structural plan stages
		// (plan_candidate, plan_candidate_scored, plan_selected) are
		// what the renderer actually wants — token-level visibility is
		// debug noise.
		switch stage {
		case "token", "llm_start", "llm_end":
			return
		}
		payload := map[string]interface{}{"stage": stage, "detail": detail}
		for k, v := range data {
			payload[k] = v
		}
		ctx.Stream("v3_plan", payload)
		// Mirror to the typed broker so non-TUI consumers (logs, audit)
		// see the same stream.
		Emit(NewEnvelope(EvtMetric, "v3:plan:"+stage, payload))
	})
	dur := time.Since(planStart).Milliseconds()

	if err != nil {
		log.Printf("[agent] plan generation failed: %v", err)
		Emit(Envelope{
			EventID:    NewEventID(),
			Timestamp:  float64(time.Now().UnixNano()) / 1e9,
			Type:       EvtStageEnd,
			Stage:      "v3:plan",
			DurationMS: dur,
			Payload:    map[string]interface{}{"success": false, "error": err.Error()},
		})
		return nil
	}

	Emit(Envelope{
		EventID:    NewEventID(),
		Timestamp:  float64(time.Now().UnixNano()) / 1e9,
		Type:       EvtStageEnd,
		Stage:      "v3:plan",
		DurationMS: dur,
		Payload: map[string]interface{}{
			"success":           true,
			"steps":             len(plan.Steps),
			"verify_step":       plan.VerifyStep,
			"winning_score":     plan.WinningScore,
			"candidates_tested": plan.CandidatesTested,
		},
	})

	// Stream the full plan structure so the TUI / IDE plugins can
	// render the step list. Per-stage events (plan_start, plan_selected,
	// etc.) only carry counts and indices — the actual step rows live
	// here. One event per plan: subsequent step satisfaction goes
	// through plan_adherence, and a revision fires another plan_loaded.
	planPayload := map[string]interface{}{
		"steps":         plan.Steps,
		"verify_step":   plan.VerifyStep,
		"rationale":     plan.Rationale,
		"winning_score": plan.WinningScore,
		"revision":      0,
	}
	ctx.Stream("plan_loaded", planPayload)
	Emit(NewEnvelope(EvtMetric, "v3:plan:loaded", planPayload))

	return plan
}

// One turn of the agent loop: the run opens, the model is called, and its reply is read.

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// maxContentLoopRecoveries bounds how often a run answers a repetition
// cut with a corrective. A degenerating model is the failure ATLAS exists
// to absorb, so the first cut must not end a run that still owes work --
// but an unbounded retry is its own hang.
const maxContentLoopRecoveries = 2

// contentLoopRecoveryAllowance and contentLoopCountUnproductive expose the two
// halves of that bound separately, so an experiment can change ONE of them.
// Defaults reproduce the shipped behaviour exactly: allowance 2, every
// recovery charged.
func contentLoopRecoveryAllowance() int {
	if v := envOr("ATLAS_CONTENT_LOOP_RECOVERIES", ""); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			return n
		}
	}
	return maxContentLoopRecoveries
}

// contentLoopCountUnproductive reports whether the allowance counts only
// CONSECUTIVE recoveries that led to nothing landing on disk.
//
// Measured (stabilization cycle 4, flask_pause, both repetitions, identical to
// the token): cut at turn 1, recovery 1, cut at turn 2, recovery 2, then a
// valid replace_lines at turn 3 that landed, then a third cut at turn 4 which
// hit the allowance and ended the run with the feature half applied — at 271 s
// of a 570 s work budget, with the prompt at 7,270 tokens of an 18,432-token
// conversation budget. The counter had spent both charges before the run made
// its only productive edit, and that edit did not give any of them back.
func contentLoopCountUnproductive() bool {
	return envOr("ATLAS_CONTENT_LOOP_COUNT", "all") == "unproductive"
}

// --- runAgentLoop's steps -----------------------------------------------------
//
// The state runAgentLoop carries from turn to turn, and the steps of a turn in
// the order they run.

// agentLoop is what runAgentLoop carries from one turn to the next: the run's
// context, the request, the run state and the loop's own counters.
// Exit-gate evidence + the shared bounce shape live on runState (see
// its field docs); the remaining counters are loop-local.
type agentLoop struct {
	ctx         *AgentContext
	userMessage string
	st          *runState

	consecutiveReads  int // Track consecutive read-only calls
	consecutiveErrors int
	totalFailures     int // Track consecutive tool failures to break error loops
	repeatDetections  int // hard-stop after the 2nd repeated-identical-call detection
	// Runaway backstop for content-varying write loops (#147 review finding
	// #14): the content-fingerprint repeat detector, by design, does not
	// catch a model that rewrites one file with materially different content
	// every time and never converges. This counts total writes per path and
	// escalates to the repeat corrective only at a threshold far above any
	// realistic iteration (polyglot's healthiest run was ~10), so it stops a
	// true runaway without regressing legitimate iteration.
	writeCountByPath map[string]int
	// One-shot: when a loop-stop is about to fire but the task's named
	// deliverable was never written, steer toward it once instead of
	// stopping (many hard tasks loop on run_command exploration and
	// hard-stop without ever reaching the done/text exit where the
	// expected-output gate lives — observed on sqlite and merge-diff).
	outputRescueUsed bool
	// Flag whether we've already injected the approaching-budget hint,
	// so we don't fire it every turn after crossing the threshold.
	budgetHintFired bool
}

// newAgentLoop sets up one run of the agent loop: its run state, with the
// output obligation already decided, and counters that start at zero.
func newAgentLoop(ctx *AgentContext, userMessage string, outputObligation obligationDecision) *agentLoop {
	st := &runState{
		expectedOutputs:       outputObligation.Items,
		outputObligation:      outputObligation,
		userWantsVerification: isFixIntentMessage(userMessage),
	}
	return &agentLoop{ctx: ctx, userMessage: userMessage, st: st, writeCountByPath: map[string]int{}}
}

// endStream ends the run with its terminal event.
//
// A tool_call is streamed the moment it parses, before permission and
// execution, so every early exit between that point and the tool_result
// leaves the client holding a call that never resolves — a spinner with
// nothing coming.
//
// endStream answers the outstanding call before the summary, so the
// invariant holds at every exit rather than at each one that
// remembered to.
func (loop *agentLoop) endStream(status TerminalStatus, reason, summary string) {
	emitTerminal(loop.ctx, loop.st, status, reason, summary)
}

// accountRefusedCall counts a call that a gate refused before dispatch as a
// failed call, and reports whether the run must stop.
//
// Several branches refuse a call before dispatch and continue. Every one of
// them is a failed call the model may ignore forever, so each has to reach
// the same path-aware accounting an executed failure reaches -- the raw
// pre-resolution intent, the canonical target, the counters, and the
// bounded failure policy that reads them. Returns true when the caller must
// stop; the caller keeps its own diagnostic, which is unchanged.
//
// A run that ended because the client left or the deadline fired is not a
// model repeating itself, and those paths keep their own terminals.
func (loop *agentLoop) accountRefusedCall(name string, intent json.RawMessage, rejection, failPath string) bool {
	ctx, st := loop.ctx, loop.st
	if ctx.Ctx != nil && ctx.Ctx.Err() != nil {
		loop.consecutiveErrors++
		return false
	}
	recordFailedToolCall(ctx, name, intent, rejection)
	loop.consecutiveErrors++
	loop.totalFailures++
	ctx.RecentFailurePaths = appendRecentFailurePath(ctx.RecentFailurePaths, failPath)
	if !shouldStopForFailures(loop.totalFailures, loop.consecutiveErrors, ctx.RecentFailurePaths) {
		return false
	}
	log.Printf("[agent] breaking at turn %d: %d refused/failed calls, %d consecutive on %q",
		st.turn, loop.totalFailures, loop.consecutiveErrors, failPath)
	loop.endStream(TerminalStopped, "repeated_refusal",
		repeatedRefusalSummary(name, failPath, st.madeProductiveChange)+
			liveBackgroundJobNote(ctx))
	return true
}

// beginAgentRun records what the request asked, before any turn runs, and
// tells the event stream that the agent stage has started. It returns the
// time the stage started.
func beginAgentRun(ctx *AgentContext, userMessage string) time.Time {
	// One snapshot per validated request, before any turn runs. Only the
	// immutable inputs: the live decision belongs to the gate records.
	emitShadowRequestSnapshot(ctx, userMessage)
	// Capture the human's actual instruction before the loop appends
	// anything: correctives, manifests and re-injected content all ride
	// user-role messages, and everything downstream that needs "what was
	// I asked" (the V3 bridge above all) must not confuse those with this.
	ctx.HumanTask = userMessage
	ctx.LiteralBlocks = extractLiteralBlocks(userMessage)
	// What the user's workspace held before this request: a shell command
	// that removes one of these files removes the user's work.
	ctx.InitialWorkspace = snapshotWorkspace(ctx.WorkingDir)
	if n := len(ctx.LiteralBlocks); n > 0 {
		log.Printf("[agent] %d literal content contract(s) extracted from the request", n)
	}
	// Emit a stage_start envelope so the TUI's pipeline pane shows
	// the agent is working. Mirrors the typed-event broker.
	loopStart := time.Now()
	Emit(NewEnvelope(EvtStageStart, "agent", map[string]interface{}{
		"detail": fmt.Sprintf("tier=%s msg=%q", ctx.Tier,
			truncateStr(userMessage, 80)),
	}))
	return loopStart
}

// endAgentStage closes the agent stage on the event stream and reports how
// the run ended. runAgentLoop defers it, so it runs at every exit.
func endAgentStage(ctx *AgentContext, loopStart time.Time) {
	// A deletion approval the turn never consumed holds a reference to
	// the object it was about. The turn is over; let it go.
	releaseDeleteApproval(ctx)
	// Close the "agent" stage so the pipeline pane stops showing it
	// running. Without this, the TUI's pipelineState.apply only ever
	// sees EvtDone (overall finish) and the agent row is stuck in
	// Running() forever — visually misleading after the turn ended.
	dur := time.Since(loopStart).Milliseconds()
	// The broker said "success": true on every terminal, including every
	// stop. It now reports the session's actual outcome, from the same
	// field the SSE terminal used, so the two streams cannot disagree.
	// `success` keeps its key and its type for existing readers.
	status := ctx.TerminalStatus
	if !status.Classified() {
		status = TerminalIncomplete
	}
	Emit(Envelope{
		EventID:    NewEventID(),
		Timestamp:  float64(time.Now().UnixNano()) / 1e9,
		Type:       EvtStageEnd,
		Stage:      "agent",
		DurationMS: dur,
		Payload: map[string]interface{}{
			"success":       status.Completed(),
			"status":        string(status),
			"reason":        ctx.TerminalReason,
			"total_tokens":  ctx.TotalTokens,
			"fenced_calls":  ctx.FencedCalls,
			"fenced_tokens": ctx.FencedTokens,
		},
	})
	donePayload := map[string]interface{}{
		"success":           status.Completed(),
		"status":            string(status),
		"reason":            ctx.TerminalReason,
		"total_duration_ms": dur,
		"total_tokens":      ctx.TotalTokens,
		"fenced_calls":      ctx.FencedCalls,
		"fenced_tokens":     ctx.FencedTokens,
	}
	if ctx.TerminalUnresolved != "" {
		donePayload["unresolved"] = ctx.TerminalUnresolved
	}
	Emit(Envelope{
		EventID:    NewEventID(),
		Timestamp:  float64(time.Now().UnixNano()) / 1e9,
		Type:       EvtDone,
		Stage:      "agent",
		DurationMS: dur,
		Payload:    donePayload,
	})
}

// openConversation plans the request, builds the system prompt and opens the
// conversation the model will see. It reports false when the run must not
// start because the proxy and the sandbox look at different directories; the
// run has then already ended with that reason.
func openConversation(ctx *AgentContext, userMessage string) bool {
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
	// Refuse to start on a split workspace. The proxy writing to one host
	// directory while the sandbox that runs commands is bound to another is
	// invisible to every /health, and the session that follows is worse than
	// useless: files land, `run_command` reports them missing, and the model
	// spends its turns concluding its own work does not exist. Cached with a
	// TTL, so this is one probe per session at most.
	if problem := verifyWorkspaceAlignment(ctx); problem != "" {
		log.Printf("[agent] refusing to start — proxy and sandbox workspaces are not aligned")
		emitTerminal(ctx, nil, TerminalFailed, "workspace_misaligned", problem)
		return false
	}

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
		injectSymbolContext(ctx, symbols)
	}

	ctx.Messages = append(ctx.Messages, AgentMessage{Role: "user", Content: userMessage})

	// Per-session cache scope. llama.cpp's KV slot persists between requests
	// by default — that's what keepLlamaWarm relies on. But a slot also
	// persists across user sessions, so context from a previous session's
	// conversation can bias the next session. Erase every llama slot at the
	// start of each agent loop call (eraseLlamaSlot). llama.cpp re-encodes
	// the system prompt from scratch (~1-2s on a warm GPU); the per-turn
	// cache benefit within the session is preserved. Disable with
	// ATLAS_FRESH_SLOT_PER_SESSION=0.
	if envOr("ATLAS_FRESH_SLOT_PER_SESSION", "1") != "0" && !ctx.DisableFreshSlot {
		eraseLlamaSlot(ctx)
	}
	return true
}

// injectSymbolContext adds the definitions of the project symbols the request
// names, and their call-graph neighbourhood, to the conversation, so the model
// need not spend turns finding them. Every failure leaves the conversation as
// it was.
func injectSymbolContext(ctx *AgentContext, symbols []string) {
	fileMap := walkPythonFiles(ctx.WorkingDir)
	if len(fileMap) > 0 {
		if idx, ok := resolveProjectSymbols(ctx, fileMap, symbols); ok && len(idx.Matched) > 0 {
			body := formatProjectContextMessage(idx.Matched)
			// #39 Phase 3: append the call-graph neighborhood v3-service
			// returned. Empty string when it returned none.
			body += formatGraphNeighborhood(idx.Graph)
			if body != "" {
				ctx.Messages = append(ctx.Messages, systemNote(body))
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

// turnOutcome is what one step of a turn tells runAgentLoop to do next.
type turnOutcome int

const (
	turnContinues turnOutcome = iota // the turn goes on to its next step
	turnEnds                         // the turn is over: the next turn starts
	runEnds                          // the run is over: runAgentLoop returns
)

// takeModelTurn runs the start of turn `turn`: it calls the model, then parses
// and checks the reply. The outcome says whether the turn goes on with the
// parsed reply, ends here, or ends the run; when the run ends, the error is
// what runAgentLoop returns.
func (loop *agentLoop) takeModelTurn(turn int) (string, ModelResponse, turnOutcome, error) {
	ctx, st := loop.ctx, loop.st
	loop.startTurn(turn)

	// Bail out fast if the upstream request was cancelled (the client closed the
	// connection, user hit Ctrl-C, terminal exited). Without this check the
	// loop would keep grinding LLM calls and tool work for a client that's
	// already gone, burning GPU.
	if ctx.Ctx != nil {
		select {
		case <-ctx.Ctx.Done():
			return "", ModelResponse{}, runEnds, finishCancelledRun(ctx, st, turn)
		default:
		}
	}

	llmStart := announceModelCall(ctx, turn)

	// Call LLM with grammar constraint
	response, tokens, err := callLLMConstrained(ctx)
	llmElapsed := time.Since(llmStart)
	if err != nil {
		return "", ModelResponse{}, runEnds, loop.endOnInferenceFailure(turn, err, llmElapsed)
	}
	loop.recordModelReply(turn, response, tokens, llmElapsed)

	// Parse the response — extract JSON even if model added surrounding text
	parsed, parseErr := extractModelResponse(response)
	if parseErr != nil {
		return response, parsed, loop.recoverFromParseFailure(turn, response, parseErr), nil
	}
	if outcome := loop.refuseBrokenContent(turn, parsed); outcome != turnContinues {
		return response, parsed, outcome, nil
	}
	logModelTurn(ctx, turn, parsed, response)
	return response, parsed, turnContinues, nil
}

// startTurn opens turn `turn` on the run state and, once the run nears its
// turn cap, tells the model so, once.
func (loop *agentLoop) startTurn(turn int) {
	ctx, st := loop.ctx, loop.st
	st.turn = turn
	// Budget hint — only relevant when there IS a turn cap. T1/T2/T3
	// default to uncapped (ctx.MaxTurns == 0), so the hint is mostly
	// dormant unless an operator sets ATLAS_MAX_TURNS. It fires at 80%
	// of the cap, so T0 (capped by TierMaxTurns) still gets one if a
	// conversational request unexpectedly tries to loop.
	if !loop.budgetHintFired && ctx.MaxTurns > 0 && turn > 0 && turn*5 >= ctx.MaxTurns*4 {
		loop.budgetHintFired = true
		ctx.Messages = append(ctx.Messages, AgentMessage{
			Role: "system",
			Content: fmt.Sprintf(
				"Turn budget notice: you're at turn %d of %d. If significant work remains, prioritize finishing the highest-impact items and verifying them — do not start new exploration. If you can finish in the remaining turns, keep going. If you cannot, summarize what's done and what's not in your `done` summary so the user knows what to follow up on.",
				turn, ctx.MaxTurns),
		})
	}
}

// announceModelCall trims the conversation to its token budget and announces
// the coming model call on the stream. It returns the time the call starts.
func announceModelCall(ctx *AgentContext, turn int) time.Time {
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
	return llmStart
}

// endOnInferenceFailure reports a model call that failed and ends the run. It
// returns what runAgentLoop returns: the cancelled run's own result when the
// work context ended, and the call's error otherwise.
func (loop *agentLoop) endOnInferenceFailure(turn int, err error, llmElapsed time.Duration) error {
	ctx, st := loop.ctx, loop.st
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
	// A call that failed BECAUSE the work context ended is not an
	// inference failure: the deadline is ours, and reporting the
	// symptom would hide the cause and skip finalisation.
	if ctx.Ctx != nil && ctx.Ctx.Err() != nil {
		return finishCancelledRun(ctx, st, turn)
	}
	// An `error` event is not an outcome. This exit streamed one and
	// returned, so the client saw a tool call, an error, and then
	// nothing — aoc_sonar died here in BOTH reps on a context-size
	// 400 and the user got silence. Every other exit in this loop
	// authors a `done`; this one has to as well, or the failure is
	// invisible to anything rendering the stream.
	emitTerminal(ctx, st, TerminalFailed, "inference_failed",
		inferenceFailureSummary(err, st.madeProductiveChange)+liveBackgroundJobNote(ctx))
	return fmt.Errorf("LLM call failed on turn %d: %w", turn, err)
}

// recordModelReply counts the reply's tokens, keeps the reply on the run state
// and reports the finished model call on the stream.
func (loop *agentLoop) recordModelReply(turn int, response string, tokens int, llmElapsed time.Duration) {
	ctx, st := loop.ctx, loop.st
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
}

// recoverFromParseFailure answers a reply that did not parse. A content loop is
// corrected while its allowance lasts and a cut text answer is delivered; any
// other failure goes back to the model with feedback until three in a row end
// the run.
func (loop *agentLoop) recoverFromParseFailure(turn int, response string, parseErr error) turnOutcome {
	ctx, st := loop.ctx, loop.st
	// Classify the failure shape once: a category for the log so docker
	// logs reads "what kind of broken" at a glance, and targeted
	// feedback for the model. A generic "your response wasn't JSON"
	// gives the model no way to see that its response was truncated at
	// the llama-server token cap, so it keeps emitting the same too-big
	// payload. A text answer we cut mid-string is still an answer.
	// Deliver what was written rather than nothing — the alternative is
	// a user who asked a question and received silence.
	if ctx.LastStreamCut == "content_loop" {
		// A repetition loop is a model failure, and absorbing model
		// failure is what this system is for. Ending the run the first
		// time it happens hands back a half-built project, while the
		// corrective that names this exact failure
		// (classifyParseFailure) goes unused. Answer the loop and keep
		// going, bounded, before considering the partial reply an
		// outcome. Under the unproductive-only accounting, work that
		// landed since the last charge clears the count: the run is
		// making progress between cuts, which is not the runaway this
		// bound exists to stop.
		if outcome := loop.answerContentLoop(turn, response); outcome != turnContinues {
			return outcome
		}
	}
	category, feedback := parseFailureFeedback(ctx, response, ctx.LastStreamCut)
	log.Printf("[agent] parse error: %v | category=%s raw_len=%d | raw %s",
		parseErr, category, len(response), safeTextSummary(response))
	ctx.Stream("error", map[string]string{
		"error":    "failed to parse model response",
		"category": category,
	})
	// The attempt goes back in as the assistant turn it was, before
	// the feedback, so the retry does not resume from the prefix that
	// produced it (see attemptEcho).
	ctx.Messages = append(ctx.Messages,
		AgentMessage{Role: "assistant", Content: attemptEcho(response, ctx.LastStreamCut != "")},
		AgentMessage{Role: "user", Content: feedback})
	// Cap parse failures the same way we cap tool failures.
	// Three parse errors in a row is a stuck loop; bailing
	// keeps us from burning more LLM round-trips.
	loop.consecutiveErrors++
	if loop.consecutiveErrors >= 3 {
		log.Printf("[agent] breaking parse-error loop at turn %d (%d consecutive)", turn, loop.consecutiveErrors)
		summary := "Stopped after 3 unparseable responses — the model's tool calls keep " +
			"getting truncated. Try a more targeted request (e.g. 'edit just the " +
			"@app.route(\"/product\") handler in app.py') so the response stays under the " +
			"token cap."
		if ctx.LastStreamCut == "old_str_unmatched" {
			summary = "Stopped: three edit_file calls in a row ran old_str past the text in " +
				"the file, and each was cut before it was sent. Nothing was changed by them. " +
				"Ask again and name the line to change, or ask for replace_lines with line numbers."
		}
		if ctx.LastStreamCut == "content_loop" {
			// Same misdiagnosis the classifier used to make: the token cap
			// had nothing to do with it. The model began repeating itself
			// and the proxy cut the stream, so "make the request smaller"
			// is advice the user cannot act on.
			summary = "Stopped: the model began repeating itself and its response was cut " +
				"off mid-call, three times. This usually means it tried to reproduce a " +
				"large block of data — the contents of an input or fixture file — " +
				"instead of writing code that reads it. Ask again and say explicitly " +
				"that the data file should be read at runtime, not rewritten."
		}
		emitTerminal(ctx, st, TerminalStopped, "unusable_model_output", summary)
		return runEnds
	}
	return turnEnds
}

// answerContentLoop answers a reply the stream cut because the model began
// repeating itself. While the allowance lasts the model is corrected and the
// turn ends; after that a cut text answer is delivered and the run ends. It
// goes on when neither applies.
func (loop *agentLoop) answerContentLoop(turn int, response string) turnOutcome {
	ctx, st, userMessage := loop.ctx, loop.st, loop.userMessage
	if contentLoopCountUnproductive() && st.productiveChanges > st.productiveAtLastCharge {
		if st.contentLoopRecoveries > 0 {
			log.Printf("[agent] content-loop allowance reset at turn %d — %d change(s) landed since the last one",
				turn, st.productiveChanges-st.productiveAtLastCharge)
		}
		st.contentLoopRecoveries = 0
	}
	if st.contentLoopRecoveries < contentLoopRecoveryAllowance() {
		st.contentLoopRecoveries++
		st.productiveAtLastCharge = st.productiveChanges
		_, corrective := parseFailureFeedback(ctx, response, ctx.LastStreamCut)
		log.Printf("[agent] content loop at turn %d — correcting and continuing (%d/%d)",
			turn, st.contentLoopRecoveries, contentLoopRecoveryAllowance())
		ctx.Stream("agent_loop_recovery", map[string]interface{}{
			"turn": turn, "attempt": st.contentLoopRecoveries,
			"reason": "the model began repeating itself; the stream was cut and it was told why",
		})
		ctx.Messages = append(ctx.Messages,
			AgentMessage{Role: "assistant", Content: attemptEcho(response, true)},
			AgentMessage{Role: "user", Content: corrective})
		return turnEnds
	}
	if salvaged, ok := recoverTruncatedText(response); ok {
		log.Printf("[agent] salvaged %d chars of a cut text answer at turn %d", len(salvaged), turn)
		ctx.Stream("text", map[string]string{"content": salvaged})
		// The salvaged text is the model's own words and already
		// reached the client as a `text` event. Repeating it in
		// the summary of an INCOMPLETE terminal is how half-written
		// code came to read as the answer, so the summary describes
		// what happened instead of restating it.
		salvageSummary := "The reply was cut short — it had begun repeating itself, " +
			"so what arrived above is partial. Ask again if something is missing."
		// Salvage is a third exit, alongside done and text, and it
		// reached the user without the honesty the other two apply.
		// Observed on aoc_slope rep2 and smallrung_toml rep2 (run
		// 16): the cut reply was half-written code, so the run
		// finished by handing back code that reads like the answer
		// while nothing was on disk.
		if st.actionDemandedAndUnmet(ctx, userMessage) {
			log.Printf("[agent] salvaged text at turn %d with nothing written — saying so", turn)
			salvageSummary = nothingWrittenSummary(salvageSummary)
		}
		emitTerminal(ctx, st, TerminalIncomplete, "text_instead_of_work",
			salvageSummary+liveBackgroundJobNote(ctx))
		return runEnds
	}
	return turnContinues
}

// refuseBrokenContent refuses, before anything runs, a tool call whose content
// string ended early or whose intended bytes are ambiguous. Three refusals in a
// row end the run.
func (loop *agentLoop) refuseBrokenContent(turn int, parsed ModelResponse) turnOutcome {
	ctx, st := loop.ctx, loop.st
	// A tool_call can parse cleanly and still be mis-segmented: an
	// unescaped quote inside a file body closes the JSON string early and
	// the file's remainder is discarded into unknown keys, so a truncated
	// write reports success. Catch it here, before execution, and hand the
	// model the real cause under the same retry cap parse failures use —
	// never write the truncated content.
	if parsed.Type == "tool_call" {
		if feedback, bad := swallowedContentFeedback(parsed.Name, parsed.Args); bad {
			log.Printf("[agent] turn=%d %s: content string terminated early (unescaped quote) — refusing the truncated write", turn, parsed.Name)
			ctx.Stream("error", map[string]string{
				"error":    "tool call content was truncated by an unescaped quote",
				"category": "swallowed_content",
			})
			ctx.Messages = append(ctx.Messages, AgentMessage{Role: "user", Content: feedback})
			loop.consecutiveErrors++
			if loop.consecutiveErrors >= 3 {
				log.Printf("[agent] breaking swallowed-content loop at turn %d (%d consecutive)", turn, loop.consecutiveErrors)
				emitTerminal(ctx, st, TerminalStopped, "unusable_model_output",
					"Stopped: the model's file writes kept ending early on an unescaped double-quote inside the content, three times in a row. Ask again and, for a large HTML/JS file, request that it be built with structural_edit or split across smaller writes."+liveBackgroundJobNote(ctx))
				return runEnds
			}
			return turnEnds
		}
		// Content whose intended bytes are ambiguous (prose around a fence,
		// a doubled escape, a line break that lost its backslash) is
		// refused, not rewritten into a guess. Same retry cap.
		if feedback, bad := jsonChannelContentFeedback(parsed.Name, parsed.Args); bad {
			log.Printf("[agent] turn=%d %s: content bytes ambiguous — refused before execution", turn, parsed.Name)
			ctx.Stream("error", map[string]string{
				"error":    "tool call content was refused before execution: its intended bytes were ambiguous",
				"category": "ambiguous_content",
			})
			ctx.Messages = append(ctx.Messages, AgentMessage{Role: "user", Content: feedback})
			loop.consecutiveErrors++
			if loop.consecutiveErrors >= 3 {
				log.Printf("[agent] breaking ambiguous-content loop at turn %d (%d consecutive)", turn, loop.consecutiveErrors)
				emitTerminal(ctx, st, TerminalStopped, "unusable_model_output",
					"Stopped: nothing was written for the last three file writes — each one's content was ambiguous (a markdown fence with text around it, or a doubled or lost escape) and was refused rather than guessed at. Ask again."+liveBackgroundJobNote(ctx))
				return runEnds
			}
			return turnEnds
		}
	}
	return turnContinues
}

// logModelTurn writes the turn's reply to the operator log, its arguments
// summarised, and the raw reply's shape when a tool call came without any.
func logModelTurn(ctx *AgentContext, turn int, parsed ModelResponse, response string) {
	// Log the args truncated — enables diagnosing failures like
	// "all 3 tool calls returned Success=false" without having to add
	// breakpoints.
	logEvent("info",
		fmt.Sprintf("[agent] turn=%d type=%s name=%s args=%s",
			turn, parsed.Type, parsed.Name, safeArgsSummary(parsed.Name, parsed.Args)),
		requestIDFromContext(ctx.Ctx), nil)

	// When a tool_call still has no args after liftMissingArgs,
	// log the raw model output so we can see exactly what shape was
	// emitted — helps catch new alt-shapes the lift logic missed.
	if parsed.Type == "tool_call" && (len(parsed.Args) == 0 || string(parsed.Args) == "null") {
		// The shape is the diagnosis; the text is model output. A size and
		// a stable hash correlate two occurrences without reproducing one.
		log.Printf("[agent] turn=%d EMPTY ARGS — model output %s", turn, safeTextSummary(response))
	}
}

// endOnDone answers the model's done. A done that the run's own evidence
// contradicts is bounced and the turn ends (false); otherwise the run ends with
// its terminal (true).
func (loop *agentLoop) endOnDone(turn int, parsed ModelResponse) bool {
	ctx, st, userMessage := loop.ctx, loop.st, loop.userMessage
	// The four honesty gates (see runState.exitGates): a done that
	// the run's own evidence contradicts is bounced, capped.
	if gate, rejection := st.exitGates(ctx, userMessage, parsed.Summary); gate != "" {
		st.bounce(ctx, gate, rejection)
		return false
	}
	// A model saying it is finished is a claim, not a demonstration.
	// The decision comes FIRST, because it decides whose words the
	// user reads: the model's account is only repeated where the
	// exact-hash gate authorised the completion it describes.
	status, reason := finalizeCompletion(ctx, st, userMessage, "")
	if reason == "unresolved_mutation_debt" {
		if msg := offerDebtRecovery(ctx, st); msg != "" {
			st.bounce(ctx, "done", msg)
			return false
		}
	}
	summary := modelProseIfAuthorized(status, parsed.Summary)
	if reason == "unresolved_mutation_debt" {
		summary = unresolvedDebtSummary(st)
	}
	// Past the gates, but the verification gate can be past because
	// it ran out of bounces rather than because anything verified.
	if st.verificationDemandedAndUnmet() {
		log.Printf("[agent] done at turn %d with no passing verification — replacing the summary", turn)
		summary = unverifiedSummary(st.madeProductiveChange,
			modelProseIfAuthorized(status, parsed.Summary))
	}
	// Same reasoning one gate over: the action gate can be past
	// because its bounces ran out, not because anything was written.
	if st.actionDemandedAndUnmet(ctx, userMessage) {
		log.Printf("[agent] done at turn %d with nothing written — saying so", turn)
		summary = nothingWrittenSummary(summary)
	} else {
		summary = withNoChangeNote(ctx, st, status, summary)
	}
	emitTerminal(ctx, st, status, reason, summary+liveBackgroundJobNote(ctx))
	return true
}

// endOnText answers the model's text reply, the same way endOnDone answers a
// done: bounced, the turn ends (false); otherwise the run ends with its
// terminal (true).
func (loop *agentLoop) endOnText(turn int, parsed ModelResponse) bool {
	ctx, st, userMessage := loop.ctx, loop.st, loop.userMessage
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
		return false
	}
	ctx.Stream("text", map[string]string{"content": parsed.Content})
	textSummary := ""
	if st.actionDemandedAndUnmet(ctx, userMessage) {
		log.Printf("[agent] text exit at turn %d with nothing written — saying so", turn)
		textSummary = nothingWrittenSummary("")
	}
	// A text reply carries no file obligation of its own; when the
	// run also wrote something, or was asked to change something and
	// did not, the same demonstration is required. Same decision as
	// the done exit, so the two cannot drift.
	textStatus, textReason := finalizeCompletion(ctx, st, userMessage, "text_reply")
	if textReason == "unresolved_mutation_debt" {
		if msg := offerDebtRecovery(ctx, st); msg != "" {
			st.bounce(ctx, "text", msg)
			return false
		}
		textSummary = unresolvedDebtSummary(st)
	}
	if textSummary == "" {
		textSummary = withNoChangeNote(ctx, st, textStatus, "")
	}
	emitTerminal(ctx, st, textStatus, textReason, textSummary)
	return true
}

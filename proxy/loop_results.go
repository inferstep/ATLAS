// A tool call runs and its result is answered, with what the loop learns from it.

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// maxTotalFailures bounds a whole run's failed tool calls, independent of the
// consecutive-error breaker. That breaker now resets when the rejection
// changes kind (a converging model must not be killed for iterating), so this
// is what stops a run cycling through failure modes forever. Set well above
// what a legitimate multi-edit task needs: run 11 used 3 and was still short.
const maxTotalFailures = 12

// runToolCall takes a screened tool call through its last refusals and the
// loop-health detectors, runs it and reports its result. It returns the result
// and the correctives queued for the next turn. The outcome says whether the
// turn goes on, ends, or ends the run; the error is then what runAgentLoop
// returns.
func (loop *agentLoop) runToolCall(turn int, parsed ModelResponse, intentArgs json.RawMessage) (*ToolResult, turnCorrectives, turnOutcome, error) {
	ctx, st := loop.ctx, loop.st
	var steers turnCorrectives
	runawayWrite, outcome := loop.refuseBeforeDispatch(turn, parsed, intentArgs)
	if outcome != turnContinues {
		return nil, steers, outcome, nil
	}
	steers.repeat, outcome = loop.detectRepetition(turn, parsed, intentArgs, runawayWrite)
	if outcome != turnContinues {
		return nil, steers, outcome, nil
	}
	steers.reasoning = detectReasoningRepetition(ctx, turn)
	lens, outcome, err := loop.scoreWithLens(turn, parsed)
	if outcome != turnContinues {
		return nil, steers, outcome, err
	}
	steers.lens = lens

	// Execute tool. A re-read of an unchanged file already in
	// context is served from a compact pointer instead of
	// re-injecting + re-encoding the whole file (see
	// redundantReadShortCircuit).
	startTime := time.Now()
	result, outcome, err := loop.dispatchToolCall(turn, parsed)
	if outcome != turnContinues {
		return nil, steers, outcome, err
	}
	elapsed := time.Since(startTime)

	loop.recordCallResult(turn, parsed, intentArgs, result)
	streamToolResult(ctx, st, parsed, result, elapsed)
	// The lens stopped scoring inside this call (V3 reported it):
	// the run ends here, saying why (lens_required.go).
	if why := ctx.lensDownReason(); why != "" {
		emitTerminal(ctx, st, TerminalFailed, "lens_unavailable",
			lensUnavailableSummary(why, st.madeProductiveChange)+liveBackgroundJobNote(ctx))
		return nil, steers, runEnds, nil
	}
	loop.markWarnedWrite(parsed, result)
	loop.dischargeWarnedRuns(parsed, result)
	return result, steers, turnContinues, nil
}

// turnCorrectives are the loop-health correctives one tool call queues for the
// next turn: from the lens's quality crash, the repeated call and the rehashed
// reasoning, drained in that order. Each holds at most one message.
type turnCorrectives struct {
	lens, repeat, reasoning string
}

// detectRepetition runs the tool-call repetition detector and returns the
// corrective it queued for the next turn. The second detection ends the run,
// unless the task's named deliverable is still missing: that earns one
// output-rescue steer instead.
//
// Tool-call repetition detector. Catches the structural-loop
// case the lens scoring doesn't see: same exact (tool, args)
// emitted N times in close succession. Lens covers semantic
// repetition (model produced the same low-quality content);
// this covers structural repetition (model emitted the same
// call to read_file or run_command). Fires before tool
// execution so the corrective lands in the same iteration
// as the lens corrective if both trigger.
func (loop *agentLoop) detectRepetition(turn int, parsed ModelResponse, intentArgs json.RawMessage, runawayWrite bool) (string, turnOutcome) {
	ctx, st := loop.ctx, loop.st
	pendingRepeatCorrective := ""
	if msg, _, repeating := recordToolCall(ctx, parsed.Name, intentArgs); repeating || runawayWrite {
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
		loop.repeatDetections++
		// Steer-before-kill ladder. On the FIRST detection, fall
		// through: pendingRepeatCorrective is injected below and the
		// loop continues, so the model gets an explicit nudge to change
		// approach before we ever terminate. Only hard-stop on the
		// SECOND detection — the model saw the steer and repeated
		// anyway (genuinely stuck). The first detection steers even
		// after work has landed: a model repeating a verify command
		// (one that may itself be broken) can be one nudge from
		// finishing, and the broken-verify steer below can turn exactly
		// these into completions.
		if loop.repeatDetections >= 2 {
			// Output-rescue: if the task named a deliverable and it
			// isn't on disk, the model is looping WITHOUT having
			// committed its answer — steer toward the file once and
			// keep going rather than hard-stopping empty-handed.
			if missing := missingExpectedOutputs(ctx, st.expectedOutputs); len(missing) > 0 && !loop.outputRescueUsed {
				loop.outputRescueUsed = true
				loop.repeatDetections = 0
				pendingRepeatCorrective = expectedOutputMissingMessage(missing)
				log.Printf("[agent] repeat loop at turn %d but named deliverable(s) %v not on disk — output-rescue steer instead of stopping", turn, logPaths(missing))
			} else {
				log.Printf("[agent] second repetition detection at turn %d — stopping (productive_change_hint=%v)", turn, st.madeProductiveChange)
				// The one terminal wired to recovery. It runs BEFORE
				// the summary so the disclosure describes the bytes
				// that are actually on disk when the run ends. It is
				// a system action: no progress hint is set, no tool
				// event is emitted, and the terminal stays stopped.
				recovered := restoreSaferDeliverables(ctx)
				loop.endStream(TerminalStopped, "repeat_detector",
					repeatTerminalSummary(ctx, st.expectedOutputs,
						st.madeProductiveChange, recovered))
				return "", runEnds
			}
		}
	}
	return pendingRepeatCorrective, turnContinues
}

// detectReasoningRepetition watches the model's reasoning for the same opening
// turn after turn, and returns the corrective it queued, or "".
//
// Reasoning-repetition detector. The model's reasoning_content stream
// is captured per-turn in ctx.LastTurnReasoning by callLLMOnceWithGrammar.
// recordReasoning compares the normalized opening prefix against the
// prior turn's snippet; three consecutive identical openings
// (reasoningRepeatThreshold repeats) fire the intervention. Sibling to
// the structural repeat detector (above) and the lens regression
// detector (below) — three different angles on "model is stuck",
// catching different shapes of stuck-ness.
func detectReasoningRepetition(ctx *AgentContext, turn int) string {
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
	return pendingReasoningCorrective
}

// scoreWithLens scores the content of a write_file or edit_file with the
// geometric lens before it executes, and returns the corrective it queued on a
// quality crash, or "". A lens that cannot score ends the run, and the error is
// then what runAgentLoop returns.
//
// Score write_file/edit_file content with the geometric lens BEFORE executing.
// The score reflects what the model produced (independent of whether the tool
// succeeds). On a quality-crash pattern (N consecutive low scores) we queue a
// corrective that is appended AFTER the tool result, so the next LLM call sees:
// assistant(tool_call) → tool(result) → user([system note]: lens warning). This
// catches a model that keeps retrying the same stub while the stub gate keeps
// rejecting it.
func (loop *agentLoop) scoreWithLens(turn int, parsed ModelResponse) (string, turnOutcome, error) {
	ctx, st := loop.ctx, loop.st
	pendingLensCorrective := ""
	if scorable, ok := extractScorableContent(parsed.Name, parsed.Args); ok {
		score, scored, down := scoreContentForAgent(ctx.Ctx, ctx.LensURL, scorable)
		if down != "" {
			return "", runEnds, st.stopLensDown(ctx, parsed.Name, down)
		}
		if scored {
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
	return pendingLensCorrective, turnContinues, nil
}

// dispatchToolCall runs the tool call, unless a re-read can be answered from
// context or a re-run is held back until an edit lands. A deadline that lands
// during the call ends the run; the error is then what runAgentLoop returns.
func (loop *agentLoop) dispatchToolCall(turn int, parsed ModelResponse) (*ToolResult, turnOutcome, error) {
	ctx, st := loop.ctx, loop.st
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
		// The deadline can land in the middle of a tool call. Stop
		// here rather than starting another turn on a dead context.
		if ctx.Ctx != nil && ctx.Ctx.Err() != nil {
			return nil, runEnds, finishCancelledRun(ctx, st, turn)
		}
		// Debt is settled from the ledger's own evidence, never from
		// the call reporting that it worked.
		settleMutationDebt(ctx, st)
	}
	return result, turnContinues, nil
}

// recordCallResult records a failed call for the re-send ban, answering a
// second distinct rejected proposal in the same result, and clears what
// earlier failures left once a call works.
//
// On failure, log the error so it shows up in `docker compose
// logs atlas-proxy` without having to attach a debugger.
func (loop *agentLoop) recordCallResult(turn int, parsed ModelResponse, intentArgs json.RawMessage, result *ToolResult) {
	ctx, st := loop.ctx, loop.st
	if !result.Success {
		log.Printf("[agent] turn=%d tool=%q FAIL: %q", turn,
			truncateStr(parsed.Name, 64), truncateStr(result.Error, 240))
		recordFailedToolCall(ctx, parsed.Name,
			retryIdentityArgs(parsed.Name, intentArgs, parsed.Args), result.Error)
		// C4: a replacement refused while the file it targeted stayed
		// valid. The second distinct proposal against one generation
		// is the evidence that the refusal text alone is not landing,
		// and the answer rides on the SAME result -- one call, one
		// result, and the model reads it in the same message.
		if key, sha, distinct := noteRejectedProposal(ctx, st, parsed.Name,
			parsed.Args, result); distinct >= 2 {
			rel := ledgerArgPath(parsed.Args, "path")
			if msg := rejectedProposalRecovery(ctx, st, rel, key, sha); msg != "" {
				result.Error += "\n\n" + msg
			}
		}
	} else {
		// A call can fail and later succeed — an edit rejected for a
		// stale range works after a re-read. Drop the memory with the
		// condition that caused it.
		clearFailedToolCall(ctx, parsed.Name,
			retryIdentityArgs(parsed.Name, intentArgs, parsed.Args))
		clearSteerState(ctx, st, parsed.Name, parsed.Args)
		clearBrokenArtifactState(ctx, st, parsed.Name, intentArgs)
	}
}

// streamToolResult answers the tool call on both event streams.
func streamToolResult(ctx *AgentContext, st *runState, parsed ModelResponse, result *ToolResult, elapsed time.Duration) {
	st.pendingToolCall = ""
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
}

// markWarnedWrite marks a landed write_file whose result carries a parse
// warning as never run, and clears the mark of a clean one.
//
// A landed write whose result carries a parse warning is pending
// execution; a landed clean write clears its own pending mark.
func (loop *agentLoop) markWarnedWrite(parsed ModelResponse, result *ToolResult) {
	ctx, st := loop.ctx, loop.st
	if result.Success && parsed.Name == "write_file" {
		var wf WriteFileInput
		if json.Unmarshal(parsed.Args, &wf) == nil {
			var out WriteFileOutput
			warned := json.Unmarshal(result.Data, &out) == nil && out.Warning != ""
			st.markWarnedRun(ctx, wf.Path, warned)
		}
	}
}

// dischargeWarnedRuns clears the warned marks of the files a run_command or
// run_background attempted to execute, and settles the content debt that a
// clean execution demonstrates.
func (loop *agentLoop) dischargeWarnedRuns(parsed ModelResponse, result *ToolResult) {
	ctx, st := loop.ctx, loop.st
	if (parsed.Name == "run_command" || parsed.Name == "run_background") && result != nil {
		// Discharge only the marks this command actually attempted to
		// EXECUTE. Clearing on any command would let `ls` bless a
		// warned file, and clearing on a filename substring would let
		// `cat solve.py` or `grep main solve.py` do the same — naming a
		// file is not running it. The mark is "this version has never
		// been executed"; only an execution attempt (interpreter
		// invocation or ./file) changes that fact, and the attempt
		// itself suffices — pass or fail, the model has now seen the
		// real runtime behavior.
		//
		// run_background is an execution attempt too. A server script
		// is redirected there by the foreground gate, and its settle
		// window returns the same traceback run_command would; a mark
		// that only run_command could discharge would leave the file
		// permanently "never run" and refuse the model's fix.
		var rc struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(parsed.Args, &rc) == nil {
			for p := range st.pendingWarnedRun {
				if executionAttempt(rc.Command, p) {
					delete(st.pendingWarnedRun, p)
				}
			}
			// A clean execution is the strongest demonstration a file
			// can get, and it settles the content debt the parse-based
			// routes never reached.
			settleDebtByExecution(ctx, st, rc.Command, executionSucceeded(parsed.Name, result))
		}
	}
}

// reportLiteralRepair reports the repair of spacing inside lines of a literal
// the user stated: in the log and the event stream, in the result the model
// is about to read, and to the ledger, which follows the repaired bytes.
func reportLiteralRepair(ctx *AgentContext, result *ToolResult, path, lp, fixed string, repairedLits []string) {
	for _, l := range repairedLits {
		log.Printf("[agent] literal contract repaired in %s: %q now byte-exact", path, truncateStr(l, 60))
	}
	Emit(NewEnvelope(EvtMetric, "tool", map[string]interface{}{
		"name": "literal_repair", "value": path,
	}))
	// Stated in the result the model is about to read
	// -- a note, not a corrective turn -- so a change to
	// what it sent is never silent. The ledger follows
	// the bytes: without this, the session's own next
	// edit of the file was refused as "modified since
	// last read", and the write's verdict described
	// bytes no longer on disk.
	noteContentChange(result, fmt.Sprintf(
		"After the write, spacing inside %d line(s) was changed to match text the user stated exactly: %s",
		len(repairedLits), truncateStr(strings.Join(repairedLits, " | "), 200)))
	observeRewrite(ctx, lp, []byte(fixed))
}

// recordCallEffects records what the call changed and what it showed: a
// productive change, which takes back an earlier verification; what a
// run_command demonstrated; the repairs in progress; and the plan's
// adherence, which may ask for a revised plan.
func (loop *agentLoop) recordCallEffects(turn int, parsed ModelResponse, result *ToolResult) {
	ctx, st, userMessage := loop.ctx, loop.st, loop.userMessage
	// Track productive state changes — write/edit/delete/move that landed. Used
	// below to soften the error-loop exit when work was completed AND by the
	// done-without-action gate so a feature prompt ("rewrite X", "add Y") can't
	// declare done without any actual edit on disk. Every mutating tool belongs
	// in this list (structural_edit, insert_after, replace_lines and move_file
	// included), or a run whose only work was that tool counts as having done
	// nothing. A landed write whose result carries a parse warning is pending
	// execution; a landed clean write clears its own pending mark.
	if result.Success && (parsed.Name == "write_file" || parsed.Name == "edit_file" ||
		parsed.Name == "structural_edit" || parsed.Name == "delete_file" ||
		parsed.Name == "insert_after" || parsed.Name == "replace_lines" ||
		parsed.Name == "move_file") {
		st.madeProductiveChange = true
		st.productiveChanges++
		// A write AFTER a successful verification un-verifies the
		// run: what was checked is no longer what is on disk. Three
		// novel-benchmark sessions ran a working version, rewrote
		// it, and exited — the checker then found a traceback the
		// session never saw, because nothing demanded a re-run of
		// the final artifact. Verification is of an artifact, not
		// of a session.
		if st.verifiedThisLoop {
			log.Printf("[agent] %s after verification — the final artifact is unverified, re-arming the gate", parsed.Name)
			st.verifiedThisLoop = false
			st.verifiedStandalone = false
			st.verifiedByRedirect = ""
		}
	}

	// What this command demonstrated about the artifact.
	if parsed.Name == "run_command" {
		var rc RunCommandInput
		if json.Unmarshal(parsed.Args, &rc) == nil {
			st.observeVerification(ctx, userMessage, turn, rc.Command, result)
		}
	}

	// Repairs in progress: this call opens, extends or closes one on
	// its own verdict, and the result the model reads names every
	// file still open (repair.go).
	st.observeRepair(ctx, turn, parsed.Name, parsed.Args, result)
	result.OpenRepair = st.repairNote()

	// Plan-adherence accounting. Records whether this tool
	// call satisfied an unsatisfied step on ctx.Plan (if any),
	// updates the off-streak counter, and asks us to revise
	// the plan if the streak crossed the threshold. Advisory
	// — never blocks the call. recordPlanAdherence is a no-op
	// when ctx.Plan is nil (T0 / planner failure).
	if recordPlanAdherence(ctx, parsed.Name, parsed.Args, result.Success) {
		revisePlan(ctx, userMessage,
			fmt.Sprintf("agent went off-plan for %d consecutive tool calls (last: %s)",
				ctx.PlanOffStreak, parsed.Name))
	}
}

// editMissSteer is the steer for an edit_file whose old_str did not match: to
// structural_edit when the file has selectors, to replace_lines otherwise.
func editMissSteer(mp, source string) string {
	if sels := selectorsInFile(mp, source); len(sels) > 0 {
		return "edit_file's old_str did not match " +
			mp + " (small drift in whitespace/quotes is enough to miss). " +
			"Do NOT re-read or run the file — switch to structural_edit, which " +
			"needs no old_str: call it with path " + mp + ", the selector of the " +
			"node you are changing, and that node's complete replacement as " +
			"content. " + selectorGuidance(mp, source) + "."
	}
	return "edit_file's old_str did not match " +
		mp + " (small drift in whitespace/quotes is enough to miss). " +
		selectorGuidanceOrOutline(mp, source) + ". Use replace_lines with " +
		"the line numbers read_file shows instead of reproducing old_str."
}

// breakErrorLoop counts a failed call and reports whether the run must stop;
// a call that worked clears the streak.
//
// Break error loops: if 3 tool calls fail in a row, stop.
// When the agent has already written/edited a file
// and is now failing on `run_command` (verification noise — no
// TTY for curses, missing toolchain, etc.), a different exit
// message is appropriate so the user isn't told "the file may
// be too large to modify" when their file is, in fact, on disk.
func (loop *agentLoop) breakErrorLoop(turn int, parsed ModelResponse, result *ToolResult) bool {
	ctx := loop.ctx
	if !result.Success {
		return loop.countFailure(turn, parsed, result)
	}
	loop.consecutiveErrors = 0
	// Successful tool call resets the path window — the model
	// is clearly making progress somewhere.
	ctx.RecentFailurePaths = nil
	return false
}

// countFailure counts a failed call against the streak and against the run's
// ceiling, and reports whether the run must stop. A failure of a new kind
// starts a new streak.
func (loop *agentLoop) countFailure(turn int, parsed ModelResponse, result *ToolResult) bool {
	ctx, st := loop.ctx, loop.st
	// A failure that differs in KIND from the last one means the
	// model acted on the previous rejection. Reset the streak:
	// the breaker exists to stop a loop, and three distinct
	// rejections in a row is the opposite of one.
	if class := rejectionClass(result.Error); class != "" && class != ctx.LastRejectionClass {
		if loop.consecutiveErrors > 0 {
			log.Printf("[agent] rejection changed kind at turn %d — resetting the error streak (was %d)", turn, loop.consecutiveErrors)
		}
		loop.consecutiveErrors = 0
		ctx.RecentFailurePaths = nil
		ctx.LastRejectionClass = class
	}
	loop.consecutiveErrors++
	loop.totalFailures++
	// Ceiling. Resetting the streak on a changed rejection kind
	// is what lets a converging model keep going; without an
	// absolute bound it also lets one cycle through failure modes
	// indefinitely. Generous, because the whole point is that
	// legitimate iteration costs several attempts per edit.
	if loop.totalFailures >= maxTotalFailures {
		log.Printf("[agent] breaking: %d failed tool calls this run (ceiling %d) at turn %d (productive=%v)",
			loop.totalFailures, maxTotalFailures, turn, st.madeProductiveChange)
		if st.madeProductiveChange {
			emitTerminal(ctx, st, TerminalStopped, "failure_ceiling",
				unverifiedSummary(true, "The run hit its failed-call ceiling before finishing."))
		} else {
			emitTerminal(ctx, st, TerminalStopped, "failure_ceiling", fmt.Sprintf(
				"Stopped after %d failed tool calls with nothing landing on disk. The per-turn errors above say what was refused each time; the last one is the one to act on.", loop.totalFailures))
		}
		return true
	}
	// Path-aware breaker. Track which file each failure was on; only
	// escalate when 3 consecutive failures share the same path (= truly
	// stuck on one file). 3 fails across DIFFERENT files = grinding
	// through multi-file work, keep going.
	failPath := extractFailurePath(parsed.Name, parsed.Args)
	ctx.RecentFailurePaths = append(ctx.RecentFailurePaths, failPath)
	if len(ctx.RecentFailurePaths) > 3 {
		ctx.RecentFailurePaths = ctx.RecentFailurePaths[len(ctx.RecentFailurePaths)-3:]
	}
	if loop.consecutiveErrors >= 3 {
		return loop.breakSamePathLoop(turn)
	}
	return false
}

// breakSamePathLoop judges three failures in a row: across different files the
// run goes on, a missing named deliverable earns one output-rescue steer, and
// otherwise the run stops (true).
func (loop *agentLoop) breakSamePathLoop(turn int) bool {
	ctx, st := loop.ctx, loop.st
	samePath := stuckOnOnePath(ctx.RecentFailurePaths)
	if !samePath {
		log.Printf("[agent] path-aware breaker: %d consecutive failures across different paths (%v) — continuing, not a stuck loop", loop.consecutiveErrors, logPaths(ctx.RecentFailurePaths))
		// Reset consecutiveErrors so the multi-file grind
		// can keep going. The recent-paths list stays as
		// a rolling window so if subsequent fails DO
		// collapse onto one path, we still catch it.
		loop.consecutiveErrors = 0
	} else if missing := missingExpectedOutputs(ctx, st.expectedOutputs); len(missing) > 0 && !loop.outputRescueUsed {
		// Output-rescue (same as the repeat breaker): looping
		// on failures without ever committing the named
		// deliverable — steer toward it once before stopping.
		loop.outputRescueUsed = true
		loop.consecutiveErrors = 0
		ctx.RecentFailurePaths = nil
		ctx.Messages = append(ctx.Messages, AgentMessage{Role: "user", Content: expectedOutputMissingMessage(missing)})
		log.Printf("[agent] error loop at turn %d but named deliverable(s) %v not on disk — output-rescue steer instead of stopping", turn, logPaths(missing))
	} else {
		log.Printf("[agent] breaking error loop: %d consecutive failures on the same path %q at turn %d (productive=%v)",
			loop.consecutiveErrors, ctx.RecentFailurePaths[0], turn, st.madeProductiveChange)
		if st.madeProductiveChange {
			emitTerminal(ctx, st, TerminalStopped, "same_target_failures",
				"Wrote your changes to disk; couldn't verify them automatically (the verification commands failed). Run them yourself to confirm — they're on disk.")
		} else {
			emitTerminal(ctx, st, TerminalStopped, "same_target_failures",
				"Stopped after 3 tool failures on the same target with no successful changes. Common causes: the file you referenced isn't in the workspace, an empty path argument was passed, or a regex was malformed. Check the per-turn errors above, then try a more specific request (e.g. \"fix snake_game.py at line 95 — the curses bounds are wrong\").")
		}
		return true
	}
	return false
}

// answerToolCall puts the call and its result into the conversation, then the
// notes the next turn needs: the handoff of delivered content, the queued
// correctives, the steers after a failed run, the cross-file notes, the
// nudge after a V3 delivery and the exploration budget.
func (loop *agentLoop) answerToolCall(turn int, response string, parsed ModelResponse, result *ToolResult, steers turnCorrectives) {
	ctx, st := loop.ctx, loop.st
	loop.trackReadOnlyCalls(parsed, result)

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
	// The handoff: a write can succeed and install something else.
	// Decided from the filesystem, once, before anything is said to
	// the model about what it just did.
	superseded, wasSuperseded := deliveredDiffersFromSubmitted(ctx, parsed.Name, parsed.Args, result)
	if wasSuperseded {
		ctx.Messages = append(ctx.Messages, AgentMessage{
			Role:    "user",
			Content: adoptDeliveredContent(ctx, superseded),
		})
		// The lens scored the SUBMITTED bytes, before execution. They
		// are not on disk, so "your last write ... do not re-issue it"
		// is advice about a file that does not exist -- and it was the
		// instruction both adopting sessions acted on. The score is
		// still recorded and still counts toward the history; what is
		// dropped is telling the model to act on it.
		if steers.lens != "" {
			log.Printf("[agent] dropping the lens corrective for %s — it scored content the delivery replaced",
				logPath(superseded.Path))
			steers.lens = ""
		}
	}
	st.queueCorrective(steers.lens)
	st.queueCorrective(steers.repeat)
	st.queueCorrective(steers.reasoning)
	// A background job's outcome is invisible unless the model asks
	// for it, so a server that died on startup reads the same as one
	// serving happily. Surfaced once per job, through the same queue
	// as every other steer.
	st.queueCorrective(finishedBackgroundNote(ctx))
	st.drainCorrectives(ctx)

	steerAfterFailedRun(ctx, parsed, result)
	noteCrossFileCoherence(ctx, turn, parsed, result)
	nudgeAfterV3Delivery(ctx, parsed, result, superseded, wasSuperseded)

	// Exploration budget: after a run of read-only calls, steer the next
	// turn toward a write, or toward the answer (explorationNudge).
	if note := explorationNudge(&loop.consecutiveReads, turn, !answerOnlyRequest(ctx)); note != "" {
		ctx.Messages = append(ctx.Messages, AgentMessage{Role: "user", Content: note})
	}
}

// trackReadOnlyCalls counts read-only calls in a row, for the exploration
// budget, and notes that the model looked at the project.
//
// Track consecutive read-only calls to detect exploration loops.
// outline_file/find_file MUST be here too — otherwise an
// interleaved outline resets the counter and the model
// read→outline→read→outline forever without the breaker firing
// (observed live with a compact reasoning model). Every navigation-only tool counts.
func (loop *agentLoop) trackReadOnlyCalls(parsed ModelResponse, result *ToolResult) {
	st := loop.st
	isReadOnly := parsed.Name == "read_file" ||
		parsed.Name == "outline_file" ||
		parsed.Name == "list_directory" ||
		parsed.Name == "search_files" ||
		parsed.Name == "find_file"
	if isReadOnly {
		loop.consecutiveReads++
		if result.Success {
			// The model went looking at the project, so it read the
			// message as work rather than conversation. Used by the
			// done-without-action gate below to tell "remove the
			// debug logging" (opens the file, writes nothing) apart
			// from "thanks, that looks great" (no tool calls at all).
			st.inspectedWorkspace = true
		}
	} else {
		loop.consecutiveReads = 0
	}
}

// steerAfterFailedRun points the model at the cause of a failed run_command or
// run_background: a broken inline test, the frame of a traceback, a missing
// module, a missing command or a file name that differs only in case.
//
// Option 3 (issue #39): traceback → directed edit. When a
// run_command surfaced a Python traceback, mechanically extract
// the fix site and hand the model a directed instruction ("fix
// function X here") instead of leaving it to localize — the step
// a weak model fails by hallucinating symbols / editing the wrong
// function. The stack frame IS the localization; no LLM reasoning
// needed to read it.
func steerAfterFailedRun(ctx *AgentContext, parsed ModelResponse, result *ToolResult) {
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
}

// noteCrossFileCoherence adds, after a mutation that landed, the session's file
// manifest and the asset-graph lint to the conversation.
//
// Cross-file coherence signals after a successful mutation:
// the session file manifest (so later files reference earlier
// ones instead of re-creating them) and the asset-graph lint
// (orphaned templates/static files, dangling refs). Both are
// advisory [system note]s — never blockers.
func noteCrossFileCoherence(ctx *AgentContext, turn int, parsed ModelResponse, result *ToolResult) {
	if result.Success &&
		(parsed.Name == "write_file" || parsed.Name == "edit_file" ||
			parsed.Name == "structural_edit" || parsed.Name == "move_file" ||
			parsed.Name == "delete_file") {
		if note := sessionManifestNote(ctx); note != "" {
			ctx.Messages = append(ctx.Messages, systemNote(note))
			log.Printf("[agent] session manifest announced (%d files)", len(ctx.SessionWrites))
		}
		if note := assetLintNote(ctx); note != "" {
			ctx.Messages = append(ctx.Messages, systemNote(note))
			ctx.Stream("asset_lint", map[string]interface{}{
				"turn":   turn,
				"detail": note,
			})
			log.Printf("[agent] asset lint: %s", logPath(truncateStr(note, 160)))
		}
	}
}

// nudgeAfterV3Delivery tells the model what V3's delivery of a write or an edit
// established, from the proxy's own evidence.
//
// Say what V3's delivery established, from the proxy's own
// evidence. Compact models otherwise keep grinding after V3
// delivers: re-read the file, edit unrelated functions, run
// another V3 cycle (~110s each), so the run is told where it
// stands. But a phase name is not a run: phase1 can be reached by
// agreement between candidates, or on a compile, with nothing ever
// running the code. The nudge used to say "verified ...
// build-checked ... respond NOW with done" on those too. It now
// says the edit works only when a current run shows it
// (pathCoverageSatisfied, which also reads the evidence V3's
// delivery staged), and otherwise asks for that run.
// "none" is the phase a run reports when nothing passed, and it
// is not the empty string — so this fired on every unverified
// fallback and told the model its code was build-checked when
// no candidate had passed anything. Measured across one
// 28-session run: 0 of 44 candidates passed the sandbox and
// this nudge fired 11 times, each one pushing the model to stop
// working on a file that had failed verification.
func nudgeAfterV3Delivery(ctx *AgentContext, parsed ModelResponse, result *ToolResult, superseded supersededWrite, wasSuperseded bool) {
	if result.Success && result.V3Used && verifiedPhase(result.PhaseSolved) &&
		(parsed.Name == "write_file" || parsed.Name == "edit_file") {
		// Two shapes, because there are two situations. When the
		// delivered bytes ARE the model's own, the original nudge is
		// unchanged. When they are not, telling it not to re-read is
		// telling it not to look at the only copy that matters, and
		// "respond NOW with done" would have it certify content it has
		// never seen. Measured: both sessions that adopted a candidate
		// were sent the second situation's context with the first
		// situation's instruction.
		var landed struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(parsed.Args, &landed)
		supersededRel := ""
		if wasSuperseded {
			supersededRel = superseded.Rel
		}
		nudge, shown := v3DeliveryNudge(ctx, result.PhaseSolved, result.CandidatesTested,
			result.WinningScore, landed.Path, supersededRel)
		ctx.Messages = append(ctx.Messages, AgentMessage{
			Role:    "user",
			Content: nudge,
		})
		// Name what landed, not what was proposed. This line used to
		// print the SUBMITTED args, so a reader of a session where a
		// candidate replaced them saw the superseded content's hash
		// and size described as the thing V3 verified.
		if wasSuperseded {
			log.Printf("[agent] V3 %s delivered its own %s (%dB, not the %dB submitted) — pointing the model at what landed",
				parsed.Name, logPath(superseded.Path),
				len(superseded.Delivered), len(superseded.Submitted))
		} else {
			log.Printf("[agent] V3 delivered %s on %s (shown working: %v)", parsed.Name, safeArgsSummary(parsed.Name, parsed.Args), shown)
		}
	}
}

// askForAKnownResponseType answers a reply of an unknown type with the types
// the model may use.
func askForAKnownResponseType(ctx *AgentContext, parsed ModelResponse) {
	// Unknown type — grammar should prevent this
	ctx.Messages = append(ctx.Messages, AgentMessage{
		Role:    "user",
		Content: fmt.Sprintf("Unknown response type '%s'. Use tool_call, text, or done.", parsed.Type),
	})
}

// endOutOfTurns ends a run that used all its turns, with what the run found.
//
// Running out of turns is not a reason to say nothing. This path used to
// stream an `error` and return, so a user who asked a question and whose
// turn hit the cap saw an empty reply — no answer, no partial, no
// explanation. Observed on a fresh workspace with "how does the contact
// form work?": four searches, cap reached, zero bytes back. Every other
// loop exit authors a summary; this one has to as well.
func (loop *agentLoop) endOutOfTurns() {
	ctx, st := loop.ctx, loop.st
	log.Printf("[agent] max turns (%d) exceeded for %s — returning what the run found", ctx.MaxTurns, ctx.Tier)
	emitTerminal(ctx, st, TerminalIncomplete, "turn_budget_exhausted",
		outOfTurnsSummary(ctx, st.madeProductiveChange)+liveBackgroundJobNote(ctx))
}

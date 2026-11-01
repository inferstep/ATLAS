// The checks a tool call passes before it runs, and the refusal each one gives.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// announceToolCall streams a tool call the moment it parses, before any gate
// judges it, and returns the call's arguments as the model wrote them.
//
// The repetition detector has to judge what the MODEL sent, and
// `parsed.Args` does not stay that. Fenced resolution rewrites it
// with the fetched file body further down, so by the time the
// detector runs it is fingerprinting bytes the model never wrote --
// bytes that differ on every attempt, while the call itself is
// byte-identical each time. That is why the seven-turn @fenced
// loops in the frozen run reached the 600s cap with the detector
// silent: the instrument built to catch exactly that repetition
// was reading the output of the channel that resolved it.
//
// Snapshotting here, before anything can rewrite it, is the whole
// fix. The copy is used for the signature and nothing else: the
// fetched bytes still drive the mutation, the gates, the ledger
// and the write, exactly as before.
func announceToolCall(ctx *AgentContext, turn int, parsed ModelResponse) json.RawMessage {
	intentArgs := append(json.RawMessage(nil), parsed.Args...)
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
	return intentArgs
}

// screenBeforeWorkspaceCheck runs the gates a tool call meets before the
// workspace check: permission (a deletion asks after the check) and arguments
// that do not parse.
func (loop *agentLoop) screenBeforeWorkspaceCheck(turn int, response string, parsed ModelResponse, intentArgs json.RawMessage) turnOutcome {
	// Check permissions. In default and accept-edits modes a
	// destructive tool pauses the loop until the client approves or
	// denies it (via POST /v1/permission). Yolo mode and pre-approved
	// tools short-circuit needsPermission and never reach here. The
	// legacy PermissionFn is still honored for non-interactive callers.
	//
	// Deletion runs this LATER -- see below. It has to be asked after
	// the path is canonicalised and its structured intent is on the
	// record, or a refusal leaves nothing owed and a run whose only
	// act was a refused deletion reports completed.
	if parsed.Name != "delete_file" {
		if outcome := loop.askPermission(turn, response, parsed, intentArgs); outcome != turnContinues {
			return outcome
		}
	}

	return loop.refuseTruncatedArgs(turn, parsed)
}

// refuseOutsideWorkspace answers a call the workspace check refused, counts the
// refusal, and reports whether the run must stop.
func (loop *agentLoop) refuseOutsideWorkspace(parsed ModelResponse, intentArgs json.RawMessage, rejection string) bool {
	ctx, st := loop.ctx, loop.st
	st.bounceToolCall(ctx, parsed.Name, rejection)
	// A refusal here is a failed call, and this branch returned
	// before everything that counts one -- including the only
	// reader of the counter it incremented. It is the fourth
	// early-refusal branch with that shape, after the per-path
	// ban, the retry ban and the fenced bounce.
	//
	// Measured: a session whose workspace root did not exist
	// refused the same byte-identical find_file 60 times over its
	// whole budget, with no intervention, no log line and no
	// terminal of its own.
	if loop.accountRefusedCall(parsed.Name, intentArgs, rejection,
		workspaceRefusalPath(ctx, parsed.Name, parsed.Args)) {
		return true
	}
	return false
}

// screenAfterWorkspaceCheck runs what follows the workspace check, before any
// path in the call is read: the record of the intent, the deletion handshake,
// the run-first gate for edits and the write_file preflight. The preflight may
// put a fetched body in place of a fenced sentinel; nothing else here changes
// *parsed.
func (loop *agentLoop) screenAfterWorkspaceCheck(turn int, response string, parsed *ModelResponse, intentArgs json.RawMessage) turnOutcome {
	ctx, st := loop.ctx, loop.st
	// The intent is on the record from here: the arguments parse, the
	// path is inside the workspace, and for every tool but delete_file
	// permission was granted (deletion asks just below). Every later
	// refusal -- a gate, a failed fenced resolution, a failed write --
	// leaves the debt standing, so these pre-dispatch failures do not
	// disappear from the record.
	noteMutationIntent(ctx, st, parsed.Name, parsed.Args)

	// Deletion asks here, not with the others. By this point the path
	// has parsed, canonicalised and cleared the workspace boundary,
	// and the structured intent is recorded -- so a refusal, a denial,
	// a timeout or a cancel all leave the debt standing and the run
	// cannot call itself finished. Asking earlier meant a refused
	// deletion owed nothing: a session whose only act was trying to
	// remove a non-empty directory reported completed.
	if parsed.Name == "delete_file" {
		if outcome := loop.askPermission(turn, response, *parsed, intentArgs); outcome != turnContinues {
			return outcome
		}
	}

	if outcome := loop.runFirstGateForEdits(*parsed); outcome != turnContinues {
		return outcome
	}
	return loop.prepareWriteFile(turn, parsed, intentArgs)
}

// askPermission asks whether this tool call may run, when it needs asking. A
// denial is a failed call like any other: it is answered on the stream and
// counted, and the turn ends; three in a row on one target end the run.
func (loop *agentLoop) askPermission(turn int, response string, parsed ModelResponse, intentArgs json.RawMessage) turnOutcome {
	ctx, st := loop.ctx, loop.st
	if !needsPermission(ctx, parsed.Name, parsed.Args) {
		return turnContinues
	}
	{
		allowed := true
		// A deletion always goes through the interactive handshake.
		// PermissionFn is a programmatic approver -- yolo installs one
		// that says yes to everything -- and a function returning true
		// is not a user deciding about a file. Routing deletion around
		// it is what makes the yolo bypass actually closed rather than
		// closed-looking: needsPermission alone would just hand the
		// call to that function.
		if ctx.PermissionFn != nil && parsed.Name != "delete_file" {
			allowed = ctx.PermissionFn(parsed.Name, parsed.Args)
		} else {
			allowed = awaitPermission(ctx, parsed.Name, permCallID(turn), parsed.Args)
		}
		if !allowed {
			denial := permissionDenial(ctx, permCallID(turn))
			ctx.Stream("permission_denied",
				permissionDeniedEvent(parsed.Name, denial))
			// A refusal here is a failed call like any other, and this
			// branch returned before everything that counts one. It
			// matters more now that a deletion always reaches the
			// handshake: a model repeating a delete of a path the
			// preflight refuses gets denied every time, and without
			// accounting it repeats until the turn cap -- measured at
			// 21 turns with no terminal of ATLAS's own.
			if loop.accountRefusedCall(parsed.Name, intentArgs,
				denial,
				workspaceRefusalPath(ctx, parsed.Name, parsed.Args)) {
				return runEnds
			}
			// A denied call still produced a result the model reads,
			// so it owes the stream one too. Without this the run
			// emits more tool_calls than tool_results, which every
			// balance check treats as a dropped call -- invisible
			// until deletion started always routing through here.
			// The call is answered, so it is no longer pending; the
			// terminal must not flush a second "not run" result for it.
			st.pendingToolCall = ""
			ctx.Stream("tool_result", map[string]interface{}{
				"tool":    parsed.Name,
				"success": false,
				"data":    json.RawMessage("null"),
				"error":   denial,
				"elapsed": "0s",
			})
			// Bespoke bounce: the permission flow keys its tool-call
			// ID via permCallID so the TUI can match the decision.
			ctx.Messages = append(ctx.Messages, AgentMessage{
				Role:    "assistant",
				Content: response,
			})
			ctx.Messages = append(ctx.Messages, AgentMessage{
				Role:       "tool",
				Content:    deniedToolMessage(denial),
				ToolCallID: permCallID(turn),
				ToolName:   parsed.Name,
			})
			return turnEnds
		}
		return turnContinues
	}
}

// refuseTruncatedArgs refuses a write_file, edit_file or run_command whose
// arguments do not parse, before anything runs. Three in a row end the run.
//
// Fix C: Detect truncated args BEFORE execution.
// If the args JSON doesn't parse, don't attempt execution —
// tell the model to use smaller edits instead.
func (loop *agentLoop) refuseTruncatedArgs(turn int, parsed ModelResponse) turnOutcome {
	ctx, st := loop.ctx, loop.st
	if parsed.Name == "write_file" || parsed.Name == "edit_file" || parsed.Name == "run_command" {
		var testParse map[string]interface{}
		if err := json.Unmarshal(parsed.Args, &testParse); err != nil {
			log.Printf("[agent] truncated args detected for %s at turn %d", parsed.Name, turn)
			st.bounceToolCall(ctx, parsed.Name, "Your output was truncated — the content is too long for a single tool call. For existing files, use edit_file with small targeted changes (replace specific functions or sections). For new files, keep them under 100 lines per write_file call.")
			loop.consecutiveErrors++
			if loop.consecutiveErrors >= 3 {
				loop.endStream(TerminalStopped, "oversized_tool_content",
					"Stopped: content too large for tool calls. Try requesting smaller, targeted changes.")
				return runEnds
			}
			return turnEnds
		}
	}
	return turnContinues
}

// runFirstGateForEdits holds back an edit of a file whose warned version on
// disk has never been run, within the gate's bounce budget.
//
// Surgical-edit gate (applied below, after fenced resolution):
// write_file is for creating files; edits to an existing file must use
// edit_file with old_str/new_str, because a whole-file rewrite, even of
// a short template, can destroy the original. Files of 5 lines or fewer
// (e.g. a single-line config) may be overwritten: at that size there is
// no edit-vs-rewrite distinction. Session-owned and corrupted files are
// carved out there.
func (loop *agentLoop) runFirstGateForEdits(parsed ModelResponse) turnOutcome {
	ctx, st := loop.ctx, loop.st
	if parsed.Name == "edit_file" || parsed.Name == "structural_edit" ||
		parsed.Name == "insert_after" || parsed.Name == "replace_lines" {
		var ed struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(parsed.Args, &ed) == nil && st.pendingWarnedRun[ed.Path] &&
			st.chargeBounce("run_first_gate") {
			log.Printf("[agent] run-first gate (%s): %s has a warned, unexecuted version on disk (bounce %d/%d)",
				parsed.Name, ed.Path, st.gateBounces["run_first_gate"], maxGateBounces)
			st.bounceToolCall(ctx, parsed.Name, fmt.Sprintf(
				"The version of %s on disk carries a parse warning and has never been run. Run it first — %s — before editing further.",
				ed.Path, runFirstInstruction(ctx, ed.Path)))
			return turnEnds
		}
	}
	return turnContinues
}

// prepareWriteFile runs the write_file preflight: the run-first gate, then the
// resolution of a fenced body.
//
// write_file preflight. Order matters: the run-first gate is
// checked BEFORE fenced resolution, because "@fenced" resolution
// costs a full model sub-call — paying that for a write the gate
// is about to bounce is pure waste (audit finding: the gate sat
// after resolution and every bounced warned-file rewrite burned
// a generation first).
func (loop *agentLoop) prepareWriteFile(turn int, parsed *ModelResponse, intentArgs json.RawMessage) turnOutcome {
	if parsed.Name == "write_file" {
		var wfInput WriteFileInput
		if json.Unmarshal(parsed.Args, &wfInput) == nil {
			if outcome := loop.runFirstGateForWrite(wfInput); outcome != turnContinues {
				return outcome
			}
			return loop.resolveFencedContent(turn, parsed, intentArgs, wfInput)
		}
	}
	return turnContinues
}

// runFirstGateForWrite holds back a write_file over a file whose warned version
// on disk has never been run: the first recurrence is shown the file as it is,
// later ones are told to run it first, within the gate's bounce budget.
func (loop *agentLoop) runFirstGateForWrite(wfInput WriteFileInput) turnOutcome {
	ctx, st := loop.ctx, loop.st
	if st.pendingWarnedRun[wfInput.Path] {
		// The gate has already said this once. Saying it again
		// while the same call stays available is the C5 shape:
		// in the frozen run the demand repeated until its
		// bounce budget ran out and the identical writes
		// resumed. On the recurrence the model gets what it
		// has been unable to get for itself -- the file as it
		// actually is -- and the call that made no progress is
		// held back until it changes.
		if msg := fencedRunFirstRecovery(ctx, st, wfInput.Path, wfInput.Content); msg != "" {
			st.bounceToolCall(ctx, "write_file", msg)
			return turnEnds
		}
		if st.chargeBounce("run_first_gate") {
			log.Printf("[agent] run-first gate: %s has a warned, unexecuted version on disk (bounce %d/%d)",
				wfInput.Path, st.gateBounces["run_first_gate"], maxGateBounces)
			st.bounceToolCall(ctx, "write_file", fmt.Sprintf(
				"The version of %s you wrote is on disk with a parse warning and has never been run. Run it first — %s — before writing again. A rewrite that has not seen the runtime error is a guess.",
				wfInput.Path, runFirstInstruction(ctx, wfInput.Path)))
			return turnEnds
		}
	}
	return turnContinues
}

// resolveFencedContent sees to a write_file whose content is the fenced
// sentinel. The channel opens only for a call that can execute, and a spent
// channel is offered its way out once, before any resolution.
func (loop *agentLoop) resolveFencedContent(turn int, parsed *ModelResponse, intentArgs json.RawMessage, wfInput WriteFileInput) turnOutcome {
	ctx, st := loop.ctx, loop.st
	// Fenced-content resolution: everything downstream (the
	// remaining gates, tier classification, execution) must see
	// real bytes. "@fenced" is the model routing the file body
	// around the JSON channel — one unconstrained sub-call
	// fetches it in a fenced block, its native emission format.
	// See fetchFencedContent.
	trimmed := strings.TrimSpace(wfInput.Content)
	// A call that cannot execute must not open the channel.
	// Falling through leaves the sentinel in `content` and
	// hands the call to the tool, which refuses it with the
	// same check that refused it here -- so the model gets the
	// authoritative message, the ledger sees MutationNone, and
	// the session spends one turn instead of a generation.
	fencedUsable, fencedWhy := fencedCallIsExecutable("write_file", parsed.Args, ctx)
	if !fencedUsable {
		log.Printf("[agent] not opening the fenced channel for an unusable write_file call: %s", fencedWhy)
	}
	// The channel can be spent while the model keeps asking
	// for it. Offer the way out ONCE, before anything tries
	// another resolution, so the turn costs no generation.
	if fencedUsable && isFencedSentinel(trimmed) {
		if msg := fencedChannelRecovery(ctx, st, wfInput.Path); msg != "" {
			st.bounceToolCall(ctx, "write_file", msg)
			return turnEnds
		}
	}
	if fencedUsable && isFencedSentinel(trimmed) {
		return loop.replaceFencedSentinel(turn, parsed, intentArgs, wfInput, trimmed)
	}
	return turnContinues
}

// replaceFencedSentinel puts the file body in place of the sentinel: a body
// that arrived inline and proves its own completion, or else one a fenced
// sub-call fetches. The body becomes the call's arguments. A fetch that fails
// is refused and counted.
func (loop *agentLoop) replaceFencedSentinel(turn int, parsed *ModelResponse, intentArgs json.RawMessage, wfInput WriteFileInput, trimmed string) turnOutcome {
	ctx := loop.ctx
	// Anything after the sentinel is the model inlining
	// the file anyway. Exactly one of two things arrived,
	// and only one of them is a file:
	//
	//   * a complete body, bare or in its own fence — use it
	//   * a body cut off mid-emission, which is precisely
	//     what the JSON channel does and the whole reason
	//     @fenced exists. Trusting that wrote truncated
	//     programs to disk: `@fenced\n```python\nprint(`
	//     had its fence line removed by the sanitizer and
	//     landed as a one-line `print(`, so the session
	//     shipped a file that could not parse and then
	//     repeated the identical write for five more turns.
	//     Measured at 6 of 20 create sessions.
	//
	// Only protocol framing decides. An unterminated
	// fence is one incomplete shape; a BARE body with no
	// fence at all is the other, and it was accepted
	// unconditionally until now. Across the sealed
	// Stage-A run every inlined body was bare, so the
	// unterminated check never fired once and the
	// unframed case wrote 1106 truncated bytes that
	// parsed cleanly. See classifyFencedPayload.
	inline := strings.TrimLeft(strings.TrimPrefix(trimmed, "@fenced"), " \t\r\n")
	resolved, framed, why := resolveInlineFencedBody(inline)
	if framed {
		inline = resolved
	} else {
		log.Printf("[agent] inline @fenced body for %s does not prove completion (%s) — falling back to the sub-call", wfInput.Path, why)
		inline = ""
	}
	if inline != "" {
		wfInput.Content = inline
		log.Printf("[agent] fenced sentinel stripped for %s (%d bytes arrived inline)", wfInput.Path, len(inline))
	} else {
		stallsBefore := ctx.FencedStalls
		fetched, ferr := fetchFencedContent(ctx, rawResponseForFence(*parsed), wfInput.Path)
		if ferr != nil {
			return loop.refuseFencedFetch(turn, *parsed, intentArgs, wfInput, ferr, stallsBefore)
		}
		wfInput.Content = fetched
		log.Printf("[agent] fenced content resolved for %s (%d bytes via sub-call)", wfInput.Path, len(fetched))
	}
	if rebuilt, merr := json.Marshal(wfInput); merr == nil {
		parsed.Args = rebuilt
	}
	return turnContinues
}

// refuseFencedFetch answers a fenced fetch that failed, with the reason that
// is true for it, and counts the refusal unless the run itself stopped. The
// turn ends; three failures in a row on one file end the run.
func (loop *agentLoop) refuseFencedFetch(turn int, parsed ModelResponse, intentArgs json.RawMessage, wfInput WriteFileInput, ferr error, stallsBefore int) turnOutcome {
	ctx, st := loop.ctx, loop.st
	log.Printf("[agent] fenced-content fetch failed for %s: %v", wfInput.Path, ferr)
	fencedBounce := "You wrote \"content\": \"@fenced\" but no fenced block followed. Either reply with the complete file in one fenced code block when asked, or re-issue write_file with the full content inline."
	if ctx.Ctx != nil && ctx.Ctx.Err() != nil {
		// The session stopped while the file was being fetched: its
		// time ran out, or it was cancelled. The model sent nothing
		// wrong, and "no fenced block followed" would be false: the
		// block may already have arrived.
		why := "the session was cancelled"
		if errors.Is(ctx.Ctx.Err(), context.DeadlineExceeded) {
			why = "the session ran out of time"
		}
		fencedBounce = fmt.Sprintf("write_file for %s was not applied: %s while its content was being fetched. Nothing was written.",
			wfInput.Path, why)
	} else if !fencedFitsRemainingBudget(ctx) {
		// Refused before any fetch: too little session
		// time is left to fetch the file and check it.
		fencedBounce = fmt.Sprintf("write_file for %s was not applied: too little session time is left to fetch its content and check it. Nothing was written. Send the complete file inline, or make a smaller change with edit_file or structural_edit.",
			wfInput.Path)
	} else if fencedChannelDisabledForSession(ctx) {
		// The channel is off for the rest of the run;
		// telling the model to try fenced again would
		// only stall. Steer it to inline, the safe path.
		// Say where the stall was: this fetch's own
		// stall used to be reported as "earlier in
		// this run" (#254).
		when := "stalled earlier in this run"
		if ctx.FencedStalls > stallsBefore {
			when = "stalled on this file just now (it sent no content)"
		}
		fencedBounce = fmt.Sprintf(
			"The fenced-content channel %s and is now off for the rest of the session — a fenced sub-call would only stall again. Re-issue write_file for %s with the COMPLETE file inline in the content field (write any inner double-quote as \\\"), or make a targeted change with edit_file or structural_edit.",
			when, wfInput.Path)
	}
	st.bounceToolCall(ctx, "write_file", fencedBounce)
	// A refusal here is a failed call like any
	// other, and this branch returned before every
	// mechanism that counts one. The same defect
	// was found at the per-path ban and at the
	// retry ban; this is the third instance, and
	// the one the frozen run paid for: debounce2
	// re-sent this call 147 times over 570s with
	// the allowance correctly refusing all but
	// four generations and nothing bounding the
	// TURNS.
	//
	// A run that ended because the client left, or
	// because the work deadline fired, is not a
	// model repeating itself, and those paths keep
	// their own terminals.
	if ctx.Ctx == nil || ctx.Ctx.Err() == nil {
		// The intent, not the resolved args: this
		// call is byte-identical every time the
		// model sends it, and the resolved body is
		// what made it look different.
		recordFailedToolCall(ctx, parsed.Name, intentArgs, ferr.Error())
		loop.consecutiveErrors++
		loop.totalFailures++
		failPath := fencedKey(ctx, wfInput.Path)
		ctx.RecentFailurePaths = appendRecentFailurePath(ctx.RecentFailurePaths, failPath)
		if shouldStopForFailures(loop.totalFailures, loop.consecutiveErrors, ctx.RecentFailurePaths) {
			log.Printf("[agent] breaking at turn %d: %d refused/failed calls, %d consecutive on %q",
				turn, loop.totalFailures, loop.consecutiveErrors, failPath)
			loop.endStream(TerminalStopped, "repeated_refusal",
				repeatedRefusalSummary("write_file", wfInput.Path, st.madeProductiveChange)+
					liveBackgroundJobNote(ctx))
			return runEnds
		}
	}
	return turnEnds
}

// guardExistingFileWrite applies the existing-file guards to a write_file,
// given what the read of its target found: a target this session never read
// is refused, and so is a whole-file rewrite of a file over five lines that is
// neither corrupted nor this session's own.
func (loop *agentLoop) guardExistingFileWrite(parsed ModelResponse, intentArgs json.RawMessage, wfInput WriteFileInput,
	existingPath string, existing []byte, readErr error) turnOutcome {
	ctx := loop.ctx
	if readErr != nil && !os.IsNotExist(readErr) {
		// Every existing-file protection lives inside the
		// success branch below, so a read that fails for any
		// reason other than "the file is genuinely new" silently
		// disarms all of them and the write lands unguarded.
		// A ~100-line file was replaced by three lines this way
		// with no guard log at all, and the guard has never once
		// fired in a full session log.
		log.Printf("[agent] write_file pre-check could not read %q (resolved %q): %v — existing-file guards are NOT applied to this write",
			wfInput.Path, existingPath, readErr)
	}
	if readErr == nil {
		existingLines := strings.Count(string(existing), "\n") + 1
		// Exempt corrupted files. If the existing file looks like it
		// has prose preamble or stray markdown fences
		// (sanitizeFileContent would change it), the only way to clean
		// it up is full replacement: edit_file can't express "remove
		// these specific corrupted lines" cleanly, and a model can loop
		// on old_str = new_str. Allow write_file in that case and log
		// the self-heal. Self-iteration carveout: if this session wrote
		// the file itself (it's not the user's code, it's the agent's
		// own draft), allow overwriting. Otherwise the agent can't
		// correct its own first-pass mistakes, such as a stub app.py
		// that turns out to need more wiring.
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
			return loop.refuseUnreadOverwrite(parsed, intentArgs, wfInput, existingPath, existingLines)
		}
		if existingLines > 5 && !corrupted && !sessionOwned {
			return loop.refuseWholeFileRewrite(parsed, intentArgs, wfInput, existingPath, existingLines)
		}
		if existingLines > 5 {
			// Name the actual carveout: the corrupted-file message on a
			// session-owned overwrite sends a loop diagnosis down the
			// wrong path.
			if corrupted {
				log.Printf("[agent] allowing write_file on corrupted %s (%d lines, sanitizer would clean it)", wfInput.Path, existingLines)
			} else {
				log.Printf("[agent] allowing write_file on session-owned %s (%d lines, self-iteration carveout)", wfInput.Path, existingLines)
			}
		}
	}
	return turnContinues
}

// refuseUnreadOverwrite refuses a write_file over an existing file this
// session never read, and counts the refusal.
func (loop *agentLoop) refuseUnreadOverwrite(parsed ModelResponse, intentArgs json.RawMessage, wfInput WriteFileInput,
	existingPath string, existingLines int) turnOutcome {
	ctx, st := loop.ctx, loop.st
	rejection := fmt.Sprintf(
		"%s already exists and this session has not read it. Use read_file first: if it holds input or configuration you were given, you need its real contents, not a replacement. If you have read it and still mean to replace the whole file, use edit_file or structural_edit.",
		wfInput.Path)
	log.Printf("[agent] rejecting write_file over unread existing %q (%d lines)", wfInput.Path, existingLines)
	if r := steerRecovery(ctx, st, wfInput.Path, existingPath, true); r != "" {
		rejection = r
	}
	st.bounceToolCall(ctx, "write_file", rejection)
	// Steering, not a verdict on the work: the model
	// is being sent to a better tool. But a model that
	// ignores the steer repeats the same refused write
	// forever -- measured at 31 turns in a healthy
	// workspace with no terminal of ATLAS's own -- so
	// the ignored steer is accounted like any other
	// refused call. A model that follows it pays
	// nothing further: the next call is a different
	// action on the path and clears the state.
	if loop.accountRefusedCall("write_file", intentArgs, rejection,
		workspaceRefusalPath(ctx, "write_file", parsed.Args)) {
		return runEnds
	}
	return turnEnds
}

// refuseWholeFileRewrite refuses a write_file that rewrites an existing file of
// more than five lines, points the model at a targeted edit, and counts the
// refusal.
func (loop *agentLoop) refuseWholeFileRewrite(parsed ModelResponse, intentArgs json.RawMessage, wfInput WriteFileInput,
	existingPath string, existingLines int) turnOutcome {
	ctx, st := loop.ctx, loop.st
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
	if r := steerRecovery(ctx, st, wfInput.Path, existingPath, false); r != "" {
		rejection = r
	}
	st.bounceToolCall(ctx, "write_file", rejection)
	// Same shape, same bound as the unread-overwrite
	// steer above.
	if loop.accountRefusedCall("write_file", intentArgs, rejection,
		workspaceRefusalPath(ctx, "write_file", parsed.Args)) {
		return runEnds
	}
	return turnEnds
}

// refuseBeforeDispatch runs the last gates before a tool call executes: the
// shell gates, the runaway-write count, the per-file tool ban, the answers to
// a repeated rejected proposal or a repeated no-op edit, and the identical
// re-send ban. It reports whether this call is a runaway write, and the outcome.
func (loop *agentLoop) refuseBeforeDispatch(turn int, parsed ModelResponse, intentArgs json.RawMessage) (bool, turnOutcome) {
	if outcome := loop.redirectForegroundServer(parsed, intentArgs); outcome != turnContinues {
		return false, outcome
	}
	if outcome := loop.refuseUnsafeRunCommand(parsed, intentArgs); outcome != turnContinues {
		return false, outcome
	}
	if outcome := loop.refuseUnsafeRunBackground(parsed, intentArgs); outcome != turnContinues {
		return false, outcome
	}
	runawayWrite := loop.countWholeFileWrites(parsed)
	if outcome := loop.refuseBannedTool(turn, parsed); outcome != turnContinues {
		return runawayWrite, outcome
	}
	if outcome := loop.answerRejectedProposal(parsed, intentArgs); outcome != turnContinues {
		return runawayWrite, outcome
	}
	if outcome := loop.answerBrokenArtifact(parsed, intentArgs); outcome != turnContinues {
		return runawayWrite, outcome
	}
	return runawayWrite, loop.refuseIdenticalRetry(turn, parsed, intentArgs)
}

// redirectForegroundServer refuses a run_command that would start a server in
// the foreground and points the model at run_background.
//
// The foreground-server redirect runs in EVERY mode. Yolo opts out
// of the shell-mutation and working-dir gates — those are
// permission questions, and yolo is the user saying don't ask.
// Starting a server in the foreground is not a permission
// question: it burns the sandbox timeout and reports a failure
// that says nothing about the code, whatever mode you are in.
func (loop *agentLoop) redirectForegroundServer(parsed ModelResponse, intentArgs json.RawMessage) turnOutcome {
	ctx, st := loop.ctx, loop.st
	if parsed.Name == "run_command" {
		var rc RunCommandInput
		if json.Unmarshal(parsed.Args, &rc) == nil {
			if rejection := foregroundServerRejectionWithSource(rc.Command,
				workspaceFileReader(ctx)); rejection != "" {
				log.Printf("[agent] redirecting a foreground server start to run_background: %q",
					truncateStr(rc.Command, 80))
				st.bounceToolCall(ctx, "run_command", rejection)
				// Counted like every other refusal, so a model that
				// re-sends it meets the identical-retry refusal and the
				// failure bounds. Measured: 20 identical re-sends, each
				// bounced, until the session deadline.
				if loop.accountRefusedCall(parsed.Name, intentArgs, rejection,
					workspaceRefusalPath(ctx, parsed.Name, parsed.Args)) {
					return runEnds
				}
				return turnEnds
			}
		}
	}
	return turnContinues
}

// refuseUnsafeRunCommand refuses a run_command the shell validator rejects.
func (loop *agentLoop) refuseUnsafeRunCommand(parsed ModelResponse, intentArgs json.RawMessage) turnOutcome {
	ctx, st := loop.ctx, loop.st
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
				if loop.accountRefusedCall(parsed.Name, intentArgs, rejection,
					workspaceRefusalPath(ctx, parsed.Name, parsed.Args)) {
					return runEnds
				}
				return turnEnds
			}
		}
	}
	return turnContinues
}

// refuseUnsafeRunBackground refuses a run_background the shell validator
// rejects.
//
// Same shell-validation + working-dir gate for run_background; without
// it a command such as
// `run_background "cd /workspace && python app.py"` passes while the
// same run_command is bounced. validateRunCommand chains both gates so
// destructive shell verbs and /workspace drift get the same treatment
// regardless of which run_* tool the model picks.
func (loop *agentLoop) refuseUnsafeRunBackground(parsed ModelResponse, intentArgs json.RawMessage) turnOutcome {
	ctx, st := loop.ctx, loop.st
	if parsed.Name == "run_background" && !ctx.YoloMode {
		var rb RunBackgroundInput
		if json.Unmarshal(parsed.Args, &rb) == nil {
			if rejection := validateRunCommand(rb.Command, ctx.WorkingDir); rejection != "" {
				log.Printf("[agent] rejecting run_background %q: %q",
					truncateStr(rb.Command, 80), rejection)
				st.bounceToolCall(ctx, "run_background", rejection)
				if loop.accountRefusedCall(parsed.Name, intentArgs, rejection,
					workspaceRefusalPath(ctx, parsed.Name, parsed.Args)) {
					return runEnds
				}
				return turnEnds
			}
		}
	}
	return turnContinues
}

// countWholeFileWrites counts each write_file per path, and reports whether
// this one is the write that reaches the runaway threshold.
//
// Runaway backstop (#147 review #14): count writes per path and
// force the repeat path once a single file is rewritten far more
// than any real iteration would.
func (loop *agentLoop) countWholeFileWrites(parsed ModelResponse) bool {
	const runawayWriteThreshold = 20
	runawayWrite := false
	if parsed.Name == "write_file" {
		if wp := writeFilePath(parsed.Args); wp != "" {
			loop.writeCountByPath[wp]++
			if loop.writeCountByPath[wp] == runawayWriteThreshold {
				runawayWrite = true
				log.Printf("[agent] runaway write backstop: %q rewritten %d times — escalating", wp, loop.writeCountByPath[wp])
			}
		}
	}
	return runawayWrite
}

// refuseBannedTool refuses a tool the loop has taken away for this file.
//
// A tool the loop has taken away for this file stays taken away.
// Enforced before the identical-resend check so a model that
// varies one byte cannot walk around the ban.
func (loop *agentLoop) refuseBannedTool(turn int, parsed ModelResponse) turnOutcome {
	ctx, st := loop.ctx, loop.st
	if p := extractFailurePath(parsed.Name, parsed.Args); p != "" &&
		st.toolBanned[parsed.Name+"\x00"+p] {
		log.Printf("[agent] turn=%d blocked %s on %s (tool withdrawn for this file)", turn, parsed.Name, p)
		st.bounceToolCall(ctx, parsed.Name, toolBanNoteFor(ctx, parsed.Name, p))
		loop.consecutiveErrors++
		loop.totalFailures++
		// A bounce off the ban is a failure like any other. This
		// branch incremented the counters and then continued without
		// reading them, so the ceiling could not end a session that
		// only ever bounced here — measured at 19 and 85 bounces.
		ctx.RecentFailurePaths = appendRecentFailurePath(ctx.RecentFailurePaths, p)
		if shouldStopForFailures(loop.totalFailures, loop.consecutiveErrors, ctx.RecentFailurePaths) {
			log.Printf("[agent] breaking at turn %d: %d refused/failed calls, %d consecutive on %q",
				turn, loop.totalFailures, loop.consecutiveErrors, p)
			loop.endStream(TerminalStopped, "repeated_refusal",
				repeatedRefusalSummary(parsed.Name, p, st.madeProductiveChange)+liveBackgroundJobNote(ctx))
			return runEnds
		}
		return turnEnds
	}
	return turnContinues
}

// answerRejectedProposal finds a replacement that was already refused against
// the bytes still on disk, and answers it before the re-send ban can.
//
// C4: this exact replacement was already refused against the
// bytes still on disk. It has to be answered BEFORE the resend
// ban, which would otherwise end the run with the model never
// having been shown the file it keeps trying to replace.
func (loop *agentLoop) answerRejectedProposal(parsed ModelResponse, intentArgs json.RawMessage) turnOutcome {
	ctx, st := loop.ctx, loop.st
	if sha := resolvedProposalHash(parsed.Name, parsed.Args); sha != "" {
		rel := ledgerArgPath(parsed.Args, "path")
		if canon, diskHash := survivingKnownGood(ctx, rel); canon != "" {
			if ev := st.c4Rejected[canon]; ev != nil && ev.diskHash == diskHash {
				if _, already := ev.diagnostics[sha]; already {
					return loop.refuseRepeatedProposal(parsed, intentArgs, rel, canon, sha)
				}
			}
		}
	}
	return turnContinues
}

// refuseRepeatedProposal shows the model the file its repeated replacement was
// refused against, and counts the refusal. It goes on when there is nothing
// to show.
func (loop *agentLoop) refuseRepeatedProposal(parsed ModelResponse, intentArgs json.RawMessage, rel, canon, sha string) turnOutcome {
	ctx, st := loop.ctx, loop.st
	if msg := rejectedProposalRecovery(ctx, st, rel, canon, sha); msg != "" {
		st.bounceToolCall(ctx, parsed.Name, msg)
		if loop.accountRefusedCall(parsed.Name, intentArgs, msg,
			workspaceRefusalPath(ctx, parsed.Name, parsed.Args)) {
			return runEnds
		}
		return turnEnds
	}
	return turnContinues
}

// answerBrokenArtifact answers a no-op edit over a file already shown to be
// broken, before the re-send ban can.
//
// C3: the no-op edit over an artifact already shown to be broken.
// This has to come BEFORE the identical-resend ban, which would
// otherwise intercept the recurrence and end the run with the
// broken file still on disk -- the retained shape exactly.
func (loop *agentLoop) answerBrokenArtifact(parsed ModelResponse, intentArgs json.RawMessage) turnOutcome {
	ctx, st := loop.ctx, loop.st
	if relPath := noopEditIntent(parsed.Name, intentArgs); relPath != "" {
		if msg := brokenArtifactRecovery(ctx, st, relPath); msg != "" {
			st.bounceToolCall(ctx, parsed.Name, msg)
			// Counted once, here. The call never reaches the tool, so
			// no post-execution accounting runs for it, and the ban
			// below is skipped by the continue.
			if loop.accountRefusedCall(parsed.Name, intentArgs, msg,
				workspaceRefusalPath(ctx, parsed.Name, parsed.Args)) {
				return runEnds
			}
			return turnEnds
		}
	}
	return turnContinues
}

// refuseIdenticalRetry refuses an exact re-send of a call already rejected,
// takes the tool away for that file, and counts the refusal.
//
// Refuse an exact re-send of an already-rejected call before it
// executes. Checked ahead of the repetition detector because that
// one needs three occurrences and only steers the NEXT turn,
// which a two-turn identical pair never reaches.
// Same representation the repeat detector uses, for the same
// reason: a fenced re-send is byte-identical as the model wrote
// it and different only in the body the channel fetched for it.
// The lookup, the record and the clear all key on the intent, or
// they key on three different things and never meet.
func (loop *agentLoop) refuseIdenticalRetry(turn int, parsed ModelResponse, intentArgs json.RawMessage) turnOutcome {
	ctx, st := loop.ctx, loop.st
	if refusal := identicalRetryRefusal(ctx, parsed.Name,
		retryIdentityArgs(parsed.Name, intentArgs, parsed.Args)); refusal != "" {
		log.Printf("[agent] turn=%d refusing an identical re-send of a rejected %s", turn, parsed.Name)
		// Escalate from refusing THIS call to removing the tool for
		// THIS file. The model has now sent the same rejected call
		// twice, so a third refusal buys another identical turn.
		if p := extractFailurePath(parsed.Name, parsed.Args); p != "" {
			if st.toolBanned == nil {
				st.toolBanned = map[string]bool{}
			}
			key := parsed.Name + "\x00" + p
			if !st.toolBanned[key] {
				st.toolBanned[key] = true
				log.Printf("[agent] %s is now unavailable for %s — identical rejected call re-sent", parsed.Name, p)
			}
			refusal += " " + toolBanNoteFor(ctx, parsed.Name, p)
		}
		st.bounceToolCall(ctx, parsed.Name, refusal)
		// A refusal is a failure and has to count as one. Skipping
		// the counters would leave a model that spams one rejected
		// call running to the turn cap with nothing to stop it — the
		// refusal returns before both the repetition window and the
		// error counter, so neither breaker would ever see it.
		loop.consecutiveErrors++
		loop.totalFailures++
		failPath := extractFailurePath(parsed.Name, parsed.Args)
		ctx.RecentFailurePaths = appendRecentFailurePath(ctx.RecentFailurePaths, failPath)
		// ...and the same stopping rules have to apply here. Both the
		// ceiling and the path-aware breaker live inside the
		// post-execution failure branch, which this path skips, so
		// incrementing alone left the counters with no reader.
		if shouldStopForFailures(loop.totalFailures, loop.consecutiveErrors, ctx.RecentFailurePaths) {
			log.Printf("[agent] breaking at turn %d: %d refused/failed calls, %d consecutive on %q",
				turn, loop.totalFailures, loop.consecutiveErrors, ctx.RecentFailurePaths[len(ctx.RecentFailurePaths)-1])
			loop.endStream(TerminalStopped, "repeated_refusal",
				repeatedRefusalSummary(parsed.Name, failPath, st.madeProductiveChange)+liveBackgroundJobNote(ctx))
			return runEnds
		}
		return turnEnds
	}
	return turnContinues
}

// needsPermission returns true if the tool call requires user confirmation.
func needsPermission(ctx *AgentContext, toolName string, args json.RawMessage) bool {
	// Deleting is decided per object, so no blanket answer substitutes for it.
	// yolo, the yolo flag and session_allowed_tools all answer "may this TOOL
	// run", which is a different question from "may this file be removed", and
	// a session that cannot ask therefore cannot delete. That is the intended
	// cost: the alternative is an unattended run destroying a file nobody
	// approved. Every other tool keeps its existing semantics.
	if toolName == "delete_file" {
		return true
	}
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
	if tool.Effect == ToolEffectCommandUnobserved {
		return true
	}
	// Destructive tools need permission in default mode
	return tool.Destructive
}

// workspaceRefusalPath names the target a boundary refusal was about, for the
// path-aware breaker. It uses the same field map the validator itself keys on,
// so the two cannot disagree about which argument is the path, and it
// canonicalises the same way every other failure identity does -- a refusal
// spelled ./app.py is the same refusal as app.py.
//
// A workspace root that cannot be opened resolves nothing, so the canonical
// form falls back to the raw spelling rather than inventing a path inside a
// directory that does not exist.
func workspaceRefusalPath(ctx *AgentContext, name string, args json.RawMessage) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(args, &fields) != nil {
		return name
	}
	for _, key := range workspacePathFields[name] {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) != nil || strings.TrimSpace(value) == "" {
			continue
		}
		return filepath.Clean(strings.TrimSpace(value))
	}
	// No usable path field: the tool itself is the identity, so repeated
	// refusals of the same tool still converge on one failure target.
	return name
}

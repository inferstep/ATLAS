package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Agent loop — iterative tool-calling loop between model and executors
// ---------------------------------------------------------------------------

// planGateMinScore is the plan-quality floor below which the plan-completion
// gate does not fire. The planner reports WinningScore per plan; a weak plan
// blocking a finished task is worse than no gate, and 0.6 keeps the gate on the
// plans the planner itself rates as sound (live plans score about 0.80).
const planGateMinScore = 0.6

// maxTotalFailures bounds a whole run's failed tool calls, independent of the
// consecutive-error breaker. That breaker now resets when the rejection
// changes kind (a converging model must not be killed for iterating), so this
// is what stops a run cycling through failure modes forever. Set well above
// what a legitimate multi-edit task needs: run 11 used 3 and was still short.
const maxTotalFailures = 12

// maxGateBounces caps EACH of the verification, done-without-action,
// expected-output, and claim-check gates independently. Mirrors the
// parse-error cap: a gate that has bounced the same `done` three times is
// in a stuck loop, so its fourth is accepted rather than bounced forever.
// The other gates keep their own budgets — see runState.gateBounces.
const maxGateBounces = 3

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
	// What the latest exit claim's closing said about itself, recorded by
	// exitGates for finalizeCompletion. replyOutstanding: it deferred work
	// the agent never did and could not be sent back to do it (bounces spent
	// or too little budget left). replyDeclaredIncomplete: it said, in the
	// first person, that the agent could not accomplish the request.
	replyOutstanding        bool
	replyDeclaredIncomplete bool
	// The closing asks the user for something (replyAsksUser) and nothing was
	// written. replyAwaitsUser: the question is legitimate as far as the run
	// can tell -- there was nothing to inspect, or the run already looked.
	// replyHandedBack: the workspace holds files the run never opened, and
	// the one send-back (handoffSentBack) was spent or could not fit.
	replyAwaitsUser bool
	replyHandedBack bool
	// replyScopeUnmet: the request asked about several files, the run opened
	// several, and the answer accounted for fewer than two of them.
	replyScopeUnmet bool
	handoffSentBack bool
	// unresolvedGates records, for the exit being judged, each gate whose
	// finding still held when its bounces were spent, with the finding in one
	// line. A spent gate used to fall straight through to "completed" with
	// nothing in the status, the reason or the summary. finalizeCompletion
	// ends the run on the findings that are facts about the delivered work;
	// the heuristic ones become caveats in the summary (unresolvedGateCaveats).
	unresolvedGates map[string]string
	// driftedAfterVerify names a file whose bytes changed, outside the edit
	// tools, after the run that verified it. It holds until a run verifies
	// again: the artifact gate that found it clears verifiedThisLoop on its
	// first bounce, and without this the next exit no longer saw the drift.
	driftedAfterVerify string
	// Name of a tool_call that has been streamed but not yet answered by a
	// tool_result. The call is announced before permission and execution,
	// so any exit in between has to answer it or the consumer is left with
	// a call that never resolves. Cleared by whichever path emits the
	// result — the normal one, or bounceToolCall on a refusal.
	pendingToolCall string
	// Set when a write/edit/structural_edit/delete landed in this run.
	madeProductiveChange bool
	// Set when a read-only tool succeeds — the model opened the project
	// to answer this message. Distinguishes a request the model treated
	// as work from one it answered conversationally, without consulting
	// a vocabulary list. See wantsStateChange.
	inspectedWorkspace bool
	// shadowGate is bounded diagnostic-only sequencing. Nothing but the
	// shadow emitter reads it.
	shadowGate shadowGateSeq
	// Files the prompt explicitly asks the model to produce ("save your
	// solution in X"). Checked against disk before `done` is allowed — a
	// model can satisfy the generic action gate with a PARTIAL artifact
	// or by exploring without ever committing the named output. Computed
	// once from the prompt.
	expectedOutputs []string
	// The decision that named them, carried whole. A later change cannot
	// merge the contract list with the prose heuristic again: the source and
	// whether knowledge was stated travel with the paths.
	outputObligation obligationDecision
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
	// verifiedByRedirect names the file a verification command piped into the
	// program's stdin, when one did. verifiedStandalone records that some
	// verification ran the program without a redirect. Both are needed: only
	// a run that ONLY ever verified through a redirect has failed to check
	// the artifact the way the caller will use it.
	verifiedByRedirect string
	verifiedStandalone bool
	// verifiedHashes binds the verification to the BYTES it verified: the
	// sha256 of every session-written file, snapshotted when a verifying run
	// succeeds. The exit re-hashes; any drift means the final artifact is not
	// the one that was checked. Session-level booleans cannot express that —
	// they verify a moment, not an artifact — which is the shared root of the
	// verify-then-modify, warned-write and stale-evidence holes (third-party
	// audit finding: evidence must be a contract tied to the final artifact).
	verifiedHashes map[string]string
	// pendingWarnedRun is the SET of paths whose last landed write carried a
	// parse warning and has not been executed since. A warned landing is
	// pending work, not advice: measured, a session wrote six times without a
	// single run, ignoring "Run it now" in four consecutive warnings, then
	// died on edit repeats. Further writes to such a path bounce until any
	// verification command runs.
	//
	// Membership IS the warning. It was briefly a map of booleans written
	// with both values, and the exit gate reads it by ranging over keys, so a
	// clean landing stored false and was then announced to the model as "on
	// disk with a parse warning ... as written it cannot work" over a file
	// that parsed. Both frozen Stage-1 sessions that spent themselves
	// rewriting an already-valid file took that gate at turn 1. Go through
	// markWarnedRun and the value is never anything but true.
	pendingWarnedRun map[string]bool
	// repairs are the files this session left unparseable, keyed by
	// ledgerKey, with what was tried on each (repair.go). repairOrder keeps
	// the order they opened in, for stable messages.
	repairs     map[string]*repairState
	repairOrder []string
	// contentLoopRecoveries counts the times this run has answered a
	// repetition cut with a corrective instead of ending. Bounded, so a
	// model that will not stop repeating still terminates.
	contentLoopRecoveries int
	// productiveChanges counts successful writes/edits; recoveriesAtLastCharge
	// remembers the count when the allowance was last charged, so a recovery
	// that was followed by work landing can give its charge back under
	// ATLAS_CONTENT_LOOP_COUNT=unproductive.
	productiveChanges      int
	productiveAtLastCharge int
	// Phase 4B: how many times a raw @fenced write for a canonical path has
	// met the run-first demand, and whether that path's one recovery has been
	// spent. Both are session-local, bounded by the number of paths the run
	// touches, and hold no file contents.
	fencedRunFirstRepeats map[string]int
	fencedRecoverySpent   map[string]bool
	// fencedChannelClosed records the canonical paths whose fenced channel has
	// been declared spent to the model, so the offer is made once and a new
	// turn, an alias, or an unrelated success cannot re-open it.
	fencedChannelClosed map[string]bool
	// steerRepeats counts, per canonical path, how many times a write_file
	// steering refusal has been ignored and repeated; steerRecovered records
	// which paths have already spent their one recovery. Both are cleared by
	// a materially different action on the SAME path, and by nothing else --
	// a success elsewhere is not evidence that this path is unstuck.
	steerRepeats   map[string]int
	steerRecovered map[string]bool
	// noopEditRepeats counts explicit old_str == new_str edits per canonical
	// path AND the exact broken hash they were sent against; brokenArtifact-
	// Recovered records which of those evidence generations have spent their
	// one recovery. Keying on the hash is what makes a new generation re-arm
	// and a stale one unusable.
	noopEditRepeats         map[string]int
	brokenArtifactRecovered map[string]bool
	// c4Rejected is what the session knows about replacements that were
	// refused while the file they targeted stayed valid on disk. Keyed by
	// canonical path AND the surviving disk hash, so new bytes are a new
	// question and the old evidence cannot describe them.
	c4Rejected map[string]*proposalRejection
	// mutationDebt is what the session still owes on a per-path basis: a
	// valid, permitted, in-workspace mutation the model asked for that has
	// not reached a demonstrated resolved state. Canonically keyed, bounded,
	// and deliberately NOT the deliverable ledger -- that records what the
	// session owns on disk, and an intent that never landed owns nothing.
	mutationDebt   map[string]*mutationDebtEntry
	debtGeneration int
	// debtRecoveryOffered is the last generation the model was given a chance
	// to settle. Bumping the generation when NEW work goes unresolved buys
	// exactly one more offer, and the total is capped so it cannot loop.
	debtRecoveryOffered int
	debtRecoveryCount   int
	// debtOverflow fails closed past the ceiling: the session stops naming
	// individual paths but never stops reporting that work is unresolved.
	debtOverflow bool
	// toolBanned records (tool, path) pairs the loop has taken away from the
	// model after it proved it cannot use them on that file. Advice is not a
	// fix when the model ignores advice: measured dogfooding "build me a
	// snake game", a no-op edit_file was refused with an explicit "re-sending
	// will not help, use structural_edit instead" and the model re-sent the
	// identical call on the very next turn, twice, until the breaker killed a
	// 48-minute session. A tool the harness removes is a contract; a tool the
	// harness merely discourages is a suggestion.
	toolBanned map[string]bool

	// redRunStreak counts consecutive FAILED verification commands with no
	// green in between. Past a threshold, incremental edits have had their
	// chance: the bare-model retry loop's whole advantage is the fresh
	// rewrite, and sessions here were observed re-running a broken program
	// five times while nibbling at it with edits.
	redRunStreak int
	// headOnlyProbe is the last successful headers-only probe this run made.
	// Such a probe is not verification; naming it is how the run learns that.
	headOnlyProbe string
	// uncountedCheck explains, for the exit gates, the most recent passing
	// command that did not count as verification: a parse, a lint or a
	// `--version`, or a check whose result the line hid (`pytest | tail`).
	// Naming it is how the run learns why, instead of repeating it.
	uncountedCheck string
	// lastVerifyWasLocalProbe records that the verification this loop holds
	// came from probing a local service rather than from running the artifact.
	// Such a probe only speaks for the process that answered it.
	lastVerifyWasLocalProbe bool
	// Set when a verification command RAN AND FAILED and none has
	// succeeded since. Observed session state, not a guess about the
	// request: once a test has gone red in this loop, declaring done is
	// dishonest regardless of how the user phrased the ask. Closes the
	// case the message-shape check cannot see: a model that watches a
	// test fail, diagnoses the fix in prose, and exits through a bare
	// text narration.
	sawFailedVerification bool
	// failing holds the commands whose latest run failed since a run last
	// verified, and sawFailedVerification is set exactly while it is not
	// empty. A passing run, test or probe clears it all. A passing check that
	// runs nothing (a parse, a lint, a declared command) clears only its own
	// earlier failure, so a lint cannot clear a failed test.
	failing map[string]bool
	// The red verification command was a long-running server rather than a
	// broken build — it never exited, or the port was already bound. Changes
	// what the verification gate tells the model to do next, because
	// re-running a server in the foreground can never exit clean.
	serverStartBlocked bool
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
	// The rejection reaches the model through Messages. It reached nothing
	// else: a completion gate holding a run back — "you were asked to change
	// something and have not" — produced no event, so the TUI showed an
	// unexplained pause and the run's own event stream held no record that a
	// gate had fired at all. Measured across 84 sessions, that made the
	// completion gates unobservable while 11 of 35 failures were the model
	// stopping short, which is exactly what they exist to catch.
	//
	// Emitted as its own type rather than a tool_result: nothing was
	// executed, and a consumer pairing calls with results must not see a
	// result it never made a call for.
	ctx.Stream("gate", map[string]interface{}{
		"gate":   toolName,
		"turn":   s.turn,
		"reason": truncateStr(rejection, 200),
	})
	// A refusal is a tool result too, so it names what is still unparseable.
	// The repair gate's own message already does.
	note := s.repairNote()
	if toolName == "repair_gate" {
		note = ""
	}
	ctx.Messages = append(ctx.Messages, AgentMessage{Role: "assistant", Content: s.response})
	ctx.Messages = append(ctx.Messages, AgentMessage{
		Role:       "tool",
		Content:    bounceContent(rejection, note),
		ToolCallID: fmt.Sprintf("call_%d", s.turn),
		ToolName:   toolName,
	})
}

// bounceContent is a refusal's tool message: the legacy two-key shape, and
// open_repair while a file this session wrote does not parse (#214).
func bounceContent(rejection, openRepair string) string {
	if openRepair == "" {
		return fmt.Sprintf(`{"success":false,"error":%q}`, rejection)
	}
	b, _ := json.Marshal(struct {
		Success    bool   `json:"success"`
		Error      string `json:"error"`
		OpenRepair string `json:"open_repair"`
	}{false, rejection, openRepair})
	return string(b)
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
	s.pendingToolCall = ""
	ctx.Stream("tool_result", map[string]interface{}{
		"tool":    toolName,
		"success": false,
		"error":   rejection,
	})
}

// verificationDemandedAndUnmet reports whether this run needed a passing
// verification command and never got one. Independent of the bounce budget:
// exhausting the bounces means the gate stopped blocking, not that the work
// was verified.
func (s *runState) verificationDemandedAndUnmet() bool {
	return (s.userWantsVerification || s.sawFailedVerification) && !s.verifiedThisLoop
}

// observeVerification updates the run's verification state from one finished
// run_command.
//
// Only a run that ran the program or its tests, or fetched a page, and whose
// exit status the line actually reports (classifyCommandEvidence) counts as
// verification. A parse, a lint or a `--version` does not, and neither does a
// test whose status `| tail` or `|| true` replaced; the exit gates name such a
// command back to the run instead of counting it. Once a verification passes,
// the fix-intent gate stops blocking `done`. A later failure takes that back:
// the latest result on the latest bytes is the one that describes the
// artifact.
func (s *runState) observeVerification(ctx *AgentContext, userMessage string, turn int, command string, result *ToolResult) {
	if ctx == nil || result == nil {
		return
	}
	if result.Success && isHeadOnlyProbe(command) {
		log.Printf("[agent] headers-only probe does not verify: %q", truncateStr(command, 60))
		s.headOnlyProbe = command
	}
	ev := classifyCommandEvidence(command)
	declared := contractRequiresCommand(ctx, command)
	if ev.Kind == evidenceNone && !declared {
		if result.Success && ev.Masked {
			s.uncountedCheck = uncountedNote(command, ev.MaskNote)
			log.Printf("[agent] result hidden, not verification: %q", truncateStr(command, 60))
		}
		return
	}
	switch {
	case !result.Success && verificationNeverRan(result):
		// The command failed before it could exercise anything. Not
		// evidence the artifact is broken, so it must not latch -- and not
		// evidence it works, so it does not clear either. Strictly neutral.
		log.Printf("[agent] verification did not run: turn=%d cmd=%q — neither latching nor clearing",
			turn, truncateStr(command, 60))
	case !result.Success:
		s.observeFailedCheck(ctx, turn, command, ev, result)
	case ev.Kind.verifies() && !declared && inlineCodeTouchesNothing(ctx, ev):
		// Inline code that neither names nor imports a file this session
		// changed ran nothing the work is made of, so it neither verifies nor
		// clears a failure. `python -c "print(1)"` passed the fix-intent gate
		// and cleared an earlier red run of the real program.
		s.uncountedCheck = uncountedNote(command,
			"the inline code it ran touches no file this session changed. Run the program or its tests.")
		log.Printf("[agent] inline code touched no changed file, not verification: %q",
			truncateStr(command, 60))
	case ev.Kind.verifies() && silentRunWhenOutputPromised(ctx, userMessage, command, result.Data):
		// Exit 0 with empty stdout is not verification of a task whose
		// prompt demands printed output. Measured: a generation drifted into
		// comment-reasoning, the tail of the file (including the solve()
		// call) was swallowed by a comment, and the program parsed, ran,
		// printed nothing and exited 0 — the session recorded that as
		// verification and reported success on a program that provably
		// produced no answer.
		// Latch, don't just decline: a silent run IS a failed verification of
		// a print-demanding task. Merely not counting it left done free to
		// pass when nothing else demanded verification — measured: the gate
		// fired three times in one night and three silent finals still
		// shipped.
		s.noteFailure(command)
		s.verifiedThisLoop = false
		log.Printf("[agent] run exited 0 with no stdout on a print-demanding task — latching the verification gate: %q",
			truncateStr(command, 60))
	case ev.Kind.verifies():
		s.verifiedThisLoop = true
		s.lastVerifyWasLocalProbe = isHTTPProbe(strings.Join(ev.Covering, " && "))
		s.failing, s.sawFailedVerification = nil, false
		s.redRunStreak = 0
		s.uncountedCheck = ""
		s.driftedAfterVerify = ""
		s.verifiedHashes = sessionWriteHashes(ctx)
		// A program run as `prog < data` is verified under a contract the
		// caller may not use. Tracked so the exit can tell the two apart.
		// See stdinRedirectSource.
		if src := stdinRedirectSource(command); src != "" {
			s.verifiedByRedirect = src
		} else {
			s.verifiedStandalone = true
		}
		recordVerificationEvidence(ctx, turn, command, ev, false)
	default:
		// It passed and ran nothing: a static check, or a declared command
		// that runs nothing. It clears only its own earlier failure, and it
		// is recorded only when the client declared it, so the declaration
		// can be discharged; the record binds no file (showsWorking).
		if s.failing[command] {
			delete(s.failing, command)
			if len(s.failing) == 0 {
				s.sawFailedVerification = false
				s.redRunStreak = 0
				log.Printf("[agent] %q passes again — its earlier failure no longer holds the gate", truncateStr(command, 60))
			}
		}
		if declared {
			recordVerificationEvidence(ctx, turn, command, ev, false)
		}
		switch {
		case ev.Masked:
			s.uncountedCheck = uncountedNote(command, ev.MaskNote)
		case ev.Kind == evidenceStatic:
			s.uncountedCheck = uncountedNote(command,
				"it checks that the code is well formed, not that it works. Run the program or its tests.")
			log.Printf("[agent] static check only, not verification: %q", truncateStr(command, 60))
		}
	}
}

// observeFailedCheck latches the verification gate on a red run and takes
// back any earlier pass, because the latest result is the one that describes
// what is on disk.
func (s *runState) observeFailedCheck(ctx *AgentContext, turn int, command string, ev commandEvidence, result *ToolResult) {
	s.noteFailure(command)
	s.verifiedThisLoop = false
	s.verifiedStandalone = false
	s.verifiedByRedirect = ""
	s.redRunStreak++
	s.serverStartBlocked = blockedServerStart(result)
	recordVerificationEvidence(ctx, turn, command, ev, true)
	log.Printf("[agent] verification FAILED: turn=%d cmd=%q server_blocked=%v — done is gated until it passes",
		turn, truncateStr(command, 60), s.serverStartBlocked)
	// Advice at the crossing, not at the done-gate: waiting for the model to
	// attempt `done` meant it kept nibbling edits for turns after the streak
	// already proved the approach dead. Queue once, on the transition — the
	// done-gate text repeats it if the model still tries to exit red.
	if s.redRunStreak == rewriteThreshold+1 && !s.serverStartBlocked {
		s.queueCorrective(freshRewriteAdvice(s.redRunStreak))
		log.Printf("[agent] red streak crossed %d — fresh-rewrite advice injected now", rewriteThreshold)
	}
}

// noteFailure latches the verification gate on command's failure.
func (s *runState) noteFailure(command string) {
	if s.failing == nil {
		s.failing = map[string]bool{}
	}
	s.failing[command] = true
	s.sawFailedVerification = true
}

// recordVerificationEvidence appends the evidence record for one run. A
// passing run binds the files its covering segments exercised, at the exact
// bytes they held. A failed run is charged to every segment whose result the
// line reported, so it takes back an earlier pass over the same bytes
// (coverageRecord).
//
// Stamped AFTER recordLedgerEffect ran for this call: executeToolCall
// reconciles a shell effect into the ledger (invalidateTrackedValidation
// rehashes every tracked path and bumps the generation where bytes moved)
// before the result reaches the loop. So the identity below describes the
// workspace the command LEFT, never the one it found.
func recordVerificationEvidence(ctx *AgentContext, turn int, command string, ev commandEvidence, failed bool) {
	segs := ev.Covering
	if failed {
		segs = ev.Honest
	}
	generation, state := workspaceIdentity(ctx)
	ctx.VerificationEvidence = append(ctx.VerificationEvidence, VerificationRecord{
		Command:             command,
		Redirect:            stdinRedirectSource(command),
		Covered:             coverageForGreenCommand(ctx, strings.Join(segs, " && ")),
		Turn:                turn,
		Kind:                ev.Kind.String(),
		Failed:              failed,
		WorkspaceGeneration: generation,
		WorkspaceStateHash:  state,
	})
	log.Printf("[agent] verification recorded: turn=%d kind=%s failed=%v cmd=%q",
		turn, ev.Kind, failed, truncateStr(command, 60))
}

// uncountedNote says why a passing command did not count as verification.
func uncountedNote(command, why string) string {
	return fmt.Sprintf("\n\n`%s` did not count as verification: %s", truncateStr(strings.TrimSpace(command), 60), why)
}

// actionDemandedAndUnmet reports a run that was asked to change something on
// disk and finished without changing anything.
//
// The action gate bounces this while it has bounces left, and then stops:
// chargeBounce is capped so an exhausted gate cannot loop. Past that cap the
// exit goes through unremarked, which is how a session ends having written
// nothing while saying nothing about it. Observed on smallrung_toml: a
// structural_edit was refused for making the file invalid, the model gave up
// on tools and emitted the replacement as chat text, and the run finished
// with that code as its summary — the user is shown a block of code that
// was never applied, with no indication it was not.
func (s *runState) actionDemandedAndUnmet(ctx *AgentContext, userMessage string) bool {
	return observeActionDemand(ctx, s, shadowGateActionDemanded,
		decideActionDemand(ctx.TaskContract, userMessage, ctx.Tier, s.inspectedWorkspace)) &&
		!s.madeProductiveChange
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
	//
	// Whichever of these matches, the reply is not an answer. It goes back for
	// one more attempt only while a bounce is left AND the session has time to
	// act on it; otherwise the flags below make finalizeCompletion report the
	// session incomplete. Before this, a spent bounce fell straight through
	// to "completed", so the cap turned an unfinished reply into a success.
	s.replyOutstanding, s.replyDeclaredIncomplete = false, false
	s.replyAwaitsUser, s.replyHandedBack, s.replyScopeUnmet = false, false, false
	s.unresolvedGates = nil
	if ctx != nil && ctx.ShellEffectsUnobserved {
		s.gateUnresolved("shell_observation", "the workspace was too large to observe every file the shell commands changed")
	}
	if gate, msg := s.repairGate(ctx); gate != "" {
		return gate, msg
	}
	if gate, msg := s.intentGates(ctx, claimText); gate != "" {
		return gate, msg
	}
	if gate, msg := s.handoffGate(ctx, claimText); gate != "" {
		return gate, msg
	}
	if gate, msg := s.evidenceGates(ctx, claimText); gate != "" {
		return gate, msg
	}
	if gate, msg := s.scopeGate(ctx, userMessage, claimText); gate != "" {
		return gate, msg
	}
	if gate, msg := s.runFirstGateAtExit(ctx); gate != "" {
		return gate, msg
	}
	if gate, msg := s.routeContractGate(ctx); gate != "" {
		return gate, msg
	}
	contractDemand := decideVerificationDemand(ctx, ctx.TaskContract, s.expectedOutputs)
	contractOwed := contractDemand.Required && !contractDemand.Met
	if gate, msg := s.backgroundGate(ctx, contractOwed); gate != "" {
		return gate, msg
	}
	if gate, msg := s.verificationGates(ctx, contractDemand, contractOwed); gate != "" {
		return gate, msg
	}
	return s.completionGates(ctx, userMessage, claimText)
}

// repairGate holds the exit while a file this session wrote does not parse.
func (s *runState) repairGate(ctx *AgentContext) (string, string) {
	// A file this session left unparseable is an open repair, and the work is
	// not finished while one is open (repair.go). First, because it is the
	// most concrete fact there is about the work. Bounded like every exit
	// gate: past the bounces, finalizeCompletion ends the run
	// repair_unfinished and the final message hands the file to the user
	// with what was tried.
	s.refreshRepairs(ctx, s.turn, "")
	if open := s.openRepairs(); len(open) > 0 {
		if s.continuationFits(ctx) && s.chargeBounce("repair_gate") {
			log.Printf("[agent] repair gate: %s does not parse (bounce %d/%d)",
				repairNames(open), s.gateBounces["repair_gate"], maxGateBounces)
			return "repair_gate", repairGateMessage(open)
		}
		s.gateUnresolved("repair_gate", repairNames(open)+" does not parse")
	}
	return "", ""
}

// intentGates hold a reply that is not an answer yet: one that promises the
// content, announces a tool call it never made, or closes by deferring the
// work. A reply that says it cannot do the work is marked, not sent back.
func (s *runState) intentGates(ctx *AgentContext, claimText string) (string, string) {
	if promisesMoreContent(claimText) {
		if s.continuationFits(ctx) && s.chargeBounce("intent_gate") {
			log.Printf("[agent] intent gate: bouncing a reply that promised content it did not deliver (bounce %d/%d)",
				s.gateBounces["intent_gate"], maxGateBounces)
			return "intent_gate", "You ended by saying you would provide the answer, but the reply stops there and the turn ends with it — the user sees only the promise. Give the actual content now, in full, in a single `text` reply."
		}
		s.replyOutstanding = true
	}
	if s.toolsRun == 0 && announcesImminentToolUse(claimText) {
		if s.continuationFits(ctx) && s.chargeBounce("intent_gate") {
			log.Printf("[agent] intent gate: bouncing a text exit that announced a tool call without making one (bounce %d/%d)",
				s.gateBounces["intent_gate"], maxGateBounces)
			return "intent_gate", "You described the tool call you were about to make instead of making it, and a `text` reply ends the turn. Emit the tool_call itself now — read the file, then answer in a single `text` reply once you have its contents."
		}
		s.replyOutstanding = true
	}
	// The same failure after work has started. The zero-tools condition above
	// was there so narration could not interrupt work in progress -- but a
	// `text` exit has already stopped the work, and a reply that CLOSES by
	// deferring ("I will now check the generate_plan function", "Please wait
	// while I verify the exact line") hands the user nothing. Every
	// bugfind_tiebreak session across four benchmark runs ended this way and
	// was reported completed. Judged on the closing only, with offers to the
	// user removed, so a real answer that mentions reading, or ends "If you'd
	// like, I can fix it", still completes.
	if replyDeclaresInability(claimText) {
		// Honest, and final: sending it back would demand work the agent has
		// just said it cannot do.
		s.replyDeclaredIncomplete = true
	} else if s.toolsRun > 0 && replyDefersWork(claimText) {
		if s.continuationFits(ctx) && s.chargeBounce("intent_gate") {
			log.Printf("[agent] intent gate: bouncing a reply that closed by deferring work it never did (bounce %d/%d)",
				s.gateBounces["intent_gate"], maxGateBounces)
			return "intent_gate", "Your reply ends by saying what you will do next, but a `text` reply ends the session — that work would never happen, and the user would be left without the answer. If you need to look at something, make that tool call now. If you already have what you need, give your complete final answer as a `text` reply that stands on its own. If you cannot answer, say so plainly."
		}
		s.replyOutstanding = true
	}
	return "", ""
}

// handoffGate sends a reply that asks the user back once, to look first at a
// workspace the run never opened.
func (s *runState) handoffGate(ctx *AgentContext, claimText string) (string, string) {
	// A reply that ends by asking the user. A question is a legitimate
	// outcome, and not a completed one: the user owes an answer. Whether it
	// was avoidable is decided from evidence, not wording -- if the workspace
	// holds files and the run opened none, the thing asked about may be right
	// there, so the reply goes back once (within the shared bounce cap and the
	// continuation floor) to look first. Only when nothing was written: a run
	// that delivered work and asks a follow-up is judged by its deliverables.
	if !s.replyOutstanding && !s.replyDeclaredIncomplete && !s.madeProductiveChange && replyAsksUser(claimText) {
		if s.inspectedWorkspace || len(inspectableWorkspaceFiles(ctx, 1)) == 0 {
			s.replyAwaitsUser = true
		} else if !s.handoffSentBack && s.continuationFits(ctx) && s.chargeBounce("handoff_gate") {
			s.handoffSentBack = true
			log.Printf("[agent] handoff gate: reply asks the user about a workspace the run never opened (bounce %d/%d)",
				s.gateBounces["handoff_gate"], maxGateBounces)
			return "handoff_gate", handoffMessage(ctx)
		} else {
			s.replyHandedBack = true
		}
	}
	return "", ""
}

// evidenceGates hold a reply that cites a file the run never read, or code
// that no read showed.
func (s *runState) evidenceGates(ctx *AgentContext, claimText string) (string, string) {
	// A claim about a file the run never opened. This is the conversational
	// half of an invariant the write path already enforces — edit_file,
	// structural_edit, insert_after and replace_lines all refuse a path that
	// was not read first — and until now answers were exempt, because
	// "conversational messages are never gated" (see wantsStateChange).
	// Diagnostic questions are exactly where that exemption costs the most:
	// the reply IS the deliverable, and a guess is indistinguishable from an
	// answer.
	if cited := unreadFileCitations(ctx, claimText); len(cited) > 0 {
		if s.chargeBounce("evidence_gate") {
			log.Printf("[agent] evidence gate: bouncing exit at turn %d — reply cites %v with no read (bounce %d/%d)",
				s.turn, cited, s.gateBounces["evidence_gate"], maxGateBounces)
			return "evidence_gate", unreadCitationMessage(cited)
		}
		s.gateUnresolved("evidence_gate", "the reply cites "+strings.Join(cited, ", ")+", which this run never read")
	}
	// The same claim one level down: the file was opened, but a truncated or
	// ranged read never showed the code the reply describes.
	if gaps := unshownSymbolCitations(ctx, claimText); len(gaps) > 0 {
		if s.chargeBounce("evidence_gate") {
			log.Printf("[agent] evidence gate: bouncing exit at turn %d — reply describes %s, which no read showed (bounce %d/%d)",
				s.turn, symbolNames(gaps), s.gateBounces["evidence_gate"], maxGateBounces)
			return "evidence_gate", unshownSymbolMessage(gaps)
		}
		s.gateUnresolved("evidence_gate", "the reply describes "+symbolNames(gaps)+", whose code this run never showed")
	}
	return "", ""
}

// scopeGate holds a read-only investigation whose reply accounts for fewer
// of the files it read than the request named.
func (s *runState) scopeGate(ctx *AgentContext, userMessage, claimText string) (string, string) {
	// An investigation that answers for less than it opened. Only for a
	// read-only run whose request named several files (investigationScopeUnmet),
	// and only after the reply has been judged by the gates above, so a
	// deferral, a question or an unread citation keeps its own, more specific
	// outcome.
	if !s.replyOutstanding && !s.replyDeclaredIncomplete && !s.replyAwaitsUser && !s.replyHandedBack {
		if read, cited, unmet := investigationScopeUnmet(ctx, userMessage, claimText,
			!s.madeProductiveChange, s.toolsRun); unmet {
			if s.continuationFits(ctx) && s.chargeBounce("scope_gate") {
				log.Printf("[agent] scope gate: reply accounts for %d of %d files read (bounce %d/%d)",
					len(cited), len(read), s.gateBounces["scope_gate"], maxGateBounces)
				return "scope_gate", investigationScopeMessage(read, cited)
			}
			s.replyScopeUnmet = true
		}
	}
	return "", ""
}

// runFirstGateAtExit holds the exit while a file on disk carries a parse
// warning and has never been run.
func (s *runState) runFirstGateAtExit(ctx *AgentContext) (string, string) {
	// A warned, never-executed artifact is not a deliverable. Without this a
	// session whose prompt carried no verification wording could end with a
	// file that never parsed and was never run, reported as success (audit
	// finding: warned state must be a terminal integrity condition, not a
	// rewrite throttle).
	for _, p := range sortedKeys(s.pendingWarnedRun) {
		if s.repairOpenFor(ctx, p) {
			// The repair gate owns a file that still does not parse.
			continue
		}
		if s.chargeBounce("run_first_gate") {
			log.Printf("[agent] run-first gate at exit: %s warned and never executed (bounce %d/%d)",
				p, s.gateBounces["run_first_gate"], maxGateBounces)
			return "run_first_gate", fmt.Sprintf(
				"`%s` is on disk with a parse warning and has never been run. Run it first — %s — and fix it before finishing; as written it cannot work.",
				p, runFirstInstruction(ctx, p))
		}
		s.gateUnresolved("run_first_gate", p+" has a parse warning and was never run")
		break
	}
	return "", ""
}

// routeContractGate holds the exit while a page this run wrote submits to a
// route the server does not define.
func (s *runState) routeContractGate(ctx *AgentContext) (string, string) {
	// A page this run wrote submits to a route the server never defines. The
	// lint reports it at write time, but an advisory finding can be ignored
	// and the delivered form then 404s. Bounded like every other exit gate: a
	// route registered in a way the check cannot see (a blueprint prefix, a
	// catch-all) is a real false positive, so the model can say so and finish
	// once the bounces are spent. Scoped, like the other exit gates, to what
	// THIS run did: it fires only when the run authored a web file. A question
	// answered about a project that already had a stale form action is not
	// this run's contract to keep, and bouncing that answer three times would
	// be a false positive the user cannot act on.
	if sessionWroteWebFiles(ctx) {
		if broken := routeContractFindings(ctx.WorkingDir); len(broken) > 0 {
			if s.chargeBounce("route_contract_gate") {
				log.Printf("[agent] route contract gate: %d unmatched submit target(s) at exit (bounce %d/%d)",
					len(broken), s.gateBounces["route_contract_gate"], maxGateBounces)
				return "route_contract_gate", routeContractMessage(broken)
			}
			s.gateUnresolved("route_contract_gate", fmt.Sprintf(
				"%d form or request target(s) in the pages this run wrote name a route the server does not define", len(broken)))
		}
	}
	return "", ""
}

// backgroundGate asks the run to stop a job of its own that is still
// running, unless a verification or a planned run still needs the job.
func (s *runState) backgroundGate(ctx *AgentContext, contractOwed bool) (string, string) {
	// A job this run started is still running. Completion is refused while a
	// process of the run's own may still be writing (finalizeCompletion:
	// background_work_unresolved), and that refusal replaces the model's
	// summary with a note about the job, so the user gets no account of the
	// work. The model is the only party that knows whether the process was a
	// verification server or something the user asked to keep up, so it is
	// told, and asked to stop it; otherwise working code can be reported
	// unfinished because the server that verified it was left running. Order
	// matters. While verification is still owed, the running job is the thing
	// to verify AGAINST, so this gate yields to the verification gate below;
	// stopping the server first leaves a verification that can no longer pass.
	// Nothing is weakened: once verification lands, this gate fires, and
	// finalizeCompletion still refuses completion while a job of the run's own
	// is live.
	//
	// A planned step that runs a command is owed the same way, so the gate
	// also yields while the plan gate has bounces left; a spent plan gate
	// cannot hold the job open.
	verificationOwed := ((s.userWantsVerification || s.sawFailedVerification) && !s.verifiedThisLoop) || contractOwed
	planRunOwed := planOwesRun(ctx) && s.gateBounces["plan_gate"] < maxGateBounces
	if live := settleBackgroundHazard(ctx); len(live) > 0 && !verificationOwed && !planRunOwed && s.chargeBounce("background_gate") {
		log.Printf("[agent] background gate: %d job(s) still running at exit (bounce %d/%d)",
			len(live), s.gateBounces["background_gate"], maxGateBounces)
		return "background_gate", backgroundStopMessage(ctx, live)
	}
	return "", ""
}

// verificationGates hold an exit whose verification is missing, out of date
// or partial, in this order: a run only ever fed through stdin, bytes that
// changed after the run that verified them, no verifying run at all, a probe
// of older code, and a work contract that nothing current shows working.
func (s *runState) verificationGates(ctx *AgentContext, contractDemand verificationDemand, contractOwed bool) (string, string) {
	if gate, msg := s.redirectGate(ctx); gate != "" {
		return gate, msg
	}
	if gate, msg := s.artifactGate(ctx); gate != "" {
		return gate, msg
	}
	if gate, msg := s.verificationGate(ctx); gate != "" {
		return gate, msg
	}
	if gate, msg := s.staleProbeGate(ctx); gate != "" {
		return gate, msg
	}
	return s.contractVerificationGate(ctx, contractDemand, contractOwed)
}

// redirectGate holds a run that was verified only with input piped into its
// stdin.
func (s *runState) redirectGate(ctx *AgentContext) (string, string) {
	// Verified only through a stdin redirect: the program was never run the
	// way its caller will run it. See stdinRedirectSource for the measurement.
	if s.verifiedByRedirect != "" && !s.verifiedStandalone {
		if s.chargeBounce("contract_gate") {
			log.Printf("[agent] contract gate: every verification piped %q into stdin (bounce %d/%d)",
				s.verifiedByRedirect, s.gateBounces["contract_gate"], maxGateBounces)
			return "contract_gate", redirectOnlyVerificationMessage(s.verifiedByRedirect)
		}
		s.gateUnresolved("contract_gate", "the program was only ever run with "+s.verifiedByRedirect+
			" piped into its stdin, never the way a caller that does not pipe input would run it")
	}
	return "", ""
}

// artifactGate holds a run whose bytes changed after the run that verified
// them.
func (s *runState) artifactGate(ctx *AgentContext) (string, string) {
	// Verification is of bytes, not of a moment. If any file this session
	// wrote no longer matches the hash snapshotted when the verifying run
	// succeeded, the final artifact is unverified whatever the booleans say.
	if s.verifiedThisLoop {
		if changed := driftedSinceVerification(ctx, s.verifiedHashes); changed != "" {
			// Bounce or not, what is on disk now was never run.
			s.verifiedThisLoop = false
			s.verifiedStandalone = false
			s.driftedAfterVerify = changed
		}
	}
	if changed := s.driftedAfterVerify; changed != "" {
		if s.chargeBounce("artifact_gate") {
			log.Printf("[agent] artifact gate: %s changed after the run that verified it (bounce %d/%d)",
				changed, s.gateBounces["artifact_gate"], maxGateBounces)
			return "artifact_gate", fmt.Sprintf(
				"`%s` changed after the run that verified it, so what is on disk now has never been executed. Run it again and confirm the output before finishing.", changed)
		}
		s.gateUnresolved("artifact_gate", relativeToWorkspace(ctx, changed)+
			" changed after the run that verified it and was not run again")
	}
	return "", ""
}

// verificationGate holds a run that was asked to verify its work, or saw a
// verification fail, and has no successful verification since.
func (s *runState) verificationGate(ctx *AgentContext) (string, string) {
	if (s.userWantsVerification || s.sawFailedVerification) && !s.verifiedThisLoop && s.chargeBounce("verification_gate") {
		log.Printf("[agent] verification gate: bouncing exit at turn %d (trigger=%s, no successful verification command this loop, bounce %d/%d)",
			s.turn, gateTrigger(s.userWantsVerification, s.sawFailedVerification), s.gateBounces["verification_gate"], maxGateBounces)
		staleJob, staleFile := staleServingJob(ctx)
		if staleJob != "" {
			log.Printf("[agent] job %s predates the last change to %s — probing it would not show it", logPath(staleJob), logPath(staleFile))
		}
		return "verification_gate", verificationRejectionFor(
			s.sawFailedVerification, s.serverStartBlocked, anyBackgroundJobID(ctx), s.redRunStreak,
			s.headOnlyProbe, staleJob, staleFile) + s.uncountedCheck
	}
	return "", ""
}

// staleProbeGate holds a run verified by a probe of a job that started before
// the last change to the code the job serves.
func (s *runState) staleProbeGate(ctx *AgentContext) (string, string) {
	// Verified by a probe, but the probe reached a job that predates the last
	// change to the file, so it answered for older code. finalizeCompletion
	// would refuse this exit with verification_demanded_unmet and no chance to
	// act; the same fact is worth one bounce, out of the verification gate's
	// own budget, so the run can restart the process and probe the code it
	// actually wrote.
	if s.verifiedThisLoop && s.lastVerifyWasLocalProbe {
		if staleJob, staleFile := staleServingJob(ctx); staleJob != "" {
			if s.chargeBounce("verification_gate") {
				log.Printf("[agent] the probe that verified this loop hit job %s, started before the last change to %s (bounce %d/%d)",
					logPath(staleJob), logPath(staleFile), s.gateBounces["verification_gate"], maxGateBounces)
				return "verification_gate", verificationRejectionFor(false, false, "", 0, "", staleJob, staleFile)
			}
			// The probe answered for older code, so nothing verified this.
			s.verifiedThisLoop = false
			s.gateUnresolved("stale_probe", fmt.Sprintf(
				"the probe that verified the run reached job %s, which started before the last change to %s",
				staleJob, relativeToWorkspace(ctx, staleFile)))
		}
	}
	return "", ""
}

// contractVerificationGate holds a run whose work contract owes evidence
// that nothing current shows.
func (s *runState) contractVerificationGate(ctx *AgentContext, contractDemand verificationDemand, contractOwed bool) (string, string) {
	// The work contract owes evidence that what this run wrote works, and
	// nothing current shows it. finalizeCompletion refuses the exit either
	// way; this says so while the run can still act on it, out of the
	// verification gate's own budget.
	if contractOwed && s.continuationFits(ctx) && s.chargeBounce("verification_gate") {
		log.Printf("[agent] contract verification gate: nothing current shows %q working (bounce %d/%d)",
			contractDemand.Missing, s.gateBounces["verification_gate"], maxGateBounces)
		return "verification_gate", contractDemandMessage(ctx, contractDemand) + s.uncountedCheck
	}
	return "", ""
}

// completionGates hold an exit that leaves the work unfinished, in this
// order: code nothing calls, plan steps never carried out, no change where
// the request asked for one, a named deliverable not on disk, and a claim of
// completeness the files do not bear out.
func (s *runState) completionGates(ctx *AgentContext, userMessage, claimText string) (string, string) {
	if gate, msg := s.orphanGate(ctx); gate != "" {
		return gate, msg
	}
	if gate, msg := s.planGate(ctx); gate != "" {
		return gate, msg
	}
	if gate, msg := s.actionGate(ctx, userMessage); gate != "" {
		return gate, msg
	}
	if gate, msg := s.outputGate(ctx); gate != "" {
		return gate, msg
	}
	return s.claimCheckGate(ctx, userMessage, claimText)
}

// orphanGate holds the exit while code this run added is never called.
func (s *runState) orphanGate(ctx *AgentContext) (string, string) {
	// Code the run added that nothing calls. Checked at the exit rather than
	// at the write, because wiring it up on a later turn is normal — only
	// finishing with it unwired is the defect.
	if orphans := orphanedAdditions(ctx); len(orphans) > 0 {
		if s.chargeBounce("orphan_gate") {
			log.Printf("[agent] orphan gate: bouncing exit at turn %d — added-but-uncalled in %d file(s) (bounce %d/%d)",
				s.turn, len(orphans), s.gateBounces["orphan_gate"], maxGateBounces)
			return "orphan_gate", orphanedAdditionsMessage(orphans)
		}
		s.gateUnresolved("orphan_gate", "code this run added is never called: "+orphanNames(ctx, orphans))
	}
	return "", ""
}

// planGate holds the exit while steps of the plan were never carried out.
func (s *runState) planGate(ctx *AgentContext) (string, string) {
	// Steps the plan named and no tool call ever satisfied. Same shape as the
	// verification gate: a fact the run already holds, used at the exit
	// instead of only being shown mid-run.
	if msg := planIncompleteMessage(ctx); msg != "" {
		if s.chargeBounce("plan_gate") {
			log.Printf("[agent] plan gate: bouncing exit at turn %d — %d/%d steps satisfied (bounce %d/%d)",
				s.turn, countTrue(ctx.PlanStepsSatisfied), len(ctx.Plan.Steps),
				s.gateBounces["plan_gate"], maxGateBounces)
			return "plan_gate", msg
		}
		s.gateUnresolved("plan_gate", fmt.Sprintf("%d of %d plan steps were never carried out",
			len(ctx.Plan.Steps)-countTrue(ctx.PlanStepsSatisfied), len(ctx.Plan.Steps)))
	}
	return "", ""
}

// actionGate holds an exit that changed nothing on disk when the request
// asked for a change.
func (s *runState) actionGate(ctx *AgentContext, userMessage string) (string, string) {
	if observeActionDemand(ctx, s, shadowGateActionGate,
		decideActionDemand(ctx.TaskContract, userMessage, ctx.Tier, s.inspectedWorkspace)) &&
		!s.madeProductiveChange && s.chargeBounce("action_gate") {
		log.Printf("[agent] done-without-action gate: bouncing exit at turn %d (user prompt %q wants a state change, no successful write/edit/structural_edit this loop, bounce %d/%d)",
			s.turn, truncateStr(userMessage, 60), s.gateBounces["action_gate"], maxGateBounces)
		return "action_gate", actionWithoutProductiveChangeMessage(userMessage)
	}
	return "", ""
}

// outputGate holds the exit, once, while a deliverable the request named is
// not on disk.
func (s *runState) outputGate(ctx *AgentContext) (string, string) {
	if missing := missingExpectedOutputs(ctx, s.expectedOutputs); len(missing) > 0 && !s.outputGateUsed && s.chargeBounce("output_gate") {
		s.outputGateUsed = true // fire once — see field doc
		log.Printf("[agent] expected-output gate: bouncing exit at turn %d — named deliverable(s) %v not on disk (bounce %d/%d)",
			s.turn, logPaths(missing), s.gateBounces["output_gate"], maxGateBounces)
		return "output_gate", expectedOutputMissingMessage(missing)
	}
	return "", ""
}

// claimCheckGate holds an exit whose claim of completeness the files on disk
// do not bear out.
func (s *runState) claimCheckGate(ctx *AgentContext, userMessage, claimText string) (string, string) {
	if claimsUniversal(claimText) || promptIsMultiIssue(userMessage) {
		if gap := verifyCompletionClaims(ctx.WorkingDir); gap != "" {
			if s.chargeBounce("claim_check") {
				log.Printf("[agent] claim-check gate: bouncing exit at turn %d (bounce %d/%d) — %q",
					s.turn, s.gateBounces["claim_check"], maxGateBounces, truncateStr(gap, 200))
				return "claim_check", gap
			}
			s.gateUnresolved("claim_check", strings.TrimRight(truncateStr(
				strings.Join(completionClaimGaps(ctx.WorkingDir), " "), 240), "."))
		}
	}
	return "", ""
}

// contractDemandMessage names what a work request still owes: a passing run
// of a command the client declared, or a run of a file this session wrote.
func contractDemandMessage(ctx *AgentContext, d verificationDemand) string {
	if d.MissingCommand {
		return fmt.Sprintf("Cannot finish yet: the task requires `%s` to pass against the current files, "+
			"and it has not. Run it now and fix what it reports.", d.Missing)
	}
	path := relativeToWorkspace(ctx, d.Missing)
	return fmt.Sprintf("Cannot finish yet: nothing that ran since `%s` was last written shows it working. "+
		"Run it — %s — or run tests that exercise it, and check the output before finishing.",
		path, runFirstInstruction(ctx, path))
}

// gateUnresolved records that gate's finding still holds and its bounces are
// spent, so the exit that follows cannot read as a clean completion.
func (s *runState) gateUnresolved(gate, finding string) {
	if s.unresolvedGates == nil {
		s.unresolvedGates = map[string]string{}
	}
	s.unresolvedGates[gate] = finding
	log.Printf("[agent] %s: bounces spent and the finding still holds: %s", gate, logPath(truncateStr(finding, 160)))
}

// unresolvedReasons are the spent gates whose finding is a fact about the
// delivered work, with the terminal reason each ends the run with. They are
// checked in this order.
var unresolvedReasons = []struct{ gate, reason string }{
	{"claim_check", "claim_check_unresolved"},
	{"run_first_gate", "warned_file_never_run"},
	{"evidence_gate", "unread_citation"},
}

// caveatGates are the spent gates whose finding is a heuristic with known
// false positives: a route registered where the lint cannot see it, code
// called dynamically, a plan step done another way, a program whose real
// interface is stdin, a drift or a stale probe the verification demand
// already judges. The run may complete, and the summary says what was not
// confirmed.
var caveatGates = []string{"artifact_gate", "contract_gate", "orphan_gate", "plan_gate",
	"route_contract_gate", "shell_observation", "stale_probe"}

// unresolvedGateCaveats is the part of a completed run's summary that names
// what the spent heuristic gates still found.
func unresolvedGateCaveats(st *runState) string {
	if st == nil {
		return ""
	}
	var sb strings.Builder
	for _, g := range caveatGates {
		if f, ok := st.unresolvedGates[g]; ok {
			sb.WriteString("\n\nNot confirmed by this run: " + f + ".")
		}
	}
	return sb.String()
}

// unresolvedGateNames lists the spent gates in a stable order, for the
// terminal event.
func unresolvedGateNames(st *runState) string {
	if st == nil || len(st.unresolvedGates) == 0 {
		return ""
	}
	names := make([]string, 0, len(st.unresolvedGates))
	for g := range st.unresolvedGates {
		names = append(names, g)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// orphanNames names up to three uncalled additions.
func orphanNames(ctx *AgentContext, orphans map[string][]orphanedSymbol) string {
	var names []string
	for file, syms := range orphans {
		for _, sym := range syms {
			names = append(names, sym.Name+" in "+relativeToWorkspace(ctx, file))
		}
	}
	sort.Strings(names)
	if len(names) > 3 {
		names = append(names[:3], fmt.Sprintf("%d more", len(names)-3))
	}
	return strings.Join(names, ", ")
}

// replyContinuationFloor is the least session time worth sending an unfinished
// reply back for: one turn to act on what it deferred and one to answer, at a
// median model think-time of about 26 s before a call.
const replyContinuationFloor = 60 * time.Second

// continuationFits reports whether the session can still afford to send an
// unfinished reply back. With less than replyContinuationFloor left, the
// honest outcome is to end incomplete now rather than to start work that the
// deadline will cut off and report as a timeout instead.
func (s *runState) continuationFits(ctx *AgentContext) bool {
	if ctx == nil || ctx.Ctx == nil {
		return true
	}
	if deadline, ok := ctx.Ctx.Deadline(); ok && time.Until(deadline) < replyContinuationFloor {
		return false
	}
	return true
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

// v3DeliveryNudge is what the run is told after V3 delivered a write or an
// edit, and whether a current run already shows the landed bytes working. It
// reads the proxy's own evidence (pathCoverageSatisfied, which includes the
// evidence V3's delivery staged), never the service's phase name: phase1 can
// be reached by agreement between candidates, or on a compile, with nothing
// ever running the code. supersededRel is set when V3 delivered its own
// version instead of the model's bytes.
func v3DeliveryNudge(ctx *AgentContext, phase string, candidates int, score float64,
	path, supersededRel string) (string, bool) {
	shown := false
	if path != "" {
		resolved := resolveAgentPath(ctx, path)
		if h := fileSHA256(ctx, resolved); h != "" {
			shown = pathCoverageSatisfied(ctx, resolved, h)
		}
	}
	switch {
	case supersededRel != "":
		return fmt.Sprintf(
			"V3 delivered ITS OWN version of %s through its %s pipeline (%d candidates, score=%.2f) — the bytes on disk are not the ones you sent. V3's checks are not evidence that the user's request is finished. Read %s and judge it against what was asked before you claim anything is done; if it is short of the request, change it from what is there.",
			supersededRel, phase, candidates, score, supersededRel), shown
	case shown:
		return fmt.Sprintf(
			"V3 delivered this edit through its %s pipeline (%d candidates, score=%.2f), and a run on these exact bytes shows it working. If this resolves the user's original request, respond with {\"type\":\"done\",\"summary\":\"<one sentence describing the fix>\"}. Only continue if you have a specific, concrete additional change to make — do not edit unrelated code.",
			phase, candidates, score), shown
	}
	return fmt.Sprintf(
		"V3 delivered this edit through its %s pipeline (%d candidates, score=%.2f). V3's checks are not a run of the program, and nothing has run what is on disk yet. Run it — %s — or its tests, check the output, and finish only if it does what was asked. Do not edit unrelated code.",
		phase, candidates, score, runFirstInstruction(ctx, path)), shown
}

// runAgentLoop runs the agent loop for a single user request.
// The model emits tool calls (constrained by grammar), the proxy executes them,
// and returns results. Continues until the model emits "done" or max turns hit.
func runAgentLoop(ctx *AgentContext, userMessage string) error {
	loopStart := beginAgentRun(ctx, userMessage)
	defer endAgentStage(ctx, loopStart)
	if !openConversation(ctx, userMessage) {
		return nil
	}

	// edit_file old_str-mismatch failures per path. A successful read_file
	// between attempts resets consecutiveErrors/RecentFailurePaths, which
	// masks the classic read→edit-miss→read loop (smaller models can't
	// reproduce old_str byte-for-byte). This counter survives interleaved
	// reads so we can force the structural_edit steer after the second miss.
	editMissByPath := map[string]int{}
	// THE output-obligation decision, made once, from the contract the
	// request boundary already validated. Nothing the model emits later can
	// change which source spoke.
	outputObligation := resolveOutputObligation(ctx, userMessage)
	loop := newAgentLoop(ctx, userMessage, outputObligation)
	st := loop.st

	for turn := 0; ctx.MaxTurns <= 0 || turn < ctx.MaxTurns; turn++ {
		response, parsed, outcome, err := loop.takeModelTurn(turn)
		switch outcome {
		case turnEnds:
			continue
		case runEnds:
			return err
		}

		switch parsed.Type {
		case "done":
			if loop.endOnDone(turn, parsed) {
				return nil
			}

		case "text":
			if loop.endOnText(turn, parsed) {
				return nil
			}

		case "tool_call":
			st.toolsRun++
			st.pendingToolCall = parsed.Name
			intentArgs := announceToolCall(ctx, turn, parsed)
			switch loop.screenBeforeWorkspaceCheck(turn, response, parsed, intentArgs) {
			case turnEnds:
				continue
			case runEnds:
				return nil
			}

			// Enforce the workspace boundary before any pre-execution gate reads
			// a path. executeToolCall repeats this check for parallel dispatch.
			if rejection := enforceWorkspacePaths(&parsed, ctx); rejection != "" {
				if loop.refuseOutsideWorkspace(parsed, intentArgs, rejection) {
					return nil
				}
				continue
			}

			switch loop.screenAfterWorkspaceCheck(turn, response, &parsed, intentArgs) {
			case turnEnds:
				continue
			case runEnds:
				return nil
			}
			if parsed.Name == "write_file" {
				var wfInput WriteFileInput
				if json.Unmarshal(parsed.Args, &wfInput) == nil {
					existingPath := resolveAgentPath(ctx, wfInput.Path)
					existing, readErr := os.ReadFile(existingPath)
					switch loop.guardExistingFileWrite(parsed, intentArgs, wfInput, existingPath, existing, readErr) {
					case turnEnds:
						continue
					case runEnds:
						return nil
					}
				}
			}

			result, steers, callOutcome, callErr := loop.runToolCall(turn, parsed, intentArgs)
			switch callOutcome {
			case turnEnds:
				continue
			case runEnds:
				return callErr
			}
			if result.Success && len(ctx.LiteralBlocks) > 0 &&
				(parsed.Name == "write_file" || parsed.Name == "edit_file" ||
					parsed.Name == "structural_edit" ||
					parsed.Name == "insert_after" || parsed.Name == "replace_lines") {
				// Literal-contract enforcement: the user's own bytes are the
				// authoritative rendering of any content they spelled out, and
				// the model measurably cannot transcribe bytes (space-prefixed
				// BPE tokens win after quotes — `BANNER = "ready"` arrives as
				// `BANNER = " ready"` deterministically). Spacing drift within
				// lines of a stated literal is repaired in place; indentation
				// is never changed (repairLiteralDrift).
				var wp struct {
					Path string `json:"path"`
				}
				if json.Unmarshal(parsed.Args, &wp) == nil && wp.Path != "" {
					lp := resolveAgentPath(ctx, wp.Path)
					if body, rerr := os.ReadFile(lp); rerr == nil {
						if fixed, repairedLits, changed := repairLiteralDrift(string(body), ctx.LiteralBlocks); changed {
							if werr := os.WriteFile(lp, []byte(fixed), 0o644); werr == nil {
								reportLiteralRepair(ctx, result, wp.Path, lp, fixed, repairedLits)
							}
						}
					}
				}
			}
			loop.recordCallEffects(turn, parsed, result)

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
					var source string
					if b, rerr := os.ReadFile(resolveAgentPath(ctx, mp)); rerr == nil {
						source = string(b)
					}
					steers.repeat = editMissSteer(mp, source)
					log.Printf("[agent] edit_file miss on %q — forcing structural_edit steer", mp)
				}
			}

			if loop.breakErrorLoop(turn, parsed, result) {
				return nil
			}
			loop.answerToolCall(turn, response, parsed, result, steers)

		default:
			askForAKnownResponseType(ctx, parsed)
		}
	}

	loop.endOutOfTurns()
	return nil
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
				// Role MUST be "user" with a "[system note]:" prefix —
				// some Jinja chat templates enforce "System message
				// must be at the beginning" and 500 on any system role
				// appended mid-conversation. Same convention the
				// lens-intervention path uses (drainCorrectives).
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

// fetchFencedContent asks the model for a file's contents in its native
// channel: a single fenced block (attempt 0 constrained by the fence grammar, a
// retry as free text).
//
// Code emitted INSIDE a JSON string pays escaping pressure on every dense line,
// and slips such as a missing close-paren, a literal \n fusing a statement into
// a comment, or list joins losing their spaces are this one channel problem.
// The envelope stays under the JSON constraint; only the file body moves to
// plain text.
//
// The sub-call is ephemeral: nothing is appended to ctx.Messages, so from the
// main conversation's view the model wrote "@fenced" and the write simply
// happened.
func fetchFencedContent(ctx *AgentContext, rawCall, path string) (string, error) {
	if err := fencedFetchRefusal(ctx, path); err != nil {
		return "", err
	}
	tag := fenceTagForPath(path)
	note := AgentMessage{Role: "user", Content: fmt.Sprintf(
		"[system note]: Now provide ONLY the complete contents of %s, as plain "+
			"code in a single fenced block, fenced with FOUR backticks (````%s ... ````) "+
			"so a ``` line inside the file stays inside it. No JSON, no "+
			"commentary, no partial file.", path, tag)}
	msgs := append(append([]AgentMessage{}, ctx.Messages...),
		AgentMessage{Role: "assistant", Content: rawCall}, note)
	fetch := &fencedFetch{ctx: ctx, path: path, tag: tag, msgs: msgs}
	// Attempts remaining are what the SESSION still allows for this path, not
	// a fresh two. Requirement: a new write_file call must not restore the
	// allowance a previous turn spent.
	remaining := maxFencedFailuresPerPath - ctx.FencedFailures[fencedKey(ctx, path)]
	for attempt := 0; attempt < remaining; attempt++ {
		if attempt > 0 && !fencedFitsRemainingBudget(ctx) {
			break // a retry that cannot finish and still be validated
		}
		// A stall on attempt 0 already raised FencedStalls to the disable
		// limit; a retry would only pay another watchdog cut for a channel
		// the session has given up on. Stop here and let the caller steer the
		// model inline, reclaiming that ~25s on the file that first stalls.
		if attempt > 0 && fencedChannelDisabledForSession(ctx) {
			break
		}
		attemptStart := time.Now()
		// Attempt 0 constrains decoding to the contract the note describes:
		// one fenced block with the requested tag. Probe M (stabilization
		// cycle 4): 44 of 49 captured sub-calls returned clean usable content
		// that way, against 19 of 49 as free text, with no empty replies at
		// all (27 of 49 as free text) and a median of 12 s against 60 s.
		// A later attempt drops the grammar, so a server that refuses it
		// still gets the free-text request this channel has always sent.
		grammar := fenceBlockGrammar(tag)
		current, _ := os.ReadFile(resolveAgentPath(ctx, path)) // absent: a new file
		if attempt > 0 || !fencedGrammarFits(string(current)) {
			grammar = rawEmissionSentinel
		}
		answer := fetch.ask(grammar, attempt, attemptStart)
		if content, done, err := fetch.settle(answer, attempt); done {
			return content, err
		}
	}
	if fetch.lastErr != nil {
		return "", fmt.Errorf("fenced resolution for %s was cut after %d attempt(s) "+
			"this session (%v); send the file inline or edit it instead",
			path, ctx.FencedFailures[fencedKey(ctx, path)], fetch.lastErr)
	}
	return "", fmt.Errorf("no fenced block after %d attempt(s) for %s this session; "+
		"send the file inline or edit it instead",
		ctx.FencedFailures[fencedKey(ctx, path)], path)
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
		out = append(out, pythonToolchain(workingDir, pyManifests))
	}

	// Node / TypeScript — pkg manager picked from lockfile.
	if hasFile(workingDir, "package.json") {
		pm, install := nodePackageManager(workingDir)
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
		runner, install, test := gradleCommands(workingDir)
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
		out = append(out, dartToolchain(workingDir))
	}

	return out
}

// pythonToolchain is the Python entry: the project's own venv interpreter when
// it has one, and the install command its lockfile or manifest calls for.
func pythonToolchain(workingDir string, pyManifests []string) Toolchain {
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
	return Toolchain{
		Name: "python", Manifests: pyManifests,
		Runner: runner, InstallCommand: install,
		TestCommand: "pytest",
	}
}

// nodePackageManager picks Node's package manager, and the command that
// installs with it, from the lockfile the project has.
func nodePackageManager(workingDir string) (string, string) {
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
	return pm, install
}

// gradleCommands picks Gradle's run, install and test commands, through the
// project's own wrapper when it has one.
func gradleCommands(workingDir string) (string, string, string) {
	runner := "gradle run"
	install := "gradle build -x test"
	test := "gradle test"
	if hasFile(workingDir, "gradlew") {
		runner = "./gradlew run"
		install = "./gradlew build -x test"
		test = "./gradlew test"
	}
	return runner, install, test
}

// dartToolchain is the Dart entry, run through Flutter when the project is a
// Flutter app.
func dartToolchain(workingDir string) Toolchain {
	runner, install := "dart run", "dart pub get"
	if hasFile(workingDir, ".flutter-plugins") || hasFile(workingDir, "flutter.yaml") {
		runner, install = "flutter run", "flutter pub get"
	}
	return Toolchain{
		Name: "dart", Manifests: pickExisting(workingDir, "pubspec.yaml", "pubspec.lock"),
		Runner: runner, InstallCommand: install,
		TestCommand: "dart test",
	}
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

// listWorkspaceFiles returns every file in the workspace, relative, for the
// planner. v3-service has no /workspace mount, so this is the only way it can
// know what already exists — and a plan that opens by recreating existing
// input is the failure this prevents.
//
// Capped: a plan does not need ten thousand paths, and the request has to stay
// small. Names only, no content.
// inspectableWorkspaceFiles is listWorkspaceFiles without ATLAS's own marker
// files, which are not something a user could be asked about.
func inspectableWorkspaceFiles(ctx *AgentContext, max int) []string {
	if ctx == nil {
		return nil
	}
	var out []string
	for _, f := range listWorkspaceFiles(ctx.WorkingDir, max+8) {
		if strings.HasPrefix(filepath.Base(f), ".atlas") {
			continue
		}
		if out = append(out, f); len(out) >= max {
			break
		}
	}
	return out
}

// handoffMessage sends back a reply that asked the user about a workspace the
// run never opened. It names only what list_directory would show.
func handoffMessage(ctx *AgentContext) string {
	files := inspectableWorkspaceFiles(ctx, 6)
	return fmt.Sprintf("Your reply asks the user for something, but you have not looked at the project, and it is "+
		"available to you: it holds files such as %s. Use list_directory, search_files and read_file to find "+
		"what you need, then answer in a single `text` reply. Ask the user only for something the files cannot "+
		"tell you.", strings.Join(files, ", "))
}

func listWorkspaceFiles(workingDir string, max int) []string {
	if workingDir == "" {
		return nil
	}
	var out []string
	skip := map[string]bool{".git": true, "node_modules": true,
		"__pycache__": true, ".venv": true, "venv": true, "dist": true}
	_ = filepath.WalkDir(workingDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skip[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if len(out) >= max {
			return fs.SkipAll
		}
		if rel, err := filepath.Rel(workingDir, path); err == nil {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// recoverTruncatedText salvages the answer from a `text` response whose JSON
// was cut off mid-string.
//
// A cut text answer used to be thrown away entirely. Observed on
// bugfind_tiebreak: the model had written 5,897 characters answering a
// question, began repeating itself, the loop detector cut the stream, and the
// user received nothing at all — because the closing quote and brace were
// missing.
//
// Only for a stream WE cut. A response the model ended on its own is not
// truncated, and guessing at one would invent content.
func recoverTruncatedText(raw string) (string, bool) {
	const marker = `"content":`
	i := strings.Index(raw, marker)
	if i < 0 {
		if i = strings.Index(raw, `"content" :`); i < 0 {
			return "", false
		}
	}
	rest := strings.TrimSpace(raw[i+len(marker):])
	if !strings.HasPrefix(rest, `"`) {
		return "", false
	}
	rest = rest[1:]

	// Walk the JSON string body by hand: it has no closing quote, so the
	// decoder cannot help. Stop at an unescaped quote if one somehow exists.
	var sb strings.Builder
	for j := 0; j < len(rest); j++ {
		c := rest[j]
		if c == '\\' && j+1 < len(rest) {
			j += writeJSONEscape(&sb, rest[j:]) - 1
			continue
		}
		if c == '"' {
			break
		}
		sb.WriteByte(c)
	}
	out := strings.TrimSpace(sb.String())
	// Too little to be worth showing, and a short fragment is more likely to
	// mislead than help.
	if len(out) < 200 {
		return "", false
	}
	return out, true
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

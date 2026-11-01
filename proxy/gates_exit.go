// The exit gates: what a run is held to before it may finish, and the state they read.

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// planGateMinScore is the plan-quality floor below which the plan-completion
// gate does not fire. The planner reports WinningScore per plan; a weak plan
// blocking a finished task is worse than no gate, and 0.6 keeps the gate on the
// plans the planner itself rates as sound (live plans score about 0.80).
const planGateMinScore = 0.6

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
		ctx.Messages = append(ctx.Messages, systemNote(msg))
	}
	s.correctives = nil
}

// systemNote is a note from the proxy to the model. runState.correctives says
// why it goes as a user message.
func systemNote(text string) AgentMessage {
	return AgentMessage{Role: "user", Content: "[system note]: " + text}
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
		s.observePassedRun(ctx, turn, command, ev)
	default:
		s.observeUnprovenPass(ctx, turn, command, ev, declared)
	}
}

// observePassedRun records a run that showed the work working, which clears
// every earlier failure.
func (s *runState) observePassedRun(ctx *AgentContext, turn int, command string, ev commandEvidence) {
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
}

// observeUnprovenPass handles a command that passed and ran nothing: a static
// check, or a declared command that runs nothing. It clears only its own
// earlier failure, and it is recorded only when the client declared it, so the
// declaration can be discharged; the record binds no file (showsWorking).
func (s *runState) observeUnprovenPass(ctx *AgentContext, turn int, command string, ev commandEvidence, declared bool) {
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

// listWorkspaceFiles returns every file in the workspace, relative, for the
// planner. v3-service has no /workspace mount, so this is the only way it can
// know what already exists — and a plan that opens by recreating existing
// input is the failure this prevents.
//
// Capped: a plan does not need ten thousand paths, and the request has to stay
// small. Names only, no content.
// inspectableWorkspaceFiles is listWorkspaceFiles without ATLAS's own marker
// files, which are not something a user could be asked about.
func inspectableWorkspaceFiles(ctx *AgentContext, limit int) []string {
	if ctx == nil {
		return nil
	}
	var out []string
	for _, f := range listWorkspaceFiles(ctx.WorkingDir, limit+8) {
		if strings.HasPrefix(filepath.Base(f), ".atlas") {
			continue
		}
		if out = append(out, f); len(out) >= limit {
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

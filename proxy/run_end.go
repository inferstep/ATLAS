// How a run ends: its one terminal, its time budget, and the summary the client is given.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// --- Phase 2B: the one terminal emitter -------------------------------------
//
// Thirteen producers each built their own done payload, so "did this run
// finish?" was answered by matching English in `summary`. A caller could not
// tell a completion from a loop-breaker without knowing every phrase the
// proxy might use, and the broker's own done envelope said "success": true
// unconditionally -- including for every stop.
//
// Every terminal now goes through here. It fires at most once per session:
// a timeout racing a completion produces one event, not two, and whichever
// arrives first is the outcome.
func emitTerminal(ctx *AgentContext, st *runState, status TerminalStatus, reason, summary string) {
	if !status.Classified() {
		// A producer that did not name its outcome does not get to imply one.
		status, reason = TerminalIncomplete, "unclassified_producer"
	}
	ctx.terminalOnce.Do(func() {
		// Nothing may be delivered from here on. An authorization that
		// outlived the turn it was granted in would let a candidate land
		// against a workspace whose story has already been told.
		//
		// Here rather than in finalizeCompletion because that function is
		// also called on the path that BOUNCES for debt recovery and keeps
		// running: retiring there would leave the recovery turn unable to
		// deliver the thing it was bounced to write. This is the emission
		// itself, and it happens once.
		retireAuthorizationGrants(ctx, grantTerminal)
		status, reason = withheldForOpenRepairs(ctx, st, status, reason)
		ctx.TerminalStatus = status
		ctx.TerminalReason = reason
		ctx.TerminalUnresolved = unresolvedGateNames(st)
		if st != nil && st.pendingToolCall != "" {
			// The outstanding call is answered first, so tool_call and
			// tool_result stay balanced at every exit.
			ctx.Stream("tool_result", map[string]interface{}{
				"tool":    st.pendingToolCall,
				"success": false,
				"data":    json.RawMessage("null"),
				"error":   "not run — the session stopped before this call executed",
				"elapsed": "0s",
			})
			st.pendingToolCall = ""
		}
		// `summary` keeps its exact legacy meaning and position. `status` and
		// `reason` are additive, so a consumer that never learned about them
		// reads the same event it always did -- and reads the same TRUTH,
		// which is what honestTerminalSummary enforces.
		done := map[string]string{
			"summary": honestTerminalSummary(ctx, st, status, reason, summary),
			"status":  string(status),
			"reason":  reason,
		}
		// Additive: present only when an exit gate spent its bounces with its
		// finding still true, so a consumer reading status alone can still
		// tell a clean completion from one with caveats.
		if ctx.TerminalUnresolved != "" {
			done["unresolved"] = ctx.TerminalUnresolved
		}
		// Additive: the files still unparseable at the end, whatever the
		// reason, so a consumer can find every hand-off without reading
		// the summary.
		if open := st.openRepairs(); len(open) > 0 {
			names := make([]string, 0, len(open))
			for _, r := range open {
				names = append(names, r.Rel)
			}
			done["repair_open"] = strings.Join(names, ",")
			st.emitRepairHandoff(ctx, reason)
		}
		ctx.Stream("done", done)
	})
}

// withheldForOpenRepairs is the terminal to report: no run that leaves a file it
// wrote unparseable is reported completed, whichever path reached here
// (repair.go).
func withheldForOpenRepairs(ctx *AgentContext, st *runState, status TerminalStatus, reason string) (TerminalStatus, string) {
	if st == nil {
		return status, reason
	}
	st.refreshRepairs(ctx, st.turn, "")
	if open := st.openRepairs(); len(open) > 0 && status.Completed() {
		log.Printf("[agent] terminal %s/%s with %s unparseable — reporting repair_unfinished",
			status, reason, repairNames(open))
		return TerminalIncomplete, "repair_unfinished"
	}
	return status, reason
}

// terminalCompletionAllowed decides whether a model-issued `done` may be
// called completed. It answers from the workspace as it is right now, never
// from the run's history, so a delete's success can never authorise the
// completion by itself. Existence is not validity, an earlier successful
// write is not the current bytes, and a removal is not an achievement unless
// removal was the task. The proxy cannot infer that from the model; only a
// deletion the user approved through the permission endpoint counts
// (fulfilledApprovedDeletion).
func terminalCompletionAllowed(ctx *AgentContext, expected []string) (bool, string) {
	if blockingTombstone(ctx) {
		// Something was deleted or moved. Whether that WAS the task is not
		// knowable here, so completion is not claimable here.
		return false, "delete_intent_unestablished"
	}
	paths := declaredOrOwnedDeliverables(ctx, expected)
	if len(paths) == 0 {
		// A run whose work was removing files the user approved has an
		// obligation and met it; saying "no file obligation" would be false.
		if len(approvedDeletionPaths(ctx)) > 0 {
			return true, "approved_deletions_demonstrated"
		}
		// Nothing declared and nothing written: there is no file obligation
		// to demonstrate.
		return true, "no_file_obligation"
	}
	if deliverablesDemonstrablyValid(ctx, paths) {
		// The reason names the evidence. Code and pages that nothing in this
		// run ran are current and parse; that is all the completion rests on.
		if len(unexecutedDeliverables(ctx, expected)) > 0 {
			return true, "deliverables_parse_only"
		}
		return true, "deliverables_demonstrated"
	}
	return false, "deliverables_not_demonstrated"
}

// unexecutedDeliverables lists the deliverables that can be run -- code in an
// executable language, and HTML pages, which run in a browser -- that no
// current run in this session showed working (pathCoverageSatisfied).
// Documents and data have nothing to run and are not listed.
func unexecutedDeliverables(ctx *AgentContext, expected []string) []string {
	var out []string
	for _, rel := range declaredOrOwnedDeliverables(ctx, expected) {
		resolved := resolveAgentPath(ctx, rel)
		ext := strings.ToLower(filepath.Ext(resolved))
		if meta, gated := syntaxGateLanguages[ext]; !(gated && meta.Executable) && ext != ".html" && ext != ".htm" {
			continue
		}
		h := fileSHA256(ctx, resolved)
		if h == "" || pathCoverageSatisfied(ctx, resolved, h) {
			continue
		}
		out = append(out, rel)
	}
	return out
}

// declaredOrOwnedDeliverables is the union of what the run said it would
// produce and what it actually wrote, minus anything deliberately removed.
// Sorted so a summary and a decision never disagree on order.
func declaredOrOwnedDeliverables(ctx *AgentContext, expected []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(rel string) {
		if rel == "" || seen[rel] {
			return
		}
		seen[rel] = true
		out = append(out, rel)
	}
	for _, rel := range expected {
		add(rel)
	}
	if ctx != nil {
		ctx.LedgerMu.Lock()
		for key, d := range ctx.Ledger {
			if d.Tombstoned || d.Generation == 0 {
				continue
			}
			rel := key
			if r, err := filepath.Rel(ctx.WorkingDir, key); err == nil && !strings.HasPrefix(r, "..") {
				rel = r
			}
			add(rel)
		}
		ctx.LedgerMu.Unlock()
	}
	sort.Strings(out)
	return out
}

// blockingTombstone reports whether anything was removed that this run cannot
// account for. A plain deletion blocks unless the user approved it through
// the permission endpoint and it is still carried out
// (fulfilledApprovedDeletion): whether removing a file was the task is not
// knowable from the workspace, and a delete must not authorise its own
// completion.
//
// A move is a different fact and does not need intent inferred. The source is
// gone AND the bytes are somewhere the ledger can point at -- TombstoneReason
// records exactly where, as `moved:<canonical destination>` -- so the removal
// is accounted for by the artifact that replaced it. That is only true when
// every part of it is demonstrated NOW: the reason parses, the source is
// confirmed absent on disk, the destination is readable, the ledger's record
// describes the bytes actually there, and the destination clears the same
// deliverable contract every other artifact clears. Anything less blocks.
//
// The contract is reused rather than restated, deliberately: a move must not
// be an easier way to demonstrate a file than writing one. Restoration stays
// prohibited on the tombstone either way -- this decides completion, not
// whether the old path can come back. An approved deletion whose path is back
// on disk also blocks.
func blockingTombstone(ctx *AgentContext) bool {
	if ctx == nil {
		return false
	}
	ctx.LedgerMu.Lock()
	type tomb struct{ key, reason string }
	var tombs []tomb
	for k, d := range ctx.Ledger {
		if d.Tombstoned {
			tombs = append(tombs, tomb{k, d.TombstoneReason})
		}
	}
	ctx.LedgerMu.Unlock()

	for _, t := range tombs {
		if demonstratedMove(ctx, t.key, t.reason) {
			continue
		}
		if fulfilledApprovedDeletion(ctx, t.key, t.reason) {
			continue
		}
		return true
	}
	// A path the user approved removing that is back on disk is a
	// contradiction, not a completion: the approval described a removal this
	// run then undid, and recreating it does not settle what was authorised.
	// The tombstone is gone by then, so this is checked from the record.
	if undoneApprovedDeletion(ctx) {
		return true
	}
	return false
}

// undoneApprovedDeletion reports whether any deletion the user approved has
// since come back.
func undoneApprovedDeletion(ctx *AgentContext) bool {
	if ctx == nil {
		return false
	}
	ctx.mu.Lock()
	keys := make([]string, 0, len(ctx.fulfilledDeletions))
	for k := range ctx.fulfilledDeletions {
		keys = append(keys, k)
	}
	ctx.mu.Unlock()
	for _, k := range keys {
		if _, err := os.Lstat(k); err == nil {
			return true
		}
	}
	return false
}

// fulfilledApprovedDeletion answers whether a plain deletion tombstone is one
// the USER approved and the system then carried out.
//
// Every fact is re-checked here, at terminal time, against the workspace as it
// is now: the record is for this exact canonical path, the generation is the
// one that deletion produced, the path is still absent, the tombstone is a
// deletion rather than a move, restoration is still prohibited, and the delete
// debt is settled. A path that came back is a newer generation and blocks
// again -- which is the point of binding the generation rather than the name.
//
// The record itself can only exist if the decision arrived through the
// permission endpoint. No word of the user's message, and no claim of the
// model's, reaches this.
func fulfilledApprovedDeletion(ctx *AgentContext, key, reason string) bool {
	if reason != "deleted" {
		return false
	}
	f, ok := fulfilledDeletionFor(ctx, key)
	if !ok {
		return false
	}
	if _, err := os.Lstat(key); !os.IsNotExist(err) {
		return false // it is back: this record describes older bytes
	}
	ctx.LedgerMu.Lock()
	d := ctx.Ledger[key]
	var live bool
	if d != nil {
		live = d.Tombstoned && d.TombstoneReason == "deleted" &&
			d.RestoreProhibited && d.Generation == f.Generation
	}
	ctx.LedgerMu.Unlock()
	return live
}

// approvedDeletionPaths lists, in a stable order, the paths this run removed
// with the user's approval. Bounded for disclosure.
func approvedDeletionPaths(ctx *AgentContext) []string {
	if ctx == nil {
		return nil
	}
	ctx.LedgerMu.Lock()
	keys := make([]string, 0, len(ctx.Ledger))
	for k, d := range ctx.Ledger {
		if d.Tombstoned && d.TombstoneReason == "deleted" {
			keys = append(keys, k)
		}
	}
	ctx.LedgerMu.Unlock()
	var out []string
	for _, k := range keys {
		if fulfilledApprovedDeletion(ctx, k, "deleted") {
			out = append(out, relativeToWorkspace(ctx, k))
		}
	}
	sort.Strings(out)
	return out
}

// relativeToWorkspace renders a canonical path the way the user wrote it.
func relativeToWorkspace(ctx *AgentContext, key string) string {
	if ctx == nil || ctx.WorkingDir == "" {
		return key
	}
	if rel, err := filepath.Rel(ctx.WorkingDir, key); err == nil &&
		!strings.HasPrefix(rel, "..") {
		return rel
	}
	return key
}

// demonstratedMove answers whether one tombstone is a relocation this run can
// point at, rather than a removal it cannot explain.
func demonstratedMove(ctx *AgentContext, srcKey, reason string) bool {
	dest := strings.TrimPrefix(reason, "moved:")
	if dest == reason || strings.TrimSpace(dest) == "" {
		return false // a plain deletion, or a reason that says nothing
	}
	// The source has to be gone right now, not merely reported gone.
	if _, err := os.Stat(srcKey); !os.IsNotExist(err) {
		return false
	}
	data, ok := readLedgerBytes(dest)
	if !ok {
		return false
	}
	ctx.LedgerMu.Lock()
	d := ctx.Ledger[dest]
	var current string
	var tombstoned bool
	if d != nil {
		current, tombstoned = d.CurrentHash, d.Tombstoned
	}
	ctx.LedgerMu.Unlock()
	// The destination must be a live deliverable the ledger still describes.
	if d == nil || tombstoned || current != hashBytes(data) {
		return false
	}
	// And it must clear the same bar as any other artifact.
	if !deliverablesDemonstrablyValid(ctx, []string{dest}) {
		return false
	}
	// Whatever the session still owes on this move is owed regardless. The
	// debt gate in finalizeCompletion reads it a few lines later and names it
	// specifically, so pre-empting it here would only replace a precise
	// terminal with a vaguer one.
	return true
}

// --- Phase 2B: the server-owned session budget ------------------------------
//
// The proxy had no clock of its own. A session ran until the model stopped,
// the client gave up, or a detector fired, and a client that timed out
// mid-stream took the only explanation with it -- the Stage-1 sessions that
// reached 590s did so against the HARNESS cap, not a server one, and the
// server never got to say what it had.
//
// The budget is owned here, and it is split: work stops one reserve early so
// the reserve can be spent on stopping cleanly and saying so.

const (
	defaultSessionTotalSec = 600
	sessionReserve         = 30 * time.Second
	minSessionTotalSec     = 120
	maxSessionTotalSec     = 3600
)

// sessionBudget returns the total session limit and the reserve held back for
// finalisation. ATLAS_AGENT_SESSION_TIMEOUT_SEC overrides the total within
// conservative bounds; anything malformed, zero, negative or out of range
// falls back to the default and says so, because a silently ignored operator
// setting is worse than no setting.
func sessionBudget() (total, reserve time.Duration) {
	total = defaultSessionTotalSec * time.Second
	raw := strings.TrimSpace(os.Getenv("ATLAS_AGENT_SESSION_TIMEOUT_SEC"))
	if raw == "" {
		return total, sessionReserve
	}
	n, err := strconv.Atoi(raw)
	switch {
	case err != nil:
		log.Printf("[agent] ATLAS_AGENT_SESSION_TIMEOUT_SEC=%q is not a number — using %ds",
			raw, defaultSessionTotalSec)
	case n < minSessionTotalSec:
		log.Printf("[agent] ATLAS_AGENT_SESSION_TIMEOUT_SEC=%d is below the %ds floor — using %ds",
			n, minSessionTotalSec, defaultSessionTotalSec)
	case n > maxSessionTotalSec:
		log.Printf("[agent] ATLAS_AGENT_SESSION_TIMEOUT_SEC=%d is above the %ds ceiling — using %ds",
			n, maxSessionTotalSec, defaultSessionTotalSec)
	default:
		total = time.Duration(n) * time.Second
	}
	return total, sessionReserve
}

// finalizeOnWorkDeadline is what the reserve is for. It runs after the work
// context is done and before the handler returns, on the response lifetime.
//
// Order matters: nothing may look at the workspace until the things that
// could still be writing to it have been confirmed gone.
func finalizeOnWorkDeadline(ctx *AgentContext, st *runState) {
	// 1. Stop anything still running on the work context.
	if ctx.cancelWork != nil {
		ctx.cancelWork()
	}
	// 2. Reap this session's background jobs and confirm they exited. Only
	// this session's -- another session's server is not ours to kill.
	reapSessionBackgroundJobs(ctx)
	// 3. Now the workspace is quiet, so a hash means something. Re-read every
	// tracked path: a job killed mid-write leaves bytes nobody validated.
	invalidateTrackedValidation(ctx)
	// 4. Decide restoration per path, under the Phase 3B rules. A timeout
	// does not relax any of them.
	recovered := restoreSaferDeliverables(ctx)
	// 5. One terminal, on the response lifetime, inside the reserve. A
	// timeout never claims completion, whatever is on disk afterwards.
	wrote := st != nil && st.madeProductiveChange
	emitTerminal(ctx, st, TerminalTimedOut, "work_deadline",
		sessionTimeoutSummary(ctx, wrote, recovered))
}

// reapSessionBackgroundJobs stops the jobs THIS session started and waits for
// each to be confirmed gone. A job that cannot be confirmed leaves the
// workspace hazard raised, which is what keeps restoration from touching a
// file something may still be writing.
func reapSessionBackgroundJobs(ctx *AgentContext) {
	if ctx == nil || len(ctx.BackgroundJobs) == 0 {
		return
	}
	// Once they are confirmed gone, record what they left behind.
	defer settleBackgroundEffects(ctx)
	ids := make([]string, 0, len(ctx.BackgroundJobs))
	for id := range ctx.BackgroundJobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		out, err := sandboxStopBackground(ctx, id)
		delete(ctx.BackgroundJobs, id)
		if err != nil {
			log.Printf("[agent] could not stop background job %s: %v", id, err)
			continue
		}
		if out.ExitCode != nil {
			clearWorkspaceHazard(ctx, id)
			continue
		}
		log.Printf("[agent] background job %s did not report an exit code — "+
			"the workspace stays marked as possibly still being written", id)
	}
}

// sessionTimeoutSummary is the terminal a timed-out session ends on. It says
// the run ran out of time and what state the files are in; it never says the
// work is done, and a successful restore does not change that.
func sessionTimeoutSummary(ctx *AgentContext, wrote bool, recovered []restoreDecision) string {
	var sb strings.Builder
	sb.WriteString("Stopped: the session ran out of time before the work finished")
	switch {
	case !wrote:
		sb.WriteString(", and nothing was written to disk")
	default:
		sb.WriteString(". Anything already written is still on disk, unverified")
	}
	sb.WriteString(". Try a smaller, more specific request.")
	sb.WriteString(restorationDisclosure(recovered))
	sb.WriteString(liveBackgroundJobNote(ctx))
	return sb.String()
}

// finishCancelledRun tells apart the two ways a run can stop early, because
// they are not the same event and must not be reported as one.
//
// The work deadline is OURS: the client is still there, the reserve is
// unspent, and the session owes an explanation. A client disconnect is not a
// server timeout -- the response channel is gone, so there is nobody to tell,
// and claiming timed_out into a closed socket would put a fact in the record
// that nothing observed.
//
// Either way the work stops and this session's background jobs are reaped.
func finishCancelledRun(ctx *AgentContext, st *runState, turn int) error {
	clientGone := ctx.RequestCtx != nil && ctx.RequestCtx.Err() != nil
	if clientGone {
		log.Printf("[agent] client disconnected at turn %d — cancelling work and reaping jobs", turn)
		if ctx.cancelWork != nil {
			ctx.cancelWork()
		}
		reapSessionBackgroundJobs(ctx)
		return ctx.Ctx.Err()
	}
	if errors.Is(ctx.Ctx.Err(), context.DeadlineExceeded) {
		log.Printf("[agent] work deadline reached at turn %d — finalising within the reserve", turn)
		finalizeOnWorkDeadline(ctx, st)
		return nil
	}
	// An explicit POST /cancel: the caller asked to stop, so the run ends
	// incomplete rather than pretending it ran out of time.
	log.Printf("[agent] cancelled at turn %d: %v", turn, ctx.Ctx.Err())
	if ctx.cancelWork != nil {
		ctx.cancelWork()
	}
	reapSessionBackgroundJobs(ctx)
	emitTerminal(ctx, st, TerminalIncomplete, "cancelled",
		"Stopped: the run was cancelled before the work finished."+liveBackgroundJobNote(ctx))
	return nil
}

// --- The summary a non-completed run is allowed to carry ---------------------
//
// A client that reads only `summary` would otherwise read a success from the
// model's own prose ("I have successfully implemented ...") over an artifact
// nothing verified, or get no summary at all.
//
// So the server owns the sentence whenever it does not own a completion. A
// completed status keeps the model's account, because the exact-hash gate has
// already agreed with it; when the completion rests only on a parse, a claim
// that the code ran is labelled (parseOnlySummary).

// completionClaims are the phrases a finished run is allowed to use. Matching
// is on the claim, not on the word: "no verification command completed
// successfully" is a report of failure and must not trip this, so each phrase
// is checked for a negation immediately before it.
var completionClaims = []string{
	"successfully implemented", "successfully created", "successfully wrote",
	"successfully added", "successfully fixed", "successfully completed",
	"i have successfully", "made your change", "the change is on disk",
	"final product", "task is complete", "task is done", "work is complete",
	"is now complete", "everything works", "all tests pass", "it works correctly",
	"correctly handles", "correctly processes", "correctly implements",
}

// negators immediately preceding a claim invert it.
var claimNegators = []string{
	"no ", "not ", "never ", "cannot ", "can't ", "could not ", "couldn't ",
	"did not ", "didn't ", "without ", "nothing ", "fails to ", "failed to ",
	"unverified", "unconfirmed",
}

// completionClaimIn returns the first unnegated completion claim in s, or "".
func completionClaimIn(s string) string {
	return claimIn(s, completionClaims)
}

// claimIn returns the first of claims that s makes unnegated, or "".
func claimIn(s string, claims []string) string {
	low := strings.ToLower(s)
	for _, claim := range claims {
		from := 0
		for {
			i := strings.Index(low[from:], claim)
			if i < 0 {
				break
			}
			at := from + i
			if !negatedBefore(low, at) {
				return claim
			}
			from = at + len(claim)
		}
	}
	return ""
}

// negatedBefore reports a negator in the 48 characters before position at.
func negatedBefore(low string, at int) bool {
	window := low[max(0, at-48):at]
	for _, n := range claimNegators {
		if strings.Contains(window, n) {
			return true
		}
	}
	return false
}

// honestMarkers are the ways a summary can already be saying the run did not
// finish. One of them must be present on every non-completed terminal.
var honestMarkers = []string{
	"stopped", "ran out of time", "ran out of turns", "nothing was written",
	"cannot say the task is done", "did not confirm", "not reported as finished",
	"unverified", "before finishing", "was cancelled", "could not continue",
	"not shown to be valid", "check them before relying", "run it yourself",
	"partial", "did not complete",
}

func hasHonestMarker(s string) bool {
	low := strings.ToLower(s)
	for _, m := range honestMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// runNotConfirmedNote ends the summary of a run that did not finish.
const runNotConfirmedNote = " This run did not confirm the task was complete."

// deletionSummaryLimit bounds how many paths a completion summary names before
// counting the rest, so a large tidy-up stays readable.
const deletionSummaryLimit = 5

// honestTerminalSummary is the last thing between a terminal and the client.
//
// For a completed status the account stands, because the gate authorised the
// claim; what a spent heuristic gate still found is appended as a caveat. For
// every other status it guarantees three properties --
// there is a summary, it carries no completion claim, and it says plainly that
// the task was not confirmed finished.
func honestTerminalSummary(ctx *AgentContext, st *runState, status TerminalStatus,
	reason, summary string) string {
	if status.Completed() {
		out := completedTerminalSummary(ctx, reason, summary)
		var expected []string
		if st != nil {
			expected = st.expectedOutputs
		}
		if unrun := unexecutedDeliverables(ctx, expected); len(unrun) > 0 {
			out = parseOnlySummary(out, unrun)
		}
		return out + unresolvedGateCaveats(st) + v3FallbackNote(ctx)
	}
	out := strings.TrimSpace(summary)
	if claim := completionClaimIn(out); claim != "" {
		// A producer, or prose that reached one, is claiming completion on a
		// run that did not complete. The server replaces it outright rather
		// than editing around it.
		log.Printf("[agent] terminal (%s/%s) carried the completion claim %q — replacing the summary",
			status, reason, claim)
		out = ""
	}
	// A file still unparseable is handed to the user with what was tried
	// (repairHandoff). For repair_unfinished that is the whole account;
	// after any other ending it follows the ending's own summary.
	if handoff := repairHandoff(st, reason); handoff != "" {
		if reason == "repair_unfinished" || out == "" {
			out = handoff
		} else {
			out += "\n\n" + handoff
		}
	}
	if out == "" {
		out = serverTerminalFallback(ctx, st, status, reason)
	}
	if !hasHonestMarker(out) {
		out += runNotConfirmedNote
	}
	return out + v3FallbackNote(ctx)
}

// executionClaims are what a model says when it believes the code ran:
// claims a parse cannot support. Checked with completionClaimIn's negation
// rule, so "nothing verified it works" is not one.
var executionClaims = []string{
	"everything works", "all tests pass", "tests pass", "works correctly", "works as expected",
	"fully working", "fully functional", "runs correctly", "runs successfully",
	"runs without error", "i verified", "verified that", "verified it", "i tested",
	"tested it", "i ran it", "i ran the", "correctly handles", "correctly processes",
	"correctly implements",
}

// parseOnlySummary is the account of a completion that rests on a parse. The
// model's prose stands when it claims no more than that; when it says the
// code works or its tests pass, the server's sentence comes first and the
// model's account is labelled, because nothing in the run checked it
// (audit P-agent-3/INTEGRITY#1: "All tests pass and everything works" was
// shown word for word over an index.html nothing had loaded).
func parseOnlySummary(prose string, unrun []string) string {
	named := unrun
	more := ""
	if len(named) > 3 {
		more = fmt.Sprintf(" and %d more", len(named)-3)
		named = named[:3]
	}
	fact := fmt.Sprintf("%s%s parse, but nothing in this run ran them.", strings.Join(named, ", "), more)
	if len(unrun) == 1 {
		fact = fmt.Sprintf("%s parses, but nothing in this run ran it.", unrun[0])
	}
	prose = strings.TrimSpace(prose)
	switch {
	case prose == "":
		return fact
	case claimIn(prose, executionClaims) != "":
		return fact + "\n\nThe agent's own account, which nothing in this run checked:\n" + truncateStr(prose, 1200)
	}
	return prose + "\n\n" + fact
}

// completedTerminalSummary is the account of a completion the gate authorised.
func completedTerminalSummary(ctx *AgentContext, reason, summary string) string {
	// A run whose work was removing files says which ones, from the ledger
	// rather than from anything the model wrote. Paths only: no hashes, no
	// ledger vocabulary, no permission machinery.
	if reason == "approved_deletions_demonstrated" {
		if paths := approvedDeletionPaths(ctx); len(paths) > 0 {
			named := paths
			more := ""
			if len(named) > deletionSummaryLimit {
				more = fmt.Sprintf(" and %d more", len(named)-deletionSummaryLimit)
				named = named[:deletionSummaryLimit]
			}
			line := fmt.Sprintf("Deleted %s%s, as you approved. Confirmed gone.",
				strings.Join(named, ", "), more)
			if s := strings.TrimSpace(summary); s != "" {
				return line + "\n\n" + s
			}
			return line
		}
	}
	return summary
}

// serverTerminalFallback is what the user reads when the producer had nothing
// to say, or said something the run cannot support. It reports only facts the
// server holds: what the outcome was, whether anything is on disk, and whether
// the deliverables were shown to be valid.
func serverTerminalFallback(ctx *AgentContext, st *runState, status TerminalStatus, reason string) string {
	var sb strings.Builder
	switch {
	case reason == "clarification_requested":
		sb.WriteString("Waiting for your answer: the reply asks you a question, so the task is not reported as finished.")
	case reason == "investigation_handed_back":
		sb.WriteString("Stopped: the reply asks you to find something in the project files instead of reading " +
			"them, so the task is not reported as finished.")
	case reason == "deliverables_not_demonstrated" && len(uncheckableDeliverables(ctx, st)) > 0:
		sb.WriteString("Stopped: ATLAS has no check for " + strings.Join(uncheckableDeliverables(ctx, st), ", ") +
			", so this run could not confirm the task was complete.")
	case unresolvedFinding(st, reason) != "":
		sb.WriteString("Stopped: a check at the end still failed after the agent was sent back to fix it — " +
			unresolvedFinding(st, reason) + ".")
	case status == TerminalTimedOut:
		sb.WriteString("Stopped: the session ran out of time before the work finished.")
	case status == TerminalFailed:
		sb.WriteString("Stopped: the run could not continue.")
	case status == TerminalStopped:
		sb.WriteString("Stopped: the run was cut short before the work finished.")
	default:
		sb.WriteString("Stopped: the run ended without finishing the task.")
	}

	wrote := st != nil && st.madeProductiveChange
	switch {
	case !wrote:
		sb.WriteString(" Nothing was written to disk.")
	default:
		var expected []string
		if st != nil {
			expected = st.expectedOutputs
		}
		paths := declaredOrOwnedDeliverables(ctx, expected)
		switch {
		case len(paths) == 0:
			sb.WriteString(" Changes were written to disk and nothing verified them.")
		case deliverablesDemonstrablyValid(ctx, paths):
			sb.WriteString(" What is on disk parses, but nothing in this run verified it does " +
				"the right thing.")
		default:
			sb.WriteString(" Changes are on disk and were not shown to be valid — treat them " +
				"as unverified.")
		}
	}
	sb.WriteString(runNotConfirmedNote)
	sb.WriteString(liveBackgroundJobNote(ctx))
	return sb.String()
}

// uncheckableDeliverables names the deliverables no check applies to and that
// are not prose: a file of a kind outside the syntax registry, which
// completion cannot demonstrate (audit GB-4#5). The reason stays
// deliverables_not_demonstrated; this only says why, instead of "the run
// ended without finishing the task".
func uncheckableDeliverables(ctx *AgentContext, st *runState) []string {
	var expected []string
	if st != nil {
		expected = st.expectedOutputs
	}
	var out []string
	for _, rel := range declaredOrOwnedDeliverables(ctx, expected) {
		if _, gated := syntaxGateLanguages[strings.ToLower(filepath.Ext(rel))]; gated || isDocumentAsset(rel) {
			continue
		}
		out = append(out, rel)
		if len(out) == 3 {
			break
		}
	}
	return out
}

// unresolvedFinding is the finding of the spent gate a terminal reason names,
// or "".
func unresolvedFinding(st *runState, reason string) string {
	if st == nil {
		return ""
	}
	for _, u := range unresolvedReasons {
		if u.reason == reason {
			return st.unresolvedGates[u.gate]
		}
	}
	return ""
}

// modelProseIfAuthorized passes the model's own account through only where the
// completion gate has already agreed with it. Elsewhere it returns "", and the
// server composes the summary instead.
func modelProseIfAuthorized(status TerminalStatus, prose string) string {
	if status.Completed() {
		return prose
	}
	return ""
}

// finalizeCompletion is the one decision both exits make, in one order.
//
// The status reads the same evidence the summary does. Decided from the
// deliverable evidence alone, a run that wrote nothing, or whose only write was
// correctly refused, has no ledger entry and so no obligation to fail, and
// would report completed / no_file_obligation while its own summary says
// nothing was written.
//
// Nothing new is inferred. wantsStateChange, the action demand and the
// verification demand already exist and are already trusted enough to rewrite
// the user's summary; this lets the machine-readable half read the same
// evidence. No prose is parsed, no obligation is invented for a path the
// session never owned, and no more-specific existing failure is replaced.
//
// The order is fixed:
//
//  1. a file this session left unparseable (repair_unfinished);
//  2. a deliverable failure keeps its own, more specific reason;
//  3. work was demanded on disk and none landed;
//  4. verification was demanded and none passed, including the work
//     contract's bound verification;
//  5. an authorized delivery is not yet settled;
//  6. a prose reply after work in work mode;
//  7. a reply that leaves work outstanding, declares itself incomplete,
//     hands the investigation back, misses the asked scope, or asks the
//     user;
//  8. background work still live;
//  9. unresolved mutation debt;
//  10. an exit gate spent with its finding still true;
//  11. otherwise the completion stands.
//
// completedReason lets an exit name its own reason for a genuine completion
// (the text exit says text_reply); empty keeps the deliverable evidence's.
func finalizeCompletion(ctx *AgentContext, st *runState, userMessage, completedReason string) (TerminalStatus, string) {
	// Settle what can be settled FIRST, so the evidence the rest of this
	// function reads is about a workspace nothing is still writing to.
	liveJobs := settleBackgroundHazard(ctx)

	// A file this session left unparseable outranks every other reason: it
	// is the most specific thing the user needs to know, and the final
	// message says what was tried on it (repairHandoff).
	st.refreshRepairs(ctx, st.turn, "")
	if len(st.openRepairs()) > 0 {
		return TerminalIncomplete, "repair_unfinished"
	}

	ok, why := terminalCompletionAllowed(ctx, st.expectedOutputs)
	if !ok {
		return TerminalIncomplete, why
	}
	if st.actionDemandedAndUnmet(ctx, userMessage) {
		return TerminalIncomplete, "action_demanded_unmet"
	}
	if st.verificationDemandedAndUnmet() {
		return TerminalIncomplete, "verification_demanded_unmet"
	}
	// A declared work request cannot finish on prose, existence, a parse, or a
	// command that named nothing. Evaluated here, in the one finalizer both the
	// model's `done` and the text exit call, so the two cannot drift.
	if d := decideVerificationDemand(ctx, ctx.TaskContract, st.expectedOutputs); d.Required && !d.Met {
		log.Printf("[agent] work contract: no current bound verification for %q", d.Missing)
		return TerminalIncomplete, "verification_demanded_unmet"
	}
	// A candidate this run authorized and delivered owes one more thing that
	// could not be answered before it landed: that it is still there, at the
	// bytes it was authorized at, with the session's own record agreeing.
	//
	// Scoped to targets this run actually delivered to. An output that was
	// never produced is owed by missingExpectedOutputs, which already owns
	// that question; claiming it here too would be a second rule for one
	// obligation.
	if owed, why := postDeliverySettlementOwed(ctx); owed {
		log.Printf("[agent] authorized delivery is not settled: %s", why)
		return TerminalIncomplete, "post_delivery_settlement_pending"
	}
	// The model asking to finish is part of the evidence. A prose reply after a
	// mutation is a chat turn, not a completion claim, so it stays honest even
	// when everything else is satisfied. Bounded continuation is a later slice.
	if tc := ctx.TaskContract; completedReason == "text_reply" && tc != nil &&
		tc.TaskMode == TaskModeWork && st.madeProductiveChange {
		return TerminalIncomplete, "work_not_declared_complete"
	}
	if reason := st.replyIncompleteReason(); reason != "" {
		return TerminalIncomplete, reason
	}
	// Something may still be writing. A hash taken now describes an instant,
	// not a result, and nothing here can tell a quiet process from a finished
	// one -- only a confirmed exit can.
	if len(liveJobs) > 0 || workspaceHazardous(ctx) {
		return TerminalIncomplete, "background_work_unresolved"
	}
	// Settle first, from the workspace as it is now. A debt can become
	// resolved without another tool call -- a deletion of a path that was
	// already absent is owed and discharged by the same fact -- and settling
	// only after an execution meant those never retired. This is the existing
	// structural rule (confirmed absence, demonstrated bytes, a completed
	// move), evaluated at the moment the decision is made.
	settleMutationDebt(ctx, st)
	// Work the model asked for, that the system permitted, and that never
	// reached a state anything could check. A success elsewhere does not
	// settle it.
	if hasUnresolvedDebt(st) {
		return TerminalIncomplete, "unresolved_mutation_debt"
	}
	// A gate that spent its bounces with its finding still true, where the
	// finding is a fact about the delivered work: a claim-check gap (a
	// template the code renders does not exist), a file with a parse warning
	// that never ran, a reply citing files the run never read. Last, so every
	// more specific reason above keeps its place. The heuristic gates stand as
	// caveats in the summary instead.
	if reason := st.unresolvedGateReason(); reason != "" {
		return TerminalIncomplete, reason
	}
	if completedReason != "" {
		return TerminalCompleted, completedReason
	}
	return TerminalCompleted, why
}

// replyIncompleteReason is why the closing itself is not an answer, or "".
func (st *runState) replyIncompleteReason() string {
	switch {
	// The claim itself says it is not an answer. exitGates recorded it: a
	// closing that deferred work the session could not be sent back to do,
	// or a first-person statement that the request could not be accomplished.
	// Product status only -- whether an answer is acceptable to a user is
	// judged elsewhere.
	case st.replyOutstanding:
		return "reply_left_work_outstanding"
	case st.replyDeclaredIncomplete:
		return "reply_declared_incomplete"
	// The reply asks the user for something. Handed back: the run had files it
	// never opened. Awaiting: it had nothing to open, or already looked.
	case st.replyHandedBack:
		return "investigation_handed_back"
	// The run opened several files and answered for fewer than two of them,
	// after being sent back for the coverage the request asked for.
	case st.replyScopeUnmet:
		return "investigation_scope_unmet"
	case st.replyAwaitsUser:
		return "clarification_requested"
	}
	return ""
}

// unresolvedGateReason is the reason of the first listed gate that spent its
// bounces with its finding still true, or "".
func (st *runState) unresolvedGateReason() string {
	for _, u := range unresolvedReasons {
		if _, ok := st.unresolvedGates[u.gate]; ok {
			return u.reason
		}
	}
	return ""
}

// --- Live workspace hazards at completion ------------------------------------
//
// run_background raises the workspace hazard and only a confirmed exit lowers
// it, but the completion decision never asked. A server started mid-run could
// keep rewriting a tracked deliverable while the run reported completed over a
// hash taken at one instant.
//
// This asks the sandbox what is actually true, without changing anything the
// model did not ask for: a job that has ALREADY exited is reaped and its
// hazard lowered, and a job that is still running is left alone and blocks.
// Nothing is killed to make a completion possible.

// settleBackgroundHazard returns the ids of this session's jobs that are still
// running. Exited jobs are reaped through the existing session-owned path, and
// every tracked deliverable is rehashed afterwards so a verdict about bytes a
// job changed on its way out cannot survive.
func settleBackgroundHazard(ctx *AgentContext) []string {
	if ctx == nil {
		return nil
	}
	// Last, after reaping and the rehash: files a job created or removed are
	// not in the ledger for the rehash to find.
	defer settleBackgroundEffects(ctx)
	if len(ctx.BackgroundJobs) == 0 {
		return nil
	}
	ids := make([]string, 0, len(ctx.BackgroundJobs))
	for id := range ctx.BackgroundJobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var live []string
	reaped := false
	for _, id := range ids {
		out, err := sandboxTailBackground(ctx, id, 1)
		if err != nil {
			// Unobservable is not the same as finished. The hazard stands.
			log.Printf("[agent] cannot establish whether background job %s has exited: %v", id, err)
			live = append(live, id)
			continue
		}
		if out.Running || out.ExitCode == nil {
			live = append(live, id)
			continue
		}
		// Already gone. Reaping is bookkeeping at this point, not a kill.
		if _, err := sandboxStopBackground(ctx, id); err != nil {
			log.Printf("[agent] could not reap the exited background job %s: %v", id, err)
			live = append(live, id)
			continue
		}
		delete(ctx.BackgroundJobs, id)
		clearWorkspaceHazard(ctx, id)
		reaped = true
		log.Printf("[agent] background job %s had already exited — reaped at completion", id)
	}
	if reaped {
		// A job can change a file on its way out. Rehash every tracked path:
		// unchanged files keep their verdicts, changed ones lose them.
		invalidateTrackedValidation(ctx)
	}
	return live
}

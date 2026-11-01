// The recoveries: what a stuck model is shown in place of a repeated refusal, and the work a run still owes.

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// --- The run-first recovery transition ---------------------------------------
//
// A warned version of the file is on disk, the run-first gate is demanding it
// be run, and the model answers with the identical raw `@fenced` write it has
// already sent. Left alone, that exchange repeats until the gate's bounce
// budget runs out, and then the same writes start landing again.
//
// Repeating a demand the model has already failed to satisfy is not a
// mechanism. On the recurrence it gets the one thing it has not been able to
// obtain for itself -- the file as it actually is -- and the call that made no
// progress is held back until it changes.
//
// Deliberately narrow. It runs BEFORE fenced resolution, so a blocked repeat
// costs zero generations. It reads; it never writes, never runs a command, and
// never forces a tool. It fires at most once per canonical path, and the
// budget has to be there to spend.

// fencedRecoveryFloor is the work budget a recovery needs to be worth doing:
// enough for the model to read the context, run something, and write once.
const fencedRecoveryFloor = 90 * time.Second

// fencedRunFirstRecovery returns the focused context to hand back, or "" when
// this is not the state, the recovery is already spent, or there is not enough
// budget left to act on it.
func fencedRunFirstRecovery(ctx *AgentContext, st *runState, relPath, content string) string {
	// Raw model intent only. An inline write is the model doing something
	// different, which is exactly what this is asking for.
	if !isFencedSentinel(content) {
		return ""
	}
	key := ledgerKey(ctx, relPath)
	if st.fencedRecoverySpent[key] {
		return ""
	}
	if st.fencedRunFirstRepeats == nil {
		st.fencedRunFirstRepeats = map[string]int{}
	}
	st.fencedRunFirstRepeats[key]++
	// The first occurrence is the gate's own business. This is the recurrence.
	if st.fencedRunFirstRepeats[key] < 2 {
		return ""
	}
	// A recovery the run cannot afford to act on is worse than stopping: it
	// spends the remaining budget on context nobody gets to use.
	if ctx.Ctx != nil {
		if deadline, ok := ctx.Ctx.Deadline(); ok && time.Until(deadline) < fencedRecoveryFloor {
			log.Printf("[agent] skipping the run-first recovery for %s — %v of budget left",
				relPath, time.Until(deadline).Round(time.Second))
			return ""
		}
	}

	source, truncated, err := boundedCurrentSource(ctx, relPath)
	if err != nil {
		// Nothing to show. The gate's own path still applies.
		log.Printf("[agent] run-first recovery for %s could not read the file: %v", relPath, err)
		return ""
	}
	if st.fencedRecoverySpent == nil {
		st.fencedRecoverySpent = map[string]bool{}
	}
	st.fencedRecoverySpent[key] = true
	log.Printf("[agent] run-first recovery for %s — supplying the current source once", relPath)

	var sb strings.Builder
	fmt.Fprintf(&sb, "You have now sent the same whole-file write for %s twice without "+
		"anything changing on disk, so re-sending it is not a route to a working file. "+
		"Here is what %s actually contains right now", relPath, relPath)
	if truncated {
		fmt.Fprintf(&sb, " (first %d lines)", fencedRecoveryMaxLines)
	}
	sb.WriteString(":\n\n")
	sb.WriteString(source)
	sb.WriteString("\n\n")
	if detail := currentValidationDetail(ctx, relPath); detail != "" {
		fmt.Fprintf(&sb, "The last thing checked about those exact bytes: %s\n\n", detail)
	}
	fmt.Fprintf(&sb, "That version is on disk with a parse warning and has never been run. "+
		"Do one of these instead of sending that write again: %s, read more of it with "+
		"read_file, or send a correction that is materially different from what is above — a "+
		"targeted edit_file or replace_lines against a line you can see here is usually smaller "+
		"and lands more often than another whole-file rewrite.", runFirstInstruction(ctx, relPath))
	return sb.String()
}

// fencedRecoveryMaxLines bounds what the recovery reads back. Enough to see a
// small solution whole and the top of a large one; never the whole file.
const fencedRecoveryMaxLines = 120

// boundedCurrentSource reads the file through the workspace reader the tools
// use and returns it numbered and bounded. Nothing is retained: the text goes
// into one message and the caller keeps only a spent flag.
func boundedCurrentSource(ctx *AgentContext, relPath string) (string, bool, error) {
	data, _, err := readWorkspaceFile(ctx, relPath)
	if err != nil {
		return "", false, err
	}
	lines := strings.Split(string(data), "\n")
	truncated := false
	if len(lines) > fencedRecoveryMaxLines {
		lines = lines[:fencedRecoveryMaxLines]
		truncated = true
	}
	var sb strings.Builder
	for i, l := range lines {
		fmt.Fprintf(&sb, "%d\t%s\n", i+1, l)
	}
	return strings.TrimRight(sb.String(), "\n"), truncated, nil
}

// steerRecoveryRepeat is the number of ignored steering refusals on one path
// that buys the recovery. The first refusal is the steer itself; the second is
// the model ignoring it, which is the only evidence that repeating the
// diagnostic will not work.
const steerRecoveryRepeat = 2

// steerRecovery answers the second ignored write_file steer on a path with the
// thing the model was missing, once.
//
// The two steering branches send a model that asked to overwrite an existing
// file somewhere better: read it first, or edit it instead. Both are correct
// and both are only text. A model that ignores the text repeats the identical
// write, and repeating the identical diagnostic back cannot change that. The
// failure accounting bounds it, but the bound is a stop, not an outcome. The
// two refusals fail for different reasons, so they get different recoveries:
//
//   - unread: the model has never seen the file. Show it, bounded, through the
//     same reader read_file uses, and record exactly what was shown the way
//     read_file records a truncated read. It genuinely has the body now, so
//     the read state is real -- but the file is still not session-owned, and
//     the surgical-edit gate still stands.
//   - already read: the model has the body and reached for the wrong tool.
//     Re-showing it teaches nothing, so this is one reminder naming the tools
//     that work on an existing file. Neither is imposed: edit_file stays right
//     for a surgical change, structural_edit for a whole node.
//
// Bounded by construction: one recovery per canonical path, spent whether or
// not the model takes it, and released only by a materially different action
// on that same path. The refused call is still accounted as a failure, so the
// recovery buys the model a better turn, never an extra one.
func steerRecovery(ctx *AgentContext, st *runState, relPath, resolvedPath string, unread bool) string {
	if ctx == nil || st == nil {
		return ""
	}
	// A run that is already ending owns its own terminal.
	if ctx.Ctx != nil && ctx.Ctx.Err() != nil {
		return ""
	}
	key := ledgerKey(ctx, relPath)
	if st.steerRepeats == nil {
		st.steerRepeats = map[string]int{}
	}
	st.steerRepeats[key]++
	if st.steerRepeats[key] < steerRecoveryRepeat || st.steerRecovered[key] {
		return ""
	}
	if tooLateToRecover(ctx, "the write_file steering recovery for "+relPath) {
		return ""
	}
	if st.steerRecovered == nil {
		st.steerRecovered = map[string]bool{}
	}
	st.steerRecovered[key] = true

	var sb strings.Builder
	if unread {
		source, truncated, err := boundedCurrentSource(ctx, relPath)
		if err != nil {
			// The file was there a moment ago and is not readable now. Say
			// nothing rather than something untrue; the plain steer stands.
			log.Printf("[agent] steering recovery for %s could not read it: %v", relPath, err)
			return ""
		}
		fmt.Fprintf(&sb, "You have asked to overwrite %s twice without reading it, so here it is",
			relPath)
		if truncated {
			fmt.Fprintf(&sb, " (first %d lines)", fencedRecoveryMaxLines)
		}
		sb.WriteString(":\n\n")
		sb.WriteString(source)
		sb.WriteString("\n\n")
		sb.WriteString("This is the real file. If it holds input or configuration you were given, " +
			"your replacement would have destroyed it. Now that you have seen it, change it in " +
			"place with edit_file (old_str/new_str) or structural_edit (a selector and the new " +
			"body) — whichever fits the change you mean to make. write_file on this path will " +
			"keep being refused.")
		recordSteeredRead(ctx, relPath, resolvedPath, source, truncated)
		log.Printf("[agent] steering recovery for %s: showed the file (truncated=%v)", relPath, truncated)
	} else {
		fmt.Fprintf(&sb, "You have already read %s, and write_file will keep refusing it — "+
			"repeating the same call cannot land. You do not need to read it again. Make the "+
			"change in place: edit_file with old_str/new_str for a surgical change, or "+
			"structural_edit with a selector and the new body for a whole function or element. "+
			"If what you actually want is a different file, write_file works on a path that "+
			"does not exist yet.", relPath)
		log.Printf("[agent] steering recovery for %s: reminded once, no reread", relPath)
	}
	return sb.String()
}

// tooLateToRecover reports a run with too little time left for a recovery:
// context nobody has time to act on is worse than stopping.
func tooLateToRecover(ctx *AgentContext, what string) bool {
	if ctx.Ctx == nil {
		return false
	}
	deadline, ok := ctx.Ctx.Deadline()
	if !ok || time.Until(deadline) >= fencedRecoveryFloor {
		return false
	}
	log.Printf("[agent] skipping %s — %v of budget left", what, time.Until(deadline).Round(time.Second))
	return true
}

// recordSteeredRead records only what the recovery showed, exactly as read_file
// does for a truncated read: the model owns what it saw and nothing more.
func recordSteeredRead(ctx *AgentContext, relPath, resolvedPath, source string, truncated bool) {
	shown := source
	if !truncated {
		if data, _, err := readWorkspaceFile(ctx, relPath); err == nil {
			shown = string(data)
		}
	}
	ctx.RecordFileRead(resolvedPath, shown)
	if truncated {
		// Only the head was shown; the file runs past it.
		ctx.RecordBodyRead(resolvedPath, 1, fencedRecoveryMaxLines, fencedRecoveryMaxLines+1)
	} else {
		ctx.RecordBodySeen(resolvedPath)
	}
}

// clearSteerState releases a path's steering state after a materially
// different action succeeded on it.
//
// "Materially different" is doing something other than the refused write on
// the same file. An unrelated success elsewhere is not evidence that this path
// is unstuck, which is why this is keyed and not a global reset.
func clearSteerState(ctx *AgentContext, st *runState, name string, args json.RawMessage) {
	if st == nil || name == "write_file" {
		return
	}
	key := ledgerKey(ctx, workspaceRefusalPath(ctx, name, args))
	delete(st.steerRepeats, key)
	delete(st.steerRecovered, key)
}

// --- Replacements refused while the known-good bytes survive -----------------
//
// Retained twice in Stage 1: a valid artifact was already on disk, every later
// replacement was refused for a syntax failure before any byte moved, and the
// model kept sending replacements until a breaker ended the run. Disk was
// never damaged and the task was never finished.
//
// This is not the broken-artifact case, where the file on disk is itself
// broken and nothing valid was ever kept, and it is not restoration, because
// the safer bytes never left disk -- they need preserving, which the refusal
// already does. What is missing is that the model is never shown what it is
// replacing, or told plainly that its replacement was thrown away and the good
// version is intact.
//
// Two hashes, two purposes, deliberately not shared. The retry fingerprint
// from the identity change is a NORMALISED sha1 -- trailing whitespace is
// dropped -- which is right for "is this the same call" and wrong for "is this
// diagnostic about these bytes". Evidence uses sha256 of the exact resolved
// proposal, and a diagnostic is stored with its hash or not at all.
const (
	// Ceilings, both on LIVE state: how many paths are tracked at once, and
	// how many distinct proposals are remembered for the bytes currently on
	// one of them. A session doing more than this is not being helped by
	// remembering more of it.
	maxC4Generations = 8
	maxC4Proposals   = 8
)

// proposalRejection is one canonical path's CURRENT generation: the surviving
// bytes everything here is about, the exact proposals refused against them --
// each with the diagnostic produced for those bytes -- and whether its one
// recovery has been spent.
//
// One entry per path, never one per generation. Keying the map on path AND
// surviving hash looked tidier and starved the thing it was meant to protect:
// every correction a path lands is a new surviving hash, so a single file
// iterating eight times filled a session-wide ceiling with obsolete entries
// and the ninth generation -- the live one -- was refused a recovery by its
// own history. New bytes now REPLACE the generation in place, which is what
// "released when the surviving disk hash changes" has to mean.
type proposalRejection struct {
	diskHash    string            // the surviving bytes this generation is about
	diagnostics map[string]string // proposal sha256 -> its own diagnostic
	order       []string
	recovered   bool
}

// reset re-arms an entry for a new generation of surviving bytes. The old
// hashes and diagnostics go with the bytes they described.
func (e *proposalRejection) reset(diskHash string) {
	e.diskHash = diskHash
	e.diagnostics = map[string]string{}
	e.order = nil
	e.recovered = false
}

// resolvedProposalHash is sha256 of the exact bytes a write_file would have
// written, after fenced resolution. "" for anything else.
func resolvedProposalHash(name string, args json.RawMessage) string {
	if name != "write_file" {
		return ""
	}
	var in WriteFileInput
	if json.Unmarshal(args, &in) != nil || in.Content == "" {
		return ""
	}
	return hashBytes([]byte(in.Content))
}

// survivingKnownGood returns the canonical path and the hash of its bytes on
// disk, when those bytes are readable, are what the ledger describes, and are
// demonstrably valid. Everything else -- unknown, not_run, not_applicable,
// failed, a verdict about other bytes, an unreadable path -- returns "".
func survivingKnownGood(ctx *AgentContext, relPath string) (canon, diskHash string) {
	if ctx == nil {
		return "", ""
	}
	key := ledgerKey(ctx, relPath)
	data, ok := readLedgerBytes(key)
	if !ok {
		return "", ""
	}
	h := hashBytes(data)
	ctx.LedgerMu.Lock()
	d := ctx.Ledger[key]
	var status ValidationStatus
	var kind ValidationKind
	var current string
	if d != nil {
		kind, status = d.CurrentValidation()
		current = d.CurrentHash
	}
	ctx.LedgerMu.Unlock()
	if current != h || status != ValidationPassed || kind != ValidationKindSyntax {
		return "", ""
	}
	return key, h
}

// evictStaleGenerations drops entries whose surviving bytes are no longer the
// bytes on disk. Only provable staleness is evicted -- a path still holding
// the bytes its evidence describes is never touched -- so the ceiling bounds
// how many LIVE paths are tracked rather than how many times the session has
// been round the loop.
func evictStaleGenerations(ctx *AgentContext, st *runState) {
	for path, ev := range st.c4Rejected {
		data, ok := readLedgerBytes(path)
		if !ok || hashBytes(data) != ev.diskHash {
			delete(st.c4Rejected, path)
		}
	}
}

// noteRejectedProposal records a refused replacement against the known-good
// bytes that survived it, and reports how many distinct proposals this
// generation has now refused.
//
// Every clause is about evidence that already exists. The diagnostic is the
// one the checker produced for THESE bytes, carried on the result; no error
// prose is parsed, no lens sample is read, and no historical verdict is reused.
func noteRejectedProposal(ctx *AgentContext, st *runState, name string,
	args json.RawMessage, result *ToolResult) (string, string, int) {
	if st == nil || result == nil || name != "write_file" {
		return "", "", 0
	}
	if result.MutationStatus != MutationRefused ||
		result.ValidationStatus != ValidationFailed ||
		result.ValidationKind != ValidationKindSyntax ||
		result.ValidationDetail == "" {
		return "", "", 0
	}
	sha := resolvedProposalHash(name, args)
	if sha == "" {
		return "", "", 0
	}
	rel := ledgerArgPath(args, "path")
	canon, diskHash := survivingKnownGood(ctx, rel)
	if canon == "" {
		return "", "", 0
	}
	ev := rejectionEvidence(ctx, st, canon, diskHash)
	if ev == nil {
		return "", "", 0
	}
	if _, seen := ev.diagnostics[sha]; !seen {
		if len(ev.order) >= maxC4Proposals {
			return canon, sha, len(ev.order)
		}
		ev.order = append(ev.order, sha)
	}
	// Stored together, always: a hash without its own diagnostic would be a
	// diagnostic waiting to be attached to the wrong bytes.
	ev.diagnostics[sha] = result.ValidationDetail
	return canon, sha, len(ev.order)
}

// rejectionEvidence is what is held for canon at these bytes, or nil when there
// is no room to hold it.
func rejectionEvidence(ctx *AgentContext, st *runState, canon, diskHash string) *proposalRejection {
	if st.c4Rejected == nil {
		st.c4Rejected = map[string]*proposalRejection{}
	}
	ev := st.c4Rejected[canon]
	switch {
	case ev == nil:
		if len(st.c4Rejected) >= maxC4Generations {
			// Make room only where the evidence is provably about bytes that
			// are gone. If every tracked path still holds what its evidence
			// describes, this fails closed: nothing is recorded, so no
			// diagnostic can be offered for these bytes at all.
			evictStaleGenerations(ctx, st)
			if len(st.c4Rejected) >= maxC4Generations {
				return nil
			}
		}
		ev = &proposalRejection{}
		ev.reset(diskHash)
		st.c4Rejected[canon] = ev
	case ev.diskHash != diskHash:
		// The surviving bytes changed: this is a new question, and the old
		// hashes and diagnostics are released with the bytes they described.
		ev.reset(diskHash)
	}
	return ev
}

// rejectedProposalRecovery shows the model the file it is replacing and says
// what happened to its replacement, once per path and surviving-bytes
// generation.
//
// It mutates nothing, runs nothing, invents no selector and claims no
// completion. The diagnostic it quotes is the one stored against this exact
// proposal hash; a different proposal never inherits it.
func rejectedProposalRecovery(ctx *AgentContext, st *runState, relPath, canon, sha string) string {
	if ctx == nil || st == nil || canon == "" || sha == "" {
		return ""
	}
	// A run that is already ending owns its own terminal.
	if ctx.Ctx != nil && ctx.Ctx.Err() != nil {
		return ""
	}
	ev := st.c4Rejected[canon]
	if ev == nil || ev.recovered {
		return ""
	}
	// The evidence has to still be about the bytes that are there now.
	if _, diskHash := survivingKnownGood(ctx, relPath); diskHash != ev.diskHash {
		return ""
	}
	detail, bound := ev.diagnostics[sha]
	if !bound || detail == "" {
		// No diagnostic for THESE bytes. Saying nothing beats saying
		// something true about a different proposal.
		return ""
	}
	// Context nobody has time to act on is worse than stopping.
	if ctx.Ctx != nil {
		if deadline, ok := ctx.Ctx.Deadline(); ok && time.Until(deadline) < fencedRecoveryFloor {
			log.Printf("[agent] skipping the refused-replacement recovery for %s — %v of budget left",
				relPath, time.Until(deadline).Round(time.Second))
			return ""
		}
	}
	source, truncated, err := boundedCurrentSource(ctx, relPath)
	if err != nil {
		log.Printf("[agent] refused-replacement recovery for %s could not read it: %v", relPath, err)
		return ""
	}
	ev.recovered = true
	log.Printf("[agent] refused-replacement recovery for %s: %d proposal(s) refused against surviving valid bytes",
		relPath, len(ev.order))

	var sb strings.Builder
	fmt.Fprintf(&sb, "Your replacement for %s failed its syntax check, so it was not written and "+
		"nothing on disk changed. The working version is still there, exactly as it was.\n\n", relPath)
	fmt.Fprintf(&sb, "%s currently contains", relPath)
	if truncated {
		fmt.Fprintf(&sb, " (first %d lines)", fencedRecoveryMaxLines)
	}
	sb.WriteString(":\n\n")
	sb.WriteString(source)
	fmt.Fprintf(&sb, "\n\nThe check on the bytes you just sent failed: %s\n\n", detail)
	sb.WriteString("That is the version you are replacing. Send something materially different " +
		"from what was just refused: a whole new file with write_file, or -- usually better here, " +
		"since the file above already works -- a targeted change with edit_file, or structural_edit " +
		"if you are replacing a whole function or element. Re-sending the same content will fail " +
		"the same way.")
	return sb.String()
}

// --- The no-op edit over a demonstrably broken artifact ----------------------
//
// Retained twice in Stage 1, and identical both times: write_file lands a file
// that does not parse and says so, verification reports the concrete failure,
// edit_file demands a read, the model reads, and then it sends an edit whose
// old_str and new_str are the same string -- over and over, until repetition
// protection ends the run with the broken file on disk and no valid version to
// fall back to.
//
// The class is already bounded, and a bound is not an outcome. The model is
// copying a span it cannot reproduce with a change applied; edit_file already
// tells it the two sides match, and being told that a second time is the one
// thing already known not to work. So the recurrence gets the evidence
// instead: the file as it is now, and the failure already recorded against
// exactly those bytes.
//
// Nothing here mutates, runs, guesses the intended character, or converts the
// edit. The model has to supply the correction; this only makes that possible.

// c3RecoveryRepeat is the number of explicit no-op edits on one evidence
// generation that arms the recovery. The first is an accident and gets the
// tool's own answer unchanged; the second is evidence that answer did not work.
const c3RecoveryRepeat = 2

// noopEditIntent returns the path of an edit_file call whose old_str and
// new_str are identical, and "" for anything else.
//
// Deliberately only the explicit form, which is the one the retained evidence
// shows. edit_file also refuses edits that are merely INEFFECTIVE -- a
// replacement that leaves the file byte-identical -- and that is a different
// failure with different evidence, so it stays where it is.
func noopEditIntent(name string, args json.RawMessage) string {
	if name != "edit_file" {
		return ""
	}
	var in struct {
		Path   string `json:"path"`
		OldStr string `json:"old_str"`
		NewStr string `json:"new_str"`
	}
	if json.Unmarshal(args, &in) != nil {
		return ""
	}
	if strings.TrimSpace(in.Path) == "" || in.OldStr != in.NewStr {
		return ""
	}
	return in.Path
}

// brokenArtifactRecovery answers a repeated no-op edit with the current file
// and the failure already bound to it, once per evidence generation.
//
// Every clause is an entry condition, and all of them are about evidence that
// already exists: the ledger's verdict on the bytes that are on disk right
// now. Nothing is inferred from error prose, and a verdict that describes
// other bytes is not usable -- CurrentValidation is what enforces that.
func brokenArtifactRecovery(ctx *AgentContext, st *runState, relPath string) string {
	if ctx == nil || st == nil {
		return ""
	}
	// A run that is already ending owns its own terminal.
	if ctx.Ctx != nil && ctx.Ctx.Err() != nil {
		return ""
	}
	key := ledgerKey(ctx, relPath)
	// The file as it is NOW. A path that cannot be read fabricates nothing:
	// no source, no read state, no recovery.
	data, ok := readLedgerBytes(key)
	if !ok {
		return ""
	}
	diskHash := hashBytes(data)
	// The body has to have been in front of the model through the real read
	// path. WasFileRead is weaker -- outline_file satisfies it while showing
	// signatures only -- so a file whose contents were never displayed is not
	// eligible.
	if !ctx.WasBodySeen(key) {
		return ""
	}
	hazardous := workspaceHazardous(ctx)

	ctx.LedgerMu.Lock()
	d := ctx.Ledger[key]
	var status ValidationStatus
	var kind ValidationKind
	var detail string
	var sessionWritten, restorable bool
	if d != nil {
		kind, status = d.CurrentValidation()
		detail = d.ValidationDetail
		// Generation > 0 is the ledger's own record that this session wrote
		// the file, and it is canonical -- unlike SessionWrites, which is
		// keyed on the path as the model spelled it.
		sessionWritten = d.Generation > 0 && d.CurrentHash == diskHash
		restorable, _ = checkpointRestorable(d, diskHash, hazardous)
	}
	ctx.LedgerMu.Unlock()

	switch {
	case !sessionWritten:
		return ""
	case status != ValidationFailed:
		// unknown, not_run, not_applicable, passed, and every verdict that
		// describes bytes no longer on disk, all end here.
		return ""
	case restorable:
		// A path with an eligible safer checkpoint is restoration's, not this.
		return ""
	}

	genKey := key + "\x00" + diskHash
	if st.noopEditRepeats == nil {
		st.noopEditRepeats = map[string]int{}
	}
	st.noopEditRepeats[genKey]++
	if st.noopEditRepeats[genKey] < c3RecoveryRepeat || st.brokenArtifactRecovered[genKey] {
		return ""
	}
	if tooLateToRecover(ctx, "the no-op-edit recovery for "+relPath) {
		return ""
	}
	source, truncated, err := boundedCurrentSource(ctx, relPath)
	if err != nil {
		log.Printf("[agent] no-op-edit recovery for %s could not read it: %v", relPath, err)
		return ""
	}
	if st.brokenArtifactRecovered == nil {
		st.brokenArtifactRecovered = map[string]bool{}
	}
	st.brokenArtifactRecovered[genKey] = true
	if detail == "" {
		detail = string(kind) + " check failed"
	}
	log.Printf("[agent] no-op-edit recovery for %s: %s/%s on the current bytes", relPath, kind, status)

	var sb strings.Builder
	fmt.Fprintf(&sb, "That edit changes nothing: old_str and new_str are the same string, "+
		"so %s would be left exactly as it is. It is still broken.\n\n", relPath)
	writeCurrentSource(&sb, relPath, source, truncated)
	fmt.Fprintf(&sb, "\n\nThe %s check on exactly these bytes failed: %s\n\n", kind, detail)
	sb.WriteString("Send an edit whose new_str actually differs from old_str and fixes that. " +
		"Nothing has been changed for you, and nothing will be until you do.")
	return sb.String()
}

// writeCurrentSource shows the file as it is now, and says when that is only its head.
func writeCurrentSource(sb *strings.Builder, relPath, source string, truncated bool) {
	fmt.Fprintf(sb, "%s currently contains", relPath)
	if truncated {
		fmt.Fprintf(sb, " (first %d lines)", fencedRecoveryMaxLines)
	}
	sb.WriteString(":\n\n")
	sb.WriteString(source)
}

// clearBrokenArtifactState releases a path's broken-artifact state once a
// materially different mutation on that same path reaches its normal result.
//
// Keyed, so an unrelated success elsewhere does not clear it, and narrow, so
// another no-op does not either -- a no-op is the thing being recovered from.
// A read does not clear it, and neither does a new turn. The hash is the other
// release: different bytes are a different evidence generation and get their
// own key.
func clearBrokenArtifactState(ctx *AgentContext, st *runState, name string, args json.RawMessage) {
	if st == nil || len(st.noopEditRepeats)+len(st.brokenArtifactRecovered) == 0 {
		return
	}
	if noopEditIntent(name, args) != "" {
		return
	}
	targets := mutationIntentTargets(ctx, name, args)
	if len(targets) == 0 {
		return
	}
	prefix := ledgerKey(ctx, targets[0].Rel) + "\x00"
	for k := range st.noopEditRepeats {
		if strings.HasPrefix(k, prefix) {
			delete(st.noopEditRepeats, k)
		}
	}
	for k := range st.brokenArtifactRecovered {
		if strings.HasPrefix(k, prefix) {
			delete(st.brokenArtifactRecovered, k)
		}
	}
}

// markWarnedRun records or discharges a path's pending warned landing.
//
// One invariant, in one place: a warned landing puts the path in the set, and
// anything else takes it out. Storing "not warned" as a value is what let a
// key-only reader announce a parse warning over a file that parses.
func (s *runState) markWarnedRun(ctx *AgentContext, path string, warned bool) {
	if !warned {
		s.clearWarnedRun(ctx, path)
		return
	}
	if s.pendingWarnedRun == nil {
		s.pendingWarnedRun = map[string]bool{}
	}
	s.pendingWarnedRun[path] = true
}

// clearWarnedRun drops every spelling of the same file.
//
// The set is keyed on the path as the model sent it, because the gate quotes
// it back ("Run it first -- `python3 solve.py`") and executionAttempt matches
// the command against it. So the identity used to DISCHARGE is the ledger's
// canonical one, which is the identity the rest of the run already uses: a
// clean rewrite of ./solve.py has to retire solve.py's warning, or the mark
// outlives the bytes it describes.
func (s *runState) clearWarnedRun(ctx *AgentContext, path string) {
	if len(s.pendingWarnedRun) == 0 {
		return
	}
	key := ledgerKey(ctx, path)
	for p := range s.pendingWarnedRun {
		if ledgerKey(ctx, p) == key {
			delete(s.pendingWarnedRun, p)
		}
	}
}

// currentValidationDetail reports what the ledger knows about the bytes that
// are there NOW, and says nothing when the verdict describes older bytes.
func currentValidationDetail(ctx *AgentContext, relPath string) string {
	key := ledgerKey(ctx, relPath)
	ctx.LedgerMu.Lock()
	d := ctx.Ledger[key]
	var kind ValidationKind
	var status ValidationStatus
	var detail string
	if d != nil {
		kind, status = d.CurrentValidation()
		detail = d.ValidationDetail
	}
	ctx.LedgerMu.Unlock()
	if status != ValidationFailed {
		return ""
	}
	if detail == "" {
		return string(kind) + " check failed"
	}
	return detail
}

// --- Unresolved mutation debt -----------------------------------------------
//
// Completion has three inputs: the user's named outputs, the deliverable
// ledger, and one session-wide madeProductiveChange bool. A valid mutation the
// model asked for and never landed leaves no trace in any of them, so without
// this record a success on an unrelated path would retire it.
//
// The ledger is not the place for this. It records what the session OWNS on
// disk, and an intent that never landed owns nothing. Debt is a separate,
// bounded, session-local record of what is still owed.

// maxTrackedMutationDebt bounds the map. A session that somehow exceeds it
// stops naming individual paths and never stops reporting that work is
// unresolved -- the failure direction that cannot manufacture a completion.
const maxTrackedMutationDebt = 64

// debtKind is what would have to be demonstrated for the debt to clear.
type debtKind string

const (
	debtContent debtKind = "content" // bytes must exist and validate
	debtDelete  debtKind = "delete"  // the path must be demonstrably absent
	debtMove    debtKind = "move"    // source absent AND destination validated
)

// mutationDebtEntry holds only what a terminal or a recovery needs to say.
// No file contents: the bytes live on disk and in the ledger's bounded
// checkpoint, never here.
type mutationDebtEntry struct {
	Rel  string // workspace-relative, for plain-language disclosure
	Kind debtKind
	Dest string // canonical destination, move only
	Gen  int    // the debt generation this entry was opened in
}

// mutationIntentTargets returns the canonical paths a path-targeted mutator is
// asking to change, or nil when the call is not one, is malformed, names a
// blank path, or is deny-listed. run_command and run_background are excluded
// on purpose: their effects are unobserved by construction and a single path
// cannot represent them.
func mutationIntentTargets(ctx *AgentContext, name string, args json.RawMessage) []*mutationDebtEntry {
	switch name {
	case "write_file", "edit_file", "structural_edit", "insert_after", "replace_lines",
		"delete_file", "move_file":
	default:
		return nil
	}
	// The same authoritative refusals the tool applies. An attempt the system
	// forbids is not work the session owes; unmet-action evidence covers it.
	if denied, _ := shouldDenyToolCall(name, args); denied {
		return nil
	}
	if validateToolWorkspacePaths(name, args, ctx) != "" {
		return nil
	}
	rel := func(field string) string {
		var m map[string]json.RawMessage
		if json.Unmarshal(args, &m) != nil {
			return ""
		}
		var s string
		if raw, ok := m[field]; !ok || json.Unmarshal(raw, &s) != nil {
			return ""
		}
		return strings.TrimSpace(s)
	}
	if name == "move_file" {
		src, dst := rel("source"), rel("destination")
		if src == "" || dst == "" {
			return nil
		}
		return []*mutationDebtEntry{{Rel: src, Kind: debtMove, Dest: ledgerKey(ctx, dst)}}
	}
	p := rel("path")
	if p == "" {
		return nil
	}
	kind := debtContent
	if name == "delete_file" {
		kind = debtDelete
	}
	return []*mutationDebtEntry{{Rel: p, Kind: kind}}
}

// noteMutationIntent opens debt for a permitted in-workspace mutation. It runs
// BEFORE dispatch, because a fenced resolution that fails never reaches
// executeToolCall and would leave no trace anywhere.
func noteMutationIntent(ctx *AgentContext, st *runState, name string, args json.RawMessage) {
	for _, e := range mutationIntentTargets(ctx, name, args) {
		key := ledgerKey(ctx, e.Rel)
		if st.mutationDebt == nil {
			st.mutationDebt = map[string]*mutationDebtEntry{}
		}
		if prev, exists := st.mutationDebt[key]; exists {
			st.retireMistakenPath(key, prev, e)
			continue // one debt per canonical path, whatever the spelling
		}
		if len(st.mutationDebt) >= maxTrackedMutationDebt {
			// Fail closed: stop naming, keep blocking.
			if !st.debtOverflow {
				log.Printf("[agent] mutation-debt ceiling reached (%d paths) — further "+
					"unresolved work is reported without naming the paths", maxTrackedMutationDebt)
			}
			st.debtOverflow = true
			return
		}
		if st.debtRecoveryOffered >= st.debtGeneration {
			// Work that went unresolved AFTER the model was already given its
			// chance is a new situation, and earns one more -- bounded below.
			st.debtGeneration++
		}
		e.Gen = st.debtGeneration
		st.mutationDebt[key] = e
		log.Printf("[agent] tracking unresolved %s work on %s", e.Kind, e.Rel)
	}
}

// retireMistakenPath is the one structured way a mistaken path is retired: the
// model explicitly asks for the same path to be REMOVED. That converts what it
// owes from "produce this" to "prove it is gone", and the proof is a confirmed
// absence, not a claim.
func (st *runState) retireMistakenPath(key string, prev, e *mutationDebtEntry) {
	if prev.Kind == debtContent && e.Kind == debtDelete {
		e.Gen = prev.Gen
		st.mutationDebt[key] = e
		log.Printf("[agent] %s: explicit removal requested — it now has to be shown gone", e.Rel)
	}
}

// settleMutationDebt clears what the LEDGER can prove, and nothing else. It is
// driven by observed state rather than by a tool reporting success, so a
// refusal, an unknown verdict, stale evidence, a read, or a success on another
// path all leave the debt standing.
func settleMutationDebt(ctx *AgentContext, st *runState) {
	if st == nil || len(st.mutationDebt) == 0 {
		return
	}
	for key, e := range st.mutationDebt {
		if debtResolved(ctx, key, e) {
			delete(st.mutationDebt, key)
			log.Printf("[agent] unresolved %s work on %s is now demonstrated", e.Kind, e.Rel)
		}
	}
}

// validationSettles reports whether a path's CURRENT validation is good enough
// to call the work demonstrated: a pass, or a genuine not_applicable from a
// producer that deliberately checked nothing because nothing applies.
func validationSettles(d *DeliverableState) bool {
	if d == nil || d.Tombstoned || d.CurrentHash == "" {
		return false
	}
	kind, status := d.CurrentValidation() // fails closed on a hash mismatch
	switch status {
	case ValidationPassed:
		return true
	case ValidationNotApplicable:
		return kind == ValidationKindNone
	}
	return false
}

func debtResolved(ctx *AgentContext, key string, e *mutationDebtEntry) bool {
	ctx.LedgerMu.Lock()
	d := ctx.Ledger[key]
	var tombstoned bool
	var reason string
	if d != nil {
		tombstoned, reason = d.Tombstoned, d.TombstoneReason
	}
	ctx.LedgerMu.Unlock()

	switch e.Kind {
	case debtContent:
		return validationSettles(d)
	case debtDelete:
		// The entry only exists because the model explicitly asked for THIS
		// path to be removed, so the intent is already on the record. What
		// settles it is the absence, confirmed against disk right now.
		//
		// The tombstone is not required, and requiring it made the retirement
		// route unusable for the case it exists for: a path that never landed
		// cannot be deleted -- delete_file fails with "file not found" and
		// writes no tombstone -- so a model abandoning work it never managed
		// to produce could never say so. A path that IS still there fails this
		// check whatever the delete reported.
		_, err := os.Stat(key)
		return os.IsNotExist(err)
	case debtMove:
		if !tombstoned || !strings.HasPrefix(reason, "moved:") {
			return false
		}
		if _, err := os.Stat(key); !os.IsNotExist(err) {
			return false // the source is still there
		}
		// The destination is judged by the same contract every other
		// deliverable is judged by, read from disk. The ledger's own verdict
		// cannot be used here: move_file deliberately records the destination
		// as unknown, because a syntax pass earned under the old name says
		// nothing about this path, so a demonstrated rename could never finish
		// on a verdict nothing ever writes.
		if e.Dest == "" {
			return false
		}
		return deliverablesDemonstrablyValid(ctx, []string{e.Dest})
	}
	return false
}

// sessionWroteWebFiles reports whether this run wrote a file the route
// contract is about: a page or script that submits, or the handler file that
// serves. It is the scope of the route-contract exit gate.
func sessionWroteWebFiles(ctx *AgentContext) bool {
	for rel := range ctx.SessionWrites {
		switch strings.ToLower(filepath.Ext(rel)) {
		case ".py", ".html", ".htm", ".js":
			return true
		}
	}
	return false
}

// executionSucceeded reads the executor's own structural answer for a
// run_command or run_background result: exit 0 and not killed, or a
// background job still serving after its settle window (a crash at startup
// exits non-zero before the window ends).
func executionSucceeded(tool string, result *ToolResult) bool {
	if result == nil || !result.Success || len(result.Data) == 0 {
		return false
	}
	switch tool {
	case "run_command":
		var out RunCommandOutput
		return json.Unmarshal(result.Data, &out) == nil && out.ExitCode == 0 && !out.TimedOut
	case "run_background":
		var out RunBackgroundOutput
		if json.Unmarshal(result.Data, &out) != nil {
			return false
		}
		return out.Running || (out.ExitCode != nil && *out.ExitCode == 0)
	}
	return false
}

// settleDebtByExecution retires content debt for every owed path this command
// executed, when the execution came up clean. The verdict is recorded against
// the exact bytes that ran, as its own kind, and without touching the
// checkpoint: an execution demonstrates the file, it does not author it, and
// a syntax checkpoint that restoration may still need is left as it was.
// Without this, a file that landed with no parse verdict of its own (a
// structural_edit through V3) and then ran clean would still be reported at
// completion as never written in a checkable state.
func settleDebtByExecution(ctx *AgentContext, st *runState, command string, succeeded bool) {
	if !succeeded || st == nil || len(st.mutationDebt) == 0 {
		return
	}
	// Only a segment whose result the line reports can have come up clean:
	// `python app.py | tail` exits 0 whatever app.py did.
	honest := strings.Join(classifyCommandEvidence(command).Honest, " && ")
	for key, e := range st.mutationDebt {
		if e.Kind != debtContent || !executionAttempt(honest, e.Rel) {
			continue
		}
		data, err := os.ReadFile(key)
		if err != nil {
			continue
		}
		h := hashBytes(data)
		ctx.LedgerMu.Lock()
		d := ledgerEntry(ctx, key)
		// Running the file cannot see a defect in a script it embeds for the
		// browser: the server starts and serves the broken <script> fine. A
		// current embedded-script failure stands, and the debt with it.
		if d.ValidationStatus == ValidationFailed && d.ValidatedHash == h &&
			strings.HasPrefix(d.ValidationDetail, embeddedScriptErrPrefix) {
			ctx.LedgerMu.Unlock()
			continue
		}
		d.CurrentHash, d.CurrentSize = h, len(data)
		d.Tombstoned, d.TombstoneReason = false, ""
		d.ValidationKind, d.ValidationStatus = ValidationKindExecution, ValidationPassed
		d.ValidationDetail = "executed clean: " + truncateStr(command, 80)
		d.ValidatedHash = h
		ctx.LedgerMu.Unlock()
		log.Printf("[agent] %s executed clean — content debt settled by execution", e.Rel)
	}
	settleMutationDebt(ctx, st)
}

// unresolvedDebtPaths lists what is still owed, in a stable order, bounded for
// disclosure. The second return says whether more exist than are named.
func unresolvedDebtPaths(st *runState, limit int) ([]string, bool) {
	if st == nil {
		return nil, false
	}
	var out []string
	for _, e := range st.mutationDebt {
		out = append(out, e.Rel)
	}
	sort.Strings(out)
	more := st.debtOverflow
	if len(out) > limit {
		out, more = out[:limit], true
	}
	return out, more
}

func hasUnresolvedDebt(st *runState) bool {
	return st != nil && (len(st.mutationDebt) > 0 || st.debtOverflow)
}

// unresolvedDebtSummary is the plain-language disclosure. It names paths and
// says what would settle them, in the words a user would use.
func unresolvedDebtSummary(st *runState) string {
	paths, more := unresolvedDebtPaths(st, 5)
	var sb strings.Builder
	sb.WriteString("Stopped: work you asked for was started and never finished")
	switch {
	case len(paths) == 0:
		sb.WriteString(".")
	case len(paths) == 1:
		fmt.Fprintf(&sb, ": %s was never written in a state this run could check.", paths[0])
	default:
		fmt.Fprintf(&sb, ": %s were never written in a state this run could check.",
			strings.Join(paths, ", "))
	}
	if more {
		sb.WriteString(" Other files are in the same state.")
	}
	sb.WriteString(runNotConfirmedNote)
	return sb.String()
}

// maxDebtRecoveries bounds the whole session. New unresolved work opens a new
// generation and earns another offer, but never without end.
const maxDebtRecoveries = 2

// offerDebtRecovery is the one chance to settle before the terminal. Returning
// incomplete is honest and does nothing for the user; this says exactly what is
// outstanding and exactly what would settle it, once per debt generation.
//
// It changes nothing itself: no file is written, deleted, moved or run, and no
// tool is forced. The model chooses through its normal tools, under their
// normal guards.
func offerDebtRecovery(ctx *AgentContext, st *runState) string {
	if st == nil || !hasUnresolvedDebt(st) {
		return ""
	}
	if st.debtRecoveryOffered >= st.debtGeneration || st.debtRecoveryCount >= maxDebtRecoveries {
		return ""
	}
	if tooLateToRecover(ctx, "the unresolved-work recovery") {
		return ""
	}
	st.debtRecoveryOffered = st.debtGeneration
	st.debtRecoveryCount++

	paths, more := unresolvedDebtPaths(st, 5)
	var sb strings.Builder
	sb.WriteString("Before finishing: work you started never reached a state this run " +
		"could check, so it cannot be reported as done.\n\n")
	for _, p := range paths {
		var kind debtKind
		for _, e := range st.mutationDebt {
			if e.Rel == p {
				kind = e.Kind
			}
		}
		switch kind {
		case debtDelete:
			fmt.Fprintf(&sb, "  %s — you asked for it to be removed; it is still there.\n", p)
		case debtMove:
			fmt.Fprintf(&sb, "  %s — the move is unfinished.\n", p)
		default:
			fmt.Fprintf(&sb, "  %s — never written in a form that could be checked.\n", p)
		}
	}
	if more {
		sb.WriteString("  (other files are in the same state)\n")
	}
	sb.WriteString("\nFor each one, either finish it — write the complete file and make sure " +
		"it is valid, or make the change with edit_file — or, if you decided that file " +
		"should not exist after all, say so by calling delete_file on that exact path so " +
		"its absence can be confirmed. Saying you no longer need it is not enough. " +
		"Finishing another file does not settle this one.")
	return sb.String()
}

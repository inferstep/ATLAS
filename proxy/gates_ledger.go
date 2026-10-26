// The deliverable ledger: what this session put at each path, the last
// version of it shown to be valid, and the restoration that puts that
// version back. recordLedgerEffect in gates.go records each resolved tool
// call here; applyShellChanges (workspace_observation.go) adds what a shell
// command changed. Also in gates.go: invalidateTrackedValidation, which
// re-proves every tracked verdict after a shell effect; readLedgerBytes,
// which reads the bytes the ledger hashes; and observePathFromDisk, which
// records what is on disk after an effect.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
)

// --- Phase 3A: observational deliverable ledger -----------------------------
//
// Records what happened to each session-owned deliverable. It observes only:
// nothing here changes what is written, refused, restored, or reported.
//
// The lock is held for map and struct mutation ONLY. Reading the file,
// hashing it and validating it all happen outside it, because a session mutex
// held across a sandbox round-trip would serialise the agent loop behind the
// network.

func ledgerKey(ctx *AgentContext, path string) string {
	return filepath.Clean(resolveAgentPath(ctx, path))
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ledgerEntry returns the entry for a canonical key, creating it if absent.
// Caller must hold LedgerMu.
func ledgerEntry(ctx *AgentContext, key string) *DeliverableState {
	if ctx.Ledger == nil {
		ctx.Ledger = map[string]*DeliverableState{}
	}
	d := ctx.Ledger[key]
	if d == nil {
		d = &DeliverableState{Path: key}
		ctx.Ledger[key] = d
	}
	return d
}

// ledgerSessionBytes sums checkpoint bytes held. Caller must hold LedgerMu.
func ledgerSessionBytes(ctx *AgentContext) int {
	n := 0
	for _, d := range ctx.Ledger {
		n += len(d.CheckpointBytes)
	}
	return n
}

// observeDeliverable records the CURRENT bytes of a path and, when the caller
// supplies an explicit passed verdict bound to those exact bytes, promotes
// them to the path's single checkpoint.
//
// Promotion happens ONLY from an explicit ValidationPassed whose hash matches
// the bytes just read. Never from legacy Success, warning absence, extension,
// error prose, not_run, not_applicable, failed, unknown, or a mismatch. A
// syntax pass establishes syntax; it is not runtime success and never task
// correctness.
func observeDeliverable(ctx *AgentContext, path string, content []byte,
	kind ValidationKind, status ValidationStatus, detail string) {
	if ctx == nil {
		return
	}
	key := ledgerKey(ctx, path)
	h := hashBytes(content)

	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	d := ledgerEntry(ctx, key)
	d.CurrentHash = h
	d.CurrentSize = len(content)
	d.Generation++
	d.Tombstoned = false
	d.TombstoneReason = ""
	d.ValidationKind = kind
	d.ValidationStatus = status
	d.ValidationDetail = detail
	d.ValidatedHash = h // the verdict describes exactly these bytes

	if status != ValidationPassed {
		return
	}
	// Same bytes already checkpointed: nothing to do.
	if d.CheckpointHash == h {
		return
	}
	// The kind travels with the bytes. Restoration will not swap a
	// structural pass in for a syntax failure.
	d.CheckpointKind = kind
	if len(content) > maxCheckpointFileBytes {
		d.CheckpointUnavailable = "exceeds the per-file checkpoint ceiling"
		return
	}
	// Deterministic admission: the replacement's cost is its own size minus
	// whatever this path already holds. Nothing else is evicted, so there is
	// no map-order dependence.
	projected := ledgerSessionBytes(ctx) - len(d.CheckpointBytes) + len(content)
	if projected > maxCheckpointSessionBytes {
		d.CheckpointUnavailable = "exceeds the per-session checkpoint ceiling"
		return
	}
	d.CheckpointBytes = append([]byte(nil), content...)
	d.CheckpointHash = h
	d.CheckpointUnavailable = ""
}

// observeRewrite records that ATLAS itself changed a file's bytes after the
// tool that wrote them was validated. The file is still the session's own, and
// no verdict is carried over to the new bytes.
func observeRewrite(ctx *AgentContext, path string, content []byte) {
	if ctx == nil {
		return
	}
	key := ledgerKey(ctx, path)
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	d := ledgerEntry(ctx, key)
	d.CurrentHash = hashBytes(content)
	d.CurrentSize = len(content)
	d.Generation++
	d.ValidatedHash = ""
	d.ValidationStatus = ValidationUnknown
	d.ValidationKind = ValidationKindUnknown
}

// tombstoneDeliverable records a deliberate removal. Checkpoint bytes are
// retained for a later policy decision, and automatic restoration is
// prohibited outright: resurrecting a file the user asked to delete is worse
// than losing a checkpoint.
func tombstoneDeliverable(ctx *AgentContext, path, reason string) {
	if ctx == nil {
		return
	}
	key := ledgerKey(ctx, path)
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	d := ledgerEntry(ctx, key)
	d.Tombstoned = true
	d.TombstoneReason = reason
	d.RestoreProhibited = true
	d.CurrentHash = ""
	d.CurrentSize = 0
	d.Generation++
	// The verdict described bytes that are no longer there.
	d.ValidationStatus = ValidationUnknown
	d.ValidationKind = ValidationKindUnknown
	d.ValidatedHash = ""
}

// raiseWorkspaceHazard marks the workspace concurrently mutable. Raised by
// run_background. stop_background does NOT lower it: a signalled process may
// still be flushing, so only a confirmed exit does.
func raiseWorkspaceHazard(ctx *AgentContext, key string) {
	if ctx == nil || key == "" {
		return
	}
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	if ctx.WorkspaceHazards == nil {
		ctx.WorkspaceHazards = map[string]bool{}
	}
	// Keyed, so observing the same job twice cannot raise twice and a
	// duplicate id cannot demand two reaps.
	ctx.WorkspaceHazards[key] = true
}

// clearWorkspaceHazard lowers the hazard on a CONFIRMED process exit.
func clearWorkspaceHazard(ctx *AgentContext, key string) {
	if ctx == nil || key == "" {
		return
	}
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	// Only this job's hazard. Idempotent, and it cannot underflow or reach
	// another job that is still live or unconfirmed.
	delete(ctx.WorkspaceHazards, key)
}

func workspaceHazardous(ctx *AgentContext) bool {
	if ctx == nil {
		return false
	}
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	return len(ctx.WorkspaceHazards) > 0
}

// ledgerTracks reports whether the session has already observed a path.
func ledgerTracks(ctx *AgentContext, path string) bool {
	key := ledgerKey(ctx, path)
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	return ctx.Ledger[key] != nil
}

// ledgerTombstoned reports whether the ledger records the path as gone: a
// delete, a move, or a removal a shell observation already charged.
func ledgerTombstoned(ctx *AgentContext, path string) bool {
	key := ledgerKey(ctx, path)
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	d := ctx.Ledger[key]
	return d != nil && d.Tombstoned
}

// ledgerArgPath pulls one string field out of a tool's raw args without
// binding the ledger to any tool's input struct.
func ledgerArgPath(args json.RawMessage, field string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(args, &m) != nil {
		return ""
	}
	var s string
	if raw, present := m[field]; !present || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// --- Phase 3B: demonstrably-safer restoration -------------------------------
//
// Scope: ONE terminal. The repeat detector is where a run stops with a
// deliverable it has itself shown to be broken -- the seed-20260901 debounce5
// shape -- and it is the only producer wired to this. The other twelve done
// emitters are untouched, deliberately: a terminal that never demonstrated
// breakage has nothing to recover from, and attaching recovery to all of them
// would make a rare, evidence-bound action routine.
//
// Restoration is a system action, not a model mutation. It sets no progress
// hint, claims no V3 provenance, emits no tool call or tool result, and never
// turns a stopped run into a completed one.

// restoreDecision is what happened for ONE path. Recovery is per-path and is
// disclosed that way: there is no transaction, and a run that recovers two of
// three files must not read as if it recovered all three.
type restoreDecision struct {
	Path      string // workspace-relative, for disclosure
	Restored  bool
	Attempted bool   // a write was issued
	Reason    string // why not, or the real failure
}

// checkpointRestorable answers the eligibility question and nothing else. It
// takes the freshly-read current bytes so the decision is about what is on
// disk right now, not about what the ledger last heard.
//
// Every clause is a reason NOT to act. Unknown, not_run, not_applicable,
// unobserved, a hash that moved, a kind that does not compare, or missing
// bytes all end here, because the alternative is overwriting a user's file on
// a guess.
func checkpointRestorable(d *DeliverableState, currentHash string, hazardous bool) (bool, string) {
	switch {
	case d == nil || d.Generation == 0:
		return false, "not a deliverable this session wrote"
	case d.Tombstoned:
		return false, "deleted or moved on purpose"
	case d.RestoreProhibited:
		return false, "restoration prohibited for this path"
	case d.CheckpointHash == "":
		if d.CheckpointUnavailable != "" {
			return false, "no earlier valid version was kept (" + d.CheckpointUnavailable + ")"
		}
		return false, "no version of it was ever shown to be valid"
	case len(d.CheckpointBytes) == 0:
		return false, "the earlier valid version is no longer available"
	case len(d.CheckpointBytes) > maxCheckpointFileBytes:
		return false, "the earlier valid version is too large to hold"
	case hashBytes(d.CheckpointBytes) != d.CheckpointHash:
		// Held bytes and recorded hash disagree: something is wrong with the
		// ledger itself, so it is not allowed to touch the workspace.
		return false, "the earlier valid version could not be verified"
	case d.CheckpointHash == currentHash:
		return false, "the file already holds the last version shown to be valid"
	case d.ValidatedHash != currentHash:
		// The verdict describes bytes that are no longer there.
		return false, "the current contents were never checked"
	case d.ValidationStatus != ValidationFailed:
		// Only a DEMONSTRATED failure justifies replacing what is there.
		return false, "the current contents were not shown to be broken"
	case d.CheckpointKind == ValidationKindUnknown || d.ValidationKind == ValidationKindUnknown:
		return false, "the two versions were not checked the same way"
	case d.CheckpointKind != d.ValidationKind:
		return false, "the two versions were not checked the same way"
	case hazardous:
		return false, "a background job may still be writing"
	}
	return true, ""
}

// restoreDeliverable re-reads, decides, and -- only if every clause holds --
// puts the checkpoint back through the same atomic replace the write tools
// use. The write is then re-read and hashed: a restore that cannot prove it
// landed exactly is a failure, not a success.
func restoreDeliverable(ctx *AgentContext, key string) restoreDecision {
	rel := key
	if r, err := filepath.Rel(ctx.WorkingDir, key); err == nil && !strings.HasPrefix(r, "..") {
		rel = r
	}
	dec := restoreDecision{Path: rel}

	// Intent is checked BEFORE anything is observed. A fresh observation
	// clears the tombstone flag by design -- the path exists again -- and a
	// path the model deliberately deleted or moved must not become eligible
	// just because something later recreated it.
	if restoreRefusedByIntent(ctx, key, &dec) {
		return dec
	}

	// Read immediately before deciding. Anything the ledger believes is a
	// starting point; the file is the fact.
	current, ok := readLedgerBytes(key)
	if !ok {
		dec.Reason = "its current contents could not be read"
		return dec
	}
	currentHash := hashBytes(current)
	observeFreshSyntaxVerdict(ctx, key, current)

	// The hazard counter lives under the same mutex, so it is read before the
	// entry is locked rather than from inside the decision.
	hazardous := workspaceHazardous(ctx)

	ctx.LedgerMu.Lock()
	d := ctx.Ledger[key]
	eligible, reason := checkpointRestorable(d, currentHash, hazardous)
	var want []byte
	var wantHash string
	var wantKind ValidationKind
	var wantDetail string
	if eligible {
		want = append([]byte(nil), d.CheckpointBytes...)
		wantHash, wantKind, wantDetail = d.CheckpointHash, d.CheckpointKind, d.ValidationDetail
	}
	ctx.LedgerMu.Unlock()

	if !eligible {
		dec.Reason = reason
		return dec
	}

	dec.Attempted = true
	if err := atomicReplaceFile(key, want); err != nil {
		// The atomic path leaves the target untouched on failure, so the
		// current bytes are still there. Report the real error.
		dec.Reason = err.Error()
		return dec
	}
	// Prove it landed. Nothing is claimed from the write returning nil.
	after, ok := readLedgerBytes(key)
	if !ok {
		dec.Reason = "the restored file could not be read back"
		return dec
	}
	if hashBytes(after) != wantHash {
		dec.Reason = "the file on disk does not match the version that was restored"
		return dec
	}

	// The ledger now describes the restored bytes, carrying the evidence that
	// was already earned for exactly this hash. No new verdict is invented.
	ctx.LedgerMu.Lock()
	if d := ctx.Ledger[key]; d != nil {
		d.CurrentHash = wantHash
		d.CurrentSize = len(want)
		d.Generation++
		d.ValidationKind = wantKind
		d.ValidationStatus = ValidationPassed
		d.ValidationDetail = wantDetail
		d.ValidatedHash = wantHash
		d.Recovered = true
	}
	ctx.LedgerMu.Unlock()

	dec.Restored = true
	log.Printf("[recovery] restored %s to the last version shown to be valid", logPath(rel))
	return dec
}

// restoreRefusedByIntent reports whether what the ledger records about a path
// rules out restoring it before anything is observed: a path this session did
// not write, one it deleted or moved, one it may not restore, or one with no
// version shown to be valid. The reason goes into dec.
func restoreRefusedByIntent(ctx *AgentContext, key string, dec *restoreDecision) bool {
	ctx.LedgerMu.Lock()
	entry := ctx.Ledger[key]
	switch {
	case entry == nil || entry.Generation == 0:
		ctx.LedgerMu.Unlock()
		dec.Reason = "not a deliverable this session wrote"
		return true
	case entry.Tombstoned:
		ctx.LedgerMu.Unlock()
		dec.Reason = "deleted or moved on purpose"
		return true
	case entry.RestoreProhibited:
		ctx.LedgerMu.Unlock()
		dec.Reason = "restoration prohibited for this path"
		return true
	case entry.CheckpointHash == "":
		reason := "no version of it was ever shown to be valid"
		if entry.CheckpointUnavailable != "" {
			reason = "no earlier valid version was kept (" + entry.CheckpointUnavailable + ")"
		}
		ctx.LedgerMu.Unlock()
		dec.Reason = reason
		return true
	}
	ctx.LedgerMu.Unlock()
	return false
}

// observeFreshSyntaxVerdict checks the bytes on disk now, through the syntax
// check the write path uses, and records that verdict in the ledger.
func observeFreshSyntaxVerdict(ctx *AgentContext, key string, current []byte) {
	// Check these exact bytes NOW, through the same syntax contract the write
	// path uses, and record the result. This is what makes the failure a
	// fresh demonstration rather than a memory: a shell command that rewrote
	// the file left the ledger holding no verdict at all, and no verdict is
	// not evidence of breakage. When the checker cannot run, the observation
	// is unknown and nothing is restored.
	fresh := fallbackSyntaxOutcomeFor(ctx, key, string(current)).aggregate()
	freshKind := ValidationKindSyntax
	if fresh.Status == ValidationNotApplicable {
		freshKind = ValidationKindNone
	}
	observeDeliverable(ctx, key, current, freshKind, fresh.Status, fresh.Detail)
}

// restoreSaferDeliverables walks the session's deliverables in a stable order
// and decides each one independently. Returns only the paths where something
// happened or was deliberately declined after a demonstrated failure -- a
// path with nothing to say produces no disclosure.
func restoreSaferDeliverables(ctx *AgentContext) []restoreDecision {
	if ctx == nil || ctx.WorkingDir == "" {
		return nil
	}
	ctx.LedgerMu.Lock()
	keys := make([]string, 0, len(ctx.Ledger))
	for k, d := range ctx.Ledger {
		// A path with no checkpoint and no failure has no decision worth
		// reporting; skipping it here keeps the disclosure about recovery.
		if d.CheckpointHash == "" && d.ValidationStatus != ValidationFailed {
			continue
		}
		keys = append(keys, k)
	}
	ctx.LedgerMu.Unlock()
	sort.Strings(keys)

	var out []restoreDecision
	for _, k := range keys {
		dec := restoreDeliverable(ctx, k)
		if dec.Restored || dec.Attempted {
			out = append(out, dec)
			continue
		}
		// Declining is only worth saying when the file is actually broken.
		ctx.LedgerMu.Lock()
		broken := ctx.Ledger[k] != nil && ctx.Ledger[k].ValidationStatus == ValidationFailed
		ctx.LedgerMu.Unlock()
		if broken {
			out = append(out, dec)
		}
	}
	return out
}

// restorationDisclosure renders the three outcomes the user must be able to
// tell apart: a file put back, a file left alone, and a recovery that was
// tried and did not work. Per path, named, with no implication that the set
// moved together.
//
// Nothing from the ledger's shape appears here -- no hashes, no status names,
// no generation counts. Only what happened, in the terms a reader can act on.
func restorationDisclosure(decisions []restoreDecision) string {
	if len(decisions) == 0 {
		return ""
	}
	var restored, kept, failed []string
	for _, d := range decisions {
		switch {
		case d.Restored:
			restored = append(restored, d.Path)
		case d.Attempted:
			failed = append(failed, fmt.Sprintf("%s (%s)", d.Path, d.Reason))
		default:
			kept = append(kept, fmt.Sprintf("%s (%s)", d.Path, d.Reason))
		}
	}
	var sb strings.Builder
	if len(restored) > 0 {
		sb.WriteString(fmt.Sprintf(" Put back the last version shown to be valid, file by file: %s",
			strings.Join(restored, ", ")))
		sb.WriteString(" — each was decided on its own, and nothing else was rolled back.")
	}
	if len(kept) > 0 {
		sb.WriteString(fmt.Sprintf(" Left as they are: %s.", strings.Join(kept, ", ")))
	}
	if len(failed) > 0 {
		sb.WriteString(fmt.Sprintf(" Tried and could not restore: %s.", strings.Join(failed, ", ")))
	}
	return sb.String()
}

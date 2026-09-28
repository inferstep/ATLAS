package main

// Declared commands run against exact candidate bytes, before that candidate
// ever touches the user's workspace.
//
// Candidate staging already executes the client's declared commands in an
// isolated overlay and answers with a fully identified result: the request,
// the invocation, the candidate instance, the exact candidate hash, the
// canonical target, the baseline and workspace it was bound to, the exact
// command by hash, and a structured outcome that names refusal, cancellation,
// timeout, budget exhaustion and unobservability separately. That evidence was
// consumed by the authorization decision and by nothing else, so the same
// execution that authorized a delivery could not tell the completion gate its
// own command had run.
//
// This is the missing consumer. It is deliberately NOT a conversion into
// VerificationRecord: that record is the weaker one, and downgrading a staged
// result into it would throw away every identity that makes it trustworthy.
type stagedCommandFulfillment struct {
	// ObligationID and CommandIdentity are both derived from the client's
	// declared command text. The text itself is never held here: Subject
	// never leaves the process, and a hash is enough to match it exactly.
	ObligationID    string
	CommandIdentity string

	RequestID           string
	InvocationID        string
	CandidateInstanceID string
	CandidateHash       string
	// TargetPath is canonical: resolved, cleaned, absolute.
	TargetPath string

	BaselineIdentity    string
	WorkspaceGeneration int
	WorkspaceStateHash  string
}

// recordStagedCommandFulfillment keeps one staged execution that is sound
// enough to stand for its declared command.
//
// Admission is the strict outcome only. A command that exited non-zero, was
// refused, cancelled, timed out, ran past the budget, could not be observed,
// or changed the candidate or the workspace it was measuring produces nothing
// here -- a fact recorded as "unmet" by the caller, not softened into a
// weaker kind of evidence.
func recordStagedCommandFulfillment(ctx *AgentContext, o taskObligation,
	r stagingCommandResult, id stagingIdentity) {
	if ctx == nil {
		return
	}
	if r.Outcome != stagingExitedZero || r.MutatedTarget || r.MutatedWorkspace {
		return
	}
	if o.Kind != ObligationDeclaredCommand || r.ObligationID != o.ID {
		return
	}
	// The command that ran is the command the client declared, matched by the
	// identity the request was built with. No text is compared, normalised or
	// parsed.
	if r.CommandIdentity == "" || r.CommandIdentity != contentSHA256(o.Subject) {
		return
	}
	if ok, _ := id.complete(); !ok {
		return
	}
	// The overlay must have held the candidate itself: staging compares this
	// per command, and a run whose starting bytes were something else is about
	// something else.
	if r.TargetHashBefore != id.CandidateHash {
		return
	}
	ctx.StagedCommands = append(ctx.StagedCommands, stagedCommandFulfillment{
		ObligationID:        o.ID,
		CommandIdentity:     r.CommandIdentity,
		RequestID:           id.RequestID,
		InvocationID:        id.InvocationID,
		CandidateInstanceID: id.CandidateInstanceID,
		CandidateHash:       id.CandidateHash,
		TargetPath:          id.TargetPath,
		BaselineIdentity:    id.BaselineIdentity,
		WorkspaceGeneration: id.WorkspaceGeneration,
		WorkspaceStateHash:  id.WorkspaceStateHash,
	})
}

// stagedFulfillmentCurrent reports whether a staged execution still describes
// the artifact as it is now.
//
// The candidate it ran against has to be what is actually there: consumed
// under its own one-time grant, on disk at exactly those bytes, recorded in
// the ledger at exactly those bytes, and validated at exactly those bytes.
//
// It deliberately does NOT consult a settlement record. Settlement is decided
// after the verification demand, and requiring it here would make each wait
// for the other. Every fact this reads is written by delivery, which happens
// strictly before completion is finalised.
func stagedFulfillmentCurrent(ctx *AgentContext, f stagedCommandFulfillment) bool {
	if ctx == nil || f.RequestID == "" || f.RequestID != requestIDOf(ctx) {
		return false
	}
	// The one-time grant for exactly this candidate at exactly this target was
	// minted, validated and spent. An unconsumed, refused or replayed grant
	// leaves this false.
	if !grantConsumedForCandidate(ctx, f.RequestID, f.InvocationID,
		f.CandidateInstanceID, f.TargetPath, f.CandidateHash) {
		return false
	}
	// The exact bytes landed and are still there.
	if fileSHA256(ctx, f.TargetPath) != f.CandidateHash {
		return false
	}
	// The session's own record agrees, about these bytes, with a verdict that
	// is about these bytes.
	ctx.LedgerMu.Lock()
	d := ctx.Ledger[ledgerKey(ctx, f.TargetPath)]
	var current, validated string
	var status ValidationStatus
	var tombstoned bool
	if d != nil {
		current, validated, status, tombstoned =
			d.CurrentHash, d.ValidatedHash, d.ValidationStatus, d.Tombstoned
	}
	ctx.LedgerMu.Unlock()
	if d == nil || tombstoned {
		return false
	}
	return current == f.CandidateHash && validated == f.CandidateHash &&
		status == ValidationPassed
}

// stagedCommandSatisfied answers the exact-command question from staged
// evidence. Identity only: the declared text hashes to the command that ran.
func stagedCommandSatisfied(ctx *AgentContext, want string) bool {
	if ctx == nil {
		return false
	}
	id, ok := obligationID(ObligationDeclaredCommand, want)
	if !ok {
		return false
	}
	identity := contentSHA256(want)
	for _, f := range ctx.StagedCommands {
		if f.ObligationID == id && f.CommandIdentity == identity &&
			stagedFulfillmentCurrent(ctx, f) {
			return true
		}
	}
	return false
}

// stagedCoverageSatisfied answers the artifact question from staged evidence.
//
// A staged record may answer it -- unlike a pathless direct run -- because it
// is structurally bound to the target and to the exact bytes it ran against,
// and those bytes are required to be the ones on disk now. That binding is the
// coverage; nothing is inferred from the command text.
func stagedCoverageSatisfied(ctx *AgentContext, path, hash string) bool {
	if ctx == nil {
		return false
	}
	for _, f := range ctx.StagedCommands {
		if f.TargetPath == path && f.CandidateHash == hash &&
			stagedFulfillmentCurrent(ctx, f) {
			return true
		}
	}
	return false
}

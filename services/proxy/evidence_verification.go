package main

import (
	"sort"
	"strings"

	"github.com/itigges22/atlas-proxy/internal/taskcontract"
	"github.com/itigges22/atlas-proxy/internal/wire"
)

// Evidence that a command the CLIENT required ran, and what it ran against.
//
// This is the second producer and the first that can reach behavioral
// strength. Its whole claim is narrow: a command the validated request named,
// executed exactly as the client wrote it, exiting successfully against an
// isolated workspace that held these exact candidate bytes and was left
// unchanged by the run.
//
// It executes nothing. Staging runs the command, through the sandbox the rest
// of the system already uses and the safety check the model's own commands go
// through; a producer that ran commands of its own would be a second execution
// route with a second set of rules. What it consumes is the observation
// staging brings back: hashes either side, an exit status and a closed
// outcome, with no command text and no output attached.
//
// Three things it deliberately cannot do:
//
//   - upgrade a model's command. Authority comes from the DECLARATION, not
//     from the text. A command the client never declared has no obligation, so
//     there is nothing for a matching string to attach to.
//   - prove something about an unrelated artifact. A run that named nothing
//     covers nothing, and one that named other files covers those.
//   - close anything but the one command it is about. Two declared commands
//     are two obligations; one passing leaves the other owed.
//
// Nothing consumes what this produces: the record goes to private telemetry,
// and no delivery, completion or generation decision reads it.

// verificationEvidenceRequest is one declared command, one staged run of it,
// and the candidate it is claimed to be about.
type verificationEvidenceRequest struct {
	// Obligation must be a declared-command obligation whose subject is the
	// exact command string the client wrote.
	Obligation taskcontract.TaskObligation
	// Result is what the staging run observed for that command. It is
	// consumed, never synthesised here, and it carries no conclusion: the
	// executor reports hashes and an exit status, and what those mean is
	// decided below.
	Result stagingCommandResult
	// Identity is what the proxy bound the staging run to. Evidence is
	// stamped with the workspace this run actually saw, not with whatever the
	// workspace happens to be by the time anyone reads the record.
	Identity stagingIdentity
}

// produceDeclaredVerificationEvidence is THE client-declared verification
// producer.
//
// Every condition below is necessary, and each is a fact about THIS run rather
// than a property of the command's shape. Nothing here parses a command,
// infers its purpose, or recognises a test runner: the client said what it
// required, and this reports whether that exact thing ran and passed against
// these exact bytes.
func produceDeclaredVerificationEvidence(ctx *AgentContext, req verificationEvidenceRequest) (proxyEvidence, bool) {
	if !verificationRunAdmissible(ctx, req) {
		return proxyEvidence{}, false
	}

	command := req.Obligation.Subject
	// The client's own list, re-read from the validated request. A command the
	// caller never declared has no authority here whoever typed it, and a
	// caller that stated no knowledge declared nothing at all.
	if !requestDeclaredCommand(ctx, command) {
		return proxyEvidence{}, false
	}
	// The client's typed declaration for this exact command, re-read from the
	// validated request rather than accepted from the caller. It is where the
	// strength comes from, so a caller cannot hand in a stronger one than the
	// client wrote -- and an obligation whose floor disagrees with the
	// declaration it should have come from is refused rather than reconciled.
	requirement, declared := requestVerificationRequirement(ctx, command)
	if !declared || requirement.DeclaredStrength() != req.Obligation.RequiredStrength {
		return proxyEvidence{}, false
	}
	// What ran must be what the client wrote, byte for byte. The staging side
	// never sends the text back, so the binding is by identity -- and the
	// identity is recomputed here from the obligation rather than trusted from
	// the result, so a result naming someone else's command matches nothing.
	if req.Result.CommandIdentity != contentSHA256(command) {
		return proxyEvidence{}, false
	}
	// And the result must be about THIS obligation. A staging run that
	// reported an obligation the validated request does not own is refused
	// rather than re-labelled.
	if req.Result.ObligationID != req.Obligation.ID {
		return proxyEvidence{}, false
	}

	requestID := requestIDOf(ctx)
	if strings.TrimSpace(requestID) == "" || req.Identity.RequestID != requestID {
		return proxyEvidence{}, false
	}
	if ok, _ := req.Identity.complete(); !ok {
		return proxyEvidence{}, false
	}
	// The staged bytes must have been the candidate. The executor reported
	// what it saw before the command ran; if that is not the candidate hash,
	// the run happened against something else and covers nothing here.
	if req.Result.TargetHashBefore != req.Identity.CandidateHash {
		return proxyEvidence{}, false
	}

	p := declaredVerificationProvenance(ctx, req, requestID, requirement)
	outcome := wire.ValidationFailed
	if req.Result.passedClean() {
		outcome = wire.ValidationPassed
	}
	return proxyEvidence{Provenance: p, Outcome: outcome}, true
}

// verificationRunAdmissible reports whether this producer may judge the
// request at all: a live context, a declared-command obligation whose floor a
// client can declare, and a staged outcome from the closed set.
func verificationRunAdmissible(ctx *AgentContext, req verificationEvidenceRequest) bool {
	if ctx == nil {
		return false
	}
	if ctx.Ctx != nil && ctx.Ctx.Err() != nil {
		return false
	}
	if req.Obligation.Kind != taskcontract.ObligationDeclaredCommand {
		return false
	}
	// The floor may be any strength a client can declare. What it may not be
	// is a strength no declaration produces: an obligation carrying oracle, or
	// carrying nothing, was not built from a verification requirement.
	if !taskcontract.IsVerificationKind(req.Obligation.RequiredStrength) {
		return false
	}
	if !stagingCommandOutcomes[req.Result.Outcome] {
		return false
	}
	return true
}

// declaredVerificationProvenance is the binding for one staged run of a
// declared command: the identity the run was bound to, the command by hash,
// and the strength the run may be said to have shown.
func declaredVerificationProvenance(ctx *AgentContext, req verificationEvidenceRequest,
	requestID string, requirement taskcontract.VerificationRequirement) wire.V3EvidenceProvenance {
	p := wire.V3EvidenceProvenance{
		Source:              wire.ProvenanceClientDeclaredVerification,
		RequestID:           requestID,
		RouteEntryID:        req.Identity.RouteEntryID,
		InvocationID:        req.Identity.InvocationID,
		CandidateInstanceID: req.Identity.CandidateInstanceID,
		CandidateHash:       req.Identity.CandidateHash,
		// The workspace the staging run was bound to. Recording the CURRENT
		// one would make every record self-consistent and staleness
		// undetectable -- the reader's job is to notice the two have diverged.
		WorkspaceGeneration: req.Identity.WorkspaceGeneration,
		WorkspaceStateHash:  req.Identity.WorkspaceStateHash,
		// The command is named by hash, like every other subject: a command
		// string in an operator log is a content leak.
		CommandIdentity:  req.Result.CommandIdentity,
		BaselineIdentity: req.Identity.BaselineIdentity,
		ObligationID:     req.Obligation.ID,
		RequiredStrength: req.Obligation.RequiredStrength,
		// What the run may be said to have demonstrated: the weaker of what
		// the client declared and what its assets can support. Never oracle --
		// nothing here compared an answer with a reference -- and never
		// behavioral for a command measured against something this session
		// wrote, because a request that authors its own oracle demonstrates
		// only that it agrees with itself.
		ObservedStrength: verificationObservedStrength(ctx, requirement),
	}
	return p
}

// How strong a declared verification may count, measured against what this
// session saw. The requirement types are in
// internal/taskcontract/verification_requirements.go.

// observedAssetAuthority is where the assets actually came from, as this
// session can see it.
//
// The contract's claim is the ceiling, never the floor. A client may say
// workspace_observed about assets it does own, and that is its own business;
// it may not say client_supplied about a file this session wrote, because the
// ledger records that it did.
func observedAssetAuthority(ctx *AgentContext, r taskcontract.VerificationRequirement) string {
	if r.AssetAuthority != taskcontract.AssetAuthorityClientSupplied {
		return taskcontract.AssetAuthorityWorkspaceObserved
	}
	if ctx == nil {
		return taskcontract.AssetAuthorityWorkspaceObserved
	}
	if len(r.Assets) == 0 {
		// Nothing outside the candidate is named, so there is no workspace
		// asset for this session to have authored. The command text itself is
		// the client's, and the candidate is what it runs against.
		return taskcontract.AssetAuthorityClientSupplied
	}
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	for _, a := range r.Assets {
		resolved := resolveAgentPath(ctx, a)
		for key, d := range ctx.Ledger {
			if d == nil {
				continue
			}
			if resolveAgentPath(ctx, key) != resolved {
				continue
			}
			// This session wrote it. Whoever asked for it, the file the
			// command is measured against is not the client's any more.
			return taskcontract.AssetAuthorityWorkspaceObserved
		}
	}
	return taskcontract.AssetAuthorityClientSupplied
}

// verificationObservedStrength is the strongest thing a passing run of this
// requirement may be recorded as having demonstrated.
//
// The weaker of what the client declared and what its assets can support. A
// behavioral requirement whose oracle this session wrote observes runtime, so
// it does not reach its own floor and does not authorize -- which is the
// honest outcome rather than a quietly downgraded requirement.
func verificationObservedStrength(ctx *AgentContext, r taskcontract.VerificationRequirement) string {
	declared := r.DeclaredStrength()
	ceiling, ok := taskcontract.AssetAuthorityCeilingFor(observedAssetAuthority(ctx, r))
	if !ok {
		return taskcontract.VerificationKindRuntime
	}
	if wire.StrengthRank(ceiling) < wire.StrengthRank(declared) {
		return ceiling
	}
	return declared
}

// requestDeclaredCommand reports whether the validated request named this
// exact command. It reads the same decision the completion gate reads, so the
// two cannot disagree about what the client asked for, and it requires stated
// knowledge: a caller that declared nothing declared not-this-command.
func requestDeclaredCommand(ctx *AgentContext, command string) bool {
	if strings.TrimSpace(command) == "" {
		return false
	}
	decision := resolveVerificationObligation(ctx)
	if !decision.KnowledgeSpecified {
		return false
	}
	for _, want := range decision.Items {
		if want == command {
			return true
		}
	}
	return false
}

// declaredVerificationCoverage reports which declared-command obligations have
// an authorizing observation and which are still owed.
//
// Independence is the point: two declared commands are two obligations, and
// one passing says nothing about the other. Missing is returned sorted so a
// caller reporting it does not depend on map order.
func declaredVerificationCoverage(obligations []taskcontract.TaskObligation,
	evidence []proxyEvidence) (met []string, missing []string) {
	authorized := map[string]bool{}
	for _, e := range evidence {
		if e.Provenance.Source != wire.ProvenanceClientDeclaredVerification {
			continue
		}
		if ok, _ := e.Authorizes(); !ok {
			continue
		}
		authorized[e.Provenance.ObligationID] = true
	}
	for _, o := range obligations {
		if o.Kind != taskcontract.ObligationDeclaredCommand || !o.Required {
			continue
		}
		if authorized[o.ID] {
			met = append(met, o.ID)
		} else {
			missing = append(missing, o.ID)
		}
	}
	sort.Strings(met)
	sort.Strings(missing)
	return met, missing
}

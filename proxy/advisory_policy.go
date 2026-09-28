package main

import (
	"sort"
)

// What the delivery rule may look at, and what disqualifies a candidate
// outright.
//
// The split matters more than either half. A veto is a FACT about this
// candidate -- it does not parse, its oracle failed, it touched something it
// was measuring, the workspace moved underneath it. A signal is an opinion
// about quality, and every signal available here comes from the same model, or
// from a service ranking that model's own output, or from a scorer whose
// normalisation may not even be calibrated on this deployment. One veto is
// enough to refuse. No number of signals is enough to prove anything.
//
// So the vetoes decide, and the signals are recorded. Recording them is not
// hedging: a threshold over these signals cannot be chosen honestly until
// there is a measurement relating them to held-out outcomes, and the records
// this policy writes are what that measurement will be computed from.

// The closed veto vocabulary. Each is a disqualifying observation with an
// owner that is not the model: the proxy's own gates, the staging executor, the
// workspace ledger, or the permission owner.
const (
	// VetoSyntaxOrStructural: the candidate does not parse, or introduces a
	// call nothing binds.
	VetoSyntaxOrStructural = "syntax_or_structural_failure"
	// VetoExecutionUnavailable: execution was required to say anything and
	// could not happen. Distinct from a failure: nothing was observed.
	VetoExecutionUnavailable = "execution_evidence_unavailable"
	// VetoMutatedProtectedAssets: the run changed the candidate it was
	// measuring, or the workspace around it.
	VetoMutatedProtectedAssets = "candidate_mutated_protected_assets"
	// VetoLanguageOrTargetMismatch: the candidate is not the artifact class or
	// the target the request is about.
	VetoLanguageOrTargetMismatch = "language_or_target_mismatch"
	// VetoStaleIdentity: the candidate or the workspace it was observed
	// against is not the one about to be written.
	VetoStaleIdentity = "stale_candidate_or_workspace_identity"
	// VetoDeclaredVerificationFailed: a command the client required ran and
	// did not pass.
	VetoDeclaredVerificationFailed = "declared_verification_failed"
	// VetoUnauthorizedPathExpansion: the delivery target is not one the client
	// declared.
	VetoUnauthorizedPathExpansion = "unauthorized_path_expansion"
	// VetoWeakerThanBaseline: the artifact on disk already carries a stronger
	// current verdict than the candidate earned.
	VetoWeakerThanBaseline = "weaker_than_baseline_on_a_trusted_check"
	// VetoCancelledOrTimedOut: the request or the evidence run ended before it
	// could answer.
	VetoCancelledOrTimedOut = "cancelled_or_timed_out"
	// VetoIncompleteEvidence: some declared obligation has no observation at
	// all, so the evidence set does not cover what was asked.
	VetoIncompleteEvidence = "incomplete_evidence"
	// VetoDestructiveWithoutPermission: a destructive operation was implied
	// and the permission owner did not grant it. Advisory confidence is not a
	// permission.
	VetoDestructiveWithoutPermission = "destructive_operation_without_permission"
	// VetoOutsideMutationScope: the candidate is outside the boundary the
	// model's own tool call defined, or that call defined no boundary at all.
	// A scope is not evidence and never authorizes; this is the one direction
	// it acts in.
	VetoOutsideMutationScope = "outside_structured_mutation_scope"
)

var advisoryVetoNames = map[string]bool{
	VetoSyntaxOrStructural:           true,
	VetoExecutionUnavailable:         true,
	VetoMutatedProtectedAssets:       true,
	VetoLanguageOrTargetMismatch:     true,
	VetoStaleIdentity:                true,
	VetoDeclaredVerificationFailed:   true,
	VetoUnauthorizedPathExpansion:    true,
	VetoWeakerThanBaseline:           true,
	VetoCancelledOrTimedOut:          true,
	VetoIncompleteEvidence:           true,
	VetoDestructiveWithoutPermission: true,
	VetoOutsideMutationScope:         true,
}

// advisoryInput is the closed set of typed facts the policy may read.
//
// Every field is something an owner outside the model already decided. There
// is no field for a model claim, a service verdict treated as authority, or a
// hidden evaluator, and there is no free-form map a future caller could smuggle
// one through.
type advisoryInput struct {
	// Observed is the proxy's own gate verdict on the exact candidate bytes.
	Observed checkOutcome
	// TargetDeclared is whether the client stated what this request produces
	// at all. A request that declared nothing names no target to expand
	// beyond, so the path veto has nothing to be about; a request that
	// declared its outputs owns the answer.
	TargetDeclared bool
	// TargetAuthorized is whether this delivery target is one of them.
	TargetAuthorized bool
	// LanguageOrBoundaryViolation is set when a gate found the candidate is
	// not the artifact this route is about: a language swap, or a rewrite
	// past the edit.
	LanguageOrBoundaryViolation bool
	// Unmet is why each declared obligation went unsatisfied, from the
	// staging owner. The reasons are what separate "it failed" from "it never
	// ran".
	Unmet map[string]AuthorizationReason
	// Decision is the typed authorization answer over the same candidate.
	Decision AuthorizationDecision
	// Evidence is what the trusted producers observed about these bytes.
	Evidence []proxyEvidence
	// Envelope is the service's own record. Advisory only: it is read for
	// ranking signals and never for authority.
	Envelope *V3EvidenceEnvelope
	// Cancelled is whether the request itself is over.
	Cancelled bool
	// DestructivePermitted is the permission owner's answer when the route
	// implies a destructive operation, and true when it implies none.
	DestructivePermitted bool
	// DestructiveImplied says whether it does.
	DestructiveImplied bool
	// MutatedProtectedAssets is the staging owner's report that a run changed
	// the candidate it was measuring or the workspace around it. It is a
	// separate fact from a failing command and is named separately.
	MutatedProtectedAssets bool
	// ScopeAdmits is whether the model's own tool call bounds a mutation that
	// contains these bytes. False covers both "it does not" and "there is no
	// scope", and ScopeRefusal says which.
	ScopeAdmits bool
	// ScopeRefusal is the closed reason, for the record.
	ScopeRefusal string
	// CaptureOnlySuppressed is set when an acquisition control took the licence
	// away from a decision that had earned it. The decision below is still
	// computed and recorded as what it was; only Delivers changes.
	CaptureOnlySuppressed bool
	// AutomaticEligible is the authorization owner's answer to the one
	// question automatic_v3 adds: are these the exact bytes the V3 selection
	// path named, under a grant this machine minted. It is computed there and
	// never here, because a policy that decided its own authorization would be
	// the service certifying itself with an extra step.
	AutomaticEligible bool
	// Vetoes, when non-nil, are the disqualifying facts already computed for
	// this candidate by the single owner. The authorization owner needs them
	// before it can mint an automatic grant, and computing them twice is how
	// two answers about the same candidate come to disagree.
	Vetoes []string
}

// advisoryVetoes are the disqualifying facts observed about this candidate, in
// canonical order.
//
// Read the reasons, not just the outcomes. A command that could not run
// because the executor was unreachable and one that ran and failed are
// different facts, and only the second is about the candidate -- but neither
// permits a preference, so both veto.
func advisoryVetoes(in advisoryInput) []string {
	fired := map[string]bool{}

	if in.Observed.Status == ValidationFailed {
		fired[VetoSyntaxOrStructural] = true
	}
	if in.Observed.Status == ValidationNotRun || in.Observed.Status == ValidationUnknown {
		// A check that applies to these bytes did not run -- the sandbox was
		// down or stopped the checker -- or answered nothing readable. Nothing
		// spoke for the candidate, and replacing what the model wrote needs
		// something that did. Without this, an automatic delivery landed on a
		// syntax check that never ran.
		fired[VetoExecutionUnavailable] = true
	}
	if in.LanguageOrBoundaryViolation {
		fired[VetoLanguageOrTargetMismatch] = true
	}
	if in.TargetDeclared && !in.TargetAuthorized {
		fired[VetoUnauthorizedPathExpansion] = true
	}
	if in.Cancelled {
		fired[VetoCancelledOrTimedOut] = true
	}
	if in.DestructiveImplied && !in.DestructivePermitted {
		fired[VetoDestructiveWithoutPermission] = true
	}
	if in.MutatedProtectedAssets {
		fired[VetoMutatedProtectedAssets] = true
	}
	if !in.ScopeAdmits {
		fired[VetoOutsideMutationScope] = true
	}

	for _, why := range in.Unmet {
		switch why {
		case ReasonEvidenceExecutionFailed:
			fired[VetoDeclaredVerificationFailed] = true
		case ReasonProducerUnavailable, ReasonProducerNotRun,
			ReasonEvidenceResourceExhausted:
			// Resource exhaustion belongs HERE and not with
			// VetoDeclaredVerificationFailed: the execution that would have
			// produced this evidence never reached its own end, so what is
			// missing is the observation, not the candidate's case.
			fired[VetoExecutionUnavailable] = true
		case ReasonEvidenceTimedOut, ReasonEvidenceCancelled:
			fired[VetoCancelledOrTimedOut] = true
		case ReasonEvidenceRefused:
			fired[VetoExecutionUnavailable] = true
		case ReasonEvidenceMissing:
			fired[VetoIncompleteEvidence] = true
		}
	}

	switch in.Decision.Reason {
	case ReasonBaselineNotPreserved:
		fired[VetoWeakerThanBaseline] = true
	case ReasonWorkspaceStale, ReasonCandidateMismatch, ReasonRequestOrInvocationMismatch,
		ReasonCommandMismatch:
		fired[VetoStaleIdentity] = true
	case ReasonTargetNotDeclared:
		fired[VetoUnauthorizedPathExpansion] = true
	}
	// An obligation with no observation at all is not covered, whatever else
	// was seen. The decision reports exactly which ones.
	if len(in.Decision.Missing) > 0 && in.Decision.Reason != ReasonAuthorized {
		fired[VetoIncompleteEvidence] = true
	}

	out := make([]string, 0, len(fired))
	for name := range fired {
		if advisoryVetoNames[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// advisorySignals are the quality observations available, recorded verbatim.
//
// None of them is consulted as a threshold, and the reason is written into the
// record rather than left to memory: every one is either the same model
// grading its own output, a service ranking that output, or a scorer whose
// normalisation carries its own calibration flag. Values here describe what
// was seen. They do not describe how likely the candidate is to be correct,
// and nothing may present them as if they did.
func advisorySignals(in advisoryInput) map[string]interface{} {
	out := map[string]interface{}{
		"proxy_gate_status":     string(in.Observed.Status),
		"trusted_observations":  len(in.Evidence),
		"authorization_reason":  string(in.Decision.Reason),
		"scope_admits":          in.ScopeAdmits,
		"scope_refusal":         in.ScopeRefusal,
		"obligations_satisfied": len(in.Decision.Satisfied),
		"obligations_missing":   len(in.Decision.Missing),
	}
	strongest := ""
	for _, ev := range in.Evidence {
		if strengthRank(ev.Provenance.ObservedStrength) > strengthRank(strongest) {
			strongest = ev.Provenance.ObservedStrength
		}
	}
	out["strongest_trusted_strength"] = strongest
	if in.Envelope != nil {
		// The service's own record, labelled as the service's. A reader of
		// this map must be able to tell at a glance which side produced each
		// number, because only one side of it is trusted for anything.
		out["service_closure_eligible"] = in.Envelope.Evaluation.ClosureEligible
		out["service_evidence_strength"] = in.Envelope.Evaluation.EvidenceStrength
		out["service_execution_status"] = in.Envelope.Evaluation.ExecutionStatus
		out["service_requirements_complete"] = in.Envelope.Evaluation.RequirementsComplete
		out["service_selection_status"] = in.Envelope.Selection.Status
		out["service_tied_count"] = in.Envelope.Selection.TiedCount
		out["service_incomparable_count"] = in.Envelope.Selection.IncomparableCount
		out["service_ineligible_count"] = in.Envelope.Selection.IneligibleCount
		out["service_coverage_missing"] = len(in.Envelope.Coverage.Missing)
	}
	return out
}

// decideCandidatePolicy is THE policy owner.
//
// Order is the whole design. Vetoes first, because a disqualifying fact is not
// something a strong signal elsewhere can outweigh. Then a declared
// verification that passed, the strongest thing a candidate can show. Then the
// V3 selection basis, once nothing disqualifying was observed and the
// authorization owner approved the candidate's identity. Otherwise the
// model's own bytes stand.
func decideCandidatePolicy(ctx *AgentContext, in advisoryInput,
	strictAuthorized bool) candidatePolicyOutcome {
	vetoes := in.Vetoes
	if vetoes == nil {
		vetoes = advisoryVetoes(in)
	}
	out := candidatePolicyOutcome{
		Mode: CandidatePolicyAutomaticV3, Source: CandidatePolicySourceFixed,
		Vetoes:  vetoes,
		Signals: advisorySignals(in),
	}
	if len(out.Vetoes) > 0 {
		out.Decision = PolicyCandidateRejectedHardVeto
		return out
	}
	// The two decisions that deliver, chosen here and acted on once below.
	// One assignment, so the acquisition control cannot be applied to one
	// delivering decision and forgotten on the other.
	delivers := false
	switch {
	case strictAuthorized:
		out.Decision, delivers = PolicyCandidateAuthorizedStrict, true
	case in.AutomaticEligible:
		out.Decision, delivers = PolicyCandidateAutomaticV3, true
	default:
		out.Decision = PolicyBaselineRetained
	}
	// The acquisition control takes the licence and leaves the answer, in one
	// place, for every decision that earned one.
	out.Delivers = delivers && !in.CaptureOnlySuppressed
	return out
}

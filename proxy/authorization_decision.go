package main

import (
	"sort"
	"strings"
)

// Whether a candidate WOULD be authorized to land, computed and not consulted.
//
// Every piece of the answer already exists: the request says which targets it
// owns, the task says what it obliges, the adapter says what it can observe,
// the evidence says where it came from and what it is about. Nothing had ever
// put them together, so "may these bytes land" was still answered by
// v3DeliveryAuthorized reading a selection status.
//
// This is that answer, assembled from the typed parts, and it is observe-only.
// It reaches no write, no ledger, no completion, no prompt and no generation
// decision; the existing delivery decisions remain authoritative and unchanged.
// What it produces is a private record saying what would have happened, which
// is the only way to find out whether the typed answer agrees with the live one
// before letting it decide anything.

// AuthorizationReason is the closed vocabulary of why a decision came out the
// way it did. A reason outside this set is a state nobody classified, which is
// a refusal rather than a default.
type AuthorizationReason string

const (
	// Granted.
	ReasonAuthorized AuthorizationReason = "authorized"

	// Refused.
	ReasonTargetNotDeclared             AuthorizationReason = "target_not_declared"
	ReasonObligationUnknown             AuthorizationReason = "obligation_unknown"
	ReasonAdapterUnsupported            AuthorizationReason = "adapter_unsupported"
	ReasonEvidenceMissing               AuthorizationReason = "evidence_missing"
	ReasonEvidenceTooWeak               AuthorizationReason = "evidence_too_weak"
	ReasonProvenanceUntrusted           AuthorizationReason = "provenance_untrusted"
	ReasonCandidateMismatch             AuthorizationReason = "candidate_mismatch"
	ReasonRequestOrInvocationMismatch   AuthorizationReason = "request_or_invocation_mismatch"
	ReasonWorkspaceStale                AuthorizationReason = "workspace_stale"
	ReasonBaselineNotPreserved          AuthorizationReason = "baseline_not_preserved"
	ReasonCommandMismatch               AuthorizationReason = "command_mismatch"
	ReasonLegacyRecord                  AuthorizationReason = "legacy_record"
	ReasonPostDeliverySettlementPending AuthorizationReason = "post_delivery_settlement_pending"
	// ReasonNoAuthorizationPrerequisite is a DECLARED target that states
	// nothing demonstrable about itself. A markdown file is the ordinary case:
	// the client asked for it, and the structural gate does not govern its
	// class, so there is no prerequisite for evidence to satisfy.
	//
	// Distinct from target_not_declared, which says the client never asked for
	// this artifact at all, and from evidence_missing, which says something
	// was owed and nothing spoke for it. Collapsing them would tell a caller
	// its declared document was undeclared.
	ReasonNoAuthorizationPrerequisite AuthorizationReason = "no_authorization_prerequisite"

	// Why a prerequisite went unmet, when something is known about it.
	//
	// evidence_missing is the honest answer only when a producer WAS available
	// and simply did not speak. Reporting it for a sandbox that was down says
	// the candidate had nothing to show for itself, when in truth nothing was
	// checked -- a different fact, and the one an operator needs.
	//
	// ReasonProducerUnavailable: the thing that produces this evidence could
	// not be reached.
	ReasonProducerUnavailable AuthorizationReason = "trusted_producer_unavailable"
	// ReasonProducerNotRun: it was reachable and was not asked.
	ReasonProducerNotRun AuthorizationReason = "trusted_producer_not_run"
	// ReasonEvidenceExecutionFailed: it ran and what it observed does not
	// support the candidate. This IS a fact about the candidate.
	ReasonEvidenceExecutionFailed AuthorizationReason = "evidence_execution_failed"
	// ReasonEvidenceRefused: a safety owner declined to run it.
	ReasonEvidenceRefused AuthorizationReason = "evidence_safety_refused"
	// ReasonEvidenceTimedOut: it did not finish inside its budget.
	ReasonEvidenceTimedOut AuthorizationReason = "evidence_timed_out"
	// ReasonEvidenceCancelled: the request ended before it could.
	ReasonEvidenceCancelled AuthorizationReason = "evidence_cancelled"
	// ReasonEvidenceResourceExhausted: it was stopped at a memory, process or
	// output ceiling before it reached its own conclusion. Deliberately NOT
	// ReasonEvidenceExecutionFailed: a command killed at a ceiling exits
	// non-zero exactly like a failing one, and recording it as a failure
	// asserts something about the candidate that nothing observed.
	ReasonEvidenceResourceExhausted AuthorizationReason = "evidence_resource_exhausted"
	// ReasonUnknown is never chosen deliberately. It is what a contradictory
	// or unclassified state becomes, and it never authorizes.
	ReasonUnknown AuthorizationReason = "unknown"
)

var authorizationReasons = map[AuthorizationReason]bool{
	ReasonAuthorized: true, ReasonTargetNotDeclared: true,
	ReasonObligationUnknown: true, ReasonAdapterUnsupported: true,
	ReasonEvidenceMissing: true, ReasonEvidenceTooWeak: true,
	ReasonProvenanceUntrusted: true, ReasonCandidateMismatch: true,
	ReasonRequestOrInvocationMismatch: true, ReasonWorkspaceStale: true,
	ReasonBaselineNotPreserved: true, ReasonCommandMismatch: true,
	ReasonLegacyRecord: true, ReasonPostDeliverySettlementPending: true,
	ReasonNoAuthorizationPrerequisite: true, ReasonProducerUnavailable: true,
	ReasonProducerNotRun: true, ReasonEvidenceExecutionFailed: true,
	ReasonEvidenceRefused: true, ReasonEvidenceTimedOut: true,
	ReasonEvidenceCancelled: true, ReasonUnknown: true,
	ReasonEvidenceResourceExhausted: true,
}

// authorizingReasons are the ones that grant. Exactly one, and naming the set
// is what stops a new member being added on the granting side by accident.
var authorizingReasons = map[AuthorizationReason]bool{ReasonAuthorized: true}

// AuthorizationDecision is one complete answer about one candidate.
//
// It names what it consumed as well as what it concluded. A decision that
// cannot say which evidence it would have relied on is one nobody can audit
// against what actually landed.
type AuthorizationDecision struct {
	Authorized bool
	Reason     AuthorizationReason

	// Satisfied and Missing are obligation ids, sorted. Together they account
	// for every authorization prerequisite the task stated.
	Satisfied []string
	Missing   []string

	// EvidenceConsumed names the candidate-instance/obligation pairs this
	// decision would have relied on, sorted. Identities only.
	EvidenceConsumed []string

	// SettlementRequired is true whenever the task states a post-delivery
	// obligation that is not yet answerable. Authorization does not wait for
	// it -- that is the circle this design removed -- but a caller that
	// delivers still owes it afterwards.
	SettlementRequired bool

	// InfluencesLiveDecision says whether this answer decided anything. It is
	// true for a request that declared structured obligations, because there
	// the typed path owns delivery, and false for contractless traffic, where
	// it has nothing to be about.
	InfluencesLiveDecision bool
}

// authorizationInput is everything the decision may look at. Assembling it
// explicitly is what stops the decision reaching for something else.
type authorizationInput struct {
	// Obligations are the task's own, derived from the validated request.
	Obligations []taskObligation
	// TargetPath is the canonical artifact the delivery would replace.
	TargetPath string
	// CandidateHash names the exact bytes proposed.
	CandidateHash string
	// Identity is what the evidence must bind to.
	Identity V3EvidenceProvenance
	// RouteEntry names the entry of the candidate-generation route this
	// decision answers for. It is separate from Identity on purpose: a
	// refusal reached before any candidate evidence exists has no invocation
	// and no candidate instance, and inventing them to have something to
	// record would be a fabricated identity. The route entry always exists by
	// the time a decision is made, so a refusal can still be attributed.
	RouteEntry routeEntry
	// Evidence is what was actually observed about this candidate.
	Evidence []proxyEvidence
	// Envelope is the service's own record, consulted only for adapter
	// support and legacy status.
	Envelope *V3EvidenceEnvelope
	// BaselineWitnessCommand identifies the command whose pass established the
	// behavioural baseline the candidate would replace, when there is one. It
	// is "" for a syntax baseline, which has no command behind it, and for a
	// target with no baseline at all.
	BaselineWitnessCommand string
	// Unmet explains, per obligation id, why a prerequisite could not be
	// satisfied, when the caller observed something more specific than its
	// absence. Closed vocabulary; identities only.
	Unmet map[string]AuthorizationReason
	// OutputKnowledgeDeclared is whether the client STATED what this request
	// produces. It is presence-aware and is the only thing that decides
	// whether this owns the answer.
	//
	// Not len(Obligations): a contract declaring `expected_outputs: []` states
	// authoritatively that it produces nothing, and derives no obligations for
	// exactly that reason. Reading the count would make the most explicit
	// statement a client can make indistinguishable from silence, and would
	// hand the candidate to the legacy decision on the strength of it.
	OutputKnowledgeDeclared bool
	// StructuredTargetGrounded is the other way this decision owns a request:
	// automatic_v3 was selected and the structured mutation target of the tool
	// call names exactly the candidate's target. Set by the delivery owner from
	// structuredMutationTargetGrounds; read here only to say truthfully that
	// the answer influences a live decision.
	StructuredTargetGrounded bool
	// Scope is the structured intent of the tool call this candidate belongs
	// to: which target it may touch and what boundary it may not leave. It
	// narrows and never grants -- a licence is refused without one, and no
	// scope has ever caused one to be minted.
	Scope mutationScope
	// CandidateBytes are the exact bytes the decision is about. They are read
	// only to ask the scope whether they stayed inside the boundary, and they
	// never enter a record, a log or a hash other than the one already carried
	// as CandidateHash.
	CandidateBytes string
}

// baselinePreservedBy answers whether the evidence that authorized this
// candidate ALSO shows the baseline it replaces has survived.
//
// Preservation is derived, never produced: no producer owns it, and one that
// did would be asserting a comparison rather than observing a fact. The
// derivation is the comparison, and it has one rule it may not break --
// evidence never gets stronger by being compared. A syntax pass over the
// candidate says the candidate parses. Against a syntax baseline that is
// exactly the same claim the baseline holds, so it preserves it. Against a
// behavioural baseline it is a weaker claim about a stronger one, and calling
// that "preserved" is how a working artifact gets replaced by one that merely
// compiles.
//
// Every record here has already been matched against the asked-for identity,
// so it is current, about these exact candidate bytes, and about this
// invocation. What is left to decide is strength and witness.
func baselinePreservedBy(o taskObligation, witnessCommand string,
	authorizing []proxyEvidence) (bool, string) {
	required := o.RequiredStrength
	if strengthRank(required) < 0 {
		return false, "unclassified baseline strength"
	}
	for _, ev := range authorizing {
		p := ev.Provenance
		if strengthRank(p.ObservedStrength) < strengthRank(required) {
			// Weaker evidence never preserves a stronger baseline.
			continue
		}
		if required == "syntax" {
			// A syntax baseline is preserved by current syntax evidence over
			// the exact candidate bytes. Anything at or above that strength,
			// already bound to this candidate, is that.
			return true, ""
		}
		// A behavioural or oracle baseline was established by something being
		// RUN. Preserving it needs the same thing run again on the candidate,
		// not a different command that also passed.
		if witnessCommand == "" {
			return false, "the baseline names no command to re-run"
		}
		if p.CommandIdentity != witnessCommand {
			continue
		}
		return true, ""
	}
	return false, "no current evidence reaches the baseline's " + required
}

// decideAuthorization is THE observe-only authorization owner.
//
// The order of the checks is the order in which an answer stops being possible:
// identity before obligations, obligations before evidence, evidence before
// strength. A refusal names the first thing that was wrong, so a reader learns
// what to fix rather than that something was.
// influencesLiveDecision is the one derivation of the decision's own weight:
// it reaches a live delivery when the request declared output knowledge, or
// when automatic_v3 grounded the target in the model's structured call. The
// decision and its record both read this, so they cannot disagree, and neither
// hardcodes an answer.
func (in authorizationInput) influencesLiveDecision() bool {
	return in.OutputKnowledgeDeclared || in.StructuredTargetGrounded
}

func decideAuthorization(ctx *AgentContext, in authorizationInput) AuthorizationDecision {
	// Whether this answer decides anything is a property of the REQUEST, not
	// of the answer: a request that STATED what it produces is one this owns,
	// and a request that stated nothing is not.
	d := AuthorizationDecision{
		Reason:                 ReasonUnknown,
		InfluencesLiveDecision: in.influencesLiveDecision(),
	}

	// No stated output knowledge, so there is nothing to authorize on. This is
	// not a refusal of the request -- legacy delivery is unchanged and
	// authoritative -- it is this decision declining to have an opinion.
	if !in.OutputKnowledgeDeclared {
		d.Reason = ReasonLegacyRecord
		return d
	}
	// A record produced before provenance existed carries no authority, and a
	// service record whose envelope is unusable is not evidence about anything.
	if in.Envelope != nil {
		if availability, _ := in.Envelope.Validate(); availability != EvidenceAvailable {
			d.Reason = ReasonLegacyRecord
			return d
		}
		if !in.Envelope.Evaluation.Supported {
			d.Reason = ReasonAdapterUnsupported
			return d
		}
	}
	// The client must have asked for this artifact. Identity is necessary and
	// never sufficient: everything below still has to hold.
	//
	// A declared-and-empty output set arrives here with no targets at all, and
	// takes this branch: it authorizes nothing, which is exactly what it says.
	if !targetIsAuthorized(in.Obligations, in.TargetPath) {
		d.Reason = ReasonTargetNotDeclared
		return d
	}
	if strings.TrimSpace(in.CandidateHash) == "" {
		d.Reason = ReasonCandidateMismatch
		return d
	}

	prerequisites := authorizationPrerequisites(in.Obligations)
	// Settlement is owed whenever the task states one and the bytes are not
	// yet confirmed. It never blocks authorization. Without a context there is
	// nothing to confirm against, so it is owed by definition.
	settled := false
	if ctx != nil {
		settled, _ = settlementIsComplete(ctx, in.Obligations, in.CandidateHash)
	}
	if !settled {
		d.SettlementRequired = len(postDeliverySettlement(in.Obligations)) > 0
	}

	// An obligation nobody classified, or one nothing can satisfy, ends the
	// question before any evidence is looked at.
	for _, o := range prerequisites {
		if _, ok := obligationRole(o.Kind); !ok {
			d.Reason = ReasonObligationUnknown
			d.Missing = append(d.Missing, o.ID)
			sort.Strings(d.Missing)
			return d
		}
		if obligationUnsatisfiableKinds[o.Kind] {
			d.Reason = ReasonObligationUnknown
			d.Missing = append(d.Missing, o.ID)
			sort.Strings(d.Missing)
			return d
		}
		if strengthRank(o.RequiredStrength) < 0 {
			d.Reason = ReasonObligationUnknown
			d.Missing = append(d.Missing, o.ID)
			sort.Strings(d.Missing)
			return d
		}
	}

	// Match evidence to obligations. The first refusal encountered is the one
	// reported, and it is specific: a caller learns that the workspace moved
	// rather than that "evidence was missing".
	// What each prerequisite's evidence must name, derived from the client's
	// own declaration. An obligation absent from this map is one the validated
	// request does not own, and evidence for it satisfies nothing.
	wantCommand := map[string]string{}
	for _, o := range prerequisites {
		if o.Kind == ObligationDeclaredCommand {
			wantCommand[o.ID] = contentSHA256(o.Subject)
			continue
		}
		wantCommand[o.ID] = ""
	}

	satisfied := map[string]bool{}
	var authorizing []proxyEvidence
	firstRefusal := ReasonUnknown
	for _, ev := range in.Evidence {
		want, owned := wantCommand[ev.Provenance.ObligationID]
		if !owned {
			if firstRefusal == ReasonUnknown {
				firstRefusal = ReasonObligationUnknown
			}
			continue
		}
		reason := evidenceRefusalFor(ev, in.Identity, want)
		if reason == ReasonAuthorized {
			satisfied[ev.Provenance.ObligationID] = true
			authorizing = append(authorizing, ev)
			d.EvidenceConsumed = append(d.EvidenceConsumed,
				ev.Provenance.CandidateInstanceID+"/"+ev.Provenance.ObligationID)
			continue
		}
		if firstRefusal == ReasonUnknown {
			firstRefusal = reason
		}
	}
	sort.Strings(d.EvidenceConsumed)

	// Baseline preservation is settled last, because it is derived from what
	// the rest of the evidence already established rather than observed on its
	// own.
	for _, o := range prerequisites {
		if o.Kind != ObligationBaselinePreserved || satisfied[o.ID] {
			continue
		}
		if ok, _ := baselinePreservedBy(o, in.BaselineWitnessCommand, authorizing); ok {
			satisfied[o.ID] = true
		}
	}

	for _, o := range prerequisites {
		if !o.Required {
			continue
		}
		if satisfied[o.ID] {
			d.Satisfied = append(d.Satisfied, o.ID)
		} else {
			d.Missing = append(d.Missing, o.ID)
		}
	}
	sort.Strings(d.Satisfied)
	sort.Strings(d.Missing)

	if len(d.Missing) > 0 {
		// A prerequisite nothing spoke for is missing evidence; one something
		// spoke for badly gets that something's own reason.
		d.Reason = ReasonEvidenceMissing
		if firstRefusal != ReasonUnknown {
			d.Reason = firstRefusal
		}
		// And when the caller knows WHY a prerequisite went unmet -- the
		// producer was down, refused, timed out, or ran and found against the
		// candidate -- that is the truthful answer. `evidence_missing` for a
		// sandbox that was never reachable says the candidate had nothing to
		// show for itself, when in truth nothing was checked.
		//
		// Sorted order, so the reported reason does not depend on map order.
		if len(in.Unmet) > 0 {
			for _, id := range d.Missing {
				if why, known := in.Unmet[id]; known && authorizationReasons[why] &&
					!authorizingReasons[why] {
					d.Reason = why
					break
				}
			}
		}
		// A baseline obligation left unmet is its own fact, and the more
		// useful one to report.
		for _, o := range prerequisites {
			if o.Kind == ObligationBaselinePreserved && !satisfied[o.ID] {
				d.Reason = ReasonBaselineNotPreserved
				break
			}
		}
		return d
	}
	if len(d.Satisfied) == 0 {
		// The client asked for this artifact and its class states nothing
		// demonstrable -- a document, ordinarily. Target identity alone does
		// not authorize arbitrary bytes, and saying so precisely matters: this
		// is a DECLARED target with no prerequisite, not an undeclared one and
		// not one whose evidence went missing.
		// Reached only when nothing was OWED: a prerequisite that was owed and
		// unmet took the branch above and reported evidence_missing there.
		d.Reason = ReasonNoAuthorizationPrerequisite
		return d
	}

	d.Authorized = true
	d.Reason = ReasonAuthorized
	return d
}

// evidenceRefusalFor is why one piece of evidence cannot be relied on, or
// ReasonAuthorized when it can.
//
// Ordered so the most specific mismatch wins: a record about a different
// candidate is a candidate mismatch even though its workspace also differs.
func evidenceRefusalFor(ev proxyEvidence, asked V3EvidenceProvenance,
	wantCommand string) AuthorizationReason {
	p := ev.Provenance
	if _, ok := provenanceCeiling[p.Source]; !ok {
		return ReasonProvenanceUntrusted
	}
	if p.RequestID != asked.RequestID || p.InvocationID != asked.InvocationID {
		return ReasonRequestOrInvocationMismatch
	}
	if p.CandidateHash != asked.CandidateHash ||
		p.CandidateInstanceID != asked.CandidateInstanceID {
		return ReasonCandidateMismatch
	}
	// The command identity is per-obligation, not per-candidate: a syntax
	// record names no command, and a behavioral one must name the exact
	// command ITS obligation declared. The expected value is derived by the
	// caller from the validated request, never read off the record.
	if p.CommandIdentity != wantCommand {
		return ReasonCommandMismatch
	}
	if p.BaselineIdentity != asked.BaselineIdentity {
		return ReasonBaselineNotPreserved
	}
	if p.WorkspaceGeneration != asked.WorkspaceGeneration ||
		p.WorkspaceStateHash != asked.WorkspaceStateHash {
		return ReasonWorkspaceStale
	}
	if ev.Outcome != ValidationPassed {
		return ReasonEvidenceMissing
	}
	if ok, _ := p.MayAuthorize(); !ok {
		// Source and strength alone. Distinguish "this source may never close
		// this" from "it could, but did not reach the floor".
		ceiling := provenanceCeiling[p.Source]
		if strengthRank(ceiling) < strengthRank(p.RequiredStrength) {
			return ReasonProvenanceUntrusted
		}
		return ReasonEvidenceTooWeak
	}
	return ReasonAuthorized
}

// recordAuthorizationDecision writes the shadow decision to private telemetry.
//
// Identities, obligation ids and a closed reason. No candidate byte, no
// command string, no path content. influences_live_decision is false and is a
// fact about this build: nothing consumes what this returns.
func recordAuthorizationDecision(ctx *AgentContext, in authorizationInput,
	d AuthorizationDecision) {
	sink := activeShadowSink.Load()
	if !sink.enabled() {
		return
	}
	reason := d.Reason
	if !authorizationReasons[reason] {
		// An unclassified reason is written as the fail-closed member rather
		// than as arbitrary prose.
		reason = ReasonUnknown
	}
	sink.submit(map[string]interface{}{
		"schema_version":        shadowSchemaVersionAuthorization,
		"record_kind":           "candidate_authorization_decision",
		"request_id":            in.Identity.RequestID,
		"route_entry_id":        in.RouteEntry.ID,
		"invocation_id":         in.Identity.InvocationID,
		"candidate_instance_id": in.Identity.CandidateInstanceID,
		"candidate_hash":        in.CandidateHash,
		"authorized":            d.Authorized,
		"reason":                string(reason),
		"obligations_satisfied": d.Satisfied,
		"obligations_missing":   d.Missing,
		"evidence_consumed":     d.EvidenceConsumed,
		"settlement_required":   d.SettlementRequired,
		// A fact about the wiring rather than an intention: for a request that
		// STATED what it produces, this decision is what decides whether the
		// candidate lands. Read from the same field the decision itself reads
		// -- deriving it here from the obligation count would make the record
		// disagree with the answer it describes, and would call a
		// verification-only contract owned when the output route is not.
		"influences_live_decision":   in.influencesLiveDecision(),
		"structured_target_grounded": in.StructuredTargetGrounded,
		"build_version":              APIVersion,
	})
}

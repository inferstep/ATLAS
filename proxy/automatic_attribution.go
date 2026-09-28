package main

import "strings"

// Why an automatic candidate did not land, as a closed, typed, observe-only
// shadow record.
//
// The automatic decision is made by four owners in sequence -- the route's own
// gates, the authorization owner (automaticDeliveryAllowed, then the grant
// mint), the policy owner and the delivery owner -- and each already names
// what it found. None of that was written down: the policy record says
// baseline_retained, the disposition says how the entry ended, and a reader
// who wanted to know WHICH requirement the candidate failed had nothing to
// read. This file writes it down.
//
// Everything here is derived from decisions the live owners already made and
// handed over verbatim: the mode the policy resolver returned, the refusal the
// eligibility owner named, the mint's reason, the delivery outcome, the
// disposition the lifecycle recorded. Nothing is recomputed, nothing is read
// back by any owner, and with the capture off nothing is built.

// automaticRefusal is the closed vocabulary. Every value is a fact one owner
// established, never a judgement of the candidate.
type automaticRefusal string

const (
	// automaticRefusalNone: the candidate landed. A landed candidate has no
	// refusal reason.
	automaticRefusalNone automaticRefusal = ""
	// automaticRefusalRouteNotEntered is the analysis-side reading of a
	// mutation the candidate producer was never consulted for. No route entry
	// exists for it, so no attribution record is written; the
	// candidate_generation_bypass record carries the predicate that turned it
	// away, and an analyser joins the two on the request. Registered here so
	// the vocabulary an analysis reads is one closed set.
	automaticRefusalRouteNotEntered automaticRefusal = "route_not_entered"
	// automaticRefusalNoCandidate: the producer returned nothing, or nothing
	// materially different from the caller's own bytes.
	automaticRefusalNoCandidate automaticRefusal = "no_candidate_produced"
	// automaticRefusalV3Unavailable: the producer could not be reached, or
	// generation is disabled for this session.
	automaticRefusalV3Unavailable automaticRefusal = "v3_unavailable"
	// automaticRefusalV3TimedOut: the producer did not answer in time.
	automaticRefusalV3TimedOut automaticRefusal = "v3_timed_out"
	// automaticRefusalCancelled: the request ended before a candidate could
	// land, whether the route or a veto observed it.
	automaticRefusalCancelled automaticRefusal = "cancelled"
	// automaticRefusalRouteGateRevoked: the route's own gate withdrew the
	// candidate before policy -- it rewrote beyond the edit, swapped the
	// language, did not parse, or was not closure-eligible.
	automaticRefusalRouteGateRevoked automaticRefusal = "route_gate_revoked"
	// automaticRefusalHardVeto: the policy's veto owner observed a
	// disqualifying fact not covered by a more specific reason below.
	automaticRefusalHardVeto automaticRefusal = "hard_veto"
	// automaticRefusalNoSelection: the service named no selected candidate.
	automaticRefusalNoSelection automaticRefusal = "no_selection_identity"
	// automaticRefusalSelectedHashMismatch: the service selected something
	// other than the bytes that arrived, or the bytes a grant was spent on
	// were not the ones it named.
	automaticRefusalSelectedHashMismatch automaticRefusal = "selected_hash_mismatch"
	// automaticRefusalIdentityIncomplete: the binding identity for the bytes
	// was not complete enough to mint a grant on.
	automaticRefusalIdentityIncomplete automaticRefusal = "candidate_identity_incomplete"
	// automaticRefusalNoScope: the tool call bounded no mutation scope.
	automaticRefusalNoScope automaticRefusal = "no_mutation_scope"
	// automaticRefusalTargetNotGrounded: nothing grounded the target -- no
	// declared output and no structured mutation target.
	automaticRefusalTargetNotGrounded automaticRefusal = "target_not_grounded"
	// automaticRefusalTargetMismatch: the target the grant or delivery was
	// about is not the one the structured call named.
	automaticRefusalTargetMismatch automaticRefusal = "target_mismatch"
	// automaticRefusalScopeExpansion: the candidate left its mutation
	// boundary, or reached a path nobody authorised.
	automaticRefusalScopeExpansion automaticRefusal = "scope_expansion"
	// automaticRefusalStaleBaseline: the target or the workspace moved between
	// the decision and the delivery, or the candidate's identity is stale.
	automaticRefusalStaleBaseline automaticRefusal = "stale_baseline"
	// automaticRefusalAuthorizationUnavailable: no authorization path exists
	// for this artifact -- no closure path, no supporting adapter, an owed
	// prerequisite, or a service record this build cannot use.
	automaticRefusalAuthorizationUnavailable automaticRefusal = "authorization_unavailable"
	// automaticRefusalGrantNotMinted: eligible, and the mint still refused for
	// a reason none of the above names.
	automaticRefusalGrantNotMinted automaticRefusal = "grant_not_minted"
	// automaticRefusalCaptureOnlySuppressed: an acquisition control took the
	// licence the decision had earned.
	automaticRefusalCaptureOnlySuppressed automaticRefusal = "capture_only_suppressed"
	// automaticRefusalDeliveryFailed: a grant was spent and the bytes did not
	// land as authorized.
	automaticRefusalDeliveryFailed automaticRefusal = "delivery_failed"
	// automaticRefusalUnattributed: the facts on hand fit no reason above, or
	// contradict each other. Fails closed: an analysis treats it as a
	// contradiction, never as a guess.
	automaticRefusalUnattributed automaticRefusal = "unattributed"
)

// automaticRefusalVocabulary is the closed set an analysis may read. A value
// outside it is a bug in this file, and the emitter refuses it.
var automaticRefusalVocabulary = map[automaticRefusal]bool{
	automaticRefusalNone:            true,
	automaticRefusalRouteNotEntered: true, automaticRefusalNoCandidate: true,
	automaticRefusalV3Unavailable: true, automaticRefusalV3TimedOut: true,
	automaticRefusalCancelled: true, automaticRefusalRouteGateRevoked: true,
	automaticRefusalHardVeto: true, automaticRefusalNoSelection: true,
	automaticRefusalSelectedHashMismatch: true, automaticRefusalIdentityIncomplete: true,
	automaticRefusalNoScope: true, automaticRefusalTargetNotGrounded: true,
	automaticRefusalTargetMismatch: true, automaticRefusalScopeExpansion: true,
	automaticRefusalStaleBaseline: true, automaticRefusalAuthorizationUnavailable: true,
	automaticRefusalGrantNotMinted: true, automaticRefusalCaptureOnlySuppressed: true,
	automaticRefusalDeliveryFailed: true, automaticRefusalUnattributed: true,
}

// automaticOutcome is what became of the automatic question on this entry.
type automaticOutcome string

const (
	automaticOutcomeLanded    automaticOutcome = "landed"
	automaticOutcomeNotLanded automaticOutcome = "not_landed"
)

// automaticFacts is what a route hands the attribution as it goes: the live
// owners' answers, copied verbatim in the order they were produced. The
// struct is inert -- no owner reads it -- and nothing is allocated for the
// record until the sink is known to be on.
type automaticFacts struct {
	identity      candidateEvidenceIdentity
	candidateHash string

	authorization *deliveryAuthorization
	vetoes        []string

	deliveryAttempted bool
	delivery          deliveryOutcome
}

// noteAuthorization records the authorization owner's answer over the named
// bytes, and the vetoes it was handed, verbatim.
func (l *routeLifecycle) noteAuthorization(d deliveryAuthorization,
	id candidateEvidenceIdentity, hash string, vetoes []string) {
	if l == nil {
		return
	}
	copied := d
	l.auto.identity, l.auto.candidateHash = id, hash
	l.auto.authorization, l.auto.vetoes = &copied, append([]string(nil), vetoes...)
}

// noteDelivery records what the delivery owner reported.
func (l *routeLifecycle) noteDelivery(out deliveryOutcome) {
	if l == nil {
		return
	}
	l.auto.deliveryAttempted, l.auto.delivery = true, out
}

// deriveAutomaticRefusal maps the collected facts onto the closed vocabulary.
// It reads them in the order the owners produced them and answers with the
// first that decided the outcome. Pure, and covered by a test per value.
func deriveAutomaticRefusal(l *routeLifecycle) (automaticOutcome, automaticRefusal) {
	if l == nil {
		return automaticOutcomeNotLanded, automaticRefusalUnattributed
	}
	a := &l.auto
	if a.deliveryAttempted && a.delivery.Delivered {
		return automaticOutcomeLanded, automaticRefusalNone
	}
	if a.deliveryAttempted {
		return automaticOutcomeNotLanded, deliveryRefusalReason(a.delivery.Reason)
	}
	if a.authorization != nil {
		return automaticOutcomeNotLanded, authorizationRefusalReason(a)
	}
	disposition, reason := l.ending()
	return automaticOutcomeNotLanded, dispositionRefusalReason(disposition, reason)
}

// dispositionRefusalReason reads an entry that ended before the policy was
// consulted: the route itself said why.
func dispositionRefusalReason(d routingDisposition, reason AuthorizationReason) automaticRefusal {
	switch d {
	case routingProducerUnavailable:
		return automaticRefusalV3Unavailable
	case routingProducerTimedOut:
		return automaticRefusalV3TimedOut
	case routingCancelled:
		return automaticRefusalCancelled
	case routingNoCandidate, routingBaselineRetained:
		return automaticRefusalNoCandidate
	case routingRevokedByGate, routingNotClosureEligible:
		return automaticRefusalRouteGateRevoked
	case routingSkippedInfeasible:
		return automaticRefusalAuthorizationUnavailable
	}
	return automaticRefusalUnattributed
}

// authorizationRefusalReason reads an entry the policy was consulted about and
// that did not go on to a delivery.
func authorizationRefusalReason(a *automaticFacts) automaticRefusal {
	d := a.authorization
	if d.CaptureOnly {
		return automaticRefusalCaptureOnlySuppressed
	}
	switch d.AutomaticRefusal {
	case automaticHardVeto:
		return vetoRefusalReason(a.vetoes)
	case automaticNoSelection:
		return automaticRefusalNoSelection
	case automaticNotTheWinner:
		return automaticRefusalSelectedHashMismatch
	case automaticIdentityIncomplete:
		return automaticRefusalIdentityIncomplete
	case automaticNoScope:
		return automaticRefusalNoScope
	case automaticTargetNotGrounded:
		return automaticRefusalTargetNotGrounded
	case automaticEligible:
		if d.Grant == nil {
			return mintRefusalReason(d.Refusal)
		}
		// Eligible, minted, and the route ended before the delivery owner was
		// asked, or the policy still did not deliver. Neither is a state this
		// vocabulary knows a reason for.
		return automaticRefusalUnattributed
	}
	return automaticRefusalUnattributed
}

// vetoRefusalReason narrows a hard veto to the veto that fired when the
// vocabulary has a more specific word for it.
func vetoRefusalReason(vetoes []string) automaticRefusal {
	if len(vetoes) == 0 {
		return automaticRefusalUnattributed
	}
	onlyCancelled := true
	for _, v := range vetoes {
		switch v {
		case VetoUnauthorizedPathExpansion, VetoOutsideMutationScope:
			return automaticRefusalScopeExpansion
		case VetoStaleIdentity:
			return automaticRefusalStaleBaseline
		case VetoCancelledOrTimedOut:
		default:
			onlyCancelled = false
		}
	}
	if onlyCancelled {
		return automaticRefusalCancelled
	}
	return automaticRefusalHardVeto
}

// mintRefusalReason reads the grant mint's own reason for an eligible
// candidate it would not licence. The mint's strings are its closed set; this
// keys on the facts they state.
func mintRefusalReason(why string) automaticRefusal {
	switch {
	case why == "":
		return automaticRefusalGrantNotMinted
	case why == structuredTargetMismatch, why == structuredTargetNotThisRequest,
		containsAny(why, "target is not declared"):
		return automaticRefusalTargetMismatch
	case why == structuredTargetNoScope, containsAny(why, "mutation scope"):
		return automaticRefusalScopeExpansion
	case containsAny(why, "workspace", "moved", "stale", "recreated"):
		return automaticRefusalStaleBaseline
	case containsAny(why, "prerequisite", "demonstrated", "trusted evidence",
		"adapter", "service record", "output knowledge"):
		return automaticRefusalAuthorizationUnavailable
	}
	return automaticRefusalGrantNotMinted
}

// deliveryRefusalReason reads the delivery owner's outcome for a grant that
// was spent, or refused at consumption, and did not land.
func deliveryRefusalReason(reason string) automaticRefusal {
	switch {
	case reason == "target_mismatch", containsAny(reason, "target is not what"):
		return automaticRefusalTargetMismatch
	case reason == "candidate_hash_mismatch":
		return automaticRefusalSelectedHashMismatch
	case containsAny(reason, "changed since", "moved since", "deliberately removed",
		"stale", "generation"):
		return automaticRefusalStaleBaseline
	case containsAny(reason, "cancel"):
		return automaticRefusalCancelled
	}
	return automaticRefusalDeliveryFailed
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if n != "" && strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// recordAttribution writes the one attribution record for this entry. Called
// once, deferred at route exit, after the lifecycle has recorded its ending;
// nothing is built when the sink is off, and the record carries identities
// and closed values only.
func (l *routeLifecycle) recordAttribution(ctx *AgentContext) {
	if l == nil {
		return
	}
	l.attributed.Do(func() {
		sink := activeShadowSink.Load()
		if !sink.enabled() {
			return
		}
		outcome, refusal := deriveAutomaticRefusal(l)
		if !automaticRefusalVocabulary[refusal] {
			refusal = automaticRefusalUnattributed
		}
		disposition, _ := l.ending()
		a := &l.auto
		rec := map[string]interface{}{
			"schema_version":           shadowSchemaVersionAutomaticAttribution,
			"record_kind":              "automatic_delivery_attribution",
			"request_id":               requestIDOf(ctx),
			"route_entry_id":           l.entry.ID,
			"policy_mode":              string(CandidatePolicyAutomaticV3),
			"policy_source":            string(CandidatePolicySourceFixed),
			"policy_consulted":         true,
			"disposition":              string(disposition),
			"outcome":                  string(outcome),
			"refusal":                  string(refusal),
			"influences_live_decision": false,
			"build_version":            APIVersion,
		}
		if a.identity.InvocationID != "" {
			rec["invocation_id"] = a.identity.InvocationID
		}
		if a.identity.CandidateInstanceID != "" {
			rec["candidate_instance_id"] = a.identity.CandidateInstanceID
		}
		if a.candidateHash != "" {
			rec["candidate_hash"] = a.candidateHash
		}
		sink.submit(rec)
	})
}

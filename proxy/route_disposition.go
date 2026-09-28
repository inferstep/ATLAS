package main

import "sync"

// How one entry of the candidate-generation route ended, and — separately —
// what became of any licence it minted.
//
// Before this, no branch of that route said how it ended. A route entry with a
// feasibility decision and a stage marker and nothing after it was equally
// consistent with a service outage, a cancelled turn, a pipeline that produced
// nothing, a winner the evidence would not authorize, a gate revoking a
// candidate, an authorization refusal, and an ordinary fallback that kept the
// caller's own bytes. Three entries in the v6 acquisition ended exactly that
// way and could not be told apart, which is what left that run permanently
// defective.
//
// Two vocabularies, not one, because routing and delivery are answered at
// different moments. Selection happens inside the write; delivery happens
// afterwards under a grant, and a candidate that was authorized has not
// thereby been delivered or settled.

type routingDisposition string

const (
	routingSkippedInfeasible    routingDisposition = "skipped_infeasible"
	routingProducerUnavailable  routingDisposition = "producer_unavailable"
	routingProducerTimedOut     routingDisposition = "producer_timed_out"
	routingCancelled            routingDisposition = "cancelled"
	routingNoCandidate          routingDisposition = "no_candidate_produced"
	routingNotClosureEligible   routingDisposition = "candidate_not_closure_eligible"
	routingRevokedByGate        routingDisposition = "candidate_revoked_by_gate"
	routingBaselineRetained     routingDisposition = "baseline_retained"
	routingAuthorizationRefused routingDisposition = "authorization_refused"
	routingCandidateAuthorized  routingDisposition = "candidate_authorized"
	// routingUnclassified is the fail-closed member. A branch nobody taught
	// this vocabulary about ends here rather than ending silently, and it may
	// never be read as success.
	routingUnclassified routingDisposition = "internal_unclassified"
)

func knownRoutingDisposition(d routingDisposition) bool {
	switch d {
	case routingSkippedInfeasible, routingProducerUnavailable, routingProducerTimedOut,
		routingCancelled, routingNoCandidate, routingNotClosureEligible,
		routingRevokedByGate, routingBaselineRetained, routingAuthorizationRefused,
		routingCandidateAuthorized, routingUnclassified:
		return true
	}
	return false
}

// routingDispositionSucceeded reports whether a disposition means a candidate
// was authorized to land. Nothing else is success, and the fail-closed member
// never is.
func routingDispositionSucceeded(d routingDisposition) bool {
	return d == routingCandidateAuthorized
}

type deliveryDisposition string

const (
	deliveryNotAttemptedBaseline   deliveryDisposition = "not_attempted_baseline_retained"
	deliveryNotAttemptedSuperseded deliveryDisposition = "not_attempted_superseded"
	deliveryConsumedAndLanded      deliveryDisposition = "consumed_and_landed"
	deliveryConsumedDidNotSettle   deliveryDisposition = "consumed_did_not_settle"
	deliveryRefusedAtConsumption   deliveryDisposition = "refused_at_consumption"
	deliveryRetiredAtTerminal      deliveryDisposition = "retired_at_terminal"
	deliveryCancelled              deliveryDisposition = "cancelled"
	deliveryUnclassified           deliveryDisposition = "internal_unclassified"
)

func knownDeliveryDisposition(d deliveryDisposition) bool {
	switch d {
	case deliveryNotAttemptedBaseline, deliveryNotAttemptedSuperseded,
		deliveryConsumedAndLanded, deliveryConsumedDidNotSettle,
		deliveryRefusedAtConsumption, deliveryRetiredAtTerminal,
		deliveryCancelled, deliveryUnclassified:
		return true
	}
	return false
}

// deliveryDispositionLanded reports whether a disposition means the exact
// authorized bytes reached the artifact. Only one member does.
func deliveryDispositionLanded(d deliveryDisposition) bool {
	return d == deliveryConsumedAndLanded
}

// routeLifecycle finalises one route entry exactly once.
//
// The owner is created where the entry is minted and finalised by a deferred
// default, so a branch that forgets to speak still produces a record — the
// fail-closed one — rather than an entry that never ended.
// The target is deliberately absent: a canonical path is content-adjacent, the
// capture is path-free by design, and the route entry already says which
// invocation this is. A reader joins on identity, never on a path.
type routeLifecycle struct {
	entry routeEntry
	once  sync.Once
	// ended and reason are what finish recorded, kept so the attribution
	// written at route exit copies the ending rather than re-deciding it.
	ended  routingDisposition
	reason AuthorizationReason
	// auto is what the route hands the automatic-delivery attribution as it
	// goes; attributed makes that record, like the ending, a once-only event.
	auto       automaticFacts
	attributed sync.Once
}

// ending is the recorded disposition, or the fail-closed member when the
// entry has not ended.
func (l *routeLifecycle) ending() (routingDisposition, AuthorizationReason) {
	if l == nil || l.ended == "" {
		return routingUnclassified, ""
	}
	return l.ended, l.reason
}

func newRouteLifecycle(entry routeEntry) *routeLifecycle {
	return &routeLifecycle{entry: entry}
}

// finish records how this entry ended. The second call and every call after it
// does nothing: two endings for one entry would be a contradiction, and the
// first one is the one that ran.
func (l *routeLifecycle) finish(ctx *AgentContext, d routingDisposition,
	candidateHash string, reason AuthorizationReason) {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if !knownRoutingDisposition(d) {
			d = routingUnclassified
		}
		l.ended, l.reason = d, reason
		sink := activeShadowSink.Load()
		if !sink.enabled() {
			return
		}
		rec := map[string]interface{}{
			"schema_version": shadowSchemaVersionRouteDisposition,
			"record_kind":    "shadow_route_disposition",
			"request_id":     requestIDOf(ctx),
			"route_entry_id": l.entry.ID,
			"disposition":    string(d),
			// A fact about telemetry, not about policy: this record is written
			// after the decision it describes and changes nothing.
			"influences_live_decision": false,
			"build_version":            APIVersion,
		}
		if candidateHash != "" {
			rec["candidate_hash"] = candidateHash
		}
		if reason != "" {
			rec["authorization_reason"] = string(reason)
		}
		sink.submit(rec)
	})
}

// finalizeDefault is the deferred backstop. It ends an entry nobody classified
// as unclassified, which fails closed everywhere that reads it.
func (l *routeLifecycle) finalizeDefault(ctx *AgentContext) {
	l.finish(ctx, routingUnclassified, "", "")
}

// markBaselineRetainedGrant marks a grant the route minted and then did not
// use, because the caller's own bytes were what stayed. It is not a delivery
// and must never be reported as one.
func markBaselineRetainedGrant(ctx *AgentContext, a deliveryAuthorization) {
	if ctx == nil || a.Grant == nil {
		return
	}
	ctx.grantMu.Lock()
	if g := ctx.grants[a.Grant.ID]; g != nil {
		g.baselineRetained = true
	}
	ctx.grantMu.Unlock()
}

// markGrantDelivery ends the delivery lifecycle of one grant by id, taking the
// lock the recorder expects the caller to hold nothing of.
func markGrantDelivery(ctx *AgentContext, id string, d deliveryDisposition) {
	if ctx == nil {
		return
	}
	ctx.grantMu.Lock()
	g := ctx.grants[id]
	ctx.grantMu.Unlock()
	recordDeliveryDisposition(ctx, g, d)
}

// recordDeliveryDisposition ends one grant's delivery lifecycle.
//
// Separate from the routing record because it answers a later question: the
// route says what was selected and authorized, this says what became of the
// licence. Emitted once per grant, from the grant's own terminal transition.
func recordDeliveryDisposition(ctx *AgentContext, g *authorizationGrant,
	d deliveryDisposition) {
	if g == nil || g.deliveryRecorded {
		return
	}
	g.deliveryRecorded = true
	if !knownDeliveryDisposition(d) {
		d = deliveryUnclassified
	}
	sink := activeShadowSink.Load()
	if !sink.enabled() {
		return
	}
	sink.submit(map[string]interface{}{
		"schema_version":           shadowSchemaVersionDeliveryDisposition,
		"record_kind":              "shadow_delivery_disposition",
		"request_id":               g.RequestID,
		"route_entry_id":           g.RouteEntryID,
		"invocation_id":            g.InvocationID,
		"candidate_instance_id":    g.CandidateInstanceID,
		"candidate_hash":           g.CandidateHash,
		"grant_id":                 g.ID,
		"disposition":              string(d),
		"influences_live_decision": false,
		"build_version":            APIVersion,
	})
}

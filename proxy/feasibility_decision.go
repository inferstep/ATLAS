package main

import (
	"sort"
	"strings"
)

// Whether this invocation could close its task at all, asked before generation
// and consulted by nothing.
//
// The sealed Stage-A acquisition is the question this answers. 100 of 103
// candidate evaluations ran under a route that could not reach closure by any
// path, and the run found that out 103 times, one candidate at a time, after
// generating each. Nothing had ever asked the cheap question first: given what
// this task obliges and what can actually observe it, is there a closure path
// at all?
//
// It is observe-only. Generation proceeds exactly as it did at e8fefe8; the
// answer is private telemetry and skips nothing.
//
// What it may read is fixed and small: the obligations fixed for this
// invocation, the targets the request authorized, what adapters can measure,
// which trusted sources can actually run, and the baseline floor. It may not
// read prompt semantics, model output, a candidate result, a hidden evaluator,
// or any previous benchmark outcome -- a guard enumerates that.

// FeasibilityReason is the closed vocabulary.
type FeasibilityReason string

const (
	// FeasibilityClosurePathAvailable: some trusted source that can run on this
	// build could reach every prerequisite's floor.
	FeasibilityClosurePathAvailable FeasibilityReason = "closure_path_available"
	// FeasibilityNoTrustedSource: a prerequisite needs a source no producer can
	// supply here.
	FeasibilityNoTrustedSource FeasibilityReason = "no_trusted_source"
	// FeasibilityAdapterCannotMeasure: no adapter owns an evaluator for a kind
	// the task requires.
	FeasibilityAdapterCannotMeasure FeasibilityReason = "adapter_cannot_measure"
	// FeasibilityUnsupportedObligation: the task owes something nothing can name.
	FeasibilityUnsupportedObligation FeasibilityReason = "unsupported_obligation"
	// FeasibilityBaselineFloorUnreachable: preserving what is already there
	// needs evidence stronger than anything available.
	FeasibilityBaselineFloorUnreachable FeasibilityReason = "baseline_floor_unreachable"
	// FeasibilityUnspecifiedContract: nothing structured was stated, so there is
	// no closure question to answer. Legacy behaviour is unchanged.
	FeasibilityUnspecifiedContract FeasibilityReason = "unspecified_contract"
	// FeasibilityUnknown is what a contradictory state becomes. Never feasible.
	FeasibilityUnknown FeasibilityReason = "unknown"
)

var feasibilityReasons = map[FeasibilityReason]bool{
	FeasibilityClosurePathAvailable: true, FeasibilityNoTrustedSource: true,
	FeasibilityAdapterCannotMeasure: true, FeasibilityUnsupportedObligation: true,
	FeasibilityBaselineFloorUnreachable: true, FeasibilityUnspecifiedContract: true,
	FeasibilityUnknown: true,
}

// FeasibilityDecision is one invocation's answer.
type FeasibilityDecision struct {
	// Feasible is true only for FeasibilityClosurePathAvailable.
	Feasible bool
	Reason   FeasibilityReason
	// Unreachable names the obligation ids nothing on this build could close,
	// sorted. Empty when feasible.
	Unreachable []string
	// Floor is the strongest prerequisite floor this task states, or "" when
	// it states none.
	Floor string
	// InfluencesLiveDecision is false on this build.
	InfluencesLiveDecision bool
}

// producibleStrengths is what each trusted source could observe on this build,
// keyed by the obligation kind it speaks for.
//
// Read from the wiring inventory, not from a wish list: a source declared
// unavailable contributes nothing, so a task that needs it has no path. That
// is the whole point -- the answer changes when a producer is wired, and a
// later invocation recomputes from what is available then. The current
// invocation cannot gain authority retroactively, because the decision is made
// once, before generation, from what exists at that moment.
func producibleStrengths() map[string]string {
	return producibleStrengthsWith(true)
}

// producibleStrengthsWith is the same inventory, told whether the sandbox that
// runs both producers is actually reachable.
//
// A producer that is wired and unreachable can demonstrate nothing, and a
// feasibility answer that ignored that would say a task could close while the
// authorization it predicts is refusing every candidate for exactly that
// reason. The two must agree about availability or they are answering about
// different builds.
//
// Still observe-only: nothing reads the verdict.
func producibleStrengthsWith(sandboxReachable bool) map[string]string {
	out := map[string]string{}
	if !sandboxReachable {
		// Both producers run through the sandbox. With it down, neither can
		// speak, and saying so is the whole point.
		return out
	}
	if evidenceProducerStatus[ProvenanceProxyOwnedValidation] == evidenceProducerWired {
		// The proxy's own gate: structural validity, at syntax.
		out[ObligationSyntacticValidity] = "syntax"
	}
	if evidenceProducerStatus[ProvenanceClientDeclaredVerification] == evidenceProducerWired {
		out[ObligationDeclaredCommand] = "behavioral"
	}
	return out
}

// feasibilityInput is the closed set of things the decision may look at.
type feasibilityInput struct {
	Obligations []taskObligation
	// AuthorizedTargets is identity only, and is consulted to answer "is there
	// anything to deliver to at all".
	AuthorizedTargets []string
	// Producible is what trusted sources can actually observe here.
	Producible map[string]string
}

// decideInvocationFeasibility is THE observe-only feasibility owner.
//
// One pass over the prerequisites. The first kind with no path decides the
// answer, and every unreachable obligation is named so a reader learns what
// would have to change rather than that something would.
func decideInvocationFeasibility(in feasibilityInput) FeasibilityDecision {
	d := FeasibilityDecision{Reason: FeasibilityUnknown, InfluencesLiveDecision: false}

	if len(in.Obligations) == 0 {
		d.Reason = FeasibilityUnspecifiedContract
		return d
	}
	prerequisites := authorizationPrerequisites(in.Obligations)
	d.Floor = authorizationFloor(in.Obligations)

	if len(prerequisites) == 0 {
		// Targets but nothing to demonstrate about them. Not a closure path:
		// naming a path is not evidence about bytes.
		d.Reason = FeasibilityNoTrustedSource
		return d
	}

	reason := FeasibilityClosurePathAvailable
	worst := 0
	rank := map[FeasibilityReason]int{
		FeasibilityClosurePathAvailable:     0,
		FeasibilityBaselineFloorUnreachable: 1,
		FeasibilityAdapterCannotMeasure:     2,
		FeasibilityNoTrustedSource:          3,
		FeasibilityUnsupportedObligation:    4,
	}
	note := func(r FeasibilityReason, id string) {
		d.Unreachable = append(d.Unreachable, id)
		if rank[r] > worst {
			worst, reason = rank[r], r
		}
	}

	for _, o := range prerequisites {
		if !o.Required {
			continue
		}
		if obligationUnsatisfiableKinds[o.Kind] {
			note(FeasibilityUnsupportedObligation, o.ID)
			continue
		}
		if _, known := obligationRole(o.Kind); !known {
			note(FeasibilityUnsupportedObligation, o.ID)
			continue
		}
		if o.Kind == ObligationBaselinePreserved {
			// Preservation has no producer of its own and never will: it is
			// derived from whatever else spoke for the candidate. So it is
			// reachable exactly when something this build CAN produce reaches
			// the strength the existing artifact already has.
			//
			// This asks whether closure is possible, not whether it will
			// happen. Whether the behavioural evidence turns out to name the
			// command that established the baseline is a fact about the run,
			// and the authorization decision is where that is settled.
			if !baselineFloorReachable(o.RequiredStrength, in.Producible) {
				note(FeasibilityBaselineFloorUnreachable, o.ID)
			}
			continue
		}
		available, ok := in.Producible[o.Kind]
		if !ok {
			// A kind no adapter owns an evaluator for is a different failure
			// from one whose evaluator exists but has no trusted source.
			if !anyAdapterMeasures(o.Kind) {
				note(FeasibilityAdapterCannotMeasure, o.ID)
				continue
			}
			note(FeasibilityNoTrustedSource, o.ID)
			continue
		}
		if strengthRank(available) < strengthRank(o.RequiredStrength) {
			note(FeasibilityNoTrustedSource, o.ID)
		}
	}

	sort.Strings(d.Unreachable)
	d.Reason = reason
	d.Feasible = reason == FeasibilityClosurePathAvailable && len(d.Unreachable) == 0
	if d.Feasible && len(in.AuthorizedTargets) == 0 {
		// Nothing to deliver to. A prerequisite with no target is a closure
		// path to nowhere.
		d.Feasible = false
		d.Reason = FeasibilityNoTrustedSource
	}
	return d
}

// anyAdapterMeasures reports whether ANY verifier on either side owns an
// evaluator for this obligation kind.
//
// The proxy does not hold the V3 registry, so this is the proxy's own honest
// answer about what it can measure, stated once. It is deliberately not a
// guess about the service: a kind the proxy cannot measure and no trusted
// source can supply has no path here whatever the service could do, because
// the service's records carry no trusted provenance.
func anyAdapterMeasures(kind string) bool {
	switch kind {
	case ObligationSyntacticValidity:
		// The structural gate exists whether or not a producer is wired to it.
		return true
	case ObligationDeclaredCommand, ObligationDeclaredExample:
		// Executing a client's command or case is a mechanism, not a checker,
		// and the proxy owns no such mechanism at candidate time.
		return false
	case ObligationBaselinePreserved:
		// Preservation is a claim about survival, and nothing here evaluates
		// it directly.
		return false
	}
	return false
}

// recordFeasibilityDecision writes the answer to private telemetry.
func recordFeasibilityDecision(ctx *AgentContext, entry routeEntry,
	d FeasibilityDecision, targets int) {
	sink := activeShadowSink.Load()
	if !sink.enabled() {
		return
	}
	reason := d.Reason
	if !feasibilityReasons[reason] {
		reason = FeasibilityUnknown
	}
	sink.submit(map[string]interface{}{
		"schema_version": shadowSchemaVersionFeasibility,
		"record_kind":    "shadow_invocation_feasibility",
		"request_id":     requestIDOf(ctx),
		// Which entry of the candidate-generation route this answers for. A
		// request may enter it several times; without this the decisions pile
		// up under one request id and none of them is attributable.
		"route_entry_id":          entry.ID,
		"feasible":                d.Feasible,
		"reason":                  string(reason),
		"unreachable_obligations": d.Unreachable,
		"authorization_floor":     d.Floor,
		"authorized_target_count": targets,
		// The answer is recorded and never enforced: generation proceeds
		// whatever it says. The enforce mode, a canary experiment, was
		// removed with the other switches; the fields keep their meaning for
		// existing readers.
		"generation_proceeded":     true,
		"mode":                     "observe",
		"influences_live_decision": false,
		"build_version":            APIVersion,
	})
}

// observeInvocationFeasibility is the one production call path: it answers the
// question before generation and records the answer.
//
// Under observe the answer is recorded and generation proceeds regardless.
// Under enforce the caller reads it, and an invocation with no closure path
// generates nothing.
// sandboxConfigured reports whether a sandbox is even addressable. It probes
// nothing: a feasibility answer must not make a network call of its own, and
// the runtime outage is what the authorization decision reports truthfully
// after the fact.
func sandboxConfigured(ctx *AgentContext) bool {
	return ctx != nil && strings.TrimSpace(ctx.SandboxURL) != ""
}

func observeInvocationFeasibility(ctx *AgentContext, entry routeEntry) FeasibilityDecision {
	obs := requestObligations(ctx)
	targets := authorizedTargets(obs)
	d := decideInvocationFeasibility(feasibilityInput{
		Obligations:       obs,
		AuthorizedTargets: targets,
		Producible:        producibleStrengthsWith(sandboxConfigured(ctx)),
	})
	recordFeasibilityDecision(ctx, entry, d, len(targets))
	return d
}

// feasibilityForbiddenInputs is what this decision may never read, named here
// so a guard can enumerate it.
var feasibilityForbiddenInputs = []string{
	"HumanTask", "latestUserMessage", "LastTurnReasoning",
	"SessionWrites", "V3GenerateResponse",
	"WinningScore", "PhaseSolved", "CandidatesTested", "Passed",
}

func init() {
	// The forbidden list is only meaningful if it is non-empty and every entry
	// is a real name. A silently emptied list would make the guard vacuous.
	for _, name := range feasibilityForbiddenInputs {
		if strings.TrimSpace(name) == "" {
			panic("feasibility forbidden-input list contains an empty name")
		}
	}
}

// baselineFloorReachable mirrors the derivation the authorization owner uses.
//
// The two must agree: a build that calls a baseline unreachable and then
// preserves it anyway, or the reverse, is a build whose pre-generation answer
// and post-generation answer are about different systems.
func baselineFloorReachable(required string, producible map[string]string) bool {
	for kind, strength := range producible {
		if kind == ObligationBaselinePreserved {
			continue
		}
		if strengthRank(strength) >= strengthRank(required) {
			return true
		}
	}
	return false
}

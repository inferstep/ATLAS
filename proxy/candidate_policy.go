package main

// The one rule a candidate answers before it replaces what the model wrote.
//
// ATLAS is an interactive coding agent, not a formal verifier. For an ordinary
// coding task there is no oracle to consult and no proof to be had. A V3
// candidate replaces the model's bytes when no hard veto fired and one of two
// bases holds:
//
//	declared verification  a verification the client declared passed, at the
//	                       declared strength, against these exact bytes
//	                       (recorded as candidate_authorized_strict);
//	V3 selection           the V3 selection path named these exact bytes and
//	                       every hard safety requirement holds
//	                       (candidate_automatic_v3).
//
// Otherwise the model's own bytes stand. A declared verification that failed
// is a hard veto either way.
//
// There were three client-selectable modes (strict, advisory, automatic_v3)
// and an operator default. Under the default, strict, a request that declared
// no outputs never generated a candidate, so the pipeline ran only for
// clients that opted in, and what was measured was not what shipped. There is
// one rule now, and nothing a client or an operator sends changes it.

type candidatePolicyMode string

// CandidatePolicyAutomaticV3 names the one rule in the records that carry a
// mode field, so their readers keep their schema.
const CandidatePolicyAutomaticV3 candidatePolicyMode = "automatic_v3"

type candidatePolicySource string

// CandidatePolicySourceFixed says the rule is not selectable.
const CandidatePolicySourceFixed candidatePolicySource = "fixed"

// --- what the policy answers -------------------------------------------------

// candidatePolicyDecision is the closed vocabulary of honest answers.
//
// Every one of them is a statement about what happened, not about how likely
// the candidate is to be correct. Nothing here is a probability, and nothing
// here may be presented as one.
type candidatePolicyDecision string

const (
	// PolicyBaselineRetained: the model's own proposal is what lands. Either
	// nothing materially different was proposed, or the candidate did not earn
	// its way past the policy.
	PolicyBaselineRetained candidatePolicyDecision = "baseline_retained"
	// PolicyCandidateAuthorizedStrict: trusted client-declared verification
	// passed at the declared strength against these exact bytes.
	PolicyCandidateAuthorizedStrict candidatePolicyDecision = "candidate_authorized_strict"
	// PolicyCandidateAutomaticV3: the V3 selection path chose this candidate
	// and every hard safety requirement holds. The competition that produced
	// it is internal; what the user reviews is the diff.
	PolicyCandidateAutomaticV3 candidatePolicyDecision = "candidate_automatic_v3"
	// PolicyCandidateRejectedHardVeto: something disqualifying was observed.
	// Vetoes are facts, never scores, and one is enough.
	PolicyCandidateRejectedHardVeto candidatePolicyDecision = "candidate_rejected_hard_veto"
)

var candidatePolicyDecisions = map[candidatePolicyDecision]bool{
	PolicyBaselineRetained:          true,
	PolicyCandidateAuthorizedStrict: true,
	PolicyCandidateAutomaticV3:      true,
	PolicyCandidateRejectedHardVeto: true,
}

// candidatePolicyOutcome is one policy answer, with everything needed to say
// why it holds.
type candidatePolicyOutcome struct {
	Mode     candidatePolicyMode
	Source   candidatePolicySource
	Decision candidatePolicyDecision
	// Vetoes are the disqualifying facts observed, in canonical order. A
	// non-empty list forces the rejection decision whatever else was seen.
	Vetoes []string
	// Signals are the advisory observations that were available. They are
	// recorded so a later calibration can be computed from what actually
	// happened; none of them is consulted as a threshold here.
	Signals map[string]interface{}
	// Delivers reports whether this outcome is one the delivery path may act
	// on: the declared-verification and V3-selection decisions, and nothing
	// else.
	Delivers bool
}

// mayDeliverUnderPolicy is the single predicate the delivery path asks. The two
// bases that deliver each set Delivers in exactly one place.
func (o candidatePolicyOutcome) mayDeliverUnderPolicy() bool { return o.Delivers }

// recordCandidatePolicyDecision writes one policy answer to the private shadow
// sink.
//
// Identities, a closed decision, a closed veto list and the advisory signals as
// they were observed. No candidate byte, no command string, no path content and
// no prose. influences_live_decision says whether this answer delivered. Every
// decision is recorded, so a calibration can be computed from what actually
// happened rather than from what a threshold would have predicted.
func recordCandidatePolicyDecision(ctx *AgentContext, entry routeEntry,
	candidateHash string, out candidatePolicyOutcome) {
	sink := activeShadowSink.Load()
	if !sink.enabled() {
		return
	}
	decision := out.Decision
	if !candidatePolicyDecisions[decision] {
		// An unclassified decision is written as the fail-closed member rather
		// than as arbitrary prose.
		decision = PolicyBaselineRetained
	}
	vetoes := make([]string, 0, len(out.Vetoes))
	for _, v := range out.Vetoes {
		if advisoryVetoNames[v] {
			vetoes = append(vetoes, v)
		}
	}
	sink.submit(map[string]interface{}{
		"schema_version":           shadowSchemaVersionCandidatePolicy,
		"record_kind":              "candidate_policy_decision",
		"request_id":               requestIDOf(ctx),
		"route_entry_id":           entry.ID,
		"candidate_hash":           candidateHash,
		"policy_mode":              string(out.Mode),
		"policy_source":            string(out.Source),
		"decision":                 string(decision),
		"vetoes":                   vetoes,
		"signals":                  out.Signals,
		"delivers":                 out.Delivers,
		"influences_live_decision": out.Delivers,
		"build_version":            APIVersion,
	})
}

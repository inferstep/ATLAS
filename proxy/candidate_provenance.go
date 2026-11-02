package main

// Where the bytes on disk came from, said plainly enough to show a user.
//
// The terminal is the last place this is still knowable. Once a file is
// written, the model's own work, a candidate a declared check passed and a
// candidate someone approved by hand are all just bytes -- and they are not
// the same thing to the person reviewing the diff. A vocabulary the UI can
// render is what keeps that difference visible.
//
// None of these values is a confidence, a score or a guarantee. Each names an
// origin and the rule that let it land, and nothing more.
const (
	// DeliveryFromModelProposal: the model wrote it. No candidate replaced it,
	// either because none was proposed or because the policy kept the
	// baseline. This is the default and the overwhelming majority.
	DeliveryFromModelProposal = "model_proposal"
	// DeliveryFromStrictCandidate: a V3 candidate replaced it, and a
	// client-declared verification passed at the declared strength against
	// exactly these bytes.
	DeliveryFromStrictCandidate = "strict_trusted_candidate"
	// DeliveryFromAutomaticV3: a V3 candidate replaced it because the pipeline
	// selected it and every hard safety requirement held. Nothing about its
	// correctness was proven, and the UI must not present it as proven -- what
	// it says is where the bytes came from, which is exactly what a reviewer
	// of the diff needs to know.
	DeliveryFromAutomaticV3 = "automatic_v3_candidate"
)

var deliveryProvenanceValues = map[string]bool{
	DeliveryFromModelProposal:   true,
	DeliveryFromStrictCandidate: true,
	DeliveryFromAutomaticV3:     true,
}

// deliveryProvenanceFor maps a policy answer to what the user is looking at.
//
// Only the decisions that actually deliver map to a candidate origin. Everything
// else -- a veto or a retained baseline -- means the bytes on disk are the
// model's own, and
// saying anything else about them would be a false claim in the one place a
// person is relying on it.
func deliveryProvenanceFor(out candidatePolicyOutcome) string {
	if !out.Delivers {
		return DeliveryFromModelProposal
	}
	switch out.Decision {
	case PolicyCandidateAuthorizedStrict:
		return DeliveryFromStrictCandidate
	case PolicyCandidateAutomaticV3:
		return DeliveryFromAutomaticV3
	}
	return DeliveryFromModelProposal
}

// emitDeliveryProvenance tells the terminal what it is about to show, and under
// which rule.
//
// Identities and closed values only: a path the user already knows, the
// provenance, the policy decision and the mode that produced it. No candidate
// bytes and no evidence detail -- the diff the user reviews is the artifact
// itself, and this event is the label on it.
func emitDeliveryProvenance(ctx *AgentContext, path, provenance string,
	out candidatePolicyOutcome) {
	if !deliveryProvenanceValues[provenance] {
		return
	}
	Emit(NewEnvelope(EvtMetric, "candidate_policy", map[string]interface{}{
		"path":          logPath(path),
		"provenance":    provenance,
		"decision":      string(out.Decision),
		"policy_mode":   string(out.Mode),
		"policy_source": string(out.Source),
		// The vetoes are what a user would want to see when a candidate did
		// NOT land, and they are facts rather than scores.
		"vetoes": out.Vetoes,
	}))
}

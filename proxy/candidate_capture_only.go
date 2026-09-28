package main

import (
	"os"
	"strings"
)

// An acquisition control, not a policy mode.
//
// An outcome-blind eligibility or calibration acquisition has one invariant
// that outranks everything it is trying to measure: candidate bytes must never
// enter the active task workspace. A delivered candidate changes what the model
// sees next, how many routes the task takes, which terminal it reaches and what
// evidence exists by the end -- and it does all of that whether or not anyone
// runs an evaluator afterwards. The first pilot delivered two candidates under
// STRICT authorization and the run's own report called the mechanism layer
// clean, because the gate it wrote only forbade an ADVISORY delivery. The
// distinction does not repair the violation.
//
// So this suppresses the delivery and keeps the reason. It is deliberately NOT
// a fourth product mode: strict, advisory and automatic_v3 are what a client
// may ask for, they are unchanged, and no experiment gets to redefine them.
// This is an operator-owned switch on a private experimental process, default
// off, and nothing a client, a model, a service or a task contract sends can
// reach it.

// CandidateCaptureOnlyEnv is the one place the control is read from. An
// environment variable on the process an operator started: not a request field,
// not a contract field, not a header, and not anything the pipeline returns.
const CandidateCaptureOnlyEnv = "ATLAS_CANDIDATE_CAPTURE_ONLY"

// candidateCaptureOnly reports whether this process is running an acquisition
// that may not deliver.
//
// Fails closed to ordinary behaviour. An unset, empty, unknown or malformed
// value is "off", because the value that would be dangerous to get wrong is the
// one that silently stops deliveries in production -- and the value that would
// be dangerous to guess is one that quietly enables an experiment. Neither
// happens: only the exact affirmative spellings enable it.
func candidateCaptureOnly() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(CandidateCaptureOnlyEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// What the policy WOULD have done, kept separately from the fact that delivery
// was suppressed. Rewriting "would authorize strict" into "baseline retained"
// would lose the only thing the acquisition is there to measure.
const (
	CaptureWouldAuthorizeStrict    = "would_authorize_strict"
	CaptureWouldDeliverAutomaticV3 = "would_deliver_automatic_v3"
	CaptureRejectedHardVeto        = "rejected_hard_veto"
	CaptureBaselineRetained        = "baseline_retained"
	CaptureSuppressedDelivery      = "capture_only_suppressed_delivery"
)

var captureOnlyDispositions = map[string]bool{
	CaptureWouldAuthorizeStrict:    true,
	CaptureWouldDeliverAutomaticV3: true,
	CaptureRejectedHardVeto:        true,
	CaptureBaselineRetained:        true,
	CaptureSuppressedDelivery:      true,
}

// captureOnlyDispositionFor maps a policy answer onto what the acquisition
// records. One value, from the closed set, for the decision the policy actually
// reached -- the suppression is a separate fact and is recorded beside it.
func captureOnlyDispositionFor(decision candidatePolicyDecision) string {
	switch decision {
	case PolicyCandidateAuthorizedStrict:
		return CaptureWouldAuthorizeStrict
	case PolicyCandidateAutomaticV3:
		return CaptureWouldDeliverAutomaticV3
	case PolicyCandidateRejectedHardVeto:
		return CaptureRejectedHardVeto
	}
	return CaptureBaselineRetained
}

// recordCaptureOnlySuppression writes the one fact the suppression creates: a
// licence that would have been minted was not.
//
// Identities and closed values, like every other private record. It is written
// at the moment of suppression rather than reconstructed later, because the
// authorization decision it describes is about to be discarded.
func recordCaptureOnlySuppression(ctx *AgentContext, entry routeEntry,
	candidateHash string, d AuthorizationDecision) {
	sink := activeShadowSink.Load()
	if !sink.enabled() {
		return
	}
	reason := d.Reason
	if !authorizationReasons[reason] {
		reason = ReasonUnknown
	}
	sink.submit(map[string]interface{}{
		"schema_version":  shadowSchemaVersionCaptureOnly,
		"record_kind":     "candidate_capture_only_suppression",
		"request_id":      requestIDOf(ctx),
		"route_entry_id":  entry.ID,
		"candidate_hash":  candidateHash,
		"disposition":     CaptureSuppressedDelivery,
		"would_authorize": d.Authorized,
		// The authorization's own answer, preserved rather than replaced. A
		// reader must be able to tell a candidate that earned a licence from
		// one that never could.
		"authorization_reason":     string(reason),
		"obligations_satisfied":    d.Satisfied,
		"grant_minted":             false,
		"grant_consumed":           false,
		"influences_live_decision": true,
		"build_version":            APIVersion,
	})
}

// recordCaptureOnlyDisposition writes the policy's would-have answer under an
// acquisition, in the closed vocabulary the analysis reads.
func recordCaptureOnlyDisposition(ctx *AgentContext, entry routeEntry,
	candidateHash string, out candidatePolicyOutcome, suppressed bool) {
	sink := activeShadowSink.Load()
	if !sink.enabled() {
		return
	}
	disposition := captureOnlyDispositionFor(out.Decision)
	if !captureOnlyDispositions[disposition] {
		disposition = CaptureBaselineRetained
	}
	sink.submit(map[string]interface{}{
		"schema_version": shadowSchemaVersionCaptureOnly,
		"record_kind":    "candidate_capture_only_disposition",
		"request_id":     requestIDOf(ctx),
		"route_entry_id": entry.ID,
		"candidate_hash": candidateHash,
		// The would-have and the suppression are two fields, never one: the
		// first says what the policy concluded and the second says that no
		// delivery followed it.
		"would_have":               disposition,
		"policy_decision":          string(out.Decision),
		"policy_mode":              string(out.Mode),
		"policy_source":            string(out.Source),
		"delivery_suppressed":      suppressed,
		"delivered":                false,
		"influences_live_decision": true,
		"build_version":            APIVersion,
	})
}

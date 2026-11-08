package app

import "github.com/itigges22/atlas-proxy/internal/wire"

// ---------------------------------------------------------------------------
// Evidence envelope validation
// ---------------------------------------------------------------------------
//
// The delivery gate owns this: it decides whether what arrived from
// /v3/generate may be read. There is no separate evidence
// service or helper layer -- one wire representation in internal/wire/v3.go, one place
// that validates it here.

// EvidenceSupportsProvenanceFor is THE gate for replacing the caller's content
// with a generated candidate and for attaching service provenance to it. Every
// condition is necessary and each one is a claim the service actually made:
//
//	available        the envelope is present, same-major and self-consistent
//	verified_winner  the SELECTION concluded a winner, not merely a best record
//	closure_eligible that winner met its own contract's floor
//	describes these  the evidence is about the exact bytes about to be written
//
// `Passed`, `PhaseSolved`, `WinningScore` and the verification-evidence strings
// are deliberately not consulted: `passed` collapses a compile smoke, a partial
// oracle score and a complete one into one boolean, which is the conflation the
// envelope exists to replace. A caller may still report them; none of them may
// authorize anything.
func EvidenceSupportsProvenanceFor(e *wire.V3EvidenceEnvelope, code string) (bool, string) {
	if code == "" {
		return false, "no candidate code"
	}
	availability, reason := e.Validate()
	if availability != wire.EvidenceAvailable {
		return false, reason
	}
	if e.Selection.Status != "verified_winner" {
		return false, "selection status " + e.Selection.Status
	}
	if !e.Evaluation.ClosureEligible {
		return false, "record is not closure-eligible"
	}
	if !e.DescribesBytes(code) {
		return false, "evidence describes a different candidate"
	}
	return true, ""
}

// v3DeliveryAuthorized answers the one question the write path asks of a V3
// response: may THESE bytes replace the caller's content and carry service
// provenance. It is the only place that decision is made, so a second caller
// cannot invent a looser rule.
func v3DeliveryAuthorized(result *wire.V3GenerateResponse, code string) (bool, string) {
	if result == nil {
		return false, "no V3 response"
	}
	return EvidenceSupportsProvenanceFor(result.Evidence, code)
}

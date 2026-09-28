package main

import "strings"

// Whether the exact candidate V3 selected may land automatically.
//
// Strict asks a question that a request declaring no verification cannot
// answer: does trusted evidence meet the floor the client declared. There is
// no floor, so the honest answer was to keep the model's own bytes -- and a
// pipeline that generates K candidates, ranks them and picks a winner never
// reached the artifact at all. That is not a safety property, it is an
// evidence property, and the two were being conflated.
//
// automatic_v3 separates them. The evidence question keeps its honest answer:
// nothing here claims the candidate is correct, and no score, consensus, Lens
// value or service verdict is consulted. What is checked is everything that
// was never about evidence in the first place -- that these are the exact
// bytes the selection path named, over a target the client declared, inside
// the mutation scope its own tool call defined, on a workspace nothing has
// moved underneath, with syntax intact and no declared check failed. Those
// hold or they do not, and they are the same requirements a strict delivery
// has always had to satisfy on top of its evidence.
//
// The user's involvement does not change: the ordinary permission prompts
// still gate dangerous tools, deletion keeps its exact-object approval, and
// what lands is reviewed as a diff. Nothing here asks anyone to adjudicate a
// candidate, because the competition is internal and always was.

// grantBasis is why a licence was minted. It is recorded on the grant, so a
// later reader can tell a delivery that satisfied a declared floor from one
// that satisfied the safety requirements and had no floor to satisfy.
type grantBasis string

const (
	// grantBasisStrict: trusted client-declared verification passed at the
	// declared strength against these exact bytes.
	grantBasisStrict grantBasis = "strict_trusted_evidence"
	// grantBasisAutomaticV3: the V3 selection path chose these exact bytes and
	// every hard safety requirement held.
	grantBasisAutomaticV3 grantBasis = "automatic_v3_selection"
)

func knownGrantBasis(b grantBasis) bool {
	return b == grantBasisStrict || b == grantBasisAutomaticV3
}

// automaticRefusal is the closed set of reasons an automatic delivery was not
// available. Each names a fact, and none of them is a score.
const (
	automaticHardVeto           = "hard_veto_observed"
	automaticNoSelection        = "no_selected_candidate_identity"
	automaticNotTheWinner       = "candidate_is_not_the_selected_winner"
	automaticIdentityIncomplete = "candidate_identity_incomplete"
	automaticNoScope            = "no_structured_mutation_scope"
	automaticTargetNotGrounded  = "target_not_declared_or_structured"
	automaticEligible           = ""
)

// automaticEligibilityInput is everything the automatic decision reads.
//
// Deliberately small. Everything else it would need has already been decided
// by an owner that is not this one: the vetoes by the policy's veto owner, the
// scope by deriveMutationScope, the identity by the evidence producers, and
// the target by the obligation owner. Re-deriving any of them here would be a
// second opinion able to disagree with the first.
type automaticEligibilityInput struct {
	// Vetoes are the disqualifying facts, computed once by the single owner.
	Vetoes []string
	// SelectedCandidateID is what the V3 selection path named as its winner.
	// It is a content hash the service produced; it is trusted to say WHICH
	// candidate was selected and for nothing else.
	SelectedCandidateID string
	// CandidateHash is the hash of the exact bytes about to be authorized,
	// computed on this side from the bytes themselves.
	CandidateHash string
	// Identity is the proxy-built binding for those bytes.
	Identity V3EvidenceProvenance
	// Scope is the structured intent of the tool call that produced them.
	Scope mutationScope
	// TargetGrounded says the target has a grounding: the client declared it
	// as an output, or the request declared work and no outputs, and the
	// model's own structured call names exactly this path. Decided by
	// the authorization owner, the only reader of both; a target nobody
	// grounded gets no automatic delivery however good the candidate looks.
	TargetGrounded bool
}

// automaticDeliveryAllowed answers whether an automatic grant may be minted,
// and names the first thing that was wrong when it may not.
//
// The selected candidate is identified, never reconstructed. Nothing here
// reads a score, a rank, an array position or a "best" flag: the service says
// which content hash it selected, this side hashes the bytes it actually holds,
// and the two must be the same string. A service that named nothing, or named
// something other than what arrived, gets no automatic delivery -- which is the
// fail-closed behaviour for every legacy or ambiguous record.
func automaticDeliveryAllowed(in automaticEligibilityInput) (bool, string) {
	if len(in.Vetoes) > 0 {
		return false, automaticHardVeto
	}
	if strings.TrimSpace(in.SelectedCandidateID) == "" {
		// No selection identity at all: a legacy record, a service that did
		// not fill it in, or a response this build cannot read. None of them
		// establishes that these bytes won anything.
		return false, automaticNoSelection
	}
	if strings.TrimSpace(in.CandidateHash) == "" {
		return false, automaticIdentityIncomplete
	}
	if in.SelectedCandidateID != in.CandidateHash {
		// The service selected something, and it is not what arrived here.
		// Delivering anyway would be delivering bytes nothing chose.
		return false, automaticNotTheWinner
	}
	// The identity that will bind the grant has to be complete before it can
	// bind anything. A blank field here becomes a grant that joins to the
	// wrong pool, and hash-prefix collisions are exactly what the instance id
	// exists to separate.
	for _, value := range []string{
		in.Identity.RequestID, in.Identity.InvocationID,
		in.Identity.CandidateInstanceID, in.Identity.WorkspaceStateHash,
	} {
		if strings.TrimSpace(value) == "" {
			return false, automaticIdentityIncomplete
		}
	}
	if in.Identity.CandidateHash != in.CandidateHash {
		return false, automaticIdentityIncomplete
	}
	if !in.Scope.valid() {
		return false, automaticNoScope
	}
	if !in.TargetGrounded {
		return false, automaticTargetNotGrounded
	}
	return true, automaticEligible
}

// automaticIntent is what a route hands the authorization owner: the mode the
// request is running under, and the disqualifying facts already computed for
// this candidate.
//
// Passed in rather than looked up. The delivery file must not read the policy
// for itself -- a structural test pins that, and the reason is that a second
// reader of the mode is a second place the answer can be different from the
// one the policy owner reached.
type automaticIntent struct {
	// VetoInput is what the route knows about the candidate before the
	// authorization decision. The vetoes are computed from it only after the
	// decision and the structural classification, which they also read.
	VetoInput advisoryInput
}

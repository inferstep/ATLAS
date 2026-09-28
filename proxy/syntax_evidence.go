package main

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// Evidence the proxy itself produced, and what it is allowed to say.
//
// The typed provenance envelope has existed since af94b75 with no producer:
// it could describe where evidence came from, and nothing came from anywhere.
// This file is the first producer, and it is deliberately the weakest one.
//
// What it attests is exactly what ATLAS's own structural gate already
// evaluated -- the same fallbackSyntaxOutcomeFor every write goes through, not
// a second checker with its own opinion. What it may therefore close is one
// kind of obligation, at one strength: structural validity, at syntax. It
// cannot be talked into anything above that, because the ceiling is on the
// SOURCE and not on the record: proxy_owned_validation's ceiling is syntax in
// provenanceCeiling, so even a forged observed strength cannot raise it.
//
// Nothing consumes what this produces. authorizedV3Replacement, the write
// path, the ledger join and terminal completion are unchanged; a structural
// guard enumerates every production consumer of provenance and fails when the
// delivery graph gains one.

// proxyEvidence is one observation the proxy made about one candidate, bound
// to everything needed to say what it is about.
//
// Outcome and authority are separate on purpose. A structural failure is a
// real, bound observation -- it is how a caller knows the bytes were looked at
// and rejected -- and it authorizes nothing. Collapsing the two would make
// "we checked and it failed" indistinguishable from "we never looked".
type proxyEvidence struct {
	Provenance V3EvidenceProvenance
	// Outcome is ValidationPassed or ValidationFailed. Any other status means
	// no evidence was produced at all and this value never exists.
	Outcome ValidationStatus
}

// Authorizes reports whether this evidence could close its obligation.
// A negative observation never can, whatever its binding says.
//
// Not called from any delivery path in this build.
func (e proxyEvidence) Authorizes() (bool, string) {
	if e.Outcome != ValidationPassed {
		return false, "observation is " + string(e.Outcome)
	}
	p := e.Provenance
	return p.MayAuthorize()
}

// syntaxEvidenceRequest is everything the producer needs and nothing it does
// not. Candidate bytes are passed for hashing and validation only; they are
// never stored on the record and never reach a log line.
type syntaxEvidenceRequest struct {
	// Obligation must be a structural-validity obligation. Anything else is
	// refused rather than downgraded.
	Obligation taskObligation
	// Path is the artifact the validator evaluated, used only to pick the
	// language the existing gate already assigns.
	Path string
	// CandidateBytes are the exact bytes the validator ran against.
	CandidateBytes string
	// CandidateHash is the hash the caller is asking about. Evidence is
	// produced only when it names these exact bytes: a caller that has moved
	// on to different bytes is asking about a candidate this run never saw.
	CandidateHash string

	// Outcome is what the gate ALREADY said about these exact bytes.
	//
	// The producer does not run the gate. The write path observes the final
	// bytes because it has to, and a producer that re-ran the checker to
	// "produce evidence" would be a second verdict about one artifact -- two
	// answers that can disagree, and a second sandbox round trip per
	// delivery. What it does instead is refuse to speak unless the caller
	// hands it an outcome the gate actually reached.
	Outcome checkOutcome

	RouteEntryID        string
	InvocationID        string
	CandidateInstanceID string
	// BaselineIdentity names the validated baseline this candidate would
	// replace, or "" when there is none. Two bindings must agree on it, so a
	// record earned against one baseline cannot be used against another.
	BaselineIdentity string
}

// produceSyntaxEvidence is THE proxy-owned syntax producer.
//
// It reports what the gate that already exists said. It runs nothing, makes
// no judgement of its own, adds no extension table, and reaches no verdict
// the gate did not reach.
//
// Absent -- nil, false -- on every case where the gate did not actually
// evaluate these exact bytes: not run, not applicable, unknown, an unknown or
// wrong-kind obligation, a hash that names other bytes, a cancelled context,
// or a missing identity. Absence is the safe answer, and it is the answer
// whenever anything is uncertain.
func produceSyntaxEvidence(ctx *AgentContext, req syntaxEvidenceRequest) (proxyEvidence, bool) {
	if ctx == nil {
		return proxyEvidence{}, false
	}
	// A cancelled run's observations describe a workspace nobody is
	// maintaining any more.
	if ctx.Ctx != nil && ctx.Ctx.Err() != nil {
		return proxyEvidence{}, false
	}
	// One kind, one strength. A syntax pass may describe a syntax obligation
	// and nothing else -- not a declared command, not an example, not the
	// claim that a baseline's behaviour survived.
	if req.Obligation.Kind != ObligationSyntacticValidity {
		return proxyEvidence{}, false
	}
	if req.Obligation.RequiredStrength != "syntax" {
		return proxyEvidence{}, false
	}
	if strings.TrimSpace(req.Obligation.ID) == "" ||
		strings.TrimSpace(req.InvocationID) == "" ||
		strings.TrimSpace(req.CandidateInstanceID) == "" {
		return proxyEvidence{}, false
	}
	requestID := requestIDOf(ctx)
	if strings.TrimSpace(requestID) == "" {
		return proxyEvidence{}, false
	}
	// The bytes the caller names must be the bytes that get validated.
	actual := contentSHA256(req.CandidateBytes)
	if actual == "" || req.CandidateHash == "" || actual != req.CandidateHash {
		return proxyEvidence{}, false
	}

	// The verdict the gate reached about these exact bytes, handed over by
	// the caller that made it. not_run, not_applicable and unknown are all
	// "we did not evaluate these bytes"; none of them is a negative
	// observation, and none produces a record.
	if !req.Outcome.attempted() {
		return proxyEvidence{}, false
	}

	generation, stateHash := workspaceIdentity(ctx)
	p := V3EvidenceProvenance{
		Source:              ProvenanceProxyOwnedValidation,
		RequestID:           requestID,
		RouteEntryID:        req.RouteEntryID,
		InvocationID:        req.InvocationID,
		CandidateInstanceID: req.CandidateInstanceID,
		CandidateHash:       actual,
		WorkspaceGeneration: generation,
		WorkspaceStateHash:  stateHash,
		BaselineIdentity:    req.BaselineIdentity,
		ObligationID:        req.Obligation.ID,
		RequiredStrength:    req.Obligation.RequiredStrength,
		ObservedStrength:    "syntax",
	}
	return proxyEvidence{Provenance: p, Outcome: req.Outcome.Status}, true
}

// --- workspace identity -------------------------------------------------------
//
// Everything a candidate evaluation depended on, other than the candidate
// itself, is represented by these two values. Evidence carries them so a later
// mutation, move or recreation makes it plainly about a workspace that no
// longer exists, rather than silently about the wrong one.

// workspaceIdentity is the generation and state of the evaluation workspace.
//
// Generation is the sum of the ledger's per-path generations: every observed
// mutation increments exactly one of them, so the sum increments too, and it
// cannot go backwards while the session lives. State is a hash over the
// canonical paths and the bytes currently observed at each -- names and
// hashes, never contents.
func workspaceIdentity(ctx *AgentContext) (int, string) {
	if ctx == nil {
		return 0, contentSHA256("")
	}
	ctx.LedgerMu.Lock()
	type entry struct{ path, hash string }
	entries := make([]entry, 0, len(ctx.Ledger))
	generation := 0
	for key, d := range ctx.Ledger {
		if d == nil {
			continue
		}
		generation += d.Generation
		h := d.CurrentHash
		if d.Tombstoned {
			h = "tombstoned"
		}
		entries = append(entries, entry{path: key, hash: h})
	}
	ctx.LedgerMu.Unlock()

	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	var sb strings.Builder
	for _, e := range entries {
		sb.WriteString(e.path)
		sb.WriteByte(0)
		sb.WriteString(e.hash)
		sb.WriteByte('\n')
	}
	return generation, contentSHA256(sb.String())
}

// baselineIdentityFor names the validated baseline a candidate would replace,
// or "" when nothing current describes that path. The name is a hash pair --
// the canonical path and the bytes currently there -- so it changes the moment
// either does and never carries the bytes themselves.
func baselineIdentityFor(ctx *AgentContext, resolved string) string {
	strength := baselineEvidenceStrength(ctx, resolved)
	if strength == "" {
		return ""
	}
	return strength + ":" + contentSHA256(resolved+"\x00"+fileSHA256(ctx, resolved))
}

// contentSHA256 is the one hash function this side uses to name bytes. It
// matches contract.content_hash in the V3 service.
func contentSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

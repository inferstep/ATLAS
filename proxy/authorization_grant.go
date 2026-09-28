package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Turning an authorized decision into something that can be spent exactly once.
//
// A decision is a statement about a moment: these bytes, this candidate, this
// workspace, and every obligation the client stated satisfied by evidence that
// binds. It is not a licence. Between deciding and writing, the workspace can
// move, the same candidate can be offered again, the request can be cancelled,
// and two goroutines can arrive at the same conclusion at once -- and a
// decision that is merely TRUE is true every one of those times.
//
// A grant is the decision made spendable. It carries the identities the
// decision was about, it is consumed atomically at one owner, and a second
// attempt gets nothing. That is the whole difference between "these bytes were
// authorized" and "these bytes may land now".
//
// Three separations the vocabulary keeps: selection does not imply
// authorization -- V3 picking a winner says nothing about whether the client's
// obligations are met; authorization does not imply delivery -- a grant can be
// minted and never spent; and consumption never re-decides -- it validates what
// the grant was bound to and refuses, because a decision recomputed under new
// state is a different decision wearing the old one's identity.

// grantCapacity bounds how many live grants one request may hold.
//
// A request delivers a handful of artifacts. A number far above that is a
// runaway, and the honest response to a runaway is to refuse before mutating
// rather than to grow.
const grantCapacity = 64

// grantRetirement says why a grant can no longer be spent. A retired grant is
// kept, not deleted: "spent" and "cancelled" are different answers to a second
// attempt, and collapsing them would make a replay look like a timeout.
type grantRetirement string

const (
	grantLive     grantRetirement = ""
	grantConsumed grantRetirement = "already_consumed"
	// grantAttempted: a consumer took it and its claim did not match. It is
	// spent either way -- taking before validating is what stops a failed
	// attempt from being a free probe.
	grantAttempted  grantRetirement = "already_attempted"
	grantCancelled  grantRetirement = "request_cancelled"
	grantTerminal   grantRetirement = "terminal_emitted"
	grantSuperseded grantRetirement = "superseded_by_a_later_decision"
	grantSessionEnd grantRetirement = "session_ended"
)

// authorizationGrant is one spendable authorization.
//
// Every field is an identity or a hash. No command text, candidate byte,
// source fragment, prompt or evidence detail is held: a grant that carried
// content would put it somewhere the redaction rules do not reach.
type authorizationGrant struct {
	// ID is canonical, so two spellings of the same delivery are one grant.
	ID string

	RequestID string
	// RouteEntryID is the entry of the candidate-generation route this grant
	// belongs to. InvocationID is derived from the same entry, and holding it
	// explicitly keeps a grant attributable even where an invocation identity
	// was never minted.
	RouteEntryID        string
	InvocationID        string
	CandidateInstanceID string
	CandidateHash       string
	// TargetPath is canonical: resolved, cleaned, absolute.
	TargetPath string

	WorkspaceGeneration int
	WorkspaceStateHash  string

	BaselineIdentity string
	BaselineHash     string
	// BaselineGeneration is the target's own ledger generation when the
	// grant was minted. The workspace digest catches a change anywhere; this
	// catches a move, a recreation or a tombstone at exactly this path, which
	// is the one that decides whether the thing being replaced is still the
	// thing that was authorized.
	BaselineGeneration int
	BaselineTombstoned bool
	// MutationScopeID names the tool call's structured intent this licence is
	// bound to. A licence that cannot name one describes a mutation nobody
	// asked for, so minting refuses without it.
	MutationScopeID string
	// baselineRetained marks a grant the route minted over the caller's own
	// bytes and then kept: no delivery was ever attempted and none was owed.
	baselineRetained bool

	// ObligationSetID and EvidenceSetID name the exact sets the decision was
	// reached over. A grant minted against one set may not be spent after
	// either changed, and naming them by hash is what makes that checkable
	// without holding either.
	ObligationSetID string
	EvidenceSetID   string

	// SelectedCandidateID is what the pipeline picked, recorded so the two
	// facts stay distinguishable. Selection is not authorization.
	SelectedCandidateID string
	// TargetGrounding names what made the target a legitimate place for a
	// candidate to land: the client's declared output, or the structured
	// mutation target of the tool call itself. Recorded so a reader can tell
	// the two apart; consulted by nothing.
	TargetGrounding string
	// Basis is WHY this licence exists: a declared floor that trusted evidence
	// met, or a V3 selection that cleared every safety requirement with no
	// floor to meet. Recorded on the grant so a later reader never has to
	// infer it from what happened to be present.
	Basis grantBasis

	// DecisionGeneration orders decisions within one request. A later
	// decision for the same target supersedes an earlier grant rather than
	// coexisting with it.
	DecisionGeneration int

	retired grantRetirement
	// deliveryRecorded keeps the delivery lifecycle to exactly one ending.
	deliveryRecorded bool
}

// grantKey is the canonical identity of a grant.
//
// Built from the canonical target and the candidate instance, so `solve.py`,
// `./solve.py` and the absolute spelling all name the same grant: an alias
// that minted its own would be a second licence for one delivery.
func grantKey(requestID, invocationID, candidateInstanceID, canonicalTarget string) string {
	return contentSHA256(strings.Join([]string{
		requestID, invocationID, candidateInstanceID, canonicalTarget,
	}, "\x00"))
}

// obligationSetIdentity names a set of obligations without holding them.
func obligationSetIdentity(obs []taskObligation) string {
	ids := make([]string, 0, len(obs))
	for _, o := range obs {
		ids = append(ids, fmt.Sprintf("%s|%v|%s", o.ID, o.Required, o.RequiredStrength))
	}
	sort.Strings(ids)
	return contentSHA256(strings.Join(ids, "\n"))
}

// evidenceSetIdentity names the evidence a decision was reached over.
//
// Outcome and strength are part of the identity: the same records with one
// verdict flipped are a different set, and a grant bound to the old one must
// not survive into the new.
func evidenceSetIdentity(evidence []proxyEvidence) string {
	ids := make([]string, 0, len(evidence))
	for _, e := range evidence {
		p := e.Provenance
		ids = append(ids, strings.Join([]string{
			p.Source, p.ObligationID, p.CandidateInstanceID, p.CandidateHash,
			p.CommandIdentity, p.ObservedStrength, string(e.Outcome),
		}, "|"))
	}
	sort.Strings(ids)
	return contentSHA256(strings.Join(ids, "\n"))
}

// mintAuthorizationGrant turns an authorized decision into a spendable one.
//
// It mints nothing it is not sure of: grantFor decides whether the basis
// holds, and only then is the grant stored where a delivery can spend it.
func mintAuthorizationGrant(ctx *AgentContext, in authorizationInput,
	d AuthorizationDecision, selectedCandidateID string,
	basis grantBasis) (*authorizationGrant, bool, string) {
	g, ok, why := grantFor(ctx, in, d, selectedCandidateID, basis)
	if !ok {
		return nil, false, why
	}
	ctx.grantMu.Lock()
	defer ctx.grantMu.Unlock()
	if ctx.grantsOff != "" {
		return nil, false, ctx.grantsOff
	}
	if ctx.grants == nil {
		ctx.grants = map[string]*authorizationGrant{}
	}
	// Overflow refuses BEFORE anything is stored. A capacity check that
	// evicted would silently retire a grant somebody else is about to spend.
	//
	// Counts LIVE grants: a spent one holds no authority, so keeping it as a
	// tombstone -- which is what distinguishes "already spent" from "never
	// existed" -- must not consume the budget for the next one.
	if _, exists := ctx.grants[g.ID]; !exists && liveGrantsLocked(ctx) >= grantCapacity {
		return nil, false, "too many live authorizations for one request"
	}
	// A later decision for the same target supersedes the earlier grant. Two
	// live licences for one delivery is the state this whole type exists to
	// make impossible.
	supersedeGrantsForTarget(ctx, g)
	ctx.grantSeq++
	g.DecisionGeneration = ctx.grantSeq
	ctx.grants[g.ID] = g
	recordGrantEvent(ctx, g, "minted", "")
	return g, true, ""
}

// grantFor builds the grant a basis would earn, without storing it.
//
// Every condition below is a fact the decision already established or an
// identity the grant must bind, and a missing one is a refusal rather than a
// grant with a hole in it. It stores nothing, so the delivery owner can ask
// which basis holds before the acquisition boundary, and the decision, the
// capture-only answer and the mint all read the same answer.
func grantFor(ctx *AgentContext, in authorizationInput,
	d AuthorizationDecision, selectedCandidateID string,
	basis grantBasis) (*authorizationGrant, bool, string) {
	if ctx == nil {
		return nil, false, "no request"
	}
	if !knownGrantBasis(basis) {
		return nil, false, "no grant basis"
	}
	// The EVIDENCE preconditions, which belong to the strict basis alone.
	// automatic_v3 has no declared floor to satisfy -- that is the whole
	// reason it exists -- and its own precondition, that these are the exact
	// bytes the selection path named, was established by
	// automaticDeliveryAllowed before this was called. Everything BELOW this
	// block is a safety requirement rather than an evidence one, and applies
	// to both bases without exception.
	if basis == grantBasisStrict {
		if !d.Authorized || d.Reason != ReasonAuthorized {
			return nil, false, "the decision did not authorize"
		}
	}
	// The target has to be GROUNDED: something structured, from the client or
	// from the model's own tool call, has to have named exactly this path. A
	// grant over a path nothing named is the one thing no amount of evidence
	// can fix.
	//
	// Strict grounds it one way: the client declared the path as an expected
	// output. automatic_v3 may also ground it in the structured mutation
	// target -- the canonical path in the parsed arguments of the tool call
	// this candidate belongs to -- because the ordinary interactive request
	// declares no outputs and the substitution is for that one attempted
	// mutation only. Neither grounding is ever read from prose.
	//
	// Canonical before the check: obligations hold canonical paths, and an
	// alias spelling of a declared target is still that target. Resolving
	// afterwards would let `./solve.py` be refused and `solve.py` accepted.
	if ctx.TaskContract == nil {
		return nil, false, "output knowledge was not declared"
	}
	target := resolveAgentPath(ctx, in.TargetPath)
	grounding := ""
	switch {
	case in.OutputKnowledgeDeclared && targetIsAuthorized(in.Obligations, target):
		grounding = targetGroundingDeclaredOutput
	case in.OutputKnowledgeDeclared:
		// The client named outputs and this is not one of them. No tool call
		// widens a declared set, under any basis.
		return nil, false, "the target is not declared"
	case basis == grantBasisAutomaticV3:
		// No declared outputs: only the model's own structured call can
		// ground the target, and only for the automatic basis.
		ok, why := structuredMutationTargetGrounds(ctx, in.Scope, target)
		if !ok {
			return nil, false, why
		}
		grounding = targetGroundingStructuredMutationTarget
	default:
		return nil, false, "output knowledge was not declared"
	}
	// The structured intent of the call this candidate belongs to. A licence
	// is bounded by the mutation its own tool call defined: the target it
	// names, and for an edit the boundary it may not leave. The scope grants
	// nothing -- everything above still had to hold -- it only refuses to let
	// a licence be wider than the call that produced it.
	if !in.Scope.valid() || in.Scope.Target != target {
		return nil, false, "no structured mutation scope for this target"
	}
	if admitted, why := scopeAdmitsCandidate(ctx, in.Scope, in.CandidateBytes); !admitted {
		return nil, false, "the candidate is outside its mutation scope (" + why + ")"
	}
	if basis == grantBasisStrict {
		// Every prerequisite satisfied and nothing owed. The decision says so;
		// this refuses to take its word without the accounting agreeing.
		if len(d.Missing) != 0 {
			return nil, false, "an authorization prerequisite is still owed"
		}
		if len(d.Satisfied) == 0 {
			return nil, false, "nothing was demonstrated"
		}
		// Every required declared command must have current trusted evidence.
		// Read from the obligation set rather than from the decision, so a
		// decision that somehow satisfied a command without a record cannot
		// mint.
		if _, missing := declaredVerificationCoverage(in.Obligations, in.Evidence); len(missing) != 0 {
			return nil, false, "a declared command has no current trusted evidence"
		}
		// Modern, non-legacy record only.
		if in.Envelope != nil {
			if availability, _ := in.Envelope.Validate(); availability != EvidenceAvailable {
				return nil, false, "the service record is not usable"
			}
			// An adapter that cannot measure this artifact class cannot
			// support a claim ABOUT evidence. The selection basis makes no such
			// claim, which is why this check belongs to the strict basis alone:
			// "nobody could measure it" is unavailable evidence, not failed
			// evidence, and refusing on it would be treating the absence of an
			// oracle as a fault of the candidate.
			if !in.Envelope.Evaluation.Supported {
				return nil, false, "the adapter does not support this artifact"
			}
		}
	}
	// A DECLARED requirement is binding under every basis. If the client named
	// commands, each one must have current trusted evidence before anything
	// lands -- automatic_v3 lowers no floor the client actually set, it only
	// declines to invent one the client did not.
	if basis == grantBasisAutomaticV3 {
		if _, missing := declaredVerificationCoverage(in.Obligations, in.Evidence); len(missing) != 0 {
			return nil, false, "a declared command has no current trusted evidence"
		}
	}

	id := in.Identity
	for _, f := range []struct{ name, value string }{
		{"request_id", id.RequestID},
		{"invocation_id", id.InvocationID},
		{"candidate_instance_id", id.CandidateInstanceID},
		{"candidate_hash", id.CandidateHash},
		{"workspace_state_hash", id.WorkspaceStateHash},
		{"target", target},
	} {
		if strings.TrimSpace(f.value) == "" {
			return nil, false, f.name + " is missing"
		}
	}
	if id.CandidateHash != in.CandidateHash {
		return nil, false, "the identity and the candidate disagree"
	}
	if requestIDOf(ctx) != id.RequestID {
		return nil, false, "the identity is not this request's"
	}

	g := &authorizationGrant{
		ID:                  grantKey(id.RequestID, id.InvocationID, id.CandidateInstanceID, target),
		RequestID:           id.RequestID,
		RouteEntryID:        in.RouteEntry.ID,
		InvocationID:        id.InvocationID,
		CandidateInstanceID: id.CandidateInstanceID,
		CandidateHash:       id.CandidateHash,
		TargetPath:          target,
		WorkspaceGeneration: id.WorkspaceGeneration,
		WorkspaceStateHash:  id.WorkspaceStateHash,
		BaselineIdentity:    id.BaselineIdentity,
		BaselineHash:        fileSHA256(ctx, target),
		BaselineGeneration:  targetGeneration(ctx, target),
		BaselineTombstoned:  targetTombstoned(ctx, target),
		ObligationSetID:     obligationSetIdentity(in.Obligations),
		EvidenceSetID:       evidenceSetIdentity(in.Evidence),
		SelectedCandidateID: selectedCandidateID,
		Basis:               basis,
		TargetGrounding:     grounding,
		MutationScopeID:     in.Scope.identity(),
	}
	return g, true, ""
}

// grantClaim is what a consumer must present. It is built from the LIVE state
// at the moment of consumption, never copied off the grant -- a claim taken
// from the grant would be asking every grant whether it matched itself.
type grantClaim struct {
	RequestID           string
	InvocationID        string
	CandidateInstanceID string
	CandidateHash       string
	TargetPath          string
	WorkspaceGeneration int
	WorkspaceStateHash  string
	BaselineIdentity    string
	BaselineHash        string
	BaselineGeneration  int
	BaselineTombstoned  bool
	ObligationSetID     string
	EvidenceSetID       string
}

// grantAttemptOutcome is the closed vocabulary of what one consumption
// attempt did. Telemetry writes these and nothing else: no path, no bytes.
type grantAttemptOutcome string

const (
	grantConsumedAuthorized grantAttemptOutcome = "consumed_authorized"
	grantConsumedRefused    grantAttemptOutcome = "consumed_refused"
	grantAlreadySpent       grantAttemptOutcome = "already_spent"
	grantNotFound           grantAttemptOutcome = "not_found"
)

// consumeAuthorizationGrant takes a grant, once, and then decides.
//
// The order is take-then-validate, and that is the whole point. Validating
// first and spending only on success makes a failed attempt free, and a free
// failed attempt is a probe: a caller can present claim after claim against a
// live licence until one fits, and every near-miss costs it nothing. So the
// first consumer to reach a live grant BURNS it, and then finds out whether
// its claim was right.
//
// A retry is therefore not a retry. It needs a newly minted decision, with its
// own decision generation, reached over whatever the state actually is now --
// which is the honest thing anyway, because a claim that did not match was a
// claim about a moment this grant was not about.
//
// Nothing restores, refreshes or unspends a grant. The only paths out of spent
// are the ones that were already there: mint a new one, or do not deliver.
func consumeAuthorizationGrant(ctx *AgentContext, claim grantClaim) (*authorizationGrant, string) {
	if ctx == nil {
		return nil, "no request"
	}
	target := resolveAgentPath(ctx, claim.TargetPath)
	// The key is the address. A claim that names no live grant burns nothing,
	// which is what stops a wrong path, request or invocation from reaching
	// past its own candidate and exhausting somebody else's licence.
	key := grantKey(claim.RequestID, claim.InvocationID, claim.CandidateInstanceID, target)

	// --- take -------------------------------------------------------------
	ctx.grantMu.Lock()
	g := ctx.grants[key]
	if g == nil {
		ctx.grantMu.Unlock()
		return nil, "no authorization for this delivery"
	}
	if g.retired != grantLive {
		prior := g.retired
		recordGrantAttempt(ctx, g, grantAlreadySpent, string(prior))
		ctx.grantMu.Unlock()
		return nil, string(prior)
	}
	// Cancellation is settled here, inside the same critical section, so a
	// cancel that lands mid-consumption cannot be raced past.
	if ctx.Ctx != nil && ctx.Ctx.Err() != nil {
		g.retired = grantCancelled
		recordGrantAttempt(ctx, g, grantConsumedRefused, string(grantCancelled))
		recordDeliveryDisposition(ctx, g, deliveryCancelled)
		ctx.grantMu.Unlock()
		return nil, string(grantCancelled)
	}
	// Taken. From here every path leaves it spent.
	g.retired = grantAttempted
	held := *g
	ctx.grantMu.Unlock()

	// --- then validate ----------------------------------------------------
	for _, f := range []struct {
		name       string
		held, want string
	}{
		{"request_id", held.RequestID, claim.RequestID},
		{"invocation_id", held.InvocationID, claim.InvocationID},
		{"candidate_instance_id", held.CandidateInstanceID, claim.CandidateInstanceID},
		{"candidate_hash", held.CandidateHash, claim.CandidateHash},
		{"target", held.TargetPath, target},
		{"workspace_state_hash", held.WorkspaceStateHash, claim.WorkspaceStateHash},
		{"baseline_identity", held.BaselineIdentity, claim.BaselineIdentity},
		{"baseline_hash", held.BaselineHash, claim.BaselineHash},
		{"obligation_set", held.ObligationSetID, claim.ObligationSetID},
		{"evidence_set", held.EvidenceSetID, claim.EvidenceSetID},
	} {
		if f.held != f.want {
			return nil, grantRefused(ctx, key, f.name+"_differs",
				f.name+" is not what the authorization was about")
		}
	}
	if held.BaselineGeneration != claim.BaselineGeneration {
		return nil, grantRefused(ctx, key, "baseline_generation_differs",
			"the target changed since the authorization")
	}
	if held.BaselineTombstoned != claim.BaselineTombstoned || claim.BaselineTombstoned {
		return nil, grantRefused(ctx, key, "target_tombstoned",
			"the target was deliberately removed")
	}
	if held.WorkspaceGeneration != claim.WorkspaceGeneration {
		return nil, grantRefused(ctx, key, "workspace_generation_differs",
			"the workspace moved since the authorization")
	}

	ctx.grantMu.Lock()
	if g := ctx.grants[key]; g != nil {
		g.retired = grantConsumed
		recordGrantAttempt(ctx, g, grantConsumedAuthorized, "")
	}
	ctx.grantMu.Unlock()
	return &held, ""
}

// grantConsumedForCandidate reports whether the one-time licence for exactly
// this candidate at exactly this target was minted, validated and spent.
//
// A read, never a take: it changes nothing, and a grant that is still live,
// merely attempted, cancelled or absent answers false. Consumption is the
// proof that the delivery happened under authorization, which is what lets
// evidence gathered before the landing describe the artifact afterwards.
func grantConsumedForCandidate(ctx *AgentContext, requestID, invocationID,
	candidateInstanceID, canonicalTarget, candidateHash string) bool {
	if ctx == nil {
		return false
	}
	key := grantKey(requestID, invocationID, candidateInstanceID, canonicalTarget)
	ctx.grantMu.Lock()
	defer ctx.grantMu.Unlock()
	g := ctx.grants[key]
	return g != nil && g.retired == grantConsumed &&
		g.RequestID == requestID && g.InvocationID == invocationID &&
		g.CandidateInstanceID == candidateInstanceID &&
		g.TargetPath == canonicalTarget && g.CandidateHash == candidateHash
}

// supersedeGrantsForTarget retires every live grant for the same target.
//
// It records the retirement. Before, the state changed and nothing was
// written, so a superseded grant simply stopped appearing: three of the seven
// grants in the v6 acquisition ended that way, and "minted, never seen again"
// was indistinguishable from a dropped record.
//
// The caller already holds ctx.grantMu.
func supersedeGrantsForTarget(ctx *AgentContext, g *authorizationGrant) {
	if ctx == nil || g == nil {
		return
	}
	for _, other := range ctx.grants {
		if other.retired == grantLive && other.TargetPath == g.TargetPath && other.ID != g.ID {
			other.retired = grantSuperseded
			recordGrantEvent(ctx, other, "retired", string(grantSuperseded))
			recordDeliveryDisposition(ctx, other, deliveryNotAttemptedSuperseded)
		}
	}
}

// knownGrantRetirement is the closed set of endings a grant may have. A cause
// outside it is a cause nothing can reconcile.
func knownGrantRetirement(r grantRetirement) bool {
	switch r {
	case grantConsumed, grantAttempted, grantCancelled, grantTerminal,
		grantSuperseded, grantSessionEnd:
		return true
	}
	return false
}

// grantRefused records a mismatch on a grant that is already spent, and
// returns the caller's reason. It never unspends: a failed attempt that gave
// the licence back would make probing free.
func grantRefused(ctx *AgentContext, key, detail, reason string) string {
	ctx.grantMu.Lock()
	if g := ctx.grants[key]; g != nil {
		recordGrantAttempt(ctx, g, grantConsumedRefused, detail)
		recordDeliveryDisposition(ctx, g, deliveryRefusedAtConsumption)
	}
	ctx.grantMu.Unlock()
	return reason
}

// retireAuthorizationGrants ends every live grant and refuses future minting.
//
// Called when the request can no longer deliver anything: cancelled, terminal
// emitted, session over. Unrelated success never calls it, and nothing ever
// refreshes a grant -- a licence that could be renewed is not one-time.
func retireAuthorizationGrants(ctx *AgentContext, reason grantRetirement) int {
	if ctx == nil || reason == grantLive {
		return 0
	}
	ctx.grantMu.Lock()
	defer ctx.grantMu.Unlock()
	n := 0
	for _, g := range ctx.grants {
		if g.retired == grantLive {
			g.retired = reason
			recordGrantEvent(ctx, g, "retired", string(reason))
			end := deliveryRetiredAtTerminal
			switch reason {
			case grantCancelled:
				end = deliveryCancelled
			case grantTerminal:
				// The route kept the caller's own bytes and never attempted a
				// delivery, so say that rather than only naming the sweep.
				if g.baselineRetained {
					end = deliveryNotAttemptedBaseline
				}
			}
			recordDeliveryDisposition(ctx, g, end)
			n++
		}
	}
	ctx.grantsOff = string(reason)
	return n
}

// liveGrantCount is for tests and telemetry. It counts, and changes nothing.
func liveGrantCount(ctx *AgentContext) int {
	if ctx == nil {
		return 0
	}
	ctx.grantMu.Lock()
	defer ctx.grantMu.Unlock()
	return liveGrantsLocked(ctx)
}

// liveGrantsLocked counts spendable grants. Caller holds grantMu.
func liveGrantsLocked(ctx *AgentContext) int {
	n := 0
	for _, g := range ctx.grants {
		if g.retired == grantLive {
			n++
		}
	}
	return n
}

// recordGrantAttempt writes one consumption attempt to the private shadow
// sink, in a closed vocabulary. Caller holds grantMu.
func recordGrantAttempt(ctx *AgentContext, g *authorizationGrant,
	outcome grantAttemptOutcome, detail string) {
	recordGrantEvent(ctx, g, string(outcome), detail)
}

// recordGrantEvent writes one grant transition to the private shadow sink.
//
// Identities only, like every other record on this path. Caller holds grantMu.
func recordGrantEvent(ctx *AgentContext, g *authorizationGrant, event, detail string) {
	sink := activeShadowSink.Load()
	if !sink.enabled() {
		return
	}
	sink.submit(map[string]interface{}{
		"schema_version":        shadowSchemaVersionEvidence,
		"record_kind":           "authorization_grant_event",
		"event":                 event,
		"detail":                detail,
		"grant_id":              g.ID,
		"request_id":            g.RequestID,
		"route_entry_id":        g.RouteEntryID,
		"invocation_id":         g.InvocationID,
		"candidate_instance_id": g.CandidateInstanceID,
		"candidate_hash":        g.CandidateHash,
		"workspace_generation":  g.WorkspaceGeneration,
		"workspace_state_hash":  g.WorkspaceStateHash,
		"baseline_identity":     g.BaselineIdentity,
		"obligation_set_id":     g.ObligationSetID,
		"evidence_set_id":       g.EvidenceSetID,
		"selected_candidate_id": g.SelectedCandidateID,
		"grant_basis":           string(g.Basis),
		"decision_generation":   g.DecisionGeneration,
		"build_version":         APIVersion,
	})
}

// targetGeneration and targetTombstoned read one path's ledger facts.
//
// Separate from workspaceIdentity on purpose: the digest says the workspace
// moved, and these say whether THIS artifact did. A delivery cares about the
// second, and reporting the first for it would blame an unrelated write.
func targetGeneration(ctx *AgentContext, resolved string) int {
	if ctx == nil {
		return 0
	}
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	if d := ctx.Ledger[filepath.Clean(resolved)]; d != nil {
		return d.Generation
	}
	return 0
}

func targetTombstoned(ctx *AgentContext, resolved string) bool {
	if ctx == nil {
		return false
	}
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	if d := ctx.Ledger[filepath.Clean(resolved)]; d != nil {
		return d.Tombstoned
	}
	return false
}

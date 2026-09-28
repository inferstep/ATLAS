package main

import "log"

// Where an authorization becomes bytes on disk.
//
// Two things had to be true before this file could exist, and both now are: a
// decision that says whether a candidate MAY land, and a grant that can be
// spent exactly once. What this adds is the only place either is acted on.
//
// The shape is deliberately narrow. For a request that declared structured
// obligations, no candidate lands without a grant -- the typed answer is
// binding, and a typed refusal falls back to the caller's own content rather
// than to a candidate the envelope happened to like. For a request that
// declared nothing there is no typed answer to give, so the existing decision
// stands untouched and byte-for-byte.
//
// Everything between consuming the grant and writing is re-checked against
// what is on disk right now, because a grant froze a moment and the write
// happens in a later one.

// deliveryAuthorization is the typed answer about one candidate.
type deliveryAuthorization struct {
	// Typed is true when the request STATED what it produces, so the typed path
	// owns whether this candidate may land. False for contractless traffic and
	// for a contract whose output knowledge is unspecified, where it has
	// nothing to say and says nothing.
	//
	// Presence-aware, not count-based: `expected_outputs: []` is a client
	// stating authoritatively that this request produces nothing, and it is
	// owned -- it authorizes no target, which is the answer, not an absence of
	// one.
	Typed bool
	// Decision is what the obligation-and-evidence machinery concluded.
	Decision AuthorizationDecision
	// Grant is non-nil only when the decision authorized AND a one-time grant
	// was minted for it. A nil grant on a typed request is a refusal.
	Grant *authorizationGrant
	// Refusal names why, for the log. It carries no content.
	Refusal string
	// MetCommands are the declared-command obligations this decision found
	// covered by current trusted evidence, and BaselinePreserved says the
	// preservation requirement was satisfied. Both travel to settlement so it
	// can tell what the delivery actually answered for.
	MetCommands       []string
	BaselinePreserved bool
	// AutomaticEligible is whether the exact bytes the V3 selection path named
	// cleared every hard safety requirement. Computed here, by the
	// authorization owner, and read by the policy owner -- never the reverse.
	AutomaticEligible bool
	// AutomaticRefusal names the first thing that was wrong when it did not.
	AutomaticRefusal string
	// Vetoes are the hard vetoes, and Unmet the classified unmet obligations
	// they were computed from: the one list the automatic eligibility and the
	// policy owner both read.
	Vetoes []string
	Unmet  map[string]AuthorizationReason
	// Basis is why a licence would exist, kept even when capture-only takes it
	// away, so the acquisition can record which rule earned it.
	Basis grantBasis
	// CaptureOnly is set when an acquisition control suppressed the licence
	// this decision had earned. WouldAuthorize keeps what the authorization
	// concluded, because the suppression discards the grant and not the answer.
	CaptureOnly    bool
	WouldAuthorize bool
	// TargetGrounding is what grounded the target of the minted grant: the
	// client's declared output, or the structured mutation target under
	// automatic_v3. Empty when nothing was minted.
	TargetGrounding string
}

// mayDeliver reports whether the candidate may land under the typed answer.
//
// A contractless request returns true because the typed path is not its owner:
// it has no target a candidate could be delivered to, and the model's own
// bytes are what its route writes.
func (a deliveryAuthorization) mayDeliver() bool {
	if !a.Typed {
		return true
	}
	return a.Grant != nil
}

// holds reports whether basis b earned this candidate a licence: minted, or
// minted but for an acquisition control. The policy owner reads this rather
// than the eligibility answer, because the mint can still refuse what the
// eligibility check allowed.
func (a deliveryAuthorization) holds(b grantBasis) bool {
	return a.Typed && a.Basis == b && (a.Grant != nil || a.WouldAuthorize)
}

// authorizeCandidateDelivery is THE live authorization owner for a candidate.
//
// It runs at the final-byte observation, where the bytes are fixed and both
// producers have just spoken for exactly those bytes. It decides, and on an
// authorized decision it mints the grant that the delivery below will spend.
// It is also told why anything it was handed came up short. The caller has
// facts this side cannot re-derive without asking again: the structural gate
// said not_run because the sandbox was unreachable, a staged command was
// refused, another timed out. Reporting all of that as "evidence missing" says
// the candidate had nothing to show for itself, when in truth nothing was
// checked -- and that is the wrong thing to go and fix.
func authorizeCandidateDelivery(ctx *AgentContext, entry routeEntry, path, code string,
	id candidateEvidenceIdentity, envelope *V3EvidenceEnvelope,
	evidence []proxyEvidence, selectedCandidateID string,
	unmet map[string]AuthorizationReason, observed checkOutcome,
	scope mutationScope, automatic automaticIntent) deliveryAuthorization {
	resolved := resolveAgentPath(ctx, path)
	hash := contentSHA256(code)

	// The identity evidence must bind to, built HERE from the live request and
	// the workspace as it stands. Never copied off a record being checked.
	generation, stateHash := workspaceIdentity(ctx)
	asked := V3EvidenceProvenance{
		RequestID:           requestIDOf(ctx),
		InvocationID:        id.InvocationID,
		CandidateInstanceID: id.CandidateInstanceID,
		CandidateHash:       hash,
		WorkspaceGeneration: generation,
		WorkspaceStateHash:  stateHash,
		BaselineIdentity:    baselineIdentityFor(ctx, resolved),
	}
	_, witness := baselineWitness(ctx, resolved)
	// The one question that decides ownership, asked of the obligation owner
	// rather than inferred from how many obligations came back.
	declared := outputKnowledgeDeclared(ctx)
	// The second way a request can be this owner's: the request declared
	// work and no outputs, and the model's own structured tool call names
	// exactly this target. Read from the contract's task mode and the scope
	// derived from the parsed arguments -- never from what anyone wrote. Declared outputs are never widened by a call, so the
	// structured target is consulted only when there are none.
	structured := false
	if !declared {
		structured, _ = structuredMutationTargetGrounds(ctx, scope, resolved)
	}
	obligations := requestObligations(ctx)
	// Whether the target has any grounding at all. Decided here, once, and
	// handed to the automatic eligibility so the policy owner's answer and
	// the mint's cannot disagree about it.
	targetGrounded := (declared && targetIsAuthorized(obligations, resolved)) || structured
	unmet = classifyStructuralUnmet(ctx, resolved, observed, unmet)
	in := authorizationInput{
		Obligations:              obligations,
		TargetPath:               resolved,
		CandidateHash:            hash,
		Identity:                 asked,
		Evidence:                 evidence,
		Envelope:                 envelope,
		BaselineWitnessCommand:   witness,
		Unmet:                    unmet,
		OutputKnowledgeDeclared:  declared,
		StructuredTargetGrounded: structured,
		RouteEntry:               entry,
		Scope:                    scope,
		CandidateBytes:           code,
	}
	d := decideAuthorization(ctx, in)
	recordAuthorizationDecision(ctx, in, d)
	// The hard vetoes, computed after the decision and the structural
	// classification they read. Computed before them, the decision's vetoes
	// (weaker than the baseline, stale identity, missing evidence) and a
	// syntax check that never ran could not fire, and an automatic delivery
	// landed on a declared output whose check never ran.
	vetoIn := automatic.VetoInput
	vetoIn.Unmet, vetoIn.Decision = unmet, d
	vetoes := advisoryVetoes(vetoIn)
	// Contractless traffic, a question, and a work request whose tool call
	// named no usable target: there is nothing for a typed answer to be
	// about, and the existing delivery decision keeps its exact previous
	// behaviour.
	if !declared && !structured {
		return deliveryAuthorization{Typed: false, Decision: d, Vetoes: vetoes, Unmet: unmet}
	}

	met, _ := declaredVerificationCoverage(in.Obligations, evidence)
	auth := deliveryAuthorization{
		Typed: true, Decision: d, MetCommands: met,
		BaselinePreserved: baselineObligationsSatisfied(in.Obligations, d),
		Vetoes:            vetoes, Unmet: unmet,
	}
	// The automatic question, asked once, here, where the bytes and their
	// identity are both fixed. It reads the same veto list the policy owner
	// is handed, because two answers about one candidate are two chances to
	// disagree.
	basis := grantBasisStrict
	auth.AutomaticEligible, auth.AutomaticRefusal = automaticDeliveryAllowed(
		automaticEligibilityInput{
			Vetoes:              vetoes,
			SelectedCandidateID: selectedCandidateID,
			CandidateHash:       hash,
			Identity:            asked,
			Scope:               scope,
			TargetGrounded:      targetGrounded,
		})
	if !d.Authorized {
		if !auth.AutomaticEligible {
			auth.Refusal = string(d.Reason)
			return auth
		}
		// The typed decision could not authorize -- there was no floor to
		// meet, or no adapter that could measure this class. For the
		// selection basis that is unavailable evidence rather than failed
		// evidence, and the safety requirements below are unchanged.
		basis = grantBasisAutomaticV3
	}
	// Which basis holds, asked of the checks the mint makes and before the
	// acquisition boundary. Where the strict basis cannot hold, the selection
	// basis is the other way the one rule can, under the same safety
	// requirements. Asked once, here, so the policy decision, the capture-only
	// answer and the mint cannot disagree: a decision that said "delivers"
	// over a grant the mint then refused wrote nothing at all.
	_, holds, why := grantFor(ctx, in, d, selectedCandidateID, basis)
	if !holds && basis == grantBasisStrict && auth.AutomaticEligible {
		basis = grantBasisAutomaticV3
		_, holds, why = grantFor(ctx, in, d, selectedCandidateID, basis)
	}
	// Which rule earned the licence, recorded before anything can take it
	// away. A caller that read only WouldAuthorize could not tell a strict
	// authorization from an automatic one, and would report the wrong decision
	// for a candidate whose declared floor was never met.
	auth.Basis = basis
	if !holds {
		auth.Refusal = why
		return auth
	}
	// THE acquisition boundary. Every candidate grant in this build is minted
	// on the next line, so suppressing here suppresses all of them -- and a
	// structural guard pins that there is no second minting site to route
	// around. Nothing above this point changed: the policy ran, the evidence
	// ran, the vetoes ran, and their answers are recorded exactly as they were.
	if candidateCaptureOnly() {
		auth.CaptureOnly, auth.WouldAuthorize = true, true
		auth.Refusal = CaptureSuppressedDelivery
		recordCaptureOnlySuppression(ctx, entry, hash, d)
		return auth
	}
	g, ok, why := mintAuthorizationGrant(ctx, in, d, selectedCandidateID, basis)
	if !ok {
		auth.Refusal = why
		return auth
	}
	auth.Grant = g
	auth.TargetGrounding = g.TargetGrounding
	return auth
}

// deliveryOutcome is the live result: true only when the exact authorized
// bytes are what is on disk afterwards.
//
// It is separate from the pool record's `delivered` field on purpose. That
// field is the service's description of what it selected -- history, written
// before anything reached this machine's filesystem. This is a statement about
// disk, made after the write, by the side that did it.
type deliveryOutcome struct {
	// Delivered is true only after the write returned successfully AND a
	// re-read of the target hashed to exactly the authorized candidate.
	Delivered bool
	// Hash is what is actually on disk, whatever happened.
	Hash string
	// Generation is the target's ledger generation as the delivery found it.
	// The ledger effect itself stays with its existing owner; this records
	// what this delivery is answerable for.
	Generation int
	// Restored records that post-write validation failed and an eligible
	// baseline was put back.
	Restored bool
	// Reason names the first thing that went wrong. Identities and
	// classifications only -- never a source line or a command.
	Reason string
}

// deliverAuthorizedCandidate is THE consumer of an authorization grant.
//
// The order below is the order in which the authorization stops being valid,
// and every step is checked against the filesystem rather than against what
// something earlier believed:
//
//  1. the grant is consumed, atomically and once;
//  2. workspace and baseline are re-read;
//  3. every bound identity is re-validated (which is what consumption does);
//  4. the bytes about to be written must hash to exactly the grant's candidate;
//  5. the target must still be the declared canonical target;
//  6. nothing may have mutated, moved, recreated or tombstoned it in between;
//  7. the existing write path does the mutation and its accounting;
//  8. the exact authorized bytes are written -- no normalisation, no repair,
//     no appended newline, no rewrite;
//  9. the existing validation runs on the bytes that landed;
//  10. what landed is re-read and compared with what was authorized.
//
// A mismatch before the write mutates nothing. A write failure claims no
// delivery and leaves the mutation debt standing. A validation failure after
// the write never reports delivered, and restores an eligible baseline where
// one structurally exists.
// candidateDeliveryRequest is one delivery, described by the route that owns
// it. Everything tool-specific arrives here as a parameter so the owner below
// stays the only implementation of grant consumption, the exact-byte write,
// the post-write hash check, settlement and restoration.
//
// Write is the originating tool's own write, because the result a caller
// returns has to be that tool's result: an edit must not answer with a
// write_file payload, or the loop's tool-call accounting and mutation debt
// describe a call that never happened.
type candidateDeliveryRequest struct {
	Tool              string
	Path              string
	Code              string
	Grant             *authorizationGrant
	Observed          checkOutcome
	MetCommands       []string
	BaselinePreserved bool
	// AutomaticEligible is whether the exact bytes the V3 selection path named
	// cleared every hard safety requirement. Computed here, by the
	// authorization owner, and read by the policy owner -- never the reverse.
	AutomaticEligible bool
	// AutomaticRefusal names the first thing that was wrong when it did not.
	AutomaticRefusal string
	// Basis is why a licence would exist, kept even when capture-only takes it
	// away, so the acquisition can record which rule earned it.
	Basis grantBasis
	Write func(path, code string, ctx *AgentContext) (*ToolResult, error)
}

// deliverAuthorizedCandidate is the new-file route's spelling of the owner.
func deliverAuthorizedCandidate(ctx *AgentContext, path, code string,
	g *authorizationGrant, observed checkOutcome, met []string,
	baselinePreserved bool) (*ToolResult, deliveryOutcome, error) {
	return deliverCandidateBytes(ctx, candidateDeliveryRequest{
		Tool: "write_file", Path: path, Code: code, Grant: g,
		Observed: observed, MetCommands: met,
		BaselinePreserved: baselinePreserved, Write: writeFileRecorded,
	})
}

// deliverCandidateBytes is THE owner of candidate delivery, for every route.
func deliverCandidateBytes(ctx *AgentContext,
	req candidateDeliveryRequest) (*ToolResult, deliveryOutcome, error) {
	path, code, g := req.Path, req.Code, req.Grant
	observed, met, baselinePreserved := req.Observed, req.MetCommands, req.BaselinePreserved
	tool := req.Tool
	if tool == "" {
		tool = "write_file"
	}
	write := req.Write
	if write == nil {
		write = writeFileRecorded
	}
	out := deliveryOutcome{}
	if ctx == nil || g == nil {
		out.Reason = "no authorization"
		return nil, out, errNoMutation(errDeliveryUnauthorized)
	}
	resolved := resolveAgentPath(ctx, path)

	// (4) and (5), before anything is spent: the bytes must be the grant's,
	// and the target must be the one it was minted for.
	if contentSHA256(code) != g.CandidateHash {
		out.Reason = "candidate_hash_mismatch"
		return nil, out, errNoMutation(errDeliveryUnauthorized)
	}
	if resolved != g.TargetPath {
		out.Reason = "target_mismatch"
		return nil, out, errNoMutation(errDeliveryUnauthorized)
	}

	// (2) and (6): read the workspace and the target as they are NOW. A grant
	// froze a moment; this is a later one, and the claim has to describe it.
	generation, stateHash := workspaceIdentity(ctx)
	claim := grantClaim{
		RequestID:           requestIDOf(ctx),
		InvocationID:        g.InvocationID,
		CandidateInstanceID: g.CandidateInstanceID,
		CandidateHash:       contentSHA256(code),
		TargetPath:          resolved,
		WorkspaceGeneration: generation,
		WorkspaceStateHash:  stateHash,
		BaselineIdentity:    baselineIdentityFor(ctx, resolved),
		BaselineHash:        fileSHA256(ctx, resolved),
		BaselineGeneration:  targetGeneration(ctx, resolved),
		BaselineTombstoned:  targetTombstoned(ctx, resolved),
		ObligationSetID:     g.ObligationSetID,
		EvidenceSetID:       g.EvidenceSetID,
	}
	// (1) and (3): consuming IS the re-validation, and it happens exactly
	// once. Nothing is mutated before this returns.
	spent, why := consumeAuthorizationGrant(ctx, claim)
	if spent == nil {
		out.Reason = why
		log.Printf("[%s] authorization not spendable for %s (%s)", tool, logPath(path), why)
		return nil, out, errNoMutation(errDeliveryUnauthorized)
	}

	// Read before the write, so settlement can tell the generation this
	// delivery produced from the one that was already there.
	priorGeneration := targetGeneration(ctx, resolved)

	// (7) and (8): the existing write path, handed the exact authorized bytes.
	result, err := write(path, code, ctx)
	if err != nil {
		// The candidate never became an artifact. No delivery is claimed and
		// the mutation debt the write path recorded stands.
		out.Reason = "write_failed"
		return result, out, overlayValidationOnError(err, observed)
	}

	// (10): what landed, read back from disk rather than assumed.
	landed, readable := readLedgerBytes(resolved)
	out.Hash = ""
	if readable {
		out.Hash = hashBytes(landed)
	}
	out.Generation = targetGeneration(ctx, resolved)

	// (9): the observation made on exactly these bytes.
	overlayValidation(result, observed)

	switch {
	case !readable:
		out.Reason = "delivered_bytes_unreadable"
	case out.Hash != g.CandidateHash:
		out.Reason = "delivered_bytes_are_not_the_authorized_ones"
	case observed.Status == ValidationFailed:
		out.Reason = "post_write_validation_failed"
	default:
		out.Delivered = true
	}
	if out.Delivered {
		result.AuthorizedDeliveryHash = g.CandidateHash
		markGrantDelivery(ctx, spent.ID, deliveryConsumedAndLanded)
		// The only writer of a settlement record, and only for a delivery
		// whose exact authorized bytes were just confirmed on disk. Nothing
		// else can produce one, which is what stops a successful tool result
		// or a selection label from manufacturing settlement.
		recordDeliverySettlement(ctx, deliverySettlement{
			GrantID:             spent.ID,
			RequestID:           spent.RequestID,
			InvocationID:        spent.InvocationID,
			CandidateInstanceID: spent.CandidateInstanceID,
			CandidateHash:       spent.CandidateHash,
			TargetPath:          resolved,
			PriorGeneration:     priorGeneration,
			MetCommands:         met,
			BaselinePreserved:   baselinePreserved,
		})
		return result, out, nil
	}

	// Post-write failure. Never report delivered, and put back an eligible
	// baseline where one structurally exists -- restoration rehashes disk and
	// the ledger for itself, and declines when there is nothing demonstrably
	// valid to return to.
	log.Printf("[%s] authorized delivery for %s did not settle (%s)",
		tool, logPath(path), out.Reason)
	markGrantDelivery(ctx, spent.ID, deliveryConsumedDidNotSettle)
	if dec := restoreDeliverable(ctx, ledgerKey(ctx, resolved)); dec.Restored {
		out.Restored = true
	}
	result.Success = false
	if result.Error == "" {
		result.Error = "the authorized candidate did not land as authorized"
	}
	return result, out, nil
}

// errDeliveryUnauthorized is what a refused delivery returns. It names no
// path, no bytes and no reason: the reason travels in the outcome, where the
// caller can classify it without it reaching a model-visible string.
var errDeliveryUnauthorized = &deliveryRefusal{}

type deliveryRefusal struct{}

func (d *deliveryRefusal) Error() string {
	return "the candidate is not authorized to land"
}

// baselineObligationsSatisfied reports whether every preservation obligation
// the task stated was among the ones the decision satisfied.
//
// A task with no baseline states none, and vacuously satisfies it: a new file
// replaces nothing.
func baselineObligationsSatisfied(obs []taskObligation, d AuthorizationDecision) bool {
	satisfied := map[string]bool{}
	for _, id := range d.Satisfied {
		satisfied[id] = true
	}
	for _, o := range authorizationPrerequisites(obs) {
		if o.Kind != ObligationBaselinePreserved || !o.Required {
			continue
		}
		if !satisfied[o.ID] {
			return false
		}
	}
	return true
}

// classifyStructuralUnmet adds the syntax obligation's own reason, when the
// structural gate said something more specific than silence.
//
// not_run has two causes and they are not the same problem: the gate was
// unreachable, or it was reachable and nobody asked. Both leave the obligation
// unmet; only one is an outage.
func classifyStructuralUnmet(ctx *AgentContext, resolved string,
	observed checkOutcome, unmet map[string]AuthorizationReason) map[string]AuthorizationReason {
	if ctx == nil || observed.Status == ValidationPassed {
		return unmet
	}
	var why AuthorizationReason
	switch {
	case observed.ProducerUnavailable:
		why = ReasonProducerUnavailable
	case observed.Status == ValidationNotRun, observed.Status == ValidationUnknown:
		why = ReasonProducerNotRun
	case observed.Status == ValidationFailed:
		// It ran and found against these bytes. That IS a fact about the
		// candidate, and the only one in this set that is.
		why = ReasonEvidenceExecutionFailed
	default:
		return unmet
	}
	for _, o := range authorizationPrerequisites(requestObligations(ctx)) {
		if o.Kind != ObligationSyntacticValidity || o.Subject != resolved {
			continue
		}
		if unmet == nil {
			unmet = map[string]AuthorizationReason{}
		}
		if _, already := unmet[o.ID]; !already {
			unmet[o.ID] = why
		}
	}
	return unmet
}

// kind is the validation kind an observation implies, using exactly the
// mapping overlayValidation already applies. Having it in one place is what
// stops a refusal path from classifying the same outcome differently.
func (o checkOutcome) kind() ValidationKind {
	switch o.Status {
	case ValidationNotApplicable:
		return ValidationKindNone
	case ValidationUnknown:
		return ValidationKindUnknown
	default:
		return ValidationKindSyntax
	}
}

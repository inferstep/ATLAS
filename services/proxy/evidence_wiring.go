package main

import (
	"strings"

	"github.com/itigges22/atlas-proxy/internal/shadowcapture"
	"github.com/itigges22/atlas-proxy/internal/taskcontract"
	"github.com/itigges22/atlas-proxy/internal/wire"
)

// Where the evidence producers actually run.
//
// The producers landed with no production caller: correct for a slice that had
// to prove inertness, and useless past it. This file is the one place either
// producer is invoked on a live request.
//
// The evidence these return feeds the delivery decision, and a staged pass also
// reaches the completion gate through recordStagedCommandFulfillment. The
// records themselves go to the private shadow sink and to nowhere else: no SSE
// event, no ToolResult, no model-visible text.

// evidenceProducerAvailability says, for each trusted source, that a live
// production path produces it on this build. A producer with no caller fails
// the structural guard.
type evidenceProducerAvailability string

const (
	// evidenceProducerWired: exactly one live production call path exists.
	evidenceProducerWired evidenceProducerAvailability = "wired"
)

// evidenceProducerStatus is the enumerated inventory. A structural guard reads
// it and fails when a producer's real wiring disagrees with its entry.
var evidenceProducerStatus = map[string]evidenceProducerAvailability{
	wire.ProvenanceProxyOwnedValidation: evidenceProducerWired,

	// Staging supplies what this producer needs and nothing else could: the
	// exact declared command, run against an isolated workspace holding the
	// exact candidate bytes, observed either side.
	//
	// The two mechanisms it is NOT remain what they were. The V3 service runs
	// its own smoke checks and never receives the task contract, so it cannot
	// run what the client declared and cannot manufacture this authority by
	// calling its own endpoint. The MODEL running the same command through
	// run_command is a different, untrusted event against the production
	// workspace after bytes have landed. Neither reaches this producer.
	wire.ProvenanceClientDeclaredVerification: evidenceProducerWired,
}

// candidateEvidenceIdentity is the identity a candidate carries through one
// invocation. Minted once, at the point the candidate's bytes are fixed.
type candidateEvidenceIdentity struct {
	InvocationID        string
	CandidateInstanceID string
}

// nextInvocationIdentity mints the identity for one candidate.
//
// The invocation is the generation call; the candidate instance is the exact
// bytes within it. Two candidates in one invocation differ by content hash;
// two invocations for the same bytes differ by sequence. Neither can be
// mistaken for the other, which is what stops one candidate's evidence binding
// to another's.
func nextInvocationIdentity(ctx *AgentContext, entry routeEntry,
	candidateHash string) candidateEvidenceIdentity {
	if ctx == nil || strings.TrimSpace(candidateHash) == "" {
		return candidateEvidenceIdentity{}
	}
	// The invocation IS the route entry that produced it. Minting a second
	// identity here would leave the feasibility decision on one side of the
	// chain and every downstream record on the other, which is the join the
	// route entry exists to close. An entry that could not be named produces
	// no invocation rather than an unattributable one.
	if !entry.valid() {
		return candidateEvidenceIdentity{}
	}
	return candidateEvidenceIdentity{
		InvocationID:        entry.ID,
		CandidateInstanceID: entry.ID + ":" + candidateHash[:16],
	}
}

// proposedCandidateIdentity is the identity of a proposed candidate when no
// producer minted one: the same binding nextInvocationIdentity makes, for the
// same route entry and the same bytes, so the two can never name one candidate
// differently.
//
// Identity is not evidence. The syntax producer above mints an identity only
// alongside a record about a DECLARED obligation, and a request that declared
// no outputs has none -- which left the ordinary interactive candidate with no
// identity at all, and an automatic delivery with nothing to bind. The bytes
// and the route entry exist whether or not anyone declared anything; this
// names them. It authorizes nothing: an identity with no grounding for its
// target still gets no grant.
func proposedCandidateIdentity(ctx *AgentContext, entry routeEntry, code string) candidateEvidenceIdentity {
	return nextInvocationIdentity(ctx, entry, contentSHA256(code))
}

// observeDeliveredCandidateSyntax is THE production call path for the
// proxy-owned syntax producer.
//
// It runs at the final-byte observation the write path already makes: the
// point where the bytes about to land are fixed and the structural gate has
// just reported on exactly those bytes. Nothing is re-checked and no second
// sandbox call is made -- the verdict is handed over, not recomputed.
//
// Returns the evidence for the caller's delivery decision. The record also goes
// to private telemetry.
func observeDeliveredCandidateSyntax(ctx *AgentContext, entry routeEntry, path, code string,
	outcome checkOutcome) (proxyEvidence, candidateEvidenceIdentity, bool) {
	if ctx == nil || ctx.TaskContract == nil {
		return proxyEvidence{}, candidateEvidenceIdentity{}, false
	}
	obs := requestObligations(ctx)
	if len(obs) == 0 {
		return proxyEvidence{}, candidateEvidenceIdentity{}, false
	}
	resolved := resolveAgentPath(ctx, path)
	// Only a target the client declared. A delivery elsewhere is not a
	// delivery this evidence has anything to say about.
	if !taskcontract.TargetIsAuthorized(obs, resolved) {
		return proxyEvidence{}, candidateEvidenceIdentity{}, false
	}
	var syntax taskcontract.TaskObligation
	for _, o := range taskcontract.AuthorizationPrerequisites(obs) {
		if o.Kind == taskcontract.ObligationSyntacticValidity && o.Subject == resolved {
			syntax = o
			break
		}
	}
	if syntax.ID == "" {
		// A class the gate does not govern owes no structural obligation, and
		// inventing one to have something to evidence is the fabrication this
		// whole split exists to prevent.
		return proxyEvidence{}, candidateEvidenceIdentity{}, false
	}

	hash := contentSHA256(code)
	id := nextInvocationIdentity(ctx, entry, hash)
	ev, ok := produceSyntaxEvidence(ctx, syntaxEvidenceRequest{
		Obligation:          syntax,
		Path:                path,
		CandidateBytes:      code,
		CandidateHash:       hash,
		Outcome:             outcome,
		RouteEntryID:        entry.ID,
		InvocationID:        id.InvocationID,
		CandidateInstanceID: id.CandidateInstanceID,
		BaselineIdentity:    baselineIdentityFor(ctx, resolved),
	})
	if !ok {
		return proxyEvidence{}, candidateEvidenceIdentity{}, false
	}
	recordEvidenceObservation(ctx, ev)
	return ev, id, true
}

// observeCandidateVerification is THE production call path for the
// client-declared verification producer.
//
// The trust boundary is the whole point of the shape below. The proxy -- not
// the service, not the executor, not the model -- reads the client's declared
// commands out of the validated request, builds the staging request itself,
// and afterwards matches every returned result back against the obligations it
// derived. A result naming a command the request never declared, or an
// obligation the proxy does not own, or bytes other than the candidate,
// matches nothing and produces nothing. Staging reports observations; it does
// not declare its own provenance trusted.
//
// Runs only when a client actually declared commands. A request that declared
// none stages nothing, so no command executes on its behalf. The target must be
// grounded the way a grant needs it: a declared output, or, where the request
// declared no outputs, the target the model's own structured call named.
// Staging only for declared outputs meant a request that declared commands and
// no outputs could never have its candidate verified, so it could never land.
//
// Returns the evidence for the caller's own inspection in tests. Production
// ignores the return value: records go to private telemetry and no delivery,
// completion or generation decision reads them.
// It also says WHY each declared command went unsatisfied. The reason matters
// as much as the outcome: a command that could not run because the executor
// was unreachable, one a safety owner declined, one that ran out of budget and
// one that ran and failed are four different facts, and only the last is a
// fact about the candidate.
func observeCandidateVerification(ctx *AgentContext, path, code string,
	id candidateEvidenceIdentity, scope mutationScope) ([]proxyEvidence, map[string]AuthorizationReason, bool) {
	if !candidateIdentified(ctx, id) {
		return nil, nil, false
	}
	obs := requestObligations(ctx)
	resolved, declared, ok := groundedDeclaredCommands(ctx, obs, path, scope)
	if !ok {
		return nil, nil, false
	}
	req := stagingRequestFor(ctx, resolved, code, id, declared)
	if ok, _ := req.validate(); !ok {
		// A declared set the staging budget cannot hold whole. Nothing runs:
		// a partial set is not a smaller obligation.
		return nil, unmetForAll(declared, ReasonEvidenceTimedOut), false
	}
	result, staged := stageCandidate(ctx, req)
	if refused := stagingRefusal(declared, req, result, staged); refused != nil {
		return nil, refused, false
	}

	byID := obligationsByID(declared)
	var out []proxyEvidence
	// Whether any staged run changed what it was measuring. The unmet reason
	// cannot say: a non-zero exit and a mutation are the same reason there,
	// and they are different facts -- one is a candidate that failed its own
	// command, the other is a candidate that rewrote the thing checking it.
	mutated := false
	unmet := map[string]AuthorizationReason{}
	for _, r := range result.Commands {
		o, known := byID[r.ObligationID]
		if !known {
			continue
		}
		if r.MutatedTarget || r.MutatedWorkspace {
			mutated = true
		}
		if !r.passedClean() {
			unmet[o.ID] = stagingUnmetReason(r)
		}
		// The same execution, kept for the completion gate as well as the
		// authorization one. It was already the only place the client's
		// declared command actually ran against the exact candidate; the
		// completion gate simply had no way to hear about it.
		recordStagedCommandFulfillment(ctx, o, r, result.Identity)
		ev, ok := produceDeclaredVerificationEvidence(ctx, verificationEvidenceRequest{
			Obligation: o, Result: r, Identity: result.Identity,
		})
		if !ok {
			markEvidenceMissing(unmet, o.ID)
			continue
		}
		recordEvidenceObservation(ctx, ev)
		out = append(out, ev)
	}
	return out, unmet, mutated
}

// candidateIdentified reports whether there is a task contract and the
// candidate carries both its invocation and its instance identity: without
// them no evidence can be tied to it.
func candidateIdentified(ctx *AgentContext, id candidateEvidenceIdentity) bool {
	if ctx == nil || ctx.TaskContract == nil {
		return false
	}
	if strings.TrimSpace(id.InvocationID) == "" ||
		strings.TrimSpace(id.CandidateInstanceID) == "" {
		return false
	}
	return true
}

// groundedDeclaredCommands returns the resolved target and the commands the
// request declared for it, or false when there is nothing to stage: no
// obligations, a target that is not grounded, or no declared command.
func groundedDeclaredCommands(ctx *AgentContext, obs []taskcontract.TaskObligation, path string,
	scope mutationScope) (string, []taskcontract.TaskObligation, bool) {
	if len(obs) == 0 {
		return "", nil, false
	}
	resolved := resolveAgentPath(ctx, path)
	if !verificationTargetGrounded(ctx, obs, resolved, scope) {
		return "", nil, false
	}

	// The declared commands, from the proxy's own derivation of the validated
	// request. This list is the authority; nothing downstream may add to it.
	declared := requiredDeclaredCommands(obs)
	if len(declared) == 0 {
		return "", nil, false
	}
	return resolved, declared, true
}

// stagingRefusal is why nothing staged may count, for every declared command:
// the staging did not answer, or its result contradicted the request it
// answers. Nil when the result may be read.
func stagingRefusal(declared []taskcontract.TaskObligation, req stagingRequest, result stagingResult,
	staged bool) map[string]AuthorizationReason {
	if !staged {
		return unmetForAll(declared, ReasonProducerUnavailable)
	}
	if valid, _ := result.validateAgainst(req); !valid {
		// The result contradicted the request it answers. Refusing here is
		// what makes a malformed or forged result worth nothing.
		return unmetForAll(declared, ReasonEvidenceExecutionFailed)
	}
	return nil
}

// obligationsByID indexes the declared obligations by their id, so a staged
// result is matched against the request's own obligations only.
func obligationsByID(declared []taskcontract.TaskObligation) map[string]taskcontract.TaskObligation {
	byID := map[string]taskcontract.TaskObligation{}
	for _, o := range declared {
		byID[o.ID] = o
	}
	return byID
}

// markEvidenceMissing marks a command whose run produced no evidence as
// missing evidence, unless its run already said why it went unmet.
func markEvidenceMissing(unmet map[string]AuthorizationReason, id string) {
	if _, already := unmet[id]; !already {
		unmet[id] = ReasonEvidenceMissing
	}
}

// verificationTargetGrounded reports whether the target is grounded the way a
// grant needs it: a declared output or, where the request declared no outputs,
// the target the model's own structured call named.
func verificationTargetGrounded(ctx *AgentContext, obs []taskcontract.TaskObligation, resolved string,
	scope mutationScope) bool {
	if !taskcontract.TargetIsAuthorized(obs, resolved) {
		if outputKnowledgeDeclared(ctx) {
			return false
		}
		if grounded, _ := structuredMutationTargetGrounds(ctx, scope, resolved); !grounded {
			return false
		}
	}
	return true
}

// requiredDeclaredCommands is every required declared-command obligation among
// the request's authorization prerequisites, in their order.
func requiredDeclaredCommands(obs []taskcontract.TaskObligation) []taskcontract.TaskObligation {
	var declared []taskcontract.TaskObligation
	for _, o := range taskcontract.AuthorizationPrerequisites(obs) {
		if o.Kind == taskcontract.ObligationDeclaredCommand && o.Required {
			declared = append(declared, o)
		}
	}
	return declared
}

// stagingRequestFor builds the staging request the proxy itself sends: the
// exact candidate bytes, the identity the proxy bound them to, and each
// declared command in order.
func stagingRequestFor(ctx *AgentContext, resolved, code string,
	id candidateEvidenceIdentity, declared []taskcontract.TaskObligation) stagingRequest {
	hash := contentSHA256(code)
	generation, stateHash := workspaceIdentity(ctx)
	req := stagingRequest{
		WireVersion:    stagingWireVersion,
		CandidateBytes: code,
		Budget:         defaultStagingBudget(),
		Identity: stagingIdentity{
			RequestID:           requestIDOf(ctx),
			RouteEntryID:        id.InvocationID,
			InvocationID:        id.InvocationID,
			CandidateInstanceID: id.CandidateInstanceID,
			CandidateHash:       hash,
			TargetPath:          resolved,
			BaselineIdentity:    baselineIdentityFor(ctx, resolved),
			WorkspaceGeneration: generation,
			WorkspaceStateHash:  stateHash,
		},
	}
	for i, o := range declared {
		req.Commands = append(req.Commands, stagingCommand{
			Text:         o.Subject,
			Identity:     contentSHA256(o.Subject),
			ObligationID: o.ID,
			Index:        i,
			Count:        len(declared),
		})
	}
	return req
}

// stagingUnmetReason turns one staged outcome into the truthful reason its
// obligation went unmet. The distinctions are the point: only exited_nonzero
// and a mutation are facts about the candidate.
func stagingUnmetReason(r stagingCommandResult) AuthorizationReason {
	switch {
	case r.MutatedTarget, r.MutatedWorkspace:
		return ReasonEvidenceExecutionFailed
	case r.Outcome == stagingUnavailable:
		return ReasonProducerUnavailable
	case r.Outcome == stagingRefused:
		return ReasonEvidenceRefused
	case r.Outcome == stagingTimedOut, r.Outcome == stagingBudgetExceeded:
		return ReasonEvidenceTimedOut
	case r.Outcome == stagingResourceExhausted:
		// Not ReasonEvidenceExecutionFailed. The command was stopped before it
		// could answer, so the obligation is unmet for want of an observation
		// rather than because an observation went against the candidate.
		return ReasonEvidenceResourceExhausted
	case r.Outcome == stagingCancelled:
		return ReasonEvidenceCancelled
	case r.Outcome == stagingUnobservable:
		return ReasonProducerNotRun
	case r.Outcome == stagingExitedNonZero:
		return ReasonEvidenceExecutionFailed
	default:
		return ReasonEvidenceMissing
	}
}

// unmetForAll marks every declared obligation with one reason, for the cases
// where nothing ran at all.
func unmetForAll(declared []taskcontract.TaskObligation, why AuthorizationReason) map[string]AuthorizationReason {
	out := map[string]AuthorizationReason{}
	for _, o := range declared {
		out[o.ID] = why
	}
	return out
}

// recordEvidenceObservation writes one observation to the private shadow sink.
//
// Identities and hashes only. No candidate byte, no command string, no path
// content, no diagnostic text -- a structural gate's failure detail can quote
// the source line that failed, so the detail is deliberately not carried.
// influences_live_decision is false and is a fact about this build, not a
// hope: no production consumer of provenance exists.
func recordEvidenceObservation(ctx *AgentContext, ev proxyEvidence) {
	sink := shadowcapture.Active()
	if !sink.Enabled() {
		return
	}
	p := ev.Provenance
	sink.Submit(map[string]interface{}{
		"schema_version":           shadowSchemaVersionEvidence,
		"record_kind":              "candidate_evidence_observation",
		"request_id":               p.RequestID,
		"route_entry_id":           p.RouteEntryID,
		"invocation_id":            p.InvocationID,
		"candidate_instance_id":    p.CandidateInstanceID,
		"candidate_hash":           p.CandidateHash,
		"workspace_generation":     p.WorkspaceGeneration,
		"workspace_state_hash":     p.WorkspaceStateHash,
		"baseline_identity":        p.BaselineIdentity,
		"obligation_id":            p.ObligationID,
		"source":                   p.Source,
		"required_strength":        p.RequiredStrength,
		"observed_strength":        p.ObservedStrength,
		"outcome":                  string(ev.Outcome),
		"influences_live_decision": false,
		"build_version":            APIVersion,
	})
}

package main

// Where a run's obligations came from.
//
// Two sources can name the files a run must produce, and merging them is what
// broke the fifty-task benchmark: the prose heuristic reads a seventy-character
// window before a filename, so "Write solve.py that reads input.txt" made the
// INPUT file a deliverable. A path the session only reads owns nothing and can
// never be demonstrated, so terminalCompletionAllowed refused completion before
// any verification, action-demand, hazard or debt logic was consulted. Fifty
// runs, zero completions, none of it about the artifact the task asked for.
//
// The rule is not "prefer the contract". It is: a caller that SAYS it knows is
// the authority on that class, and a caller that says nothing gets the
// heuristic exactly as before. The two are alternatives, never a union, and
// the source travels with the decision so a later change cannot re-merge them
// by accident.
//
// Outputs and verification are decided separately: a caller may know its files
// and not its commands. Each decision is made once, from the validated
// contract, at the point the request already resolved it -- nothing the model
// later emits can change which source spoke.

type ObligationSource string

const (
	// ObligationSourceLegacy: the caller declared no knowledge of this class,
	// so ATLAS's own inference stands, unchanged.
	ObligationSourceLegacy ObligationSource = "legacy"
	// ObligationSourceContractDeclared: the caller stated it knows, and its
	// list is the obligation -- including when that list is empty.
	ObligationSourceContractDeclared ObligationSource = "contract_declared"
)

// obligationDecision is one class's answer, with everything needed to say why
// it holds and which request it belongs to.
type obligationDecision struct {
	// Items are the canonical obligations: paths for outputs, exact command
	// strings for verification. Empty from a declared source is authoritative
	// none; empty from legacy is "the heuristic found nothing".
	Items []string
	// Typed is the verification class's per-command declaration, keyed by the
	// exact command in Items. Every command has an entry: a typed one where
	// the client declared it, the legacy one otherwise. Empty for the output
	// class, which declares paths and not commands.
	Typed  map[string]VerificationRequirement
	Source ObligationSource
	// KnowledgeSpecified is true only when the caller stated `declared`. It is
	// what separates "no obligations" from "no knowledge of obligations", and
	// downstream gates must consult it rather than len(Items).
	KnowledgeSpecified bool
	// RequestID binds the decision to the request that produced it, so a
	// decision cannot be carried into another one.
	RequestID string
}

// requestIDOf is the request this decision belongs to, or "" when the context
// carries none. A decision without an identity cannot be shown to belong to
// the request that made it.
func requestIDOf(ctx *AgentContext) string {
	if ctx == nil || ctx.Ctx == nil {
		return ""
	}
	return requestIDFromContext(ctx.Ctx)
}

// declaredItems returns the caller's list and whether it may be used as
// authority.
//
// The unstated case carries the legacy compatibility rule, and carries it HERE
// rather than only at the request boundary: a contract that reached policy
// without passing through normalizeKnowledge -- a fixture, an internal caller,
// a future decode path -- must reach the same answer the boundary would have.
// Two places deciding what a caller meant is the defect this file exists to
// remove, so the rule lives in one place and both readers use it.
//
//	unstated + non-empty list  a legacy client always meant "these are the
//	                           obligations", so it keeps that meaning
//	unstated + empty or absent the storage those clients were written against
//	                           could not tell [] from omitted, so this cannot
//	                           be promoted to authoritative none
//
// Fails closed otherwise: an unrecognised knowledge value, and a `declared`
// with no list at all, both fall back to legacy. Neither can arrive through
// validateTaskContract, and if one ever does it must not become authority.
func declaredItems(knowledge ObligationKnowledge, present bool, items []string) ([]string, bool) {
	switch knowledge {
	case KnowledgeDeclared:
		if !present {
			return nil, false
		}
	case "":
		if !present || len(items) == 0 {
			return nil, false
		}
	default:
		return nil, false
	}
	out := make([]string, 0, len(items))
	out = append(out, items...)
	return out, true
}

// resolveOutputObligation is THE decision about which files this run owes.
//
// Called once, from the agent loop, against the contract the request boundary
// already validated and canonicalised. It reads no shadow state: the capture's
// copy of the contract exists to be compared with production, and a policy
// that read it would be comparing production with itself.
func resolveOutputObligation(ctx *AgentContext, userMessage string) obligationDecision {
	d := obligationDecision{Source: ObligationSourceLegacy, RequestID: requestIDOf(ctx)}
	var tc *TaskContract
	if ctx != nil {
		tc = ctx.TaskContract
	}
	if tc != nil {
		if items, ok := declaredItems(tc.OutputKnowledge, tc.OutputsPresent(),
			tc.OutputPaths()); ok {
			d.Items = items
			d.Source = ObligationSourceContractDeclared
			d.KnowledgeSpecified = true
			return d
		}
	}
	// No contract, or no stated knowledge: the heuristic, byte for byte.
	d.Items = expectedOutputPaths(userMessage)
	return d
}

// resolveVerificationObligation is THE decision about which commands this run
// owes. Same shape, same rules, decided independently of the output class.
func resolveVerificationObligation(ctx *AgentContext) obligationDecision {
	d := obligationDecision{Source: ObligationSourceLegacy, RequestID: requestIDOf(ctx)}
	var tc *TaskContract
	if ctx != nil {
		tc = ctx.TaskContract
	}
	if tc != nil {
		// Both lists, merged by exact command identity before the knowledge
		// rule is applied. A client that sent only typed requirements declared
		// its commands, and one that sent both declared their union.
		if items, ok := declaredItems(tc.VerificationKnowledge, tc.VerificationPresent(),
			verificationCommandsOf(tc)); ok {
			d.Items = items
			d.Typed = verificationRequirementsFor(tc)
			d.Source = ObligationSourceContractDeclared
			d.KnowledgeSpecified = true
			return d
		}
	}
	// Legacy verification has no list of its own: the obligation is derived
	// from what the run actually produced, which decideVerificationDemand
	// already computes from the deliverables.
	return d
}

// requestVerificationRequirement is the client's typed declaration for one
// exact command, or the legacy form for a command it declared without a type.
//
// It lives here, with the other obligation-source decisions, so the contract is
// read in one place: a producer that decoded the contract for itself would be a
// second reader able to disagree with the derivation about what the client
// asked for.
func requestVerificationRequirement(ctx *AgentContext, command string) (VerificationRequirement, bool) {
	decision := resolveVerificationObligation(ctx)
	if !decision.KnowledgeSpecified {
		return VerificationRequirement{}, false
	}
	req, ok := decision.Typed[command]
	return req, ok
}

// outputKnowledgeDeclared reports whether this request STATED what it produces.
//
// It is the ownership question, and it is presence-aware on purpose. A contract
// declaring `expected_outputs: []` says authoritatively that the request
// produces nothing; a contract that says nothing about outputs, and a request
// with no contract at all, say only that nobody stated anything. Those are
// different facts and the count of derived obligations cannot tell them apart,
// because both derive none.
//
// One reader, asking the one owner. Nothing re-derives it from contract fields.
// requestDeclaresWork reports whether the validated contract declares work.
// A question declares no mutation and can ground no mutation authority; a
// request with no contract declared nothing. Read here, in the owner that
// already reads the validated contract, so no decision elsewhere reads it.
func requestDeclaresWork(ctx *AgentContext) bool {
	return ctx != nil && ctx.TaskContract != nil && ctx.TaskContract.TaskMode == TaskModeWork
}

func outputKnowledgeDeclared(ctx *AgentContext) bool {
	if ctx == nil || ctx.TaskContract == nil {
		return false
	}
	message := ""
	if ctx.HumanTask != "" {
		message = ctx.HumanTask
	}
	return resolveOutputObligation(ctx, message).KnowledgeSpecified
}

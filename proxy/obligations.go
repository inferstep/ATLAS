package main

import "github.com/itigges22/atlas-proxy/internal/taskcontract"

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

// requestIDOf is the request this decision belongs to, or "" when the context
// carries none. A decision without an identity cannot be shown to belong to
// the request that made it.
func requestIDOf(ctx *AgentContext) string {
	if ctx == nil || ctx.Ctx == nil {
		return ""
	}
	return requestIDFromContext(ctx.Ctx)
}

// resolveOutputObligation is THE decision about which files this run owes.
//
// Called once, from the agent loop, against the contract the request boundary
// already validated and canonicalised. It reads no shadow state: the capture's
// copy of the contract exists to be compared with production, and a policy
// that read it would be comparing production with itself.
func resolveOutputObligation(ctx *AgentContext, userMessage string) taskcontract.ObligationDecision {
	d := taskcontract.ObligationDecision{Source: taskcontract.ObligationSourceLegacy, RequestID: requestIDOf(ctx)}
	var tc *taskcontract.TaskContract
	if ctx != nil {
		tc = ctx.TaskContract
	}
	if tc != nil {
		if items, ok := taskcontract.DeclaredItems(tc.OutputKnowledge, tc.OutputsPresent(),
			tc.OutputPaths()); ok {
			d.Items = items
			d.Source = taskcontract.ObligationSourceContractDeclared
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
func resolveVerificationObligation(ctx *AgentContext) taskcontract.ObligationDecision {
	d := taskcontract.ObligationDecision{Source: taskcontract.ObligationSourceLegacy, RequestID: requestIDOf(ctx)}
	var tc *taskcontract.TaskContract
	if ctx != nil {
		tc = ctx.TaskContract
	}
	if tc != nil {
		// Both lists, merged by exact command identity before the knowledge
		// rule is applied. A client that sent only typed requirements declared
		// its commands, and one that sent both declared their union.
		if items, ok := taskcontract.DeclaredItems(tc.VerificationKnowledge, tc.VerificationPresent(),
			taskcontract.VerificationCommandsOf(tc)); ok {
			d.Items = items
			d.Typed = taskcontract.VerificationRequirementsFor(tc)
			d.Source = taskcontract.ObligationSourceContractDeclared
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
func requestVerificationRequirement(ctx *AgentContext, command string) (taskcontract.VerificationRequirement, bool) {
	decision := resolveVerificationObligation(ctx)
	if !decision.KnowledgeSpecified {
		return taskcontract.VerificationRequirement{}, false
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
	return ctx != nil && ctx.TaskContract != nil && ctx.TaskContract.TaskMode == taskcontract.TaskModeWork
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

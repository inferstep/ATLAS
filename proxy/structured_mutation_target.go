package main

import "strings"

// A structured mutation target is the one path the model's own tool call
// named, read off that call's parsed arguments and nothing else.
//
// It exists for the interactive request. A person types into the TUI, the
// client declares work -- and it declares no expected outputs, because it knows nothing structured about
// which files the task requires and guessing them from the prose is the thing
// the contract replaces. Without a declared output there was no target for an
// automatic delivery to bind to, so the pipeline's selected candidate never
// reached the artifact for exactly the traffic it exists to serve.
//
// The model's write_file or edit call is a third thing, and it is structured
// rather than inferred: fields, not sentences. It names a canonical target and
// bounds a mutation, and that target may ground the delivery of the selected
// candidate to that exact path.
//
// What it is NOT, and structural tests pin each one:
//
//   - not a task obligation. It never becomes an expected output, never feeds
//     completion, never proves anything finished, and never says a file the
//     user asked for exists;
//   - not permission for any other path, any additional file, any deletion,
//     move, rename or command, and not a way past containment, deny lists,
//     protected assets, stale-baseline checks, permissions, mutation debt or
//     completion gates;
//   - not evidence. A candidate delivered on this ground is exactly as
//     unproven as it was, and every hard veto still applies;
//   - not a widening of declared outputs: a request that named outputs is
//     bound to them, and a question request can create no mutation authority
//     at all.
//
// The mutation scope owner derives the target (deriveMutationScope, from the
// parsed call); the authorization owner decides on it and the grant records
// what grounded the target. Nothing else reads it.

// What grounded a grant's target. Closed vocabulary, recorded on the grant and
// in telemetry; never a path and never prose.
const (
	// targetGroundingDeclaredOutput: the client declared this path as an
	// expected output. The only grounding the declared-verification basis
	// accepts.
	targetGroundingDeclaredOutput = "declared_output"
	// targetGroundingStructuredMutationTarget: the model's own structured tool call
	// named this path, in a work request that declared no outputs.
	targetGroundingStructuredMutationTarget = "structured_mutation_target"
)

// Why the structured target did not ground a delivery, when it did not. Facts
// about the request and the call, closed, never a score, never a path.
const (
	structuredTargetNoContract     = "request_declared_no_contract"
	structuredTargetNotWork        = "task_mode_is_not_work"
	structuredTargetNoScope        = "no_structured_mutation_scope"
	structuredTargetMismatch       = "target_is_not_the_structured_mutation_target"
	structuredTargetNotThisRequest = "scope_belongs_to_another_request"
)

// structuredMutationTargetGrounds reports whether the structured intent of the
// call being executed may ground an automatic delivery to target, and names
// the first fact that stood in the way when it may not.
//
// Every condition is a fact about the request and the call, none about the
// candidate: the request is work, the scope was fully derived from a
// supported mutating tool, it belongs to this request, and its canonical
// target IS the target being decided. A scope for another path, a partial
// scope or a question grounds nothing. The scope's own admission checks -- workspace and target identity,
// the edit boundary -- are applied by the grant after this, exactly as they
// are for a declared target.
func structuredMutationTargetGrounds(ctx *AgentContext,
	scope mutationScope, target string) (bool, string) {
	if ctx == nil {
		return false, structuredTargetNoContract
	}
	// Asked of the obligation owner, which is the one reader of the validated
	// contract; this file reads no contract field.
	if !requestDeclaresWork(ctx) {
		return false, structuredTargetNotWork
	}
	if !scope.valid() || !mutationScopeTools[scope.Tool] {
		return false, structuredTargetNoScope
	}
	if strings.TrimSpace(target) == "" || scope.Target != target {
		return false, structuredTargetMismatch
	}
	if requestIDOf(ctx) != scope.RequestID {
		return false, structuredTargetNotThisRequest
	}
	return true, ""
}

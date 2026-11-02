package main

import (
	"github.com/itigges22/atlas-proxy/internal/taskcontract"
	"github.com/itigges22/atlas-proxy/internal/wire"
)

// What a client says about the commands it requires, in types rather than in
// command text.
//
// A bare command string carries no semantics. `python3 -c "import ast; ..."`
// and `pytest -q tests/` are the same shape to a parser and demonstrate very
// different things, and there is no honest way to tell them apart by reading
// them: a filename, the word pytest and the presence of assertions are all
// things a caller can write over anything at all. Strength therefore comes
// from a typed declaration and from nowhere else.
//
// The declaration is additive. A client that sends the older command list
// keeps its exact meaning, with the strength that list can actually support --
// runtime, the fact that a command ran and exited zero against these bytes.
// Reaching behavioral requires saying so, and saying what the command depends
// on.

// observedAssetAuthority is where the assets actually came from, as this
// session can see it.
//
// The contract's claim is the ceiling, never the floor. A client may say
// workspace_observed about assets it does own, and that is its own business;
// it may not say client_supplied about a file this session wrote, because the
// ledger records that it did.
func observedAssetAuthority(ctx *AgentContext, r taskcontract.VerificationRequirement) string {
	if r.AssetAuthority != taskcontract.AssetAuthorityClientSupplied {
		return taskcontract.AssetAuthorityWorkspaceObserved
	}
	if ctx == nil {
		return taskcontract.AssetAuthorityWorkspaceObserved
	}
	if len(r.Assets) == 0 {
		// Nothing outside the candidate is named, so there is no workspace
		// asset for this session to have authored. The command text itself is
		// the client's, and the candidate is what it runs against.
		return taskcontract.AssetAuthorityClientSupplied
	}
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	for _, a := range r.Assets {
		resolved := resolveAgentPath(ctx, a)
		for key, d := range ctx.Ledger {
			if d == nil {
				continue
			}
			if resolveAgentPath(ctx, key) != resolved {
				continue
			}
			// This session wrote it. Whoever asked for it, the file the
			// command is measured against is not the client's any more.
			return taskcontract.AssetAuthorityWorkspaceObserved
		}
	}
	return taskcontract.AssetAuthorityClientSupplied
}

// verificationObservedStrength is the strongest thing a passing run of this
// requirement may be recorded as having demonstrated.
//
// The weaker of what the client declared and what its assets can support. A
// behavioral requirement whose oracle this session wrote observes runtime, so
// it does not reach its own floor and does not authorize -- which is the
// honest outcome rather than a quietly downgraded requirement.
func verificationObservedStrength(ctx *AgentContext, r taskcontract.VerificationRequirement) string {
	declared := r.DeclaredStrength()
	ceiling, ok := taskcontract.AssetAuthorityCeilingFor(observedAssetAuthority(ctx, r))
	if !ok {
		return taskcontract.VerificationKindRuntime
	}
	if wire.StrengthRank(ceiling) < wire.StrengthRank(declared) {
		return ceiling
	}
	return declared
}

package main

import (
	"fmt"
	"sort"
	"strings"
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

// The kinds a client may declare. They are the same ladder the evidence
// strengths use, minus oracle: an oracle compares an answer against a
// reference, and no command's exit status is that comparison.
const (
	VerificationKindSyntax     = "syntax"
	VerificationKindRuntime    = "runtime"
	VerificationKindBehavioral = "behavioral"
)

var verificationKinds = map[string]bool{
	VerificationKindSyntax:     true,
	VerificationKindRuntime:    true,
	VerificationKindBehavioral: true,
}

// VerificationExpectsExitZero is the only expectation this build can observe.
// The executor reports an exit status, hashes either side and a closed
// outcome; it returns no output, so nothing here can compare what a command
// printed. A closed vocabulary of one is still a vocabulary: an expectation
// this build cannot observe is refused at the boundary rather than silently
// treated as exit_zero.
const VerificationExpectsExitZero = "exit_zero"

var verificationExpectations = map[string]bool{VerificationExpectsExitZero: true}

// Where the things a command runs against came from.
//
// client_supplied: the caller owns the assets. Either the command names none
// beyond the candidate itself, or every named asset was already in the
// workspace and this session has not written it.
//
// workspace_observed: the assets are whatever happens to be there. A file this
// session produced is in that class, and so is a test the model wrote, which is
// why observing it caps what the run may demonstrate.
const (
	AssetAuthorityClientSupplied    = "client_supplied"
	AssetAuthorityWorkspaceObserved = "workspace_observed"
)

var assetAuthorities = map[string]bool{
	AssetAuthorityClientSupplied:    true,
	AssetAuthorityWorkspaceObserved: true,
}

// assetAuthorityCeiling is the strongest thing a run may be said to have
// demonstrated, given where its assets came from.
//
// A command whose oracle the model wrote demonstrates that the model agrees
// with itself. That is a runtime fact -- something ran and exited zero -- and
// calling it behavioral would let a request manufacture its own authority by
// writing the test it is measured by.
var assetAuthorityCeiling = map[string]string{
	AssetAuthorityClientSupplied:    VerificationKindBehavioral,
	AssetAuthorityWorkspaceObserved: VerificationKindRuntime,
}

// verificationRequirementsVersion is the schema version a client must state
// alongside typed requirements. An unknown version is refused rather than
// interpreted: the whole point of the field is that a later shape cannot be
// read with today's rules.
const verificationRequirementsVersion = 1

// VerificationRequirement is one typed requirement: the exact command, what
// the client says it demonstrates, what result counts, and what it runs
// against.
type VerificationRequirement struct {
	// Command is the exact text, kept verbatim. It is hashed to form the
	// command identity the staged execution is matched by, and it is never
	// parsed, normalised or inspected for meaning.
	Command string `json:"command"`
	// Kind is what the client declares this command demonstrates. It is the
	// ONLY source of required strength.
	Kind string `json:"kind"`
	// Expects is the result that counts as satisfied, from the closed
	// vocabulary above.
	Expects string `json:"expects"`
	// Assets are the workspace-relative files the command depends on beyond
	// the candidate itself: a test module, a fixture, a golden file. Declaring
	// them is what lets the proxy check that the client, and not this session,
	// owns them.
	Assets []string `json:"assets,omitempty"`
	// AssetAuthority is the client's claim about where those assets came from.
	// It is a claim, and it is checked: an asset this session wrote is
	// workspace-observed whatever the contract says.
	AssetAuthority string `json:"asset_authority"`
}

// legacyVerificationRequirement is the typed form of an untyped command.
//
// runtime, never behavioral. The older field is a list of strings with no
// statement about what they demonstrate, and every client that ever sent one
// sent it under a build that inferred behavioral from the declaration alone.
// Keeping that inference would mean a caller who said nothing about semantics
// still carried the strongest authority the machine can act on. Runtime is
// what such a command actually shows: it ran, against these bytes, and exited
// zero.
func legacyVerificationRequirement(command string) VerificationRequirement {
	return VerificationRequirement{
		Command:        command,
		Kind:           VerificationKindRuntime,
		Expects:        VerificationExpectsExitZero,
		AssetAuthority: AssetAuthorityWorkspaceObserved,
	}
}

// validate checks one requirement in isolation. Every field is closed, and an
// unrecognised value is refused rather than defaulted.
func (r VerificationRequirement) validate() error {
	if strings.TrimSpace(r.Command) == "" {
		return fmt.Errorf("verification_requirements: a requirement has no command")
	}
	if !verificationKinds[r.Kind] {
		return fmt.Errorf("verification_requirements: kind %q is not supported", r.Kind)
	}
	if !verificationExpectations[r.Expects] {
		return fmt.Errorf("verification_requirements: expects %q is not supported", r.Expects)
	}
	if !assetAuthorities[r.AssetAuthority] {
		return fmt.Errorf("verification_requirements: asset_authority %q is not supported",
			r.AssetAuthority)
	}
	if len(r.Assets) > maxTaskContractEntries {
		return fmt.Errorf("verification_requirements: a requirement exceeds %d assets",
			maxTaskContractEntries)
	}
	for _, a := range r.Assets {
		if strings.TrimSpace(a) == "" {
			return fmt.Errorf("verification_requirements: a requirement has an empty asset")
		}
	}
	return nil
}

// declaredStrength is the strength this requirement demands. It is the kind
// and nothing else: no filename, no command text, no assertion count.
func (r VerificationRequirement) declaredStrength() string { return r.Kind }

// observedAssetAuthority is where the assets actually came from, as this
// session can see it.
//
// The contract's claim is the ceiling, never the floor. A client may say
// workspace_observed about assets it does own, and that is its own business;
// it may not say client_supplied about a file this session wrote, because the
// ledger records that it did.
func observedAssetAuthority(ctx *AgentContext, r VerificationRequirement) string {
	if r.AssetAuthority != AssetAuthorityClientSupplied {
		return AssetAuthorityWorkspaceObserved
	}
	if ctx == nil {
		return AssetAuthorityWorkspaceObserved
	}
	if len(r.Assets) == 0 {
		// Nothing outside the candidate is named, so there is no workspace
		// asset for this session to have authored. The command text itself is
		// the client's, and the candidate is what it runs against.
		return AssetAuthorityClientSupplied
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
			return AssetAuthorityWorkspaceObserved
		}
	}
	return AssetAuthorityClientSupplied
}

// verificationObservedStrength is the strongest thing a passing run of this
// requirement may be recorded as having demonstrated.
//
// The weaker of what the client declared and what its assets can support. A
// behavioral requirement whose oracle this session wrote observes runtime, so
// it does not reach its own floor and does not authorize -- which is the
// honest outcome rather than a quietly downgraded requirement.
func verificationObservedStrength(ctx *AgentContext, r VerificationRequirement) string {
	declared := r.declaredStrength()
	ceiling, ok := assetAuthorityCeiling[observedAssetAuthority(ctx, r)]
	if !ok {
		return VerificationKindRuntime
	}
	if strengthRank(ceiling) < strengthRank(declared) {
		return ceiling
	}
	return declared
}

// validateVerificationRequirements is the request-boundary check.
//
// All or nothing, like the rest of the contract: a requirement this build
// cannot express is a client asking for something it will not get, and
// dropping it would leave the caller believing it declared an obligation that
// no longer exists. Returns the stored form in canonical order.
func validateVerificationRequirements(in *TaskContract) ([]VerificationRequirement, error) {
	reqs := in.TypedVerificationRequirements()
	if len(reqs) == 0 {
		if in.VerificationRequirementsVersion != 0 {
			return nil, fmt.Errorf(
				"task_contract.verification_requirements_version was sent without requirements")
		}
		return nil, nil
	}
	if in.VerificationRequirementsVersion != verificationRequirementsVersion {
		return nil, fmt.Errorf(
			"task_contract.verification_requirements_version %d is not supported (this build reads %d)",
			in.VerificationRequirementsVersion, verificationRequirementsVersion)
	}
	if len(reqs) > maxTaskContractEntries {
		return nil, fmt.Errorf("task_contract.verification_requirements exceeds %d entries",
			maxTaskContractEntries)
	}
	seen := map[string]bool{}
	out := make([]VerificationRequirement, 0, len(reqs))
	for _, r := range reqs {
		if err := r.validate(); err != nil {
			return nil, err
		}
		if seen[r.Command] {
			// The same command declared twice is one obligation, and two
			// declarations of it would have to agree about strength.
			continue
		}
		seen[r.Command] = true
		assets := append([]string(nil), r.Assets...)
		sort.Strings(assets)
		r.Assets = assets
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Command < out[j].Command })
	return out, nil
}

// verificationRequirementsFor is the typed requirement set this request owns,
// keyed by exact command.
//
// A typed declaration answers for the commands it names. Any command the older
// list declares and the typed set does not keeps the legacy answer, so a client
// may adopt the typed form one command at a time without the untyped remainder
// silently gaining or losing authority.
func verificationRequirementsFor(tc *TaskContract) map[string]VerificationRequirement {
	out := map[string]VerificationRequirement{}
	if tc == nil {
		return out
	}
	for _, r := range tc.TypedVerificationRequirements() {
		out[r.Command] = r
	}
	for _, c := range tc.VerificationCommands() {
		if _, typed := out[c]; !typed {
			out[c] = legacyVerificationRequirement(c)
		}
	}
	return out
}

// verificationCommandsOf is every command this contract requires, typed and
// untyped, in canonical order and deduplicated by exact identity.
func verificationCommandsOf(tc *TaskContract) []string {
	set := verificationRequirementsFor(tc)
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

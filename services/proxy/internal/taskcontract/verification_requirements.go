package taskcontract

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

// VerificationRecord binds one green verification command to what it
// exercised. Covered maps each session-written path the command named to
// the sha256 of its bytes at the moment the command passed — the bytes the
// run vouched for, nothing else.
type VerificationRecord struct {
	Command  string            // the run_command line
	Redirect string            // stdin redirect source ("" = ran standalone)
	Covered  map[string]string // session-written path -> sha256 when it ran

	// Kind is what the run demonstrates (shellcmd.CommandEvidenceKind.String):
	// "execution" or "probe" show the program working; "static" (a parse,
	// lint or build) and "none" are recorded only because the client declared
	// the command, and bind no coverage. Empty is a record made before kinds
	// existed, which only a verification command could produce.
	Kind string
	// Failed marks a run that exited non-zero. It is kept so a later failure
	// on the same bytes takes back an earlier pass (coverageRecord).
	Failed bool

	// The workspace this run was about, stamped from workspaceIdentity after
	// the command's own effects were reconciled into the ledger.
	//
	// Covered answers "which artifact bytes did this command exercise", and a
	// command that names no file answers it with nothing -- which is honest,
	// and is why a pathless command could never be current before. These two
	// answer a different question: WHEN did it run, in terms the session can
	// re-check. A pathless command is current exactly while both still equal
	// the workspace's current identity, and any material mutation to a tracked
	// artifact moves them.
	//
	// They are not a second coverage: they say nothing about which bytes the
	// command touched, and nothing here may satisfy a path obligation.
	WorkspaceGeneration int
	WorkspaceStateHash  string
}

// showsWorking reports a passing run of a kind that shows the program
// working.
func (r VerificationRecord) ShowsWorking() bool {
	if r.Failed {
		return false
	}
	switch r.Kind {
	case "", "execution", "probe":
		return true
	}
	return false
}

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

// IsVerificationKind reports whether strength is one a client may declare.
func IsVerificationKind(strength string) bool {
	return verificationKinds[strength]
}

// verificationExpectsExitZero is the only expectation this build can observe.
// The executor reports an exit status, hashes either side and a closed
// outcome; it returns no output, so nothing here can compare what a command
// printed. A closed vocabulary of one is still a vocabulary: an expectation
// this build cannot observe is refused at the boundary rather than silently
// treated as exit_zero.
const verificationExpectsExitZero = "exit_zero"

var verificationExpectations = map[string]bool{verificationExpectsExitZero: true}

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

// AssetAuthorityCeilingFor is the strongest floor an asset of this authority
// may set, and whether the authority is known.
func AssetAuthorityCeilingFor(authority string) (string, bool) {
	ceiling, ok := assetAuthorityCeiling[authority]
	return ceiling, ok
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

// LegacyVerificationRequirement is the typed form of an untyped command.
//
// runtime, never behavioral. The older field is a list of strings with no
// statement about what they demonstrate, and every client that ever sent one
// sent it under a build that inferred behavioral from the declaration alone.
// Keeping that inference would mean a caller who said nothing about semantics
// still carried the strongest authority the machine can act on. Runtime is
// what such a command actually shows: it ran, against these bytes, and exited
// zero.
func LegacyVerificationRequirement(command string) VerificationRequirement {
	return VerificationRequirement{
		Command:        command,
		Kind:           VerificationKindRuntime,
		Expects:        verificationExpectsExitZero,
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
	if len(r.Assets) > MaxTaskContractEntries {
		return fmt.Errorf("verification_requirements: a requirement exceeds %d assets",
			MaxTaskContractEntries)
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
func (r VerificationRequirement) DeclaredStrength() string { return r.Kind }

// ValidateVerificationRequirements is the request-boundary check.
//
// All or nothing, like the rest of the contract: a requirement this build
// cannot express is a client asking for something it will not get, and
// dropping it would leave the caller believing it declared an obligation that
// no longer exists. Returns the stored form in canonical order.
func ValidateVerificationRequirements(in *TaskContract) ([]VerificationRequirement, error) {
	reqs := in.typedVerificationRequirements()
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
	if len(reqs) > MaxTaskContractEntries {
		return nil, fmt.Errorf("task_contract.verification_requirements exceeds %d entries",
			MaxTaskContractEntries)
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

// VerificationRequirementsFor is the typed requirement set this request owns,
// keyed by exact command.
//
// A typed declaration answers for the commands it names. Any command the older
// list declares and the typed set does not keeps the legacy answer, so a client
// may adopt the typed form one command at a time without the untyped remainder
// silently gaining or losing authority.
func VerificationRequirementsFor(tc *TaskContract) map[string]VerificationRequirement {
	out := map[string]VerificationRequirement{}
	if tc == nil {
		return out
	}
	for _, r := range tc.typedVerificationRequirements() {
		out[r.Command] = r
	}
	for _, c := range tc.VerificationCommands() {
		if _, typed := out[c]; !typed {
			out[c] = LegacyVerificationRequirement(c)
		}
	}
	return out
}

// VerificationCommandsOf is every command this contract requires, typed and
// untyped, in canonical order and deduplicated by exact identity.
func VerificationCommandsOf(tc *TaskContract) []string {
	set := VerificationRequirementsFor(tc)
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

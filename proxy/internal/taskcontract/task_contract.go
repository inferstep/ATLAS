// Package taskcontract holds the words of what a request asked for: the task
// contract, the obligations it creates and the verification it requires. What
// reads those words against a run stays in the proxy's main package.
package taskcontract

import (
	"github.com/itigges22/atlas-proxy/internal/wire"
)

// DeliverableState is one canonical path's observed state. Four different
// things are kept apart deliberately and must never be collapsed: what is on
// disk now, what evidence exists about some exact bytes, whether a restorable
// checkpoint is available, and task correctness -- which this type never
// claims and never stores.
// DeliverableState, the ledger's record of one path: the bytes last seen on
// disk, the verdict about exact bytes, the checkpoint and the tombstone; and
// the ceilings on what the ledger keeps in memory and re-reads to hash.
type DeliverableState struct {
	Path        string // canonical workspace-relative
	CurrentHash string // sha256 of the bytes last observed on disk
	CurrentSize int
	Generation  int // increments on every observed mutation

	// The verdict and the EXACT bytes it describes. A verdict whose
	// ValidatedHash differs from CurrentHash is historical, never current.
	ValidationKind   wire.ValidationKind
	ValidationStatus wire.ValidationStatus
	ValidationDetail string
	ValidatedHash    string

	// At most one prior demonstrably-passed version. Bytes are held in
	// memory only, under explicit ceilings.
	CheckpointHash        string
	CheckpointBytes       []byte
	CheckpointUnavailable string // why there is none, when there is none
	// CheckpointKind is the kind of check the checkpoint's pass came from.
	// Restoration compares kinds: a structural pass is not evidence about
	// syntax, so swapping one for the other would be a guess dressed as
	// recovery.
	CheckpointKind wire.ValidationKind
	// Recovered records that the CURRENT bytes were put there by system
	// recovery rather than by the model. It is a server-side fact like the
	// rest of the ledger and never reaches the model, SSE or the wire.
	Recovered bool

	// Tombstones. A deliberately removed file must never be resurrected, so
	// the state is recorded and restoration is prohibited outright.
	Tombstoned        bool
	TombstoneReason   string // "deleted" | "moved:<canonical dst>"
	RestoreProhibited bool
}

// CurrentValidation reports the verdict ONLY when it describes the bytes
// currently recorded. Anything else is historical evidence and is reported as
// unknown, which fails closed.
func (d *DeliverableState) CurrentValidation() (wire.ValidationKind, wire.ValidationStatus) {
	if d == nil || d.ValidatedHash == "" || d.ValidatedHash != d.CurrentHash {
		return wire.ValidationKindUnknown, wire.ValidationUnknown
	}
	return d.ValidationKind, d.ValidationStatus
}

// --- The structured task contract -------------------------------------------
//
// Without it, ATLAS decides what the user demanded by reading their English:
// which verbs appeared decides whether work was required, which filenames sat
// near a write verb become deliverables, which phrases demand verification.
// Those lists are open vocabularies and they are measurably wrong -- "Write
// four files." produces no expected outputs at all, and "Run the tests and make
// sure they pass." sets no verification requirement.
//
// The contract is the client saying, in typed fields, what it already knows.
//
// Three rules hold for good. The model cannot write this -- it arrives in the
// HTTP request or not at all. It is a statement of obligation, never of
// fulfilment: what actually happened is still the ledger's to say. And nothing
// in it can authorise a destructive act; deletion stays with the permission
// endpoint and never appears here.
type TaskMode string

const (
	// TaskModeWork: the request asks for the workspace to change.
	TaskModeWork TaskMode = "work"
	// TaskModeQuestion: the request is answered by reading.
	TaskModeQuestion TaskMode = "question"
)

// MaxTaskContractEntries bounds each list, matching the ceiling the session's
// other per-path state already uses.
const MaxTaskContractEntries = 64

// ObligationKnowledge is what the CLIENT says it knows about a class of
// obligation. It is stated, never inferred.
//
// encoding/json can tell `"expected_outputs": []` from an omitted key -- the
// first decodes to a non-nil empty slice, the second to nil -- and output
// rebuilt by append turns an explicit [] into nil, byte-identical to omitted,
// so validateTaskContract stores presence explicitly.
//
// That is not a cosmetic loss. Every owned sender -- the TUI,
// tests/e2e/conftest.py, scripts/eval/e2e-reliability.py -- sends
// `{"task_mode": ...}` and nothing else, so a rule of the form "a contract is
// present, therefore its output list is authoritative" would read 100% of
// owned traffic as "this caller requires no outputs" and drop the legacy
// obligation for all of it.
type ObligationKnowledge string

const (
	// KnowledgeUnspecified: the caller has no structured knowledge of this
	// obligation class. It does NOT mean none are required -- it means the
	// caller did not say, and the legacy inference still applies.
	KnowledgeUnspecified ObligationKnowledge = "unspecified"
	// KnowledgeDeclared: the caller is the authority here, including when the
	// list it declares is empty.
	KnowledgeDeclared ObligationKnowledge = "declared"
)

// TaskContract is what the client declares about the request. Absent is
// distinct from present-and-empty, and stays distinct: an absent contract must
// not be read as work here.
type TaskContract struct {
	TaskMode TaskMode `json:"task_mode"`

	// OutputKnowledge / VerificationKnowledge are independent: a caller may
	// know its outputs and not its verification, or the reverse. Omitted
	// means unspecified, so a client that predates these fields is unchanged.
	OutputKnowledge       ObligationKnowledge `json:"output_knowledge,omitempty"`
	VerificationKnowledge ObligationKnowledge `json:"verification_knowledge,omitempty"`

	// ExpectedOutputs are exact paths the client knows the request must
	// produce. Validated and deduplicated through the workspace resolver the
	// rest of the proxy uses (there is no second path rule), and stored as
	// the client spelled them.
	//
	// A POINTER, so absence survives: nil is "no list arrived", a non-nil
	// empty slice is "the caller sent []". Read it through OutputPaths and
	// OutputsPresent rather than testing nilness at each site, where that
	// test is easy to get wrong.
	ExpectedOutputs *[]string `json:"expected_outputs,omitempty"`
	// Verification are the exact commands the client requires be run. Kept as
	// the client wrote them: the command/result contract already records
	// commands verbatim, so binding a requirement to an execution later needs
	// no parsing. Declaring one here does not run anything.
	Verification *[]string `json:"verification,omitempty"`

	// VerificationRequirements is the typed form of the same thing: the exact
	// command, what the client declares it demonstrates, what result counts,
	// and the assets it runs against. It is the only source of a required
	// strength above runtime.
	//
	// Additive and independent. A client may send neither, either, or both;
	// a command that appears in both is answered by its typed declaration,
	// and one that appears only in the older list keeps the legacy answer.
	VerificationRequirements *[]VerificationRequirement `json:"verification_requirements,omitempty"`
	// VerificationRequirementsVersion is stated by the client and checked
	// exactly. A version this build does not read is refused rather than
	// interpreted under today's rules.
	VerificationRequirementsVersion int `json:"verification_requirements_version,omitempty"`

	// CandidatePolicy is accepted and never read: there is one delivery rule
	// (candidate_policy.go), and an older client that still sends the field
	// keeps working.
	CandidatePolicy string `json:"candidate_policy,omitempty"`
}

// TypedVerificationRequirements is the declared typed set, or nil.
func (tc *TaskContract) TypedVerificationRequirements() []VerificationRequirement {
	if tc == nil || tc.VerificationRequirements == nil {
		return nil
	}
	return *tc.VerificationRequirements
}

// OutputPaths is the declared output list, or nil when none arrived. Safe on a
// nil contract.
func (tc *TaskContract) OutputPaths() []string {
	if tc == nil || tc.ExpectedOutputs == nil {
		return nil
	}
	return *tc.ExpectedOutputs
}

// OutputsPresent reports whether a list ARRIVED, which an empty one did.
func (tc *TaskContract) OutputsPresent() bool {
	return tc != nil && tc.ExpectedOutputs != nil
}

// VerificationCommands is the declared command list, or nil when none arrived.
func (tc *TaskContract) VerificationCommands() []string {
	if tc == nil || tc.Verification == nil {
		return nil
	}
	return *tc.Verification
}

// VerificationPresent reports whether a list ARRIVED, which an empty one did.
//
// Either list arriving is a list arriving: a client that sends only typed
// requirements has stated its verification knowledge as surely as one that
// sends only command strings.
func (tc *TaskContract) VerificationPresent() bool {
	if tc == nil {
		return false
	}
	return tc.Verification != nil || tc.VerificationRequirements != nil
}

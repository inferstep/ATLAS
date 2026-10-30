// What a tool may do to the workspace (ToolEffect, declared on each ToolDef),
// and the closed vocabularies in which a tool result says what happened:
// MutationStatus, ValidationKind and ValidationStatus, with
// ToolResult.Classified over all three. ToolResult itself is in types.go.

package main

import "encoding/json"

// ---------------------------------------------------------------------------
// Tool definitions
// ---------------------------------------------------------------------------

// ToolEffect declares what a tool is CAPABLE of doing to workspace state. It
// is a property of the tool, not of any one call, and it lives on ToolDef so a
// new tool cannot be registered without the decision being made. A parallel
// map keyed by tool name would drift; this cannot.
//
// Capability is not outcome. A direct mutator still reports applied, refused,
// failed or none per branch -- the effect only says the tool is capable of
// mutating and therefore owes a local classification that the shared boundary
// must not invent for it.
type ToolEffect string

const (
	// ToolEffectUnknown is the zero value: a registration that never declared
	// its effect. It is a migration defect, never a runtime state.
	ToolEffectUnknown ToolEffect = ""

	// ToolEffectReadOnly cannot mutate workspace state by construction.
	ToolEffectReadOnly ToolEffect = "read_only"

	// ToolEffectDirectMutation performs and observes its own mutation, so only
	// the branch that ran it knows the outcome.
	ToolEffectDirectMutation ToolEffect = "direct_mutation"

	// ToolEffectCommandUnobserved may change the workspace without measuring
	// it: arbitrary shell, background jobs, and killing a running process
	// (SIGTERM/SIGKILL mid-write can leave partial bytes nobody compared).
	ToolEffectCommandUnobserved ToolEffect = "command_unobserved"
)

// BoundaryClassifiable reports whether executeToolCall may supply the
// classification itself. It may do so only where the ANSWER FOLLOWS FROM THE
// CLASS: read-only tools mutate nothing, command tools measure nothing.
// Direct mutators are excluded on purpose -- their outcome is branch-local,
// and letting the boundary fill it in would conceal a missed producer.
func (e ToolEffect) BoundaryClassifiable() bool {
	return e == ToolEffectReadOnly || e == ToolEffectCommandUnobserved
}

// ToolDef defines a tool that the model can call.
type ToolDef struct {
	// Effect is required. The registry exhaustiveness test fails on any
	// production tool left at ToolEffectUnknown.
	Effect ToolEffect

	Name        string
	Description string
	InputSchema interface{} // Go struct with json tags, marshaled to JSON Schema
	Execute     func(input json.RawMessage, ctx *AgentContext) (*ToolResult, error)
	ReadOnly    bool // true = can run in parallel, no side effects
	Destructive bool // true = requires permission confirmation
}

// MutationStatus reports what happened to the filesystem, separately from
// whether the tool ran. `Success` alone could not express the difference
// between a write that landed and one a gate refused.
//
// The zero value is MutationUnknown, NOT MutationNone. An unmigrated
// producer and one that intentionally mutated nothing must stay
// distinguishable, or the producer audit cannot prove its own completeness:
// a missed call site would look exactly like a deliberate no-op.
type MutationStatus string

const (
	MutationUnknown MutationStatus = ""     // unclassified: nobody set it (zero value)
	MutationNone    MutationStatus = "none" // deliberately mutated nothing
	MutationApplied MutationStatus = "applied"
	MutationRefused MutationStatus = "refused" // a gate declined it
	MutationFailed  MutationStatus = "failed"  // attempted, errored

	// MutationUnobserved: this tool can change the workspace and did not
	// look. Command tools run arbitrary shell -- `sed -i`, a build step,
	// `python fix.py` -- and perform no pre/post state comparison, so
	// reporting MutationNone would assert a fact the producer does not have.
	// Unobserved is a current producer explicitly saying side effects were
	// not measured. It is Classified (the producer spoke) but never Applied
	// (nothing was demonstrated).
	MutationUnobserved MutationStatus = "unobserved"
)

// Applied is true for exactly MutationApplied. Comparing against a literal
// at a call site is what lets an unknown, case-shifted or future value leak
// through as success.
func (m MutationStatus) Applied() bool { return m == MutationApplied }

// Classified reports whether a producer actually set this field.
func (m MutationStatus) Classified() bool {
	switch m {
	case MutationNone, MutationApplied, MutationRefused, MutationFailed,
		MutationUnobserved:
		return true
	}
	return false
}

// ValidationKind names WHAT was checked. A syntax pass is not evidence the
// task was verified; keeping the kind explicit stops a cheap check from
// being read later as an expensive one.
type ValidationKind string

const (
	ValidationKindUnknown ValidationKind = ""     // unclassified (zero value)
	ValidationKindNone    ValidationKind = "none" // deliberately checked nothing
	ValidationKindSyntax  ValidationKind = "syntax"

	// ValidationKindStructural: the content parses but references something
	// that does not resolve. In the write_file handler the syntax gate runs
	// BEFORE the structural one, so a structural rejection means syntax
	// PASSED on those exact bytes -- recording it as syntax/failed would
	// assert the opposite of what happened.
	//
	// Where two checks ran with different outcomes, these singular fields
	// carry the DECISIVE one: the failure that caused the refusal, or the
	// applicable result for an applied write.
	ValidationKindStructural ValidationKind = "structural"

	// ValidationKindExecution: the run executed these exact bytes and the
	// process came up clean (exited 0, or is still serving after its settle
	// window). That is stronger evidence than a parse, and it settles a
	// content debt the parse-based routes cannot, such as a structural_edit
	// that landed through V3 with no parse verdict of its own. Recorded
	// without a checkpoint: an execution is not a write.
	ValidationKindExecution ValidationKind = "execution"
)

func (k ValidationKind) Classified() bool {
	switch k {
	case ValidationKindNone, ValidationKindSyntax, ValidationKindStructural, ValidationKindExecution:
		return true
	}
	return false
}

// ValidationStatus reports the OUTCOME of that check. `not_run` and
// `not_applicable` are distinct facts -- "could not check" versus "nothing
// here to check" -- and both are distinct from Unknown, which means no
// producer spoke at all.
type ValidationStatus string

const (
	ValidationUnknown       ValidationStatus = ""        // unclassified (zero value)
	ValidationNotRun        ValidationStatus = "not_run" // deliberately not checked
	ValidationNotApplicable ValidationStatus = "not_applicable"
	ValidationPassed        ValidationStatus = "passed"
	ValidationFailed        ValidationStatus = "failed"
)

// Passed is true for exactly ValidationPassed, for the same reason as Applied.
func (v ValidationStatus) Passed() bool { return v == ValidationPassed }

func (v ValidationStatus) Classified() bool {
	switch v {
	case ValidationNotRun, ValidationNotApplicable, ValidationPassed, ValidationFailed:
		return true
	}
	return false
}

// Classified reports whether every fact on this result was set by a
// producer. The producer audit asserts this over each mutation-producing
// path; until it holds everywhere, no consumer may read the new fields.
//
// A syntax kind paired with an unknown status is NOT classified: claiming a
// check happened without saying how it came out is exactly the ambiguity
// these fields exist to remove.
func (r *ToolResult) Classified() bool {
	return r.MutationStatus.Classified() &&
		r.ValidationKind.Classified() &&
		r.ValidationStatus.Classified()
}

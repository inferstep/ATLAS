// Package wire holds the shapes that cross a boundary: the model's messages,
// tool inputs and results, the V3 service's JSON and the /events envelope. It
// depends on nothing else in the proxy.
package wire

import (
	"encoding/json"
	"fmt"
)

// SupersededWrite is what a completed write turned out to be.
type SupersededWrite struct {
	// Path is the resolved target, as the filesystem knows it.
	Path string
	// Rel is the model's own spelling, for anything shown to it.
	Rel string
	// Submitted is what the model sent.
	Submitted string
	// Delivered is what is on disk now.
	Delivered string
}

// ModelResponse is what the LLM emits (constrained by grammar/json_schema).
// Exactly one of the three variants is populated per response.
type ModelResponse struct {
	Type    string          `json:"type"`    // "tool_call", "text", or "done"
	Name    string          `json:"name"`    // tool name (only for tool_call)
	Args    json.RawMessage `json:"args"`    // tool arguments (only for tool_call)
	Content string          `json:"content"` // text content (only for text)
	Summary string          `json:"summary"` // completion summary (only for done)
}

// AgentMessage represents a message in the agent loop conversation.
type AgentMessage struct {
	Role       string `json:"role"` // "system", "user", "assistant", "tool"
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id,omitempty"` // for tool results
	ToolName   string `json:"tool_name,omitempty"`    // for tool results
}

// ToolResult is the structured output returned to the model after tool execution.
type ToolResult struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`

	// Facts about the mutation and the check, reported independently of
	// `Success` (which keeps its existing tool-operation meaning on the
	// wire). All three are omitempty, so an older consumer sees the payload
	// it always saw, and a payload from an older producer decodes to the
	// fail-closed zero values above.
	//
	// Nothing here says whether the work counts as progress or authorises
	// completion. That is task-contextual and is derived in the agent layer.
	MutationStatus   MutationStatus   `json:"mutation_status,omitempty"`
	ValidationKind   ValidationKind   `json:"validation_kind,omitempty"`
	ValidationStatus ValidationStatus `json:"validation_status,omitempty"`
	ValidationDetail string           `json:"validation_detail,omitempty"`

	// AuthorizedDeliveryHash names the candidate this call actually
	// delivered, and is set ONLY after the write returned and a re-read of
	// the target hashed to exactly the bytes a one-time authorization grant
	// was spent on.
	//
	// It is not the pool record's `delivered` field, which is the service
	// describing what it selected before anything reached this filesystem.
	// That one is history; this one is a statement about disk, made by the
	// side that wrote it.
	AuthorizedDeliveryHash string `json:"authorized_delivery_hash,omitempty"`

	// Substituted is set when an edit tool's delivery installed bytes other
	// than the caller's own post-edit file. An edit sends a fragment, so this
	// is the only whole-file "what the caller proposed" there is to compare
	// with disk. Not serialised: the agent loop turns it into the handoff.
	Substituted *SupersededWrite `json:"-"`

	// V3 metadata (populated when V3 pipeline was used)
	V3Used               bool                     `json:"v3_used,omitempty"`
	CandidatesTested     int                      `json:"candidates_tested,omitempty"`
	WinningScore         float64                  `json:"winning_score,omitempty"`
	PhaseSolved          string                   `json:"phase_solved,omitempty"`
	VerificationEvidence []V3VerificationEvidence `json:"verification_evidence,omitempty"`

	// DeliveryProvenance says WHERE the bytes that landed came from, from the
	// closed vocabulary in candidate_provenance.go. It is a server-side fact
	// for the terminal, not part of modelFacingResult: the user needs to know
	// whether they are reading the model's own work, a candidate a declared
	// check passed, or one they approved themselves, and the model does not
	// get to argue with the answer.
	DeliveryProvenance string `json:"delivery_provenance,omitempty"`

	// OpenRepair names the files this session left unparseable, with each
	// one's parse error and the lines around it (repairNote). The agent loop
	// sets it on every result while a repair is open; no tool does.
	OpenRepair string `json:"-"`
}

// modelFacingResult is the shape a tool result has in the MODEL's context.
//
// The classification fields are a server-side fact. They exist so the agent
// loop, the gates and the ledger can read what happened instead of inferring
// it from Success -- not so the model can. Serialising them into the
// conversation would put four new keys in front of a model whose behaviour
// was measured without them, and would invite it to argue with a verdict it
// cannot check.
//
// This is an allowlist, not a deny-list: a field added to ToolResult is
// invisible here until someone decides otherwise, which is the direction that
// fails safe. V3 provenance stays because it predates the evidence contract
// and the model has always been told when a candidate replaced its content.
type modelFacingResult struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`

	V3Used               bool                     `json:"v3_used,omitempty"`
	CandidatesTested     int                      `json:"candidates_tested,omitempty"`
	WinningScore         float64                  `json:"winning_score,omitempty"`
	PhaseSolved          string                   `json:"phase_solved,omitempty"`
	VerificationEvidence []V3VerificationEvidence `json:"verification_evidence,omitempty"`

	// OpenRepair is present only while a file this session wrote does not
	// parse: every result says so until it is fixed.
	OpenRepair string `json:"open_repair,omitempty"`
}

// ModelFacing projects a result down to what the model sees. It is the ONLY
// supported way a ToolResult reaches the conversation.
func (r *ToolResult) ModelFacing() modelFacingResult {
	return modelFacingResult{
		Success:              r.Success,
		Data:                 r.Data,
		Error:                r.Error,
		V3Used:               r.V3Used,
		CandidatesTested:     r.CandidatesTested,
		WinningScore:         r.WinningScore,
		PhaseSolved:          r.PhaseSolved,
		VerificationEvidence: r.VerificationEvidence,
		OpenRepair:           r.OpenRepair,
	}
}

// MarshalText returns a compact string representation for the model. The full
// struct still marshals normally for internal use; this is the boundary.
func (r *ToolResult) MarshalText() string {
	b, err := json.Marshal(r.ModelFacing())
	if err != nil {
		return fmt.Sprintf(`{"success":false,"error":"marshal error: %s"}`, err)
	}
	return string(b)
}

type WriteFileInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type WriteFileOutput struct {
	BytesWritten int `json:"bytes_written"`
	// Warning carries a non-blocking defect notice: the write LANDED, and
	// the model should act on this next (e.g. "does not parse — run it and
	// read the traceback"). See writeNewFileWithWarning.
	Warning              string                   `json:"warning,omitempty"`
	V3Used               bool                     `json:"v3_used,omitempty"`
	CandidatesTested     int                      `json:"candidates_tested,omitempty"`
	WinningScore         float64                  `json:"winning_score,omitempty"`
	PhaseSolved          string                   `json:"phase_solved,omitempty"`
	VerificationEvidence []V3VerificationEvidence `json:"verification_evidence,omitempty"`
}

// TerminalStatus is what a session's single terminal event says happened. It
// exists because `summary` is prose, and matching on English ("Stopped: ...",
// "Made your change") is not a contract.
//
// The zero value is TerminalUnknown. A consumer reads it, or an absent value,
// as incomplete, and never reads an unrecognised value as completed. Failing
// closed is the whole point: a run whose outcome cannot be named did not finish.
type TerminalStatus string

const (
	TerminalUnknown    TerminalStatus = "" // unclassified (zero value)
	TerminalCompleted  TerminalStatus = "completed"
	TerminalIncomplete TerminalStatus = "incomplete"
	TerminalStopped    TerminalStatus = "stopped"
	TerminalTimedOut   TerminalStatus = "timed_out"
	TerminalFailed     TerminalStatus = "failed"
)

// Completed is true for exactly TerminalCompleted. Comparing against the
// literal at a call site is what lets an unknown or future value read as
// success.
func (t TerminalStatus) Completed() bool { return t == TerminalCompleted }

// Classified reports whether a producer actually named the outcome.
func (t TerminalStatus) Classified() bool {
	switch t {
	case TerminalCompleted, TerminalIncomplete, TerminalStopped,
		TerminalTimedOut, TerminalFailed:
		return true
	}
	return false
}

// --- evidence provenance ------------------------------------------------------
//
// The envelope already carries how STRONG a verifier was. It does not carry
// who the verifier was, so a model-generated test that ran and a repository
// test that ran arrive indistinguishable. Strength is not trust: the same
// model wrote the code and the test, and on the captured pool 21 of 36 valid
// generated keys disagreed with the task's own reference.
//
// Sources are a closed, source-SPECIFIC vocabulary rather than a
// `trusted=true` boolean, because a boolean cannot say that the proxy's own
// syntax gate may close a syntax obligation and may not close a behavioural
// one -- and that distinction is what decides whether a candidate may land.
//
// The hidden benchmark evaluator has no representation here. Not unused:
// absent, so production cannot name it even by mistake.
const (
	ProvenanceRepoOwnedCheck             = "repo_owned_check"
	ProvenanceClientDeclaredExample      = "client_declared_example"
	ProvenanceClientDeclaredVerification = "client_declared_verification"
	ProvenanceProxyOwnedValidation       = "proxy_owned_validation"
	ProvenanceModelGenerated             = "model_generated"
	ProvenanceLegacy                     = "legacy"
	ProvenanceUnknown                    = "unknown"
)

var provenanceSources = map[string]bool{
	ProvenanceRepoOwnedCheck: true, ProvenanceClientDeclaredExample: true,
	ProvenanceClientDeclaredVerification: true, ProvenanceProxyOwnedValidation: true,
	ProvenanceModelGenerated: true, ProvenanceLegacy: true, ProvenanceUnknown: true,
}

// provenanceCeiling is the strongest obligation each source may ever close.
// A source absent from this map may close nothing -- model_generated, legacy
// and unknown are absent rather than mapped low, because "weak authority" and
// "no authority" are different and only one of them can be raised later by a
// stronger observation.
var provenanceCeiling = map[string]string{
	ProvenanceRepoOwnedCheck:             "oracle",
	ProvenanceClientDeclaredExample:      "oracle",
	ProvenanceClientDeclaredVerification: "oracle",
	ProvenanceProxyOwnedValidation:       "syntax",
}

// CeilingFor is the strongest obligation evidence from source may close, and
// whether that source can close any.
func CeilingFor(source string) (string, bool) {
	ceiling, ok := provenanceCeiling[source]
	return ceiling, ok
}

func StrengthRank(s string) int {
	for i, v := range evidenceStrengthOrder {
		if v == s {
			return i
		}
	}
	return -1
}

// PlanStep is a single step in a Plan. Mirrors v3-service/planning.py's
// PLAN_PROMPT_TEMPLATE shape: id, action, target, why.
type PlanStep struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	Target string `json:"target"`
	Why    string `json:"why"`
}

// Plan is the structured plan returned by /v3/plan. The agent loop
// consults this to gate tool calls (PC plan-adherence) and replays
// VerifyStep at the verification gate.
type Plan struct {
	Steps            []PlanStep `json:"steps"`
	VerifyStep       string     `json:"verify_step"`
	Rationale        string     `json:"rationale"`
	CandidatesTested int        `json:"candidates_tested"`
	WinningScore     float64    `json:"winning_score"`
}

type SSEEvent struct {
	Type string      `json:"type"` // "tool_call", "tool_result", "text", "done", "permission_request", "error"
	Data interface{} `json:"data"`
}

type PermissionRequest struct {
	ToolName   string          `json:"tool_name"`
	Args       json.RawMessage `json:"args"`
	Message    string          `json:"message"`      // human-readable description
	ToolCallID string          `json:"tool_call_id"` // echoed back on POST /v1/permission

	// Additive, and populated only for a deletion, which is confirmed against
	// an inspected target rather than against a call. A client that does not
	// know these fields reads exactly the event it always did; one that does
	// can show the user what is about to be removed and refuse to offer a
	// session-wide answer. Every value is structured -- a resolved path, a
	// stat, a hash -- never prose.
	CanonicalPath string `json:"canonical_path,omitempty"`
	TargetType    string `json:"target_type,omitempty"`
	ContentSHA256 string `json:"content_sha256,omitempty"`
	OneTimeOnly   bool   `json:"one_time_only,omitempty"`
}

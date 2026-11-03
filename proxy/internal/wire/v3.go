package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// The versioned evidence envelope, as the V3 service serialises it.
//
// This part is TRANSPORT AND VALIDATION ONLY. The domain lives in the
// service's contract.py: what closure means, how coverage is computed, how
// records compare. Nothing here re-derives any of it, and nothing here reads
// `passed`, `phase_solved`, `winning_score` or the verification-evidence list
// to guess a strength — those carry no strength, which is the whole reason
// this envelope exists.
//
// It is also kept apart from the local ToolResult validation fields. Those say
// what THIS process checked about the bytes it wrote (syntax, structural).
// The envelope says what the SERVICE demonstrated about a candidate. Merging
// them would let a local syntax pass read as behavioural evidence, which is
// the conflation the whole workstream exists to remove.

// V3GenerateRequest is sent to the Python V3 service for arbitrary file generation.
type V3GenerateRequest struct {
	FilePath       string            `json:"file_path"`
	BaselineCode   string            `json:"baseline_code"`
	ProjectContext map[string]string `json:"project_context,omitempty"`
	Framework      string            `json:"framework,omitempty"`
	BuildCommand   string            `json:"build_command,omitempty"`
	Constraints    []string          `json:"constraints,omitempty"`
	// UserMessage is what the user actually asked for. Without it the
	// pipeline is handed "Create the file `solve.py`", the project context
	// and the baseline, and told to improve on the baseline "preserving all
	// functionality" — so it can only mimic a draft whose requirement it has
	// never seen, and cannot correct one that is subtly wrong.
	UserMessage string `json:"user_message,omitempty"`
	Tier        int    `json:"tier"`
	WorkingDir  string `json:"working_dir,omitempty"`
	// BudgetMs is the wall-clock cap the bridge applies to this call, set by
	// v3client.GenerateStreaming and never by a caller. The service plans its
	// phases against it: planning against ATLAS_V3_TIMEOUT instead, while the
	// bridge may cut the call at half of the session's remaining time, makes
	// late phases start work the bridge then abandons. Omitted when the call
	// is uncapped.
	BudgetMs int64 `json:"budget_ms,omitempty"`
}

// V3GenerateResponse is the response from the V3 service.
type V3GenerateResponse struct {
	Code                 string                   `json:"code"`
	Passed               bool                     `json:"passed"`
	PhaseSolved          string                   `json:"phase_solved"`
	CandidatesTested     int                      `json:"candidates_tested"`
	WinningScore         float64                  `json:"winning_score"`
	TotalTokens          int                      `json:"total_tokens"`
	TotalTimeMs          float64                  `json:"total_time_ms"`
	VerificationEvidence []V3VerificationEvidence `json:"verification_evidence,omitempty"`

	// The versioned evidence envelope (evidence_wire.go). Declared explicitly
	// because an undeclared field is silently discarded on decode, with no
	// error anywhere. nil means the producer sent none — a legacy service,
	// or a run that measured nothing — which is not the same as evidence that
	// arrived and cannot be trusted. See EvidenceAvailability.
	//
	// Nothing in it is derived from Passed/PhaseSolved/WinningScore, and none
	// of those may be derived from it.
	Evidence *V3EvidenceEnvelope `json:"evidence,omitempty"`
	// Why the producer sent no envelope, when it knows. Travels beside the
	// absence so a gap is visible rather than silent.
	EvidenceUnavailableReason string `json:"evidence_unavailable_reason,omitempty"`
	// LensUnavailable is set, with the reason, when the lens could not score
	// during this call. The run stops on it (lens_required.go); the result
	// carries no candidate.
	LensUnavailable string `json:"lens_unavailable,omitempty"`
}

// V3VerificationEvidence describes the concrete verifier that accepted or
// rejected a V3 candidate. It is intentionally small and bounded by the V3
// service before crossing the wire.
type V3VerificationEvidence struct {
	Verifier   string `json:"verifier"`
	Command    string `json:"command,omitempty"`
	Status     string `json:"status"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	DurationMs int    `json:"duration_ms,omitempty"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
}

// evidenceWireMajor is the envelope contract this build understands. A
// same-major envelope may add fields; a different major may have changed the
// meaning of one, so it is not interpreted at all.
const evidenceWireMajor = "1"

// EvidenceAvailability is the only thing a consumer may branch on before
// reading any field. Three states, never two: an envelope that was not sent
// and one that cannot be trusted are different facts, and neither is a
// failure of the candidate.
type EvidenceAvailability string

const (
	// EvidenceAbsent: no envelope was sent. A legacy producer, or a run that
	// measured nothing.
	EvidenceAbsent EvidenceAvailability = "absent"
	// EvidenceUnavailable: an envelope arrived and cannot be trusted —
	// unknown version, incomplete identity, internally contradictory. NEVER
	// "failed": nothing about the candidate was demonstrated either way.
	EvidenceUnavailable EvidenceAvailability = "unavailable"
	// EvidenceAvailable: structurally valid and internally consistent. Says
	// nothing yet about whether it describes the bytes being delivered.
	EvidenceAvailable EvidenceAvailability = "available"
)

type V3EvidenceIdentity struct {
	ContractID            string `json:"contract_id"`
	ContractVersion       string `json:"contract_version"`
	AdapterID             string `json:"adapter_id"`
	AdapterVersion        string `json:"adapter_version"`
	CalibrationID         string `json:"calibration_id"`
	ArtifactScope         string `json:"artifact_scope"`
	EvaluationContextHash string `json:"evaluation_context_hash"`
	CandidateContentHash  string `json:"candidate_content_hash"`
}

type V3EvidenceQuality struct {
	RequiredCoverage float64 `json:"required_coverage"`
	OptionalQuality  float64 `json:"optional_quality"`
	Overall          float64 `json:"overall"`
}

type V3EvidenceEvaluation struct {
	ExecutionStatus      string            `json:"execution_status"`
	Supported            bool              `json:"supported"`
	EvidenceStrength     string            `json:"evidence_strength"`
	RequirementsComplete bool              `json:"requirements_complete"`
	ClosureEligible      bool              `json:"closure_eligible"`
	Quality              V3EvidenceQuality `json:"quality"`
}

// V3EvidenceOptional is one non-required criterion's observation. Ids are
// opaque; this side never interprets them.
type V3EvidenceOptional struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type V3EvidenceCoverage struct {
	Required     []string             `json:"required"`
	Demonstrated []string             `json:"demonstrated"`
	Missing      []string             `json:"missing"`
	Unmeasurable []string             `json:"unmeasurable"`
	Optional     []V3EvidenceOptional `json:"optional"`
}

type V3EvidenceSelection struct {
	Status            string `json:"status"`
	Reason            string `json:"reason"`
	TiedCount         int    `json:"tied_count"`
	IncomparableCount int    `json:"incomparable_count"`
	IneligibleCount   int    `json:"ineligible_count"`
}

type V3EvidenceDelivery struct {
	DeliveredContentHash string `json:"delivered_content_hash"`
	// The producer's own reading. Never trusted on its own: DescribesBytes
	// recomputes the hash of what is actually about to be written.
	DescribesDeliveredCandidate bool `json:"describes_delivered_candidate"`
}

type V3EvidenceEnvelope struct {
	WireVersion         string               `json:"wire_version"`
	RecordSchemaVersion string               `json:"record_schema_version"`
	Identity            V3EvidenceIdentity   `json:"identity"`
	Evaluation          V3EvidenceEvaluation `json:"evaluation"`
	Coverage            V3EvidenceCoverage   `json:"coverage"`
	Selection           V3EvidenceSelection  `json:"selection"`
	Delivery            V3EvidenceDelivery   `json:"delivery"`

	// Provenance says WHO produced the evidence and WHAT it is about.
	// Additive and omitted when absent: a producer that does not send it
	// emits the same envelope as before, and this build reads that envelope
	// the same way.
	Provenance *V3EvidenceProvenance `json:"provenance,omitempty"`
}

var evidenceStrengthOrder = []string{"syntax", "runtime", "behavioral", "oracle"}

// StrongestStrength is the top of the evidence strength order.
func StrongestStrength() string {
	return evidenceStrengthOrder[len(evidenceStrengthOrder)-1]
}

// V3EvidenceProvenance is where a piece of evidence came from and what it is
// about. It carries hashes and identities; candidate bytes stay where they
// already are and never enter this structure or a log line.
type V3EvidenceProvenance struct {
	Source string `json:"source"`

	// What the evidence is about. Every one must match before the evidence
	// may be used for the thing being asked about.
	RequestID string `json:"request_id"`
	// RouteEntryID is the entry of the candidate-generation route this
	// evidence belongs to, and it is what a reader groups on. InvocationID
	// answers a different question -- which candidate evidence was minted --
	// and is never a substitute: a record that has the first and honestly
	// lacks the second is still attributable.
	// Deliberately untagged: it is written into telemetry by name and never
	// decoded from any inbound payload, so nothing off the wire can supply it.
	RouteEntryID        string `json:"-"`
	InvocationID        string `json:"invocation_id"`
	CandidateInstanceID string `json:"candidate_instance_id"`
	CandidateHash       string `json:"candidate_hash"`
	WorkspaceGeneration int    `json:"workspace_generation"`
	WorkspaceStateHash  string `json:"workspace_state_hash"`
	// Absent for an obligation that runs no command. Absence is a real
	// answer, and two bindings must still agree on it.
	CommandIdentity string `json:"command_identity,omitempty"`
	// BaselineIdentity names the validated baseline the candidate would
	// replace, or "" when there is none. Absence is a real answer here too:
	// evidence earned against one baseline is not about a different one, and
	// evidence earned where nothing existed is not about a replacement.
	BaselineIdentity string `json:"baseline_identity,omitempty"`

	ObligationID     string `json:"obligation_id"`
	RequiredStrength string `json:"required_strength"`
	ObservedStrength string `json:"observed_strength"`
}

// MayAuthorize reports whether this evidence COULD close its obligation, on
// source and strength alone. It says nothing about which candidate the
// evidence is about: that is BindsTo, and both must hold.
func (p *V3EvidenceProvenance) MayAuthorize() (bool, string) {
	if p == nil {
		return false, "no provenance"
	}
	if !provenanceSources[p.Source] {
		return false, "unknown evidence source " + p.Source
	}
	ceiling, ok := provenanceCeiling[p.Source]
	if !ok {
		return false, p.Source + " evidence never authorizes an obligation"
	}
	for _, f := range []struct{ name, value string }{
		{"request_id", p.RequestID}, {"invocation_id", p.InvocationID},
		{"candidate_instance_id", p.CandidateInstanceID},
		{"candidate_hash", p.CandidateHash},
		{"workspace_state_hash", p.WorkspaceStateHash},
		{"obligation_id", p.ObligationID},
	} {
		if strings.TrimSpace(f.value) == "" {
			return false, f.name + " is missing"
		}
	}
	if p.WorkspaceGeneration < 0 {
		return false, "workspace_generation is negative"
	}
	req, obs := StrengthRank(p.RequiredStrength), StrengthRank(p.ObservedStrength)
	if req < 0 {
		return false, "unknown required strength " + p.RequiredStrength
	}
	if obs < 0 {
		return false, "unknown observed strength " + p.ObservedStrength
	}
	if StrengthRank(ceiling) < req {
		return false, p.Source + " may not establish " + p.RequiredStrength +
			" strength (its ceiling is " + ceiling + ")"
	}
	if obs < req {
		return false, "observed strength " + p.ObservedStrength +
			" is below the required " + p.RequiredStrength
	}
	return true, ""
}

// BindsTo reports whether this evidence is about the thing `asked` describes.
// One candidate may not borrow another's evidence, one invocation may not
// borrow another's, and evidence observed against an earlier workspace
// generation is about a workspace that no longer exists: a later mutation,
// move or recreation bumps the generation and invalidates it.
func (p *V3EvidenceProvenance) BindsTo(asked V3EvidenceProvenance) (bool, string) {
	if p == nil {
		return false, "no provenance"
	}
	for _, f := range []struct {
		name       string
		held, want string
	}{
		{"request_id", p.RequestID, asked.RequestID},
		{"invocation_id", p.InvocationID, asked.InvocationID},
		{"candidate_instance_id", p.CandidateInstanceID, asked.CandidateInstanceID},
		{"candidate_hash", p.CandidateHash, asked.CandidateHash},
		{"workspace_state_hash", p.WorkspaceStateHash, asked.WorkspaceStateHash},
		{"command_identity", p.CommandIdentity, asked.CommandIdentity},
		{"baseline_identity", p.BaselineIdentity, asked.BaselineIdentity},
		{"obligation_id", p.ObligationID, asked.ObligationID},
	} {
		if f.held != f.want {
			return false, f.name + " differs: evidence is about " + f.held +
				", not " + f.want
		}
	}
	if p.WorkspaceGeneration != asked.WorkspaceGeneration {
		return false, "workspace_generation differs: evidence is about an earlier workspace"
	}
	return true, ""
}

// V3PlanRequest is sent to the Python V3 service for plan generation.
// project_context inlines small file contents (truncated server-side) so
// the planner sees what's actually in the working directory.
type V3PlanRequest struct {
	UserMessage    string            `json:"user_message"`
	WorkingDir     string            `json:"working_dir,omitempty"`
	ProjectContext map[string]string `json:"project_context,omitempty"`
	// ExistingFiles is every path already in the workspace, relative. The
	// planner runs in v3-service, which has no /workspace mount, so it cannot
	// look for itself — and project_context only carries a handful of
	// priority files by content. Without this the planner cheerfully opens
	// with "write_file input.txt — create the necessary input data" against a
	// fixture already on disk.
	ExistingFiles []string `json:"existing_files,omitempty"`
	NCandidates   int      `json:"n_candidates,omitempty"` // 0 → server default (3)
}

// Closed vocabularies. An unknown value is not a new case to guess at — it
// means this build and that producer disagree about what the field can say.
var (
	evidenceStrengths = map[string]bool{
		"syntax": true, "runtime": true, "behavioral": true, "oracle": true,
	}
	evidenceExecutionStatuses = map[string]bool{
		"ok": true, "timeout": true, "error": true, "crash": true, "skipped": true,
	}
	evidenceSelectionStatuses = map[string]bool{
		"verified_winner": true, "best_not_closure_eligible": true, "tied": true,
		"incomparable": true, "ineligible": true, "no_verified_winner": true,
	}
)

// Validate reports whether the envelope may be read, and why not when it may
// not. It is strict on purpose: every rejection here is a case where reading
// on would mean asserting something the producer did not establish.
func (e *V3EvidenceEnvelope) Validate() (EvidenceAvailability, string) {
	if e == nil {
		return EvidenceAbsent, "no evidence envelope"
	}
	if why := e.versionProblem(); why != "" {
		return EvidenceUnavailable, why
	}
	if why := e.identityProblem(); why != "" {
		return EvidenceUnavailable, why
	}
	if why := e.vocabularyProblem(); why != "" {
		return EvidenceUnavailable, why
	}
	if why := e.contradiction(); why != "" {
		return EvidenceUnavailable, why
	}
	return EvidenceAvailable, ""
}

func (e *V3EvidenceEnvelope) versionProblem() string {
	if e.WireVersion == "" {
		return "envelope carries no wire version"
	}
	if strings.SplitN(e.WireVersion, ".", 2)[0] != evidenceWireMajor {
		return "unsupported wire version " + e.WireVersion
	}
	return ""
}

// identityProblem holds the producer's own rule: a partial identity is how
// unrelated measurements compare equal through blanks.
func (e *V3EvidenceEnvelope) identityProblem() string {
	id := e.Identity
	if id.ContractID == "" || id.ContractVersion == "" || id.ArtifactScope == "" ||
		id.EvaluationContextHash == "" {
		return "identity incomplete"
	}
	if id.CalibrationID == "" && (id.AdapterID == "" || id.AdapterVersion == "") {
		return "identity incomplete: no calibration or adapter"
	}
	return ""
}

func (e *V3EvidenceEnvelope) vocabularyProblem() string {
	if !evidenceStrengths[e.Evaluation.EvidenceStrength] {
		return "unknown evidence strength " + e.Evaluation.EvidenceStrength
	}
	if !evidenceExecutionStatuses[e.Evaluation.ExecutionStatus] {
		return "unknown execution status " + e.Evaluation.ExecutionStatus
	}
	if !evidenceSelectionStatuses[e.Selection.Status] {
		return "unknown selection status " + e.Selection.Status
	}
	return ""
}

// contradiction finds a claim the envelope's own fields deny. A producer that
// claims closure over a run that did not complete, or over unmet requirements,
// is a producer whose other fields cannot be relied on either.
func (e *V3EvidenceEnvelope) contradiction() string {
	if e.Evaluation.ClosureEligible {
		if e.Evaluation.ExecutionStatus != "ok" {
			return "closure claimed over execution status " + e.Evaluation.ExecutionStatus
		}
		if !e.Evaluation.RequirementsComplete {
			return "closure claimed with incomplete requirements"
		}
		if !e.Evaluation.Supported {
			return "closure claimed for an unsupported artifact"
		}
	}
	if e.Selection.Status == "verified_winner" && !e.Evaluation.ClosureEligible {
		return "verified winner without closure eligibility"
	}
	return ""
}

// Available is the single predicate a caller may use before reading fields.
func (e *V3EvidenceEnvelope) Available() bool {
	a, _ := e.Validate()
	return a == EvidenceAvailable
}

// DescribesBytes reports whether this evidence is about exactly `code`. The
// producer's own describes_delivered_candidate flag is not consulted: the
// consumer knows which bytes it is about to write and hashes those.
func (e *V3EvidenceEnvelope) DescribesBytes(code string) bool {
	if e == nil || e.Identity.CandidateContentHash == "" {
		return false
	}
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:]) == e.Identity.CandidateContentHash
}

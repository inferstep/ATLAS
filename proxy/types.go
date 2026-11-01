package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Tier extensions — Tier, Tier0-3 constants, and String() already in main.go
// ---------------------------------------------------------------------------

// TierMaxTurns returns the maximum agent loop iterations for this tier.
// Tool-using tiers (T1/T2/T3) are uncapped: the stuck-pattern detectors
// (parse-error, tool-repeat, reasoning-repeat, lens-regression,
// exploration-budget, path-aware error-loop, action gate, verification
// gate) are the safety net, and a numeric turn cap cuts legitimate
// multi-file long-form work more often than it catches a real runaway.
// User-initiated cancellation (ctx.Ctx) is the upper bound. T0 keeps a
// small cap as a SHAPE constraint (conversational input shouldn't loop),
// not as runaway protection.
//
// MaxTurns == 0 is the agent loop's "uncapped" sentinel. ATLAS_MAX_TURNS
// overrides it: any positive int caps every tier; unset or 0 keeps the
// tier defaults.

func TierMaxTurns(t Tier) int {
	if n := envOverrideMaxTurns(); n > 0 {
		return n
	}
	switch t {
	case Tier0Conversational:
		// A question about the code is classified conversational and still has
		// to LOOK at the code: a few turns can go to a bounced text exit and to
		// searches before the right file is open, and a tighter cap leaves the
		// user with no answer at all. The cap is here to stop a conversational
		// request looping, and 12 still does that while leaving room to read a
		// few files and reply.
		return 12
	case Tier1Simple, Tier2Medium, Tier3Hard:
		return 0 // uncapped
	}
	return 0
}

// envOverrideMaxTurns reads ATLAS_MAX_TURNS. Returns:
//   - n > 0  → use n (no upper cap — operator's call)
//   - n == 0 / unset / invalid → 0 (caller falls through to tier defaults)
func envOverrideMaxTurns() int {
	raw := os.Getenv("ATLAS_MAX_TURNS")
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// ---------------------------------------------------------------------------
// Agent messages — the conversation between model and tool executor
// ---------------------------------------------------------------------------

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
	Substituted *supersededWrite `json:"-"`

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

// ---------------------------------------------------------------------------
// Tool input/output types
// ---------------------------------------------------------------------------

// -- write_file --

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

// ---------------------------------------------------------------------------
// Agent context — shared state for the agent loop
// ---------------------------------------------------------------------------

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

// AgentContext holds all state for a single agent loop execution.
type AgentContext struct {
	// Configuration
	Tier           Tier
	MaxTurns       int
	WorkingDir     string // Project directory for agent operations (container path, e.g. /workspace)
	HostWorkingDir string // The host-side path that's bind-mounted as WorkingDir
	// (e.g. /home/isaac/snake when /workspace is mounted from there).
	// Used to translate absolute host paths the model receives back from
	// the user's prompt — e.g. "fix /home/isaac/snake/app.py" — into
	// container paths the proxy can actually open. Empty when the proxy
	// runs without a bind mount (dev / test).
	PermissionMode PermissionMode
	YoloMode       bool

	// approvedDelete is the single outstanding user approval for a deletion,
	// bound to one canonical target in one exact state. It is set only by the
	// permission handshake and consumed by the first execution attempt, so an
	// answer about one file can never authorise another. Nil means no
	// deletion is approved, which is the state a session spends almost all of
	// its life in.
	approvedDelete *approvedDeletion

	// deletionAttempts and fulfilledDeletions are the record of destructive
	// work the USER authorised and the system then carried out. An attempt is
	// written only by the delete tool, on the exact route where a consumed
	// approval survived revalidation and os.Remove succeeded; it is promoted
	// only by the ledger, which is the one place that both confirms absence
	// and writes the tombstone. Nothing the model emits reaches either map.
	// lastDeleteCallID is the tool-call id the outstanding approval was
	// granted under, kept so the fulfilled record can name the exact call the
	// user answered.
	lastDeleteCallID string

	deletionAttempts   map[string]*deletionAttempt
	fulfilledDeletions map[string]*fulfilledDeletion

	// TaskContract is what the client declared about this request, validated
	// at the request boundary (validateTaskContract). Nil means the client
	// declared nothing, which stays distinct from declaring an empty
	// contract.
	TaskContract *TaskContract

	// AllowedTools names tools the client has pre-approved for this session
	// (seeded from the request's session_allowed_tools) plus any the user
	// approves with session scope during this turn. A named tool skips the
	// interactive permission prompt. Guarded by mu.
	AllowedTools map[string]bool

	// Service URLs
	InferenceURL string
	SandboxURL   string
	LensURL      string
	V3URL        string

	// DisableFreshSlot skips the slot-erase at the start of the
	// agent loop so the demo's pre-warm pass actually survives into the
	// real run. Without this, the prefix cache the warmup builds is
	// wiped on the next /v1/agent call and the demo pays the 25-second
	// cold-start cost twice. Only set this from controlled flows
	// (`/demo`, tests) — production sessions want the per-session KV
	// isolation.
	DisableFreshSlot bool

	// Project info (populated by project detection)
	Project *ProjectInfo

	// State
	Messages []AgentMessage
	// PriorHistory is the prior-turn user/assistant transcript, sent by
	// the TUI on each /v1/agent request so the agent can answer follow-ups
	// like "what did you just delete?" — without it, every user message
	// is a fresh agent loop with empty context. Populated by handleAgent
	// from the request body; consumed once at the top of runAgentLoop
	// and then ignored. Tool/system rows are filtered out at the TUI
	// boundary; only role=user|assistant text turns flow through here.
	PriorHistory  []AgentMessage
	FileReadTimes map[string]time.Time // for staleness detection
	FilesRead     map[string]string    // cache of read file contents
	TotalTokens   int

	// VerificationEvidence holds one record per verification run, binding
	// the result to what the command actually exercised: the command line,
	// its stdin contract, and the sha256 of each covered session-written
	// file at that moment. A failed run is recorded too and takes back an
	// earlier pass over the same bytes. Lens labels are drawn from these
	// records, never from a session-wide boolean: a flag can't answer
	// "verified WHAT, at WHICH bytes", and labelling on it would train the
	// lens on files the passing command never touched.
	VerificationEvidence []VerificationRecord

	// StagedCommands are declared commands that ran against exact candidate
	// bytes in the staging overlay, kept in their own store rather than
	// flattened into the record above. That record can say which paths a run
	// named; it cannot say which candidate, invocation or grant a run belongs
	// to, and those are exactly what makes a staged execution worth trusting.
	StagedCommands []stagedCommandFulfillment

	// routeEntrySeq numbers the entries this request has made into the
	// candidate-generation route. One write_file call that reaches the route
	// is one entry, and a retry is a new one -- see routeEntry.
	routeEntryMu  sync.Mutex
	routeEntrySeq int

	// grants holds the one-time authorization grants this request has
	// minted, keyed by canonical grant id. A grant is spent exactly once at
	// the delivery owner; nothing else may read or clear one.
	//
	// Guarded by grantMu, which is its own lock rather than LedgerMu: a
	// consumption re-reads the ledger while it decides, and one mutex for
	// both would be a self-deadlock the first time that happened.
	grantMu   sync.Mutex
	grants    map[string]*authorizationGrant
	grantSeq  int
	grantsOff string
	// lensDown is why the lens stopped scoring during this run, set by
	// noteLensDown; the agent loop ends the run when it is not empty.
	lensDown string
	// v3Unchecked holds the writes that landed after V3 failed on them (ran
	// out of time or was unavailable), keyed by ledger key. The final summary
	// names each one whose bytes are still on disk (v3_fallback_note.go).
	v3Unchecked   map[string]v3UncheckedWrite
	v3UncheckedMu sync.Mutex

	// settlements holds what each authorized delivery left behind, keyed by
	// canonical target. Written only by the delivery owner, and only for a
	// delivery whose exact authorized bytes were confirmed on disk. Guarded
	// by grantMu, beside the licence that produced it.
	settlements map[string]*deliverySettlement

	// HumanTask is the CURRENT request's actual human instruction, captured
	// once at the top of runAgentLoop before the loop appends anything.
	// ATLAS represents internal correctives, manifests and re-injected file
	// content as user-role messages for chat-template compatibility, so
	// "last user message" stops meaning "what the human asked" the moment
	// the first [system note] lands — V3 was observed generating against
	// "run the program standalone" instead of the task (third-party audit
	// finding). Role conversion is a serialization concern; this field
	// preserves the provenance the role field erases.
	HumanTask string

	// LiteralBlocks are the byte-exact content contracts extracted from
	// HumanTask (fenced blocks, "exactly ...:" lines). Landed writes are
	// verified against these and whitespace-only drift is repaired
	// mechanically — the model plans, the harness copies. See
	// extractLiteralBlocks / repairLiteralDrift in guardrails_write.go.
	LiteralBlocks []string

	// FencedCalls / FencedTokens account the @fenced sub-calls
	// (fetchFencedContent): every attempt is a full model generation, and
	// leaving it out makes a session's reported token count short by one
	// whole generation per written file. FencedTokens is also folded into
	// TotalTokens; these two exist so the fenced share is visible on its own.
	FencedCalls  int
	FencedTokens int

	// BodySeen records the files whose CONTENTS were put in front of the
	// model. FilesRead answers a different question: outline_file caches a
	// file's full source for staleness tracking while showing the model only
	// signatures and line ranges, so a path sits in FilesRead having never
	// been displayed. read_file already draws this distinction the same way,
	// recording only the head of a truncated read because the head is all the
	// model saw.
	//
	// Measured on a diagnostic question over three files: the model outlined
	// scoring.py, received symbol names with line ranges, and answered "line
	// 142 tries to access keys like..." about lines it had never been shown.
	// Across 12 sessions it never once read a body before diagnosing, and the
	// answer was decided entirely by which filename it guessed: scoring.py
	// wrong 11/11, planning.py right 1/1.
	BodySeen map[string]bool

	// BodyShown records WHICH lines a read showed, where BodySeen records only
	// that some were. A truncated or ranged read shows part of a file, and a
	// claim about code outside that part is as much a guess as a claim about a
	// file never opened. A file with no entry, or a whole one, counts as
	// shown.
	BodyShown map[string]*bodyCoverage

	// SessionWrites tracks files this agent loop wrote during this run. The
	// write_file guard rejects overwrites of existing files over 5 lines,
	// to protect the user's code from clobbering. A file the agent itself
	// wrote in this session is NOT the user's code; it is the agent's own
	// working output, and the agent must be allowed to iterate on it:
	// without this, a model that realizes mid-loop that its own app.py was
	// a stub cannot rewrite it.
	//
	// Keyed on the RAW path the model supplied, so "solve.py" and
	// "./solve.py" are two entries for one file and the overwrite guard can
	// refuse the agent's own output under a second spelling. Recorded here
	// rather than changed: every consumer reads these keys, and
	// canonicalising them is a behavioural change with its own evidence to
	// gather. The ledger keys on the resolved path instead, so the two do
	// not share this defect.
	SessionWrites map[string]bool

	// FencedFailures counts CONSECUTIVE zero-byte fenced resolutions per
	// target path, for the life of the session. It lives here and not in
	// fetchFencedContent because that function's attempt counter is a local:
	// a new write_file call re-entered it with a fresh budget, so a path that
	// had already burned two ~300s attempts could burn two more on the next
	// turn. Keyed by path so one file's failures cannot exhaust another's
	// allowance. A successful resolution clears the entry.
	FencedFailures map[string]int

	// FencedStalls counts fenced sub-calls cut with no content this session,
	// across ALL paths (unlike FencedFailures, which is per-path). The stall
	// is a property of the session's llama-server state, not the file: once
	// one fenced fetch opens a stream that then says nothing, the next file's
	// fetch stalls the same way. After the first stall the channel is turned
	// off session-wide (fencedChannelDisabledForSession) and writes go inline,
	// which the swallowed-content detector now makes safe.
	FencedStalls int
	// LastFencedStream is what the wire showed during the last fenced
	// sub-call, for the fenced_fetch event its attempt emits.
	LastFencedStream fencedStreamStats

	// RequestCtx is the RESPONSE lifetime: alive for finalisation after the
	// work context has been cancelled. Ctx is the WORK lifetime -- LLM calls,
	// tools, gates, V3 and the sandbox all hang off it -- and it ends one
	// reserve before the session budget does.
	RequestCtx context.Context
	cancelWork context.CancelFunc

	// The session's single terminal outcome, set by the one emitter. Read by
	// the deferred broker envelope so the two cannot disagree.
	TerminalStatus TerminalStatus
	TerminalReason string
	// InitialWorkspace is what the workspace held when this request started:
	// the files a shell command's removal can take from the user
	// (applyShellChanges). ShellEffectsUnobserved says some command's changes
	// could not all be seen, because the workspace outgrew the walk.
	InitialWorkspace       workspaceSnapshot
	ShellEffectsUnobserved bool
	// BackgroundBaseline is the walk taken after the call that started the
	// first background job still live. What jobs changed is recorded against
	// it once none can still be writing (settleBackgroundEffects).
	BackgroundBaseline workspaceSnapshot

	// TerminalUnresolved names the exit gates that spent their bounces with
	// their finding still true, comma-separated; empty when there were none.
	TerminalUnresolved string
	terminalOnce       sync.Once

	// Ledger is the record of what this session has done to each declared
	// or session-owned deliverable, keyed by CANONICAL workspace-relative
	// path.
	//
	// It exists because at a stall the server otherwise knows only which
	// paths were touched, not whether the current bytes are valid or what
	// the last good version was. ValidationStatus never crosses the event
	// boundary, so without the ledger a finaliser has nothing true to say
	// and nothing safe to leave behind. Restoration, the completion check
	// and delivery grants read it.
	Ledger   map[string]*DeliverableState
	LedgerMu sync.Mutex

	// WorkspaceHazards is the set of background work that may still be
	// writing, keyed by job identity rather than counted by start attempt, so
	// a job's confirmed exit clears only its own hazard and a start refused
	// before dispatch raises none. run_background raises it. A signal alone
	// does not clear it, because a signalled process may still be flushing:
	// only a confirmed exit (a reaped exit code) clears it, and tracked paths
	// must be rehashed afterwards. A dispatch that came back with no job id
	// raises hazardUnidentifiedJob, which nothing can clear.
	WorkspaceHazards map[string]bool

	// ManifestAnnounced tracks which SessionWrites paths have been named
	// in a session-file-manifest note (context.go) so each file
	// is announced to the model once.
	ManifestAnnounced map[string]bool

	// BackgroundJobs maps job_id -> command for jobs this loop started and
	// has not stopped. A background server the model started to verify its
	// own work keeps holding the port, so the model's next `python app.py`
	// fails with "address already in use" — naming a program it cannot see.
	// An observed session spent its remaining turns on that conflict.
	// Tracked so the run_command failure can say which of the model's own
	// jobs is holding the port.
	BackgroundJobs map[string]string
	// BackgroundJobStarted records when each job was started, so evidence can
	// tell a process that predates the last edit from one that runs the bytes
	// on disk now.
	BackgroundJobStarted map[string]time.Time

	// AssetLintSeen dedupes asset-graph lint findings (gates.go) so
	// a persistent orphan is mentioned once, not after every write.
	AssetLintSeen map[string]bool

	// PassID identifies this prompt-pass (the session id from the request).
	PassID string

	// Rolling list of gx_score_min values
	// from lens scoring of write_file/edit_file tool calls. When the
	// recent N values all fall below the selected model's calibrated threshold the loop
	// injects a corrective system message before the next LLM call.
	// See proxy/lens.go for the pattern detection.
	LensScoreHistory []float64

	// Tool-call repetition detector: rolling window of recent (tool,
	// args) signatures. When the same signature appears
	// toolRepeatThreshold times within the last toolRepeatWindow
	// entries, the loop injects a corrective system message. Owned by
	// the detector — recordToolCall appends to it and clears it as it
	// fires; callers go through resetToolRepeatWindow rather than
	// assigning here. See proxy/detectors.go for the detection logic.
	RecentToolCalls []string

	// LastReadPath is the file most recently returned by read_file, restated
	// near the generation point so the model copies from close range.
	LastReadPath string

	// AppliedEdits keys every edit_file that already succeeded, by
	// (path, old_str, new_str). A model that re-applies an identical edit has
	// lost track of its own work, and when the edit is whitespace-only the
	// original old_str still matches afterwards, so it can apply forever.
	// Observed live: the same `'PAUSED';` -> `' PAUSED';` edit ran twice,
	// 1m25s and 1m15s, each adding another space.
	AppliedEdits map[string]bool

	// FailedToolCalls keys every tool call that has already been REJECTED,
	// by the same (name, args) signature the repetition detector uses.
	// Re-sending a byte-identical call whose last run failed cannot produce
	// a different result — the harness is deterministic — so the second one
	// is refused before it executes rather than nudged afterwards.
	FailedToolCalls map[string]string

	// LastRejectionClass is the skeleton of the previous failure's message.
	// The error-loop breaker resets when a new failure differs from it: three
	// DIFFERENT rejections is a model converging, not one looping.
	LastRejectionClass string

	// LastStreamCut records why the proxy itself ended the generation, when
	// it did. A cut lands mid-JSON, so the parse then fails; the parse-failure
	// classifier reads this known cause instead of inferring one from the
	// broken output.
	LastStreamCut string

	// LastOldStrCut is what the old_str watch saw when LastStreamCut is
	// "old_str_unmatched": the file, and where old_str stopped matching it.
	LastOldStrCut *oldStrWatch

	// OriginalContent is each touched file as the run FIRST saw it. FilesRead
	// is overwritten on every edit, so it cannot answer "what did this run
	// change".
	OriginalContent map[string]string

	// Reasoning-repetition detector state. Per-turn snapshot of the
	// model's reasoning_content stream. When the same opening prose
	// ("Now I need to look at the file" or similar) appears across
	// consecutive turns, the loop injects a corrective so the model
	// breaks out of the thought loop. The streak fields are owned by the
	// detector: recordReasoning clears them as it fires and hands the
	// count and snippet back in its repeatObservation. See
	// proxy/detectors.go.
	LastTurnReasoning           string
	LastReasoningSnippet        string
	ConsecutiveReasoningRepeats int

	// Path-aware error-loop detector. Tracks recent failure paths so
	// the 3-consecutive-failures breaker can distinguish "stuck on one
	// file" (real loop, stop) from "grinding through different files,
	// some succeed, some fail" (progress, keep going). Cleared on any
	// successful tool result. See the error-loop break in
	// proxy/loop_results.go for the use site.
	RecentFailurePaths []string

	mu sync.Mutex

	// Plan is the optional pre-flight plan produced by /v3/plan. Set
	// once at the top of the agent loop for non-trivial requests; nil
	// when we skipped planning (T0, simple greetings, dev mode without
	// V3). Read by the plan-adherence gate to compare actual tool calls
	// against the planned step actions.
	Plan *Plan

	// PlanStepsSatisfied[i] flips true once a tool call has matched
	// plan step i. Length tracks len(Plan.Steps); nil when no plan.
	// Reset whenever the plan is revised so we re-track from scratch.
	PlanStepsSatisfied []bool

	// PlanOffStreak counts consecutive tool calls that DIDN'T match
	// any unsatisfied plan step. Crosses the auto-revise threshold ->
	// planner re-runs with whatever context we've discovered so far.
	PlanOffStreak int

	// PlanRevisions counts how many times we've auto-regenerated the
	// plan in this loop. Capped to keep us from thrashing — after the
	// cap is hit we stop revising and let the agent run plan-free.
	PlanRevisions int

	// VerifyOnHost flips run_command from sandbox-routing to local
	// host execution. Set only from ATLAS_VERIFY_IN=host together with
	// ATLAS_TRUST_MODE=fully-trusted; a workspace's .atlas/config.toml
	// may choose only the sandbox. The default (false) is the safer path;
	// opt-in is for codebases that depend on host services (DBs, env vars,
	// system tools) the sandbox can't see. Guardrails apply either way.
	VerifyOnHost bool

	// TrustMode gates command execution (untrusted refuses; trusted =
	// sandbox; fully-trusted permits host execution). Resolved once per
	// turn from ATLAS_TRUST_MODE.
	TrustMode trustMode

	// Streaming callback
	StreamFn func(eventType string, data interface{})

	// Permission callback
	PermissionFn func(toolName string, args json.RawMessage) bool

	// Context for cancellation
	Ctx context.Context
	// Mandatory checks that could not run because their service did not
	// answer, by check name. Guarded by mu. See noteCheckServiceUnavailable.
	CheckServiceFailures map[string]int
}

// NewAgentContext creates a new agent context with defaults.
func NewAgentContext(workingDir string, tier Tier) *AgentContext {
	return &AgentContext{
		Tier:            tier,
		MaxTurns:        TierMaxTurns(tier),
		WorkingDir:      workingDir,
		PermissionMode:  PermissionDefault,
		FileReadTimes:   make(map[string]time.Time),
		FilesRead:       make(map[string]string),
		BodySeen:        make(map[string]bool),
		AppliedEdits:    make(map[string]bool),
		FailedToolCalls: make(map[string]string),
		SessionWrites:   make(map[string]bool),
		Ctx:             context.Background(),
	}
}

// Stream sends an SSE event to the client.
func (c *AgentContext) Stream(eventType string, data interface{}) {
	if c.StreamFn != nil {
		c.StreamFn(eventType, data)
	}
}

// RecordFileRead tracks when a file was last read (for staleness detection).
func (c *AgentContext) RecordFileRead(path string, content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.FileReadTimes[path] = time.Now()
	// First sighting wins: FilesRead is overwritten after every edit, so it
	// tracks current state. Comparing what the RUN changed needs the state it
	// started from, which is only available the first time a path is seen.
	if c.OriginalContent == nil {
		c.OriginalContent = make(map[string]string)
	}
	if _, seen := c.OriginalContent[path]; !seen {
		c.OriginalContent[path] = content
	}
	c.FilesRead[path] = content
	c.LastReadPath = path
}

// OriginalOf returns a file's content as the run first saw it, and whether the
// run has seen it at all.
func (c *AgentContext) OriginalOf(path string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.OriginalContent[path]
	return v, ok
}

// LastRead returns the most recently read file and its content, for restating
// just before the generation point. Empty when nothing has been read.
func (c *AgentContext) LastRead() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.LastReadPath, c.FilesRead[c.LastReadPath]
}

// WasFileRead returns true if the file was read during this agent session.
func (c *AgentContext) WasFileRead(path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.FileReadTimes[path]
	return ok
}

// GetFileRead returns the cached content for path under the context lock.
func (c *AgentContext) GetFileRead(path string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	content, ok := c.FilesRead[path]
	return content, ok
}

// ForgetFileRead drops the read-cache entry for path under the context
// lock. Used when a file is moved away from path.
func (c *AgentContext) ForgetFileRead(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.FilesRead, path)
}

// SnapshotFilesRead returns a copy of the session read-cache under the
// context lock, for callers that iterate over it.
func (c *AgentContext) SnapshotFilesRead() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]string, len(c.FilesRead))
	for k, v := range c.FilesRead {
		out[k] = v
	}
	return out
}

// allowToolForTurn records a tool the user approved with session scope so
// later calls to it in this turn skip the permission prompt.
func (c *AgentContext) allowToolForTurn(toolName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.AllowedTools == nil {
		c.AllowedTools = map[string]bool{}
	}
	c.AllowedTools[toolName] = true
}

// isToolAllowed reports whether a tool has been pre-approved for the session.
func (c *AgentContext) isToolAllowed(toolName string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AllowedTools[toolName]
}

// ---------------------------------------------------------------------------
// Permission system types
// ---------------------------------------------------------------------------

type PermissionMode int

const (
	PermissionDefault     PermissionMode = iota // Ask before write_file, delete_file and every command; in-place edits and moves run
	PermissionAcceptEdits                       // Also run write_file without asking; still ask before deletes and commands
	PermissionYolo                              // Auto-approve everything
)

func (m PermissionMode) String() string {
	switch m {
	case PermissionDefault:
		return "default"
	case PermissionAcceptEdits:
		return "accept-edits"
	case PermissionYolo:
		return "yolo"
	}
	return "default"
}

// ---------------------------------------------------------------------------
// Project detection types
// ---------------------------------------------------------------------------

type ProjectInfo struct {
	Language     string   `json:"language"`      // "nodejs", "python", "rust", "go", "c", "shell"
	Framework    string   `json:"framework"`     // "nextjs", "flask", "actix", etc.
	ConfigFiles  []string `json:"config_files"`  // detected config file paths
	BuildCommand string   `json:"build_command"` // e.g., "npm run build"
	DevCommand   string   `json:"dev_command"`   // e.g., "npm run dev"
	TestCommand  string   `json:"test_command"`  // e.g., "npm test"
}

// ---------------------------------------------------------------------------
// V3 pipeline types
// ---------------------------------------------------------------------------

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
	// callV3GenerateStreaming and never by a caller. The service plans its
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

// The versioned evidence envelope, as the V3 service serialises it.
//
// This file is TRANSPORT AND VALIDATION ONLY. The domain lives in the
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

var evidenceStrengthOrder = []string{"syntax", "runtime", "behavioral", "oracle"}

func strengthRank(s string) int {
	for i, v := range evidenceStrengthOrder {
		if v == s {
			return i
		}
	}
	return -1
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
	req, obs := strengthRank(p.RequiredStrength), strengthRank(p.ObservedStrength)
	if req < 0 {
		return false, "unknown required strength " + p.RequiredStrength
	}
	if obs < 0 {
		return false, "unknown observed strength " + p.ObservedStrength
	}
	if strengthRank(ceiling) < req {
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

// ---------------------------------------------------------------------------
// SSE event types for the CLI protocol
// ---------------------------------------------------------------------------

type SSEEvent struct {
	Type string      `json:"type"` // "tool_call", "tool_result", "text", "done", "permission_request", "error"
	Data interface{} `json:"data"`
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

// maxTaskContractEntries bounds each list, matching the ceiling the session's
// other per-path state already uses.
const maxTaskContractEntries = 64

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

package main

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/itigges22/atlas-proxy/internal/taskcontract"
	"github.com/itigges22/atlas-proxy/internal/wire"
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
// Agent context — shared state for the agent loop
// ---------------------------------------------------------------------------

// AgentContext holds all state for a single agent loop execution, grouped by
// concern into the structs embedded below. A field is still read as ctx.Field.
type AgentContext struct {
	runSetup
	modelExchange
	planProgress
	fileHistory
	loopWatch
	fencedChannel
	checkRecords
	deliverableLedger
	deliveryGrants
	workspaceSafety
	backgroundWork
	serviceHealth
	routeEntries
	runOutcome

	// mu stays here because four groups share it: AllowedTools, the file history,
	// the deletion records and CheckServiceFailures are read and written under it.
	mu sync.Mutex
}

// runSetup is what the request fixed when the run started: its limits and
// directories, what it may do, the services it calls, what the client sent,
// and the callbacks and lifetimes it runs under.
type runSetup struct {
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

	// TaskContract is what the client declared about this request, validated
	// at the request boundary (validateTaskContract). Nil means the client
	// declared nothing, which stays distinct from declaring an empty
	// contract.
	TaskContract *taskcontract.TaskContract

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

	// PriorHistory is the prior-turn user/assistant transcript, sent by
	// the TUI on each /v1/agent request so the agent can answer follow-ups
	// like "what did you just delete?" — without it, every user message
	// is a fresh agent loop with empty context. Populated by handleAgent
	// from the request body; consumed once at the top of runAgentLoop
	// and then ignored. Tool/system rows are filtered out at the TUI
	// boundary; only role=user|assistant text turns flow through here.
	PriorHistory []wire.AgentMessage

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

	// PassID identifies this prompt-pass (the session id from the request).
	PassID string

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

	// RequestCtx is the RESPONSE lifetime: alive for finalisation after the
	// work context has been cancelled. Ctx is the WORK lifetime -- LLM calls,
	// tools, gates, V3 and the sandbox all hang off it -- and it ends one
	// reserve before the session budget does.
	RequestCtx context.Context
	cancelWork context.CancelFunc

	// Context for cancellation
	Ctx context.Context
}

// modelExchange is the conversation with the model: the messages, the tokens
// they cost, which session files it has been told about, and why the proxy
// last cut a generation.
type modelExchange struct {
	Messages    []wire.AgentMessage
	TotalTokens int

	// ManifestAnnounced tracks which SessionWrites paths have been named
	// in a session-file-manifest note (context.go) so each file
	// is announced to the model once.
	ManifestAnnounced map[string]bool

	// LastStreamCut records why the proxy itself ended the generation, when
	// it did. A cut lands mid-JSON, so the parse then fails; the parse-failure
	// classifier reads this known cause instead of inferring one from the
	// broken output.
	LastStreamCut string

	// LastOldStrCut is what the old_str watch saw when LastStreamCut is
	// "old_str_unmatched": the file, and where old_str stopped matching it.
	LastOldStrCut *oldStrWatch
}

// planProgress is the pre-flight plan and how far the run's tool calls have
// followed it.
type planProgress struct {
	// Plan is the optional pre-flight plan produced by /v3/plan. Set
	// once at the top of the agent loop for non-trivial requests; nil
	// when we skipped planning (T0, simple greetings, dev mode without
	// V3). Read by the plan-adherence gate to compare actual tool calls
	// against the planned step actions.
	Plan *wire.Plan

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
}

// fileHistory is what the run has done with files: what it read and when,
// what it showed the model, what it wrote and which edits it applied.
type fileHistory struct {
	FileReadTimes map[string]time.Time // for staleness detection
	FilesRead     map[string]string    // cache of read file contents

	// LastReadPath is the file most recently returned by read_file, restated
	// near the generation point so the model copies from close range.
	LastReadPath string

	// OriginalContent is each touched file as the run FIRST saw it. FilesRead
	// is overwritten on every edit, so it cannot answer "what did this run
	// change".
	OriginalContent map[string]string

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

	// AppliedEdits keys every edit_file that already succeeded, by
	// (path, old_str, new_str). A model that re-applies an identical edit has
	// lost track of its own work, and when the edit is whitespace-only the
	// original old_str still matches afterwards, so it can apply forever.
	// Observed live: the same `'PAUSED';` -> `' PAUSED';` edit ran twice,
	// 1m25s and 1m15s, each adding another space.
	AppliedEdits map[string]bool
}

// loopWatch is the state of the detectors that step in when the model repeats
// itself: the same tool call or reasoning, the same failure, or a run of low
// lens scores.
type loopWatch struct {
	// Tool-call repetition detector: rolling window of recent (tool,
	// args) signatures. When the same signature appears
	// toolRepeatThreshold times within the last toolRepeatWindow
	// entries, the loop injects a corrective system message. Owned by
	// the detector — recordToolCall appends to it and clears it as it
	// fires; callers go through resetToolRepeatWindow rather than
	// assigning here. See proxy/detectors.go for the detection logic.
	RecentToolCalls []string

	// FailedToolCalls keys every tool call that has already been REJECTED,
	// by the same (name, args) signature the repetition detector uses.
	// Re-sending a byte-identical call whose last run failed cannot produce
	// a different result — the harness is deterministic — so the second one
	// is refused before it executes rather than nudged afterwards.
	FailedToolCalls map[string]string

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

	// LastRejectionClass is the skeleton of the previous failure's message.
	// The error-loop breaker resets when a new failure differs from it: three
	// DIFFERENT rejections is a model converging, not one looping.
	LastRejectionClass string

	// Path-aware error-loop detector. Tracks recent failure paths so
	// the 3-consecutive-failures breaker can distinguish "stuck on one
	// file" (real loop, stop) from "grinding through different files,
	// some succeed, some fail" (progress, keep going). Cleared on any
	// successful tool result. See the error-loop break in
	// proxy/loop_results.go for the use site.
	RecentFailurePaths []string

	// Rolling list of gx_score_min values
	// from lens scoring of write_file/edit_file tool calls. When the
	// recent N values all fall below the selected model's calibrated threshold the loop
	// injects a corrective system message before the next LLM call.
	// See proxy/lens.go for the pattern detection.
	LensScoreHistory []float64
}

// fencedChannel is the session's record of the @fenced sub-calls: what they
// cost, where they failed or stalled, and what the last one streamed.
type fencedChannel struct {
	// FencedCalls / FencedTokens account the @fenced sub-calls
	// (fetchFencedContent): every attempt is a full model generation, and
	// leaving it out makes a session's reported token count short by one
	// whole generation per written file. FencedTokens is also folded into
	// TotalTokens; these two exist so the fenced share is visible on its own.
	FencedCalls  int
	FencedTokens int

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
}

// checkRecords is what the run's checks found: each verification run, each
// staged command, and the asset lint findings already mentioned.
type checkRecords struct {
	// VerificationEvidence holds one record per verification run, binding
	// the result to what the command actually exercised: the command line,
	// its stdin contract, and the sha256 of each covered session-written
	// file at that moment. A failed run is recorded too and takes back an
	// earlier pass over the same bytes. Lens labels are drawn from these
	// records, never from a session-wide boolean: a flag can't answer
	// "verified WHAT, at WHICH bytes", and labelling on it would train the
	// lens on files the passing command never touched.
	VerificationEvidence []taskcontract.VerificationRecord

	// StagedCommands are declared commands that ran against exact candidate
	// bytes in the staging overlay, kept in their own store rather than
	// flattened into the record above. That record can say which paths a run
	// named; it cannot say which candidate, invocation or grant a run belongs
	// to, and those are exactly what makes a staged execution worth trusting.
	StagedCommands []stagedCommandFulfillment

	// AssetLintSeen dedupes asset-graph lint findings (gates.go) so
	// a persistent orphan is mentioned once, not after every write.
	AssetLintSeen map[string]bool
}

// deliverableLedger holds the ledger and its lock.
type deliverableLedger struct {
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
	Ledger   map[string]*taskcontract.DeliverableState
	LedgerMu sync.Mutex
}

// deliveryGrants is the one-time grants this request minted and what each
// authorized delivery left behind, under one lock.
type deliveryGrants struct {
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

	// settlements holds what each authorized delivery left behind, keyed by
	// canonical target. Written only by the delivery owner, and only for a
	// delivery whose exact authorized bytes were confirmed on disk. Guarded
	// by grantMu, beside the licence that produced it.
	settlements map[string]*deliverySettlement
}

// workspaceSafety protects what the user had: the workspace as the request
// found it, background work that may still be writing, and the approval and
// record of each deletion.
type workspaceSafety struct {
	// InitialWorkspace is what the workspace held when this request started:
	// the files a shell command's removal can take from the user
	// (applyShellChanges). ShellEffectsUnobserved says some command's changes
	// could not all be seen, because the workspace outgrew the walk.
	InitialWorkspace       workspaceSnapshot
	ShellEffectsUnobserved bool

	// WorkspaceHazards is the set of background work that may still be
	// writing, keyed by job identity rather than counted by start attempt, so
	// a job's confirmed exit clears only its own hazard and a start refused
	// before dispatch raises none. run_background raises it. A signal alone
	// does not clear it, because a signalled process may still be flushing:
	// only a confirmed exit (a reaped exit code) clears it, and tracked paths
	// must be rehashed afterwards. A dispatch that came back with no job id
	// raises hazardUnidentifiedJob, which nothing can clear.
	WorkspaceHazards map[string]bool

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
}

// backgroundWork is the background jobs this loop started and the walk their
// effects are measured against.
type backgroundWork struct {
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

	// BackgroundBaseline is the walk taken after the call that started the
	// first background job still live. What jobs changed is recorded against
	// it once none can still be writing (settleBackgroundEffects).
	BackgroundBaseline workspaceSnapshot
}

// serviceHealth is what the run noticed about services that stopped
// answering: the lens, V3, and the services behind mandatory checks.
type serviceHealth struct {
	// lensDown is why the lens stopped scoring during this run, set by
	// noteLensDown; the agent loop ends the run when it is not empty.
	lensDown string
	// v3Unchecked holds the writes that landed after V3 failed on them (ran
	// out of time or was unavailable), keyed by ledger key. The final summary
	// names each one whose bytes are still on disk (v3_fallback_note.go).
	v3Unchecked   map[string]v3UncheckedWrite
	v3UncheckedMu sync.Mutex
	// Mandatory checks that could not run because their service did not
	// answer, by check name. Guarded by mu. See noteCheckServiceUnavailable.
	CheckServiceFailures map[string]int
}

// routeEntries counts this request's entries into the candidate-generation
// route, under its own lock.
type routeEntries struct {
	// routeEntrySeq numbers the entries this request has made into the
	// candidate-generation route. One write_file call that reaches the route
	// is one entry, and a retry is a new one -- see routeEntry.
	routeEntryMu  sync.Mutex
	routeEntrySeq int
}

// runOutcome is how the session ended, set once.
type runOutcome struct {
	// The session's single terminal outcome, set by the one emitter. Read by
	// the deferred broker envelope so the two cannot disagree.
	TerminalStatus wire.TerminalStatus
	TerminalReason string

	// TerminalUnresolved names the exit gates that spent their bounces with
	// their finding still true, comma-separated; empty when there were none.
	TerminalUnresolved string
	terminalOnce       sync.Once
}

// NewAgentContext creates a new agent context with defaults.
func NewAgentContext(workingDir string, tier Tier) *AgentContext {
	return &AgentContext{
		runSetup: runSetup{
			Tier:           tier,
			MaxTurns:       TierMaxTurns(tier),
			WorkingDir:     workingDir,
			PermissionMode: PermissionDefault,
			Ctx:            context.Background(),
		},
		fileHistory: fileHistory{
			FileReadTimes: make(map[string]time.Time),
			FilesRead:     make(map[string]string),
			BodySeen:      make(map[string]bool),
			AppliedEdits:  make(map[string]bool),
			SessionWrites: make(map[string]bool),
		},
		loopWatch: loopWatch{
			FailedToolCalls: make(map[string]string),
		},
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

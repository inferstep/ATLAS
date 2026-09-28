// Request/response and SSE event shapes for the atlas-proxy public client
// API. Mirrors docs/API.md and the Go structs in proxy/types.go /
// tui/chat.go — field tags there are the source of truth.

/** One prior-turn message replayed to the proxy on each /v1/agent call.
 * The proxy caps history at the most recent 40 entries. */
export interface HistoryMessage {
	role: 'user' | 'assistant';
	content: string;
}

/** Permission mode for a turn (docs/API.md POST /v1/agent `mode`). */
export type PermissionMode = 'default' | 'accept-edits' | 'yolo';

/** POST /v1/agent request body. Field names MUST match the anonymous
 * struct in proxy/agent.go's handleAgent (see tui/chat.go agentRequest). */
export interface AgentRequest {
	message: string;
	working_dir: string;
	mode: PermissionMode;
	/** Required for /cancel and /v1/permission — the proxy keys the cancel
	 * handle and pending permission requests by this id. */
	session_id: string;
	history?: HistoryMessage[];
	/** Tools the user approved for the whole session; re-sent every turn. */
	session_allowed_tools?: string[];
}

/** POST /v1/permission request body. */
export interface PermissionDecisionRequest {
	session_id: string;
	tool_call_id: string;
	decision: 'allow' | 'deny';
	scope: 'once' | 'session';
}

/** One SSE event on the /v1/agent stream: {"type":"<name>","data":{...}}.
 * `data` stays loosely typed at the envelope level — narrow via the
 * payload interfaces below after switching on `type`. Unknown types must
 * be tolerated (forward compatibility). */
export interface ChatEvent {
	type: string;
	data: unknown;
}

// --- Payloads for the events the extension renders (docs/API.md table). ---

export interface TextEventData {
	content: string;
}

export interface ReasoningTokenEventData {
	text: string;
}

export interface ToolCallEventData {
	name: string;
	args: unknown;
	turn: number;
}

export interface ToolResultEventData {
	tool: string;
	success: boolean;
	data: unknown;
	error?: string;
	/** Go duration string, e.g. "245ms". */
	elapsed?: string;
}

export interface PermissionRequestEventData {
	tool_name: string;
	args: unknown;
	/** Human-readable description of the pending call. */
	message: string;
	/** Echo back on POST /v1/permission. */
	tool_call_id: string;
	/** The answer covers this request only (the proxy sets it on deletions):
	 * no session-wide answer may be offered or applied. */
	one_time_only?: boolean;
	/** For a deletion: the exact path that would be removed. */
	canonical_path?: string;
}

export interface PermissionDeniedEventData {
	tool: string;
}

export interface LlmCallStartEventData {
	turn: number;
	messages: number;
	prompt_tokens: number;
}

export interface LlmFirstTokenEventData {
	prompt_ms: number;
}

export interface LlmCallEndEventData {
	turn: number;
	tokens: number;
	total_tokens: number;
	ms: number;
	chars?: number;
	error?: string;
}

export interface LlmPromptProgressEventData {
	processed: number;
	total: number;
	/** 0–1 float. */
	pct: number;
	elapsed_ms: number;
}

export interface DoneEventData {
	/** Empty for a text-shaped turn. */
	summary: string;
}

export interface ErrorEventData {
	error: string;
}

export interface PlanStep {
	id: string;
	action: string;
	target: string;
	why: string;
}

export interface PlanLoadedEventData {
	steps: PlanStep[];
	verify_step: string;
	rationale: string;
	winning_score: number;
	/** 0 for the initial plan, 1+ for revisions. */
	revision: number;
}

/** plan_adherence fires after each tool call. Match shape carries
 * step_index/step_id; miss shape carries tool/off_streak (+ neutral=true
 * for recon tools that leave the off-streak unchanged). */
export interface PlanAdherenceEventData {
	matched: boolean;
	step_index?: number;
	step_id?: string;
	step_action?: string;
	satisfied: number;
	total: number;
	tool?: string;
	off_streak?: number;
	neutral?: boolean;
}

export interface PlanReviseEventData {
	reason: string;
	/** 1-indexed. */
	revision: number;
}

/** v3_progress — fallback for V3 stages without a dedicated typed event. */
export interface V3ProgressEventData {
	message: string;
}

/** Common shape of the typed V3 stage events (v3_phase / v3_sandbox /
 * v3_repair) — every one carries stage + detail plus stage-specific extras
 * the progress line does not need. */
export interface V3StageEventData {
	stage: string;
	detail: string;
}

export interface V3LensVetoEventData {
	stage: string;
	detail: string;
	/** Candidate index. */
	index: number;
	gx_score_min: number;
	first_off_rails_idx: number;
}

export interface V3StructuralVetoEventData {
	stage: string;
	detail: string;
	/** Candidate index. */
	index: number;
	n_unresolved: number;
	unresolved_calls: string[];
	n_calls_total: number;
}

export interface AgentLensScoreEventData {
	tool: 'write_file' | 'edit_file';
	turn: number;
	n_tokens: number;
	first_off_rails_idx: number;
	gx_score_min: number;
	gx_score_mean: number;
	latency_ms: number;
}

export interface AgentLensInterventionEventData {
	turn: number;
	tool: string;
	/** The corrective injected into the next LLM call. */
	reason: string;
}

// --- Non-stream endpoint payloads. ---

/** POST /cancel response. 200 → cancelled:true; 404 → cancelled:false. */
export interface CancelResponse {
	cancelled: boolean;
}

/** POST /v1/permission response. 200 → delivered:true; 404 → the request
 * already resolved (treated as success by convention — see tui/chat.go
 * postPermissionDecision). */
export interface PermissionDecisionResponse {
	delivered: boolean;
}

/** GET /ready body — 200 when all gates pass, 503 (same shape) otherwise. */
export interface ReadyResponse {
	ready: boolean;
	inference: boolean;
	lens_ready: boolean;
	sandbox: boolean;
	v3: boolean;
}

/** GET /version body. */
export interface VersionResponse {
	api_version: string;
	protocol_version: number;
	error_codes: string[];
}

/** GET /v1/calibration/status — lens + ASA compat verdict for the loaded
 * model. Called once at activation and on manual refresh only: every call
 * re-probes the lens service (~50–200 ms, docs/API.md). */
export interface CalibrationStatusResponse {
	lens: {
		verdict: 'supported' | 'no-artifacts' | 'incomplete-artifacts' | 'uncalibrated' | 'dim-mismatch' | 'unreachable' | string;
		hint?: string;
	};
	asa: {
		verdict: 'supported' | 'missing' | 'unverified' | 'incompatible' | string;
		hint?: string;
	};
	/** The seven status dimensions `atlas doctor` renders. */
	dimensions?: { name: string; status: string; detail: string }[];
}

/** Closed error-code set from docs/API.md. Switch on `error`, never on
 * `detail` — the human message may change between versions. */
export type ErrorCode =
	| 'unauthorized'
	| 'invalid_input'
	| 'unsupported_operation'
	| 'permission_denied'
	| 'timeout'
	| 'cancelled'
	| 'dependency_unavailable'
	| 'incompatible_artifact'
	| 'resource_limit'
	| 'sandbox_policy_rejected'
	| 'model_failure'
	| 'internal_error';

/** Stable error envelope on non-2xx JSON responses. */
export interface ErrorEnvelope {
	error: ErrorCode | string;
	detail?: string;
	api_version?: string;
}

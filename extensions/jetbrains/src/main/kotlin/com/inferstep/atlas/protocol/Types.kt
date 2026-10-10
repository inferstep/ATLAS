// Protocol types for the atlas-proxy public client API.
//
// Mirrors extensions/vscode/src/client/types.ts and docs/API.md; the field
// tags in proxy/types.go are the source of truth, so every name here that
// differs from the Kotlin identifier is pinned with @SerialName to the exact
// wire name.
//
// Nothing in this package may import com.intellij.*: the layer exists so the
// protocol can be exercised with plain JUnit, without launching an IDE.
package com.inferstep.atlas.protocol

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.SerializationException
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.decodeFromJsonElement

/**
 * The one [Json] instance the client uses for both directions.
 *
 * `ignoreUnknownKeys` is the forward-compatibility rule: the proxy adds
 * fields to payloads additively, and a client that throws on them is a client
 * that breaks on a proxy upgrade. `explicitNulls = false` keeps an absent
 * optional field absent on the wire rather than sending an explicit null.
 */
val AtlasJson: Json =
    Json {
        ignoreUnknownKeys = true
        explicitNulls = false
        encodeDefaults = false
    }

// ---------------------------------------------------------------------------
// The SSE envelope
// ---------------------------------------------------------------------------

/**
 * One event on the `/v1/agent` stream: `{"type":"<name>","data":{...}}`.
 *
 * `data` is deliberately left as a raw [JsonElement] at the envelope level.
 * The proxy documents event types the client does not render (every V3 stage,
 * every detector intervention) and may add more, so an unknown type has to
 * survive the envelope and only fail when someone asks for a payload shape it
 * does not have. Use [payload] for that: it decodes on demand and throws for
 * a shape mismatch, which is the caller's own type error, not a stream error.
 *
 * A decoder of this envelope must switch on [type] first; a payload type is
 * only meaningful for the event type it belongs to.
 */
@Serializable
data class ChatEvent(
    val type: String,
    val data: JsonElement = JsonObject(emptyMap()),
)

/** Decode this event's `data` as [T]; throws if the payload is not that shape. */
inline fun <reified T> ChatEvent.payload(json: Json = AtlasJson): T =
    json.decodeFromJsonElement<T>(data)

/**
 * Decode this event's `data` as [T], or null when it is not that shape.
 *
 * Note what "not that shape" means here. Every payload field has a default, so
 * a JSON *object* with keys this type does not know decodes successfully to an
 * all-defaults instance — that is the forward-compatibility rule working, not
 * a failure. This returns null for a payload of the wrong JSON kind (an array
 * or a scalar where an object is expected), which is the case worth guarding.
 */
inline fun <reified T> ChatEvent.payloadOrNull(json: Json = AtlasJson): T? =
    try {
        payload(json)
    } catch (_: SerializationException) {
        null
    }

// ---------------------------------------------------------------------------
// POST /v1/agent
// ---------------------------------------------------------------------------

/** Permission mode for a turn (`mode` on POST /v1/agent). */
@Serializable
enum class PermissionMode {
    @SerialName("default")
    DEFAULT,

    @SerialName("accept-edits")
    ACCEPT_EDITS,

    @SerialName("yolo")
    YOLO,
}

/** One prior-turn message replayed to the proxy on each `/v1/agent` call.
 * The proxy caps history at the most recent 40 entries. */
@Serializable
data class HistoryMessage(
    val role: String,
    val content: String,
)

/**
 * POST /v1/agent request body. Field names match the anonymous struct in
 * proxy/agent.go's handleAgent (see tui/chat.go's agentRequest).
 *
 * `task_contract` is intentionally absent: it is stage 3 material, and the
 * reference VS Code client does not send one yet either.
 */
@Serializable
data class AgentRequest(
    val message: String,
    @SerialName("working_dir") val workingDir: String,
    val mode: PermissionMode = PermissionMode.DEFAULT,
    /** Required for `/cancel` and `/v1/permission`: the proxy keys the cancel
     * handle and pending permission requests by this id. */
    @SerialName("session_id") val sessionId: String,
    val history: List<HistoryMessage>? = null,
    /** Tools the user approved for the whole session; re-sent every turn. */
    @SerialName("session_allowed_tools") val sessionAllowedTools: List<String>? = null,
)

// ---------------------------------------------------------------------------
// POST /v1/permission and POST /cancel
// ---------------------------------------------------------------------------

/** POST /v1/permission request body. */
@Serializable
data class PermissionDecisionRequest(
    @SerialName("session_id") val sessionId: String,
    @SerialName("tool_call_id") val toolCallId: String,
    val decision: PermissionDecision,
    val scope: PermissionScope,
)

/** `decision` on POST /v1/permission. Anything other than `allow` denies. */
@Serializable
enum class PermissionDecision {
    @SerialName("allow")
    ALLOW,

    @SerialName("deny")
    DENY,
}

/** `scope` on POST /v1/permission. `session` skips re-prompting for the same
 * tool for the rest of the turn; the proxy downgrades it for `delete_file`. */
@Serializable
enum class PermissionScope {
    @SerialName("once")
    ONCE,

    @SerialName("session")
    SESSION,
}

/** POST /cancel request body. */
@Serializable
data class CancelRequest(
    @SerialName("session_id") val sessionId: String,
)

/** POST /cancel response. 200 carries `cancelled: true`; a 404 means there was
 * nothing in flight, which is success for an idempotent cancel. */
@Serializable
data class CancelResponse(
    val cancelled: Boolean = false,
)

/** POST /v1/permission response. 200 carries `delivered: true`; a 404 means
 * the request already resolved and is treated as success. */
@Serializable
data class PermissionDecisionResponse(
    val delivered: Boolean = false,
)

// ---------------------------------------------------------------------------
// Non-stream endpoints
// ---------------------------------------------------------------------------

/** GET /ready body — 200 when every gate passes, 503 (same shape) otherwise. */
@Serializable
data class ReadyResponse(
    val ready: Boolean = false,
    val inference: Boolean = false,
    @SerialName("lens_ready") val lensReady: Boolean = false,
    val sandbox: Boolean = false,
    val v3: Boolean = false,
    /** Present only when the lens cannot score: why `/v1/agent` would refuse. */
    @SerialName("lens_reason") val lensReason: String? = null,
)

/** GET /version body. `grammar_mode` and `session_timeout_s` are additive:
 * a measurement records the configuration it ran against. */
@Serializable
data class VersionResponse(
    @SerialName("api_version") val apiVersion: String = "",
    @SerialName("protocol_version") val protocolVersion: Int = 0,
    @SerialName("error_codes") val errorCodes: List<String> = emptyList(),
    @SerialName("grammar_mode") val grammarMode: String? = null,
    @SerialName("session_timeout_s") val sessionTimeoutSeconds: Int? = null,
)

/** GET /workspace body. Requires the service token like any other route. */
@Serializable
data class WorkspaceResponse(
    /** The host path; empty when the proxy cannot report an absolute one. */
    @SerialName("project_dir") val projectDir: String = "",
    /** The path the proxy writes to in its own process. */
    @SerialName("working_dir") val workingDir: String = "",
    val containerized: Boolean = false,
)

/** GET /v1/calibration/status body — the lens/ASA verdict for the loaded
 * model, plus the seven status dimensions `atlas doctor` renders. */
@Serializable
data class CalibrationStatusResponse(
    val lens: CalibrationComponent = CalibrationComponent(),
    val asa: CalibrationComponent = CalibrationComponent(),
    val dimensions: List<StatusDimension> = emptyList(),
)

/** One half of the calibration verdict. `verdict` is left as a string: the
 * closed set is per-component and grows, so a new verdict must not break an
 * older client. */
@Serializable
data class CalibrationComponent(
    val verdict: String = "",
    val hint: String? = null,
)

/** One row of the canonical seven-dimension lens/ASA status. */
@Serializable
data class StatusDimension(
    val name: String = "",
    val status: String = "",
    val detail: String = "",
)

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

/**
 * The closed error-code set (`docs/API.md` § Versioning and error codes).
 *
 * Exactly the six codes `AllErrorCodes` in proxy/main.go emits, which a
 * contract test asserts. Switch on these, never on `detail`: the human
 * message may change between versions.
 *
 * Note the drift this file does NOT copy: the VS Code client's `ErrorCode`
 * union lists twelve codes, including ones (`permission_denied`, `timeout`,
 * `cancelled`, `model_failure`, ...) the proxy never writes. The Go source is
 * the authority, so the six below are what this client treats as known; any
 * other code is still readable as an [ErrorEnvelope.error] string.
 */
object AtlasErrorCodes {
    const val UNAUTHORIZED = "unauthorized"
    const val INVALID_INPUT = "invalid_input"
    const val UNSUPPORTED_OPERATION = "unsupported_operation"
    const val DEPENDENCY_UNAVAILABLE = "dependency_unavailable"
    const val RESOURCE_LIMIT = "resource_limit"
    const val INTERNAL_ERROR = "internal_error"

    /** Every code, in the order proxy/main.go declares them. */
    val ALL =
        listOf(
            UNAUTHORIZED,
            INVALID_INPUT,
            UNSUPPORTED_OPERATION,
            DEPENDENCY_UNAVAILABLE,
            RESOURCE_LIMIT,
            INTERNAL_ERROR,
        )
}

/** The stable error envelope on non-2xx JSON responses. */
@Serializable
data class ErrorEnvelope(
    val error: String = "",
    val detail: String = "",
    @SerialName("api_version") val apiVersion: String = "",
)

// ---------------------------------------------------------------------------
// /v1/agent event payloads
// ---------------------------------------------------------------------------

/** `turn_start` — the start of every agent loop iteration. */
@Serializable
data class TurnStartPayload(
    val turn: Int = 0,
    val messages: Int = 0,
    /** True when the conversation history was trimmed for the context window. */
    val trimmed: Boolean = false,
)

/** `llm_call_start` — before each LLM round-trip. */
@Serializable
data class LlmCallStartPayload(
    val turn: Int = 0,
    val messages: Int = 0,
    /** Estimated as chars/4. */
    @SerialName("prompt_tokens") val promptTokens: Int = 0,
)

/** `llm_prompt_progress` — prompt-eval counters, roughly every 100 ms. */
@Serializable
data class LlmPromptProgressPayload(
    val processed: Int = 0,
    val total: Int = 0,
    val pct: Double = 0.0,
    @SerialName("elapsed_ms") val elapsedMs: Long = 0,
)

/** `llm_first_token` — time to the first streamed delta. */
@Serializable
data class LlmFirstTokenPayload(
    @SerialName("prompt_ms") val promptMs: Long = 0,
)

/** `llm_token` — one streamed delta of the model's tool-call content. */
@Serializable
data class LlmTokenPayload(
    val text: String = "",
)

/** `llm_call_end` — one LLM round-trip finished. On error `chars` is absent
 * and `tokens` is 0. */
@Serializable
data class LlmCallEndPayload(
    val turn: Int = 0,
    val tokens: Int = 0,
    @SerialName("total_tokens") val totalTokens: Int = 0,
    val ms: Long = 0,
    val chars: Int? = null,
    val error: String? = null,
)

/** `reasoning_token` — one delta of `reasoning_content`, not of tool content. */
@Serializable
data class ReasoningTokenPayload(
    val text: String = "",
)

/** `tool_call` — the model emitted a tool call. `args` is raw JSON. */
@Serializable
data class ToolCallPayload(
    val name: String = "",
    val args: JsonElement = JsonObject(emptyMap()),
    val turn: Int = 0,
)

/** `tool_result` — a tool finished executing. `data` is raw JSON. */
@Serializable
data class ToolResultPayload(
    val tool: String = "",
    val success: Boolean = false,
    val data: JsonElement = JsonObject(emptyMap()),
    val error: String? = null,
    /** Go duration string, e.g. "245ms". */
    val elapsed: String? = null,
)

/**
 * `permission_request` — a destructive tool call is awaiting approval. The
 * turn pauses until the client answers via POST /v1/permission, disconnects,
 * cancels, or the fail-safe timeout denies.
 */
@Serializable
data class PermissionRequestPayload(
    @SerialName("tool_name") val toolName: String = "",
    val args: JsonElement = JsonObject(emptyMap()),
    /** Human-readable description of the pending call. */
    val message: String = "",
    /** Echo back on POST /v1/permission. */
    @SerialName("tool_call_id") val toolCallId: String = "",
    /** Present only for a deletion: the exact path that would be removed.
     * A client should not offer a session-wide answer when these are set. */
    @SerialName("canonical_path") val canonicalPath: String? = null,
    @SerialName("target_type") val targetType: String? = null,
    @SerialName("content_sha256") val contentSha256: String? = null,
    @SerialName("one_time_only") val oneTimeOnly: Boolean = false,
)

/** `permission_denied` — the pending call was not allowed. `reason` is set
 * whenever no user denied it (timeout, disconnect, not askable). */
@Serializable
data class PermissionDeniedPayload(
    val tool: String = "",
    val reason: String? = null,
)

/** `text` — a conversational reply. */
@Serializable
data class TextPayload(
    val content: String = "",
)

/**
 * `done` — the session ended, once per request, whatever the outcome.
 *
 * Only `completed` means the work finished. An absent or unknown [status]
 * reads as incomplete; use [resolvedStatus] rather than comparing the raw
 * string, which may be missing on an older proxy.
 */
@Serializable
data class DonePayload(
    /** The server's account of the run; may be empty for a text-shaped turn. */
    val summary: String = "",
    val status: String? = null,
    /** Why the run ended with that status, for example `repair_unfinished`. */
    val reason: String? = null,
    /** Exit gates that spent their bounces with their finding still true.
     * Present only when there are some. */
    val unresolved: String? = null,
    /** Files still unparseable at the end, comma-separated. */
    @SerialName("repair_open") val repairOpen: String? = null,
) {
    /** The status with the documented absent/unknown rule applied. */
    fun resolvedStatus(): DoneStatus = DoneStatus.from(status)
}

/** The `done.status` closed set, plus the documented default. */
enum class DoneStatus {
    COMPLETED,
    INCOMPLETE,
    STOPPED,
    TIMED_OUT,
    FAILED,
    ;

    /** Only `completed` is a finished task; anything else, including an
     * absent or unrecognized status, is incomplete (docs/API.md). */
    fun isCompleted(): Boolean = this == COMPLETED

    companion object {
        fun from(raw: String?): DoneStatus =
            when (raw) {
                "completed" -> COMPLETED
                "stopped" -> STOPPED
                "timed_out" -> TIMED_OUT
                "failed" -> FAILED
                else -> INCOMPLETE
            }
    }
}

/** `error` — an LLM, parse or turn-cap error ended the stream. */
@Serializable
data class ErrorPayload(
    val error: String = "",
)

/** `v3_progress` — a V3 stage without a dedicated typed event yet. */
@Serializable
data class V3ProgressPayload(
    val message: String = "",
)

/** The common shape of the typed V3 stage events (`v3_phase`, `v3_sandbox`,
 * `v3_repair`, ...): a stage label and a human-readable detail. */
@Serializable
data class V3StagePayload(
    val stage: String = "",
    val detail: String = "",
)

/** `plan_loaded` — a winning plan, after initial generation and each revise. */
@Serializable
data class PlanLoadedPayload(
    val steps: List<PlanStep> = emptyList(),
    @SerialName("verify_step") val verifyStep: String = "",
    val rationale: String = "",
    @SerialName("winning_score") val winningScore: Double = 0.0,
    /** 0 for the initial plan, 1+ for a revision. */
    val revision: Int = 0,
)

/** One step of a generated plan. */
@Serializable
data class PlanStep(
    val id: String = "",
    val action: String = "",
    val target: String = "",
    val why: String = "",
)

/** `plan_adherence` — whether the last tool call satisfied a plan step.
 * Carries either the match shape (step fields) or the miss shape (tool,
 * off-streak), plus `neutral` for recon tools. */
@Serializable
data class PlanAdherencePayload(
    val matched: Boolean = false,
    @SerialName("step_index") val stepIndex: Int? = null,
    @SerialName("step_id") val stepId: String? = null,
    @SerialName("step_action") val stepAction: String? = null,
    val satisfied: Int = 0,
    val total: Int = 0,
    val tool: String? = null,
    @SerialName("off_streak") val offStreak: Int? = null,
    /** True for a recon tool: it neither satisfies a step nor extends the
     * off-streak. */
    val neutral: Boolean = false,
)

/** `plan_revise` — the off-streak crossed the threshold and a fresh plan is
 * being generated. */
@Serializable
data class PlanRevisePayload(
    val reason: String = "",
    /** 1-indexed. */
    val revision: Int = 0,
)

/** `agent_lens_score` — the lens scored a write/edit's content. */
@Serializable
data class AgentLensScorePayload(
    /** `write_file` or `edit_file`. */
    val tool: String = "",
    val turn: Int = 0,
    @SerialName("n_tokens") val nTokens: Int = 0,
    /** -1 when no token went off-rails. */
    @SerialName("first_off_rails_idx") val firstOffRailsIndex: Int = -1,
    @SerialName("gx_score_min") val gxScoreMin: Double = 0.0,
    @SerialName("gx_score_mean") val gxScoreMean: Double = 0.0,
    @SerialName("latency_ms") val latencyMs: Double = 0.0,
)

/** `agent_lens_intervention` — a corrective was queued for the next LLM
 * call. Absent when calibration is missing. */
@Serializable
data class AgentLensInterventionPayload(
    val turn: Int = 0,
    val tool: String = "",
    /** The corrective injected into the next call's messages. */
    val reason: String = "",
)

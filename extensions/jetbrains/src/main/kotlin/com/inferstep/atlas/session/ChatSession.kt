// The chat, as state: the transcript, the open permission prompts, the two
// controls the user operates (permission mode and Work/Question), and the
// send/cancel/reset actions over [TurnManager] and [PermissionFlow]. Since
// Stage 4 it also holds what the diff view needs: the predicted change a
// permission prompt may show, the exact change a finished file op made, and
// the verdicts of the workspace-mismatch check.
//
// Free of com.intellij and of Compose, so the whole conversation — streaming
// text, tool chips, the done banner, answered prompts, applied diffs — is
// exercised under plain JUnit. The tool window is a renderer of [ChatUiState]
// and nothing else: it reads the diffs out of this state and hands them to the
// IDE's diff viewer.
package com.inferstep.atlas.session

import com.inferstep.atlas.protocol.AgentLensInterventionPayload
import com.inferstep.atlas.protocol.ChatEvent
import com.inferstep.atlas.protocol.DonePayload
import com.inferstep.atlas.protocol.ErrorPayload
import com.inferstep.atlas.protocol.LlmPromptProgressPayload
import com.inferstep.atlas.protocol.PermissionDeniedPayload
import com.inferstep.atlas.protocol.PermissionMode
import com.inferstep.atlas.protocol.PermissionRequestPayload
import com.inferstep.atlas.protocol.PlanAdherencePayload
import com.inferstep.atlas.protocol.PlanLoadedPayload
import com.inferstep.atlas.protocol.PlanRevisePayload
import com.inferstep.atlas.protocol.TaskMode
import com.inferstep.atlas.protocol.TextPayload
import com.inferstep.atlas.protocol.ToolCallPayload
import com.inferstep.atlas.protocol.ToolResultPayload
import com.inferstep.atlas.protocol.V3ProgressPayload
import com.inferstep.atlas.protocol.V3StagePayload
import com.inferstep.atlas.protocol.payloadOrNull
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlin.math.roundToInt

private const val TEXT_EVENT = "text"
private const val TOOL_CALL_EVENT = "tool_call"
private const val TOOL_RESULT_EVENT = "tool_result"
private const val PERMISSION_REQUEST_EVENT = "permission_request"
private const val PERMISSION_DENIED_EVENT = "permission_denied"
private const val DONE_EVENT = "done"
private const val ERROR_EVENT = "error"
private const val TURN_START_EVENT = "turn_start"
private const val LLM_CALL_START_EVENT = "llm_call_start"
private const val LLM_PROMPT_PROGRESS_EVENT = "llm_prompt_progress"
private const val LLM_FIRST_TOKEN_EVENT = "llm_first_token"
private const val LLM_CALL_END_EVENT = "llm_call_end"
private const val V3_PROGRESS_EVENT = "v3_progress"
private const val V3_LENS_VETO_EVENT = "v3_lens_veto"
private const val V3_STRUCTURAL_VETO_EVENT = "v3_structural_veto"
private const val AGENT_LENS_INTERVENTION_EVENT = "agent_lens_intervention"
private const val PLAN_LOADED_EVENT = "plan_loaded"
private const val PLAN_ADHERENCE_EVENT = "plan_adherence"
private const val PLAN_REVISE_EVENT = "plan_revise"

/** The long-form V3 stages share the `{stage, detail}` shape. */
private val V3_STAGE_EVENTS =
    setOf(
        "v3_phase",
        "v3_sandbox",
        "v3_repair",
        "v3_plansearch",
        "v3_divsampling",
        "v3_select",
        "v3_probe",
        "v3_self_test",
        "v3_plan",
    )

/** The one tool whose result carries a server-computed unified diff. */
private const val EDIT_FILE_TOOL = "edit_file"

private const val MISMATCH_NOTICE =
    "ATLAS reported a file change that this workspace does not show. The proxy is " +
        "editing a different directory, so its changes are landing where you cannot see them."

/**
 * The exact change a successful file-write tool call made.
 *
 * [before] is the file's text captured at tool_call time (null when it did not
 * exist here), [after] the text on disk once the call returned. Either side can
 * be null, and [serverPreview] is the fallback for the one case with nothing
 * local to show: edit_file's server-computed diff, used when the file is not
 * readable in this workspace.
 */
data class AppliedChange(
    val path: String,
    val before: String?,
    val after: String?,
    val serverPreview: String? = null,
) {
    /** What the IDE's diff tab is called. */
    fun title(): String = "ATLAS: $path (applied)"

    /** The fallback view's title: one side is the server's diff text. */
    fun serverPreviewTitle(): String = "ATLAS: $path (server preview)"
}

/**
 * One line of the conversation.
 *
 * Every entry carries an id so Compose can key a list without re-creating rows
 * as text streams in.
 */
sealed interface TranscriptEntry {
    val id: Long

    /** What the user asked. */
    data class User(
        override val id: Long,
        val text: String,
    ) : TranscriptEntry

    /** Assistant prose, updated in place as deltas arrive. */
    data class Assistant(
        override val id: Long,
        val text: String,
    ) : TranscriptEntry

    /** A tool call, with the one-line summary of what it returned. */
    data class Tool(
        override val id: Long,
        val name: String,
        val summary: String?,
        /** Null while the call is still running. */
        val success: Boolean?,
        /** The exact change to offer as a diff, when there is one to show. */
        val change: AppliedChange? = null,
    ) : TranscriptEntry

    /** Something the client says about itself, never the model's words. */
    data class Notice(
        override val id: Long,
        val text: String,
        val isError: Boolean = false,
        /** Worth a native warning, but not an error: the workspace mismatch. */
        val isWarning: Boolean = false,
    ) : TranscriptEntry

    /** How the run ended: the summary, the status and the reason (#236). */
    data class Done(
        override val id: Long,
        val outcome: DoneOutcome,
    ) : TranscriptEntry
}

/** One step of the plan the proxy is following, as the panel shows it. */
data class ChatPlanStep(
    val id: String,
    val label: String,
)

/** The current plan and which steps the proxy says it has satisfied. */
data class ChatPlan(
    val steps: List<ChatPlanStep>,
    val revision: Int,
    val satisfied: Set<String> = emptySet(),
)

/** Everything the panel renders. */
data class ChatUiState(
    val entries: List<TranscriptEntry> = emptyList(),
    val pending: List<PendingPermission> = emptyList(),
    val running: Boolean = false,
    /** What the next turn declares: work (default) or question. */
    val taskMode: TaskMode = TaskMode.WORK,
    val permissionMode: PermissionMode = PermissionMode.DEFAULT,
    /** Predicted changes for the open prompts, keyed by prompt id. A prompt
     * with no entry either is not a file edit or has nothing predictable to
     * show — the card then offers no diff. */
    val previews: Map<Long, EditPrediction> = emptyMap(),
    /** The latest liveness line from the model or V3 pipeline, cleared when
     * the turn ends. Transient by design: a replay must not show a stale
     * spinner. */
    val progress: String? = null,
    /** The plan the current turn is following, when it generated one. */
    val plan: ChatPlan? = null,
)

/**
 * One conversation against one proxy.
 *
 * [send] suspends for the length of the turn; the caller launches it in the
 * scope it wants the turn's lifetime tied to, and [cancel] stops it from
 * anywhere. The posted permission decisions run on [scope].
 */
class ChatSession(
    client: TurnClient,
    poster: PermissionPoster,
    private val scope: CoroutineScope,
    /** The client's workspace root, sent as `working_dir`. */
    private val workingDir: String,
    /** The local view of that root: reads for diffs, stats for the mismatch
     * check. The default is the plain filesystem one, which is what the tests
     * want; the panel passes one that also refreshes the IDE's VFS. */
    private val workspace: Workspace = LocalWorkspace(workingDir),
    /** GET /workspace, asked once when the panel appears. */
    probe: WorkspaceProbe = WorkspaceProbe { null },
) : PermissionUi {
    @Volatile private var client: TurnClient = client

    @Volatile private var poster: PermissionPoster = poster

    @Volatile private var probe: WorkspaceProbe = probe
    private val turns = TurnManager(scope)
    private val permissions = PermissionFlow(turns.sessionAllowedTools, this, scope)

    private val mutableState = MutableStateFlow(ChatUiState())

    /** The transcript and controls, as the panel renders them. */
    val state: StateFlow<ChatUiState> = mutableState.asStateFlow()

    /** Pre-edit file states awaiting their tool_result, FIFO per tool name —
     * tool_result carries only the tool name, the same matching scheme the
     * reference clients use. */
    private val snapshots = mutableMapOf<String, ArrayDeque<EditSnapshot>>()

    /** Warns once when a successful file op leaves no trace on local disk. */
    private val mismatch =
        MismatchDetector(
            stat = workspace::stat,
            onMismatch = {
                append(TranscriptEntry.Notice(nextId++, MISMATCH_NOTICE, isWarning = true))
            },
        )

    private var nextId = 1L

    @Volatile private var cancelRequested = false

    /**
     * Use a new proxy connection for the next turn.
     *
     * A turn captures the client and poster it started with, so swapping this
     * while a turn is in flight cannot route its permission answers to a
     * different proxy; the new connection takes effect on the next turn.
     */
    fun updateConnection(
        client: TurnClient,
        poster: PermissionPoster,
        probe: WorkspaceProbe,
    ) {
        this.client = client
        this.poster = poster
        this.probe = probe
    }

    /** The next turn's declaration. Nothing reads the message text. */
    fun setTaskMode(mode: TaskMode) {
        mutableState.update { it.copy(taskMode = mode) }
    }

    /** The permission mode the proxy enforces for the turn. */
    fun setPermissionMode(mode: PermissionMode) {
        mutableState.update { it.copy(permissionMode = mode) }
    }

    /**
     * Answer an open prompt. Unknown and already-settled ids are no-ops, which
     * is what a card left over from a finished turn looks like.
     */
    fun answer(
        id: Long,
        choice: PermissionChoice,
    ) {
        permissions.settleById(id, choice)
    }

    /**
     * Ask the proxy which host directory it edits and say so when it is not the
     * one this window has open.
     *
     * A mismatch the proxy can name exactly (GET /workspace, #244) is worth
     * telling the user about before a turn: either they point the IDE at that
     * folder or they run `atlas workspace align` themselves. Every failure is
     * silent — an older proxy without the route must not produce a warning.
     */
    suspend fun checkWorkspace() {
        val report = probe.workspace() ?: return
        val alignment = workspaceAlignment(report.projectDir, workspace.root)
        if (alignment != WorkspaceAlignment.MISALIGNED) {
            return
        }
        append(
            TranscriptEntry.Notice(
                nextId++,
                "ATLAS is editing ${report.projectDir}, not ${workspace.root} — changes it " +
                    "makes will land outside this window.",
                isWarning = true,
            ),
        )
    }

    /**
     * Run one turn: append the user's message, stream the events into the
     * transcript, and end the turn whatever happens.
     *
     * Returns immediately for an empty message or while a turn is in flight.
     */
    suspend fun send(message: String) {
        val text = message.trim()
        if (text.isEmpty() || turns.busy) {
            return
        }

        val mode = mutableState.value.permissionMode
        val taskMode = mutableState.value.taskMode
        // A turn uses one connection from start to finish: a settings change
        // mid-turn affects the next turn, not this one's permission answers.
        val activeClient = client
        val activePoster = poster
        cancelRequested = false
        var sawDone = false

        append(TranscriptEntry.User(nextId++, text))
        mutableState.update { it.copy(running = true, progress = null, plan = null) }

        try {
            turns
                .runTurn(activeClient, text, mode, taskMode, workingDir)
                .collect { event ->
                    if (event.type == DONE_EVENT) {
                        sawDone = true
                    }
                    apply(event, activePoster)
                }
        } catch (e: CancellationException) {
            // The scope went away mid-turn (the panel was disposed); the
            // finally below still closes the turn's prompts.
            throw e
        } catch (e: Throwable) {
            // The turn failed before or during the stream: a 401 when the
            // proxy has a token and this client does not, a 503 while the lens
            // cannot score, a connection that never opened. The panel has to
            // say so — an escaping failure here would leave the user's message
            // on screen with nothing after it, which reads as a hang.
            //
            // AtlasApiError's message is its stable code plus the detail
            // (`unauthorized: ...`), so reporting it verbatim names the cause.
            append(
                TranscriptEntry.Notice(
                    id = nextId++,
                    text = "The turn failed: ${e.message.orEmpty()}",
                    isError = true,
                ),
            )
        } finally {
            permissions.endTurn()
            if (cancelRequested && !sawDone) {
                append(TranscriptEntry.Notice(nextId++, "Turn cancelled."))
            }
            snapshots.clear()
            mismatch.reset()
            mutableState.update {
                it.copy(
                    running = false,
                    pending = emptyList(),
                    previews = emptyMap(),
                    progress = null,
                )
            }
        }
    }

    /** Stop the in-flight turn. A no-op when nothing is running. */
    fun cancel() {
        if (!turns.busy) {
            return
        }
        cancelRequested = true
        turns.cancel()
    }

    /** Start a fresh conversation: drop the transcript, the history, the
     * session-approved tools and every captured diff. */
    fun reset() {
        if (turns.busy) {
            return
        }
        turns.reset()
        snapshots.clear()
        mismatch.reset()
        mutableState.update {
            it.copy(
                entries = emptyList(),
                pending = emptyList(),
                running = false,
                previews = emptyMap(),
                progress = null,
                plan = null,
            )
        }
    }

    // ------------------------------------------------------------------
    // PermissionUi — the panel is a renderer, so the prompts are state too.
    // ------------------------------------------------------------------

    override fun onPrompt(pending: PendingPermission) {
        // The prediction is computed here because this is the only place that
        // knows the prompt's id, which is the key the card reads it back by.
        val request = pending.request
        val target = editTargetPath(request.toolName, request.args)
        val current = target?.let { workspace.readText(it) }
        val preview = target?.let { predictEdit(request.toolName, request.args, current) }
        mutableState.update { state ->
            state.copy(
                pending = state.pending + pending,
                previews =
                    if (preview == null) {
                        state.previews
                    } else {
                        state.previews + (pending.id to preview)
                    },
            )
        }
    }

    override fun onDismiss(
        pending: PendingPermission,
        reason: DismissReason,
    ) {
        mutableState.update { state ->
            state.copy(
                pending = state.pending.filterNot { it === pending },
                previews = state.previews - pending.id,
            )
        }
    }

    override fun onAutoAllow(toolName: String) {
        append(TranscriptEntry.Notice(nextId++, "$toolName is allowed for this session."))
    }

    override fun onPostError(
        toolName: String,
        error: Throwable,
    ) {
        append(
            TranscriptEntry.Notice(
                id = nextId++,
                text = "Could not answer the $toolName prompt: ${error.message.orEmpty()}",
                isError = true,
            ),
        )
    }

    /** Apply one stream event to the transcript and the progress state. */
    private suspend fun apply(
        event: ChatEvent,
        activePoster: PermissionPoster,
    ) {
        // Each step ignores the types it does not own, so high-volume token
        // and loop events still cost no recomposition.
        applyTranscript(event)
        applyPermission(event, activePoster)
        applyProgress(event)
        applyPlan(event)
    }

    /** The conversation itself: text, tools and the banner that ends a turn. */
    private suspend fun applyTranscript(event: ChatEvent) {
        when (event.type) {
            TEXT_EVENT -> {
                appendText(event.payloadOrNull<TextPayload>()?.content.orEmpty())
            }

            TOOL_CALL_EVENT -> {
                appendToolCall(event)
            }

            TOOL_RESULT_EVENT -> {
                appendToolResult(event)
            }

            DONE_EVENT -> {
                append(
                    TranscriptEntry.Done(
                        id = nextId++,
                        outcome = doneOutcome(event.payloadOrNull<DonePayload>()),
                    ),
                )
            }

            ERROR_EVENT -> {
                append(
                    TranscriptEntry.Notice(
                        id = nextId++,
                        text = event.payloadOrNull<ErrorPayload>()?.error.orEmpty(),
                        isError = true,
                    ),
                )
            }

            else -> {
                Unit
            }
        }
    }

    /** The prompts, which are state too: the panel and the notification share
     * them. */
    private fun applyPermission(
        event: ChatEvent,
        activePoster: PermissionPoster,
    ) {
        when (event.type) {
            PERMISSION_REQUEST_EVENT -> {
                event
                    .payloadOrNull<PermissionRequestPayload>()
                    ?.let { permissions.handleRequest(activePoster, turns.sessionId, it) }
            }

            PERMISSION_DENIED_EVENT -> {
                // The proxy answers a denial with permission_denied and then a
                // tool_result whose success is false (proxy/loop_checks.go), so that
                // result is what consumes the snapshot and the pre-op stat,
                // exactly as for a call that failed. A stream that ends before
                // it leaves nothing behind: the turn's finally clears both.
                val tool = event.payloadOrNull<PermissionDeniedPayload>()?.tool.orEmpty()
                permissions.handleDenied(tool)
            }

            else -> {
                Unit
            }
        }
    }

    /** The liveness line: model, V3 and lens events, and the event that clears
     * it. */
    private fun applyProgress(event: ChatEvent) {
        when (event.type) {
            TURN_START_EVENT, LLM_CALL_START_EVENT -> {
                setProgress("Thinking…")
            }

            LLM_PROMPT_PROGRESS_EVENT -> {
                val progress = event.payloadOrNull<LlmPromptProgressPayload>()
                val percent =
                    progress
                        ?.takeIf { it.pct > 0 }
                        ?.let { " ${(it.pct * 100).roundToInt()}%" }
                        .orEmpty()
                setProgress("Processing prompt$percent…")
            }

            LLM_FIRST_TOKEN_EVENT, LLM_CALL_END_EVENT -> {
                setProgress(null)
            }

            V3_PROGRESS_EVENT -> {
                val message = event.payloadOrNull<V3ProgressPayload>()?.message.orEmpty()
                setProgress(if (message.isEmpty()) null else "V3: $message")
            }

            in V3_STAGE_EVENTS -> {
                val stage = event.payloadOrNull<V3StagePayload>()
                val label = stage?.stage.orEmpty()
                val detail = stage?.detail.orEmpty()
                setProgress(
                    when {
                        label.isEmpty() -> null
                        detail.isEmpty() -> "V3: $label"
                        else -> "V3: $label — $detail"
                    },
                )
            }

            V3_LENS_VETO_EVENT, V3_STRUCTURAL_VETO_EVENT -> {
                val stage = event.payloadOrNull<V3StagePayload>()
                val why =
                    if (event.type ==
                        V3_LENS_VETO_EVENT
                    ) {
                        "lens quality"
                    } else {
                        "unresolved calls"
                    }
                val detail = stage?.detail.orEmpty()
                setProgress(
                    "V3: candidate rejected ($why)${if (detail.isEmpty()) "" else " — $detail"}",
                )
            }

            AGENT_LENS_INTERVENTION_EVENT -> {
                val reason = event.payloadOrNull<AgentLensInterventionPayload>()?.reason.orEmpty()
                setProgress("Lens intervention: ${reason.ifEmpty { "corrective queued" }}")
            }

            else -> {
                Unit
            }
        }
    }

    /** The plan and the adherence events that mark its steps. */
    private fun applyPlan(event: ChatEvent) {
        when (event.type) {
            PLAN_LOADED_EVENT -> {
                setPlan(event.payloadOrNull<PlanLoadedPayload>())
            }

            PLAN_ADHERENCE_EVENT -> {
                val adherence = event.payloadOrNull<PlanAdherencePayload>()
                if (adherence?.matched == true) {
                    adherence.stepId?.let(::satisfyPlanStep)
                }
            }

            PLAN_REVISE_EVENT -> {
                val reason = event.payloadOrNull<PlanRevisePayload>()?.reason.orEmpty()
                append(
                    TranscriptEntry.Notice(
                        nextId++,
                        "Plan going off track — revising (${reason.ifEmpty {
                            "off-plan streak"
                        }}).",
                    ),
                )
            }

            else -> {
                Unit
            }
        }
    }

    private fun setProgress(text: String?) {
        mutableState.update { it.copy(progress = text) }
    }

    private fun setPlan(loaded: PlanLoadedPayload?) {
        if (loaded == null) {
            return
        }
        val steps =
            loaded.steps.map { step ->
                val label = "${step.action} ${step.target}".trim()
                ChatPlanStep(step.id, label.ifEmpty { step.id })
            }
        mutableState.update {
            it.copy(plan = ChatPlan(steps, loaded.revision))
        }
    }

    private fun satisfyPlanStep(stepId: String) {
        mutableState.update { state ->
            val plan = state.plan ?: return@update state
            state.copy(plan = plan.copy(satisfied = plan.satisfied + stepId))
        }
    }

    /** A tool call: its chip, the pre-call snapshot and the pre-op stat. */
    private fun appendToolCall(event: ChatEvent) {
        val call = event.payloadOrNull<ToolCallPayload>()
        val name = call?.name.orEmpty()
        append(
            TranscriptEntry.Tool(
                id = nextId++,
                name = name,
                summary = null,
                success = null,
            ),
        )
        // Snapshot the target now (before permission/execution) so a successful
        // result can show the exact applied change. At tool_call time rather
        // than permission time: accept-edits and yolo turns never raise a
        // permission_request.
        editTargetPath(name, call?.args)?.let { target ->
            snapshots
                .getOrPut(name) { ArrayDeque() }
                .addLast(EditSnapshot(target, workspace.readText(target)))
        }
        mismatch.recordToolCall(name, call?.args)
    }

    /** A tool result: the chip's outcome, the applied change, the VFS refresh
     * and the post-op check. A failed result takes its queues and shows no
     * change — there is nothing on disk to verify. */
    private suspend fun appendToolResult(event: ChatEvent) {
        val result = event.payloadOrNull<ToolResultPayload>()
        val tool = result?.tool.orEmpty()
        val change = appliedChange(tool, result)
        append(
            TranscriptEntry.Tool(
                id = nextId++,
                name = tool,
                summary = summarizeToolResult(tool, result?.data),
                success = result?.success,
                change = change,
            ),
        )
        if (change != null) {
            // The proxy wrote through its bind mount; tell the IDE so a new file
            // shows up without waiting for its own refresh.
            workspace.changed(change.path)
        }
        mismatch.recordToolResult(tool, result?.success == true)
    }

    /**
     * Consume the snapshot for a finished tool_result and, when there is
     * something to show, capture the on-disk state and register the change.
     *
     * Read immediately: the proxy writes before emitting the result, and a
     * later read could already include the NEXT edit to the same file.
     */
    private fun appliedChange(
        tool: String,
        result: ToolResultPayload?,
    ): AppliedChange? {
        val snapshot = snapshots[tool]?.removeFirstOrNull() ?: return null
        if (result?.success != true) {
            return null
        }
        val after = workspace.readText(snapshot.path)
        val preview = if (tool == EDIT_FILE_TOOL) serverDiffPreview(result.data) else null
        if (after == null && preview == null) {
            return null // nothing viewable (likely a workspace mismatch)
        }
        return AppliedChange(snapshot.path, snapshot.before, after, preview)
    }

    /** Trail a delta onto the streaming assistant entry, or start one. */
    private fun appendText(delta: String) {
        if (delta.isEmpty()) {
            return
        }
        val last = mutableState.value.entries.lastOrNull() as? TranscriptEntry.Assistant
        if (last == null) {
            append(TranscriptEntry.Assistant(nextId++, delta))
        } else {
            mutableState.update { state ->
                state.copy(
                    entries = state.entries.dropLast(1) + last.copy(text = last.text + delta),
                )
            }
        }
    }

    private fun append(entry: TranscriptEntry) {
        mutableState.update { it.copy(entries = it.entries + entry) }
    }

    /** The file state captured at tool_call time. */
    private data class EditSnapshot(
        val path: String,
        val before: String?,
    )
}

/** `edit_file`'s server-computed unified diff, when the result carries one. */
private fun serverDiffPreview(data: JsonElement?): String? {
    val value = (data as? JsonObject)?.get("diff_preview") as? JsonPrimitive ?: return null
    return if (value.isString) value.content.ifEmpty { null } else null
}

// Turn state for the chat: session_id minting, rolling history, the session
// allowlist, the task contract and cancellation.
//
// A port of extensions/vscode/src/session/turnManager.ts. Where the VS Code
// client and the TUI (tui/chat.go) disagree, the TUI is the reference.
//
// Deliberately free of com.intellij imports: this layer runs under plain JUnit,
// with no IDE and no live proxy, which is what makes the turn semantics
// testable at all.
package com.inferstep.atlas.session

import com.inferstep.atlas.protocol.AgentRequest
import com.inferstep.atlas.protocol.ChatEvent
import com.inferstep.atlas.protocol.DonePayload
import com.inferstep.atlas.protocol.HistoryMessage
import com.inferstep.atlas.protocol.PermissionMode
import com.inferstep.atlas.protocol.TaskContract
import com.inferstep.atlas.protocol.TaskMode
import com.inferstep.atlas.protocol.TextPayload
import com.inferstep.atlas.protocol.assistantHistoryContent
import com.inferstep.atlas.protocol.payloadOrNull
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.channels.awaitClose
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.callbackFlow
import kotlinx.coroutines.launch
import java.security.SecureRandom

/** The proxy caps replayed history at the most recent 40 entries; the client
 * caps too, so the request body never grows unbounded (tui/chat.go). */
const val HISTORY_LIMIT = 40

/** The `done` event type, the one that carries the terminal summary. */
private const val DONE_EVENT = "done"

/** The `text` event type, a conversational reply delta. */
private const val TEXT_EVENT = "text"

private const val ROLE_USER = "user"
private const val ROLE_ASSISTANT = "assistant"

private val sessionIdRandom = SecureRandom()

/**
 * A client-minted per-turn session id: 12 random bytes, hex-encoded (24
 * characters) — the same recipe as tui/chat.go and the VS Code client. The
 * proxy keys `/cancel` and pending permission requests by it, so it has to be
 * unguessable and unique per turn.
 */
fun mintSessionId(): String {
    val bytes = ByteArray(12)
    sessionIdRandom.nextBytes(bytes)
    return bytes.joinToString(separator = "") { byte -> "%02x".format(byte) }
}

/**
 * Runs one agent turn at a time and owns what survives between turns: the
 * rolling history and the session allowlist.
 *
 * Not thread-safe by design: the UI drives it from one coroutine context, and
 * [busy] is what stops a second turn from starting.
 */
class TurnManager(
    /** Where the best-effort POST /cancel runs. The UI passes its own scope;
     * a test passes one it can wait on. */
    private val scope: CoroutineScope,
    private val historyLimit: Int = HISTORY_LIMIT,
) {
    private var history: List<HistoryMessage> = emptyList()

    /**
     * Tools the user approved with "allow for session", shared with
     * [PermissionFlow] and re-sent as `session_allowed_tools` on every later
     * turn. The client owns this list: the proxy does not persist it, which is
     * why an auto-answered request is posted with scope `once`.
     */
    val sessionAllowedTools: MutableSet<String> = linkedSetOf()

    private var stream: Job? = null
    private var activeClient: TurnClient? = null
    private var activeSessionId: String = ""

    /** True while a turn's event stream is being consumed. */
    val busy: Boolean
        get() = stream != null

    /** session_id of the in-flight (or most recent) turn — needed to route a
     * permission decision and to cancel. */
    val sessionId: String
        get() = activeSessionId

    /**
     * Run one turn: mint a session id, send the message with the rolling
     * history, the session allowlist and a `task_contract`, and emit the raw
     * event stream.
     *
     * Collect it once. The history is finalized when the stream ends — on
     * `done`, on error and on cancel alike — so a cancelled turn still replays
     * the partial assistant text it produced.
     *
     * A second concurrent turn fails with [IllegalStateException] when
     * collected; check [busy] to avoid it rather than catching it.
     */
    fun runTurn(
        client: TurnClient,
        message: String,
        mode: PermissionMode,
        taskMode: TaskMode,
        workingDir: String,
    ): Flow<ChatEvent> =
        callbackFlow {
            check(stream == null) { "a turn is already in progress" }

            val sessionId = mintSessionId()
            activeSessionId = sessionId
            activeClient = client

            val request =
                AgentRequest(
                    message = message,
                    workingDir = workingDir,
                    mode = mode,
                    sessionId = sessionId,
                    history = history.takeIf { it.isNotEmpty() },
                    sessionAllowedTools = sessionAllowedTools.toList().takeIf { it.isNotEmpty() },
                    taskContract = TaskContract(taskMode),
                )

            var assistantText = ""
            var doneSummary = ""

            stream =
                launch {
                    try {
                        client.agentTurn(request).collect { event ->
                            when (event.type) {
                                TEXT_EVENT -> {
                                    assistantText +=
                                        event.payloadOrNull<TextPayload>()?.content.orEmpty()
                                }

                                DONE_EVENT -> {
                                    // On a tool-shaped turn the model's final
                                    // answer arrives only here (proxy/agent.go):
                                    // no text event precedes it.
                                    doneSummary =
                                        event.payloadOrNull<DonePayload>()?.summary.orEmpty()
                                }
                            }
                            send(event)
                        }
                        close()
                    } catch (e: CancellationException) {
                        // cancel() aborted the collection. The channel is
                        // closed normally rather than failed, because the
                        // caller asked for this: the VS Code client surfaces an
                        // AbortError here, and a Kotlin consumer has the same
                        // information from having called cancel() itself.
                        // Close first, or a cancelled child would leave the
                        // collector waiting on a channel nobody closes.
                        close()
                        throw e
                    } catch (e: Throwable) {
                        close(e)
                    }
                }

            awaitClose {
                stream = null
                activeClient = null
                record(message, assistantText, doneSummary)
            }
        }

    /**
     * Cancel the in-flight turn: abort the local stream (the primary mechanism)
     * and fire a best-effort POST /cancel as defense in depth, which is what
     * the TUI does. A no-op when idle.
     */
    fun cancel() {
        val active = stream ?: return
        val client = activeClient
        val sessionId = activeSessionId

        active.cancel()
        if (client != null && sessionId.isNotEmpty()) {
            scope.launch {
                try {
                    client.cancelTurn(sessionId)
                } catch (_: CancellationException) {
                    // The scope going away is not a cancel failure: the local
                    // abort has already stopped the turn, and /cancel is only
                    // the proxy-side backstop.
                }
            }
        }
    }

    /** Start a fresh conversation: drop the history and the session-approved
     * tools. Call [cancel] first if a turn is in flight. */
    fun reset() {
        history = emptyList()
        sessionAllowedTools.clear()
    }

    /**
     * Append the finished turn to the rolling history.
     *
     * Streamed text wins; the `done` summary stands in for it on a tool-shaped
     * turn, so the next turn's history is not silent. A cancelled or failed
     * turn keeps whatever text it had produced.
     */
    private fun record(
        message: String,
        assistantText: String,
        doneSummary: String,
    ) {
        var entries = history + HistoryMessage(role = ROLE_USER, content = message)
        val finalText = assistantText.ifEmpty { doneSummary }
        if (finalText.isNotEmpty()) {
            entries = entries + HistoryMessage(ROLE_ASSISTANT, assistantHistoryContent(finalText))
        }
        history = if (entries.size > historyLimit) entries.takeLast(historyLimit) else entries
    }
}

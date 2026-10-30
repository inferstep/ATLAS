// The permission flow for mid-turn `permission_request` events.
//
// A port of extensions/vscode/src/session/permissionFlow.ts. Pure logic, no
// Composable and no Swing: the UI is injected through [PermissionUi], so the
// flow runs under plain JUnit.
//
// Conventions mirrored from tui/model.go:
//   - A tool already in the session allowlist is auto-answered allow/"once"
//     (NOT "session" — the allowlist itself is re-sent on the next turn's
//     request; the proxy does not persist it), fire-and-forget, no prompt.
//   - "Allow for session" adds the tool to the allowlist AND answers with
//     scope "session" so the in-flight turn honours it too.
//   - A deletion is never auto-answered, and "Allow for session" on it allows
//     this request only.
//   - A proxy-side permission_denied (timeout, cancel) can arrive while the
//     prompt is still up — the pending prompt is dismissed by tool name.
//   - Decision POSTs are advisory: the proxy has its own deny fail-safe, so a
//     post failure is reported to the UI for logging, never rethrown.
package com.inferstep.atlas.session

import com.inferstep.atlas.protocol.PermissionDecision
import com.inferstep.atlas.protocol.PermissionDecisionRequest
import com.inferstep.atlas.protocol.PermissionRequestPayload
import com.inferstep.atlas.protocol.PermissionScope
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.launch
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive

/** Where a decision goes. A `fun interface`, so a caller can pass
 * `PermissionPoster { client.postPermissionDecision(it) }`. */
fun interface PermissionPoster {
    suspend fun postPermissionDecision(decision: PermissionDecisionRequest)
}

/** What the user picked on a prompt. */
enum class PermissionChoice {
    ALLOW_ONCE,
    ALLOW_SESSION,
    DENY,
}

/** Why a pending prompt went away. */
enum class DismissReason {
    /** The user answered it. */
    ANSWERED,

    /** The proxy resolved it on its side (timeout or cancel). */
    DENIED_REMOTE,

    /** The turn ended with the prompt still open. */
    TURN_ENDED,
}

/** The UI surface the flow drives. Every callback is fire-and-forget from the
 * flow's point of view. */
interface PermissionUi {
    /** Show the prompt (an inline card in the tool window). */
    fun onPrompt(pending: PendingPermission)

    /** Remove or resolve the prompt. When [reason] is [DismissReason.ANSWERED],
     * [PendingPermission.choice] carries what the user picked. */
    fun onDismiss(
        pending: PendingPermission,
        reason: DismissReason,
    )

    /** A session-allowlisted tool was auto-answered without a prompt. */
    fun onAutoAllow(toolName: String)

    /** A decision POST failed (non-404). Advisory — log and move on. */
    fun onPostError(
        toolName: String,
        error: Throwable,
    )
}

/**
 * One live permission prompt.
 *
 * More than one surface can call [settle] for the same request (a keyboard
 * shortcut and the button); the first answer wins and later calls are no-ops.
 */
class PendingPermission(
    /** Stable id for routing a click back to this prompt. */
    val id: Long,
    /** session_id to echo on the decision POST. */
    val sessionId: String,
    val request: PermissionRequestPayload,
    private val onSettle: (PendingPermission, PermissionChoice) -> Unit,
) {
    /** The winning choice, once a user settled it. */
    var choice: PermissionChoice? = null
        private set

    private var settled = false

    val isSettled: Boolean
        get() = settled

    /** Record the user's answer. Returns true when this call decided the
     * request, false when another surface — or a remote deny — beat it. */
    fun settle(picked: PermissionChoice): Boolean {
        if (settled) {
            return false
        }
        settled = true
        choice = picked
        onSettle(this, picked)
        return true
    }

    /** Mark settled without a user choice (remote deny, turn end), so a late
     * click becomes a no-op. */
    fun dismiss() {
        settled = true
    }
}

/**
 * A request whose answer covers only itself.
 *
 * The proxy marks deletions `one_time_only`, and `delete_file` is treated so
 * for a proxy that does not. One "Allow for Session" used to answer every
 * later deletion in the conversation without the user seeing which file.
 */
fun isOneTimeOnly(request: PermissionRequestPayload): Boolean =
    request.oneTimeOnly || request.toolName == DELETE_FILE_TOOL

/** The tool whose approvals never outlive the file they name. */
private const val DELETE_FILE_TOOL = "delete_file"

/**
 * The command a request would run, shown whole.
 *
 * Approval is the only per-command control, and a detail cut short hides the
 * end of a chain. Null when the call runs no command.
 */
fun commandOf(args: JsonElement?): String? {
    val command = (args as? JsonObject)?.get("command") as? JsonPrimitive ?: return null
    return if (command.isString) command.content else null
}

/**
 * Tracks the open prompts for one turn and answers them on the proxy.
 *
 * Holds no coroutines of its own beyond the fire-and-forget decision POSTs,
 * which run on [scope].
 */
class PermissionFlow(
    /** Shared with [TurnManager] — an addition here is re-sent as
     * `session_allowed_tools` on every later turn. */
    private val sessionAllowedTools: MutableSet<String>,
    private val ui: PermissionUi,
    private val scope: CoroutineScope,
) {
    private var pending: List<PendingPermission> = emptyList()
    private var nextId = 1L

    /** Handle a `permission_request` event from the stream. */
    fun handleRequest(
        poster: PermissionPoster,
        sessionId: String,
        request: PermissionRequestPayload,
    ) {
        val once = isOneTimeOnly(request)
        if (!once && sessionAllowedTools.contains(request.toolName)) {
            post(poster, sessionId, request, PermissionDecision.ALLOW, PermissionScope.ONCE)
            ui.onAutoAllow(request.toolName)
            return
        }

        val prompt =
            PendingPermission(nextId++, sessionId, request) { settled, picked ->
                pending = pending.filterNot { it === settled }
                val forSession = picked == PermissionChoice.ALLOW_SESSION && !once
                if (forSession) {
                    sessionAllowedTools.add(request.toolName)
                }
                post(
                    poster = poster,
                    sessionId = sessionId,
                    request = request,
                    decision =
                        if (picked == PermissionChoice.DENY) {
                            PermissionDecision.DENY
                        } else {
                            PermissionDecision.ALLOW
                        },
                    permissionScope =
                        if (forSession) PermissionScope.SESSION else PermissionScope.ONCE,
                )
                ui.onDismiss(settled, DismissReason.ANSWERED)
            }
        pending = pending + prompt
        ui.onPrompt(prompt)
    }

    /** Handle a `permission_denied` event: the proxy resolved the request on
     * its side (timeout or cancel), so dismiss the oldest matching prompt. */
    fun handleDenied(toolName: String) {
        val match = pending.firstOrNull { it.request.toolName == toolName } ?: return
        match.dismiss()
        pending = pending.filterNot { it === match }
        ui.onDismiss(match, DismissReason.DENIED_REMOTE)
    }

    /** The turn is over (done, error or cancel) — nothing is left to answer. */
    fun endTurn() {
        val open = pending
        pending = emptyList()
        open.forEach { prompt ->
            prompt.dismiss()
            ui.onDismiss(prompt, DismissReason.TURN_ENDED)
        }
    }

    /** Route an answer by prompt id. An unknown or already-settled id is a
     * no-op, which is what a stale card after a turn ends looks like. */
    fun settleById(
        id: Long,
        choice: PermissionChoice,
    ): Boolean {
        val prompt = pending.firstOrNull { it.id == id } ?: return false
        return prompt.settle(choice)
    }

    private fun post(
        poster: PermissionPoster,
        sessionId: String,
        request: PermissionRequestPayload,
        decision: PermissionDecision,
        permissionScope: PermissionScope,
    ) {
        // `this@PermissionFlow.scope` is the coroutine scope; the parameter is
        // the wire-level permission scope, and the two names must not collide.
        this.scope.launch {
            try {
                poster.postPermissionDecision(
                    PermissionDecisionRequest(
                        sessionId = sessionId,
                        toolCallId = request.toolCallId,
                        decision = decision,
                        scope = permissionScope,
                    ),
                )
            } catch (e: CancellationException) {
                throw e
            } catch (e: Throwable) {
                ui.onPostError(request.toolName, e)
            }
        }
    }
}

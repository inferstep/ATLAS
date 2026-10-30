// Native notifications for the two moments the user may not be looking at the
// tool window: a permission request, and a turn that ended with an error or a
// workspace warning.
//
// The session state is the source, not the panel, so closing the tool window
// does not silence a prompt. The inline card and the notification answer the
// same PendingPermission; whichever is clicked first wins.
package com.inferstep.atlas.ui

import com.inferstep.atlas.ide.ATLAS_TOOL_WINDOW_ID
import com.inferstep.atlas.protocol.PermissionRequestPayload
import com.inferstep.atlas.session.ChatSession
import com.inferstep.atlas.session.ChatUiState
import com.inferstep.atlas.session.EditPrediction
import com.inferstep.atlas.session.PendingPermission
import com.inferstep.atlas.session.PermissionChoice
import com.inferstep.atlas.session.TranscriptEntry
import com.inferstep.atlas.session.commandOf
import com.inferstep.atlas.session.diffTitle
import com.inferstep.atlas.session.editTargetPath
import com.inferstep.atlas.session.isOneTimeOnly
import com.inferstep.atlas.settings.AtlasConfigurable
import com.intellij.notification.Notification
import com.intellij.notification.NotificationAction
import com.intellij.notification.NotificationGroup
import com.intellij.notification.NotificationGroupManager
import com.intellij.notification.NotificationType
import com.intellij.openapi.Disposable
import com.intellij.openapi.application.ApplicationManager
import com.intellij.openapi.options.ShowSettingsUtil
import com.intellij.openapi.project.Project
import com.intellij.openapi.wm.ToolWindowManager
import com.intellij.xml.util.XmlStringUtil
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.launch

/** The notification group registered in `plugin.xml`. */
private const val NOTIFICATION_GROUP = "ATLAS"

/**
 * Notification content is HTML, and the platform does not escape it. The
 * request text and the error text come from the proxy, so a command containing
 * `<` or `&` must be escaped to read as the command that will run rather than
 * as markup.
 */
internal fun notificationContent(text: String): String = XmlStringUtil.escapeString(text)

class AtlasNotifier(
    private val project: Project,
    private val session: ChatSession,
    scope: CoroutineScope,
) : Disposable {
    private val group: NotificationGroup? by lazy {
        NotificationGroupManager.getInstance().getNotificationGroup(NOTIFICATION_GROUP)
    }

    /** How many transcript entries have already been offered as notifications. */
    private var seenEntries = 0
    private var seenPending = emptySet<Long>()
    private val openPrompts = mutableMapOf<Long, Notification>()
    private val job: Job = scope.launch { session.state.collect(::onState) }

    private fun onState(state: ChatUiState) {
        // A reset empties the transcript; the next entry is new again.
        if (state.entries.size < seenEntries) {
            seenEntries = 0
        }
        val fresh = state.entries.drop(seenEntries)
        if (fresh.isNotEmpty()) {
            seenEntries = state.entries.size
            fresh.filterIsInstance<TranscriptEntry.Notice>().forEach(::notifyNotice)
        }

        val ids = state.pending.map { it.id }.toSet()
        val added = state.pending.filter { it.id !in seenPending }
        val removed = seenPending - ids
        seenPending = ids
        if (added.isEmpty() && removed.isEmpty()) {
            return
        }
        // Notifications are an EDT concern; the collector runs on the service
        // scope's default dispatcher.
        ApplicationManager.getApplication().invokeLater {
            removed.forEach { id -> openPrompts.remove(id)?.expire() }
            added.forEach { prompt -> showPrompt(prompt, state.previews[prompt.id]) }
        }
    }

    private fun notifyNotice(entry: TranscriptEntry.Notice) {
        val type =
            when {
                entry.isError -> NotificationType.ERROR
                entry.isWarning -> NotificationType.WARNING
                else -> return
            }
        val title = if (entry.isError) "ATLAS turn failed" else "ATLAS workspace warning"
        ApplicationManager.getApplication().invokeLater {
            val notification =
                group
                    ?.createNotification(title, notificationContent(entry.text), type)
                    ?: return@invokeLater
            notification.addAction(
                NotificationAction.createSimpleExpiring("Open Chat") { openChat() },
            )
            if (entry.isError) {
                notification.addAction(
                    NotificationAction.createSimpleExpiring("Settings") {
                        ShowSettingsUtil
                            .getInstance()
                            .showSettingsDialog(project, AtlasConfigurable::class.java)
                    },
                )
            }
            notification.notify(project)
        }
    }

    private fun showPrompt(
        prompt: PendingPermission,
        preview: EditPrediction?,
    ) {
        val notificationGroup = group ?: return
        val request = prompt.request
        val content = notificationContent(requestContent(request))
        val notification =
            notificationGroup.createNotification(
                "ATLAS needs approval",
                content,
                NotificationType.INFORMATION,
            )
        if (preview != null) {
            notification.addAction(
                NotificationAction.createSimple("View Diff") {
                    AtlasDiffViewer.show(
                        project,
                        preview.diffTitle(request.toolName),
                        preview.path,
                        preview.left,
                        preview.right,
                    )
                },
            )
        }
        notification.addAction(
            NotificationAction.createSimpleExpiring("Allow Once") {
                session.answer(prompt.id, PermissionChoice.ALLOW_ONCE)
            },
        )
        if (!isOneTimeOnly(request)) {
            notification.addAction(
                NotificationAction.createSimpleExpiring("Allow for Session") {
                    session.answer(prompt.id, PermissionChoice.ALLOW_SESSION)
                },
            )
        }
        notification.addAction(
            NotificationAction.createSimpleExpiring("Deny") {
                session.answer(prompt.id, PermissionChoice.DENY)
            },
        )
        openPrompts[prompt.id] = notification
        notification.notify(project)
    }

    /** The request line plus the whole command or path, which a notification
     * may wrap but never cuts without saying so. */
    private fun requestContent(request: PermissionRequestPayload): String {
        val detail =
            commandOf(request.args)
                ?: request.canonicalPath
                ?: editTargetPath(request.toolName, request.args)
        val message = request.message.ifEmpty { "ATLAS wants to run '${request.toolName}'." }
        return if (detail.isNullOrBlank()) message else "$message\n$detail"
    }

    private fun openChat() {
        ToolWindowManager.getInstance(project).getToolWindow(ATLAS_TOOL_WINDOW_ID)?.show()
    }

    override fun dispose() {
        job.cancel()
        ApplicationManager.getApplication().invokeLater {
            openPrompts.values.forEach { notification -> notification.expire() }
            openPrompts.clear()
        }
    }
}

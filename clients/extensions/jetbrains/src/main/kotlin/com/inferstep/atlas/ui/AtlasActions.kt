// The actions the IDE, the status bar menu and the tool window share.
//
// Each one delegates to AtlasProjectService, so the action and the panel
// operate the same session rather than a second copy of it.
package com.inferstep.atlas.ui

import com.inferstep.atlas.ide.AtlasProjectService
import com.intellij.notification.NotificationGroupManager
import com.intellij.notification.NotificationType
import com.intellij.openapi.actionSystem.ActionUpdateThread
import com.intellij.openapi.actionSystem.AnActionEvent
import com.intellij.openapi.project.DumbAwareAction
import com.intellij.openapi.ui.Messages

class OpenChatAction : DumbAwareAction() {
    override fun actionPerformed(event: AnActionEvent) {
        val project = event.project ?: return
        AtlasProjectService.getInstance(project).openChat()
    }
}

class CancelTurnAction : DumbAwareAction() {
    override fun getActionUpdateThread(): ActionUpdateThread = ActionUpdateThread.EDT

    override fun update(event: AnActionEvent) {
        val project = event.project
        event.presentation.isEnabled =
            project != null &&
            AtlasProjectService
                .getInstance(project)
                .session.state.value.running
    }

    override fun actionPerformed(event: AnActionEvent) {
        val project = event.project ?: return
        AtlasProjectService.getInstance(project).cancel()
    }
}

class NewConversationAction : DumbAwareAction() {
    override fun actionPerformed(event: AnActionEvent) {
        val project = event.project ?: return
        AtlasProjectService.getInstance(project).newConversation()
    }
}

class RefreshStatusAction : DumbAwareAction() {
    override fun actionPerformed(event: AnActionEvent) {
        val project = event.project ?: return
        AtlasProjectService.getInstance(project).requestStatusRefresh()
    }
}

class OpenAtlasSettingsAction : DumbAwareAction() {
    override fun actionPerformed(event: AnActionEvent) {
        val project = event.project ?: return
        AtlasProjectService.getInstance(project).openSettings()
    }
}

class SetServiceTokenAction : DumbAwareAction() {
    override fun actionPerformed(event: AnActionEvent) {
        val project = event.project ?: return
        val token =
            Messages.showPasswordDialog(
                project,
                "Bearer token for the ATLAS proxy. Leave empty to remove the stored token.",
                "ATLAS: Set Service Token",
                null,
            ) ?: return
        val cleared = token.isBlank()
        AtlasProjectService.getInstance(project).setToken(token.takeUnless { cleared })
        NotificationGroupManager
            .getInstance()
            .getNotificationGroup("ATLAS")
            ?.createNotification(
                "ATLAS",
                if (cleared) {
                    "Service token cleared."
                } else {
                    "Service token saved to the IDE credential store."
                },
                NotificationType.INFORMATION,
            )?.notify(project)
    }
}

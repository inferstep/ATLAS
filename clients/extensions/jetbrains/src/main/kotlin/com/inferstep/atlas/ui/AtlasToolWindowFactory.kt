package com.inferstep.atlas.ui

import androidx.compose.ui.awt.ComposePanel
import com.inferstep.atlas.ide.AtlasProjectService
import com.intellij.openapi.project.DumbAware
import com.intellij.openapi.project.Project
import com.intellij.openapi.wm.ToolWindow
import com.intellij.openapi.wm.ToolWindowFactory
import com.intellij.ui.content.ContentFactory
import org.jetbrains.jewel.bridge.theme.SwingBridgeTheme

/**
 * Mounts the ATLAS chat panel.
 *
 * The panel is only the renderer; the conversation, its connection and the
 * token live in [AtlasProjectService], so the tool window can be closed and
 * reopened without losing a turn and the actions and status bar operate the
 * same session.
 *
 * `project.basePath` is sent as the turn's `working_dir`, the same value the
 * TUI sends as its cwd: on Docker installs the proxy uses it for
 * host-to-container path translation, and on bare metal file operations
 * resolve against it.
 */
class AtlasToolWindowFactory :
    ToolWindowFactory,
    DumbAware {
    override fun createToolWindowContent(
        project: Project,
        toolWindow: ToolWindow,
    ) {
        val service = AtlasProjectService.getInstance(project)
        val panel = ComposePanel()
        panel.setContent {
            SwingBridgeTheme {
                atlasChatPanel(service)
            }
        }
        val content = ContentFactory.getInstance().createContent(panel, "", false)
        toolWindow.contentManager.addContent(content)
    }
}

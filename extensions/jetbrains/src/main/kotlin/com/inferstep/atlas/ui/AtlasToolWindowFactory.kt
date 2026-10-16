package com.inferstep.atlas.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.awt.ComposePanel
import androidx.compose.ui.unit.dp
import com.intellij.openapi.project.DumbAware
import com.intellij.openapi.project.Project
import com.intellij.openapi.wm.ToolWindow
import com.intellij.openapi.wm.ToolWindowFactory
import com.intellij.ui.content.ContentFactory
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flow
import org.jetbrains.jewel.bridge.theme.SwingBridgeTheme
import org.jetbrains.jewel.intui.markdown.bridge.ProvideMarkdownStyling
import org.jetbrains.jewel.markdown.Markdown
import org.jetbrains.jewel.ui.component.Text

class AtlasToolWindowFactory :
    ToolWindowFactory,
    DumbAware {
    override fun createToolWindowContent(
        project: Project,
        toolWindow: ToolWindow,
    ) {
        val panel = ComposePanel()
        panel.setContent {
            SwingBridgeTheme {
                atlasStageOneSpike(project)
            }
        }
        val content = ContentFactory.getInstance().createContent(panel, "", false)
        toolWindow.contentManager.addContent(content)
    }
}

/** One frame of the stub stream: an event name and its data, as SSE carries them. */
private data class StubFrame(
    val event: String,
    val data: String,
)

/**
 * Stands in for the proxy's stream so the spike can be exercised with no
 * proxy running. Shapes only: the real event names and JSON payloads belong
 * to the Stage 2 client layer. Every frame is emitted separately, with a
 * delay, so the UI has to recompose incrementally the way the real stream
 * makes it rather than rendering a list that was there from the start.
 */
private fun stubProxyStream(): Flow<StubFrame> =
    flow {
        emit(StubFrame("progress", "tool_call  read_file  proxy/server.go"))
        delay(300)
        emit(StubFrame("progress", "v3  stage 2/4  candidate accepted"))
        delay(300)
        emit(StubFrame("assistant_delta", "Here is the plan for the next stage:\n"))
        delay(200)
        emit(StubFrame("assistant_delta", "1. open the SSE stream and parse the envelope\n"))
        delay(200)
        emit(StubFrame("assistant_delta", "2. send the task contract on every turn\n"))
        delay(200)
        emit(StubFrame("assistant_delta", "3. show done.status and reason, not just the summary\n"))
        delay(200)
        emit(StubFrame("done", "status: ok"))
    }

/**
 * Stage 1 spike only, and the whole of what this stage renders. It answers
 * the three questions the stage asked: Compose mounts inside the Swing tool
 * window, the stream recomposes incrementally, and Jewel paints with the
 * IDE's own theme.
 *
 * The third answer is partial. Jewel's Markdown renderer ships in every
 * 2026.1 product as a platform content module rather than a plugin, so it is
 * named in the descriptor as a `<module>` and never as a `<depends>`: the
 * latter takes a plugin id and only disables the plugin. Assistant text is
 * rendered with Jewel's Markdown renderer, and the progress lines with
 * Jewel [Text]; none of this is the final chat UI.
 *
 * There is no HTTP and no protocol code here; the client layer, sessions and
 * permissions are Stage 2 and later.
 */
@Composable
private fun atlasStageOneSpike(project: Project) {
    var progress by remember { mutableStateOf(listOf("Connecting to the local proxy…")) }
    var reply by remember { mutableStateOf("") }

    LaunchedEffect(Unit) {
        stubProxyStream().collect { frame ->
            if (frame.event == "assistant_delta") {
                reply += frame.data
            } else {
                progress = progress + frame.data
            }
        }
    }

    Column(
        modifier = Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(12.dp),
        verticalArrangement = Arrangement.spacedBy(6.dp),
    ) {
        Text("ATLAS — Stage 1 spike (stub stream, no proxy)")
        progress.forEach { line -> Text(line) }
        if (reply.isNotEmpty()) {
            // ProvideMarkdownStyling(project) paints the Markdown with the
            // IDE's own theme, the same way SwingBridgeTheme does for the
            // surrounding Compose content.
            ProvideMarkdownStyling(project) {
                Markdown(reply)
            }
        }
    }
}

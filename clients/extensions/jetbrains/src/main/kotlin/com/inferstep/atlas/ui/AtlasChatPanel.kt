// The ATLAS chat panel: a renderer of ChatUiState and nothing else.
//
// Everything the panel shows is decided in session/ChatSession, which is free
// of Compose and of com.intellij, so the conversation semantics are tested
// under plain JUnit and this file only has to lay them out. The session itself
// belongs to AtlasProjectService, so closing the tool window does not forget a
// conversation and an action can cancel or reset the same turn the panel shows.
//
// Drawn with Compose foundation plus Jewel's Text and Markdown renderer. There
// is no Material dependency: the IDE provides Jewel, and the tool window is
// expected to look like the IDE's own panels.
package com.inferstep.atlas.ui

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.foundation.verticalScroll
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.unit.dp
import com.inferstep.atlas.ide.AtlasProjectService
import com.inferstep.atlas.protocol.PermissionMode
import com.inferstep.atlas.protocol.TaskMode
import com.inferstep.atlas.session.AppliedChange
import com.inferstep.atlas.session.ChatPlan
import com.inferstep.atlas.session.ChatSession
import com.inferstep.atlas.session.ChatUiState
import com.inferstep.atlas.session.EditPrediction
import com.inferstep.atlas.session.PendingPermission
import com.inferstep.atlas.session.PermissionChoice
import com.inferstep.atlas.session.TranscriptEntry
import com.inferstep.atlas.session.commandOf
import com.inferstep.atlas.session.diffTitle
import com.inferstep.atlas.session.isOneTimeOnly
import com.intellij.openapi.project.Project
import org.jetbrains.jewel.markdown.Markdown
import org.jetbrains.jewel.ui.component.Text
import javax.swing.UIManager

private val ERROR_COLOR = Color(0xFFCC3333)
private val WARNING_COLOR = Color(0xFFB26A00)

/**
 * The chat panel for one project.
 *
 * The connection, the session and the token all belong to
 * [AtlasProjectService]; this composable reads them and draws them.
 */
@Composable
fun atlasChatPanel(service: AtlasProjectService) {
    val session = service.session
    val state by session.state.collectAsState()
    val connection by service.connection.collectAsState()

    // Ask which folder the proxy edits when the panel first appears, so a
    // misaligned install is visible before the first edit lands somewhere the
    // user cannot see.
    LaunchedEffect(session) { session.checkWorkspace() }

    Column(
        modifier =
            Modifier
                .fillMaxSize()
                .padding(12.dp),
        verticalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        header(connection.url, service)
        modeControls(state, session, service)
        progressLine(state)
        planView(state.plan)
        transcript(state, service.project, Modifier.fillMaxWidth().weight(1f))
        pendingPrompts(state, session, service.project)
        composer(state, service)
    }
}

@Composable
private fun header(
    url: String,
    service: AtlasProjectService,
) {
    Row(
        modifier = Modifier.fillMaxWidth(),
        horizontalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        Text("ATLAS — $url", modifier = Modifier.weight(1f))
        actionText("Settings") { service.openSettings() }
        actionText("New chat") { service.newConversation() }
    }
}

/** The two controls the user operates. Nothing here reads the message text. */
@Composable
private fun modeControls(
    state: ChatUiState,
    session: ChatSession,
    service: AtlasProjectService,
) {
    Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
        Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
            Text("Task:")
            choiceChip("Work", state.taskMode == TaskMode.WORK) {
                session.setTaskMode(TaskMode.WORK)
            }
            choiceChip("Question", state.taskMode == TaskMode.QUESTION) {
                session.setTaskMode(TaskMode.QUESTION)
            }
        }
        Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
            Text("Permissions:")
            PermissionMode.entries.forEach { mode ->
                choiceChip(mode.wireName(), state.permissionMode == mode) {
                    service.setPermissionMode(mode)
                }
            }
        }
    }
}

/** The transient liveness line: what the model or V3 pipeline is doing now. */
@Composable
private fun progressLine(state: ChatUiState) {
    val line = state.progress ?: if (state.running) "Working…" else null
    line?.let { Text(it) }
}

/** The plan the proxy generated, with the steps it says are satisfied. */
@Composable
private fun planView(plan: ChatPlan?) {
    if (plan == null || plan.steps.isEmpty()) {
        return
    }
    Column(verticalArrangement = Arrangement.spacedBy(2.dp)) {
        Text(if (plan.revision == 0) "Plan" else "Plan · revision ${plan.revision}")
        plan.steps.forEach { step ->
            val mark = if (step.id in plan.satisfied) "[x]" else "[ ]"
            Text("$mark ${step.label}")
        }
    }
}

@Composable
private fun choiceChip(
    label: String,
    selected: Boolean,
    onClick: () -> Unit,
) {
    val text = if (selected) "[$label]" else label
    Text(text, modifier = Modifier.clickable(onClick = onClick))
}

@Composable
private fun transcript(
    state: ChatUiState,
    project: Project,
    modifier: Modifier = Modifier,
) {
    Column(
        modifier = modifier.verticalScroll(rememberScrollState()),
        verticalArrangement = Arrangement.spacedBy(6.dp),
    ) {
        state.entries.forEach { entry ->
            transcriptRow(entry, project)
        }
    }
}

@Composable
private fun transcriptRow(
    entry: TranscriptEntry,
    project: Project,
) {
    when (entry) {
        is TranscriptEntry.User -> {
            Text("You: ${entry.text}")
        }

        is TranscriptEntry.Assistant -> {
            Markdown(entry.text)
        }

        is TranscriptEntry.Tool -> {
            Text(toolLine(entry))
            // The exact change the call made, when the snapshot and the disk
            // state could both be read here. A read-only tool has none.
            entry.change?.let { change ->
                actionText("View change") { showAppliedChange(project, change) }
            }
        }

        is TranscriptEntry.Notice -> {
            noticeLine(entry)
        }

        is TranscriptEntry.Done -> {
            Text(doneLine(entry))
        }
    }
}

/** An error is the client's own words about a failure, so it is marked; a
 * warning is the workspace mismatch, which is not a failure of the turn. */
@Composable
private fun noticeLine(entry: TranscriptEntry.Notice) {
    when {
        entry.isError -> Text("ATLAS error: ${entry.text}", color = ERROR_COLOR)
        entry.isWarning -> Text("ATLAS warning: ${entry.text}", color = WARNING_COLOR)
        else -> Text(entry.text)
    }
}

/** A tool chip: what ran, how it went, and the one-line answer it produced. */
private fun toolLine(entry: TranscriptEntry.Tool): String {
    val mark =
        when (entry.success) {
            true -> {
                "done"
            }

            false -> {
                "failed"
            }

            null -> {
                "running"
            }
        }
    val summary = entry.summary?.let { " — $it" }.orEmpty()
    return "tool ${entry.name} ($mark)$summary"
}

/** The banner that says how the run ended — never just the summary (#236). */
private fun doneLine(entry: TranscriptEntry.Done): String {
    val outcome = entry.outcome
    val reason = if (outcome.reason.isEmpty()) "" else " (${outcome.reason})"
    return "Done — ${outcome.label}$reason: ${outcome.text}"
}

@Composable
private fun pendingPrompts(
    state: ChatUiState,
    session: ChatSession,
    project: Project,
) {
    state.pending.forEach { prompt ->
        permissionCard(prompt, state.previews[prompt.id], session, project)
    }
}

/**
 * The approval card.
 *
 * The whole command is shown: approval is the only per-command control, and a
 * truncated detail hides the end of a chain. A deletion gets no "allow for
 * session" button, which is the UI half of the rule the flow enforces.
 */
@Composable
private fun permissionCard(
    prompt: PendingPermission,
    preview: EditPrediction?,
    session: ChatSession,
    project: Project,
) {
    val request = prompt.request
    val heading = request.message.ifEmpty { "${request.toolName} needs approval" }

    Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
        Text(heading)
        request.canonicalPath?.let { path ->
            Text(path)
        }
        commandOf(request.args)?.let { command ->
            Text(command)
        }
        // The prediction is computed from the file as it is now, which is the
        // only moment it can be shown: after the call the file has changed.
        preview?.let { predicted ->
            actionText("View diff") {
                AtlasDiffViewer.show(
                    project,
                    predicted.diffTitle(request.toolName),
                    predicted.path,
                    predicted.left,
                    predicted.right,
                )
            }
        }
        Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
            actionText("Allow once") {
                session.answer(prompt.id, PermissionChoice.ALLOW_ONCE)
            }
            if (!isOneTimeOnly(request)) {
                actionText("Allow for session") {
                    session.answer(prompt.id, PermissionChoice.ALLOW_SESSION)
                }
            }
            actionText("Deny") {
                session.answer(prompt.id, PermissionChoice.DENY)
            }
        }
    }
}

@Composable
private fun actionText(
    label: String,
    onClick: () -> Unit,
) {
    Text(label, modifier = Modifier.clickable(onClick = onClick))
}

@Composable
private fun composer(
    state: ChatUiState,
    service: AtlasProjectService,
) {
    var draft by remember { mutableStateOf("") }

    Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
        BasicTextField(
            value = draft,
            onValueChange = { typed -> draft = typed },
            modifier = Modifier.fillMaxWidth(0.8f),
            // The IDE's own field foreground, so the text is readable in light
            // and dark themes alike.
            textStyle = TextStyle(color = ideColor("TextField.foreground")),
        )
        if (state.running) {
            actionText("Cancel") { service.cancel() }
        } else {
            actionText("Send") {
                val message = draft
                draft = ""
                service.send(message)
            }
        }
    }
}

/** The exact change a finished file op made, or the server's diff text when
 * there is nothing local to compare against. */
private fun showAppliedChange(
    project: Project,
    change: AppliedChange,
) {
    if (change.after != null) {
        AtlasDiffViewer.show(
            project,
            change.title(),
            change.path,
            change.before.orEmpty(),
            change.after,
        )
    } else {
        change.serverPreview?.let { preview ->
            AtlasDiffViewer.show(
                project,
                change.serverPreviewTitle(),
                change.path,
                "",
                preview,
            )
        }
    }
}

/** A Swing UI-default colour as a Compose one; the IDE theme owns the value. */
private fun ideColor(key: String): Color {
    val awt = UIManager.getColor(key) ?: return Color.Unspecified
    return Color(awt.red, awt.green, awt.blue, awt.alpha)
}

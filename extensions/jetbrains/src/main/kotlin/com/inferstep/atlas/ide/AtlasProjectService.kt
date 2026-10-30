// The project's one ATLAS conversation, and the connection it uses.
//
// The panel, the status bar and the actions all need the same session: the
// panel may be closed while a turn streams, an action must be able to cancel
// it, and a settings change must reach whichever surface is open. Keeping the
// session here (rather than in the Compose content) is what makes those three
// true.
package com.inferstep.atlas.ide

import com.inferstep.atlas.client.AtlasClient
import com.inferstep.atlas.client.AtlasConnection
import com.inferstep.atlas.protocol.PermissionMode
import com.inferstep.atlas.session.AtlasTurnClient
import com.inferstep.atlas.session.ChatSession
import com.inferstep.atlas.session.LocalWorkspace
import com.inferstep.atlas.settings.ATLAS_SETTINGS_TOPIC
import com.inferstep.atlas.settings.AtlasConfigurable
import com.inferstep.atlas.settings.AtlasSettings
import com.inferstep.atlas.settings.AtlasSettingsListener
import com.inferstep.atlas.settings.AtlasTokenStore
import com.inferstep.atlas.ui.AtlasNotifier
import com.inferstep.atlas.ui.refreshWorkspacePath
import com.intellij.openapi.Disposable
import com.intellij.openapi.components.Service
import com.intellij.openapi.components.service
import com.intellij.openapi.options.ShowSettingsUtil
import com.intellij.openapi.project.Project
import com.intellij.openapi.wm.ToolWindowManager
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asSharedFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.launch

/** The tool window id used by the descriptor, the actions and the factory. */
const val ATLAS_TOOL_WINDOW_ID = "ATLAS"

@Service(Service.Level.PROJECT)
class AtlasProjectService(
    val project: Project,
) : Disposable {
    val settings = AtlasSettings.getInstance()
    private val tokens = AtlasTokenStore.getInstance()

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    private val workingDir = project.basePath.orEmpty()
    private val workspace =
        LocalWorkspace(workingDir) { path -> refreshWorkspacePath(workingDir, path) }
    private val clients = mutableListOf<AtlasClient>()

    val session: ChatSession =
        openGateway().let { gateway ->
            ChatSession(gateway, gateway, scope, workingDir, workspace, gateway)
        }

    private val mutableConnection = MutableStateFlow(currentConnection())
    val connection: StateFlow<AtlasConnection> = mutableConnection.asStateFlow()

    private val mutableStatusRefresh = MutableSharedFlow<Unit>(extraBufferCapacity = 1)
    val statusRefreshRequests: SharedFlow<Unit> = mutableStatusRefresh.asSharedFlow()

    private val notifier = AtlasNotifier(project, session, scope)

    init {
        session.setPermissionMode(settings.permissionMode)
        project.messageBus.connect(this).subscribe(
            ATLAS_SETTINGS_TOPIC,
            AtlasSettingsListener { applySettings() },
        )
    }

    /** Start a turn from the panel or an action. */
    fun send(message: String) {
        scope.launch { session.send(message) }
    }

    /** Stop the in-flight turn, if any. */
    fun cancel() {
        session.cancel()
    }

    /**
     * Cancel any turn and start a fresh conversation once it has closed. The
     * reset has to wait for the turn's `finally`, or its history would be
     * written back into the conversation that was just cleared.
     */
    fun newConversation() {
        session.cancel()
        scope.launch {
            session.state.first { !it.running }
            session.reset()
        }
    }

    /** Persist a permission mode and apply it to the current session. */
    fun setPermissionMode(mode: PermissionMode) {
        settings.permissionMode = mode
        session.setPermissionMode(mode)
        settings.notifyChanged()
    }

    fun openChat() {
        ToolWindowManager.getInstance(project).getToolWindow(ATLAS_TOOL_WINDOW_ID)?.show()
    }

    fun openSettings() {
        ShowSettingsUtil.getInstance().showSettingsDialog(project, AtlasConfigurable::class.java)
    }

    fun setToken(token: String?) {
        tokens.setToken(token)
        settings.notifyChanged()
    }

    fun requestStatusRefresh() {
        mutableStatusRefresh.tryEmit(Unit)
    }

    private fun currentConnection(): AtlasConnection = settings.connection(tokens.token())

    private fun openGateway(): AtlasTurnClient {
        val connection = currentConnection()
        val client = AtlasClient(connection.url, connection.token)
        clients += client
        return AtlasTurnClient(client)
    }

    /** A settings change: swap the connection only when it actually moved. */
    private fun applySettings() {
        val next = currentConnection()
        if (next != mutableConnection.value) {
            val nextGateway = openGateway()
            session.updateConnection(nextGateway, nextGateway, nextGateway)
            mutableConnection.value = next
            scope.launch { session.checkWorkspace() }
        }
        session.setPermissionMode(settings.permissionMode)
        mutableStatusRefresh.tryEmit(Unit)
    }

    override fun dispose() {
        notifier.dispose()
        scope.cancel()
        clients.forEach { client -> runCatching { client.close() } }
        clients.clear()
    }

    companion object {
        fun getInstance(project: Project): AtlasProjectService = project.service()
    }
}

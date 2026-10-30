// The ATLAS status bar item: /ready at a configurable interval, a token probe
// against an authenticated route, calibration on an explicit refresh, and a
// click menu for the common actions.
//
// Polling pauses while a turn streams: the live SSE connection is stronger
// evidence of connectivity than a probe, and /ready hits four downstream
// checks. The item refreshes immediately when the turn ends and when settings
// or the token change.
package com.inferstep.atlas.ui

import com.inferstep.atlas.client.AtlasClient
import com.inferstep.atlas.client.AtlasConnection
import com.inferstep.atlas.client.AtlasStatus
import com.inferstep.atlas.client.TokenProbe
import com.inferstep.atlas.client.rejectedAfter
import com.inferstep.atlas.client.rejectionApplies
import com.inferstep.atlas.client.statusFromAnswers
import com.inferstep.atlas.client.statusFromFailure
import com.inferstep.atlas.ide.AtlasProjectService
import com.inferstep.atlas.settings.AtlasSettings
import com.intellij.openapi.actionSystem.DefaultActionGroup
import com.intellij.openapi.actionSystem.impl.SimpleDataContext
import com.intellij.openapi.application.ApplicationManager
import com.intellij.openapi.project.Project
import com.intellij.openapi.ui.popup.JBPopupFactory
import com.intellij.openapi.wm.StatusBar
import com.intellij.openapi.wm.StatusBarWidget
import com.intellij.openapi.wm.StatusBarWidgetFactory
import com.intellij.util.Consumer
import com.intellij.xml.util.XmlStringUtil
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.currentCoroutineContext
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withTimeoutOrNull
import java.awt.Component
import java.awt.event.MouseEvent

class AtlasStatusBarWidgetFactory : StatusBarWidgetFactory {
    override fun getId(): String = AtlasStatusBarWidget.ID

    override fun getDisplayName(): String = "ATLAS"

    override fun isEnabledByDefault(): Boolean = true

    override fun createWidget(
        project: Project,
        scope: CoroutineScope,
    ): StatusBarWidget = AtlasStatusBarWidget(project, scope)

    override fun disposeWidget(widget: StatusBarWidget) {
        widget.dispose()
    }
}

/** /ready is a probe, not a turn: a blackholed host must not leave the status
 * bar saying "starting" forever. */
private const val STATUS_TIMEOUT_MS = 5_000L

/** The tooltip is HTML, and [detail] and [calibration] carry text from the
 * proxy's answers. Escape each line so a `<` or `&` in a verdict is shown as
 * the text the proxy sent rather than interpreted as markup. */
internal fun statusTooltip(
    detail: String,
    calibration: String,
    running: Boolean,
): String {
    val lines = mutableListOf("ATLAS proxy: $detail")
    if (calibration.isNotEmpty()) {
        lines += calibration
    }
    if (running) {
        lines += "turn in progress"
    }
    lines += "Click for actions."
    return "<html>${lines.joinToString("<br>") { XmlStringUtil.escapeString(it) }}</html>"
}

class AtlasStatusBarWidget(
    private val project: Project,
    scope: CoroutineScope,
) : StatusBarWidget,
    StatusBarWidget.TextPresentation {
    private val service = AtlasProjectService.getInstance(project)
    private val settings = AtlasSettings.getInstance()
    private val job = SupervisorJob(scope.coroutineContext[Job])
    private val widgetScope = CoroutineScope(scope.coroutineContext + job)
    private val pollLock = Mutex()

    @Volatile private var status: AtlasStatus = AtlasStatus.Starting(emptyList())

    @Volatile private var detail: String = "checking…"

    @Volatile private var calibration: String = ""

    /** The last authenticated probe's answer, for the connection it was
     * learned on. An unknown answer, such as a timeout, leaves it as it was;
     * only a 2xx clears a rejection, and a new address or token starts over. */
    @Volatile private var tokenRejected = false

    /** The connection [tokenRejected] belongs to. */
    @Volatile private var probedConnection: AtlasConnection? = null

    @Volatile private var statusBar: StatusBar? = null

    @Volatile private var started = false

    override fun ID(): String = ID

    // Only the no-argument presentation is implemented. The PlatformType
    // overload is deprecated, and its default implementation delegates here.
    override fun getPresentation(): StatusBarWidget.WidgetPresentation = this

    override fun install(statusBar: StatusBar) {
        this.statusBar = statusBar
        if (started) {
            return
        }
        started = true
        widgetScope.launch {
            var firstPoll = true
            while (currentCoroutineContext().isActive) {
                poll(includeCalibration = firstPoll)
                firstPoll = false
                delay(settings.statusBarPollIntervalSec.toLong() * 1000L)
            }
        }
        widgetScope.launch {
            service.statusRefreshRequests.collect { poll(includeCalibration = true) }
        }
        widgetScope.launch {
            var wasRunning = false
            service.session.state.collect { state ->
                if (wasRunning && !state.running) {
                    poll()
                }
                wasRunning = state.running
            }
        }
    }

    override fun getText(): String =
        when (status) {
            AtlasStatus.Ready -> "ATLAS"
            is AtlasStatus.Starting -> "ATLAS · starting"
            AtlasStatus.Unreachable -> "ATLAS · offline"
            AtlasStatus.Unauthorized -> "ATLAS · token"
        }

    override fun getAlignment(): Float = Component.CENTER_ALIGNMENT

    override fun getTooltipText(): String =
        statusTooltip(
            detail = detail,
            calibration = calibration,
            running = service.session.state.value.running,
        )

    override fun getClickConsumer(): Consumer<MouseEvent> = Consumer { showMenu() }

    private suspend fun poll(includeCalibration: Boolean = false) {
        pollLock.withLock {
            if (service.session.state.value.running) {
                return
            }
            val connection = service.connection.value
            tokenRejected = rejectionApplies(tokenRejected, probedConnection, connection)
            probedConnection = connection
            val client = AtlasClient(connection.url, connection.token)
            try {
                status = probe(client)
                detail = detailOf(status)
                if (includeCalibration) {
                    // A host that did not answer /ready cannot answer this
                    // either, and the last verdict would read as current.
                    calibration =
                        if (status == AtlasStatus.Unreachable) "" else calibrationLine(client)
                }
            } finally {
                client.close()
            }
            updateWidget()
        }
    }

    /**
     * Ask `/ready` first. While the proxy does not answer, "offline" is the
     * whole answer: a probe would add a second time limit to a poll that has
     * already used one, and a 401 it could not get says nothing.
     */
    private suspend fun probe(client: AtlasClient): AtlasStatus {
        val ready =
            try {
                withTimeoutOrNull(STATUS_TIMEOUT_MS) { client.getReady() }
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                val failed = statusFromFailure(e)
                if (failed == AtlasStatus.Unauthorized) {
                    tokenRejected = true
                }
                return failed
            }
        if (ready == null) {
            return statusFromAnswers(null, tokenRejected)
        }
        val tokenProbe =
            withTimeoutOrNull(STATUS_TIMEOUT_MS) { client.probeToken() }
                ?: TokenProbe.Unknown
        tokenRejected = tokenProbe.rejectedAfter(tokenRejected)
        return statusFromAnswers(ready, tokenRejected)
    }

    private suspend fun calibrationLine(client: AtlasClient): String =
        try {
            val calibrationStatus =
                withTimeoutOrNull(STATUS_TIMEOUT_MS) { client.getCalibrationStatus() }
            if (calibrationStatus == null) {
                ""
            } else {
                "lens: ${calibrationStatus.lens.verdict} · asa: ${calibrationStatus.asa.verdict}"
            }
        } catch (e: CancellationException) {
            throw e
        } catch (_: Exception) {
            ""
        }

    private fun detailOf(status: AtlasStatus): String =
        when (status) {
            AtlasStatus.Ready -> {
                "all gates passing"
            }

            is AtlasStatus.Starting -> {
                if (status.waiting.isEmpty()) {
                    "starting…"
                } else {
                    "waiting on: ${status.waiting.joinToString()}"
                }
            }

            AtlasStatus.Unreachable -> {
                "the proxy is not reachable"
            }

            AtlasStatus.Unauthorized -> {
                "the proxy rejected the token (401)"
            }
        }

    private fun updateWidget() {
        ApplicationManager.getApplication().invokeLater {
            statusBar?.updateWidget(ID)
        }
    }

    private fun showMenu() {
        val group =
            DefaultActionGroup(
                listOf(
                    OpenChatAction(),
                    CancelTurnAction(),
                    NewConversationAction(),
                    RefreshStatusAction(),
                    OpenAtlasSettingsAction(),
                ),
            )
        val popup =
            JBPopupFactory.getInstance().createActionGroupPopup(
                "ATLAS",
                group,
                SimpleDataContext.getProjectContext(project),
                JBPopupFactory.ActionSelectionAid.SPEEDSEARCH,
                false,
                null,
                -1,
            )
        val anchor = statusBar?.component ?: return
        popup.showUnderneathOf(anchor)
    }

    override fun dispose() {
        job.cancel()
        statusBar = null
    }

    companion object {
        const val ID = "AtlasStatus"
    }
}

// The plugin's persistent settings, and the one place a connection is built
// from them.
//
// The service token is deliberately not here: it lives in the IDE credential
// store (AtlasTokenStore). The settings file holds only what is safe to show.
package com.inferstep.atlas.settings

import com.inferstep.atlas.client.AtlasConnection
import com.inferstep.atlas.client.normalizeProxyUrl
import com.inferstep.atlas.protocol.PermissionMode
import com.intellij.openapi.application.ApplicationManager
import com.intellij.openapi.components.PersistentStateComponent
import com.intellij.openapi.components.Service
import com.intellij.openapi.components.State
import com.intellij.openapi.components.Storage
import com.intellij.openapi.components.service
import com.intellij.util.messages.Topic

/** The status bar's default poll interval, in seconds. */
const val DEFAULT_STATUS_POLL_SECONDS = 15

/** The shortest poll interval the settings accept; the VS Code client uses the
 * same floor because `/ready` hits four downstream checks. */
const val MIN_STATUS_POLL_SECONDS = 5

/** The longest poll interval the settings accept, so a typo cannot silence the
 * status bar for a day. */
const val MAX_STATUS_POLL_SECONDS = 3600

/** A listener for settings applied from the settings dialog or an action. */
fun interface AtlasSettingsListener {
    fun settingsChanged()
}

/** The application-wide notification that a setting changed. */
val ATLAS_SETTINGS_TOPIC: Topic<AtlasSettingsListener> =
    Topic.create("ATLAS settings changed", AtlasSettingsListener::class.java)

/**
 * The plugin-wide settings, persisted in `atlas.xml`.
 *
 * [State.proxyUrl] is blank when the user has never chosen one; the resolved
 * value then comes from `ATLAS_PROXY_URL` or the documented default, which is
 * what an existing install expects. A permission mode is stored by its wire
 * name so the XML stays readable and a mode added later cannot break an older
 * file.
 */
@State(name = "AtlasSettings", storages = [Storage("atlas.xml")])
@Service(Service.Level.APP)
class AtlasSettings : PersistentStateComponent<AtlasSettings.State> {
    /** The serialized shape. No token, ever. */
    class State {
        var proxyUrl: String = ""
        var permissionMode: String = PermissionMode.DEFAULT.wireName()
        var statusBarPollIntervalSec: Int = DEFAULT_STATUS_POLL_SECONDS
    }

    private var state = State()

    override fun getState(): State = state

    override fun loadState(state: State) {
        this.state = state
    }

    /** The URL to show in settings and send to the client. */
    fun resolvedProxyUrl(environment: AtlasConnection = AtlasConnection.fromEnvironment()): String =
        if (state.proxyUrl.isBlank()) {
            environment.url
        } else {
            normalizeProxyUrl(state.proxyUrl)
        }

    /** The configured URL, possibly blank. `apply` stores the user's text. */
    var proxyUrl: String
        get() = state.proxyUrl
        set(value) {
            state.proxyUrl = value.trim()
        }

    var permissionMode: PermissionMode
        get() = PermissionMode.fromWire(state.permissionMode)
        set(value) {
            state.permissionMode = value.wireName()
        }

    var statusBarPollIntervalSec: Int
        get() =
            state.statusBarPollIntervalSec.coerceIn(
                MIN_STATUS_POLL_SECONDS,
                MAX_STATUS_POLL_SECONDS,
            )
        set(value) {
            state.statusBarPollIntervalSec =
                value.coerceIn(MIN_STATUS_POLL_SECONDS, MAX_STATUS_POLL_SECONDS)
        }

    /**
     * Build the connection the client should use.
     *
     * A stored token wins over the environment; a blank one falls back to
     * `ATLAS_SERVICE_TOKEN_FILE`, so an existing install keeps working until
     * the user saves a token in the IDE. A blank URL follows the environment
     * the same way.
     */
    fun connection(
        storedToken: String?,
        environment: AtlasConnection = AtlasConnection.fromEnvironment(),
    ): AtlasConnection {
        val url =
            if (state.proxyUrl.isBlank()) {
                environment.url
            } else {
                normalizeProxyUrl(
                    state.proxyUrl,
                )
            }
        val token = storedToken?.trim().orEmpty().ifEmpty { environment.token }
        return AtlasConnection(url, token)
    }

    /** Tell every project service and the status bar that settings changed. */
    fun notifyChanged() {
        ApplicationManager
            .getApplication()
            .messageBus
            .syncPublisher(ATLAS_SETTINGS_TOPIC)
            .settingsChanged()
    }

    companion object {
        fun getInstance(): AtlasSettings = service()
    }
}

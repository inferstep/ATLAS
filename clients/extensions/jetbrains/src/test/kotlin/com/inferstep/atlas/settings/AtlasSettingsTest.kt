package com.inferstep.atlas.settings

import com.inferstep.atlas.client.AtlasConnection
import com.inferstep.atlas.protocol.PermissionMode
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Test
import org.junit.jupiter.params.ParameterizedTest
import org.junit.jupiter.params.provider.Arguments
import org.junit.jupiter.params.provider.MethodSource

class AtlasSettingsTest {
    private val environment = AtlasConnection("http://env:8090", "env-token")

    @ParameterizedTest(name = "{0}")
    @MethodSource("proxyUrls")
    fun `a configured proxy url wins and a blank one follows the environment`(
        name: String,
        configured: String,
        want: String,
    ) {
        val settings = AtlasSettings()
        settings.proxyUrl = configured

        assertEquals(want, settings.resolvedProxyUrl(environment), name)
        assertEquals(
            AtlasConnection(want, environment.token),
            settings.connection(storedToken = null, environment = environment),
            name,
        )
    }

    @Test
    fun `a stored token wins and a blank one falls back`() {
        val settings = AtlasSettings()

        assertEquals(
            "stored-token",
            settings.connection(storedToken = " stored-token ", environment = environment).token,
        )
        assertEquals(
            environment.token,
            settings.connection(storedToken = "  ", environment = environment).token,
        )
    }

    @Test
    fun `a permission mode is stored by its wire name`() {
        val settings = AtlasSettings()

        settings.permissionMode = PermissionMode.ACCEPT_EDITS

        assertEquals("accept-edits", settings.getState().permissionMode)
        assertEquals(PermissionMode.ACCEPT_EDITS, settings.permissionMode)
    }

    @Test
    fun `an unknown stored permission mode reads as default`() {
        val settings = AtlasSettings()
        settings.getState().permissionMode = "something-else"

        assertEquals(PermissionMode.DEFAULT, settings.permissionMode)
    }

    @Test
    fun `the status poll interval is clamped at both ends`() {
        val settings = AtlasSettings()

        settings.statusBarPollIntervalSec = 1
        assertEquals(MIN_STATUS_POLL_SECONDS, settings.statusBarPollIntervalSec)

        settings.statusBarPollIntervalSec = 99999
        assertEquals(MAX_STATUS_POLL_SECONDS, settings.statusBarPollIntervalSec)

        settings.statusBarPollIntervalSec = 30
        assertEquals(30, settings.statusBarPollIntervalSec)
    }

    companion object {
        @JvmStatic
        fun proxyUrls(): List<Arguments> =
            listOf(
                Arguments.of("a blank setting follows the environment", "", "http://env:8090"),
                Arguments.of(
                    "a configured setting wins and is normalized",
                    " http://127.0.0.1:9000/ ",
                    "http://127.0.0.1:9000",
                ),
            )
    }
}

// AtlasConnection: the documented default, the token file, URL normalization
// and the redacted toString.
package com.inferstep.atlas.client

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import org.junit.jupiter.params.ParameterizedTest
import org.junit.jupiter.params.provider.Arguments
import org.junit.jupiter.params.provider.MethodSource

class AtlasConnectionTest {
    @Test
    fun `reads the documented variables`() {
        val connection =
            AtlasConnection.fromEnvironment(
                env = { name ->
                    when (name) {
                        AtlasConnection.URL_ENV -> "http://127.0.0.1:18090"
                        AtlasConnection.TOKEN_FILE_ENV -> "/run/atlas-secrets/service-token"
                        else -> null
                    }
                },
                readFile = { path ->
                    if (path == "/run/atlas-secrets/service-token") "s3cret\n" else null
                },
            )

        assertEquals("http://127.0.0.1:18090", connection.url)
        // Trimmed: the file ends with a newline.
        assertEquals("s3cret", connection.token)
    }

    @Test
    fun `with no variables set, the url is the documented default and no token file is read`() {
        val asked = mutableListOf<String>()
        val connection =
            AtlasConnection.fromEnvironment(
                env = { null },
                readFile = { path ->
                    asked += path
                    null
                },
            )

        assertEquals(AtlasConnection.DEFAULT_URL, connection.url)
        assertEquals("http://127.0.0.1:8090", connection.url)
        assertEquals("", connection.token)
        assertEquals(emptyList<String>(), asked)
    }

    @Test
    fun `a blank url falls back rather than producing an empty base`() {
        val connection =
            AtlasConnection.fromEnvironment(
                env = { name -> if (name == AtlasConnection.URL_ENV) "  " else null },
                readFile = { null },
            )

        assertEquals(AtlasConnection.DEFAULT_URL, connection.url)
    }

    @Test
    fun `the token does not appear in toString`() {
        val connection = AtlasConnection("http://127.0.0.1:8090", "s3cret")

        assertFalse(connection.toString().contains("s3cret"), connection.toString())
        assertTrue(connection.toString().contains("<redacted>"), connection.toString())
    }

    @ParameterizedTest(name = "{0}")
    @MethodSource("urls")
    fun `normalizes the configured url`(
        name: String,
        raw: String?,
        want: String,
    ) {
        assertEquals(want, normalizeProxyUrl(raw), name)
    }

    companion object {
        @JvmStatic
        fun urls(): List<Arguments> =
            listOf(
                Arguments.of("blank uses the documented default", "", AtlasConnection.DEFAULT_URL),
                Arguments.of(
                    "whitespace uses the documented default",
                    "  ",
                    AtlasConnection.DEFAULT_URL,
                ),
                Arguments.of("null uses the documented default", null, AtlasConnection.DEFAULT_URL),
                Arguments.of(
                    "a trailing slash is removed",
                    "http://127.0.0.1:8090/",
                    "http://127.0.0.1:8090",
                ),
                Arguments.of(
                    "surrounding whitespace is removed",
                    "  http://127.0.0.1:8090  ",
                    "http://127.0.0.1:8090",
                ),
                Arguments.of(
                    "a scheme-only value is left alone",
                    "http://",
                    "http://",
                ),
            )
    }
}

package com.inferstep.atlas.client

import java.nio.file.Files
import java.nio.file.Path

// Where the proxy is and how to authenticate to it.
//
// Stage 5 moved both into the plugin's settings (settings/AtlasSettings), with
// the token in the IDE credential store. This type remains the value the rest
// of the client passes around, and [fromEnvironment] is the fallback for an
// install that configured the variables the TUI and `atlas doctor` use.

/**
 * proxy URL plus service token.
 *
 * [toString] deliberately redacts the token: nothing logs a connection today,
 * but a data class's generated toString would print the secret in a stack
 * trace, a debugger view or one interpolated log line.
 */
data class AtlasConnection(
    val url: String,
    /** Empty when no token is configured, which is an install predating it. */
    val token: String = "",
) {
    override fun toString(): String =
        "AtlasConnection(url=$url, token=${if (token.isEmpty()) "<none>" else "<redacted>"})"

    companion object {
        /** The documented default (docs/CONFIGURATION.md). */
        const val DEFAULT_URL = "http://127.0.0.1:8090"

        /** atlas-proxy endpoint, honoured by the TUI as its `--proxy` default. */
        const val URL_ENV = "ATLAS_PROXY_URL"

        /** Path to the generated service token; its value never goes in `.env`.
         * The plugin no longer guesses a relative default: a path relative to
         * the IDE's working directory is not the install the token sits beside,
         * and guessing it turns "no token configured" into the wrong answer. */
        const val TOKEN_FILE_ENV = "ATLAS_SERVICE_TOKEN_FILE"

        /**
         * Read the connection from the environment.
         *
         * A missing path or an unreadable file is not an error: an install
         * without a configured token file has internal auth disabled, and the
         * proxy answers. A blank `ATLAS_PROXY_URL` falls back to the default.
         */
        fun fromEnvironment(
            env: (String) -> String? = System::getenv,
            readFile: (String) -> String? = ::readTokenFile,
        ): AtlasConnection {
            val configuredUrl =
                env(URL_ENV)
                    ?.trim()
                    ?.takeIf { it.isNotEmpty() }
            val configuredPath =
                env(TOKEN_FILE_ENV)
                    ?.trim()
                    ?.takeIf { it.isNotEmpty() }
            val token =
                configuredPath
                    ?.let(readFile)
                    ?.trim()
                    .orEmpty()
            return AtlasConnection(url = normalizeProxyUrl(configuredUrl), token = token)
        }

        private fun readTokenFile(path: String): String? =
            try {
                val file = Path.of(path)
                if (Files.isReadable(file)) {
                    Files.readString(file)
                } else {
                    null
                }
            } catch (_: Exception) {
                // An unreadable, missing or non-file path means "no token
                // configured", not a broken connection.
                null
            }
    }
}

/**
 * The URL a client should use for a configured value.
 *
 * A blank value (the default in settings) means "not configured", so the
 * documented default applies. Trailing slashes are removed once here rather
 * than in every caller.
 */
fun normalizeProxyUrl(raw: String?): String {
    val trimmed = raw?.trim().orEmpty()
    if (trimmed.isEmpty()) {
        return AtlasConnection.DEFAULT_URL
    }
    val stripped = trimmed.trimEnd('/')
    return if (stripped.isEmpty() || stripped.endsWith(":")) trimmed else stripped
}

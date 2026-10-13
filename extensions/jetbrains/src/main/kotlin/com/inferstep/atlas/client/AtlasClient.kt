// HTTP client for the atlas-proxy public API: /v1/agent (SSE), /cancel,
// /v1/permission, /ready, /version, /workspace and /v1/calibration/status.
//
// A port of extensions/vscode/src/client/atlasClient.ts. The conventions it
// mirrors from tui/chat.go:
//   - Authorization: Bearer <token> on every request when a token is set.
//   - /v1/permission 404 means the request already resolved, so it is success.
//   - /cancel is best-effort defense-in-depth; aborting the SSE request is the
//     primary cancel mechanism.
//
// No com.intellij imports: this layer runs under plain JUnit.
package com.inferstep.atlas.client

import com.inferstep.atlas.protocol.AgentRequest
import com.inferstep.atlas.protocol.AtlasJson
import com.inferstep.atlas.protocol.CalibrationStatusResponse
import com.inferstep.atlas.protocol.CancelRequest
import com.inferstep.atlas.protocol.CancelResponse
import com.inferstep.atlas.protocol.ChatEvent
import com.inferstep.atlas.protocol.ErrorEnvelope
import com.inferstep.atlas.protocol.PermissionDecisionRequest
import com.inferstep.atlas.protocol.ReadyResponse
import com.inferstep.atlas.protocol.SseParser
import com.inferstep.atlas.protocol.VersionResponse
import com.inferstep.atlas.protocol.WorkspaceResponse
import io.ktor.client.HttpClient
import io.ktor.client.engine.java.Java
import io.ktor.client.request.HttpRequestBuilder
import io.ktor.client.request.header
import io.ktor.client.request.prepareGet
import io.ktor.client.request.preparePost
import io.ktor.client.request.setBody
import io.ktor.client.statement.HttpResponse
import io.ktor.client.statement.bodyAsChannel
import io.ktor.client.statement.bodyAsText
import io.ktor.http.ContentType
import io.ktor.http.HttpHeaders
import io.ktor.http.HttpStatusCode
import io.ktor.http.contentType
import io.ktor.http.isSuccess
import io.ktor.utils.io.readAvailable
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flow
import kotlinx.serialization.json.Json
import kotlin.coroutines.cancellation.CancellationException

private const val AGENT_PATH = "/v1/agent"
private const val CANCEL_PATH = "/cancel"
private const val PERMISSION_PATH = "/v1/permission"
private const val READY_PATH = "/ready"
private const val VERSION_PATH = "/version"
private const val WORKSPACE_PATH = "/workspace"
private const val CALIBRATION_PATH = "/v1/calibration/status"

/** Bytes per socket read. One SSE frame is usually far smaller; the parser
 * copes with a frame split across any number of reads. */
private const val READ_BUFFER_SIZE = 16 * 1024

/**
 * Error carrying the proxy's stable envelope when one was parseable.
 *
 * Callers switch on [code] (the closed set in `AtlasErrorCodes`), never on the
 * human [detail].
 */
class AtlasApiError(
    val status: Int,
    val code: String,
    val detail: String,
) : Exception(if (code.isEmpty()) "HTTP $status: $detail" else "$code: $detail") {
    /** True when the proxy rejected the request for a missing or bad token. */
    val isUnauthorized: Boolean
        get() = status == HttpStatusCode.Unauthorized.value

    companion object {
        /**
         * Read the stable error envelope out of a failed response. Must be
         * called while the response is still open — inside a Ktor `execute`
         * block — because it reads the body.
         */
        suspend fun from(response: HttpResponse): AtlasApiError {
            val body = response.bodyAsText()
            val envelope =
                try {
                    AtlasJson.decodeFromString(ErrorEnvelope.serializer(), body)
                } catch (_: Exception) {
                    // A non-JSON body (a reverse proxy's error page, say): keep
                    // the raw text as the detail, like the reference client.
                    ErrorEnvelope(detail = body.trim())
                }
            return AtlasApiError(response.status.value, envelope.error, envelope.detail)
        }
    }
}

/**
 * A client for one atlas-proxy.
 *
 * Two construction choices are deliberate and worth stating:
 *
 * - **No `HttpTimeout` plugin is installed.** While a `permission_request` is
 *   pending the proxy sends nothing on the agent stream, and the documented
 *   fail-safe is `ATLAS_PERMISSION_TIMEOUT_SEC` (600s). A read timeout shorter
 *   than that kills the turn mid-permission — exactly the bug the VS Code
 *   client worked around by disabling undici's body idle timeout. The Java
 *   engine's defaults are no connect timeout and no read timeout, so leaving
 *   the plugin out is the correct configuration, not an omission.
 * - **The SSE stream is parsed by `protocol.SseParser`, not a Ktor SSE plugin.**
 *   The wire format is `data:`-only with a `[DONE]` sentinel, and a malformed
 *   frame must be skipped rather than fail the turn; that permissiveness lives
 *   in the parser, and a strict third-party parser would silently change it.
 *   ktor-client-sse is therefore not a dependency.
 *
 * Close it when the owning component goes away, to release the connection pool.
 */
class AtlasClient(
    baseUrl: String,
    private val token: String = "",
    private val json: Json = AtlasJson,
) : AutoCloseable {
    private val root: String = baseUrl.trimEnd('/')

    private val http: HttpClient =
        HttpClient(Java) {
            // The proxy writes its own error envelopes; a non-2xx is data, not
            // something the engine should raise for us.
            expectSuccess = false
        }

    /**
     * POST /v1/agent and stream the turn's events.
     *
     * The flow completes on the `[DONE]` sentinel, or when the connection ends
     * without one (yielding the events already received, like the reference
     * parser). Collecting it issues the request; cancelling the collector
     * aborts it. A non-2xx response fails the flow with an [AtlasApiError]
     * before any event is emitted.
     */
    fun agentTurn(request: AgentRequest): Flow<ChatEvent> =
        flow {
            val body = json.encodeToString(AgentRequest.serializer(), request)
            http
                .preparePost(url(AGENT_PATH)) {
                    applyJsonHeaders(acceptEventStream = true)
                    setBody(body)
                }.execute { response ->
                    if (!response.status.isSuccess()) {
                        throw AtlasApiError.from(response)
                    }
                    val parser = SseParser(json)
                    val channel = response.bodyAsChannel()
                    val buffer = ByteArray(READ_BUFFER_SIZE)
                    while (true) {
                        val read = channel.readAvailable(buffer, 0, buffer.size)
                        if (read <= 0) {
                            break
                        }
                        for (event in parser.feed(buffer.copyOf(read))) {
                            emit(event)
                        }
                    }
                    for (event in parser.endOfStream()) {
                        emit(event)
                    }
                }
        }

    /**
     * POST /cancel for an in-flight turn. Best-effort: a connection failure or
     * a non-200 is reported as "not cancelled" rather than thrown, because the
     * SSE abort is the primary mechanism. Returns true when the proxy reported
     * `cancelled: true`.
     */
    suspend fun cancelTurn(sessionId: String): Boolean {
        if (sessionId.isEmpty()) {
            return false
        }
        return try {
            val body = json.encodeToString(CancelRequest.serializer(), CancelRequest(sessionId))
            http
                .preparePost(url(CANCEL_PATH)) {
                    applyJsonHeaders()
                    setBody(body)
                }.execute { response ->
                    if (!response.status.isSuccess()) {
                        false
                    } else {
                        json
                            .decodeFromString(
                                CancelResponse.serializer(),
                                response.bodyAsText(),
                            ).cancelled
                    }
                }
        } catch (e: CancellationException) {
            throw e
        } catch (_: Exception) {
            false
        }
    }

    /**
     * POST /v1/permission answering a `permission_request` event.
     *
     * A 404 means the pending request already resolved (cancelled, timed out),
     * which the TUI convention and the VS Code client both treat as success.
     * Every other non-2xx throws.
     */
    suspend fun postPermissionDecision(decision: PermissionDecisionRequest) {
        val body = json.encodeToString(PermissionDecisionRequest.serializer(), decision)
        http
            .preparePost(url(PERMISSION_PATH)) {
                applyJsonHeaders()
                setBody(body)
            }.execute { response ->
                if (response.status.isSuccess() ||
                    response.status.value == HttpStatusCode.NotFound.value
                ) {
                    return@execute
                }
                throw AtlasApiError.from(response)
            }
    }

    /**
     * GET /ready. Returns the gate body for both 200 and 503 — the shape is the
     * same and only [ReadyResponse.ready] differs. A network failure throws.
     */
    suspend fun getReady(): ReadyResponse =
        get(READY_PATH) { response ->
            if (!response.status.isSuccess() &&
                response.status.value != HttpStatusCode.ServiceUnavailable.value
            ) {
                throw AtlasApiError.from(response)
            }
            json.decodeFromString(ReadyResponse.serializer(), response.bodyAsText())
        }

    /** GET /version — API version, SSE protocol version, the error-code set. */
    suspend fun getVersion(): VersionResponse =
        get(VERSION_PATH) { response ->
            if (!response.status.isSuccess()) {
                throw AtlasApiError.from(response)
            }
            json.decodeFromString(VersionResponse.serializer(), response.bodyAsText())
        }

    /**
     * GET /workspace — the proxy's mounted paths, so a client can warn when its
     * own workspace folder is not the one being edited.
     *
     * Returns null for any failure, including a proxy too old to have the
     * route: a caller must read that as "can't tell", never as a mismatch.
     */
    suspend fun getWorkspace(): WorkspaceResponse? =
        try {
            get(WORKSPACE_PATH) { response ->
                if (!response.status.isSuccess()) {
                    null
                } else {
                    json.decodeFromString(WorkspaceResponse.serializer(), response.bodyAsText())
                }
            }
        } catch (e: CancellationException) {
            throw e
        } catch (_: Exception) {
            null
        }

    /**
     * GET /v1/calibration/status — the lens/ASA verdict for the loaded model.
     *
     * Uncached server-side (each call re-probes the lens service), so callers
     * hit it at activation and on an explicit refresh only.
     */
    suspend fun getCalibrationStatus(): CalibrationStatusResponse =
        get(CALIBRATION_PATH) { response ->
            if (!response.status.isSuccess()) {
                throw AtlasApiError.from(response)
            }
            json.decodeFromString(CalibrationStatusResponse.serializer(), response.bodyAsText())
        }

    override fun close() {
        http.close()
    }

    /**
     * GET with the body consumed inside the Ktor statement block.
     *
     * The block has to do the reading: `prepareGet(...).execute { }` releases
     * the response when the block returns, so handing the [HttpResponse] back
     * out and reading it afterwards is a use-after-release.
     */
    private suspend fun <T> get(
        path: String,
        parse: suspend (HttpResponse) -> T,
    ): T = http.prepareGet(url(path)) { applyJsonHeaders() }.execute { parse(it) }

    private fun url(path: String): String = root + path

    private fun HttpRequestBuilder.applyJsonHeaders(acceptEventStream: Boolean = false) {
        contentType(ContentType.Application.Json)
        header(
            HttpHeaders.Accept,
            if (acceptEventStream) "text/event-stream" else "application/json",
        )
        if (token.isNotEmpty()) {
            header(HttpHeaders.Authorization, "Bearer $token")
        }
    }
}

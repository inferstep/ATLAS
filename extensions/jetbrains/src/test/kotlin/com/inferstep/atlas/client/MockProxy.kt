// A real HTTP server fixture that streams canned SSE the way atlas-proxy
// does: `: connected` comment first, `data: {…}\n\n` frames, then
// `data: [DONE]\n\n`. It drives integration tests of AtlasClient without a
// live proxy.
//
// A port of extensions/vscode/test/fixtures/mockProxy.ts, on the JDK's own
// com.sun.net.httpserver so the fixture adds no dependency the plugin does not
// already have.
package com.inferstep.atlas.client

import com.sun.net.httpserver.HttpExchange
import com.sun.net.httpserver.HttpServer
import java.net.InetSocketAddress
import java.util.concurrent.CopyOnWriteArrayList
import java.util.concurrent.CountDownLatch
import java.util.concurrent.ExecutorService
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit

/** One request the fixture received, for assertions about what the client sent. */
data class RecordedRequest(
    val method: String,
    val path: String,
    val body: String,
    val authorization: String?,
    val accept: String?,
)

class MockProxy(
    private val options: Options = Options(),
) : AutoCloseable {
    /**
     * @param agentFrames envelope JSON strings, in order, each streamed as one
     *   `data:` frame on POST /v1/agent.
     * @param pauseAfterIndex pause the agent stream after this frame index
     *   until POST /v1/permission arrives — the real agent loop's behaviour on
     *   a destructive tool call in `default` mode.
     * @param agentStatus when non-null, answer POST /v1/agent with this status
     *   and body instead of a stream.
     * @param omitSentinel drop the trailing `[DONE]`, as a dropped connection does.
     */
    data class Options(
        val agentFrames: List<String> = emptyList(),
        val pauseAfterIndex: Int? = null,
        val agentStatus: Int? = null,
        val agentBody: String = "",
        val permissionStatus: Int = 200,
        val cancelStatus: Int = 200,
        val readyStatus: Int = 200,
        val readyBody: String =
            """{"ready":true,"inference":true,"lens_ready":true,"sandbox":true,"v3":true}""",
        val workspaceStatus: Int = 200,
        val workspaceBody: String =
            """{"project_dir":"/home/anuj/atlas","working_dir":"/workspace","containerized":true}""",
        val frameDelayMs: Long = 0,
        val omitSentinel: Boolean = false,
    )

    val requests = CopyOnWriteArrayList<RecordedRequest>()

    private val executor: ExecutorService = Executors.newCachedThreadPool()
    private val resumeAfterPermission: CountDownLatch? =
        if (options.pauseAfterIndex != null) CountDownLatch(1) else null

    private val server: HttpServer =
        HttpServer.create(InetSocketAddress("127.0.0.1", 0), 0).apply {
            // Without a pool the dispatcher handles requests one at a time,
            // and a paused /v1/agent stream would block the /v1/permission
            // that is supposed to release it — a deadlock, not a flaky test.
            executor = this@MockProxy.executor
            createContext("/") { exchange -> handle(exchange) }
            start()
        }

    /** The fixture's base URL, e.g. `http://127.0.0.1:41231`. */
    val url: String
        get() = "http://127.0.0.1:${server.address.port}"

    override fun close() {
        server.stop(0)
        executor.shutdownNow()
    }

    private fun handle(exchange: HttpExchange) {
        val path = exchange.requestURI.path
        val body = exchange.requestBody.readBytes().decodeToString()
        requests +=
            RecordedRequest(
                method = exchange.requestMethod,
                path = path,
                body = body,
                authorization = exchange.requestHeaders.getFirst("Authorization"),
                accept = exchange.requestHeaders.getFirst("Accept"),
            )
        when {
            path == "/v1/agent" -> {
                handleAgent(exchange)
            }

            path == "/v1/permission" -> {
                handlePermission(exchange)
            }

            path == "/cancel" -> {
                json(
                    exchange,
                    options.cancelStatus,
                    """{"cancelled":${options.cancelStatus == 200}}""",
                )
            }

            path == "/ready" -> {
                json(exchange, options.readyStatus, options.readyBody)
            }

            path == "/version" -> {
                json(
                    exchange,
                    200,
                    """{"api_version":"1.0.0","protocol_version":1,""" +
                        """"error_codes":["unauthorized","invalid_input"],""" +
                        """"grammar_mode":"strict","session_timeout_s":600}""",
                )
            }

            path == "/workspace" -> {
                json(exchange, options.workspaceStatus, options.workspaceBody)
            }

            path == "/v1/calibration/status" -> {
                json(
                    exchange,
                    200,
                    """{"lens":{"verdict":"supported"},"asa":{"verdict":"supported"},""" +
                        """"dimensions":[{"name":"lens","status":"ok","detail":""}]}""",
                )
            }

            else -> {
                json(exchange, 404, """{"error":"invalid_input","detail":"no route"}""")
            }
        }
    }

    private fun handleAgent(exchange: HttpExchange) {
        val failureStatus = options.agentStatus
        if (failureStatus != null) {
            json(exchange, failureStatus, options.agentBody)
            return
        }
        exchange.responseHeaders.add("Content-Type", "text/event-stream")
        exchange.responseHeaders.add("Cache-Control", "no-cache")
        // 0 means "unknown length": the body is streamed chunked, not buffered.
        exchange.sendResponseHeaders(200, 0)
        exchange.responseBody.use { out ->
            out.write(": connected\n\n".toByteArray())
            out.flush()
            options.agentFrames.forEachIndexed { index, frame ->
                if (options.frameDelayMs > 0) {
                    Thread.sleep(options.frameDelayMs)
                }
                out.write("data: $frame\n\n".toByteArray())
                out.flush()
                if (index == options.pauseAfterIndex) {
                    resumeAfterPermission?.await(WAIT_SECONDS, TimeUnit.SECONDS)
                }
            }
            if (!options.omitSentinel) {
                out.write("data: [DONE]\n\n".toByteArray())
                out.flush()
            }
        }
    }

    private fun handlePermission(exchange: HttpExchange) {
        json(
            exchange,
            options.permissionStatus,
            """{"delivered":${options.permissionStatus == 200}}""",
        )
        resumeAfterPermission?.countDown()
    }

    private fun json(
        exchange: HttpExchange,
        status: Int,
        body: String,
    ) {
        val bytes = body.toByteArray()
        exchange.responseHeaders.add("Content-Type", "application/json")
        exchange.sendResponseHeaders(status, bytes.size.toLong())
        exchange.responseBody.use { it.write(bytes) }
    }

    private companion object {
        const val WAIT_SECONDS = 30L
    }
}

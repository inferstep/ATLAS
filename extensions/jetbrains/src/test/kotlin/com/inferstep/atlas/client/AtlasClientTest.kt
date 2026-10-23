// Integration tests for AtlasClient against the in-process mock proxy. These
// are the tests that exercise the transport for real: a live HTTP connection,
// chunked SSE, and the pause the agent loop takes on a destructive call.
package com.inferstep.atlas.client

import com.inferstep.atlas.protocol.AgentRequest
import com.inferstep.atlas.protocol.AtlasErrorCodes
import com.inferstep.atlas.protocol.ChatEvent
import com.inferstep.atlas.protocol.DonePayload
import com.inferstep.atlas.protocol.PermissionDecision
import com.inferstep.atlas.protocol.PermissionDecisionRequest
import com.inferstep.atlas.protocol.PermissionMode
import com.inferstep.atlas.protocol.PermissionRequestPayload
import com.inferstep.atlas.protocol.PermissionScope
import com.inferstep.atlas.protocol.TextPayload
import com.inferstep.atlas.protocol.payload
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.toList
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import org.junit.jupiter.params.ParameterizedTest
import org.junit.jupiter.params.provider.Arguments
import org.junit.jupiter.params.provider.MethodSource
import org.junit.jupiter.params.provider.ValueSource
import java.net.ServerSocket
import java.util.concurrent.CopyOnWriteArrayList

class AtlasClientTest {
    @ParameterizedTest(name = "sentinel omitted: {0}")
    @ValueSource(booleans = [false, true])
    fun `streams a turn's events, whether or not the sentinel closes the stream`(
        omitSentinel: Boolean,
    ) {
        MockProxy(MockProxy.Options(agentFrames = TURN, omitSentinel = omitSentinel)).use { proxy ->
            AtlasClient(proxy.url).use { client ->
                val events = runBlocking { client.agentTurn(request()).toList() }

                assertEquals(listOf("turn_start", "text", "done"), events.map { it.type })
                assertEquals("hello", events[1].payload<TextPayload>().content)
                assertTrue(
                    events
                        .last()
                        .payload<DonePayload>()
                        .resolvedStatus()
                        .isCompleted(),
                )
            }
        }
    }

    @Test
    fun `sends the contract fields, the token, and the event-stream accept header`() =
        runBlocking {
            MockProxy(MockProxy.Options(agentFrames = TURN)).use { proxy ->
                AtlasClient(proxy.url, token = "s3cret").use { client ->
                    client.agentTurn(request()).toList()

                    val sent = proxy.requests.single { it.path == "/v1/agent" }
                    assertEquals("POST", sent.method)
                    assertEquals("Bearer s3cret", sent.authorization)
                    assertEquals("text/event-stream", sent.accept)
                    assertTrue(sent.body.contains("\"working_dir\":\"/workspace\""), sent.body)
                    assertTrue(sent.body.contains("\"session_id\":\"client-1\""), sent.body)
                    assertTrue(sent.body.contains("\"mode\":\"yolo\""), sent.body)
                    assertTrue(sent.body.contains("\"message\":\"do the thing\""), sent.body)
                }
            }
        }

    @Test
    fun `sends no authorization header when the install has no token`() =
        runBlocking {
            MockProxy(MockProxy.Options(agentFrames = TURN)).use { proxy ->
                AtlasClient(proxy.url).use { client ->
                    client.agentTurn(request()).toList()
                    assertNull(proxy.requests.single { it.path == "/v1/agent" }.authorization)
                }
            }
        }

    @Test
    fun `a turn paused on a permission request resumes when the decision is posted`() =
        runBlocking {
            // The heart of the contract: while the prompt is open the proxy
            // sends nothing at all on this stream — for up to 600s. The client
            // must sit there, not time out, and only continue on the decision.
            val frames =
                listOf(
                    frame("turn_start", """{"turn":1}"""),
                    frame(
                        "permission_request",
                        """{"tool_name":"write_file","args":{"path":"a.py"},""" +
                            """"message":"write a.py","tool_call_id":"call_0"}""",
                    ),
                    frame("tool_result", """{"tool":"write_file","success":true,"data":null}"""),
                    frame("done", """{"summary":"ok","status":"completed"}"""),
                )
            MockProxy(MockProxy.Options(agentFrames = frames, pauseAfterIndex = 1)).use { proxy ->
                AtlasClient(proxy.url, token = "tok").use { client ->
                    val events = CopyOnWriteArrayList<ChatEvent>()
                    val collector =
                        launch(Dispatchers.IO) {
                            client.agentTurn(request()).collect { events += it }
                        }

                    val pending =
                        withTimeout(PROMPT_TIMEOUT_MS) {
                            while (events.none { it.type == "permission_request" }) {
                                delay(5)
                            }
                            events.first { it.type == "permission_request" }
                        }
                    val prompt = pending.payload<PermissionRequestPayload>()
                    assertEquals("call_0", prompt.toolCallId)
                    assertEquals("write_file", prompt.toolName)

                    // Nothing may follow the prompt until it is answered.
                    delay(150)
                    assertEquals(
                        listOf("turn_start", "permission_request"),
                        events.map { it.type },
                    )

                    client.postPermissionDecision(
                        PermissionDecisionRequest(
                            sessionId = "client-1",
                            toolCallId = prompt.toolCallId,
                            decision = PermissionDecision.ALLOW,
                            scope = PermissionScope.ONCE,
                        ),
                    )

                    withTimeout(PROMPT_TIMEOUT_MS) { collector.join() }
                    assertTrue(
                        proxy.permissionArrivedInTime,
                        "the stream went on without the permission answer",
                    )
                    assertEquals(
                        listOf("turn_start", "permission_request", "tool_result", "done"),
                        events.map { it.type },
                    )
                }
            }
        }

    @ParameterizedTest(name = "{0}")
    @MethodSource("failedTurns")
    fun `a failed turn keeps the proxy's status, code and detail`(
        name: String,
        status: Int,
        body: String,
        code: String,
        detail: String,
    ) {
        MockProxy(MockProxy.Options(agentStatus = status, agentBody = body)).use { proxy ->
            AtlasClient(proxy.url).use { client ->
                val error =
                    assertThrows(AtlasApiError::class.java) {
                        runBlocking { client.agentTurn(request()).toList() }
                    }

                assertEquals(status, error.status, name)
                assertEquals(code, error.code, name)
                assertEquals(detail, error.detail, name)
                assertEquals(status == 401, error.isUnauthorized, name)
            }
        }
    }

    @Test
    fun `a cancellation mid-stream stops the collector`() =
        runBlocking {
            // After the first frame the proxy goes quiet, as it does while a
            // permission prompt is open. MockProxy holds that stream for 30 s,
            // longer than the 20 s this test waits, so only the cancel can end
            // the collector in time. Cancelling the collector is the primary
            // cancel mechanism: it aborts the HTTP request itself.
            MockProxy(MockProxy.Options(agentFrames = TURN, pauseAfterIndex = 0)).use { proxy ->
                AtlasClient(proxy.url).use { client ->
                    val events = CopyOnWriteArrayList<ChatEvent>()
                    val collector =
                        launch(Dispatchers.IO) {
                            client.agentTurn(request()).collect { events += it }
                        }

                    withTimeout(PROMPT_TIMEOUT_MS) {
                        while (events.isEmpty()) {
                            delay(5)
                        }
                    }

                    collector.cancel()
                    withTimeout(PROMPT_TIMEOUT_MS) { collector.join() }

                    assertEquals(listOf("turn_start"), events.map { it.type })
                }
            }
        }

    @Test
    fun `cancelTurn reports the proxy answer and tolerates nothing in flight`() =
        runBlocking {
            MockProxy().use { proxy ->
                AtlasClient(proxy.url).use { client ->
                    assertTrue(client.cancelTurn("client-1"))
                }
            }
            MockProxy(MockProxy.Options(cancelStatus = 404)).use { proxy ->
                AtlasClient(proxy.url).use { client ->
                    assertFalse(client.cancelTurn("client-1"))
                }
            }
        }

    @Test
    fun `cancelTurn is best-effort when the proxy is unreachable`() {
        // Cancel is defense-in-depth: the SSE abort is the primary stop. A
        // refused connection must read as "not cancelled", never as a thrown
        // exception that would mask the turn's own ending.
        val port = ServerSocket(0).use { it.localPort }
        AtlasClient("http://127.0.0.1:$port").use { client ->
            assertFalse(runBlocking { client.cancelTurn("client-1") })
        }
    }

    @Test
    fun `cancelTurn short-circuits an empty session id`() =
        runBlocking {
            MockProxy().use { proxy ->
                AtlasClient(proxy.url).use { client ->
                    assertFalse(client.cancelTurn(""))
                    assertTrue(
                        proxy.requests.isEmpty(),
                        "an empty session must not reach the proxy",
                    )
                }
            }
        }

    @Test
    fun `postPermissionDecision treats a 404 as already resolved`() =
        runBlocking {
            MockProxy(MockProxy.Options(permissionStatus = 404)).use { proxy ->
                AtlasClient(proxy.url).use { client ->
                    client.postPermissionDecision(decision())
                    assertEquals("POST", proxy.requests.single().method)
                    assertEquals("/v1/permission", proxy.requests.single().path)
                }
            }
        }

    @Test
    fun `postPermissionDecision throws on any other failure`() {
        MockProxy(MockProxy.Options(permissionStatus = 500)).use { proxy ->
            AtlasClient(proxy.url).use { client ->
                val error =
                    assertThrows(AtlasApiError::class.java) {
                        runBlocking { client.postPermissionDecision(decision()) }
                    }

                assertEquals(500, error.status)
            }
        }
    }

    @Test
    fun `ready returns the gate body for both 200 and 503`() =
        runBlocking {
            MockProxy().use { proxy ->
                AtlasClient(proxy.url).use { client ->
                    assertTrue(client.getReady().ready)
                }
            }
            MockProxy(
                MockProxy.Options(
                    readyStatus = 503,
                    readyBody =
                        """{"ready":false,"inference":true,"lens_ready":false,""" +
                            """"sandbox":true,"v3":true,"lens_reason":"no artifacts"}""",
                ),
            ).use { proxy ->
                AtlasClient(proxy.url).use { client ->
                    val ready = client.getReady()
                    assertFalse(ready.ready)
                    assertFalse(ready.lensReady)
                    assertEquals("no artifacts", ready.lensReason)
                }
            }
        }

    @Test
    fun `version reads the additive measurement fields, even through a trailing slash`() =
        runBlocking {
            MockProxy().use { proxy ->
                AtlasClient("${proxy.url}///").use { client ->
                    val version = client.getVersion()

                    assertEquals("/version", proxy.requests.single().path)
                    assertEquals("1.0.0", version.apiVersion)
                    assertEquals(1, version.protocolVersion)
                    assertEquals("strict", version.grammarMode)
                    assertEquals(600, version.sessionTimeoutSeconds)
                }
            }
        }

    @Test
    fun `workspace returns null when the proxy is too old to have the route`() =
        runBlocking {
            MockProxy(MockProxy.Options(workspaceStatus = 404)).use { proxy ->
                AtlasClient(proxy.url).use { client ->
                    assertNull(client.getWorkspace())
                }
            }
        }

    @Test
    fun `workspace parses the mounted paths when the route answers`() =
        runBlocking {
            MockProxy().use { proxy ->
                AtlasClient(proxy.url).use { client ->
                    val workspace = client.getWorkspace()
                    assertEquals("/workspace", workspace?.workingDir)
                    assertTrue(workspace?.containerized == true)
                }
            }
        }

    @Test
    fun `calibration status parses the lens and asa verdicts`() =
        runBlocking {
            MockProxy().use { proxy ->
                AtlasClient(proxy.url).use { client ->
                    val status = client.getCalibrationStatus()
                    assertEquals("supported", status.lens.verdict)
                    assertEquals("supported", status.asa.verdict)
                    assertEquals(1, status.dimensions.size)
                }
            }
        }

    @ParameterizedTest(name = "{0} -> {1}")
    @MethodSource("tokenProbes")
    fun `token probe distinguishes a rejected token from a route it cannot read`(
        status: Int,
        want: TokenProbe,
    ) = runBlocking {
        MockProxy(MockProxy.Options(workspaceStatus = status)).use { proxy ->
            AtlasClient(proxy.url).use { client ->
                assertEquals(want, client.probeToken())
            }
        }
    }

    private fun request(): AgentRequest =
        AgentRequest(
            message = "do the thing",
            workingDir = "/workspace",
            mode = PermissionMode.YOLO,
            sessionId = "client-1",
        )

    private fun decision(): PermissionDecisionRequest =
        PermissionDecisionRequest(
            sessionId = "client-1",
            toolCallId = "call_0",
            decision = PermissionDecision.DENY,
            scope = PermissionScope.ONCE,
        )

    private companion object {
        const val PROMPT_TIMEOUT_MS = 20_000L

        @JvmStatic
        fun failedTurns(): List<Arguments> =
            listOf(
                Arguments.of(
                    "401 with the proxy's envelope",
                    401,
                    """{"error":"unauthorized","detail":"missing token","api_version":"1.0.0"}""",
                    AtlasErrorCodes.UNAUTHORIZED,
                    "missing token",
                ),
                Arguments.of(
                    "400 with the proxy's envelope",
                    400,
                    """{"error":"invalid_input","detail":"said no","api_version":"1.0.0"}""",
                    AtlasErrorCodes.INVALID_INPUT,
                    "said no",
                ),
                Arguments.of(
                    "500 with the proxy's envelope",
                    500,
                    """{"error":"internal_error","detail":"said no","api_version":"1.0.0"}""",
                    AtlasErrorCodes.INTERNAL_ERROR,
                    "said no",
                ),
                Arguments.of(
                    "502 with a non-JSON body, kept as the detail",
                    502,
                    "bad gateway",
                    "",
                    "bad gateway",
                ),
            )

        @JvmStatic
        fun tokenProbes(): List<Arguments> =
            listOf(
                Arguments.of(200, TokenProbe.Accepted),
                Arguments.of(401, TokenProbe.Rejected),
                Arguments.of(404, TokenProbe.Unknown),
            )

        val TURN =
            listOf(
                frame("turn_start", """{"turn":1,"messages":1,"trimmed":false}"""),
                frame("text", """{"content":"hello"}"""),
                frame("done", """{"summary":"done","status":"completed"}"""),
            )
    }
}

/** One SSE envelope, exactly as the proxy writes it. */
private fun frame(
    type: String,
    data: String,
): String = """{"type":"$type","data":$data}"""

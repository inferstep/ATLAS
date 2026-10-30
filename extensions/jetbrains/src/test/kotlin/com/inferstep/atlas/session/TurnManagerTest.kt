// TurnManager: session_id minting, request shape, rolling history (40-cap,
// assistant envelope re-wrap), session_allowed_tools re-send, the busy guard
// and cancel. The task contract is checked in ChatSessionTest, where the user
// picks the mode.
//
// The cases of extensions/vscode/test/turnManager.test.ts, so the two clients
// cannot drift apart on what a turn looks like on the wire.
package com.inferstep.atlas.session

import com.inferstep.atlas.protocol.AgentRequest
import com.inferstep.atlas.protocol.AtlasJson
import com.inferstep.atlas.protocol.ChatEvent
import com.inferstep.atlas.protocol.PermissionMode
import com.inferstep.atlas.protocol.TaskMode
import com.inferstep.atlas.protocol.assistantHistoryContent
import kotlinx.coroutines.awaitCancellation
import kotlinx.coroutines.cancelAndJoin
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.collect
import kotlinx.coroutines.flow.flow
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNotEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import org.junit.jupiter.params.ParameterizedTest
import org.junit.jupiter.params.provider.Arguments
import org.junit.jupiter.params.provider.MethodSource

private const val WORKSPACE = "/tmp/ws"

private fun decode(frame: String): ChatEvent =
    AtlasJson.decodeFromString(ChatEvent.serializer(), frame)

/** The `text` event, as the proxy sends it. */
private fun text(content: String): ChatEvent =
    decode("""{"type":"text","data":{"content":"$content"}}""")

/** The `done` event with the given summary. */
private fun done(summary: String): ChatEvent =
    decode("""{"type":"done","data":{"summary":"$summary"}}""")

/**
 * A fake [TurnClient]: records the requests it is given and replays canned
 * frames. [hangAfter] makes it stop after that frame and wait forever, which
 * is what an in-flight turn looks like while a turn is being cancelled or a
 * second one is rejected.
 */
private class FakeTurnClient(
    private val frames: List<ChatEvent> = emptyList(),
) : TurnClient {
    val requests = mutableListOf<AgentRequest>()
    val cancelled = mutableListOf<String>()
    var hangAfter: Int? = null

    override fun agentTurn(request: AgentRequest): Flow<ChatEvent> =
        flow {
            requests += request
            frames.forEachIndexed { index, event ->
                emit(event)
                if (hangAfter == index) {
                    awaitCancellation()
                }
            }
        }

    override suspend fun cancelTurn(sessionId: String): Boolean {
        cancelled += sessionId
        return true
    }
}

/**
 * One turn of [message], awaited to completion, with the conventional
 * permission mode, task mode and workspace. A case that is about one of those
 * values changing calls [TurnManager.runTurn] with it itself.
 */
private suspend fun TurnManager.runOneTurn(
    client: FakeTurnClient,
    message: String,
) {
    runTurn(client, message, PermissionMode.DEFAULT, TaskMode.WORK, WORKSPACE).collect {}
}

class TurnManagerTest {
    @Test
    fun `sends the conventional request shape and yields every event`() =
        runBlocking {
            val client = FakeTurnClient(listOf(text("hi"), done("")))
            val turns = TurnManager(scope = this)

            val events = mutableListOf<ChatEvent>()
            turns
                .runTurn(client, "hello", PermissionMode.DEFAULT, TaskMode.WORK, WORKSPACE)
                .collect { events += it }

            assertEquals(listOf("text", "done"), events.map { it.type })
            val request = client.requests.single()
            assertEquals("hello", request.message)
            // The caller's workspace root goes on the wire verbatim — the proxy
            // uses it for host-to-container path translation.
            assertEquals(WORKSPACE, request.workingDir)
            assertEquals(PermissionMode.DEFAULT, request.mode)
            // A minted id: 24 hex characters, the TUI's recipe.
            assertTrue(request.sessionId.matches(Regex("[0-9a-f]{24}")), request.sessionId)
            // Absent, not empty: the proxy treats an empty history as a history.
            assertNull(request.history)
            assertNull(request.sessionAllowedTools)
            assertEquals(request.sessionId, turns.sessionId)
        }

    @Test
    fun `mints a fresh session id per turn`() =
        runBlocking {
            val client = FakeTurnClient(listOf(done("")))
            val turns = TurnManager(scope = this)

            turns.runOneTurn(client, "one")
            turns.runOneTurn(client, "two")

            assertNotEquals(client.requests[0].sessionId, client.requests[1].sessionId)
        }

    @ParameterizedTest(name = "{0}")
    @MethodSource("histories")
    fun `replays the turn before as history, one entry per side`(
        name: String,
        frames: List<ChatEvent>,
        want: List<String>,
    ) = runBlocking {
        val client = FakeTurnClient(frames)
        val turns = TurnManager(scope = this)

        turns.runOneTurn(client, "first")
        turns.runOneTurn(client, "second")

        assertEquals(want, client.requests[1].history?.map { "${it.role}|${it.content}" }, name)
    }

    @Test
    fun `caps history at the most recent $HISTORY_LIMIT entries`() =
        runBlocking {
            val client = FakeTurnClient(listOf(text("r"), done("")))
            val turns = TurnManager(scope = this)

            // 25 turns x 2 entries = 50 recorded, which is over the cap.
            repeat(25) { index -> turns.runOneTurn(client, "msg $index") }
            turns.runOneTurn(client, "final")

            val history =
                client.requests
                    .last()
                    .history
                    .orEmpty()
            assertEquals(HISTORY_LIMIT, history.size)
            // Oldest surviving entries come from the tail of the run, not turn 0.
            assertFalse(history.any { it.content.contains("msg 0") })
            assertEquals("msg 24", history[history.size - 2].content)
            assertEquals(assistantHistoryContent("r"), history.last().content)
        }

    @Test
    fun `re-sends sessionAllowedTools on every turn once populated`() =
        runBlocking {
            val client = FakeTurnClient(listOf(done("")))
            val turns = TurnManager(scope = this)
            turns.sessionAllowedTools += "edit_file"
            turns.sessionAllowedTools += "write_file"

            turns.runOneTurn(client, "one")
            turns.runOneTurn(client, "two")

            assertEquals(listOf("edit_file", "write_file"), client.requests[0].sessionAllowedTools)
            assertEquals(listOf("edit_file", "write_file"), client.requests[1].sessionAllowedTools)
        }

    @Test
    fun `rejects a second concurrent turn`() =
        runBlocking {
            val client = FakeTurnClient(listOf(text("first")))
            client.hangAfter = 0
            val turns = TurnManager(scope = this)

            val first = launch { turns.runOneTurn(client, "slow") }
            // Let the first turn reach its first suspension point.
            letTheLaunchedWorkRun()
            assertTrue(turns.busy)

            assertThrows(IllegalStateException::class.java) {
                runBlocking { turns.runOneTurn(client, "second") }
            }

            turns.cancel()
            first.cancelAndJoin()
        }

    @Test
    fun `cancel aborts the stream, posts cancel, and keeps partial text`() =
        runBlocking {
            val client = FakeTurnClient(listOf(text("half an ans")))
            client.hangAfter = 0
            val turns = TurnManager(scope = this)

            val seen = mutableListOf<ChatEvent>()
            val turn =
                launch {
                    turns
                        .runTurn(client, "go", PermissionMode.DEFAULT, TaskMode.WORK, WORKSPACE)
                        .collect { event ->
                            seen += event
                            turns.cancel()
                        }
                }
            turn.join()
            // The POST /cancel is fire-and-forget.
            letTheLaunchedWorkRun()

            assertEquals(listOf("text"), seen.map { it.type })
            assertEquals(listOf(client.requests.single().sessionId), client.cancelled)
            assertFalse(turns.busy)

            // The next turn replays the partial text: a cancelled turn still
            // counts as an assistant turn.
            client.hangAfter = null
            turns.runOneTurn(client, "next")
            assertEquals(
                listOf("user|go", "assistant|${assistantHistoryContent("half an ans")}"),
                client.requests[1].history?.map { "${it.role}|${it.content}" },
            )
        }

    @Test
    fun `cancel when idle is a no-op`() =
        runBlocking {
            val client = FakeTurnClient(listOf(done("")))
            val turns = TurnManager(scope = this)
            // The manager has run a turn with this client, so a cancel that did
            // not check for idle would have somewhere to post.
            turns.runOneTurn(client, "one")

            turns.cancel()
            letTheLaunchedWorkRun()

            assertEquals(emptyList<String>(), client.cancelled)
        }

    @Test
    fun `reset clears the history and the session-approved tools`() =
        runBlocking {
            val client = FakeTurnClient(listOf(text("a"), done("")))
            val turns = TurnManager(scope = this)
            turns.sessionAllowedTools += "edit_file"

            turns.runOneTurn(client, "one")
            turns.reset()
            turns.runOneTurn(client, "two")

            assertNull(client.requests[1].history)
            assertNull(client.requests[1].sessionAllowedTools)
        }

    companion object {
        @JvmStatic
        fun histories(): List<Arguments> {
            val toolTurnSummary = "Fixed the off-by-one in pagination."
            return listOf(
                Arguments.of(
                    "streamed text is re-wrapped in the assistant envelope",
                    listOf(text("Hello "), text("world"), done("")),
                    listOf("user|first", "assistant|${assistantHistoryContent("Hello world")}"),
                ),
                Arguments.of(
                    "a turn that produced no text records no assistant entry",
                    listOf(decode("""{"type":"error","data":{"error":"boom"}}""")),
                    listOf("user|first"),
                ),
                Arguments.of(
                    "a tool-shaped turn takes the done summary",
                    listOf(
                        decode(
                            """{"type":"tool_call","data":{"name":"edit_file","args":{},"turn":1}}""",
                        ),
                        done(toolTurnSummary),
                    ),
                    listOf(
                        "user|first",
                        "assistant|${assistantHistoryContent(toolTurnSummary)}",
                    ),
                ),
                Arguments.of(
                    "streamed text wins over a trailing done summary",
                    listOf(text("the streamed answer"), done("a trailing summary")),
                    listOf(
                        "user|first",
                        "assistant|${assistantHistoryContent("the streamed answer")}",
                    ),
                ),
            )
        }
    }
}

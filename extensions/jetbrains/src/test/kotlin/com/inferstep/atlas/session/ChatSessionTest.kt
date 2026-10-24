// ChatSession: what the panel would render, as values. Streaming text, tool
// chips with their summaries, the done banner, live permission prompts,
// auto-allowed tools, cancellation, the two mode controls and reset.
package com.inferstep.atlas.session

import com.inferstep.atlas.protocol.AgentRequest
import com.inferstep.atlas.protocol.AtlasJson
import com.inferstep.atlas.protocol.ChatEvent
import com.inferstep.atlas.protocol.PermissionDecision
import com.inferstep.atlas.protocol.PermissionDecisionRequest
import com.inferstep.atlas.protocol.PermissionMode
import com.inferstep.atlas.protocol.PermissionScope
import com.inferstep.atlas.protocol.TaskContract
import com.inferstep.atlas.protocol.TaskMode
import com.inferstep.atlas.protocol.WorkspaceResponse
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.awaitCancellation
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flow
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import org.junit.jupiter.params.ParameterizedTest
import org.junit.jupiter.params.provider.Arguments
import org.junit.jupiter.params.provider.MethodSource

private fun event(frame: String): ChatEvent =
    AtlasJson.decodeFromString(ChatEvent.serializer(), frame)

private fun textFrame(content: String): ChatEvent =
    event("""{"type":"text","data":{"content":"$content"}}""")

private fun doneFrame(json: String): ChatEvent = event("""{"type":"done","data":$json}""")

private class RecordingClient(
    private val frames: List<ChatEvent>,
) : TurnClient {
    val requests = mutableListOf<AgentRequest>()
    var hangAfter: Int? = null

    /** Makes the stream fail the way a non-2xx response does. */
    var failWith: Throwable? = null

    override fun agentTurn(request: AgentRequest): Flow<ChatEvent> =
        flow {
            requests += request
            failWith?.let { error -> throw error }
            frames.forEachIndexed { index, event ->
                emit(event)
                if (hangAfter == index) {
                    awaitCancellation()
                }
            }
        }

    override suspend fun cancelTurn(sessionId: String): Boolean = true
}

private class RecordingPoster : PermissionPoster {
    val decisions = mutableListOf<PermissionDecisionRequest>()

    override suspend fun postPermissionDecision(decision: PermissionDecisionRequest) {
        decisions += decision
    }
}

/** A workspace the tests script directly: no file IO, and every read and
 * refresh is visible to the assertions. */
private class FakeWorkspace(
    override val root: String? = "/tmp/ws",
) : Workspace {
    val files = mutableMapOf<String, String>()
    val changed = mutableListOf<String>()

    /** The contents successive reads return, per path; the last one repeats.
     * This is how a test says "the proxy wrote while the call was in flight"
     * without depending on when an event is emitted relative to its
     * collection: the snapshot read sees entry one, the post-call read entry
     * two. */
    val reads = mutableMapOf<String, ArrayDeque<String?>>()

    override fun readText(relativePath: String): String? {
        val queued = reads[relativePath]
        if (queued != null && queued.isNotEmpty()) {
            val next = queued.removeFirst()
            if (queued.isEmpty()) {
                queued.addLast(next)
            }
            return next
        }
        return files[relativePath]
    }

    /** Nothing is ever on disk here, so the mismatch check finds every
     * successful file op missing and warns. */
    override fun stat(relativePath: String): PathStat = PathStat(exists = false)

    override fun changed(relativePath: String) {
        changed += relativePath
    }
}

private class SessionFixture(
    frames: List<ChatEvent>,
    workspace: FakeWorkspace = FakeWorkspace(),
    probe: WorkspaceProbe = WorkspaceProbe { null },
) {
    val scope = CoroutineScope(Dispatchers.Unconfined)
    val client = RecordingClient(frames)
    val poster = RecordingPoster()
    val session = ChatSession(client, poster, scope, "/tmp/ws", workspace, probe)

    /** What the panel would render right now. */
    val state: ChatUiState
        get() = session.state.value
}

class ChatSessionTest {
    @ParameterizedTest(name = "{0}")
    @MethodSource("streams")
    fun `reads a stream into the transcript and ends the turn`(
        name: String,
        message: String,
        frames: List<ChatEvent>,
        want: List<TranscriptEntry>,
    ) = runBlocking {
        val fixture = SessionFixture(frames)

        fixture.session.send(message)

        assertEquals(want, fixture.state.entries, name)
        assertFalse(fixture.state.running, name)
    }

    @Test
    fun `shows a prompt with its predicted change, and posts the answer`() =
        runBlocking {
            val workspace = FakeWorkspace()
            workspace.files["app.py"] = "old"
            val fixture =
                SessionFixture(
                    listOf(
                        event(
                            """{"type":"permission_request","data":{"tool_name":"write_file",""" +
                                """"args":{"path":"app.py","content":"new"},""" +
                                """"message":"write app.py?","tool_call_id":"c1"}}""",
                        ),
                    ),
                    workspace,
                )
            // The proxy pauses the turn while a permission is pending, which
            // is the only time a prompt is on screen.
            fixture.client.hangAfter = 0

            val running = launch { fixture.session.send("write it") }
            letTheLaunchedWorkRun()

            val pending = fixture.state.pending.single()
            assertEquals("write_file", pending.request.toolName)
            assertEquals(
                EditPrediction("app.py", EditPredictionKind.FILE, "old", "new", false),
                fixture.state.previews[pending.id],
            )

            fixture.session.answer(pending.id, PermissionChoice.ALLOW_SESSION)
            letTheLaunchedWorkRun()

            // The decision has to go back on the turn's own session id, or the
            // proxy has no pending request to route it to.
            val decision = fixture.poster.decisions.single()
            assertEquals("c1", decision.toolCallId)
            assertEquals(PermissionDecision.ALLOW, decision.decision)
            assertEquals(PermissionScope.SESSION, decision.scope)
            val turn = fixture.client.requests.single()
            assertEquals(turn.sessionId, decision.sessionId)
            // The card is gone, and its prediction with it, while the turn goes on.
            assertTrue(fixture.state.pending.isEmpty())
            assertEquals(emptyMap<Long, EditPrediction>(), fixture.state.previews)
            assertTrue(fixture.state.running)

            fixture.session.cancel()
            running.join()
            assertFalse(fixture.state.running)
        }

    @Test
    fun `auto-answers a tool the user allowed for the session, on the next turn`() =
        runBlocking {
            val fixture =
                SessionFixture(
                    listOf(
                        event(
                            """{"type":"permission_request","data":{"tool_name":"edit_file",""" +
                                """"args":{"path":"a.py"},"tool_call_id":"c2"}}""",
                        ),
                    ),
                )
            fixture.client.hangAfter = 0

            // Turn 1: the user allows edit_file for the session.
            val first = launch { fixture.session.send("edit it") }
            letTheLaunchedWorkRun()
            val pending = fixture.state.pending.single()
            fixture.session.answer(pending.id, PermissionChoice.ALLOW_SESSION)
            letTheLaunchedWorkRun()
            fixture.session.cancel()
            first.join()

            // Turn 2: the same tool is now on the allowlist, so there is no
            // prompt and the proxy is told the answer instead.
            val second = launch { fixture.session.send("edit it again") }
            letTheLaunchedWorkRun()

            assertEquals(
                listOf(PermissionScope.SESSION, PermissionScope.ONCE),
                fixture.poster.decisions.map { it.scope },
            )
            assertTrue(fixture.state.pending.isEmpty())
            assertTrue(
                fixture.state.entries.any {
                    it is TranscriptEntry.Notice &&
                        it.text == "edit_file is allowed for this session."
                },
                fixture.state.entries.toString(),
            )

            fixture.session.cancel()
            second.join()
        }

    @Test
    fun `reports a failed turn instead of leaving the message unanswered`() =
        runBlocking {
            val fixture = SessionFixture(listOf(textFrame("never reached")))
            fixture.client.failWith = IllegalStateException("unauthorized: no token")

            fixture.session.send("go")

            val last = fixture.state.entries.last()
            assertTrue(last is TranscriptEntry.Notice, fixture.state.entries.toString())
            assertEquals(true, (last as TranscriptEntry.Notice).isError)
            assertTrue(last.text.contains("unauthorized: no token"), last.text)
            assertFalse(fixture.state.running)
        }

    @Test
    fun `declares the mode the user picked`() =
        runBlocking {
            val fixture = SessionFixture(listOf(doneFrame("""{"summary":"answered"}""")))

            fixture.session.setTaskMode(TaskMode.QUESTION)
            fixture.session.setPermissionMode(PermissionMode.ACCEPT_EDITS)
            fixture.session.send("what does solve.py do?")

            val request = fixture.client.requests.single()
            assertEquals(TaskContract(TaskMode.QUESTION), request.taskContract)
            assertEquals(PermissionMode.ACCEPT_EDITS, request.mode)
        }

    @Test
    fun `ignores an empty message and a second turn while one is running`() =
        runBlocking {
            val fixture = SessionFixture(listOf(textFrame("hi")))
            fixture.client.hangAfter = 0

            fixture.session.send("   ")
            assertEquals(0, fixture.state.entries.size)

            val running = launch { fixture.session.send("first") }
            letTheLaunchedWorkRun()
            fixture.session.send("second")

            // The second message leaves no trace: no entry, no request, and the
            // first turn is still running.
            assertEquals(
                listOf(TranscriptEntry.User(1, "first"), TranscriptEntry.Assistant(2, "hi")),
                fixture.state.entries,
            )
            assertEquals(1, fixture.client.requests.size)
            assertTrue(fixture.state.running)

            fixture.session.cancel()
            running.join()
        }

    @Test
    fun `cancel stops the turn, closes its prompts and says so`() =
        runBlocking {
            val fixture =
                SessionFixture(
                    listOf(
                        event(
                            """{"type":"permission_request","data":{"tool_name":"delete_file",""" +
                                """"args":{"path":"important.db"},"tool_call_id":"c3",""" +
                                """"one_time_only":true}}""",
                        ),
                    ),
                )
            fixture.client.hangAfter = 0

            val running = launch { fixture.session.send("delete it") }
            letTheLaunchedWorkRun()
            assertEquals(1, fixture.state.pending.size)

            fixture.session.cancel()
            running.join()
            letTheLaunchedWorkRun()

            assertFalse(fixture.state.running)
            assertEquals(emptyList<PendingPermission>(), fixture.state.pending)
            val last = fixture.state.entries.last()
            assertTrue(last is TranscriptEntry.Notice, fixture.state.entries.toString())
            assertEquals("Turn cancelled.", (last as TranscriptEntry.Notice).text)
            assertFalse(last.isError)
        }

    @Test
    fun `reset clears the transcript and the history, and keeps the chosen modes`() =
        runBlocking {
            val fixture =
                SessionFixture(listOf(textFrame("hi"), doneFrame("""{"summary":"done"}""")))
            fixture.session.setTaskMode(TaskMode.QUESTION)
            fixture.session.send("go")

            fixture.session.reset()

            assertEquals(emptyList<TranscriptEntry>(), fixture.state.entries)
            assertEquals(TaskMode.QUESTION, fixture.state.taskMode)

            // The turn after the reset is the one that shows the history is
            // gone: it would carry turn 1's entries back to the proxy.
            fixture.session.send("again")

            assertEquals(2, fixture.client.requests.size)
            val second = fixture.client.requests[1]
            assertEquals("again", second.message)
            assertNull(second.history)
        }

    @Test
    fun `captures the exact change a successful file op made`() =
        runBlocking {
            val workspace = FakeWorkspace()
            // Snapshot read, then post-call read: the proxy wrote in between.
            workspace.reads["a.py"] = ArrayDeque(listOf("old", "new"))
            val fixture =
                SessionFixture(
                    listOf(
                        event(
                            """{"type":"tool_call","data":{"name":"write_file",""" +
                                """"args":{"path":"a.py","content":"new"},"turn":1}}""",
                        ),
                        event(
                            """{"type":"tool_result","data":{"tool":"write_file",""" +
                                """"success":true,"data":{"ok":true}}}""",
                        ),
                    ),
                    workspace,
                )

            fixture.session.send("write it")

            // The last entry is the mismatch notice (the scripted workspace has
            // no stats), so the change is read off the result row itself.
            val result =
                fixture.state.entries
                    .filterIsInstance<TranscriptEntry.Tool>()
                    .last()
            assertEquals(AppliedChange("a.py", "old", "new"), result.change)
            // And the IDE is told, so a new file shows up without waiting for
            // its own VFS refresh.
            assertEquals(listOf("a.py"), workspace.changed)
        }

    @Test
    fun `a denied call's failed result does not leave its snapshot for the next file op`() =
        runBlocking {
            val workspace = FakeWorkspace()
            workspace.files["a.py"] = "old-a"
            workspace.reads["b.py"] = ArrayDeque(listOf("old-b", "new-b"))
            val fixture =
                SessionFixture(
                    listOf(
                        event(
                            """{"type":"tool_call","data":{"name":"write_file",""" +
                                """"args":{"path":"a.py","content":"x"},"turn":1}}""",
                        ),
                        event(
                            """{"type":"permission_denied","data":{"tool":"write_file"}}""",
                        ),
                        // The proxy answers a denied call with this failed
                        // result, in the same block that streams the denial.
                        event(
                            """{"type":"tool_result","data":{"tool":"write_file",""" +
                                """"success":false,"error":"denied","data":null}}""",
                        ),
                        event(
                            """{"type":"tool_call","data":{"name":"write_file",""" +
                                """"args":{"path":"b.py","content":"y"},"turn":2}}""",
                        ),
                        event(
                            """{"type":"tool_result","data":{"tool":"write_file",""" +
                                """"success":true,"data":{"ok":true}}}""",
                        ),
                    ),
                    workspace,
                )

            fixture.session.send("go")

            // The denied a.py call's failed result consumed its snapshot, so
            // the successful result pairs with b.py — not with a.py, which the
            // proxy never wrote.
            val result =
                fixture.state.entries
                    .filterIsInstance<TranscriptEntry.Tool>()
                    .last()
            assertEquals(AppliedChange("b.py", "old-b", "new-b"), result.change)
        }

    @Test
    fun `warns when a successful file op is nowhere on local disk`() =
        runBlocking {
            val fixture =
                SessionFixture(
                    listOf(
                        event(
                            """{"type":"tool_call","data":{"name":"write_file",""" +
                                """"args":{"path":"a.py","content":"x"},"turn":1}}""",
                        ),
                        event(
                            """{"type":"tool_result","data":{"tool":"write_file",""" +
                                """"success":true,"data":{"ok":true}}}""",
                        ),
                        doneFrame("""{"summary":"ok"}"""),
                    ),
                    FakeWorkspace(), // the file never appears here
                )

            fixture.session.send("write it")

            val notice =
                fixture.state.entries
                    .filterIsInstance<TranscriptEntry.Notice>()
                    .single()
            assertTrue(notice.text.contains("a different directory"), notice.text)
            assertFalse(notice.isError)
            // The warning flag is what makes AtlasNotifier surface this as a
            // native warning rather than a silent transcript line.
            assertTrue(notice.isWarning, notice.text)
        }

    @Test
    fun `says when the proxy edits another folder, and stays quiet when it agrees`() =
        runBlocking {
            val openFolder = "/home/isaac/demo2"
            val misaligned =
                SessionFixture(
                    emptyList(),
                    FakeWorkspace(root = openFolder),
                    probe =
                        WorkspaceProbe {
                            WorkspaceResponse(projectDir = "/home/isaac/snake-game")
                        },
                )
            misaligned.session.checkWorkspace()

            val notice = misaligned.state.entries.single() as TranscriptEntry.Notice
            assertTrue(notice.text.contains("/home/isaac/snake-game"), notice.text)
            assertTrue(notice.isWarning, notice.text)

            val aligned =
                SessionFixture(
                    emptyList(),
                    FakeWorkspace(root = openFolder),
                    probe = WorkspaceProbe { WorkspaceResponse(projectDir = openFolder) },
                )
            aligned.session.checkWorkspace()
            assertEquals(0, aligned.state.entries.size)

            // An older proxy without the route, or one that cannot name an
            // absolute path, must produce no warning at all.
            val noRoute = SessionFixture(emptyList(), probe = WorkspaceProbe { null })
            noRoute.session.checkWorkspace()
            assertEquals(0, noRoute.state.entries.size)
        }

    @Test
    fun `a swapped connection takes effect on the next turn and leaves the open prompt alone`() =
        runBlocking {
            val fixture =
                SessionFixture(
                    listOf(
                        event(
                            """{"type":"permission_request","data":{"tool_name":"write_file",""" +
                                """"args":{"path":"a.py"},"tool_call_id":"c1"}}""",
                        ),
                    ),
                )
            // The proxy pauses on a prompt, so the turn stays in flight while
            // the settings change lands.
            fixture.client.hangAfter = 0
            val running = launch { fixture.session.send("first") }
            letTheLaunchedWorkRun()
            val prompt = fixture.state.pending.single()

            val nextClient = RecordingClient(listOf(doneFrame("""{"summary":"second"}""")))
            val nextPoster = RecordingPoster()
            fixture.session.updateConnection(nextClient, nextPoster, WorkspaceProbe { null })

            // The open prompt belongs to the turn that produced it: its answer
            // must go to that turn's poster, not the one settings just built.
            fixture.session.answer(prompt.id, PermissionChoice.ALLOW_ONCE)
            letTheLaunchedWorkRun()
            assertEquals(1, fixture.poster.decisions.size)
            assertEquals(0, nextPoster.decisions.size)

            fixture.session.cancel()
            running.join()

            fixture.session.send("second")

            assertEquals(1, fixture.client.requests.size)
            assertEquals(1, nextClient.requests.size)
        }

    @ParameterizedTest(name = "{0}")
    @MethodSource("progress")
    fun `shows the latest progress line and clears it when the step is done`(
        name: String,
        frames: List<ChatEvent>,
        hangAfter: Int,
        want: String?,
    ) = runBlocking {
        val fixture = SessionFixture(frames)
        fixture.client.hangAfter = hangAfter

        val running = launch { fixture.session.send("go") }
        letTheLaunchedWorkRun()

        assertEquals(want, fixture.state.progress, name)

        fixture.session.cancel()
        running.join()
    }

    @Test
    fun `renders the plan and which steps the proxy satisfied`() =
        runBlocking {
            val fixture =
                SessionFixture(
                    listOf(
                        event(
                            """{"type":"plan_loaded","data":{"steps":[""" +
                                """{"id":"s1","action":"edit","target":"app.py"},""" +
                                """{"id":"s2","action":"run","target":"pytest"}],""" +
                                """"verify_step":"s2","revision":0}}""",
                        ),
                        event(
                            """{"type":"plan_adherence","data":{"matched":true,""" +
                                """"step_id":"s1","satisfied":1,"total":2}}""",
                        ),
                        event(
                            """{"type":"plan_revise","data":{"reason":"off-plan streak",""" +
                                """"revision":1}}""",
                        ),
                        doneFrame("""{"summary":"ok"}"""),
                    ),
                )

            fixture.session.send("go")

            assertEquals(
                ChatPlan(
                    listOf(
                        ChatPlanStep("s1", "edit app.py"),
                        ChatPlanStep("s2", "run pytest"),
                    ),
                    revision = 0,
                    satisfied = setOf("s1"),
                ),
                fixture.state.plan,
            )
            assertTrue(
                fixture.state.entries.any {
                    it is TranscriptEntry.Notice && it.text.contains("revising")
                },
                fixture.state.entries.toString(),
            )

            fixture.session.reset()
            assertNull(fixture.state.plan)
        }

    companion object {
        @JvmStatic
        fun streams(): List<Arguments> =
            listOf(
                Arguments.of(
                    "deltas collect into one assistant entry, then the banner",
                    "plan it",
                    listOf(
                        textFrame("Here is "),
                        textFrame("the plan."),
                        doneFrame("""{"summary":"Plan ready.","status":"completed"}"""),
                    ),
                    listOf(
                        TranscriptEntry.User(1, "plan it"),
                        TranscriptEntry.Assistant(2, "Here is the plan."),
                        TranscriptEntry.Done(
                            3,
                            DoneOutcome("Plan ready.", "completed", "completed", "", "completed"),
                        ),
                    ),
                ),
                Arguments.of(
                    "a tool call shows as a chip, and its result adds the summary and the outcome",
                    "list the files",
                    listOf(
                        event(
                            """{"type":"tool_call","data":{"name":"list_directory",""" +
                                """"args":{},"turn":1}}""",
                        ),
                        event(
                            """{"type":"tool_result","data":{"tool":"list_directory",""" +
                                """"success":true,"data":{"entries":""" +
                                """[{"name":"app.py","type":"file"}]}}}""",
                        ),
                    ),
                    listOf(
                        TranscriptEntry.User(1, "list the files"),
                        TranscriptEntry.Tool(2, "list_directory", null, null),
                        TranscriptEntry.Tool(3, "list_directory", "1 entries: app.py", true),
                    ),
                ),
                Arguments.of(
                    "an error event becomes an error notice",
                    "go",
                    listOf(event("""{"type":"error","data":{"error":"lens down"}}""")),
                    listOf(
                        TranscriptEntry.User(1, "go"),
                        TranscriptEntry.Notice(2, "lens down", isError = true),
                    ),
                ),
                Arguments.of(
                    "progress events never reach the transcript",
                    "go",
                    listOf(
                        event("""{"type":"llm_token","data":{"text":"thinking"}}"""),
                        event(
                            """{"type":"v3_phase","data":{"stage":"phase2","detail":"allocating"}}""",
                        ),
                        event(
                            """{"type":"plan_loaded","data":{"steps":[],"verify_step":"s1"}}""",
                        ),
                        doneFrame("""{"summary":"done"}"""),
                    ),
                    listOf(
                        TranscriptEntry.User(1, "go"),
                        TranscriptEntry.Done(
                            2,
                            DoneOutcome("done", "incomplete", "incomplete", "", "incomplete"),
                        ),
                    ),
                ),
            )

        @JvmStatic
        fun progress(): List<Arguments> =
            listOf(
                Arguments.of(
                    "an LLM call says it is thinking",
                    listOf(event("""{"type":"llm_call_start","data":{}}""")),
                    0,
                    "Thinking…",
                ),
                Arguments.of(
                    "prompt evaluation shows its percentage",
                    listOf(event("""{"type":"llm_prompt_progress","data":{"pct":0.42}}""")),
                    0,
                    "Processing prompt 42%…",
                ),
                Arguments.of(
                    "a V3 fallback message is labelled",
                    listOf(event("""{"type":"v3_progress","data":{"message":"planning"}}""")),
                    0,
                    "V3: planning",
                ),
                Arguments.of(
                    "a typed V3 stage keeps its detail",
                    listOf(
                        event(
                            """{"type":"v3_sandbox","data":{"stage":"sandbox_pass",""" +
                                """"detail":"2/3"}}""",
                        ),
                    ),
                    0,
                    "V3: sandbox_pass — 2/3",
                ),
                Arguments.of(
                    "the first token clears the thinking line",
                    listOf(
                        event("""{"type":"llm_call_start","data":{}}"""),
                        event("""{"type":"llm_first_token","data":{}}"""),
                    ),
                    1,
                    null,
                ),
                Arguments.of(
                    "the end of an LLM call clears a V3 line",
                    listOf(
                        event("""{"type":"v3_progress","data":{"message":"planning"}}"""),
                        event("""{"type":"llm_call_end","data":{}}"""),
                    ),
                    1,
                    null,
                ),
            )
    }
}

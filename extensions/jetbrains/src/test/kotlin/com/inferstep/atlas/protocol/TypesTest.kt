// Tests for the protocol types: the wire names must match the proxy's tags,
// absent optionals must stay absent, and the documented forward-compatibility
// and done-status rules must hold.
package com.inferstep.atlas.protocol

import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class TypesTest {
    @Test
    fun `agent request uses the proxy's field names and omits absent optionals`() {
        val request =
            AgentRequest(
                message = "fix the bug in app.py",
                workingDir = "/workspace",
                mode = PermissionMode.DEFAULT,
                sessionId = "client-1",
            )

        val encoded = AtlasJson.encodeToString(AgentRequest.serializer(), request)

        assertTrue(encoded.contains("\"working_dir\":\"/workspace\""), encoded)
        assertTrue(encoded.contains("\"session_id\":\"client-1\""), encoded)
        assertFalse(encoded.contains("history"), encoded)
        assertFalse(encoded.contains("session_allowed_tools"), encoded)
    }

    @Test
    fun `permission modes use the exact wire strings, hyphens included`() {
        assertEquals("default", wireMode(PermissionMode.DEFAULT))
        assertEquals("accept-edits", wireMode(PermissionMode.ACCEPT_EDITS))
        assertEquals("yolo", wireMode(PermissionMode.YOLO))
    }

    @Test
    fun `a history and session allow-list survive a round-trip`() {
        val request =
            AgentRequest(
                message = "next",
                workingDir = ".",
                mode = PermissionMode.ACCEPT_EDITS,
                sessionId = "s",
                history =
                    listOf(
                        HistoryMessage("user", "hi"),
                        HistoryMessage("assistant", "hello"),
                    ),
                sessionAllowedTools = listOf("write_file"),
            )

        val decoded =
            AtlasJson.decodeFromString(
                AgentRequest.serializer(),
                AtlasJson.encodeToString(AgentRequest.serializer(), request),
            )

        assertEquals(request, decoded)
    }

    @Test
    fun `permission decision and cancel requests use the documented shape`() {
        val decision =
            PermissionDecisionRequest(
                sessionId = "tui-1",
                toolCallId = "call_3",
                decision = PermissionDecision.ALLOW,
                scope = PermissionScope.ONCE,
            )
        val encoded = AtlasJson.encodeToString(PermissionDecisionRequest.serializer(), decision)

        assertEquals(
            """{"session_id":"tui-1","tool_call_id":"call_3","decision":"allow","scope":"once"}""",
            encoded,
        )
        assertEquals(
            """{"session_id":"tui-1"}""",
            AtlasJson.encodeToString(CancelRequest.serializer(), CancelRequest("tui-1")),
        )
    }

    @Test
    fun `an unknown event type still parses and keeps its payload`() {
        val event =
            AtlasJson.decodeFromString(
                ChatEvent.serializer(),
                """{"type":"pattern_context_injected","data":{"patterns":3},"extra":true}""",
            )

        assertEquals("pattern_context_injected", event.type)
        assertEquals(
            3,
            (event.data as JsonObject)["patterns"]!!.jsonPrimitive.content.toInt(),
        )
    }

    @Test
    fun `a payload of the wrong JSON kind decodes to null rather than throwing`() {
        val arrayData =
            AtlasJson.decodeFromString(ChatEvent.serializer(), """{"type":"text","data":[1,2]}""")
        val scalarData =
            AtlasJson.decodeFromString(ChatEvent.serializer(), """{"type":"text","data":"nope"}""")

        assertNull(arrayData.payloadOrNull<TextPayload>())
        assertNull(scalarData.payloadOrNull<TextPayload>())
    }

    @Test
    fun `unknown keys inside a known payload are ignored, not an error`() {
        // The forward-compatibility rule: a proxy that adds a field must not
        // break a client that has never heard of it.
        val payload =
            AtlasJson.decodeFromString(
                TextPayload.serializer(),
                """{"content":"hi","tone":"warm","confidence":0.9}""",
            )

        assertEquals("hi", payload.content)
    }

    @Test
    fun `an event whose type is missing is a decode failure, not an event`() {
        val failure =
            runCatching {
                AtlasJson.decodeFromString(ChatEvent.serializer(), """{"data":{"content":"x"}}""")
            }

        assertTrue(failure.isFailure)
    }

    @Test
    fun `unknown fields inside a known payload are ignored`() {
        val payload =
            AtlasJson.decodeFromString(
                DonePayload.serializer(),
                """{"summary":"done","status":"completed","reason":"x","future_field":42}""",
            )

        assertEquals("done", payload.summary)
        assertEquals(DoneStatus.COMPLETED, payload.resolvedStatus())
    }

    @Test
    fun `an absent or unrecognized done status reads as incomplete`() {
        assertEquals(DoneStatus.COMPLETED, DoneStatus.from("completed"))
        assertEquals(DoneStatus.STOPPED, DoneStatus.from("stopped"))
        assertEquals(DoneStatus.TIMED_OUT, DoneStatus.from("timed_out"))
        assertEquals(DoneStatus.FAILED, DoneStatus.from("failed"))
        assertEquals(DoneStatus.INCOMPLETE, DoneStatus.from("incomplete"))
        assertEquals(DoneStatus.INCOMPLETE, DoneStatus.from(null))
        assertEquals(DoneStatus.INCOMPLETE, DoneStatus.from("something_new"))
        assertTrue(DoneStatus.COMPLETED.isCompleted())
        assertFalse(DoneStatus.INCOMPLETE.isCompleted())
    }

    @Test
    fun `a done payload carries the additive unresolved and repair fields`() {
        val payload =
            AtlasJson.decodeFromString(
                DonePayload.serializer(),
                """{"summary":"s","status":"completed","reason":"r","unresolved":"gate_a,gate_b","repair_open":"a.py"}""",
            )

        assertEquals("gate_a,gate_b", payload.unresolved)
        assertEquals("a.py", payload.repairOpen)
        assertTrue(payload.resolvedStatus().isCompleted())
    }

    @Test
    fun `ready carries the lens reason only when the lens cannot score`() {
        val notReady =
            AtlasJson.decodeFromString(
                ReadyResponse.serializer(),
                """{"ready":false,"inference":true,"lens_ready":false,"sandbox":true,"v3":true,"lens_reason":"no artifacts"}""",
            )

        assertFalse(notReady.ready)
        assertEquals("no artifacts", notReady.lensReason)

        val ready = AtlasJson.decodeFromString(ReadyResponse.serializer(), """{"ready":true}""")
        assertTrue(ready.ready)
        assertNull(ready.lensReason)
    }

    @Test
    fun `version keeps the additive measurement fields optional`() {
        val full =
            AtlasJson.decodeFromString(
                VersionResponse.serializer(),
                """{"api_version":"1.0.0","protocol_version":1,"error_codes":["unauthorized"],"grammar_mode":"strict","session_timeout_s":600}""",
            )
        assertEquals("strict", full.grammarMode)
        assertEquals(600, full.sessionTimeoutSeconds)

        val sparse =
            AtlasJson.decodeFromString(
                VersionResponse.serializer(),
                """{"api_version":"1.0.0","protocol_version":1,"error_codes":[]}""",
            )
        assertNull(sparse.grammarMode)
        assertNull(sparse.sessionTimeoutSeconds)
    }

    @Test
    fun `a deletion permission request carries the structured target`() {
        val payload =
            AtlasJson.decodeFromString(
                PermissionRequestPayload.serializer(),
                """{"tool_name":"delete_file","args":{"path":"a.py"},"message":"remove a.py",""" +
                    """"tool_call_id":"call_0","canonical_path":"/workspace/a.py",""" +
                    """"target_type":"file","content_sha256":"ab","one_time_only":true}""",
            )

        assertEquals("/workspace/a.py", payload.canonicalPath)
        assertEquals("file", payload.targetType)
        assertTrue(payload.oneTimeOnly)
    }

    @Test
    fun `a tool call keeps its arguments as raw JSON`() {
        val payload =
            AtlasJson.decodeFromString(
                ToolCallPayload.serializer(),
                """{"name":"edit_file","args":{"path":"a.py","old_str":"x"},"turn":2}""",
            )

        assertEquals("edit_file", payload.name)
        assertEquals(
            "x",
            ((payload.args as JsonObject)["old_str"]!!).jsonPrimitive.content,
        )
    }

    @Test
    fun `an error envelope parses and an empty one does not throw`() {
        val envelope =
            AtlasJson.decodeFromString(
                ErrorEnvelope.serializer(),
                """{"error":"unauthorized","detail":"missing token","api_version":"1.0.0"}""",
            )

        assertEquals(AtlasErrorCodes.UNAUTHORIZED, envelope.error)
        assertEquals("missing token", envelope.detail)

        val empty = AtlasJson.decodeFromString(ErrorEnvelope.serializer(), """{}""")
        assertEquals("", empty.error)
    }

    @Test
    fun `the error-code set is the proxy's six codes`() {
        assertEquals(
            listOf(
                "unauthorized",
                "invalid_input",
                "unsupported_operation",
                "dependency_unavailable",
                "resource_limit",
                "internal_error",
            ),
            AtlasErrorCodes.ALL,
        )
        assertEquals(6, AtlasErrorCodes.ALL.distinct().size)
    }

    @Test
    fun `plan loaded and adherence carry the fields the UI reads`() {
        val plan =
            AtlasJson.decodeFromString(
                PlanLoadedPayload.serializer(),
                """{"steps":[{"id":"s1","action":"edit","target":"a.py","why":"fix"}],""" +
                    """"verify_step":"s1","rationale":"because","winning_score":0.9,"revision":0}""",
            )

        assertEquals("a.py", plan.steps.single().target)
        assertEquals("s1", plan.verifyStep)

        val miss =
            AtlasJson.decodeFromString(
                PlanAdherencePayload.serializer(),
                """{"matched":false,"tool":"read_file","off_streak":0,"satisfied":0,"total":1,"neutral":true}""",
            )

        assertFalse(miss.matched)
        assertTrue(miss.neutral)
        assertEquals(0, miss.offStreak)
    }

    private fun wireMode(mode: PermissionMode): String =
        AtlasJson.encodeToString(PermissionMode.serializer(), mode).trim('"')
}

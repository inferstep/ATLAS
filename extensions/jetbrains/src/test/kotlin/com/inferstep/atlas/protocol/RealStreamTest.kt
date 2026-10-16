// Replays a real /v1/agent stream through the real parser.
//
// Every other test in this suite feeds the parser frames the suite wrote
// itself, which proves the parser is self-consistent and nothing about the
// proxy. This fixture is a verbatim capture of one turn against a live
// atlas-proxy, so it fails if the wire format drifts from what the client
// expects.
//
// The fixture is a copy of the VS Code client's
// extensions/vscode/test/fixtures/real-turn.sse, re-captured with:
//   curl -sN -X POST localhost:8090/v1/agent -H 'Content-Type: application/json' \
//     -d '{"message":"...","mode":"yolo","session_id":"x","working_dir":"..."}' \
//     > src/test/resources/fixtures/real-turn.sse
package com.inferstep.atlas.protocol

import kotlinx.serialization.json.JsonObject
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class RealStreamTest {
    @Test
    fun `parses end to end and ends with done`() {
        val events = parse(RAW.length)

        assertTrue(events.size > 100, "expected a long turn, got ${events.size} events")
        assertEquals("done", events.last().type)
    }

    @Test
    fun `parses identically no matter where the socket splits it`() {
        // The proxy writes ~21 KB across many TCP reads; a parser that only
        // works on whole-frame chunks passes every hand-written test and fails
        // on the wire.
        val whole = parse(RAW.length).map { it.type }
        for (size in listOf(1, 7, 64, 1024)) {
            assertEquals(whole, parse(size).map { it.type }, "chunk size $size")
        }
    }

    @Test
    fun `carries the event types this client renders`() {
        val seen = parse(4096).map { it.type }.toSet()

        for (type in listOf("turn_start", "llm_token", "tool_call", "tool_result", "done")) {
            assertTrue(seen.contains(type), "missing $type in $seen")
        }
    }

    @Test
    fun `never emits an event without a type`() {
        for (event in parse(4096)) {
            assertTrue(event.type.isNotEmpty())
        }
    }

    @Test
    fun `an event type the client does not know survives the envelope`() {
        val events = parse(4096)
        val unknown = events.filter { it.type == "pattern_context_injected" }

        assertEquals(1, unknown.size)
        // The envelope kept the event and its payload, which is all a client
        // that does not render this type needs.
        assertTrue(unknown.single().data is JsonObject)
    }

    @Test
    fun `the final done payload decodes with its status`() {
        val done = parse(4096).last().payload<DonePayload>()

        assertTrue(done.summary.isNotEmpty())
        // Whatever the capture recorded, the rule must hold: only "completed"
        // is a finished task.
        assertEquals(done.status == "completed", done.resolvedStatus().isCompleted())
    }

    @Test
    fun `tool calls carry the arguments a caller can act on`() {
        val calls =
            parse(
                4096,
            ).filter { it.type == "tool_call" }.map { it.payload<ToolCallPayload>() }

        assertTrue(calls.isNotEmpty())
        for (call in calls) {
            assertTrue(call.name.isNotEmpty())
            assertTrue(call.args is JsonObject, "${call.name} must carry an argument object")
        }
    }

    private fun parse(chunkSize: Int): List<ChatEvent> {
        val parser = SseParser()
        val bytes = RAW.toByteArray(Charsets.UTF_8)
        val events = mutableListOf<ChatEvent>()
        var offset = 0
        while (offset < bytes.size) {
            val end = minOf(offset + chunkSize, bytes.size)
            events += parser.feed(bytes.copyOfRange(offset, end))
            offset = end
        }
        events += parser.endOfStream()
        return events
    }

    private companion object {
        val RAW: String =
            checkNotNull(
                RealStreamTest::class.java.getResourceAsStream("/fixtures/real-turn.sse"),
            ) {
                "the recorded turn must be on the test classpath"
            }.use { it.readBytes().decodeToString() }
    }
}

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
    fun `parses end to end and ends with a done payload that decodes`() {
        val events = parse(RAW.length)

        assertTrue(events.size > 100, "expected a long turn, got ${events.size} events")
        assertEquals("done", events.last().type)
        val done = events.last().payload<DonePayload>()
        assertTrue(done.summary.isNotEmpty())
        // Whatever the capture recorded, the rule must hold: only "completed"
        // is a finished task.
        assertEquals(done.status == "completed", done.resolvedStatus().isCompleted())
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
    fun `carries the event types this client renders and keeps one it does not know`() {
        val events = parse(4096)
        val seen = events.map { it.type }.toSet()

        for (type in listOf("turn_start", "llm_token", "tool_call", "tool_result", "done")) {
            assertTrue(seen.contains(type), "missing $type in $seen")
        }
        // The client renders no pattern_context_injected; the envelope still
        // keeps the event and its payload, which is all such a client needs.
        val unknown = events.single { it.type == "pattern_context_injected" }
        assertTrue(unknown.data is JsonObject)
    }

    @Test
    fun `tool calls carry the arguments a caller can act on`() {
        val calls =
            parse(4096).filter { it.type == "tool_call" }.map { it.payload<ToolCallPayload>() }

        assertTrue(calls.isNotEmpty())
        for (call in calls) {
            assertTrue(call.name.isNotEmpty())
            // An absent args field decodes to an empty object, so only a
            // non-empty one shows the arguments were read.
            val args = call.args
            assertTrue(
                args is JsonObject && args.isNotEmpty(),
                "${call.name} must carry an argument object with its arguments",
            )
        }
    }

    /** Feeds the recorded turn to one parser in chunks of [chunkSize] bytes. */
    private fun parse(chunkSize: Int): List<ChatEvent> {
        val bytes = RAW.toByteArray()
        val chunks = bytes.toList().chunked(chunkSize).map { it.toByteArray() }
        val parser = SseParser()
        return chunks.flatMap { parser.feed(it) } + parser.endOfStream()
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

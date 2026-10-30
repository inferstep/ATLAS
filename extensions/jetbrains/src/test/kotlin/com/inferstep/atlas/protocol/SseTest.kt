// Unit tests for the pure SSE frame parser. Coverage follows the reference
// suite: comments, chunk splits mid-frame, [DONE], >1MB frames, malformed
// frames, CRLF, and the end-of-stream flush.
package com.inferstep.atlas.protocol

import kotlinx.serialization.json.jsonPrimitive
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import org.junit.jupiter.params.ParameterizedTest
import org.junit.jupiter.params.provider.Arguments
import org.junit.jupiter.params.provider.MethodSource

class SseTest {
    @ParameterizedTest(name = "{0}")
    @MethodSource("streams")
    fun `a stream yields the events of its well-formed frames`(
        name: String,
        chunks: List<String>,
        want: List<String>,
    ) {
        val events = collect(chunks)

        // Each event as its type and its raw data.
        assertEquals(want, events.map { "${it.type} ${it.data}" }, name)
    }

    @Test
    fun `a frame split at any byte boundary reassembles, multibyte characters included`() {
        val text = "héllo→ 日本語 — ok"
        val bytes = (frame("text", """{"content":"$text"}""") + DONE).toByteArray()

        // Chunk size 1 splits inside "data:", inside the JSON, between the two
        // newlines and inside every multibyte character.
        for (chunkSize in 1..8) {
            val chunks = bytes.toList().chunked(chunkSize).map { it.toByteArray() }
            val parser = SseParser()
            val events = chunks.flatMap { parser.feed(it) } + parser.endOfStream()

            assertEquals(
                text,
                events.single().payload<TextPayload>().content,
                "chunk size $chunkSize",
            )
        }
    }

    @Test
    fun `parses a frame larger than one megabyte`() {
        val big = "x".repeat(1_200_000)
        val events =
            collect(
                listOf(
                    frame("tool_result", """{"tool":"read_file","success":true,"data":"$big"}"""),
                    DONE,
                ),
            )

        assertEquals(1, events.size)
        assertEquals(
            1_200_000,
            events[0]
                .payload<ToolResultPayload>()
                .data.jsonPrimitive.content.length,
        )
    }

    @Test
    fun `reports done only once the sentinel arrives`() {
        val parser = SseParser()
        parser.feed(frame("text", """{"content":"x"}""").toByteArray())
        assertFalse(parser.isDone)

        parser.feed(DONE.toByteArray())
        assertTrue(parser.isDone)
    }

    /** Feeds each chunk to one parser, then ends the stream. */
    private fun collect(chunks: List<String>): List<ChatEvent> {
        val parser = SseParser()
        return chunks.flatMap { parser.feed(it.toByteArray()) } + parser.endOfStream()
    }

    companion object {
        /** The sentinel frame that ends a stream. */
        private const val DONE = "data: [DONE]\n\n"

        private fun frame(
            type: String,
            data: String,
        ): String = "data: {\"type\":\"$type\",\"data\":$data}\n\n"

        @JvmStatic
        fun streams(): List<Arguments> =
            listOf(
                Arguments.of(
                    "comments and blank lines are skipped and the sentinel ends the stream",
                    listOf(
                        ": connected\n\n: heartbeat\n\n",
                        frame("text", """{"content":"hello"}"""),
                        frame("done", """{"summary":""}"""),
                        DONE,
                    ),
                    listOf("""text {"content":"hello"}""", """done {"summary":""}"""),
                ),
                Arguments.of(
                    "a malformed JSON frame is skipped without ending the stream",
                    listOf(
                        "data: {not json}\n\n",
                        frame("text", """{"content":"ok"}"""),
                        DONE,
                    ),
                    listOf("""text {"content":"ok"}"""),
                ),
                Arguments.of(
                    "a frame with a missing or an empty type is skipped",
                    listOf(
                        """data: {"data":{"content":"no type"}}""" + "\n\n",
                        """data: {"type":"","data":{}}""" + "\n\n",
                        DONE,
                    ),
                    emptyList<String>(),
                ),
                Arguments.of(
                    "CRLF line endings",
                    listOf(
                        ": connected\r\n\r\n" +
                            "data: {\"type\":\"text\",\"data\":{\"content\":\"crlf\"}}\r\n\r\n" +
                            "data: [DONE]\r\n\r\n",
                    ),
                    listOf("""text {"content":"crlf"}"""),
                ),
                Arguments.of(
                    "a stream that ends without the sentinel keeps the events received",
                    listOf(frame("text", """{"content":"partial"}""")),
                    listOf("""text {"content":"partial"}"""),
                ),
                Arguments.of(
                    "a final data line without its newline is flushed at the end",
                    listOf("""data: {"type":"text","data":{"content":"tail"}}"""),
                    listOf("""text {"content":"tail"}"""),
                ),
                Arguments.of(
                    "anything after the sentinel is ignored",
                    listOf(DONE, frame("text", """{"content":"late"}""")),
                    emptyList<String>(),
                ),
            )
    }
}

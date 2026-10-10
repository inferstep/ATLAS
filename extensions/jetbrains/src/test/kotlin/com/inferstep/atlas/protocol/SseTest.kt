// Unit tests for the pure SSE frame parser. Coverage follows the reference
// suite: comments, chunk splits mid-frame, [DONE], >1MB frames, malformed
// frames, CRLF, and the end-of-stream flush.
package com.inferstep.atlas.protocol

import kotlinx.serialization.json.jsonPrimitive
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class SseTest {
    @Test
    fun `parses a simple stream and stops at the sentinel`() {
        val events =
            collect(
                ": connected\n\n",
                frame("text", """{"content":"hello"}"""),
                frame("done", """{"summary":""}"""),
                "data: [DONE]\n\n",
            )

        assertEquals(listOf("text", "done"), events.map { it.type })
        assertEquals("hello", events[0].payload<TextPayload>().content)
    }

    @Test
    fun `skips comments and blank lines`() {
        val events =
            collect(
                ": connected\n\n: heartbeat\n\n",
                frame("text", """{"content":"x"}"""),
                "data: [DONE]\n\n",
            )

        assertEquals(1, events.size)
    }

    @Test
    fun `reassembles a frame split across arbitrary chunk boundaries`() {
        val full = frame("tool_call", """{"name":"read_file","args":{"path":"a.py"},"turn":1}""")
        // Split mid-"data:", mid-JSON, and mid-newline.
        val events =
            collect(
                full.substring(0, 2),
                full.substring(2, 18),
                full.substring(18, 40),
                full.substring(40),
                "data: [DONE]\n\n",
            )

        assertEquals(1, events.size)
        assertEquals("read_file", events[0].payload<ToolCallPayload>().name)
    }

    @Test
    fun `handles a multibyte character split across byte chunks`() {
        val body = frame("text", """{"content":"héllo→"}""") + "data: [DONE]\n\n"
        // One byte at a time: every multi-byte sequence is split somewhere.
        val events = collectByBytes(body, chunkSize = 1)

        assertEquals(1, events.size)
        assertEquals("héllo→", events[0].payload<TextPayload>().content)
    }

    @Test
    fun `handles a multibyte character split at every byte boundary`() {
        val body = frame("text", """{"content":"日本語 — ok"}""") + "data: [DONE]\n\n"
        val expected = "日本語 — ok"
        for (chunkSize in 1..8) {
            val events = collectByBytes(body, chunkSize)
            assertEquals(
                expected,
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
                frame("tool_result", """{"tool":"read_file","success":true,"data":"$big"}"""),
                "data: [DONE]\n\n",
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
    fun `skips malformed JSON frames without killing the stream`() {
        val events =
            collect(
                "data: {not json}\n\n",
                frame("text", """{"content":"ok"}"""),
                "data: [DONE]\n\n",
            )

        assertEquals(listOf("text"), events.map { it.type })
    }

    @Test
    fun `skips frames with a missing or empty type`() {
        val events =
            collect(
                """data: {"data":{"content":"no type"}}""" + "\n\n",
                """data: {"type":"","data":{}}""" + "\n\n",
                "data: [DONE]\n\n",
            )

        assertTrue(events.isEmpty())
    }

    @Test
    fun `handles CRLF line endings`() {
        val events =
            collect(
                ": connected\r\n\r\n" +
                    "data: {\"type\":\"text\",\"data\":{\"content\":\"crlf\"}}\r\n\r\n" +
                    "data: [DONE]\r\n\r\n",
            )

        assertEquals("crlf", events.single().payload<TextPayload>().content)
    }

    @Test
    fun `yields events already received when the stream ends without the sentinel`() {
        val events = collect(frame("text", """{"content":"partial"}"""))

        assertEquals("partial", events.single().payload<TextPayload>().content)
    }

    @Test
    fun `flushes a final unterminated data line at end of stream`() {
        val events = collect("""data: {"type":"text","data":{"content":"tail"}}""")

        assertEquals("tail", events.single().payload<TextPayload>().content)
    }

    @Test
    fun `ignores anything after the sentinel`() {
        val events =
            collect(
                "data: [DONE]\n\n",
                frame("text", """{"content":"late"}"""),
            )

        assertTrue(events.isEmpty())
    }

    @Test
    fun `reports done only once the sentinel arrives`() {
        val parser = SseParser()
        parser.feed(frame("text", """{"content":"x"}""").toByteArray(Charsets.UTF_8))
        assertFalse(parser.isDone)

        parser.feed("data: [DONE]\n\n".toByteArray(Charsets.UTF_8))
        assertTrue(parser.isDone)
    }

    private fun frame(
        type: String,
        data: String,
    ): String = "data: {\"type\":\"$type\",\"data\":$data}\n\n"

    private fun collect(vararg chunks: String): List<ChatEvent> {
        val parser = SseParser()
        val events = mutableListOf<ChatEvent>()
        for (chunk in chunks) {
            events += parser.feed(chunk.toByteArray(Charsets.UTF_8))
        }
        events += parser.endOfStream()
        return events
    }

    private fun collectByBytes(
        text: String,
        chunkSize: Int,
    ): List<ChatEvent> {
        val parser = SseParser()
        val bytes = text.toByteArray(Charsets.UTF_8)
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
}

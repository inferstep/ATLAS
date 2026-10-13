// Pure SSE frame parser for the /v1/agent chat stream.
//
// A line-oriented port of extensions/vscode/src/client/sse.ts (which mirrors
// tui/chat.go's parseChatSSE): skip `:` comments (the proxy opens with
// `: connected`), take `data:` frames, stop on the `[DONE]` sentinel, and
// skip a malformed frame rather than killing the turn.
//
// Chunk boundaries are arbitrary, so the parser buffers *bytes* and only
// decodes a line once its newline has arrived. That is what makes a
// multi-byte character split across two socket reads work, and it needs no
// incremental CharsetDecoder: a `\n` byte can never occur inside a UTF-8
// multi-byte sequence, so a newline scan on raw bytes is exact.
//
// No IntelliJ and no HTTP here: the parser takes bytes, which is what lets it
// be tested with plain JUnit, without launching an IDE.
package com.inferstep.atlas.protocol

import kotlinx.serialization.SerializationException
import kotlinx.serialization.json.Json

/** The SSE terminator the proxy writes after the final event. */
const val DONE_SENTINEL = "[DONE]"

private const val DATA_PREFIX = "data:"
private const val NEWLINE = '\n'.code.toByte()
private const val INITIAL_CAPACITY = 256

/**
 * Incremental `text/event-stream` parser.
 *
 * Feed it bytes as they arrive ([feed]) and then the end of the stream
 * ([endOfStream]); each call returns the events it completed. Once the
 * `[DONE]` sentinel arrives the parser is finished and later input is
 * ignored, matching the reference parser's early return on the sentinel.
 *
 * Not thread-safe: one parser per stream, driven by one reader.
 */
class SseParser(
    private val json: Json = AtlasJson,
) {
    private var buffer = ByteArray(INITIAL_CAPACITY)
    private var size = 0
    private var finished = false
    private var ended = false

    /** True once the `[DONE]` sentinel has been seen. */
    val isDone: Boolean
        get() = finished

    /**
     * Consume one chunk of bytes and return the events it completed. A frame
     * split across chunks completes on the chunk that supplies its newline.
     */
    fun feed(chunk: ByteArray): List<ChatEvent> {
        if (finished || ended || chunk.isEmpty()) {
            return emptyList()
        }
        append(chunk)
        return drain()
    }

    /**
     * Flush the stream: parse a final frame whose newline never arrived.
     * Idempotent, and a no-op once the sentinel has been seen.
     */
    fun endOfStream(): List<ChatEvent> {
        if (finished || ended) {
            return emptyList()
        }
        ended = true
        val events = drain().toMutableList()
        if (!finished && size > 0) {
            val trailing = String(buffer, 0, size, Charsets.UTF_8)
            size = 0
            when (val line = parseLine(trailing, json)) {
                Line.Skip -> Unit
                Line.End -> finished = true
                is Line.Event -> events += line.event
            }
        }
        return events
    }

    private fun append(chunk: ByteArray) {
        ensureCapacity(size + chunk.size)
        chunk.copyInto(buffer, destinationOffset = size)
        size += chunk.size
    }

    private fun ensureCapacity(needed: Int) {
        if (needed <= buffer.size) {
            return
        }
        var capacity = maxOf(buffer.size * 2, INITIAL_CAPACITY)
        while (capacity < needed) {
            capacity *= 2
        }
        buffer = buffer.copyOf(capacity)
    }

    /** Parse every complete line currently buffered, keeping the tail. */
    private fun drain(): List<ChatEvent> {
        val events = mutableListOf<ChatEvent>()
        var lineStart = 0
        var index = 0
        while (!finished && index < size) {
            if (buffer[index] != NEWLINE) {
                index++
                continue
            }
            val line = String(buffer, lineStart, index - lineStart, Charsets.UTF_8)
            when (val parsed = parseLine(line, json)) {
                Line.Skip -> Unit
                Line.End -> finished = true
                is Line.Event -> events += parsed.event
            }
            index++
            lineStart = index
        }
        if (lineStart > 0) {
            buffer.copyInto(buffer, destinationOffset = 0, startIndex = lineStart, endIndex = size)
            size -= lineStart
        }
        return events
    }
}

/**
 * Parse a complete SSE body in one call. Convenient for tests, a fixture
 * file, or anywhere the whole stream is already in hand.
 */
fun parseSseText(
    text: String,
    json: Json = AtlasJson,
): List<ChatEvent> {
    val parser = SseParser(json)
    return parser.feed(text.toByteArray(Charsets.UTF_8)) + parser.endOfStream()
}

/** What one SSE line turned out to be. */
private sealed interface Line {
    /** A blank line, a `:` comment, or a frame that could not be read. */
    data object Skip : Line

    /** The `[DONE]` sentinel: the stream is over. */
    data object End : Line

    /** A well-formed `data:` frame. */
    data class Event(
        val event: ChatEvent,
    ) : Line
}

/**
 * Classify one raw line. Mirrors the reference parser exactly: only a `data:`
 * line carries an event, an unparseable frame is skipped rather than fatal,
 * and an event without a usable type is not an event.
 */
private fun parseLine(
    rawLine: String,
    json: Json,
): Line {
    val line = rawLine.removeSuffix("\r")
    if (line.isEmpty() || line.startsWith(":")) {
        return Line.Skip
    }
    if (!line.startsWith(DATA_PREFIX)) {
        return Line.Skip
    }
    val data = line.substring(DATA_PREFIX.length).trim()
    if (data.isEmpty()) {
        return Line.Skip
    }
    if (data == DONE_SENTINEL) {
        return Line.End
    }
    val event =
        try {
            json.decodeFromString(ChatEvent.serializer(), data)
        } catch (_: SerializationException) {
            return Line.Skip
        } catch (_: IllegalArgumentException) {
            return Line.Skip
        }
    if (event.type.isEmpty()) {
        return Line.Skip
    }
    return Line.Event(event)
}

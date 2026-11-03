// Pure SSE frame parser for the /v1/agent chat stream. Mirrors
// clients/tui/internal/proxyclient/chat.go's parseChatSSE: line-oriented, skips `:` comments (the
// proxy sends `: connected` on open), takes `data:` frames, stops on
// the `[DONE]` sentinel, and skips malformed frames rather than
// killing the turn. No vscode imports — unit-testable under vitest.

import type { ChatEvent } from './types';

/** The SSE terminator the proxy writes after the final event. */
export const DONE_SENTINEL = '[DONE]';

/**
 * Parse a byte stream of SSE lines into ChatEvents.
 *
 * Yields each well-formed `data: {"type":...,"data":...}` frame and
 * returns when the `[DONE]` sentinel arrives or the stream ends.
 * Chunk boundaries are arbitrary — a frame may be split anywhere,
 * including mid-multibyte-character (the streaming TextDecoder holds
 * partial sequences across chunks).
 */
export async function* parseSSEStream(
	stream: AsyncIterable<Uint8Array>,
): AsyncGenerator<ChatEvent, void, undefined> {
	const decoder = new TextDecoder('utf-8');
	let buffer = '';

	for await (const chunk of stream) {
		buffer += decoder.decode(chunk, { stream: true });

		// Consume complete lines; keep the trailing partial in the buffer.
		let newlineIndex: number;
		while ((newlineIndex = buffer.indexOf('\n')) !== -1) {
			const line = buffer.slice(0, newlineIndex);
			buffer = buffer.slice(newlineIndex + 1);
			const event = parseSSELine(line);
			if (event === DONE) {
				return;
			}
			if (event) {
				yield event;
			}
		}
	}

	// Stream ended without [DONE]; flush a final unterminated line if any.
	buffer += decoder.decode();
	if (buffer.length > 0) {
		const event = parseSSELine(buffer);
		if (event && event !== DONE) {
			yield event;
		}
	}
}

/** Internal marker distinguishing the [DONE] sentinel from a skipped line. */
const DONE = Symbol('done');

function parseSSELine(rawLine: string): ChatEvent | typeof DONE | null {
	const line = rawLine.endsWith('\r') ? rawLine.slice(0, -1) : rawLine;
	// Blank separators and `:` comments (": connected", ": heartbeat").
	if (line === '' || line.startsWith(':')) {
		return null;
	}
	if (!line.startsWith('data:')) {
		return null;
	}
	const data = line.slice('data:'.length).trim();
	if (data === '') {
		return null;
	}
	if (data === DONE_SENTINEL) {
		return DONE;
	}
	let event: ChatEvent;
	try {
		event = JSON.parse(data) as ChatEvent;
	} catch {
		return null; // skip malformed frame, don't kill the turn
	}
	if (!event || typeof event.type !== 'string' || event.type === '') {
		return null;
	}
	return event;
}

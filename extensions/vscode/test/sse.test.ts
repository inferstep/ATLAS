// Unit tests for the pure SSE frame parser: comments, chunk splits mid-frame
// and mid-character, [DONE], >1MB frames, malformed frames, CRLF.

import { describe, expect, it } from 'vitest';
import { parseSSEStream } from '../src/client/sse';
import type { ChatEvent } from '../src/client/types';

/** Parse a byte stream that arrives in the given chunks; strings are sent as UTF-8. */
async function parse(chunks: (string | Uint8Array)[]): Promise<ChatEvent[]> {
	async function* stream(): AsyncGenerator<Uint8Array> {
		for (const chunk of chunks) {
			yield typeof chunk === 'string' ? new TextEncoder().encode(chunk) : chunk;
		}
	}
	const events: ChatEvent[] = [];
	for await (const event of parseSSEStream(stream())) {
		events.push(event);
	}
	return events;
}

function frame(event: ChatEvent): string {
	return `data: ${JSON.stringify(event)}\n\n`;
}

const text = (content: string): ChatEvent => ({ type: 'text', data: { content } });
const done: ChatEvent = { type: 'done', data: { summary: '' } };

describe('parseSSEStream', () => {
	it.each([
		[
			'parses a simple stream and stops at [DONE]',
			[': connected\n\n', frame(text('hello')), frame(done), 'data: [DONE]\n\n'],
			[text('hello'), done],
		],
		[
			'skips comments and blank lines',
			[': connected\n\n: heartbeat\n\n', frame(text('x')), 'data: [DONE]\n\n'],
			[text('x')],
		],
		[
			'reassembles a frame split mid-"data:", mid-JSON and mid-newline',
			['da', 'ta: {"type":"tool_call","da', 'ta":{"name":"read_file","args":{"path":"a.py"},"turn":1}}\n', '\ndata: [DONE]\n\n'],
			[{ type: 'tool_call', data: { name: 'read_file', args: { path: 'a.py' }, turn: 1 } }],
		],
		[
			'skips malformed JSON frames without killing the stream',
			['data: {not json}\n\n', frame(text('ok')), 'data: [DONE]\n\n'],
			[text('ok')],
		],
		[
			'skips frames with a missing or empty type',
			['data: {"data":{"content":"no type"}}\n\n', 'data: {"type":"","data":{}}\n\n', 'data: [DONE]\n\n'],
			[],
		],
		[
			'handles CRLF line endings',
			[': connected\r\n\r\ndata: {"type":"text","data":{"content":"crlf"}}\r\n\r\ndata: [DONE]\r\n\r\n'],
			[text('crlf')],
		],
		[
			'yields events already received when the stream ends without [DONE]',
			[frame(text('partial'))],
			[text('partial')],
		],
		[
			'flushes a final unterminated data line at end of stream',
			['data: {"type":"text","data":{"content":"tail"}}'],
			[text('tail')],
		],
		[
			'ignores events after [DONE]',
			['data: [DONE]\n\n', frame(text('late'))],
			[],
		],
	])('%s', async (_name, chunks, events) => {
		expect(await parse(chunks)).toEqual(events);
	});

	it('handles a multibyte character split across chunks', async () => {
		const bytes = new TextEncoder().encode(frame(text('héllo→')) + 'data: [DONE]\n\n');
		// The é is the two bytes C3 A9: cut between them.
		const insideTheE = bytes.indexOf(0xc3) + 1;
		const events = await parse([bytes.slice(0, insideTheE), bytes.slice(insideTheE)]);
		expect(events).toEqual([text('héllo→')]);
	});

	it('parses a >1MB frame', async () => {
		const big = 'x'.repeat(1_200_000);
		const events = await parse([frame({ type: 'tool_result', data: { tool: 'read_file', success: true, data: big } }), 'data: [DONE]\n\n']);
		expect(events).toHaveLength(1);
		expect((events[0].data as { data: string }).data).toHaveLength(1_200_000);
	});
});

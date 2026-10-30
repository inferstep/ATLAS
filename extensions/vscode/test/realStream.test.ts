// Replays a real /v1/agent stream through the real parser.
//
// Every other test in this suite feeds the parser frames the suite wrote
// itself, which proves the parser is self-consistent and nothing about the
// proxy. This fixture is a verbatim capture of one turn against a live
// atlas-proxy (gemma-4-12b, yolo mode, a write_file plus verification), so
// it fails if the wire format drifts from what the client expects.
//
// Re-capture with:
//   curl -sN -X POST localhost:8090/v1/agent -H 'Content-Type: application/json' \
//     -d '{"message":"...","mode":"yolo","session_id":"x","working_dir":"..."}' \
//     > test/fixtures/real-turn.sse

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { parseSSEStream } from '../src/client/sse';
import type { ChatEvent } from '../src/client/types';
import { FILE_EDIT_TOOLS, editTargetPath, predictEdit } from '../src/session/editPreview';

const RAW = readFileSync(join(__dirname, 'fixtures', 'real-turn.sse'), 'utf8');

async function* bytes(text: string, size: number): AsyncGenerator<Uint8Array> {
	const enc = new TextEncoder();
	for (let i = 0; i < text.length; i += size) {
		yield enc.encode(text.slice(i, i + size));
	}
}

async function parse(chunkSize: number): Promise<ChatEvent[]> {
	const out: ChatEvent[] = [];
	for await (const e of parseSSEStream(bytes(RAW, chunkSize))) {
		out.push(e);
	}
	return out;
}

describe('a real proxy turn', () => {
	it('parses end to end and ends with done', async () => {
		const events = await parse(RAW.length);
		expect(events.length).toBeGreaterThan(100);
		expect(events.at(-1)!.type).toBe('done');
	});

	it('parses identically no matter where the socket splits it', async () => {
		// The proxy writes ~21 KB across many TCP reads; a parser that only
		// works on whole-frame chunks passes every hand-written test and
		// fails on the wire.
		const whole = await parse(RAW.length);
		for (const size of [1, 7, 64, 1024]) {
			const split = await parse(size);
			expect(split.map((e) => e.type)).toEqual(whole.map((e) => e.type));
		}
	});

	it('carries the event types this client renders', async () => {
		const seen = new Set((await parse(4096)).map((e) => e.type));
		for (const t of ['turn_start', 'llm_token', 'tool_call', 'tool_result', 'done']) {
			expect(seen).toContain(t);
		}
	});

	it('yields a tool_call the edit preview can act on', async () => {
		const events = await parse(4096);
		const calls = events
			.filter((e) => e.type === 'tool_call')
			.map((e) => e.data as { name: string; args: unknown });
		const edits = calls.filter((c) => FILE_EDIT_TOOLS.has(c.name));
		expect(edits.length).toBeGreaterThan(0);

		for (const call of edits) {
			// The bug this whole exercise found: a tool name the client does
			// not recognise silently yields no path and no preview.
			expect(editTargetPath(call.name, call.args)).toBeTruthy();
			expect(predictEdit(call.name, call.args, 'existing\n')).toBeDefined();
		}
	});
});

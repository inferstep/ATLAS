// Unit tests for TurnManager: session_id minting, request shape, rolling
// history (40-cap, assistant envelope re-wrap), session_allowed_tools
// re-send, busy guard, and cancel.

import { describe, expect, it } from 'vitest';
import type { AgentRequest, ChatEvent } from '../src/client/types';
import { HISTORY_LIMIT, mintSessionId, TurnManager, type TurnClient } from '../src/session/turnManager';

/** Fake client that records requests and replays canned events. */
function fakeClient(events: ChatEvent[] = []): TurnClient & { requests: AgentRequest[]; cancelled: string[] } {
	return {
		requests: [],
		cancelled: [],
		async *sendAgentTurn(request: AgentRequest): AsyncGenerator<ChatEvent, void, undefined> {
			this.requests.push(request);
			for (const event of events) {
				yield event;
			}
		},
		async cancelTurn(sessionId: string): Promise<boolean> {
			this.cancelled.push(sessionId);
			return true;
		},
	};
}

async function drain(turn: AsyncGenerator<ChatEvent, void, undefined>): Promise<ChatEvent[]> {
	const out: ChatEvent[] = [];
	for await (const event of turn) {
		out.push(event);
	}
	return out;
}

const text = (content: string): ChatEvent => ({ type: 'text', data: { content } });
const done: ChatEvent = { type: 'done', data: { summary: '' } };

/** Rejects with an AbortError when the signal aborts — including when it
 * already aborted before this was awaited (listener-only fakes hang there). */
function abortableHang(signal?: AbortSignal): Promise<never> {
	return new Promise<never>((_, reject) => {
		const fail = () => {
			const error = new Error('aborted');
			error.name = 'AbortError';
			reject(error);
		};
		if (signal?.aborted) {
			fail();
			return;
		}
		signal?.addEventListener('abort', fail);
	});
}

describe('mintSessionId', () => {
	it('mints 24-char hex ids, unique per call', () => {
		const a = mintSessionId();
		const b = mintSessionId();
		expect(a).toMatch(/^[0-9a-f]{24}$/);
		expect(b).toMatch(/^[0-9a-f]{24}$/);
		expect(a).not.toBe(b);
	});
});

describe('TurnManager', () => {
	it('sends the conventional request shape and yields all events', async () => {
		const client = fakeClient([text('hi'), done]);
		const turns = new TurnManager();

		const events = await drain(turns.runTurn(client, 'hello', 'default', 'work', '/tmp/ws'));

		expect(events).toHaveLength(2);
		const request = client.requests[0];
		expect(request.message).toBe('hello');
		// The caller's workspace root goes on the wire verbatim — the proxy
		// uses it for host-to-container path translation (TUI sends its cwd).
		expect(request.working_dir).toBe('/tmp/ws');
		expect(request.mode).toBe('default');
		expect(request.session_id).toMatch(/^[0-9a-f]{24}$/);
		expect(request.history).toEqual([]);
		expect(request.session_allowed_tools).toBeUndefined();
		// Every owned sender declares a task mode; absence is reserved for
		// external and legacy callers (docs/API.md, "Building a non-TUI
		// client"). Without one the proxy gets no V3 candidate at all.
		// Strict: no obligations the user never stated. A chat box knows
		// nothing structured about paths or commands.
		expect(request.task_contract).toStrictEqual({ task_mode: 'work' });
		expect(turns.sessionId).toBe(request.session_id);
	});

	it('declares question when the user selects it, for the same sentence', async () => {
		const client = fakeClient([done]);
		const turns = new TurnManager();

		// The message is identical: the declaration tracks the control the
		// user operated, never the words they typed (tui/chat_test.go).
		const same = 'what does solve.py do?';
		await drain(turns.runTurn(client, same, 'default', 'work', '/tmp/ws'));
		await drain(turns.runTurn(client, same, 'default', 'question', '/tmp/ws'));

		expect(client.requests[0].task_contract).toEqual({ task_mode: 'work' });
		expect(client.requests[1].task_contract).toEqual({ task_mode: 'question' });
	});

	it('mints a fresh session_id per turn', async () => {
		const client = fakeClient([done]);
		const turns = new TurnManager();
		await drain(turns.runTurn(client, 'one', 'default', 'work', '/tmp/ws'));
		await drain(turns.runTurn(client, 'two', 'default', 'work', '/tmp/ws'));
		expect(client.requests[0].session_id).not.toBe(client.requests[1].session_id);
	});

	it.each([
		['replays history with assistant text re-wrapped in the {"type":"text"} envelope',
			[text('Hello '), text('world'), done], 'Hello world'],
		// Tool-shaped turn: no text events, the answer arrives only in done.summary.
		['uses done.summary as the assistant history entry on a tool-shaped turn', [
			{ type: 'tool_call', data: { name: 'edit_file', args: {}, turn: 1 } },
			{ type: 'tool_result', data: { tool: 'edit_file', success: true, data: {} } },
			{ type: 'done', data: { summary: 'Fixed the off-by-one in pagination.' } },
		], 'Fixed the off-by-one in pagination.'],
		['prefers streamed text over done.summary when both are present',
			[text('the streamed answer'), { type: 'done', data: { summary: 'a trailing summary' } }], 'the streamed answer'],
	])('%s', async (_name, events, reply) => {
		const client = fakeClient(events);
		const turns = new TurnManager();

		await drain(turns.runTurn(client, 'first', 'default', 'work', '/tmp/ws'));
		await drain(turns.runTurn(client, 'next', 'default', 'work', '/tmp/ws'));

		expect(client.requests[1].history).toEqual([
			{ role: 'user', content: 'first' },
			{ role: 'assistant', content: JSON.stringify({ type: 'text', content: reply }) },
		]);
	});

	it('records no assistant entry for a text-free turn', async () => {
		const client = fakeClient([{ type: 'error', data: { error: 'boom' } }]);
		const turns = new TurnManager();

		await drain(turns.runTurn(client, 'do it', 'default', 'work', '/tmp/ws'));
		await drain(turns.runTurn(client, 'retry', 'default', 'work', '/tmp/ws'));

		// History sent on turn N covers turns < N only; turn 1 produced no text.
		expect(client.requests[1].history).toEqual([{ role: 'user', content: 'do it' }]);
	});

	it(`caps history at the most recent ${HISTORY_LIMIT} entries`, async () => {
		const client = fakeClient([text('r'), done]);
		const turns = new TurnManager();

		// 25 turns × 2 entries (user + assistant) = 50 recorded > 40 cap.
		for (let i = 0; i < 25; i++) {
			await drain(turns.runTurn(client, `msg ${i}`, 'default', 'work', '/tmp/ws'));
		}
		await drain(turns.runTurn(client, 'final', 'default', 'work', '/tmp/ws'));

		const history = client.requests.at(-1)!.history!;
		expect(history).toHaveLength(HISTORY_LIMIT);
		// Oldest surviving entries come from the tail of the run, not turn 0.
		expect(history[0].content).not.toContain('msg 0');
		// Final pair is turn 24's user message + its re-wrapped assistant reply.
		expect(history.at(-2)).toEqual({ role: 'user', content: 'msg 24' });
		expect(history.at(-1)).toEqual({
			role: 'assistant',
			content: JSON.stringify({ type: 'text', content: 'r' }),
		});
	});

	it('re-sends sessionAllowedTools on every turn once populated', async () => {
		const client = fakeClient([done]);
		const turns = new TurnManager();
		turns.sessionAllowedTools.add('edit_file');
		turns.sessionAllowedTools.add('write_file');

		await drain(turns.runTurn(client, 'one', 'default', 'work', '/tmp/ws'));
		await drain(turns.runTurn(client, 'two', 'default', 'work', '/tmp/ws'));

		expect(client.requests[0].session_allowed_tools).toEqual(['edit_file', 'write_file']);
		expect(client.requests[1].session_allowed_tools).toEqual(['edit_file', 'write_file']);
	});

	it('rejects a second concurrent turn', async () => {
		const client = fakeClient();
		let release!: () => void;
		const gate = new Promise<void>((resolve) => (release = resolve));
		client.sendAgentTurn = async function* (request: AgentRequest) {
			this.requests.push(request);
			await gate;
			yield done;
		};
		const turns = new TurnManager();

		const first = drain(turns.runTurn(client, 'slow', 'default', 'work', '/tmp/ws'));
		// Give the first generator a tick to start the stream.
		await new Promise((resolve) => setImmediate(resolve));
		expect(turns.busy).toBe(true);
		await expect(drain(turns.runTurn(client, 'second', 'default', 'work', '/tmp/ws'))).rejects.toThrow(/already in progress/);

		release();
		await first;
		expect(turns.busy).toBe(false);
	});

	it('cancel aborts the stream and fires best-effort POST /cancel', async () => {
		const client = fakeClient();
		client.sendAgentTurn = async function* (request: AgentRequest, signal?: AbortSignal) {
			this.requests.push(request);
			yield text('partial');
			await abortableHang(signal);
		};
		const turns = new TurnManager();

		const consumed = (async () => {
			const seen: ChatEvent[] = [];
			try {
				for await (const event of turns.runTurn(client, 'go', 'default', 'work', '/tmp/ws')) {
					seen.push(event);
					turns.cancel();
				}
				throw new Error('stream should have aborted');
			} catch (error) {
				expect((error as Error).name).toBe('AbortError');
			}
			return seen;
		})();

		const seen = await consumed;
		expect(seen).toHaveLength(1);
		expect(client.cancelled).toEqual([client.requests[0].session_id]);
		expect(turns.busy).toBe(false);
	});

	it('keeps partial assistant text in history after a cancelled turn', async () => {
		const client = fakeClient();
		client.sendAgentTurn = async function* (request: AgentRequest, signal?: AbortSignal) {
			this.requests.push(request);
			yield text('half an ans');
			await abortableHang(signal);
		};
		const turns = new TurnManager();

		try {
			for await (const _event of turns.runTurn(client, 'go', 'default', 'work', '/tmp/ws')) {
				turns.cancel();
			}
		} catch {
			// expected abort
		}

		// Next turn replays the partial text.
		client.sendAgentTurn = async function* (request: AgentRequest) {
			this.requests.push(request);
			yield done;
		};
		await drain(turns.runTurn(client, 'next', 'default', 'work', '/tmp/ws'));
		expect(client.requests[1].history).toEqual([
			{ role: 'user', content: 'go' },
			{ role: 'assistant', content: JSON.stringify({ type: 'text', content: 'half an ans' }) },
		]);
	});

	it('cancel when idle is a no-op', async () => {
		const client = fakeClient([done]);
		const turns = new TurnManager();
		// The finished turn's client is the one a cancel would reach.
		await drain(turns.runTurn(client, 'one', 'default', 'work', '/tmp/ws'));

		turns.cancel();
		expect(client.cancelled).toEqual([]);
	});

	it('reset clears history and session-approved tools', async () => {
		const client = fakeClient([text('a'), done]);
		const turns = new TurnManager();
		turns.sessionAllowedTools.add('edit_file');
		await drain(turns.runTurn(client, 'one', 'default', 'work', '/tmp/ws'));

		turns.reset();
		await drain(turns.runTurn(client, 'two', 'default', 'work', '/tmp/ws'));

		const request = client.requests[1];
		expect(request.history).toEqual([]);
		expect(request.session_allowed_tools).toBeUndefined();
	});
});

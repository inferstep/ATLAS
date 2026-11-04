// The token that the chat view's client sends to the proxy: the
// atlas.serviceToken setting when it is set, else the token in SecretStorage,
// else none. The setting is a plaintext override for development.

import { describe, expect, it, vi } from 'vitest';

// The settings that the stand-in for VS Code gives; each test sets them.
const settings = vi.hoisted(() => ({ values: {} as Record<string, string | undefined> }));
// The lines the view writes to its output channel.
const logged = vi.hoisted(() => ({ lines: [] as string[] }));

vi.mock('vscode', () => ({
	window: { createOutputChannel: () => ({ appendLine: (line: string) => logged.lines.push(line) }) },
	workspace: { getConfiguration: () => ({ get: (key: string, fallback: unknown) => settings.values[key] ?? fallback }) },
}));

import type { Memento, SecretStorage, Uri } from 'vscode';
import { doneOutcome } from '../src/session/doneOutcome';
import { predictEdit } from '../src/session/editPreview';
import { ChatViewProvider, TOKEN_SECRET_KEY } from '../src/ui/chatView';
import { DiffProvider } from '../src/ui/diffProvider';
import { MockProxy } from './fixtures/mockProxy';

describe('the token of the chat view\'s client', () => {
	it.each([
		['is the setting when a setting and a secret are set', 'token-in-setting', 'token-in-secret', 'Bearer token-in-setting'],
		['is the secret when only a secret is set', undefined, 'token-in-secret', 'Bearer token-in-secret'],
		['is none when neither is set', undefined, undefined, undefined],
	])('%s', async (_case, setting, secret, authorization) => {
		const proxy = new MockProxy();
		await proxy.start();
		settings.values = { proxyUrl: proxy.url, serviceToken: setting };
		const secrets = { get: (key: string) => Promise.resolve(key === TOKEN_SECRET_KEY ? secret : undefined) };
		const workspaceState = { get: (_key: string, fallback: unknown) => fallback };
		// makeClient does not use the extension's folder.
		const chat = new ChatViewProvider(
			undefined as unknown as Uri,
			secrets as unknown as SecretStorage,
			new DiffProvider(),
			workspaceState as unknown as Memento,
		);

		const client = await chat.makeClient();
		await client.getReady();
		await proxy.stop();

		expect(proxy.requests[0].headers.authorization).toBe(authorization);
	});
});

// What the chat view makes of each event of a turn's stream. dispatch and the
// parts it hands work to are private, so the tests reach them through this type.
interface ViewInternals {
	view: { webview: { postMessage(message: unknown): Promise<boolean> } };
	dispatch(type: string, data: unknown, client: unknown): Promise<void>;
	readLocalFile(target: string): Promise<string | undefined>;
	mismatch: {
		recordToolCall(tool: string, args: unknown): Promise<void>;
		recordToolResult(tool: string, success: boolean): Promise<void>;
	};
	permissions: {
		handleRequest(client: unknown, sessionId: string, payload: unknown): void;
		handleDenied(tool: string): void;
	};
	promptPrediction: unknown;
}

/** A chat view whose webview is a list: each message the view posts lands in `posted`. */
function chatWithPosts(): { chat: ViewInternals; posted: unknown[] } {
	const workspaceState = { get: (_key: string, fallback: unknown) => fallback };
	const chat = new ChatViewProvider(
		undefined as unknown as Uri,
		undefined as unknown as SecretStorage,
		new DiffProvider(),
		workspaceState as unknown as Memento,
	) as unknown as ViewInternals;
	const posted: unknown[] = [];
	chat.view = {
		webview: {
			postMessage: (message) => {
				posted.push(message);
				return Promise.resolve(true);
			},
		},
	};
	return { chat, posted };
}

const V3_STAGES = ['v3_phase', 'v3_sandbox', 'v3_repair', 'v3_plansearch', 'v3_divsampling', 'v3_select', 'v3_probe',
	'v3_self_test', 'v3_plan'];
// Streams the panel drops without a word: they would only churn the DOM.
const DROPPED = ['turn_start', 'llm_token', 'v3_token', 'v3_llm_start', 'v3_llm_end', 'v3_reasoning_token', 'agent_lens_score'];

/** An event's type and data, and the messages the view posts for it. */
type EventRow = [type: string, data: unknown, messages: unknown[]];

describe('what the chat view posts for each event of the stream', () => {
	it.each<EventRow>([
		['text', { content: 'hi' }, [{ type: 'assistantDelta', text: 'hi' }]],
		['text', {}, []],
		['reasoning_token', { text: 'r' }, [{ type: 'reasoningDelta', text: 'r' }]],
		['plan_loaded', { steps: [{ id: '1', action: 'edit', target: 'a.py' }], revision: 2 },
			[{ type: 'planLoaded', steps: [{ id: '1', label: 'edit a.py' }], revision: 2 }]],
		['plan_loaded', { steps: [] }, [{ type: 'planLoaded', steps: [], revision: 0 }]],
		['plan_loaded', {}, []],
		['plan_adherence', { matched: true, step_id: '1' }, [{ type: 'planStep', stepId: '1' }]],
		['plan_adherence', { matched: false, step_id: '1' }, []],
		['plan_revise', { reason: 'drift' }, [{ type: 'note', text: 'Plan going off track — revising (drift).' }]],
		['plan_revise', {}, [{ type: 'note', text: 'Plan going off track — revising (off-plan streak).' }]],
		['llm_call_start', {}, [{ type: 'progress', text: 'Thinking…' }]],
		['llm_prompt_progress', { pct: 0.5 }, [{ type: 'progress', text: 'Processing prompt 50%…' }]],
		['llm_prompt_progress', { pct: 0 }, [{ type: 'progress', text: 'Processing prompt…' }]],
		['llm_first_token', {}, [{ type: 'progress', text: '' }]],
		['llm_call_end', {}, [{ type: 'progress', text: '' }]],
		['v3_progress', { message: 'probing' }, [{ type: 'progress', text: 'V3: probing' }]],
		['v3_progress', {}, []],
		...V3_STAGES.map((type): EventRow => [type, { stage: 'plan', detail: '3 of 5' }, [{ type: 'progress', text: 'V3: plan — 3 of 5' }]]),
		['v3_plan', { stage: 'plan' }, [{ type: 'progress', text: 'V3: plan' }]],
		['v3_plan', {}, []],
		['v3_lens_veto', { detail: 'low score' }, [{ type: 'badge', text: 'Candidate rejected (lens quality veto): low score' }]],
		['v3_structural_veto', {}, [{ type: 'badge', text: 'Candidate rejected (unresolved-call veto)' }]],
		['agent_lens_intervention', { reason: 'loop' }, [{ type: 'badge', text: 'Lens intervention: loop' }]],
		['agent_lens_intervention', {}, [{ type: 'badge', text: 'Lens intervention: corrective queued' }]],
		['error', { error: 'boom' }, [{ type: 'turnError', message: 'boom' }]],
		['error', {}, [{ type: 'turnError', message: 'unknown stream error' }]],
		['done', { status: 'completed' }, [{ type: 'doneSummary', ...doneOutcome({ status: 'completed' }) }]],
		...DROPPED.map((type): EventRow => [type, { token: 'x' }, []]),
	])('%s %j', async (type, data, messages) => {
		const { chat, posted } = chatWithPosts();
		logged.lines = [];
		await chat.dispatch(type, data, undefined);
		expect(posted).toEqual(messages);
		expect(logged.lines).toEqual([]);
	});

	it('logs an event type it does not know, and posts nothing', async () => {
		const { chat, posted } = chatWithPosts();
		logged.lines = [];
		await chat.dispatch('future_event', { x: 1 }, undefined);
		expect(posted).toEqual([]);
		expect(logged.lines).toHaveLength(1);
		expect(logged.lines[0]).toContain("unhandled event 'future_event'");
	});

	it('snapshots an edit target at its tool call, and its result offers the applied diff', async () => {
		const { chat, posted } = chatWithPosts();
		const files: Record<string, string> = { 'a.py': 'before' };
		chat.readLocalFile = async (target) => files[target];
		const recorded: unknown[] = [];
		chat.mismatch = {
			recordToolCall: async (tool, args) => { recorded.push(['call', tool, args]); },
			recordToolResult: async (tool, success) => { recorded.push(['result', tool, success]); },
		};
		const args = { path: 'a.py', content: 'after' };

		await chat.dispatch('tool_call', { name: 'write_file', args }, undefined);
		files['a.py'] = 'after';
		await chat.dispatch('tool_result', { tool: 'write_file', success: true, elapsed: '1ms', data: { summary: 'wrote a.py' } }, undefined);

		expect(posted).toEqual([
			{ type: 'toolCall', name: 'write_file', detail: JSON.stringify(args) },
			{ type: 'toolResult', tool: 'write_file', success: true, elapsed: '1ms', error: undefined, diffId: 1, summary: 'wrote a.py' },
		]);
		expect(recorded).toEqual([['call', 'write_file', args], ['result', 'write_file', true]]);
	});

	it('a failed tool result offers no diff and no summary', async () => {
		const { chat, posted } = chatWithPosts();
		chat.readLocalFile = async () => 'before';
		chat.mismatch = { recordToolCall: async () => {}, recordToolResult: async () => {} };

		await chat.dispatch('tool_call', { name: 'write_file', args: { path: 'a.py', content: 'x' } }, undefined);
		const failed = { tool: 'write_file', success: false, error: 'refused', data: { summary: 'half written' } };
		await chat.dispatch('tool_result', failed, undefined);

		expect(posted[1]).toEqual({ type: 'toolResult', tool: 'write_file', success: false, error: 'refused' });
	});

	it('a permission prompt sees the predicted edit, which is cleared after', async () => {
		const { chat } = chatWithPosts();
		chat.readLocalFile = async () => 'old\n';
		const seen: unknown[] = [];
		chat.permissions = {
			handleRequest: (client, sessionId, payload) => { seen.push([client, typeof sessionId, payload, chat.promptPrediction]); },
			handleDenied: () => {},
		};
		const payload = { tool_name: 'write_file', args: { path: 'a.py', content: 'new\n' } };

		await chat.dispatch('permission_request', payload, 'the client');

		expect(seen).toEqual([['the client', 'string', payload, predictEdit('write_file', payload.args, 'old\n')]]);
		expect(chat.promptPrediction).toBeUndefined();
	});

	it('a permission the proxy denied dismisses the prompt and posts a note', async () => {
		const { chat, posted } = chatWithPosts();
		const denied: string[] = [];
		chat.permissions = { handleRequest: () => {}, handleDenied: (tool) => { denied.push(tool); } };

		await chat.dispatch('permission_denied', { tool: 'run_command' }, undefined);

		expect(denied).toEqual(['run_command']);
		expect(posted).toEqual([{ type: 'note', text: "Permission denied for 'run_command'." }]);
	});
});

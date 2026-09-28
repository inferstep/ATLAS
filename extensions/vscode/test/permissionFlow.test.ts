// Unit tests for PermissionFlow: auto-allow for session-approved tools,
// prompt/settle round trips (once/session/deny), first-answer-wins,
// remote permission_denied dismissal, turn-end cleanup, and advisory
// POST error reporting.

import { describe, expect, it } from 'vitest';
import type { PermissionDecisionRequest, PermissionRequestEventData } from '../src/client/types';
import {
	PermissionFlow,
	commandOf,
	type DismissReason,
	type PendingPermission,
	type PermissionPoster,
	type PermissionUi,
} from '../src/session/permissionFlow';

function request(tool: string, id = `call-${tool}`): PermissionRequestEventData {
	return { tool_name: tool, args: { path: 'a.py' }, message: `run ${tool}?`, tool_call_id: id };
}

function fakePoster(failWith?: Error): PermissionPoster & { decisions: PermissionDecisionRequest[] } {
	return {
		decisions: [],
		async postPermissionDecision(decision: PermissionDecisionRequest): Promise<void> {
			this.decisions.push(decision);
			if (failWith) {
				throw failWith;
			}
		},
	};
}

interface UiEvent {
	kind: 'prompt' | 'dismiss' | 'auto-allow' | 'post-error';
	pending?: PendingPermission;
	reason?: DismissReason;
	toolName?: string;
	error?: unknown;
}

function fakeUi(): PermissionUi & { events: UiEvent[] } {
	return {
		events: [],
		onPrompt(pending) {
			this.events.push({ kind: 'prompt', pending });
		},
		onDismiss(pending, reason) {
			this.events.push({ kind: 'dismiss', pending, reason });
		},
		onAutoAllow(toolName) {
			this.events.push({ kind: 'auto-allow', toolName });
		},
		onPostError(toolName, error) {
			this.events.push({ kind: 'post-error', toolName, error });
		},
	};
}

/** Let queued microtasks (fire-and-forget POST .catch chains) run. */
const settle = () => new Promise((resolve) => setImmediate(resolve));

describe('PermissionFlow', () => {
	it('auto-answers allow/once for a session-allowed tool, no prompt', async () => {
		const poster = fakePoster();
		const ui = fakeUi();
		const allowed = new Set(['edit_file']);
		const flow = new PermissionFlow(allowed, ui);

		flow.handleRequest(poster, 'sess-1', request('edit_file'));
		await settle();

		// TUI convention: auto-allow posts scope "once", not "session".
		expect(poster.decisions).toEqual([
			{ session_id: 'sess-1', tool_call_id: 'call-edit_file', decision: 'allow', scope: 'once' },
		]);
		expect(ui.events).toEqual([{ kind: 'auto-allow', toolName: 'edit_file' }]);
	});

	it('prompts for an unlisted tool and posts allow/once on allow-once', async () => {
		const poster = fakePoster();
		const ui = fakeUi();
		const allowed = new Set<string>();
		const flow = new PermissionFlow(allowed, ui);

		flow.handleRequest(poster, 'sess-1', request('write_file'));
		expect(ui.events).toHaveLength(1);
		expect(ui.events[0].kind).toBe('prompt');
		const pending = ui.events[0].pending!;
		expect(pending.request.tool_name).toBe('write_file');

		expect(pending.settle('allow-once')).toBe(true);
		await settle();

		expect(poster.decisions).toEqual([
			{ session_id: 'sess-1', tool_call_id: 'call-write_file', decision: 'allow', scope: 'once' },
		]);
		// Allow-once must NOT touch the session allowlist.
		expect(allowed.size).toBe(0);
		expect(ui.events.at(-1)).toMatchObject({ kind: 'dismiss', reason: 'answered' });
		expect(pending.choice).toBe('allow-once');
	});

	it('allow-session adds to the allowlist and posts scope "session"', async () => {
		const poster = fakePoster();
		const ui = fakeUi();
		const allowed = new Set<string>();
		const flow = new PermissionFlow(allowed, ui);

		flow.handleRequest(poster, 'sess-2', request('structural_edit'));
		ui.events[0].pending!.settle('allow-session');
		await settle();

		expect(allowed.has('structural_edit')).toBe(true);
		expect(poster.decisions).toEqual([
			{ session_id: 'sess-2', tool_call_id: 'call-structural_edit', decision: 'allow', scope: 'session' },
		]);
	});

	it('deny posts deny/once and does not touch the allowlist', async () => {
		const poster = fakePoster();
		const ui = fakeUi();
		const allowed = new Set<string>();
		const flow = new PermissionFlow(allowed, ui);

		flow.handleRequest(poster, 'sess-3', request('run_command'));
		ui.events[0].pending!.settle('deny');
		await settle();

		expect(poster.decisions).toEqual([
			{ session_id: 'sess-3', tool_call_id: 'call-run_command', decision: 'deny', scope: 'once' },
		]);
		expect(allowed.size).toBe(0);
	});

	it('first answer wins — the second settle is a no-op', async () => {
		const poster = fakePoster();
		const ui = fakeUi();
		const flow = new PermissionFlow(new Set(), ui);

		flow.handleRequest(poster, 'sess-4', request('write_file'));
		const pending = ui.events[0].pending!;

		expect(pending.settle('allow-once')).toBe(true);
		expect(pending.settle('deny')).toBe(false);
		await settle();

		expect(poster.decisions).toHaveLength(1);
		expect(poster.decisions[0].decision).toBe('allow');
		expect(pending.choice).toBe('allow-once');
	});

	it('remote permission_denied dismisses the matching prompt; later settle is a no-op', async () => {
		const poster = fakePoster();
		const ui = fakeUi();
		const flow = new PermissionFlow(new Set(), ui);

		flow.handleRequest(poster, 'sess-5', request('write_file'));
		const pending = ui.events[0].pending!;

		flow.handleDenied('write_file');
		expect(ui.events.at(-1)).toMatchObject({ kind: 'dismiss', reason: 'denied-remote' });

		expect(pending.settle('allow-once')).toBe(false);
		await settle();
		expect(poster.decisions).toHaveLength(0);
	});

	it('permission_denied for a tool with no open prompt is a no-op', () => {
		const ui = fakeUi();
		const flow = new PermissionFlow(new Set(), ui);
		flow.handleDenied('write_file');
		expect(ui.events).toHaveLength(0);
	});

	it('endTurn dismisses all open prompts and blocks late answers', async () => {
		const poster = fakePoster();
		const ui = fakeUi();
		const flow = new PermissionFlow(new Set(), ui);

		flow.handleRequest(poster, 'sess-6', request('write_file', 'call-1'));
		flow.handleRequest(poster, 'sess-6', request('edit_file', 'call-2'));
		const first = ui.events[0].pending!;

		flow.endTurn();
		const dismissals = ui.events.filter((e) => e.kind === 'dismiss');
		expect(dismissals).toHaveLength(2);
		expect(dismissals.every((e) => e.reason === 'turn-ended')).toBe(true);

		expect(first.settle('allow-once')).toBe(false);
		await settle();
		expect(poster.decisions).toHaveLength(0);
	});

	it('settleById routes to the right prompt; unknown ids are no-ops', async () => {
		const poster = fakePoster();
		const ui = fakeUi();
		const flow = new PermissionFlow(new Set(), ui);

		flow.handleRequest(poster, 'sess-7', request('write_file', 'call-1'));
		flow.handleRequest(poster, 'sess-7', request('edit_file', 'call-2'));
		const second = ui.events[1].pending!;

		expect(flow.settleById(9999, 'deny')).toBe(false);
		expect(flow.settleById(second.id, 'allow-once')).toBe(true);
		await settle();

		expect(poster.decisions).toEqual([
			{ session_id: 'sess-7', tool_call_id: 'call-2', decision: 'allow', scope: 'once' },
		]);
	});

	it('reports POST failures via onPostError instead of throwing', async () => {
		const boom = new Error('proxy 500');
		const poster = fakePoster(boom);
		const ui = fakeUi();
		const flow = new PermissionFlow(new Set(), ui);

		flow.handleRequest(poster, 'sess-8', request('write_file'));
		ui.events[0].pending!.settle('allow-once');
		await settle();

		expect(ui.events.at(-1)).toMatchObject({ kind: 'post-error', toolName: 'write_file', error: boom });
	});

	it('auto-allow POST failure also routes to onPostError', async () => {
		const boom = new Error('proxy 500');
		const poster = fakePoster(boom);
		const ui = fakeUi();
		const flow = new PermissionFlow(new Set(['edit_file']), ui);

		flow.handleRequest(poster, 'sess-9', request('edit_file'));
		await settle();

		expect(ui.events.at(-1)).toMatchObject({ kind: 'post-error', toolName: 'edit_file', error: boom });
	});
});

// One "Allow for Session" on a deletion used to answer every later deletion
// in the conversation without the user seeing which file. The unmarked case
// is a proxy that does not send one_time_only.
describe('deletions are approved one file at a time', () => {
	function deletion(id: string, marked: boolean): PermissionRequestEventData {
		return {
			tool_name: 'delete_file',
			args: { path: 'important.db' },
			message: 'Allow this one deletion? important.db',
			tool_call_id: id,
			...(marked ? { one_time_only: true } : {}),
		};
	}
	for (const marked of [true, false]) {
		it(`never approves a deletion for the session (marked=${marked})`, async () => {
			const allowed = new Set<string>();
			const ui = fakeUi();
			const poster = fakePoster();
			const flow = new PermissionFlow(allowed, ui);

			flow.handleRequest(poster, 's', deletion('c1', marked));
			const first = ui.events.find((e) => e.kind === 'prompt')?.pending;
			expect(first).toBeDefined();
			first?.settle('allow-session');
			await settle();
			expect(poster.decisions[0]).toMatchObject({ decision: 'allow', scope: 'once' });
			expect(allowed.has('delete_file')).toBe(false);

			// Even an allowlist entry from elsewhere answers nothing.
			allowed.add('delete_file');
			flow.handleRequest(poster, 's', deletion('c2', marked));
			expect(ui.events.filter((e) => e.kind === 'prompt')).toHaveLength(2);
			expect(ui.events.some((e) => e.kind === 'auto-allow')).toBe(false);
		});
	}
});

// Approval is the only per-command control, so the card shows the command
// whole. Condensed to 117 characters, the end of a chain was never seen.
describe('commandOf', () => {
	it('returns the whole command, and nothing for a call that runs none', () => {
		const long =
			'cd tests && python3 -m pytest -q test_api.py test_models.py test_views.py 2>&1 | tail -n 40 && cd .. && rm -rf src/legacy data/ && git checkout -- .';
		expect(commandOf({ command: long })).toBe(long);
		expect(commandOf({ path: 'a.py' })).toBeUndefined();
		expect(commandOf(null)).toBeUndefined();
	});
});

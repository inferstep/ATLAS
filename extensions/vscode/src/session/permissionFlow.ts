// Permission flow for mid-turn permission_request events. Pure logic, no
// vscode imports — the UI (native notification + webview card) is injected
// via PermissionUi so the flow runs under plain vitest.
//
// Conventions mirrored from tui/model.go:
//   - A tool already in sessionAllowedTools is auto-answered allow/"once"
//     (NOT "session" — the allowlist itself is re-sent on the next turn's
//     request; the proxy does not persist it), fire-and-forget, no prompt.
//   - "Allow for session" adds the tool to the allowlist AND answers with
//     scope "session" so the in-flight turn honors it too.
//   - Except for a one-time request (a deletion): it is never auto-answered,
//     and "Allow for session" on it allows this request only.
//   - A proxy-side permission_denied (timeout, cancel) can arrive while the
//     prompt is still up — the pending prompt is dismissed by tool name.
//   - Decision POSTs are advisory: the proxy has its own deny fail-safe, so
//     post failures are reported to the UI for logging, never rethrown.

import type { PermissionDecisionRequest, PermissionRequestEventData } from '../client/types';

/** Minimal poster surface — satisfied by AtlasClient. */
export interface PermissionPoster {
	postPermissionDecision(decision: PermissionDecisionRequest): Promise<void>;
}

/** What the user picked on a prompt. */
export type PermissionChoice = 'allow-once' | 'allow-session' | 'deny';

/** Why a pending prompt went away. */
export type DismissReason = 'answered' | 'denied-remote' | 'turn-ended';

/** UI surface driven by the flow. All callbacks are synchronous fire-and-
 * forget from the flow's point of view. */
export interface PermissionUi {
	/** Show the prompt (notification and/or inline card). */
	onPrompt(pending: PendingPermission): void;
	/** Remove/resolve the prompt UI. When reason is 'answered',
	 * `pending.choice` carries what the user picked. */
	onDismiss(pending: PendingPermission, reason: DismissReason): void;
	/** A session-allowlisted tool was auto-answered without a prompt. */
	onAutoAllow(toolName: string): void;
	/** A decision POST failed (non-404). Advisory — log and move on. */
	onPostError(toolName: string, error: unknown): void;
}

/** One live permission prompt. Multiple UI surfaces may call settle();
 * the first answer wins and later calls are no-ops. */
export class PendingPermission {
	private settled = false;
	private settledChoice: PermissionChoice | undefined;

	constructor(
		/** Stable id for routing webview button clicks back to this prompt. */
		readonly id: number,
		readonly sessionId: string,
		readonly request: PermissionRequestEventData,
		private readonly onSettle: (pending: PendingPermission, choice: PermissionChoice) => void,
	) {}

	get isSettled(): boolean {
		return this.settled;
	}

	/** The winning choice, once settled by a user answer. */
	get choice(): PermissionChoice | undefined {
		return this.settledChoice;
	}

	/** Record the user's answer. Returns true when this call decided the
	 * request, false when another surface (or a remote deny) beat it. */
	settle(choice: PermissionChoice): boolean {
		if (this.settled) {
			return false;
		}
		this.settled = true;
		this.settledChoice = choice;
		this.onSettle(this, choice);
		return true;
	}

	/** Mark settled without a user choice (remote deny / turn end) so a
	 * late button click becomes a no-op. */
	dismiss(): void {
		this.settled = true;
	}
}

/** A request whose answer covers only itself. The proxy marks deletions
 * one_time_only, and delete_file is treated so for a proxy that does not. One
 * "Allow for Session" used to answer every later deletion in the
 * conversation without the user seeing which file. */
export function isOneTimeOnly(request: PermissionRequestEventData): boolean {
	return request.one_time_only === true || request.tool_name === 'delete_file';
}

/** The command a request would run, shown whole. Approval is the only
 * per-command control, and a detail cut at 117 characters hid the end of a
 * chain. undefined when the call runs no command. */
export function commandOf(args: unknown): string | undefined {
	if (args !== null && typeof args === 'object') {
		const command = (args as { command?: unknown }).command;
		if (typeof command === 'string') {
			return command;
		}
	}
	return undefined;
}

export class PermissionFlow {
	private pending: PendingPermission[] = [];
	private nextId = 1;

	constructor(
		/** Shared with TurnManager — additions here are re-sent as
		 * session_allowed_tools on every later turn. */
		private readonly sessionAllowedTools: Set<string>,
		private readonly ui: PermissionUi,
	) {}

	/** Handle a permission_request event from the stream. */
	handleRequest(poster: PermissionPoster, sessionId: string, request: PermissionRequestEventData): void {
		const once = isOneTimeOnly(request);
		if (!once && this.sessionAllowedTools.has(request.tool_name)) {
			this.post(poster, sessionId, request, 'allow', 'once');
			this.ui.onAutoAllow(request.tool_name);
			return;
		}
		const pending = new PendingPermission(this.nextId++, sessionId, request, (settled, choice) => {
			this.remove(settled);
			const forSession = choice === 'allow-session' && !once;
			if (forSession) {
				this.sessionAllowedTools.add(request.tool_name);
			}
			this.post(poster, sessionId, request, choice === 'deny' ? 'deny' : 'allow', forSession ? 'session' : 'once');
			this.ui.onDismiss(settled, 'answered');
		});
		this.pending.push(pending);
		this.ui.onPrompt(pending);
	}

	/** Handle a permission_denied event: the proxy resolved the request on
	 * its side (timeout or cancel) — dismiss the oldest matching prompt. */
	handleDenied(toolName: string): void {
		const pending = this.pending.find((p) => p.request.tool_name === toolName);
		if (!pending) {
			return;
		}
		pending.dismiss();
		this.remove(pending);
		this.ui.onDismiss(pending, 'denied-remote');
	}

	/** The turn is over (done, error, or cancel) — nothing left to answer. */
	endTurn(): void {
		const open = this.pending;
		this.pending = [];
		for (const pending of open) {
			pending.dismiss();
			this.ui.onDismiss(pending, 'turn-ended');
		}
	}

	/** Route a webview answer by prompt id. Unknown/settled ids are no-ops
	 * (stale card after a reload, or another surface answered first). */
	settleById(id: number, choice: PermissionChoice): boolean {
		const pending = this.pending.find((p) => p.id === id);
		return pending ? pending.settle(choice) : false;
	}

	private remove(pending: PendingPermission): void {
		this.pending = this.pending.filter((p) => p !== pending);
	}

	private post(
		poster: PermissionPoster,
		sessionId: string,
		request: PermissionRequestEventData,
		decision: 'allow' | 'deny',
		scope: 'once' | 'session',
	): void {
		void poster
			.postPermissionDecision({
				session_id: sessionId,
				tool_call_id: request.tool_call_id,
				decision,
				scope,
			})
			.catch((error: unknown) => this.ui.onPostError(request.tool_name, error));
	}
}

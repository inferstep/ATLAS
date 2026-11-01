// Turn/session state for the chat: session_id minting, rolling history,
// session-approved tools, and cancellation. Conventions mirror the TUI
// reference client (tui/chat.go) — see the plan and docs/API.md.
//
// Deliberately vscode-free so it is unit-testable under plain vitest.

import { randomBytes } from 'node:crypto';
import type { AgentRequest, ChatEvent, HistoryMessage, PermissionMode, TaskMode } from '../client/types';

/** The proxy caps replayed history at the most recent 40 entries; we cap
 * client-side too so the request body never grows unbounded (tui/chat.go). */
export const HISTORY_LIMIT = 40;

/** Minimal client surface TurnManager needs — satisfied by AtlasClient,
 * kept structural so tests can pass a fake. */
export interface TurnClient {
	sendAgentTurn(request: AgentRequest, signal?: AbortSignal): AsyncGenerator<ChatEvent, void, undefined>;
	cancelTurn(sessionId: string): Promise<boolean>;
}

/** Client-minted per-turn session id: 12 random bytes, hex-encoded
 * (24 chars) — same recipe as tui/chat.go. */
export function mintSessionId(): string {
	return randomBytes(12).toString('hex');
}

export class TurnManager {
	private history: HistoryMessage[] = [];
	/** Tools approved with scope "session"; re-sent on every later turn. */
	readonly sessionAllowedTools = new Set<string>();

	private abort: AbortController | undefined;
	private activeClient: TurnClient | undefined;
	private activeSessionId = '';

	/** True while a turn's event stream is being consumed. */
	get busy(): boolean {
		return this.abort !== undefined;
	}

	/** session_id of the in-flight (or most recent) turn — needed to route
	 * POST /v1/permission decisions. */
	get sessionId(): string {
		return this.activeSessionId;
	}

	/** Run one agent turn: mint a session id, send message + rolling history,
	 * and yield the raw event stream. History is finalized in `finally` so it
	 * updates on done, error, and cancel alike (partial assistant text kept).
	 *
	 * `workingDir` is the client's workspace root, like the TUI's host cwd
	 * (tui/model.go): on Docker installs the proxy uses it for host-to-
	 * container path translation; on bare metal file ops resolve against it.
	 *
	 * `taskMode` is the user's Work/Question declaration for this one request
	 * and goes on the wire as `task_contract.task_mode`. Every turn declares
	 * one: without a contract the proxy falls back to guessing from the
	 * message and produces no V3 candidate, because nothing grounds a target
	 * (docs/API.md, "Building a non-TUI client"). */
	async *runTurn(
		client: TurnClient,
		message: string,
		mode: PermissionMode,
		taskMode: TaskMode,
		workingDir: string,
	): AsyncGenerator<ChatEvent, void, undefined> {
		if (this.abort) {
			throw new Error('a turn is already in progress');
		}
		const abort = new AbortController();
		this.abort = abort;
		this.activeClient = client;
		this.activeSessionId = mintSessionId();

		const request: AgentRequest = {
			message,
			working_dir: workingDir,
			mode,
			session_id: this.activeSessionId,
			history: [...this.history],
			// Declared, never derived: the only input is the caller's
			// selection. The obligation lists stay absent for the same reason
			// the TUI's do (tui/chat.go).
			task_contract: { task_mode: taskMode },
		};
		if (this.sessionAllowedTools.size > 0) {
			request.session_allowed_tools = [...this.sessionAllowedTools];
		}

		let assistantText = '';
		let doneSummary = '';
		try {
			for await (const event of client.sendAgentTurn(request, abort.signal)) {
				if (event.type === 'text') {
					const data = event.data as { content?: unknown };
					if (typeof data?.content === 'string') {
						assistantText += data.content;
					}
				} else if (event.type === 'done') {
					// On a tool-shaped turn the model's final answer arrives ONLY
					// here (proxy/loop_turn.go) — no text event precedes it.
					const data = event.data as { summary?: unknown };
					if (typeof data?.summary === 'string') {
						doneSummary = data.summary;
					}
				}
				yield event;
			}
		} finally {
			this.history.push({ role: 'user', content: message });
			// Streamed text wins; the done summary stands in for it on
			// tool-shaped turns so the next turn's history isn't silent.
			const finalText = assistantText !== '' ? assistantText : doneSummary;
			if (finalText !== '') {
				// TUI convention: assistant history entries are re-wrapped in
				// the {"type":"text","content":...} envelope (tui/chat.go).
				this.history.push({
					role: 'assistant',
					content: JSON.stringify({ type: 'text', content: finalText }),
				});
			}
			if (this.history.length > HISTORY_LIMIT) {
				this.history = this.history.slice(-HISTORY_LIMIT);
			}
			this.abort = undefined;
			this.activeClient = undefined;
		}
	}

	/** Cancel the in-flight turn: abort the local stream (primary) and fire a
	 * best-effort POST /cancel (defense-in-depth — TUI convention). No-op when
	 * idle. */
	cancel(): void {
		if (!this.abort) {
			return;
		}
		const client = this.activeClient;
		const sessionId = this.activeSessionId;
		this.abort.abort();
		if (client && sessionId) {
			void client.cancelTurn(sessionId);
		}
	}

	/** Start a fresh conversation: drop history and session-approved tools.
	 * Callers should cancel() first if a turn is in flight. */
	reset(): void {
		this.history = [];
		this.sessionAllowedTools.clear();
	}
}

// Chat sidebar: a WebviewViewProvider that owns all turn state and treats
// the webview as a dumb renderer. Every message posted to the webview is
// also appended to a transcript so a re-created webview (sidebar closed and
// reopened, window reload of the view) can be replayed from scratch.

import { randomBytes } from 'node:crypto';
import { promises as fs } from 'node:fs';
import * as path from 'node:path';
import * as vscode from 'vscode';
import { AtlasClient } from '../client/atlasClient';
import type {
	AgentLensInterventionEventData,
	DoneEventData,
	ErrorEventData,
	LlmPromptProgressEventData,
	PermissionDeniedEventData,
	PermissionMode,
	PermissionRequestEventData,
	PlanAdherenceEventData,
	PlanLoadedEventData,
	PlanReviseEventData,
	ReasoningTokenEventData,
	TextEventData,
	ToolCallEventData,
	ToolResultEventData,
	V3ProgressEventData,
	V3StageEventData,
} from '../client/types';
import { editTargetPath, predictEdit, type EditPrediction } from '../session/editPreview';
import { summarizeToolResult } from '../session/toolSummary';
import {
	PendingPermission,
	PermissionFlow,
	type DismissReason,
	type PermissionChoice,
	commandOf,
	isOneTimeOnly,
} from '../session/permissionFlow';
import { TurnManager } from '../session/turnManager';
import { describeForLog, renderError } from '../util/errors';
import { MismatchDetector } from '../workspace/mismatch';
import { DiffProvider } from './diffProvider';
import type { StatusBar } from './statusBar';

/** SecretStorage key for the service token ('ATLAS: Set Service Token'). */
export const TOKEN_SECRET_KEY = 'atlas.serviceToken';

/** workspaceState key for the mismatch warning's "Don't show again". */
export const MISMATCH_DISMISSED_KEY = 'atlas.mismatchWarningDismissed';

/** Messages the extension posts INTO the webview (media/chat.js). */
type OutboundMessage =
	| { type: 'userMessage'; text: string }
	| { type: 'assistantDelta'; text: string }
	| { type: 'reasoningDelta'; text: string }
	| { type: 'toolCall'; name: string; detail: string }
	| { type: 'toolResult'; tool: string; success: boolean; elapsed?: string; error?: string; diffId?: number; summary?: string }
	| { type: 'toolDenied'; tool: string }
	| { type: 'doneSummary'; text: string }
	| { type: 'note'; text: string }
	| { type: 'badge'; text: string }
	| { type: 'permissionPrompt'; id: number; tool: string; detail: string; message: string; canDiff: boolean; note?: string; oneTimeOnly: boolean }
	| { type: 'permissionResolved'; id: number; outcome: string }
	| { type: 'planLoaded'; steps: { id: string; label: string }[]; revision: number }
	| { type: 'planStep'; stepId: string }
	| { type: 'progress'; text: string }
	| { type: 'turnDone' }
	| { type: 'turnError'; message: string }
	| { type: 'reset' }
	| { type: 'busy'; value: boolean };

/** Messages the webview posts back to the extension. */
type InboundMessage =
	| { type: 'ready' }
	| { type: 'submit'; text: string }
	| { type: 'cancel' }
	| { type: 'permissionAnswer'; id: number; choice: PermissionChoice }
	| { type: 'viewPermissionDiff'; id: number }
	| { type: 'viewAppliedDiff'; id: number };

/** File state captured when a file-edit tool_call streams past, keyed by
 * tool name FIFO (tool_result carries only the tool name — same matching
 * scheme the webview uses for its chips). */
interface EditSnapshot {
	path: string;
	before: string | undefined;
}

/** What a resolved "View change" chip button opens: the exact applied
 * change (snapshot vs on-disk), falling back to edit_file's server-computed
 * diff_preview when the file is not readable in this workspace. */
interface AppliedDiff {
	tool: string;
	path: string;
	before: string | undefined;
	after: string | undefined;
	preview?: string;
}

/** Condense tool args to a single short line for the tool chip. */
function condenseArgs(args: unknown): string {
	if (args === null || args === undefined) {
		return '';
	}
	let text: string;
	try {
		text = typeof args === 'string' ? args : JSON.stringify(args);
	} catch {
		return '';
	}
	return text.length > 120 ? `${text.slice(0, 117)}...` : text;
}

export class ChatViewProvider implements vscode.WebviewViewProvider {
	static readonly viewType = 'atlas.chatView';

	private view: vscode.WebviewView | undefined;
	/** Replayed into any freshly created webview; `busy` is transient and
	 * excluded. */
	private transcript: OutboundMessage[] = [];
	private readonly turns = new TurnManager();
	private readonly permissions: PermissionFlow;
	private readonly output: vscode.OutputChannel;

	/** Pre-edit file states awaiting their tool_result, FIFO per tool name. */
	private snapshots = new Map<string, EditSnapshot[]>();
	/** Applied changes viewable from resolved tool chips, by diff id. */
	private appliedDiffs = new Map<number, AppliedDiff>();
	/** Permission-time predictions viewable while the prompt is open. */
	private permissionDiffs = new Map<number, { tool: string; prediction: EditPrediction }>();
	private nextDiffId = 1;
	/** Hand-off slot: dispatch() computes the prediction, then handleRequest
	 * synchronously calls back into showPermissionPrompt which claims it. */
	private promptPrediction: EditPrediction | undefined;

	/** Passive workspace-mismatch heuristic (see workspace/mismatch.ts). */
	private readonly mismatch: MismatchDetector;
	/** Optional status bar — polling pauses while a turn streams. */
	private statusBar: StatusBar | undefined;

	constructor(
		private readonly extensionUri: vscode.Uri,
		private readonly secrets: vscode.SecretStorage,
		private readonly diffs: DiffProvider,
		private readonly workspaceState: vscode.Memento,
	) {
		this.output = vscode.window.createOutputChannel('ATLAS');
		this.permissions = new PermissionFlow(this.turns.sessionAllowedTools, {
			onPrompt: (pending) => this.showPermissionPrompt(pending),
			onDismiss: (pending, reason) => this.resolvePermissionPrompt(pending, reason),
			onAutoAllow: (toolName) => this.post({ type: 'note', text: `'${toolName}' auto-allowed (approved for this session).` }),
			onPostError: (toolName, error) => this.log(`permission decision POST failed for '${toolName}'`, error),
		});
		this.mismatch = new MismatchDetector({
			stat: (relative) => this.statLocalFile(relative),
			onMismatch: () => this.warnMismatch(),
		});
	}

	/** Late-bound (the status bar is created after the view provider). */
	attachStatusBar(statusBar: StatusBar): void {
		this.statusBar = statusBar;
	}

	/** Build a client from current settings + stored token. Shared with the
	 * status bar poller via extension.ts. */
	async makeClient(): Promise<AtlasClient> {
		const config = vscode.workspace.getConfiguration('atlas');
		// 127.0.0.1, not localhost: the proxy binds IPv4 only, and a
		// resolver that returns ::1 first makes every request hang for the
		// full connect timeout instead of failing fast. Observed as
		// UND_ERR_CONNECT_TIMEOUT against localhost:8090 on a healthy proxy.
		const baseUrl = config.get<string>('proxyUrl', 'http://127.0.0.1:8090');
		// Plaintext setting is a dev override; SecretStorage is the real home.
		const token = config.get<string>('serviceToken', '') || (await this.secrets.get(TOKEN_SECRET_KEY)) || '';
		return new AtlasClient({ baseUrl, token });
	}

	resolveWebviewView(view: vscode.WebviewView): void {
		this.view = view;
		// A post to a disposed webview throws and would kill the stream
		// consumption loop mid-turn — drop the reference instead (post()
		// already tolerates an absent view; the transcript replays later).
		view.onDidDispose(() => {
			if (this.view === view) {
				this.view = undefined;
			}
		});
		view.webview.options = {
			enableScripts: true,
			localResourceRoots: [vscode.Uri.joinPath(this.extensionUri, 'media')],
		};
		view.webview.html = this.renderHtml(view.webview);
		view.webview.onDidReceiveMessage((message: InboundMessage) => {
			switch (message.type) {
				case 'ready':
					this.replay();
					break;
				case 'submit':
					void this.runTurn(message.text);
					break;
				case 'cancel':
					this.cancelTurn();
					break;
				case 'permissionAnswer':
					// Stale/settled ids are no-ops inside the flow (first answer wins).
					this.permissions.settleById(message.id, message.choice);
					break;
				case 'viewPermissionDiff':
					void this.openPermissionDiff(message.id);
					break;
				case 'viewAppliedDiff':
					void this.openAppliedDiff(message.id);
					break;
			}
		});
	}

	cancelTurn(): void {
		this.turns.cancel();
	}

	newConversation(): void {
		this.turns.cancel();
		this.turns.reset();
		this.transcript = [];
		this.snapshots.clear();
		this.appliedDiffs.clear();
		this.permissionDiffs.clear();
		this.postTransient({ type: 'reset' });
	}

	private async runTurn(text: string): Promise<void> {
		const message = text.trim();
		if (message === '') {
			return;
		}
		if (this.turns.busy) {
			this.post({ type: 'note', text: 'A turn is already in progress — cancel it first.' });
			return;
		}

		const config = vscode.workspace.getConfiguration('atlas');
		const mode = config.get<PermissionMode>('permissionMode', 'default');
		// Host workspace root, like the TUI's host cwd (tui/model.go) — the
		// proxy uses it for host-to-container path translation on Docker and
		// resolves file ops against it on bare metal. "." only when there is
		// no folder open (a chat-only session).
		const workingDir = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath ?? '.';

		this.post({ type: 'userMessage', text: message });
		this.postTransient({ type: 'busy', value: true });
		this.statusBar?.setStreaming(true);
		try {
			// Inside the try: a SecretStorage failure must surface as an error
			// card, not make Send silently do nothing.
			const client = await this.makeClient();
			for await (const event of this.turns.runTurn(client, message, mode, workingDir)) {
				await this.dispatch(event.type, event.data, client);
			}
			this.post({ type: 'turnDone' });
		} catch (error) {
			this.handleTurnFailure(error);
		} finally {
			// Any prompt still open has nothing left to answer — the proxy
			// resolves pending requests when the turn ends.
			this.permissions.endTurn();
			this.snapshots.clear();
			this.mismatch.reset();
			this.postTransient({ type: 'progress', text: '' });
			this.postTransient({ type: 'busy', value: false });
			this.statusBar?.setStreaming(false);
		}
	}

	private async dispatch(type: string, data: unknown, client: AtlasClient): Promise<void> {
		switch (type) {
			case 'text': {
				const payload = data as TextEventData;
				if (typeof payload?.content === 'string') {
					this.post({ type: 'assistantDelta', text: payload.content });
				}
				break;
			}
			case 'reasoning_token': {
				const payload = data as ReasoningTokenEventData;
				if (typeof payload?.text === 'string') {
					this.post({ type: 'reasoningDelta', text: payload.text });
				}
				break;
			}
			case 'tool_call': {
				const payload = data as ToolCallEventData;
				this.post({ type: 'toolCall', name: payload.name, detail: condenseArgs(payload.args) });
				// Snapshot the target now (before permission/execution) so a
				// successful result can show the exact applied change. At
				// tool_call time rather than permission time: accept-edits
				// and yolo turns never raise a permission_request.
				const target = editTargetPath(payload.name, payload.args);
				if (target !== undefined) {
					const before = await this.readLocalFile(target);
					const queue = this.snapshots.get(payload.name) ?? [];
					queue.push({ path: target, before });
					this.snapshots.set(payload.name, queue);
				}
				await this.mismatch.recordToolCall(payload.name, payload.args);
				break;
			}
			case 'tool_result': {
				const payload = data as ToolResultEventData;
				const diffId = await this.recordAppliedDiff(payload);
				this.post({
					type: 'toolResult',
					tool: payload.tool,
					success: payload.success,
					elapsed: payload.elapsed,
					error: payload.error,
					diffId,
					// The proxy ships the tool's real output here; without it a
					// read-only ask ("list the files") renders as a green check
					// and nothing else.
					summary: payload.success ? summarizeToolResult(payload.tool, payload) : undefined,
				});
				// Fire-and-forget: the check sleeps its settle delay before
				// stat-ing, and the stream must not wait on it.
				void this.mismatch.recordToolResult(payload.tool, payload.success);
				break;
			}
			case 'permission_request': {
				const payload = data as PermissionRequestEventData;
				// Predict the edit from the CURRENT local file so the prompt
				// can offer "View Diff" before the user decides. structural_edit has
				// no post-content in its result, so this is the only pre-view.
				const target = editTargetPath(payload.tool_name, payload.args);
				const current = target === undefined ? undefined : await this.readLocalFile(target);
				this.promptPrediction = predictEdit(payload.tool_name, payload.args, current);
				try {
					this.permissions.handleRequest(client, this.turns.sessionId, payload);
				} finally {
					this.promptPrediction = undefined;
				}
				break;
			}
			case 'permission_denied': {
				// Proxy-side resolution (timeout/cancel) — may arrive while our
				// prompt is still up; the flow dismisses it by tool name. The
				// denied row itself renders here (TUI convention: the local deny
				// path renders NO row to avoid duplicating this event).
				const payload = data as PermissionDeniedEventData;
				this.permissions.handleDenied(payload.tool);
				// A denied call gets NO tool_result (proxy/agent.go emits
				// permission_denied then continues the loop), so every FIFO
				// keyed on tool name must consume its entry here or the next
				// allowed call of the same tool pairs with stale state.
				this.snapshots.get(payload.tool)?.shift();
				this.mismatch.recordDenied(payload.tool);
				this.post({ type: 'toolDenied', tool: payload.tool });
				this.post({ type: 'note', text: `Permission denied for '${payload.tool}'.` });
				break;
			}
			case 'plan_loaded': {
				const payload = data as PlanLoadedEventData;
				if (Array.isArray(payload?.steps)) {
					this.post({
						type: 'planLoaded',
						steps: payload.steps.map((step) => ({ id: step.id, label: `${step.action} ${step.target}`.trim() })),
						revision: payload.revision ?? 0,
					});
				}
				break;
			}
			case 'plan_adherence': {
				const payload = data as PlanAdherenceEventData;
				if (payload?.matched && typeof payload.step_id === 'string') {
					this.post({ type: 'planStep', stepId: payload.step_id });
				}
				break;
			}
			case 'plan_revise': {
				const payload = data as PlanReviseEventData;
				this.post({ type: 'note', text: `Plan going off track — revising (${payload?.reason || 'off-plan streak'}).` });
				break;
			}
			case 'llm_call_start':
				this.postProgress('Thinking…');
				break;
			case 'llm_prompt_progress': {
				const payload = data as LlmPromptProgressEventData;
				const pct = typeof payload?.pct === 'number' && payload.pct > 0 ? ` ${Math.round(payload.pct * 100)}%` : '';
				this.postProgress(`Processing prompt${pct}…`);
				break;
			}
			case 'llm_first_token':
			case 'llm_call_end':
				this.postProgress('');
				break;
			case 'v3_progress': {
				const payload = data as V3ProgressEventData;
				if (typeof payload?.message === 'string') {
					this.postProgress(`V3: ${payload.message}`);
				}
				break;
			}
			case 'v3_phase':
			case 'v3_sandbox':
			case 'v3_repair':
			// The long V3 stages (proxy/tools.go v3StageToEvent) share the same
			// {stage, detail} shape — without these the progress line goes
			// stale exactly where V3 is slowest.
			case 'v3_plansearch':
			case 'v3_divsampling':
			case 'v3_select':
			case 'v3_probe':
			case 'v3_self_test':
			case 'v3_plan': {
				const payload = data as V3StageEventData;
				if (typeof payload?.stage === 'string') {
					this.postProgress(`V3: ${payload.stage}${payload.detail ? ` — ${payload.detail}` : ''}`);
				}
				break;
			}
			case 'v3_lens_veto':
			case 'v3_structural_veto': {
				const payload = data as V3StageEventData;
				const why = type === 'v3_lens_veto' ? 'lens quality veto' : 'unresolved-call veto';
				this.post({ type: 'badge', text: `Candidate rejected (${why})${payload?.detail ? `: ${payload.detail}` : ''}` });
				break;
			}
			case 'agent_lens_intervention': {
				const payload = data as AgentLensInterventionEventData;
				this.post({ type: 'badge', text: `Lens intervention: ${payload?.reason || 'corrective queued'}` });
				break;
			}
			case 'error': {
				const payload = data as ErrorEventData;
				this.post({ type: 'turnError', message: payload.error || 'unknown stream error' });
				break;
			}
			case 'done': {
				// On a tool-shaped turn the model's final answer arrives ONLY in
				// this summary (proxy/agent.go) — render it like the TUI does
				// (tui/model.go renders a chat row for non-empty summaries).
				const payload = data as DoneEventData;
				if (typeof payload?.summary === 'string' && payload.summary !== '') {
					this.post({ type: 'doneSummary', text: payload.summary });
				}
				break;
			}
			// High-volume / TUI-internal streams the panel deliberately drops:
			// llm_token duplicates the JSON tool-call content, v3 token streams
			// churn the DOM for no user signal, agent_lens_score fires per write,
			// turn_start is loop bookkeeping.
			case 'turn_start':
			case 'llm_token':
			case 'v3_token':
			case 'v3_llm_start':
			case 'v3_llm_end':
			case 'v3_reasoning_token':
			case 'agent_lens_score':
				break;
			default:
				// Forward compatibility: unknown event types are logged, never fatal.
				this.log(`unhandled event '${type}'`, data);
		}
	}

	/** Single-line progress indicator under the newest message. Transient:
	 * replay after a reload would show a stale spinner. */
	private postProgress(text: string): void {
		this.postTransient({ type: 'progress', text });
	}

	/** Consume the pending snapshot for a tool_result and, when there is
	 * something to show, capture the on-disk "after" state and register a
	 * viewable applied diff. Returns its id, or undefined. */
	private async recordAppliedDiff(payload: ToolResultEventData): Promise<number | undefined> {
		const queue = this.snapshots.get(payload.tool);
		const snapshot = queue && queue.length > 0 ? queue.shift() : undefined;
		if (!snapshot || !payload.success) {
			return undefined;
		}
		// Read immediately: the proxy writes before emitting the result, and
		// a later read could already include the NEXT edit to the same file.
		const after = await this.readLocalFile(snapshot.path);
		const preview =
			payload.tool === 'edit_file' ? (payload.data as { diff_preview?: string } | undefined)?.diff_preview : undefined;
		if (after === undefined && !preview) {
			return undefined; // nothing viewable (likely a workspace mismatch)
		}
		const diffId = this.nextDiffId++;
		this.appliedDiffs.set(diffId, { tool: payload.tool, path: snapshot.path, before: snapshot.before, after, preview });
		return diffId;
	}

	private async openAppliedDiff(id: number): Promise<void> {
		const applied = this.appliedDiffs.get(id);
		if (!applied) {
			return;
		}
		if (applied.after !== undefined) {
			await this.diffs.openDiff(
				`ATLAS: ${applied.tool} ${applied.path} (applied)`,
				applied.path,
				applied.before ?? '',
				applied.after,
			);
		} else if (applied.preview) {
			await this.diffs.openPreview(applied.path, applied.preview);
		}
	}

	private async openPermissionDiff(id: number): Promise<void> {
		const entry = this.permissionDiffs.get(id);
		if (!entry) {
			return;
		}
		const { tool, prediction } = entry;
		const qualifier = prediction.kind === 'snippet' ? 'snippet' : prediction.approximate ? 'proposed · approximate' : 'proposed';
		await this.diffs.openDiff(
			`ATLAS: ${tool} ${prediction.path} (${qualifier})`,
			prediction.path,
			prediction.left,
			prediction.right,
		);
	}

	private handleTurnFailure(error: unknown): void {
		// Log before rendering: renderError collapses every non-API failure
		// to "Could not reach the ATLAS proxy", which is the same sentence
		// for a refused connection, a DNS miss and a bug in this extension.
		this.log('turn failed', error);
		if (error instanceof Error && error.name === 'AbortError') {
			this.post({ type: 'note', text: 'Turn cancelled.' });
			return;
		}
		const rendered = renderError(error);
		this.post({ type: 'turnError', message: rendered.message });
		if (rendered.action === 'set-token') {
			void vscode.window.showErrorMessage(rendered.message, 'Set Token').then((choice) => {
				if (choice === 'Set Token') {
					void vscode.commands.executeCommand('atlas.setToken');
				}
			});
		} else if (rendered.prominent) {
			void vscode.window.showErrorMessage(rendered.message);
		}
	}

	/** Show a permission prompt on both surfaces: an inline webview card
	 * (buttons post permissionAnswer back) and a native notification. First
	 * answer wins — PendingPermission.settle() ignores the loser. */
	private showPermissionPrompt(pending: PendingPermission): void {
		const prediction = this.promptPrediction;
		if (prediction) {
			this.permissionDiffs.set(pending.id, { tool: pending.request.tool_name, prediction });
		}
		this.post({
			type: 'permissionPrompt',
			id: pending.id,
			tool: pending.request.tool_name,
			detail: commandOf(pending.request.args) ?? condenseArgs(pending.request.args),
			message: pending.request.message || '',
			canDiff: prediction !== undefined,
			note: prediction?.note,
			oneTimeOnly: isOneTimeOnly(pending.request),
		});
		this.showPermissionNotification(pending, prediction !== undefined);
	}

	/** Native notification arm of the prompt. "View Diff" opens the diff and
	 * re-raises the notification (a notification consumes itself on any
	 * click) so the user can still answer from it. */
	private showPermissionNotification(pending: PendingPermission, canDiff: boolean): void {
		let label = pending.request.message || `ATLAS wants to run '${pending.request.tool_name}'.`;
		if (commandOf(pending.request.args) !== undefined) {
			// A notification may cut a long message; the card never does.
			label += ' (The full command is in the ATLAS chat view.)';
		}
		const answers = isOneTimeOnly(pending.request)
			? ['Allow Once', 'Deny'] // a deletion is approved one file at a time
			: ['Allow Once', 'Allow for Session', 'Deny'];
		const buttons = canDiff ? ['View Diff', ...answers] : answers;
		void vscode.window.showInformationMessage(label, ...buttons).then((choice) => {
			if (choice === undefined) {
				return; // dismissed — the card (or the timeout) decides
			}
			if (choice === 'View Diff') {
				void this.openPermissionDiff(pending.id);
				if (!pending.isSettled) {
					this.showPermissionNotification(pending, canDiff);
				}
				return;
			}
			const map: Record<string, PermissionChoice> = {
				'Allow Once': 'allow-once',
				'Allow for Session': 'allow-session',
				Deny: 'deny',
			};
			pending.settle(map[choice]);
		});
	}

	/** Collapse the prompt card into its outcome line. A user deny renders no
	 * extra row — the proxy's permission_denied event carries that (TUI
	 * convention, avoids the duplicate). */
	private resolvePermissionPrompt(pending: PendingPermission, reason: DismissReason): void {
		this.permissionDiffs.delete(pending.id);
		let outcome: string;
		switch (reason) {
			case 'answered':
				outcome =
					pending.choice === 'allow-session'
						? 'allowed for session'
						: pending.choice === 'allow-once'
							? 'allowed once'
							: 'denied';
				break;
			case 'denied-remote':
				outcome = 'resolved by proxy';
				break;
			case 'turn-ended':
				outcome = 'turn ended';
				break;
		}
		this.post({ type: 'permissionResolved', id: pending.id, outcome });
	}

	/** Read a proxy-workspace-relative path from the local workspace folder.
	 * undefined when there is no folder, the file does not exist, or it is
	 * not readable as UTF-8 text. */
	private async readLocalFile(target: string): Promise<string | undefined> {
		const absolute = this.resolveLocalPath(target);
		if (absolute === undefined) {
			return undefined;
		}
		try {
			return await fs.readFile(absolute, 'utf8');
		} catch {
			return undefined;
		}
	}

	private resolveLocalPath(target: string): string | undefined {
		// target comes from model-controlled tool args — never read outside
		// the workspace folder. Absolute paths and `..` escapes are refused
		// (the callers degrade gracefully: no diff / inconclusive verdict).
		if (path.isAbsolute(target)) {
			return undefined;
		}
		const folder = vscode.workspace.workspaceFolders?.[0];
		if (folder === undefined) {
			return undefined;
		}
		const root = folder.uri.fsPath;
		const absolute = path.resolve(root, target);
		if (absolute !== root && !absolute.startsWith(root + path.sep)) {
			return undefined;
		}
		return absolute;
	}

	/** Stat arm of the mismatch heuristic. Unresolvable (no folder) maps to
	 * absent — the detector's verdicts treat that conservatively. */
	private async statLocalFile(target: string): Promise<{ exists: boolean; mtimeMs?: number }> {
		const absolute = this.resolveLocalPath(target);
		if (absolute === undefined) {
			return { exists: false };
		}
		try {
			const stat = await fs.stat(absolute);
			return { exists: true, mtimeMs: stat.mtimeMs };
		} catch {
			return { exists: false };
		}
	}

	/** One-time warning when local disk state contradicts a successful file
	 * op — the proxy is likely mounted on a different directory. */
	private warnMismatch(): void {
		if (this.workspaceState.get<boolean>(MISMATCH_DISMISSED_KEY, false)) {
			return;
		}
		const message =
			'ATLAS applied a file change, but this workspace does not reflect it. ' +
			'The proxy is mounted on a different directory, so edits are landing outside this folder.';
		this.post({ type: 'note', text: message });
		void vscode.window
			.showWarningMessage(message, 'Use This Folder', "Don't Show Again")
			.then((choice) => {
				if (choice === 'Use This Folder') {
					void vscode.commands.executeCommand('atlas.useThisFolder');
				} else if (choice === "Don't Show Again") {
					void this.workspaceState.update(MISMATCH_DISMISSED_KEY, true);
				}
			});
	}

	private post(message: OutboundMessage): void {
		this.transcript.push(message);
		void this.view?.webview.postMessage(message);
	}

	/** Post without recording (busy toggles, reset marker). */
	private postTransient(message: OutboundMessage): void {
		void this.view?.webview.postMessage(message);
	}

	private replay(): void {
		for (const message of this.transcript) {
			void this.view?.webview.postMessage(message);
		}
		this.postTransient({ type: 'busy', value: this.turns.busy });
	}

	/** Append to the ATLAS output channel. Errors are unpacked by hand:
	 * JSON.stringify(new Error('fetch failed')) is '{}', so serializing one
	 * throws away the only part worth reading. undici hangs the real reason
	 * (ECONNREFUSED, ENOTFOUND, a TLS failure) off `cause`, which is what
	 * turns "fetch failed" into something actionable. */
	private log(context: string, detail: unknown): void {
		this.output.appendLine(`[${new Date().toISOString()}] ${context}: ${describeForLog(detail)}`);
	}

	private renderHtml(webview: vscode.Webview): string {
		const scriptUri = webview.asWebviewUri(vscode.Uri.joinPath(this.extensionUri, 'media', 'chat.js'));
		const styleUri = webview.asWebviewUri(vscode.Uri.joinPath(this.extensionUri, 'media', 'chat.css'));
		const nonce = getNonce();
		return `<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="UTF-8">
	<meta http-equiv="Content-Security-Policy"
		content="default-src 'none'; style-src ${webview.cspSource}; script-src 'nonce-${nonce}';">
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	<link href="${styleUri}" rel="stylesheet">
	<title>ATLAS Chat</title>
</head>
<body>
	<main id="messages" aria-live="polite"></main>
	<div id="progress" hidden></div>
	<footer id="composer">
		<textarea id="input" rows="2" placeholder="Ask ATLAS..."></textarea>
		<div id="actions">
			<button id="send" type="button">Send</button>
			<button id="stop" type="button" hidden>Stop</button>
		</div>
	</footer>
	<script nonce="${nonce}" src="${scriptUri}"></script>
</body>
</html>`;
	}
}

function getNonce(): string {
	// A CSP nonce is a security token: it must be unguessable, so it comes
	// from the CSPRNG rather than Math.random(). 16 bytes of hex keeps the
	// 32-character shape and stays inside the CSP base64-value grammar.
	return randomBytes(16).toString('hex');
}

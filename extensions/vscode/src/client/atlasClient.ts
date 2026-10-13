// HTTP client for the atlas-proxy public API: /v1/agent (SSE),
// /cancel, /v1/permission, /ready, /version. Zero runtime deps —
// Node 18+ global fetch + AbortController. No vscode imports so the
// mock-proxy integration tests run under plain vitest.
//
// Conventions mirrored from tui/chat.go:
//   - Authorization: Bearer <token> on every request when a token is set.
//   - /v1/permission 404 = request already resolved = success.
//   - /cancel is best-effort defense-in-depth; aborting the SSE request
//     is the primary cancel mechanism.

import { parseSSEStream } from './sse';
import type {
	AgentRequest,
	CalibrationStatusResponse,
	ChatEvent,
	ErrorEnvelope,
	PermissionDecisionRequest,
	ReadyResponse,
	VersionResponse,
	WorkspaceResponse,
} from './types';

export interface AtlasClientOptions {
	/** Base URL of the proxy, e.g. http://localhost:8090. */
	baseUrl: string;
	/** Bearer token; empty string sends no Authorization header. */
	token?: string;
}

/** Undici's dispatcher option as @types/node declares it on RequestInit —
 * kept as a named alias so the cast below is self-documenting. */
type FetchDispatcher = NonNullable<RequestInit['dispatcher']>;

/** Undici publishes its global dispatcher under this well-known symbol
 * (Node docs: getGlobalDispatcher). Read lazily — it only exists after
 * the first fetch initializes undici. */
const GLOBAL_DISPATCHER = Symbol.for('undici.globalDispatcher.1');

let agentStreamDispatcher: FetchDispatcher | undefined;
let dispatcherProbed = false;

/**
 * Dispatcher for the /v1/agent stream with the body idle timeout disabled.
 *
 * While a permission_request is pending the proxy sends NOTHING on the
 * agent stream (the heartbeat is only on /events), and the documented
 * fail-safe is ATLAS_PERMISSION_TIMEOUT_SEC = 600s. Undici's default
 * bodyTimeout (~300s idle) would kill the stream first. Zero-runtime-deps
 * rules out importing undici, so build an Agent via the constructor of the
 * global dispatcher instead; headersTimeout keeps its default (the TUI
 * likewise uses a response-header timeout and no overall one).
 *
 * Returns undefined on runtimes without the undici global (fetch then
 * behaves as before — degraded, not broken).
 *
 * Exported for tests only.
 */
export async function agentDispatcher(): Promise<FetchDispatcher | undefined> {
	if (dispatcherProbed) {
		return agentStreamDispatcher;
	}
	dispatcherProbed = true;
	try {
		// Force undici init without touching the network.
		await fetch('data:,').catch(() => {});
		const global = (globalThis as Record<symbol, unknown>)[GLOBAL_DISPATCHER];
		if (global && typeof global === 'object') {
			const Agent = global.constructor as new (options: object) => FetchDispatcher;
			agentStreamDispatcher = new Agent({ bodyTimeout: 0 });
		}
	} catch {
		agentStreamDispatcher = undefined;
	}
	return agentStreamDispatcher;
}

/** Error carrying the proxy's stable error envelope when one was parseable.
 * Callers switch on `code` (closed set), never on the human `detail`. */
export class AtlasApiError extends Error {
	readonly status: number;
	readonly code: string;
	readonly detail: string;

	constructor(status: number, envelope: Partial<ErrorEnvelope>, fallbackBody: string) {
		const code = envelope.error ?? '';
		const detail = envelope.detail ?? fallbackBody;
		super(code ? `${code}: ${detail}` : `HTTP ${status}: ${detail}`);
		this.name = 'AtlasApiError';
		this.status = status;
		this.code = code;
		this.detail = detail;
	}
}

async function toApiError(response: Response): Promise<AtlasApiError> {
	const body = await response.text().catch(() => '');
	let envelope: Partial<ErrorEnvelope> = {};
	try {
		envelope = JSON.parse(body) as ErrorEnvelope;
	} catch {
		// non-JSON body (reverse proxy error page etc.) — keep raw text
	}
	return new AtlasApiError(response.status, envelope, body.trim());
}

export class AtlasClient {
	private readonly baseUrl: string;
	private readonly token: string;

	constructor(options: AtlasClientOptions) {
		this.baseUrl = options.baseUrl.replace(/\/+$/, '');
		this.token = options.token ?? '';
	}

	private headers(json = true): Record<string, string> {
		const headers: Record<string, string> = {};
		if (json) {
			headers['Content-Type'] = 'application/json';
		}
		if (this.token !== '') {
			headers['Authorization'] = `Bearer ${this.token}`;
		}
		return headers;
	}

	/**
	 * POST /v1/agent and stream the turn's events. The generator completes
	 * on the `[DONE]` sentinel; aborting `signal` cancels the request (the
	 * caller should also fire cancelTurn as defense-in-depth).
	 *
	 * Non-200 responses throw AtlasApiError before any event is yielded.
	 */
	async *sendAgentTurn(
		request: AgentRequest,
		signal?: AbortSignal,
	): AsyncGenerator<ChatEvent, void, undefined> {
		const init: RequestInit = {
			method: 'POST',
			headers: { ...this.headers(), Accept: 'text/event-stream' },
			body: JSON.stringify(request),
			signal,
			// Long permission waits are silent on this stream; disable the
			// body idle timeout so they outlive undici's ~300s default.
			dispatcher: await agentDispatcher(),
		};
		const response = await fetch(`${this.baseUrl}/v1/agent`, init);
		if (!response.ok) {
			throw await toApiError(response);
		}
		if (!response.body) {
			throw new AtlasApiError(response.status, {}, 'empty response body');
		}
		yield* parseSSEStream(response.body);
	}

	/**
	 * POST /cancel for an in-flight turn. Best-effort: connection failures
	 * and non-200s are swallowed — the SSE abort is the primary mechanism.
	 * Returns true when the proxy reported `cancelled: true`.
	 */
	async cancelTurn(sessionId: string): Promise<boolean> {
		if (sessionId === '') {
			return false;
		}
		try {
			const response = await fetch(`${this.baseUrl}/cancel`, {
				method: 'POST',
				headers: this.headers(),
				body: JSON.stringify({ session_id: sessionId }),
			});
			if (!response.ok) {
				return false; // 404 = nothing in flight (idempotent)
			}
			const body = (await response.json()) as { cancelled?: boolean };
			return body.cancelled === true;
		} catch {
			return false;
		}
	}

	/**
	 * POST /v1/permission answering a permission_request event. A 404 means
	 * the pending request already resolved (cancelled or timed out) — treated
	 * as success per the TUI convention. Other failures throw AtlasApiError.
	 */
	async postPermissionDecision(decision: PermissionDecisionRequest): Promise<void> {
		const response = await fetch(`${this.baseUrl}/v1/permission`, {
			method: 'POST',
			headers: this.headers(),
			body: JSON.stringify(decision),
		});
		if (response.ok || response.status === 404) {
			await response.body?.cancel();
			return;
		}
		throw await toApiError(response);
	}

	/**
	 * GET /ready. Returns the gate body for both 200 and 503 (same shape).
	 * Network failure throws — the status bar maps that to "unreachable".
	 */
	async getReady(): Promise<ReadyResponse> {
		const response = await fetch(`${this.baseUrl}/ready`, {
			method: 'GET',
			headers: this.headers(false),
		});
		if (response.ok || response.status === 503) {
			return (await response.json()) as ReadyResponse;
		}
		throw await toApiError(response);
	}

	/** GET /version — api_version, protocol_version, error_codes. */
	async getVersion(): Promise<VersionResponse> {
		const response = await fetch(`${this.baseUrl}/version`, {
			method: 'GET',
			headers: this.headers(false),
		});
		if (!response.ok) {
			throw await toApiError(response);
		}
		return (await response.json()) as VersionResponse;
	}

	/** GET /workspace - project_dir, working_dir, containerized */
	async getWorkspace(): Promise<WorkspaceResponse | null> {
		try {
			const response = await fetch(`${this.baseUrl}/workspace`, {
				method: 'GET',
				headers: this.headers(false),
			});
			if (!response.ok) {
				return null;
			}
			return (await response.json()) as WorkspaceResponse;
		} catch {
			return null;
		}
	}

	/**
	 * GET /v1/calibration/status — lens/ASA verdict for the loaded model.
	 * Uncached server-side (each call re-probes the lens service), so callers
	 * hit it only at activation and on explicit user refresh.
	 */
	async getCalibrationStatus(): Promise<CalibrationStatusResponse> {
		const response = await fetch(`${this.baseUrl}/v1/calibration/status`, {
			method: 'GET',
			headers: this.headers(false),
		});
		if (!response.ok) {
			throw await toApiError(response);
		}
		return (await response.json()) as CalibrationStatusResponse;
	}
}

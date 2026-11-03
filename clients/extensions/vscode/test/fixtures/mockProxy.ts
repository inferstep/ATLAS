// Real http.Server fixture streaming canned SSE the way atlas-proxy
// does: `: connected` comment first, `data: {...}\n\n` frames, then
// `data: [DONE]\n\n`. Drives integration tests of AtlasClient without
// a live proxy.

import * as http from 'node:http';
import type { AddressInfo } from 'node:net';
import type { ChatEvent } from '../../src/client/types';

export interface RecordedRequest {
	method: string;
	url: string;
	headers: http.IncomingHttpHeaders;
	body: unknown;
}

export interface MockProxyOptions {
	/** Events streamed on POST /v1/agent, in order. */
	agentEvents?: ChatEvent[];
	/** Pause the /v1/agent stream after this event index until a
	 * POST /v1/permission arrives (simulates a permission_request block). */
	pauseAfterIndex?: number;
	/** Respond to /v1/agent with this status + error envelope, no stream. */
	agentError?: { status: number; body: unknown };
	/** Status for POST /v1/permission (default 200). */
	permissionStatus?: number;
	/** Body for GET /ready (default all-true) and its status (default 200). */
	ready?: { status: number; body: unknown };
	/** Milliseconds between streamed frames (default 0 — immediate). */
	frameDelayMs?: number;
	/** Omit the trailing [DONE] sentinel (simulates a dropped connection). */
	omitDone?: boolean;
}

export class MockProxy {
	readonly requests: RecordedRequest[] = [];
	private server: http.Server | undefined;
	private options: MockProxyOptions;
	private resumeSignal: (() => void) | undefined;

	constructor(options: MockProxyOptions = {}) {
		this.options = options;
	}

	get url(): string {
		const address = this.server?.address() as AddressInfo | null;
		if (!address) {
			throw new Error('mock proxy not started');
		}
		return `http://127.0.0.1:${address.port}`;
	}

	async start(): Promise<void> {
		this.server = http.createServer((request, response) => {
			void this.handle(request, response);
		});
		await new Promise<void>((resolve) => this.server!.listen(0, '127.0.0.1', resolve));
	}

	async stop(): Promise<void> {
		const server = this.server;
		this.server = undefined;
		if (server) {
			server.closeAllConnections();
			await new Promise<void>((resolve) => server.close(() => resolve()));
		}
	}

	private async handle(request: http.IncomingMessage, response: http.ServerResponse): Promise<void> {
		const chunks: Buffer[] = [];
		for await (const chunk of request) {
			chunks.push(chunk as Buffer);
		}
		const rawBody = Buffer.concat(chunks).toString('utf-8');
		let body: unknown = undefined;
		if (rawBody !== '') {
			try {
				body = JSON.parse(rawBody);
			} catch {
				body = rawBody;
			}
		}
		this.requests.push({
			method: request.method ?? '',
			url: request.url ?? '',
			headers: request.headers,
			body,
		});

		const route = `${request.method} ${request.url}`;
		switch (route) {
			case 'POST /v1/agent':
				return this.handleAgent(response);
			case 'POST /v1/permission':
				return this.handlePermission(response);
			case 'POST /cancel':
				return json(response, 200, { cancelled: true });
			case 'GET /ready': {
				const ready = this.options.ready ?? {
					status: 200,
					body: { ready: true, inference: true, lens_ready: true, sandbox: true, v3: true },
				};
				return json(response, ready.status, ready.body);
			}
			case 'GET /version':
				return json(response, 200, {
					api_version: '1.0.0',
					protocol_version: 1,
					error_codes: ['unauthorized', 'invalid_input'],
				});
			default:
				return json(response, 404, { error: 'invalid_input', detail: `no route ${route}` });
		}
	}

	private async handleAgent(response: http.ServerResponse): Promise<void> {
		if (this.options.agentError) {
			return json(response, this.options.agentError.status, this.options.agentError.body);
		}
		response.writeHead(200, {
			'Content-Type': 'text/event-stream',
			'Cache-Control': 'no-cache',
			Connection: 'keep-alive',
		});
		response.write(': connected\n\n');

		const events = this.options.agentEvents ?? [];
		for (let index = 0; index < events.length; index++) {
			if (this.options.frameDelayMs) {
				await sleep(this.options.frameDelayMs);
			}
			response.write(`data: ${JSON.stringify(events[index])}\n\n`);
			if (index === this.options.pauseAfterIndex) {
				// Block until a permission decision lands, like the real
				// agent loop pausing on a destructive tool call.
				await new Promise<void>((resolve) => {
					this.resumeSignal = resolve;
				});
			}
		}
		if (!this.options.omitDone) {
			response.write('data: [DONE]\n\n');
		}
		response.end();
	}

	private handlePermission(response: http.ServerResponse): void {
		const status = this.options.permissionStatus ?? 200;
		json(response, status, { delivered: status === 200 });
		if (this.resumeSignal) {
			const resume = this.resumeSignal;
			this.resumeSignal = undefined;
			resume();
		}
	}
}

function json(response: http.ServerResponse, status: number, body: unknown): void {
	response.writeHead(status, { 'Content-Type': 'application/json' });
	response.end(JSON.stringify(body));
}

function sleep(ms: number): Promise<void> {
	return new Promise((resolve) => setTimeout(resolve, ms));
}

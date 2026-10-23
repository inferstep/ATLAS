// The token that the chat view's client sends to the proxy: the
// atlas.serviceToken setting when it is set, else the token in SecretStorage,
// else none. The setting is a plaintext override for development.

import { describe, expect, it, vi } from 'vitest';

// The settings that the stand-in for VS Code gives; each test sets them.
const settings = vi.hoisted(() => ({ values: {} as Record<string, string | undefined> }));

vi.mock('vscode', () => ({
	window: { createOutputChannel: () => ({}) },
	workspace: { getConfiguration: () => ({ get: (key: string, fallback: unknown) => settings.values[key] ?? fallback }) },
}));

import type { Memento, SecretStorage, Uri } from 'vscode';
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

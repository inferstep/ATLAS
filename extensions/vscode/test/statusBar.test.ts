// The status bar shows a rejected token. /ready is an open path and answers
// without one, so the 401 of the calibration call, which needs the token, is
// the one sign that the proxy rejects it.

import { describe, expect, it, vi } from 'vitest';

const item = vi.hoisted(() => ({
	text: '',
	tooltip: '',
	name: '',
	command: '',
	backgroundColor: undefined as unknown,
	show: () => undefined,
	hide: () => undefined,
	dispose: () => undefined,
}));

vi.mock('vscode', () => ({
	window: { createStatusBarItem: () => item },
	StatusBarAlignment: { Right: 2 },
	ThemeColor: class {},
	workspace: { getConfiguration: () => ({ get: (_key: string, fallback: unknown) => fallback }) },
}));

import { AtlasApiError, type AtlasClient } from '../src/client/atlasClient';
import { StatusBar } from '../src/ui/statusBar';

describe('the status bar', () => {
	it.each([
		['a calibration call refused with 401', () => Promise.reject(new AtlasApiError(401, { error: 'unauthorized' }, '')), '$(circle-slash) ATLAS'],
		['a calibration call that answers', () => Promise.resolve({ lens: { verdict: 'ok' }, asa: { verdict: 'ok' } }), '$(check) ATLAS'],
		['a calibration call that fails another way', () => Promise.reject(new Error('connection refused')), '$(check) ATLAS'],
	])('with /ready passing and %s', async (_case, calibration, shown) => {
		const client = {
			getReady: () => Promise.resolve({ ready: true, inference: true, lens_ready: true, sandbox: true, v3: true }),
			getCalibrationStatus: calibration,
		} as unknown as AtlasClient;
		const bar = new StatusBar(() => Promise.resolve(client));
		await bar.refresh();
		expect(item.text).toBe(shown);
		bar.dispose();
	});
});

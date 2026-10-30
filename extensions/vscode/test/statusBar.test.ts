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

const refused = () => Promise.reject(new AtlasApiError(401, { error: 'unauthorized' }, ''));
const answered = () => Promise.resolve({ lens: { verdict: 'ok' }, asa: { verdict: 'ok' } });
const failed = () => Promise.reject(new Error('connection refused'));

describe('the status bar', () => {
	it.each([
		['a calibration call refused with 401', [refused], '$(circle-slash) ATLAS'],
		['a calibration call that answers', [answered], '$(check) ATLAS'],
		['a calibration call that fails another way', [failed], '$(check) ATLAS'],
		['a 401, then a call that fails another way', [refused, failed], '$(circle-slash) ATLAS'],
	])('with /ready passing and %s', async (_case, calibrations, shown) => {
		const answers = [...calibrations];
		const client = {
			getReady: () => Promise.resolve({ ready: true, inference: true, lens_ready: true, sandbox: true, v3: true }),
			getCalibrationStatus: () => answers.shift()!(),
		} as unknown as AtlasClient;
		const bar = new StatusBar(() => Promise.resolve(client));
		for (let refresh = 0; refresh < calibrations.length; refresh++) {
			await bar.refresh();
		}
		expect(item.text).toBe(shown);
		bar.dispose();
	});
});

// While a turn streams the bar does not poll: the stream shows the proxy is up.
describe('the polls of /ready', () => {
	it.each([
		['start polls at once', (bar: StatusBar) => bar.start(), 1],
		['a config change polls at once', (bar: StatusBar) => bar.applyConfig(), 1],
		['the start of a turn does not poll', (bar: StatusBar) => bar.setStreaming(true), 0],
		['the end of a turn polls at once', (bar: StatusBar) => bar.setStreaming(false), 1],
	])('%s', async (_case, trigger, expectedPolls) => {
		let polls = 0;
		const client = {
			getReady: () => {
				polls++;
				return Promise.resolve({ ready: true, inference: true, lens_ready: true, sandbox: true, v3: true });
			},
			getCalibrationStatus: answered,
		} as unknown as AtlasClient;
		const bar = new StatusBar(() => Promise.resolve(client));

		trigger(bar);
		// The poll runs in the background; let it reach the client.
		await new Promise((resolve) => setTimeout(resolve, 0));
		bar.dispose();

		expect(polls).toBe(expectedPolls);
	});
});

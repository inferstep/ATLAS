// Unit tests for renderError: known-code mapping, unknown-code passthrough,
// network-failure rendering and the localhost/IPv6 hint; and for
// describeForLog.

import { describe, expect, it } from 'vitest';
import { AtlasApiError } from '../src/client/atlasClient';
import { describeForLog, renderError } from '../src/util/errors';

function apiError(code: string, detail: string, status = 400): AtlasApiError {
	return new AtlasApiError(status, { error: code, detail }, detail);
}

describe('renderError', () => {
	it('maps unauthorized to a prominent set-token action', () => {
		const rendered = renderError(apiError('unauthorized', 'missing bearer token', 401));
		expect(rendered.action).toBe('set-token');
		expect(rendered.prominent).toBe(true);
		expect(rendered.message).toContain('unauthorized');
		expect(rendered.message).toContain('missing bearer token');
	});

	it.each([
		['dependency_unavailable', true],
		['invalid_input', false],
		['unsupported_operation', false],
		['permission_denied', false],
		['timeout', false],
		['cancelled', false],
		['incompatible_artifact', false],
		['resource_limit', false],
		['sandbox_policy_rejected', false],
		['model_failure', false],
		['internal_error', false],
	])('renders %s with a friendly prefix, the detail and no action (prominent: %s)', (code, prominent) => {
		const rendered = renderError(apiError(code, 'detail text'));
		// Friendly prefix, not the raw code alone.
		expect(rendered.message).not.toMatch(new RegExp(`^${code}:`));
		expect(rendered.message).toContain('detail text');
		expect(rendered.action).toBe('none');
		expect(rendered.prominent).toBe(prominent);
	});

	it('omits the duplicate detail when it matches the code', () => {
		const rendered = renderError(apiError('timeout', 'timeout'));
		expect(rendered.message.endsWith('.')).toBe(true);
		expect(rendered.message).not.toContain(': timeout');
	});

	it('passes unknown codes through verbatim (newer proxy)', () => {
		const rendered = renderError(apiError('quota_exceeded', 'monthly cap hit'));
		expect(rendered.message).toBe('quota_exceeded: monthly cap hit');
		expect(rendered.action).toBe('none');
	});

	it('labels a code-less HTTP failure by status', () => {
		const rendered = renderError(new AtlasApiError(502, {}, 'bad gateway'));
		expect(rendered.message).toContain('HTTP 502');
		expect(rendered.message).toContain('bad gateway');
	});

	it('renders plain Errors as unreachable-proxy (prominent)', () => {
		const rendered = renderError(new Error('fetch failed'));
		expect(rendered.message).toContain('Could not reach the ATLAS proxy');
		expect(rendered.message).toContain('fetch failed');
		expect(rendered.prominent).toBe(true);
	});

	it('stringifies non-Error throwables', () => {
		const rendered = renderError('boom');
		expect(rendered.message).toContain('boom');
	});
});

describe('describeForLog', () => {
	it('keeps an Error readable where JSON.stringify would not', () => {
		// This is the whole reason it exists.
		expect(JSON.stringify(new Error('fetch failed'))).toBe('{}');
		expect(describeForLog(new Error('fetch failed'))).toContain('fetch failed');
	});

	it('surfaces the undici cause code, which is the actionable part', () => {
		// "fetch failed" cannot distinguish a dead proxy from a wrong URL.
		// The cause can.
		const refused = new TypeError('fetch failed');
		(refused as { cause?: unknown }).cause = Object.assign(
			new Error('connect ECONNREFUSED 127.0.0.1:8090'), { code: 'ECONNREFUSED' });
		const line = describeForLog(refused);
		expect(line).toContain('fetch failed');
		expect(line).toContain('ECONNREFUSED');

		const dns = new TypeError('fetch failed');
		(dns as { cause?: unknown }).cause = Object.assign(
			new Error('getaddrinfo ENOTFOUND nope'), { code: 'ENOTFOUND' });
		expect(describeForLog(dns)).toContain('ENOTFOUND');
	});

	it('handles a cause that is not an Error', () => {
		const e = new Error('boom');
		(e as { cause?: unknown }).cause = 'plain string cause';
		expect(describeForLog(e)).toContain('plain string cause');
	});

	it('passes strings through and survives unserializable input', () => {
		expect(describeForLog('already a string')).toBe('already a string');
		const cyclic: Record<string, unknown> = {};
		cyclic.self = cyclic;
		expect(() => describeForLog(cyclic)).not.toThrow();
		expect(describeForLog(undefined)).toBeTypeOf('string');
	});
});

describe('the localhost/IPv6 connect timeout', () => {
	/** The shape undici gives a failed connect: a TypeError with a cause carrying the code and the address. */
	function connectFailure(code: string, address: string): Error {
		const e = new TypeError('fetch failed');
		(e as { cause?: unknown }).cause = Object.assign(
			new Error(`Connect Timeout Error (attempted address: ${address}, timeout: 10000ms)`),
			{ code },
		);
		return e;
	}

	it('names the fix instead of repeating "fetch failed"', () => {
		// The live failure: proxy up and healthy on 127.0.0.1:8090, extension
		// pointed at localhost:8090, request hung the full 10s.
		const r = renderError(connectFailure('UND_ERR_CONNECT_TIMEOUT', 'localhost:8090'));
		expect(r.message).toContain('127.0.0.1');
		expect(r.message).toContain('::1');
	});

	it.each([
		// A timeout against 127.0.0.1 is a different problem; sending the user
		// to change a setting they already have right wastes their time.
		['does not blame IPv6 when the address is already IPv4', 'UND_ERR_CONNECT_TIMEOUT', '127.0.0.1:8090'],
		// ECONNREFUSED means the proxy is down: a completely different fix. The
		// address is localhost, as in the live failure, so only the code differs.
		['leaves a refused connection alone', 'ECONNREFUSED', 'localhost:8090'],
	])('%s', (_name, code, address) => {
		expect(renderError(connectFailure(code, address)).message).not.toContain('Set atlas.proxyUrl');
	});
});

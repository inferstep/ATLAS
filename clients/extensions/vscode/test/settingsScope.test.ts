// The connection settings are machine-wide: a workspace or folder setting
// cannot change the proxy address, the token or the permission mode.

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

const settings = JSON.parse(readFileSync(join(__dirname, '..', 'package.json'), 'utf8'))
	.contributes.configuration.properties;

describe('the connection settings', () => {
	it.each(['atlas.proxyUrl', 'atlas.serviceToken', 'atlas.permissionMode'])('%s is machine-wide', (key) => {
		expect(settings[key].scope).toBe('machine');
	});
});

// What the extension ships is decided by .vscodeignore, not by .gitignore:
// `npm run test:coverage` (the script CI runs) writes its report — and its
// junit.xml — under coverage/, which .gitignore already ignores, so without an
// entry here `vsce package` put the whole report inside the VSIX.

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

const root = join(__dirname, '..');
const manifest = JSON.parse(readFileSync(join(root, 'package.json'), 'utf8'));

const rules = readFileSync(join(root, '.vscodeignore'), 'utf8')
	.split('\n')
	.map((line) => line.trim())
	.filter((line) => line !== '' && !line.startsWith('#'));

// `coverage`, `coverage/` and `coverage/**` all exclude the directory
// wholesale; a rule that names a file inside it does not.
const excluded = rules.flatMap((rule) => [rule, rule.replace(/\/$/, ''), rule.replace(/\/\*\*$/, '')]);

describe('the packaged VSIX', () => {
	it('leaves out the coverage report test:coverage writes', () => {
		expect(excluded).toContain('coverage');
	});

	it('keeps the bundle package.json main points at', () => {
		// Take the directory from `main` rather than naming dist here: the error
		// this guards against is an entry point moving without the ignore rule
		// moving with it, and a literal reads the same either way.
		const parts = manifest.main.replace(/^\.\//, '').split('/');
		expect(parts.length).toBeGreaterThan(1);
		const [dir] = parts;

		expect(excluded.some((rule) => rule === dir || rule.startsWith(`${dir}/`))).toBe(false);
	});
});

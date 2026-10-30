// The two icons the VSIX ships are outside everything else that checks this
// extension: the type-check and the bundle never open them, and no CI job
// packages the VSIX. The manifest's `icon` is the PNG VS Code shows in the
// Extensions view and on the Marketplace listing; the Activity Bar glyph is an
// SVG VS Code applies as a CSS mask at 24 px, so its own size decides how much
// of the slot the mark fills.

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

const root = join(__dirname, '..');
const manifest = JSON.parse(readFileSync(join(root, 'package.json'), 'utf8'));

describe('the extension icons', () => {
	it('declares a square PNG of at least 128 px', () => {
		expect(manifest.icon).toBe('media/icon.png');

		const png = readFileSync(join(root, manifest.icon));
		// PNG signature, then the IHDR chunk: its length and type follow the
		// signature, and width and height are big-endian 32-bit integers.
		expect(png.subarray(0, 8).toString('hex')).toBe('89504e470d0a1a0a');
		expect(png.subarray(12, 16).toString('latin1')).toBe('IHDR');
		const width = png.readUInt32BE(16);
		const height = png.readUInt32BE(20);

		expect(width).toBeGreaterThanOrEqual(128);
		expect(width).toBe(height);
	});

	it('gives the Activity Bar a 24x24 glyph made of shapes', () => {
		const [container] = manifest.contributes.viewsContainers.activitybar;

		expect(container.icon).toBe('media/ATLAS.svg');
		const svg = readFileSync(join(root, container.icon), 'utf8');

		expect(svg).toContain('viewBox="0 0 24 24"');
		expect(svg).toContain('width="24" height="24"');
		// The mask keeps the alpha channel only, so a bitmap or a text run in
		// this file would render as nothing at all.
		expect(svg).not.toContain('<image');
		expect(svg).not.toContain('<text');
	});
});

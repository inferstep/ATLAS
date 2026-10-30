// The failure: "Please list the files in the directory you are in." returned
// a green chip, a duration, and no files. The proxy had sent the listing in
// `data` and the extension dropped it.

import { describe, expect, it } from 'vitest';
import { summarizeToolResult } from '../src/session/toolSummary';

/** tool_result as the proxy sends it: the tool's own output nested in `data`. */
function payload(data: unknown) {
	return { tool: 'list_directory', success: true, data };
}

describe('summarizeToolResult', () => {
	it('answers a directory listing with the entries', () => {
		const s = summarizeToolResult('list_directory', payload({
			entries: [
				{ name: 'app.py', type: 'file' },
				{ name: 'src', type: 'dir' },
			],
		}))!;
		expect(s).toContain('2 entries');
		expect(s).toContain('app.py');
		expect(s).toContain('src/'); // directories marked
	});

	it('caps a large listing instead of flooding the chip', () => {
		const entries = Array.from({ length: 40 }, (_, i) => ({ name: `f${i}.py`, type: 'file' }));
		const s = summarizeToolResult('list_directory', payload({ entries }))!;
		expect(s).toContain('40 entries');
		expect(s).toContain('+28 more');
		expect(s.split('\n')).toHaveLength(1);
	});

	it.each([
		// Says so rather than rendering nothing.
		['an empty directory', 'empty directory', 'list_directory', payload({ entries: [] })],
		['three search matches', '3 matches', 'search_files', payload({ matches: [1, 2, 3] })],
		['one search match', '1 match', 'search_files', payload({ matches: [1] })],
		['no search matches', 'no matches', 'search_files', payload({ matches: [] })],
		// The generic keys the TUI already uses.
		['command output', 'ok', 'run_command', payload({ stdout: 'ok' })],
		['file content', '3 lines', 'read_file', payload({ content: 'a\nb\nc' })],
		// Degrades to the old chip rather than inventing a summary.
		['a shape it does not know', undefined, 'future_tool', payload({ weird: true })],
		['no payload', undefined, 'x', undefined],
		['blank output', undefined, 'x', payload({ stdout: '   ' })],
	])('summarizes %s as %j', (_case, summary, tool, result) => {
		expect(summarizeToolResult(tool, result)).toBe(summary);
	});
});

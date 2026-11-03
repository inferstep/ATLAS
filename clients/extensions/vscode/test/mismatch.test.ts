// Unit tests for the workspace-mismatch heuristic: per-kind verdicts,
// path extraction, the detector's FIFO matching, one-shot warning, and
// that failed results / inconclusive verdicts never fire.

import { describe, expect, it } from 'vitest';
import {
	MismatchDetector,
	fileOpPaths,
	fileOpVerdict,
	type FileOpKind,
	type PathStat,
	type Verdict,
} from '../src/workspace/mismatch';

const absent: PathStat = { exists: false };
const at = (mtimeMs: number): PathStat => ({ exists: true, mtimeMs });

describe('fileOpPaths', () => {
	it.each([
		['write_file', { path: 'a.py', content: 'x' }, { primary: 'a.py' }],
		['edit_file', { path: 'b.py' }, { primary: 'b.py' }],
		['structural_edit', { path: 'c.py' }, { primary: 'c.py' }],
		['delete_file', { path: 'd.py' }, { primary: 'd.py' }],
		['move_file', { source: 'a.py', destination: 'lib/a.py' }, { primary: 'a.py', secondary: 'lib/a.py' }],
		// Not a file-op tool, or malformed args.
		['run_command', { command: 'ls' }, undefined],
		['read_file', { path: 'a.py' }, undefined],
		['write_file', { path: 42 }, undefined],
		['move_file', { source: 'a.py' }, undefined],
		['write_file', null, undefined],
	])('%s %j touches %j', (tool, args, paths) => {
		expect(fileOpPaths(tool, args)).toEqual(paths);
	});
});

describe('fileOpVerdict', () => {
	it.each<[FileOpKind, string, Verdict, PathStat, PathStat]>([
		['write', 'a new file appearing', 'match', absent, at(2)],
		['write', 'the file staying absent', 'mismatch', absent, absent],
		['write', 'an overwrite with a bumped mtime', 'match', at(1), at(2)],
		// Coarse-mtime filesystems can stamp a real change with the same
		// time — never call that a wrong mount.
		['write', 'an overwrite with an equal mtime', 'inconclusive', at(1), at(1)],
		['edit', 'a changed mtime', 'match', at(1), at(2)],
		['edit', 'an equal mtime', 'inconclusive', at(1), at(1)],
		['edit', 'the file vanishing', 'mismatch', at(1), absent],
		// A file never present locally is an odd path, not mount proof.
		['edit', 'a file never present locally that appears', 'inconclusive', absent, at(2)],
		['edit', 'a file never present locally', 'inconclusive', absent, absent],
		['delete', 'the file disappearing', 'match', at(1), absent],
		['delete', 'the file persisting', 'mismatch', at(1), at(1)],
		['delete', 'a file absent before', 'inconclusive', absent, absent],
		// Missing mtimes count as changed (conservative: no false mismatch).
		['edit', 'a missing mtime before', 'match', { exists: true }, at(2)],
		['write', 'a missing mtime after', 'match', at(1), { exists: true }],
	])('%s: %s gives %s', (kind, _case, verdict, pre, post) => {
		expect(fileOpVerdict(kind, pre, post)).toBe(verdict);
	});

	it.each<[string, Verdict, PathStat, PathStat, PathStat, PathStat]>([
		['source to destination', 'match', at(1), absent, absent, at(2)],
		['the source unmoved', 'mismatch', at(1), at(1), absent, absent],
		['nothing there at all', 'inconclusive', absent, absent, absent, absent],
		// Both present (copy-like state / same-name overwrite) — don't guess.
		['both present', 'inconclusive', at(1), at(1), absent, at(2)],
	])('move: %s gives %s', (_case, verdict, pre, post, preDest, postDest) => {
		expect(fileOpVerdict('move', pre, post, preDest, postDest)).toBe(verdict);
	});
});

/** Detector driven by a scripted stat table keyed by path; each recordToolResult
 * consults the CURRENT table so tests mutate it between call and result. */
function detector(stats: Map<string, PathStat>) {
	let fired = 0;
	const d = new MismatchDetector({
		stat: async (p) => stats.get(p) ?? absent,
		onMismatch: () => {
			fired += 1;
		},
		settleMs: 0,
	});
	return { d, firedCount: () => fired };
}

describe('MismatchDetector', () => {
	it('does not fire when the write shows up locally', async () => {
		const stats = new Map<string, PathStat>();
		const { d, firedCount } = detector(stats);
		await d.recordToolCall('write_file', { path: 'a.py', content: 'x' });
		stats.set('a.py', at(1)); // file appeared
		await d.recordToolResult('write_file', true);
		expect(firedCount()).toBe(0);
	});

	it('fires once when a successful write never lands locally', async () => {
		const stats = new Map<string, PathStat>();
		const { d, firedCount } = detector(stats);
		await d.recordToolCall('write_file', { path: 'a.py', content: 'x' });
		await d.recordToolResult('write_file', true);
		expect(firedCount()).toBe(1);
		// A second contradiction stays silent — the warning is one-shot.
		await d.recordToolCall('write_file', { path: 'b.py', content: 'y' });
		await d.recordToolResult('write_file', true);
		expect(firedCount()).toBe(1);
	});

	it('ignores failed results (nothing was applied to verify)', async () => {
		const stats = new Map<string, PathStat>();
		const { d, firedCount } = detector(stats);
		await d.recordToolCall('write_file', { path: 'a.py', content: 'x' });
		await d.recordToolResult('write_file', false);
		expect(firedCount()).toBe(0);
	});

	it('matches results to calls FIFO per tool name', async () => {
		const stats = new Map<string, PathStat>([
			['a.py', at(1)],
			['b.py', at(1)],
		]);
		const { d, firedCount } = detector(stats);
		await d.recordToolCall('edit_file', { path: 'a.py' });
		await d.recordToolCall('edit_file', { path: 'b.py' });
		stats.set('a.py', at(2)); // first edit landed
		stats.delete('b.py'); // second target vanished locally — clear contradiction
		await d.recordToolResult('edit_file', true); // a.py -> match
		expect(firedCount()).toBe(0);
		await d.recordToolResult('edit_file', true); // b.py gone -> mismatch
		expect(firedCount()).toBe(1);
	});

	it.each([
		// Coarse-mtime file system: the edit landed but the mtime did not move.
		['an edited file keeps an equal mtime', new Map([['a.py', at(1)]])],
		['the edited file was never in this workspace', new Map<string, PathStat>()],
	])('stays silent on an inconclusive verdict: %s', async (_case, stats) => {
		const { d, firedCount } = detector(stats);
		await d.recordToolCall('edit_file', { path: 'a.py' });
		await d.recordToolResult('edit_file', true); // stat unchanged
		expect(firedCount()).toBe(0);
	});

	it('verifies move_file source/destination', async () => {
		const stats = new Map<string, PathStat>([['a.py', at(1)]]);
		const { d, firedCount } = detector(stats);
		await d.recordToolCall('move_file', { source: 'a.py', destination: 'lib/a.py' });
		stats.delete('a.py');
		stats.set('lib/a.py', at(2));
		await d.recordToolResult('move_file', true);
		expect(firedCount()).toBe(0);

		await d.recordToolCall('move_file', { source: 'lib/a.py', destination: 'lib/b.py' });
		// nothing moved locally
		await d.recordToolResult('move_file', true);
		expect(firedCount()).toBe(1);
	});

	it("a denied call's failed result consumes its pending op, so the next allowed call pairs with its own", async () => {
		const stats = new Map<string, PathStat>([
			['a.py', at(1)],
			['b.py', at(1)],
		]);
		const { d, firedCount } = detector(stats);
		// Denied edit: tool_call captured pre-op state; the proxy follows the
		// denial with a tool_result whose success is false.
		await d.recordToolCall('edit_file', { path: 'a.py' });
		await d.recordToolResult('edit_file', false);
		// Allowed edit of b.py: must pair with ITS pre-op state, not a.py's.
		// a.py is gone by then, so a pairing with a.py's state would warn.
		await d.recordToolCall('edit_file', { path: 'b.py' });
		stats.set('b.py', at(2));
		stats.delete('a.py');
		await d.recordToolResult('edit_file', true);
		expect(firedCount()).toBe(0);
	});

	it('reset drops pending pre-op state', async () => {
		const stats = new Map<string, PathStat>();
		const { d, firedCount } = detector(stats);
		await d.recordToolCall('write_file', { path: 'a.py', content: 'x' });
		d.reset();
		await d.recordToolResult('write_file', true); // no pending op to check
		expect(firedCount()).toBe(0);
	});
});

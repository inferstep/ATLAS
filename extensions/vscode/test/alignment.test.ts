// The failure this exists for: the proxy was bound to ~/snake-game while VS
// Code had ~/demo2 open. Every tool call was truthful and useless — the agent
// listed and edited a directory the user was not looking at. It took four
// rounds of manual `atlas workspace align` to notice each time.

import { describe, expect, it, vi } from 'vitest';
import { checkAlignment, WorkspaceClient } from '../src/workspace/alignment';

describe('checkAlignment', () => {
	it('maps the `atlas workspace` exit code to a state', async () => {
		// 0 = this directory is covered by the bind, 1 = it is not.
		await expect(checkAlignment('/x', async () => 0)).resolves.toBe('aligned');
		await expect(checkAlignment('/x', async () => 1)).resolves.toBe('misaligned');
	});

	it('is unknown when atlas is not installed', async () => {
		// A missing CLI must never nag a user whose setup is otherwise fine.
		const spawnFailure = () => Promise.reject(new Error('ENOENT'));
		await expect(checkAlignment('/x', spawnFailure)).resolves.toBe('unknown');
	});

	it('is unknown on any other exit code', async () => {
		// e.g. docker unreachable — do not claim misalignment we cannot see.
		await expect(checkAlignment('/x', async () => 2)).resolves.toBe('unknown');
		await expect(checkAlignment('/x', async () => 127)).resolves.toBe('unknown');
	});

	it('runs in the folder being checked', async () => {
		const run = vi.fn(async () => 0);
		await checkAlignment('/home/isaac/demo2', run);
		expect(run).toHaveBeenCalledWith('atlas workspace', '/home/isaac/demo2');
	});

	describe('with an injected client', () => {
		it('trusts the endpoint when misaligned, without touching the CLI', async () => {
			const client: WorkspaceClient = {
				getWorkspace: async () => ({
					project_dir: '/home/HiIsaac/sudoku-solver',
					working_dir: '/workspace',
					containerized: true
				})
			};

			const run = vi.fn(async () => 0); // would say 'aligned' if it were ever called
			const result = await checkAlignment('/home/HiIsaac/tic-tac-toe', run, client);

			expect(result).toBe('misaligned');
			expect(run).not.toHaveBeenCalled();
		});

		it('endpoint unreachable, CLI fallback', async () => {
			const client: WorkspaceClient = {
				getWorkspace: async () => null,
			};
			const run = vi.fn(async () => 0);

			const result = await checkAlignment('/home/HiIsaac/wanna-play', run, client);

			expect(result).toBe('aligned');
			expect(run).toHaveBeenCalledWith('atlas workspace', '/home/HiIsaac/wanna-play');
		});

		it(`is 'unknown' when both the endpoint and the CLI are unavailable`, async () => {
			const client: WorkspaceClient = {
				getWorkspace: async () => null
			};
			const run = vi.fn(() => Promise.reject(new Error('ENOENT')));
			const result = await checkAlignment('/home/HiIsaac/assassins', run, client);

			expect(result).toBe('unknown');
			expect(run).toHaveBeenCalled();
		});

		it(`relative host path falls through to the CLI instead of a bogus mismatch`, async () => {
			const client: WorkspaceClient = {
				getWorkspace: async () => ({
					project_dir: '.',
					working_dir: '/workspace',
					containerized: true,
				}),
			};
			const run = vi.fn(async () => 1); // CLI says misaligned

			const result = await checkAlignment('/home/HiIsaac/relative-path', run, client);

			expect(result).toBe('misaligned');
			expect(run).toHaveBeenCalledWith('atlas workspace', '/home/HiIsaac/relative-path');
		});

		// The endpoint is authoritative for 'no' only. It reports the proxy's
		// own bind and knows nothing about the sandbox's, so a proxy-side
		// match cannot conclude 'aligned' on its own — `atlas workspace`
		// compares both binds and is the only thing that sees a SPLIT. An
		// early return here would report the split-brain case as aligned,
		// which is precisely the failure this module exists to catch.
		it('proxy-side match still consults the CLI, which sees the sandbox bind', async () => {
			const client: WorkspaceClient = {
				getWorkspace: async () => ({
					project_dir: '/home/HiIsaac/tic-tac-toe',
					working_dir: '/workspace',
					containerized: true,
				}),
			};
			const run = vi.fn(async () => 1); // sandbox bind has drifted: SPLIT

			const result = await checkAlignment('/home/HiIsaac/tic-tac-toe', run, client);

			expect(result).toBe('misaligned');
			expect(run).toHaveBeenCalledWith('atlas workspace', '/home/HiIsaac/tic-tac-toe');
		});

		it('a folder nested inside the bind is covered, not a mismatch', async () => {
			// `covers` is containment, not equality — opening a subdirectory
			// of the bound project is aligned, and must not fast-path to
			// 'misaligned' before the CLI is asked.
			const client: WorkspaceClient = {
				getWorkspace: async () => ({
					project_dir: '/home/HiIsaac/monorepo',
					working_dir: '/workspace',
					containerized: true,
				}),
			};
			const run = vi.fn(async () => 0); // both binds agree

			const result = await checkAlignment('/home/HiIsaac/monorepo/packages/api', run, client);

			expect(result).toBe('aligned');
			expect(run).toHaveBeenCalledWith('atlas workspace', '/home/HiIsaac/monorepo/packages/api');
		});
	});
});

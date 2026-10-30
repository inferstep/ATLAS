// The failure this exists for: the proxy was bound to ~/snake-game while VS
// Code had ~/demo2 open. Every tool call was truthful and useless — the agent
// listed and edited a directory the user was not looking at. It took four
// rounds of manual `atlas workspace align` to notice each time.

import { describe, expect, it, vi } from 'vitest';
import { checkAlignment, type AlignState, type Runner, type WorkspaceClient } from '../src/workspace/alignment';

/** What GET /workspace answers when the proxy is bound to `projectDir`. */
const boundTo = (projectDir: string) => ({ project_dir: projectDir, working_dir: '/workspace', containerized: true });

describe('checkAlignment', () => {
	it.each<[string, AlignState, Runner]>([
		// `atlas workspace`: 0 = this directory is covered by the bind, 1 = it is not.
		['exit code 0', 'aligned', async () => 0],
		['exit code 1', 'misaligned', async () => 1],
		// Any other exit code, e.g. docker unreachable: do not claim a
		// misalignment we cannot see.
		['exit code 2', 'unknown', async () => 2],
		['exit code 127', 'unknown', async () => 127],
		// A missing CLI must never nag a user whose setup is otherwise fine.
		['atlas not installed', 'unknown', () => Promise.reject(new Error('ENOENT'))],
	])('runs `atlas workspace` in the folder: %s is %s', async (_case, state, cli) => {
		const run = vi.fn(cli);
		await expect(checkAlignment('/home/isaac/demo2', run)).resolves.toBe(state);
		expect(run).toHaveBeenCalledWith('atlas workspace', '/home/isaac/demo2');
	});

	describe('with an injected client', () => {
		it('trusts the endpoint when misaligned, without touching the CLI', async () => {
			const client: WorkspaceClient = { getWorkspace: async () => boundTo('/home/HiIsaac/sudoku-solver') };
			const run = vi.fn(async () => 0); // would say 'aligned' if it were ever called

			const result = await checkAlignment('/home/HiIsaac/tic-tac-toe', run, client);

			expect(result).toBe('misaligned');
			expect(run).not.toHaveBeenCalled();
		});

		// In every case below the CLI decides, and it runs in the open folder.
		it.each([
			['endpoint unreachable, CLI fallback',
				{ workspace: null, folder: '/home/HiIsaac/wanna-play', cli: async () => 0, state: 'aligned' }],
			[`is 'unknown' when both the endpoint and the CLI are unavailable`,
				{ workspace: null, folder: '/home/HiIsaac/assassins', cli: () => Promise.reject(new Error('ENOENT')), state: 'unknown' }],
			['relative host path falls through to the CLI instead of a bogus mismatch',
				{ workspace: boundTo('.'), folder: '/home/HiIsaac/relative-path', cli: async () => 1, state: 'misaligned' }],
			// The endpoint is authoritative for 'no' only. It reports the proxy's
			// own bind and knows nothing about the sandbox's, so a proxy-side
			// match cannot conclude 'aligned' on its own — `atlas workspace`
			// compares both binds and is the only thing that sees a SPLIT. An
			// early return here would report the split-brain case as aligned,
			// which is precisely the failure this module exists to catch. Here
			// the sandbox bind has drifted, so the CLI says 1.
			['proxy-side match still consults the CLI, which sees the sandbox bind',
				{ workspace: boundTo('/home/HiIsaac/tic-tac-toe'), folder: '/home/HiIsaac/tic-tac-toe', cli: async () => 1, state: 'misaligned' }],
			// `covers` is containment, not equality — opening a subdirectory of
			// the bound project is aligned, and must not fast-path to
			// 'misaligned' before the CLI is asked.
			['a folder nested inside the bind is covered, not a mismatch',
				{ workspace: boundTo('/home/HiIsaac/monorepo'), folder: '/home/HiIsaac/monorepo/packages/api', cli: async () => 0, state: 'aligned' }],
		])('%s', async (_name, { workspace, folder, cli, state }) => {
			const client: WorkspaceClient = { getWorkspace: async () => workspace };
			const run = vi.fn(cli);

			await expect(checkAlignment(folder, run, client)).resolves.toBe(state);
			expect(run).toHaveBeenCalledWith('atlas workspace', folder);
		});
	});
});

// Is the proxy's bind the folder the user has open?
//
// When it is not, every tool call is truthful and useless: the agent lists,
// reads and edits a directory the user is not looking at. The passive
// heuristic in mismatch.ts only notices AFTER an edit lands invisibly, which
// costs a turn to learn.
//
// `atlas workspace` already answers this exactly — exit 0 when the open folder
// is covered by the bind, 1 when it is not — so this asks it rather than
// reimplementing the docker inspect in TypeScript.
//
// Deliberately vscode-free (the runner is injected) so it runs under plain
// vitest, same as mismatch.ts.

import { exec } from 'node:child_process';
import * as path from 'node:path';
import * as fs from 'node:fs';

export type AlignState = 'aligned' | 'misaligned' | 'unknown';

/** Runs a command in cwd and resolves its exit code. Rejects if it cannot spawn. */
export type Runner = (command: string, cwd: string) => Promise<number>;

export interface WorkspaceClient {
	getWorkspace(): Promise<{ project_dir: string, working_dir: string, containerized: boolean } | null>;
}

export function defaultRunner(command: string, cwd: string): Promise<number> {
	return new Promise((resolve, reject) => {
		const child = exec(command, { cwd, timeout: 15_000 }, (err) => {
			if (err && typeof err.code !== 'number') {
				reject(err); // spawn failure: atlas not on PATH
				return;
			}
			resolve(err ? (err.code as number) : 0);
		});
		child.on('error', reject);
	});
}

function realPathOrResolve(p: string): string {
	try {
		return fs.realpathSync(p);
	} catch {
		return path.resolve(p);
	}
}

/** Port of workspace.py's _covers - true when `target` is inside `bound`
 * Tries realpath first (resolves symlinks, matches Python original).
 * Falls back to path.resolve if the path doesn't exist yet (e.g. test
 * fixtures) - realpathSync throws on missing paths, so we can't rely on
 * it alone and stay testable without a real filesystem. */
function covers(bound: string, target: string): boolean {
	if (!bound) {
		return false;
	}
	const rel = path.relative(realPathOrResolve(bound), realPathOrResolve(target));
	if (path.isAbsolute(rel)) {
		return false; // different roots/drives - Python's ValueError case
	}
	return rel === '' || !rel.startsWith('..');
}

/** 'unknown' when the CLI is missing or fails in a way we cannot interpret — a
 * user without `atlas` on PATH must never be nagged about alignment. */
export async function checkAlignment(
	cwd: string,
	run: Runner = defaultRunner,
	client?: WorkspaceClient,
): Promise<AlignState> {
	if (client) {
		const ws = await client.getWorkspace();
		if (ws && ws.project_dir && path.isAbsolute(ws.project_dir)
			&& !covers(ws.project_dir, cwd)) {
			return 'misaligned';   // fast negative, no CLI needed
		}
		// aligned-per-proxy still needs the CLI: only it sees the sandbox bind
	}

	try {
		const code = await run('atlas workspace', cwd);
		if (code === 0) {
			return 'aligned';
		}
		return code === 1 ? 'misaligned' : 'unknown';
	} catch {
		return 'unknown';
	}
}

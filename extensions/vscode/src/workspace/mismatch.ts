// Passive workspace-mismatch heuristic. The proxy applies file ops to its
// own bind-mounted workspace (ATLAS_WORKSPACE_DIR) and no endpoint exposes
// that path, so the extension cannot verify the mount directly. Instead:
// after a file-op tool_result reports success, wait a short settle delay,
// stat the target path inside the local workspace folder, and compare with
// a pre-op stat captured at tool_call time:
//
//   write_file  -> the file must exist (an overwrite with an unchanged
//                  mtime is inconclusive, not proof of a wrong mount)
//   edit_file / structural_edit -> the file must exist; unchanged mtime is
//                  inconclusive (coarse-mtime filesystems)
//   delete_file -> the file must be absent
//   move_file   -> destination present, source absent
//
// No probe writes, ever. A single 'mismatch' verdict is enough to warn
// once ("proxy likely mounted elsewhere"); 'inconclusive' verdicts are
// dropped silently — mtime granularity, unsaved editors, and absolute
// proxy-side paths make false positives too easy otherwise.
//
// Deliberately vscode-free (stat is injected) so it runs under plain vitest.

/** File-op tools the heuristic watches, mapped to how their effect is
 * verified locally. */
const FILE_OP_KINDS: Record<string, FileOpKind> = {
	write_file: 'write',
	edit_file: 'edit',
	structural_edit: 'edit',
	insert_after: 'edit',
	replace_lines: 'edit',
	delete_file: 'delete',
	move_file: 'move',
};

export type FileOpKind = 'write' | 'edit' | 'delete' | 'move';

export type Verdict = 'match' | 'mismatch' | 'inconclusive';

/** Result of stat-ing a workspace-relative path. */
export interface PathStat {
	exists: boolean;
	mtimeMs?: number;
}

/** Injected stat: resolves a proxy-relative path inside the local
 * workspace folder and stats it. Never throws — absent maps to
 * {exists: false}. */
export type StatFn = (relativePath: string) => Promise<PathStat>;

/** Paths a file-op call touches, from its args. undefined = not a file op
 * or malformed args. */
export function fileOpPaths(tool: string, args: unknown): { primary: string; secondary?: string } | undefined {
	if (!(tool in FILE_OP_KINDS) || typeof args !== 'object' || args === null) {
		return undefined;
	}
	const record = args as Record<string, unknown>;
	if (tool === 'move_file') {
		// MoveFileInput: source + destination (proxy/types.go).
		const source = record['source'];
		const destination = record['destination'];
		return typeof source === 'string' && typeof destination === 'string'
			? { primary: source, secondary: destination }
			: undefined;
	}
	const path = record['path'];
	return typeof path === 'string' ? { primary: path } : undefined;
}

/** Compare pre-op and post-op stats for one successful file op. */
export function fileOpVerdict(
	kind: FileOpKind,
	pre: PathStat,
	post: PathStat,
	/** move_file only: destination stats. */
	preDest?: PathStat,
	postDest?: PathStat,
): Verdict {
	switch (kind) {
		case 'write':
			if (!post.exists) {
				return 'mismatch';
			}
			if (!pre.exists) {
				return 'match'; // file appeared where the proxy said it wrote
			}
			return mtimeVerdict(pre, post);
		case 'edit':
			if (!pre.exists) {
				// The proxy edited a file this workspace never had — suspicious,
				// but read_file would have had to see it, so this is more likely
				// an absolute/odd path than a wrong mount. Don't warn on it.
				return 'inconclusive';
			}
			if (!post.exists) {
				return 'mismatch';
			}
			return mtimeVerdict(pre, post);
		case 'delete':
			if (!pre.exists) {
				return 'inconclusive'; // absent before and (presumably) after
			}
			return post.exists ? 'mismatch' : 'match';
		case 'move':
			if (!pre.exists || preDest === undefined || postDest === undefined) {
				return 'inconclusive';
			}
			if (postDest.exists && !post.exists) {
				return 'match';
			}
			return post.exists && !postDest.exists ? 'mismatch' : 'inconclusive';
	}
}

/** mtime comparison for overwrite/edit kinds. Equal mtimes are
 * 'inconclusive', not 'mismatch': coarse-mtime filesystems (FAT, some
 * network mounts) can stamp a real change with the same time — exactly
 * the granularity caveat in the header note. Missing mtimes can't tell
 * either way. */
function mtimeVerdict(pre: PathStat, post: PathStat): Verdict {
	if (pre.mtimeMs === undefined || post.mtimeMs === undefined) {
		return 'match'; // can't tell — conservative: no false mismatch
	}
	if (post.mtimeMs !== pre.mtimeMs) {
		return 'match';
	}
	return 'inconclusive';
}

/** Pre-op stats captured at tool_call time, FIFO per tool name (same
 * matching scheme as chatView's snapshots — tool_result carries only the
 * tool name). */
interface PendingOp {
	kind: FileOpKind;
	primary: string;
	secondary?: string;
	pre: PathStat;
	preSecondary?: PathStat;
}

export interface MismatchDetectorOptions {
	stat: StatFn;
	/** Called once, on the first 'mismatch' verdict. */
	onMismatch: () => void;
	/** Delay before the post-op stat, giving the bind mount and file
	 * watcher time to settle. Tests pass 0. Default 500. */
	settleMs?: number;
}

/** Session-long detector: feed it tool_call and tool_result events; it
 * warns (once) via onMismatch when local disk state contradicts a
 * successful file op. */
export class MismatchDetector {
	private readonly stat: StatFn;
	private readonly onMismatch: () => void;
	private readonly settleMs: number;
	private readonly pending = new Map<string, PendingOp[]>();
	private fired = false;

	constructor(options: MismatchDetectorOptions) {
		this.stat = options.stat;
		this.onMismatch = options.onMismatch;
		this.settleMs = options.settleMs ?? 500;
	}

	/** Capture pre-op state at tool_call time. No-op for non-file-op tools. */
	async recordToolCall(tool: string, args: unknown): Promise<void> {
		if (this.fired) {
			return;
		}
		const paths = fileOpPaths(tool, args);
		if (paths === undefined) {
			return;
		}
		const op: PendingOp = {
			kind: FILE_OP_KINDS[tool],
			primary: paths.primary,
			secondary: paths.secondary,
			pre: await this.stat(paths.primary),
			preSecondary: paths.secondary === undefined ? undefined : await this.stat(paths.secondary),
		};
		const queue = this.pending.get(tool) ?? [];
		queue.push(op);
		this.pending.set(tool, queue);
	}

	/** A permission_denied consumed a tool_call that will never get a
	 * tool_result (the proxy skips execution entirely) — drop its pre-op
	 * state so the next allowed call of the same tool pairs correctly. */
	recordDenied(tool: string): void {
		const queue = this.pending.get(tool);
		if (queue && queue.length > 0) {
			queue.shift();
		}
	}

	/** Check a tool_result against its captured pre-op state. Failed results
	 * only consume the queue entry — the proxy made no change to verify. */
	async recordToolResult(tool: string, success: boolean): Promise<void> {
		const queue = this.pending.get(tool);
		const op = queue && queue.length > 0 ? queue.shift() : undefined;
		if (!op || !success || this.fired) {
			return;
		}
		if (this.settleMs > 0) {
			await new Promise((resolve) => setTimeout(resolve, this.settleMs));
		}
		const post = await this.stat(op.primary);
		const postSecondary = op.secondary === undefined ? undefined : await this.stat(op.secondary);
		const verdict = fileOpVerdict(op.kind, op.pre, post, op.preSecondary, postSecondary);
		if (verdict === 'mismatch' && !this.fired) {
			this.fired = true;
			this.pending.clear();
			this.onMismatch();
		}
	}

	/** Drop pending pre-op state (turn ended with results unconsumed). */
	reset(): void {
		this.pending.clear();
	}
}

// One-line human summary of a tool_result payload, for the tool chip.
//
// The proxy sends the tool's real output in `data`, and the extension used to
// drop it: asking "list the files" produced a green check, a duration, and no
// files. The answer to a read-only request IS the tool output, so a chip that
// shows only that the call succeeded answers nothing.
//
// Shapes are per-tool and the proxy is the source of truth (proxy/tool*.go
// output structs). Anything unrecognised falls back to the generic keys the
// TUI already looks for, so a new tool degrades to "ran" rather than lying.

/** Entry as list_directory returns it. */
interface DirEntry {
	name: string;
	type?: string;
}

function asRecord(value: unknown): Record<string, unknown> | undefined {
	return typeof value === 'object' && value !== null
		? (value as Record<string, unknown>)
		: undefined;
}

/** Join names, capped, with a "+N more" tail so a big directory stays one line. */
function names(entries: DirEntry[], limit: number): string {
	const shown = entries.slice(0, limit).map((e) => (e.type === 'dir' ? `${e.name}/` : e.name));
	const rest = entries.length - shown.length;
	return rest > 0 ? `${shown.join(', ')} (+${rest} more)` : shown.join(', ');
}

export function summarizeToolResult(tool: string, data: unknown): string | undefined {
	const d = asRecord(data);
	if (!d) {
		return undefined;
	}
	// The proxy nests the tool's own output under `data`.
	const inner = asRecord(d.data) ?? d;

	if (Array.isArray(inner.entries)) {
		const entries = inner.entries as DirEntry[];
		return entries.length === 0
			? 'empty directory'
			: `${entries.length} entries: ${names(entries, 12)}`;
	}
	if (Array.isArray(inner.matches)) {
		const n = (inner.matches as unknown[]).length;
		return n === 0 ? 'no matches' : `${n} match${n === 1 ? '' : 'es'}`;
	}
	for (const key of ['summary', 'stdout', 'content', 'message'] as const) {
		const v = inner[key];
		if (typeof v === 'string' && v.trim() !== '') {
			const lines = v.split('\n').length;
			return lines > 1 ? `${lines} lines` : v.trim();
		}
	}
	return undefined;
}

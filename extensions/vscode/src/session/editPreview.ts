// Client-side prediction of what a file-writing tool call will do, computed
// at permission time from the CURRENT local file. Needed because the diff has
// to be shown BEFORE the user allows the call — and for structural_edit even the
// result carries no new content (proxy/types.go StructuralEditOutput is ok/selector/
// language/byte counts only), so the post-state can only be predicted here.
//
// Accuracy contract per tool:
//   - write_file: exact (disk-or-empty vs `content`).
//   - edit_file: exact when old_str occurs (first occurrence, or all with
//     replace_all); otherwise a snippet diff of old_str vs new_str plus a
//     note — the proxy will likely reject that call anyway.
//   - structural_edit: best-effort regex/indent splice for python function:NAME /
//     class:NAME (decorator-aware) and a naive top-level <tag> scan for HTML.
//     Tree-sitter is the server-side authority (v3-service), so every
//     structural_edit prediction is labeled approximate. The exact view comes later:
//     chatView snapshots the file at tool_call time and diffs snapshot vs
//     on-disk after the tool_result.
//
// Deliberately vscode-free so it runs under plain vitest.

/** The tools whose permission prompts and tool chips get diffs.
 * move_file / delete_file / run_command are notification-only. */
export const FILE_EDIT_TOOLS = new Set([
	'write_file', 'edit_file', 'structural_edit', 'insert_after', 'replace_lines',
]);

export interface EditPrediction {
	/** Target path exactly as the model sent it (proxy-workspace-relative). */
	path: string;
	/** 'file': left/right are whole-file states. 'snippet': left/right are
	 * just old_str/new_str (edit_file whose old_str is absent). */
	kind: 'file' | 'snippet';
	left: string;
	right: string;
	/** True when the prediction is a best-effort guess rather than the exact
	 * post-state (all structural_edit predictions, and edit_file snippet mode). */
	approximate: boolean;
	note?: string;
}

function stringField(args: unknown, key: string): string | undefined {
	if (typeof args !== 'object' || args === null) {
		return undefined;
	}
	const value = (args as Record<string, unknown>)[key];
	return typeof value === 'string' ? value : undefined;
}

/** Path a file-edit tool call targets, or undefined for other tools /
 * malformed args. Used to read the local file and to key snapshots. */
export function editTargetPath(tool: string, args: unknown): string | undefined {
	if (!FILE_EDIT_TOOLS.has(tool)) {
		return undefined;
	}
	return stringField(args, 'path');
}

/** Predict the post-edit file state. `current` is the local file content, or
 * undefined when the file does not exist in this workspace. Returns undefined
 * for non-edit tools or args missing required fields. */
export function predictEdit(tool: string, args: unknown, current: string | undefined): EditPrediction | undefined {
	const path = editTargetPath(tool, args);
	if (path === undefined) {
		return undefined;
	}
	switch (tool) {
		case 'write_file': {
			const content = stringField(args, 'content');
			if (content === undefined) {
				return undefined;
			}
			return {
				path,
				kind: 'file',
				left: current ?? '',
				right: content,
				approximate: false,
				note: current === undefined ? 'new file' : undefined,
			};
		}
		case 'edit_file': {
			const oldStr = stringField(args, 'old_str');
			const newStr = stringField(args, 'new_str');
			if (oldStr === undefined || newStr === undefined) {
				return undefined;
			}
			return predictEditFile(path, current, oldStr, newStr, isReplaceAll(args));
		}
		case 'structural_edit': {
			const selector = stringField(args, 'selector');
			const content = stringField(args, 'content');
			if (selector === undefined || content === undefined) {
				return undefined;
			}
			return predictStructuralEdit(path, current, selector, content);
		}
		case 'insert_after': {
			// The only edit tool whose prediction is exact. It names a line
			// number rather than text to reproduce, so there is nothing to
			// match and nothing to guess: splice after that line.
			const content = stringField(args, 'content');
			const line = numberField(args, 'line');
			if (content === undefined || line === undefined || current === undefined) {
				return undefined;
			}
			return predictInsertAfter(path, current, line, content);
		}
		case 'replace_lines': {
			// Also exact: the range is line numbers, not text to reproduce.
			const content = stringField(args, 'content');
			const start = numberField(args, 'start_line');
			const end = numberField(args, 'end_line');
			if (content === undefined || start === undefined || end === undefined
				|| current === undefined) {
				return undefined;
			}
			return predictReplaceLines(path, current, start, end, content);
		}
		default:
			return undefined;
	}
}

/** Replace 1-based lines `start`..`end` (inclusive) with `content`. Mirrors
 * the proxy: an out-of-range or inverted span is not previewable. */
function predictReplaceLines(
	path: string, current: string, start: number, end: number, content: string,
): EditPrediction | undefined {
	const hadTrailingNewline = current.endsWith('\n');
	const lines = current.split('\n');
	if (hadTrailingNewline) {
		lines.pop();
	}
	if (start < 1 || end < start || end > lines.length) {
		return undefined;
	}
	const replacement = content.split('\n');
	if (content.endsWith('\n')) {
		replacement.pop();
	}
	const next = [...lines.slice(0, start - 1), ...replacement, ...lines.slice(end)];
	return {
		path,
		kind: 'file',
		left: current,
		right: next.join('\n') + (hadTrailingNewline ? '\n' : ''),
		approximate: false,
		note: start === end ? `replaced line ${start}` : `replaced lines ${start}-${end}`,
	};
}

function numberField(args: unknown, key: string): number | undefined {
	if (typeof args !== 'object' || args === null) {
		return undefined;
	}
	const value = (args as Record<string, unknown>)[key];
	return typeof value === 'number' && Number.isInteger(value) ? value : undefined;
}

/** Splice `content` in after 1-based `line`; 0 puts it at the top of the file.
 * A line past the end appends, which is what the proxy does. */
function predictInsertAfter(
	path: string, current: string, line: number, content: string,
): EditPrediction | undefined {
	if (line < 0) {
		return undefined;
	}
	const hadTrailingNewline = current.endsWith('\n');
	const lines = current.split('\n');
	if (hadTrailingNewline) {
		lines.pop(); // split() leaves a trailing '' — not a real line
	}
	const at = Math.min(line, lines.length);
	const inserted = content.split('\n');
	if (content.endsWith('\n')) {
		inserted.pop();
	}
	const next = [...lines.slice(0, at), ...inserted, ...lines.slice(at)];
	return {
		path,
		kind: 'file',
		left: current,
		right: next.join('\n') + (hadTrailingNewline ? '\n' : ''),
		approximate: false,
		note: line === 0 ? 'inserted at top of file' : `inserted after line ${line}`,
	};
}

function isReplaceAll(args: unknown): boolean {
	if (typeof args !== 'object' || args === null) {
		return false;
	}
	return (args as Record<string, unknown>)['replace_all'] === true;
}

function predictEditFile(
	path: string,
	current: string | undefined,
	oldStr: string,
	newStr: string,
	replaceAll: boolean,
): EditPrediction {
	if (current === undefined) {
		return {
			path,
			kind: 'snippet',
			left: oldStr,
			right: newStr,
			approximate: true,
			note: 'target file not found in this workspace — showing old_str vs new_str',
		};
	}
	const index = current.indexOf(oldStr);
	if (oldStr === '' || index === -1) {
		return {
			path,
			kind: 'snippet',
			left: oldStr,
			right: newStr,
			approximate: true,
			note: 'old_str not found in the file — showing old_str vs new_str (the proxy will likely reject this edit)',
		};
	}
	// split/join instead of String.replace: the replacement string must be
	// spliced literally ($& and friends have no meaning here).
	if (replaceAll) {
		return { path, kind: 'file', left: current, right: current.split(oldStr).join(newStr), approximate: false };
	}
	// Without replace_all the proxy requires a UNIQUE match and rejects the
	// call otherwise (proxy/tools.go uniqueness check) — a first-occurrence
	// splice labeled exact would show a diff that can never apply.
	const count = current.split(oldStr).length - 1;
	if (count > 1) {
		return {
			path,
			kind: 'file',
			left: current,
			right: current.slice(0, index) + newStr + current.slice(index + oldStr.length),
			approximate: true,
			note: `old_str matches ${count} locations — the proxy requires a unique match and will reject this edit (showing the first occurrence)`,
		};
	}
	const right = current.slice(0, index) + newStr + current.slice(index + oldStr.length);
	return { path, kind: 'file', left: current, right, approximate: false };
}

function predictStructuralEdit(path: string, current: string | undefined, selector: string, content: string): EditPrediction {
	if (current === undefined) {
		return {
			path,
			kind: 'file',
			left: '',
			right: content,
			approximate: true,
			note: 'target file not found in this workspace — showing the proposed node content only',
		};
	}
	const fn = /^function:([A-Za-z_]\w*)$/.exec(selector.trim());
	const cls = /^class:([A-Za-z_]\w*)$/.exec(selector.trim());
	const tag = /^<([A-Za-z][\w-]*)>$/.exec(selector.trim());
	if (fn || cls) {
		const spliced = splicePythonNode(current, fn ? 'function' : 'class', (fn ?? cls)![1], content);
		if (spliced) {
			return {
				path,
				kind: 'file',
				left: current,
				right: spliced.right,
				approximate: true,
				note: spliced.note ?? 'predicted client-side — tree-sitter runs server-side',
			};
		}
	} else if (tag) {
		let body = content;
		if (tag[1].toLowerCase() === 'html') {
			// Mirror the proxy's <html>-selector quirk: the element replace
			// does not cover a preceding <!DOCTYPE>, so the proxy strips a
			// leading doctype from the content (proxy/tools.go).
			body = stripLeadingDoctype(body);
		}
		const spliced = spliceHtmlElement(current, tag[1], body);
		if (spliced !== undefined) {
			return {
				path,
				kind: 'file',
				left: current,
				right: spliced,
				approximate: true,
				note: 'predicted client-side — tree-sitter runs server-side',
			};
		}
	}
	return {
		path,
		kind: 'file',
		left: current,
		right: content,
		approximate: true,
		note: `selector '${selector}' not matched locally — showing the proposed content against the whole file`,
	};
}

function stripLeadingDoctype(content: string): string {
	const match = /^\s*<!doctype[^>]*>\r?\n?/i.exec(content);
	return match ? content.slice(match[0].length) : content;
}

function escapeRegExp(text: string): string {
	return text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

/** Replace the named python function/class (plus contiguous decorator lines
 * directly above it) with `content`. Indent-scan only — no parser. */
function splicePythonNode(
	source: string,
	kind: 'function' | 'class',
	name: string,
	content: string,
): { right: string; note?: string } | undefined {
	const lines = source.split('\n');
	const head =
		kind === 'function'
			? new RegExp(`^([ \\t]*)(?:async[ \\t]+)?def[ \\t]+${escapeRegExp(name)}[ \\t]*\\(`)
			: new RegExp(`^([ \\t]*)class[ \\t]+${escapeRegExp(name)}[ \\t]*[(:]`);
	const matches: number[] = [];
	for (let i = 0; i < lines.length; i++) {
		if (head.test(lines[i])) {
			matches.push(i);
		}
	}
	if (matches.length === 0) {
		return undefined;
	}
	const defLine = matches[0];
	const indent = /^[ \t]*/.exec(lines[defLine])![0];

	// Decorators are part of the node (proxy behavior: "decorators included
	// automatically"). Naive: contiguous same-indent lines starting with '@'.
	let start = defLine;
	while (start > 0 && lines[start - 1].startsWith(`${indent}@`)) {
		start--;
	}

	// Body ends at the last line indented deeper than the def; blank lines
	// inside the body are skipped, trailing blanks stay outside the node.
	let lastNonEmpty = defLine;
	for (let i = defLine + 1; i < lines.length; i++) {
		if (lines[i].trim() === '') {
			continue;
		}
		const lineIndent = /^[ \t]*/.exec(lines[i])![0];
		if (lineIndent.length <= indent.length) {
			break;
		}
		lastNonEmpty = i;
	}

	const right = [...lines.slice(0, start), ...content.split('\n'), ...lines.slice(lastNonEmpty + 1)].join('\n');
	const note =
		matches.length > 1
			? `${matches.length} definitions named '${name}' — the first one is shown replaced`
			: undefined;
	return { right, note };
}

/** Replace the first top-level <tag>...</tag> element (nesting-aware for the
 * same tag name, self-closing handled) with `content`. Naive text scan. */
function spliceHtmlElement(source: string, tag: string, content: string): string | undefined {
	const tokenRe = new RegExp(`<(/?)${escapeRegExp(tag)}(?=[\\s/>])`, 'gi');
	let depth = 0;
	let start = -1;
	let match: RegExpExecArray | null;
	while ((match = tokenRe.exec(source)) !== null) {
		const gt = source.indexOf('>', match.index);
		if (gt === -1) {
			return undefined;
		}
		if (match[1] === '') {
			const selfClosing = source[gt - 1] === '/';
			if (depth === 0) {
				start = match.index;
				if (selfClosing) {
					return source.slice(0, start) + content + source.slice(gt + 1);
				}
			}
			if (!selfClosing) {
				depth++;
			}
		} else {
			depth--;
			if (depth === 0 && start !== -1) {
				return source.slice(0, start) + content + source.slice(gt + 1);
			}
			if (depth < 0) {
				return undefined;
			}
		}
	}
	return undefined;
}

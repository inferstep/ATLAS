// Unit tests for the permission-time edit predictions: write_file (new and
// existing file), edit_file (old_str found / not found / replace_all /
// literal-$ splice), and the structural_edit best-effort splices (python function
// incl. decorators and async, class, html tag incl. nesting and the <html>
// doctype quirk, no-match fallback).

import { describe, expect, it } from 'vitest';
import { editTargetPath, predictEdit } from '../src/session/editPreview';

const PY = [
	'import os',
	'',
	"@app.route('/dash')",
	'@login_required',
	'def dashboard():',
	'    users = get_users()',
	'',
	'    return render(users)',
	'',
	'def other():',
	'    pass',
].join('\n');

const HTML = [
	'<!DOCTYPE html>',
	'<html>',
	'<head><title>t</title></head>',
	'<body>',
	'  <div class="outer"><div>inner</div></div>',
	'</body>',
	'</html>',
].join('\n');

describe('editTargetPath', () => {
	it('returns the path for the three file-edit tools only', () => {
		expect(editTargetPath('write_file', { path: 'a.py', content: 'x' })).toBe('a.py');
		expect(editTargetPath('edit_file', { path: 'a.py' })).toBe('a.py');
		expect(editTargetPath('structural_edit', { path: 'a.py' })).toBe('a.py');
		expect(editTargetPath('run_command', { command: 'ls' })).toBeUndefined();
		expect(editTargetPath('move_file', { source: 'a', destination: 'b' })).toBeUndefined();
		expect(editTargetPath('delete_file', { path: 'a.py' })).toBeUndefined();
	});

	it('rejects malformed args', () => {
		expect(editTargetPath('write_file', null)).toBeUndefined();
		expect(editTargetPath('write_file', { path: 42 })).toBeUndefined();
	});
});

describe('predictEdit: write_file', () => {
	it('new file: empty left, content right, exact', () => {
		const p = predictEdit('write_file', { path: 'new.py', content: 'x = 1\n' }, undefined)!;
		expect(p).toMatchObject({ kind: 'file', left: '', right: 'x = 1\n', approximate: false });
		expect(p.note).toBe('new file');
	});

	it('existing file: current left, content right', () => {
		const p = predictEdit('write_file', { path: 'a.py', content: 'new' }, 'old')!;
		expect(p).toMatchObject({ kind: 'file', left: 'old', right: 'new', approximate: false });
		expect(p.note).toBeUndefined();
	});

	it('missing content field is not previewable', () => {
		expect(predictEdit('write_file', { path: 'a.py' }, 'old')).toBeUndefined();
	});
});

describe('predictEdit: edit_file', () => {
	it('unique old_str: exact single-occurrence replacement', () => {
		const p = predictEdit('edit_file', { path: 'a.txt', old_str: 'aa', new_str: 'bb' }, 'aa x cc')!;
		expect(p).toMatchObject({ kind: 'file', approximate: false, right: 'bb x cc' });
	});

	it('multi-match without replace_all is approximate — the proxy rejects it', () => {
		const p = predictEdit('edit_file', { path: 'a.txt', old_str: 'aa', new_str: 'bb' }, 'aa x aa')!;
		expect(p).toMatchObject({ kind: 'file', approximate: true, right: 'bb x aa' });
		expect(p.note).toContain('matches 2 locations');
		expect(p.note).toContain('reject');
	});

	it('replace_all replaces every occurrence', () => {
		const p = predictEdit('edit_file', { path: 'a.txt', old_str: 'aa', new_str: 'bb', replace_all: true }, 'aa x aa')!;
		expect(p.right).toBe('bb x bb');
		expect(p.approximate).toBe(false);
	});

	it('splices new_str literally (no $-pattern semantics)', () => {
		const p = predictEdit('edit_file', { path: 'a.txt', old_str: 'X', new_str: "$& $' $1", replace_all: false }, 'a X b')!;
		expect(p.right).toBe("a $& $' $1 b");
	});

	it('old_str not found: snippet diff plus note', () => {
		const p = predictEdit('edit_file', { path: 'a.txt', old_str: 'zz', new_str: 'bb' }, 'aa x aa')!;
		expect(p).toMatchObject({ kind: 'snippet', left: 'zz', right: 'bb', approximate: true });
		expect(p.note).toContain('old_str not found');
	});

	it('file missing locally: snippet diff plus workspace note', () => {
		const p = predictEdit('edit_file', { path: 'a.txt', old_str: 'zz', new_str: 'bb' }, undefined)!;
		expect(p.kind).toBe('snippet');
		expect(p.note).toContain('not found in this workspace');
	});
});

describe('predictEdit: structural_edit python', () => {
	it('function: replaces the def block including its decorators', () => {
		const content = "@app.route('/dash')\ndef dashboard():\n    return quick()";
		const p = predictEdit('structural_edit', { path: 'app.py', selector: 'function:dashboard', content }, PY)!;
		expect(p.kind).toBe('file');
		expect(p.approximate).toBe(true);
		expect(p.right).toContain('return quick()');
		// Old decorators and body are gone; sibling function untouched.
		expect(p.right).not.toContain('@login_required');
		expect(p.right).not.toContain('get_users');
		expect(p.right).toContain('def other():');
		expect(p.right).toContain('import os');
	});

	it('function: matches async def', () => {
		const source = 'async def fetch():\n    return await go()\n\nprint(1)';
		const p = predictEdit('structural_edit', { path: 'a.py', selector: 'function:fetch', content: 'async def fetch():\n    return 2' }, source)!;
		expect(p.right).toBe('async def fetch():\n    return 2\n\nprint(1)');
	});

	it('class: replaces the class body, keeps surroundings', () => {
		const source = 'class A:\n    def m(self):\n        pass\n\nclass B(Base):\n    x = 1\n\ntail = 2';
		const p = predictEdit('structural_edit', { path: 'a.py', selector: 'class:B', content: 'class B(Base):\n    x = 9' }, source)!;
		expect(p.right).toBe('class A:\n    def m(self):\n        pass\n\nclass B(Base):\n    x = 9\n\ntail = 2');
	});

	it('no match: whole file vs content, labeled approximate', () => {
		const p = predictEdit('structural_edit', { path: 'app.py', selector: 'function:missing', content: 'def missing():\n    pass' }, PY)!;
		expect(p).toMatchObject({ kind: 'file', left: PY, approximate: true });
		expect(p.right).toBe('def missing():\n    pass');
		expect(p.note).toContain('not matched locally');
	});

	it('duplicate names: first one replaced, note says so', () => {
		const source = 'def f():\n    pass\n\ndef f():\n    return 1';
		const p = predictEdit('structural_edit', { path: 'a.py', selector: 'function:f', content: 'def f():\n    return 9' }, source)!;
		expect(p.right).toBe('def f():\n    return 9\n\ndef f():\n    return 1');
		expect(p.note).toContain('2 definitions');
	});
});

describe('predictEdit: structural_edit html', () => {
	it('<body>: nesting-aware replace of the whole element', () => {
		const p = predictEdit('structural_edit', { path: 'i.html', selector: '<body>', content: '<body><p>new</p></body>' }, HTML)!;
		expect(p.right).toContain('<body><p>new</p></body>');
		expect(p.right).not.toContain('outer');
		expect(p.right).toContain('<head><title>t</title></head>');
	});

	it('<div>: matching close found past the nested same-tag', () => {
		const p = predictEdit('structural_edit', { path: 'i.html', selector: '<div>', content: '<div>x</div>' }, HTML)!;
		expect(p.right).toContain('  <div>x</div>\n</body>');
		expect(p.right).not.toContain('inner');
	});

	it('<html>: strips the leading doctype from content (proxy quirk)', () => {
		const content = '<!DOCTYPE html>\n<html><body>n</body></html>';
		const p = predictEdit('structural_edit', { path: 'i.html', selector: '<html>', content }, HTML)!;
		// Original doctype stays, content's duplicate is dropped.
		expect(p.right).toBe('<!DOCTYPE html>\n<html><body>n</body></html>');
	});

	it('tag absent: whole-file approximate fallback', () => {
		const p = predictEdit('structural_edit', { path: 'i.html', selector: '<table>', content: '<table></table>' }, HTML)!;
		expect(p.left).toBe(HTML);
		expect(p.note).toContain('not matched locally');
	});
});

describe('predictEdit: insert_after', () => {
	const SRC = 'a\nb\nc\n';

	it('inserts after a 1-based line number', () => {
		const p = predictEdit('insert_after', { path: 'f.py', line: 2, content: 'X' }, SRC)!;
		expect(p.right).toBe('a\nb\nX\nc\n');
		// Exact, unlike structural_edit: the model named a line, it did not
		// reproduce one, so there is nothing to match approximately.
		expect(p.approximate).toBe(false);
		expect(p.kind).toBe('file');
	});

	it('treats line 0 as the top of the file', () => {
		const p = predictEdit('insert_after', { path: 'f.py', line: 0, content: 'X' }, SRC)!;
		expect(p.right).toBe('X\na\nb\nc\n');
	});

	it('appends when the line is past the end', () => {
		const p = predictEdit('insert_after', { path: 'f.py', line: 99, content: 'X' }, SRC)!;
		expect(p.right).toBe('a\nb\nc\nX\n');
	});

	it('preserves a missing trailing newline', () => {
		const p = predictEdit('insert_after', { path: 'f.py', line: 1, content: 'X' }, 'a\nb')!;
		expect(p.right).toBe('a\nX\nb');
	});

	it('does not double a newline when content carries one', () => {
		const p = predictEdit('insert_after', { path: 'f.py', line: 1, content: 'X\n' }, SRC)!;
		expect(p.right).toBe('a\nX\nb\nc\n');
	});

	it('handles multi-line content', () => {
		const p = predictEdit('insert_after', { path: 'f.py', line: 1, content: 'X\nY' }, SRC)!;
		expect(p.right).toBe('a\nX\nY\nb\nc\n');
	});

	it('returns undefined on a missing or non-integer line', () => {
		expect(predictEdit('insert_after', { path: 'f.py', content: 'X' }, SRC)).toBeUndefined();
		expect(predictEdit('insert_after', { path: 'f.py', line: '2', content: 'X' }, SRC)).toBeUndefined();
		expect(predictEdit('insert_after', { path: 'f.py', line: 1.5, content: 'X' }, SRC)).toBeUndefined();
		expect(predictEdit('insert_after', { path: 'f.py', line: -1, content: 'X' }, SRC)).toBeUndefined();
	});

	it('returns undefined when the file is not readable locally', () => {
		// Unlike write_file, there is no sensible prediction without a base.
		expect(predictEdit('insert_after', { path: 'f.py', line: 1, content: 'X' }, undefined)).toBeUndefined();
	});

	it('is recognised as a file-edit tool', () => {
		expect(editTargetPath('insert_after', { path: 'f.py' })).toBe('f.py');
	});
});

describe('predictEdit: replace_lines', () => {
	const SRC = 'a\nb\nc\nd\n';

	it('replaces an inclusive range exactly', () => {
		const p = predictEdit('replace_lines',
			{ path: 'f.py', start_line: 2, end_line: 3, content: 'X\nY' }, SRC)!;
		expect(p.right).toBe('a\nX\nY\nd\n');
		expect(p.approximate).toBe(false);
		expect(p.note).toBe('replaced lines 2-3');
	});

	it('replaces a single line', () => {
		const p = predictEdit('replace_lines',
			{ path: 'f.py', start_line: 1, end_line: 1, content: 'Z' }, SRC)!;
		expect(p.right).toBe('Z\nb\nc\nd\n');
		expect(p.note).toBe('replaced line 1');
	});

	it('preserves a missing trailing newline', () => {
		const p = predictEdit('replace_lines',
			{ path: 'f.py', start_line: 2, end_line: 2, content: 'X' }, 'a\nb')!;
		expect(p.right).toBe('a\nX');
	});

	it('declines a range the file cannot satisfy', () => {
		for (const range of [{ start_line: 0, end_line: 1 }, { start_line: 3, end_line: 2 },
			{ start_line: 2, end_line: 99 }]) {
			expect(predictEdit('replace_lines', { path: 'f.py', ...range, content: 'X' }, SRC))
				.toBeUndefined();
		}
	});

	it('declines when a field is missing or not an integer', () => {
		expect(predictEdit('replace_lines', { path: 'f.py', end_line: 2, content: 'X' }, SRC))
			.toBeUndefined();
		expect(predictEdit('replace_lines',
			{ path: 'f.py', start_line: '1', end_line: 2, content: 'X' }, SRC)).toBeUndefined();
		expect(predictEdit('replace_lines',
			{ path: 'f.py', start_line: 1, end_line: 2, content: 'X' }, undefined)).toBeUndefined();
	});

	it('names the target path', () => {
		expect(editTargetPath('replace_lines', { path: 'f.py' })).toBe('f.py');
	});
});

// The size rules of the lint job, tried on code written for the test: a
// function over a limit is an error in a file the settings do not list, a
// function at the limit is not, and a listed file's number is no larger
// than its largest function needs.

import { readFileSync, readdirSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { ESLint, type Linter } from 'eslint';
import { describe, expect, it } from 'vitest';

const ROOT = resolve(__dirname, '..');
const LINES = 'max-lines-per-function';
const COMPLEXITY = 'complexity';
const NEW_FILE = 'src/aNewFile.ts';

type Limits = { lines: number; complexity: number };

/** What a file may hold when the settings do not list it. */
const LIMITS: Limits = { lines: 100, complexity: 15 };

const sizeRules = (limits: Limits): Linter.RulesRecord => ({
	[LINES]: ['error', { max: limits.lines }],
	[COMPLEXITY]: ['error', { max: limits.complexity, variant: 'modified' }],
});

/** The limits the lint job sets for one file of the extension. */
async function limitsOf(file: string): Promise<Limits> {
	const config = await new ESLint({ cwd: ROOT }).calculateConfigForFile(join(ROOT, file));
	return { lines: config.rules[LINES][1].max, complexity: config.rules[COMPLEXITY][1].max };
}

/** The size rules `code` breaks as the file `file`; `limits` replaces the file's own. */
async function broken(code: string, file = NEW_FILE, limits?: Limits): Promise<string[]> {
	const eslint = new ESLint({
		cwd: ROOT,
		overrideConfig: limits ? { rules: sizeRules(limits) } : undefined,
	});
	const [result] = await eslint.lintText(code, { filePath: join(ROOT, file) });
	return result.messages
		.filter((message) => message.severity === 2)
		.map((message) => message.ruleId ?? '')
		.filter((rule) => rule === LINES || rule === COMPLEXITY);
}

/** A function that spans `lines` lines. */
function functionOfLines(lines: number): string {
	const body = Array.from({ length: lines - 3 }, (_, i) => `\tvalue += ${i};`);
	return ['export function long(value: number): number {', ...body, '\treturn value;', '}', ''].join('\n');
}

/** A function of the given complexity: one path, and one more for each `if`. */
function functionOfComplexity(complexity: number): string {
	const ifs = Array.from({ length: complexity - 1 }, (_, i) => `\tif (value === ${i}) { return ${i}; }`);
	return ['export function branchy(value: number): number {', ...ifs, '\treturn value;', '}', ''].join('\n');
}

function switchOfCases(cases: number): string {
	const each = Array.from({ length: cases }, (_, i) => `\t\tcase ${i}: return ${i};`);
	return [
		'export function chosen(value: number): number {',
		'\tswitch (value) {',
		...each,
		'\t\tdefault: return -1;',
		'\t}',
		'}',
		'',
	].join('\n');
}

function sourceFiles(): string[] {
	return (readdirSync(join(ROOT, 'src'), { recursive: true }) as string[])
		.filter((name) => name.endsWith('.ts'))
		.map((name) => join('src', name).replaceAll('\\', '/'))
		.sort();
}

/** The listed limits of `file` that could be lower: its code passes one below them. */
async function looseLimits(file: string): Promise<string[]> {
	const own = await limitsOf(file);
	const code = readFileSync(join(ROOT, file), 'utf8');
	const loose: string[] = [];
	if (own.lines > LIMITS.lines && !(await broken(code, file, { ...own, lines: own.lines - 1 })).includes(LINES)) {
		loose.push(`${file}: ${LINES} is ${own.lines}, and no function in the file is that long. Lower the number in eslint.config.mjs.`);
	}
	if (own.complexity > LIMITS.complexity && !(await broken(code, file, { ...own, complexity: own.complexity - 1 })).includes(COMPLEXITY)) {
		loose.push(`${file}: ${COMPLEXITY} is ${own.complexity}, and no function in the file is that complex. Lower the number in eslint.config.mjs.`);
	}
	return loose;
}

describe('size rules, in a file the settings do not list', () => {
	it('accepts a function at the line limit and refuses one line more', async () => {
		expect(await broken(functionOfLines(LIMITS.lines))).toEqual([]);
		expect(await broken(functionOfLines(LIMITS.lines + 1))).toEqual([LINES]);
	});

	it('accepts a function at the complexity limit and refuses one decision more', async () => {
		expect(await broken(functionOfComplexity(LIMITS.complexity))).toEqual([]);
		expect(await broken(functionOfComplexity(LIMITS.complexity + 1))).toEqual([COMPLEXITY]);
	});

	it('counts a switch once, however many cases it has', async () => {
		expect(await broken(switchOfCases(LIMITS.complexity * 2))).toEqual([]);
	});
});

describe('size rules, in the files the settings list', () => {
	it('gives a listed file no more room than its largest function needs', async () => {
		const loose: string[] = [];
		for (const file of sourceFiles()) {
			loose.push(...(await looseLimits(file)));
		}
		expect(loose).toEqual([]);
	});

	it('finds no size error in the code as it is', async () => {
		const withErrors: string[] = [];
		for (const file of sourceFiles()) {
			const rules = await broken(readFileSync(join(ROOT, file), 'utf8'), file);
			if (rules.length > 0) {
				withErrors.push(`${file}: ${rules.join(', ')}`);
			}
		}
		expect(withErrors).toEqual([]);
	});
});

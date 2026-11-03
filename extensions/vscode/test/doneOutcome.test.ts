// The done event's status and reason reach the panel (#236). Both clients
// showed only the summary, so a stopped or failed run looked finished.

import { describe, expect, it } from 'vitest';
import { doneOutcome } from '../src/session/doneOutcome';

describe('doneOutcome', () => {
	it('carries the status, the reason and the summary', () => {
		expect(doneOutcome({ summary: 'Stopped: mod.py still does not parse.', status: 'incomplete', reason: 'repair_unfinished' }))
			.toEqual({ text: 'Stopped: mod.py still does not parse.', status: 'incomplete', label: 'incomplete', reason: 'repair_unfinished', cls: 'incomplete' });
	});

	it('reads a missing status as incomplete, as docs/reference/API.md says', () => {
		const o = doneOutcome({ summary: 'from an older proxy' });
		expect(o.status).toBe('incomplete');
		expect(o.cls).toBe('incomplete');
		expect(o.reason).toBe('');
	});

	it('gives each known status its own class', () => {
		const classes = ['completed', 'incomplete', 'stopped', 'timed_out', 'failed'].map((s) => doneOutcome({ summary: '', status: s }).cls);
		expect(new Set(classes).size).toBe(5);
		expect(doneOutcome({ summary: '', status: 'timed_out' }).label).toBe('timed out');
	});

	it('never turns an unknown status into a class name', () => {
		const o = doneOutcome({ summary: '', status: 'x" onclick="y' });
		expect(o.cls).toBe('other');
		expect(o.label).toBe('x" onclick="y');
	});

	it('handles a payload that is not there at all', () => {
		expect(doneOutcome(undefined)).toEqual({ text: '', status: 'incomplete', label: 'incomplete', reason: '', cls: 'incomplete' });
	});
});

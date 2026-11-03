// A run's outcome, from the done event (#236). Both clients showed only the
// summary, so a stopped or failed run looked like a finished one until its
// text was read. docs/reference/API.md: a missing status reads as incomplete.

import type { DoneEventData } from '../client/types';

/** The terminal statuses the proxy sends (proxy/internal/wire/messages.go, TerminalStatus). */
const KNOWN_STATUSES = new Set(['completed', 'incomplete', 'stopped', 'timed_out', 'failed']);

export interface DoneOutcome {
	/** done.summary; empty for a text-shaped turn. */
	text: string;
	/** done.status, or 'incomplete' when absent. */
	status: string;
	/** The status in words. */
	label: string;
	/** done.reason; may be empty. */
	reason: string;
	/** The CSS class suffix: the status when known, 'other' otherwise, so a
	 * value from the wire never becomes an arbitrary class name. */
	cls: string;
}

export function doneOutcome(payload: Partial<DoneEventData> | undefined): DoneOutcome {
	const status =
		typeof payload?.status === 'string' && payload.status !== '' ? payload.status : 'incomplete';
	return {
		text: typeof payload?.summary === 'string' ? payload.summary : '',
		status,
		label: status === 'timed_out' ? 'timed out' : status,
		reason: typeof payload?.reason === 'string' ? payload.reason : '',
		cls: KNOWN_STATUSES.has(status) ? status : 'other',
	};
}

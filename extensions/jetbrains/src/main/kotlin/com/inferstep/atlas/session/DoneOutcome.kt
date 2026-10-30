// A run's outcome, from the `done` event (#236).
//
// Both clients showed only the summary, so a stopped or failed run looked like
// a finished one until its text was read. docs/API.md: a missing status reads
// as incomplete.
//
// Ported from extensions/vscode/src/session/doneOutcome.ts. The `cls` field is
// the reference client's CSS class; here it names the banner style, and the
// rule that matters is the same — an unrecognised status from the wire never
// becomes a style name of its own.
package com.inferstep.atlas.session

import com.inferstep.atlas.protocol.DonePayload

/** The terminal statuses the proxy sends (proxy/types.go, TerminalStatus). */
private val KNOWN_STATUSES = setOf("completed", "incomplete", "stopped", "timed_out", "failed")

private const val MISSING_STATUS = "incomplete"
private const val OTHER = "other"

/** One finished run, as the transcript banner shows it. */
data class DoneOutcome(
    /** `done.summary`; empty for a text-shaped turn. */
    val text: String,
    /** `done.status`, or `incomplete` when absent. */
    val status: String,
    /** The status in words. */
    val label: String,
    /** `done.reason`; may be empty. */
    val reason: String,
    /** The banner style: the status when known, `other` otherwise. */
    val cls: String,
)

/**
 * Read a `done` payload into what the banner needs.
 *
 * Null covers the stream that ended without a `done` event at all, which is a
 * dropped connection: the same rule applies, and the run reads as incomplete.
 */
fun doneOutcome(payload: DonePayload?): DoneOutcome {
    val status = payload?.status?.takeIf { it.isNotEmpty() } ?: MISSING_STATUS
    return DoneOutcome(
        text = payload?.summary.orEmpty(),
        status = status,
        label = if (status == "timed_out") "timed out" else status,
        reason = payload?.reason.orEmpty(),
        cls = if (status in KNOWN_STATUSES) status else OTHER,
    )
}

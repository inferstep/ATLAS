// The done event's status and reason reach the panel (#236). Both clients
// showed only the summary, so a stopped or failed run looked finished.
package com.inferstep.atlas.session

import com.inferstep.atlas.protocol.DonePayload
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.params.ParameterizedTest
import org.junit.jupiter.params.provider.Arguments
import org.junit.jupiter.params.provider.MethodSource

class DoneOutcomeTest {
    @ParameterizedTest(name = "{0}")
    @MethodSource("outcomes")
    fun `reads a done payload into the banner, and invents no style of its own`(
        name: String,
        payload: DonePayload?,
        want: DoneOutcome,
    ) {
        assertEquals(want, doneOutcome(payload), name)
    }

    companion object {
        // Each outcome reads DoneOutcome(text, status, label, reason, cls).
        @JvmStatic
        fun outcomes(): List<Arguments> =
            listOf(
                Arguments.of(
                    "an incomplete run carries its summary and its reason",
                    DonePayload(
                        summary = "Stopped: mod.py still does not parse.",
                        status = "incomplete",
                        reason = "repair_unfinished",
                    ),
                    DoneOutcome(
                        "Stopped: mod.py still does not parse.",
                        "incomplete",
                        "incomplete",
                        "repair_unfinished",
                        "incomplete",
                    ),
                ),
                Arguments.of(
                    "a completed run",
                    DonePayload(status = "completed"),
                    DoneOutcome("", "completed", "completed", "", "completed"),
                ),
                Arguments.of(
                    "a stopped run",
                    DonePayload(status = "stopped"),
                    DoneOutcome("", "stopped", "stopped", "", "stopped"),
                ),
                Arguments.of(
                    "a timed-out run is labelled in words",
                    DonePayload(status = "timed_out"),
                    DoneOutcome("", "timed_out", "timed out", "", "timed_out"),
                ),
                Arguments.of(
                    "a failed run",
                    DonePayload(status = "failed"),
                    DoneOutcome("", "failed", "failed", "", "failed"),
                ),
                Arguments.of(
                    "a status this client has never seen gets the style other",
                    DonePayload(status = """x" onclick="y"""),
                    DoneOutcome("", """x" onclick="y""", """x" onclick="y""", "", "other"),
                ),
                Arguments.of(
                    "a missing status reads as incomplete, as the API doc says",
                    DonePayload(summary = "from an older proxy"),
                    DoneOutcome(
                        "from an older proxy",
                        "incomplete",
                        "incomplete",
                        "",
                        "incomplete",
                    ),
                ),
                Arguments.of(
                    "a stream that ended with no done event at all reads as incomplete",
                    null,
                    DoneOutcome("", "incomplete", "incomplete", "", "incomplete"),
                ),
            )
    }
}

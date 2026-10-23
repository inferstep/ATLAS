// The done event's status and reason reach the panel (#236). Both clients
// showed only the summary, so a stopped or failed run looked finished.
package com.inferstep.atlas.session

import com.inferstep.atlas.protocol.DonePayload
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.params.ParameterizedTest
import org.junit.jupiter.params.provider.Arguments
import org.junit.jupiter.params.provider.CsvSource
import org.junit.jupiter.params.provider.MethodSource

class DoneOutcomeTest {
    // The banner of each status: its label, and its style. A status this client has never seen
    // keeps its text as the label and gets the style "other": the panel invents no style.
    @ParameterizedTest(name = "{0}")
    @CsvSource(
        delimiter = '|',
        textBlock = """
        completed | completed | completed
        stopped | stopped | stopped
        timed_out | timed out | timed_out
        failed | failed | failed
        x" onclick="y | x" onclick="y | other""",
    )
    fun `gives each status its label and its style, and an unknown one the style other`(
        status: String,
        label: String,
        style: String,
    ) {
        val want = DoneOutcome("", status, label, "", style)

        assertEquals(want, doneOutcome(DonePayload(status = status)), status)
    }

    @ParameterizedTest(name = "{0}")
    @MethodSource("outcomes")
    fun `reads the summary and the reason, and a missing status or done event as incomplete`(
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

package com.inferstep.atlas.client

import com.inferstep.atlas.protocol.ReadyResponse
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import org.junit.jupiter.params.ParameterizedTest
import org.junit.jupiter.params.provider.Arguments
import org.junit.jupiter.params.provider.MethodSource

class AtlasStatusTest {
    @ParameterizedTest(name = "{0}")
    @MethodSource("gates")
    fun `a ready answer is Ready, or Starting with the failing gates in status order`(
        name: String,
        response: ReadyResponse,
        want: AtlasStatus,
    ) {
        assertEquals(want, statusFromReady(response), name)
    }

    @Test
    fun `a 401 is unauthorized and every other failure is unreachable`() {
        assertEquals(
            AtlasStatus.Unauthorized,
            statusFromFailure(AtlasApiError(401, "unauthorized", "no token")),
        )
        assertEquals(
            AtlasStatus.Unreachable,
            statusFromFailure(AtlasApiError(503, "dependency_unavailable", "lens down")),
        )
        assertEquals(
            AtlasStatus.Unreachable,
            statusFromFailure(IllegalStateException("connection refused")),
        )
    }

    @ParameterizedTest(name = "{0}")
    @MethodSource("polls")
    fun `the ready answer decides reachability and only a 401 rejects the token`(
        name: String,
        ready: ReadyResponse?,
        previouslyRejected: Boolean,
        probe: TokenProbe,
        want: AtlasStatus,
    ) {
        // A poll that never reached /ready does not probe at all, which the
        // table spells as an Unknown probe over the answer it already had.
        val status = statusFromAnswers(ready, probe.rejectedAfter(previouslyRejected))

        assertEquals(want, status, name)
    }

    @Test
    fun `a rejection belongs to the connection that produced it`() {
        val connection = AtlasConnection("http://127.0.0.1:8090", "s3cret")

        assertTrue(rejectionApplies(true, connection, connection))
        assertFalse(
            rejectionApplies(true, connection, connection.copy(url = "http://127.0.0.1:8091")),
            "a new address asks again",
        )
        assertFalse(
            rejectionApplies(true, connection, connection.copy(token = "next")),
            "a new token asks again",
        )
        assertFalse(rejectionApplies(true, null, connection), "nothing was probed yet")
        assertFalse(rejectionApplies(false, connection, connection), "no rejection to carry")
    }

    companion object {
        private val readyEveryGate =
            ReadyResponse(
                ready = true,
                inference = true,
                lensReady = true,
                sandbox = true,
                v3 = true,
            )

        private val lensAndV3Waiting =
            ReadyResponse(ready = false, inference = true, sandbox = true)

        @JvmStatic
        fun polls(): List<Arguments> =
            listOf(
                Arguments.of(
                    "ready and an accepted token",
                    readyEveryGate,
                    false,
                    TokenProbe.Accepted,
                    AtlasStatus.Ready,
                ),
                Arguments.of(
                    "ready and a rejected token",
                    readyEveryGate,
                    false,
                    TokenProbe.Rejected,
                    AtlasStatus.Unauthorized,
                ),
                Arguments.of(
                    "a ready answer without a rejection",
                    lensAndV3Waiting,
                    false,
                    TokenProbe.Accepted,
                    AtlasStatus.Starting(listOf("lens", "v3 service")),
                ),
                Arguments.of(
                    "waiting gates do not hide a rejection",
                    lensAndV3Waiting,
                    false,
                    TokenProbe.Rejected,
                    AtlasStatus.Unauthorized,
                ),
                Arguments.of(
                    "an unknown probe keeps the last answer",
                    readyEveryGate,
                    true,
                    TokenProbe.Unknown,
                    AtlasStatus.Unauthorized,
                ),
                Arguments.of(
                    "an unknown probe after a 2xx stays ready",
                    readyEveryGate,
                    false,
                    TokenProbe.Unknown,
                    AtlasStatus.Ready,
                ),
                Arguments.of(
                    "no ready answer is offline, never a token problem",
                    null,
                    true,
                    TokenProbe.Unknown,
                    AtlasStatus.Unreachable,
                ),
                Arguments.of(
                    "no ready answer over a previously good token",
                    null,
                    false,
                    TokenProbe.Unknown,
                    AtlasStatus.Unreachable,
                ),
            )

        @JvmStatic
        fun gates(): List<Arguments> =
            listOf(
                Arguments.of("all gates passing", readyEveryGate, AtlasStatus.Ready),
                Arguments.of(
                    "inference alone",
                    ReadyResponse(ready = false, lensReady = true, sandbox = true, v3 = true),
                    AtlasStatus.Starting(listOf("inference")),
                ),
                Arguments.of(
                    "lens and v3",
                    lensAndV3Waiting,
                    AtlasStatus.Starting(listOf("lens", "v3 service")),
                ),
                Arguments.of(
                    "all four, in display order",
                    ReadyResponse(ready = false),
                    AtlasStatus.Starting(listOf("inference", "lens", "sandbox", "v3 service")),
                ),
                Arguments.of(
                    "a ready flag that disagrees with the gates",
                    ReadyResponse(
                        ready = false,
                        inference = true,
                        lensReady = true,
                        sandbox = true,
                        v3 = true,
                    ),
                    AtlasStatus.Starting(emptyList()),
                ),
            )
    }
}

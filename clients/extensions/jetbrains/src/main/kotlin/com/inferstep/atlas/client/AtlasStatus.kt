// The proxy's readiness, as a value the status bar and its tests share.
//
// Kept in the IDE-free client package: it is a rule about the API's response,
// not about Swing. `AtlasStatusBarWidget` only decides how to draw it.
package com.inferstep.atlas.client

import com.inferstep.atlas.protocol.ReadyResponse

/** What an authenticated route says about the configured service token. */
enum class TokenProbe {
    /** The proxy accepted the token. */
    Accepted,

    /** The proxy answered 401: the token is missing or wrong. */
    Rejected,

    /** The route is absent, or answered something other than 2xx/401. */
    Unknown,
}

/** What the status bar says about the proxy, in display order. */
sealed interface AtlasStatus {
    /** Every gate passes and `/v1/agent` will accept a turn. */
    data object Ready : AtlasStatus

    /** Reachable, but one or more gates are still failing. */
    data class Starting(
        /** The gate labels that are not true yet, in status-bar order. */
        val waiting: List<String>,
    ) : AtlasStatus

    /** The proxy could not be reached at all. */
    data object Unreachable : AtlasStatus

    /** The proxy answered 401: it wants a service token this client lacks. */
    data object Unauthorized : AtlasStatus
}

/** Map `GET /ready` to the state the status bar shows. */
fun statusFromReady(response: ReadyResponse): AtlasStatus {
    if (response.ready) {
        return AtlasStatus.Ready
    }
    val waiting =
        buildList {
            if (!response.inference) {
                add("inference")
            }
            if (!response.lensReady) {
                add("lens")
            }
            if (!response.sandbox) {
                add("sandbox")
            }
            if (!response.v3) {
                add("v3 service")
            }
        }
    return AtlasStatus.Starting(waiting)
}

/**
 * The state one poll shows. [ready] is null when `/ready` did not answer, and
 * an unreachable proxy is "offline" even if the token was rejected on an
 * earlier poll: the bar does not report a token problem the proxy is not
 * there to confirm.
 */
fun statusFromAnswers(
    ready: ReadyResponse?,
    tokenRejected: Boolean,
): AtlasStatus =
    when {
        ready == null -> AtlasStatus.Unreachable
        tokenRejected -> AtlasStatus.Unauthorized
        else -> statusFromReady(ready)
    }

/**
 * Whether the token stands rejected after this probe. Only a 2xx clears a
 * rejection and only a 401 sets one: an unknown answer, such as a probe that
 * timed out or a route an older proxy does not serve, says nothing about the
 * token and leaves the last answer standing.
 */
fun TokenProbe.rejectedAfter(previous: Boolean): Boolean =
    when (this) {
        TokenProbe.Accepted -> false
        TokenProbe.Rejected -> true
        TokenProbe.Unknown -> previous
    }

/**
 * Whether a rejection carries over to [connection]. It belongs to the address
 * and token it was learned on: a new one is a new question and starts
 * un-rejected, while an unchanged one keeps the last answer until a probe
 * contradicts it.
 */
fun rejectionApplies(
    rejected: Boolean,
    probed: AtlasConnection?,
    connection: AtlasConnection,
): Boolean = rejected && probed == connection

/** Map a failed `/ready` request to the state the status bar shows. */
fun statusFromFailure(error: Throwable): AtlasStatus =
    if (error is AtlasApiError && error.isUnauthorized) {
        AtlasStatus.Unauthorized
    } else {
        AtlasStatus.Unreachable
    }

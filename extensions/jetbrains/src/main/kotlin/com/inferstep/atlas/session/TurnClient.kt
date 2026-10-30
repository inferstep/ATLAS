// The client surface the session layer needs, and the adapter that satisfies
// it with AtlasClient.
//
// TurnManager could take AtlasClient directly, but then the session layer's
// tests would need a live HTTP fixture for every turn-shaped case. Keeping the
// surface structural (two methods, no Ktor types but the Flow it returns) lets
// a test pass a fake, exactly as the VS Code client does with TurnClient.
package com.inferstep.atlas.session

import com.inferstep.atlas.client.AtlasClient
import com.inferstep.atlas.protocol.AgentRequest
import com.inferstep.atlas.protocol.ChatEvent
import com.inferstep.atlas.protocol.PermissionDecisionRequest
import com.inferstep.atlas.protocol.WorkspaceResponse
import kotlinx.coroutines.flow.Flow

/** One agent turn, streaming. Satisfied by [AtlasTurnClient]. */
interface TurnClient {
    /** POST /v1/agent and stream the turn's events. Cancelling the collection
     * aborts the request, which is the primary cancel mechanism. */
    fun agentTurn(request: AgentRequest): Flow<ChatEvent>

    /** POST /cancel, best-effort. Returns false when nothing was in flight. */
    suspend fun cancelTurn(sessionId: String): Boolean
}

/**
 * Adapts [AtlasClient] to the surfaces the session layer drives: [TurnClient],
 * [PermissionPoster], and the [WorkspaceProbe] the workspace check asks.
 *
 * The HTTP client stays a thin port of the VS Code one and knows nothing about
 * turns, history, permissions or diffs; this is the one place that knows they
 * fit together.
 */
class AtlasTurnClient(
    private val client: AtlasClient,
) : TurnClient,
    PermissionPoster,
    WorkspaceProbe {
    override fun agentTurn(request: AgentRequest): Flow<ChatEvent> = client.agentTurn(request)

    override suspend fun cancelTurn(sessionId: String): Boolean = client.cancelTurn(sessionId)

    override suspend fun postPermissionDecision(decision: PermissionDecisionRequest) {
        client.postPermissionDecision(decision)
    }

    override suspend fun workspace(): WorkspaceResponse? = client.getWorkspace()
}

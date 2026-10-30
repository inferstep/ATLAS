// The client's half of the workspace: reading and stat-ing the files the proxy
// edits, so a diff can be computed, and comparing the folder the IDE has open
// with the host path the proxy reports.
//
// Free of com.intellij (and of Compose) on purpose: the panel supplies the
// refresh callback that tells the IDE's VFS a file changed, and everything else
// — path containment, reads, stats, the alignment verdict — runs under plain
// JUnit. A port of the parts of extensions/vscode/src/workspace/ that v1 needs;
// `atlas workspace align` is deliberately out of scope for the plugin v1.
package com.inferstep.atlas.session

import com.inferstep.atlas.protocol.WorkspaceResponse
import java.io.File
import java.nio.file.Path
import java.nio.file.Paths

/** Result of stat-ing a workspace-relative path. */
data class PathStat(
    val exists: Boolean,
    /** Milliseconds since the epoch, or null when the platform cannot say. */
    val mtimeMs: Long? = null,
)

/**
 * The local view of the workspace the proxy edits.
 *
 * [root] is null when there is no folder open (a chat-only session); every
 * read then degrades to "not readable here", which is the honest answer, and
 * the callers treat it as such (no diff, inconclusive mismatch verdicts).
 */
interface Workspace {
    val root: String?

    /** The file's text, or null when it is absent, unreadable, not UTF-8 text,
     * or resolves outside [root]. */
    fun readText(relativePath: String): String?

    /** Stat a workspace-relative path. Never throws. */
    fun stat(relativePath: String): PathStat

    /** The proxy changed this path. The IDE updates its VFS from disk on its
     * own, but a tool window can be the only thing looking at a new file, so
     * the UI refreshes it here rather than waiting. */
    fun changed(relativePath: String) {}
}

/**
 * [Workspace] over a local directory.
 *
 * [onChanged] is where the IDE refresh lives; the session layer only says which
 * path changed. Paths that resolve outside [root] are refused before any
 * filesystem call, because the target comes from model-controlled tool args.
 */
class LocalWorkspace(
    override val root: String?,
    private val onChanged: (String) -> Unit = {},
) : Workspace {
    override fun readText(relativePath: String): String? {
        val file = resolve(relativePath) ?: return null
        return try {
            file.readText()
        } catch (_: Exception) {
            // Absent, a directory, binary, or unreadable: the callers' contract
            // is "can't show this", never an exception out of a diff.
            null
        }
    }

    override fun stat(relativePath: String): PathStat {
        val file = resolve(relativePath) ?: return PathStat(false)
        if (!file.exists()) {
            return PathStat(false)
        }
        return PathStat(true, file.lastModified())
    }

    override fun changed(relativePath: String) {
        onChanged(relativePath)
    }

    /** Resolve inside [root], refusing an absolute path or a `..` escape. */
    private fun resolve(relativePath: String): File? {
        val base = root?.takeIf { it.isNotEmpty() } ?: return null
        if (relativePath.isEmpty() || File(relativePath).isAbsolute) {
            return null
        }
        val dir = File(base).absoluteFile
        val target = File(dir, relativePath).normalize()
        if (target != dir && !target.path.startsWith(dir.path + File.separator)) {
            return null
        }
        return target
    }
}

// ---------------------------------------------------------------------------
// GET /workspace: is the folder the IDE has open the one being edited?
// ---------------------------------------------------------------------------

/** How the IDE's open folder compares with the proxy's reported host path. */
enum class WorkspaceAlignment {
    /** The open folder is covered by the proxy's project dir. */
    ALIGNED,

    /** The proxy reports a host path that does not cover the open folder. */
    MISALIGNED,

    /** Older proxy, empty/relative path, or no folder open: cannot tell. */
    UNKNOWN,
}

/** The proxy's workspace report. A function type rather than an interface, so
 * a test passes a lambda and the panel passes the client. */
fun interface WorkspaceProbe {
    /** GET /workspace, or null when the proxy cannot be asked. */
    suspend fun workspace(): WorkspaceResponse?
}

/**
 * Compare the proxy's reported host project dir with the folder the IDE has
 * open. Returns [WorkspaceAlignment.UNKNOWN] whenever either side cannot be
 * trusted — a missing or unreachable route must never be read as a mismatch.
 */
fun workspaceAlignment(
    projectDir: String?,
    localRoot: String?,
): WorkspaceAlignment {
    val reported = projectDir?.trim().orEmpty()
    if (reported.isEmpty() || !File(reported).isAbsolute) {
        return WorkspaceAlignment.UNKNOWN
    }
    if (localRoot.isNullOrEmpty()) {
        return WorkspaceAlignment.UNKNOWN
    }
    return if (covers(reported, localRoot)) {
        WorkspaceAlignment.ALIGNED
    } else {
        WorkspaceAlignment.MISALIGNED
    }
}

/**
 * True when [target] is inside [bound].
 *
 * The same rule as `atlas workspace`'s own `_covers`: both sides are realpath'd
 * first where they exist, so a symlinked project dir still matches. A path that
 * does not exist yet (or a different filesystem root) resolves lexically, and a
 * root that differs entirely is not covered.
 */
fun covers(
    bound: String,
    target: String,
): Boolean {
    if (bound.isEmpty()) {
        return false
    }
    return try {
        val relative = realPathOrResolve(bound).relativize(realPathOrResolve(target))
        val text = relative.toString()
        text.isEmpty() || !text.startsWith("..")
    } catch (_: Exception) {
        // Different roots/drives — Python's ValueError case.
        false
    }
}

/** realpath when the path exists (resolves symlinks, matching the Python
 * original), the lexically resolved path otherwise. */
private fun realPathOrResolve(path: String): Path =
    try {
        Paths.get(path).toRealPath()
    } catch (_: Exception) {
        Paths.get(path).toAbsolutePath().normalize()
    }

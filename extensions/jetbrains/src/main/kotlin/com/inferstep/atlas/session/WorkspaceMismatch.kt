// Passive workspace-mismatch heuristic. The proxy applies file ops to its own
// workspace and GET /workspace has only recently been able to name it, so a
// client should not rely on that route alone. Instead: after a file-op
// tool_result reports success, wait a short settle delay, stat the target path
// inside the local workspace folder, and compare with a pre-op stat captured at
// tool_call time:
//
//   write_file  -> the file must exist (an overwrite with an unchanged mtime
//                  is inconclusive, not proof of a wrong mount)
//   edit_file / structural_edit -> the file must exist; unchanged mtime is
//                  inconclusive (coarse-mtime filesystems)
//   delete_file -> the file must be absent
//   move_file   -> destination present, source absent
//
// No probe writes, ever. A single 'mismatch' verdict is enough to warn once
// ("proxy likely mounted elsewhere"); 'inconclusive' verdicts are dropped
// silently — mtime granularity, unsaved editors, and absolute proxy-side paths
// make false positives too easy otherwise.
//
// A port of extensions/vscode/src/workspace/mismatch.ts, deliberately free of
// com.intellij so it runs under plain JUnit. The stat is injected through
// [Workspace].
package com.inferstep.atlas.session

import kotlinx.coroutines.delay
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive

/** File-op tools the heuristic watches, mapped to how their effect is
 * verified locally. */
private val FILE_OP_KINDS: Map<String, FileOpKind> =
    mapOf(
        "write_file" to FileOpKind.WRITE,
        "edit_file" to FileOpKind.EDIT,
        "structural_edit" to FileOpKind.EDIT,
        "insert_after" to FileOpKind.EDIT,
        "replace_lines" to FileOpKind.EDIT,
        "delete_file" to FileOpKind.DELETE,
        "move_file" to FileOpKind.MOVE,
    )

enum class FileOpKind {
    WRITE,
    EDIT,
    DELETE,
    MOVE,
}

enum class Verdict {
    MATCH,
    MISMATCH,
    INCONCLUSIVE,
}

/** Injected stat: resolves a proxy-relative path inside the local workspace
 * folder and stats it. Never throws — absent maps to [PathStat] with
 * `exists = false`. */
typealias StatFn = (String) -> PathStat

/** The paths one file-op call touches. `secondary` is a move's destination. */
data class FileOpPaths(
    val primary: String,
    val secondary: String? = null,
)

/** Paths a file-op call touches, from its args. Null = not a file op or
 * malformed args. */
fun fileOpPaths(
    tool: String,
    args: JsonElement?,
): FileOpPaths? {
    if (tool !in FILE_OP_KINDS) {
        return null
    }
    val record = args as? JsonObject ?: return null
    if (tool == "move_file") {
        // MoveFileInput: source + destination (proxy/types.go).
        val source = stringField(record, "source") ?: return null
        val destination = stringField(record, "destination") ?: return null
        return FileOpPaths(source, destination)
    }
    val path = stringField(record, "path") ?: return null
    return FileOpPaths(path)
}

/** Compare pre-op and post-op stats for one successful file op. */
fun fileOpVerdict(
    kind: FileOpKind,
    pre: PathStat,
    post: PathStat,
    /** move_file only: destination stats. */
    preDest: PathStat? = null,
    postDest: PathStat? = null,
): Verdict =
    when (kind) {
        FileOpKind.WRITE -> {
            if (!post.exists) {
                Verdict.MISMATCH
            } else if (!pre.exists) {
                // The file appeared where the proxy said it wrote.
                Verdict.MATCH
            } else {
                mtimeVerdict(pre, post)
            }
        }

        FileOpKind.EDIT -> {
            if (!pre.exists) {
                // The proxy edited a file this workspace never had — suspicious,
                // but read_file would have had to see it, so this is more likely
                // an absolute/odd path than a wrong mount.
                Verdict.INCONCLUSIVE
            } else if (!post.exists) {
                Verdict.MISMATCH
            } else {
                mtimeVerdict(pre, post)
            }
        }

        FileOpKind.DELETE -> {
            // Absent before and (presumably) after.
            if (!pre.exists) {
                Verdict.INCONCLUSIVE
            } else if (post.exists) {
                Verdict.MISMATCH
            } else {
                Verdict.MATCH
            }
        }

        FileOpKind.MOVE -> {
            if (!pre.exists || preDest == null || postDest == null) {
                Verdict.INCONCLUSIVE
            } else if (postDest.exists && !post.exists) {
                Verdict.MATCH
            } else if (post.exists && !postDest.exists) {
                Verdict.MISMATCH
            } else {
                Verdict.INCONCLUSIVE
            }
        }
    }

/** mtime comparison for overwrite/edit kinds. Equal mtimes are
 * 'inconclusive', not 'mismatch': coarse-mtime filesystems (FAT, some network
 * mounts) can stamp a real change with the same time — exactly the granularity
 * caveat in the header note. Missing mtimes can't tell either way. */
private fun mtimeVerdict(
    pre: PathStat,
    post: PathStat,
): Verdict {
    if (pre.mtimeMs == null || post.mtimeMs == null) {
        return Verdict.MATCH // can't tell — conservative: no false mismatch
    }
    if (post.mtimeMs != pre.mtimeMs) {
        return Verdict.MATCH
    }
    return Verdict.INCONCLUSIVE
}

/** Pre-op stats captured at tool_call time, FIFO per tool name (same matching
 * scheme as the chat's edit snapshots — tool_result carries only the tool
 * name). */
private class PendingOp(
    val kind: FileOpKind,
    val paths: FileOpPaths,
    val pre: PathStat,
    val preSecondary: PathStat?,
)

/**
 * Session-long detector: feed it tool_call and tool_result events; it warns
 * (once) via [onMismatch] when local disk state contradicts a successful file
 * op.
 */
class MismatchDetector(
    private val stat: StatFn,
    private val onMismatch: () -> Unit,
    /** Delay before the post-op stat, giving the bind mount and file watcher
     * time to settle. Tests pass 0. Default 500. */
    private val settleMs: Long = 500L,
) {
    private val pending = mutableMapOf<String, MutableList<PendingOp>>()
    private var fired = false

    /** Capture pre-op state at tool_call time. No-op for non-file-op tools. */
    fun recordToolCall(
        tool: String,
        args: JsonElement?,
    ) {
        if (fired) {
            return
        }
        val paths = fileOpPaths(tool, args) ?: return
        val kind = FILE_OP_KINDS[tool] ?: return
        val op =
            PendingOp(
                kind = kind,
                paths = paths,
                pre = stat(paths.primary),
                preSecondary = paths.secondary?.let { stat(it) },
            )
        pending.getOrPut(tool) { mutableListOf() } += op
    }

    /** Check a tool_result against its captured pre-op state. Failed results
     * only consume the queue entry — the proxy made no change to verify. */
    suspend fun recordToolResult(
        tool: String,
        success: Boolean,
    ) {
        val op = pending[tool]?.removeFirstOrNull() ?: return
        if (!success || fired) {
            return
        }
        if (settleMs > 0) {
            delay(settleMs)
        }
        val post = stat(op.paths.primary)
        val postSecondary = op.paths.secondary?.let { stat(it) }
        val verdict = fileOpVerdict(op.kind, op.pre, post, op.preSecondary, postSecondary)
        if (verdict == Verdict.MISMATCH && !fired) {
            fired = true
            pending.clear()
            onMismatch()
        }
    }

    /** Drop pending pre-op state (turn ended with results unconsumed). */
    fun reset() {
        pending.clear()
    }
}

private fun stringField(
    obj: JsonObject,
    key: String,
): String? {
    val value = obj[key] as? JsonPrimitive ?: return null
    return if (value.isString) value.content else null
}

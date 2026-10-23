// The mismatch heuristic: which calls are watched, what each kind of change
// proves, and when the detector stays quiet.
//
// Both halves are tables: every row is the same question asked with other
// inputs — two stats for a verdict; for the detector, a short scripted session
// of calls, results and stats, and whether it warns.
package com.inferstep.atlas.session

import com.inferstep.atlas.protocol.AtlasJson
import kotlinx.coroutines.runBlocking
import kotlinx.serialization.json.JsonElement
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.params.ParameterizedTest
import org.junit.jupiter.params.provider.Arguments
import org.junit.jupiter.params.provider.MethodSource

private fun args(json: String): JsonElement = AtlasJson.parseToJsonElement(json)

private val ABSENT = PathStat(exists = false)

private fun at(mtimeMs: Long): PathStat = PathStat(exists = true, mtimeMs = mtimeMs)

class WorkspaceMismatchTest {
    @ParameterizedTest(name = "{0}")
    @MethodSource("paths")
    fun `names the paths a file op touches, and null for anything else`(
        name: String,
        tool: String,
        argsJson: String,
        want: FileOpPaths?,
    ) {
        assertEquals(want, fileOpPaths(tool, args(argsJson)), name)
    }

    @ParameterizedTest(name = "{0}")
    @MethodSource("verdicts")
    fun `reads a pre-op and post-op stat as a verdict`(
        name: String,
        kind: FileOpKind,
        pre: PathStat,
        post: PathStat,
        preDest: PathStat?,
        postDest: PathStat?,
        want: Verdict,
    ) {
        assertEquals(want, fileOpVerdict(kind, pre, post, preDest, postDest), name)
    }

    /** One step of a session: a call, its result, a stat the test changes
     * between the two, or the end of a turn. A row is a whole session, not a
     * single call — the steps come in the order a stream sends them. */
    sealed interface Step {
        data class Call(
            val tool: String,
            val args: String,
        ) : Step

        data class Result(
            val tool: String,
            val success: Boolean,
        ) : Step

        data class Stat(
            val path: String,
            val value: PathStat,
        ) : Step

        object Reset : Step
    }

    @ParameterizedTest(name = "{0}")
    @MethodSource("sessions")
    fun `warns once, and only when a successful file op contradicts local disk`(
        name: String,
        steps: List<Step>,
        want: Int,
    ) = runBlocking {
        // The local disk: the steps change it between a call and its result,
        // which is what the real filesystem does.
        val disk = mutableMapOf<String, PathStat>()
        var warnings = 0
        val detector =
            MismatchDetector(
                stat = { path -> disk[path] ?: ABSENT },
                onMismatch = { warnings += 1 },
                settleMs = 0,
            )

        for (step in steps) {
            when (step) {
                is Step.Call -> detector.recordToolCall(step.tool, args(step.args))
                is Step.Result -> detector.recordToolResult(step.tool, step.success)
                is Step.Stat -> disk[step.path] = step.value
                Step.Reset -> detector.reset()
            }
        }

        assertEquals(want, warnings, name)
    }

    companion object {
        @JvmStatic
        fun sessions(): List<Arguments> =
            listOf(
                Arguments.of(
                    "a write that appears locally is no contradiction",
                    listOf(
                        Step.Call("write_file", """{"path":"a.py","content":"x"}"""),
                        Step.Stat("a.py", at(1)),
                        Step.Result("write_file", true),
                    ),
                    0,
                ),
                Arguments.of(
                    "a successful write that never lands warns once, and a second one stays silent",
                    listOf(
                        Step.Call("write_file", """{"path":"a.py","content":"x"}"""),
                        Step.Result("write_file", true),
                        Step.Call("write_file", """{"path":"b.py","content":"y"}"""),
                        Step.Result("write_file", true),
                    ),
                    1,
                ),
                Arguments.of(
                    "a failed result applied nothing to verify",
                    listOf(
                        Step.Call("write_file", """{"path":"a.py","content":"x"}"""),
                        Step.Result("write_file", false),
                    ),
                    0,
                ),
                Arguments.of(
                    "results pair with the calls in order",
                    listOf(
                        Step.Stat("a.py", at(1)),
                        Step.Call("edit_file", """{"path":"a.py"}"""),
                        Step.Call("edit_file", """{"path":"b.py"}"""),
                        Step.Stat("a.py", ABSENT),
                        // a.py's result: a successful edit of a file that vanished.
                        Step.Result("edit_file", true),
                        // b.py's result failed, so nothing is checked for it.
                        Step.Result("edit_file", false),
                    ),
                    1,
                ),
                Arguments.of(
                    "an equal mtime is inconclusive, never a wrong mount",
                    listOf(
                        Step.Stat("a.py", at(1)),
                        Step.Call("edit_file", """{"path":"a.py"}"""),
                        Step.Result("edit_file", true),
                    ),
                    0,
                ),
                Arguments.of(
                    "a move that lands is no contradiction",
                    listOf(
                        Step.Stat("a.py", at(1)),
                        Step.Call("move_file", """{"source":"a.py","destination":"lib/a.py"}"""),
                        Step.Stat("a.py", ABSENT),
                        Step.Stat("lib/a.py", at(2)),
                        Step.Result("move_file", true),
                    ),
                    0,
                ),
                Arguments.of(
                    "a move that stays put warns",
                    listOf(
                        Step.Stat("a.py", at(1)),
                        Step.Call("move_file", """{"source":"a.py","destination":"lib/a.py"}"""),
                        Step.Result("move_file", true),
                    ),
                    1,
                ),
                Arguments.of(
                    "a denied call's failed result consumes its entry, so the next write pairs with its own",
                    listOf(
                        Step.Call("write_file", """{"path":"a.py","content":"x"}"""),
                        // The proxy follows a denial with this failed result.
                        Step.Result("write_file", false),
                        Step.Call("write_file", """{"path":"b.py","content":"y"}"""),
                        Step.Stat("b.py", at(2)),
                        Step.Result("write_file", true),
                    ),
                    0,
                ),
                Arguments.of(
                    "a tool that is not a file op is never watched",
                    listOf(
                        Step.Call("read_file", """{"path":"a.py"}"""),
                        Step.Result("read_file", true),
                    ),
                    0,
                ),
                Arguments.of(
                    "reset drops the pre-op state of a turn that ended without results",
                    listOf(
                        Step.Call("write_file", """{"path":"a.py","content":"x"}"""),
                        Step.Reset,
                        Step.Result("write_file", true),
                    ),
                    0,
                ),
            )

        @JvmStatic
        fun paths(): List<Arguments> =
            listOf(
                Arguments.of(
                    "write_file carries one path",
                    "write_file",
                    """{"path":"a.py","content":"x"}""",
                    FileOpPaths("a.py"),
                ),
                Arguments.of(
                    "edit_file carries one path",
                    "edit_file",
                    """{"path":"b.py"}""",
                    FileOpPaths("b.py"),
                ),
                Arguments.of(
                    "structural_edit carries one path",
                    "structural_edit",
                    """{"path":"c.py"}""",
                    FileOpPaths("c.py"),
                ),
                Arguments.of(
                    "insert_after carries one path",
                    "insert_after",
                    """{"path":"g.py"}""",
                    FileOpPaths("g.py"),
                ),
                Arguments.of(
                    "replace_lines carries one path",
                    "replace_lines",
                    """{"path":"h.py"}""",
                    FileOpPaths("h.py"),
                ),
                Arguments.of(
                    "delete_file carries one path",
                    "delete_file",
                    """{"path":"d.py"}""",
                    FileOpPaths("d.py"),
                ),
                Arguments.of(
                    "move_file carries a source and a destination",
                    "move_file",
                    """{"source":"a.py","destination":"lib/a.py"}""",
                    FileOpPaths("a.py", "lib/a.py"),
                ),
                Arguments.of(
                    "read_file touches no file",
                    "read_file",
                    """{"path":"a.py"}""",
                    null,
                ),
                Arguments.of(
                    "a path that is not a string",
                    "write_file",
                    """{"path":42}""",
                    null,
                ),
                Arguments.of(
                    "a move with no destination",
                    "move_file",
                    """{"source":"a.py"}""",
                    null,
                ),
                Arguments.of(
                    "no args at all",
                    "write_file",
                    "null",
                    null,
                ),
            )

        @JvmStatic
        fun verdicts(): List<Arguments> =
            listOf(
                Arguments.of(
                    "write: a new file appearing is a match",
                    FileOpKind.WRITE,
                    ABSENT,
                    at(2),
                    null,
                    null,
                    Verdict.MATCH,
                ),
                Arguments.of(
                    "write: staying absent is a mismatch",
                    FileOpKind.WRITE,
                    ABSENT,
                    ABSENT,
                    null,
                    null,
                    Verdict.MISMATCH,
                ),
                Arguments.of(
                    "write: an overwrite with a bumped mtime is a match",
                    FileOpKind.WRITE,
                    at(1),
                    at(2),
                    null,
                    null,
                    Verdict.MATCH,
                ),
                Arguments.of(
                    "write: an equal mtime is inconclusive, never a wrong mount",
                    FileOpKind.WRITE,
                    at(1),
                    at(1),
                    null,
                    null,
                    Verdict.INCONCLUSIVE,
                ),
                Arguments.of(
                    "edit: a changed mtime is a match",
                    FileOpKind.EDIT,
                    at(1),
                    at(2),
                    null,
                    null,
                    Verdict.MATCH,
                ),
                Arguments.of(
                    "edit: an equal mtime is inconclusive",
                    FileOpKind.EDIT,
                    at(1),
                    at(1),
                    null,
                    null,
                    Verdict.INCONCLUSIVE,
                ),
                Arguments.of(
                    "edit: a vanished file is a mismatch",
                    FileOpKind.EDIT,
                    at(1),
                    ABSENT,
                    null,
                    null,
                    Verdict.MISMATCH,
                ),
                Arguments.of(
                    "edit: a file this workspace never had is inconclusive",
                    FileOpKind.EDIT,
                    ABSENT,
                    at(2),
                    null,
                    null,
                    Verdict.INCONCLUSIVE,
                ),
                Arguments.of(
                    "delete: disappearance is a match",
                    FileOpKind.DELETE,
                    at(1),
                    ABSENT,
                    null,
                    null,
                    Verdict.MATCH,
                ),
                Arguments.of(
                    "delete: the file still being there is a mismatch",
                    FileOpKind.DELETE,
                    at(1),
                    at(1),
                    null,
                    null,
                    Verdict.MISMATCH,
                ),
                Arguments.of(
                    "delete: a file that was already absent is inconclusive",
                    FileOpKind.DELETE,
                    ABSENT,
                    ABSENT,
                    null,
                    null,
                    Verdict.INCONCLUSIVE,
                ),
                Arguments.of(
                    "move: source gone and destination present is a match",
                    FileOpKind.MOVE,
                    at(1),
                    ABSENT,
                    ABSENT,
                    at(2),
                    Verdict.MATCH,
                ),
                Arguments.of(
                    "move: source unmoved is a mismatch",
                    FileOpKind.MOVE,
                    at(1),
                    at(1),
                    ABSENT,
                    ABSENT,
                    Verdict.MISMATCH,
                ),
                Arguments.of(
                    "move: a source that was never here is inconclusive",
                    FileOpKind.MOVE,
                    ABSENT,
                    ABSENT,
                    ABSENT,
                    ABSENT,
                    Verdict.INCONCLUSIVE,
                ),
                Arguments.of(
                    "move: both present (copy-like state) is inconclusive",
                    FileOpKind.MOVE,
                    at(1),
                    at(1),
                    ABSENT,
                    at(2),
                    Verdict.INCONCLUSIVE,
                ),
                Arguments.of(
                    "edit: a missing mtime is read as changed, conservatively",
                    FileOpKind.EDIT,
                    PathStat(exists = true),
                    at(2),
                    null,
                    null,
                    Verdict.MATCH,
                ),
                Arguments.of(
                    "write: a missing post-op mtime is read as changed too",
                    FileOpKind.WRITE,
                    at(1),
                    PathStat(exists = true),
                    null,
                    null,
                    Verdict.MATCH,
                ),
            )
    }
}

// The local workspace: what a read and a stat return, that a path from tool
// args cannot escape the root, and the exact alignment check GET /workspace
// makes possible.
//
// The failure this exists for: the proxy was bound to ~/snake-game while the
// IDE had ~/demo2 open. Every tool call was truthful and useless, and it took
// several rounds of manual `atlas workspace align` to notice.
package com.inferstep.atlas.session

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.io.TempDir
import org.junit.jupiter.params.ParameterizedTest
import org.junit.jupiter.params.provider.Arguments
import org.junit.jupiter.params.provider.MethodSource
import org.junit.jupiter.params.provider.NullAndEmptySource
import java.io.File

class WorkspaceTest {
    @TempDir
    lateinit var dir: File

    /** The folder the window has open: a subfolder of the test's own temp
     * folder, so a file "outside" it is still one this test made and deletes
     * with the folder, never one beside the runner's temporary root. */
    private lateinit var root: File

    @BeforeEach
    fun makeRoot() {
        root = File(dir, "root").apply { mkdirs() }
    }

    @ParameterizedTest(name = "{0}")
    @MethodSource("reads")
    fun `reads a file under the root, and nothing for an absent one`(
        name: String,
        path: String,
        want: String?,
    ) {
        File(root, "a.py").writeText("x = 1\n")
        File(root, "src").apply { mkdirs() }
        File(root, "src/a.py").writeText("nested")
        val workspace = LocalWorkspace(root.absolutePath)

        assertEquals(want, workspace.readText(path), name)
    }

    @ParameterizedTest(name = "{0}")
    @MethodSource("refusals")
    fun `refuses a path that leaves the root`(
        name: String,
        path: String,
    ) {
        // What each path would reach if it were let through: the root's own
        // a.py for the absolute path read as relative, and a file beside the root.
        File(root, "a.py").writeText("inside")
        File(dir, "outside.py").writeText("outside")
        val workspace = LocalWorkspace(root.absolutePath)

        assertNull(workspace.readText(path), name)
        assertEquals(PathStat(exists = false), workspace.stat(path), name)
    }

    @ParameterizedTest(name = "root {0}")
    @NullAndEmptySource
    fun `reads nothing when no folder is open`(noRoot: String?) {
        val workspace = LocalWorkspace(noRoot)

        assertNull(workspace.readText("a.py"))
        assertEquals(PathStat(exists = false), workspace.stat("a.py"))
    }

    @Test
    fun `stat reports existence and an mtime`() {
        val file = File(root, "a.py")
        file.writeText("x")
        val workspace = LocalWorkspace(root.absolutePath)

        val stat = workspace.stat("a.py")

        assertTrue(stat.exists)
        assertEquals(file.lastModified(), stat.mtimeMs)
        assertEquals(PathStat(exists = false), workspace.stat("missing.py"))
    }

    @Test
    fun `the refresh callback names the path the proxy changed`() {
        val changed = mutableListOf<String>()
        val workspace = LocalWorkspace(root.absolutePath, onChanged = { changed += it })

        workspace.changed("src/new.py")

        assertEquals(listOf("src/new.py"), changed)
    }

    @ParameterizedTest(name = "{0}")
    @MethodSource("coverages")
    fun `covers is containment of a resolved path`(
        name: String,
        bound: String,
        target: String,
        want: Boolean,
    ) {
        assertEquals(want, covers(bound, target), name)
    }

    @ParameterizedTest(name = "{0}")
    @MethodSource("alignments")
    fun `the proxy's host path is compared with the folder this window has open`(
        name: String,
        projectDir: String?,
        localRoot: String?,
        want: WorkspaceAlignment,
    ) {
        assertEquals(want, workspaceAlignment(projectDir, localRoot), name)
    }

    companion object {
        @JvmStatic
        fun reads(): List<Arguments> =
            listOf(
                Arguments.of("a file at the root", "a.py", "x = 1\n"),
                Arguments.of("a file in a subdirectory", "src/a.py", "nested"),
                Arguments.of("an absent file reads as nothing", "missing.py", null),
            )

        @JvmStatic
        fun refusals(): List<Arguments> =
            listOf(
                Arguments.of("an absolute path, even when the root has a file there", "/a.py"),
                Arguments.of("a path that climbs out of the root", "../outside.py"),
            )

        @JvmStatic
        fun coverages(): List<Arguments> =
            listOf(
                Arguments.of("the same path", "/home/isaac/monorepo", "/home/isaac/monorepo", true),
                Arguments.of(
                    "a folder nested inside the bound project",
                    "/home/isaac/monorepo",
                    "/home/isaac/monorepo/packages/api",
                    true,
                ),
                Arguments.of(
                    "a sibling directory",
                    "/home/isaac/sudoku-solver",
                    "/home/isaac/tic-tac-toe",
                    false,
                ),
                Arguments.of(
                    "a bind below the open folder",
                    "/home/isaac/monorepo/api",
                    "/home/isaac/monorepo",
                    false,
                ),
                Arguments.of(
                    "a prefix that is not a path boundary",
                    "/home/isaac/mono",
                    "/home/isaac/monorepo",
                    false,
                ),
                Arguments.of(
                    "no bound path at all",
                    "",
                    "/home/isaac/monorepo",
                    false,
                ),
            )

        @JvmStatic
        fun alignments(): List<Arguments> =
            listOf(
                Arguments.of(
                    "a host path that names another directory is a mismatch",
                    "/home/isaac/sudoku-solver",
                    "/home/isaac/tic-tac-toe",
                    WorkspaceAlignment.MISALIGNED,
                ),
                Arguments.of(
                    "the open folder covered by the proxy's project dir is aligned",
                    "/home/isaac/monorepo",
                    "/home/isaac/monorepo/packages/api",
                    WorkspaceAlignment.ALIGNED,
                ),
                Arguments.of(
                    "a relative project dir is never trusted",
                    ".",
                    "/home/isaac/monorepo",
                    WorkspaceAlignment.UNKNOWN,
                ),
                Arguments.of(
                    "an empty project dir (the proxy cannot say) is unknown",
                    "",
                    "/home/isaac/monorepo",
                    WorkspaceAlignment.UNKNOWN,
                ),
                Arguments.of(
                    "a missing project dir is unknown",
                    null,
                    "/home/isaac/monorepo",
                    WorkspaceAlignment.UNKNOWN,
                ),
                Arguments.of(
                    "no folder open is unknown, not a mismatch",
                    "/home/isaac/monorepo",
                    null,
                    WorkspaceAlignment.UNKNOWN,
                ),
                Arguments.of(
                    "an empty local root is unknown",
                    "/home/isaac/monorepo",
                    "",
                    WorkspaceAlignment.UNKNOWN,
                ),
            )
    }
}

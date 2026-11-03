package com.inferstep.atlas.ui

import com.intellij.openapi.vfs.LocalFileSystem
import com.intellij.openapi.vfs.VfsUtilCore
import com.intellij.testFramework.fixtures.BasePlatformTestCase
import java.nio.file.Files

/**
 * The proxy writes through its bind mount; the plugin only asks the IDE to
 * look again. This exercises that adapter with a real file outside the test
 * project, in the case a new file in the workspace is: the VFS already knows
 * the folder and its children, and has never seen the new path.
 */
class AtlasWorkspaceRefreshTest : BasePlatformTestCase() {
    fun testRefreshesANewFileIntoTheVfs() {
        val fs = LocalFileSystem.getInstance()
        val root = Files.createTempDirectory("atlas-vfs-refresh").toFile()
        try {
            // Listing the folder's children makes the VFS trust that list: a
            // later lookup of a name it does not hold answers null without
            // looking at the disk. Without this, the lookup itself would find
            // the file and the test would pass with no refresh at all.
            val folder = fs.refreshAndFindFileByPath(root.path)
            assertNotNull("the VFS must know the workspace folder", folder)
            assertEquals(0, folder!!.children.size)
            val file =
                root.resolve("new.py").apply {
                    writeText("print(1)\n")
                }
            assertNull("the VFS must not see the new file yet", fs.findFileByPath(file.path))

            refreshWorkspacePath(root.path, "new.py")

            val virtual = fs.findFileByPath(file.path)
            assertNotNull("the new file must be in the VFS after the refresh", virtual)
            assertEquals("print(1)\n", VfsUtilCore.loadText(virtual!!))
        } finally {
            root.deleteRecursively()
        }
    }
}

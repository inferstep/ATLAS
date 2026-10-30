// The IDE half of diff review: open the platform's own side-by-side diff, and
// tell its VFS that the proxy changed a file.
//
// The diff content is decided entirely in session/ChatSession (predicted change
// at permission time, exact change after a successful tool_result), so this
// file is only the adapter the platform needs: it takes two strings and shows
// them. Nothing here is unit-tested — the reasoning lives in the session layer,
// which is — and the smoke run in an IDE is what proves it opens.
package com.inferstep.atlas.ui

import com.intellij.diff.DiffContentFactory
import com.intellij.diff.DiffManager
import com.intellij.diff.requests.SimpleDiffRequest
import com.intellij.openapi.fileTypes.FileType
import com.intellij.openapi.fileTypes.FileTypeManager
import com.intellij.openapi.project.Project
import com.intellij.openapi.util.io.FileUtil
import com.intellij.openapi.vfs.LocalFileSystem
import java.io.File

/** Opens the IDE's diff window for two pieces of text. */
object AtlasDiffViewer {
    /**
     * A side-by-side diff of [left] against [right], titled [title].
     *
     * [path] is the tool's target path; only its name is used, to pick a
     * [FileType] so both panes get the IDE's syntax highlighting. The contents
     * are plain strings, so a path the IDE does not know degrades to plain
     * text rather than failing.
     */
    fun show(
        project: Project,
        title: String,
        path: String,
        left: String,
        right: String,
    ) {
        val factory = DiffContentFactory.getInstance()
        val fileType = fileTypeOf(path)
        val request =
            SimpleDiffRequest(
                title,
                factory.create(project, left, fileType),
                factory.create(project, right, fileType),
                "before",
                "after",
            )
        DiffManager.getInstance().showDiff(project, request)
    }

    private fun fileTypeOf(path: String): FileType =
        FileTypeManager.getInstance().getFileTypeByFileName(File(path).name)
}

/**
 * Tell the IDE a path changed, by its workspace-relative name.
 *
 * The proxy writes through its bind mount, so IntelliJ's own VFS watcher
 * usually notices first. This is the belt: called right after a successful file
 * op, it guarantees a brand-new file appears even when the tool window is the
 * only thing looking at the directory.
 */
fun refreshWorkspacePath(
    root: String,
    relativePath: String,
) {
    if (root.isEmpty() || relativePath.isEmpty()) {
        return
    }
    val absolute = FileUtil.toSystemIndependentName(File(File(root), relativePath).path)
    LocalFileSystem.getInstance().refreshAndFindFileByPath(absolute)?.refresh(false, false)
}

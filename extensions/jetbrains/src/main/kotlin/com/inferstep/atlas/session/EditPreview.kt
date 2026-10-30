// Client-side prediction of what a file-writing tool call will do, computed at
// permission time from the CURRENT local file. Needed because the diff has to
// be shown BEFORE the user allows the call — and for structural_edit even the
// result carries no new content (proxy/types.go StructuralEditOutput is
// ok/selector/language/byte counts only), so the post-state can only be
// predicted here.
//
// A port of extensions/vscode/src/session/editPreview.ts. Accuracy contract per
// tool:
//   - write_file: exact (disk-or-empty vs `content`).
//   - edit_file: exact when old_str occurs (first occurrence, or all with
//     replace_all); otherwise a snippet diff of old_str vs new_str plus a note
//     — the proxy will likely reject that call anyway.
//   - structural_edit: best-effort regex/indent splice for python
//     function:NAME / class:NAME (decorator-aware) and a naive top-level <tag>
//     scan for HTML. Tree-sitter is the server-side authority (v3-service), so
//     every structural_edit prediction is labelled approximate. The exact view
//     comes later: ChatSession snapshots the file at tool_call time and diffs
//     snapshot vs on-disk after the tool_result.
//
// Deliberately free of com.intellij and of Compose, so this runs under plain
// JUnit.
package com.inferstep.atlas.session

import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.intOrNull

/** The tools whose permission prompts and tool chips get diffs. move_file /
 * delete_file / run_command are notification-only. */
val FILE_EDIT_TOOLS: Set<String> =
    setOf("write_file", "edit_file", "structural_edit", "insert_after", "replace_lines")

/** How a prediction's two sides are read. */
enum class EditPredictionKind {
    /** `left`/`right` are whole-file states. */
    FILE,

    /** `left`/`right` are just old_str/new_str — an edit_file whose old_str is
     * absent from the file, so there is no post-state to predict. */
    SNIPPET,
}

/** What a file-writing call is predicted to do, as a diff. */
data class EditPrediction(
    /** Target path exactly as the model sent it (proxy-workspace-relative). */
    val path: String,
    val kind: EditPredictionKind,
    val left: String,
    val right: String,
    /** True when the prediction is a best-effort guess rather than the exact
     * post-state (all structural_edit predictions, and edit_file snippet mode). */
    val approximate: Boolean,
    val note: String? = null,
)

/** What the IDE's diff tab is called for a predicted change. */
fun EditPrediction.diffTitle(tool: String): String {
    val qualifier =
        when {
            kind == EditPredictionKind.SNIPPET -> "snippet"
            approximate -> "proposed · approximate"
            else -> "proposed"
        }
    return "ATLAS: $tool $path ($qualifier)"
}

/** Path a file-edit tool call targets, or null for other tools / malformed
 * args. Used to read the local file and to key snapshots. */
fun editTargetPath(
    tool: String,
    args: JsonElement?,
): String? {
    if (tool !in FILE_EDIT_TOOLS) {
        return null
    }
    return stringField(args, "path")
}

/** Predict the post-edit file state. `current` is the local file content, or
 * null when the file does not exist in this workspace. Returns null for
 * non-edit tools or args missing required fields. */
fun predictEdit(
    tool: String,
    args: JsonElement?,
    current: String?,
): EditPrediction? {
    val path = editTargetPath(tool, args) ?: return null
    return when (tool) {
        "write_file" -> {
            val content = stringField(args, "content") ?: return null
            EditPrediction(
                path = path,
                kind = EditPredictionKind.FILE,
                left = current.orEmpty(),
                right = content,
                approximate = false,
                note = if (current == null) "new file" else null,
            )
        }

        "edit_file" -> {
            val oldStr = stringField(args, "old_str") ?: return null
            val newStr = stringField(args, "new_str") ?: return null
            predictEditFile(
                path,
                current,
                oldStr,
                newStr,
                replaceAll = boolField(args, "replace_all"),
            )
        }

        "structural_edit" -> {
            val selector = stringField(args, "selector") ?: return null
            val content = stringField(args, "content") ?: return null
            predictStructuralEdit(path, current, selector, content)
        }

        // insert_after is exact: it names a line number rather than text to
        // reproduce, so there is nothing to match and nothing to guess.
        "insert_after" -> {
            val content = stringField(args, "content") ?: return null
            val line = intField(args, "line") ?: return null
            predictInsertAfter(path, current ?: return null, line, content)
        }

        // replace_lines is exact too: the range is line numbers, not text.
        "replace_lines" -> {
            val content = stringField(args, "content") ?: return null
            val start = intField(args, "start_line") ?: return null
            val end = intField(args, "end_line") ?: return null
            predictReplaceLines(path, current ?: return null, start, end, content)
        }

        else -> {
            null
        }
    }
}

/** Replace 1-based lines `start`..`end` (inclusive) with `content`. Mirrors the
 * proxy: an out-of-range or inverted span is not previewable. */
private fun predictReplaceLines(
    path: String,
    current: String,
    start: Int,
    end: Int,
    content: String,
): EditPrediction? {
    val hadTrailingNewline = current.endsWith("\n")
    val lines = current.split("\n").toMutableList()
    if (hadTrailingNewline) {
        lines.removeAt(lines.lastIndex)
    }
    if (start < 1 || end < start || end > lines.size) {
        return null
    }
    val replacement = content.split("\n").toMutableList()
    if (content.endsWith("\n")) {
        replacement.removeAt(replacement.lastIndex)
    }
    val next = lines.subList(0, start - 1) + replacement + lines.subList(end, lines.size)
    return EditPrediction(
        path = path,
        kind = EditPredictionKind.FILE,
        left = current,
        right = next.joinToString("\n") + if (hadTrailingNewline) "\n" else "",
        approximate = false,
        note = if (start == end) "replaced line $start" else "replaced lines $start-$end",
    )
}

/** Splice `content` in after 1-based `line`; 0 puts it at the top of the file.
 * A line past the end appends, which is what the proxy does. */
private fun predictInsertAfter(
    path: String,
    current: String,
    line: Int,
    content: String,
): EditPrediction? {
    if (line < 0) {
        return null
    }
    val hadTrailingNewline = current.endsWith("\n")
    val lines = current.split("\n").toMutableList()
    if (hadTrailingNewline) {
        lines.removeAt(lines.lastIndex) // split() leaves a trailing "" — not a real line
    }
    val at = minOf(line, lines.size)
    val inserted = content.split("\n").toMutableList()
    if (content.endsWith("\n")) {
        inserted.removeAt(inserted.lastIndex)
    }
    val next = lines.subList(0, at) + inserted + lines.subList(at, lines.size)
    return EditPrediction(
        path = path,
        kind = EditPredictionKind.FILE,
        left = current,
        right = next.joinToString("\n") + if (hadTrailingNewline) "\n" else "",
        approximate = false,
        note = if (line == 0) "inserted at top of file" else "inserted after line $line",
    )
}

private fun predictEditFile(
    path: String,
    current: String?,
    oldStr: String,
    newStr: String,
    replaceAll: Boolean,
): EditPrediction {
    if (current == null) {
        return EditPrediction(
            path = path,
            kind = EditPredictionKind.SNIPPET,
            left = oldStr,
            right = newStr,
            approximate = true,
            note = "target file not found in this workspace — showing old_str vs new_str",
        )
    }
    val index = current.indexOf(oldStr)
    if (oldStr.isEmpty() || index == -1) {
        return EditPrediction(
            path = path,
            kind = EditPredictionKind.SNIPPET,
            left = oldStr,
            right = newStr,
            approximate = true,
            note =
                "old_str not found in the file — showing old_str vs new_str " +
                    "(the proxy will likely reject this edit)",
        )
    }
    // split/join instead of a regex replacement: new_str is spliced literally
    // ($& and friends have no meaning here).
    if (replaceAll) {
        return EditPrediction(
            path = path,
            kind = EditPredictionKind.FILE,
            left = current,
            right = current.split(oldStr).joinToString(newStr),
            approximate = false,
        )
    }
    // Without replace_all the proxy requires a UNIQUE match and rejects the
    // call otherwise (proxy/tools.go uniqueness check) — a first-occurrence
    // splice labelled exact would show a diff that can never apply.
    val count = current.split(oldStr).size - 1
    val firstOccurrence =
        current.substring(0, index) + newStr + current.substring(index + oldStr.length)
    if (count > 1) {
        return EditPrediction(
            path = path,
            kind = EditPredictionKind.FILE,
            left = current,
            right = firstOccurrence,
            approximate = true,
            note =
                "old_str matches $count locations — the proxy requires a unique match " +
                    "and will reject this edit (showing the first occurrence)",
        )
    }
    return EditPrediction(
        path,
        EditPredictionKind.FILE,
        current,
        firstOccurrence,
        approximate = false,
    )
}

private fun predictStructuralEdit(
    path: String,
    current: String?,
    selector: String,
    content: String,
): EditPrediction {
    if (current == null) {
        return EditPrediction(
            path = path,
            kind = EditPredictionKind.FILE,
            left = "",
            right = content,
            approximate = true,
            note =
                "target file not found in this workspace — showing the proposed node " +
                    "content only",
        )
    }
    val trimmed = selector.trim()
    val fn = Regex("^function:([A-Za-z_]\\w*)$").find(trimmed)
    val cls = Regex("^class:([A-Za-z_]\\w*)$").find(trimmed)
    val tag = Regex("^<([A-Za-z][\\w-]*)>$").find(trimmed)
    if (fn != null || cls != null) {
        val name = (fn ?: cls)!!.groupValues[1]
        val kind = if (fn != null) "function" else "class"
        val spliced = splicePythonNode(current, kind, name, content)
        if (spliced != null) {
            return EditPrediction(
                path = path,
                kind = EditPredictionKind.FILE,
                left = current,
                right = spliced.right,
                approximate = true,
                note = spliced.note ?: "predicted client-side — tree-sitter runs server-side",
            )
        }
    } else if (tag != null) {
        var body = content
        if (tag.groupValues[1].lowercase() == "html") {
            // Mirror the proxy's <html>-selector quirk: the element replace
            // does not cover a preceding <!DOCTYPE>, so the proxy strips a
            // leading doctype from the content (proxy/tools.go).
            body = stripLeadingDoctype(body)
        }
        val spliced = spliceHtmlElement(current, tag.groupValues[1], body)
        if (spliced != null) {
            return EditPrediction(
                path = path,
                kind = EditPredictionKind.FILE,
                left = current,
                right = spliced,
                approximate = true,
                note = "predicted client-side — tree-sitter runs server-side",
            )
        }
    }
    return EditPrediction(
        path = path,
        kind = EditPredictionKind.FILE,
        left = current,
        right = content,
        approximate = true,
        note =
            "selector '$selector' not matched locally — showing the proposed content " +
                "against the whole file",
    )
}

private val DOCTYPE = Regex("^\\s*<!doctype[^>]*>\\r?\\n?", RegexOption.IGNORE_CASE)

private fun stripLeadingDoctype(content: String): String {
    val match = DOCTYPE.find(content) ?: return content
    return content.substring(match.value.length)
}

/** The result of a python splice: the new file and, when more than one
 * definition carries the name, the note that says which one was replaced. */
private data class Splice(
    val right: String,
    val note: String? = null,
)

/** Replace the named python function/class (plus contiguous decorator lines
 * directly above it) with `content`. Indent-scan only — no parser. */
private fun splicePythonNode(
    source: String,
    kind: String,
    name: String,
    content: String,
): Splice? {
    val lines = source.split("\n")
    val escaped = Regex.escape(name)
    val head =
        if (kind == "function") {
            Regex("^([ \t]*)(?:async[ \t]+)?def[ \t]+$escaped[ \t]*\\(")
        } else {
            Regex("^([ \t]*)class[ \t]+$escaped[ \t]*[(:]")
        }
    val matches = lines.indices.filter { head.containsMatchIn(lines[it]) }
    if (matches.isEmpty()) {
        return null
    }
    val defLine = matches.first()
    val indent = lines[defLine].takeWhile { it == ' ' || it == '\t' }

    // Decorators are part of the node (proxy behaviour: "decorators included
    // automatically"). Naive: contiguous same-indent lines starting with '@'.
    var start = defLine
    while (start > 0 && lines[start - 1].startsWith("$indent@")) {
        start--
    }

    // Body ends at the last line indented deeper than the def; blank lines
    // inside the body are skipped, trailing blanks stay outside the node.
    var lastNonEmpty = defLine
    for (i in defLine + 1 until lines.size) {
        if (lines[i].trim().isEmpty()) {
            continue
        }
        val lineIndent = lines[i].takeWhile { c -> c == ' ' || c == '\t' }
        if (lineIndent.length <= indent.length) {
            break
        }
        lastNonEmpty = i
    }

    val before = lines.subList(0, start)
    val spliced = content.split("\n")
    val after = lines.subList(lastNonEmpty + 1, lines.size)
    val right = (before + spliced + after).joinToString("\n")
    val note =
        if (matches.size > 1) {
            "${matches.size} definitions named '$name' — the first one is shown replaced"
        } else {
            null
        }
    return Splice(right, note)
}

/** Replace the first top-level `<tag>...</tag>` element (nesting-aware for the
 * same tag name, self-closing handled) with `content`. Naive text scan. */
private fun spliceHtmlElement(
    source: String,
    tag: String,
    content: String,
): String? {
    val tokenRe = Regex("<(/?)${Regex.escape(tag)}(?=[\\s/>])", RegexOption.IGNORE_CASE)
    var depth = 0
    var start = -1
    for (match in tokenRe.findAll(source)) {
        val gt = source.indexOf('>', match.range.first)
        if (gt == -1) {
            return null
        }
        val closing = match.groupValues[1] == "/"
        if (!closing) {
            val selfClosing = source[gt - 1] == '/'
            if (depth == 0) {
                start = match.range.first
                if (selfClosing) {
                    return source.substring(0, start) + content + source.substring(gt + 1)
                }
            }
            if (!selfClosing) {
                depth++
            }
        } else {
            depth--
            if (depth == 0 && start != -1) {
                return source.substring(0, start) + content + source.substring(gt + 1)
            }
            if (depth < 0) {
                return null
            }
        }
    }
    return null
}

private fun stringField(
    args: JsonElement?,
    key: String,
): String? {
    val value = (args as? JsonObject)?.get(key) as? JsonPrimitive ?: return null
    return if (value.isString) value.content else null
}

/** Integer field, rejecting a JSON string: `{"line":"2"}` is not a line the
 * model named as a number, and the proxy rejects it too. */
private fun intField(
    args: JsonElement?,
    key: String,
): Int? {
    val value = (args as? JsonObject)?.get(key) as? JsonPrimitive ?: return null
    if (value.isString) {
        return null
    }
    return value.intOrNull
}

/** Boolean field, rejecting a JSON string: `{"replace_all":"true"}` is not a
 * boolean the model set, and the reference client reads only the boolean.
 * Same rule as [intField]. */
private fun boolField(
    args: JsonElement?,
    key: String,
): Boolean {
    val value = (args as? JsonObject)?.get(key) as? JsonPrimitive ?: return false
    if (value.isString) {
        return false
    }
    return value.booleanOrNull == true
}

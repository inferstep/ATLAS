// One-line human summary of a `tool_result` payload, for the tool chip.
//
// Ported from extensions/vscode/src/session/toolSummary.ts. The failure it
// fixes: the proxy sends the tool's real output in `data` and the client used
// to drop it, so "list the files" produced a green check, a duration, and no
// files. The answer to a read-only request IS the tool output.
//
// Shapes are per-tool and the proxy is the source of truth (proxy/tool*.go
// output structs). Anything unrecognised falls back to the generic keys the
// TUI already looks for, so a new tool degrades to the old chip rather than
// lying about what happened.
package com.inferstep.atlas.session

import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive

/** Entries shown before the tail is collapsed into "+N more". */
private const val ENTRY_LIMIT = 12

/** The generic keys the TUI reads when a tool has no shape of its own. */
private val GENERIC_KEYS = listOf("summary", "stdout", "content", "message")

/**
 * A one-line summary of a tool result, or null when the shape is not one this
 * client knows — a chip that says only "ran" is honest; an invented summary is
 * not.
 */
fun summarizeToolResult(
    tool: String,
    data: JsonElement?,
): String? {
    val outer = data as? JsonObject ?: return null
    // The proxy nests the tool's own output under `data`.
    val inner = outer["data"] as? JsonObject ?: outer

    val entries = inner["entries"] as? JsonArray
    if (entries != null) {
        return summarizeEntries(entries)
    }

    val matches = inner["matches"] as? JsonArray
    if (matches != null) {
        return when (matches.size) {
            0 -> "no matches"
            1 -> "1 match"
            else -> "${matches.size} matches"
        }
    }

    GENERIC_KEYS.forEach { key ->
        val text = stringAt(inner, key) ?: return@forEach
        val trimmed = text.trim()
        if (trimmed.isEmpty()) {
            return@forEach
        }
        val lines = text.split("\n").size
        return if (lines > 1) "$lines lines" else trimmed
    }
    return null
}

/** `list_directory`'s answer: how many entries, and which ones. */
private fun summarizeEntries(entries: JsonArray): String {
    if (entries.isEmpty()) {
        return "empty directory"
    }
    val shown = entries.take(ENTRY_LIMIT).map { entryName(it) }
    val rest = entries.size - shown.size
    val joined = shown.joinToString(", ")
    val tail = if (rest > 0) " (+$rest more)" else ""
    return "${entries.size} entries: $joined$tail"
}

/** A directory is marked with a trailing slash, as the reference client does. */
private fun entryName(entry: JsonElement): String {
    val obj = entry as? JsonObject ?: return ""
    val name = stringAt(obj, "name") ?: return ""
    return if (stringAt(obj, "type") == "dir") "$name/" else name
}

private fun stringAt(
    obj: JsonObject,
    key: String,
): String? {
    val value = obj[key] as? JsonPrimitive ?: return null
    return if (value.isString) value.content else null
}

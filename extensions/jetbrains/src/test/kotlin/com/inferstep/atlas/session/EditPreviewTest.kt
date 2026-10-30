// The predicted diff a permission prompt can show: which tools name a path,
// and what the post-state is for each of them.
//
// Tables, because every case in a table is the same question asked with other
// arguments: given this tool and these args, which path; and given this tool,
// these args and this local file, what diff — or none, which is what an
// unpredictable call gets. The two line-number tools have a table of their own,
// of plain values. A port of extensions/vscode/test/editPreview.test.ts.
package com.inferstep.atlas.session

import com.inferstep.atlas.protocol.AtlasJson
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.params.ParameterizedTest
import org.junit.jupiter.params.provider.Arguments
import org.junit.jupiter.params.provider.CsvSource
import org.junit.jupiter.params.provider.MethodSource

private const val SERVER_SIDE_NOTE = "predicted client-side — tree-sitter runs server-side"

/** The python fixture: a decorated function with a sibling after it. */
private val PY =
    listOf(
        "import os",
        "",
        "@app.route('/dash')",
        "@login_required",
        "def dashboard():",
        "    users = get_users()",
        "",
        "    return render(users)",
        "",
        "def other():",
        "    pass",
    ).joinToString("\n")

/** The html fixture: doctype, head, and a body holding two nested divs. */
private val HTML =
    listOf(
        "<!DOCTYPE html>",
        "<html>",
        "<head><title>t</title></head>",
        "<body>",
        "  <div class=\"outer\"><div>inner</div></div>",
        "</body>",
        "</html>",
    ).joinToString("\n")

class EditPreviewTest {
    @ParameterizedTest(name = "{0}")
    @MethodSource("targets")
    fun `names the path of a file-edit tool, and null for anything else`(
        name: String,
        tool: String,
        argsJson: String,
        want: String?,
    ) {
        assertEquals(want, editTargetPath(tool, AtlasJson.parseToJsonElement(argsJson)), name)
    }

    @ParameterizedTest(name = "{1}: {0}")
    @MethodSource("predictions")
    fun `predicts the change a file-edit call will make, or none when it cannot`(
        name: String,
        tool: String,
        argsJson: String,
        current: String?,
        want: EditPrediction?,
    ) {
        val args = AtlasJson.parseToJsonElement(argsJson)

        assertEquals(want, predictEdit(tool, args, current), "$tool: $name")
    }

    // insert_after and replace_lines edit f.py by line number. The columns: the case, the tool,
    // the call's args besides the path, the file before the call (empty: not readable locally),
    // and the file after it with the note the preview shows (empty: no preview). In the two file
    // columns, \n is a line break.
    @ParameterizedTest(name = "{1}: {0}")
    @CsvSource(
        delimiter = '|',
        textBlock = """
        replaces an inclusive range, exactly | replace_lines | "start_line":2,"end_line":3,"content":"X\nY" | a\nb\nc\nd\n | a\nX\nY\nd\n | replaced lines 2-3
        replaces a single line | replace_lines | "start_line":1,"end_line":1,"content":"Z" | a\nb\nc\nd\n | Z\nb\nc\nd\n | replaced line 1
        preserves a missing trailing newline | replace_lines | "start_line":2,"end_line":2,"content":"X" | a\nb | a\nX | replaced line 2
        declines a range that starts before the file | replace_lines | "start_line":0,"end_line":1,"content":"X" | a\nb\nc\nd\n |  |
        declines an inverted range | replace_lines | "start_line":3,"end_line":2,"content":"X" | a\nb\nc\nd\n |  |
        declines a range past the end | replace_lines | "start_line":2,"end_line":99,"content":"X" | a\nb\nc\nd\n |  |
        declines a missing end line | replace_lines | "start_line":1,"content":"X" | a\nb\nc\nd\n |  |
        declines a line number sent as a string | replace_lines | "start_line":"1","end_line":2,"content":"X" | a\nb\nc\nd\n |  |
        declines a file that is not readable locally | replace_lines | "start_line":1,"end_line":2,"content":"X" |  |  |
        inserts after a 1-based line number, and is exact | insert_after | "line":2,"content":"X" | a\nb\nc\n | a\nb\nX\nc\n | inserted after line 2
        line 0 is the top of the file | insert_after | "line":0,"content":"X" | a\nb\nc\n | X\na\nb\nc\n | inserted at top of file
        a line past the end appends | insert_after | "line":99,"content":"X" | a\nb\nc\n | a\nb\nc\nX\n | inserted after line 99
        preserves a missing trailing newline | insert_after | "line":1,"content":"X" | a\nb | a\nX\nb | inserted after line 1
        does not double a newline the content already carries | insert_after | "line":1,"content":"X\n" | a\nb\nc\n | a\nX\nb\nc\n | inserted after line 1
        handles multi-line content | insert_after | "line":1,"content":"X\nY" | a\nb\nc\n | a\nX\nY\nb\nc\n | inserted after line 1
        no line at all is not previewable | insert_after | "content":"X" | a\n |  |
        a line sent as a string is not a number | insert_after | "line":"2","content":"X" | a\n |  |
        a fractional line is not a line | insert_after | "line":1.5,"content":"X" | a\n |  |
        a negative line is refused | insert_after | "line":-1,"content":"X" | a\n |  |
        a file that is not readable locally | insert_after | "line":1,"content":"X" |  |  |""",
    )
    fun `predicts a line-number edit exactly, or none when it cannot`(
        name: String,
        tool: String,
        argsBesidesThePath: String,
        before: String?,
        after: String?,
        note: String?,
    ) {
        val args = AtlasJson.parseToJsonElement("""{"path":"f.py",$argsBesidesThePath}""")
        val current = before?.replace("\\n", "\n")
        val want =
            after?.let {
                EditPrediction(
                    "f.py",
                    EditPredictionKind.FILE,
                    current!!,
                    it.replace("\\n", "\n"),
                    false,
                    note!!,
                )
            }

        assertEquals(want, predictEdit(tool, args, current), "$tool: $name")
    }

    companion object {
        @JvmStatic
        fun targets(): List<Arguments> =
            listOf(
                Arguments.of(
                    "write_file",
                    "write_file",
                    """{"path":"a.py","content":"x"}""",
                    "a.py",
                ),
                Arguments.of("edit_file", "edit_file", """{"path":"a.py"}""", "a.py"),
                Arguments.of("structural_edit", "structural_edit", """{"path":"a.py"}""", "a.py"),
                Arguments.of("insert_after", "insert_after", """{"path":"f.py"}""", "f.py"),
                Arguments.of("replace_lines", "replace_lines", """{"path":"f.py"}""", "f.py"),
                Arguments.of(
                    "delete_file names a path, but is not a file edit",
                    "delete_file",
                    """{"path":"a.py"}""",
                    null,
                ),
                Arguments.of("no args at all", "write_file", "null", null),
                Arguments.of("a path that is not a string", "write_file", """{"path":42}""", null),
            )

        @JvmStatic
        fun predictions(): List<Arguments> =
            listOf(
                // write_file: exact, the file on disk (or nothing) against the new content.
                Arguments.of(
                    "a new file shows empty on the left and its content on the right",
                    "write_file",
                    """{"path":"new.py","content":"x = 1\n"}""",
                    null,
                    EditPrediction(
                        "new.py",
                        EditPredictionKind.FILE,
                        "",
                        "x = 1\n",
                        false,
                        "new file",
                    ),
                ),
                Arguments.of(
                    "an existing file shows the old content against the new",
                    "write_file",
                    """{"path":"a.py","content":"new"}""",
                    "old",
                    EditPrediction("a.py", EditPredictionKind.FILE, "old", "new", false),
                ),
                Arguments.of(
                    "a missing content field is not previewable",
                    "write_file",
                    """{"path":"a.py"}""",
                    "old",
                    null,
                ),
                // edit_file: splices old_str, or falls back to a snippet.
                Arguments.of(
                    "a unique old_str is an exact single-occurrence replacement",
                    "edit_file",
                    """{"path":"a.txt","old_str":"aa","new_str":"bb"}""",
                    "aa x cc",
                    EditPrediction("a.txt", EditPredictionKind.FILE, "aa x cc", "bb x cc", false),
                ),
                Arguments.of(
                    "two matches without replace_all is approximate — the proxy rejects it",
                    "edit_file",
                    """{"path":"a.txt","old_str":"aa","new_str":"bb"}""",
                    "aa x aa",
                    EditPrediction(
                        "a.txt",
                        EditPredictionKind.FILE,
                        "aa x aa",
                        "bb x aa",
                        true,
                        "old_str matches 2 locations — the proxy requires a unique match and " +
                            "will reject this edit (showing the first occurrence)",
                    ),
                ),
                Arguments.of(
                    "replace_all replaces every occurrence",
                    "edit_file",
                    """{"path":"a.txt","old_str":"aa","new_str":"bb","replace_all":true}""",
                    "aa x aa",
                    EditPrediction("a.txt", EditPredictionKind.FILE, "aa x aa", "bb x bb", false),
                ),
                Arguments.of(
                    "replace_all as the string \"true\" is no boolean: one replacement, approximate",
                    "edit_file",
                    """{"path":"a.txt","old_str":"aa","new_str":"bb","replace_all":"true"}""",
                    "aa x aa",
                    EditPrediction(
                        "a.txt",
                        EditPredictionKind.FILE,
                        "aa x aa",
                        "bb x aa",
                        true,
                        "old_str matches 2 locations — the proxy requires a unique match and " +
                            "will reject this edit (showing the first occurrence)",
                    ),
                ),
                Arguments.of(
                    "new_str is spliced literally, with no \$-pattern semantics",
                    "edit_file",
                    """{"path":"a.txt","old_str":"X","new_str":"${'$'}& ${'$'}' ${'$'}1"}""",
                    "a X b",
                    EditPrediction(
                        "a.txt",
                        EditPredictionKind.FILE,
                        "a X b",
                        "a \$& \$' \$1 b",
                        false,
                    ),
                ),
                Arguments.of(
                    "old_str not found: a snippet diff plus the note",
                    "edit_file",
                    """{"path":"a.txt","old_str":"zz","new_str":"bb"}""",
                    "aa x aa",
                    EditPrediction(
                        "a.txt",
                        EditPredictionKind.SNIPPET,
                        "zz",
                        "bb",
                        true,
                        "old_str not found in the file — showing old_str vs new_str " +
                            "(the proxy will likely reject this edit)",
                    ),
                ),
                Arguments.of(
                    "file missing locally: a snippet diff plus the workspace note",
                    "edit_file",
                    """{"path":"a.txt","old_str":"zz","new_str":"bb"}""",
                    null,
                    EditPrediction(
                        "a.txt",
                        EditPredictionKind.SNIPPET,
                        "zz",
                        "bb",
                        true,
                        "target file not found in this workspace — showing old_str vs new_str",
                    ),
                ),
                // structural_edit on python: splices the named node, or says it could not.
                Arguments.of(
                    "function: replaces the def block including its decorators",
                    "structural_edit",
                    """{"path":"app.py","selector":"function:dashboard",""" +
                        """"content":"@app.route('/dash')\ndef dashboard():\n    return quick()"}""",
                    PY,
                    EditPrediction(
                        "app.py",
                        EditPredictionKind.FILE,
                        PY,
                        listOf(
                            "import os",
                            "",
                            "@app.route('/dash')",
                            "def dashboard():",
                            "    return quick()",
                            "",
                            "def other():",
                            "    pass",
                        ).joinToString("\n"),
                        true,
                        SERVER_SIDE_NOTE,
                    ),
                ),
                Arguments.of(
                    "function: matches async def too",
                    "structural_edit",
                    """{"path":"a.py","selector":"function:fetch",""" +
                        """"content":"async def fetch():\n    return 2"}""",
                    "async def fetch():\n    return await go()\n\nprint(1)",
                    EditPrediction(
                        "a.py",
                        EditPredictionKind.FILE,
                        "async def fetch():\n    return await go()\n\nprint(1)",
                        "async def fetch():\n    return 2\n\nprint(1)",
                        true,
                        SERVER_SIDE_NOTE,
                    ),
                ),
                Arguments.of(
                    "class: replaces the class body and keeps its surroundings",
                    "structural_edit",
                    """{"path":"a.py","selector":"class:B","content":"class B(Base):\n    x = 9"}""",
                    "class A:\n    def m(self):\n        pass\n\nclass B(Base):\n    x = 1\n\ntail = 2",
                    EditPrediction(
                        "a.py",
                        EditPredictionKind.FILE,
                        "class A:\n    def m(self):\n        pass\n\nclass B(Base):\n    x = 1\n\ntail = 2",
                        "class A:\n    def m(self):\n        pass\n\nclass B(Base):\n    x = 9\n\ntail = 2",
                        true,
                        SERVER_SIDE_NOTE,
                    ),
                ),
                Arguments.of(
                    "no match: the whole file against the content, labelled approximate",
                    "structural_edit",
                    """{"path":"app.py","selector":"function:missing",""" +
                        """"content":"def missing():\n    pass"}""",
                    PY,
                    EditPrediction(
                        "app.py",
                        EditPredictionKind.FILE,
                        PY,
                        "def missing():\n    pass",
                        true,
                        "selector 'function:missing' not matched locally — showing the proposed " +
                            "content against the whole file",
                    ),
                ),
                Arguments.of(
                    "two definitions with the name: the first is replaced, and the note says so",
                    "structural_edit",
                    """{"path":"a.py","selector":"function:f","content":"def f():\n    return 9"}""",
                    "def f():\n    pass\n\ndef f():\n    return 1",
                    EditPrediction(
                        "a.py",
                        EditPredictionKind.FILE,
                        "def f():\n    pass\n\ndef f():\n    return 1",
                        "def f():\n    return 9\n\ndef f():\n    return 1",
                        true,
                        "2 definitions named 'f' — the first one is shown replaced",
                    ),
                ),
                // structural_edit on html: splices a top-level element, or says it could not.
                Arguments.of(
                    "<body>: the whole element is replaced, nesting ignored",
                    "structural_edit",
                    """{"path":"i.html","selector":"<body>","content":"<body><p>new</p></body>"}""",
                    HTML,
                    EditPrediction(
                        "i.html",
                        EditPredictionKind.FILE,
                        HTML,
                        listOf(
                            "<!DOCTYPE html>",
                            "<html>",
                            "<head><title>t</title></head>",
                            "<body><p>new</p></body>",
                            "</html>",
                        ).joinToString("\n"),
                        true,
                        SERVER_SIDE_NOTE,
                    ),
                ),
                Arguments.of(
                    "<div>: the matching close is found past the nested same-tag",
                    "structural_edit",
                    """{"path":"i.html","selector":"<div>","content":"<div>x</div>"}""",
                    HTML,
                    EditPrediction(
                        "i.html",
                        EditPredictionKind.FILE,
                        HTML,
                        listOf(
                            "<!DOCTYPE html>",
                            "<html>",
                            "<head><title>t</title></head>",
                            "<body>",
                            "  <div>x</div>",
                            "</body>",
                            "</html>",
                        ).joinToString("\n"),
                        true,
                        SERVER_SIDE_NOTE,
                    ),
                ),
                Arguments.of(
                    "<html>: a leading doctype in the content is stripped (the proxy's quirk)",
                    "structural_edit",
                    """{"path":"i.html","selector":"<html>",""" +
                        """"content":"<!DOCTYPE html>\n<html><body>n</body></html>"}""",
                    HTML,
                    EditPrediction(
                        "i.html",
                        EditPredictionKind.FILE,
                        HTML,
                        "<!DOCTYPE html>\n<html><body>n</body></html>",
                        true,
                        SERVER_SIDE_NOTE,
                    ),
                ),
                Arguments.of(
                    "tag absent: the whole-file approximate fallback",
                    "structural_edit",
                    """{"path":"i.html","selector":"<table>","content":"<table></table>"}""",
                    HTML,
                    EditPrediction(
                        "i.html",
                        EditPredictionKind.FILE,
                        HTML,
                        "<table></table>",
                        true,
                        "selector '<table>' not matched locally — showing the proposed content " +
                            "against the whole file",
                    ),
                ),
            )
    }
}

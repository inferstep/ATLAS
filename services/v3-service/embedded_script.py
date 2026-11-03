"""The JavaScript and CSS embedded in HTML: finding the <script> and <style>
blocks of an HTML file or of a Python string literal, parsing them, and
reading their syntax errors and render loops. The entry points,
embedded_script_check and embedded_region_outline, are in symbols.py."""

import contextlib
import re

from grammars import _HTML_LANG, _JS_LANG, _PY_LANG

# The functions here run only when symbols._embedded_available() is true.
with contextlib.suppress(ImportError):
    from grammars import _ts

# The names that symbols.py imports from this module.
__all__ = [
    "_EMBEDDED_HTML_EXTS", "_EMBEDDED_MAX_BYTES", "_EMBEDDED_MAX_FINDINGS", "_css_error", "_js_error",
    "_js_function_names", "_js_redeclaration", "_line_at", "_stopped_loop_findings", "embedded_script_blocks_for",
]


# --- embedded_script_check (finding F1) ---------------------------------------
#
# 2026-08-01 dogfooding: the model edited a Flask app whose entire UI is one
# HTML string (`HTML_TEMPLATE = """..."""` handed to render_template_string)
# and left a stray closing paren inside its <script> block:
#     else if(key === 'ArrowDown' && direction !== 'UP') nextDirection = 'DOWN');
# Every gate on the write path is structurally blind to that: the Python
# compiles (the JavaScript is string content), the server starts, `curl /`
# returns 200 — so the verification gate passed and `done` was accepted while
# the game was dead in the browser.
#
# This checker parses the JavaScript (and brace-balances the CSS) that lives
# INSIDE HTML — both in .html/.htm/.jinja/.jinja2 files and in Python string
# literals — and reports tree-sitter ERROR / MISSING nodes with a file line
# number.
#
# It backs a WRITE-BLOCKING gate, so it is conservative by construction:
# every ambiguous case yields NO finding rather than a guess. Specifically it
# declines to report when
#   - the script/style tag counts don't balance (a `</script>` inside a JS
#     string truncates the raw text and would produce a phantom error),
#   - the block is `<script src=...>` with no body, or carries a non-JS
#     `type` (text/template, application/json, importmap, ...),
#   - the block still contains template tags after placeholder masking
#     ({% %}, <% %>, multi-line {{ }}),
#   - masking the {{ }} placeholders makes the block parse (the "error" was
#     the template syntax, not the JavaScript),
#   - the block comes from a Python string literal that is an f-string, is
#     part of a concatenation, or contains a backslash escape (what Python
#     renders then differs from the source bytes we would be parsing).

_EMBEDDED_HTML_EXTS = (".html", ".htm", ".jinja", ".jinja2")

# Whole-source cap. Above this the walk is skipped rather than risking a
# multi-second parse inside a synchronous write gate.
_EMBEDDED_MAX_BYTES = 400_000

# Findings returned per call. The caller reports the first; the rest are
# telemetry.
_EMBEDDED_MAX_FINDINGS = 3

# `type` values whose <script> body is JavaScript. Anything else is data or
# another language (text/template, application/json, importmap, text/x-*) and
# must never be JS-parsed.
_JS_SCRIPT_TYPES = frozenset({
    "", "module", "text/javascript", "application/javascript",
    "text/ecmascript", "application/ecmascript",
})

# Template constructs. Expression placeholders can be masked (they stand where
# a value goes); statement tags cannot (they wrap arbitrary control flow), so a
# block still carrying one after masking is undecidable and gets no finding.
_TEMPLATE_MARKERS = (b"{{", b"{%", b"<%")
_JINJA_EXPR_RE = re.compile(rb"\{\{[^\n{}]*\}\}")

# Closing delimiters, for the "you left a stray X" hint.
_CLOSERS = {")": "(", "}": "{", "]": "["}


def _js_function_names(block: bytes) -> list:
    """Names of the functions declared in a JavaScript block, in source order.

    These are the names a model will try to select by, so the outline has to
    show them alongside the note that no selector reaches them.
    """
    parser = _ts.Parser(_JS_LANG)
    tree = parser.parse(block)
    names, seen = [], set()

    _add_js_function_names(tree.root_node, names, seen)
    return names


def _add_js_function_names(node, names: list, seen: set) -> None:
    """Add to `names` each function declared in `node` or below it that `seen`
    does not hold yet, in source order."""
    if node.type in ("function_declaration", "generator_function_declaration"):
        ident = node.child_by_field_name("name")
        if ident is not None:
            name = ident.text.decode("utf-8", "replace")
            if name not in seen:
                seen.add(name)
                names.append(name)
    elif node.type == "variable_declarator":
        value = node.child_by_field_name("value")
        ident = node.child_by_field_name("name")
        if (ident is not None and value is not None
                and value.type in ("function_expression", "arrow_function")):
            name = ident.text.decode("utf-8", "replace")
            if name not in seen:
                seen.add(name)
                names.append(name)
    for child in node.children:
        _add_js_function_names(child, names, seen)


def _blank_jinja_comments(block: bytes, blank: int = 0x20) -> bytes:
    """Overwrite `{# ... #}` comments with `blank`, preserving length and CR/LF.

    This was a regex (`\\{#[^\\n]*#\\}`) and cannot go back to being one. With
    many `{#` and no closer, every opener rescans to end-of-line, which is
    quadratic on bytes the model chose: 18ms at 1.6k openers, 15.9s at 25k.
    Scanning with `find` is linear, because a line with no `#}` after its first
    `{#` has none after any later one either, so that line is finished.

    It also drops an over-match the regex had: `[^\\n]*` is greedy, so
    `{# a #} tail {# b #}` matched as ONE comment and ` tail ` got blanked with
    it. Jinja closes a comment at the FIRST `#}` and renders that input as
    ` tail `, so the greedy read was masking live template text.
    """
    if b"{#" not in block:
        return block
    out = bytearray(block)
    base = 0
    for line in block.split(b"\n"):
        _blank_line_comments(out, line, base, blank)
        base += len(line) + 1  # +1 for the '\n' that split() removed
    return bytes(out)


def _blank_line_comments(out: bytearray, line: bytes, base: int, blank: int) -> None:
    """Blank in `out` the `{# ... #}` comments of one `line` of the block; the
    line starts at offset `base` of `out`."""
    i = 0
    while True:
        s = line.find(b"{#", i)
        if s < 0:
            break
        e = line.find(b"#}", s + 2)
        if e < 0:
            break  # nothing closes on this line; later openers can't either
        for p in range(base + s, base + e + 2):
            if out[p] not in (0x0A, 0x0D):
                out[p] = blank
        i = e + 2


def _mask_placeholders(block: bytes) -> bytes:
    """Replace Jinja/Django expression placeholders and comments with
    equal-LENGTH filler so the JS parse sees an identifier where the template
    interpolates a value. Length- and newline-preserving: every byte offset in
    the masked block still points at the same byte of the real file."""
    def _ident(m):
        return b"J" + b"x" * (m.end() - m.start() - 1)

    return _JINJA_EXPR_RE.sub(_ident, _blank_jinja_comments(block))


def _has_template_marker(block: bytes) -> bool:
    return any(marker in block for marker in _TEMPLATE_MARKERS)


def _first_error_node(root):
    """Earliest ERROR / MISSING node in the tree, or None. Doesn't descend
    into an error node — the outermost one carries the offending text."""
    best = None
    stack = [root]
    while stack:
        node = stack.pop()
        if node.type == "ERROR" or node.is_missing:
            if best is None or node.start_byte < best.start_byte:
                best = node
            continue
        if node.has_error:
            stack.extend(node.children)
    return best


# Closing tokens whose absence is reported at the point the parser gave up,
# which is generally NOT where the unclosed block began.
_JS_CLOSERS_MISSING = frozenset({"}", ")", "]"})


def _js_error(block: bytes):
    """(offset_in_block, message, hint, opener_offset) for the first JavaScript
    syntax error in `block`, or None when it parses (or when template syntax
    makes the answer undecidable).

    `opener_offset` is set only for a missing closing token, and points at the
    block that was left open. tree-sitter reports a missing `}` at the position
    where it expected one — the end of the enclosing construct, which is often
    a line the edit never touched. An observed session was told "line 202: a
    `}` is missing" against `setInterval(draw, 100);`, a line it had not
    changed, and re-sent the same broken content twice before the breaker
    stopped it. The line that needs looking at is where the block opened.

    Masking only ever REMOVES findings: the raw block is parsed first, so
    legitimate JavaScript that happens to contain `{{` (a block inside a
    block) is never masked into an error it didn't have.
    """
    parser = _ts.Parser(_JS_LANG)
    tree = parser.parse(block)
    if not tree.root_node.has_error:
        return None
    text = block
    if _has_template_marker(block):
        masked = _mask_placeholders(block)
        if _has_template_marker(masked):
            return None  # statement tags / multi-line placeholders — undecidable
        tree = parser.parse(masked)
        if not tree.root_node.has_error:
            return None  # the error WAS the template syntax
        text = masked
    node = _first_error_node(tree.root_node)
    if node is None:
        return None

    if node.is_missing:
        return _js_missing_token_error(node)
    snippet = text[node.start_byte:node.end_byte].decode("utf-8", "replace").strip()
    if snippet in _CLOSERS:
        opener = _CLOSERS[snippet]
        return (node.start_byte,
                f"unexpected `{snippet}`",
                f"Nothing opened a `{opener}` for it to close — delete the stray "
                f"`{snippet}`, or add the `{opener}` it was meant to close.", None)
    if snippet and "\n" not in snippet and len(snippet) <= 24:
        return (node.start_byte, f"unexpected `{snippet}`",
                "Rewrite that statement so it parses as JavaScript.", None)
    return (node.start_byte, "invalid JavaScript starts here",
            "Rewrite that statement so it parses as JavaScript.", None)


def _js_missing_token_error(node):
    """(offset_in_block, message, hint, opener_offset) for a token tree-sitter
    reports missing; opener_offset is set for a closing token only."""
    token = node.type
    if token in _JS_CLOSERS_MISSING:
        # The nearest ancestor that actually STARTS on an earlier line.
        # The immediate parent is often a node the recovery invented on
        # the stopping line itself (an expression_statement wrapping the
        # phantom token), which is no more useful than the stop position.
        opener, p = None, node.parent
        while p is not None:
            if p.start_point[0] < node.start_point[0]:
                opener = p.start_byte
                break
            p = p.parent
        if opener is not None:
            return (node.start_byte,
                    f"a `{token}` is missing",
                    f"Add the `{token}` that closes it.",
                    opener)
    return (node.start_byte,
            f"a `{token}` is missing",
            f"Add the missing `{token}`.",
            None)


# Nodes that open a fresh lexical scope. A `let` may shadow an outer binding
# freely; only a repeat within ONE of these is the early error.
_JS_SCOPE_NODES = frozenset({
    "program", "statement_block", "class_body", "switch_body",
    "for_statement", "for_in_statement",
})


def _js_redeclaration(block: bytes):
    """(offset, message, hint) for the first `let`/`const` redeclared in the
    same scope, or None.

    A duplicate lexical binding is an early SyntaxError — the engine refuses
    the whole script before running a line of it, so one stray `let score = 0`
    appended to a page kills every handler on it. tree-sitter parses it
    happily, which is why the syntax check cannot see it.

    Only `let`/`const` are considered, and only within a single scope: those
    are unconditionally errors per spec, so there is no judgment call and no
    false positive. `var`, function declarations and shadowing across scopes
    are all legal and left alone.
    """
    parser = _ts.Parser(_JS_LANG)
    tree = parser.parse(block)
    if tree.root_node.has_error or _has_template_marker(block):
        return None

    hit = _js_first_redeclaration(tree.root_node, {})
    if hit is None:
        return None
    offset, name = hit
    return (offset,
            f"`{name}` is declared twice in the same scope",
            f"A repeated `let`/`const` is a SyntaxError before anything runs, so "
            f"the whole script is dead, not just this line. Drop this declaration "
            f"and use the existing `{name}`, or rename one of them.")


def _js_declared_names(decl):
    """(name, node) for each plain identifier a lexical declaration binds.
    Destructuring patterns are skipped — conservative on purpose."""
    for child in decl.named_children:
        if child.type != "variable_declarator":
            continue
        ident = child.child_by_field_name("name")
        if ident is not None and ident.type == "identifier":
            yield ident.text.decode("utf-8", "replace"), ident


def _js_first_redeclaration(node, scope):
    """(offset, name) of the first `let`/`const` name in `node` declared again
    in a scope that holds it, or None. `scope` maps the names declared so far
    in the current scope to their offsets."""
    if node.type == "lexical_declaration":
        for name, ident in _js_declared_names(node):
            if name in scope:
                return (ident.start_byte, name)
            scope[name] = ident.start_byte
    for child in node.children:
        inner = {} if child.type in _JS_SCOPE_NODES else scope
        hit = _js_first_redeclaration(child, inner)
        if hit is not None:
            return hit
    return None


# Timers that re-invoke their callback on their own. requestAnimationFrame is
# NOT one: it fires once, and a render loop built on it re-arms from inside the
# callback — which is exactly the shape checked for below.
_RECURRING_TIMERS = frozenset({"setInterval"})
_ONE_SHOT_TIMERS = frozenset({"setTimeout", "requestAnimationFrame"})


def _js_looping_functions(block: bytes):
    """(looping, one_shot) for this JavaScript block.

    `looping` is the set of function names the code keeps calling; `one_shot`
    maps a function name to the byte offset of the timer that fires it exactly
    once, so a caller can point at the call site.

    A function loops when `setInterval` drives it, or when its own body arms
    any timer — the `setTimeout(draw, delay)` inside `draw()` that schedules
    the next frame. A one-shot `setTimeout(showBanner, 3000)` at top level is
    not a loop, and neither is a `draw` whose body no longer re-arms.

    Names only, no scope analysis: this feeds a healthy->broken comparison, so
    a name that resolves differently in the two versions is the same name in
    both and cancels out. Returns an empty set when the block does not parse.
    """
    parser = _ts.Parser(_JS_LANG)
    tree = parser.parse(block)
    if tree.root_node.has_error:
        return set(), {}

    looping = set()
    one_shot = {}

    _add_js_loops(tree.root_node, "", looping, one_shot)
    return looping, one_shot


def _js_timer_call(node):
    """(timer_name, callback_identifier) for a timer call, else None."""
    if node.type != "call_expression":
        return None
    fn = node.child_by_field_name("function")
    args = node.child_by_field_name("arguments")
    if fn is None or args is None or fn.type != "identifier":
        return None
    name = fn.text.decode("utf-8", "replace")
    if name not in _RECURRING_TIMERS and name not in _ONE_SHOT_TIMERS:
        return None
    for arg in args.named_children:
        return name, (arg.text.decode("utf-8", "replace")
                      if arg.type == "identifier" else "")
    return name, ""


def _add_js_loops(node, enclosing: str, looping: set, one_shot: dict) -> None:
    """Walk `node`, adding the functions its timers keep calling to `looping`
    and the ones they call once to `one_shot`. `enclosing` names the function
    the walk is in, or is ""."""
    _add_js_timer(node, enclosing, looping, one_shot)
    name = enclosing
    if node.type in ("function_declaration", "method_definition"):
        ident = node.child_by_field_name("name")
        if ident is not None:
            name = ident.text.decode("utf-8", "replace")
    elif node.type == "variable_declarator":
        value = node.child_by_field_name("value")
        ident = node.child_by_field_name("name")
        if (ident is not None and value is not None
                and value.type in ("function_expression", "arrow_function")):
            name = ident.text.decode("utf-8", "replace")
    for child in node.children:
        _add_js_loops(child, name, looping, one_shot)


def _add_js_timer(node, enclosing: str, looping: set, one_shot: dict) -> None:
    """When `node` is a timer call, add its callback to `looping` or `one_shot`,
    and `enclosing` to `looping`: a timer armed in a function body re-arms it."""
    hit = _js_timer_call(node)
    if hit is not None:
        timer, callback = hit
        if timer in _RECURRING_TIMERS and callback:
            looping.add(callback)
        elif timer in _ONE_SHOT_TIMERS and callback:
            one_shot.setdefault(callback, node.start_byte)
        # Any timer armed from inside a function body re-arms that body's
        # own loop — `setTimeout(draw, delay)` written inside draw(), and
        # the `requestAnimationFrame(loop)` idiom alike.
        if enclosing:
            looping.add(enclosing)


def _css_error(block: bytes):
    """(offset_in_block, message, hint) for the first unbalanced brace in a CSS
    body, or None.

    Balance only — no CSS grammar ships with the service, and anything
    finer-grained would be guesswork. Quoted strings and /* */ comments are
    skipped so `content: "}"` and a commented-out rule can't trip it; a body
    carrying template markers is skipped outright.
    """
    if _has_template_marker(block):
        return None
    opens = []
    i, n = 0, len(block)
    quote = 0
    while i < n:
        c = block[i]
        if quote:
            i, quote = _css_string_step(c, i, quote)
            continue
        if c in (0x22, 0x27):  # " '
            quote = c
            i += 1
            continue
        if c == 0x2F and i + 1 < n and block[i + 1] == 0x2A:  # /*
            end = block.find(b"*/", i + 2)
            i = n if end < 0 else end + 2
            continue
        if extra := _css_brace_error(c, i, opens):
            return extra
        i += 1
    if opens:
        return (opens[0], "a `{` that is never closed",
                "Close the rule with `}` — everything after an unclosed rule is "
                "dropped by the browser.")
    return None


def _css_string_step(c: int, i: int, quote: int):
    """(next index, quote) after the byte `c` at `i` inside a string that
    `quote` opened: a backslash skips the byte after it, and the closing quote
    ends the string (quote 0)."""
    if c == 0x5C:  # backslash escape
        return i + 2, quote
    if c == quote:
        quote = 0
    return i + 1, quote


def _css_brace_error(c: int, i: int, opens: list):
    """Track the brace `c` at `i` on the stack `opens`; the error for a `}`
    that closes nothing, else None."""
    if c == 0x7B:  # {
        opens.append(i)
    elif c == 0x7D:  # }
        if not opens:
            return (i, "an extra `}`",
                    "Delete the stray `}` — no rule is open at that point.")
        opens.pop()
    return None


def _tag_counts_balance(html: bytes) -> bool:
    """False when <script>/<style> open and close counts disagree — the shape
    a `</script>` inside a JS string produces, which truncates tree-sitter's
    raw_text and would make a valid block look broken."""
    low = html.lower()
    return (low.count(b"<script") == low.count(b"</script")
            and low.count(b"<style") == low.count(b"</style"))


def _start_tag_attributes(start_tag, source: bytes) -> dict:
    """{lowercased attribute name: value text} for a tree-sitter-html start_tag."""
    attrs = {}
    for child in start_tag.children:
        if child.type != "attribute":
            continue
        name, value = _attribute_name_value(child, source)
        if name:
            attrs[name] = value
    return attrs


def _attribute_name_value(child, source: bytes):
    """(lowercased name, value text) of one attribute node of a start_tag; the
    name is None when the node has none."""
    name, value = None, ""
    for part in child.children:
        if part.type == "attribute_name":
            name = source[part.start_byte:part.end_byte].decode("utf-8", "replace").lower()
        elif part.type == "quoted_attribute_value":
            inner = [g for g in part.children if g.type == "attribute_value"]
            if inner:
                value = source[inner[0].start_byte:inner[0].end_byte].decode("utf-8", "replace")
        elif part.type == "attribute_value":
            value = source[part.start_byte:part.end_byte].decode("utf-8", "replace")
    return name, value


def _embedded_blocks(html: bytes, base_offset: int, where_suffix: str,
                     no_escapes: bool):
    """[(kind, absolute_body_offset, body_bytes, where)] for the parseable
    <script>/<style> bodies in `html`. Empty when the document's tag counts
    don't balance — see _tag_counts_balance."""
    if not _tag_counts_balance(html):
        return []
    parser = _ts.Parser(_HTML_LANG)
    tree = parser.parse(html)
    out = []
    stack = [tree.root_node]
    while stack:
        node = stack.pop()
        stack.extend(node.children)
        if node.type not in ("script_element", "style_element"):
            continue
        if block := _embedded_block(node, html, base_offset, where_suffix, no_escapes):
            out.append(block)
    return out


def _embedded_block(node, html: bytes, base_offset: int, where_suffix: str,
                    no_escapes: bool):
    """(kind, absolute_body_offset, body_bytes, where) for one <script> or
    <style> element, or None when its body is not one to parse."""
    kind = "javascript" if node.type == "script_element" else "css"
    start_tag = next((c for c in node.children if c.type == "start_tag"), None)
    raw = next((c for c in node.children if c.type == "raw_text"), None)
    if start_tag is None or raw is None:
        return None
    attrs = _start_tag_attributes(start_tag, html)
    if kind == "javascript":
        if "src" in attrs:
            return None  # the browser ignores an inline body on a src script
        if attrs.get("type", "").strip().lower() not in _JS_SCRIPT_TYPES:
            return None  # text/template, application/json, importmap, ...
    body = html[raw.start_byte:raw.end_byte]
    if not body.strip():
        return None
    if no_escapes and b"\\" in body:
        # Python would render the escape, so the source bytes we'd parse
        # are not the bytes the browser receives.
        return None
    tag = "<script>" if kind == "javascript" else "<style>"
    return (kind, base_offset + raw.start_byte, body,
            f"the {tag} block{where_suffix}")


def _python_html_strings(source: bytes):
    """[(content_offset, content_bytes, where_suffix)] for Python string
    literals whose content embeds HTML with a <script>/<style> block. Offsets
    are into `source`, so a reported line number is the line in the .py file."""
    parser = _ts.Parser(_PY_LANG)
    tree = parser.parse(source)
    out = []
    stack = [tree.root_node]
    while stack:
        node = stack.pop()
        stack.extend(node.children)
        if node.type != "string":
            continue
        if node.parent is not None and node.parent.type == "concatenated_string":
            continue  # only half the document lives in this literal
        if _is_f_string(node, source):
            continue
        contents = [c for c in node.children if c.type == "string_content"]
        if len(contents) != 1:
            continue
        content = source[contents[0].start_byte:contents[0].end_byte]
        low = content.lower()
        if b"<script" not in low and b"<style" not in low:
            continue
        out.append((contents[0].start_byte, content, _python_string_label(node, source)))
    return out


def _is_f_string(node, source: bytes) -> bool:
    """True when the Python string literal `node` is an f-string."""
    prefix_node = next((c for c in node.children if c.type == "string_start"), None)
    if prefix_node is not None:
        prefix = source[prefix_node.start_byte:prefix_node.end_byte].decode("utf-8", "replace")
        if "f" in prefix.lower().replace('"', "").replace("'", ""):
            return True  # f-string: `{`/`}` are Python's, not JavaScript's
    return False


def _python_string_label(node, source: bytes) -> str:
    """" inside the Python string HTML_TEMPLATE" when the literal is assigned
    to a name, else " inside a Python string" — the model needs to know WHICH
    string to go fix."""
    parent = node.parent
    while parent is not None and parent.type not in ("assignment", "module"):
        parent = parent.parent
    if parent is not None and parent.type == "assignment":
        left = parent.child_by_field_name("left")
        if left is not None and left.type == "identifier":
            name = source[left.start_byte:left.end_byte].decode("utf-8", "replace")
            return f" inside the Python string {name}"
    return " inside a Python string"


def _line_at(source: bytes, offset: int):
    """(1-based line, 1-based column, stripped line text) for a byte offset."""
    offset = max(0, min(offset, len(source)))
    line_no = source.count(b"\n", 0, offset) + 1
    line_start = source.rfind(b"\n", 0, offset) + 1
    line_end = source.find(b"\n", offset)
    if line_end < 0:
        line_end = len(source)
    text = source[line_start:line_end].decode("utf-8", "replace").strip()
    if len(text) > 200:
        text = text[:200] + " …"
    return line_no, offset - line_start + 1, text


def _stopped_loop_findings(path, source: bytes, previous_text: str, blocks) -> list:
    """Findings for render loops the edit stopped driving.

    Compares the whole file's looping functions before and after, rather than
    block by block: an edit is free to move a loop between <script> blocks, and
    only the names that stop looping everywhere are dead. A name the edit also
    deleted is not reported — removing a loop outright is a decision, leaving
    one scheduled once is a mistake.
    """
    try:
        before = embedded_script_blocks_for(path, previous_text)
    except Exception:
        return []
    if not before:
        return []

    was_looping = set()
    for kind, _offset, body, _where in before:
        if kind == "javascript":
            was_looping |= _js_looping_functions(body)[0]
    if not was_looping:
        return []

    still_looping, one_shot, where_by_name = set(), {}, {}
    for kind, offset, body, where in blocks:
        if kind != "javascript":
            continue
        looping, shots = _js_looping_functions(body)
        still_looping |= looping
        for name, block_offset in shots.items():
            one_shot.setdefault(name, offset + block_offset)
            where_by_name.setdefault(name, where)

    findings = []
    for name in sorted(was_looping - still_looping):
        if name not in one_shot:
            continue  # the function is gone entirely, or nothing calls it now
        line, column, text = _line_at(source, one_shot[name])
        findings.append({
            "line": line,
            "column": column,
            "kind": "javascript",
            "where": where_by_name.get(name, ""),
            "defect": "stopped_loop",
            "message": f"`{name}` used to run on a repeating timer and now runs once",
            "hint": (f"Nothing schedules the next call, so the loop stops after one "
                     f"frame. Either put the timer back inside `{name}` so each call "
                     f"arms the next one, or restore setInterval."),
            "text": text,
        })
    return findings


def embedded_script_blocks_for(path: str, source_text: str):
    """The embedded <script>/<style> blocks of a file, as
    (kind, offset, body, where). Empty when the carrier is unsupported."""
    source = source_text.encode("utf-8")
    p = (path or "").lower()
    if p.endswith(_EMBEDDED_HTML_EXTS):
        return _embedded_blocks(source, 0, "", no_escapes=False)
    if p.endswith(".py"):
        blocks = []
        for offset, content, label in _python_html_strings(source):
            blocks.extend(_embedded_blocks(content, offset, label, no_escapes=True))
        return blocks
    return []

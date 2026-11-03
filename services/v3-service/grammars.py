"""The tree-sitter grammars this build has, which files each one parses, and
how a friendly selector (function:NAME, class:NAME, <tag>) becomes a
tree-sitter query in each grammar's node types."""

# The names that symbols.py and embedded_script.py import from this module.
__all__ = [
    "_EMBEDDED_SCRIPT_AVAILABLE", "_HTML_LANG", "_JS_LANG", "_PY_LANG", "_STRUCTURAL_EDIT_AVAILABLE",
    "_ast_language_for_path", "_ast_selector_to_query", "_supported_structural_exts", "_ts",
]

try:
    import tree_sitter as _ts
    import tree_sitter_python as _tsp
    import tree_sitter_html as _tsh
    _PY_LANG = _ts.Language(_tsp.language())
    _HTML_LANG = _ts.Language(_tsh.language())
    _STRUCTURAL_EDIT_AVAILABLE = True
except ImportError as _e:
    print(f"[structural_edit] tree-sitter not available: {_e} — endpoint will return 501", flush=True)
    _STRUCTURAL_EDIT_AVAILABLE = False
    _PY_LANG = None
    _HTML_LANG = None

# The JavaScript grammar backs embedded_script_check only, so it gets its own
# availability flag: a build without it must still serve structural_edit and
# the call-resolution checks. A missing grammar degrades to "no finding",
# never a crash and never a blocked write.
# Go and TypeScript grammars for structural_edit. Independent of the
# JavaScript block below: a build without them still serves Python, HTML and
# JS rather than failing to import.
try:
    import tree_sitter_go as _tsg
    _GO_LANG = _ts.Language(_tsg.language())
except ImportError:
    _GO_LANG = None

try:
    import tree_sitter_typescript as _tst
    _TS_LANG = _ts.Language(_tst.language_typescript())
    _TSX_LANG = _ts.Language(_tst.language_tsx())
except ImportError:
    _TS_LANG = None
    _TSX_LANG = None

try:
    import tree_sitter_javascript as _tsj
    _JS_LANG = _ts.Language(_tsj.language())
    _EMBEDDED_SCRIPT_AVAILABLE = True
except Exception as _e:  # ImportError, or _ts undefined when tree-sitter is absent
    print(f"[embedded_script] tree-sitter-javascript not available: {_e} — embedded-script check returns no findings", flush=True)
    _EMBEDDED_SCRIPT_AVAILABLE = False
    _JS_LANG = None


def _ast_language_for_path(path: str):
    """Map a path to its tree-sitter grammar.

    v1 shipped Python and HTML. The engine is language-agnostic — a grammar
    plus a selector mapping is all a language needs — and the gap was
    measured: across 168 sessions, `unsupported file type for
    structural_edit` was the only ATLAS-side wall a session hit, 12 times,
    when the model reached for it on a .go file. The JavaScript grammar was
    already installed for embedded_script_check and simply not wired here.
    """
    p = path.lower()
    if p.endswith(".py"):
        return "python", _PY_LANG
    if p.endswith((".html", ".htm")):
        return "html", _HTML_LANG
    if p.endswith(".go") and _GO_LANG is not None:
        return "go", _GO_LANG
    if p.endswith(".tsx") and _TSX_LANG is not None:
        return "typescript", _TSX_LANG
    if p.endswith((".ts", ".mts", ".cts")) and _TS_LANG is not None:
        return "typescript", _TS_LANG
    if p.endswith((".js", ".mjs", ".cjs", ".jsx")) and _JS_LANG is not None:
        return "javascript", _JS_LANG
    return None, None


def _supported_structural_exts() -> str:
    """The extensions this build can actually address, for the error text.

    Built from the grammars that imported rather than hardcoded, so a build
    missing one never advertises it.
    """
    exts = [".py", ".html", ".htm"]
    if _GO_LANG is not None:
        exts.append(".go")
    if _TS_LANG is not None:
        exts.extend([".ts", ".tsx"])
    if _JS_LANG is not None:
        exts.extend([".js", ".jsx"])
    return ", ".join(exts)


# Selector prefixes used by the selectors of more than one language.
_FUNCTION_PREFIX = "function:"
_CLASS_PREFIX = "class:"


def _ast_selector_to_query(selector: str, language: str):
    """Translate friendly selector → (tree-sitter query string, target capture).
    Returns (None, None, error_message) for unknown selectors.
    """
    s = selector.strip()
    if language == "go":
        return _go_selector_to_query(s, selector)

    if language in ("javascript", "typescript"):
        return _js_selector_to_query(s, selector, language)

    if language == "python":
        return _python_selector_to_query(s, selector)
    if language == "html":
        return _html_selector_to_query(s, selector)
    return None, None, f"unsupported language: {language}"


def _go_selector_to_query(s: str, selector: str):
    """The query for a Go selector: function:NAME (a func or a method) or
    type:NAME. Returns (query, capture, error) as _ast_selector_to_query does."""
    # Go separates plain functions from methods, and the model does not
    # know which it is looking at — `function:Name` matches either, so a
    # method does not need a different selector than a function.
    if s.startswith(_FUNCTION_PREFIX):
        name = s[len(_FUNCTION_PREFIX):].strip()
        if not name:
            return None, None, "selector 'function:' missing name (e.g. 'function:Solve')"
        return (
            f'[(function_declaration name: (identifier) @_name (#eq? @_name "{name}"))'
            f' (method_declaration name: (field_identifier) @_name (#eq? @_name "{name}"))] @target',
            "target", None,
        )
    if s.startswith((_CLASS_PREFIX, "type:")):
        name = s.split(":", 1)[1].strip()
        if not name:
            return None, None, "selector 'type:' missing name (e.g. 'type:Server')"
        return (
            f'(type_declaration (type_spec name: (type_identifier) @_name '
            f'(#eq? @_name "{name}"))) @target',
            "target", None,
        )
    return None, None, (
        f"unknown selector '{selector}' for go. Supported: function:NAME "
        f"(matches a func or a method), type:NAME.")


def _js_selector_to_query(s: str, selector: str, language: str):
    """The query for a JavaScript or TypeScript selector: function:NAME or
    class:NAME. Returns (query, capture, error) as _ast_selector_to_query does."""
    # One selector covers the four ways JS spells a function, because
    # which one a file used is not something the model reliably knows:
    # `function f(){}`, `const f = () => {}`, `const f = function(){}`,
    # and a class method.
    if s.startswith(_FUNCTION_PREFIX):
        name = s[len(_FUNCTION_PREFIX):].strip()
        if not name:
            return None, None, "selector 'function:' missing name (e.g. 'function:render')"
        return (
            f'[(function_declaration name: (identifier) @_name (#eq? @_name "{name}"))'
            f' (generator_function_declaration name: (identifier) @_name (#eq? @_name "{name}"))'
            f' (lexical_declaration (variable_declarator name: (identifier) @_name'
            f'   (#eq? @_name "{name}") value: [(arrow_function) (function_expression)]))'
            f' (method_definition name: (property_identifier) @_name (#eq? @_name "{name}"))] @target',
            "target", None,
        )
    if s.startswith(_CLASS_PREFIX):
        name = s[len(_CLASS_PREFIX):].strip()
        if not name:
            return None, None, "selector 'class:' missing name (e.g. 'class:Board')"
        # TypeScript names a class with type_identifier, JavaScript with
        # identifier, and tree-sitter rejects a pattern naming a node the
        # grammar does not have — an alternation covering both is an
        # error in each. Same selector for the model either way.
        name_node = "type_identifier" if language == "typescript" else "identifier"
        return (
            f'(class_declaration name: ({name_node}) @_name '
            f'(#eq? @_name "{name}")) @target',
            "target", None,
        )
    return None, None, (
        f"unknown selector '{selector}' for {language}. Supported: "
        f"function:NAME (declaration, arrow, expression or method), class:NAME.")


def _python_selector_to_query(s: str, selector: str):
    """The query for a Python selector: function:NAME or class:NAME. Returns
    (query, capture, error) as _ast_selector_to_query does."""
    if s.startswith(_FUNCTION_PREFIX):
        name = s[len(_FUNCTION_PREFIX):].strip()
        if not name:
            return None, None, "selector 'function:' missing name (e.g. 'function:dashboard')"
        return (
            f'(function_definition name: (identifier) @_name (#eq? @_name "{name}")) @target',
            "target", None,
        )
    if s.startswith(_CLASS_PREFIX):
        name = s[len(_CLASS_PREFIX):].strip()
        if not name:
            return None, None, "selector 'class:' missing name (e.g. 'class:UserModel')"
        return (
            f'(class_definition name: (identifier) @_name (#eq? @_name "{name}")) @target',
            "target", None,
        )
    if s.startswith("<") and s.endswith(">") and len(s) > 2:
        # Reaching for an HTML tag on a .py file means the target is markup
        # inside a template string. That is one string literal to the Python
        # grammar, not an element, so no selector can address it — say what
        # does work instead of listing selectors that miss the intent.
        return None, None, (
            f"selector '{selector}' is HTML-only and this is a Python file. "
            f"Markup inside a Python string is one string literal to the Python "
            f"grammar, so NO selector reaches into it. Use edit_file, with "
            f"old_str set to a short unique line copied byte-for-byte from "
            f"inside the template — anchor on the one line you are changing, "
            f"not the whole block. Do NOT reach for function:NAME here: the "
            f"template is usually a module-level constant, so rewriting a "
            f"function that merely renders it changes nothing."
        )
    return None, None, (
        f"unknown selector '{selector}' for python. Supported: function:NAME, class:NAME"
    )


def _html_selector_to_query(s: str, selector: str):
    """The query for an HTML selector: <tag>. Returns (query, capture, error)
    as _ast_selector_to_query does."""
    if s.startswith("<") and s.endswith(">") and len(s) > 2:
        tag = s[1:-1].strip().lower()
        if not tag.replace("-", "").replace("_", "").isalnum():
            return None, None, (
                f"selector '{selector}' has invalid tag name — use a bare "
                f"tag like <script> or <body>, not attributes"
            )
        # tree-sitter-html parses <script> and <style> as dedicated
        # script_element / style_element nodes (their bodies are raw
        # JS/CSS, not HTML), NOT generic `element` nodes — so the generic
        # element query matches them 0 times. Target their real node type.
        if tag == "script":
            return "(script_element) @target", "target", None
        if tag == "style":
            return "(style_element) @target", "target", None
        return (
            f'(element (start_tag (tag_name) @_tag (#eq? @_tag "{tag}"))) @target',
            "target", None,
        )
    return None, None, (
        f"unknown selector '{selector}' for html. Supported: <tag> (e.g. <body>, <head>, <h1>, <script>, <style>)"
    )

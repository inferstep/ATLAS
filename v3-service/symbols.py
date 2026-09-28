"""Tree-sitter structural tooling: friendly-selector structural edits, symbol
indexing, direct-call resolution (structural_score), embedded-script syntax
checking, traceback frame parsing, and cyclomatic complexity."""

import re


# --- structural_edit (GH #39 v1) ----------------------------------------------------
#
# Friendly-selector-driven structural edits. Replaces the model's edit_file
# old_str/new_str pair (which truncates on long blocks: 2716-char Flask
# template hit max_tokens mid-JSON in the May 7 session) with a tree-sitter
# syntax-tree node selector.
#
# v1 supports:
#   - Python: function:NAME, class:NAME
#   - HTML:   <tag>
# Single-match enforcement: ambiguous selectors fail with a clear error so
# the model knows to be more specific. Returns new content for the proxy to
# write, preserving the lens-score-before-write pattern that write_file uses.

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


def _ast_selector_to_query(selector: str, language: str):
    """Translate friendly selector → (tree-sitter query string, target capture).
    Returns (None, None, error_message) for unknown selectors.
    """
    s = selector.strip()
    if language == "go":
        # Go separates plain functions from methods, and the model does not
        # know which it is looking at — `function:Name` matches either, so a
        # method does not need a different selector than a function.
        if s.startswith("function:"):
            name = s[len("function:"):].strip()
            if not name:
                return None, None, "selector 'function:' missing name (e.g. 'function:Solve')"
            return (
                f'[(function_declaration name: (identifier) @_name (#eq? @_name "{name}"))'
                f' (method_declaration name: (field_identifier) @_name (#eq? @_name "{name}"))] @target',
                "target", None,
            )
        if s.startswith("class:") or s.startswith("type:"):
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

    if language in ("javascript", "typescript"):
        # One selector covers the four ways JS spells a function, because
        # which one a file used is not something the model reliably knows:
        # `function f(){}`, `const f = () => {}`, `const f = function(){}`,
        # and a class method.
        if s.startswith("function:"):
            name = s[len("function:"):].strip()
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
        if s.startswith("class:"):
            name = s[len("class:"):].strip()
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

    if language == "python":
        if s.startswith("function:"):
            name = s[len("function:"):].strip()
            if not name:
                return None, None, "selector 'function:' missing name (e.g. 'function:dashboard')"
            return (
                f'(function_definition name: (identifier) @_name (#eq? @_name "{name}")) @target',
                "target", None,
            )
        if s.startswith("class:"):
            name = s[len("class:"):].strip()
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
    if language == "html":
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
    return None, None, f"unsupported language: {language}"


# GH #39 point 4: project-aware symbol resolution. Caller (proxy) extracts
# candidate symbols from the user message and ships a file_map of relevant
# project files; we tree-sitter-walk each, build a symbol index, return
# snippets for the symbols that are actually defined in the project.
# Stateless — no caching, fresh index per call. v1 supports Python only.

def _symbol_index_for_python_source(source: bytes):
    """Return list of (name, kind, start_byte, end_byte) for each top-level
    function/class definition in source. Decorator-aware: function with
    @app.route(...) returns the byte range that includes the decorator,
    so callers paste the whole decorated unit."""
    try:
        parser = _ts.Parser(_PY_LANG)
        tree = parser.parse(source)
    except Exception:
        return []
    out = []
    # Walk root children only — top-level definitions. Skip nested functions
    # and methods inside classes for v1 (they'd noise up the index without
    # adding much value for the kinds of references users actually make).
    for node in tree.root_node.children:
        target = node
        kind = None
        if node.type == "function_definition":
            kind = "function"
        elif node.type == "class_definition":
            kind = "class"
        elif node.type == "decorated_definition":
            for child in node.children:
                if child.type == "function_definition":
                    target = child
                    kind = "function"
                    break
                if child.type == "class_definition":
                    target = child
                    kind = "class"
                    break
            # Use the wrapper's byte range so the decorator is included
        if not kind:
            continue
        # Find name child of the function/class itself
        name = None
        for child in target.children:
            if child.type == "identifier":
                name = source[child.start_byte:child.end_byte].decode("utf-8", errors="replace")
                break
        if not name:
            continue
        # Use outer node's byte range (decorator wrapper if present)
        out.append((name, kind, node.start_byte, node.end_byte))
    return out


def symbol_index(file_map: dict, candidate_symbols: list, max_snippets: int = 3, max_lines_per_snippet: int = 200) -> dict:
    """Resolve candidate_symbols against a project's Python files.

    file_map: {path: source_text} of project .py files
    candidate_symbols: ['dashboard', 'UserModel', ...] extracted from user msg
    Returns:
        matched: [{name, kind, file, snippet, n_lines}] for symbols defined in the project
        skipped: [{name, reason}] for symbols mentioned but not found
    """
    if not _STRUCTURAL_EDIT_AVAILABLE:
        return {"matched": [], "skipped": [{"name": s, "reason": "tree-sitter not installed"} for s in candidate_symbols]}

    # Build {symbol_name: [(file, kind, start_byte, end_byte)]} index
    index: dict = {}
    for path, source_text in (file_map or {}).items():
        if not path.lower().endswith(".py"):
            continue
        try:
            source_bytes = source_text.encode("utf-8")
        except (UnicodeEncodeError, AttributeError):
            continue
        for name, kind, sb, eb in _symbol_index_for_python_source(source_bytes):
            index.setdefault(name, []).append((path, kind, sb, eb, source_bytes))

    matched, skipped, seen = [], [], set()
    for sym in candidate_symbols:
        if sym in seen:
            continue
        seen.add(sym)
        if len(matched) >= max_snippets:
            skipped.append({"name": sym, "reason": "snippet cap reached"})
            continue
        hits = index.get(sym)
        if not hits:
            skipped.append({"name": sym, "reason": "not defined in scanned project files"})
            continue
        if len(hits) > 1:
            # Ambiguous — multiple files define the same symbol. Skip
            # rather than guess; the model can read_file directly if the
            # context matters.
            skipped.append({"name": sym, "reason": f"ambiguous ({len(hits)} definitions)"})
            continue
        path, kind, sb, eb, source_bytes = hits[0]
        snippet_bytes = source_bytes[sb:eb]
        snippet = snippet_bytes.decode("utf-8", errors="replace")
        # Trim very long snippets — keep the head only. The model can
        # read_file for the full content if it actually needs it.
        snippet_lines = snippet.split("\n")
        truncated = False
        if len(snippet_lines) > max_lines_per_snippet:
            snippet = "\n".join(snippet_lines[:max_lines_per_snippet]) + f"\n# ... ({len(snippet_lines) - max_lines_per_snippet} more lines truncated)"
            truncated = True
        matched.append({
            "name": sym,
            "kind": kind,
            "file": path,
            "snippet": snippet,
            "n_lines": len(snippet_lines),
            "truncated": truncated,
        })
    return {"matched": matched, "skipped": skipped}


# GH #39 point 1: structural verification of V3 candidates.
#
# Sandbox tests whether code RUNS; structural verification tests whether
# the candidate's calls actually resolve. The two answer different
# questions — sandbox can pass for code with try/except ImportError
# fallbacks, lazy imports, or dead branches that never execute the
# unresolved call. Tree-sitter sees what sandbox can't.
#
# v1 supports Python only. Direct-identifier calls only (skips method
# calls like `obj.foo()` and chained calls — they'd need import-graph
# resolution that's a v2 problem). Resolution order:
#   1. Local function/class definition in the same file
#   2. Imported name (top-of-file imports only, no conditional imports)
#   3. Python builtin
#   4. Project-wide symbol (any function/class in any scanned file)
# Anything that doesn't match → unresolved. Strict: 1+ unresolved → veto.

# The COMPLETE builtin namespace, derived from the interpreter rather
# than hand-curated. A previous curated subset was missing real builtins
# (TimeoutError, ConnectionError, memoryview, breakpoint, ...), and any
# gap here is a false VETO of valid code — `exit(1)` in a new file was
# rejected as a would-be NameError. Site builtins (exit/quit/help/...)
# are added explicitly so the set doesn't depend on how this interpreter
# was started; over-crediting a shadowed builtin only makes the veto
# more lenient, never blocks valid code.
import builtins as _builtins_mod

PY_BUILTINS = frozenset(
    {n for n in dir(_builtins_mod) if not n.startswith("_")}
    | {"exit", "quit", "help", "license", "copyright", "credits",
       "__import__", "__build_class__"}
)


def _extract_python_imports(source: bytes) -> set:
    """Names introduced into the file's namespace by import statements.

    Handles `import foo`, `import foo.bar`, `import foo as bar`,
    `from foo import bar`, `from foo import bar as baz`. Doesn't track
    star imports — `from foo import *` returns nothing because we don't
    know what's in `foo` without resolving the import. Star imports are
    a known v1 gap; conservative behavior is "treat the file's calls
    as more likely unresolved" rather than silently passing them.
    """
    if not _STRUCTURAL_EDIT_AVAILABLE:
        return set()
    try:
        parser = _ts.Parser(_PY_LANG)
        tree = parser.parse(source)
    except Exception:
        return set()

    imported = set()

    def text_of(node):
        return source[node.start_byte:node.end_byte].decode("utf-8", errors="replace")

    def walk(node):
        if node.type == "import_statement":
            for child in node.children:
                if child.type == "dotted_name":
                    # `import foo.bar` introduces `foo` into namespace
                    imported.add(text_of(child).split(".")[0])
                elif child.type == "aliased_import":
                    # `import foo as bar` — alias is the trailing identifier
                    last_ident = None
                    for c in child.children:
                        if c.type == "identifier":
                            last_ident = c
                    if last_ident is not None:
                        imported.add(text_of(last_ident))
        elif node.type == "import_from_statement":
            past_import_kw = False
            for child in node.children:
                if not past_import_kw:
                    if child.type == "import" or text_of(child) == "import":
                        past_import_kw = True
                    continue
                # After `import` keyword: dotted_name, identifier,
                # aliased_import, or wildcard_import
                if child.type == "dotted_name":
                    imported.add(text_of(child).split(".")[0])
                elif child.type == "identifier":
                    imported.add(text_of(child))
                elif child.type == "aliased_import":
                    last_ident = None
                    for c in child.children:
                        if c.type == "identifier":
                            last_ident = c
                    if last_ident is not None:
                        imported.add(text_of(last_ident))
                elif child.type == "wildcard_import":
                    # `from foo import *` — can't enumerate without
                    # resolving the import. Best we can do: bail out
                    # of strict mode for this file by adding a sentinel.
                    imported.add("*")
        for child in node.children:
            walk(child)

    walk(tree.root_node)
    return imported


def _extract_python_call_targets(source: bytes) -> list:
    """All direct-identifier call targets. Skips attribute / subscript /
    chained calls — those need full import-graph resolution and are out
    of scope for v1. Returns a list (not set) because duplicate calls
    matter when reporting — caller may dedup later."""
    if not _STRUCTURAL_EDIT_AVAILABLE:
        return []
    try:
        parser = _ts.Parser(_PY_LANG)
        tree = parser.parse(source)
    except Exception:
        return []

    out = []
    stack = [tree.root_node]
    while stack:
        node = stack.pop()
        if node.type == "call":
            # `function:` field is the first non-paren child
            for child in node.children:
                if child.type == "identifier":
                    out.append(source[child.start_byte:child.end_byte].decode("utf-8", errors="replace"))
                    break
                # attribute / subscript / lambda → skip silently
                # so we don't false-positive on `obj.method()`
                if child.type not in ("(",):
                    break
        stack.extend(node.children)
    return out


def _extract_python_top_level_defs(source: bytes) -> set:
    """Top-level function and class names defined in the file. Used as
    one input to call resolution. Skips nested functions and class
    methods — those don't introduce names into the file's top-level
    namespace."""
    if not _STRUCTURAL_EDIT_AVAILABLE:
        return set()
    try:
        parser = _ts.Parser(_PY_LANG)
        tree = parser.parse(source)
    except Exception:
        return set()

    names = set()
    for node in tree.root_node.children:
        target = node
        if node.type == "decorated_definition":
            for c in node.children:
                if c.type in ("function_definition", "class_definition"):
                    target = c
                    break
        if target.type in ("function_definition", "class_definition"):
            for c in target.children:
                if c.type == "identifier":
                    names.add(source[c.start_byte:c.end_byte].decode("utf-8", errors="replace"))
                    break
    return names


def _extract_python_bound_names(source: bytes) -> set:
    """Every name BOUND anywhere in the file — assignment targets, function
    and lambda parameters, for / with-as / except-as / comprehension
    targets, walrus, global/nonlocal, and def/class names at any nesting.

    Used to credit local callables so the structural resolver does NOT flag
    a call to a local variable, function parameter, or loop variable as
    unresolved (which would false-reject valid code — the #147 review's
    top finding). Deliberately scope-BLIND: a name bound inside one
    function is credited when called from another, which can miss a rare
    genuine cross-function NameError. That false-negative is the correct
    trade for a gate that BLOCKS writes — wrongly rejecting valid code is
    far worse than letting an uncommon bug through.
    """
    if not _STRUCTURAL_EDIT_AVAILABLE:
        return set()
    try:
        parser = _ts.Parser(_PY_LANG)
        tree = parser.parse(source)
    except Exception:
        return set()

    names = set()

    def add_pattern(node):
        # Recursively pull bare-identifier targets from a binding pattern
        # (a, (b, c), *rest = ...). Skip subscript/attribute targets
        # (a[0]=, a.b=) — those don't bind a NEW bare name.
        if node is None:
            return
        if node.type == "identifier":
            names.add(source[node.start_byte:node.end_byte].decode("utf-8", "replace"))
            return
        if node.type in ("subscript", "attribute"):
            return
        for c in node.children:
            add_pattern(c)

    stack = [tree.root_node]
    while stack:
        n = stack.pop()
        t = n.type
        if t in ("function_definition", "class_definition"):
            nm = n.child_by_field_name("name")
            if nm is not None and nm.type == "identifier":
                names.add(source[nm.start_byte:nm.end_byte].decode("utf-8", "replace"))
        if t in ("function_definition", "lambda"):
            params = n.child_by_field_name("parameters")
            if params is not None:
                pstack = list(params.children)
                while pstack:
                    p = pstack.pop()
                    if p.type == "identifier":
                        names.add(source[p.start_byte:p.end_byte].decode("utf-8", "replace"))
                    elif p.type in ("subscript", "attribute", "default_parameter",
                                    "typed_parameter", "typed_default_parameter",
                                    "list_splat_pattern", "dictionary_splat_pattern"):
                        # descend, but a default_parameter's VALUE isn't a binding —
                        # take only its leading identifier target.
                        for c in p.children:
                            if c.type == "identifier":
                                names.add(source[c.start_byte:c.end_byte].decode("utf-8", "replace"))
                                break
                            pstack.append(c)
        elif t in ("assignment", "augmented_assignment", "named_expression"):
            add_pattern(n.child_by_field_name("left") or n.child_by_field_name("name"))
        elif t in ("for_statement", "for_in_clause"):
            add_pattern(n.child_by_field_name("left"))
        elif t == "as_pattern":  # with ... as X / except ... as X
            add_pattern(n.child_by_field_name("alias") or (n.children[-1] if n.children else None))
        elif t in ("global_statement", "nonlocal_statement"):
            for c in n.children:
                if c.type == "identifier":
                    names.add(source[c.start_byte:c.end_byte].decode("utf-8", "replace"))
        stack.extend(n.children)
    return names


def build_project_symbols(file_map: dict) -> set:
    """Aggregate top-level function/class names across every .py file
    in file_map. Built once per V3 run, reused across all candidates."""
    out = set()
    for path, source_text in (file_map or {}).items():
        if not path.lower().endswith(".py"):
            continue
        try:
            out |= _extract_python_top_level_defs(source_text.encode("utf-8"))
        except Exception:
            continue
    return out


def structural_score(project_symbols, candidate_code: str,
                     max_names: int = 10) -> dict:
    """Check a candidate for unresolved direct-identifier calls.

    project_symbols: set built by build_project_symbols(file_map). Pass
    {} or set() if the project is empty / unavailable — every call
    will fall through to imports/builtins/unresolved.

    max_names caps the reported unresolved_calls list (telemetry-friendly
    default); 0 returns every name — required by callers that DIFF the
    lists (the proxy structural gate).

    Returns:
        ok: True if parse succeeded
        n_calls_total / n_unresolved: aggregate counts
        unresolved_calls: list of unique unresolved names (capped at
                          max_names unless max_names=0)
        wildcard_imports: True if the candidate has `from x import *`
                          (unresolved reporting is suppressed in that
                          case, so the list is always empty then)
    """
    if not _STRUCTURAL_EDIT_AVAILABLE:
        return {"ok": False, "error": "tree-sitter not installed"}
    try:
        candidate_bytes = candidate_code.encode("utf-8")
    except (UnicodeEncodeError, AttributeError) as e:
        return {"ok": False, "error": f"candidate not utf-8: {e}"}

    try:
        local_defs = _extract_python_top_level_defs(candidate_bytes)
        imports = _extract_python_imports(candidate_bytes)
        calls = _extract_python_call_targets(candidate_bytes)
        # Bound names (locals, params, loop/with targets, nested defs) credit
        # local callables so `fn = build(); fn()` is not flagged as a
        # NameError — #147 review finding #4/#5. Scope-blind on purpose.
        bound = _extract_python_bound_names(candidate_bytes)
    except Exception as e:
        return {"ok": False, "error": f"parse failed: {type(e).__name__}: {e}"}

    has_wildcard = "*" in imports
    if has_wildcard:
        # Star import in scope → can't reliably mark anything unresolved.
        # Be lenient and only flag calls that aren't obviously local /
        # builtin — wildcard might supply the rest.
        pass

    unresolved = []
    seen_unresolved = set()
    for name in calls:
        if name in seen_unresolved:
            continue
        if name in local_defs:
            continue
        if name in imports:
            continue
        if name in bound:  # local var / param / loop target / nested def
            continue
        if name in PY_BUILTINS:
            continue
        if name in (project_symbols or set()):
            continue
        if has_wildcard:
            # Wildcard import might supply this — treat as resolved-by-
            # wildcard rather than unresolved. False negatives possible
            # but better than blocking valid code.
            continue
        seen_unresolved.add(name)
        unresolved.append(name)

    return {
        "ok": True,
        "n_calls_total": len(calls),
        "n_unresolved": len(unresolved),
        # max_names=0 returns the FULL list. The proxy's structural gate
        # diffs original-vs-edited name lists; a truncated list makes that
        # comparison unsound in both directions on files with more
        # unresolved names than the cap.
        "unresolved_calls": unresolved[:max_names] if max_names else unresolved,
        "wildcard_imports": has_wildcard,
        "n_local_defs": len(local_defs),
        "n_imports": len(imports),
    }


# GH #39 point 3: Phase 3 repair with call-graph context.
#
# When all candidates fail sandbox and we drop to PR-CoT / refinement,
# the repair model gets `error` (raw stderr) + `code` (the failing
# candidate). It has to guess from the traceback alone what the
# failing function does inside the project. This module contributes the
# traceback parse (which function failed); the context block itself is
# built by graph.repair_context.

# Python traceback frame: `File "path", line N, in funcname`
_TRACEBACK_FRAME_RE = re.compile(r'File "[^"]+", line \d+, in (\S+)')


def _failing_function_from_stderr(stderr: str):
    """Return the deepest function name in a Python traceback, or None
    if stderr doesn't look like a traceback. The deepest frame is the
    one nearest the actual error; earlier frames are callers."""
    if not stderr:
        return None
    matches = _TRACEBACK_FRAME_RE.findall(stderr)
    if not matches:
        return None
    # Filter sentinels — `<module>`, `<lambda>`, `<genexpr>` aren't
    # callable names we can look up. Walk back until we find one.
    for name in reversed(matches):
        if not name.startswith("<"):
            return name
    return None


def cyclomatic_complexity(path: str, source_text: str) -> dict:
    """McCabe-style cyclomatic complexity from the tree-sitter syntax tree.

    Counts decision points across the whole file (sum of per-function CC,
    not strictly McCabe's per-function definition — we want one number for
    tier classification, not a per-symbol map). Decision-point set targets
    the things that actually predict V3-pipeline benefit: branches, loops,
    exception handlers, short-circuit booleans, comprehensions with filters,
    match/case clauses.

    v1 supports Python only. HTML CC isn't meaningful (markup, no real
    branching in tree-sitter's view of it — Jinja control blocks parse as
    text content). Other languages return {"ok": False} so the proxy's
    regex-based classifyFileTier stays the fallback floor.
    """
    if not _STRUCTURAL_EDIT_AVAILABLE:
        return {"ok": False, "error": "tree-sitter not installed in this build"}

    p = (path or "").lower()
    if not p.endswith(".py"):
        return {"ok": False, "error": f"cyclomatic_complexity v1 supports .py only (got {path})"}

    try:
        parser = _ts.Parser(_PY_LANG)
        tree = parser.parse(source_text.encode("utf-8"))
    except Exception as e:
        return {"ok": False, "error": f"parse failed: {type(e).__name__}: {e}"}

    # Decision-point node types in Python's tree-sitter grammar.
    # Each adds 1 to CC. `if_clause` inside a comprehension is the
    # filter clause (e.g. `[x for x in xs if x > 0]`) and counts as a branch.
    DECISION = {
        "if_statement", "elif_clause",
        "for_statement", "while_statement",
        "except_clause",
        "conditional_expression",  # ternary x if cond else y
        "boolean_operator",        # and / or short-circuit
        "case_clause",             # match-case
        "if_clause",               # comprehension filter
    }

    cc = 1  # base path
    stack = [tree.root_node]
    while stack:
        n = stack.pop()
        if n.type in DECISION:
            cc += 1
        stack.extend(n.children)

    return {"ok": True, "language": "python", "cyclomatic_complexity": cc}


def _duplicate_definitions(before_text: str, after_text: str) -> str:
    """Names defined more than once after the splice that were not before.

    Compares counts rather than presence: a file that already defined a name
    twice is the caller's business, but a splice that ADDS a second
    definition is the edit duplicating code it should have left alone.
    Returns a human-readable name list, or "" when nothing was duplicated.
    """
    import ast as _ast
    from collections import Counter

    def counts(text: str):
        try:
            tree = _ast.parse(text)
        except SyntaxError:
            return None
        return Counter(
            n.name for n in tree.body
            if isinstance(n, (_ast.FunctionDef, _ast.AsyncFunctionDef, _ast.ClassDef))
        )

    b, a = counts(before_text), counts(after_text)
    if b is None or a is None:
        return ""      # unparseable either side: the syntax gate owns that
    dupes = [name for name, n in a.items() if n > 1 and n > b.get(name, 0)]
    return ", ".join(sorted(dupes))


def structural_edit(path: str, source_text: str, selector: str, content: str) -> dict:
    """Apply a friendly-selector structural edit. Stateless transform — caller provides
    the source bytes (read from their own filesystem) and gets back new content.
    v3-service does no file IO; the proxy reads + writes via its existing
    workspace mount, which keeps lens-score-before-write intact."""
    if not _STRUCTURAL_EDIT_AVAILABLE:
        return {"success": False, "error": "structural_edit unavailable: tree-sitter not installed in this v3-service build"}

    # Empty-content guard (defense-in-depth; the proxy also checks). Splicing
    # empty content over a node deletes it — a model that omits `content`
    # would silently remove the function instead of fixing it.
    if not content.strip():
        return {"success": False, "error": (
            f"structural_edit: content is empty — that would DELETE '{selector}', not edit it. "
            f"Provide the full replacement body of the node."
        )}

    language, lang_obj = _ast_language_for_path(path)
    if not language:
        return {"success": False, "error": (
            f"unsupported file type for structural_edit: {path}. This build supports "
            f"{_supported_structural_exts()} — use edit_file for other languages."
        )}

    query_str, target_cap, err = _ast_selector_to_query(selector, language)
    if err:
        return {"success": False, "error": err}

    try:
        source = source_text.encode("utf-8")
    except (UnicodeEncodeError, AttributeError) as e:
        return {"success": False, "error": f"source not valid utf-8 string: {e}"}

    try:
        parser = _ts.Parser(lang_obj)
        tree = parser.parse(source)
        query = _ts.Query(lang_obj, query_str)
        # tree_sitter ≥0.23 moved captures off Query onto QueryCursor; older
        # versions exposed Query.captures directly. Support both so the
        # service works whichever wheel pip resolves.
        if hasattr(_ts, "QueryCursor"):
            captures = _ts.QueryCursor(query).captures(tree.root_node)
        else:
            captures = query.captures(tree.root_node)
    except Exception as e:
        return {"success": False, "error": f"tree-sitter parse/query error: {type(e).__name__}: {e}"}

    targets = captures.get(target_cap, [])
    if len(targets) == 0:
        # Ground the retry in the file's REAL symbols. A weak model
        # hallucinates selectors for functions that don't exist
        # (observed: function:get_inventory_count, function:calculate_inventory
        # against a file that defines item_subtotal / total_value). The lens
        # can't catch this — the replacement text is plausible code; the
        # TARGET is the problem. Listing what's actually defined turns a
        # dead-end "verify the symbol exists" into an actionable retry.
        available = ""
        if language == "python":
            try:
                names = []
                for name, kind, _sb, _eb in _symbol_index_for_python_source(source_text.encode("utf-8")):
                    names.append(f"{kind}:{name}")
                if names:
                    available = " This file defines: " + ", ".join(names[:30]) + ". Use one of these exact selectors, or read the file to confirm."
            except Exception:
                available = ""
        # The name the model asked for often DOES exist — as JavaScript inside
        # a template string, which no selector reaches. Saying only "that
        # symbol does not exist" is then flatly contradicted by the file the
        # model just read, and it tries the same selector again. Observed on
        # three runs, all opening with `function:draw` against a Flask app
        # whose game loop lives in HTML_TEMPLATE.
        if found := _embedded_symbol_note(path, source_text, selector):
            # The name exists, just not where a selector can go. Leading with
            # "does not exist" here is contradicted by the file the model just
            # read, which is why it re-sent the same selector.
            return {"success": False, "error": found}
        # structural_edit REPLACES an existing node; it cannot bring one into
        # existence. A model adding a feature reaches for the name it is about
        # to create — observed on "add a done command": turn 1 was
        # `function:done_task` against a file that does not have one yet. The
        # list of existing selectors is the right information and the wrong
        # advice, because the model does not want any of them.
        return {"success": False, "error": (
            f"selector '{selector}' matched 0 nodes in {path} — that symbol does not exist in this file."
            + " If you are ADDING it, structural_edit is the wrong tool: it replaces a node that"
            + " is already there. Use insert_after with the line number read_file printed to put"
            + " the new code in, or edit_file anchored on one unique nearby line."
            + (available or " Read the file first to see what's defined.")
            + _embedded_regions_note(path, source_text)
        )}
    if len(targets) > 1:
        return {"success": False, "error": (
            f"selector '{selector}' matched {len(targets)} nodes in {path}. "
            f"structural_edit requires exactly one match — use a more specific selector."
        )}

    target = targets[0]
    # Python grammar wraps decorated functions/classes in decorated_definition.
    # function:dashboard matches the inner function_definition; if its parent
    # is decorated_definition we want THAT byte range so @app.route(...) lines
    # get replaced too. Otherwise the model writes new @decorator lines and
    # the old ones stay, double-decorating the function.
    if language == "python" and target.parent is not None and target.parent.type == "decorated_definition":
        target = target.parent
    try:
        new_bytes = source[:target.start_byte] + content.encode("utf-8") + source[target.end_byte:]
        new_content = new_bytes.decode("utf-8")
    except UnicodeDecodeError as e:
        return {"success": False, "error": f"replacement produced invalid utf-8: {e}"}

    # Node-size precondition. A replacement many times the size of the node is
    # not an edit of that node — it is the whole file wearing a selector.
    #
    # This check used to live inside the post-splice `except SyntaxError`
    # handler below, so it only ever fired when the blob ALSO failed to
    # compile. Observed live: `function:index` (3 lines) was replaced by a
    # 194-line `HTML_TEMPLATE = """..."""` assignment, which is perfectly
    # valid Python. compile() passed, every gate passed, and the app lost its
    # only @app.route — the file still parsed, the model reported success.
    # Whether the blob happens to be syntactically valid says nothing about
    # whether it belongs in this node, so the check belongs here.
    if too_big := _replacement_dwarfs_node(source_text, target, content, selector):
        return {"success": False, "error": too_big}

    # Post-splice syntax gate (Python). Tree-sitter is error-tolerant: it
    # happily locates the node and splices in replacement content that is
    # not valid Python — observed live: a model emitted `item["id""]` and
    # `&quot;`-escaped quotes, structural_edit reported success, and a previously
    # runnable Flask app shipped with a SyntaxError. Refuse to hand back a
    # broken file; return the parse error so the model can fix its quoting
    # on the retry. Keyed off file type, not the model.
    if language == "python":
        # Duplicate-definition guard. A replacement that carries its
        # neighbours along splices them in a second time: the node is
        # replaced, but the originals after it survive, so the file ends up
        # defining the same names twice. Observed live: `function:a` was
        # replaced with a body containing a, b AND c, and the file came back
        # 10 -> 18 lines with b and c duplicated. It compiles, every other
        # gate passes, and the later definition silently wins at import time.
        #
        # This is the same "whole file wearing a selector" failure
        # _replacement_dwarfs_node exists for, caught by its actual signature
        # rather than by a size ratio — a 3x replacement is well under any
        # sane size threshold but still corrupts the file.
        if dup := _duplicate_definitions(source_text, new_content):
            return {"success": False, "error": (
                f"structural_edit: replacing '{selector}' this way would define "
                f"{dup} twice in {path}. Your replacement carries code that is "
                f"already in the file outside the node being replaced, so it "
                f"gets spliced in a second time while the original stays. Send "
                f"ONLY the new body of {selector} — nothing that already exists "
                f"elsewhere in the file."
            )}
        try:
            compile(new_content, path, "exec")
        except SyntaxError as e:
            # e.text for a multi-line literal is the WHOLE literal, so this
            # was pasting an entire ~190-line template into the error as the
            # "offending line" — flooding the context the model needs to
            # recover with the very thing it just failed to reproduce.
            _raw = (e.text or "").strip()
            snippet = _raw.split("\n", 1)[0][:160]
            if snippet != _raw:
                snippet += " ..."
            # When the content really is entity-encoded, say so FIRST. The
            # generic checklist below already mentions entities, but a
            # SyntaxError from `&lt;head&gt;` points at a line far from the
            # cause (an unterminated string several lines down), and a model
            # handed a line number plus a list of five things to check tends
            # to re-emit the same encoding. Observed live: two consecutive
            # entity-encoded replacements, neither corrected.
            entities = [ent for ent in ("&lt;", "&gt;", "&quot;", "&amp;", "&#39;")
                        if ent in content]
            lead = ""
            # An unterminated string here is the signature of re-emitting a
            # large template inside the node and truncating it. Observed live:
            # seven consecutive structural_edits on function:index, each
            # failing this way, because the JS the model wanted to change lives
            # in a module-level template the function only renders. The
            # tag-selector message says this, but the model never sees it —
            # function:index is a VALID selector, so it never takes that path.
            # Must be a TRIPLE-QUOTED string specifically. An ordinary
            # unterminated string ("return 'oops") is a plain quoting slip and
            # gets the general advice; only a cut-off triple-quoted literal is
            # the template case this describes. Python words them differently
            # ("unterminated string literal" vs "unterminated triple-quoted
            # string literal"), and matching the shorter one fired template
            # advice on every quoting error — and, because it set `lead`,
            # silently suppressed the large-node message below it. Caught in
            # CI on 3.12; the host's 3.9 says "EOL while scanning" and never
            # tripped it.
            node_lines = source_text[target.start_byte:target.end_byte].count("\n") + 1
            # The inverse case, and the more confusing one: a SMALL node with a
            # huge replacement. The model wants to change markup or JS that
            # lives in a module-level string constant, cannot select into a
            # string, and settles for the nearest function — then inlines the
            # whole template into it. Observed live: `function:index` in a Flask
            # app is 2 lines (`return render_template_string(HTML_TEMPLATE)`)
            # and the replacement was ~190, three attempts running. Every
            # rejection talked about quoting, because the content really was
            # malformed, so nothing ever said the selector could not hold this.
            # The small-node/huge-replacement case is refused before the
            # splice now (see _replacement_dwarfs_node), so it cannot reach
            # here.

            _msg = (e.msg or "").lower()
            # `and not lead` matters: the wrong-node steer above is the more
            # specific diagnosis. Quoting broke as a CONSEQUENCE of re-emitting
            # a node that cannot hold the change, so telling the model to fix
            # its quotes sends it back to reproduce the same 190 lines again.
            if ("triple-quoted" in _msg or "eof while scanning triple-quoted" in _msg) and not lead:
                lead = ("An unterminated string usually means you re-emitted a "
                        "large template literal and it got cut off. If the code "
                        "you actually need to change lives inside that template, "
                        "do not rewrite the node at all: use edit_file with "
                        "old_str set to one unique line copied from inside the "
                        "template. ")
            if entities:
                lead = (f"Your replacement contains HTML-escaped characters "
                        f"({', '.join(entities)}) where the file needs literal "
                        f"ones. Re-emit it with literal < > \" & — a JSON string "
                        f"carries those directly and must not entity-encode them. ")
            # A big node is the wrong unit of work for a small change, and
            # re-emitting it is where compact models fall over. Observed on a
            # 1,702-line file: the model navigated correctly to the right
            # ~200-line function, then failed three times trying to re-emit it
            # to add six lines, each attempt a different syntax error. Say so
            # once the syntax check has already failed — structural_edit is
            # still the right tool for a whole-node rewrite that the model can
            # actually produce.
            node_lines = source_text[target.start_byte:target.end_byte].count("\n") + 1

            if node_lines >= 40 and not lead:
                lead = (f"`{selector}` is {node_lines} lines, and structural_edit "
                        f"replaces the WHOLE node, so a small change means "
                        f"re-emitting all {node_lines} lines correctly. If you are "
                        f"changing only part of it, use edit_file instead with "
                        f"old_str set to one unique line from the region you are "
                        f"changing — that edits in place and cannot truncate. If you "
                        f"are ADDING code rather than changing it, use insert_after "
                        f"with the line number read_file printed: there is no anchor "
                        f"to reproduce at all. ")
            return {"success": False, "error": (
                f"structural_edit: the replacement makes {path} invalid Python — "
                f"SyntaxError at line {e.lineno}: {e.msg}"
                + (f" (offending line: {snippet})" if snippet else "")
                + f". The file was NOT modified. {lead}"
                # The checklist ends in "re-emit the full node", which
                # contradicts a lead that just said to use a different tool.
                # Only append it when the advice is still "try this node again".
                + ('' if "edit_file" in lead else
                   'Check your quoting (no doubled '
                   'quotes like ["id""], no escaped \\" inside the content, no '
                   'HTML entities like &quot;) and re-emit the full node.')
            )}

        # Semantic no-op gate. The proxy already rejects a replacement that is
        # byte-identical to the node, but a model that has lost the thread
        # writes its deliberation into the node as comments and leaves the code
        # untouched — observed live: the replacement for index() carried "Wait,
        # the instruction is to update the JS inside the HTML_TEMPLATE string"
        # as five comment lines, the splice succeeded, the file parsed, the app
        # answered curl, and the model reported a feature it had never added.
        # Comments are absent from the AST, so comparing the parsed trees is
        # exactly the "did anything executable change" question, and it costs
        # one parse. Fails open when the ORIGINAL does not parse — a repair in
        # progress must not be blocked.
        try:
            import ast as _ast
            if _ast.dump(_ast.parse(source_text)) == _ast.dump(_ast.parse(new_content)):
                return {"success": False, "error": (
                    f"structural_edit: your replacement for `{selector}` changes "
                    f"no code — only comments or formatting differ, so {path} "
                    f"would behave exactly as it does now. If the code you need "
                    f"to change lives inside a string literal (an HTML template, "
                    f"a SQL block), no selector reaches it: use edit_file "
                    f"anchored on one unique line from inside that string. If "
                    f"you did mean to change only comments, use edit_file for "
                    f"that too. Otherwise re-emit the node with the executable "
                    f"change actually in it."
                )}
        except SyntaxError:
            # This gate only exists to catch a semantic no-op, and it needs
            # BOTH sides to parse to compare their ASTs. If either fails, the
            # edit is not a no-op question at all — it is a syntax problem,
            # and the caller below reports that with a far better message.
            pass

    return {
        "success": True,
        "language": language,
        "selector": selector,
        "new_content": new_content,
        "byte_range": [target.start_byte, target.end_byte],
        "old_size": len(source),
        "new_size": len(new_bytes),
    }


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


def _embedded_available() -> bool:
    return bool(_STRUCTURAL_EDIT_AVAILABLE and _EMBEDDED_SCRIPT_AVAILABLE)


def _embedded_symbol_note(path: str, source_text: str, selector: str) -> str:
    """The whole error message when the selector's name exists as embedded
    code, or "" when it does not.

    A selector-not-found error is where the model actually looks — it does not
    call outline_file first. Observed on three runs, all opening with
    `function:draw` against a Flask app whose game loop lives in
    HTML_TEMPLATE, and re-sending it after being told the symbol did not
    exist: which, from the file it had just read, was plainly false.
    """
    wanted = selector.split(":", 1)[1].strip() if ":" in selector else ""
    if not wanted:
        return ""
    try:
        regions = embedded_region_outline(path, source_text)
    except Exception:
        return ""
    for r in regions:
        if wanted in r["symbols"]:
            return (f"`{wanted}` exists in {path}, but NOT as a node any selector can reach: "
                    f"it is {r['kind']} at lines {r['start_line']}-{r['end_line']}, inside "
                    f"{r['where']}. To the host grammar that whole block is one string "
                    f"literal, so `structural_edit` cannot address anything in it however "
                    f"many lines it spans. Change it with replace_lines on lines "
                    f"{r['start_line']}-{r['end_line']}, edit_file with old_str set to one "
                    f"unique line copied from it, or insert_after.")
    return ""


def _embedded_regions_note(path: str, source_text: str) -> str:
    """A trailing note naming embedded regions, for when the missing selector
    is not one of their symbols either."""
    try:
        regions = embedded_region_outline(path, source_text)
    except Exception:
        return ""
    if not regions:
        return ""
    described = "; ".join(
        f"{r['kind']} at lines {r['start_line']}-{r['end_line']}"
        + (f" defining {', '.join(r['symbols'])}" if r["symbols"] else "")
        for r in regions)
    return (f" This file also holds embedded code no selector reaches ({described}). "
            f"For anything in there use replace_lines, edit_file or insert_after.")


def embedded_region_outline(path: str, source_text: str) -> list:
    """Regions of `source_text` that hold code in ANOTHER language.

    Returns [{where, start_line, end_line, kind, symbols}] — the <script> and
    <style> blocks of an HTML file, and the ones inside Python string literals
    (the render_template_string shape).

    outline_file otherwise reports only what the host grammar sees. For a
    Flask app whose whole UI is one module-level string, that is
    `function:index` and nothing else, so a model asked to change the game
    loop reaches for `structural_edit selector="function:draw"` — a symbol the
    outline never mentioned and no selector can reach, because the template is
    one string literal to the Python grammar however many lines it spans.
    Observed on two consecutive runs, both opening with exactly that call.

    Naming the regions makes the view true. It does not ask the model to
    reason better about nesting; it stops hiding the nesting.
    """
    if not _embedded_available():
        return []
    try:
        blocks = embedded_script_blocks_for(path, source_text)
    except Exception:
        return []
    if not blocks:
        return []
    source = source_text.encode("utf-8")
    out = []
    for kind, offset, body, where in blocks:
        start = source[:offset].count(b"\n") + 1
        out.append({
            "where": where,
            "kind": kind,
            "start_line": start,
            "end_line": start + body.count(b"\n"),
            "symbols": _js_function_names(body) if kind == "javascript" else [],
        })
    return out


def _js_function_names(block: bytes) -> list:
    """Names of the functions declared in a JavaScript block, in source order.

    These are the names a model will try to select by, so the outline has to
    show them alongside the note that no selector reaches them.
    """
    parser = _ts.Parser(_JS_LANG)
    tree = parser.parse(block)
    names, seen = [], set()

    def walk(node):
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
            walk(child)

    walk(tree.root_node)
    return names


def orphaned_new_symbols(previous_text: str, source_text: str) -> list:
    """Top-level Python functions the edit ADDED that nothing references.

    The mirror of the unresolved-call check: that one catches a call with no
    definition, this catches a definition with no callers. Adding a function
    and forgetting to wire it up is what "add a feature" fails as — observed
    on "add a done command that marks a task complete": `done_task` was
    written correctly and the argv dispatcher was never touched, so the
    command silently did nothing and `python todo.py done 1` still exited 0.

    Only NEWLY added names are considered, so a codebase full of
    externally-called helpers reports nothing. A name is referenced if it
    appears anywhere outside its own `def` line — a call, a decorator, a
    dispatch table, an __all__ entry, a string in an argv comparison.
    Deliberately loose: the cost of missing one is a warning not shown, and
    the cost of a false one is a user told their working code is broken.

    Returns [{"name", "line"}], empty when nothing qualifies or the file does
    not parse.
    """
    if not _STRUCTURAL_EDIT_AVAILABLE:
        return []
    try:
        before = {n for n, kind, _s, _e in
                  _symbol_index_for_python_source(previous_text.encode("utf-8"))
                  if kind == "function"}
        after = list(_symbol_index_for_python_source(source_text.encode("utf-8")))
    except Exception:
        return []

    lines = source_text.splitlines()
    orphans = []
    for name, kind, start_byte, _end in after:
        if kind != "function" or name in before:
            continue
        if name.startswith("_") or name.startswith("test_"):
            continue  # private helpers and pytest-collected tests
        def_line = source_text[:start_byte].count("\n")
        referenced = False
        for i, line in enumerate(lines):
            if i == def_line:
                continue
            if name in line:
                referenced = True
                break
        if not referenced:
            orphans.append({"name": name, "line": def_line + 1})
    return orphans


def _replacement_dwarfs_node(source_text: str, target, content: str, selector: str) -> str:
    """A refusal when `content` is many times the size of the node it replaces,
    or "" when the replacement is node-sized.

    The model wants to change markup or JavaScript that lives in a module-level
    string constant, cannot select into a string, and settles for the nearest
    function — then inlines the whole template into it. Observed live:
    `function:index` in a Flask app is 3 lines (`return
    render_template_string(HTML_TEMPLATE)`), the replacement was 194, and the
    result was a file with no `@app.route` left in it.

    Size alone is not the test. Writing a real implementation over a `pass`
    stub is also many times the node, and refusing that would block ordinary
    work. What separates the two is that the blob is a MOVE: most of what it
    contains already exists in the file, outside the node being replaced. A
    genuine implementation is new text.
    """
    node_lines = source_text[target.start_byte:target.end_byte].count("\n") + 1
    new_lines = content.count("\n") + 1
    if new_lines < node_lines * 5 or new_lines - node_lines < 30:
        return ""
    # Suspicious by size. Refuse only with a second signal, so that writing a
    # real body over a stub stays allowed. Either one is enough:
    #
    #   (a) the replacement is a MOVE — most of it already exists in the file
    #       outside this node (the 258-line HTML_TEMPLATE case);
    #   (b) the file HAS a module-level string constant big enough to be the
    #       real target, so a small node swelling by 30+ lines is the model
    #       reaching for markup it cannot select into — whether it pastes the
    #       existing template or writes a fresh one inline.
    #
    # A plain stub file has neither, which is why an implementation of any
    # length passes.
    where = _large_string_constants(source_text)
    if not where:
        outside = source_text[:target.start_byte] + source_text[target.end_byte:]
        # Short lines (`}`, `else:`, blank) collide across unrelated code, so
        # only lines with real content vote.
        elsewhere = {ln.strip() for ln in outside.splitlines() if len(ln.strip()) > 20}
        body = [ln.strip() for ln in content.splitlines() if len(ln.strip()) > 20]
        reused = sum(1 for ln in body if ln in elsewhere)
        if not body or reused < 30 or reused * 2 < len(body):
            return ""
    msg = (f"structural_edit: `{selector}` is only {node_lines} line(s) but your "
           f"replacement is {new_lines} — you are moving code INTO this node that "
           f"does not live here, which would delete whatever the node actually "
           f"held. The file was NOT modified. ")
    if where:
        msg += (f"The bulk of this file is in {where}, a module-level string. No "
                f"selector reaches inside a string literal: a template is ONE "
                f"literal to the grammar however many lines it spans. ")
    msg += ("To change code inside that string, use replace_lines with the line "
            "numbers read_file printed (up to 60 lines per call, split into "
            "consecutive calls from the bottom up for more), edit_file with "
            "old_str set to one unique line, or insert_after to add at a line.")
    return msg


def _large_string_constants(source_text: bytes) -> str:
    """Name module-level string constants big enough to be the real target.

    When a selector cannot reach the code the model is editing, the code is
    usually sitting in one of these. Naming it turns "your quoting is wrong"
    into "you are editing the wrong thing".
    """
    import ast as _ast
    try:
        tree = _ast.parse(source_text)
    except SyntaxError:
        return ""
    found = []
    for node in tree.body:
        if not isinstance(node, _ast.Assign):
            continue
        value = node.value
        if not (isinstance(value, _ast.Constant) and isinstance(value.value, str)):
            continue
        span = (value.end_lineno or 0) - value.lineno + 1
        if span < 20:
            continue
        for tgt in node.targets:
            if isinstance(tgt, _ast.Name):
                found.append(f"`{tgt.id}` (lines {value.lineno}-{value.end_lineno})")
    return ", ".join(found[:2])


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
        base += len(line) + 1  # +1 for the '\n' that split() removed
    return bytes(out)


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

    def declared_names(decl):
        """(name, node) for each plain identifier a lexical declaration binds.
        Destructuring patterns are skipped — conservative on purpose."""
        for child in decl.named_children:
            if child.type != "variable_declarator":
                continue
            ident = child.child_by_field_name("name")
            if ident is not None and ident.type == "identifier":
                yield ident.text.decode("utf-8", "replace"), ident

    def walk(node, scope):
        if node.type == "lexical_declaration":
            for name, ident in declared_names(node):
                if name in scope:
                    return (ident.start_byte, name)
                scope[name] = ident.start_byte
        for child in node.children:
            inner = {} if child.type in _JS_SCOPE_NODES else scope
            hit = walk(child, inner)
            if hit is not None:
                return hit
        return None

    hit = walk(tree.root_node, {})
    if hit is None:
        return None
    offset, name = hit
    return (offset,
            f"`{name}` is declared twice in the same scope",
            f"A repeated `let`/`const` is a SyntaxError before anything runs, so "
            f"the whole script is dead, not just this line. Drop this declaration "
            f"and use the existing `{name}`, or rename one of them.")


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

    def call_target(node):
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

    def walk(node, enclosing):
        hit = call_target(node)
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
            walk(child, name)

    walk(tree.root_node, "")
    return looping, one_shot


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
            if c == 0x5C:  # backslash escape
                i += 2
                continue
            if c == quote:
                quote = 0
            i += 1
            continue
        if c in (0x22, 0x27):  # " '
            quote = c
            i += 1
            continue
        if c == 0x2F and i + 1 < n and block[i + 1] == 0x2A:  # /*
            end = block.find(b"*/", i + 2)
            i = n if end < 0 else end + 2
            continue
        if c == 0x7B:  # {
            opens.append(i)
        elif c == 0x7D:  # }
            if not opens:
                return (i, "an extra `}`",
                        "Delete the stray `}` — no rule is open at that point.")
            opens.pop()
        i += 1
    if opens:
        return (opens[0], "a `{` that is never closed",
                "Close the rule with `}` — everything after an unclosed rule is "
                "dropped by the browser.")
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
        if name:
            attrs[name] = value
    return attrs


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
        kind = "javascript" if node.type == "script_element" else "css"
        start_tag = next((c for c in node.children if c.type == "start_tag"), None)
        raw = next((c for c in node.children if c.type == "raw_text"), None)
        if start_tag is None or raw is None:
            continue
        attrs = _start_tag_attributes(start_tag, html)
        if kind == "javascript":
            if "src" in attrs:
                continue  # the browser ignores an inline body on a src script
            if attrs.get("type", "").strip().lower() not in _JS_SCRIPT_TYPES:
                continue  # text/template, application/json, importmap, ...
        body = html[raw.start_byte:raw.end_byte]
        if not body.strip():
            continue
        if no_escapes and b"\\" in body:
            # Python would render the escape, so the source bytes we'd parse
            # are not the bytes the browser receives.
            continue
        tag = "<script>" if kind == "javascript" else "<style>"
        out.append((kind, base_offset + raw.start_byte, body,
                    f"the {tag} block{where_suffix}"))
    return out


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
        prefix_node = next((c for c in node.children if c.type == "string_start"), None)
        if prefix_node is not None:
            prefix = source[prefix_node.start_byte:prefix_node.end_byte].decode("utf-8", "replace")
            if "f" in prefix.lower().replace('"', "").replace("'", ""):
                continue  # f-string: `{`/`}` are Python's, not JavaScript's
        contents = [c for c in node.children if c.type == "string_content"]
        if len(contents) != 1:
            continue
        content = source[contents[0].start_byte:contents[0].end_byte]
        low = content.lower()
        if b"<script" not in low and b"<style" not in low:
            continue
        out.append((contents[0].start_byte, content, _python_string_label(node, source)))
    return out


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


def embedded_script_check(path: str, source_text: str, previous_text: str = "") -> dict:
    """Syntax-check the JavaScript / CSS embedded in `source_text`.

    Handles two carriers:
      (a) .html / .htm / .jinja / .jinja2 — <script> and <style> blocks;
      (b) .py — HTML held in a string literal (the render_template_string
          pattern), which is the shape that shipped a broken snake game.

    With `previous_text` (the pre-edit file) it also reports a render loop the
    edit stopped: a function a recurring timer used to drive that now fires
    once and never re-arms. That code parses, the server still starts and
    `curl /` still returns 200 — the page just freezes after one frame.

    Returns:
        ok:       False when the check COULDN'T RUN (grammar missing, non-UTF-8
                  source). Callers fail open on it. A file with no embedded
                  script is ok:True with no findings.
        findings: [{line, column, kind, where, message, hint, text}] against
                  the FILE's line numbering, capped at _EMBEDDED_MAX_FINDINGS.
    """
    if not _embedded_available():
        return {"ok": False, "error": "tree-sitter javascript grammar not installed in this build"}
    try:
        source = source_text.encode("utf-8")
    except (UnicodeEncodeError, AttributeError) as e:
        return {"ok": False, "error": f"source not utf-8: {e}"}

    p = (path or "").lower()
    if len(source) > _EMBEDDED_MAX_BYTES:
        return {"ok": True, "findings": [], "skipped": "source larger than the embedded-check cap"}

    if not (p.endswith(_EMBEDDED_HTML_EXTS) or p.endswith(".py")):
        return {"ok": True, "findings": []}
    try:
        blocks = embedded_script_blocks_for(path, source_text)
    except Exception as e:
        return {"ok": False, "error": f"parse failed: {type(e).__name__}: {e}"}

    findings = []
    for kind, offset, body, where in blocks:
        defect = ""
        try:
            if kind == "javascript":
                hit = _js_error(body)
                if hit is None:
                    # Parses, but a repeated lexical binding still refuses to
                    # run — checked second because a broken parse makes the
                    # scope walk meaningless.
                    hit = _js_redeclaration(body)
                    if hit is not None:
                        defect = "redeclaration"
            else:
                hit = _css_error(body)
        except Exception as e:
            return {"ok": False, "error": f"parse failed: {type(e).__name__}: {e}"}
        if hit is None:
            continue
        block_offset, message, hint = hit[0], hit[1], hit[2]
        opener_offset = hit[3] if len(hit) > 3 else None
        line, column, text = _line_at(source, offset + block_offset)
        finding = {
            "line": line,
            "column": column,
            "kind": kind,
            "defect": defect,
            "where": where,
            "message": message,
            "hint": hint,
            "text": text,
        }
        if opener_offset is not None:
            oline, _ocol, otext = _line_at(source, offset + opener_offset)
            if oline != line:
                # The line the model has to look at. Reporting only the
                # parser's stopping point sends it to fix a line it never
                # touched.
                finding["opened_line"] = oline
                finding["opened_text"] = otext
        findings.append(finding)
        if len(findings) >= _EMBEDDED_MAX_FINDINGS:
            break
    if previous_text and len(findings) < _EMBEDDED_MAX_FINDINGS:
        findings.extend(_stopped_loop_findings(path, source, previous_text, blocks))
    findings.sort(key=lambda f: f["line"])
    return {"ok": True, "findings": findings[:_EMBEDDED_MAX_FINDINGS]}


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

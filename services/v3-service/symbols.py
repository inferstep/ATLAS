"""Tree-sitter structural tooling: friendly-selector structural edits, symbol
indexing, direct-call resolution (structural_score), embedded-script syntax
checking, traceback frame parsing, and cyclomatic complexity.

This file holds the entry points that other code imports. Their helpers are
in grammars.py (the grammars and the selector queries), edit_checks.py (the
checks that refuse an edit) and embedded_script.py (the JavaScript and CSS
inside HTML)."""

import contextlib
import re

import node_splice
from edit_checks import _post_splice_refusal, _replacement_dwarfs_node
from embedded_script import (_EMBEDDED_HTML_EXTS, _EMBEDDED_MAX_BYTES, _EMBEDDED_MAX_FINDINGS,
                             _css_error, _js_error, _js_function_names, _js_redeclaration,
                             _line_at, _stopped_loop_findings, embedded_script_blocks_for)
from grammars import (_EMBEDDED_SCRIPT_AVAILABLE, _PY_LANG, _STRUCTURAL_EDIT_AVAILABLE,
                      _ast_language_for_path, _ast_selector_to_query, _supported_structural_exts)

# tree-sitter itself can be missing from a build; then grammars.py binds no
# _ts and this file binds none either. The code that parses runs only when
# _STRUCTURAL_EDIT_AVAILABLE is true.
with contextlib.suppress(ImportError):
    from grammars import _ts


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


def _extract_python_imports(source: bytes) -> set:  # noqa: C901
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

    def walk(node):  # noqa: C901
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


def _extract_python_bound_names(source: bytes) -> set:  # noqa: C901
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
        except Exception:  # noqa: S112
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
    if refusal := _match_count_refusal(path, source_text, selector, language, targets):
        return refusal

    target = _replaced_node(targets, language)
    content, new_bytes, splice_error = node_splice.splice(source, target, content, path, language)
    if splice_error:
        return {"success": False, "error": splice_error}
    new_content = new_bytes.decode("utf-8")

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

    if refusal := _post_splice_refusal(path, source_text, selector, content, target, new_content, language):
        return refusal

    return {
        "success": True,
        "language": language,
        "selector": selector,
        "new_content": new_content,
        "byte_range": [target.start_byte, target.end_byte],
        "old_size": len(source),
        "new_size": len(new_bytes),
    }


def _match_count_refusal(path: str, source_text: str, selector: str, language: str,
                         targets: list):
    """The refusal when the selector matched no node or more than one, or None
    when it matched exactly one."""
    if len(targets) == 0:
        available = _defined_selectors_note(source_text, language)
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
    return None


def _defined_selectors_note(source_text: str, language: str) -> str:
    """The note naming the selectors a Python file defines, for the refusal of
    a selector that matched no node; "" when there is nothing to name."""
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
    return available


def _replaced_node(targets: list, language: str):
    """The node the edit replaces: the one match, or for a decorated Python
    function or class the decorated_definition around it."""
    target = targets[0]
    # Python grammar wraps decorated functions/classes in decorated_definition.
    # function:dashboard matches the inner function_definition; if its parent
    # is decorated_definition we want THAT byte range so @app.route(...) lines
    # get replaced too. Otherwise the model writes new @decorator lines and
    # the old ones stay, double-decorating the function.
    if language == "python" and target.parent is not None and target.parent.type == "decorated_definition":
        target = target.parent
    return target


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


def _python_top_level_bindings(text: str):
    """What a Python module binds at its top level, and how.

    {name: "def" | "class" | "assign" | "import:<module>"}, read from the
    statements directly in the module body. A name bound inside an `if`,
    `try` or `with` is not read, so neither is the `if __name__ ==
    "__main__":` block: its names exist only when the file runs as a script.
    None when the text does not parse.
    """
    import ast as _ast
    try:
        tree = _ast.parse(text)
    except (SyntaxError, ValueError):
        return None
    out = {}
    for node in tree.body:
        if isinstance(node, (_ast.FunctionDef, _ast.AsyncFunctionDef)):
            out[node.name] = "def"
        elif isinstance(node, _ast.ClassDef):
            out[node.name] = "class"
        elif isinstance(node, _ast.Assign) or (
                isinstance(node, _ast.AnnAssign) and node.value is not None):
            targets = node.targets if isinstance(node, _ast.Assign) else [node.target]
            for target in targets:
                for n in _ast.walk(target):
                    if isinstance(n, _ast.Name) and isinstance(n.ctx, _ast.Store):
                        out[n.id] = "assign"
        elif isinstance(node, _ast.ImportFrom):
            module = "." * node.level + (node.module or "")
            for alias in node.names:
                if alias.name != "*":
                    out[alias.asname or alias.name] = "import:" + module
        elif isinstance(node, _ast.Import):
            for alias in node.names:
                # `import pkg.util` binds `pkg`, and what it imports is
                # pkg.util: the module is what says where the name comes from.
                out[alias.asname or alias.name.split(".")[0]] = "import:" + alias.name
    return out


def _project_module_names(project_files) -> set:
    """The import names the project's own Python files answer to.

    `pkg/util.py` answers to `pkg.util` and, for a relative or path-rooted
    import, to `util`.
    """
    names = set()
    for path in project_files or ():
        p = re.sub(r"^(?:\./)+", "", str(path).replace("\\", "/"))
        if not p.endswith(".py"):
            continue
        dotted = p[:-3].replace("/", ".")
        if dotted.endswith(".__init__"):
            dotted = dotted[:-len(".__init__")]
        names.add(dotted)
        names.add(dotted.rsplit(".", 1)[-1])
    return names


def dropped_top_level_names(incumbent_text: str, candidate_text: str,
                            path: str, project_files=()) -> list:
    """The file's top-level names a replacement no longer binds.

    The role check (#259). A file's top-level names are how the rest of the
    project uses it: other files import them, a test runner collects them, a
    caller reads its constants. A replacement that drops one has changed what
    the file is, whatever else it gets right. Observed: V3's winner for
    test_stats.py was a module of median() implementations, test_mean and
    test_median were gone, and running the file tested nothing; another
    winner renamed store.py's FILE_PATH.

    A name the file imports from ANOTHER PROJECT FILE is part of that role
    too -- it is how a test or a script uses the code it exists for -- so it
    must still be imported from the same module. Defining it locally instead
    is the replacement no longer using that file. Names imported from outside
    the project (the standard library, a package) may come and go.

    Returns ["test_mean", "mean (imported from stats)", ...], sorted. Empty
    when either side does not parse (the syntax check owns that) or the file
    is not Python: this reads Python modules only.
    """
    if not str(path or "").lower().endswith((".py", ".pyw", ".pyi")):
        return []
    before = _python_top_level_bindings(incumbent_text or "")
    after = _python_top_level_bindings(candidate_text or "")
    if before is None or after is None:
        return []
    project = _project_module_names(project_files)
    dropped = []
    for name, kind in before.items():
        if kind.startswith("import:"):
            module = kind[len("import:"):]
            bare = module.lstrip(".")
            if bare not in project and bare.rsplit(".", 1)[-1] not in project:
                continue
            if after.get(name) != kind:
                dropped.append(f"{name} (imported from {module})")
        elif name not in after:
            dropped.append(name)
    return sorted(dropped)


def embedded_script_check(path: str, source_text: str, previous_text: str = "") -> dict:  # noqa: C901
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

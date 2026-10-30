"""The checks that can refuse a structural_edit after the splice: a
replacement far larger than the node it replaces, and, for Python, a result
that defines a name twice, does not compile, or changes no code."""


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


def _post_splice_refusal(path: str, source_text: str, selector: str, content: str,
                         target, new_content: str, language: str):
    """The refusal for a file the splice would leave broken: for Python, a
    duplicate definition, a syntax error or no code change. None when the edit
    may go ahead."""
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
            return _syntax_error_refusal(e, path, source_text, selector, content, target)

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
    return None


def _syntax_error_refusal(e: SyntaxError, path: str, source_text: str, selector: str,
                          content: str, target) -> dict:
    """The refusal for a replacement that leaves the Python file failing to
    compile: the error, the offending line and the advice that fits it."""
    # e.text for a multi-line literal is the WHOLE literal, so this
    # was pasting an entire ~190-line template into the error as the
    # "offending line" — flooding the context the model needs to
    # recover with the very thing it just failed to reproduce.
    _raw = (e.text or "").strip()
    snippet = _raw.split("\n", 1)[0][:160]
    if snippet != _raw:
        snippet += " ..."
    lead = _syntax_error_lead(e, source_text, selector, content, target)
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


def _syntax_error_lead(e: SyntaxError, source_text: str, selector: str, content: str,
                       target) -> str:
    """The sentence that opens a syntax-error refusal with the likely cause, or
    "" when none of the known causes fits."""
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
    return lead


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

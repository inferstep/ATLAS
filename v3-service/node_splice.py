"""Place a replacement node into a source file.

A node is spliced in at its first byte. For a top-level node that is column 0.
For a method it is the column after its line's indentation, so the replacement
has to arrive at that column for the file to parse: first line bare, later
lines at their columns in the file. A model usually sends the node at column 0
or with its original indentation on every line, so a Python replacement is
re-based onto the node's column before the splice.
"""
import io
import tokenize
from typing import List, Optional, Set, Tuple


def splice(source: bytes, target, content: str, path: str,
           language: str) -> Tuple[str, bytes, Optional[str]]:
    """Replace the tree-sitter node `target` in `source` with `content`.

    Returns (content_used, new_source, error). `content_used` differs from
    `content` when a Python replacement was re-based.
    """
    new_source = _spliced(source, target, content)
    try:
        new_text = new_source.decode("utf-8")
    except UnicodeDecodeError as e:
        return content, b"", f"replacement produced invalid utf-8: {e}"
    if language != "python":
        return content, new_source, None
    rebased, refusal = _rebase_python(source, target, content, path, new_text)
    if refusal:
        return content, b"", refusal
    if rebased is None:
        return content, new_source, None
    return rebased, _spliced(source, target, rebased), None


def _spliced(source: bytes, target, content: str) -> bytes:
    return source[:target.start_byte] + content.encode("utf-8") + source[target.end_byte:]


def _leading_ws(row: str) -> str:
    return row[:len(row) - len(row.lstrip(" \t"))]


def _compile_error(text: str, path: str) -> Optional[SyntaxError]:
    try:
        compile(text, path, "exec")
    except SyntaxError as e:
        return e
    return None


def _row_roles(content: str) -> Optional[Tuple[Set[int], Set[int]]]:
    """Classify the rows of a Python fragment, whatever its indentation.

    Returns (statement_rows, string_rows) as 0-based row numbers, or None when
    the fragment does not tokenize (an unterminated string, for example).
    `statement_rows` are rows where a statement starts. `string_rows` are rows
    that begin inside a string literal: their leading whitespace is data.

    Rows are tokenized with their leading whitespace removed, so indentation
    cannot make this fail, and a string still starts and ends on the same rows.
    """
    flat = "".join(row.lstrip(" \t") for row in content.splitlines(keepends=True))
    statements: Set[int] = set()
    strings: Set[int] = set()
    fstring_start = getattr(tokenize, "FSTRING_START", None)
    fstring_end = getattr(tokenize, "FSTRING_END", None)
    open_fstrings: List[int] = []
    not_code = {tokenize.NL, tokenize.NEWLINE, tokenize.COMMENT, tokenize.INDENT,
                tokenize.DEDENT, tokenize.ENCODING, tokenize.ENDMARKER}
    at_statement_start = True
    try:
        for tok in tokenize.generate_tokens(io.StringIO(flat).readline):
            if tok.type == tokenize.NEWLINE:
                at_statement_start = True
                continue
            if at_statement_start and tok.type not in not_code:
                statements.add(tok.start[0] - 1)
                at_statement_start = False
            # Token rows are 1-based: a string from row s to row e covers the
            # 0-based rows s .. e-1 after its first row.
            if tok.type == tokenize.STRING and tok.end[0] > tok.start[0]:
                strings.update(range(tok.start[0], tok.end[0]))
            elif tok.type == fstring_start:
                open_fstrings.append(tok.start[0])
            elif tok.type == fstring_end and open_fstrings:
                strings.update(range(open_fstrings.pop(), tok.end[0]))
    except (tokenize.TokenError, SyntaxError):
        return None
    return statements, strings


def _rebase_python(source: bytes, target, content: str, path: str,
                   sent_text: str) -> Tuple[Optional[str], Optional[str]]:
    """Re-base a Python replacement onto the column where its node starts.

    Returns (rebased, refusal). Both None means: use the content as sent.

    The content is used as sent when it already fits: its first statement
    lands on the node's column, no statement lands left of that column (which
    would put it outside the node's class or function), and the file compiles.
    Otherwise the content is dedented to its own base, the smallest indentation
    of a row that starts a statement, and every row after the first gets the
    node line's indentation. The first row gets none: the splice point is
    already at the node's column. Tabs and spaces are never converted into
    each other; a mix is refused.
    """
    line_start = target.start_byte - target.start_point[1]
    prefix_bytes = source[line_start:target.start_byte]
    if prefix_bytes.strip(b" \t"):
        return None, None  # the node shares its line with other code
    prefix = prefix_bytes.decode("utf-8")
    roles = _row_roles(content)
    if not roles or not roles[0]:
        return None, None  # no statement to place; the syntax gate reports it
    statements, strings = roles
    rows = content.splitlines(keepends=True)
    col, first = len(prefix), min(statements)

    def lands(i: int) -> int:
        """Column of row i when the content is spliced in as sent."""
        indent = len(_leading_ws(rows[i]))
        return col + indent if i == 0 else indent

    in_scope = lands(first) == col and all(lands(i) >= col for i in statements)
    sent_error = _compile_error(sent_text, path) if in_scope else None
    if in_scope and sent_error is None:
        return None, None

    code_rows = [i for i, row in enumerate(rows) if row.strip() and i not in strings]
    if len(set(prefix) | set("".join(_leading_ws(rows[i]) for i in code_rows))) > 1:
        return None, _mixed_indent_refusal(prefix)
    base = min(len(_leading_ws(rows[i])) for i in statements)
    if len(_leading_ws(rows[first])) != base:
        return None, None  # not one block that starts at its first statement
    rebased = _rebased(rows, strings, base, prefix)
    if rebased == content:
        return None, None
    rebased_error = _compile_error(_spliced(source, target, rebased).decode("utf-8"), path)
    if rebased_error is None or not in_scope:
        return rebased, None
    # Neither form compiles. Re-basing cannot fix quoting, but when it turns an
    # indentation error into another error, that is the one to report.
    if isinstance(sent_error, IndentationError) and not isinstance(rebased_error, IndentationError):
        return rebased, None
    return None, None


def _rebased(rows: List[str], strings: Set[int], base: int, prefix: str) -> str:
    """Dedent the rows by `base` and indent every row after the first by `prefix`.

    Rows that begin inside a string literal, and blank rows, stay as they are.
    """
    out = []
    for i, row in enumerate(rows):
        if i in strings or not row.strip():
            out.append(row)
            continue
        indent = len(_leading_ws(row))
        body = row[min(indent, base):]
        out.append(body if i == 0 else prefix + body)
    return "".join(out)


def _mixed_indent_refusal(prefix: str) -> str:
    if "\t" in prefix:
        fix = "This node is indented with tabs. Re-send the node indented with tabs only."
    elif prefix:
        fix = "This node is indented with spaces. Re-send the node indented with spaces only."
    else:
        fix = "Re-send the node indented with spaces only, or with tabs only."
    return ("structural_edit: the replacement's indentation mixes tabs and spaces, "
            "so it cannot be placed without guessing. The file was NOT modified. " + fix)

"""structural_edit across Go, TypeScript and JavaScript.

v1 shipped Python and HTML. The engine is language-agnostic — tree-sitter
plus a selector mapping is all a language needs — and the gap was measured:
across 168 sessions, `unsupported file type for structural_edit` was the
only ATLAS-side wall a session hit, 12 times, when the model reached for it
on a .go file. The JavaScript grammar was already installed for
embedded_script_check and simply not wired into the dispatch.

The cases here are the ones that break a naive mapping: a Go method is a
different AST node than a func, and a TypeScript arrow function assigned to
a const is not a function_declaration at all. A model does not know which
spelling a file used, so one selector has to cover them.
"""

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

import symbols  # noqa: E402

pytestmark = pytest.mark.skipif(
    not getattr(symbols, "_STRUCTURAL_EDIT_AVAILABLE", False),
    reason="tree-sitter not installed in this environment",
)

GO = """package main

import "fmt"

func Add(a, b int) int {
\treturn a + b
}

func (s *Server) Handle(w int) error {
\treturn nil
}

type Server struct {
\tport int
}
"""

TS = """export function render(x: number): string {
  return String(x);
}

const helper = (a: number) => a * 2;

const legacy = function (b: number) { return b; };

class Board {
  reset(): void {}
}
"""

JS = """function draw(ctx) {
  ctx.clear();
}

const tick = () => { step(); };

class Game {
  start() {}
}
"""


def _edit(path, src, selector):
    return symbols.structural_edit(path, src, selector, "REPLACED\n")


@pytest.mark.parametrize("path,src,selector", [
    ("main.go", GO, "function:Add"),          # plain func
    ("main.go", GO, "function:Handle"),       # method — a different node
    ("main.go", GO, "type:Server"),
    ("app.ts", TS, "function:render"),        # declaration
    ("app.ts", TS, "function:helper"),        # arrow assigned to a const
    ("app.ts", TS, "function:legacy"),        # function expression
    ("app.ts", TS, "class:Board"),
    ("app.tsx", TS, "function:render"),
    ("game.js", JS, "function:draw"),
    ("game.js", JS, "function:tick"),         # arrow
    ("game.js", JS, "class:Game"),
])
def test_selector_resolves(path, src, selector):
    out = _edit(path, src, selector)
    assert out.get("success"), out.get("error")
    assert "REPLACED" in out["new_content"]


def test_a_language_without_a_grammar_says_what_is_supported():
    out = _edit("main.rs", "fn main() {}\n", "function:main")
    assert not out["success"]
    # The advertised list is built from the grammars that imported, so a
    # build missing one never offers it.
    assert ".go" in out["error"] and ".py" in out["error"]
    assert "edit_file" in out["error"]


def test_an_unknown_selector_names_what_the_language_takes():
    out = _edit("main.go", GO, "<div>")
    assert not out["success"]
    assert "function:NAME" in out["error"]


def test_a_missing_symbol_is_not_a_silent_success():
    out = _edit("main.go", GO, "function:Missing")
    assert not out["success"]


def test_go_method_and_func_of_the_same_name_are_ambiguous():
    """Single-match enforcement still applies: two nodes matching one
    selector must fail rather than pick one."""
    src = GO + "\nfunc Handle(x int) {}\n"
    out = _edit("main.go", src, "function:Handle")
    assert not out["success"]

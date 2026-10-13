"""scripts/code_health.py measures what docs/CODE_STYLE.md limits, and the
committed baseline matches the code."""

import importlib.util
import os

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
_spec = importlib.util.spec_from_file_location("code_health", os.path.join(ROOT, "scripts", "code_health.py"))
ch = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(ch)


def test_go_functions_and_methods_are_measured():
    src = "\n".join([
        "package x",
        "func plain() {",
        "\ta := 1",
        "}",
        "func (s *server) handle(w http.ResponseWriter) {",
        "\tfn := func() {",
        "\t}",
        "\t_ = fn",
        "}",
        "func (l list[T]) Len() int { return 0 }",
    ])
    assert dict(ch.go_functions(src)) == {"plain": 3, "server.handle": 5, "list.Len": 1}


def test_python_functions_get_qualified_names():
    src = "class A:\n    def m(self):\n        return 1\n\ndef f():\n    def inner():\n        pass\n    return inner\n"
    assert dict(ch.py_functions(src, "x.py")) == {"A.m": 2, "f": 4, "f.inner": 2}


def test_new_and_grown_items_fail_and_shrunk_ones_pass():
    base = {"functions": {"a.go:old": 150, "a.go:small": 120}, "files": {"big.go": 2000}}
    funcs = {"a.go:old": 151, "a.go:small": 110, "b.py:new": 101}
    problems = ch.check(funcs, {"big.go": 1999}, base)
    assert any("grew: a.go:old" in p for p in problems)
    assert any("new function over" in p and "b.py:new" in p for p in problems)
    assert not any("small" in p or "big.go" in p for p in problems)


def test_the_committed_baseline_matches_the_code():
    funcs, files, _ = ch.measure()
    assert ch.check(funcs, files, ch.load_baseline()) == [], (
        "a function or file is new over the limit or grew: split it (docs/CODE_STYLE.md)")

"""Tests for graph support pieces: import resolution, cache, and the
build_graph entry point (issue #39, Phase 0)."""

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

import graph  # noqa: E402
from graph.analyses import reachability  # noqa: E402
from graph.extract import available as extraction_available  # noqa: E402
from graph.types import CodeGraph, ImportsFact  # noqa: E402
from graph.resolve import resolve_imports, _module_name  # noqa: E402
from graph.cache import FileGraphCache, file_hash  # noqa: E402

_HAS_TS = extraction_available()


class TestModuleName:
    def test_paths_to_modules(self):
        assert _module_name("pkg/mod.py") == "pkg.mod"
        assert _module_name("pkg/__init__.py") == "pkg"
        assert _module_name("a/b/c.pyi") == "a.b.c"


class TestResolveImports:
    def test_exact_module(self):
        # `from pkg.util import thing` -> source "pkg.util", which is a file.
        g = CodeGraph(imports=[ImportsFact("app.py", "thing", "pkg.util")])
        resolve_imports(g, ["app.py", "pkg/util.py"])
        assert g.imports[0].resolved == "pkg/util.py"

    def test_from_pkg_import_module(self):
        # from pkg import mod  -> source "pkg", name "mod", pkg/mod.py exists
        g = CodeGraph(imports=[ImportsFact("app.py", "mod", "pkg")])
        resolve_imports(g, ["app.py", "pkg/mod.py", "pkg/__init__.py"])
        assert g.imports[0].resolved == "pkg/mod.py"

    def test_unresolved_external(self):
        g = CodeGraph(imports=[ImportsFact("app.py", "os", "os")])
        resolve_imports(g, ["app.py"])
        assert g.imports[0].resolved is None

    def test_ambiguous_suffix_not_resolved(self):
        g = CodeGraph(imports=[ImportsFact("app.py", "x", "mod")])
        resolve_imports(g, ["app.py", "a/mod.py", "b/mod.py"])
        assert g.imports[0].resolved is None  # two candidates -> leave unresolved


class TestCache:
    def test_hit_on_same_content(self):
        if not _HAS_TS:
            pytest.skip("tree-sitter not installed")
        c = FileGraphCache()
        g1 = c.get_or_extract("a.py", "def f():\n    pass\n")
        assert len(c) == 1
        g2 = c.get_or_extract("a.py", "def f():\n    pass\n")
        # Same content -> not re-parsed (cache stays size 1), but a distinct copy
        # is returned so callers can't corrupt the cached object.
        assert len(c) == 1
        assert g1 is not g2
        assert [d.name for d in g1.defines] == [d.name for d in g2.defines]

    def test_resolve_does_not_corrupt_cache_across_batches(self):
        if not _HAS_TS:
            pytest.skip("tree-sitter not installed")
        c = graph.FileGraphCache()
        # Batch A includes the util module, so a.py's import resolves.
        files_a = {"a.py": "from pkg.util import helper\n\ndef main():\n    return helper()\n",
                   "pkg/util.py": "def helper():\n    return 1\n"}
        ga = graph.build_graph(files_a, cache=c)
        assert next(i for i in ga.imports if i.name == "helper").resolved == "pkg/util.py"
        # Batch B is a.py alone. Its import must NOT still show the resolution
        # from batch A (the bug: shared mutable ImportsFact on the cached graph).
        gb = graph.build_graph({"a.py": files_a["a.py"]}, cache=c)
        assert next(i for i in gb.imports if i.name == "helper").resolved is None

    def test_miss_on_changed_content(self):
        if not _HAS_TS:
            pytest.skip("tree-sitter not installed")
        c = FileGraphCache()
        c.get_or_extract("a.py", "def f(): pass")
        c.get_or_extract("a.py", "def g(): pass")
        assert len(c) == 2  # different content -> separate entries

    def test_hash_includes_path(self):
        assert file_hash("a.py", "x") != file_hash("b.py", "x")
        assert file_hash("a.py", "x") == file_hash("a.py", "x")

    def test_lru_eviction(self):
        if not _HAS_TS:
            pytest.skip("tree-sitter not installed")
        c = FileGraphCache(max_entries=2)
        c.get_or_extract("a.py", "def a(): pass")
        c.get_or_extract("b.py", "def b(): pass")
        c.get_or_extract("c.py", "def c(): pass")
        assert len(c) == 2


class TestBuildGraph:
    def test_end_to_end_cross_file(self):
        if not _HAS_TS:
            pytest.skip("tree-sitter not installed")
        files = {
            "app.py": "from pkg.util import helper\n\ndef main():\n    return helper()\n",
            "pkg/util.py": "def helper():\n    return 1\n",
        }
        g = graph.build_graph(files)
        names = {d.name for d in g.defines}
        assert {"main", "helper"} <= names
        # cross-file import resolved to the defining file
        imp = next(i for i in g.imports if i.name == "helper")
        assert imp.resolved == "pkg/util.py"
        # reachability across the project graph
        assert reachability(g, "main", "helper") is True

    def test_ignores_non_python(self):
        g = graph.build_graph({"a.md": "# not code", "b.json": "{}"})
        assert g.defines == []

"""#147: the structural veto must catch an edit that calls a name the file
neither imports nor defines (render_template with only render_template_string
imported), using the candidate's OWN imports — i.e. with EMPTY project
symbols, the case the edit path hits when it sends no project_context."""

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

import adapters  # noqa: E402
import main  # noqa: E402
import symbols  # noqa: E402

pytestmark = pytest.mark.skipif(
    not getattr(main, "_STRUCTURAL_EDIT_AVAILABLE", False),
    reason="tree-sitter not installed",
)


def test_unimported_call_is_unresolved_with_empty_project():
    # The exact #147 shape: import render_template_string, call render_template.
    code = (
        "from flask import Flask, render_template_string\n"
        "app = Flask(__name__)\n"
        "@app.route('/')\n"
        "def index():\n"
        "    return render_template('index.html')\n"
    )
    struct = main.structural_score(set(), code)  # empty project symbols
    assert struct["ok"]
    assert "render_template" in struct["unresolved_calls"], struct
    assert struct["n_unresolved"] >= 1


def test_imported_name_resolves():
    # Calling the name that IS imported must NOT be flagged.
    code = (
        "from flask import render_template_string\n"
        "def index():\n"
        "    return render_template_string('<b>x</b>')\n"
    )
    struct = main.structural_score(set(), code)
    assert struct["ok"]
    assert "render_template_string" not in struct["unresolved_calls"], struct


def test_name_defined_elsewhere_in_file_passes():
    # A helper defined at top level in the same file resolves via local defs.
    code = (
        "def helper():\n    return 1\n"
        "def index():\n    return helper()\n"
    )
    struct = main.structural_score(set(), code)
    assert struct["ok"]
    assert "helper" not in struct["unresolved_calls"], struct


def test_real_builtins_not_flagged():
    # The builtin set is interpreter-derived, not hand-curated: a curated
    # subset was missing exit/TimeoutError/ConnectionError/memoryview and
    # false-vetoed valid code (xhigh review of the #147 close-out).
    code = (
        "def check():\n"
        "    try:\n"
        "        data = memoryview(b'x')\n"
        "    except TimeoutError:\n"
        "        raise ConnectionError('down')\n"
        "    except PermissionError:\n"
        "        breakpoint()\n"
        "    if not data:\n"
        "        exit(1)\n"
    )
    struct = main.structural_score(set(), code)
    assert struct["ok"]
    assert struct["unresolved_calls"] == [], struct


def test_unresolved_list_capped_by_default_uncapped_on_request():
    # Default (in-pipeline telemetry) caps at 10; max_names=0 returns every
    # name — the proxy gate DIFFS original-vs-edited lists, and a truncated
    # list makes that comparison unsound past 10 unresolved names.
    code = "".join(f"def f{i}():\n    return missing_{i}()\n" for i in range(12))
    capped = main.structural_score(set(), code)
    full = main.structural_score(set(), code, max_names=0)
    assert capped["n_unresolved"] == 12
    assert len(capped["unresolved_calls"]) == 10
    assert len(full["unresolved_calls"]) == 12, full


def test_project_symbol_credits_cross_file_call():
    # A name supplied by project symbols is credited (lenient cross-file).
    code = "def index():\n    return shared_util()\n"
    struct = main.structural_score({"shared_util"}, code)
    assert struct["ok"]
    assert "shared_util" not in struct["unresolved_calls"], struct


def test_local_variable_call_not_flagged():
    # #147 review #4: fn is a local variable, not a top-level def — must NOT flag.
    code = (
        "def run():\n"
        "    fn = build_pipeline()\n"
        "    return fn()\n"
        "def build_pipeline():\n    return lambda: 1\n"
    )
    struct = main.structural_score(set(), code)
    assert struct["ok"]
    assert "fn" not in struct["unresolved_calls"], struct


def test_function_parameter_call_not_flagged():
    code = "def apply(handler, evt):\n    return handler(evt)\n"
    struct = main.structural_score(set(), code)
    assert "handler" not in struct["unresolved_calls"], struct


def test_loop_and_comprehension_target_not_flagged():
    code = (
        "def dispatch(handlers, evt):\n"
        "    for h in handlers:\n"
        "        h(evt)\n"
        "    return [g() for g in handlers]\n"
    )
    struct = main.structural_score(set(), code)
    assert "h" not in struct["unresolved_calls"], struct
    assert "g" not in struct["unresolved_calls"], struct


def test_with_as_target_not_flagged():
    code = "def f():\n    with open('x') as fh:\n        return fh()\n"
    struct = main.structural_score(set(), code)
    assert "fh" not in struct["unresolved_calls"], struct


def test_genuine_nameerror_still_flagged_amid_locals():
    # render_template is neither imported, defined, bound, nor builtin —
    # must still flag even though the file has locals.
    code = (
        "from flask import render_template_string\n"
        "def index():\n"
        "    tmpl = load()\n"
        "    return render_template(tmpl)\n"
        "def load():\n    return 'x'\n"
    )
    struct = main.structural_score(set(), code)
    assert "render_template" in struct["unresolved_calls"], struct
    assert "tmpl" not in struct["unresolved_calls"], struct


# ---------------------------------------------------------------------------
# Endpoint-level coverage: the Go gate consumes /internal/structural_check's
# exact response contract ({"ok", "unresolved"}); a drift there (e.g. the
# unresolved_calls -> "unresolved" rename in the handler) would silently
# fail-open the whole gate while every resolver-level test stays green.
# ---------------------------------------------------------------------------

@pytest.fixture()
def structural_check_url(monkeypatch):
    """Serve the real V3Handler on an ephemeral port, auth disabled."""
    import threading
    from http.server import ThreadingHTTPServer

    monkeypatch.setattr(adapters, "SERVICE_TOKEN", "")
    srv = ThreadingHTTPServer(("127.0.0.1", 0), main.V3Handler)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    try:
        yield f"http://127.0.0.1:{srv.server_address[1]}/internal/structural_check"
    finally:
        srv.shutdown()
        srv.server_close()


def _post_json(url, payload):
    import json as _json
    import urllib.request
    req = urllib.request.Request(
        url, data=_json.dumps(payload).encode(),
        headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as resp:
        return _json.loads(resp.read())


def test_endpoint_flags_unimported_call(structural_check_url):
    # Direction (a) through the real endpoint: composed file calls a name
    # it never imports -> "unresolved" (the key the Go client parses)
    # names it.
    out = _post_json(structural_check_url, {
        "path": "app.py",
        "source": (
            "from flask import Flask, render_template_string\n"
            "app = Flask(__name__)\n"
            "@app.route('/')\n"
            "def index():\n"
            "    return render_template('index.html')\n"
        ),
    })
    assert out["ok"] is True
    assert "render_template" in out["unresolved"], out
    assert out["n_unresolved"] >= 1
    assert "wildcard_imports" in out


def test_endpoint_credits_import_elsewhere_in_file(structural_check_url):
    # Direction (b) through the real endpoint: the import lives at the top
    # of the composed file, the call inside a function body -> clean.
    out = _post_json(structural_check_url, {
        "path": "app.py",
        "source": (
            "from flask import render_template\n"
            "def index():\n"
            "    return render_template('index.html')\n"
        ),
    })
    assert out["ok"] is True
    assert out["unresolved"] == [], out


def test_endpoint_delete_import_direct_call_flagged(structural_check_url):
    # The delete-import edge for direct calls: the post-edit file kept the
    # call but lost the import -> flagged. (Attribute calls like
    # os.getcwd() after deleting `import os` are a documented v1 limit.)
    out = _post_json(structural_check_url, {
        "path": "app.py",
        "source": "def index():\n    return render_template('index.html')\n",
    })
    assert out["ok"] is True
    assert "render_template" in out["unresolved"], out


def test_endpoint_fails_open_without_tree_sitter(structural_check_url,
                                                 monkeypatch):
    # ok:false = "check couldn't run"; the Go caller treats it as pass.
    # (Malformed Python is NOT this case: tree-sitter parses it tolerantly,
    # so the checkable trigger is tree-sitter being unavailable.)
    monkeypatch.setattr(symbols, "_STRUCTURAL_EDIT_AVAILABLE", False)
    out = _post_json(structural_check_url, {
        "path": "app.py",
        "source": "def index():\n    return render_template('x')\n",
    })
    assert out["ok"] is False


def test_endpoint_returns_full_unresolved_list(structural_check_url):
    # The endpoint must NOT cap the list: the Go gate diffs original vs
    # edited names, so a truncated response makes the healthy->broken
    # comparison unsound on files with >10 unresolved calls.
    code = "".join(f"def f{i}():\n    return missing_{i}()\n" for i in range(12))
    out = _post_json(structural_check_url, {"path": "app.py", "source": code})
    assert out["ok"] is True
    assert len(out["unresolved"]) == 12, out


def test_endpoint_project_context_credits_symbol(structural_check_url):
    # project_context symbols are additive leniency through the endpoint.
    out = _post_json(structural_check_url, {
        "path": "app.py",
        "source": "def index():\n    return shared_util()\n",
        "project_context": {"util.py": "def shared_util():\n    return 1\n"},
    })
    assert out["ok"] is True
    assert "shared_util" not in out["unresolved"], out


def test_html_text_is_misread_as_python_calls():
    # Why the pipeline gates the veto on language: the Python grammar parses
    # an HTML template tolerantly and reports `Time (mins)` as a call.
    html = (
        "<table><tr><th> Time (mins) </th><th> Distance (km) </th></tr></table>\n"
        "<style>th { color: rgba(0, 0, 0, 0.5); }</style>\n"
    )
    struct = main.structural_score(set(), html)
    assert struct["ok"]
    assert struct["n_unresolved"] >= 1, struct

"""The V3 service's outline route. A Python file gets its top-level functions and classes, each with its lines and its
callers and callees in the file, and the code in another language that the file holds. A body that is not JSON is
refused."""
import json
import threading
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer

import pytest

import adapters
import main

PYTHON_FILE = (
    "import functools\n"
    "\n"
    "@functools.cache\n"
    "def total(items):\n"
    "    return sum(subtotal(i) for i in items)\n"
    "\n"
    "\n"
    "def subtotal(item):\n"
    "    return item\n"
    "\n"
    "\n"
    "class Cart:\n"
    "    pass\n"
)


@pytest.fixture
def outline(monkeypatch):
    """POST a body to /internal/outline on the real handler, auth off; gives (status, answer)."""
    monkeypatch.setattr(adapters, "SERVICE_TOKEN", "")
    server = ThreadingHTTPServer(("127.0.0.1", 0), main.V3Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()

    def post(body):
        request = urllib.request.Request(f"http://127.0.0.1:{server.server_port}/internal/outline", data=body,
                                         headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(request, timeout=10) as response:
                return response.status, json.loads(response.read())
        except urllib.error.HTTPError as error:
            return error.code, json.loads(error.read())

    yield post
    server.shutdown()
    server.server_close()


def test_a_python_file_lists_its_definitions_with_their_lines_and_calls(outline):
    status, answer = outline(json.dumps({"path": "app.py", "source": PYTHON_FILE}).encode())
    assert status == 200
    # A decorated function starts at its decorator; a definition with no calls either way has neither key.
    assert answer == {"supported": True, "symbols": [
        {"name": "total", "kind": "function", "start_line": 3, "end_line": 5, "calls": ["sum", "subtotal"]},
        {"name": "subtotal", "kind": "function", "start_line": 8, "end_line": 9, "called_by": ["total"]},
        {"name": "Cart", "kind": "class", "start_line": 12, "end_line": 13},
    ]}


def test_the_javascript_a_python_string_holds_is_listed(outline):
    source = 'PAGE = """\n<script>\nfunction draw() {}\n</script>\n"""\n'
    status, answer = outline(json.dumps({"path": "app.py", "source": source}).encode())
    assert status == 200
    assert [(region["kind"], region["symbols"]) for region in answer["embedded_regions"]] == [("javascript", ["draw"])]


@pytest.mark.parametrize("body, supported", [
    pytest.param(json.dumps({"path": "app.js", "source": PYTHON_FILE}).encode(), False, id="a file that is not python"),
    pytest.param(b'{"path": null, "source": null}', False, id="no path and no source"),
    pytest.param(b'{"path": "app.py", "source": null}', True, id="a python file with no source"),
    pytest.param(b"", False, id="an empty body"),
])
def test_a_file_with_no_definitions_to_read_lists_none(outline, body, supported):
    answer = outline(body)
    assert answer == (200, {"symbols": [], "supported": supported})


def test_a_graph_failure_keeps_the_symbol_outline(outline, monkeypatch):
    def unavailable(file_map):
        raise RuntimeError("graph unavailable")

    monkeypatch.setattr("graph.build_graph", unavailable)
    status, answer = outline(json.dumps({"path": "app.py", "source": "def total():\n    pass\n"}).encode())
    assert status == 200
    assert answer == {"supported": True, "symbols": [
        {"name": "total", "kind": "function", "start_line": 1, "end_line": 2},
    ]}


def test_without_tree_sitter_a_python_file_lists_nothing(outline, monkeypatch):
    monkeypatch.setattr(main, "_STRUCTURAL_EDIT_AVAILABLE", False)
    answer = outline(json.dumps({"path": "app.py", "source": PYTHON_FILE}).encode())
    assert answer == (200, {"symbols": [], "supported": False})


def test_a_body_that_is_not_json_is_refused(outline):
    status, answer = outline(b"{not json")
    assert status == 400
    assert answer["symbols"] == []
    assert answer["supported"] is False
    assert answer["error"].startswith("invalid JSON body:")

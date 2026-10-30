"""OpenAPI ↔ registered-routes parity.

Every discrete route the proxy registers (mux.HandleFunc) must appear in
the OpenAPI spec, and vice versa — so the machine-readable API doc can't
silently drift from the code. The `/` catch-all passthrough is excluded
(it's not a discrete endpoint). Parsing only.
"""

import re
from pathlib import Path

import pytest

from tests.contracts import go_source

yaml = pytest.importorskip("yaml")

REPO = Path(__file__).resolve().parents[2]
SPEC = REPO / "docs" / "schemas" / "proxy_openapi.yaml"
MAIN = REPO / "proxy" / "main.go"


def _registered_routes():
    routes = set()
    for m in re.finditer(r'mux\.HandleFunc\("([^"]+)"', MAIN.read_text()):
        path = m.group(1)
        if path == "/":
            continue  # catch-all passthrough, not a discrete endpoint
        routes.add(path)
    return routes


def _documented_paths():
    spec = yaml.safe_load(SPEC.read_text())
    return set(spec["paths"].keys())


def test_every_registered_route_is_documented():
    missing = _registered_routes() - _documented_paths()
    assert not missing, f"routes registered but absent from OpenAPI: {missing}"


def test_no_phantom_documented_paths():
    extra = _documented_paths() - _registered_routes()
    assert not extra, f"OpenAPI documents non-existent routes: {extra}"


def test_spec_version_matches_go_constant():
    spec = yaml.safe_load(SPEC.read_text())
    go = go_source("proxy", "const APIVersion")
    m = re.search(r'const APIVersion = "([\d.]+)"', go)
    assert m, "OpenAPI info.version must match the Go APIVersion constant"
    assert spec["info"]["version"] == m.group(1), "OpenAPI info.version must match the Go APIVersion constant"


def test_error_response_references_envelope_schema():
    text = SPEC.read_text()
    assert "error_envelope.schema.json" in text


# --- OpenAPI ↔ docs/API.md prose parity -----------------------------------

API_MD = REPO / "docs" / "API.md"
HEADING_PATH_RE = re.compile(r"^###\s+(?:GET|POST|PUT|PATCH|DELETE)\s+(/\S+)(.*)$")


def _proxy_section() -> str:
    """The atlas-proxy section of API.md: its `## ` heading (located by
    text, tolerant of restructuring) up to the next `## ` heading."""
    lines = API_MD.read_text().splitlines()
    start = next((i for i, l in enumerate(lines)
                  if l.startswith("## ") and "atlas-proxy" in l.lower()), None)
    assert start is not None, "docs/API.md has no '## ... atlas-proxy ...' section"
    end = next((i for i in range(start + 1, len(lines))
                if lines[i].startswith("## ")), len(lines))
    return "\n".join(lines[start:end])


def test_every_spec_path_is_documented_in_api_md():
    section = _proxy_section()
    headed = {m.group(1) for line in section.splitlines()
              if (m := HEADING_PATH_RE.match(line))}
    missing = {p for p in _documented_paths()
               if p not in headed and f"`{p}`" not in section}
    assert not missing, (
        f"OpenAPI paths absent from docs/API.md atlas-proxy section "
        f"(need a '### METHOD {sorted(missing)[0]}' heading or `path` "
        f"mention): {sorted(missing)}")


def test_every_api_md_proxy_heading_is_in_spec():
    phantoms = []
    for line in _proxy_section().splitlines():
        m = HEADING_PATH_RE.match(line)
        if not m:
            continue
        if "passthrough" in m.group(2).lower():
            continue  # documented `/` catch-all, excluded from the spec
        if m.group(1) not in _documented_paths():
            phantoms.append(line.strip())
    assert not phantoms, (
        f"docs/API.md atlas-proxy section documents endpoints missing "
        f"from the OpenAPI spec: {phantoms}")

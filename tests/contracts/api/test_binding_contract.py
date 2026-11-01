"""Local-only binding contract.

ATLAS is a local single-user tool (.github/SECURITY.md, ADR 0001): every port a
compose file publishes to the host must bind loopback, so a laptop on
hotel wifi never silently exposes an internal service. Configuration
parsing only — no sockets are opened.

Covered surfaces:
  - docker-compose.yml + every overlay: `ports:` entries must be
    127.0.0.1-prefixed (host side).
  - K3s templates: no hostNetwork, no LoadBalancer services. NodePort
    is the documented K3s access mode (cluster-scoped by design and
    called out in SETUP.md), so it is allowed but each use must carry
    a nodePort field intentionally, not accidentally.
  - Native macOS launcher: llama binds ATLAS_LLAMA_HOST with a
    127.0.0.1 default (overriding to 0.0.0.0 is the explicit,
    documented remote escape hatch).
"""

import re
from pathlib import Path

REPO = Path(__file__).resolve().parents[3]

COMPOSE_FILES = sorted([*REPO.glob("docker-compose*.yml"), *REPO.glob("compose/*.yml")])
TEMPLATE_FILES = sorted((REPO / "templates").glob("*.yaml.tmpl"))

# host-side publish forms docker accepts; we require an explicit
# loopback IP prefix.
_PORT_LINE = re.compile(r'^\s*-\s*"(?P<spec>[^"]+)"\s*(#.*)?$')


def _publish_lines(path):
    """(lineno, spec) for entries inside a ports: block."""
    out = []
    in_ports = False
    ports_indent = 0
    for i, line in enumerate(path.read_text().splitlines(), 1):
        stripped = line.strip()
        if stripped.startswith("#"):
            continue
        indent = len(line) - len(line.lstrip())
        if stripped == "ports:":
            in_ports = True
            ports_indent = indent
            continue
        if in_ports:
            if stripped and indent <= ports_indent:
                in_ports = False
                continue
            m = _PORT_LINE.match(line)
            if m:
                out.append((i, m.group("spec")))
    return out


def test_compose_files_exist():
    assert COMPOSE_FILES, "no compose files found"
    assert any(p.name == "docker-compose.yml" for p in COMPOSE_FILES)
    assert {f"docker-compose.{name}.yml" for name in ("cpu", "macos", "rocm", "vulkan")} <= {p.name for p in COMPOSE_FILES}


def test_all_compose_publishes_bind_loopback():
    offenders = []
    for path in COMPOSE_FILES:
        for lineno, spec in _publish_lines(path):
            if not spec.startswith(("127.0.0.1:", "::1:", "[::1]:")):
                offenders.append(f"{path.name}:{lineno}: {spec}")
    assert not offenders, (
        "host port publishes must bind loopback (127.0.0.1:host:container) "
        "so internal services are never reachable off-machine by default:\n  "
        + "\n  ".join(offenders))


def test_k3s_templates_no_host_network_or_loadbalancer():
    assert TEMPLATE_FILES, "no K3s templates found"
    for path in TEMPLATE_FILES:
        text = path.read_text()
        assert "hostNetwork" not in text, (
            f"{path.name}: hostNetwork bypasses the cluster network "
            f"boundary")
        assert "LoadBalancer" not in text, (
            f"{path.name}: LoadBalancer provisions an externally-"
            f"reachable endpoint; K3s access mode is NodePort (SETUP.md)")


def test_k3s_nodeports_are_explicit():
    for path in TEMPLATE_FILES:
        text = path.read_text()
        if "type: NodePort" in text:
            assert "nodePort:" in text, (
                f"{path.name}: NodePort service without an explicit "
                f"nodePort gets a random port — pin it")


def test_macos_launcher_defaults_to_loopback():
    launcher = REPO / "scripts" / "atlas-llama-macos.sh"
    text = launcher.read_text()
    m = re.search(r'\$\{ATLAS_LLAMA_HOST:-(?P<default>[^}"]+)\}', text)
    assert m, "launcher no longer reads ATLAS_LLAMA_HOST — update this test"
    assert m.group("default") == "127.0.0.1", (
        f"native llama default host is {m.group('default')!r}; the "
        f"loopback default is the documented posture (.env.example)")


def test_env_example_documents_loopback_default():
    text = (REPO / ".env.example").read_text()
    assert "ATLAS_LLAMA_HOST" in text
    assert "127.0.0.1" in text

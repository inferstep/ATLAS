"""Pointing ATLAS at a host-native llama-server must work on Linux too.

GH #146: the documented recipe uses `host.docker.internal`, which is a Docker
Desktop convenience and does not resolve on stock Linux Docker. Users had to
reverse-engineer the docker0 gateway (172.17.0.1) themselves. Mapping the same
hostname via host-gateway makes one override serve every platform.
"""
from pathlib import Path

import pytest

yaml = pytest.importorskip("yaml")

REPO = Path(__file__).resolve().parents[3]
COMPOSE = REPO / "docker-compose.yml"

# Services that talk to llama-server and therefore need the hostname to
# resolve when the bundled container is swapped for a host-native server.
CONSUMERS = {"geometric-lens", "v3-service", "sandbox", "atlas-proxy"}


def _services():
    return yaml.safe_load(COMPOSE.read_text())["services"]


def test_llama_consumers_can_resolve_the_host_gateway():
    services = _services()
    missing = []
    for name in sorted(CONSUMERS):
        hosts = services.get(name, {}).get("extra_hosts") or []
        if not any("host.docker.internal:host-gateway" in h for h in hosts):
            missing.append(name)
    assert not missing, (
        f"{missing} cannot resolve host.docker.internal on Linux, so a "
        f"host-native llama-server override fails there (GH #146)")


def test_the_bundled_server_itself_is_not_given_the_mapping():
    """llama-server is the service being REPLACED in that setup — a
    host-gateway entry on it would be noise, and hints at a copy-paste."""
    hosts = _services().get("llama-server", {}).get("extra_hosts") or []
    assert not any("host-gateway" in h for h in hosts)


def test_every_consumer_named_here_actually_exists():
    """Guards the guard: a service rename must fail loudly here rather than
    silently shrinking what the first test covers."""
    services = set(_services())
    assert CONSUMERS <= services, f"unknown services listed: {CONSUMERS - services}"

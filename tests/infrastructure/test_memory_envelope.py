"""The deployment's memory maxima have to fit the machine.

A per-command ceiling stops one runaway command. It does not stop a deployment
whose configured maxima exceed the host: the sandbox was allowed eleven
gigabytes on a fifteen-gigabyte host where the inference server held nine with
no limit at all, so every process was inside its own bounds at the moment the
kernel went looking for something to kill.

These read docker-compose.yml as written. Nothing is applied and no container
is started.
"""

from __future__ import annotations

import os
import re

import pytest

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
COMPOSE = os.path.join(ROOT, "docker-compose.yml")

SUFFIX = {"k": 1 << 10, "m": 1 << 20, "g": 1 << 30}


def as_bytes(raw: str) -> int:
    raw = raw.strip().lower().rstrip("b")
    if not raw:
        return 0
    if raw[-1] in SUFFIX:
        return int(raw[:-1]) * SUFFIX[raw[-1]]
    return int(raw)


@pytest.fixture(scope="module")
def compose_text():
    with open(COMPOSE) as fh:
        return fh.read()


@pytest.fixture(scope="module")
def declared(compose_text):
    """Every mem_limit default, keyed by the variable that sets it."""
    out = {}
    for var, default in re.findall(r"mem_limit:\s*\$\{([A-Z0-9_]+):-([0-9a-zA-Z]+)\}",
                                   compose_text):
        out[var] = as_bytes(default)
    return out


def test_every_service_declares_a_memory_budget(declared):
    for var in ("ATLAS_LENS_MEM", "ATLAS_V3_MEM", "ATLAS_PROXY_MEM",
                "ATLAS_SANDBOX_MEM"):
        assert var in declared, f"{var} has no mem_limit default"
        assert declared[var] > 0
    # The inference service is the exception, deliberately: see below.
    assert declared.get("ATLAS_LLAMA_MEM") == 0


def test_the_declared_maxima_fit_the_host_they_are_written_for(compose_text, declared):
    """16 GiB, the host these defaults are sized for, with the reserve."""
    host = 15731 * (1 << 20)          # what `free` reports on a 16 GiB machine
    reserve = 1610612736              # the compose default for the host reserve
    assert f"ATLAS_HOST_RESERVE_BYTES:-{reserve}" in compose_text
    total = reserve + sum(declared.values())  # the unset inference term is 0
    assert total <= host, (
        f"the declared maxima total {total / (1 << 30):.2f} GiB on a "
        f"{host / (1 << 30):.2f} GiB host")
    # Explicit headroom, not a rounding accident.
    assert host - total >= 256 * (1 << 20)


def test_one_command_cannot_consume_the_sandbox(compose_text, declared):
    m = re.search(r"ATLAS_EXEC_MEMORY_BYTES:-(\d+)", compose_text)
    assert m, "no per-command memory ceiling default"
    per_command = int(m.group(1))
    sandbox = declared["ATLAS_SANDBOX_MEM"]
    assert per_command < sandbox, (
        "one command may take the whole container the executor lives in")
    # Room for the executor process and the output it buffers.
    assert sandbox - per_command >= 256 * (1 << 20)


def test_the_inference_limit_ships_unset_pending_a_canary(compose_text, declared):
    """A limit below the peak is a certain kill, not reclaim.

    The obvious fix for the OOM victim was a cgroup limit, and the first draft
    of the compose file shipped one at 9.5 GiB. Measuring the deployed server
    showed that is below what it already uses: process peak RSS 10.31 GiB, an
    anonymous working set of 8.81 GiB once its swapped pages are counted back,
    and 16 MiB of reclaimable page cache to give back under pressure. So it
    stays unenforced until a real-model canary establishes a value with
    measured headroom, and the accounting term carries the expectation.
    """
    assert declared["ATLAS_LLAMA_MEM"] == 0, (
        "an inference limit is enforced without canary evidence")
    m = re.search(r"ATLAS_LLAMA_BUDGET_BYTES:-(\d+)", compose_text)
    assert m, "the envelope has no accounting term for the largest component"
    # Above the measured peak resident set, not below it.
    assert int(m.group(1)) >= 10.31 * (1 << 30)
    llama = compose_text[compose_text.index("  llama-server:"):
                         compose_text.index("  geometric-lens:")]
    assert "canary" in llama, "the compose file does not say why it is unset"


def test_the_sandbox_may_not_swap(compose_text):
    """A swapping test is a hung one, and swap hides an over-commit."""
    assert re.search(r"memswap_limit:\s*\$\{ATLAS_SANDBOX_MEM", compose_text)


def test_the_proxy_is_told_the_whole_envelope(compose_text):
    proxy = compose_text[compose_text.index("  atlas-proxy:"):]
    for var in ("ATLAS_HOST_MEMORY_BYTES", "ATLAS_HOST_RESERVE_BYTES",
                "ATLAS_LLAMA_MEM", "ATLAS_LLAMA_BUDGET_BYTES",
                "ATLAS_LENS_MEM", "ATLAS_V3_MEM",
                "ATLAS_PROXY_MEM", "ATLAS_SANDBOX_MEM",
                "ATLAS_EXEC_MEMORY_BYTES", "ATLAS_EXEC_CONCURRENCY"):
        assert var in proxy, f"the proxy cannot check the envelope without {var}"


def test_the_executor_is_told_the_per_command_contract(compose_text):
    sandbox = compose_text[compose_text.index("  sandbox:"):
                           compose_text.index("  atlas-proxy:")]
    for var in ("ATLAS_EXEC_MEMORY_BYTES", "ATLAS_EXEC_MAX_PROCESSES",
                "ATLAS_EXEC_OUTPUT_BYTES", "MAX_EXECUTION_TIME"):
        assert var in sandbox, f"the executor is not given {var}"


def test_the_assumptions_are_written_down(compose_text):
    sandbox = compose_text[compose_text.index("  sandbox:"):
                           compose_text.index("  atlas-proxy:")]
    for phrase in ("MEMORY ENVELOPE", "12B Q4", "ATLAS_HOST_MEMORY_BYTES"):
        assert phrase in sandbox, f"the envelope does not state {phrase!r}"


# --- verification builds may not claim a deployable tag ----------------------


DEPLOYABLE_TAGS = ("ghcr.io/inferstep/atlas-proxy:dev",
                   "ghcr.io/inferstep/atlas-sandbox:dev",
                   "ghcr.io/inferstep/atlas-v3:dev")


def test_compose_build_writes_the_deployable_tag(compose_text):
    """A `docker compose build` claims the tag the running stack was started from.

    This is not a defect in compose; it is a fact an operator has to know. A
    verification build during development retagged `atlas-proxy:dev` while a
    container was still running the previous image, and that image had since
    been pruned — so the tag could not be put back, and the deployable name now
    points at something the running container is not. Verification builds
    belong under throwaway tags: `docker build -t atlas-proxy:<slice>-check`.
    """
    assert "image: ghcr.io/${ATLAS_GHCR_OWNER:-inferstep}/atlas-proxy:" in compose_text
    # The tag is a variable, so an operator CAN point a build somewhere else.
    assert "${ATLAS_IMAGE_TAG:-" in compose_text


def test_the_runbook_says_how_to_build_without_claiming_it():
    ops = os.path.join(ROOT, "docs", "OPERATIONS.md")
    with open(ops) as fh:
        text = fh.read()
    assert "Building for verification" in text, (
        "OPERATIONS.md does not tell an operator how to build for verification "
        "without overwriting the deployable tag")
    assert "ATLAS_IMAGE_TAG" in text

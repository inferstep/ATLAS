"""Status-dimension parity contract.

The canonical seven-dimension lens/ASA status is defined once in the Go
producer (proxy/calibration_status.go, buildDimensions) and rendered by
the TUI badge, atlas doctor, and documented in SUPPORT_MATRIX. This
contract asserts the dimension NAMES agree across those surfaces so a
rename in one place can't silently desync the others.

Parsing only — no services are started.
"""

import re
from pathlib import Path

from tests.contracts import go_source

REPO = Path(__file__).resolve().parents[3]

CANONICAL = [
    "model_runtime",
    "direct_agent",
    "lens_identity",
    "lens_scoring",
    "lens_calibration",
    "lens_intervention",
    "asa",
]


def test_go_producer_emits_the_seven_dimensions():
    src = go_source("proxy", "return []StatusDimension{")
    # Extract the ordered list literal returned by buildDimensions.
    block = src[src.index("return []StatusDimension{"):]
    names = re.findall(r'\{"([a-z_]+)",', block)
    # first seven quoted keys after the return are the dimension names
    assert names[:7] == CANONICAL, (
        f"Go producer dimensions {names[:7]} != canonical {CANONICAL}")


def test_support_matrix_documents_the_seven_dimensions():
    text = (REPO / "docs" / "SUPPORT_MATRIX.md").read_text()
    assert "Reference-model status dimensions" in text, (
        "SUPPORT_MATRIX lost the status-dimensions section")
    for name in CANONICAL:
        assert f"`{name}`" in text, (
            f"SUPPORT_MATRIX status table is missing `{name}`")


def test_intervention_neutrality_is_documented():
    text = (REPO / "docs" / "SUPPORT_MATRIX.md").read_text()
    # The load-bearing correctness claim must be stated.
    assert re.search(r"intervention stays neutral or disabled",
                     text, re.I), (
        "SUPPORT_MATRIX must state the intervention-neutral-when-"
        "uncalibrated guarantee")


def test_doctor_reads_the_endpoint_not_a_private_copy():
    # doctor must render dimensions from the shared endpoint (so it
    # agrees with the TUI) rather than recomputing them.
    src = (REPO / "atlas" / "commands" / "doctor.py").read_text()
    assert "/v1/calibration/status" in src
    assert "check_status_dimensions" in src

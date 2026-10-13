"""The V3 service plans against the cap its caller applies.

The proxy cuts each /v3/generate call to at most half of the session's
remaining time and sends that cap as budget_ms. The handler reads it and
hands it to the pipeline; anything that is not a positive number is not a
cap, and the run keeps its ATLAS_V3_TIMEOUT behaviour.
"""

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

import main


@pytest.mark.parametrize("value, want", [
    (40_000, 40_000.0),
    (1500.5, 1500.5),
    (None, None),
    (0, None),
    (-5, None),
    (True, None),
    ("40000", None),
    (float("nan"), None),
    (float("inf"), None),
])
def test_only_a_positive_number_is_a_cap(value, want):
    assert main._positive_budget_ms(value) == want


def test_the_handler_hands_the_cap_to_the_run():
    src = Path(main.__file__).read_text()
    handler = src[src.index("def _handle_generate"):]
    handler = handler[:handler.index("\n    def ", 1)]
    assert 'budget_ms = _positive_budget_ms(body.get("budget_ms"))' in handler
    run_call = handler[handler.index("pipeline.run("):]
    run_call = run_call[:run_call.index("\n            )")]
    assert "budget_ms=budget_ms" in run_call

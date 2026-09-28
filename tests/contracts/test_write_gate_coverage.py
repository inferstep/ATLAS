"""Every write path must run the same gates.

The gates are only as good as their weakest caller. `edit_file` — the most
used edit tool — ran the syntax check and the unresolved-call check but never
`embeddedScriptGate`, so the two comparative findings (a render loop that
stopped repeating, a lexical binding declared twice) were never evaluated on
it. Run 9 was caught only because that run happened to reach for
`replace_lines`; run 10 made the identical dead-loop edit through `edit_file`
and it landed, with the page still returning 200.

A check wired into four of five write paths reads as covered and is not. This
asserts the wiring directly, because nothing else does: each tool builds its
own chain and Go cannot tell you one is missing a call.
"""
import re
from pathlib import Path

import pytest

TOOLS_GO = Path(__file__).resolve().parents[2] / "proxy" / "tools.go"

# Gates every write path must run, and why skipping one is not cosmetic.
REQUIRED = {
    "editIntroducesUnresolved": "a would-be NameError reaches disk",
    "embeddedScriptGate": "broken or dead JavaScript inside a template reaches disk",
    "duplicateMainGuard": "a second module entrypoint reaches disk",
}

# Tools that write to a file the user can see. structural_edit does its own
# post-splice compile() in v3-service instead of checkFallbackSyntax, which is
# why syntax is not in REQUIRED — the others all call it, and the contract
# below would be asserting an implementation detail rather than a property.
WRITE_TOOLS = ["writeFile", "editFile", "structuralEdit", "insertAfter", "replaceLines"]


def _tool_bodies() -> dict:
    src = TOOLS_GO.read_text().splitlines()
    starts = [(i, m.group(1)) for i, line in enumerate(src)
              if (m := re.match(r"func ([a-zA-Z]+)Tool\(\) \*ToolDef", line))]
    assert starts, "no tool constructors found — did tools.go get restructured?"
    bounds = starts + [(len(src), "")]
    return {name: "\n".join(src[a:b])
            for (a, name), (b, _) in zip(bounds, bounds[1:]) if name}


@pytest.fixture(scope="module")
def bodies():
    return _tool_bodies()


def test_the_write_tools_this_contract_names_all_exist(bodies):
    """Guards the guard: a rename should fail here, pointing at the rename."""
    missing = [t for t in WRITE_TOOLS if t not in bodies]
    assert not missing, f"tools.go has no constructor for {missing}; found {sorted(bodies)}"


# ATLAS is a pipeline with a harness around it, not a harness. A content edit
# that skips tier classification and V3 produces a single greedy sample with no
# candidate generation and no lens scoring — which is exactly what the tier
# system exists to prevent. `insert_after` and `replace_lines` shipped without
# it while the tool guidance was being changed to steer toward them, so the
# model was migrated off the quality pipeline onto the tools that lacked it.
PIPELINE_ENTRYPOINTS = ("runEditPipeline", "writeFileWithV3", "improveContentWithV3")


@pytest.mark.parametrize("tool", WRITE_TOOLS)
def test_every_write_path_goes_through_the_pipeline(bodies, tool):
    assert any(e in bodies[tool] for e in PIPELINE_ENTRYPOINTS), (
        f"{tool}Tool never enters the V3 pipeline. Its edits get one greedy "
        f"sample, no candidates and no lens scoring, regardless of file tier."
    )


@pytest.mark.parametrize("tool", WRITE_TOOLS)
@pytest.mark.parametrize("gate,consequence", sorted(REQUIRED.items()))
def test_every_write_path_runs_every_gate(bodies, tool, gate, consequence):
    assert gate in bodies[tool], (
        f"{tool}Tool does not call {gate} — {consequence}. "
        f"A gate wired into some write paths and not others reads as covered "
        f"and is not."
    )

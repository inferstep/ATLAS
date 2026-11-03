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

PROXY = Path(__file__).resolve().parents[3] / "services" / "proxy"
TOOL_FILES = ("tool_write_v3.go", "tool_write.go", "tool_edit.go", "tool_structural_edit.go")

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


def _tool_source() -> str:
    """The tool files without their comment lines: a name in a comment calls nothing."""
    lines = "\n".join((PROXY / name).read_text() for name in TOOL_FILES).splitlines()
    return "\n".join(line for line in lines if not line.lstrip().startswith("//"))


def _function(src: str, name: str) -> str:
    """One function of the tool files, from its `func` line to its closing brace."""
    return src[src.index(f"func {name}("):].split("\n}\n", 1)[0]


def _reach(src: str, start: str) -> str:
    """`start` and every function of the tool files that it names, and that those name in turn.

    A gate counts for a tool only when the tool's own path names it: a gate in
    a function that the tool no longer calls is not in its reach.
    """
    defined = set(re.findall(r"^func (\w+)\(", src, re.M))
    seen, todo = [], [start]
    while todo:
        name = todo.pop()
        if name not in seen:
            seen.append(name)
            todo += [n for n in re.findall(r"\w+", _function(src, name)) if n in defined]
    return "\n".join(_function(src, name) for name in seen)


def _tool_bodies() -> dict:
    src = _tool_source()
    tools = re.findall(r"^func ([a-zA-Z]+)Tool\(\) \*ToolDef", src, re.M)
    assert tools, "no tool constructors found — did the tool files get restructured?"
    return {name: _reach(src, f"{name}Tool") for name in tools}


@pytest.fixture(scope="module")
def bodies():
    return _tool_bodies()


def test_the_write_tools_this_contract_names_all_exist(bodies):
    """Guards the guard: a rename should fail here, pointing at the rename."""
    missing = [t for t in WRITE_TOOLS if t not in bodies]
    assert not missing, f"the tool files have no constructor for {missing}; found {sorted(bodies)}"


# ATLAS is a pipeline with a harness around it, not a harness. A content edit
# that skips tier classification and V3 produces a single greedy sample with no
# candidate generation and no lens scoring — which is exactly what the tier
# system exists to prevent. `insert_after` and `replace_lines` shipped without
# it while the tool guidance was being changed to steer toward them, so the
# model was migrated off the quality pipeline onto the tools that lacked it.
PIPELINE_ENTRYPOINTS = ("runEditPipeline", "writeFileWithV3", "improveContentWithV3", "deliverEditCandidate")


@pytest.mark.parametrize("tool", WRITE_TOOLS)
def test_every_write_path_goes_through_the_pipeline(bodies, tool):
    assert any(e in bodies[tool] for e in PIPELINE_ENTRYPOINTS), (
        f"{tool}Tool never enters the V3 pipeline. Its edits get one greedy "
        f"sample, no candidates and no lens scoring, regardless of file tier."
    )


def _steps_of_the_write_route() -> str:
    """The bodies of the gates that applyWriteGates runs. A gate's name anywhere else in the tool runs nothing."""
    src = _tool_source()
    listed = re.search(r"func applyWriteGates\(.*?\[\]func\(\*v3Write\) \*wire\.ToolResult\{(.*?)\n\t\} \{", src, re.S)
    assert listed, "applyWriteGates has no list of gates — did the write route get restructured?"
    names = re.findall(r"\w+", listed.group(1))
    assert names, "applyWriteGates lists no gate"
    return "\n".join(_function(src, name) for name in names)


@pytest.mark.parametrize("gate,consequence", sorted(REQUIRED.items()))
def test_the_write_route_runs_every_gate_from_its_list(gate, consequence):
    assert gate in _steps_of_the_write_route(), (
        f"no gate on applyWriteGates's list calls {gate} — {consequence}."
    )


@pytest.mark.parametrize("tool", WRITE_TOOLS)
@pytest.mark.parametrize("gate,consequence", sorted(REQUIRED.items()))
def test_every_write_path_runs_every_gate(bodies, tool, gate, consequence):
    assert gate in bodies[tool], (
        f"{tool}Tool does not call {gate} — {consequence}. "
        f"A gate wired into some write paths and not others reads as covered "
        f"and is not."
    )

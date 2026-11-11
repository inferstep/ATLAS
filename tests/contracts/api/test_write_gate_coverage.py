"""Every write path must run the same gates.

The gates are only as good as their weakest caller. `edit_file` — the most
used edit tool — ran the syntax check and the unresolved-call check but never
`embeddedScriptGate`, so the two comparative findings (a render loop that
stopped repeating, a lexical binding declared twice) were never evaluated on
it. Run 9 was caught only because that run happened to reach for
`replace_lines`; run 10 made the identical dead-loop edit through `edit_file`
and it landed, with the page still returning 200.

A check wired into four of five write paths reads as covered and is not.
Each write route must reach every required gate through its function calls and check phases.
"""
import re
from pathlib import Path

import pytest

PROXY = Path(__file__).resolve().parents[3] / "services" / "proxy" / "internal" / "app"
TOOL_FILES = (
    "tool_write_v3.go", "tool_write.go", "tool_edit.go", "tool_structural_edit.go",
    "tool_args.go", "tool_read.go", "loop.go",
)

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

# Each way a write tool lands bytes, named by the function that takes it. Each
# way must run every gate itself: a gate on another way of the same tool does
# not run on this one. writeFile's V3 route is checked through applyWriteGates
# and its check phase, below.
LANDING_PATHS = {
    "writeFile": ("writeFileDirectRoute", "writeAfterV3Failure"),
    "editFile": ("executeEditFile",),
    "insertAfter": ("insertLinesAfter",),
    "replaceLines": ("replaceLineRange",),
    "structuralEdit": ("executeStructuralEdit",),
}
LANDINGS = [(tool, path) for tool, paths in LANDING_PATHS.items() for path in paths]
# The function that starts writeFile's V3 route; applyWriteGates runs its gates.
V3_ROUTE = "writeFileWithV3"
# The one way that lands bytes without the gates, on purpose, and why
# (tool_write.go, writeFileThroughV3): "a NEW file lands with a warning so the
# model can run it and read the real traceback." The direct route lands such
# content the same way, before its gates.
UNGATED_WAY = "writeNewFileWithWarning"
# What a function of the tool files calls to change the disk.
DISK_WRITES = ("os.WriteFile(", "os.Rename(", "os.Create(", "os.OpenFile(")


def _tool_source() -> str:
    """The tool files without their comment lines: a name in a comment calls nothing."""
    lines = "\n".join((PROXY / name).read_text() for name in TOOL_FILES).splitlines()
    return "\n".join(line for line in lines if not line.lstrip().startswith("//"))


def _function(src: str, name: str) -> str:
    """One function, typed adapter, or check phase, up to its closing brace."""
    end = "\n}\n"
    if name.startswith("proxyChecks."):
        pattern = rf"^\t{re.escape(name)} = \[\]orderedCheck\[\w+\]\{{"
        end = "\n\t}\n"
    elif "." in name:
        receiver, method = name.split(".")
        pattern = rf"^func \(\w+ \*{receiver}\) {method}\("
    else:
        pattern = rf"^func {re.escape(name)}\("
    found = re.search(pattern, src, re.M)
    assert found, f"the tool files have no function or phase {name} — was it renamed?"
    return src[found.start():].split(end, 1)[0]


def _reached(src: str, start: str, stops=frozenset()) -> list:
    """Follow function calls, executed check phases, and their typed adapters. Do not enter names in `stops`."""
    defined = set(re.findall(r"^func (\w+)\(", src, re.M))
    seen, todo = [], [start]
    while todo:
        name = todo.pop()
        if name not in seen and name not in stops:
            seen.append(name)
            body = _function(src, name)
            todo += [n for n in re.findall(r"\b(\w+)\(", body) if n in defined]
            todo += re.findall(r"\bExecute:\s+(\w+),", body)
            todo += re.findall(r"runCheckPhase\((proxyChecks\.\w+),", body)
            todo += [f"{receiver}.{method}" for receiver, method in re.findall(r"\(\*(\w+)\)\.(\w+)", body)]
    return seen


def _reach(src: str, start: str) -> str:
    """The text of `start` and of every function it reaches (see _reached).

    A gate in a function that `start` no longer calls is not in its reach.
    """
    return "\n".join(_function(src, name) for name in _reached(src, start))


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


@pytest.mark.parametrize("gate,consequence", sorted(REQUIRED.items()))
def test_the_write_route_runs_every_gate_from_its_list(gate, consequence):
    assert gate in _reach(_tool_source(), "applyWriteGates"), (
        f"no gate on applyWriteGates's list calls {gate} — {consequence}."
    )


@pytest.mark.parametrize("start,original,replacement", [
    pytest.param("applyWriteGates", "refuseBrokenEmbeddedScript(input.write)", "nil", id="inline-adapter"),
    pytest.param("writeFileDirectRoute", "(*directLandingCheckInput).embedded",
                 "(*directLandingCheckInput).entrypoint", id="typed-adapter"),
    pytest.param("writeFileDirectRoute", "runCheckPhase(proxyChecks.directLanding, &check)",
                 "skippedPhase(&check)", id="disconnected-phase"),
])
def test_a_disconnected_gate_is_not_counted_on_its_write_path(start, original, replacement):
    src = _tool_source()
    assert src.count(original) == 1, f"the fault must replace one occurrence of {original}"
    assert "embeddedScriptGate" in _reach(src, start)

    disconnected = src.replace(original, replacement)

    assert "embeddedScriptGate" in _reach(disconnected, "executeEditFile"), "the edit path still runs the gate"
    assert "embeddedScriptGate" not in _reach(disconnected, start)


def test_every_write_tool_names_the_ways_it_lands_bytes():
    assert sorted(LANDING_PATHS) == sorted(WRITE_TOOLS)


@pytest.mark.parametrize("tool", WRITE_TOOLS)
def test_no_write_tool_lands_bytes_outside_its_named_ways(tool):
    src = _tool_source()
    named = {path for paths in LANDING_PATHS.values() for path in paths} | {V3_ROUTE, UNGATED_WAY}
    outside = sorted(name for name in _reached(src, f"{tool}Tool", stops=named)
                     if any(write in _function(src, name) for write in DISK_WRITES))
    assert not outside, (
        f"{tool}Tool reaches {outside}, which change the disk, on a way that LANDING_PATHS does not name. "
        f"Name the way, so its gates are checked, or make it run them."
    )


@pytest.mark.parametrize("marker,fault", [
    pytest.param("func (check *writeEarlyCheckInput) sibling() checkResult {",
                 '\n\tos.WriteFile("unexpected.txt", nil, 0600)', id="typed-adapter"),
    pytest.param("\tproxyChecks.writeEarly = []orderedCheck[writeEarlyCheckInput]{",
                 '\n\t\t{"writeEarly.unexpectedWrite", func(check *writeEarlyCheckInput) checkResult {\n'
                 '\t\t\tos.WriteFile("unexpected.txt", nil, 0600)\n'
                 '\t\t\treturn checkResult{checkPass, turnContinues}\n\t\t}},', id="inline-adapter"),
])
def test_the_disk_write_check_catches_writes_inside_adapters(monkeypatch, marker, fault):
    src = _tool_source()
    assert src.count(marker) == 1, f"the fault must change one occurrence of {marker}"
    changed = src.replace(marker, marker + fault)
    monkeypatch.setattr(f"{__name__}._tool_source", lambda: changed)

    with pytest.raises(AssertionError, match="which change the disk"):
        test_no_write_tool_lands_bytes_outside_its_named_ways("writeFile")


@pytest.mark.parametrize("tool, path", LANDINGS)
def test_each_way_to_land_bytes_is_on_its_tools_path(bodies, tool, path):
    assert f"func {path}(" in bodies[tool], f"{tool}Tool no longer reaches {path}"


@pytest.mark.parametrize("tool, path", LANDINGS)
@pytest.mark.parametrize("gate,consequence", sorted(REQUIRED.items()))
def test_every_write_path_runs_every_gate(tool, path, gate, consequence):
    assert gate in _reach(_tool_source(), path), (
        f"{tool}Tool lands bytes through {path}, which does not call {gate} — {consequence}. "
        f"A gate wired into some write paths and not others reads as covered "
        f"and is not."
    )

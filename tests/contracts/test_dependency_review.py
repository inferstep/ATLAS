"""The dependency review judges a new dependency's licence against one list, and a licence with no name fails.

The list is in .github/dependency-review-config.yml. The review action reads
it; a step after the action (scripts/licence_names.py) fails for a new
dependency whose licence GitHub cannot name.
"""
import importlib.util
import json
import os
import re
import subprocess
import sys
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[2]
SETTINGS = ROOT / ".github" / "dependency-review-config.yml"
SCRIPT = ROOT / "scripts" / "licence_names.py"
_spec = importlib.util.spec_from_file_location("licence_names", SCRIPT)
names = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(names)


def test_the_list_holds_single_licences_once_each_and_the_level_of_a_vulnerability_that_fails():
    found = yaml.safe_load(SETTINGS.read_text(encoding="utf-8"))
    allowed = found["allow-licenses"]
    assert found["fail-on-severity"] == "high"
    assert len(allowed) == len(set(allowed))
    assert all(isinstance(entry, str) and entry.strip() == entry for entry in allowed)
    joined = [entry for entry in allowed if " AND " in entry or " OR " in entry]
    assert not joined, (
        f"{joined} in `allow-licenses` join two licences. The review action leaves such an entry out without a word. "
        "Fix: give each licence an entry of its own.")


def test_no_entry_of_the_list_lets_gpl_2_only_through():
    allowed = yaml.safe_load(SETTINGS.read_text(encoding="utf-8"))["allow-licenses"]
    wrong = [entry for entry in allowed if re.match(r"(?:A|L)?GPL-(?:1|2)\.0", entry)]
    assert not wrong, (
        f"{wrong} in `allow-licenses`: the review reads an older GPL entry as a range that GPL-2.0-only is in, and a "
        "work under AGPL-3.0 cannot take GPL-2.0-only in. Fix: take the entry out. A dependency under "
        "GPL-2.0-or-later still passes, by the GPL-3.0 entries.")
    assert {"GPL-3.0-only", "GPL-3.0-or-later", "AGPL-3.0-only", "AGPL-3.0-or-later", "MIT", "Apache-2.0"} <= set(allowed)


def test_each_package_that_was_read_by_hand_has_its_licence_beside_it_and_every_part_of_it_is_allowed():
    text = SETTINGS.read_text(encoding="utf-8")
    found = yaml.safe_load(text)
    entries = re.findall(r"^  - (pkg:\S+)[ \t]*(?:#[ \t]*(\S.*))?$", text.split("allow-dependencies-licenses:")[1], re.M)
    assert sorted(purl for purl, _licence in entries) == sorted(found["allow-dependencies-licenses"])
    assert names.read_by_hand(text) == {purl for purl, _licence in entries}
    for purl, licence in entries:
        assert "@" not in purl, f"{purl} names a version, so the next version would not be covered"
        parts = re.split(r" (?:AND|OR) ", licence or "")
        off_the_list = [part for part in parts if part not in found["allow-licenses"]]
        why = (f"{purl} has `{licence}` beside it, and {off_the_list or 'nothing'} of it is on `allow-licenses`. Fix: "
               "write the licence that the package index names for the package after a `#` on its line; each part "
               "of it must be a licence of the list.")
        assert licence, why
        assert not off_the_list, why


def test_the_workflow_gives_the_action_the_settings_file_and_then_runs_the_step_for_a_licence_with_no_name():
    workflow = yaml.safe_load((ROOT / ".github" / "workflows" / "dependency-review.yml").read_text(encoding="utf-8"))
    steps = workflow["jobs"]["dependency-review"]["steps"]
    uses = [str(step.get("uses") or "").split("@")[0] for step in steps]
    at = uses.index("actions/dependency-review-action")
    assert steps[at]["with"] == {"config-file": "./.github/dependency-review-config.yml"}
    after = steps[at + 1]
    assert after["run"].rstrip().endswith('python3 "$RUNNER_TEMP/licence_names.py" --settings .github/dependency-review-config.yml')
    assert "python3 scripts/" not in after["run"], "the step runs a script from the checkout, which is the change's copy"
    assert after["env"] == {"BASE": "${{ steps.base.outputs.sha }}",
                            "INVALID_LICENSE_CHANGES": "${{ steps.%s.outputs.invalid-license-changes }}" % steps[at]["id"]}
    assert "if" not in after
    assert uses.index("actions/checkout") < at
    assert [step.get("id") for step in steps].index("base") < at + 1


def git(root, *args):
    return subprocess.run(["git", "-C", str(root), "-c", "user.name=test", "-c", "user.email=test@example.invalid", *args],
                          capture_output=True, text=True, check=True, timeout=60).stdout.strip()


def the_step_in_a_merge(tmp_path, on_the_base, on_the_branch, unlicensed):
    """Run the commands of the step in the merge of a branch into a base, each with its own copy of the script (None: no copy)."""
    workflow = yaml.safe_load((ROOT / ".github" / "workflows" / "dependency-review.yml").read_text(encoding="utf-8"))
    step = next(step for step in workflow["jobs"]["dependency-review"]["steps"] if "licence_names.py" in str(step.get("run")))
    root = tmp_path / "repo"
    (root / "scripts").mkdir(parents=True)
    (root / ".github").mkdir()
    git(root, "init", "-q", "-b", "dev")
    (root / ".github" / "dependency-review-config.yml").write_text(SETTINGS.read_text(encoding="utf-8"), encoding="utf-8")
    if on_the_base is not None:
        (root / "scripts" / "licence_names.py").write_text(on_the_base, encoding="utf-8")
    git(root, "add", "-A")
    git(root, "commit", "-q", "-m", "base")
    base = git(root, "rev-parse", "HEAD")
    git(root, "checkout", "-q", "-b", "work")
    (root / "scripts" / "licence_names.py").write_text(on_the_branch, encoding="utf-8")
    git(root, "add", "-A")
    git(root, "commit", "-q", "-m", "the branch's copy of the script")
    git(root, "checkout", "-q", "--detach", "dev")
    git(root, "merge", "-q", "--no-ff", "--no-edit", "work")
    (tmp_path / "runner").mkdir()
    return subprocess.run(["bash", "-e", "-c", step["run"]], cwd=root, capture_output=True, text=True, timeout=60, env={
        "PATH": f"{Path(sys.executable).parent}:/usr/bin:/bin", "RUNNER_TEMP": str(tmp_path / "runner"), "BASE": base,
        "INVALID_LICENSE_CHANGES": json.dumps({"unlicensed": unlicensed})})


def change(purl, kind="added", licence=None):
    """One entry of what the review action found, as GitHub writes it."""
    name = purl.split("/", 1)[1].split("@")[0]
    return {"change_type": kind, "package_url": purl, "name": name, "version": purl.split("@")[-1] if "@" in purl else "",
            "manifest": "requirements.txt", "license": licence}


A_SCRIPT_THAT_PASSES_ALL = 'print("the copy of the change ran")\n'


def test_the_step_runs_the_base_branchs_copy_of_the_script_when_a_change_rewrites_the_script(tmp_path):
    # The change adds a dependency with no named licence and a script that passes everything.
    real = SCRIPT.read_text(encoding="utf-8")
    done = the_step_in_a_merge(tmp_path, real, A_SCRIPT_THAT_PASSES_ALL, [change("pkg:pypi/new-package@1.0.0")])
    assert done.returncode == 1, done.stdout + done.stderr
    assert "::error title=no named licence::new-package 1.0.0" in done.stdout
    assert "the copy of the change ran" not in done.stdout


def test_the_changes_copy_of_the_script_runs_only_while_the_base_has_none(tmp_path):
    done = the_step_in_a_merge(tmp_path, None, A_SCRIPT_THAT_PASSES_ALL, [])
    assert done.returncode == 0
    assert "the copy of the change ran" in done.stdout


def test_the_base_branchs_copy_reads_the_settings_file_of_the_change(tmp_path):
    # The copy of the script lies outside the checkout; the list of the packages read by hand is the checkout's.
    real = SCRIPT.read_text(encoding="utf-8")
    done = the_step_in_a_merge(tmp_path, real, real + "# a line of the branch\n", [change("pkg:pypi/torch@2.14.0"), change("pkg:pypi/one@1.0")])
    assert done.returncode == 1
    assert done.stdout.count("::error title=no named licence::") == 1
    assert "::error title=no named licence::one 1.0" in done.stdout


@pytest.mark.parametrize("purl, kind, named", [
    pytest.param("pkg:pypi/new-package@1.0.0", "added", True, id="a-new-package"),
    pytest.param("pkg:pypi/torch-extra@1.0.0", "added", True, id="a-name-that-starts-as-one-read-by-hand"),
    pytest.param("pkg:npm/torch@1.0.0", "added", True, id="a-name-read-by-hand-in-another-index"),
    pytest.param("pkg:pypi/torch@2.14.0", "added", False, id="read-by-hand"),
    pytest.param("pkg:pypi/torch", "added", False, id="read-by-hand-with-no-version"),
    pytest.param("pkg:pypi/torch@2.14.0?arch=x86#sub", "added", False, id="read-by-hand-with-qualifiers"),
    pytest.param("pkg:npm/%40scope/name@3.1.0", "added", False, id="read-by-hand-with-a-scope"),
    pytest.param("pkg:githubactions/actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1", "added", False,
                 id="an-action"),
    pytest.param("pkg:pypi/new-package@1.0.0", "removed", False, id="a-removed-package"),
])
def test_a_new_package_with_no_named_licence_is_named_unless_it_was_read_by_hand_or_is_an_action(purl, kind, named):
    read_by_hand = {"pkg:pypi/torch", "pkg:npm/%40scope/name"}
    found = names.not_named({"unlicensed": [change(purl, kind)], "forbidden": [], "unresolved": []}, read_by_hand)
    assert [c["package_url"] for c in found] == ([purl] if named else [])


def run_the_script(*args, **env):
    """Run scripts/licence_names.py with PATH and the given variables only."""
    return subprocess.run([sys.executable, str(SCRIPT), *args], env={"PATH": os.environ["PATH"], **env},
                          capture_output=True, text=True, timeout=30)


def test_the_step_passes_when_nothing_is_without_a_name_and_fails_with_the_fix_for_each_that_is():
    nothing = run_the_script(INVALID_LICENSE_CHANGES=json.dumps({"forbidden": [], "unresolved": [], "unlicensed": []}))
    assert nothing.returncode == 0
    assert "has a named licence" in nothing.stdout
    # torch was read by hand: two lines for three packages.
    two = run_the_script(INVALID_LICENSE_CHANGES=json.dumps(
        {"unlicensed": [change("pkg:pypi/one@1.0"), change("pkg:pypi/torch@2.14.0"), change("pkg:npm/two@2.0")]}))
    assert two.returncode == 1
    lines = [line for line in two.stdout.splitlines() if line.startswith("::error title=no named licence::")]
    assert len(lines) == 2
    assert lines[0].startswith("::error title=no named licence::one 1.0 (requirements.txt): ")
    assert all("Fix: read the package's licence." in line and "allow-dependencies-licenses" in line for line in lines)


@pytest.mark.parametrize("env, settings, said", [
    pytest.param({"INVALID_LICENSE_CHANGES": ""}, None, ["cannot be read", "Fix: the step must get"], id="empty"),
    pytest.param({"INVALID_LICENSE_CHANGES": "not json"}, None, ["cannot be read", "Fix: the step must get"],
                 id="not-json"),
    pytest.param({"INVALID_LICENSE_CHANGES": "[]"}, None, ["cannot be read", "Fix: the step must get"], id="a-list"),
    pytest.param({"INVALID_LICENSE_CHANGES": '"text"'}, None, ["cannot be read", "Fix: the step must get"],
                 id="a-text"),
    pytest.param({"INVALID_LICENSE_CHANGES": '{"unlicensed": []}'}, "not-there.yml", ["cannot be read", "not-there.yml"],
                 id="no-settings-file"),
    pytest.param({}, None, ["INVALID_LICENSE_CHANGES"], id="no-output-of-the-action"),
])
def test_what_cannot_be_read_ends_the_step_with_status_2_and_the_fix(tmp_path, env, settings, said):
    args = ["--settings", str(tmp_path / settings)] if settings else []
    done = run_the_script(*args, **env)
    assert done.returncode == 2
    assert all(text in done.stderr for text in said), done.stderr


def test_the_bug_form_has_the_cause_field_and_the_triage_guide_says_when_to_set_it():
    form = yaml.safe_load((ROOT / ".github" / "ISSUE_TEMPLATE" / "bug_report.yml").read_text(encoding="utf-8"))
    (cause,) = [field for field in form["body"] if field.get("id") == "cause"]
    assert cause["type"] == "dropdown"
    assert cause["attributes"]["label"] == "Cause"
    options = cause["attributes"]["options"]
    assert options[cause["attributes"]["default"]] == "Not known yet"
    assert len(options) == len(set(options)) >= 5
    assert "validations" not in cause
    guide = " ".join((ROOT / "docs" / "TRIAGE.md").read_text(encoding="utf-8").split())
    assert "## When a bug closes Set **Cause** in the text of the issue" in guide
    for option in options[1:]:
        assert option.lower() in guide.lower(), f"the triage guide does not name the cause `{option}`"

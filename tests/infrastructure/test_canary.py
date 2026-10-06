"""The canary list fits the repository, its violations can be planted, and a check that stopped checking is named.

A check that is green on the canary is the one thing the script must not
miss, so each way a listed check can fail to be red has its own test. The
list itself is tested against the workflow files and the files it edits: a
renamed job or a moved line must fail here, not on the day the canary is
renewed.
"""
import importlib.util
import json
import shutil
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "canary.py"
NOW = datetime(2000, 1, 10, tzinfo=timezone.utc)
BASE_DATE = "2000-01-05T00:00:00Z"


def load(name):
    spec = importlib.util.spec_from_file_location(f"atlas_{name}", ROOT / "scripts" / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


@pytest.fixture(scope="module")
def canary():
    return load("canary")


@pytest.fixture(scope="module")
def manifest(canary):
    return canary.load_manifest(ROOT)


def names(manifest, key):
    return [name for plant in manifest["plants"] for name in plant.get(key, [])]


def pull(manifest, **changes):
    record = {"number": 7, "state": "open", "draft": True, "title": manifest["title"],
              "head": {"ref": manifest["branch"], "sha": "0" * 40}, "base": {"ref": "dev"}}
    return {**record, **changes}


def runs_as_listed(manifest):
    """The check runs of a canary on which every check does what the list says."""
    red = [{"id": n, "name": name, "status": "completed", "conclusion": "failure", "started_at": "2000-01-09T00:00:00Z"}
           for n, name in enumerate(names(manifest, "red"))]
    reports = [{"id": 100 + n, "name": name, "status": "completed", "conclusion": "success",
                "started_at": "2000-01-09T00:00:00Z", "annotation_paths": [plant["path"]]}
               for n, (name, plant) in enumerate((name, plant) for plant in manifest["plants"]
                                                 for name in plant.get("reports", []))]
    return red + reports


def required(manifest):
    return names(manifest, "red") + list(manifest["not_covered"])


def judged(canary, manifest, runs=None, required_names=None, **pull_changes):
    found = canary.judge(manifest, pull(manifest, **pull_changes), runs_as_listed(manifest) if runs is None else runs,
                         required(manifest) if required_names is None else required_names, BASE_DATE, NOW)
    return [finding.message for finding in found]


def changed(runs, name, **fields):
    return [{**run, **fields} if run["name"] == name else run for run in runs]


def only(found):
    """The one finding of a canary that differs from the list in one thing."""
    assert len(found) == 1, found
    return found[0]


# --- the list ------------------------------------------------------------------

def test_every_listed_check_is_a_job_of_a_workflow(manifest):
    checks = load("checks_ran")
    patterns = [checks.name_regex(job_id, job or {})[0] for workflow in checks.read_workflows(ROOT).values()
                for job_id, job in (workflow.get("jobs") or {}).items()]
    listed = names(manifest, "red") + names(manifest, "reports") + list(manifest["not_covered"])
    unknown = [name for name in listed if not any(pattern.fullmatch(name) for pattern in patterns)]
    assert not unknown, f"no workflow has a job with these names; change them in .github/canary.json: {unknown}"


def test_no_check_is_listed_twice_and_every_entry_says_what_it_plants(manifest):
    listed = names(manifest, "red") + list(manifest["not_covered"])
    assert len(listed) == len(set(listed))
    for plant in manifest["plants"]:
        assert plant["what"].strip(), plant["id"]
        assert plant.get("red") or plant.get("reports"), plant["id"]
    assert all(reason.strip() for reason in manifest["not_covered"].values())


def test_the_title_of_the_canary_fails_the_title_check(manifest):
    assert "(" not in manifest["title"].split(":")[0]


# --- planting ------------------------------------------------------------------

@pytest.fixture
def checkout(tmp_path, manifest):
    """A copy of the files the list edits, as they are in the repository."""
    for plant in manifest["plants"]:
        if plant["action"] in ("append", "insert_after_first_line", "replace_line"):
            (tmp_path / plant["path"]).parent.mkdir(parents=True, exist_ok=True)
            shutil.copy(ROOT / plant["path"], tmp_path / plant["path"])
    return tmp_path


def test_every_violation_can_be_planted_on_the_files_as_they_are(canary, manifest, checkout):
    written = canary.apply_plants(checkout, manifest)
    assert len(written) == sum(1 for plant in manifest["plants"] if plant["action"] != "title")
    for plant in manifest["plants"]:
        if plant["action"] in ("write", "append", "insert_after_first_line"):
            assert plant["lines"][-1] in (checkout / plant["path"]).read_text(encoding="utf-8"), plant["id"]


def test_the_edits_land_where_they_take_effect(canary, manifest, checkout):
    canary.apply_plants(checkout, manifest)
    by_action = {plant["action"]: plant for plant in manifest["plants"]}
    inserted = (checkout / by_action["insert_after_first_line"]["path"]).read_text(encoding="utf-8").splitlines()
    assert inserted[0].startswith("#!")
    assert inserted[2] == by_action["insert_after_first_line"]["lines"][-1]
    replaced = by_action["replace_line"]
    before = (ROOT / replaced["path"]).read_text(encoding="utf-8").splitlines()
    after = (checkout / replaced["path"]).read_text(encoding="utf-8").splitlines()
    assert [line for line in after if line not in before] == replaced["lines"]
    assert len(after) == len(before)


def test_the_planted_function_is_over_the_size_limit(canary, manifest, checkout):
    canary.apply_plants(checkout, manifest)
    size_gate = load("code_health")
    plant = next(plant for plant in manifest["plants"] if plant["action"] == "long_function")
    text = (checkout / plant["path"]).read_text(encoding="utf-8")
    assert max(length for _, length in size_gate.py_functions(text, plant["path"])) > size_gate.FUNC_MAX


def test_planting_twice_is_refused_and_writes_nothing(canary, manifest, checkout):
    canary.apply_plants(checkout, manifest)
    before = {path: path.read_bytes() for path in checkout.rglob("*") if path.is_file()}
    with pytest.raises(canary.PlantError, match="exists already"):
        canary.apply_plants(checkout, manifest)
    assert before == {path: path.read_bytes() for path in checkout.rglob("*") if path.is_file()}


def test_an_edit_that_no_longer_fits_its_file_is_refused_with_a_fix(canary, manifest, checkout):
    replaced = next(plant for plant in manifest["plants"] if plant["action"] == "replace_line")
    line = (replaced["starts_with"] + "0" * 40 + "\n")
    for text, count in (("FROM scratch\n", 0), (line + line, 2)):
        (checkout / replaced["path"]).write_text(text, encoding="utf-8")
        with pytest.raises(canary.PlantError, match=f"{count} line.*needs exactly one. Fix:"):
            canary.apply_plants(checkout, manifest)
        assert not (checkout / "tests").exists()


@pytest.mark.parametrize("length", [100, 1001, "120", None])
def test_a_planted_function_length_outside_its_range_is_refused(canary, tmp_path, length):
    plant = {"id": "size", "action": "long_function", "path": "scripts/long.py", "length": length}
    with pytest.raises(canary.PlantError, match="must have 101 to 1000 lines.*Fix:"):
        canary.apply_plants(tmp_path, {"plants": [plant]})
    assert not (tmp_path / "scripts").exists()


def test_a_path_outside_the_checkout_is_refused(canary, tmp_path):
    with pytest.raises(canary.PlantError, match="outside the checkout"):
        canary.apply_plants(tmp_path, {"plants": [{"id": "x", "action": "write", "path": "../x.py", "lines": ["x"]}]})


def test_planting_is_refused_on_any_other_branch(tmp_path):
    subprocess.run(["git", "init", "-q", "-b", "dev", str(tmp_path)], check=True)
    (tmp_path / ".github").mkdir()
    shutil.copy(ROOT / ".github" / "canary.json", tmp_path / ".github" / "canary.json")
    done = subprocess.run([sys.executable, str(SCRIPT), "--root", str(tmp_path), "plant"], capture_output=True,
                          text=True, check=False)
    assert done.returncode == 2
    assert "belong only on" in done.stderr
    assert "fix:" in done.stderr
    assert [path.name for path in tmp_path.iterdir() if path.name != ".git"] == [".github"]


# --- checking ------------------------------------------------------------------

def test_a_canary_that_is_as_listed_has_no_finding(canary, manifest):
    assert judged(canary, manifest) == []


def test_a_listed_check_that_passed_is_named_with_what_it_no_longer_catches(canary, manifest):
    message = only(judged(canary, manifest, changed(runs_as_listed(manifest), "shellcheck", conclusion="success")))
    assert "`shellcheck` passed on the canary" in message
    assert "an unused variable" in message
    assert "Fix:" in message


@pytest.mark.parametrize("fields, words", [
    ({"conclusion": "skipped"}, "ended as `skipped`"),
    ({"conclusion": "cancelled"}, "ended as `cancelled`"),
    ({"status": "in_progress", "conclusion": None}, "has not finished"),
])
def test_a_listed_check_that_did_not_judge_is_named(canary, manifest, fields, words):
    message = only(judged(canary, manifest, changed(runs_as_listed(manifest), "pr title", **fields)))
    assert "`pr title`" in message
    assert words in message


def test_a_listed_check_that_did_not_run_is_named(canary, manifest):
    runs = [run for run in runs_as_listed(manifest) if run["name"] != "go test (proxy)"]
    assert "`go test (proxy)` did not run" in only(judged(canary, manifest, runs))


def test_the_newest_run_of_a_check_counts(canary, manifest):
    older = {"id": 999, "name": "shellcheck", "status": "completed", "conclusion": "success",
             "started_at": "2000-01-08T00:00:00Z"}
    assert judged(canary, manifest, runs_as_listed(manifest) + [older]) == []
    newer = {**older, "started_at": "2000-01-09T12:00:00Z"}
    assert len(judged(canary, manifest, runs_as_listed(manifest) + [newer])) == 1


@pytest.mark.parametrize("paths", [[], [".github"]])
def test_a_report_only_check_with_no_note_on_the_planted_file_is_named(canary, manifest, paths):
    name = names(manifest, "reports")[0]
    message = only(judged(canary, manifest, changed(runs_as_listed(manifest), name, annotation_paths=paths)))
    assert f"`{name}` reported nothing for" in message


def test_a_required_check_that_the_list_does_not_know_is_named(canary, manifest):
    message = only(judged(canary, manifest, required_names=required(manifest) + ["a new required check"]))
    assert "`a new required check` is not in the canary list" in message


@pytest.mark.parametrize("changes, words", [
    ({"draft": False}, "is not a draft"),
    ({"state": "closed"}, "is closed"),
    ({"title": "fix(ci): a title that passes"}, "its title is"),
    ({"head": {"ref": "fix/something", "sha": "0" * 40}}, "is on branch `fix/something`"),
])
def test_a_pull_request_that_is_not_the_canary_as_listed_is_named(canary, manifest, changes, words):
    message = only(judged(canary, manifest, **changes))
    assert words in message
    assert "Fix:" in message


def test_a_canary_that_was_not_renewed_in_time_is_named(canary, manifest):
    late = datetime(2000, 1, 5 + manifest["max_age_days"] + 1, tzinfo=timezone.utc)
    found = canary.judge(manifest, pull(manifest), runs_as_listed(manifest), required(manifest), BASE_DATE, late)
    assert "days ago" in only(found).message
    in_time = datetime(2000, 1, 5 + manifest["max_age_days"], tzinfo=timezone.utc)
    assert canary.judge(manifest, pull(manifest), runs_as_listed(manifest), required(manifest), BASE_DATE, in_time) == []


def test_without_a_token_it_stops_with_a_fix(tmp_path):
    env = {"PATH": str(tmp_path), "GITHUB_REPOSITORY": "example/example"}
    done = subprocess.run([sys.executable, str(SCRIPT), "check", "--pr", "1"], capture_output=True, text=True, env=env,
                          check=False)
    assert done.returncode == 2
    assert "no GitHub token" in done.stderr
    assert "fix:" in done.stderr


def test_the_list_is_plain_json_with_the_keys_the_script_reads():
    data = json.loads((ROOT / ".github" / "canary.json").read_text(encoding="utf-8"))
    assert set(data) == {"branch", "title", "max_age_days", "plants", "not_covered"}

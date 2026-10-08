"""The canary list fits the repository, its violations can be planted, and a check that stopped checking is named.

A check that is green on the canary is the one thing the script must not
miss, so each way a listed check can fail to be red has its own test. The
list itself is tested against the workflow files and the files it edits: a
renamed job or a moved line must fail here, not on the day the canary is
renewed.
"""
import http.server
import importlib.util
import json
import re
import shutil
import subprocess
import sys
import threading
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "canary.py"
NOW = datetime(2000, 1, 10, tzinfo=timezone.utc)
# The date of the oldest commit of `dev` that the canary does not have.
LACKING_SINCE = "2000-01-05T00:00:00Z"


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


def shown(plant, name):
    """The text the list says check `name` shows when it is red for this plant."""
    return plant["shows"][name] if isinstance(plant["shows"], dict) else plant["shows"]


def runs_as_listed(manifest):
    """The check runs of a canary on which every check does what the list says."""
    red = [{"id": n, "name": name, "status": "completed", "conclusion": "failure", "started_at": "2000-01-09T00:00:00Z",
            "log": f"a line of the job\n##[error]{shown(plant, name)}\nProcess completed with exit code 1.\n"}
           for n, (name, plant) in enumerate((name, plant) for plant in manifest["plants"]
                                             for name in plant.get("red", []))]
    reports = [{"id": 100 + n, "name": name, "status": "completed", "conclusion": "success",
                "started_at": "2000-01-09T00:00:00Z", "annotation_paths": [plant["path"]]}
               for n, (name, plant) in enumerate((name, plant) for plant in manifest["plants"]
                                                 for name in plant.get("reports", []))]
    return red + reports


def required(manifest):
    return names(manifest, "red") + list(manifest["not_covered"])


def judged(canary, manifest, runs=None, required_names=None, **pull_changes):
    found = canary.judge(manifest, pull(manifest, **pull_changes), runs_as_listed(manifest) if runs is None else runs,
                         required(manifest) if required_names is None else required_names, LACKING_SINCE, NOW)
    return [finding.message for finding in found]


def changed(runs, name, **fields):
    return [{**run, **fields} if run["name"] == name else run for run in runs]


def only(found):
    """The one finding of a canary that differs from the list in one thing."""
    assert len(found) == 1, found
    return found[0]


# --- the list ------------------------------------------------------------------

def job_names(root=ROOT):
    """Each job of the workflows: the pattern of its names, how many letters of a name it fixes, and its `name:`."""
    checks = load("checks_ran")
    return [(*checks.name_regex(job_id, job or {}), str((job or {}).get("name") or ""))
            for workflow in checks.read_workflows(root).values() for job_id, job in (workflow.get("jobs") or {}).items()]


def is_a_job(name, jobs):
    """Whether a workflow has a job that GitHub reports under this name.

    A job whose name is an expression alone fits every name, so it proves
    nothing. It counts only for the one name GitHub gives it when it is
    skipped: the text of the expression.
    """
    return any(pattern.fullmatch(name) if fixed else name == written.strip("${} ")
               for pattern, fixed, written in jobs)


def test_every_listed_check_is_a_job_of_a_workflow(manifest):
    jobs = job_names()
    listed = (names(manifest, "red") + names(manifest, "reports") + list(manifest["not_covered"])
              + list(manifest["side_effects"]))
    unknown = [name for name in listed if not is_a_job(name, jobs)]
    assert not unknown, f"no workflow has a job with these names; change them in .github/canary.json: {unknown}"


def test_a_name_that_no_job_has_is_not_taken_for_a_job():
    jobs = job_names()
    assert any(fixed == 0 for _, fixed, _ in jobs), "no job is named by an expression alone; this test can go"
    assert not is_a_job("a job that is not there", jobs)
    assert not is_a_job("shellchek", jobs)
    assert is_a_job("shellcheck", jobs)
    assert is_a_job("bootstrap on debian-12", jobs)
    assert is_a_job("matrix.service.name", jobs)


def test_a_check_of_a_service_is_not_a_job_of_a_workflow(manifest):
    jobs = job_names()
    assert [name for name in manifest["other_checks"] if is_a_job(name, jobs)] == []


def test_no_check_is_listed_twice_and_every_entry_says_what_it_plants(manifest):
    listed = (names(manifest, "red") + list(manifest["not_covered"]) + list(manifest["other_checks"])
              + list(manifest["side_effects"]))
    assert len(listed) == len(set(listed))
    for plant in manifest["plants"]:
        assert plant["what"].strip(), plant["id"]
        assert plant.get("red") or plant.get("reports"), plant["id"]
    assert all(reason.strip() for reason in [*manifest["not_covered"].values(), *manifest["other_checks"].values(),
                                             *manifest["side_effects"].values()])


def test_every_check_that_must_be_red_has_the_text_its_log_shows(manifest):
    for plant in manifest["plants"]:
        for name in plant.get("red", []):
            assert len(shown(plant, name).strip()) >= 8, f"{plant['id']}: `shows` for {name} is too short to mean it"
        if isinstance(plant.get("shows"), dict):
            assert sorted(plant["shows"]) == sorted(plant["red"]), plant["id"]


def test_a_check_with_no_plant_is_not_one_that_a_plant_turns_red(manifest):
    planted = set(names(manifest, "red")) | set(names(manifest, "reports"))
    assert planted.isdisjoint(manifest["not_covered"])
    assert planted.isdisjoint(manifest["other_checks"])


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
    assert sorted(written) == sorted({plant["path"] for plant in manifest["plants"] if plant["action"] != "title"})
    for plant in manifest["plants"]:
        if plant["action"] in ("write", "append", "insert_after_first_line"):
            assert plant["lines"][-1] in (checkout / plant["path"]).read_text(encoding="utf-8"), plant["id"]


def test_the_edits_land_where_they_take_effect(canary, manifest, checkout):
    canary.apply_plants(checkout, manifest)
    by_action = {plant["action"]: plant for plant in manifest["plants"]}
    inserted = (checkout / by_action["insert_after_first_line"]["path"]).read_text(encoding="utf-8").splitlines()
    assert inserted[0].startswith("#!")
    assert inserted[2] == by_action["insert_after_first_line"]["lines"][-1]
    replaced = {}
    for plant in manifest["plants"]:
        if plant["action"] == "replace_line":
            replaced.setdefault(plant["path"], []).extend(plant["lines"])
    for path, lines in replaced.items():
        before = (ROOT / path).read_text(encoding="utf-8").splitlines()
        after = (checkout / path).read_text(encoding="utf-8").splitlines()
        assert sorted(line for line in after if line not in before) == sorted(lines), path
        assert len(after) == len(before), path


def test_plants_that_share_a_file_all_land_in_it(canary, manifest, checkout):
    shared = ".github/workflows/test.yml"
    plants = [plant for plant in manifest["plants"] if plant.get("path") == shared]
    assert len(plants) == 4
    canary.apply_plants(checkout, manifest)
    text = (checkout / shared).read_text(encoding="utf-8")
    for plant in plants:
        assert plant["lines"][0] in text, plant["id"]
        # The marker the job shows is put together when the sample runs: it is not in the workflow file as one word.
        assert plant["shows"] not in text, plant["id"]
    samples = yaml.safe_load(text)["jobs"]["container-smoke"]["strategy"]["matrix"]["include"]
    assert [sample["language"] for sample in samples] == ["java", "kotlin", "ruby", "php"]
    assert all("failed" in sample["code"] for sample in samples)


def test_a_planted_file_stays_valid_where_its_reader_needs_that(canary, manifest, checkout):
    canary.apply_plants(checkout, manifest)
    budgets = json.loads((checkout / "tests/perf/budgets.json").read_text(encoding="utf-8"))
    assert budgets["deterministic_max"]["proxy_binary_bytes"] == 1
    changed = [line for line in (checkout / "proxy/agent.go").read_text(encoding="utf-8").splitlines()
               if "canaryreference" in line]
    assert len(changed) == 1 and changed[0].startswith("\tsb.WriteString(")


def test_the_planted_function_is_over_the_size_limit(canary, manifest, checkout):
    canary.apply_plants(checkout, manifest)
    size_gate = load("code_health")
    plant = next(plant for plant in manifest["plants"]
                 if plant["action"] == "long_function" and plant.get("language", "python") == "python")
    text = (checkout / plant["path"]).read_text(encoding="utf-8")
    assert max(length for _, length in size_gate.py_functions(text, plant["path"])) > size_gate.FUNC_MAX


def test_the_planted_typescript_function_is_over_the_extensions_limit(canary, manifest, checkout):
    canary.apply_plants(checkout, manifest)
    plant = next(plant for plant in manifest["plants"] if plant.get("language") == "typescript")
    lines = (checkout / plant["path"]).read_text(encoding="utf-8").splitlines()
    start, end = lines.index("export function canaryLongFunction(): number {"), lines.index("}")
    limit = re.search(r"^const FUNCTION_LINES = (\d+);", (ROOT / "extensions/vscode/eslint.config.mjs")
                      .read_text(encoding="utf-8"), re.M)
    assert end - start + 1 > int(limit.group(1))
    assert plant["path"].startswith("extensions/vscode/src/")


def test_a_planted_function_in_a_language_the_script_does_not_write_is_refused(canary, tmp_path):
    plant = {"id": "size", "action": "long_function", "path": "src/long.rs", "length": 120, "language": "rust"}
    with pytest.raises(canary.PlantError, match="python or typescript.*Fix:"):
        canary.apply_plants(tmp_path, {"plants": [plant]})
    assert not (tmp_path / "src").exists()


def test_two_plants_that_write_the_same_new_file_are_refused(canary, tmp_path):
    plant = {"id": "a", "action": "write", "path": "tests/test_x.py", "lines": ["x = 1"]}
    with pytest.raises(canary.PlantError, match="exists already"):
        canary.apply_plants(tmp_path, {"plants": [plant, {**plant, "id": "b"}]})
    assert not (tmp_path / "tests").exists()


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
        written = [plant["path"] for plant in manifest["plants"] if plant["action"] in ("write", "long_function")]
        assert [path for path in written if (checkout / path).exists()] == []


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


def test_a_check_that_is_red_without_the_text_of_its_plant_is_named(canary, manifest):
    network = "curl: (28) Connection timed out\nProcess completed with exit code 28.\n"
    message = only(judged(canary, manifest, changed(runs_as_listed(manifest), "shellcheck", log=network)))
    assert "`shellcheck` is red on the canary, but not for its plant" in message
    assert "does not hold `canary_unused`" in message
    assert "run the job again" in message
    assert "Fix:" in message
    no_log = [{key: value for key, value in run.items() if key != "log"} if run["name"] == "shellcheck" else run
              for run in runs_as_listed(manifest)]
    assert "but not for its plant" in only(judged(canary, manifest, no_log))


def test_each_check_of_a_plant_with_two_checks_has_a_text_of_its_own(canary, manifest):
    plant = next(plant for plant in manifest["plants"] if plant["id"] == "workflow lint")
    one, other = plant["red"]
    swapped = changed(runs_as_listed(manifest), one, log=shown(plant, other))
    assert f"`{one}` is red on the canary, but not for its plant" in only(judged(canary, manifest, swapped))


def test_a_check_that_ran_and_that_the_list_does_not_know_is_named(canary, manifest):
    new = {"id": 900, "name": "a new job", "status": "completed", "conclusion": "success",
           "started_at": "2000-01-09T00:00:00Z"}
    message = only(judged(canary, manifest, runs_as_listed(manifest) + [new]))
    assert "`a new job` ran on the canary and the list does not know it" in message
    assert "`not_covered` with the reason" in message
    for name in [*manifest["not_covered"], *manifest["other_checks"]]:
        assert judged(canary, manifest, runs_as_listed(manifest) + [{**new, "name": name}]) == [], name


SIDE = "a check that another makes red"
SIDE_REASON = "It has no violation of its own. It is red through the violation of `ruff`."


def with_a_side_effect(manifest):
    """The list with one side effect made for the test, so the rule is tested whatever the real list holds."""
    return {**manifest, "side_effects": {SIDE: SIDE_REASON}}


@pytest.mark.parametrize("conclusion", ["failure", "timed_out", "cancelled", "startup_failure"])
def test_a_check_with_no_plant_that_does_not_pass_is_named_unless_the_list_says_why(canary, manifest, conclusion):
    listed = with_a_side_effect(manifest)
    ended = {"id": 901, "status": "completed", "conclusion": conclusion, "started_at": "2000-01-09T00:00:00Z"}
    for name in [*listed["not_covered"], *listed["other_checks"]]:
        message = only(judged(canary, listed, runs_as_listed(listed) + [{**ended, "name": name}]))
        assert f"`{name}` ended as `{conclusion}` on the canary, and the list gives it no violation and no side effect" in message
        assert "`side_effects`" in message and "Fix:" in message
    assert judged(canary, listed, runs_as_listed(listed) + [{**ended, "name": SIDE, "conclusion": "failure"}]) == []


@pytest.mark.parametrize("conclusion", ["success", "skipped", "neutral"])
def test_a_check_with_no_plant_that_passed_or_did_not_run_is_not_named(canary, manifest, conclusion):
    ended = {"id": 904, "status": "completed", "conclusion": conclusion, "started_at": "2000-01-09T00:00:00Z"}
    for name in [*manifest["not_covered"], *manifest["other_checks"]]:
        assert judged(canary, manifest, runs_as_listed(manifest) + [{**ended, "name": name}]) == [], name


def test_a_check_listed_as_red_through_another_violation_that_is_not_red_is_named(canary, manifest):
    listed = with_a_side_effect(manifest)
    done = {"id": 902, "name": SIDE, "status": "completed", "conclusion": "success", "started_at": "2000-01-09T00:00:00Z"}
    message = only(judged(canary, listed, runs_as_listed(listed) + [done]))
    assert f"`{SIDE}` is listed as red on the canary through another check's violation, and it ended as `success`" in message
    assert "`not_covered`" in message
    running = {**done, "status": "in_progress", "conclusion": None}
    assert judged(canary, listed, runs_as_listed(listed) + [running]) == []


def test_a_red_through_another_violation_and_a_listed_check_that_is_not_there_get_a_line(canary, manifest):
    listed = with_a_side_effect(manifest)
    red = {"id": 903, "status": "completed", "conclusion": "failure", "started_at": "2000-01-09T00:00:00Z"}
    lines = canary.notes(listed, canary.latest_checks(runs_as_listed(listed) + [{**red, "name": SIDE}]))
    assert lines[0] == f"red through another check's violation, as listed: `{SIDE}`. {SIDE_REASON}"
    assert lines[0].count("no violation of its own") == 1
    absent = [line for line in lines if line.startswith("listed, and not there in this run: ")]
    others = [*listed["not_covered"], *listed["other_checks"]]
    assert sorted(line.split("`")[1] for line in absent) == sorted(others)
    assert len(lines) == 1 + len(others)
    everything = runs_as_listed(listed) + [{**red, "name": name, "conclusion": "success"} for name in [*others, SIDE]]
    assert canary.notes(listed, canary.latest_checks(everything)) == []
    assert [line for line in canary.notes(listed, canary.latest_checks(runs_as_listed(listed)))
            if SIDE in line] == [f"listed, and not there in this run: `{SIDE}`. {SIDE_REASON}"]


def test_the_rule_for_side_effects_holds_with_none_on_the_list(canary, manifest):
    none = {**manifest, "side_effects": {}}
    assert judged(canary, none, runs_as_listed(none)) == []
    assert not [line for line in canary.notes(none, canary.latest_checks(runs_as_listed(none))) if line.startswith("red ")]


def test_a_side_effect_names_the_violation_it_comes_from(manifest):
    planted = set(names(manifest, "red"))
    for name, reason in manifest["side_effects"].items():
        assert name not in planted
        assert any(f"`{check}`" in reason for check in planted), f"{name}: the reason names no check that has a violation"


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
    found = canary.judge(manifest, pull(manifest), runs_as_listed(manifest), required(manifest), LACKING_SINCE, late)
    message = only(found).message
    assert f"does not have for {manifest['max_age_days'] + 1} days (limit {manifest['max_age_days']})" in message
    assert "Fix: Renew the canary" in message
    in_time = datetime(2000, 1, 5 + manifest["max_age_days"], tzinfo=timezone.utc)
    assert canary.judge(manifest, pull(manifest), runs_as_listed(manifest), required(manifest), LACKING_SINCE, in_time) == []


def test_a_canary_on_the_head_of_dev_is_not_old_whatever_the_age_of_its_runs(canary, manifest):
    # Its checks ran in the year 2000, and `dev` has no commit that it lacks: they ran on the files of today.
    much_later = datetime(2026, 1, 1, tzinfo=timezone.utc)
    assert canary.judge(manifest, pull(manifest), runs_as_listed(manifest), required(manifest), None, much_later) == []


# --- the check as a job runs it: with no number, against what GitHub answers ---------------------------------------

REPO, HEAD = "o/r", "0" * 40
SEARCH = "repos/o/r/pulls?state=open&head=o%3Acanary%2Fmust-stay-red&per_page=100"


def days_ago(days):
    return (datetime.now(timezone.utc) - timedelta(days=days)).strftime("%Y-%m-%dT%H:%M:%SZ")


class GitHub:
    """What GitHub answers for a canary on which every check does what the list says. A test changes one answer."""

    API = "https://api.github.invalid"

    def __init__(self, manifest, runs=None, lacking=3, lacking_since=None, pulls=None, fails=()):
        self.manifest, self.asked, self.fails = manifest, [], fails
        self.runs = runs_as_listed(manifest) if runs is None else runs
        commits = [{"sha": "c" * 40, "commit": {"committer": {"date": lacking_since or days_ago(2)}}}] if lacking else []
        self.answers = {
            SEARCH: [[pull(manifest)] if pulls is None else pulls],
            "repos/o/r/pulls/7": [pull(manifest)],
            f"repos/o/r/commits/{HEAD}/check-runs?per_page=100": [{"check_runs": [{k: v for k, v in run.items() if k != "log"} for run in self.runs]}],
            "repos/o/r/rules/branches/dev?per_page=100": [[]],
            f"repos/o/r/compare/{HEAD}...dev?per_page=100": [{"merge_base_commit": {"sha": "b" * 40, "commit": {"committer": {"date": "1999-01-01T00:00:00Z"}}},
                                                          "total_commits": lacking, "commits": commits}],
        }

    def api(self, path, key):
        assert key == "a-token"
        self.asked.append(path)
        if any(words in path for words in self.fails):
            raise RuntimeError(f"GitHub did not answer {path}: a planted failure")
        if "/annotations" in path:
            run = next(run for run in self.runs if str(run["id"]) == path.split("/")[4])
            return [[{"path": place} for place in run.get("annotation_paths", [])]]
        return self.answers[path]

    def required_names(self, rules, own):
        return required(self.manifest)

    def log_of(self, api_root, repo, job, key):
        assert (api_root, repo, key) == (self.API, REPO, "a-token")
        return next(run for run in self.runs if run["id"] == job)["log"]


def checked(canary, monkeypatch, tmp_path, capsys, github, number=None):
    """Run the check against these answers. Gives its status, what it printed, and what it wrote to the page of the run."""
    page = tmp_path / "page.md"
    monkeypatch.setenv("GITHUB_STEP_SUMMARY", str(page))
    monkeypatch.setattr(canary, "load_checks_ran", lambda: github)
    monkeypatch.setattr(canary, "token", lambda: "a-token")
    monkeypatch.setattr(canary, "repository", lambda root: REPO)
    monkeypatch.setattr(canary, "job_log", github.log_of)
    status = canary.check(ROOT, number)
    printed = capsys.readouterr()
    return status, printed.out + printed.err, page.read_text(encoding="utf-8") if page.exists() else ""


def outside_code(page):
    """The text of a page that is in no fenced block and in no code marks: what a reader's program may read as a
    mention or as a link to an issue. The weekly cleanup's script has the reading; both pages are read by one rule."""
    plain = load("weekly_cleanup").plain_part(page)
    assert "a fenced block is not closed" not in plain
    return plain


def test_the_one_open_pull_request_of_the_canary_branch_is_found_and_a_canary_as_listed_passes(canary, manifest, monkeypatch, tmp_path, capsys):
    github = GitHub(manifest, lacking_since=days_ago(3))
    status, printed, page = checked(canary, monkeypatch, tmp_path, capsys, github)
    assert status == 0, printed
    assert github.asked[0] == SEARCH
    assert github.asked[1] == "repos/o/r/pulls/7"
    assert "note pull request 7 at 0000000:" in printed
    assert "note the canary stands on commit `bbbbbbb` of `dev`; `dev` is 3 commit(s) ahead, and the oldest of them is 3 day(s) old (limit 14)" in printed
    assert printed.rstrip().endswith("canary: every listed check is red for its plant, and every listed report is there")
    by = (datetime.now(timezone.utc) - timedelta(days=3) + timedelta(days=manifest["max_age_days"])).date().isoformat()
    assert page.splitlines()[2] == ("**As listed.** Every listed check is red for its own violation, and every listed report is "
                                    f"there. Renew the canary by {by}.")


def test_with_a_number_no_pull_request_is_looked_for(canary, manifest, monkeypatch, tmp_path, capsys):
    github = GitHub(manifest)
    status, printed, _page = checked(canary, monkeypatch, tmp_path, capsys, github, number=7)
    assert status == 0, printed
    assert SEARCH not in github.asked


def test_a_pull_request_of_another_branch_in_the_answer_is_not_taken_for_the_canary(canary, manifest, monkeypatch, tmp_path, capsys):
    other = pull(manifest, number=9, head={"ref": "fix/something", "sha": "9" * 40})
    status, printed, _page = checked(canary, monkeypatch, tmp_path, capsys, GitHub(manifest, pulls=[other, pull(manifest)]))
    assert status == 0, printed
    assert "note pull request 7 at" in printed


def one_run(manifest):
    """A check that must be red, and the runs with that check as given."""
    name = names(manifest, "red")[0]
    return name, lambda **fields: changed(runs_as_listed(manifest), name, **fields)


def test_a_listed_check_that_is_green_fails_the_job_and_is_named_on_the_page(canary, manifest, monkeypatch, tmp_path, capsys):
    name, runs = one_run(manifest)
    status, printed, page = checked(canary, monkeypatch, tmp_path, capsys, GitHub(manifest, runs=runs(conclusion="success")))
    assert status == 1
    assert f"FAIL check `{name}` passed on the canary" in printed
    assert "canary: 1 thing(s) are not as the list says" in printed
    assert page.splitlines()[2].startswith("**A finding.** Something is not as the list says.")
    assert f"FAIL check `{name}` passed on the canary" in page


def test_a_listed_check_that_is_red_for_another_reason_than_its_plant_fails_the_job(canary, manifest, monkeypatch, tmp_path, capsys):
    name, runs = one_run(manifest)
    status, printed, _page = checked(canary, monkeypatch, tmp_path, capsys,
                                     GitHub(manifest, runs=runs(log="The runner lost the network.\n")))
    assert status == 1
    assert f"FAIL check `{name}` is red on the canary, but not for its plant" in printed


def test_a_listed_check_that_is_missing_fails_the_job(canary, manifest, monkeypatch, tmp_path, capsys):
    name, _runs = one_run(manifest)
    runs = [run for run in runs_as_listed(manifest) if run["name"] != name]
    status, printed, _page = checked(canary, monkeypatch, tmp_path, capsys, GitHub(manifest, runs=runs))
    assert status == 1
    assert f"FAIL check `{name}` did not run on the canary" in printed


def test_a_canary_whose_runs_are_of_an_old_dev_fails_the_job_and_the_page_says_when_it_had_to_be_renewed(canary, manifest, monkeypatch, tmp_path, capsys):
    limit = manifest["max_age_days"]
    status, printed, page = checked(canary, monkeypatch, tmp_path, capsys, GitHub(manifest, lacking_since=days_ago(limit + 1)))
    assert status == 1
    assert f"FAIL `dev` has had a commit that the canary does not have for {limit + 1} days (limit {limit})" in printed
    by = (datetime.now(timezone.utc) - timedelta(days=1)).date().isoformat()
    assert page.splitlines()[2] == ("**A finding.** Something is not as the list says. The lines below name each thing and its "
                                    f"fix. The canary had to be renewed by {by}.")


def test_on_its_last_day_the_canary_still_passes(canary, manifest, monkeypatch, tmp_path, capsys):
    status, printed, page = checked(canary, monkeypatch, tmp_path, capsys, GitHub(manifest, lacking_since=days_ago(manifest["max_age_days"])))
    assert status == 0, printed
    assert f"Renew the canary by {datetime.now(timezone.utc).date().isoformat()}." in page


def test_the_age_is_not_counted_from_the_commit_that_the_canary_stands_on(canary, manifest, monkeypatch, tmp_path, capsys):
    # The commit that the canary stands on is from 1999 in these answers. `dev` has no newer commit.
    status, printed, page = checked(canary, monkeypatch, tmp_path, capsys, GitHub(manifest, lacking=0))
    assert status == 0, printed
    assert "note the canary stands on the head of `dev` (`bbbbbbb`), so there is nothing to renew" in printed
    assert page.splitlines()[2].endswith("The canary stands on the head of `dev`: there is nothing to renew.")


@pytest.mark.parametrize("fails", ["/check-runs?", "/pulls/7", "/pulls?state=open", "/compare/", "/rules/branches/", "/annotations"])
def test_when_github_cannot_be_read_nothing_is_judged_and_that_is_not_a_pass(canary, manifest, monkeypatch, tmp_path, capsys, fails):
    status, printed, page = checked(canary, monkeypatch, tmp_path, capsys, GitHub(manifest, fails=(fails,)))
    assert status == 2
    assert "canary: cannot read the canary pull request or its checks:" in printed
    assert "fix: run this again" in printed
    assert page.splitlines()[2].startswith("**Not judged.**")
    assert "This is not a pass." in page.splitlines()[2]
    assert "every listed check is red" not in printed + page


def test_an_answer_of_another_shape_is_not_judged(canary, manifest, monkeypatch, tmp_path, capsys):
    github = GitHub(manifest)
    # `dev` is four commits ahead by the number, and the list of those commits is empty.
    github.answers[f"repos/o/r/compare/{HEAD}...dev?per_page=100"] = [{"merge_base_commit": {"sha": "b" * 40}, "total_commits": 4, "commits": []}]
    status, _printed, page = checked(canary, monkeypatch, tmp_path, capsys, github)
    assert status == 2
    assert page.splitlines()[2].startswith("**Not judged.**")


@pytest.mark.parametrize("commits", [None, "not a list", [{"commit": None}], [{}], [None]])
def test_an_answer_with_a_part_of_another_kind_is_not_judged_and_is_no_finding(canary, manifest, monkeypatch, tmp_path, capsys, commits):
    github = GitHub(manifest)
    github.answers[f"repos/o/r/compare/{HEAD}...dev?per_page=100"] = [{"merge_base_commit": {"sha": "b" * 40}, "total_commits": 4, "commits": commits}]
    status, printed, page = checked(canary, monkeypatch, tmp_path, capsys, github)
    assert status == 2, printed
    assert page.splitlines()[2].startswith("**Not judged.**")


def test_with_no_open_canary_pull_request_the_job_fails_and_says_how_to_renew(canary, manifest, monkeypatch, tmp_path, capsys):
    github = GitHub(manifest, pulls=[])
    status, printed, page = checked(canary, monkeypatch, tmp_path, capsys, github)
    assert status == 1
    assert "FAIL no pull request from `canary/must-stay-red` is open, so no run shows that a check still turns red. Fix: Renew the canary" in printed
    assert github.asked == [SEARCH]
    assert page.splitlines()[2].startswith("**A finding.**")


def test_with_two_open_canary_pull_requests_the_job_fails_and_names_both(canary, manifest, monkeypatch, tmp_path, capsys):
    github = GitHub(manifest, pulls=[pull(manifest), pull(manifest, number=9)])
    status, printed, _page = checked(canary, monkeypatch, tmp_path, capsys, github)
    assert status == 1
    assert "FAIL 2 pull requests from `canary/must-stay-red` are open (numbers 7, 9), and the canary is one pull request. Fix: close all but one." in printed
    assert github.asked == [SEARCH]


def test_nothing_that_github_or_a_file_gave_can_be_read_from_the_page_as_a_mention_or_a_link(canary, manifest, monkeypatch, tmp_path, capsys):
    github = GitHub(manifest)
    # A title with one code mark in it: the marks around it in the finding would end at that mark.
    title = "one ` mark, then @someone and #12, and ````` five"
    github.answers["repos/o/r/pulls/7"] = [pull(manifest, title=title)]
    status, _printed, page = checked(canary, monkeypatch, tmp_path, capsys, github)
    assert status == 1
    assert f"its title is `{title}`" in page
    plain = outside_code(page)
    assert "@" not in plain
    assert not re.search(r"#\d", plain)
    assert plain.splitlines()[0] == "### The canary"


@pytest.mark.parametrize("text, fence", [("no marks", "```"), ("one ` and three ```", "````"), ("a line\n``````\nof six", "```````")])
def test_the_fence_of_a_block_is_longer_than_any_run_of_marks_in_it(canary, text, fence):
    block = canary.fenced(text)
    assert block.splitlines()[0] == fence + "text"
    assert block.splitlines()[-1] == fence
    assert outside_code("before\n" + block + "after") == "before\nafter"


def test_outside_a_job_the_check_writes_no_page(canary, manifest, monkeypatch, tmp_path, capsys):
    github = GitHub(manifest)
    monkeypatch.delenv("GITHUB_STEP_SUMMARY", raising=False)
    monkeypatch.chdir(tmp_path)
    monkeypatch.setattr(canary, "load_checks_ran", lambda: github)
    monkeypatch.setattr(canary, "token", lambda: "a-token")
    monkeypatch.setattr(canary, "repository", lambda root: REPO)
    monkeypatch.setattr(canary, "job_log", github.log_of)
    assert canary.check(ROOT, None) == 0
    assert list(tmp_path.iterdir()) == []


def test_the_check_only_reads(canary, manifest, monkeypatch, tmp_path, capsys):
    github = GitHub(manifest)
    checked(canary, monkeypatch, tmp_path, capsys, github)
    # Every call went through the reader, which only asks. And the script has no call that sends.
    assert len(github.asked) >= 6
    source = SCRIPT.read_text(encoding="utf-8")
    assert not re.search(r"method\s*=|\bdata\s*=|[\"'](POST|PATCH|PUT|DELETE)[\"']", source)


def test_without_a_token_it_stops_with_a_fix(tmp_path):
    env = {"PATH": str(tmp_path), "GITHUB_REPOSITORY": "example/example"}
    done = subprocess.run([sys.executable, str(SCRIPT), "check", "--pr", "1"], capture_output=True, text=True, env=env,
                          check=False)
    assert done.returncode == 2
    assert "no GitHub token" in done.stderr
    assert "fix:" in done.stderr


def test_the_list_is_plain_json_with_the_keys_the_script_reads():
    data = json.loads((ROOT / ".github" / "canary.json").read_text(encoding="utf-8"))
    assert set(data) == {"branch", "title", "max_age_days", "plants", "not_covered", "other_checks", "side_effects"}


class _Logs(http.server.BaseHTTPRequestHandler):
    """A stand-in for GitHub: /direct gives a log, /moved points to another address, and the headers are kept."""

    seen = []

    def do_GET(self):
        self.seen.append((self.path, self.headers.get("Authorization")))
        if self.path.endswith("/actions/jobs/1/logs"):
            body = b"line one\ncanary_unused appears unused\n"
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        else:
            self.send_response(302)
            self.send_header("Location", f"http://127.0.0.1:{self.server.server_port}/elsewhere")
            self.send_header("Content-Length", "0")
            self.end_headers()

    def log_message(self, *args):
        pass


@pytest.fixture
def github():
    server = http.server.HTTPServer(("127.0.0.1", 0), _Logs)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    _Logs.seen.clear()
    yield f"http://127.0.0.1:{server.server_port}"
    server.shutdown()
    thread.join(timeout=5)


def test_the_log_of_a_job_is_read_with_the_token(canary, github):
    assert "canary_unused appears unused" in canary.job_log(github, "o/r", 1, "a-token")
    assert _Logs.seen == [("/repos/o/r/actions/jobs/1/logs", "Bearer a-token")]


def test_a_log_address_that_is_not_https_is_not_followed(canary, github):
    with pytest.raises(RuntimeError, match="did not give the log of job 2"):
        canary.job_log(github, "o/r", 2, "a-token")
    assert [path for path, _ in _Logs.seen] == ["/repos/o/r/actions/jobs/2/logs"]

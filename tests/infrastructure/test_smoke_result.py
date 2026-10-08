"""A pull request that changes the text every request carries needs the result line of a smoke run made with that text.

The check reads recordings from two commits. Each test makes a small repository with recordings made for the test.
"""
import importlib.util
import json
import os
import subprocess
import sys
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "smoke_result.py"
WORKFLOW = ROOT / ".github" / "workflows" / "smoke-result.yml"
_spec = importlib.util.spec_from_file_location("smoke_result", SCRIPT)
smoke = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(smoke)


def recording(system="You are a coding assistant. Tools: read_file, write_file.", grammar="root ::= call", schema="object",
              task="add a function", note="a note that the proxy writes in one situation"):
    """A recording as the replay tests keep it, with two requests to the model after one to another service."""
    first = {"messages": [{"role": "system", "content": system}, {"role": "user", "content": task}],
             "grammar": grammar, "response_format": {"schema": {"type": schema}}, "temperature": 0.3}
    second = {**first, "messages": first["messages"] + [{"role": "user", "content": note}]}
    return {"source": {"kind": "smoke"}, "exchanges": [
        {"service": "sandbox", "method": "GET", "path": "/health", "request": "", "response": "{}"},
        {"service": "model", "method": "POST", "path": "/v1/chat/completions", "request": json.dumps(first), "response": ""},
        {"service": "model", "method": "POST", "path": "/v1/chat/completions", "request": json.dumps(second), "response": ""}]}


class Repo:
    """A repository with recordings, and the commits that were made in it."""

    def __init__(self, root):
        self.root = root
        self.commits = 0
        (root / "tests" / "replay" / "recordings").mkdir(parents=True)
        self.git("init", "-q", "-b", "dev")

    def git(self, *args):
        return subprocess.run(["git", "-C", str(self.root), "-c", "user.name=test", "-c", "user.email=test@example.invalid", *args],
                              capture_output=True, text=True, check=True, timeout=60).stdout.strip()

    def commit(self, **recordings):
        """Write these recordings (None takes one away) and commit. Gives the commit."""
        for name, content in recordings.items():
            path = self.root / "tests" / "replay" / "recordings" / f"{name}.json"
            if content is None:
                path.unlink(missing_ok=True)
            else:
                path.write_text(content if isinstance(content, str) else json.dumps(content, indent=1), encoding="utf-8")
        # A file that differs in each commit, so that a commit with no other change can be made.
        self.commits += 1
        (self.root / "count.txt").write_text(str(self.commits), encoding="utf-8")
        self.git("add", "-A")
        self.git("commit", "-q", "-m", "a commit")
        return self.git("rev-parse", "HEAD")

    def check(self, base, head, text=""):
        return subprocess.run([sys.executable, str(SCRIPT), "--base", base, "--head", head], cwd=self.root, capture_output=True,
                              text=True, timeout=60, check=False, env={"PATH": os.environ["PATH"], "PR_BODY": text})

    def mark(self, commit):
        return subprocess.run([sys.executable, str(SCRIPT), "--mark", commit], cwd=self.root, capture_output=True, text=True,
                              timeout=60, check=True).stdout.strip()


def line(commit, mark, passed=3, seconds=412):
    return (f"Smoke run on {commit[:12]}: {passed} of 3 sessions with no harness defect, {seconds} s; "
            f"the changed text is part of every request (text {mark}).")


@pytest.fixture
def repo(tmp_path):
    made = Repo(tmp_path)
    made.base = made.commit(one=recording(), two=recording(task="repair a fault"))
    return made


NEW = "You are a coding assistant. Tools: read_file, write_file, delete_file."


# --- when the smoke run is asked for ------------------------------------------------------------------------------

def test_a_pull_request_that_changes_no_recording_is_asked_for_nothing(repo):
    head = repo.commit()
    done = repo.check(repo.base, head)
    assert done.returncode == 0, done.stdout + done.stderr
    assert "the smoke run is not asked for" in done.stdout, done.stdout + done.stderr


@pytest.mark.parametrize("change", [
    {"one": recording(note="another note in the same situation")},
    {"one": recording(task="add another function")},
    {"three": recording(task="a new case")},
    {"two": None},
    # The system prompt of one session only, for example a block about that session's project.
    {"one": recording(system=NEW)},
    {"three": recording(system=NEW, grammar="root ::= other", schema="array")},
])
def test_a_change_of_a_recording_in_text_that_not_every_request_carries_is_asked_for_nothing(repo, change):
    # The recording shows that text in its situation, and the replay job holds it.
    done = repo.check(repo.base, repo.commit(**change))
    assert done.returncode == 0, done.stdout + done.stderr
    assert "the smoke run is not asked for" in done.stdout, done.stdout + done.stderr


@pytest.mark.parametrize("change, what", [
    ({"system": NEW}, "the system prompt"),
    ({"grammar": "root ::= call | done"}, "the grammar"),
    ({"schema": "array"}, "the schema of the reply"),
    ({"system": NEW, "grammar": "root ::= call | done"}, "the system prompt and the grammar"),
])
def test_a_change_of_text_that_every_request_carries_is_red_until_the_result_line_is_there(repo, change, what):
    head = repo.commit(one=recording(**change), two=recording(task="repair a fault", **change))
    done = repo.check(repo.base, head, "A pull request with no result line.")
    assert done.returncode == 1
    assert f"::error title=smoke result::this pull request changes {what}, which every request to the model carries" in done.stdout
    assert "Fix: ask a maintainer for the smoke run of the head commit" in done.stdout
    assert "after two red smoke runs the change stops and is thought over" in done.stdout


def test_a_new_recording_beside_changed_ones_does_not_hide_the_change(repo):
    head = repo.commit(one=recording(system=NEW), two=recording(system=NEW, task="repair a fault"), three=recording(system=NEW))
    assert repo.check(repo.base, head).returncode == 1


# --- the result line ----------------------------------------------------------------------------------------------

def test_the_result_line_of_a_run_with_this_text_makes_the_check_green(repo):
    head = repo.commit(one=recording(system=NEW), two=recording(system=NEW, task="repair a fault"))
    text = "What this changes.\n\n" + line(head, repo.mark(head)) + "\n\nMore text."
    done = repo.check(repo.base, head, text)
    assert done.returncode == 0, done.stdout + done.stderr
    assert f"the smoke run on {head[:12]} (412 s, 3 of 3 sessions) was made with the text of this head" in done.stdout


def test_the_line_holds_after_a_later_commit_and_after_a_rebase_that_leave_the_text_as_it_is(repo):
    ran_on = repo.commit(one=recording(system=NEW), two=recording(system=NEW, task="repair a fault"))
    text = line(ran_on, repo.mark(ran_on))
    later = repo.commit(one=recording(system=NEW, note="a note in another wording"))
    assert repo.check(repo.base, later, text).returncode == 0
    # A rebase makes new commits: the commit of the run is no longer part of the branch.
    repo.git("checkout", "-q", "--orphan", "rebased")
    rebased = repo.commit()
    assert rebased != later
    assert repo.mark(rebased) == repo.mark(ran_on)
    assert repo.check(repo.base, rebased, text).returncode == 0


def test_the_line_no_longer_holds_when_the_text_changes_again(repo):
    ran_on = repo.commit(one=recording(system=NEW), two=recording(system=NEW, task="repair a fault"))
    text = line(ran_on, repo.mark(ran_on))
    again = NEW + " Use them with care."
    head = repo.commit(one=recording(system=again), two=recording(system=again, task="repair a fault"))
    done = repo.check(repo.base, head, text)
    assert done.returncode == 1
    assert f"is for another text (text {repo.mark(ran_on)})" in done.stdout
    assert f"has the mark {repo.mark(head)}" in done.stdout
    # A second line, of a run with the new text, makes it green; the old line may stay.
    assert repo.check(repo.base, head, text + "\n" + line(head, repo.mark(head))).returncode == 0


def no_server(mark):
    return ("No smoke run: the server is not available; the replay tests are the check, and the first nightly run after "
            f"the server is back covers this text (text {mark}).")


def test_when_the_server_is_not_available_one_line_says_so_and_the_check_is_green(repo):
    head = repo.commit(one=recording(system=NEW), two=recording(system=NEW, task="repair a fault"))
    red = repo.check(repo.base, head, "no line")
    assert red.returncode == 1, "the message gives the line to write, with the mark"
    assert no_server(repo.mark(head)) in red.stdout, "the message gives the line to write, with the mark"
    done = repo.check(repo.base, head, "Some text.\n\n" + no_server(repo.mark(head)) + "\n")
    assert done.returncode == 0, done.stdout + done.stderr
    assert "no smoke run was made for this change of the system prompt: the server was not available" in done.stdout
    assert "The recorded replay tests are the check" in done.stdout


def test_the_line_for_a_server_that_is_not_available_holds_only_for_the_text_it_names(repo):
    first = repo.commit(one=recording(system=NEW), two=recording(system=NEW, task="repair a fault"))
    text = no_server(repo.mark(first))
    again = NEW + " Use them with care."
    head = repo.commit(one=recording(system=again), two=recording(system=again, task="repair a fault"))
    assert repo.check(repo.base, head, text).returncode == 1
    assert repo.check(repo.base, head, "No smoke run: the server is not available.").returncode == 1


def test_a_red_smoke_run_is_not_set_aside_by_the_line_for_a_server_that_is_not_available(repo):
    head = repo.commit(one=recording(system=NEW), two=recording(system=NEW, task="repair a fault"))
    mark = repo.mark(head)
    done = repo.check(repo.base, head, line(head, mark, passed=1) + "\n" + no_server(mark))
    assert done.returncode == 1
    assert "had 1 of 3 sessions with no harness defect" in done.stdout


@pytest.mark.parametrize("passed", [0, 1, 2])
def test_a_smoke_run_that_was_not_three_of_three_keeps_the_check_red(repo, passed):
    head = repo.commit(one=recording(system=NEW), two=recording(system=NEW, task="repair a fault"))
    done = repo.check(repo.base, head, line(head, repo.mark(head), passed=passed))
    assert done.returncode == 1
    assert f"had {passed} of 3 sessions with no harness defect" in done.stdout
    assert "No text is changed to make the smoke pass" in done.stdout


@pytest.mark.parametrize("text", [
    "Smoke run: passed",
    "The smoke run on {commit} was 3 of 3 sessions (text {mark}).",
    "> Smoke run on {commit}: 3 of 3 sessions with no harness defect, 412 s; the changed text is part of every request (text {mark}).",
    "Smoke run on {commit}: 3 of 3 sessions with no harness defect, 412 s; the changed text is part of every request (text {mark}) and more.",
    "Smoke run on {commit}: 4 of 3 sessions with no harness defect, 412 s; the changed text is part of every request (text {mark}).",
])
def test_a_line_in_another_form_is_not_the_result_line(repo, text):
    head = repo.commit(one=recording(system=NEW), two=recording(system=NEW, task="repair a fault"))
    done = repo.check(repo.base, head, text.format(commit=head[:12], mark=repo.mark(head)))
    assert done.returncode == 1
    assert "has no result line of a smoke run" in done.stdout


def test_the_mark_is_made_from_the_text_that_every_request_carries_and_from_nothing_else(repo):
    same = repo.commit(one=recording(note="another note"), three=recording(task="a third case"))
    assert repo.mark(same) == repo.mark(repo.base)
    for change in ({"system": NEW}, {"grammar": "root ::= other"}, {"schema": "array"}):
        other = repo.commit(one=recording(**change), two=recording(task="repair a fault", **change), three=None)
        assert repo.mark(other) != repo.mark(repo.base), change
    assert len(repo.mark(repo.base)) == 12


# --- what cannot be read ------------------------------------------------------------------------------------------

@pytest.mark.parametrize("content", ["not json", '{"exchanges": [{"service": "model", "path": "/v1/chat/completions", "request": "not json"}]}'])
def test_a_recording_that_cannot_be_read_ends_the_check_with_status_2_and_nothing_is_judged(repo, content):
    done = repo.check(repo.base, repo.commit(one=content))
    assert done.returncode == 2
    assert "the recordings cannot be read, so nothing was judged" in done.stderr
    assert "one.json" in done.stderr


def test_a_commit_that_is_not_there_ends_the_check_with_status_2(repo):
    done = repo.check(repo.base, "0" * 40)
    assert done.returncode == 2
    assert "Fix: the checkout of this job needs the base commit and the head commit" in done.stderr


# --- the recordings of this repository, and the workflow ----------------------------------------------------------

def test_every_recording_of_this_repository_has_the_parts_that_the_check_reads():
    recordings = sorted((ROOT / "tests" / "replay" / "recordings").glob("*.json"))
    assert recordings
    for path in recordings:
        parts = smoke.carried_by_every_request(json.loads(path.read_text(encoding="utf-8")))
        assert set(parts) == set(smoke.PARTS), path.name
        for part in ("the system prompt", "the schema of the reply"):
            assert parts[part], (
                f"{path.name}: the first request to the model has no value for {part}, so the check would not see a "
                "change of it. Fix: PARTS and `carried_by_every_request` in scripts/smoke_result.py.")


def step(name):
    workflow = yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))
    return next(step for step in workflow["jobs"]["smoke-result"]["steps"] if name in str(step.get("run")) + str(step.get("name")))


def test_the_job_reads_the_text_of_the_pull_request_from_the_event_and_never_as_part_of_a_command():
    workflow = yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))
    judge = step("smoke_result.py")
    assert judge["env"] == {"BASE": "${{ steps.base.outputs.sha }}", "PR_BODY": "${{ github.event.pull_request.body }}"}
    for one in workflow["jobs"]["smoke-result"]["steps"]:
        assert "github.event" not in str(one.get("run")), "a value of the event stands in a command; give it through `env:`"
    # `on` is read as the boolean True by the YAML reader.
    assert "edited" in workflow[True]["pull_request"]["types"], "without `edited` the job does not see the result line come into the text"
    assert workflow["permissions"] == {"contents": "read"}
    assert "concurrency" not in workflow


def in_a_merge(tmp_path, on_the_base, on_the_branch, text):
    """Run the commands of the job's step in the merge of a branch into a base, each with its own copy of the script."""
    repo = Repo(tmp_path / "repo")
    (repo.root / "scripts").mkdir()
    if on_the_base is not None:
        (repo.root / "scripts" / "smoke_result.py").write_text(on_the_base, encoding="utf-8")
    base = repo.commit(one=recording(), two=recording(task="repair a fault"))
    repo.git("checkout", "-q", "-b", "work")
    (repo.root / "scripts" / "smoke_result.py").write_text(on_the_branch, encoding="utf-8")
    repo.commit(one=recording(system=NEW), two=recording(system=NEW, task="repair a fault"))
    repo.git("checkout", "-q", "--detach", "dev")
    repo.git("merge", "-q", "--no-ff", "--no-edit", "work")
    (tmp_path / "runner").mkdir()
    return subprocess.run(["bash", "-e", "-c", step("smoke_result.py")["run"]], cwd=repo.root, capture_output=True, text=True,
                          timeout=60, check=False, env={"PATH": f"{Path(sys.executable).parent}:/usr/bin:/bin",
                                                        "RUNNER_TEMP": str(tmp_path / "runner"), "BASE": base, "PR_BODY": text})


PASSES_ALL = 'print("the copy of the change ran")\n'


def test_the_job_runs_the_base_branchs_copy_of_the_script_when_a_change_rewrites_the_script(tmp_path):
    # The change alters the text of every request and brings a script that passes everything.
    done = in_a_merge(tmp_path, SCRIPT.read_text(encoding="utf-8"), PASSES_ALL, "no result line")
    assert done.returncode == 1, done.stdout + done.stderr
    assert "::error title=smoke result::this pull request changes the system prompt" in done.stdout
    assert "the copy of the change ran" not in done.stdout


def test_the_changes_copy_of_the_script_runs_only_while_the_base_has_none(tmp_path):
    done = in_a_merge(tmp_path, None, PASSES_ALL, "")
    assert done.returncode == 0
    assert "the copy of the change ran" in done.stdout


def test_the_gates_page_has_the_words_of_the_rule():
    page = " ".join((ROOT / "docs" / "quality" / "gates.md").read_text(encoding="utf-8").split())
    for sentence in (
        ("The smoke result says that a session still runs with the new text: it starts, the model's replies are read, the "
         "tool calls run, it ends. It is a yes or a no, with the seconds. It is not a score."),
        ("A pass does not show that the model behaves as well as before. A claim about behaviour needs a measurement of its "
         "own: two builds, enough sessions, the affected sessions shown from the diff, and the rule written down before "
         "the run."),
        ("The three tasks are fixed and are the driver's own. They are never taken from held-out data, and no product text "
         "names them."),
        "No text is changed to make the smoke pass. After two red smoke runs the change stops and is thought over.",
        ("When the server is not available, the pull request says so in one line; the recorded replay tests are the check; "
         "the first nightly run after the server is back covers it."),
        "the changed text is part of every request",
    ):
        assert sentence in page, f"docs/quality/gates.md no longer has: {sentence!r}"

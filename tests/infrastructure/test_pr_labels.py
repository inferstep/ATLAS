"""The size label and the risk label of a pull request are what the rules and the settings say."""
import importlib.util
import re
import subprocess
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, ROOT / path)
    module = importlib.util.module_from_spec(spec)
    # A module with a dataclass must be findable by its name while it is loaded.
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


rules = load("pr_labels", "scripts/bot/pr_labels.py")
bot = load("atlas_bot_for_labels", "scripts/bot/atlas_bot.py")
integrity = load("integrity_check_for_labels", "scripts/integrity_check.py")
SETTINGS = bot.load_config()["pull_requests"]
# Settings made for the tests, so that a test of a rule holds when the real numbers move.
MADE = {"sizes": {"S": 0, "M": 10, "L": 40, "XL": 100}, "high_risk_from": "L", "risk_label": "risk:high",
        "not_counted": {"go.sum": "lock files", "locks/ci.txt": "lock files", "checks/": "tests", "_check.go": "tests",
                        ".note": "documents"},
        "core_paths": {"core/loop.go": "the loop", "core/guard_*": "the guards", "flows/": "the workflows",
                       "core/gate.go": "the guards"}}


def changed(path, added=1, removed=0):
    return {"filename": path, "additions": added, "deletions": removed}


@pytest.mark.parametrize("lines, size", [(0, "S"), (9, "S"), (10, "M"), (39, "M"), (40, "L"), (99, "L"), (100, "XL"), (5000, "XL")])
def test_the_size_is_the_largest_one_whose_first_number_is_not_above_the_changed_lines(lines, size):
    assert rules.size_of(lines, MADE) == size
    assert rules.labels_for([changed("docs/a.md", lines)], False, MADE).size == f"size/{size}"


def test_added_and_removed_lines_of_every_file_count_and_the_files_a_tool_writes_do_not():
    files = [changed("a.py", 3, 4), changed("b/c.go", 1, 1), changed("go.sum", 500, 500), changed("proxy/go.sum", 70, 0),
             changed("locks/ci.txt", 0, 900), changed("locks/ci.txt.md", 2, 0), changed("picture.png", None, None)]
    assert rules.counted_lines(files, MADE) == 3 + 4 + 1 + 1 + 2
    assert rules.labels_for(files, False, MADE) == rules.Labels("size/M", 11, ())


@pytest.mark.parametrize("path", ["checks/a.py", "deep/er/checks/a/b.py", "core/loop_check.go", "a.note", "docs/b.note", "x/go.sum"])
def test_the_lines_of_a_test_a_document_and_a_lock_file_do_not_count(path):
    # An entry of the settings is the end of a path, or a folder at any depth with all that is in it.
    assert not rules.counts(path, MADE)
    assert rules.labels_for([changed(path, 5000, 5000), changed("a.py", 3)], False, MADE) == rules.Labels("size/S", 3, ())


@pytest.mark.parametrize("path", ["mychecks/a.py", "checks.py", "a/checks", "core/loop_check.go.py", "a.notes", "note", "go.sum.py",
                                  "core/check.go"])
def test_a_path_that_only_looks_like_one_that_does_not_count_counts(path):
    assert rules.counts(path, MADE)
    assert rules.counted_lines([changed(path, 4, 1)], MADE) == 5


def test_with_no_such_setting_every_file_counts():
    assert rules.counted_lines([changed("checks/a.py", 2), changed("go.sum", 3)], {**MADE, "not_counted": {}}) == 5


def test_each_label_says_what_it_is_computed_from_in_its_description():
    assert rules.descriptions(MADE) == {
        "size/S": "Under 10 changed lines, without lock files, tests and documents",
        "size/M": "10 to 39 changed lines, without lock files, tests and documents",
        "size/L": "40 to 99 changed lines, without lock files, tests and documents",
        "size/XL": "100 or more changed lines, without lock files, tests and documents",
        "risk:high": "A core path (loop, guards, workflows), 40 or more counted lines, or a first pull request"}
    plain = rules.descriptions({**MADE, "not_counted": {}, "sizes": {"S": 0, "M": 1000}, "high_risk_from": "M"})
    assert plain["size/S"] == "Under 1,000 changed lines" and plain["size/M"] == "1,000 or more changed lines"


def test_a_change_with_no_reason_is_not_high_risk():
    labels = rules.labels_for([changed("docs/a.md", 37), changed("core/loop_test.go"), changed("core/gates.go")], False, MADE)
    assert labels.lines == 39
    assert labels.reasons == ()
    assert not labels.high_risk


@pytest.mark.parametrize("path, what", [
    ("core/loop.go", "the loop"),
    ("core/guard_file.go", "the guards"),
    ("core/guard_file_test.go", "the guards"),
    ("flows/test.yml", "the workflows"),
    ("flows/deep/er/action.yml", "the workflows"),
])
def test_a_change_to_a_core_path_is_high_risk_and_says_which_part(path, what):
    labels = rules.labels_for([changed("docs/a.md"), changed(path)], False, MADE)
    assert labels.reasons == (f"it changes {what} ({path})",)
    assert labels.high_risk


@pytest.mark.parametrize("path", ["core/loop.go.md", "other/core/loop.go", "core/guards.go", "Core/loop.go", "docs/flows/x.yml", "flows",
                                  "core/gates.go", "core/gate.go.bak", "core/guard"])
def test_a_path_that_only_looks_like_a_core_path_is_not_one(path):
    assert rules.labels_for([changed(path)], False, MADE).reasons == ()


def test_each_core_part_is_named_once_with_its_first_file_and_in_the_order_of_the_settings():
    files = [changed("flows/b.yml"), changed("core/gate.go"), changed("core/guard_b.go"), changed("core/guard_a.go"),
             changed("flows/a.yml"), changed("core/loop.go")]
    assert rules.labels_for(files, False, MADE).reasons == (
        "it changes the loop (core/loop.go)", "it changes the guards (core/guard_b.go)",
        "it changes the workflows (flows/b.yml)")


@pytest.mark.parametrize("lines, high", [(39, False), (40, True), (41, True), (100, True)])
def test_a_change_of_the_size_named_in_the_settings_or_larger_is_high_risk(lines, high):
    labels = rules.labels_for([changed("docs/a.md", lines)], False, MADE)
    assert labels.reasons == ((f"it has {lines} counted lines (40 or more)",) if high else ())


def test_the_lines_that_do_not_count_for_the_size_do_not_make_a_change_high_risk():
    assert rules.labels_for([changed("go.sum", 5000), changed("a.py", 39)], False, MADE).reasons == ()


def test_the_first_pull_request_of_an_author_is_high_risk():
    assert rules.labels_for([changed("docs/a.md")], True, MADE).reasons == ("it is the author's first pull request here",)


def test_every_reason_that_holds_is_given():
    labels = rules.labels_for([changed("core/loop.go", 50)], True, MADE)
    assert labels == rules.Labels("size/L", 50, (
        "it changes the loop (core/loop.go)", "it has 50 counted lines (40 or more)",
        "it is the author's first pull request here"))


def want(size, high):
    return rules.Labels(f"size/{size}", 0, ("a reason",) if high else ())


@pytest.mark.parametrize("have, size, high, add, remove", [
    (set(), "M", False, ["size/M"], []),
    (set(), "M", True, ["risk:high", "size/M"], []),
    ({"size/M"}, "M", False, [], []),
    ({"size/M", "risk:high"}, "M", True, [], []),
    ({"size/S"}, "M", False, ["size/M"], ["size/S"]),
    ({"size/S", "size/XL", "size/M"}, "M", False, [], ["size/S", "size/XL"]),
    ({"size/M", "risk:high"}, "M", False, [], ["risk:high"]),
    ({"size/M"}, "M", True, ["risk:high"], []),
    ({"area/proxy", "status/ready", "risk:low", "sizes", "resize/M"}, "S", False, ["size/S"], []),
    ({"area/proxy", "size/L", "risk:high"}, "S", False, ["size/S"], ["risk:high", "size/L"]),
])
def test_only_the_size_labels_and_the_risk_label_are_added_and_removed(have, size, high, add, remove):
    assert rules.label_changes(have, want(size, high), MADE) == (add, remove)


# --- the settings of this repository -------------------------------------------------------------------------

def test_the_sizes_start_at_zero_and_go_up_and_the_high_risk_size_is_one_of_them():
    sizes = SETTINGS["sizes"]
    assert list(sizes) == ["S", "M", "L", "XL"]
    assert sizes["S"] == 0 and list(sizes.values()) == sorted(set(sizes.values())), sizes
    assert SETTINGS["high_risk_from"] in sizes
    assert SETTINGS["risk_label"] == "risk:high"


def test_the_high_risk_size_and_the_files_that_do_not_count_are_the_integrity_checks_own():
    assert SETTINGS["sizes"][SETTINGS["high_risk_from"]] == integrity.LARGE_CHANGE_LINES, (
        "the bot calls a pull request large from another number of lines than the integrity check does. Fix: give "
        "`sizes` in .github/atlas-bot.yml and LARGE_CHANGE_LINES in scripts/integrity_check.py the same number.")
    lock_files = sorted(entry for entry, kind in SETTINGS["not_counted"].items() if kind == "lock files")
    assert lock_files == sorted(integrity.LOCK_FILES), (
        "the bot and the integrity check take other files for lock files. Fix: give the lock files of `not_counted` "
        "in .github/atlas-bot.yml and LOCK_FILES in scripts/integrity_check.py the same files.")
    assert list(dict.fromkeys(SETTINGS["not_counted"].values())) == ["lock files", "tests", "documents"]


def test_every_core_path_of_the_settings_names_a_file_that_is_there():
    tracked = subprocess.run(["git", "ls-files"], cwd=ROOT, capture_output=True, text=True, check=True).stdout.splitlines()
    gone = [entry for entry in SETTINGS["core_paths"] if not any(rules.is_under(path, entry) for path in tracked)]
    assert not gone, (
        f"{gone} in `core_paths` of .github/atlas-bot.yml name no file of the repository, so a change to what they "
        "meant is no longer marked high risk. Fix: give each the path that the file or folder has now.")
    assert set(SETTINGS["core_paths"].values()) == {"the agent loop", "the tool handlers", "the guards"}


def test_a_change_to_a_workflow_is_not_high_risk_by_the_label_and_the_integrity_check_names_it():
    # The label means a change that can alter what the product does. A change that can alter what the checks accept
    # has its own line on the pull request, from the integrity check.
    for path in (".github/workflows/test.yml", ".github/actions/upload-coverage/action.yml"):
        assert rules.labels_for([changed(path, 30, 5)], False, SETTINGS) == rules.Labels("size/S", 35, ())
    assert ".github/workflows/" in integrity.GATE_FILES


@pytest.mark.parametrize("path", ["tests/infrastructure/test_x.py", "geometric-lens/tests/test_y.py", "extensions/vscode/test/z.test.ts",
                                  "proxy/agent_test.go", "docs/SETUP.md", "docs/images/a.svg", "README.md", "proxy/go.sum"])
def test_the_tests_the_documents_and_the_lock_files_of_this_repository_do_not_count(path):
    assert rules.labels_for([changed(path, 700, 600), changed("atlas/env.py", 5, 1)], False, SETTINGS) == rules.Labels("size/S", 6, ())


def test_every_description_of_a_label_has_at_most_the_100_characters_that_github_takes():
    # GitHub's page for a label's description: "Must be 100 characters or fewer". The label script stops at a longer one.
    script = (ROOT / "scripts" / "setup" / "labels.sh").read_text(encoding="utf-8")
    listed = re.findall(r'^    "([^"|]+)\|[0-9a-f]{6}\|([^"]*)"$', script, re.M)
    assert len(listed) >= 25, f"only {len(listed)} labels were read from scripts/setup/labels.sh; the form of its list has changed"
    computed = list(rules.descriptions(SETTINGS).items())
    too_long = {label: len(said) for label, said in listed + computed if len(said) > 100}
    assert not too_long, (
        f"these labels have a description of more than 100 characters, which GitHub refuses (label: length): {too_long}. "
        "For a size label or the risk label the text is computed from `pull_requests` in .github/atlas-bot.yml: a longer "
        "word there makes it longer. Fix: shorten the description, or the word in the settings.")


def test_the_label_script_gives_each_label_the_description_that_the_settings_give():
    script = (ROOT / "scripts" / "setup" / "labels.sh").read_text(encoding="utf-8")
    for label, said in rules.descriptions(SETTINGS).items():
        assert re.search(rf'^    "{re.escape(label)}\|[0-9a-f]{{6}}\|{re.escape(said)}"$', script, re.M), (
            f"scripts/setup/labels.sh does not give `{label}` the description \"{said}\", which is what "
            ".github/atlas-bot.yml says the label is computed from. Fix: write that text in the label's line of the "
            "script, and run the script.")


def test_a_change_to_the_agent_loop_of_this_repository_is_high_risk_and_a_small_change_to_the_docs_is_not():
    loop = rules.labels_for([changed("proxy/agent.go", 3, 1)], False, SETTINGS)
    assert loop == rules.Labels("size/S", 4, ("it changes the agent loop (proxy/agent.go)",))
    assert rules.labels_for([changed("docs/SETUP.md", 60, 20), changed("proxy/agent_test.go", 10)], False, SETTINGS) == (
        rules.Labels("size/S", 0, ()))
    lock = rules.labels_for([changed(".github/requirements/ci.txt", 900, 800), changed("pyproject.toml", 2, 2)], False, SETTINGS)
    assert lock == rules.Labels("size/S", 4, ())


def test_contributing_states_the_sizes_and_the_reasons_as_the_settings_have_them():
    guide = " ".join((ROOT / "CONTRIBUTING.md").read_text(encoding="utf-8").split())
    sizes = SETTINGS["sizes"]
    assert f"under {sizes['M']}, from {sizes['M']}, from {sizes['L']}, from {sizes['XL']:,}" in guide, (
        "CONTRIBUTING gives other sizes than `sizes` in .github/atlas-bot.yml. Fix: give both the same numbers.")
    assert f"it has {sizes[SETTINGS['high_risk_from']]} counted lines or more" in guide
    assert "The lines of lock files, tests and documents do not count" in guide
    assert "`size/S`, `size/M`, `size/L`, `size/XL`" in guide and f"`{SETTINGS['risk_label']}`" in guide
    for what in set(SETTINGS["core_paths"].values()):
        assert what in guide, f"CONTRIBUTING does not name `{what}` as a core path"


def test_the_pull_request_template_asks_about_ai_tools_and_about_every_line():
    template = (ROOT / ".github" / "PULL_REQUEST_TEMPLATE.md").read_text(encoding="utf-8")
    assert "- [ ] AI tools helped write this change" in template
    assert "- [ ] I can explain every line of this change" in template

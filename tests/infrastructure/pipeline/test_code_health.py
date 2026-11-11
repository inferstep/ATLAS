"""scripts/ci/code_health.py measures what docs/project/CODE_STYLE.md limits, and the
committed baseline matches the code."""

import ast
import importlib.util
import os
import subprocess
import sys

import pytest

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))
_spec = importlib.util.spec_from_file_location("code_health", os.path.join(ROOT, "scripts", "ci", "code_health.py"))
ch = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(ch)


def test_the_script_finds_the_repo_root():
    assert ch.ROOT == ROOT


def test_go_functions_and_methods_are_measured():
    src = "\n".join([
        "package x",
        "func plain() {",
        "\ta := 1",
        "}",
        "func (s *server) handle(w http.ResponseWriter) {",
        "\tfn := func() {",
        "\t}",
        "\t_ = fn",
        "}",
        "func (l list[T]) Len() int { return 0 }",
    ])
    assert dict(ch.go_functions(src)) == {"plain": 3, "server.handle": 5, "list.Len": 1}


def test_shell_functions_are_measured_and_a_one_line_function_counts_one():
    src = "\n".join([
        "#!/usr/bin/env bash",
        'log_ok()    { echo "ok: $*"; }',
        "main() {",
        "  log_ok start",
        "  if true; then",
        "    echo inner",
        "  fi",
        "}",
        'main "$@"',
    ])
    assert dict(ch.shell_functions(src)) == {"log_ok": 1, "main": 6}


def test_a_shell_function_of_100_lines_passes_and_one_of_101_fails(tmp_path, monkeypatch):
    # Each function is its "name() {" line, its body and its "}" line: 1 + 98 + 1 and 1 + 99 + 1 lines.
    at_limit = "at_limit() {\n" + "  echo line\n" * 98 + "}\n"
    over = "over() {\n" + "  echo line\n" * 99 + "}\n"
    (tmp_path / "scripts").mkdir()
    (tmp_path / "scripts" / "run.sh").write_text(at_limit + over)
    monkeypatch.setattr(ch, "ROOT", str(tmp_path))
    funcs, files, _ = ch.measure()
    assert ch.check(funcs, files, {}) == ["new function over 100 lines: scripts/run.sh:over (101)"]


def test_shell_kotlin_typescript_and_javascript_files_over_1500_lines_are_measured(tmp_path, monkeypatch):
    for rel in ["scripts/big.sh", "clients/extensions/jetbrains/src/main/kotlin/Big.kt",
                "clients/extensions/vscode/src/big.ts", "clients/extensions/vscode/media/big.js",
                "clients/extensions/jetbrains/src/test/kotlin/BigTest.kt"]:
        (tmp_path / rel).parent.mkdir(parents=True, exist_ok=True)
        (tmp_path / rel).write_text("x\n" * 1501)
    monkeypatch.setattr(ch, "ROOT", str(tmp_path))
    _, files, _ = ch.measure()
    assert files == {  # the Kotlin file in the test folder is left out
        "scripts/big.sh": 1501,
        "clients/extensions/jetbrains/src/main/kotlin/Big.kt": 1501,
        "clients/extensions/vscode/src/big.ts": 1501,
        "clients/extensions/vscode/media/big.js": 1501,
    }


def test_python_functions_get_qualified_names():
    src = "class A:\n    def m(self):\n        return 1\n\ndef f():\n    def inner():\n        pass\n    return inner\n"
    assert dict(ch.py_functions(src, "x.py")) == {"A.m": 2, "f": 4, "f.inner": 2}


def test_new_and_grown_items_fail_and_shrunk_ones_pass():
    base = {"functions": {"a.go:old": 150, "a.go:small": 120}, "files": {"big.go": 2000}}
    funcs = {"a.go:old": 151, "a.go:small": 110, "b.py:new": 101}
    problems = ch.check(funcs, {"big.go": 1999}, base)
    assert any("grew: a.go:old" in p for p in problems)
    assert any("new function over" in p and "b.py:new" in p for p in problems)
    assert not any("small" in p or "big.go" in p for p in problems)


def test_a_folder_over_the_soft_limit_counts_files_and_sub_folders():
    paths = [f"crowded/file{i}" for i in range(20)] + [f"crowded/sub{i}/file" for i in range(6)]
    paths += [f"fits/file{i}" for i in range(25)]
    paths += [f".github/workflows/file{i}.yml" for i in range(30)]
    assert ch.crowded_folders(paths) == {"crowded": 26}


def test_a_crowded_folder_is_a_note_and_never_a_failure(monkeypatch, capsys):
    monkeypatch.setattr(ch, "tracked_paths", lambda: [f"crowded/file{i}" for i in range(26)])
    monkeypatch.setattr(ch, "measure_complexity", lambda base, update: {"cognitive": {}, "cyclomatic": {}})
    monkeypatch.setattr(sys, "argv", ["code_health.py"])
    assert ch.main() == 0
    assert "crowded (26)" in capsys.readouterr().out


def test_no_git_gives_no_note_and_no_failure(monkeypatch):
    def no_git(*args, **kwargs):
        raise FileNotFoundError("git")

    monkeypatch.setattr(ch.subprocess, "run", no_git)
    assert ch.tracked_paths() == []


def test_the_committed_baseline_matches_the_code():
    funcs, files, _ = ch.measure()
    assert ch.check(funcs, files, ch.load_baseline()) == [], (
        "a function or file is new over the limit or grew: split it (docs/project/CODE_STYLE.md)")


def test_the_main_package_may_lose_files_but_not_gain_one():
    base = {"proxy_main": {"files": 90}}
    assert ch.main_package_problems({"files": 90, "lines": 40000}, base) == []
    assert ch.main_package_problems({"files": 89, "lines": 39000}, base) == []
    grew = ch.main_package_problems({"files": 91, "lines": 40000}, base)
    assert any("files 90 -> 91" in p for p in grew)


def test_a_baseline_without_the_pin_fails():
    for base in ({"proxy_main": {}}, {"functions": {}, "files": {}}):
        problems = ch.main_package_problems({"files": 90, "lines": 40000}, base)
        assert any("no pinned file count" in p for p in problems), base


def test_more_lines_in_the_main_package_are_no_failure():
    base = {"proxy_main": {"files": 90}}
    assert ch.main_package_problems({"files": 90, "lines": 50000}, base) == []


def test_the_main_package_counts_only_its_own_code_files(tmp_path, monkeypatch):
    (tmp_path / "services" / "proxy" / "internal" / "x").mkdir(parents=True)
    (tmp_path / "services" / "proxy" / "a.go").write_text("package main\n\nfunc a() {}\n")
    (tmp_path / "services" / "proxy" / "a_test.go").write_text("package main\n")
    (tmp_path / "services" / "proxy" / "internal" / "x" / "x.go").write_text("package x\n")
    monkeypatch.setattr(ch, "ROOT", str(tmp_path))
    assert ch.main_package_size() == {"files": 1, "lines": 3}


def test_the_main_package_is_within_its_pin():
    assert ch.main_package_problems(ch.main_package_size(), ch.load_baseline()) == [], (
        "proxy's main package grew past its pin: put the new subject in its own package")


# Two functions whose cognitive complexity SonarQube's rule S3776 counts by hand:
# for +1, if +2 (nested once), and +1, else +1; while +1, except +2 (nested once),
# if +3 (nested twice), or +1. The copied measure must give the same.
@pytest.mark.parametrize("src, sonar", [
    ("def first(rows):\n"
     "    total = 0\n"
     "    for row in rows:\n"
     "        if row.ok and row.size > 0:\n"
     "            total += row.size\n"
     "        else:\n"
     "            total -= 1\n"
     "    return total\n", 5),
    ("def second(path, retries):\n"
     "    while retries > 0:\n"
     "        try:\n"
     "            return fetch(path)\n"
     "        except OSError:\n"
     "            retries -= 1\n"
     "            if retries == 0 or not path:\n"
     "                raise\n"
     "    return None\n", 7),
])
def test_the_copied_cognitive_measure_counts_like_sonar(src, sonar):
    get_cognitive_complexity = ch.load_cognitive_measure()
    assert get_cognitive_complexity(ast.parse(src).body[0]) == sonar
    assert ch.HERE not in sys.path


def test_a_go_tool_line_names_the_function_as_the_size_check_does(monkeypatch):
    printed = ("42 app (*tuiModel).appendChatEvent clients/tui/internal/app/events.go:26:1\n"
               "17 app formatV3StageEvent clients/tui/internal/app/events.go:997:1\n")
    monkeypatch.setattr(ch, "installed_go_tool", lambda tool: "/home/runner/go/bin/gocyclo")
    monkeypatch.setattr(ch.subprocess, "run",
                        lambda *args, **kwargs: subprocess.CompletedProcess(args, 1, printed, ""))
    assert ch.go_scores(ch.GOCYCLO, ["clients/tui/internal/app/events.go"]) == {
        "clients/tui/internal/app/events.go:tuiModel.appendChatEvent": 42,
        "clients/tui/internal/app/events.go:formatV3StageEvent": 17,
    }


def test_a_go_tool_that_did_not_run_fails_rather_than_listing_nothing(monkeypatch):
    monkeypatch.setattr(ch, "installed_go_tool", lambda tool: "/home/runner/go/bin/gocognit")
    monkeypatch.setattr(ch.subprocess, "run", lambda *args, **kwargs: subprocess.CompletedProcess(
        args, 1, "", "gocognit: open services/proxy/main.go: no such file or directory\n"))
    with pytest.raises(ch.CannotMeasure, match="did not run"):
        ch.go_scores(ch.GOCOGNIT, ["services/proxy/main.go"])


def test_in_ci_a_machine_without_go_fails_and_names_the_install(monkeypatch, capsys):
    monkeypatch.setenv("GITHUB_ACTIONS", "true")
    monkeypatch.setattr(ch.shutil, "which", lambda name: None)
    monkeypatch.setattr(sys, "argv", ["code_health.py"])
    assert ch.main() == 1
    assert "FAIL Go is not on the PATH: install Go (https://go.dev/dl/)" in capsys.readouterr().out


def test_a_local_run_without_go_skips_go_says_so_and_still_checks_python(monkeypatch, capsys):
    monkeypatch.delenv("GITHUB_ACTIONS", raising=False)
    monkeypatch.setattr(ch.shutil, "which", lambda name: None)
    monkeypatch.setattr(ch, "python_cognitive", lambda paths: {"scripts/new.py:tangled": 16})
    monkeypatch.setattr(sys, "argv", ["code_health.py"])
    assert ch.main() == 1
    out = capsys.readouterr().out
    assert "skip Go's complexity on this machine (Go is not on the PATH" in out
    fails = [line for line in out.splitlines() if line.startswith("FAIL")]
    assert fails == ["FAIL new function over cognitive complexity 15: scripts/new.py:tangled (16)"]


def test_writing_the_baseline_without_go_fails_and_leaves_the_file(monkeypatch, tmp_path):
    baseline = tmp_path / "code-health-baseline.json"
    baseline.write_text("{}\n")
    monkeypatch.delenv("GITHUB_ACTIONS", raising=False)
    monkeypatch.setattr(ch, "BASELINE", str(baseline))
    monkeypatch.setattr(ch.shutil, "which", lambda name: None)
    monkeypatch.setattr(sys, "argv", ["code_health.py", "--update"])
    assert ch.main() == 1
    assert baseline.read_text() == "{}\n"


def test_ci_installs_the_go_tools_at_the_versions_the_check_pins():
    with open(os.path.join(ROOT, ".github", "workflows", "test.yml"), encoding="utf-8") as fh:
        workflow = fh.read()
    for tool in (ch.GOCOGNIT, ch.GOCYCLO):
        assert ch.install_command(tool) in workflow


def test_tests_are_measured_for_complexity_in_go_and_python_and_the_two_held_files_are_not(monkeypatch):
    measured = []

    def record(tool, paths):
        measured.extend(paths)
        return {}

    def record_python(paths):
        measured.extend(paths)
        return {}

    monkeypatch.setattr(ch, "go_scores", record)
    monkeypatch.setattr(ch, "missing_go_tools", lambda: [])
    monkeypatch.setattr(ch, "python_cognitive", record_python)
    ch.measure_complexity({}, update=False)
    assert "services/proxy/main_test.go" in measured
    assert "tests/infrastructure/pipeline/test_code_health.py" in measured
    assert "tests/replay/recording.py" in measured  # a helper in the test folder
    assert "services/geometric-lens/tests/test_asa_prompt_templates.py" in measured
    assert "services/proxy/tool_args.go" not in measured
    assert "services/proxy/tool_args_test.go" not in measured


def test_sizes_leave_test_files_and_test_folders_out(tmp_path, monkeypatch):
    for rel in ["scripts/tool.py", "scripts/test_tool.py", "scripts/tests/helpers.py"]:
        (tmp_path / rel).parent.mkdir(parents=True, exist_ok=True)
        (tmp_path / rel).write_text("x = 1\n")
    monkeypatch.setattr(ch, "ROOT", str(tmp_path))
    assert list(ch.source_files()) == ["scripts/tool.py"]


def test_a_new_or_risen_complexity_fails_and_a_fallen_one_passes():
    base = {"cognitive": {"a.go:rises": 20, "a.go:falls": 30}, "cyclomatic": {"b.go:stays": 18}}
    scores = {"cognitive": {"a.go:rises": 21, "a.go:falls": 25, "c.py:new": 16}, "cyclomatic": {"b.go:stays": 18}}
    assert ch.complexity_problems(scores, base) == [
        "cognitive complexity grew: a.go:rises (20 -> 21); the baseline allows no growth",
        "new function over cognitive complexity 15: c.py:new (16)",
    ]

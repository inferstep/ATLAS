# Quality gates

What runs on every change, which checks can stop a merge, how the outside
tools are set, and where the numbers stood when this page was last measured.

A number on this page is a starting point, not a target. When a setting of a
tool changes, change this page too, so that a difference between the page and
the tool is a mistake and not a mystery.

## How a change reaches `dev`

- Every change is a pull request. A direct push to `dev`, `staging` or `main`
  is refused for every account, and no account can skip a required check.
  The one exception is the
  [release step](../RELEASE.md#the-release-step), which moves `main`
  forward and merges it back into `dev`, each time with the owner's
  approval.
- A pull request merges through the merge queue, which runs the required
  checks again on the merged result and then squashes it into one commit.
- `main` and `staging` require the same checks as `dev`, except
  `code health (size)`, whose job is not on `main` yet.
- On a pull request from a fork, GitHub holds every run until a maintainer
  approves it ("Approve and run"). Until then the checks have no result, and
  nothing is wrong with the change; the contributor has nothing to do. If
  `checks ran` runs while a run still waits, it says "waiting for a
  maintainer's approval", names the workflow and gives no verdict: it does
  not pass, and it does not call the waiting jobs missing. A maintainer
  approves the runs and starts it again.

## Checks on a pull request

### Required (24)

| Check | What it does |
|---|---|
| `go test (proxy)`, `go test (tui)` | The Go suites, with the race detector. A gate that ran no test fails |
| `pytest (tests/v3)`, `pytest (tests/v3-service)`, `pytest (tests/cli)`, `pytest (tests/contracts)`, `pytest (tests/infrastructure)`, `pytest (geometric-lens/tests)` | The Python suites |
| `e2e acceptance (proxy + sandbox + fake llama)` | The real proxy and sandbox against a stand-in model server |
| `ruff (python lint)`, `shellcheck`, `yamllint (workflows)` | Lint for Python, shell and workflow files |
| `docker compose config` | Every compose file and overlay parses |
| `code health (size)` | No new function over 100 lines or file over 1,500, and nothing on the size baseline grows |
| `llama.cpp patches apply to pinned SHA` | The patches still apply to the pinned upstream commit |
| `bootstrap on debian-12`, `bootstrap on rockylinux-9`, `bootstrap on ubuntu-22.04`, `bootstrap on ubuntu-24.04`, `bootstrap via sudo for a regular user` | The installer's dry run on each system |
| `codeql (go)`, `codeql (python)` | CodeQL analysis ran to its end |
| `dependency review` | No new dependency with a known advisory |
| `pr title` | The title is a conventional commit; it becomes the commit on `dev` |

### Not required (they report, and a red one is a reason to look)

| Check | What it does |
|---|---|
| `checks ran` | Fails when a workflow did not start, a job with no condition was skipped or cancelled, or a required check was skipped |
| `replay (proxy)` | The proxy, built from the change, does on each recorded session what the recording says. Runs when `proxy/` or `tests/replay/` changed, and in the merge queue |
| `integrity check` | Reads the change for weakened checks: removed or skipped tests, new suppressions, changes to the files that configure checks, new documents |
| `golangci-lint (proxy)`, `golangci-lint (tui)` | Go lint on the code a change adds |
| `hadolint (dockerfiles)` | Lint of every Dockerfile. A finding does not fail it; it fails when it could not lint |
| `sonar scan`, `SonarCloud Code Analysis` | The SonarQube Cloud analysis, and Sonar's verdict on the new code |
| `coverage upload`, `coverage upload (extension)`, `codecov/patch`, `codecov/project` | Coverage reports sent to Codecov, and Codecov's two statuses |
| `pytest (tests/perf)`, `pytest (tests/concurrency)`, `perf budget gate` | Performance and concurrency suites |
| the four `sandbox smoke` jobs | The sandbox image runs Java, Kotlin, PHP and Ruby |
| `codeql (javascript-typescript)`, `lint + test + build` | Analysis and build of the VS Code extension |
| the `PR build check` jobs | Each service image builds |

## Tool settings

| Tool | Where the setting is | Setting |
|---|---|---|
| Size check | `scripts/code_health.py`, `.github/code-health-baseline.json` | 100 lines per function, 1,500 per file. Entries on the baseline may shrink and may not grow |
| Go lint | `.golangci.yml` | errcheck, nilerr, unused, gocognit (15), forbidigo for `os.Getenv`, nolintlint. Judges new code only |
| Extension lint | `extensions/vscode/eslint.config.mjs` | 100 lines per function, and 15 decision points per function, where a switch counts once. A file that holds a larger function is listed with that function's size, and the limit holds for the whole file. A listed number may go down and may not go up |
| Coverage | `pyproject.toml` (`[tool.coverage.run]`), `scripts/production-readiness.py` | Go statement coverage per module; Python line coverage over `atlas`, `v3-service`, `geometric-lens`, `sandbox`, `scripts`; TypeScript line coverage of the extension |
| Codecov | `codecov.yml`; default branch set to `dev` on Codecov | No comment on pull requests, no notes on diff lines. Two statuses that show numbers and always pass. One flag per report (`go-proxy`, `go-tui`, `python`, `typescript`); a flag that a commit does not upload keeps its parent's result |
| SonarQube Cloud | `sonar-project.properties`, `.github/workflows/sonar.yml`; rule sets and gate on SonarQube Cloud | Analysis runs from CI on `dev`, `main` and pull requests. Test code is named as test code. Coverage is left out. Rule sets: "Sonar way comprehensive", with `pythonsecurity:S8705` and `pythonsecurity:S8707` off for Python (488 of 490 rules active) and `githubactions:S8545` and `githubactions:S8541` off for workflows (32 of 34); each raised mostly false alarms on this repository's own scripts and workflows. `pythonsecurity:S8707` reported 71 findings, all in the `atlas` command-line tool, in `scripts/` and in one calibration tool, where the path comes from the person who runs the command |
| SonarQube Cloud, gate | on SonarQube Cloud | The built-in "Sonar way" gate, on new code: reliability, security and maintainability rating A, duplicated lines at most 3%, security hotspots reviewed 100%. Its coverage condition has nothing to judge, because coverage is left out of the analysis. The gate reports and is not a required check |
| CodeScene | on CodeScene | Analyses `dev` |
| Dockerfile lint | `.github/workflows/hadolint.yml`, `scripts/dockerfile_lint.py` | hadolint v2.15.1, its release binary held against the checksum the workflow records. Dependabot does not update this version; a maintainer moves it. Every rule is on. The findings in a Dockerfile a change touches are annotations; all counts are in the job summary |
| Image scan | `.github/workflows/container-scan.yml` | Trivy, weekly and by hand, on the six images CI publishes with the tag `dev`: proxy, v3, lens, sandbox, llama (CUDA) and llama-vulkan. Critical and high findings that have a fix. It reports and does not fail. The ROCm image is built on the user's machine and is not scanned |

## Baselines on `dev`

Measured 2026-10-06 on commit `a413df4`, unless a row says otherwise.

| Measure | Value |
|---|---|
| Coverage, Go proxy | 86.7% of statements (12,187 of 14,052) |
| Coverage, Go tui | 51.7% of statements (1,466 of 2,837) |
| Coverage, Python, the 8 suites together | 59.8% of lines (12,553 of 20,996). By folder: `v3-service` 84.0%, `sandbox` 68.1%, `geometric-lens` 67.2%, `atlas` 49.5%, `scripts` 30.0% |
| Coverage, TypeScript (VS Code extension) | 51.5% of lines (943 of 1,831), from the extension's last run |
| Coverage on Codecov | 67.91% of lines, all flags together (30,948 of 45,567). `go-proxy` 83.07%, `go-tui` 47.13%, `python` 59.78%, `typescript` 50.3% |
| Size baseline | 88 functions over 100 lines (longest: `runAgentLoop`, 2,112); 11 files over 1,500 lines (longest: `proxy/agent.go`, 9,929) |
| Extension size rules, on commit `f65e01b` | 2 files listed: `src/ui/chatView.ts` for `dispatch` (188 lines, 33 decision points) and `src/session/editPreview.ts` for `predictEdit` (17 decision points). No other function is over a limit; the next longest has 94 lines |
| Go lint, all code, with the repository's settings | proxy 220 (gocognit 121, errcheck 52, forbidigo 23, nilerr 22, gofmt 2); tui 48 (gocognit 21, errcheck 16, forbidigo 9, nilerr 2) |
| SonarQube Cloud, 2026-10-06 on commit `be88079` | 72,603 lines of code in 277 files, test code not counted. As the tool reports them, not as confirmed defects: 133 security findings, 7 bugs, 1,384 code smells. Duplication 0.9%. Cognitive complexity 19,913. The gate passes. The security count is 71 lower than at the measurement before, and no fix is behind that: the 71 findings of `pythonsecurity:S8707` left the count when the rule was switched off |
| CodeScene, first analysis of `dev`, 2026-10-05 | 165,035 lines of code. Hotspot code health 2.8, average code health 6.6 (scale 1 to 10) |
| Dockerfile lint, on commit `f65e01b` | 37 findings in 8 Dockerfiles: no error, 32 warnings, 5 notes. By rule: DL3008 12, DL3003 11, DL4006 5, DL3066 4, DL3041 2, DL3016 1, SC2046 1, SC2086 1 |

How to read the coverage rows:

- In: `go test` for the proxy and the tui, the eight pytest jobs, and the
  extension's tests. Out: the end-to-end job, the sandbox smoke jobs, the
  installer tests and the performance gate. So the proxy number is unit-test
  coverage only.
- The Python number merges the eight suites by file and line: a line counts
  when any suite ran it.
- Codecov counts lines and counts a partly covered line as not covered.
  `go tool cover` counts statements. The two Go numbers differ for that
  reason alone. For Python the two agree exactly.

## Keeping the baselines honest

- The size and lint counts may go down and may not go up. When something on
  the size baseline shrinks, run `python scripts/code_health.py --update` and
  commit the baseline with the change.
- When a listed function of the extension shrinks, lower its number in
  `extensions/vscode/eslint.config.mjs`. The extension's test
  `test/sizeRules.test.ts` fails while a listed number is larger than the
  file needs.
- Measure the table again when a group of changes has landed, and write the
  date and the commit above it.
- A check that raises mostly false alarms is switched off or changed, and the
  change is written in the tool settings table with its reason.

## The replay tests

The proxy is tested from outside on recorded sessions (`tests/replay`). It
runs as a binary built from the change. Its four services (the model, the
sandbox, V3 and the lens) are stand-ins that play a recording. Each request
the proxy sends to one of them is compared, whole, with the recorded request,
in the order of that service. The events the proxy sends to the client and
the files it leaves are compared too.

- A recording is one JSON file in `tests/replay/recordings/`. It says where
  it came from: the task, the set the task belongs to (e2e, smoke or
  development, and no other), the commit, and whether the model replies were
  written by hand or captured from a model.
- Left out of the comparison, by name, are the fields of an event that
  differ from run to run: six times and one estimate that depends on how
  long the path of the workspace is (`VARIES_IN_EVENTS` in
  `tests/replay/recording.py`). A field that is added later is compared.
- A test here does not know how the proxy is built inside, so a change that
  only moves code does not touch it. A change in what the proxy sends, says
  or writes on a recorded session fails it, at the first place the run
  differs.
- A green replay means the same behaviour on these recorded sessions. It
  does not mean correct. It cannot show how a real model reacts to a changed
  message, because the replies are fixed.
- A recording is an expected value. When a change is meant to alter what the
  proxy does on a recorded session (a text the model reads, the order of two
  calls), the recording is made again in the same pull request
  (`python -m tests.replay.record <case>`), the pull request says which
  behaviour changed and why, and a maintainer approves it.

Run them with `python -m pytest tests/replay`. The tests build the proxy
themselves and need Go.

## The canary

A check that silently stops checking looks the same as a check that passes.
The canary shows the difference. It is one draft pull request that is never
merged. Its branch is a copy of `dev` plus one harmless violation for each
check: a failing test in each test job, a lint error, a function that is too
long, a line that stops the installer. Every listed check must be red on it.

- The list is `.github/canary.json`: each violation, the checks it must turn
  red, and the required checks the canary does not cover, with the reason.
- `python3 scripts/canary.py check --pr <number>` reads the checks of the
  canary pull request and names each thing that is not as the list says: a
  listed check that passed, did not run, was skipped or has not finished, a
  required check the list does not know, and a canary older than 14 days.
  It needs the packages of `.github/requirements/ci.txt` and a GitHub token
  (`GITHUB_TOKEN`, or a `gh` sign-in). It changes nothing.

A maintainer renews the canary once a week, and after a change to a workflow
or to a file that configures a check:

```bash
git fetch origin
git checkout -B canary/must-stay-red origin/dev
python3 scripts/canary.py plant
git commit -m "canary: planted violations, never merge"
git push --force-with-lease origin canary/must-stay-red
```

When the checks of the canary pull request have ended, run the check. A check
that it names has stopped catching its violation: repair the check, not the
list. Change the list only when a job is renamed, a required check is added,
or a file that a violation edits has moved.

Not covered: `codeql (go)` and `codeql (python)` report that the analysis
ran, and a finding does not turn them red. `dependency review` turns red only
for a dependency with a published advisory, and none is planted.

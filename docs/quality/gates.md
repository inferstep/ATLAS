# Quality gates

What runs on every change, which checks can stop a merge, how the outside
tools are set, and where the numbers stood when this page was last measured.

A number on this page is a starting point, not a target. When a setting of a
tool changes, change this page too, so that a difference between the page and
the tool is a mistake and not a mystery.

The rules that reviews and failures have taught us are in one section of
this page: [Rules learned from review and from failures](#rules-learned-from-review-and-from-failures).

## How a change reaches `dev`

- Every change is a pull request. A direct push to `dev`, `staging` or `main`
  is refused for every account, and no account can skip a required check.
  The one exception is the
  [release step](../RELEASE.md#the-release-step), which moves `main`
  forward and merges it back into `dev`, each time with the owner's
  approval.
- A pull request merges through the merge queue, which runs the required
  checks again on the merged result and then squashes it into one commit.
- After two red CI rounds on the same pull request, stop and rethink the
  change before a third push. A third round that only tries again costs the
  runners of every other pull request, and two reds say that the cause is
  not yet known.
- The push of that commit to `dev` runs no test again. The workflow
  `dev results` takes the coverage reports and the test results that the
  queue's run of the commit kept, and sends them for the commit. It also
  fills the Go build cache that pull request jobs read. It waits until the
  queue's run has ended: the queue merges when the required checks passed,
  and other jobs of the run may go on.
- The image workflow moves the `dev` tags only when the `tests` run of the
  commit ended as a success. On `dev` that run is the queue's. When a job
  that is not a required check failed in it, the commit merged all the same,
  and the tags do not move: a maintainer reads the failed job, runs it again
  when its fault is not in the change, and then runs the job
  `promote moving tags` again.
- A commit that reaches `dev` without the queue, as the release step's
  merge-back does, has no such run. `dev results` fails for it and says
  "this commit did not come through the merge queue; start `tests` for it by
  hand". Started by hand on `dev`, the `tests` workflow does what a push run
  does: the tests, both uploads and the cache.
- `main` and `staging` require the same checks as `dev`, except
  `code health (size)`, whose job is not on `main` yet.
- On a pull request from a fork, GitHub holds every run until a maintainer
  approves it ("Approve and run"). Until then the checks have no result, and
  nothing is wrong with the change; the contributor has nothing to do. If
  `checks ran` runs while a run still waits, it says "waiting for a
  maintainer's approval", names the workflow and gives no verdict: it does
  not pass, and it does not call the waiting jobs missing. A maintainer
  approves the runs and starts it again.

### What `checks ran` judges

`checks ran` looks at the jobs of each workflow that had to start.

- A job with no condition must have run. That holds for every job definition
  in the workflow files that has no `if:` of its own.
- 16 job definitions have an `if:` of their own, so they may be skipped by
  design, and `checks ran` does not report one of them as missing. Every
  other job is judged for absence. The table below names the 16. On a pull
  request five of them count: Sonar's scan, the `PR build check`, `coverage upload`,
  `test results upload` and `test results upload (extension)`. If one of
  these does not start at all, nothing says so. The rest run only on other
  events: the four bot jobs, the three image jobs of a push, the release
  file job, the scorecard, the staging record and the star chart.
- For a job with a matrix, one reported leg is enough. The required checks
  cover the required legs by name. A leg that is not required can be missing
  unseen.
- A job that was skipped because a job it needs failed repeats that failure.
  It is a note that names the failed job, and no finding. One case stays a
  finding: a required check that was skipped behind a failed job that is not
  required. The rules count a skipped required check as passed, and a job
  that is not required holds no merge.
- A job whose name is an expression alone is known only by the name GitHub
  gives it when it is skipped.

The job definitions with an `if:` of their own, by workflow file and job id. A
job on this list that does not start gives no red:

| Workflow file | Jobs with a condition |
|---|---|
| `bot-claim.yml` | `claim` |
| `bot-new-issue.yml` | `new-issue` |
| `bot-stale-claims.yml` | `stale` |
| `bot-sync.yml` | `sync` |
| `build-images.yml` | `pr-build`, `alias`, `build`, `promote` |
| `release-files.yml` | `sign` |
| `scorecard.yml` | `analysis` |
| `sonar.yml` | `scan` |
| `staging-promotion.yml` | `record` |
| `star-chart.yml` | `render` |
| `test.yml` | `coverage-upload`, `test-results-upload` |
| `vscode-extension.yml` | `test-results-upload` |

## Checks on a pull request

### Required (24)

| Check | What it does |
|---|---|
| `go test (proxy)`, `go test (tui)` | The Go suites, with the race detector. A gate that ran no test fails, and so does one that took a result from Go's test cache. The jobs keep a module and build cache; a run in the merge queue takes no part in it |
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
| `fix tests (fail without the fix)` | For a pull request with the type `fix`: runs the tests it adds or changes on the base's code and on the pull request, and names each test that passes without the fix |
| `integrity check` | Reads the change for weakened checks: removed or skipped tests, new suppressions, changes to the files that configure checks, new documents |
| `zizmor (workflows)`, `actionlint (workflows)` | The workflow files themselves: security mistakes and mistakes GitHub shows only at run time. A finding fails the job |
| `golangci-lint (proxy)`, `golangci-lint (tui)` | Go lint on the code a change adds |
| `hadolint (dockerfiles)` | Lint of every Dockerfile. A finding does not fail it; it fails when it could not lint |
| `sonar scan`, `SonarCloud Code Analysis` | The SonarQube Cloud analysis, and Sonar's verdict on the new code |
| `dev results lookup (sends nothing)` | On a pull request that changes the `dev results` workflow, the upload actions or the lookup script: the lookup and the download for the newest commit of `dev` that came through the merge queue. It sends nothing |
| `coverage upload`, `coverage upload (extension)`, `codecov/patch`, `codecov/project` | Coverage reports sent to Codecov, and Codecov's two statuses. `coverage upload` and `test results upload` have the head commit of a pull request in their workspace, because Codecov files a report under that commit. They take the upload action from a second checkout, the commit that the workflow file comes from, in the folder `workflow-commit`: a branch that left `dev` before an action was added does not have it. A test holds this for every job that uses an action of this repository |
| `test results upload`, `test results upload (extension)` | The result of each test sent to Codecov, also when a test job failed. A refused upload turns only this job red. When a job stopped before its tests ran there is no file of results; the upload job then says so and is not red for it |
| `pytest (tests/perf)`, `pytest (tests/concurrency)`, `perf budget gate` | Performance and concurrency suites |
| the four `sandbox smoke` jobs | The sandbox image runs Java, Kotlin, PHP and Ruby |
| `codeql (javascript-typescript)`, `lint + test + build` | Analysis and build of the VS Code extension |
| the `PR build check` jobs | Each service image builds |

A check that compares a change with its base compares with the base branch
as it is now: the first parent of the merge commit that the job checks out.
`scripts/change_base.py` gives that commit to the integrity check, `checks
ran`, golangci-lint and hadolint, and each takes its script from there. The
base commit that the event names is the branch as it was when the event was
made; compared with that one, everything the base branch got since would read
as part of the pull request. When the checkout is not that merge, the step
stops and says so. It takes no other base in its place. The base branch's
copy of the script gives the answer, so a change cannot choose its own base.
The step also stops when the parent commit is not in the checkout, as in a
checkout of one commit: without the parent it cannot read the base branch's
copy, and it does not run the change's copy in its place.

`fix tests` gives one line for the fix as a whole, the strongest that holds:

| The line | What it means |
|---|---|
| Guarded | At least one test fails on the base and passes with the fix |
| Not shown | Tests of the fix ran on the base and none failed. This is a finding about the fix |
| Cannot tell | No test could run on the base: each needs code or a file the fix adds. This is the limit of the check, not a finding about the fix |
| No test | The fix adds or changes no test |

Below it, each new or changed test is in one of three classes that are not
added together: "fails on the base" (it fails there for what the code does),
"needs the fix's code" (on the base it cannot run as written: it does not
build or import, names something that is not there yet, or calls a function
in a way the base's function does not take), and "passes on the base too".

- A test the fix adds that passes on the base too is a finding when no test
  of the fix fails on the base. Beside a test that does, it is most likely a
  control that holds what the fix must not change, and it is listed for
  information. For a test the fix only changes it is information.
- A test that fails on the base and with the fix reads "not judged here",
  with the first line of its failure. The cause is often the place it runs
  in: a test that needs docker gives another result where there is none.
- A failure says where it was raised: in the test, in product code, or in
  the standard library or an installed package, with the file of the
  repository that called it.

Its reach, measured on 16 fixes that are merged, each against its parent,
with the script of commit `c461b30`: guarded 10, cannot tell 4, not
shown 1, no test 1. Of the 6 proxy fixes among them 3 are guarded and 3 read
"cannot tell", by the Go limit below.

It cannot judge these, and says so when it meets one:
- Go builds the tests of a package together. When a changed test file uses
  code the fix adds, no test of the package builds on the base. Each changed
  test file is then tried alone (up to six); a file that still does not
  build reads "needs the fix's code" for all its tests.
- A TypeScript or JavaScript test file, and a test under `geometric-lens/`,
  whose packages the job does not install.
- A test that is skipped where the job runs, and a run that does not end in
  its time limit.
- A fix whose guard is a changed recording or data file and no test
  function. A fix with no new or changed test gets its own line.

## Tool settings

| Tool | Where the setting is | Setting |
|---|---|---|
| Size check | `scripts/code_health.py`, `.github/code-health-baseline.json` | 100 lines per function, 1,500 per file. Entries on the baseline may shrink and may not grow |
| Workflow lint | `.github/workflows/workflow-lint.yml` | zizmor v1.30.1 at its default level, with lookups on GitHub by the job's read-only token; actionlint 1.7.12 without its shellcheck and pyflakes passes, which would use whatever version the runner has. Both are release binaries held against the checksums the workflow records. A step that must stay as it is carries `# zizmor: ignore[<rule>]` with its reason. Five lines carry one today: the checkout of `star-chart.yml`, whose job pushes with the token; and the four lines of `test.yml` and `dev-results.yml` that use the two upload actions of this repository (`ignore[self-repository]`). zizmor asks for the form `$/...` on those four, and actionlint 1.7.12, its newest release, does not read that form yet (its issues 711 and 732). When actionlint reads it, the four lines take the new form and their marks go |
| Go lint | `.golangci.yml` | errcheck, nilerr, unused, gocognit (15), forbidigo for `os.Getenv`, nolintlint. Judges new code only |
| Extension lint | `extensions/vscode/eslint.config.mjs` | 100 lines per function, and 15 decision points per function, where a switch counts once. A file that holds a larger function is listed with that function's size, and the limit holds for the whole file. A listed number may go down and may not go up |
| Coverage | `pyproject.toml` (`[tool.coverage.run]`), `scripts/production-readiness.py` | Go statement coverage per module; Python line coverage over `atlas`, `v3-service`, `geometric-lens`, `sandbox`, `scripts`; TypeScript line coverage of the extension |
| Codecov | `codecov.yml`; default branch set to `dev` on Codecov | No comment on pull requests, no notes on diff lines. Two statuses that show numbers and always pass. One flag per report (`go-proxy`, `go-tui`, `python`, `typescript`); a flag that a commit does not upload keeps its parent's result. The result of each test goes there too, under the same four flags: the Go and pytest gates and the extension's tests write JUnit files in CI. The report is the "Tests" tab of the repository on Codecov (https://app.codecov.io/gh/inferstep/ATLAS/tests); it names failed tests and tests that pass only sometimes, and it blocks nothing |
| SonarQube Cloud | `sonar-project.properties`, `.github/workflows/sonar.yml`; rule sets and gate on SonarQube Cloud | Analysis runs from CI on `dev`, `main` and pull requests. Test code is named as test code. Coverage is left out. Rule sets: "Sonar way comprehensive", with `pythonsecurity:S8705` and `pythonsecurity:S8707` off for Python (488 of 490 rules active) and `githubactions:S8545` and `githubactions:S8541` off for workflows (32 of 34); each raised mostly false alarms on this repository's own scripts and workflows. `pythonsecurity:S8707` reported 71 findings, all in the `atlas` command-line tool, in `scripts/` and in one calibration tool, where the path comes from the person who runs the command |
| SonarQube Cloud, gate | on SonarQube Cloud | The built-in "Sonar way" gate, on new code: reliability, security and maintainability rating A, duplicated lines at most 3%, security hotspots reviewed 100%. Its coverage condition has nothing to judge, because coverage is left out of the analysis. The gate reports and is not a required check |
| CodeScene | on CodeScene | Analyses `dev` |
| Dockerfile lint | `.github/workflows/hadolint.yml`, `scripts/dockerfile_lint.py` | hadolint v2.15.1, its release binary held against the checksum the workflow records. Dependabot does not update this version; a maintainer moves it. Every rule is on. The findings in a Dockerfile a change touches are annotations; all counts are in the job summary |
| Hashed installs | `.github/requirements/ci.in`, `ci.txt`, `scripts/ci-lock.sh` | The pytest jobs, the lint jobs and the e2e job install from one lock that has a hash for every file, with `pip install --require-hashes`: pip refuses a file whose hash is not in the lock. The lock holds the tools and the packages of the product that the tests need. It is made by pip-compile under Python 3.12, and its header says so; Dependabot reads the header and makes the lock again the same way. Thirteen pins of `ci.in` are copies of pins in `sandbox/requirements-runtime.txt`, `sandbox/requirements-verify.txt` and `v3-service/requirements.txt`; a contract test holds each copy the same as its product file, and every line of `ci.in` against the lock. Not from the lock yet: the lens job's torch and lens packages, and the extension's `npm ci` (#335) |
| Image scan | `.github/workflows/container-scan.yml` | Trivy, weekly and by hand, on the six images CI publishes with the tag `dev`: proxy, v3, lens, sandbox, llama (CUDA) and llama-vulkan. Critical and high findings that have a fix. It reports and does not fail. The ROCm image is built on the user's machine and is not scanned |
| Integrity check | `scripts/integrity_check.py` | Suppression markers are read by kind of file: Go, Python, TypeScript and JavaScript, shell, workflow files, Dockerfiles. In a workflow file `continue-on-error: true` and `persist-credentials: true` are named. The script holds the lists of files that need a maintainer's approval: the files that decide what a check accepts, and the scripts a workflow runs with a credential that can write. A skip with its reason asks for approval; a size baseline that only goes down and a test renamed in place are listed for information. The forms by which a test stops running are one table for each runner (pytest, unittest, Go, vitest). A form that stops more than one test says how far it reaches: the class, the group, the file, every other test of the file, whole test files, or the number of uses of a skip marker that has a name of its own. Such a name is one that the file sets at its top level or takes from a module by an import; a reason that is given by a name is shown as the text of that name |

## Baselines on `dev`

Measured 2026-10-06 on commit `a413df4`, unless a row says otherwise.

| Measure | Value |
|---|---|
| Coverage, Go proxy | 86.7% of statements (12,187 of 14,052) |
| Coverage, Go tui | 51.7% of statements (1,466 of 2,837) |
| Coverage, Python, the 8 suites together | 59.8% of lines (12,553 of 20,996). By folder: `v3-service` 84.0%, `sandbox` 68.1%, `geometric-lens` 67.2%, `atlas` 49.5%, `scripts` 30.0% |
| Coverage, TypeScript (VS Code extension) | 51.5% of lines (943 of 1,831), from the extension's last run |
| Coverage on Codecov | 67.91% of lines, all flags together (30,948 of 45,567). `go-proxy` 83.07%, `go-tui` 47.13%, `python` 59.78%, `typescript` 50.3% |
| Workflow lint, on commit `f65e01b` | zizmor: 25 findings before the fixes (20 checkouts that kept the job's token, 4 values pasted into a shell line, 1 pin comment that named no tag); none after, with one written exception in `star-chart.yml`. actionlint: none. Measured and not adopted: zizmor's level "pedantic" 61, "auditor" 62 |
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
- The coverage and the result of each test are there for every commit of
  `dev`: `dev results` sends them from the merge queue's run, and no push
  cancels the run of another. The same holds for a push to `main` (`tests`)
  and for the `vscode-extension` workflow. On a pull request a newer commit
  still cancels the run of the older one.
- Dependabot updates the actions and the package files. It does not update a
  tool that a workflow installs with a command: golangci-lint, hadolint, zizmor
  and actionlint. The settings table names the version of each. Look for a new
  release of each when the canary is renewed, and move the version and its
  checksum in one pull request.
- Dependabot does not update the pins inside `.github/actions/` either. With
  the directory `/` it reads `.github/workflows` and an `action.yml` at the
  root, and no other folder. The two upload actions there pin the Codecov
  action: when Dependabot moves that pin in a workflow file, move it in the
  two action files in the same pull request.

## What the checks found so far

For each check that reports, or that blocks by another rule: how often it
was red or made a finding, and how many of those were right. A check
becomes required, or is switched off, on these numbers.

- **Right:** a change was the right answer to the red or the finding.
- **False alarm:** no change was needed.
- **Outside:** the cause was not in the change (the network, the runner, a
  cancelled run).
- The maintainers keep the count. This is its state on 2026-10-07. A week of
  counts, in which every red of every pull request is sorted, runs from
  2026-10-07 to 2026-10-14; the table gets its next state then.

| Check | Mode | Counted on | Reds or findings | Right | False alarm | Outside |
|---|---|---|---|---|---|---|
| Sonar's gate on a pull request | reports | every pull request into `dev` since 2026-10-06 | 4 red pull requests | 4 | 0 | 0 |
| Sonar's security findings that opened a review thread | blocks: every thread must be resolved | the trial of 2026-10-05 and 2026-10-06 | 8 threads | 1 | 7 | 0 |
| Code scanning (CodeQL) alerts on a pull request | blocks: every thread must be resolved | 2026-10-07 | 3 alerts | 0 | 3 | 0 |
| `integrity check`, lines that ask for action or approval | reports | the 30 commits of ordinary work before the pipeline | 8 lines in 7 commits | 8 | 0 | 0 |
| `integrity check`, in the week of counts | reports | pull requests since 2026-10-07 | 14 lines on one pull request | 14 | 0 | 0 |
| `checks ran` | reports | every pull request and merge group since #303 | 2 red | 0 | 2 | 0 |
| golangci-lint on new Go code | reports | the 12 commits that touch Go among the last 50 before the pipeline | 3 findings in 2 commits | 2 | 1 | 0 |
| zizmor | reports | every workflow file, at the first sorting | 25 findings | 25 | 0 | 0 |
| actionlint | reports | every workflow file | 0 with the pinned version | 0 | 0 | 0 |
| hadolint | reports | 8 Dockerfiles | 37 findings, none at the level "error" | not sorted yet | not sorted yet | 0 |
| The two upload jobs of `tests` | reports | pull requests since 2026-10-07 | 1 red | 0 | 1 | 0 |
| A required check that was red for another reason than the change | required | merge groups and pull requests since 2026-10-07 | 2 (the install matrix) | 0 | 0 | 2 |
| A merge group that failed | required | every merge group on `dev` | 1 | 1 | 0 | 0 |

- Sonar's seven false alarms came from four rules: two that read a
  maintainer script's own command-line arguments as input from outside
  (S8705: 3, S8707: 2), and two for workflow files (S8545: 1, S8541: 1). All
  four are off since. The three CodeQL alerts were two texts of a list with
  no comma between them, and a host name in a test: each was removed by
  writing the line in another way.
- Before five repairs of the `integrity check` the same 30 commits gave 29
  lines, 23 of them false alarms.
- The two reds of `checks ran` were a fault in its reading of the list of
  runs (repaired by #344) and a run that waited for a maintainer's approval.
- The older version of actionlint gave 4 findings, all false; the pinned
  version reads the same files with none.
- The red of the upload jobs came from the pipeline itself and not from the
  pull request it stood on; #415 repaired the cause.
- The two reds of the install matrix were the network; the installer now
  tries a failed download again (#406).
- Not in the table: the replay job (no red so far), and Codecov's statuses
  and CodeScene, which show numbers and fail nothing.

## Rules learned from review and from failures

One line for each rule that a review or a failure has taught us, with the
pull request or issue it came from. The same finding should not have to be
made twice.

- "Held by" names the check or the test that holds the rule. A rule with no
  check there is held by review: a reviewer looks for it.
- A rule that is written here can become a check. When it does, name the
  check in its line.
- To add a rule: one line, in plain words, with the number of the pull
  request or issue that showed the need for it. A rule that the
  [contributor guide](../../CONTRIBUTING.md) states already is not written
  again: its line points to the guide.

### Tests

| Rule | From | Held by |
|---|---|---|
| The guide's rule for a test of a limit: it is harmless when the limit fails ([Testing](../../CONTRIBUTING.md#testing)) | #393 | `tests/infrastructure/test_bounded_commands.py` |
| The guide's rule for a skip: it has its reason ([Testing](../../CONTRIBUTING.md#testing)) | #389, #353 | `integrity check` names each new skip with its reason, for approval |
| A test that stops running in the plain jobs (a skip, a mark, a move to another folder) is named in the change that does it | #391, #397, #404 | `integrity check` |
| The guide's rule for a test gate that ran no test ([section 4](../../CONTRIBUTING.md#4-run-the-quality-gate)) | #304 | `scripts/production-readiness.py`, `tests/infrastructure/test_production_readiness_tests_ran.py` |
| A Go test gate does not take a result from the cache | #382 | `scripts/production-readiness.py` |
| A test writes under the test runner's own folder, never into a folder of the user | #305 | review |
| A test claims only what the code under test promises | #355 | review |
| A test of a list of the repository uses an entry made for the test, so that it holds when the real list is empty | #403 | review |

### Checks and workflows

| Rule | From | Held by |
|---|---|---|
| A check that stops checking must be seen ([The canary](#the-canary)) | #350, #396 | `scripts/canary.py check` |
| A change whose checks did not run fails. A job that was skipped behind a failed job is not counted twice | #303, #403 | `checks ran` |
| The script that judges a change is the base branch's copy, so a change cannot rewrite its own judge | #390 | review |
| A check that compares with the base uses the base branch as it is now, and stops when the base is not in the checkout | #390, #392 | `scripts/change_base.py`, `tests/infrastructure/test_change_base.py` |
| Every job names its runner image, so the image changes in a pull request and not on the day the label moves | #299 | review |
| The guide's rule for a checkout step and for a value from the event ([Code style](../../CONTRIBUTING.md#code-style), Workflows). The one exception has its reason on its line | #377 | `workflow lint` (zizmor), `tests/infrastructure/test_workflow_lint.py` |
| A workflow that starts on a push to `dev` or `main` is on a list that says whether a newer push may cancel its run | #394 | `tests/infrastructure/test_workflow_concurrency.py` |
| A push to `dev` runs no test again: it sends what the merge queue's run of the commit kept | #402 | `tests/infrastructure/test_dev_results.py` |
| The message of a check says what failed, why it matters, and how to fix it | #300 | review |
| A red that the change did not cause is counted apart, and its cause is repaired where the user meets it too: a failed download is tried again | #406 | `tests/infrastructure/test_bootstrap_download_retry.py` |
| A job that checks out another commit than the one its workflow file comes from takes no action from that checkout | #415 | `tests/infrastructure/test_workflow_local_actions.py` |
| A word that ends an issue counts only where it starts a line, outside a fenced block. A sentence that quotes such a line ends nothing | #416 | `tests/infrastructure/test_atlas_bot.py` |
| A test job installs every package from a lock file with hashes, the product's packages too | #407 | `tests/contracts/test_ci_lock.py` |

### Scripts

| Rule | From | Held by |
|---|---|---|
| Every shell script is read by shellcheck at warning level, and no line switches a warning off | #342 | `shellcheck` |
| A script keeps the downloads and logs of a run in a temporary folder that it makes for that run, and says where they are when the run fails | #409 | `tests/infrastructure/test_bootstrap_run_folder.py` |
| A script that removes a folder names it first, and refuses a setting that is empty, is not a full path, or is or holds the home folder or the repository | #414 | `tests/infrastructure/test_uninstall_script.py` |

### Product code

| Rule | From | Held by |
|---|---|---|
| A path is judged by where it leads, not by its text: a read inside the workspace goes through the confined reader | #309, #338, #348 | the tests of #339 and of those changes |
| A refusal gives its true reason | #353, #354 | the tests of those changes |

### Documents

| Rule | From | Held by |
|---|---|---|
| Every document is on the docs index | #345 | review |
| Content goes into the document that owns the topic, so that nobody misses a rule because it stands elsewhere. A new documentation file needs a maintainer's approval | #300, #362 | `integrity check` |
| A number on a quality page is measured, and the page says on what and when | #311, #347 | review |

## The replay tests

The proxy is tested from outside on recorded sessions (`tests/replay`). It
runs as a binary built from the change. Its four services (the model, the
sandbox, V3 and the lens) are stand-ins that play a recording. Each request
the proxy sends to one of them is compared, whole, with the next recorded
request. The events the proxy sends to the client and the files it leaves are
compared too.

- A recording is one JSON file in `tests/replay/recordings/`. It says where
  it came from: the task, the set the task belongs to (e2e, smoke or
  development, and no other), the commit, whether the model replies were
  written by hand or captured from a model, and what the case is for.
- The proxy makes its calls one after another, so the order is held across
  all four services: a call to the sandbox where the recording has a call to
  V3 fails the case. A recording of a session in which the proxy calls two
  services at the same time says `"order": "per service"`.
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

The cases: a normal session; a tool call that is not well formed; a reply
that is cut off; the same call again and again until the proxy stops the
session; an edit that would leave the file unparseable; a write of a whole
existing file, which is refused and redirected; and a `done` before anything
was run.

| Command | What it does |
|---|---|
| `python -m pytest tests/replay` | Replays every recording. The tests build the proxy themselves and need Go. `make verify` runs them when a file of the proxy changed |
| `python -m tests.replay.record <case>` | Makes a recording from a scripted case, with the real sandbox executor of the checkout. Needs the packages of `sandbox/requirements-runtime.txt` |
| `python -m tests.replay.rewrite <case>` | Writes the expected side of a recording again from the proxy of the checkout: the text of each request, the events, the end files. The recorded answers stay |
| `python -m tests.replay.reach [--checks]` | Says how much of the proxy the recordings run through, by file, and which check functions no case reaches |

A recording is an expected value. When a change is meant to alter what the
proxy does on a recorded session (a text the model reads, a field of a
request), the expected side is written again in the same pull request with
the rewrite command, so the diff of the recording shows exactly what changed.
The pull request says which behaviour changed and why, and a maintainer
approves it. When the change alters which calls the proxy makes or their
order, the recorded answers no longer fit and the session is recorded again.

How far the cases reach today: they run the handlers of 4 of the 16 tools
(read_file, edit_file, structural_edit, run_command). The other 12 are
registered at start and never run; their code is not covered by a replay.
The reach command counts a function as reached when one statement of it ran,
and a tool's handler sits inside the function that registers the tool, so a
tool that no case uses still shows 1 to 8%.

What a replay cannot reach today: at a replay no command really runs, so a
file that a command would make does not exist. Every check that reads such a
file (a deliverable written by a script, the output of a build) is outside
the replays, and a session whose command changes a file that the proxy reads
later cannot be recorded; the recording step refuses it. The way to lift
this: the recording keeps, for each call to the sandbox, the files that call
changed, and the stand-in puts them into the workspace at a replay.

## Tests that the plain jobs leave out

The pytest jobs run with `-m 'not integration'` (`addopts` in
`pyproject.toml`). A test that carries the `integration` mark is not run by
them. Such a test needs a running stack: the model server, the sandbox
service, or the built TUI with a proxy.

A test gets the mark in two ways: from a mark in its own file, or from the
hook `pytest_collection_modifyitems` in `tests/conftest.py`. The hook gives
the mark to every test of the files it lists and to every test under
`tests/integration/`.

| File | Tests | What it needs | The job that runs it |
|---|---|---|---|
| `tests/infrastructure/test_llm.py` | 32 | a running model server | none until the nightly (#314) |
| `tests/infrastructure/test_sandbox.py` | 38 | a running sandbox service | none until the nightly (#314) |
| `tests/infrastructure/test_sandbox_java_kotlin.py` | 24 | a running sandbox service | none until the nightly (#314) |
| `tests/infrastructure/test_sandbox_ruby_php.py` | 18 | a running sandbox service | none until the nightly (#314) |
| `tests/infrastructure/test_tui_render.py` | 4 | the built TUI and a running proxy | none until the nightly (#314) |
| `tests/infrastructure/test_tui_commands.py` | 4 | the built TUI and a running proxy | none until the nightly (#314) |
| `tests/infrastructure/test_control_plane.py` | 5 | a running stack | none until the nightly (#314) |

- That is 125 tests in 7 files. No workflow selects the mark, so no CI job
  runs them today. `tests/integration/` holds no test.
- The four `sandbox smoke` jobs send their own requests to the sandbox image.
  They do not run these files.
- The four sandbox and model files import `httpx` with `importorskip`. Where
  that package is not installed they are skipped whole, also when the mark is
  selected: 13 tests are collected then, not 125. CI does not install
  `httpx`. A job that runs these tests has to install it, and its report has
  to say how many tests it collected against the 125.
- To run them: `pytest -m integration tests/infrastructure`, on a host with
  the stack up.

The integrity check names a change that takes a test out of the plain jobs
this way: the mark on a test or a file that was there before, a file added to
the hook's list, or a test file moved under a folder the hook names. A test
that is new with the mark is listed for information. A test in the test
suite keeps this table and the hook's list the same.

- A name that a file gives to the mark (`live = pytest.mark.integration`,
  then `@live`) counts as the mark, also when the name comes from another
  file by an import.
- When the settings leave out one more mark, the check names the mark and
  counts the tests that leave the plain jobs through it, in how many files.
  A test that an older mark had taken out already is not counted again. A
  mark that comes back gives no finding.
- A mark is read from code. The same words in a text, as in the cases of a
  parametrized test, are not a mark.

## The canary

A check that silently stops checking looks the same as a check that passes.
The canary shows the difference. It is one draft pull request that is never
merged. Its branch is a copy of `dev` plus one harmless violation for each
check: a failing test in each test job, a lint error, a function that is too
long, a line that stops the installer. Every listed check must be red on it,
and red for its own violation.

- The list is `.github/canary.json`: each violation, the checks it must turn
  red, and the text that the log of each such check shows when it is red for
  that violation (`shows`).
- A check that is red for another cause shows nothing: a network fault makes
  a job red too. So the color is not enough. The script reads the log of each
  red check and looks for the text.
- Every check that runs on the canary is in the list: with a violation, or
  under `not_covered` (a job of a workflow) or `other_checks` (the check of
  a service) with the reason why it has none.
- A check with no violation of its own that is red through the violation of
  another check is under `side_effects`, with the path. A check with no
  violation that is red and is not there is named: a red with no cause on the
  list can hide a fault.
- `python3 scripts/canary.py check --pr <number>` reads the checks of the
  canary pull request and names each thing that is not as the list says: a
  listed check that passed, did not run, was skipped or has not finished; a
  check that is red but not for its violation; a check with no violation
  that is red where the list does not say why; a check that ran and that the
  list does not know; a required check the list does not know; and a canary
  older than 14 days. It needs the packages of `.github/requirements/ci.txt`
  and a GitHub token (`GITHUB_TOKEN`, or a `gh` sign-in). It changes nothing.

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

Checks with no violation, and why:

| Check | Why it has none |
|---|---|
| the three `codeql` jobs, `CodeQL`, `sonar scan`, `SonarCloud`, `SonarCloud Code Analysis` | They report findings. A finding in the planted files does not turn them red |
| `codecov/patch` | The service's own check of the coverage of the change. It is not there when no coverage report was sent, as on the canary |
| `dependency review` | It turns red only for a dependency with a published advisory, and none is planted |
| the four `PR build check` jobs | A build that fails makes other jobs red for the wrong reason |
| `integrity check` | It reports and does not fail |
| `checks ran` | Its violation is a job that had to run and reported nothing. A plant for that takes a job out of the run, and then the red of that job is missing on the canary. It passes there, with a note: `coverage upload (extension)` is skipped behind the failed job `lint + test + build` |
| `coverage upload`, `coverage upload (extension)`, `test results upload`, `test results upload (extension)` | They send numbers to Codecov and judge nothing. On the canary the extension's coverage upload is skipped, because the job it needs is red, and the extension's results upload has nothing to send and says so |
| `dev results lookup (sends nothing)` | It reads what the merge queue's run of a commit of `dev` kept. No planted file changes that. It runs only on a pull request that changes the files of the push-to-dev results |
| the image jobs of a push (`alias image tag`, `promote moving tags`) | They are skipped on a pull request |

No check with no violation of its own is red on the canary today. When one
is red through the violation of another check, it stands under `side_effects`
in the list with the path, and this page names it here.

A violation must fail every time. The time measures of the performance gate
have none for that reason: a planted slowdown fails only on some runs. The
gate's violation is a budget of 1 byte for the proxy binary, which every
build is over.

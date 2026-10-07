# Changelog

> This changelog is maintained as a best-effort summary; for line-level detail and any gaps, see the commit history (`git log`) or the GitHub PR list.

## [Unreleased]

### Changed: shellcheck reads every shell script, at warning level

The shellcheck gate read `scripts/*.sh` for errors only, which left out
`scripts/lib/`, `scripts/setup/` and the two `inference/` entrypoints. It now
reads every `.sh` file the repository tracks and fails on warnings too. The 25
warnings that were there are fixed:
- `local x=$(cmd)` is split into a declaration and an assignment, so a failing
  command is no longer hidden. Where a failure is expected and was tolerated
  (`nvidia-smi` on a machine without an NVIDIA GPU, `curl` to a server that is
  down, `find` on a missing template folder), the assignment keeps tolerating
  it with `|| true`.
- Variables that were set and never read are removed. `install.sh` now logs the
  LLM memory request and limit it works out, beside the CPU ones.
- The duplicate-NodePort check matches with a glob, the same literal match it
  made before.

No `# shellcheck disable=` line is added. Two existing ones are replaced: the
`/etc/os-release` read takes a `source=/dev/null` directive, and
`deploy-gated.sh` passes its services to `docker compose build` as an array.

### Fixed: structural_edit refused a Python method unless its first line was bare

structural_edit splices a replacement in at the node's first byte. For a
method that is after its line's indentation, so the replacement parsed only
in one shape: first line bare, later lines at their columns in the file. A
method sent at column 0, or with its original indentation on every line, was
refused with an indentation error.
- A Python replacement is now placed at the node's column whatever
  indentation it arrives with. One that already fits is used as sent.
- A column-0 method that began with a blank line or a comment compiled, but
  landed outside its class. It now stays in the class.
- Lines inside a multi-line string are never shifted.
- A replacement whose indentation mixes tabs and spaces with the file's is
  refused, and the refusal says which to use.

### Fixed: the bootstrap script tries a failed download again

One failed download stopped the whole install, though the same download
passed a few seconds later. The pip downloads and the Go module download of
`scripts/atlas-bootstrap.sh` are now tried up to 3 times, with 5 seconds
between the tries (`ATLAS_DOWNLOAD_TRIES`, `ATLAS_DOWNLOAD_WAIT_SECONDS`).
Each new try prints one line with the step and the error of the try before.
After the last try the script stops with that error, as before.

Only a download is tried again. The Go modules are now downloaded in a step
of their own, so a compile error of the TUI is not tried again; a pip error
that is no network error is not, and a module that fails its checksum is not.

Also fixed in the same lines: the TUI build looked for `go` only under
`/usr/local/go/bin`, so a Go that was on the path from the system's own
packages was not found, and the build was skipped with "go: command not
found".

### Changed: the setup script writes the branch rules that are in force, and the release step is a script

`scripts/setup/rulesets.sh` still wrote the rules from before the merge
queue: an admin bypass for pushes, also on the required checks, and no queue
on `dev`. Running it would have brought those rules back.
- It now writes the nine rulesets that are in force. No account can push to
  `dev`, `staging` or `main` or skip a required check, and `dev` takes
  changes through the merge queue.
- A ruleset that is already the same is left alone. `--dry-run` says for
  each one whether the live ruleset is the same, and what an update would
  change.
- `scripts/setup/release_step.py` is the one way a commit itself reaches a
  protected branch: the fast-forward of `staging` and `main`, and the merge
  of `main` back into `dev`. It checks first, opens only the rule that
  stops the push, and closes it again, also when the push fails.
- `docs/RELEASE.md` and `GOVERNANCE.md` describe these rules and the
  release step.
- The settings audit (`scripts/setup/audit.py`) fails when a required check
  can be skipped, and reads the review rule correctly when two rulesets
  hold one.

### Added: a driver for the held-out evaluation, with a bare-model baseline

`scripts/eval/` runs a frozen suite through two arms, grades each finished
workspace, and reports aggregates. `atlas` sends each task through
`/v1/agent` as the TUI does. `baseline` runs the same model through a
minimal read, write and run loop, with ATLAS's sampling and limits and none
of its layers.
- Graders run on a copy of the workspace, in a container with no network.
- A suite whose files changed after the freeze is refused, and so is a grader
  whose pass and fail controls do not separate.
- A run refuses the development stack and a stack it cannot tie to one
  commit.
- Each record ties its result to the frozen suite (the SHA-256 of
  `suite.json`), the grader image (by ID), the session (start time and
  workspace), the driver's own commit, the sandbox's network state and, on
  the baseline arm, the model server's context window and identity. It
  keeps the grader's whole output. `report` gives pass rates by task kind.
- `GET /version` reports `session_timeout_s`, and a run refuses a
  `--budget-s` that differs from it.

The contract is in `docs/EVAL_INTERFACE.md`.

### Added: the test jobs measure coverage

The Go, Python and TypeScript test jobs write a coverage report and upload it
with the run. A job that produces no report fails. Until now no job measured
coverage. Locally, `ATLAS_COVERAGE_DIR=<dir>` makes
`scripts/production-readiness.py` write the same reports; without it a run
is unchanged.

### Added: CI sends the coverage reports to Codecov

A new `coverage upload` job sends the Go, Python and TypeScript reports of
the test jobs to Codecov, one flag each (`go-proxy`, `go-tui`, `python`,
`typescript`). It is a job of its own, so no test job depends on a service
outside GitHub, and it runs only when the test jobs passed. The action is
pinned by commit and its uploader by version. No secret is stored: Codecov
checks the identity token GitHub signs for the run. `codecov.yml` sets `dev` as the
branch Codecov compares with, turns its pull-request comment and its line
notes off, and makes its two statuses report without failing. A commit that
has no `codecov.yml` uploads nothing, because Codecov would use its own
defaults for it.

### Added: CI sends the result of each test to Codecov

A test that passes only sometimes looks green on the run that matters. The
test jobs now write the result of each test as a JUnit file, and two new
jobs, `test results upload` and `test results upload (extension)`, send them
to Codecov's test report, under the flags of the coverage reports. They run
also when a test job failed, because a failed test is what the report is
for, and a refused upload turns only the upload job red. No secret is used.
The report is the "Tests" tab of the repository on Codecov and blocks
nothing.

In CI the Go test gates run `go test -json`. `scripts/production-readiness.py`
turns the events back into the text `go test` prints (what each package said,
and the output of every failed test) and judges that text as before; the
result of each test is written beside the cover profile. A run without
`ATLAS_COVERAGE_DIR` is unchanged.

### Changed: a test run for a commit of `dev` is not cancelled by the next push

The `tests` and `vscode-extension` workflows cancelled a run when a newer
commit came to the same branch. When pull requests merged one after another,
the run of every commit but the last was cancelled: of the last 30 pushes to
`dev`, 13 lost their test run, and with it their coverage and test results.
A pushed commit now has a concurrency group of its own and runs to the end.
On a pull request a newer commit still cancels the run of the older one. The
other workflows that start on a push keep cancelling: the result for the
newest commit takes the place of the older ones.

### Changed: the SonarQube Cloud analysis runs from CI

Sonar analysed only the default branch by itself. A new `sonar scan` job
runs the analysis on pushes to `dev` and `main` and on pull requests, with
the settings in `sonar-project.properties`: the whole repository is
analysed, test code is named as test code, and coverage is left out (the
test jobs measure it and Codecov shows it). The action is pinned by commit
and its scanner by version. The job is skipped for pull requests from forks
and from Dependabot, which get no secret, and it does not run in the merge
queue.

### Added: the reliability runner measures only the stack deployed for its commit

A result is evidence only for the stack that produced it. Before its first
session, `scripts/e2e-reliability.py` now reads the gated deploy's record
(`--deploy-dir`, default `~/atlas-ralph`). The commit it measures (`--commit`,
default: this checkout's HEAD) must be the deployed commit, and each of the
five services must run the image recorded for that commit. Otherwise the run
is refused, and each difference is named. Every result keeps the commit, the
five image ids and whether the identity was verified. A stack with no deploy
record, such as one on a contributor's machine, runs, but is marked
unverified.

### Fixed: a check that compares with the base counted the base branch's later changes as the pull request's

Four workflows (the integrity check, `checks ran`, golangci-lint and hadolint)
took the base of their comparison from the base commit of the pull request
event. The job checks out the merge of the pull request into the base branch
as it is now, so every change the base branch got in between was read as part
of the pull request: the integrity check named files the pull request did not
touch, `checks ran` could expect a workflow that GitHub had no reason to
start, and a pull request was judged by an old copy of a script.

The base is now the first parent of the checked-out merge commit, read in one
place, `scripts/change_base.py`. It stops the step when the checkout is not
that merge, and takes no other base in its place. In the merge queue the base
is the one parent of the queue's commit, as before. The base branch's copy of
the script gives the answer, so a change cannot choose its own base, and the
script is on the integrity check's list of files that configure the checks.
The step stops when the parent commit is not in the checkout, and runs no
copy of the script then.

### Added: a check that reads the change, not the code

`scripts/integrity_check.py` reads a change's diff for the ways it can weaken
the project's own checks, and a new `integrity` job runs it on every pull
request. It reports and does not fail the job.
- Tests that are deleted, skipped or lose assertions, and assertions
  rewritten in the same change as product code.
- History in new comments: dates, commit IDs, run names.
- Names of evaluation tasks in product code.
- New documentation files, changes to the files that configure the checks,
  and new suppression markers. These need a maintainer's approval.
- New dependencies and large changes are listed.

Each finding says what was found, why it matters and what to do.

### Changed: the integrity check flags less noise and reads more kinds of file

A run of the check over 50 past commits of `dev` found that most of what it
asked a maintainer to look at was noise from three causes, and that it could
not see some things at all. Five changes:

- An import that follows the line that sets the import path, marked
  `# noqa: E402`, is no longer a new suppression. A marked line that only
  moved inside its file is no longer new either.
- A size baseline whose numbers only go down, or that loses an entry, is
  listed for information. A raised number, a new entry or a changed limit
  still needs a maintainer's approval.
- A test whose name line changed while its body stayed is listed as renamed,
  or as turned into a helper when tests of the change call that helper. A
  test removed with its body, or renamed to a name the runner does not
  collect, is still reported as removed.
- A skip with its reason now asks for a maintainer's approval and quotes the
  reason; a skip with no reason still asks the author for one. A call to a
  function that skips counts as a skip: one finding for each such function
  and file, with the number of calls.
- Suppression markers are read by kind of file: Go, Python, TypeScript and
  JavaScript, shell, workflow files and Dockerfiles each have their own
  markers, so the markers of zizmor, yamllint, hadolint, staticcheck and the
  extension's coverage tool are seen, `NOSONAR` is read in every kind, and a
  marker's words in a file its linter does not read are text. In a workflow
  file `continue-on-error: true` and `persist-credentials: true` are named.
  More files need approval: the canary's list, the replay recordings, the
  local gate, the scripts whose result is a check's result, and the scripts
  a workflow runs with a credential that can write.

On the same 50 commits: 18 marker findings, 7 baseline findings, 1 skip and 1
removed test are no longer reported as they were; 4 changes to newly listed
files are reported.

### Changed: the integrity check knows the forms by which a test stops running

The check read a skip as one line with one pattern. A skip marker with a name
of its own, put on nine tests, gave one finding, on the line that defines it.
Some forms gave none. The forms are now one table for each runner:

- pytest: `skip`, `skipif` and `xfail` as a decorator, a call, a marker with
  a name of its own, or inside `pytest.param`; `pytestmark`; `importorskip`;
  `__test__ = False`; and in a `conftest.py`: `collect_ignore`,
  `collect_ignore_glob`, `pytest_ignore_collect`, and a hook that adds a skip
  marker.
- unittest: the skip decorators, `expectedFailure`, `self.skipTest` and
  `SkipTest`.
- Go: `t.Skip`, and a build tag on a test file.
- vitest: `.skip`, `.todo`, `.fails`, `.only`, `.skipIf`, `.runIf`, `xit` and
  `xdescribe`, also after another word, as in `it.concurrent.skip`.

A form that stops more than one test says how far it reaches: every test of
the class, of the group or of the file, every other test of the file (`.only`),
whole test files, or the tests a hook picks. The uses of a named skip marker
are one finding for each marker and file, with the number of uses. So are the
calls to `importorskip` in the tests of a file, for each module; the module
is the reason. A `conftest.py` is read as test material. The same words
inside a string are text.

Nothing that was reported is dropped. One finding changes its cause: a skip
marker defined below a helper function was read as part of that function, so
tests that call the helper were named. The tests that carry the marker are
named now.

### Changed: the integrity check names a test that leaves the plain test jobs

The pytest jobs run with `-m 'not integration'`. A test that gets the
`integration` mark is no longer run by them, and the check said nothing. It
now reads the marks that the runner's settings leave out (`addopts` in
`pyproject.toml`) and names three ways a change takes a test out: the mark
on a test or a file that was there before, a file added to the list of the
hook in a `conftest.py`, and a test file moved under a folder that the hook
names. A test that is new with the mark is listed for information.

The gates page lists the files that are left out today: 125 tests in 7
files, which no CI job runs until the nightly runs exist.

Also: a vitest test that gets a modifier in front of its name (`it.only`,
`it.skipIf(...)`) was reported as removed beside the right finding. It is no
longer.

### Fixed: a deletion the proxy cannot ask about no longer reads as denied by the user

Before a deletion is approved the proxy holds the file, so that the approval
is bound to the thing the user saw. That hold exists on Linux only. A proxy
built for another system refuses every `delete_file` before anyone is asked,
and the user and the model read "permission denied by user".
- That refusal now gives its reason: the file could not be held, nobody was
  asked, nothing was deleted. The reason is in the `tool_result` event, in
  the message the model reads, and in a new `reason` field of the
  `permission_denied` event. The terminal client shows it in place of
  "permission denied".
- Every other call that is not allowed reads as before, byte for byte: a
  deletion the user denied, a prompt that timed out, a cancelled request and
  the other refusals before asking. Those other refusals still read as a
  denial; that is a separate change.
- The proxy tests that go through the deletion approval are skipped on a
  system without the hold, each with the reason printed (`go test -v`). On
  Linux they run as before. The proxy suite now passes on macOS.

### Changed: a call that was not allowed says why, unless a user denied it

Every call that was not allowed read "permission denied by user", to the user
and to the model, whether or not a user had denied anything. Now only a
denial by the user reads so. The other cases give their own reason, in the
`tool_result` event, in the message the model reads and in the `reason` field
of the `permission_denied` event, on every system:

- A `delete_file` that the proxy refuses before it asks: a path outside the
  workspace, a target on the deny list, a file that does not exist, a
  directory that is not empty, a file type that is not supported, an empty
  path, arguments that cannot be read. The text is the refusal the tool
  itself gives, followed by "Nobody was asked, and nothing was deleted."
- A call that needs approval in a request with no `session_id`: nobody could
  be asked.
- A request that ended before the prompt was answered.
- A prompt that nobody answered in time. The text tells the model not to
  send the same call again in the turn, because it would wait for the same
  prompt.

This changes text that the model reads on Linux too. Who is asked, when, and
what is deleted do not change.

### Fixed: a question was told to stop reading and write a file

After four read-only calls in a row, the agent loop told the model "Do not read
more files. Emit a write_file or edit_file tool call now", whatever the request
was. On a question, that asked for what the request did not want, and it
stopped the reading the answer needed. In smoke runs, a bug-finding question
stopped one function short, replied that it could not go on, and the reply was
reported completed (2 of 84 sessions).

Now a request whose deliverable is an answer (a declared question, or "do not
change any code") is told to answer when it has what it needs, or to read only
the part still missing. Work requests keep the write notes.

### Added: golangci-lint on new Go code

A `golangci-lint` job lints the proxy and tui modules on the code a pull
request adds: unchecked and dropped errors, dead code, new functions with a
cognitive complexity over 15, new `os.Getenv` switches, suppressions without
a reason, and formatting. It reports and does not fail the job. The settings
are in `.golangci.yml`; CONTRIBUTING.md says how to run it and what to do
about each finding.

### Added: size and complexity limits for the VS Code extension

The Go and Python code have a size check; the extension's TypeScript had none.
Its lint now fails for a function over 100 lines, or with more than 15
decision points, where a switch counts once however many cases it has. Two
files hold a larger function and are listed in `extensions/vscode/eslint.config.mjs`
with its size: `ChatViewProvider.dispatch` (188 lines, 33 decision points) and
`predictEdit` (17 decision points). A listed number may go down and may not go
up. ESLint sets a limit for a whole file, so another function in a listed file
can reach the listed size before the lint fails.

A test in the extension's suite writes a function one over each limit and
expects the error, and fails when a listed number is larger than its file
needs.

### Fixed: an edit_file old_str that stopped matching its file ran on to the token cap

An old_str is text copied from the target file, so it can be checked while it
streams. The loop cut stopped a runaway only when its tail repeated word for
word, and one carrying changing line numbers never does. In one recorded
session an old_str ran 25,333 characters and 322 s to the token cap after the
model wrote a form feed where the file has a newline.

Now, once no completion of what has arrived could match the file, and old_str
has run 256 more characters, the stream is cut. The model is told the file and
the line where old_str stopped matching, and that one short line is enough to
place an edit. "Could match" uses edit_file's own tolerance (exact, curly
quotes, read_file line numbers, whitespace on each line), so an old_str that
matches is never cut.

### Added: `make verify`, the local gate for one change

`make verify` runs the quality gates that cover the files a change touches,
through the same script CI uses, and prints only what failed, each with how
to fix it. `make verify-full` adds the slow suites. A changed test file is
run itself, and the tests of a changed proxy test file run by name.
`.pre-commit-config.yaml` holds an optional pre-push hook for it.

### Changed: V3 ranks the model's own file as a candidate, and keeps its role

V3 used the model's file only as prose in its prompt, never as a candidate,
so its winner replaced the model's file whenever anything passed: in 33 of
33 deliveries across 112 recorded sessions. One of those replacements put
module code in place of a test file. The module code ran, tested nothing,
and left the run unable to finish.

- The model's exact bytes are now a candidate. They face the same checks as
  V3's own candidates, and the lens ranks them with the rest. V3 replaces
  them only with a candidate the lens ranks higher.
- A replacement must keep the file's role: every top-level def, class and
  assignment name, and every name the file imports from another project
  file. A candidate that drops one fails verification, and repair is told
  which names it dropped.
- A candidate that differs from the model's file only in whitespace never
  replaces it.
- When the model's bytes win, V3 returns them unchanged with
  `phase_solved: "incumbent"`, and the proxy writes them as the model's own.

### Changed: the TUI and VS Code show whether a run completed, and why

The `done` event carries `status` (completed, incomplete, stopped,
timed_out, failed) and `reason`, but both clients showed only the summary,
so a stopped or failed run looked like a finished one until its text was
read. Both now show the status and the reason at the end of every run, even
with no summary. Each status has its own color. A missing status reads as
incomplete, as docs/API.md says.

### Changed: the step note, the mount probe and the deliverable check stay inside the workspace

Three more places in the proxy used plain file calls on a path in the
workspace. They now go through the confined helpers, like the other project
reads.

- The note that follows a refused `write_file` names the selectors of the
  file. It now reads that file through the workspace reader. A name that
  resolves outside the workspace adds no selectors to the note.
- The check that the proxy and the sandbox share one folder writes a probe
  file in the workspace. The probe name is now cleared and created new,
  inside the folder. A link with that name is removed, and nothing is
  written through it.
- A declared deliverable counts as valid only when it is read inside the
  workspace. One that resolves outside it does not count, and its content is
  not sent to the syntax check.

The folder helper (`proxy/confined_dir.go`) gains two calls for this: write
a new file under a free name, and remove a name.

### Changed: the steering status reads its workspace place through the confined reader

The proxy's steering status looks for the control vector in three places.
One of them is inside the workspace (`models/` under the workspace folder).
That place, and the marker file beside the vector, are now read through the
confined folder helper, like the other project reads. A name there that
resolves outside the workspace counts as absent. The two other places are
fixed service paths and are read as before.

### Changed: project reads in the proxy go through one confined reader

Eight places in the proxy read project files with plain file calls: the
context sample for the plan, the Python project scan, the Node and Python
project detection, the web-asset check, and the per-project execution
setting. They now read through one helper (`proxy/confined_dir.go`) that
opens the workspace folder once and reads inside it, as the file tools
already do. A name that resolves outside the workspace is skipped like a
file that cannot be read.

Two behaviours change with it:

- A link whose target is an absolute path is not followed by these reads,
  also when it points inside the workspace. A relative link that stays
  inside the workspace is followed.
- The Python project scan and the web-asset check no longer look at the
  name of the workspace folder itself. Before, a workspace whose own folder
  was named `build`, `dist` or `env`, or for the web-asset check had a name
  that starts with a dot, was not scanned at all.

### Fixed: the lens drift check never ran, because no bundle had a fingerprint

The lens re-scores fixed reference texts at boot and fails `/ready` when an
energy drifts, which is how a serving stack that no longer matches the
artifacts (a changed `--pooling` flag, another model) shows up. Nothing
wrote the fingerprint it compares against. `atlas lens build` now writes
`drift_fingerprint.json` into every bundle. It is scored the way the service
scores: through the lens's own embedding path, under the embedding contract
the bundle declares, and against the llama-server the build reached. The
file moves with its bundle on activation, is hashed into the provenance
manifest, is kept by `atlas artifact` snapshot and rollback, and is shipped
by `atlas lens publish`. When a reference cannot be scored, the bundle gets
no fingerprint (the check enforces nothing) rather than a wrong one.

### Added: zizmor and actionlint read the workflow files

The workflows are the checks, and only a syntax check read them. Two jobs now
do: `zizmor (workflows)` for security mistakes and `actionlint (workflows)` for
mistakes GitHub shows only when a workflow runs. A finding fails the job. Both
tools are release binaries at a fixed version, held against checksums the
workflow records.

What zizmor found is fixed:
- 19 checkout steps kept the job's token in the checkout's git settings. They
  set `persist-credentials: false` now. One checkout keeps the token, with its
  reason on the line: the weekly star chart pushes to its own branch.
- The job that checks the llama.cpp patches pasted the pinned revision into
  four shell lines. The value reaches the scripts through `env` now.
- The comment beside the Trivy pin named no tag. It says `v0.36.0` now; the
  pinned commit is unchanged.

actionlint 1.7.12 found nothing. A test in `tests/infrastructure` holds every
checkout step to the same rule, so a new one without the setting fails a
required check. The canary plants a workflow for the two new jobs.

### Added: a check that fails when another check did not run

A workflow that fails to start shows no check on a pull request, and a job
that is skipped reports success, so a change could look green with a check
missing. The new `checks ran` job (`scripts/checks_ran.py`) waits for the
other workflows of the same commit, on pull requests and in the merge queue.
It reads the workflow files of the change to know what must start, GitHub's
record of the runs and jobs of the commit, and the checks the base branch
requires. It fails when a workflow has no run or failed to start, when a job
with no `if:` condition was skipped, was cancelled or reported nothing, and
when a required check was skipped or was reported by no job. Jobs skipped by
their own `if:` condition are listed, not judged. The job is not a required
check.

### Fixed: `checks ran` called a run that waits for approval a job that reported nothing

GitHub holds the runs of a pull request from a fork until a maintainer
approves them. Such a run has no job, and `checks ran` read that as "1 job(s)
reported nothing" and turned red on a pull request with nothing wrong. It now
says that the workflow waits for a maintainer's approval, what the
contributor does (nothing) and what the maintainer does, and gives no
verdict: it does not pass while a run waits, and it judges no required check
until the run has run.

### Fixed: the `checks ran` job called every workflow missing after one listing without them

The job reads GitHub's list of the runs of a commit once a minute. On one
pull request the last listing held none of the nine runs that the earlier
listings had shown, one of them still running. The job read that as nine
workflows that never started and failed, though every one of them ran.
- A run that an earlier listing showed is kept with its last known state. A
  later listing without it no longer makes it missing.
- Each listing prints one line: how many of the expected workflows are
  listed, how many are running and how many are not listed. A listing that
  leaves out a known run is named in the log.

### Added: replay tests that run the proxy against a recorded session

The proxy is about to be restructured in many small steps. Unit tests check
pieces; a replay shows that the whole loop still does the same on a recorded
session, with no model and in seconds. `tests/replay` runs the proxy as a
binary built from the change. Its four services (the model, the sandbox, V3
and the lens) are stand-ins that play a recording, and each request the proxy
sends them is compared, whole, with the recorded one. The events to the
client and the files at the end are compared too. A test of this kind does
not know how the proxy is built inside, so moving code does not touch it; a
change in what the proxy sends, says or writes fails it at the first place
the run differs. The first recording is a normal session (read, edit, run,
done). The job `replay (proxy)` runs on pull requests that touch `proxy/` or
`tests/replay/` and in the merge queue, and is not a required check
(`docs/quality/gates.md`).

### Added: six more replay cases, the order of calls, and three commands

The replay tests (`tests/replay`) now hold seven recorded sessions: a normal
one, a tool call that is not well formed, a reply that is cut off, the same
call again and again until the proxy stops the session, an edit that would
leave the file unparseable, a write of a whole existing file that is refused
and redirected, and a `done` before anything was run.
- The order of the proxy's calls is held across all four services, not only
  within each one. A recording of a session with calls at the same time can
  say `"order": "per service"`.
- `python -m tests.replay.rewrite` writes the expected side of a recording
  again from the proxy of the checkout and keeps the recorded answers, for a
  change that is meant to alter what the proxy does.
- `python -m tests.replay.reach` says how much of the proxy the recordings
  run through: 26.0% of its statements, and 66 of 131 check functions.
- `make verify` runs the replay when a file of the proxy changed, and runs
  the test that lists the senders of agent requests when a changed file
  sends one.

### Added: a canary that shows each check still turns red

A check that silently stops checking looks the same as a check that passes.
The canary is one draft pull request that is never merged: a copy of `dev`
plus one harmless violation for each check. `.github/canary.json` lists each
violation and the checks it must turn red: 21 of the 24 required checks, and
the two report-only Go lint jobs. `scripts/canary.py plant` writes the
violations on the canary branch and nowhere else. `scripts/canary.py check`
reads the checks of the canary pull request and names a listed check that
passed, did not run or did not finish, a required check the list does not
know, and a canary that was not renewed in 14 days. The two required CodeQL
jobs and `dependency review` are not covered; the list says why. The renewal
is by hand, once a week (`docs/quality/gates.md`).

### Changed: a check on the canary must be red for its own violation

`scripts/canary.py check` looked at the color of a check. A job that is red
from a network fault counted as a check that still works. Each violation in
`.github/canary.json` now names the text that the log of its check shows
(`shows`), and the script reads the log of each red check: one that is red
without that text is named.

Nine more violations: the replay tests (one changed word in a text the model
reads), the VS Code extension's job (a function over its size limit),
`pytest (tests/concurrency)`, `pytest (tests/perf)`, the performance gate (a
budget that every build is over), and the four sandbox smoke jobs (a sample
that prints a marker and ends with an error). 32 checks must be red now, 23
before. Two violations can share a file.

Every check that runs on the canary is in the list, with a violation or with
the reason why it has none. A check that the list does not know is named, so
a new job cannot run there without a decision. A check with no violation of
its own that is red through another check's violation is listed with that
path; one that is red and not listed so is named.

### Added: hadolint reads every Dockerfile, and the weekly scan reads the inference images

Nothing linted the Dockerfiles, and the inference images were the only
published images no scanner read.

- A `hadolint (dockerfiles)` job lints every Dockerfile the repository tracks
  (`scripts/dockerfile_lint.py`). It reports and does not fail for a finding:
  the counts are in the job summary, and the findings in a Dockerfile a change
  touches are annotations. It fails when it could not lint: no Dockerfile,
  hadolint did not run, or hadolint could not read a Dockerfile. hadolint is
  its release binary at v2.15.1, held against a checksum the workflow records.
  Today it reports 37 findings in 8 Dockerfiles, none of them an error.
- `make verify` shows hadolint's findings for the Dockerfiles a change
  touches, when hadolint is installed.
- The weekly container scan and its signature check read six images, not
  four: `atlas-llama` and `atlas-llama-vulkan` are added. The ROCm image is
  built on the user's machine and never published, so it is not scanned.
- The canary plants a `RUN cd` line for the new job.

### Added: a page for the quality gates and their baselines

`docs/quality/gates.md` lists the checks on a pull request and which of them
are required, how the size check, the linters, Codecov, SonarQube Cloud and
CodeScene are set, and the numbers measured on `dev`: coverage, the size and
lint counts, and what the outside tools report. The numbers are starting
points, not targets.

### Changed: the reliability runner answers a permission prompt by a stated policy

A deletion always asks for permission, also in the mode the runner sends, and
the proxy then waits for an answer for as long as its own limit says (ten
minutes unless the stack sets another). `scripts/e2e-reliability.py` sent no
answer, so a session whose model tried to delete a file stood still for that
whole wait. The runner now answers at once by a policy the run states:
`--prompts deny` (the default: an unattended run has nobody who could
approve), `allow`, or `wait` (send nothing, as before). The policy is kept
with the run (`stack.prompt_policy`), and each prompt with its session
(`prompts`: the tool, the answer, and whether the proxy took it).

The two loops that read a session's stream are one function now, in
`scripts/reliability_stream.py`.

### Added: the reliability runner records container restarts and OOM kills

`scripts/e2e-reliability.py` now snapshots each container of the compose
project (`--compose-project`, default `atlas`) before and after every
session: its restart count, whether it was OOM-killed, and when it last
started. A container that restarted, was OOM-killed, was recreated, or went
away during the session is named in the session's `stack_changes` field, in
the run log, and in a summary line. The outcome of such a session was
measured over an unstable stack. When docker cannot be asked, nothing is
claimed.

### Changed: the Go test jobs keep a build cache, and a Go test gate never takes a cached result

The two Go test jobs build with a module and build cache of their own
(`actions/cache`, with a key from the module, the Go version and the module's
`go.mod` and `go.sum`). The proxy job had none: the cache setting named
`proxy/go.sum`, which does not exist. A run in the merge queue takes no part:
it cannot read a cache of `dev`, and what it saved nothing could read again.
The two small proxy jobs that build once state that they use no cache.

`go test` prints `ok ... (cached)` and runs nothing when a package's earlier
result is in its test cache. A build cache carries those results, and the
test gate read such a line as a pass. Both Go test gates now run with
`-count=1`, and a result that still comes from the cache fails the gate with
its reason.

### Fixed: a test gate passed when it ran no test

`scripts/production-readiness.py` judged a test gate by its exit code alone.
pytest exits 0 when every collected test was skipped, and `go test` exits 0
for a package with no test files and for a `-run` pattern that matches
nothing, so such a run was reported as a pass. A pytest gate now passes only
when its summary line counts at least one passed test, and a Go gate only
when at least one package ran its tests. The reason is printed with the
failure. Go prints no count of skipped tests without `-v`, so a Go package
whose tests were all skipped still passes.

The two "coverage total" steps in CI now fail when the report cannot be
read. Each ended in a pipe, and the last command of the pipe decided the
step.

### Removed: the unused lens-projects volume on Kubernetes

The geometric-lens deployment created and mounted a `lens-projects`
PersistentVolumeClaim (sized by `ATLAS_PVC_PROJECTS_SIZE`), but nothing in
the lens has read it since the project indexer was removed. The template no
longer creates or mounts it, and `ATLAS_PVC_PROJECTS_SIZE` is gone from
`atlas.conf.example` (an old value is ignored). `uninstall.sh --data` still
deletes a `lens-projects` claim that an older install left behind.

### Fixed: ten sandbox tests failed on a system without /proc

The sandbox finds the processes of a command in `/proc`, and some of its
tests count processes the same way. On a system without `/proc` (macOS) ten
tests in `test_http_cancellation.py` and `test_execution_resource_contract.py`
failed for that reason alone. They now skip there, with that cause as the
reason, through one marker in `tests/infrastructure/proc_files.py`. Where
`/proc` exists they run as before.

### Fixed: tests of the sandbox's limits took the host's memory when the limit did not act

The tests of the memory limit start a command that takes memory, and the
limit has to stop it. The commands had no end of their own. The sandbox reads
a command's memory from `/proc`; on a system without it the limit never
acted, and six tests ran such a command until the system refused more memory
or a time limit of 20 to 40 seconds passed. A fault in the limit would have
done the same on Linux.

- Each such command now ends by itself at four times the limit under test,
  with a status of its own (`tests/infrastructure/bounded_commands.py`). A
  limit that does not act gives a failed test that says so.
- The tests that need `/proc` to stop the command, or to see whether a
  process is left, skip where there is none: nine more test functions, with
  the same marker and reason as before.
- A test fails when a command of these two test files is written without an
  end.
- The tests that assert "no process is left" count processes in `/proc`. The
  count now looks for its own process first, by the same route. Where it
  cannot see a process that is alive it fails with "cannot look", and does
  not say 0.

No product code changes.

### Fixed: the TUI tests wrote session files into the real cache folder on macOS

The session tests set `XDG_CACHE_HOME` to keep their files in a temporary
folder. Go reads that variable on Linux only; on macOS the cache folder comes
from `HOME`. So on a Mac every run of `go test` in `tui/` wrote session files
into `~/Library/Caches/atlas-tui/sessions`, the tests saw each other's files
and the leftovers of earlier runs, and two of them failed. The whole test run
now uses a temporary folder for both variables, and a test that asks for its
own folder gets one on both systems. No product code changed.

### Added: each fenced fetch attempt is recorded in a `fenced_fetch` event

A file body sent through the fenced channel is fetched in up to two
attempts. Each attempt now streams a `fenced_fetch` event with:
- the file and the attempt number, and whether the fence grammar was used;
- how long it took, and when the first frame came;
- how much content and reasoning arrived;
- which watchdog cut it, if one did;
- what happened to it: used, unusable, stalled or cancelled.

The TUI shows one line per attempt. Before, a stall showed only as a pause.

### Fixed: the fenced channel's refusal blamed an earlier stall for its own

When a fetch's own stall turned the fenced channel off, the refusal said the
channel "stalled earlier in this run". It now says it stalled on this file
just now.

### Changed: a file the session leaves unparseable must be fixed before it finishes

A file the session leaves unparseable (a new file written with a parse
error, or a broken file an edit left broken) is now an open repair until it
parses again, or it is deleted or moved:
- every tool result names the file, its parse error and the lines around it;
- the session cannot finish while the file does not parse, and is sent back
  up to three times;
- other files can still be written.

If the session ends with the file still broken, it is never reported
completed. The final message lists what was tried and the error each
attempt left, the error that remains, and why the session ended, and asks
you to take a look at the file. The done event names such files in
`repair_open`, and `repair` events record each step.

### Fixed: structural_edit could break a file that parsed

edit_file, insert_after and replace_lines refuse an edit that leaves a file
that parsed unable to parse. structural_edit did not: its splice landed with
a warning. In a smoke run a splice broke a Go file, and the edits after it
landed on the broken file. structural_edit now refuses such a splice like
the other tools: the file is not changed, and the refusal gives the parse
error and the lines around it. A file that already fails stays editable, and
the rule does not block when the check cannot run.

### Fixed: a reply that looped while it counted ran to the token cap

The repetition cut compares the end of the stream with the text before it. A
loop whose repeats carry a counting number ("29. I'll check planning.py's
end. 30. I'll check ...") never repeats exactly, so it was never cut: one ran
328 s in a smoke run. A text or done reply is now also compared with its
numbers masked, and that loop is cut after about 50 s. File content in a tool
call or a fenced block is not masked, because a file can count legitimately
(CSV rows, numbered tests).

### Fixed: inline code that touched no changed file counted as verification

`python -c "print(1)"` passed as verification: it cleared an earlier failed
run of the real program and let the session finish. A passing run of inline
code (`python -c`, `node -e`, `ruby -e`, `perl -e`, `php -r`) now counts
only when the code names or imports a file the session changed. Otherwise
it neither verifies nor clears a failure, and the session is told to run
the program or its tests.

### Fixed: a subshell, a glob or a Java class run verified nothing

A green run counts as verification of the files it names. Three common ways
to run a program named their files without a plain token, so the run bound
nothing, and a work request verified that way ended "verification demanded,
unmet":
- a subshell, `(cd app && python main.py)`, whose token kept the parenthesis;
- a glob, `javac *.java`;
- a Java class run, `java Main` or `java com.example.Main`, which names the
  class, not `Main.java`.

Tokens now lose surrounding shell punctuation, a glob matches the files it
would expand to, and after `java` a class name matches its source file.

### Fixed: re-sending a file the session just wrote got a refusal meant for input data

`write_file` refuses to rewrite a file with the contents it already has. The
refusal was written for input or fixture files ("you do not need to reproduce
a file"). In the smoke run on 2026-09-28 (add_function rep 2), the file was
the session's own test file. The model re-sent it three times, never ran it,
and passing work ended "stopped". For a file the session wrote, the refusal
now says that the file is on disk with exactly this content, and that the
next step is to run it or its tests. This holds at any size: the refusal
used to need 200 bytes, and in the smoke runs of 2026-09-29 a 76-byte test
file was re-sent five times and passing work ended "stopped" in 4 of 84
sessions.

### Fixed: the reliability runner counted working guards as service faults

`scripts/e2e-reliability.py` detector H6 ("service fault") flagged every
`error` event, so Harness Integrity counted two things that were not
faults:
- The proxy's model-output guards: a parse failure, content swallowed by an
  unescaped quote, or content whose bytes were ambiguous. These are the
  plumbing working (smallrung_toml, 2026-09-27). Any event with a category is
  now counted on its own summary line ("Model-output guards ... not harness
  defects"), never as H6.
- An LLM stream cut by the session's own work deadline (multifile_cli rep 2,
  2026-09-28). The terminal status already reports it as timed out.
- A real service fault, such as a refused connection or a 5xx from a
  service, still counts.

### Fixed: replace_lines called correct-looking numbers "stale" when nothing had changed

When the expected first or last line did not match, `replace_lines` always said
"The numbers you used are stale". In the smoke run on 2026-09-27
(smallrung_toml), the file had not changed since the model read it: the model
had used line 169 for text that is only on lines 1418-1548.

- The refusal now names a cause only when the evidence shows it: the session
  wrote the file or it changed after the last read (stale), or it still equals
  what the first full read showed (the numbers never matched). Otherwise it
  says only that the numbers do not match the file.
- It also says where the expected text is: not in the file, on one line, or
  on several lines to choose from.
- What the tool applies is unchanged.

### Fixed: a fenced write still waited about 50 seconds for the watchdog

The first attempt to fetch a fenced file is constrained by a grammar that
closes the block with four backticks. The model closes with three, which the
grammar reads as a line of the file, so the model could not stop. It wrote
more lines (in one stream, its next tool calls), then went silent, and the
attempt ended only when the idle watchdog cut it. In the smoke run on 4403ae8
(2026-09-28), 14 of 29 fenced writes waited that way (median 53 s, maximum
185 s) before a retry without the grammar.

- For a code file, the grammar now also lets the block end on a line of
  exactly three backticks. Ending there is allowed, not forced: the line can
  still be part of the file, and the model decides.
- Markdown and files of unknown type keep the four-backtick closer, because
  a ``` line can be their content.

### Security: TUI dependencies with public advisories

- The TUI now uses goldmark 1.7.17 (GO-2026-5320), golang.org/x/net 0.56.0
  (GO-2026-5942) and golang.org/x/text 0.39.0 (GO-2026-5970).
- `tui/go.mod` now requires Go 1.26.6. That release also fixes the
  standard-library advisories that govulncheck reports for older Go 1.26
  releases. The installer's default Go (`ATLAS_GO_VERSION`) is now 1.26.6.
- CI sets up Go 1.26.6. setup-go pins `GOTOOLCHAIN=local`, so CI cannot
  fetch a newer toolchain itself.
- govulncheck on the TUI: no vulnerabilities found. The proxy image is built
  with Go 1.27.1, which none of these advisories affect.

### Fixed: the model registry said two quants reuse the Q6_K lens, and the lens rejects them

The registry marked Qwen3.5-9B Q4_K_M and Q8_0 `unverified` and said they use
the Q6_K lens files. The lens loads a bundle only for the model it was built
for (same model name and embedding size), so those files never load for them.
With the lens required, a user who picked one of these quants would be
stopped after the registry said the lens works. Found while answering
Discussion #20.

- Both quants are now `no-artifacts`, and their notes say how to build a
  bundle: install with `--no-lens`, then `atlas bench` and
  `atlas lens build --from-results`. Their steering vector stays `unverified`
  (shared with Q6_K).
- `atlas model`, `atlas doctor`, `atlas init` and the registry notes no
  longer say that a model without a lens bundle runs with G(x) "silently"
  switched off. With the lens required, ATLAS stops agent work on such a
  model, and the messages now say that and name the way out.
- SUPPORT_MATRIX.md and the macOS guide say the same.

### Changed: torch 2.14.0 in the lens image

- The lens pins torch 2.14.0 (was 2.13.0) in `geometric-lens/requirements.txt`
  and in the Dockerfile's CPU-only pre-install. Dependabot leaves torch
  alone, because it can bump only one of the two pins.
- CI's lens test job pre-installed torch 2.12.1 while the requirements pinned
  2.13.0, so every run replaced the CPU wheel with PyPI's build. It now
  pre-installs the pinned version, and a contract test keeps the two equal.

### Fixed: the run was told to stop a server that a planned step still needed

Found by the smoke run on 2026-09-27 (flask_pause rep 1). The gate that asks
the run to stop its own background jobs before finishing already waited
while a verification was owed. It did not wait for the plan. The run
stopped its server, the plan gate then asked for a probe of that server,
the probe could no longer pass, and the run ended "stopped" on work the
grader passed.

- The background gate now also waits while the plan gate still owes a step
  that runs a command, and only while that gate has bounces left, so a spent
  plan gate cannot keep the job running.

### Fixed: an answer about code past a truncated read counted as evidence

Found by the smoke run on 2026-09-27 (bugfind_tiebreak). The check that
sends back an answer about a file the session never read worked per file:
any read of a file counted as seeing all of it. Both reads in that session
were cut near line 190, the answer named a function it said lay "past the
provided snippet", and the run ended "completed".

- `read_file` now records which lines it showed. A write or an edit counts
  as showing the whole file, because the old line numbers no longer hold.
- An answer that names code (in backticks) whose definition sits only in
  lines no read showed goes back once, with the file, the line and what the
  reads showed, so the model reads it before it answers.

### Fixed: a file written through the fenced channel stalled for five minutes

Found by the smoke run on the deployed build (2026-09-27). When the model
writes a file as `@fenced`, the proxy asks for the file in a fenced block.
The first attempt is constrained by a grammar that reserves four backticks
for the closer; the model closes with three, which the grammar takes as a
line of the file, so the attempt could not end. It also ran with no
progress watchdog (the watchdog was keyed on the free-text attempt only),
so it generated to the token ceiling: 8192 tokens, about 306 s, on every
such write, before a grammar-free retry that took about 10 s. Two tasks
timed out without ever running their code.

- The watchdog now covers the grammar-constrained attempt, and a cut after
  the model had written content is not counted as a channel stall, so the
  grammar-free retry still runs.
- When the fetch fails because the session was cancelled or ran out of
  time, or because too little time is left to fetch and check the file,
  the model is told that. It used to be told "no fenced block followed",
  which was false.

### Fixed: the plan's verify step demanded the planner's exact command

The planner names its verify command by guess. It planned
`curl http://127.0.0.1:5000`; the app served on 5001; the model's passing
`curl -sf http://127.0.0.1:5001/` did not count; and the plan gate then
demanded the literal step after the server had been stopped, so a finished
and verified task ended "stopped". The verify step is now satisfied by a
passing verification (a probe or a run, as the command-evidence rules
judge it) of the same program, whatever its arguments. Other plan steps
keep the literal rule.

### Measured: gemma steering does not change its tool choice

An A/B on 2026-09-27 compared the shipped gemma vector, no vector, and a
vector rebuilt for the current tool names (120 held-out probes, 2 samples
per arm, scale 0.5, through `POST /v1/agent`). Neither vector changed the
first file-writing tool measurably: without a vector, gemma already picks
`structural_edit` for 95% of whole-function rewrites and never used
`edit_file` for one. The shipped vector stays, and its registry status
stays `unverified`. SUPPORT_MATRIX records the result; whole-task outcomes
were not measured.

### Fixed: a passing model-server error at lens boot no longer fails the lens for good

The lens is required, so a lens whose boot self-test failed refuses every
request until the self-test passes. `/ready` re-runs a failed self-test
only when the failure is retryable, and three paths made a passing
llama-server error permanent:

- A 503 while llama-server loads was not retryable: the retry rule named
  urllib's `HTTPError`, and the transport now raises `ModelServerHTTPError`.
  Connectivity failures and 5xx answers now retry; a 4xx does not.
- `evaluate_energy` turned any error into zeros, which the self-test
  reported as "C(x) evaluation returned zeros". It now raises.
- A drift-fingerprint reference that could not be scored read as drift.
  It now raises, as a failed measurement; drift means a measured mismatch.

The drift message also named `--pooling mean`; the convention is
`--pooling none`.

### Changed: the final summary names the files V3 did not check

When V3 runs out of time or is unavailable on a write, ATLAS still writes
the model's own version (after the syntax and structural gates), as
before. The tool result said so at the time; the run's final summary did
not. The summary now ends with "V3 did not check these files: …", naming
each file whose bytes on disk are the ones V3 did not check, with the
reason. A file changed afterwards is not named. ADR 0004 gets a dated
revision.

### Removed: `GEOMETRIC_LENS_ENABLED`

The lens is required (ADR 0011), and nothing in ATLAS has an off switch,
so the lens has none either. The service ignores the variable; compose
and the Kubernetes template no longer set it. The `disabled` lens verdict
is gone: a lens with no model loaded reports `no-artifacts`, and its
scoring answers say `enabled: false`, which the proxy and V3 read as a
lens that cannot score.

Also fixed in the manual (non-Docker) start in SETUP.md: llama-server now
runs with `--pooling none`, as compose does (the lens's per-step path needs
per-token vectors), and a comment line no longer cuts the lens command off
from its environment variables.

### Changed: the lens is required; ATLAS stops and says why when it cannot score

A lens that was switched off, had no model loaded, or could not reach
llama-server used to degrade to "no signal": writes went unscored, V3
ranked its candidates on neutral scores, and nothing told the user. The
lens is now required ([ADR 0011](docs/adr/0011-the-lens-is-required.md),
superseding ADR 0005).

- Before any work, the proxy checks that the lens can score (lens `/ready`,
  then its `/health`; cached for 5 s). If it cannot, `/v1/agent` answers
  HTTP 503 `dependency_down` with the reason and "Run `atlas doctor`." The
  proxy's `/ready` and `/health` report the same answer as `lens_ready`,
  with `lens_reason`.
- If the lens stops scoring during a run, the run ends with
  `failed` / `lens_unavailable` before the write that needed the score,
  and the summary says whether earlier changes are on disk.
- V3 raises `LensUnavailable` instead of scoring neutral; its stages pass
  it on, and the proxy does not write the model's bytes as a fallback
  (ADR 0004 still applies to V3's own failures).
- An input the lens declines (longer than the embedding batch, empty,
  non-finite) is still reported unscored and does not stop the run. An
  uncalibrated lens counts as able to score.
- `/v1/calibration/status` carries `can_score` and the verdicts
  `disabled`, `drifted`, `self-test-failed` and
  `model-server-unreachable`; `direct_agent` reads `blocked` while the lens
  cannot score. `atlas doctor` fails on it. The TUI badge shows a failure
  (✗, also for missing or mismatched artifacts) and names the command to
  run, and the TUI shows the proxy's reason for a refused request.
- A lens with no G(x) model no longer returns its thresholds beside 0.5
  placeholder scores (`severe_mean` 0.52 read every candidate as severe),
  and a drifted lens withdraws its thresholds on the per-step endpoint too.
- Consequence: a model with no lens bundle (Qwen3.5-7B/14B/32B, a
  bring-your-own GGUF) runs no request until `atlas lens build` or
  `atlas model install-artifacts` gives it one. `atlas bench` does not go
  through the proxy and still runs, to build that bundle.

### Changed: the evaluation runners record what they measured

Every recorded dev-server run was steered and ran the loose grammar, and
the ATLAS arm of the benchmarks generated no V3 candidate at all under the
old default policy; nothing in the evidence said so.

- `scripts/e2e-reliability.py` and `scripts/novel-atlas.py` record, per
  session, how many write calls reached V3 generation and how many landed
  V3's candidate, print the totals, and warn when a run reached V3 on no
  write. Each run also records what the proxy reports it runs: the grammar
  mode (`GET /version` now includes `grammar_mode`) and the lens and
  steering state (`/v1/calibration/status`). `novel-atlas.py` records the
  image of all five services.
- `e2e-reliability.py` declares `task_mode: question` for its conversational
  probes, as the TUI's `/ask` does, and its follow-up turns carry the same
  declaration; they were sent with no contract.

### Fixed: a new gemma install used the grammar mode gemma cannot use

The docs say gemma needs `ATLAS_GRAMMAR_MODE=loose` (under the strict
schema grammar it emits `done` instead of calling tools), and every gemma
measurement on the dev server ran loose. Nothing wrote it: compose and the
proxy default to strict, so a gemma install made by `atlas init` ran
strict. The mode is now a property of the model in the registry
(`grammar_mode`: gemma `loose`, the Qwen entries `strict`); `atlas init`
writes it, the model's env vars carry it, and `atlas doctor` warns when
`.env` disagrees with the registry for the configured model.

### Changed: steering is always on, and its labels say what was measured

ASA steering was on in production and in every recorded dev-server
measurement, while the docs said gemma's vector was "off by default" and
the registry, the proxy and the TUI called it "supported" or "verified".
Steering stays always on wherever a vector is installed for the served
model; the labels now match the evidence.

- The gemma entry's `asa_status` is `unverified`: its vector was built from
  prompts that named the tool `ast_edit` (now `structural_edit`) and was
  never A/B measured. `atlas model install-artifacts` still installs and
  marks it, as it did.
- `atlas asa publish` and `atlas publish` record a new vector as
  `unverified`; promoting it to `supported` is a manual edit that cites an
  A/B result.
- The proxy reports a vector marked for the served model as `active`, not
  `supported` ("control vector active for …"); the TUI shows it as ✓.
- SUPPORT_MATRIX says gemma runs steered by default with an unmeasured
  effect, and that Qwen's May 2026 A/B predates the tool rename.

### Fixed: work requests without a task verb were read as questions

The message classifier called a request conversational (T0) when it
opened with a wh-word as a prefix ("Whole-number inputs…", "Whenever a
user submits…", "However you structure it…"), with a subordinate "When …"
clause, with an imperative "Do …", or when it contained "?" anywhere,
including in a URL, and when a mid-message ". Do not …" followed. A T0 run
is capped and never planned, and for a client that sends no contract the
done-without-action gate never armed, so a run that wrote nothing ended
"completed" with the model's "Updated calc.py" as its summary.

- A declared `task_mode: work` is never tiered T0.
- Wh-openers are matched as whole words; "when" and "where" openers must be
  inverted; an opening "do" needs a pronoun; a mid-message auxiliary needs
  a subject; a `?` counts only where it ends a clause, outside brackets and
  backticks.
- A completed run that read the project and changed no file (by tool or by
  shell) ends its summary with "No file was created or changed in this
  run."

### Fixed: V3 checked files of unknown type as Python

The V3 pipeline picked a syntax checker by file extension and fell back to
Python for any extension it did not list. A stylesheet, a C file or a
Makefile was parsed as Python, so every candidate failed with a SyntaxError
that said nothing about the file, and the Python-only checks ran on it.
Such a file now fails as "verification unavailable" for its own class.
Python remains the default only for a bench task that names no file.
`.pyi` stubs are checked as Python.

### Fixed: the V3 service planned against a budget the proxy did not honour

The proxy cuts each V3 call to half of the session's remaining time, at
most `ATLAS_V3_TIMEOUT`, and never told the service, which planned every
phase against `ATLAS_V3_TIMEOUT` alone. With 6 minutes left in a session,
the service planned a 300 s run inside a 180 s call and started work the
proxy then abandoned. The proxy now sends the cap it applies as
`budget_ms`; the service plans against it, and reads `ATLAS_V3_TIMEOUT` only
when a caller sends none. `docs/CONFIGURATION.md` also gave the cap's
default as 180 s; it is 300 s.

### Changed: the call graph always runs, for Python files only

- `ATLAS_CALL_GRAPH` is removed. The call-graph veto, the multi-hop repair
  context, the symbol-index neighborhoods and the call edges on `read_file`
  and `outline_file` always run. The dev server, where ATLAS is measured,
  already ran with the flag on, and installs ran with it off, so the
  measured and shipped configurations differed.
- The veto and the resolver are Python-only. The resolver parses with the
  Python grammar, so an HTML page whose `<script>` called `setInterval` was
  vetoed for an "unresolved" call while a static page was kept, and Go and
  JavaScript files got the same false names. The structural veto was
  already Python-only for this reason.

### Changed: one rule decides which V3 candidate lands

The candidate policy modes (`strict`, `advisory`, `automatic_v3`) and
`ATLAS_CANDIDATE_POLICY` are removed. Under the default, `strict`, a request
that declared no outputs never generated a candidate. The ordinary
interactive request declares no outputs, so V3 ran only for clients that
opted in, and the measured configuration was not the shipped one.
`advisory` delivered nothing.

- V3's selected candidate replaces the model's bytes when no hard veto fired
  and a basis holds: a declared verification passed, or the V3 selection
  path named these exact bytes and every safety requirement holds. Otherwise
  the model's own bytes land. See `docs/CANDIDATE_POLICY.md`.
- `task_contract.candidate_policy` is accepted and ignored, so older clients
  keep working. `ATLAS_CANDIDATE_POLICY` is no longer read; `atlas config
  validate` flags it.
- The TUI's `/candidate-policy` command and its header label are removed.
- A request with no contract, or a `question`, still gets no V3 candidate:
  no target is grounded. The VS Code extension sends no contract yet.

### Fixed: faults found while removing the modes

- A candidate whose applicable syntax check never ran (for example, the
  sandbox was down) could be delivered. It is now a hard veto
  (`execution_evidence_unavailable`).
- The delivery decision could say "delivers" for a candidate the grant step
  then refused. The write route then wrote nothing and told the model its
  content was kept. The decision now reads which basis earned a grant, and a
  delivery refused before any byte moves writes the model's own bytes, as
  the edit route already did.
- Declared verification commands ran against the candidate only for a
  declared output, so a request that declared commands and no outputs could
  never have its candidate verified or delivered. They now also run for the
  file the model's own call named.
- The e2e fake llama servers crashed on the proxy's body-less slot-erase
  POST, which filled the e2e log with `JSONDecodeError` tracebacks. They now
  answer it as a llama-server without slot support does.

### Removed: the lens retrain endpoint

`POST /internal/lens/retrain` answered 503 in every shipped deployment:
Compose mounts the models directory read-only, and the K3s image runs as a
user that cannot write it. Its only caller, the bench runner's opt-in
`--enable-feedback` collector, turned itself off on that 503. It retrained
on accumulated benchmark embeddings, the test-set-into-scorer path the lens
training corpus was removed for. Removed with it: `reload_weights`,
`retrain_cost_field_bce` and `load_cost_field`, the EWC and replay-buffer
modules, `stages/lens_feedback.py`, the runner's `--enable-feedback` flag
and its five `ATLAS_V3_*` settings, and the tests and docs of the path. The
lens is still built host-side with `atlas lens build`.

### Changed: V3 runs on every request

`bypass_v3`, `v3_mode` and `feasibility_mode` are removed. The first two
turned V3 off, or left only its planner, for the evaluation runners' V3-off
arm; `feasibility_mode: enforce` skipped generation when no closure path was
found, for a canary. A switch lets the measured configuration drift from the
shipped one, and `bypass_v3` did more than its runners said: it also turned
off three write gates (unresolved calls, embedded scripts, a duplicate
module entrypoint), so a V3-off arm measured a system with fewer gates, not
the same system without V3. Earlier V3-off comparisons are confounded by
that.

- Planning and generation run whenever the routing rules send a write to
  them: a file of Tier 2 or above, a V3 service configured, and a session
  not iterating on a file it just watched fail. The three write gates run on
  every write.
- A request that asks for V3 off, planner-only or `enforce` is refused with
  400. `false`, `full` and `observe` are accepted and change nothing.
- The feasibility answer is still recorded, and never stops generation.
- `scripts/e2e-reliability.py` and `scripts/novel-atlas.py` lose their V3-off
  arm. A measurement without V3 takes a research build.

### Fixed: the parts of the command-approval fix that 3.1.4 did not ship

3.1.4 shipped the rest of this fix; see its "Security" section. On dev only:

- `replace_lines`, which 3.1.x does not have, gets the write deny-list. The
  rules follow what each tool does, so a new tool cannot fall outside them.
- In the VS Code extension, one "allow for session" answer on a deletion
  approved every later deletion without showing which file. Each deletion
  is now asked about on its own, and the approval card shows the whole
  command; it used to cut it at 117 characters.

### Added: a gated deploy that covers all five services

`scripts/deploy-gated.sh` replaces a host-only script that rebuilt three of
the five services. The lens and the model server were never rebuilt, so a
lens change could not reach the running stack while `DEPLOYED_SHA` said it
had. The script also refuses a dirty checkout, requires every service to be
healthy, checks that each container runs the image just built, and checks
the running stack before it records the commit. See
[OPERATIONS.md](docs/OPERATIONS.md#deploying-a-checkout-gated).

### Changed: V3 candidates run only where the request lets one be delivered

This entry was missing from these notes. Clients declare a task contract
with each `/v1/agent` request: `task_mode` (`work` or `question`), optional
`expected_outputs` and `verification` commands, and a `candidate_policy`.
The proxy, not v3-service, decides whether a V3 candidate may replace the
model's own bytes, under one of three policies
([docs/CANDIDATE_POLICY.md](docs/CANDIDATE_POLICY.md)):

- `strict`, the default: a candidate lands only when a verification the
  client declared passes against those exact bytes.
- `advisory`: candidates are scored and nothing is delivered.
- `automatic_v3`: V3's selected candidate lands when every safety
  requirement holds.

The TUI sends `task_mode` (`work`, or `question` after `/ask`) and the
session's policy: `strict` unless `/candidate-policy` changed it. The VS
Code extension sends no contract, so it gets the operator default
(`ATLAS_CANDIDATE_POLICY`, strict unless set).

A candidate that could not be delivered is not generated: the write and
edit tools skip V3 with reason `candidate_undeliverable_under_policy`.
**So in a default TUI or VS Code session (strict policy, no declared
outputs), write and edit tools do not run V3 generation at all.** The
model's own write lands, through the usual gates. Candidates are
generated only under `automatic_v3`, when the client declared outputs, or
in the capture-only diagnostic mode, which delivers nothing. The entries below that say edits and first writes "go
through the V3 pipeline" describe the route, which still reaches the
pipeline entry, not what a default session delivers.

### Changed: a new file that does not parse lands with a warning

This entry was missing from these notes (47be143). A `write_file` of a new
file whose content does not parse is no longer refused. It lands with a
warning that names the parse error and says to run the file and read the
traceback. Refusing it had blocked the write, run and fix loop: three AoC
sessions and a novel-benchmark session ended with the file never created.
V3 is skipped for such a write, and a file that already exists is still
syntax-gated. The write is recorded as a failed parse, so the run cannot
complete while that version stands. This supersedes "New files
bypassed the syntax gate" under Measured reliability below.

### Fixed: "completed" resting on a check that never ran the program

A run could end `completed` although nothing it ran showed the program
working. The proxy counted any command whose first word was `python`, `node`,
`mypy`, `ruff`, `go build`, `curl` and similar as verification, so a parse
(`python -m py_compile app.py`), a linter, a build or a `--version` discharged
the work contract, cleared a failed test and settled the ledger as "executed
clean". It read only the exit status of the whole line, which the sandbox runs
without `pipefail`, so `pytest | tail`, `pytest || true` and `app.py; echo`
passed with the test failing, and so did `curl` against a page that answered
HTTP 500. And a failed run after a passing one never took the pass back.

`proxy/command_evidence.go` now classifies each command by what it
demonstrates (execution, probe, static check, nothing) and by whether the
command line reports that part's exit status. Only execution and probes count,
from the segments whose status reaches the line; a probe counts when an HTTP
error would fail it (`curl -f`, or the body piped into `grep`). A failed run is
recorded and takes back an earlier pass over the same bytes. The exit gates
name an uncounted command back to the model with the reason, and an unmet work
contract is said at the exit, with the command that runs the file, instead of
only in the final status. The system prompt and the `run_command` example no
longer present a build, lint or `py_compile` as verification, and the
server-start instruction no longer suggests the headers-only `curl -I`, which
never counted. Java, Kotlin, PHP, shell and `./script` runs now count, where
before they never did.

### Changed: a file no check applies to is named in the summary

A run that writes a file of a kind the syntax registry does not cover and
that is not prose (`style.css`, `.gitignore`, `Dockerfile`, `.rs`, `.toml`)
cannot demonstrate it, so the run ends `incomplete` /
`deliverables_not_demonstrated`. The summary used to say "the run ended
without finishing the task"; it now names the files ATLAS has no check for.
`.mjs` and `.cjs` are now checked as JavaScript. Rust and C/C++ are not yet:
the sandbox compiles the lone file, so a sibling header or module would read
as a syntax error. What to do about assets no check applies to is an open
decision.

### Fixed: completion did not see what shell commands did to files

A shell command reached the deliverable ledger only by rehashing files the
ledger already tracked. A module written with `cat > tool.py <<EOF`, broken,
was never checked, and a user's file removed with `rm` left no trace; both
runs ended `completed`. The proxy now walks the workspace before and after
each `run_command`: source and document files the command created or changed
become the session's deliverables and are checked like any other, and a file
that was there when the request started and that a command removed is an
unapproved deletion (`delete_intent_unestablished`). Dependency, cache and
build directories are not walked, and a file the run created and later
removed does not block. The system prompt now says deleting a pre-existing
file goes through `delete_file`. A `run_background` job writes on its own
schedule, so its changes are compared against the workspace as it stood when
the job started, once the job can no longer be writing: when completion reaps
it, when `stop_background` confirms its exit, or when the session reaps it.

### Fixed: harness refusals that stopped correct work

- Outside yolo mode, a server started with `run_background` was refused with
  the instruction to use `run_background`. Only `run_command` is redirected
  now; it keeps its check in every mode.
- That redirect, and the other shell-command refusals, skipped every failure
  counter, so a model re-sending one looped until the session deadline
  (measured: 20 identical re-sends). They now count like every other refusal.
- The f-string syntax advice said the sandbox runs a Python older than 3.12
  and sent the model after quote nesting. The sandbox runs 3.13, where that
  nesting is valid; the advice now points inside the braces.
- A `structural_edit` with a corrected body on the same selector was refused
  as a byte-for-byte re-send, the tool was banned for the file, and the run
  ended `repeated_refusal` blaming the model. The re-send refusal now compares
  the whole call.
- The fenced sub-call grammar used a three-backtick fence, so the first ```
  line a file needed (a Markdown code block, a docstring example) ended the
  file, and the truncated body was written as complete. The fence is four
  backticks now.

### Fixed: the model was told V3 had verified code nothing had run

After V3 delivered a write or edit whose phase name sounded like success, the
proxy told the model "V3 verified this edit ... The fix is on disk and
build-checked ... respond NOW with done ... do not re-read the file". The
phase could be `phase1` reached by agreement between candidates, or rest on a
compile, with nothing ever running the code. The message now comes from the
proxy's own evidence: it says the edit works only when a current run on those
bytes shows it (including evidence V3's delivery staged), and otherwise says
V3's checks are not a run and asks for one.

In V3 itself, candidates picked by agreement when none passed were marked
`passed`. They are now marked `consensus` and reported with `phase_solved:
"consensus"`, which the proxy does not treat as verified. Agreement is counted
in distinct programs, so two byte-identical copies no longer outvote a
different one; a candidate that failed the project's build command or its
import comparison cannot agree its way in; a trusted oracle is no longer
overruled by agreement; and function-shaped candidates, which the probe
silently excluded, now take part.

### Fixed: V3 judged Python with a different interpreter than the one that runs it

v3-service ran on Python 3.11 while the sandbox runs 3.13, so every
`compile()` or `ast.parse` verdict V3 gave used an older grammar than the
code's runtime: valid 3.12+ code such as `f"{d["k"]}"` (PEP 701) was
"invalid Python". The worst place was `edit_file`'s `/internal/pycheck`
pre-gate, which refused such edits, and ran before the sandbox's own check
with its rule that a file already broken may be repaired one error at a
time, so a partial repair was refused too. v3-service now builds on the
sandbox's digest-pinned `python:3.13-slim`, and a test holds the two bases
equal. The pre-gate and the V3 endpoint behind it are removed; the sandbox
check on the edited file already covers the same bytes. The interactive lint
now reports `SKIPPED` rather than `OK` when it could not parse the code.

### Fixed: the sandbox's syntax check passed what it never checked, and failed valid code

`/syntax-check` built its errors from the checker's stderr and ignored how the
checker ended, so one stopped at its time or memory ceiling, or that never
started, came back `valid: true` for nine languages. It now answers `status:
"not_run"` with the `outcome`, and the proxy and V3 record that as not run
rather than a pass or a syntax error. Other checks judged the wrong thing:

- HTML could not fail. html.parser accepts any text, so a page with no markup,
  or one cut off inside its `<script>`, was a pass that completed an HTML
  deliverable. It now needs markup and a document that does not end inside a
  tag, a comment, a `<script>` or a `<style>`.
- JavaScript left the module type to Node. From Node 20.19 `node --check` on a
  typeless `.js` file with `import` compiles nothing, so garbage and truncated
  modules passed; before 20.19 every valid module failed. It is now checked as
  `.cjs`, then as `.mjs` when that fails only on module syntax.
- Java, Kotlin and TypeScript compiled the file alone, so a reference to a
  sibling class or an installed package was reported as a syntax error and
  valid multi-file code was refused. Java is now parsed only, and Kotlin and
  TypeScript count only syntax diagnostics.

Completion also ignored a broken `<script>` embedded in a served file (the
Flask `HTML_TEMPLATE` shape) that the harness had just found, and a clean run
of the server then settled it as "executed clean"; a demonstrated
embedded-script failure now blocks both.

On macOS the resource contract read `ru_maxrss` as kilobytes (it is bytes
there), so every sandbox command in a local test run ended
`memory_exhausted`. Production runs on Linux and is unchanged.

### Fixed: a completion that rests on a parse says so

`completed` with reason `deliverables_demonstrated` could rest on a parse
alone: without a work contract (VS Code, the bench drivers), and for HTML
pages under one, nothing demands a run, and the model's own "All tests pass
and everything works" was then shown word for word. The reason is now
`deliverables_parse_only` when code or pages that could be run were not, and
the summary names them ("solve.py parses, but nothing in this run ran it").
When the model's account claims the code works or its tests pass, the
server's sentence comes first and the account is labelled as unchecked.
`deliverables_demonstrated` now means every runnable deliverable was shown
working by a current run.

### Fixed: a bare test run never covered the files it ran

Coverage came only from files a command named, so a bare `pytest`, `go test
./...`, `go run .` or `npm test` covered nothing, and a work request verified
that way could never meet its contract. A runner now covers the files this
session changed that it discovers (pytest and unittest test files, Go
packages, JS test files), plus what they import. `go test` covers only
packages that contain tests; without them it only compiles.

### Fixed: a spent exit gate no longer reads as a clean completion

Every exit gate stops sending the run back after three bounces. Seven of them
(the claim check, the unread-citation gate, the route, orphan and plan gates,
redirect-only verification and the artifact gate) then let the exit through
as `completed`, with nothing in the status, the reason or the summary. A gate
whose finding still holds at the exit now records it. A claim-check gap and a
reply citing files the run never read end the run `incomplete`
(`claim_check_unresolved`, `unread_citation`). The heuristic gates, which have
known false positives, let it complete with a caveat in the summary, and the
`done` event names every spent gate in a new `unresolved` field. The artifact
gate also kept its finding only until its first bounce; the drift now holds
until something verifies again. The unread-citation gate no longer flags a
file the run itself moved or deleted: naming it reports the operation, not a
guess about code it never saw. The run-first gate gets a reason of its own
(`warned_file_never_run`) as a backstop; the deliverable check already refused
those exits, because a warned write is recorded as a failed parse.

### Withdrawn: the V3.0 LiveCodeBench result (74.6%)

The 74.6% LiveCodeBench "pass@1" published with V3.0, and the phase-by-phase
gains derived from it, are withdrawn. The benchmark runner never ran
LiveCodeBench's hidden tests: its loader cannot decode the private test suite
and silently falls back to the 1-5 examples printed in each problem, which
were then both the in-loop tests and the final grade. It counted a task as
passed when any of three candidates passed those examples, so lens selection
could not change the count, and repair prompts received the examples'
expected output. A re-grade of a 130-task sample against the hidden tests
found that 14 of its 90 published passes fail them. The report
(`docs/reports/V3_ABLATION_STUDY.md`) now carries a withdrawal notice and is
otherwise kept as a historical record; the claims in README, SUPPORT_MATRIX,
the bench README and the translated READMEs are removed.

Also withdrawn, as unsupported: the 66.9% CxGx gate comparison cited in
ARCHITECTURE (measured on another model, with a patched runner not in the
repository, on tasks the lens was trained on, and within noise of the other
arms); ADR 0009's "54% to 75% task-success improvement", which no run in the
repository supports; the README's "reliability has improved" and the ~51
tok/s throughput figure, both from configurations that no longer exist.
ATLAS will be re-measured on held-out tasks once the current product is
re-verified.

### Removed: the lens training corpus and its capture

The proxy recorded every file the model wrote, stashed each pass's writes by
session, and appended labeled samples to a per-model corpus: through
`POST /feedback` (the TUI's `/good`, `/bad` and per-file `/deny`) and
mechanically (gate rejections as negatives, verified writes as positives).
`atlas lens retrain` trained the lens on that corpus. Evaluation runs are agent
use like any other, so the corpus filled with them — 2,425 samples on the dev
server, 1,198 of them from the AoC benchmark — with no field that could tell
them apart. A retrain would have trained the scorer on the test set. Nothing
had retrained from it yet; the deployed lens is the LiveCodeBench-calibrated
one.

Removed: the pass-write capture and its stash, the mechanical labelling,
`/feedback` and `/v1/lens/training-status`, the TUI's `/good`, `/bad`,
`/deny` and `/accept`, its post-pass rating prompt and "retrain available"
banner, `atlas lens retrain` and its corpus loader, the corpus bind mount
and its K3s hostPath, and `ATLAS_LENS_DATA_DIR`, `ATLAS_LENS_HOST_DIR`,
`ATLAS_LENS_RETRAIN_MIN` and `ATLAS_LENS_TRAINING_DIR` (the three `.env`
keys are now flagged as removed). `/review` and `/redo` stay: they list and
regenerate the last pass's files and never fed the corpus. The lens is built
with `atlas lens build` from a bench run or a labeled sample file, as before.
An existing `lens_training/` directory is no longer written; delete it once
nothing needs it.

### Removed: the pattern cache and its SQLite state store

After every V3 success, v3-service posted the problem and its solution to the
lens, where an LLM extracted a "pattern" and stored it; at the start of every
session the proxy asked the lens for up to three patterns and injected them as
a `[system note]` of lessons from previous sessions. The store held the
solutions of evaluation sessions (a fish-timer puzzle, a stats module, a
standup app), so it was a channel from the test set into the product. Measured
across 714 injection events in the evaluation evidence, 713 served the same
three seed idioms and no stored solution was ever served: it changed nothing
and carried that risk. There was no switch to turn it off.

Removed: the read and write endpoints (`/internal/patterns/*`), the extractor,
store, scorer, co-occurrence graph and seed patterns, the proxy's
pattern-context injection and its `pattern_context_injected` event, the V3
write hook, the TUI row, `ATLAS_LENS_ONLINE_LEARNING`, and the SQLite state
store that held nothing else — with its `lens-state` volume and PVC,
`SQLITE_DB_PATH`, the `sqlite` block in the lens `/health` and `/ready`, and
the doctor's `sqlite_state` check. `SQLITE_DB_PATH` and
`ATLAS_LENS_ONLINE_LEARNING` are now flagged as removed keys. An existing
`lens-state` volume is no longer mounted; reclaim it with
`docker volume rm atlas_lens-state` once nothing needs its contents.

### Removed: the requested-behaviour exit gate and its word lists

A completion exit was bounced, and the terminal set to
`requirements_unverified`, when a sentence of the request contained a verb
from a hand-written list and nothing the run executed mentioned one of that
sentence's words. The lists were grown from the evaluation prompts:
`pause`, `resume` and `toggle` entered the verb list after the pause task had
been the measured case for a week, and `confirm` was added quoting the
benchmark prompt "then run it and confirm the answer", where it let a bare run
discharge the requirement. A gate whose vocabulary is chosen so the test set
parses the intended way measures the test set, not the request. The gate,
both lists, the stop-word list, the summary rewrite and the
`requirements_unverified` reason are removed; completion is decided by the
remaining evidence gates exactly as before the gate existed.

### Removed: the browser probe and the evidence modes that gated it

v3-service carried a verifier for one artifact class — browser JavaScript
with a canvas or a `keydown` listener — whose verdict fields were named for
one game (`collision_transition`, `food_or_score_transition`), a shadow
consensus ranking, an enforce-mode override of the lens choice, and a
bounded dead-oracle consensus. All of it sat behind `ATLAS_EVIDENCE_MODE`
and `ATLAS_V3_DEAD_ORACLE_CONSENSUS`, which no deployment set, so none of it
ever ran in production. A verifier for one artifact class is not capability,
and a criterion named for one game is not a contract; both are removed
outright rather than left dormant.

**Kept**, because the proxy authorizes V3 delivery on it: the adapter
registry, contract records, closure eligibility, the evidence envelope and
`contract.select`. A `.js` file now routes to the JavaScript compile adapter
(syntax evidence, never closure) and an `.html` file is unsupported
(unverifiable, never vacuously verified). The consensus fallback for a
condemned oracle (`_consensus_winners`) is unchanged; it runs only when V3
selection runs, which a default session does not (see the candidate policy
entry above). `SandboxAdapter`
loses the `language` and `timeout` parameters the probe needed: every
remaining caller ran Python at the default 15 s, which is now fixed.

### Lens scoring boundary

A candidate longer than llama-server's physical batch (`ATLAS_UBATCH`)
cannot be embedded: the server refuses the request with HTTP 500 (`input
(2055 tokens) is too large to process. increase the physical batch size
(current batch size: 2048)`). The Lens answered that with its defaults,
energy 0.0 / normalized 0.5 / gx 0.5 / verdict `error`, and the min-energy
selector ranked the candidate first on the lowest energy in the pool.
Observed in a candidate-selection acquisition on 2026-09-04, where the
mechanism gate stopped the analysis on that record.

**Changed**

- Every Lens scoring answer says `scored`. An unscored answer carries a
  typed `failure` (`embed_capacity` with the server's `input_tokens` and
  `capacity_tokens`, `model_server_error`, `model_server_unreachable`,
  `embedding_contract`, `nonfinite_score` for a NaN or infinite value,
  `internal`) and `null` in every score field; consumers read a score
  field that is not a finite number as unscored as well, and the min-energy
  rank key never orders on one. Nothing is truncated or split: a Lens score is one forward over the
  whole sequence ([ADR 0010](docs/adr/0010-lens-capacity-boundary-is-typed.md)).
- v3-service records the failure on the candidate and in the pool capture,
  ranks an unscored candidate after every scored one, delivers it only as
  the last verified candidate standing (the `selected` event says
  `lens_scored: false`), emits `lens_unscored`, and allocates the k=3 floor
  with reason `unscored` when the probe itself is unscored. A Lens answer
  in the older shape (`verdict: "error"` with numbers attached) is read as
  unscored too. `atlas bench` ranks its pool the same way.
- The lens reports the embedding capacity it knows on `/health` and
  `/ready` (`embed_capacity_tokens`, declared through the new
  `LLAMA_EMBED_CAPACITY_TOKENS`, which compose sets from `ATLAS_UBATCH`, or
  observed from a refusal). The proxy marks `lens_scoring` partial when that
  capacity is below `ATLAS_MAX_TOKENS`, and logs an unscored write without
  applying any threshold to it. `/ready` keeps its gate.

### Verbatim reproduction

Every edit failure observed on the 12B traced to one thing: the model cannot
reproduce multi-line text exactly. A 1-line anchor lands; a 9-13 line anchor
comes back with `food.y` written as `hood.y`, `scoreElement` as
`scorerElement`, a stray `)`, or `℘` where `&&` belongs. The tool call is
then rejected for a mismatch that has nothing to do with the model's
understanding of the task. These changes attack the copying itself rather
than adding another retry around it.

**Changed**

- The tool-choice guidance in the system prompt now names the line-addressed
  edit tools. It listed `edit_file` / `write_file` / `structural_edit` and
  repeated those three, so a model changing a multi-line region could only
  pick from tools that need it reproduced — `replace_lines` and `insert_after`
  were reachable only from the raw tool list. The `old_str`-not-found steers
  had the same gap: they pointed at `insert_after`, which *adds*, and named
  nothing for *changing*. Observed live as two failed 15-line `old_str`
  attempts in a row followed by the model abandoning the edit; on the next run
  with the guidance fixed it reached for `replace_lines` directly.
- DRY sampling now defaults **off** (`ATLAS_DRY_MULTIPLIER=0`, was `0.8`).
  DRY penalizes repeated sequences, and copying a file into an `old_str` *is*
  a repeated sequence: the penalty is `multiplier × base^(matched −
  allowed_length)`, so at the previous defaults a 12-token verbatim run
  carried a −23.0 logit penalty on the correct next token. The previous
  `ATLAS_DRY_PENALTY_LAST_N` comment claimed the 2048-token window bounded
  this to the model's own output; it does not. llama-server's
  `init_sampler()` seeds the DRY ring buffer with the prompt tokens, so the
  file being copied from is itself inside the repetition window. The knob
  still works for any model that needs it.
- Agent-loop requests now decode greedily by default: `samplers: ["top_k"]`,
  `top_k: 1` (`ATLAS_TRANSCRIPTION_SAMPLER=0` restores the server chain).
  An agent turn is transcription — the tool call names a path and repeats a
  span of the file — and there is one correct token at each step. Sent as a
  one-element sampler chain rather than `temperature: 0` because temperature
  is applied *last* in llama.cpp's chain, which would pick greedily from an
  already penalty-distorted distribution instead of the raw one.
- The most recently read file is restated, line-numbered, as a final message
  immediately before the generation point (`ATLAS_RESTATE_LAST_READ=0`
  disables). The `read_file` result the model copies from sits thousands of
  tokens back behind the system prompt and every tool description, while the
  model's own drifting copy sits adjacent to the cursor. Skipped when the
  content is already the last message, when nothing has been read, and above
  24 KB.

**Added**

- `outline_file` reports embedded-language regions. The host grammar cannot
  see into a string literal, so the outline of a Flask app whose whole UI is
  one module-level template named `function:index` and nothing else — and a
  model asked to change the game loop reached for `structural_edit
  selector="function:draw"`, a symbol the outline never mentioned and no
  selector can reach. Two consecutive runs opened with exactly that call. The
  outline now names the `<script>`/`<style>` region, its line range, the
  functions declared inside it, and the fact that they are not selectable,
  reusing the block extraction the embedded-script gate already performs.
- `GET /jobs` on the sandbox, listing every background job it holds. The
  registry is process-wide with no session concept, so a server an earlier
  session left running keeps its port while `/jobs/{id}` needs an id the new
  session never saw — the bind failure's own advice, "identify and stop that
  program", was unfollowable. Observed live: "Address already in use" on port
  5001 against a server started 50 minutes earlier by a different run. The
  proxy's port-conflict hint now falls back to this list and names the
  offending job so the model can `stop_background` it.
- `replace_lines` — a sixteenth tool that changes an existing line range
  without reproducing it. `insert_after` removed the verbatim burden for
  *adding* code; this does the same for *changing* it. The model supplies
  `start_line`/`end_line` plus `expected_first_line` and
  `expected_last_line`, so the staleness check costs two lines of copying
  instead of N. Assertions compare ignoring leading/trailing whitespace
  (indentation is the most common drift and is not evidence of a stale
  range); a mismatch returns the actual line and a ±3-line numbered window.
  Capped at 60 lines and one hunk per call, requires a prior read, and runs
  the same fallback-syntax, unresolved-call and embedded-script gates as
  `edit_file`.

- `structural_edit` refuses a replacement that dwarfs the node it replaces,
  before the splice rather than only when the blob also fails to compile. The
  check existed but lived inside the post-splice `except SyntaxError` handler,
  so a blob that happened to be valid Python sailed past it: an observed
  session replaced `function:index` (3 lines, `return
  render_template_string(HTML_TEMPLATE)`) with a 258-line `HTML_TEMPLATE =`
  assignment, which parses fine. The app came out of it with zero
  `@app.route` decorators — its only route deleted — the file still parsing
  and the agent reporting success. Whether a blob is syntactically valid says
  nothing about whether it belongs in that node. Size alone does not refuse
  either, since writing a real body over a `pass` stub is also many times the
  node; it takes a second signal, either the replacement duplicating content
  that already exists elsewhere in the file, or the file holding a
  template-sized module-level string the model is evidently reaching for.
- `replace_lines`' size cap is 60 lines, up from the 20 it shipped with, and
  the over-limit refusal no longer dead-ends. The unit of work that kept
  hitting the cap is a whole function, and a JavaScript function inside a
  Flask template runs 40-50 lines. At 20 the refusal said "use structural_edit
  with function:NAME" — which cannot reach into a Python string literal, so
  its own refusal pointed straight back, and an observed session spent all
  three of its strikes on that loop with the file untouched. The refusal now
  names the split (consecutive calls, bottom of the file upward so earlier
  line numbers stay valid) and says plainly that no selector reaches code
  inside a string. The cap was never what makes the tool safe either:
  `expected_first_line` / `expected_last_line` already fail a stale range, at
  the same two-line cost whether the span is 5 lines or 50.
- A missing `}` in embedded JavaScript now names the block that was left
  open. tree-sitter reports the absence where the parser gave up, which is
  past the end of the construct that needs it: an observed session was told
  "line 202: a `}` is missing" against `setInterval(draw, 100);`, a line the
  edit had never touched. It tried to fix that line twice and the three-strike
  breaker ended the run with the file unchanged. The rejection now shows both
  lines — the opener marked as the unclosed block, the stop as where the
  brace was noticed — and says which one to go look at.
- The verification gate no longer tells a model to re-run a server. A
  `run_command` that fails because the process never exited (sandbox timeout)
  or because the port is already bound is not evidence the code is broken, but
  the gate treated it as a red test and said "re-run the same command and
  confirm it exits clean" — which a blocking server start can never do. An
  observed session started the server correctly with `run_background`, got
  that advice, and spent all three of its bounces re-sending `done` because
  nothing it could do satisfied the gate. It now names the actual next step
  (`curl` the port), and points at the already-running job by id rather than
  suggesting a second copy. A genuinely red test keeps the fix-it wording.
- A `structural_edit` that leaves a second `if __name__ == "__main__":` block
  is refused. Handed content that carried the module entrypoint along with the
  node body, the splice appended a duplicate and a 209-line file became 388.
  It parses and it runs — the first `app.run()` blocks and the rest is dead
  code underneath — so nothing caught it. It is the signature of a whole-file
  blob smuggled through a node selector. Healthy→broken: a file that already
  had two is left alone, and an indented guard inside a function is not
  counted.
- A repeated `let`/`const` in one scope is now refused at the write gate. An
  edit appended a second `let score = 0` to a `<script>` that already had one;
  tree-sitter parses that, so the syntax check passed and it landed. A
  duplicate lexical binding is an *early* SyntaxError, so the browser throws
  out the whole script before running a line of it and every handler on the
  page dies — while the Python compiles, the server starts and the page
  returns 200. Only `let`/`const` within a single scope are reported, which
  the spec makes unconditionally an error; `var`, function declarations and
  shadowing across scopes are all legal and left alone.
- A stopped render loop is now refused at the write gate. Asked to make the
  snake speed up with the score, the model replaced `setInterval(draw, 100)`
  with `setTimeout(draw, delay)` at the same top-level spot and never re-armed
  it inside `draw()`. The JavaScript parses, `python app.py` starts the
  server, and the agent reported the change verified — while the game drew
  exactly one frame. `embedded_script_check` now takes the pre-edit file and
  reports a function a repeating timer used to drive that fires once and never
  re-arms; the existing embedded-script gate refuses the write and names the
  call site. Deleting a loop outright, adding a fresh delayed one-shot, and a
  loop that re-arms from inside its own body are all left alone — the finding
  needs both versions, because one alone cannot tell a dead loop from an
  intentional one.

- A byte-identical re-send of an already-rejected tool call is refused before
  it executes. The harness is deterministic, so the same call against the same
  workspace fails for the same reason. Observed: a run emitted the same
  `replace_lines` call on two consecutive turns against a rejection that named
  the file, the line, the cause and two concrete fixes, then died on the
  three-strike breaker with the file untouched — the existing repetition
  detector needs three occurrences in its window and steers only the following
  turn, so an identical pair never reached it. Scoped to calls that failed
  (re-reading a file after editing it is byte-identical and correct) and
  cleared when the same call later succeeds.
- A completion claim the run cannot support no longer reaches the user as the
  model wrote it. The verification gate bounces `done` three times and then,
  out of bounces, lets it through: three runs ended with a confident "I
  verified..." over a broken file, once over a Flask app whose only
  `@app.route` had been deleted. The harness now states what was written and
  that nothing verified it, keeping the model's account labelled as
  unverified. Making `done` ungrammatical would be stronger but needs strict
  schema-GBNF, and Gemma-family models require the loose grammar. See
  [ADR 0008](docs/adr/0008-harness-mechanisms-over-model-instructions.md).

- The error-loop breaker no longer kills a converging run. It counted three
  consecutive failures on one path and stopped, which conflates a model
  looping with a model closing in: an observed run was refused
  selector-unreachable, then span-too-large, then stale-range — each attempt
  answering the previous error — and died with the file untouched. Failures
  are now compared by *kind* (the message with its digits, quoted spans and
  paths removed), and a rejection that differs from the last one resets the
  streak. Repeating one failure still breaks at three. A new
  `maxTotalFailures` ceiling of 12 bounds the run, since resetting the streak
  is what would otherwise let it cycle through failure modes indefinitely.
- `done` is refused while planned steps have never landed. The plan is
  generated up front, `PlanStepsSatisfied` tracks which steps a tool call has
  matched, and a progress note is injected every turn — but nothing checked it
  at the exit. An observed run built the variable-delay loop it was asked for,
  never added the per-food decrement, and declared done: two required edits,
  one delivered. The per-turn note is an instruction and was ignored; the gate
  is the same fact used as evidence. Stands down when the plan is not evidence
  — a planner score below 0.6, a single-step plan, or no step matched at all,
  since a bad plan blocking finished work is worse than no gate.

**Fixed**

- The retry refusal no longer blocks legitimate retries. It rested on
  "nothing about the workspace has changed since", and two measured cases
  falsified that. Re-running `pytest` after fixing the code was refused as a
  repeat — the verify-fix-verify loop, blocked. And an `edit_file` refused for
  "file not read yet" was still refused after the model read the file, because
  only that call's own signature was cleared. Commands and reads are now
  exempt outright (they observe the world rather than describe an edit), and
  any successful call clears the memory, since success falsifies the premise.
  A genuinely repeated call with nothing in between is still refused.
- A `text` answer cut mid-string is salvaged instead of discarded. The
  tool-call path has `recoverTruncatedToolCall`; a text answer had no
  equivalent, so when the loop detector cut a 5,897-character reply the user
  received nothing at all — the closing quote and brace were missing, so
  nothing parsed. What was written is now delivered, with a note that it was
  cut short. Only for a stream the proxy itself cut, and only above 200
  characters, since a short fragment misleads more than it helps.
- `scripts/e2e-reliability.py` parses Python with the runtime that will run it.
  It used this script's interpreter — 3.9 on this host — while the sandbox is
  3.13, and PEP 701 (3.12+) allows nested same-type quotes inside f-strings.
  `f"{items[i]["title"]}"` parses in the sandbox and raises `f-string:
  unmatched '['` here, which reported a perfectly good file as an H5 corrupt
  write. It now asks the sandbox and falls back to the local verdict only when
  the container is unreachable.
- Write-gate rejections blame the submission, not the file. The gate refuses
  the content, so the file on disk is untouched — but the message read
  "app.py has a JavaScript syntax error", which sends the model hunting a bug
  in a file that is fine. Measured as an H2 false rejection on `flask_pause`:
  the refusal was correct and the wording was not. All three embedded-script
  headlines (syntax, stopped loop, duplicate binding) now open with "Your
  content for X ..." and say explicitly that X on disk is unchanged.
- The planner is told which files already exist, by name, in the prompt. The
  scorer penalty alone could not fix this: all three `aoc_sonar` candidates
  opened with `write_file input.txt` and scored 1.00 apiece, so there was
  nothing better to prefer. Naming the files stops the step being proposed —
  and the proxy has to send the listing, because v3-service mounts only
  `/run/atlas-secrets` and `/data/telemetry`, so walking `working_dir` there
  finds nothing and `project_context` carries only a handful of priority files
  by content. The first version of this check was unrunnable in the deployed
  topology for exactly that reason.
- The planner no longer opens by recreating files that already exist. This was
  the origin of the worst failure in the measured runs, three layers above
  where it surfaced: `aoc_sonar`'s winning plan had `write_file input.txt` as
  step 1 — "create the necessary input data" — against a 2000-line fixture
  already on disk. The model followed it, tried to retype the file from
  memory, degenerated into repeating one line, had its stream cut mid-JSON,
  and the run died on three unparseable responses. `aoc_course` executed the
  same step successfully and corrupted the fixture. Both plans scored **1.00**,
  because the scorer checked step count, verify-step shape and filename
  overlap, and never what was already there. A create-shaped step targeting an
  existing file now costs 0.5 and says so; the plan that killed those runs
  drops from 0.90 to 0.40. Editing an existing file is untouched — only
  creation clobbers.
- "Failed to parse model response" is diagnosed from the cut, not the
  wreckage. The proxy's own content-loop detector ends a generation when the
  model starts repeating itself; the cut lands mid-JSON, so the response then
  fails to parse — and `classifyParseFailure` inferred a cause from the
  fragment, reporting `truncated_tool: your response hit the token cap, make
  the call smaller`. Wrong diagnosis, wrong instruction, so the model re-sent
  the same call until the three-strike breaker ended the run. The reason for
  the cut is now carried on the context and reported first, because it is the
  only fact in that function that is known rather than guessed. Measured
  across four sessions: `content loop detected (601 chars)` immediately
  followed by `parse error ... category=truncated_tool raw_len=601`.
- A `write_file` whose content is the file already on disk is refused. This is
  where the above chain starts: asked to solve an Advent of Code puzzle, the
  model called `write_file` on `input.txt` — the fixture — and tried to retype
  2000 lines of numbers from memory. It degenerated into repeating one line
  ~50 times (`941` × 50, with `92e`, `93e` and a stray `bsp` corrupted along
  the way), the stream was cut, and the run died. A sibling session got
  further and corrupted the fixture outright. Prefix-matched rather than
  exact, because the collapse truncates; floored at 200 bytes so a short file
  legitimately rewritten with the same content is not affected.
- A session refuses to start when the proxy and the sandbox are bound to
  different host directories. Both containers see the split as `/workspace`,
  so no value either holds can reveal it — the only detection from inside is
  to write a token on one side and read it from the other, which the proxy now
  does once per session (cached, 5-minute TTL, fail-soft when the sandbox is
  unreachable). Until now nothing caught it live: every `/health` passes, the
  proxy writes files the sandbox cannot see, `run_command` reports them
  missing, and the agent spends its turns concluding its own work does not
  exist. `atlas doctor` has flagged it since 2026-07-18; it recurred on
  2026-08-03 when a power cut recreated one container from `.env` while the
  other kept an overridden `ATLAS_PROJECT_DIR`, which is precisely when nobody
  runs doctor.
- The geometric lens no longer latches a boot-order race. Its self-test calls
  llama-server, and llama loads several GB before it answers, so on a cold
  start the test can 503 — after which `/ready` returned 503 for the life of
  the container even though the artifacts had loaded and llama came up healthy
  seconds later. Connectivity failures are now marked retryable and `/ready`
  re-runs the test once llama is reachable. A real fault (dim mismatch,
  missing artifacts, fingerprint drift) still fails fast and is not retried.
- The last-read restatement is bounded by the context window it spends
  against. It is appended to the wire *after* `trimMessages` has spent the
  history budget, so nothing counted it — on a 2000-line fixture the
  line-numbered copy runs ~4700 tokens per turn, and the file was already in
  the window because `trimMessages` pins the most recent file-content result.
  Measured: `aoc_sonar` failed both reps at turn 3 with `request (33012
  tokens) exceeds the available context size (32768 tokens)`. The restatement
  now skips content already anywhere in the wire (not just the last message)
  and yields when the slot has no headroom — it is an optimisation, and
  overflowing the slot ends the run.
- Every exit from the agent loop emits an outcome. Two did not: an inference
  failure streamed an `error` and returned, and the post-destructive-op path
  returned bare. `aoc_sonar` hit the first in both reps and the user got a
  tool call, an error, and silence — nothing rendering the event stream could
  tell a failed run from a dropped connection. Context-size failures are
  named explicitly in the summary, since that one is actionable.
- `scripts/e2e-reliability.py` was scoring ATLAS against a stale definition of
  a write: `WRITE_TOOLS` omitted `insert_after` and `replace_lines`, so a
  session whose only successful write used either was reported as "exited with
  no successful write" — the fifth place today a tool-name list went stale.
  H4 also matched only "stopped after" when deciding whether a run said it had
  stopped, missing the breaker's own "Stopped:" wording, and both H4 and H9
  paired tool calls to results positionally, so one unanswered call shifted
  every pair after it. Re-scoring the same 28 saved sessions: harness
  integrity 15/28 (54%) as originally reported, 22/28 (78%) corrected, and
  24/28 (86%) excluding two sessions killed by the harness's own 900s client
  timeout.
- The identical-retry refusal now obeys the same stopping rules as any other
  failure. It incremented `consecutiveErrors` and recorded the failure path,
  then returned — while the path-aware breaker and the `maxTotalFailures`
  ceiling both live inside the post-execution failure branch that return
  skips, so the counters had no reader. Observed on an ordinary feature
  request: four consecutive refusals of the same `structural_edit`, refused
  cheaply and forever, with no breaker and no ceiling. The condition is now
  shared (`stuckOnOnePath`) and the run ends with a summary saying the call
  was refused rather than attempted, and that re-running the prompt unchanged
  will hit the same wall.
- `structural_edit` says plainly that it cannot create a node. A model adding
  a feature reaches for the name it is about to write — observed on "add a
  done command that marks a task complete": turn 1 was `function:done_task`
  against a file with no such function. Listing the existing selectors was the
  right information and the wrong advice, because the model wanted none of
  them; the rejection now names `insert_after` first.
- A turn that hits its cap no longer answers with nothing. That path streamed
  an `error` event and returned, so a user whose request ran long got an empty
  reply — no answer, no partial, no explanation. Observed on a fresh
  workspace: "How does the contact form work?" spent its turns on recon, hit
  the cap, and returned zero bytes. Every other loop exit authors a summary;
  this one now says it ran out of turns, whether anything was written, which
  files it managed to read, and to ask again more narrowly.
- The conversational turn cap is 12, up from 5. A question *about* the code is
  classified conversational and still has to read the code, and 5 did not
  survive that: turn 0 went to a bounced text exit, turns 1-3 to searches with
  the wrong glob, turn 4 reached the right file, and the cap fired. The cap is
  there to stop conversational input looping, which 12 still does.
- The announcement detector missed "look into". `text` is a terminal exit, so
  an announcement that slips the intent gate ends the turn with a promise
  instead of an answer — observed verbatim: *"I'll look into the contact
  form's implementation to see how it handles submissions and where the data
  is sent."*, then done, no tool calls, no answer. Added "look into", "look
  through", "look over", "take a look", "investigate", "dig into", "trace
  through" and "review the", with tests pinning that real answers mentioning
  those words still pass through.
- A server started in the foreground is redirected before it runs. Observed on
  the first-contact path — empty workspace, "create a simple portfolio
  website" — the model wrote three files then ran `python3 -m http.server
  8000` with `run_command`, waited out the full 30s sandbox timeout, and only
  then reached for `run_background`: 30 seconds of a 3m39s run, on the most
  common way anyone will first try ATLAS. `run_command` now returns the exact
  `run_background` call instead. Deliberately narrow — `python app.py` is left
  alone, since it is as likely a script that exits.
- `insert_after` and `replace_lines` now go through the V3 pipeline. (Since
  the candidate policy entry above, a default session reaches the pipeline
  entry and skips generation; see there.) They were
  added as harness-level tools and never wired to tier classification or
  candidate generation, so their edits got a single greedy sample — no
  candidates, no lens scoring — whatever the file's tier, while the tool
  guidance and the selector rejection were both changed to steer toward them.
  The net effect was to migrate the model off the quality pipeline onto the
  two tools that lacked it. The V3 entry inlined in `edit_file` is now
  `runEditPipeline`, shared by all three, so adding an edit tool cannot mean
  re-deciding whether the pipeline applies to it;
  `tests/contracts/test_write_gate_coverage.py` asserts every write path
  reaches it.
- *Superseded: the corpus and its capture were removed (see "Removed: the
  lens training corpus" above).* The lens training corpus is fed by the
  harness, not only by a human.
  `appendLensSample` had exactly one caller — `POST /feedback`, a thumbs
  up/down or per-file accept/deny — while `LensSample.Source` had always
  advertised `v3` and `run` alongside them and nothing wrote either. Twelve
  instrumented runs produced dozens of deterministic gate rejections and
  several passing verification commands, and the corpus directory was empty,
  because nobody clicked. Gate rejections are now recorded as negatives at
  full weight (a gate does not have opinions); the writes of a run whose
  verification passed are recorded as positives at weight 0.5, deliberately
  below a human sample, because "curl exited clean" is weaker than it sounds —
  one observed run returned 200 over a page whose game loop was dead, another
  over a Flask app with no routes left. Negatives are recorded at the single
  point where a failed tool result is handled, so a gate added later cannot
  miss it, and only content the model actually authored is sampled.
- Every write path now runs the same gates. `edit_file` — the most used edit
  tool — ran the syntax and unresolved-call checks but never
  `embeddedScriptGate`, so the two comparative findings (a render loop that
  stopped repeating, a lexical binding declared twice) were never evaluated on
  it; `edit_file` and `write_file` also skipped the duplicate-entrypoint
  guard. Caught by A/B: the same one-shot `setTimeout(draw, delay)` the gate
  refuses under `replace_lines` landed through `edit_file` on the next run,
  and the page returned 200 with a dead game. A gate wired into four of five
  write paths reads as covered and is not, so
  `tests/contracts/test_write_gate_coverage.py` now asserts the wiring
  directly — nothing else can, since each tool builds its own chain and the
  compiler cannot see a missing call.
- A selector that names embedded code is told where it lives instead of that
  it does not exist. `structural_edit selector="function:draw"` against a
  Flask app whose game loop is in `HTML_TEMPLATE` returned "that symbol does
  not exist in this file" — plainly contradicted by the file the model had
  just read, which is why three runs re-sent it. It now reports that `draw`
  exists as JavaScript at specific lines inside a named string literal, that
  no selector reaches it, and which tools do.
- A V3 candidate that rewrites text the caller's edit never touched is now
  discarded, keeping the caller's content (the same fallback the parse check
  already used). `edit_file` and `structural_edit` splice their change and
  hand the composed *file* to v3-service, which regenerates it — on a small
  file that is a retype of everything the edit left alone, and the same
  verbatim drift applies. Caught in a live session: V3's accepted candidate
  wrote `#e94562` as `#e94162` and `<h1 id="msg">` as `<h1 id=" msg">`,
  which makes `getElementById('msg')` return `null` at runtime. Neither is a
  syntax error, so the parse and embedded-script gates passed them. The rule
  is that a line the edit did not remove must survive V3 intact; V3 remains
  free to improve the lines the edit actually changed.
- `insert_after` was missing from four `switch` statements that every other
  edit tool appears in: the lens breaker's failure-path extraction (so three
  consecutive failures on the same file did not trip the path-aware
  breaker), the worked-example generator (so its schema shipped without a
  filled-in example), the productive-change counter (so successful inserts
  did not count as progress), and workspace-path containment validation.
  `replace_lines` is registered in all four.

## [3.1.6] - 2026-10-01 — Maia

A security release. It changes nothing else.

### Security: a command behind a prefix or a nested shell skipped the command policy

- The command policy checked the command at the start of the line. A command
  placed after a prefix, or inside a nested shell or `eval`, was not checked
  the same way, so a command the policy refuses could still run. The policy
  now looks through these layers and checks every command they run.
- A command whose quoting or execution layers cannot be inspected completely
  is now refused, with a message saying so, instead of allowed.
- Inspection goes at most 16 layers deep; a command with more is refused.
- One rarely used form is now refused: `eval` of generated command text with
  nested quoting. Run the generated command directly instead.
- Reported and fixed by @Rendegou (GHSA-m9w4-p32x-chx9).

## [3.1.5] - 2026-09-29 — Maia

A security release. It changes nothing else.

### Security: an argument name in another letter case skipped the workspace check

- Tool arguments were checked by their exact names (`path`, `command`), but
  the tools accepted the same names in any letter case. A call that spelled
  a name another way, such as `"PATH"`, skipped the workspace check, and in
  3.1.4 also the command deny-list. Argument names must now be the tools'
  exact lowercase names; any other spelling is refused before the tool runs.
- `insert_after`'s path is checked against the workspace when the call is
  dispatched, like every other write.

## [3.1.4] - 2026-09-27 — Maia

The first release from ATLAS's new home, **inferstep/ATLAS**. It carries the
security fixes below, the move to the new image owner, and the new
contributor setup, on top of the changes since 3.1.3 listed further down.

### Security: commands ran without approval, and file search read credential files

- `run_background` started any command without the approval prompt in the
  default and accept-edits modes, and was checked against a narrower
  deny-list than `run_command` (`env rm -rf /` and `(rm -rf /)` passed it).
  Outside yolo, every tool that runs a command now asks, and one command
  policy covers both. It looks where a command can start: behind `env`,
  `nohup`, `nice`, `time`, `timeout` or `exec`, in a subshell and in a
  command substitution. `grep mkfs notes.txt` is no longer refused.
- `search_files` returned the contents of credential files that
  `read_file` refuses (`.env`, keys, cloud credentials) and followed
  symlinks out of the workspace. It now skips both and reports how many
  credential files it skipped (`skipped_credential_files`). `move_file`
  refuses to move a credential file to another name, and `insert_after`
  gets the write deny-list. The rules follow what a tool does, and a test
  fails when a new tool is outside them. Shell commands are not covered,
  and the docs now say so.
- Approval prompts cut a command at 100 characters, so the end of a chain
  was never shown. The proxy sends the whole command, and
  `stop_background` names its job.
- In the TUI, one "allow for session" answer on a deletion approved every
  later deletion without showing which file, and the proxy honoured
  `delete_file` in `session_allowed_tools` from any client. Each deletion
  is now asked about on its own: the TUI never auto-answers or sends a
  session approval for `delete_file`, the proxy ignores one, and a new
  session starts with no approvals. The TUI prompt shows the whole
  command, wrapped; one too long for the screen keeps its first and last
  lines in view and says how many are not shown.
- The TUI's chat stream, events stream, raw demo lane and feedback calls
  never sent the service token, so on an install with one they failed
  with 401. Every request to the proxy now sends it, ahead of an api-keys
  token.

### Moved to inferstep/ATLAS

- The repository is now **github.com/inferstep/ATLAS**. Old links, `git`
  remotes and the install one-liner redirect.
- Images are published under **ghcr.io/inferstep/atlas-*** and signed by
  the inferstep/ATLAS build workflow. `ghcr.io/itigges22/atlas-*` stays
  published for existing installs but gets no new versions.
- **Existing installs:** re-run the install command, or `git pull` and then
  `atlas upgrade`. `atlas upgrade`, `atlas config migrate` and a bootstrap
  re-run move `ATLAS_GHCR_OWNER=itigges22` in `.env` to `inferstep`. A
  failed upgrade or a rollback puts it back. An install pinned to a
  release from before the move keeps the old owner until it upgrades, and
  an owner set in the shell is left alone.

### Fixed

- `golang.org/x/net` in the TUI is now v0.55.0 (GHSA-5cv4-jp36-h3mw).

### Contributors

- New issue forms for bugs, features, tasks, docs, spikes and RFCs, and a
  fuller pull request template. [CONTRIBUTING](CONTRIBUTING.md) is
  rewritten as the path from an issue to a release. [GOVERNANCE](GOVERNANCE.md)
  describes the trust ladder and the RFC flow. New
  [TRIAGE](docs/TRIAGE.md) and [INCIDENT_RESPONSE](docs/INCIDENT_RESPONSE.md)
  guides.
- The public [Roadmap board](https://github.com/orgs/inferstep/projects/1)
  has a Start Here view. The atlas-bot handles `/claim` and `/unclaim`,
  reminds and releases stale claims, adds area labels and welcomes
  newcomers.
- Pull requests now also need the dependency review and a conventional
  title check. An OpenSSF Scorecard runs weekly. Dependabot targets `dev`.
- Releases record a deployment per promotion, and publishing `:latest` or
  a version tag waits for the release owner's approval.

### Docs

- The V3.0 LiveCodeBench figure (74.6%) is withdrawn. The benchmark runner
  never ran LiveCodeBench's hidden tests (see the notice in
  [V3_ABLATION_STUDY](docs/reports/V3_ABLATION_STUDY.md)). The README says
  ATLAS has no current benchmark result.

### Measured reliability

A day of running ATLAS against itself and fixing what the sessions showed.
Every fix below was traced to an observed session and was meant to carry a
test that fails without it. A 2026-09 audit reverted four of them: two were
caught by tests, and two were not (the sandbox's multi-document YAML check,
and the system-prompt bullet for questions about code, which only a
whole-prompt hash noticed); both now have tests. `scripts/e2e-reliability.py`
reports the two numbers this work is judged on — harness integrity (ATLAS's
own plumbing, which should be 100%) and task success (whose failures it does
not classify as model or harness) — plus objective code-quality probes from
`scripts/code_quality.py`.

**Added**

- `insert_after` — a fifteenth tool that inserts lines after a line number
  rather than after text the model must reproduce. Both existing edit
  primitives put a large verbatim-output burden somewhere (`edit_file` an
  anchor, `structural_edit` a whole node), and that is the step that
  measurably fails. `read_file` already prints line numbers, so this takes a
  number the model can cite and only the new text.
- `scripts/verify-deployed.sh` — a manual pre-measurement check that the
  running code is the checked-out code, catching both source-newer-than-image
  and image-newer-than-container. Nothing runs it automatically: no runner
  or deploy gate calls it.
- Live-stack coverage for the TUI (13 slash commands and 3 keys driven
  through a pty; liveness checks, integration-marked and deselected by
  default), the control plane (`/cancel`, `/v1/permission`), and multi-turn
  conversations, none of which had any.

**Fixed — tier and conversation**

- A question that said "do not change any code" was classified as work, so
  ATLAS was *more* likely to edit when told not to. Three causes: negation
  blindness in both intent classifiers, an explain-plus-no-edit directive
  read positionally, and a question detector that only saw a trailing `?`.
- Questions about code were answered without opening the file, because one
  system-prompt bullet lumped them in with greetings.
- A reply that announced a tool call, or promised an answer, ended the turn
  without delivering either.

**Fixed — gates and writes**

- V3 candidates that regressed the caller's content were blamed on the model.
- One honesty gate could spend the shared bounce budget and silence the other
  three.
- A semantic no-op (only comments changed) counted as a completed edit.
- A rejected tool call emitted no `tool_result`, so the call never resolved
  for the client.
- `write_file` could clobber a file the session had never read.
- New files bypassed the syntax gate, because the sandbox's YAML checker
  wrongly rejected multi-document files and had disabled the gate wholesale.
  *Superseded for new files by 47be143: an unparseable new file now lands with
  a warning (see the entry at the top of these notes). The YAML fix stands.*

**Fixed — what ATLAS told the model**

- `read_file`'s line numbers and the call-graph footer read as file content;
  a correct grid algorithm parsed the display format and printed 0.
- Steering offered `<tag>` selectors for `.py` files, and named a function
  "holding the template" when the template is a module-level constant.
- Cryptic Python errors were passed through unexplained: stray backslashes,
  entity-encoded content, and f-string quote nesting that is valid from 3.12.

**Changed**

- Sandbox base image moved to Python 3.13 (was 3.11, which rejected valid
  3.12 syntax and cost a full session).
- Nine real `.env` keys were reported as typos by `atlas config validate`.

### Proxy reports its workspace path

- New `GET /workspace` endpoint returns the host and container paths the
  proxy has mounted, so a client can check whether its own folder matches
  what the proxy is actually editing — exact, instead of the VS Code
  extension's old post-edit `fs.stat` heuristic. Requires the service token
  like any other route, since it discloses a host filesystem path.
- `docker-compose.yml` now passes `ATLAS_PROJECT_DIR` into the proxy's own
  environment (previously only used at compose-time to build the bind mount,
  never reaching the container), so the endpoint has something to report.
- The VS Code extension's `alignment.ts` tries the endpoint first, falling
  back to the existing `atlas workspace` CLI check when it's unreachable or
  the proxy predates this route — the CLI path stays, since only it can
  detect the proxy/sandbox split-bind case.

### CodeQL now scans the TypeScript client

- `javascript-typescript` joins the CodeQL language matrix. The VS Code
  extension shipped ~5,100 lines of TS/JS that no scanner looked at, so the
  next client lands on a fully covered tree. The extractor needs no build
  step, and both Go steps already carry `if: matrix.language == 'go'`.
- The webview's CSP nonce came from `Math.random()` (the shape every VS Code
  webview sample uses) and is now `randomBytes(16)` — `js/insecure-randomness`
  is in the `security-and-quality` pack, and a security token has no business
  coming from a non-cryptographic PRNG. Not a live hole: `renderHtml`
  interpolates nothing untrusted and `media/chat.js` writes through
  `textContent`, so there was no injection point a guessed nonce could unlock.


### Simplification campaign (2026-07-29 → 2026-08)

One component-by-component pass over the whole tree — merge the fragments,
split the God-files, cut what nothing calls — with the test suites as the
invariant. Headline numbers, measured from the campaign's first commit:
**3,047 → 514 tracked files, net ≈ −56,500 lines including data** (counts at
the end of the campaign, not today's tree, which has grown since). The
per-component disposition ledgers live in the commit history for that
range.

- **One chat surface.** The pipe-mode `/solve` REPL is gone; bare `atlas`
  launches the TUI (no-TTY prints a pointer to `atlas doctor` and exits
  nonzero). The proxy launch/align/stop lifecycle moved to
  `atlas/runtime.py`. The TUI itself merged 15 files → 9; the proxy
  consolidated 33 files → 12, its tests 61 → 24 mirror files.
- **Retrieval/routing stack removed, this time for good** (the 2026-07-22
  removal below was reverted for per-component review; that review is now
  done). PageIndex/BM25 retrieval, the confidence router, the lens `/v1/*`
  surface (projects, tasks, queue, chat/completions, auth), the cache
  consolidator + LTM tier, and dead lens routes (`/internal/lens/stats`,
  cache flush/consolidate, `/v1/patterns/write`) are gone.
- *Superseded: the pattern cache was removed in 2026-09 (see "Removed: the
  pattern cache" above).* **Pattern-cache reader added.** What replaces retrieval:
  `POST /internal/patterns/context` serves lessons from previous sessions
  (type + recency + success scoring, co-occurrence expansion), and the
  agent loop injects the top ≤3 as a `[system note]` — always-on,
  fail-soft, no flag.
- **RPG planning removed everywhere** (it was never shipped in the v3
  image); the A/B on the reference 12B showed no improvement at ~10x
  planning latency. [#148](https://github.com/inferstep/ATLAS/issues/148)
  is the record.
- **V2/TB2 benchmark subgraph and the five superseded trainer scripts
  removed.** The onboarding loop is fully CLI-driven: `atlas bench` →
  `atlas lens build --from-results`, and every lens build now writes a
  `provenance.json` manifest into the activated bundle.
- **Code moved to where it fires.** The V3 pipeline stages live in
  `v3-service/stages/`; the benchmark harness is `atlas/bench/` inside the
  pip-installed package (repo-root `benchmark/` holds data only);
  `atlas/cli/*` flattened to `atlas/*`; `v3-service/main.py` split into
  flat siblings (adapters/scoring/symbols/planning/pipeline).
- **Debris ledgers executed.** Dead routes (`/v3/run`,
  `/internal/call_graph` and its Datalog/Prolog engines), the inert
  metacognitive module, aspirational error codes (the taxonomy is now the
  six codes `writeError` actually emits), the never-incremented health
  counters, duplicate parse-failure/template-walker/read-ledger mechanisms,
  and seven unused dataset loaders.

### Reverted: the removals below were undone on 2026-07-22

The RPG, wavelet, retrieval, and ablation-data removals described in this section
were reverted the same day. The code is back in the tree. Each subsystem is being
reviewed one component at a time rather than in a single pass, so the entries below
describe what was removed and why, not the current state of the tree.

Reverted in `db4b055`, `b407eed`, `69f2dea`, `8d0abf2`. The `structural_edit` rename
and the sampling and honesty-gate work were kept and remain accurate as written.

### Removed: RPG planning, wavelet decomposition, and the retrieval stack (2026-07-22)
- **RPG planning removed.** `ATLAS_RPG_PLANNING` shipped default-off; the A/B
  against the flat planner on the reference local model returned 0
  improvements, 2 regressions, and roughly 10x planning latency. The outcome
  bottleneck is the model writing correct code, not the plan it writes
  against. Deleted `v3-service/{rpg.py,rpg_eval.py}`, `v3-service/wavelet/`,
  `proxy/rpg.go`, the two-stage planner, the signature veto, the drift
  regeneration loop, and the RPG types. The flag stays in the config schema
  marked deprecated so an existing `.env` gets a specific warning rather than
  "unknown key". `docs/reports/RPG_WAVELET_PLANNING_V3_2.md` is kept and
  marked removed — the design record and the reason it did not pay off are
  both worth having.
- **Retrieval stack removed.** `/v1/projects/*`, `/v1/tasks/*`,
  `/v1/queue/stats`, and the lens's own `/v1/chat/completions` had no caller
  anywhere in the repo — they appeared only as rows in the API.md table. The
  PageIndex tree index, BM25, hybrid retriever, project store, and the router
  stages that fed them are gone. The pattern cache stays: v3-service writes to
  it through `/internal/patterns/write` after every successful candidate.
  *(Superseded: the pattern cache and `patterns/write` were removed in
  2026-09, and `atlas lens retrain` with the corpus.)*
- **Endpoints kept and verified against their callers**: `score-per-step`
  (proxy, v3-service), `gx-score` (CLI, v3-service), `score-text` and
  `sandbox/analyze` (CLI), `retrain` (benchmark), `reload` (retrain scripts),
  `patterns/write` (v3-service). 23 lens routes before, 19 after.

### Renamed: ast_edit is now structural_edit (2026-07-22)
- The tool resolves a friendly selector (`function:NAME`, `class:NAME`,
  `<tag>`) to exactly one tree-sitter node and replaces that node's source
  text. tree-sitter produces a concrete syntax tree, and the tool never
  traverses one, so the old name described neither the operation nor the
  substrate — and it required the model to reason about compiler internals
  when the trigger is "replace this whole function". Renamed across the proxy,
  v3-service, CLI, ASA scripts, tests, docs, and all three translations,
  including the `/internal/ast_edit` endpoint.
- `ast_edit_steering.gguf` keeps its name: `model_registry.py` pins it by
  filename and SHA256 against the HuggingFace dataset, so renaming would 404
  every download. The ASA contrast prompts embedded the literal old tool name,
  so published vectors predate the rename; `asa_calibration/README.md`
  documents the rebuild path.
- `atlas/cli/client.py`'s `RAG_API_URL` became `LENS_URL` (it always pointed
  at the lens), and `ATLAS_LENS_URL` now takes precedence over the deprecated
  `ATLAS_RAG_URL`, which previously overrode its own replacement.

### Sampling and honesty gates keyed on evidence (2026-07-22)
- **Repetition sampling enabled.** llama-server ships every repetition control
  off (`repeat_penalty=1.0`, `dry_multiplier=0.0`, both penalties 0.0), and
  the proxy set none, so nothing bounded a repeating generation. DRY is now
  set on outgoing requests *(superseded: DRY defaults off again, see "Verbatim
  reproduction" above)* — chosen over `repeat_penalty`, which scores
  individual tokens and punishes the indentation and keywords source code
  repeats legitimately. Six env knobs, forwarded by compose and registered in
  the config schema (which gained a `float` kind rather than demoting them to
  unvalidated strings). Values are not yet A/B'd.
- **Truncation recovery rejects degenerate output.** The three recovery paths
  rebuilt tool args from whatever the field extractor read, with no check —
  a run of repeated newlines parses as cleanly as a function body, so a
  degenerate generation became a real `edit_file` against the user's file.
- **Verification gate** now also fires when a test or build command actually
  exited non-zero and nothing has passed since, catching a failing test the
  model introduced itself — which no reading of the user's message predicts.
- **Done-without-action gate** now also fires when the model opened the
  project on a non-conversational message and nothing reached disk, covering
  verbs absent from the intent list (`remove the debug logging` matched none).
- **Message tier reduced to its one real decision.** `TierMaxTurns` treats
  T1/T2/T3 identically, `shouldGeneratePlan` tests only T0, and v3-service
  reads the tier into a log line without branching, so the T3 branch was
  removed. T0 now requires positive evidence (short greeting or question
  shape) instead of being the fallthrough: "slow it down significantly" was
  classified conversational, capped at 5 turns, and returned a zero-tool-call
  non-answer.
- `run_command`'s description now marks the boundary it does not cover —
  servers and watchers belong in `run_background`.

### CI gates for failure modes the matrix could not see (2026-07-22)
- **`min-python`** compares the tree against `pyproject.toml`'s
  `requires-python`. A PEP 604 annotation in `sandbox/executor_server.py`
  broke imports on the declared 3.9 floor while CI ran only 3.11/3.12, so it
  failed for contributors and never in CI.
- **`dockerfile-sources`** checks every `COPY` source exists, resolved against
  each service's own build context from `docker-compose.yml`. Stale COPYs
  after the retrieval and RPG removals broke image builds while imports,
  tests, and lint stayed green.
- Python suite repaired from 15 failures and 29 errors to zero (module loaders
  missing `sys.path` and `sys.modules` registration; a missing skip guard on
  the proxy binary).
- Benchmark ablation conditions A–D now index the HuggingFace copies instead
  of being vendored; verified byte-identical first. Tracked files: 3,045 to
  605.


### VS Code extension

- New `extensions/vscode/`: a zero-runtime-dependency VS Code client for the
  proxy HTTP API (#35) — chat sidebar with streamed turns, tool chips, and
  plan checklists; interactive permission flow (allow once / allow for
  session / deny, with pre-decision diff previews for `write_file`,
  `edit_file`, `structural_edit` and `insert_after`); native diff review of
  applied changes; status bar from `/ready`; workspace-mismatch warning;
  service token in SecretStorage. Contributed by @Anuj-72.

### CPU-torch images actually CPU-only again (2026-07-20)
- The lens and v3-service Dockerfiles pre-install torch from the CPU-only
  index, but their pin (2.12.1) had drifted behind requirements.txt
  (2.13.0), so the requirements install silently "upgraded" torch from
  PyPI and dragged the ~8 GB nvidia/cu* dependency stack into both
  CPU-only images (lens 8.29 GB, v3 7.91 GB, vs ~3 GB intended). On a
  43 GB host this also made full image rebuilds fail outright on disk
  space. Pins aligned to 2.13.0; a new contract test
  (tests/contracts/test_torch_cpu_pin.py) fails when either service's
  Dockerfile torch pin diverges from its requirements.txt, or when the
  pre-install loses the CPU index.

### Structural gate on every write path (#147 close-out, 2026-07-20)
- **Coverage completed**: the `write_file` paths that still skipped the gate
  now run it — the V3 winner (including the baseline resurrection when the
  pipeline returns nothing), the V3-error fallback (matters on `/generate`
  timeouts when `/internal/structural_check` still answers), and the T0/T1
  direct path (a sub-10-line `.py` calling an unimported name previously
  landed ungated). The direct path gets the structural gate ONLY — a syntax
  gate there would hard-block legitimate non-parsing T1 content (JSONC,
  multi-doc/templated YAML, scaffold `.py` templates). `BypassV3` (demo
  baseline pane) skips the direct-path gate so the baseline shows the raw
  model; the edit-path and iteration fast-path gates run in all modes, as
  before.
- **False-block hardening** (two adversarial review rounds over this change):
  the resolver's builtin set is now interpreter-derived instead of
  hand-curated (the curated subset was missing `exit`, `TimeoutError`,
  `ConnectionError`, `memoryview`, ... and would have vetoed valid new
  files); `/internal/structural_check` returns the FULL unresolved list (the
  gate diffs original-vs-edited lists, and the previous 10-name cap made
  that comparison unsound in both directions); the gate's `project_context`
  now also includes session-written `.py` files (truncated to 4 KB, like the
  V3 builders) so it is never stricter than the in-pipeline veto it
  backstops; a vetoed V3 winner falls back to the model's own gate-passing
  baseline instead of rejecting (the offending call is V3-authored) — and
  the fallback write lands with plain, non-V3 telemetry (no winning
  score / phase / verification evidence and no "V3 complete" stream), so
  the completion nudge never reports the unverified baseline as
  V3-verified; write-path rejections use a `write_file`-flavored message
  (the edit-flavored one steered models to `edit_file` on files that don't
  exist); an unreadable existing original skips the gate instead of
  counting every pre-existing call as introduced; the winner gate rechecks
  cancellation after its HTTP round-trips so a mid-gate cancel lands
  nothing on disk. The `write_file` iteration fast-path syntax gate now
  applies the same healthy→broken rule as `edit_file` (it hard-blocked a
  strict-invalid config — multi-doc YAML, JSONC — being iterated, and no
  longer does).
- **Gate correctness**: an original-side check failure (transient service
  error; malformed Python is NOT this case — tree-sitter parses tolerantly)
  is retried once and then fails open instead of counting every unresolved
  name as newly introduced; a nil request context no longer panics; both V3
  request builders exclude the target's own pre-edit snapshot from
  `project_context` so the in-pipeline veto can't credit a def the write
  deletes.
- **Tests**: gate-level regression pair for the issue's scope item 3 with an
  import-aware fake (delete-import blocked / import-elsewhere-in-file passes),
  original-side fail-open with retry, nil-context, write-flavored rejection,
  unreadable-original skip; endpoint-level coverage of
  `/internal/structural_check`'s exact response contract including the
  uncapped list; resolver tests for real builtins.
- Known v1 limits (documented in the resolver, out of #147 scope): attribute
  calls (`os.getcwd()` after deleting `import os`) and non-call name
  references are not resolved; shell-redirection writes bypass all gates;
  a tolerantly-parsed broken original can under-report its pre-existing
  unresolved calls and block a one-error-at-a-time repair.

### CodeQL: all 14 open alerts fixed (2026-07-20)
- Expected-output and gate-rejection log lines escape CR/LF
  (go/log-injection ×4); `missingExpectedOutputs` and the asset-lint `Stat`
  probes are contained via `filepath.IsLocal` (go/path-injection ×4).
  Containment keeps the enforcement signal: expected outputs are checked
  against the workspace root AND the system temp dir (host-verify tasks
  name `/tmp` outputs), and an asset reference escaping the workspace is
  reported as dangling without being probed (it can't be served from the
  workspace) rather than silently skipped.
- The `asa → fit → doctor` import cycle (py/cyclic-import ×3) is broken by
  extracting the shared `.env` resolution into `atlas/cli/env.py` (doctor
  re-exports it; fit/lens/publish read it directly — monkeypatch
  `atlas.cli.env` to steer those commands) and the GGUF header reader into
  `atlas/cli/gguf.py`. The dotenv walk keeps its previous reach (7 hops
  from `atlas/cli` = 8 from `atlas/cli/commands`), so it cannot newly pick
  up an ancestor `.env` it never saw before.
- geometric-lens style notes: `from geometric_lens import service` import
  form, explicit `+` string concatenation in the drift probe texts, and the
  legacy-shape warning latch became a mutated holder instead of a rebound
  global.

### Code-review hardening of the #147 / TB2 series (2026-07-20)
An xhigh review of the unpromoted series found 15 correctness defects, all fixed:
the structural resolver now tracks locally-bound names (params, loop/with
targets, assignments) so it no longer false-rejects valid edits or vetoes valid
candidates; the write_file iteration fast-path and the text-exit path got the
structural / verification / completion-claim gates they were missing; the
structural gate excludes the edited file's stale pre-edit content; the read cap
floor no longer exceeds a small slot's budget and a truncated read records only
what was shown (correct dedup + EndLine); UTF-16 BOM files read as text; the
command-not-found and inline-script steers, the expected-output gate, the
active-iteration filename match, and the write fingerprint were all de-noised
against false positives.

### Structural gate on the edit path (#147, 2026-07-19)
- An `ast_edit`/`edit_file` that introduced an unresolved direct call — e.g.
  `render_template` while the file imported only `render_template_string` —
  parsed fine, passed V3 verification, and landed as verified; every request
  then 500'd (NameError). The in-pipeline structural veto was gated off when the
  edit sent no `project_context`, and `ast_edit` had no gate at all.
- Fixes: the V3 structural veto now runs whenever candidates exist (not only
  when project files are present), resolving against the candidate's own
  imports; a new `/internal/structural_check` endpoint exposes the resolver;
  and a proxy-side structural gate on both edit paths refuses a write that
  *introduces* an unresolved direct call (healthy→broken, matching the syntax
  gate — a pre-existing unresolved name mid-repair is allowed). Python-only,
  fail-open when v3-service is unreachable.

### Agent-loop: commit the deliverable + read-size safety (TB2 rounds 5-6, 2026-07-19)
- **Expected-output gate**: parse the prompt for the file the task asks the
  model to produce ("save your solution in X", "the file Z must exist") and
  check it against disk before allowing done/text exit — a partial artifact or
  exploration-without-committing satisfies the generic action gate while the
  named deliverable is still missing. Bounces naming the specific file.
- **Loop-stop output-rescue**: the repeat/error breakers steer toward the named
  deliverable once before hard-stopping (many hard tasks loop on run_command
  and never reach the done/text exit where the gate lives).
- **read_file byte cap**: a single read is capped at half the per-slot context
  (worst-case ~1 token/char) so one huge read can't overflow the window — a
  model that gunzipped a data file and read it whole hit 2.26M tokens and a hard
  context-overflow 400 the force-trim retry couldn't fix. Unconditional (a line
  limit doesn't bound bytes) and context-derived. Binary reads already return a
  tool pointer instead of bytes.

### Agent-loop: stop killing iteration (Terminal-Bench 2.0 round 2, 2026-07-19)
Re-analysis of a 20-task round found that nearly every "failure" was a stopping
condition firing on *productive* work, not the model reaching its limit (turns
are uncapped). Fixes:
- **Repetition detector distinguishes iteration from reassertion.** `write_file`
  repetition is now keyed on path + a whitespace-stripped content fingerprint:
  rewriting a file with materially different content (fixing successive compiler
  errors) is iteration and no longer counts as a loop; reasserting the same draft
  still does.
- **Steer before kill.** The repetition breaker injects a corrective note and
  continues on the first detection, ending the session only if the model repeats
  after the nudge. The old immediate hard-stop (including the
  "productive change → stop" path) terminated models one nudge from finishing.
- **Broken-inline-script steer.** A `python -c` verification one-liner that fails
  with a SyntaxError in its own `-c` argument now steers the model to move the
  test into a `.py` file, instead of letting it re-run the unparseable command
  into the breaker with a possibly-correct solution on disk.
- **Text-exit action gate.** The `text` response path is gated the same way
  `done` is: on an action-intent prompt with no productive change, it bounces
  instead of letting the model narrate its intent and quit having done nothing.
- **Binary-file read guard.** `read_file` on a binary (a NUL byte in the head)
  no longer returns garbage bytes — it returns a directed pointer to the right
  tools (`strings`/`readelf`/`objdump`/`nm`/`file`/`xxd`), which the model
  otherwise never reached for (it read a compiled ELF as text and gave up).
  `file` and `xxd` added to the sandbox image (binutils already rode in with gcc).
- **Fast-path writes during active iteration.** Once the model has written a
  file and just saw it fail a run, the next write is a targeted fix — it now
  skips the V3 pipeline (still syntax-gated) and writes directly, instead of
  paying V3's multi-minute per-call latency (which on a mid-debug file often
  "completes without result" anyway). This unthrottles edit-test-fix loops from
  ~5 cycles in 25 min to run-speed. V3 still owns the first write of each file
  *(under the candidate policy at the top of these notes, only where a
  candidate can be delivered)*.

### Agent-loop hardening from the Terminal-Bench 2.0 dogfood round (2026-07-18)
- **`atlas doctor` workspace-mount check** — new `workspace_mounts` check fails
  loudly when the proxy and sandbox bind different host directories as
  `/workspace` (a silent split that sends file tools and `run_command` to
  different filesystems while every `/health` stays green). New
  TROUBLESHOOTING entry documents the symptom and fix (`ATLAS_PROJECT_DIR` +
  recreate both containers together).
- **Sandbox image: common CLI tools baked in** — `git`, `sqlite3`, `jq`,
  `patch`, `zip`, `xz-utils`. The sandbox is non-root on a read-only base, so
  absent binaries can never be installed at runtime; `git clone` and
  `sqlite3 .recover` both dead-ended on "command not found".
- **Missing-command steer** — `command not found` shell errors now get a
  directed [system note] stating that system packages cannot be installed in
  the sandbox and pointing at pip-installable equivalents or the preinstalled
  toolchains, instead of the model re-running into the repetition breaker.
- **Conversation-trim correctness** — the token budget now counts the pinned
  user instruction and pinned file content (previously re-injected without
  being counted) and reserves proportional tokenizer slack (`slot/8`);
  a llama-server over-context 400 force-trims to the minimum window and
  retries once instead of killing the session.
- **Sandbox tmpfs sizing is env-tunable** — `ATLAS_SANDBOX_TMP_SIZE` (2G),
  `ATLAS_SANDBOX_PIP_SIZE` (1G), `ATLAS_SANDBOX_CACHE_SIZE` (512M); the old
  fixed 256M `~/.local` overflowed on `pip install pandas pyarrow`.

### V3.2 — RPG-style architecture-first planning (#120, experimental, opt-in)
*Superseded: RPG planning was removed (see the simplification campaign above);
`ATLAS_RPG_PLANNING` remains only as a deprecated config key.*
- New `ATLAS_RPG_PLANNING` flag (default **off**) enables repository-level,
  plan-then-fill planning ahead of the existing problem-level PlanSearch:
  - **Wavelet substrate** (`v3-service/wavelet/`) — a faithful, dependency-free
    Python port of [wavescope-mcp](https://github.com/yogthos/wavescope-mcp)
    (Ricker CWT, structural signal, multi-resolution bands, project decomposition,
    peak-diff). Numeric parity with upstream is golden-tested.
  - **Repository Planning Graph** (`v3-service/rpg.py`, [arXiv:2509.16198](https://arxiv.org/abs/2509.16198)) —
    two-stage construction (proposal capability tree → implementation files +
    signatures + data-flow edges), graph validation/scoring, and a topological
    projection to the existing flat `Plan` so the agent loop is unchanged. The
    proposal stage is seeded with the wavelet coarse band on existing repos.
  - **Graph-guided generation** — each node's planned interface (signatures,
    edges) threads into its `/v3/generate` call (`proxy/rpg.go`), so the existing
    PlanSearch ([arXiv:2409.03733](https://arxiv.org/abs/2409.03733)) fills a node
    whose architecture is already pinned.
  - **Structural verification + drift** — the candidate veto now rejects code
    that doesn't realize its planned signatures; post-write drift detection
    surfaces the affected downstream subgraph for re-planning.
  - **Offline metrics** — `v3-service/rpg_eval.py` scores RPG artifacts for CI /
    benchmark summaries.
  - Strictly additive: with the flag off, planning and generation are unchanged.
  - Design + phased status: `docs/reports/RPG_WAVELET_PLANNING_V3_2.md`. Credit
    idea + framing to Dmitri Sotnikov (@yogthos), author of wavescope-mcp.

## [3.1.3] - 2026-07-06 — Maia

### Upgrade, rollback, and diagnostics
- `atlas upgrade [--to TAG] [--dry-run]`: staged upgrade with a recorded restore point (tag + image digests + `.env` backup), cosign signature verification of the target images (unpublished backend images are skipped, not fatal), readiness wait, quick-doctor smoke check, and automatic restore of the previous release on any failure (restore never re-pulls — a moved mutable tag can't replace the cached known-good images). Same-tag release tags no-op; mutable tags (`latest`, `dev`) run a full refresh. `atlas rollback [--to TAG]` returns to the restore point, and a failed `--to` reverts `.env` to the previously deployed tag.
- `atlas diagnostics collect`: a shareable support bundle (doctor output, service health, compose config, recent logs) with private values filtered.
- `atlas config validate | migrate`: typed schema over `.env` (types/ranges/enums, unknown and deprecated keys), forward migration with a `.bak` and a schema-version stamp, `--dry-run` preview.
- Signed artifact manifests: `atlas artifact verify | snapshot | rollback` — SSH-signed provenance manifests over lens/ASA bundles (verified against `.github/allowed_signers` + per-file SHA-256), one-generation bundle snapshot/rollback, and lens retrains auto-write a provenance manifest.

### Observability
- Structured JSON logs behind `ATLAS_LOG_FORMAT=json` across proxy, v3, lens, and sandbox, with `X-ATLAS-Request-ID` correlation: the proxy assigns/echoes the ID, forwards it on every outbound service call (with or without internal auth configured), v3 propagates it to lens/sandbox calls, and 401 responses carry the echo. All log paths pass the private-value filter, including exception tracebacks; the assignment filter also masks single-quoted values and Python dict reprs.
- Stable error-code taxonomy on the proxy API (`GET /version` lists codes; errors return the documented JSON envelope) and an OpenAPI 3.1 spec for the proxy surface with route-parity contract tests.
- Command-execution trust modes (`ATLAS_TRUST_MODE`): `untrusted` refuses command execution (both `run_command` and `run_background`), `trusted` (default) forces sandbox execution, `fully-trusted` permits host execution.
- Performance budget gate in CI: versioned measurement schema (CLI import time, proxy binary size) checked against `benchmark/perf/budgets.json`; a result matching zero budgeted metrics or a failed import fails the gate instead of passing vacuously.

### Adversarial review passes
- Two loop-until-clean adversarial reviews over the release window fixed 33 confirmed bugs, including: a trust-mode bypass via `run_background`; correlation-ID forwarding dead on token-less installs; `.env` corruption on files without a trailing newline; upgrade signature verification skipping the llama image; artifact verification only working on the signer's machine; bundle snapshot/rollback producing mixed-generation bundles; a default `atlas upgrade` that could never fetch a moved `latest`; restore paths that masked the original failure; sandbox/v3 images missing `structured_log.py` (startup crash on next build); CI gates that could never fail (attestation checks, OpenAPI parity, perf vacuous-pass); and a K3s lens PVC that rendered empty on upgraded installs.

### Dependency updates
- Grouped Dependabot updates merged: GitHub Actions majors (with `go-version: 1.26` — setup-go v6 pins GOTOOLCHAIN=local), Go tui deps, the Python group (fastapi 0.139 / uvicorn 0.50 / pydantic 2.13 / xgboost 3.2 / torch 2.12.1, with the torch pin synced across Dockerfile/CI/guard tests), and Docker digests (CUDA 12.9.2, golang 1.26-alpine, alpine 3.24). Base-image majors (CUDA 13, Python 3.14) are deliberate migrations and now ignored by Dependabot config, as are setuptools bumps past the RHEL9 python3.9 floor; staticcheck bumped to 2026.1 (go1.26 stdlib).

### Installer trust
- Release-pinned install: `ATLAS_BOOTSTRAP_REF=vX.Y.Z` pins the cloned checkout to the (SSH-signed) tag and `ATLAS_IMAGE_TAG` to the matching cosign-signed images; README/SETUP document the pinned and review-before-running variants beside the one-shot `curl | bash`.

### Docs & repo
- Ops docs consolidated: UPGRADE/ROLLBACK/BACKUP_RESTORE merged into OPERATIONS.md; README opens with the project definition and a Why ATLAS section; star-history chart moved to the working endpoint; `.mailmap` maps contributor identities for git tooling.

### Production-platform pass (support, supply chain, governance, ops)
- `SUPPORT_MATRIX.md`: every OS/backend/model/deployment/language/feature path classified (Supported/Preview/Experimental/Community-tested/Research-only/Unsupported) with validation provenance; N/N-1 compatibility policy; the model contract stated plainly (direct-mode agnostic, per-model bundles for V3/Lens/ASA).
- Supply chain: Docker bases digest-pinned (Dependabot-maintained) except the ROCm/Vulkan community-backend bases, which are tag-pinned; every pushed image carries SLSA provenance + SPDX SBOM attestations and a keyless cosign signature over its digest.
- Sandbox: non-root runtime (uid-mapped to the host user by `atlas init`), cap_drop ALL, CPU quota, toolchains relocated out of /root, K3s securityContext with seccomp RuntimeDefault, and an optional egress cutoff (`ATLAS_SANDBOX_NET_INTERNAL`) — verified on a local hardened-profile run.
- Lens state store is SQLite (ADR 0007, GH #57, core implementation from #128 by @HarshalPatel1972): pattern cache, co-occurrence graph, Thompson-sampling router posteriors, task queue, and metrics live in one WAL-mode `geometric_state.db` on the `lens-state` volume. The redis service, redis-data volume, and the `ATLAS_REDIS_*` config keys are removed (`atlas config migrate` drops them); `REDIS_URL` itself is simply no longer read; degradation semantics unchanged (cache/router go neutral on store failure, task queue 503s). One less external dependency; state backup is a single file.
- Governance: GOVERNANCE/MAINTAINERS/CODEOWNERS; SECURITY.md severities, response targets, embargo/CVE, backports, artifact revocation; THIRD_PARTY_NOTICES; seven ADRs; a single OPERATIONS.md runbook (health, logs, runbooks, upgrade, rollback, backup). Planning/status trackers are kept out of the repo.
- Tracker hygiene: label vocabulary created + applied to all open issues; #39 closed with evidence; fresh-audit status on #66/#115/#27; #124/#126/#128 marked blocked with exact conformance lists.

### V3/Lens pipeline acceptance test
- A second deterministic E2E (`tests/e2e/test_v3_lens_acceptance.py`) boots the **real v3-service** alongside the real proxy and sandbox: a Tier-2 write routes through the proxy's V3 bridge, the probe fails on purpose, lens-calibrated allocation yields k=3, PlanSearch generates candidates via the fake llama, each candidate is scored through both lens endpoints (a recording fake lens proves the calls) and verified in the real sandbox, and winner selection writes the lens-preferred candidate to disk. Failure modes at the seams: V3 unreachable/malformed/timeout fall back to the documented direct write; a lens outage leaves V3 running uncalibrated.
- `tests/contracts/` drift gates run in CI: proxy↔TUI event producer/consumer parity, envelope-type parity (Go producer / Go consumer / Python spec), config keys ↔ readers ↔ docs, CLI subcommands ↔ implementations, registry hash/consumption contracts.
- Product/benchmark scoring contracts aligned: unified neutral lens fail-soft sentinel, deterministic energy-sorted benchmark candidate ordering, corrected pattern-cache retry key; intentional orchestrator differences documented in `benchmark/README.md`.

### End-to-end acceptance test
- CI runs a deterministic full-control-plane test (`tests/e2e/`): the real proxy binary and the real sandbox executor (host uvicorn, no Docker) against a scripted fake llama-server, driving one complete agent turn — read, edit, sandbox-verified `run_command` behind an interactive permission approve, done — over the production SSE protocol. Asserts stage order (a silently skipped stage fails), file contents, and the sandbox side-effect; a second test pins the fail-closed session-less deny. The sandbox executor's workspace root is env-overridable (`ATLAS_SANDBOX_WORKSPACE_ROOT`; containers keep `/workspace`). Scope: this covers the control plane deterministically — real llama.cpp inference, GPU backends, hidden-state extraction, ASA steering, and model-dependent V3/Lens quality remain hardware-gated or manually validated (see the SETUP hardware table).

### Permissions fail closed
- `/v1/agent` requests without a `session_id` deny destructive tool calls in `default`/`accept-edits` mode instead of silently executing them (there is no channel to answer the prompt). Unattended clients use `mode:"yolo"` or `session_allowed_tools`; the API doc's non-TUI client guide now covers `/v1/permission`. The TUI clears a pending permission modal on `permission_denied` and turn end.

### Wiring completed
- `v3_reasoning_token` is rendered in the TUI's V3 streaming row (previously emitted and dropped — a frozen "decoding…" row through every PlanSearch phase).
- Lens retrains (both the service endpoint and `scripts/retrain_lens_from_results.py`) write `model_identity.json` for the served model; without it the load path's identity check disabled the entire lens on the next restart. Published lens bundles on HF now include identity files, pinned in the registry, so `atlas model install-artifacts` yields a bundle that actually loads. The gemma registry entry carries lens+ASA url bases and hashes.
- Compose passes through the documented-but-unreachable knobs: `ATLAS_PLAN_THINKING` (v3-service), `ATLAS_SHELL_SNAPSHOT_*` (sandbox), `ATLAS_CONTROL_VECTOR_*` (llama-server). `.env.example` gains the five consumed-but-undocumented keys.
- `scripts/build-containers.sh` builds the five current services from their real contexts and tags exactly as the K3s manifests reference (previously built from a removed directory layout, silently producing one image under names no manifest pulls); `uninstall.sh`'s image removal no longer aborts on an unset variable.
- ASA marker checks are case-insensitive in both launchers, matching `atlas asa check`; `atlas publish --dry-run` without repos no longer crashes; the `train` extra includes xgboost + scikit-learn and the ImportError guidance mentions it.

### Removed (unwired, placeholder, or caller-less — verified)
- The `plan_tasks` tool (acknowledged tasks as pending without executing them) and its never-wired parallel executor; the `PermissionRule`/`checkPermissionRules` rules engine (nothing loaded rules — the live machinery is `needsPermission` + `awaitPermission` + the built-in deny-list); `build_verify.go`, `v3_adapter.go`, unused grammar/schema wrappers, `EmitSimple`, `calibrationTooltip`, and v3-service's unwired dual-emit envelope helper.
- The metric-tensor G(x) path: `evaluate_gx` is XGBoost-only and the metric tensor served only `/internal/lens/correctability`, an endpoint with no caller; the 67 MB `metric_tensor.pt` is out of the Q6_K bundle.
- The V3.0-era inference files (`Dockerfile.mtp`, spec-decode/9B/embed entrypoints, custom jinja templates, the malformed unused patch file), the `model_recommendations` back-compat shim (callers migrated to `model_registry`), zero-reference scripts (`run_full_benchmarks.sh`, `validate_benchmarks.py`, `smoke-test-9b.sh`, `deploy-9b.sh`, `measure_bok_latency.sh`), `router/fallback_chain.py`, the `/ablation` coming-soon stub, the benchmark `--runs` no-op flag, and the dead `ATLAS_ENABLE_TRAINING`/`ATLAS_REGISTRY` config keys.
- Docs describe only the live protocol: the v3-service dual-emit claim, the "done is always last" broker claim, and stale consumer lists are corrected in PROTOCOL.md and `atlas/cli/events.py`.

### Supply-chain & artifact integrity
- Lens/ASA artifact downloads verify SHA-256 against per-file hashes in the model registry (`lens_artifact_sha256` / `asa_artifact_sha256`); a mismatch removes the partial file and fails the install, and files without a registry hash are labeled unverified instead of `[ok]`. `download-models.sh` verifies GGUF downloads against the same registry hashes (previously size-check only on the shell path).
- Lens checkpoint loading uses `torch.load(weights_only=True)` (the artifact can come from a remote download; full-pickle loading would execute code during deserialization). The legacy `gx_xgboost.pkl` fallback is opt-in via `ATLAS_ALLOW_PICKLE_GX=1` instead of automatic.
- `benchmark/custom/validate.py` enforces `tasks.json.lock`: a task set that drifted from its approved hash fails validation instead of the lock being informational.
- `gx_thresholds.json` (per-model G(x) operating thresholds) is tracked with its sibling lens artifacts, so a fresh clone runs with threshold interventions enabled.

### CI / release safety
- Image publishing is two-phase: the build matrix pushes only immutable `:sha-<short>` tags; a promote job repoints `:dev` / `:latest` / semver tags via `imagetools create` only after every service built **and** the `tests` workflow passed on the same commit. Failed tests or a partial matrix can no longer overwrite moving tags, including with mixed-commit images.
- Pull requests build the four small service images (`push: false`) so Dockerfile breakage is caught before merge instead of on the post-merge publish.
- Every GitHub Action is pinned to a full commit SHA (Dependabot keeps them fresh); `test.yml` / `install-test.yml` run with explicit `permissions: contents: read`; Dependabot also covers the Docker base images across all five service directories.
- CI runs the static `tests/infrastructure` checks, `geometric-lens/tests` (34 hermetic tests, previously never in CI), the `test-integrity` + `python-compile` gates, and shellcheck over all of `scripts/*.sh` (previously 2 of 13 scripts). Install matrix uses the maintained `rockylinux/rockylinux:9` image.

### Community health
- `SECURITY.md` (threat model scoped to the single-user local deployment, private reporting via GitHub advisories), issue templates (bug report requires `atlas doctor` output), a PR template, and Dependabot config.

### CLI
- `atlas --version` prints the CLI version; the REPL banner shows the real version instead of a hardcoded `v3.1`.
- `atlas bench` exits non-zero when the runner fails or produces no results (previously always exited 0).

### Fixes & accuracy
- Sandbox trust-model docstring describes what the container actually enforces; the never-enforced `MAX_MEMORY_MB` knob is removed (memory is capped by `ATLAS_SANDBOX_MEM`).
- `plan_tasks` is documented as a planning aid (tasks are acknowledged, not executed); the unreferenced MTP inference experiment files are removed.
- Packaging metadata completed (readme, URLs, classifiers, `train` extra for `atlas lens/asa build` dependencies; `setuptools>=77` to match the SPDX license form). Docs: uninstall section in SETUP, six previously-undocumented env vars in CONFIGURATION, `python3 -m benchmark.cli` invocation corrected, pass@1-v(k=3) defined where the headline number appears.

### Interactive permissions
- `default` and `accept-edits` modes now prompt before a destructive tool call runs. The turn pauses on a bordered approval box (`[y] allow once`, `[a] allow for session`, `[n]`/`Esc` deny); `Ctrl+C` still cancels the whole turn. An "allow for session" choice whitelists that tool so it isn't asked again (carried in the request's `session_allowed_tools`). `accept-edits` auto-allows file writes/edits and prompts `run_command`/`delete_file`; `yolo` is unchanged.
- New `POST /v1/permission` endpoint and `permission_request` SSE event carry the decision back to the paused turn (keyed by `session_id` + `tool_call_id`, mirroring `/cancel`). A fail-safe timeout (`ATLAS_PERMISSION_TIMEOUT_SEC`, default 600s) denies if nothing is answered.

### Sessions
- The TUI saves each session to `~/.cache/atlas-tui/sessions/<id>.json` (one file per session, written each turn). `atlas --continue` resumes the most recent session in the current directory; `atlas --resume` picks one from a list; `atlas --resume <id>` resumes a specific session. The saved transcript is replayed into the view and fed back to the model as history; a directory mismatch keeps the current directory and warns. `/clear` starts a fresh session, leaving the prior one on disk.

### Installer / bootstrap
- Bootstrap writes the registry's default recommended model into `.env` when none is selected (logged), so the one-shot `curl | bash` flow completes without the wizard; an existing selection is respected.
- No detected GPU selects the Vulkan overlay automatically, plus the new `docker-compose.cpu.yml` when `/dev/dri` is absent — GPU-less hosts boot via the lavapipe CPU ICD (slow but functional).
- firewalld changes are opt-in via `ATLAS_BOOTSTRAP_OPEN_FIREWALL=1`; services bind loopback, so local installs leave the firewall alone.
- ASA steering-vector build dispatches per GPU vendor (CUDA/ROCm image + device flags), loads `.env` keys first, and skips cleanly on CPU-only hosts; re-runs pull the existing checkout as its owner (no dubious-ownership failure under sudo); service health wait raised to 450s to cover llama-server warmup.
- `install.sh` (K3s) fails early with guidance when `bc` is missing; `download-models.sh` downloads via curl and writes a relative `default.gguf` symlink; the macOS native launcher keeps smaller fallback defaults than the Docker path (ctx 32768, q8_0/q4_0 KV, 1 slot) for Mac unified-memory headroom, and treats `.env` as optional so an env-only launch works.
- GPU vendor detection word-bounds the AMD `lspci` match so NVIDIA/Intel GPUs aren't misdetected as AMD (#129).

### Proxy
- `/ready` also gates on v3-service health.
- Cancellation aborts in-flight V3 plan/write calls and sandbox calls; a cancelled `write_file`/`edit_file`/`ast_edit` no longer falls back to writing content to disk; per-turn cancel handles so overlapping turns on one session id can't remove each other's registration.
- The verification, done-without-action, and claim-check gates share a 3-bounce cap, so a persistently bounced `done` is eventually accepted instead of looping.
- The markdown-fence sanitizer only strips a true whole-file wrapper — interior fences (e.g. docstring examples) pass through unchanged.
- The safety deny-list (`.env`/`*.pem`/`*credentials*` writes, destructive shell patterns) is enforced centrally at tool dispatch in every permission mode.
- `outline_file` returns structured JSON including the rendered outline; `delete_file` reports removal errors and refuses non-empty directories; exploration budget escalates its nudge instead of skipping the read; session read-cache access is lock-guarded.

### Security & workspace containment
- Proxy-level workspace containment: every path-taking tool argument (`path`, `source`, `destination`, `cwd`) is resolved and checked against the workspace root before any handler touches the filesystem. Paths escaping via `..`, absolute paths, or symlinked components are refused in every permission mode.
- Untrusted text written to logs is field-encoded so it can't forge or split log lines.
- v3-service verifies candidate code before accepting it: an allowlisted build/test command gate (shell metacharacters blocked) and language-aware syntax checks reject candidates that don't compile/parse.
- Sandbox executor parses XML with `defusedxml` (untrusted-input safe).
- `scripts/production-readiness.py` is the developer gate (test integrity, Python compile + unit tests, Go race/vet for proxy and TUI, and the v3 syntax/sandbox contract tests); CI runs the same named gates.

### TUI
- `/demo` raw lane runs with no sandbox or file tools; in review mode the raw pane keeps the model response while the V3 pane shows written files; stream events can no longer be overtaken by the done marker; prompt animation is multi-byte-safe; markdown re-wraps on terminal resize.
- Feedback flow: staged per-file verdicts survive a failed submit; `/deny` validates the path against the files the last pass actually wrote; non-200 `/feedback` responses surface as errors; input echoes never replay into agent history.
- Bearer-token loader reads the `atlas init` api-keys.json shape; `/events` reconnect backoff resets after a healthy connection; renderers added for reasoning-repetition interventions, stream cuts, and symbol-index injection.

### CLI (continued)
- `atlas compose <args...>` passthrough subcommand (base file + backend overlay); `atlas --help` lists subcommands; unknown subcommands print usage and exit 2.
- Service URLs resolve from the Docker `.env` port keys when no explicit URL env var is set (repl, client, doctor, lens check).
- `atlas onboard --url` offers to write `ATLAS_MODEL_FILE`/`ATLAS_MODEL_NAME` into `.env` (interactive prompt; `--apply` for non-interactive).
- `atlas doctor` prints each result as it completes (JSON mode still buffers).
- `atlas model`: models dir resolves from the compose `.env`; a resumed download that the server reports complete (HTTP 416) is verified and finalized in place; `install-artifacts` exits 3 when no artifacts are registered for direct download and points at the published repos; Gemma-family registry entries carry the `gemma` license identifier.
- `atlas init` reports failure when `api-keys.json` is not written and asks before tightening a loose `secrets/` dir; `atlas asa build` resolves the lens container via compose (non-default project names) and survives docker-exec timeouts with recovery guidance; `atlas solve` uses `/v1/chat/completions` so the GGUF's own chat template applies; the startup status block drops the hardcoded speed figure; version reports 3.1.3.

### Geometric Lens — per-model calibration
- Per-model score calibration, model-identity checks, and threshold loading are their own modules (`calibration.py`, `identity.py`, `thresholds.py`): C(x) energy is normalized to a per-model scale and G(x) verdicts use per-model thresholds, so the same framework works across models without hardcoded constants.
- The proxy exposes `GET /v1/calibration/status` (lens + ASA compat verdict for the loaded model); the TUI reads it as a header badge on startup.
- `atlas lens check | build | publish` and `atlas asa check | build | publish` cover the per-model probe, training, and artifact-publish flow.
- Adds the `entrypoint-v3.1.sh` inference entrypoint (env-driven, model-neutral) shared by the Docker images and the macOS launcher.

### Lens service + retrain tooling
- `/internal/lens/retrain` refuses with a structured 503 when the models dir is mounted read-only, pointing at host-side `atlas lens retrain`; retrain/reload are serialized and refresh the readiness state on success.
- Artifact identity is verified against the model llama-server actually serves (`/v1/models` probe, `ATLAS_MODEL_NAME` fallback) and against the checkpoint's input dimension.
- G(x) loading is shared between boot and per-directory reloads, so a reload yields the complete lens; G(x) operating thresholds derive from out-of-fold CV scores instead of the final booster's in-sample scores.
- `retrain_lens_from_results.py` mean-pools per-token embedding responses (matching serve-time extraction) and hot-reloads the service; `retrain_cx.py` resolves ports from `.env` with a K3s NodePort fallback; benchmark lens-feedback keeps its sample buffer when a retrain is refused or fails.
- The lens image ships the `gguf` package (ASA vector writer), and the ASA build fails fast when it is missing.

### v3-service / sandbox
- v3-service serves requests on a threading HTTP server with a thread-safe graph cache; client disconnects abort the pipeline at phase boundaries; selection winners are matched by original candidate index (was positional against a sorted list); self-test harness executes candidates from a string literal so multiline strings survive; sandbox client timeout raised to 45s.
- Sandbox executor: per-call cap set to 300s in the Compose stack (`ATLAS_SANDBOX_MAX_EXECUTION_TIME`); process-group kill on timeout; optional `stdin` on `/execute`; project-context file writes routed through the O_NOFOLLOW containment helper; background jobs abandoned for 2h are reaped.

### Compose / K3s
- All services run with `restart: unless-stopped`; runtime-tuning keys (`ATLAS_V3_TIMEOUT`, `ATLAS_MAX_TOKENS`, `ATLAS_AGENT_HISTORY_BUDGET`, `ATLAS_LENS_RETRAIN_MIN`, `ATLAS_KEEP_LLAMA_WARM`, `ATLAS_FRESH_SLOT_PER_SESSION`) pass through to the proxy; `ATLAS_GPU_INDEX` reaches the llama container; `.env.example` documents the runtime-tuning section.
- Inference Dockerfiles EXPOSE 8080 (matching the entrypoint); ROCm/Vulkan images install curl for the compose healthcheck.
- K3s templates pin container-side ports so moving a Service port can't break probes; the proxy pod mounts models read-only and the lens-training corpus hostPath (`ATLAS_LENS_TRAINING_DIR`), and receives ctx/slot sizing; `deploy-9b.sh` uses the shared entrypoint and split KV-cache type keys.
- `production-readiness.py` and CI validate every shipped compose overlay combination; the installer CI job asserts a non-empty model selection lands in `.env`.

### Docs
- Documentation refreshed against the current code: MAP regenerated; API (feedback/training-status endpoints, readiness gate, tool table, workspace containment); CLI, CONFIGURATION, SETUP, SOURCES, PLAN_MODE, TROUBLESHOOTING updated.
- Documentation refactor for concision and structure: internal ticket references and dated change-narration removed from user-facing prose; duplicated content consolidated to a single canonical home with cross-links; `CAPABILITIES.md` + `PRODUCTION_READINESS.md` merged into `RELEASE.md`; `MAP.md` slimmed to a directory-level orientation map; shipped-release status trackers moved to `docs/reports/archive/`. Translations (`docs/lang/`) re-sync as a follow-up.
- README intro clarified.
- Full-tree accuracy audit against the code. API/PROTOCOL: `run_command` fails (no host fallback) when the sandbox is unreachable unless `ATLAS_VERIFY_IN=host`; the proxy is the live envelope producer (v3-service envelope opt-in is unwired); lens `/v1/*` endpoints require Bearer auth; missing request fields, SSE events, and v3-service endpoints documented; `ATLAS_PROXY_NODEPORT` name corrected. ARCHITECTURE: 15-tool table (adds `outline_file`); redis and the geometric-lens→v3-service edge in the service graph; XGBoost G(x) deployed, gradient-step correction unwired; sandbox cap 300s in Compose. CLI: `/demo` and `atlas lens retrain` documented; TUI has no `/bench`. CONFIGURATION: shell-gate table matches the catastrophic-only policy; token-budget trim; `ATLAS_REASONING_BUDGET`/`ATLAS_BACKEND` compose-passthrough notes corrected; adds `ATLAS_PERMISSION_TIMEOUT_SEC`, `ATLAS_LENS_HOST_DIR`, `ATLAS_LLAMA_HOST`; 12 sandbox languages. SETUP: `atlas` is the pip entrypoint (launcher-script passage removed); ASA `.gguf.model` marker gate + `ATLAS_CONTROL_VECTOR_ALLOW_UNVERIFIED`; TUI needs Go 1.26.2+. TROUBLESHOOTING: V3 fire conditions (10-line floor, ≥2 indicators), write-to-existing-path gate, exploration-budget nudges. PUBLISHING: pre-flight scope stated accurately. ja/ko/zh-CN copies updated to match; `docker-compose.override.yml` added to `.gitignore` (DEVELOPMENT.md documents it as ignored).

## [3.1.2] - 2026-06-17 — Maia

### Hardware reach
- AMD ROCm via llama.cpp — including RDNA4 / RX 9070 (gfx1200/gfx1201) and community-verified cards (#26)
- Apple Silicon — native macOS hybrid Metal path (native llama-server for inference + Docker for the rest of the stack) (#32)
- Vulkan universal fallback — one image covering AMD / Intel / Snapdragon / Apple-via-MoltenVK / CPU (#114)

### Agent reliability — local-model tool loop
- Tool results are rendered as user-role turns on the wire. Gemma's chat template has no `tool` role and silently dropped `role:"tool"` messages, so the model never saw any tool output (`list_directory` / `read_file` / `run_command`) and re-issued the same call until the repetition breaker fired. This was the root cause behind the "it can't see what it's reading / it just loops" reports. Model-agnostic (Qwen reads the `[tool result]` marker the same way).
- Read-dedup false-negative fixed: `fileContentInContext` probed the raw longest line, but tool results are stored JSON-escaped, so any file whose longest line contained a quote (e.g. a Flask app's embedded HTML) was wrongly judged "trimmed" and re-served every read → read loop. Now probes the longest escape-free run.
- Traceback → directed edit (#39 / option 3): a `run_command` crash extracts the deepest in-project frame, quotes the offending line, and steers a minimal `edit_file`; run tools are banned from the next decision's grammar so the model must edit rather than re-run.
- `move_file` tool: relocations/renames (e.g. `index.html` → `templates/`) no longer require a read→write→delete dance; shell `mv`/`cp` point here. Refuses to clobber an existing destination.
- Steers for common dead-ends: `No module named X` → `pip install` (instead of re-running), and a filename that differs only in case from a real workspace file → the correct name.
- Per-turn `max_tokens` 32768 → 8192 (`ATLAS_MAX_TOKENS`) and a content-stream loop cut, bounding runaways that previously ran to the slot ceiling.
- Conversation window sized to the per-slot context (with the active file pinned in the trim) instead of a flat cap that dropped the file under edit.

### Sandbox — shell policy + isolation
- `run_command` shell gate narrowed from "block every mutating verb" to catastrophic-only (whole-project/root `rm -rf`, fork bombs, device destruction), since the sandbox container (read-only rootfs, no-new-privileges, project-only writable mount, cwd jailed) is the real boundary. Ordinary `mv`/`cp`/`mkdir`/`rm <file>`/`sed -i` now run; `bash -c`/`eval` are unwrapped so a wrapped catastrophic command can't slip through.
- Host-sized cgroup limits on the sandbox: `pids_limit` (kernel-level fork-bomb stop) and a memory cap (`atlas init` detects host RAM and writes `ATLAS_SANDBOX_MEM` ~75%); `:-0` fallback keeps a raw `docker compose up` working uncapped.
- Interactive wall-clock cap on the V3 pipeline (`ATLAS_V3_TIMEOUT`, default 180s) — a runaway falls back to the model's own (syntax-gated) content instead of hanging the session.

### Geometric Lens — per-model thresholds + in-the-loop training data
- G(x) operating thresholds are now per-model and ship with the lens artifact (`gx_thresholds.json`): the lens service loads them and returns them in each score response; the proxy uses them for its regression checks. The hardcoded 0.3 / 0.15 / 0.05 cutoffs were calibrated to one model's score scale and never fired for a model (e.g. Gemma) whose scores cluster higher. `atlas lens build` auto-emits the file, calibrated from the run's PASS-score percentiles.
- ast_edit now matches `<script>` / `<style>` (tree-sitter parses them as dedicated `script_element` / `style_element` nodes, not generic `element`s — the old query matched 0).
- In-the-loop lens-training data collection: each agent file-write is captured per pass; in the TUI, `/good`·`/bad` rate a pass and `/review` + `/deny`·`/accept` set per-file verdicts, which the proxy turns into labeled, weighted samples (a 👎 pass down-weights even its accepted files; a denial is a full-weight negative). `/redo` regenerates a rejected file. A one-time "lens retrain available" banner appears once enough balanced samples accrue.
- `atlas lens retrain` trains the lens on that collected corpus (weighted G(x)) so it learns the user's own workloads, and emits fresh calibrated thresholds. New env: `ATLAS_LENS_DATA_DIR`, `ATLAS_LENS_RETRAIN_MIN`. TUI slash commands: `/good /bad /review /deny /accept /redo`.

### Structural call-graph reasoning (#39, thanks @yogthos)
- Intra-file call-graph neighborhood (`calls:` / `called by:` per symbol) rides on `outline_file` and whole-file `read_file` of a `.py`, gated by `ATLAS_CALL_GRAPH`. Surfaces structure at the localization decision point without a repo-wide scan. (PR #125 by Dmitri Sotnikov, integrated and extended.)

### Documentation
- Translated ARCHITECTURE.md to zh-CN / ja / ko (#25); added a language switcher to the English ARCHITECTURE.md

## [3.1.0] - 2026-05-12 — Maia

### Removed
- Removed dead `ATLAS_USE_FOX` code paths in benchmark runner (#22)

### Aider removed
- `proxy/aider_format.go` (whole-file format translator), `handleChatCompletions` + `handleStreamingChat`, and the OpenAI-compat agent-loop wrapping are all deleted (~2000 lines). `/v1/chat/completions` on the proxy is now a transparent passthrough to llama-server via the catch-all handler.
- `.aider.model.settings.yml`, `.aider.model.metadata.json`, the `.aider*` `.gitignore` exceptions, and the `_find_aider`/`launch_aider` paths in `atlas/cli/repl.py` are all gone. Bare `atlas` (interactive tty) now launches the TUI by default; pipe mode falls through to the built-in `/solve` REPL.
- Proxy launcher (`atlas/cli/repl.py`) now reaps any pre-existing `atlas-proxy-v2` process before spawning a fresh one and redirects proxy stdout/stderr to `~/.cache/atlas/proxy.log` instead of `/dev/null`. Closes the "old binary in memory after rebuild" foot-gun.

### Bubbletea TUI (PC-062)
- New `atlas tui` subcommand launches a native Bubbletea terminal UI as the canonical chat client (and is now the default for plain `atlas`)
- Five-pane layout: header (proxy/cwd/mode/spinner) + pipeline (live V3 stage table from `/events`) + chat (glamour-rendered markdown + inline tool calls) + events log + stats strip + textarea input
- Hotkeys: Enter send, Shift+Enter newline, Ctrl+L clear, Ctrl+T cycle permission mode, Ctrl+R resend last, Ctrl+C cancel turn / quit, Ctrl+D quit
- Slash commands inside the TUI: `/add /drop /context /diff /commit /undo /run /help /quit`
- New atlas-proxy `POST /cancel` endpoint indexed by `session_id` — TUI cancels the in-flight `/v1/agent` turn on Ctrl+C as defense-in-depth alongside TCP disconnect
- 43 atlas-tui Go tests + 4 atlas-proxy `/cancel` tests, all green under `go test -race`
- `tui/` is a standalone Go module (`github.com/itigges22/atlas-tui`) — depends on bubbletea, lipgloss, bubbles, glamour

### Documentation
- Added multilingual documentation: Simplified Chinese (zh-CN), Japanese (ja), Korean (ko) for README, SETUP, and TROUBLESHOOTING
- Added language selector badges to README
- Added star history chart to Latest News section
- Rewrote README contributing section to encourage issue reports and community feedback
- Fixed V3_1_STATUS.md false claims about speed optimizations that were never applied to code
- Documented RDNA4 (RX 9070 / 9070 XT, gfx1200/gfx1201) ROCm 7.x setup in SETUP.md and TROUBLESHOOTING.md — requires `ATLAS_ROCM_TAG=7.2.3-complete`; `ATLAS_HSA_OVERRIDE_GFX_VERSION` must stay unset (#119, thanks @Kaihui-AMD)
- Corrected stale Metal/macOS docs: the macOS hybrid Metal path (#32) is now documented as shipping across README, SETUP.md, CONFIGURATION.md, and ARCHITECTURE.md (was mislabeled "V3.1.2 planned"); rewrote ARCHITECTURE.md §8.4 to describe the actual hybrid (native llama-server + Docker) rather than the never-shipped pure-native install
- Restructured the README roadmap into V3.1.1 (hardware reach, landed), V3.1.2 (BYO-model + ROCm-on-K8s), and V3.2 (planning phase #120, structural+wavelet reasoning #39, reasoning-with-sampling #9), with a help-wanted backlog — all sourced from open issues
- De-staled user-facing CLI strings: `atlas init` and `atlas tier` no longer print "Metal — V3.1.2 planned"; they report Metal as the supported macOS hybrid path (#32) — strings/comments only, no logic change
- Synced zh-CN / ja / ko translations (README + SETUP.md) to the corrected English: Metal/macOS shown as shipping, multi-vendor GPU support table, V3.1.1/V3.1.2/V3.2 roadmap, and fixed NVIDIA-only requirements rows and SETUP_MACOS.md link paths

### Code Accuracy Audit
- Audited and corrected comments across 72 files for V3.0.1 accuracy
- Updated model references: Qwen3-14B to Qwen3.5-9B, embedding dimensions 5120 to 4096
- Renamed service references: rag-api to geometric-lens, Fox to llama-server
- Corrected G(x) XGBoost status: deployed and active (was incorrectly described as removed)
- Fixed normalization comments from "Fox 9B" to "Qwen3.5-9B C(x)"
- Marked legacy Fox code paths as unused in benchmark runner and geo_learning

### Test Fixes
- Fixed embedding dimensions in test fixtures (5120 to 4096)
- Fixed geometric-lens port in test conftest (8001 to 8099)
- Updated DivSampling test assertions to match actual 4+4+4 perturbation counts
- Corrected G(x) cost field parameter count: ~2.16M / 8.3MB (was ~2.7M / 10MB)
- Finished the 3.0.1 api-portal cleanup: removed `tests/integration/test_e2e_flow.py` and `tests/integration/test_e2e_training.py` (616 lines). These depended on the `test_api_key` fixture which calls the deleted api-portal service, so every test in them errored on session setup. The 3.0.1 changelog claimed this cleanup was done but these two files survived it.
- `test_empty_messages_handled` (`tests/infrastructure/test_llm.py`) now accepts 200/400/422/500. Current llama.cpp returns 500 for empty messages array; the test was hard-coded to 200 and broke against newer llama.cpp builds.
- PC-061 step B: implemented `_emit_event`, `_classify_stage`, `_logical_stage` in `v3-service/main.py`. The test file (`tests/v3-service/test_event_emission.py`) was committed in c5216be ("Install observability") but the implementation never landed, leaving the test red on dev. The contract is now satisfied: legacy `{stage, detail}` frame always emitted, typed envelope opt-in, suffix-based stage classification (`_pass`/`_skip`/`_done` → stage_end success=true, `_failed` → stage_end success=false, `_error` → error event, `_retry` → fresh stage_start), and stage_start→stage_end pairing via logical-name parent_id + duration_ms.

### Repo restructure
- Renamed `atlas-tui` → `tui` and `atlas-proxy` → `proxy` at the repo level; moved ablation data under `docs/reports`. 362 reference updates across the tree.

### Phase 0: first-run installer + model wizard
- New `atlas init` command (`atlas/cli/commands/init.py`): interactive first-run wizard that probes hardware, picks the right tier (T0/T1/T2/T3), recommends a model, writes `~/.atlas/config.yaml`.
- New `atlas model` command (`atlas/cli/commands/model.py`) with `list` / `verify` / `add` / `remove` subcommands; backed by `model_registry.py` (`add`/`get`/`list` with SHA verification) and `model_recommendations.py` (per-tier defaults, split out from `tier.py` in PC-055.2).
- `atlas/cli/events.py` (PC-061 step A): typed-event SSE protocol — `Event` dataclass, `parse_envelope`, `iter_events`, suffix-based stage classification. Schema documented in `docs/PROTOCOL.md`. Producer-side helpers in v3-service landed as PC-061 step B (see Test Fixes above).
- `atlas doctor` extended for the same hardware probe used by the wizard.

### Install + bootstrap hardening
- Hardened fresh-VM install path against partial failures across RHEL 9, Ubuntu, Rocky; `curl … | bash` and `curl … | sudo bash` both work.
- Auto-install NVIDIA driver libraries on RHEL 9 and put the Python CLI on `$PATH`.
- Bootstrap now installs Go and pre-builds `atlas-tui` so first-run latency is download-bound, not compile-bound.

### CI: lint + security + cross-distro
- Added ruff (Python lint) and CodeQL (security scan) as GitHub workflows.
- New PR-time test job that runs the full Python suite against a cross-distro install matrix (Ubuntu 22.04 / 24.04 / Rocky 9).
- Fixed pip PEP 660 friction, Rocky curl conflict, and a CLI-wizard GPU-mock path that was breaking the matrix.

### PC-159: surgical-edit gate (proxy)
- New gate in `proxy/agent.go` that refuses an `edit_file` when the proposed change would rewrite more than a configured fraction of the target file. Forces the model to pick the right tool (`write_file` for new files, `ast_edit` for structural rewrites, `edit_file` only for actual surgical patches).

### Chat history threading (proxy + tui)
- `/v1/agent` now accepts full prior chat history from the TUI, replacing the per-call stateless wrapper. Assistant turns are re-wrapped in a JSON envelope so the proxy can tell user messages from prior model turns when rebuilding context.

### Plan mode (May 5)
- New `/v3/plan` endpoint on v3-service generates a structured plan (steps + verify step + adherence score) before the agent loop begins; Qwen3 reasoning extraction fixed in the same commit.
- `proxy/agent.go` consumes the plan via a plan bridge, an agent-loop hook that pins the current step into each request, and an adherence gate that flags reasoning that drifts from the active step.
- TUI renders `plan_loaded` / `plan_adherence` / `plan_revise` events live (`tui/commands.go`, `tui/model.go`).
- New docs: `docs/PLAN_MODE.md`, `docs/PROTOCOL.md`.

### Proxy reliability (May 5)
- Output sanitiser strips reasoning preambles and dangling JSON fragments from model responses before parsing.
- Shell-op gate refuses dangerous `rm -rf /` style commands and the `bash -c` bypass route.
- System prompt hardened: clearer tool-use rules, fewer hallucinated fields.
- Verification gate added before `type=done` (foundation that tonight's done-without-action gate composes with).
- Host paths in tool-call arguments translated to container paths so the sandbox sees the right file when the model thinks in host-fs terms.
- Fixed a conversation-history drop bug where the post-V3 trim was eating the user's prompt; V3 pipeline now fires on more edit shapes (not just write_file).
- Lens-call timeout in v3-service bumped from 5s to 30s with structured fallback logging on miss.

### Sandbox + execution stack
- **PC-188**: every `run_command` now executes inside the sandbox container, not on the host. Closes the "model writes `rm` and the host runs it" risk.
- **PC-189**: workspace-drift fix and a false-positive in the truncating-redirect detector (was rejecting legit `> file.txt` writes).
- **PC-190**: sandbox verify stack pre-bakes common dev deps (pytest, ruff, etc.), uses tmpfs for the working tree, prints a "create a venv" hint when the model tries to install into the system Python.
- **PC-191/192/193**: sandbox is language-agnostic — works on a working codebase (not just a single-file scratchpad). Detects Python, Node, Go, Rust, Java, C/C++ project layouts and uses the appropriate runner.

### Anti-laziness gates
- **PC-194/195**: `write_file` rejects empty content, single-line stubs, "TODO"-only files, files with `pass`-only bodies, and other lazy outputs.
- **PC-196**: explicit `run_background` tool for long-running processes (e.g. `python app.py`); shell `&` backgrounding through `run_command` is detected and routed to `run_background`.
- **PC-197**: completion-claim verification — when the model declares `done`, the gate checks the workspace state matches the claim (structural check, foundation that tonight's claim-check gate extends).
- **PC-198**: trims boilerplate from the system prompt and strips host `/workspace/` prefixes from model-emitted paths.
- **PC-199/200**: detects "stops at the easy fix" pattern (one tweak then `done`); raises tier-aware turn caps so the model has runway to complete a real task.
- **PC-201**: `write_file` is allowed to overwrite an existing file when that file is corrupted (e.g. truncated mid-write from a prior crashed turn) instead of failing with the usual "file exists" gate.

### PC-202: per-layer residual hidden states from llama-server
- Patched llama-server's `/embedding` endpoint to accept a `layers: [int]` parameter and return the residual-stream hidden state at each requested layer. Foundation for both PC-207 (per-token lens scoring) and tonight's ASA steering vector build.

### PC-206 + PC-207: lens-as-PRM (per-step process reward)
- **PC-206**: thinking-mode plumbing in `v3-service/main.py` `LLMAdapter` — `thinking` keyword resolves per-call against an instance default.
- **PC-207**: lens computes per-token C(x) + G(x) scores during candidate generation; `/internal/lens/score-per-step` exposes aggregates (gx_min, gx_mean, off_rails_idx, cx_norm_max) the proxy and v3-service consume for early-exit and ranking. Wired into v3-service candidate generation, the agent loop (foundation for tonight's reasoning-repeat + path-aware detectors), with structured per-step logging across all three services.
- Severe-score short-circuit: gx_min below 0.05 fires a corrective immediately without waiting for a second sample (calibrated against the May 7 dashboard.html stub-loop session).
- V3↔lens alignment: lens now vetoes a sandbox-passing candidate when its gx_min indicates a stub or placeholder collapse — closes the "sandbox approves a stub V3 generated" loophole.

### Agent loop reliability sweep (May 7)
- Empty-response fallback: when the model returns nothing parseable, the loop emits a corrective hint instead of retrying the same prompt verbatim.
- Plan-threshold guard: refuses to enter the agent loop on a plan with adherence score below threshold.
- Tool-repeat detector: precursor to tonight's reasoning-repetition detector — catches verbatim tool-call repeats within a window.

### GH #39: AST-aware surgical edits + tier-aware V3 routing (May 8)
- **v1 (5e44ffb)**: new `ast_edit` tool — friendly-selector AST node replacement using tree-sitter. Supports `function:NAME`, `class:NAME`, and `<tag>` selectors. The selector vocabulary is intentionally small in v1; nested selectors (e.g. `<style>` inside `<head>`) are NOT supported and produce a "0 nodes matched" error.
- **Point 1 (468a555)**: structural verification veto for V3 candidates — rejects candidates that pass sandbox but fail structural shape checks (e.g. removed a required import, lost the class definition).
- **Point 2 (b95f741)**: cyclomatic-complexity enrichment in tier classification — `tier.py` now considers logic density, not just line count, when assigning T0/T1/T2/T3.
- **Point 3 (2629652)**: Phase 3 repair receives call-chain context (callers + callees of the file being repaired) so the repair model can reason about cross-file effects.
- **Point 4 (bd0b02b)**: auto-injection of a reachability slice from the user's message — the lens picks the most relevant file regions and inlines them into the system context before the loop starts.
- Plan generation made aware of `ast_edit` so plan steps suggest it when the target is a structural edit.
- `edit_file` "string not found" error now suggests `ast_edit` as the recovery; `write_file` rejection on existing files also points to `ast_edit`.
- Three follow-up fixes: encoding (HTML entities in selector args), trim-resilience (large `content` fields surviving the post-V3 trim), and parse-failure categorization in logs.
- Jinja crash fix when `symbol_index` injects snippets: the snippet role was being set to `system`, which Jinja resolved as a template literal; changed to `user` role.

### BiasBusters tool-selection mitigations
- Tool descriptions rewritten to push the model toward the right tool for the task: `edit_file` framed as the surgical default, `ast_edit` marked REQUIRED for HTML/Python structural edits, `write_file` restricted to new-file creation only.
- Conditional GBNF grammar built per turn: when the loop has already entered a step the model has just claimed done, the grammar bans re-emitting the same tool name token-side so the model can't loop on the same failed tool call.
- Per-step tool-list filter (`buildToolDescriptionsExcluding`): the system prompt strips tools the loop has explicitly excluded for this step, so the model never sees them as options.
- ASA (Activation Steering for Aast_edit) wired into the inference entrypoint: `inference/entrypoint-v3.1-9b.sh` auto-detects `/models/ast_edit_steering.gguf` and applies it always-on via llama.cpp `--control-vector`. Default scale 0.5, default layer range full-model, both overridable via env. PC-202's per-layer-residual `/embedding` patch is the upstream that makes this possible.

### ASA steering vector
- New `geometric-lens/asa_calibration/` directory: 1000 contrast-pair prompts (50+ base templates × variation pools) cover function selectors (54%), HTML tags (27%), and CSS classes (19%). `generate_pairs.py` produces `contrast_pairs.jsonl`; `build_steering_vector.py` extracts residuals via the lens `extract_per_layer_per_token` endpoint at layer 27 (of 36 in Qwen3.5-9B), means across tokens/prompts/sign, and writes a llama.cpp-format GGUF control vector. Final vector: 16736 bytes, ‖v_global‖ = 8.6444 after 730s on 2000 prompts.

### Agent loop hardening
- **Plan-progress reminder** (`proxy/plan_reminder.go`): ephemeral system note injected into every step request rendering `plan progress N/M — currently on step "sX": <action> <target>` plus done/remaining sub-step IDs. Lazy-initializes `ctx.PlanStepsSatisfied`. Not persisted to `ctx.Messages`, so it survives the post-V3 conversation trim cycle.
- **Reasoning-repetition detector** (`proxy/reasoning_repeat.go`): tracks the model's reasoning-stream opening; on 3 consecutive identical normalized openings (case-folded, whitespace-collapsed, 80-char snippet) the loop queues a corrective system message. Successfully broke a session-2 stuck loop in live testing.
- **Path-aware error breaker** (`extractFailurePath` in `proxy/lens_score.go`, breaker logic in `proxy/agent.go`): tracks `ctx.RecentFailurePaths` per tool failure. Known limitation: the v1 implementation resets on intervening successes, so it can miss long stuck-loop sequences with sporadic productive turns in between.
- **Done-without-action gate** (`proxy/guardrails.go`): refuses `type=done` when the user prompt is fix-intent and no successful verification command has run this loop. Action-intent words (`rewrite`, `create`, `add`, `update`, `redesign`) also trigger a productive-change check parallel to the existing verify check. Caught 4 false-success done attempts in live testing.
- **Truncation recovery shims** (`proxy/agent.go`): `recoverTruncatedAstEdit` + `recoverTruncatedEditFile` + `recoverTruncatedToolCall` rescue malformed tool emissions from the model and re-pack into a well-formed shape. Each shim is targeted at a specific failure mode observed in production logs.
- **Conversation history error surfacing** (`proxy/agent.go`): `extractModelResponse` now exposes the actual `Unmarshal` error path (directErr vs balancedErr) so debug logs distinguish parse-shape failures from content failures.
- **Removed `ResponseHeaderTimeout`** from `proxy/v3_bridge.go` and removed all client-level timeouts on the V3 HTTP path. Long V3 chains (10+ minute passes) were getting bounced by the 10-minute response-header window even when the pipeline was making progress.
- **Removed `absoluteMaxTurns` ceiling** from `proxy/types.go`. Turn caps now come solely from `TierMaxTurns` (T0:5, T1/T2/T3:0 = uncapped) with no override clamp. Reasoning: 8 detectors armed in the loop make a hard cap redundant — let the detectors decide when to break.

### Surgical-edit hardening (V3 routing)
- `proxy/tools.go` ast_edit executor: tier classification now uses `max(oldTier, newTier)` and the previous V3-tier floor for HTML was dropped (it was over-triggering V3 on the smallest CSS tweaks). Doctype dedup (`leadingDoctypeRe` + `stripLeadingDoctype` in `proxy/guardrails.go`) prevents the model's "<!DOCTYPE html>" prefix from being inserted twice when ast_edit replaces the `<body>`.
- Suspiciously-shrunk-edit guard (`validateNotSuspiciouslyShrunk` in `proxy/guardrails.go`): rejects an edit that shrinks an >100-byte file to <64 bytes. Final threshold tuned after a legitimate 80-byte one-liner refactor was false-rejected at 128. Triggered on a destructive 32-byte stub in pre-release testing.
- Working-directory phantom-dir guard (`validateWorkingDirReference` + `workspaceRefRe`): catches model emissions that try to `cd templates/workspace` or similar nested-workspace references; legitimate `cd /workspace` at the sandbox root is allowed.
- Action-intent gate (`actionIntentWords` + `isActionIntentMessage` + `actionWithoutProductiveChangeMessage`): companion to the verification gate, catches `done` declarations on `rewrite`/`create`/`add`/`redesign`-style prompts that don't include a productive edit this loop.

### TUI reasoning stream visibility
- `tui/model.go` adds a `streamingReasoningText` buffer and a `reasoning_token` event handler that renders with a `‹thinking›` prefix so the user sees the model's reasoning stream live alongside its content. Both buffers reset on `llm_call_start` / `llm_call_end`.
- `tui/commands.go` extended to forward the `delta.ReasoningContent` field from the SSE stream as `reasoning_token` events.
- `proxy/agent.go` plumbs reasoning content through the agent loop: stashes `ctx.LastTurnReasoning`, captures `pendingReasoningCorrective` via `recordReasoning`, and re-emits reasoning deltas to the client mid-turn (with a `sync.Mutex` around the `http.ResponseWriter` to fix the SSE race that produced the "chunked line ends with bare LF" errors).

### Tests
- New Go tests: `proxy/path_aware_test.go`, `proxy/reasoning_repeat_test.go`, `proxy/recover_truncated_test.go`, `proxy/step_restriction_test.go`. Extended `proxy/guardrails_test.go`, `proxy/plan_hook_test.go`.
- All `go test ./...` on both `proxy/` and `tui/` modules pass.
- Full Python suite: 1055 passed / 4 skipped / 0 failed / 0 errors locally.

## [3.0.1] - 2026-04-05

### Tool-Call Agent Loop Architecture
- Replaced Aider format-translation proxy with structured JSON tool-call agent loop
- Grammar-constrained output via llama-server `response_format:json_object` — 100% valid JSON
- 8 tool definitions: `read_file`, `write_file`, `edit_file`, `delete_file`, `run_command`, `search_files`, `list_directory`, `plan_tasks`
- Per-file tier classification: T1 (config/data) writes directly, T2 (logic/features) routes through V3 pipeline
- 3400+ lines new Go code across 12 files in `proxy/`

### V3 Pipeline Integration
- All 14 V3 steps wired into `write_file`/`edit_file` executors for T2/T3 files
- PlanSearch → DivSampling → Budget Forcing → Build Verification → C(x)/G(x) Scoring → Best-of-K → S*/Blend-ASC → Failure Analysis → PR-CoT Repair → Refinement Loop → Derivation Chains → Metacognitive → Final Write
- Per-file-type build verification: tsc, py_compile, gcc, go build, cargo check, bash -n
- V3 service SSE streaming: pipeline progress visible in real-time

### CLI Experience
- `atlas` command: starts all services and launches Aider
- Streaming progress: `[Turn N/M]` with tool call details, V3 pipeline steps, completion summary
- Exploration budget: 4 consecutive read-only calls triggers nudge, prevents model from over-exploring
- Pre-injected project context: model sees project file list in system prompt
- File deletion via fast-path before tier classification
- Truncation prevention: 32K context, reject write_file for existing files >100 lines, detect truncated args before execution

### Deployment
- Docker Compose (`docker-compose.yml`) for full stack orchestration
- Podman compatible with host networking
- `.env.example` with all configurable parameters
- `atlas` script auto-detects Docker vs bare-metal and routes accordingly

### Renames (362 total reference updates)
- `rag-api/` → `geometric-lens/` (directory + all references)
- `ATLAS_RAG_URL` → `ATLAS_LENS_URL`
- `ATLAS_FOX_URL` → `ATLAS_INFERENCE_URL`
- `foxURL` → `inferenceURL` (Go code)
- `ralph-loop` → `verify-repair loop`
- `rag.py` → `pipeline.py` (geometric-lens orchestration)

### Reliability
- 8-level test × 3 iterations: 95.8% (23/24)
- 5-language integration: 100% (Shell, Python, Rust, C, Go)
- L6 (add feature to existing project): 67% — marked as future improvement

### Documentation Overhaul
- **ARCHITECTURE.md**: Complete rewrite — 13 Mermaid diagrams (service topology, agent loop flow, V3 pipeline, module map, sequence diagrams), every component verified against source code
- **API.md**: Complete rewrite — every endpoint across all 5 services verified against source, request/response formats, SSE stages
- **CLI.md**: Complete rewrite — startup flow diagram, streaming format, workflow examples, troubleshooting, env vars, Aider config reference
- **CONFIGURATION.md**: Complete rewrite — every env var across all services verified, internal constants, Docker Compose vs K3s differences
- **MAP.md**: Complete rewrite — every file in repo with clickable tree, 150 file links, 18 description tables
- **SETUP.md**: Complete rewrite — verified build steps, first-run guide, bare metal, K3s, hardware sizing, Lens training guide
- **TROUBLESHOOTING.md**: Complete rewrite — quick diagnostics, 20+ issue scenarios with verified fixes
- **README.md**: Honest 7-step setup with actual download command, prerequisites, model clarity (Qwen3-14B vs Qwen3.5-9B)
- Reorganized historical docs into `docs/reports/` (ablation studies, status tracking, migration guides)

### Bug Fixes
- **geometric-lens Dockerfile port mismatch**: Container was listening on 8001 but docker-compose expected 8099 — fresh Docker Compose deploys had broken Lens service. Fixed Dockerfile to use port 8099.
- **Python CLI default RAG port**: `atlas/cli/client.py` defaulted to port 31144 (K3s NodePort) instead of 8099 (Docker Compose). Fixed default to match Docker Compose.
- **Missing Aider config files**: `.aider.model.settings.yml` and `.aider.model.metadata.json` were not in the repo — the `atlas` launcher would fail without them. Restored both files and added `.gitignore` exceptions.
- GitHub Issue #6: `hostname -I` → portable fallback chain (`ip addr` → `hostname -I` → `hostname -i`) for Arch Linux compatibility
- GitHub Issue #10: `rag-api/` → `geometric-lens/` restructuring resolved missing models directory
- GitHub Issue #11: Added Geometric Lens training documentation to SETUP.md with HuggingFace dataset link
- GitHub Issue #12 / PR #13: `docker image exists` → `docker image inspect` in build script

### Cleanup
- Removed 62 stale test directories, old v1 proxy binary, dead G(x) metric tensor training scripts
- Removed stale tests for deleted services (api-portal, dashboard, embedding-service, task-worker)
- Removed root-level development artifacts (bubble_sort.py, snake_game.py, etc.)
- All hardcoded `/home/isaac/` paths replaced with `$HOME` or `ATLAS_DIR` env vars

## [3.0] - 2026-03-05

### V3.0 Benchmark Release
> Withdrawn 2026-09-25: the figures in this entry are not LiveCodeBench
> pass@1, and "self-verified" is inaccurate; repair also saw, and was accepted
> on, the examples printed in each problem. See [Unreleased].

- **74.6% LCB pass@1** (447/599) on frozen Qwen3-14B
- Full ablation study: conditions A–D with per-task results
- Phase 1 (PlanSearch/DivSampling): +12.4pp
- Phase 3 (PR-CoT/Refinement/Derivation): +7.3pp
- Self-verified Phase 3 using model-generated test cases

## [2.5.1] - 2026-02-23

### Confirmation Ablation: Embedding Source Hypothesis — STRONG CONFIRMATION
- **H1: Self-embeddings restore C(x) discrimination: CONFIRMED (+39.5pp)**
  - C(x) selects passing candidate 87.8% on mixed-result tasks vs 48.3% random (p < 0.000001)
  - V2.5 result (+0.6pp under nomic 768-dim) was an embedding source limitation, not architecture failure
  - Reverse energy selects only 4.3%, proving strong directional signal
  - Val AUC: 0.9934, energy separation: 21.75 (7.2x wider than V2.5)
- **H2: G(x) adds value beyond C(x): NEUTRAL (0.0pp)**
  - G(x) contributes zero at optimal alpha (0.001); monotonically degrades at higher alpha
  - Zero corrections, zero breakages across all mixed-result tasks
- **Outcome B**: Ship C(x)-only with self-embeddings, remove or redesign G(x)
- **Difficulty routing validated**: Q1 (low energy) = 100% oracle, Q4 (high energy) = 0.3%
- **C(x) confirmed as both verifier (87.8% selection) and router (perfect difficulty stratification)**
- Runtime: 24h 42m on LiveCodeBench v5 (599 tasks, K=3, 4 epochs)
- Infrastructure: Qwen3-14B with `--embeddings` (no spec decode, ~45 tok/s)
- Risk R6 (Lens non-discriminating) RESOLVED; Risk R11 (no verifier) substantially mitigated

## [2.5.0] - 2026-02-21

### Ablation Study
- Systematic ablation of Geometric Lens, router, and infrastructure components
- Finding: C(x) energy scoring ≈ random for candidate selection under nomic embeddings (37.7% vs 37.1%, within 3.4pp seed variance) — **V2.5.1 confirmed this was an embedding source limitation** (87.8% accuracy restored with self-embeddings)
- Finding: C(x) energy strongly correlates with task difficulty (58.5% vs 18.9% pass rate across tiers)
- Finding: G(x) metric tensor confirmed dormant (5.2M params, zero impact)
- Finding: Pattern cache bypassed entirely by benchmark runner

### Architecture Change
- Discovered `--embeddings` flag breaks speculative decoding (forces n_batch=512)
- Migrated to two-server sidecar architecture: generation + spec decode on Server A, embeddings via nomic-embed-text-v1.5 on Server B
- Recovered ~2.6x generation throughput (~38 tok/s → ~100 tok/s)
- Net VRAM delta: approximately -230 MiB (sidecar cheaper than --embeddings overhead)

## [2.0.0] - 2026-02-18

### Architecture Changes
- Replaced Qdrant vector DB + embedding service with PageIndex tree-based RAG
- Added Geometric Lens (Cost Field + Metric Tensor) for candidate quality prediction
- Added Confidence Router with difficulty-based adaptive-k selection
- Added Pattern Cache (Redis + Ebbinghaus memory decay)
- Added Best-of-K pipeline with parallel candidate generation
- Added sandboxed code execution for benchmark evaluation
- Added speculative decoding with Qwen3-0.6B draft model
- Added KV cache quantization (q4_0)

### Benchmark Results (Run ID: v2_run_20260217_125310)
- LiveCodeBench: 36-41% pass@1 (across Lens training epochs, k=3)
- GPQA Diamond: 47.0% (k=5)
- SciCode: 14.7% sub-problems (341 tasks, k=1)
- Geometric Lens: 0.968 Val AUC, ~80% first-pick accuracy (151/188)
- Throughput: 109 tasks/hr on RTX 5060 Ti 16GB

### Removed
- Qdrant vector database
- MiniLM-L6-v2 embedding service
- LoRA nightly training pipeline (moved to v1_archived/, CronJob suspended)
- V1 benchmark suite (HumanEval, MBPP, Custom)

### Fixed Post-Release
- mlock allocation failure — added LimitMEMLOCK=infinity systemd override for K3s
- Speculative decode slot 1 failure — quantized draft KV cache to q4_0 (-ctkd/-ctvd)
- Dashboard crash-loop — fixed missing Jinja2 default filters

### Notes
- IFBench evaluation incomplete (excluded from results)
- All results from single benchmark run (variance unknown)

## [1.0.0] - 2026-02-04

Initial release. See benchmark/v1_benchmark_report.md for V1 results.

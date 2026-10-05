# Contributing to ATLAS

This page takes you from "I'd like to help" to "my change is in a release".
If something here is unclear or wrong, that's a bug. Open a Documentation
issue.

**Quick links:** [Start Here](https://github.com/orgs/inferstep/projects/1/views/1) (issues ready for newcomers) ·
[Roadmap board](https://github.com/orgs/inferstep/projects/1) ·
[ARCHITECTURE](docs/ARCHITECTURE.md) · [MAP](docs/MAP.md) ·
[DEVELOPMENT](docs/DEVELOPMENT.md) · [GOVERNANCE](GOVERNANCE.md) ·
[Security reports](SECURITY.md)

## 1. How ATLAS is organized

ATLAS is a local coding agent made of a few services: the Go agent loop
(`proxy/`), the terminal client (`tui/`), the Python CLI (`atlas/`), the V3
candidate pipeline (`v3-service/`), the Geometric Lens (`geometric-lens/`),
the sandbox (`sandbox/`) and the llama.cpp images (`inference/`).
[MAP.md](docs/MAP.md) says what every directory owns, and
[ARCHITECTURE.md](docs/ARCHITECTURE.md) explains how the pieces work
together.

Every change lands on `dev`. `dev` is promoted to `staging` as a release
candidate, and `staging` to `main` as a release. See
[section 10](#10-after-your-change-merges).

## 2. Ways to help without writing code

- **Triage.** Try to reproduce a bug report, ask for what's missing, or
  point out a duplicate.
- **Docs.** Anything wrong, missing or unclear: open a Documentation
  issue, or send a pull request.
- **Hardware reports.** Run ATLAS on your GPU and report what happened,
  with your `atlas doctor` output. AMD (ROCm), Apple Silicon (Metal) and
  Vulkan reports help most; see [SUPPORT_MATRIX](SUPPORT_MATRIX.md).
- **Trained artifacts** for a new model (see below).
- **Questions** go to [Discussions Q&A](https://github.com/inferstep/ATLAS/discussions/categories/q-a).

### Contributing trained artifacts (Lens / ASA)

ATLAS needs a Geometric Lens (`cost_field.pt`) and an ASA control vector
(`*.gguf`) for each base model, because both are coupled to the model they
were trained against. If you've trained them with `atlas lens build` /
`atlas asa build`, publish them with `atlas lens publish` /
`atlas asa publish`. That uploads to a Hugging Face repo you own and opens
a registry pull request. You don't need write access here, just a Hugging
Face account and write token. The walkthrough is in
[PUBLISHING.md](docs/PUBLISHING.md).

## 3. Set up

**Without a GPU.** Most of ATLAS can be built and tested without a GPU or a
model: the quality gate, the unit tests, and the end-to-end tests, which use
a scripted fake llama-server. You need Python 3.9 or newer, Go 1.26 and git.

Fork the repo first. The fork page copies only `main` unless you untick
**Copy the `main` branch only**. Either way works: the commands below take
`dev` from this repository.

```bash
git clone https://github.com/<you>/ATLAS.git
cd ATLAS
git remote add upstream https://github.com/inferstep/ATLAS.git
git fetch upstream
git checkout -b dev upstream/dev
python -m venv .venv && . .venv/bin/activate
pip install -e . pytest pyyaml
python scripts/production-readiness.py
```

**With a GPU.** Install the full stack with [SETUP.md](docs/SETUP.md). Then
use the dev mode in [DEVELOPMENT.md](docs/DEVELOPMENT.md), which runs your
working tree in the containers without a rebuild after every edit.

## 4. Run the quality gate

```bash
python scripts/production-readiness.py            # everything
python scripts/production-readiness.py --list     # the gates
python scripts/production-readiness.py --only ruff
```

CI runs the same gates, so a green run here usually means a green pull
request. An optional tool you haven't installed shows as `unavailable`,
not as a pass. More on tests is in [Testing](#testing).

## 5. Find an issue

Open [Start Here](https://github.com/orgs/inferstep/projects/1/views/1).
It lists issues that are **Ready**, sized for newcomers, and unclaimed. The
[Help Wanted](https://github.com/orgs/inferstep/projects/1/views/2) view
shows every Ready issue. The board's fields mean:

| Field | Meaning |
|---|---|
| Status | Triage → Needs Design → Backlog → **Ready** → In Progress → In Review → Done. Only Ready issues can be claimed. |
| Priority | P0 is most urgent, P3 least |
| Size | XS (under an hour) to XL (more than a week) |
| Contributor Level | Starter, Intermediate, Advanced, Maintainer-only |
| Hardware Needed | None means you don't need a GPU |
| Shepherd | The maintainer who answers your questions on that issue |

Ready means a maintainer wants the change and has written acceptance
criteria. Meet them and pass CI, and it merges.

**No Ready issue fits?** Small fixes under about 50 lines are welcome
without an issue. For anything bigger, open an issue first. A large pull
request without one may be closed with a pointer to do that.

## 6. Claim it

Comment `/claim` (alone, as the first line) on a Ready issue. The bot
assigns you, moves the card to In Progress, and replies with your Shepherd.

- One person per issue. You can hold **2** open claims, or **1** before
  your first merged pull request.
- Link a pull request within **5 days** (put `Closes #<issue>` in its
  description, or the bot reminds you). After **7 days** without one, the
  claim is released and the issue is Ready again.
- Comment `/unclaim` any time to let it go. No hard feelings.

These numbers live in [.github/atlas-bot.yml](.github/atlas-bot.yml).

## 7. Branches, commits and pull request titles

Fork the repo, then branch from `dev` in your fork, e.g. `fix/retry-loop`.

Pull request titles are
[conventional commits](https://www.conventionalcommits.org/) with the
component as scope. CI checks this, because the title becomes the commit
message on `dev` and the line in the release notes:

```
type(scope): summary
```

- **type**: `feat`, `fix`, `docs`, `chore`, `refactor`, `perf`, `test`,
  `build`, `ci`, `revert`, `style`
- **scope**: the component: `proxy`, `tui`, `cli`, `v3`, `lens`, `sandbox`,
  `inference`, `extensions`, `install`, `ci`, `docs`, `deps`, …
- **breaking change**: a `!` before the colon, e.g. `feat(proxy)!: …`, plus
  a `BREAKING CHANGE:` line in the description

Examples: `fix(proxy): stop the retry loop on a closed stream`,
`feat(tui): show the V3 candidate count`, `docs(setup): add the ROCm driver step`.

Use the same format for your commit messages where you can. The PR title
is what's kept.

## 8. Open a draft pull request early

Open a **draft** pull request against `dev` as soon as you have something.
It links you to the issue, lets your Shepherd help early, and keeps your
claim. Fill in the template: what changed, why, how you verified it, and
the hardware you ran it on (or "untested on hardware").

GitHub sets **base** to `main` on a new pull request. Change it to `dev`
before you click **Create pull request**.

CI for a pull request from outside the org waits until a maintainer
approves the run. CI on a fork never gets the repository's secrets.

## 9. Review

- A maintainer responds within **5 business days**.
- To merge, a pull request needs:
  - an approval from a code owner ([CODEOWNERS](.github/CODEOWNERS))
  - all required checks green
  - every conversation resolved
  - no conflict with `dev` (a maintainer may ask you to click **Update branch**)
- New commits dismiss earlier approvals, so the last push gets reviewed.
- Maintainers merge with **squash** (your title becomes the commit) or
  **rebase**. History on `dev` stays linear.

**Definition of done:** linked issue, tests for new behavior, docs updated
for behavior changes, conventional title, CI green, and hardware tested (or
stated as untested).

## 10. After your change merges

| Branch | What it is | When your change gets there |
|---|---|---|
| `dev` | Integration | At merge. `:dev` images and an immutable `:sha-<commit>` image are built. |
| `staging` | Release candidate (`vX.Y.Z-rc.N`) | When a maintainer promotes `dev`. It stays at least 3 days. |
| `main` | Released (`vX.Y.Z`, `:latest`) | When the candidate passes and the release owner approves |

Your issue: GitHub's `Closes #N` acts only on `main`, and your pull request
goes into `dev`. So the bot closes the issue for you within a few hours of
the merge, and the board marks it Done. Its milestone shows the release that
ships your change. If the issue should stay open after your change lands
(it still needs a check after a deploy), write `Refs #N` instead.

Minor releases ship when `dev` is ready; there's no fixed calendar. Fixes
and security releases can ship any time. [RELEASE.md](docs/RELEASE.md) has
the details.

## 11. Proposing something big

Use the **RFC** issue form for architecture changes, new dependencies,
security-model changes, release-policy or support-matrix changes, and any
breaking change. An RFC gets a 7-day comment period and then a decision:
Accepted, Revise, or Declined. An accepted RFC becomes an Epic with
sub-issues. The whole flow is in [GOVERNANCE](GOVERNANCE.md#proposals-rfc--adr--epic).

## 12. Getting help

Ask your **Shepherd**: the bot names them when you claim, and they're on
the issue's card. You can also comment on the issue or ask in
[Discussions Q&A](https://github.com/inferstep/ATLAS/discussions/categories/q-a).
Report security problems privately, never in an issue (see
[SECURITY.md](SECURITY.md)).

## 13. Growing into Triager, Reviewer, Maintainer

Access follows trust built over time, not a count of pull requests. The
path is Contributor → Triager → Reviewer → Maintainer. Each rung has a
minimum time, a nomination, and hard technical limits. See the
[trust ladder](GOVERNANCE.md#trust-ladder).

## Code style

How code should read (size, structure, abstractions, comments) is in
[CODE_STYLE.md](docs/CODE_STYLE.md). The rules per language:

**Python.** PEP 8, type hints on function signatures, docstrings on public
functions, lines up to 100 characters. `ruff` runs in the gate.

**Go.** Format with `gofmt`. `go vet` and `staticcheck` run in the gate
for `proxy/` and `tui/`.

**Bash.** Must pass `shellcheck`. Start with `set -euo pipefail`, quote
variables (`"$var"`), use `[[` for conditionals, and comment non-obvious
logic.

**YAML and Kubernetes.** 2-space indentation, resource limits on every
container, meaningful names and labels.

**Docs.** Markdown, with examples where they help. Keep the matching
`docs/*.md` in step with any behavior change.

## Testing

```bash
# The gate's Python test gates cover the suites CI runs: tests/v3,
# tests/v3-service, tests/cli, tests/infrastructure, tests/concurrency,
# tests/perf, tests/contracts and tests/e2e; geometric-lens/tests runs in
# its own process (CPU torch is enough).
python scripts/production-readiness.py

# One file
pytest tests/v3/test_plan_search.py -v

# End-to-end acceptance: real proxy + sandbox executor + fake
# llama-server. No GPU or model needed. Build the proxy binary first.
cd proxy && go build -o /tmp/test-atlas-proxy . && cd ..
pip install -r sandbox/requirements-runtime.txt
pytest tests/e2e -v
```

- Put tests in `tests/`, name files `test_*.py`, and name tests after the
  behavior they check.
- New behavior needs a test. A bug fix needs a regression test.
- Keep tests hermetic. Never depend on a repo-root `.env`: CI runs on a
  clean checkout without one. Create your own `tmp_path / ".env"` or pass
  values through the subprocess environment.
- `tests/validate_tests.py` (the `test-integrity` gate) rejects weakened
  tests, e.g. `assert True` or a swallowed exception.

One more check reads your change, not the code. It looks for removed or
skipped tests, history in new comments, new documents, and changes to the
files that configure the checks. It says what it found, why it matters and
what to do, and it does not fail:

```bash
python scripts/integrity_check.py                 # your branch against origin/dev
python scripts/integrity_check.py --base <commit>
```

## License

This project is licensed under the **GNU Affero General Public License v3.0 (AGPL-3.0)** (see [LICENSE](LICENSE)).

By submitting a contribution (pull request, patch, or any other form), you agree to the following terms:

- Your contributions are accepted under the same license as the project: AGPL-3.0.
- You retain copyright of your contributions.
- You grant the project maintainer (Isaac Tigges) a perpetual, irrevocable, worldwide, royalty-free license to use, modify, and distribute your contributions under the project license.
- You represent that you have the legal right to grant this license and that your contributions do not infringe on any third-party rights.

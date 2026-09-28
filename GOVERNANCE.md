# Governance

ATLAS is a **single-maintainer project** today (see
[MAINTAINERS.md](MAINTAINERS.md)). This document states how decisions
are made now, how people gain access, and the limits that apply at every
level. It does not pretend a committee exists.

## Decision making

- Day-to-day decisions (bug fixes, refactors, docs) are made by
  maintainers merging to `dev`.
- Significant decisions go through an RFC and are recorded as an ADR
  under `docs/adr/` (see [Proposals](#proposals-rfc--adr--epic)). They
  include architecture changes, new dependencies, security-model
  changes, release policy, changes to the documented support matrix, and
  any breaking change.
- Community input happens in GitHub issues and Discussions. The lead
  maintainer resolves substantive disagreement, and the reasoning is
  written down in the issue or ADR.

## Change flow

All changes land on `dev`, are promoted to `staging`, then fast-forward
to `main`. Releases are tagged from `main` and published only through
the CI pipeline (immutable `sha-*` images, tag promotion gated on the
tests workflow).

The rules are enforced by repository rulesets, created by
`scripts/setup/rulesets.sh`:

- **`dev`, `staging` and `main`**
  - Nobody can force-push or delete them, including admins.
  - History must be linear.
  - The required checks must pass on an up-to-date branch.
  - A pull request needs a code-owner approval. New commits dismiss old
    approvals, the last push needs someone else's approval, and all
    conversations must be resolved. Only maintainers can dismiss a
    review.
- **Branches.** Only admins, the `maintainers` team, and Dependabot can
  create, push or delete branches. Everyone else works from a fork.
  Anyone with write access could otherwise push a branch with a new
  workflow and run it with the repository's permissions. The
  `star-history` asset branch is the one exception, because its
  workflow's token can't be exempted.
- **Release tags** (`v*`). Only admins and maintainers can create one,
  and nobody can move or delete one once it exists.

The lead maintainer pushes directly to `dev` as a ruleset bypass, which is
logged. External pull requests meet every rule. Human review of the lead's
own pushes waits until a second person holds the Reviewer role.

## Trust ladder

Access follows trust built over time, never a count of pull requests. The
xz-utils backdoor (2024) came from an attacker who spent about two years
earning maintainer trust, so every rung also has hard technical limits.

| Role | GitHub access | Can | Can't | To get here |
|---|---|---|---|---|
| Contributor | None; works from a fork | Open issues and PRs, `/claim` | Anything on the repository itself | Anyone |
| Triager | Triage role (`triagers` team) | Label, assign, close, edit project fields | Push, approve, see security advisories, touch workflows | 3+ months of real participation, merged work of substance, nominated by a maintainer |
| Reviewer | Write role (`reviewers` team), blocked from creating branches | Approvals count on their subsystem | Push branches, merge, tag, change workflows | 6+ months as Triager, owns a subsystem in practice, a video call with the lead |
| Maintainer | Maintain role (`maintainers` team) | Merge, promote `dev` → `staging`, run triage, accept RFCs | Cut a stable release alone, change security or org settings | 12+ months, known to the lead beyond GitHub, signing key in `allowed_signers`, has shadowed a release |
| Lead | Owner | Everything | | Isaac Tigges |

**Safeguards on every rung**

- Every path needs a code owner's review. The security- and
  release-critical paths stay with the lead even when subsystems gain
  owners: `.github/workflows/`, `scripts/`, `sandbox/`, `SECURITY.md`,
  `docs/RELEASE.md`, `docs/PUBLISHING.md` and
  `atlas/commands/model_registry.py` (see
  [CODEOWNERS](.github/CODEOWNERS)).
- Only keys in `.github/allowed_signers` make valid release tags. The
  `production` environment, which moves `:latest` and version tags, needs
  the lead's approval.
- Only maintainers create branches, so write access can't be used to run
  a planted workflow.
- Access is reviewed every quarter. Six months of inactivity drops a
  rung.
- Promotions are announced publicly.

## Proposals: RFC → ADR → Epic

RFCs are issues, so they sit on the same board as all other work.

1. Someone opens the **RFC** issue form. The issue gets type RFC and
   board status Needs Design.
2. The bot links open RFCs and Epics with similar titles, and who holds
   them.
3. There is a comment period on the issue, **7 days** by default. For
   line-by-line review, the author opens a pull request adding
   `docs/adr/NNNN-title.md` with status Proposed.
4. A maintainer posts the decision: **Accepted**, **Revise**, or
   **Declined**, with reasons.
5. **Accepted:** the ADR merges with status Accepted and links the issue.
   The same issue's type changes from RFC to Epic, and its sub-issues are
   created and specced.
6. **Declined:** the issue is closed as not planned, with the reason, so
   the next person with the same idea finds it.

ADR statuses and format are in [docs/adr/README.md](docs/adr/README.md).

## Review times

- A maintainer responds to a pull request within **5 business days**.
- New issues are triaged within **5 business days**; see
  [TRIAGE.md](docs/TRIAGE.md).
- Security reports follow the targets in [SECURITY.md](SECURITY.md).

## Release authority

Maintainers can promote `dev` to `staging`. A stable release (`main`, a
`vX.Y.Z` tag, `:latest`) needs the lead's approval in the `production`
environment and a tag signed by a key in `allowed_signers`. The checklist
is in [docs/RELEASE.md](docs/RELEASE.md).

**Bus-factor status: 1.** Growing to a second release-capable maintainer
is an explicit project goal and a precondition for calling the project
mature. Until then, the honest statement is that the lead maintainer
disappearing stops releases. The AGPL license and the public registry
artifacts allow forks to continue.

## Security

Security reports follow [SECURITY.md](SECURITY.md). What to do when
something goes wrong (a vulnerability, a leaked key, a bad release) is in
[INCIDENT_RESPONSE.md](docs/INCIDENT_RESPONSE.md). Security fixes may land
ahead of the normal review flow, with the ADR or notes written
afterwards.

## Inactive-maintainer / succession policy

If the sole maintainer is unresponsive for 60+ days, consider the project
fork-friendly. Everything needed to continue is in the repository by
design: the build pipeline, registry contracts, artifact hashes, and the
release process docs. The HF artifact repos are mirrored by their hashes
in the registry, so a fork can re-host and re-pin them.

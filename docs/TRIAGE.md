# Triage

How new issues become work someone can pick up. This is for triagers and
maintainers (see the [trust ladder](../GOVERNANCE.md#trust-ladder)).

**When:** at least once a week. Every new issue gets a first response
within **5 business days**.

**Where:** the [Triage view](https://github.com/orgs/inferstep/projects/1/views/7)
of the board. New issues and pull requests are added to it automatically
with Status **Triage**, and issue forms add the `needs-triage` label.

## For each new issue

1. **Security?** If it describes a vulnerability, don't discuss the
   details in public. Comment asking the reporter to use a
   [private security advisory](https://github.com/inferstep/ATLAS/security/advisories/new),
   and tell a maintainer, who hides or deletes the issue.
2. **Duplicate?** Link the original, add `duplicate`, and close it as a
   duplicate.
3. **Enough information?** If not, ask for what's missing (for bugs,
   usually the `atlas doctor` output and the logs) and add `needs-info`.
   Close it as not planned after **14 days** without an answer. It can be
   reopened any time.
4. **Type.** Check the issue type: Bug, Feature, Task, Docs; Spike and Epic
   for maintainers. A significant change needs an RFC (see
   [GOVERNANCE](../GOVERNANCE.md#proposals-rfc--adr--epic)).
5. **Labels.** Add one or more `area/*` labels, and `platform/*` when it's
   specific to a backend.
6. **Board fields.** Priority, Size, Contributor Level, Hardware Needed,
   and Critical Path. Set a milestone when it's targeted at a release.
7. **Status.**
   - **Needs Design**: an RFC or a decision is open.
   - **Backlog**: wanted, but not specced yet.
   - **Ready**: see the checklist below.
   - Or close it as not planned (`wontfix`) with the reason.
8. Remove `needs-triage`.

Don't set `status/ready` or `status/blocked` by hand. The bot mirrors them
from the board's Status every hour.

## Ready means

- [ ] Acceptance criteria are in the issue: what "done" looks like, and how
      to check it
- [ ] A **Shepherd** is set: the maintainer who answers questions on it
- [ ] Size, Contributor Level and Hardware Needed are set
- [ ] No open design question is left

Only Ready issues can be claimed with `/claim`.

## Priority

| Priority | Use for |
|---|---|
| P0 | Broken install, data loss, or a security problem in a release. Drop everything. |
| P1 | Should be in the next release |
| P2 | Planned |
| P3 | Some day |

## Starter issues

Keep 5 to 10 Starter issues in Ready, at least half of them needing no
GPU. A good Starter issue is XS or S, has clear acceptance criteria and an
available Shepherd, and needs little ATLAS-wide context. Also add the
`good first issue` label, so GitHub lists it for newcomers.

## Pull requests

The bot adds `area/*` labels from the changed paths and welcomes
first-time authors. Triagers check that a pull request links an issue and
fills in the template. A maintainer approves CI runs for pull requests
from outside the org, then reviews within 5 business days.

## Every week

- **Blocked view:** is each reason still true?
- **`needs-info`:** close anything silent for 14 days.
- **RFCs view:** ask a maintainer to decide on any RFC past its 7-day
  comment period.
- **Start Here:** keep enough Starter issues in it.

# Incident response

What to do when something goes wrong: a vulnerability report, a leaked
key, a bad release, or automation misbehaving. [SECURITY.md](../SECURITY.md)
covers how reports come in, the severity levels, and disclosure. This page
covers the response.

The **incident lead** is the lead maintainer, the only org owner. Anyone
else who spots a problem reports it: privately for anything
security-related (a [security advisory](https://github.com/inferstep/ATLAS/security/advisories/new)),
publicly otherwise.

## First hour

1. **Write it down.** When it started, what you saw, how you noticed.
   Keep a private timeline; a draft security advisory is a good place.
   Don't delete logs, workflow runs or images yet: they're the evidence.
2. **Rate it** with the severity table in [SECURITY.md](../SECURITY.md).
3. **Contain it** with the matching step below.
4. **Tell the people who need to act** (see [Communicate](#communicate)).

## Contain

| What happened | Do this |
|---|---|
| **Bot key leaked**, or the bot misbehaves | Org **Settings → GitHub Apps → inferstep-atlas-bot → Configure → Suspend** stops it at once. Then delete the key (**Settings → Developer settings → GitHub Apps → inferstep-atlas-bot → Private keys**), generate a new one, and paste it into the `bots` environment secret. |
| **A workflow misbehaves** | **Actions → the workflow → ⋯ → Disable workflow.** To stop every workflow, remove ATLAS from the org's Actions **selected repositories**. |
| **A bad image** behind `:latest` or a version tag | Repoint the tag to the last good release with the `alias` option of `build-images` (see [RELEASE.md](RELEASE.md#release-process)), then approve `production`. Delete a malicious image version only after no tag points at it. |
| **A bad release** | Release tags can't be moved or deleted. Ship a patch release with the fix (a hotfix if `dev` isn't releasable), and say in the bad release's notes that it's withdrawn. |
| **Release signing key** leaked | Remove its line from `.github/allowed_signers` and ship that in a release. Remove the key from the GitHub account, and make a new one. Treat tags signed after the leak as suspect. |
| **A maintainer account** compromised | The owner removes the account from the org and its teams, then reviews the org audit log (**Settings → Logs → Audit log**). Rotate everything that account could reach. |
| **A malicious change** merged | Revert it on `dev` at once. If it reached a release, ship a patch release. |
| **A bad model, Lens or ASA artifact** | Follow **Artifact revocation** in [SECURITY.md](../SECURITY.md). |

## Fix

Land the fix on `dev`. During an embargo (see SECURITY.md) use a neutral
commit message. Release it as a patch, through the hotfix path in
[RELEASE.md](RELEASE.md#hotfixes) if `dev` isn't releasable.

## Communicate

- **Reporter:** keep them informed through the private advisory, and
  agree on the advisory text.
- **Users:** once a fix is released, publish the advisory (and a CVE when
  it warrants one), then pin an issue or post in Discussions →
  Announcements. Say what users must do, e.g. "run `atlas upgrade`" or
  "check your image digest".
- **Contributors:** if a workflow, the bot or a rule was switched off, say
  so on the affected issues and pull requests.

## Afterwards

Within two weeks, write a short review as an issue: what happened, why,
and what changes. If policy changes, it gets an ADR. Update this page with
anything that was missing.

## Where things are

- Security advisories: https://github.com/inferstep/ATLAS/security/advisories
- Org audit log: https://github.com/organizations/inferstep/settings/audit-log
- Actions policy: https://github.com/organizations/inferstep/settings/actions
- Environments (`production`, `bots`): https://github.com/inferstep/ATLAS/settings/environments
- Bot app: https://github.com/organizations/inferstep/settings/apps/inferstep-atlas-bot

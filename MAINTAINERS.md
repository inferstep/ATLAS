# Maintainers

| Name | GitHub | Role | Subsystems |
|---|---|---|---|
| Johnathon Isaac Tigges | [@itigges22](https://github.com/itigges22) | Lead maintainer, release authority, security contact | All (proxy, TUI, CLI, v3-service, lens, sandbox, inference, CI/release) |

Roles, what each can and can't do, and how people move between them are
in the [trust ladder](GOVERNANCE.md#trust-ladder).

**Open seats.** The project is looking for co-maintainers, in order of
need:

1. **Release engineering / CI** — able to cut a release from
   `docs/RELEASE.md` without help (this is the bus-factor-1 gap).
2. **V3 / Lens** — the scoring and selection stack.
3. **Proxy / protocol** — the Go agent loop and SSE contract.

Security contact: GitHub Security Advisories (see
[SECURITY.md](SECURITY.md)). If advisories are unavailable, open a
minimal issue saying "security — need private channel" without details
and the maintainer will provide one.

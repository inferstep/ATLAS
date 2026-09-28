# Release Contract & Verification

This page defines what ATLAS supports, how each capability is gated, and the
verification levels a capability must clear before it is called Supported.
Roadmap items are tracked in GitHub and are not release claims.

## Status definitions

Status terms follow the support-level taxonomy defined in
[SUPPORT_MATRIX.md](../SUPPORT_MATRIX.md) (Supported, Preview, Experimental,
Community-tested, Research-only, Unsupported, Roadmap). One term used here
marks *audience* rather than maturity and composes with a level:

- **Internal:** service-to-service contract, not a public client API.

## User-facing capabilities

| Capability | Status | Minimum verification level |
|---|---|---|
| Python CLI installation and command dispatch | Supported | Hermetic and install matrix |
| TUI chat, file view, pipeline view, and cancellation | Supported | Hermetic Go race tests and local integration |
| Proxy `/v1/agent`, `/events`, `/cancel`, health, readiness, and model listing | Supported | Hermetic Go race tests and local integration |
| Proxy OpenAI chat-completions passthrough | Supported | Local integration |
| Workspace file tools and sandboxed command verification | Supported | Hermetic policy tests and container integration |
| V3 candidate generation and selection for Python | Supported — release-blocked (see below) | Hermetic unit tests and hardware integration |
| V3 verification for non-Python syntax/toolchain checks | Supported | Hermetic unit tests and sandbox integration |
| V3 project build-command verification | Experimental | Hermetic overlay tests plus container integration |
| Model registry list, recommend, install, remove, and verify | Supported | Hermetic CLI tests and hardware integration for inference |
| Lens compatibility check and build (rebuild with `atlas lens build --force`) | Supported for registry entries with compatible artifacts | Hermetic tests and hardware integration |
| Lens and ASA artifact publishing | Experimental | Hermetic CLI tests plus maintainer review workflow |
| ASA compatibility check and build | Experimental | Hermetic tests and hardware integration |
| CUDA backend | Supported | Hardware integration (maintainer hardware) |
| ROCm backend | Community-tested | Community hardware validation (SUPPORT_MATRIX § inference backends) |
| Apple Metal backend | Supported | Maintainer hardware (macOS hybrid) |
| Vulkan backend | Preview | Smoke-tested (lavapipe boot path); no real-GPU validation yet |
| Intel SYCL and multi-GPU backends | Roadmap | None until implemented |
| Browser or visual verification | Roadmap | None until implemented |

**Release-blocked:** V3 candidate generation and selection. Its automated
check, the `tests/e2e` V3-selection assertions, failed at `c3a881d` ("V3 never
selected", 2026-09 audit), and the cause is not established. At that commit,
under the default candidate policy (`strict`, no declared outputs), write and
edit tools did not run V3 generation at all. Since 2026-09-26 there is one
delivery rule and V3 runs for every work request whose file clears the cost
rules ([CANDIDATE_POLICY.md](CANDIDATE_POLICY.md)); the check has not been
re-run since. No release is qualified against this row until the check passes.

## Service contracts

| Service surface | Status | Notes |
|---|---|---|
| Sandbox health, languages, execute, syntax-check, shell, and background jobs | Internal | Called by proxy and V3; direct host use is a developer workflow |
| V3 generate, plan, and health | Internal | `/v3/generate` is the proxy integration path |
| V3 structural edit, symbol index, and complexity endpoints | Experimental (Internal) | Tree-sitter availability determines capability |
| Geometric Lens `/health`, `/ready` and `/internal/*` endpoints | Internal | Every lens route is internal to the stack; the proxy and v3-service are the only callers |
| llama-server inference, completion, embedding, and health | Internal (upstream llama.cpp contract, qualified against the pinned revision) | — |

A feature is Supported once it has been validated on representative hardware
(maintainer-recorded or automated) and, where an automated check exists, that
check passes; the SUPPORT_MATRIX levels govern. Hardware rows cite their
recorded evidence in the SUPPORT_MATRIX.

## Verification

ATLAS separates checks by whether they run on a normal development machine or
require containers, a model, or specific hardware.

### Developer gate

Run the default gate from the repository root:

```bash
python scripts/production-readiness.py
```

The required checks cover test integrity, Python compilation and unit tests,
Go race tests and vet for the proxy and TUI, staticcheck for both, mypy, and a
Dockerfile-source check — 13 gates in all. `min-python` sits alongside
`python-compile`: compilation proves the tree parses on whichever interpreter
is running, while `min-python` compares it against the `requires-python` floor
in `pyproject.toml`. The distinction matters for syntax that parses on every
version but is only *evaluated* correctly on newer ones — a PEP 604 annotation
(`str | None`) is valid syntax on 3.9 and raises `TypeError` at import, which
`compileall` cannot see and the CI test matrix misses because it runs 3.12
only (3.11 appears in the separate perf-gate job). Adding `from __future__ import annotations` to the file clears it. They do not require a GPU, model
download, or running ATLAS services. The developer gate also includes contract
tests for V3 language-aware syntax verification and sandbox overlay behavior.
Full project build-command qualification still belongs to the container and
release levels because it depends on the selected project's dependencies and
toolchain state.

Candidate staging is verified against a **real executor process**, not a stub,
by the optional `staging-executor` gate. Isolation, `finally` teardown, and the
before/after workspace observation are properties of the running executor, and
a stub can only assert the contract was honoured rather than that it is:

```bash
python scripts/production-readiness.py --only staging-executor
python scripts/staging-integration.py          # the same thing, directly
python scripts/staging-integration.py --keep   # leave the executor running
```

The runner starts `sandbox/executor_server.py` on a free port against a
throwaway workspace, points the proxy at the same directory (the alignment
production requires), runs `proxy/staging_integration_test.go`, and tears it
down. Without `ATLAS_STAGING_SANDBOX_URL` and `ATLAS_STAGING_SANDBOX_WORKSPACE`
those Go tests skip with a reason naming what to set — they never pass silently.

Optional checks run when their tools are installed. Missing optional tools are
reported as `unavailable`, not as successful checks. A missing tool becomes a
failure when its gate is selected explicitly:

```bash
python scripts/production-readiness.py --only ruff
python scripts/production-readiness.py --only compose
```

Use `--list` to see the available gates and `--json` for machine-readable
results. CI runs the same named gates after installing their dependencies.

### Verification levels

| Level | Purpose | Hardware or services |
|---|---|---|
| Hermetic | Unit, static, race, and configuration checks | No GPU, model, or running services |
| Local integration | HTTP, SSE, cancellation, and process lifecycle | Locally built binaries; no model where possible |
| Container integration | Compose networking, health, filesystem mounts, and sandbox behavior | Docker |
| Hardware integration | Real inference, embeddings, Lens compatibility, and accelerator behavior | Supported accelerator and registry model |
| Release qualification | Clean install plus all applicable levels and artifact checks | Declared release hardware matrix |

Hardware-dependent checks must name the model and accelerator used. For the
canonical Apple Silicon path, use the registry entry selected by `atlas model
recommend`; release qualification should record the exact registry name, GGUF
hash status, backend, context size, and service image digests.

### Skip policy

- A required dependency missing from a selected gate is a failure.
- An optional dependency missing from the default developer gate is
  `unavailable`.
- A hardware test skipped because the required hardware is absent is
  `unavailable`; it does not count as a pass.
- A supported release cannot be qualified while a required release gate is
  failed or unavailable.

## Branches and versions

| Branch | Job | Gets code by | Artifacts |
|---|---|---|---|
| `dev` | Integration | Merged pull requests, maintainer pushes | `:dev` and an immutable `:sha-<commit>` image per push |
| `staging` | Release candidate | Fast-forward from `dev` | `vX.Y.Z-rc.N` tag, RC images |
| `main` | Released | Fast-forward from `staging` after qualification | Signed `vX.Y.Z` tag, `:latest`, version images |

Contributors always target `dev`. Commits get no version: each one gets an
immutable `sha-*` image and moves `:dev`. A version is decided at release
time by what the release contains (semantic versioning):

- any breaking change → **MAJOR**
- otherwise any `feat` → **MINOR**
- only fixes → **PATCH**

**Breaking** means a change to a CLI flag, a config key, the API or SSE
contract, or on-disk state, or dropping a supported backend. Breaking
changes need an RFC (see [GOVERNANCE](../GOVERNANCE.md#proposals-rfc--adr--epic)).

Image tags drop the `v`: the git tag `v3.2.0` publishes `:3.2.0`,
`:v3.2.0` and `:3.2`. A release candidate `v3.2.0-rc.1` publishes
`:3.2.0-rc.1` and `:v3.2.0-rc.1` only, never `:3.2` or `:latest`.

The release notes come from the conventional pull request titles between
two tags, edited by hand in `CHANGELOG.md`.

## Release rhythm

Minor releases ship **when `dev` is ready**; there's no fixed calendar.
Patch and security releases ship whenever they're needed. A release
candidate stays on `staging` for **at least 3 days** before it becomes a
release.

## Release process

Publishing is gated by the `production` environment
(`scripts/setup/environments.sh`). Only after the release owner approves
the waiting deployment does the pipeline move `:latest` or a version tag.
`dev` pushes deploy to `dev` and release-candidate tags to `staging`,
without approval. Each promotion leaves a timestamped deployment record.

1. **Notes.** On `dev`, bump the "Applies to" line at the top of
   [SUPPORT_MATRIX.md](../SUPPORT_MATRIX.md) and write the
   `CHANGELOG.md` entry.
2. **Candidate.** Fast-forward `staging` to that `dev` commit
   (`git push origin dev:staging`). Check out `staging` and run
   `scripts/release-tag.sh vX.Y.Z-rc.1`. It warns that you're not on
   `main`; answer `y`, since candidates are tagged on `staging`. Push the
   tag. See [Signed release tags](#signed-release-tags).
3. **Test.** Keep the candidate on `staging` for at least 3 days. Qualify
   it per the verification levels above, on release hardware. A fix found
   now lands on `dev` and is promoted again as `-rc.2`.
4. **Release.** Fast-forward `main` to `staging`
   (`git push origin staging:main`). In the build run for that push,
   approve the waiting `production` deployment: **Actions → the run →
   Review deployments → production → Approve**. This moves `:latest`.
5. **Tag.** Check out `main`, cut and push the signed `vX.Y.Z` tag, then
   approve the tag build's `production` deployment the same way. This
   publishes `:X.Y.Z`, `:vX.Y.Z` and `:X.Y`, and `verify-tags` checks the
   signature.
6. **Announce** the release (GitHub release, Discussions Announcements).
   Publishing the GitHub release runs `release-files.yml`, which attaches
   `atlas-X.Y.Z-images.txt` (every image digest of the release) and its
   keyless cosign bundle `atlas-X.Y.Z-images.txt.sigstore.json`.

**Rolling back images.** The `alias` option of `build-images` (run it
manually from `main`; it also needs `production` approval) repoints every
service's tag at an earlier one, e.g. `alias_tag: 3.2.0=latest` puts
`:latest` back on 3.2.0. Nothing is rebuilt.

## Hotfixes

When `dev` isn't releasable and a released version needs a fix:

1. A maintainer branches `hotfix/X.Y.Z` from `main` (only maintainers can
   create branches) and lands the fix there through a pull request.
2. Fast-forward `main` to the hotfix branch, approve `production`, and tag
   `vX.Y.Z` (a patch).
3. Merge `main` back into `dev` so the next promotion stays a
   fast-forward. This is the one merge commit `dev` accepts, and the lead
   makes it as a logged ruleset bypass.

`hotfix/*` is the only branch besides `dev`, `staging` and `main`.

## Signed release tags

Release tags (`vX.Y.Z`) are SSH-signed so their provenance is
verifiable, complementing the keyless **cosign** signatures on the
published image digests (build-images.yml). The two cover different
artifacts: cosign signs the container images, tag signing signs the
git release point.

Cut a release tag with the helper (it signs, verifies, and prints the
push command — it never pushes):

```bash
scripts/release-tag.sh v1.2.0 "release notes"
git push origin v1.2.0        # the deliberate release step
```

On push, `.github/workflows/verify-tags.yml` re-verifies the signature
against `.github/allowed_signers` and fails the tag if it is unsigned or
signed by an unlisted key.

### One-time signing setup (per maintainer machine)

```bash
ssh-keygen -t ed25519 -f ~/.ssh/atlas_release_signing -C "you@example"
git config gpg.format ssh
git config user.signingkey ~/.ssh/atlas_release_signing.pub
git config gpg.ssh.allowedSignersFile .github/allowed_signers
# append "<your-git-email> <contents of atlas_release_signing.pub>" to
# .github/allowed_signers and commit it, then register the key so GitHub
# shows tags as Verified:
gh ssh-key add ~/.ssh/atlas_release_signing.pub --type signing \
    --title "atlas release signing"
```

Verify any release tag locally:

```bash
git config gpg.ssh.allowedSignersFile .github/allowed_signers
git verify-tag v1.2.0
```

Verify a release's image list (from 3.2.0; download both files from the
release page), then check any image against it:

```bash
cosign verify-blob --bundle atlas-3.2.0-images.txt.sigstore.json \
  --certificate-identity https://github.com/inferstep/ATLAS/.github/workflows/release-files.yml@refs/tags/v3.2.0 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  atlas-3.2.0-images.txt
```

**Status:** the signing key, git config, release script, CI verification
and allowed-signers file are in place, and the key is registered on the
maintainer's GitHub account, so GitHub shows release tags as **Verified**
(v3.1.4 onward).

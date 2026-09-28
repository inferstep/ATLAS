# Container Packaging

Reference for how the service images are built: the application account
each runs as, the directories each needs to write, and how dependency
versions are recorded for reproducible builds.

## Application accounts

The four service images run as a **named non-root account created inside
their own image**. No host account, host file, or host permission is
involved.

| Service | Image account | uid:gid | Port (non-privileged) |
|---|---|---|---|
| atlas-proxy | `atlas` | 1001:1001 | 8090 |
| v3-service | `appuser` | 1001:1001 | 8070 |
| geometric-lens | `lens` | 1001:1001 | 8099 |
| sandbox | `sandbox` | 1000:1000 (overridable via `ATLAS_SANDBOX_UID/GID`) | 8020 |
| llama-server (`atlas-llama`, `inference/Dockerfile.v31`) | root (no `USER` directive) | 0:0 | 8080 (`EXPOSE`) |

The inference image is the exception to the non-root pattern: it runs as
root inside its container, isolated by the container boundary and the
localhost-only port publish rather than an unprivileged account.

Application files stay root-owned and world-readable (the `COPY`
default), so the account can read them but not modify the image
contents at runtime.

## Writable directories

Each account can write only where the service actually needs to. Every
other path (including the application code) is read-only to it.

| Service | Writable at runtime | Why |
|---|---|---|
| atlas-proxy | *(none in the container fs)* | Reads config from env/mounts; the workspace and secrets are mounts; logs go to stdout/stderr. Bytecode caching is irrelevant (Go binary). |
| v3-service | `/data/telemetry` (the `v3-telemetry` volume), `$HOME` (`/home/appuser`) | Stage telemetry JSONL (fail-soft when the path is unwritable) plus library caches; everything else the pipeline does is proxied to llama/lens/sandbox. `PYTHONDONTWRITEBYTECODE=1` avoids `.pyc` writes into read-only `/app`. |
| geometric-lens | `$HOME` (`/home/lens`) | Library caches. The models dir, config, and secrets are read-only mounts. `PYTHONDONTWRITEBYTECODE=1` set. |
| sandbox | `/workspace` (mount), the per-language tmpfs set (`/home/sandbox/*`, `/tmp`) | Executes untrusted build/test commands; the tmpfs set is where toolchains install per the universal-tmpfs pattern (see docker-compose.yml). |

## Dependency version recording

Production Python dependencies are pinned with exact `==` versions so a
rebuild resolves the same set:

| File | Scope |
|---|---|
| `geometric-lens/requirements.txt` | Lens runtime (all entries pinned) |
| `v3-service/requirements.txt` | V3 runtime — tree-sitter grammars only; the file states it carries no torch, since the service parses and orchestrates rather than scoring |
| `sandbox/requirements-runtime.txt` | Sandbox executor API |
| `sandbox/requirements-verify.txt` | In-sandbox verify/lint tools (pytest, ruff, mypy, requests) |

The proxy's two Alpine packages are pinned in `proxy/Dockerfile`
(`curl=8.22.0-r0`, `bash=5.3.9-r1`).

`tests/infrastructure/test_dependency_pinning.py` asserts the pinning
policy: requirements files carry `==` pins on every entry, inline
`pip install`s in Dockerfiles are pinned, production base images are
digest-pinned (`@sha256:...`), and the proxy's `apk` packages carry
version pins. OS-level `apt-get` packages on the debian-slim images are
out of scope — the digest-pinned base fixes that snapshot. It is
configuration parsing only — it installs nothing.

### Dependencies without an exact pin (deliberate)

These are deliberately unpinned, for the reasons recorded here:

- **`curl`** in the three debian-slim images (v3, lens, sandbox) — an
  OS healthcheck tool on a digest-pinned base. Exact `apt` version pins
  fight distro security patching and the base digest already fixes the
  snapshot.
- **`nodejs`** in the sandbox — installed from NodeSource; its major
  version is fixed by the `setup_20.x` NodeSource script hardcoded in
  the Dockerfile, not an `apt` pin.

### Reproducibility caveat (Alpine)

The proxy's `apk` version pins target the live Alpine v3.24 repository,
which garbage-collects superseded `-rN` builds. A pin may need bumping
when Alpine ships a patch release; the digest-pinned base keeps the rest
of the toolchain fixed in the meantime. This is inherent to `apk` and is
noted at the pin site.

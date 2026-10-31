# Container Packaging

Reference for how the service images are built: the application account
each runs as, the directories each needs to write, and how dependency
versions are recorded for reproducible builds.

## Application accounts

The five service images each create a **named non-root account inside
their own image** (table below); `docker-compose.yml` runs them with the
installing user's uid:gid instead (see below).

| Service | Image account | uid:gid | Port (non-privileged) |
|---|---|---|---|
| atlas-proxy | `atlas` | 1001:1001 | 8090 |
| v3-service | `appuser` | 1001:1001 | 8070 |
| geometric-lens | `lens` | 1001:1001 | 8099 |
| sandbox | `sandbox` | 1000:1000 (overridable via `ATLAS_SANDBOX_UID/GID`) | 8020 |
| llama-server (`atlas-llama`, `inference/Dockerfile*`) | `atlas`, not the image's default | 1001:1001 | 8080 (`EXPOSE`) |

`docker-compose.yml` runs llama-server, the proxy, the V3 service and the lens
as `ATLAS_PROXY_UID:ATLAS_PROXY_GID`, and the sandbox as
`ATLAS_SANDBOX_UID:ATLAS_SANDBOX_GID`: the installing user's ids, which
`atlas init` and the bootstrap write. So each of them can read the service
token, which they make readable for that user only (0600).

llama-server stops with an error when its token folder or token is there and
cannot be read, so it never starts with internal auth off by accident. The K3s
template mounts no token and sets `runAsUser: 1001`, the image's account `atlas`.
Each inference Dockerfile adds that account to the `video` and `render` groups
where the base image has them, for the GPU devices. On ROCm and Vulkan the
compose overlays run llama-server as `ATLAS_LLAMA_USER` with the host's numbers
for those two groups (`ATLAS_VIDEO_GID`, `ATLAS_RENDER_GID`): a name resolves in
the image, and the device nodes belong to the host's numbers.

The inference images declare no `USER`, so whoever starts one chooses the account,
and a bare `docker run` is root. A compose file from before 3.2.0 sets no user
for llama-server: with a `USER` in the image, that stack could not read its token
and would run with internal auth off.

Application files stay root-owned and world-readable (the `COPY`
default), so the account can read them but not modify the image
contents at runtime.

## Writable directories

Each account can write only where the service actually needs to.

| Service | Writable at runtime | Why |
|---|---|---|
| atlas-proxy | *(none in the container fs)*; the `/workspace` mount (file tools) and the `/data/diagnostics` mount (only when `ATLAS_SHADOW_CAPTURE` is set) | Reads config from env/mounts; secrets are a read-only mount; logs go to stdout/stderr. Bytecode caching is irrelevant (Go binary). |
| v3-service | `/data/telemetry` (the `v3-telemetry` volume, mode 1777 in the image), `$HOME` (`/tmp` under compose) | Stage telemetry JSONL (fail-soft when the path is unwritable, with one line at start that names the repair) plus library caches; everything else the pipeline does is proxied to llama/lens/sandbox. `PYTHONDONTWRITEBYTECODE=1` avoids `.pyc` writes into read-only `/app`. |
| geometric-lens | `$HOME` (`/tmp` under compose) | Library caches. The models dir, config, and secrets are read-only mounts. `PYTHONDONTWRITEBYTECODE=1` set. |
| sandbox | `/workspace` (mount), the per-language tmpfs set (`/home/sandbox/*`, `/tmp`) | Executes untrusted build/test commands; the tmpfs set is where toolchains install per the universal-tmpfs pattern (see docker-compose.yml). |

## Dependency version recording

Production Python dependencies are pinned with exact `==` versions for
every package the files list; the packages those depend on are not
pinned, so a rebuild can resolve newer ones:

| File | Scope |
|---|---|
| `geometric-lens/requirements.txt` | Lens runtime (all entries pinned) |
| `v3-service/requirements.txt` | V3 runtime — tree-sitter grammars only; the file states it carries no torch, since the service parses and orchestrates rather than scoring |
| `sandbox/requirements-runtime.txt` | Sandbox executor API |
| `sandbox/requirements-verify.txt` | In-sandbox verify/lint tools (pytest, ruff, mypy, requests) and the executor's YAML and Jinja checkers (pyyaml, jinja2) |

The proxy's two Alpine packages are pinned in `proxy/Dockerfile`
(`curl=8.22.0-r0`, `bash=5.3.9-r1`).

`tests/infrastructure/test_dependency_pinning.py` asserts the pinning
policy: requirements files carry `==` pins on every entry, inline
`pip install`s in Dockerfiles are pinned, the four service images' bases are
digest-pinned (`@sha256:...`), and the proxy's `apk` packages carry
version pins. OS-level `apt-get` packages on the debian-slim images are
out of scope: they come from the live Debian repository at build time, so
a rebuild can install newer patched versions. It is
configuration parsing only — it installs nothing.

### Dependencies without an exact pin (deliberate)

These are deliberately unpinned, for the reasons recorded here:

- **`apt` packages** in the three debian-slim images: `curl` in v3, lens
  and sandbox, plus the sandbox's toolchain packages (gcc, git,
  openjdk-21-jdk-headless, ruby, php-cli and others). They come from the
  live Debian repository at build time; exact `apt` version pins fight
  distro security patching.

### Reproducibility caveat (Alpine)

The proxy's `apk` version pins target the live Alpine v3.24 repository.
A pin may need bumping
when Alpine ships a patch release; the digest-pinned base keeps the rest
of the toolchain fixed in the meantime. This is inherent to `apk` and is
noted at the pin site.

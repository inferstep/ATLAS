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
| llama-server (`atlas-llama`, `services/inference/Dockerfile*`) | `atlas`, not the image's default | 1001:1001 | 8080 (`EXPOSE`) |

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
| v3-service | `/data/telemetry` (the `v3-telemetry` volume; mode 770 and group 1001 in the image, a group compose adds to the service), `$HOME` (`/tmp` under compose) | Stage telemetry JSONL (fail-soft when the path is unwritable, with one line at start that names the repair) plus library caches; everything else the pipeline does is proxied to llama/lens/sandbox. `PYTHONDONTWRITEBYTECODE=1` avoids `.pyc` writes into read-only `/app`. |
| geometric-lens | `$HOME` (`/tmp` under compose) | Library caches. The models dir, config, and secrets are read-only mounts. `PYTHONDONTWRITEBYTECODE=1` set. |
| sandbox | `/workspace` (mount), the per-language tmpfs set (`/home/sandbox/*`, `/tmp`) | Executes untrusted build/test commands; the tmpfs set is where toolchains install per the universal-tmpfs pattern (see docker-compose.yml). |

## Dependency version recording

Production Python dependencies are pinned with exact `==` versions for
every package the files list. Each service image installs them from a
hashed lock beside them (`requirements-lock.txt`, made by
`scripts/ci/ci-lock.sh`), which pins the packages those depend on too:

| File | Scope |
|---|---|
| `services/geometric-lens/requirements.txt` | Lens runtime (all entries pinned) |
| `services/v3-service/requirements.txt` | V3 runtime — tree-sitter grammars only; the file states it carries no torch, since the service parses and orchestrates rather than scoring |
| `services/sandbox/requirements-runtime.txt` | Sandbox executor API |
| `services/sandbox/requirements-verify.txt` | In-sandbox verify/lint tools (pytest, ruff, mypy, requests) and the executor's YAML and Jinja checkers (pyyaml, jinja2) |

The proxy's two Alpine packages are pinned in `services/proxy/Dockerfile`
(`curl=8.23.0-r0`, `bash=5.3.9-r1`).

`tests/infrastructure/supply/test_dependency_pinning.py` asserts the pinning
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

- **Dependencies of the global npm tools**: the Dockerfile pins TypeScript,
  tsx, pnpm, and Yarn, but it has no npm lockfile. Their dependency ranges
  can select different versions on later builds. For example, tsx 4.23.15
  permits esbuild through `~0.28.0`.

Neither these npm installations nor the distribution packages provide exact
reproducibility. The base image digest and direct archive checksums do not
freeze packages that a later installation resolves.

### Sandbox toolchains

Review date: **2026-11-08**. The [sandbox Dockerfile](../../services/sandbox/Dockerfile)
records the installation choices. This table describes those choices, not
an inventory from a running branch image.

The pinned [Python base image source][python-image] uses Debian 13 (`trixie`).
For distribution packages, the table names the release series and package
source. Exact installed package revisions remain unknown until the image
is built. They can differ between builds and architectures.

| Toolchain or companion tool | Version and installation source | Support policy and end date |
|---|---|---|
| Python | 3.13.18, included in the [base image][python-image]. | Python 3.13 receives security fixes until **2029-10**, under the [Python release schedule][python-support]. |
| pip, the Python package manager | 26.2.2, bundled through [Python's `ensurepip` module][python-pip]. | The [pip release process][pip-support] schedules regular releases. It gives no fixed support end date for this version. |
| Node.js | 24.22.0, from the [release archive][node-download]. The Dockerfile records SHA-256 checksums for amd64 and arm64. | Node 24 is in maintenance long-term support (LTS). The [Node schedule][node-support] ends support on **2028-04-30**. |
| npm and its `npx` command | 11.19.1, included in the [Node 24.22.0 archive][node-npm]. | The [npm version policy][npm-support] describes stable and prerelease channels. It gives no separate fixed support end date. |
| Corepack, the package-manager launcher | 0.36.0, included in the [Node 24.22.0 archive][node-corepack]. | The [Corepack security policy][corepack-support] gives no fixed support end date. |
| TypeScript compiler (`tsc`) | 7.0.2, installed through npm from the [versioned registry package][typescript-package]. | The [TypeScript release process][typescript-support] defines regular and patch releases. It gives no fixed support end date. |
| TypeScript runner (`tsx`) | 4.23.15, installed through npm from the [versioned registry package][tsx-package]. | The [tsx security policy][tsx-support] covers only the latest release. It gives no fixed calendar end date. |
| Go compiler and module tools | 1.27.2, from the [Go release manifest][go-download]. The Dockerfile records SHA-256 checksums for amd64 and arm64. | The [Go release policy][go-support] supports a release until two newer major releases exist. Go 1.27 support ends with Go 1.29; the date is not fixed. |
| Rust compiler (`rustc`) and Cargo package manager | Rust 1.99.1 and Cargo 1.99.1, selected by the [toolchain manifest][rust-download]. rustup installs them. | The [Rust release process][rust-support] produces stable releases every six weeks. It gives no fixed support end date for this toolchain. |
| rustup, the Rust toolchain installer | 1.29.1, from the [rustup archives][rustup-download]. The Dockerfile checks the installer before execution. | The [rustup security policy][rustup-support] gives no fixed support end date. |
| Bun runtime, package manager, and `bunx` command | 1.4.3, from the [Bun release][bun-download]. The Dockerfile records SHA-256 checksums for amd64 and arm64. | The [Bun security policy][bun-support] lists 1.x as supported. It gives no fixed support end date. |
| Java runtime (`java`) and compiler (`javac`) | OpenJDK 21; Debian selects the revision of [`openjdk-21-jdk-headless`][java-package] during the build. | [Debian support][debian-support] applies. The distribution schedule does not promise an individual package support end date. |
| Kotlin compiler (`kotlinc`) and runner | 2.4.0, from the [Kotlin compiler archive][kotlin-download], checked with SHA-256. It uses the installed Java runtime. | The [Kotlin release process][kotlin-support] defines language, tooling, and bug-fix releases. It gives no fixed support end date. |
| Ruby and RubyGems (`gem`) | Ruby 3.3; Debian selects revisions of [`ruby`][ruby-package] and [`ruby-rubygems`][rubygems-package] during the build. | [Debian support][debian-support] applies. Separately, the [Ruby schedule][ruby-support] expects upstream Ruby 3.3 support to end on **2027-03-31**. |
| PHP command-line interpreter | PHP 8.4; Debian selects the revision through [`php-cli`][php-package] during the build. | [Debian support][debian-support] applies. Separately, [upstream PHP 8.4 security support][php-support] ends on **2028-12-31**. |
| C and C++ compilers, linker, and build tool | GCC 14; Debian selects revisions of [`gcc`][gcc-package], [`g++`][gpp-package], [`binutils`][binutils-package], and [`make`][make-package]. | [Debian support][debian-support] applies. The distribution schedule does not promise individual package support end dates. |
| Bash interpreter | Bash 5.2; Debian selects the revision of [`bash`][bash-package] during the build. | [Debian support][debian-support] applies. The distribution schedule does not promise an individual package support end date. |
| pnpm package manager | 12.14.0, installed through npm from the [versioned registry package][pnpm-package]. | The [pnpm security policy][pnpm-support] lists 12.x as supported. It gives no fixed support end date for 12.x. |
| Yarn package manager | Classic 1.22.22, installed through npm from the [versioned registry package][yarn-package]. | [Yarn Classic][yarn-support] has been in maintenance mode since January 2020. Its support end date is not fixed. |

Debian 13 has full distribution support until **2028-08-09**, then LTS until
**2030-06-30**. LTS can restrict package and architecture coverage.
These dates do not guarantee support for every package.
Debian can backport security fixes without adopting the latest upstream
version. See the [Debian security policy][debian-security].

SHA-256 is the download checksum algorithm. The Dockerfile checks every
direct tool archive before extraction, and checks `rustup-init` before execution.
Node, Go, rustup, and Bun reject architectures other than amd64 and arm64.
Kotlin uses one archive for both architectures.

Package managers provide different verification boundaries:

- `apt` checks signed Debian metadata and the package hashes that it records.
  Unpinned packages permit Debian security updates.
- npm checks registry integrity metadata. The Dockerfile disables installation
  scripts and pins the four requested tools. Dependencies remain unlocked.
- rustup checks downloads against the selected release manifest.
- pip checks every installed dependency against the committed requirement
  hashes. It permits only built packages.

For a support review, record the image identifier, build date, architecture,
tool versions, and `dpkg-query -W` output. Record `npm ls -g --all` to identify
resolved dependencies. Scan that same image before publication.
Report findings in the installed Go and Node tools separately from findings
in bundled binaries, such as esbuild or the TypeScript compiler.
Keep unrelated findings in the scan report.
A supported release line does not establish that a particular patch version
has no known vulnerabilities.

[python-image]: https://github.com/docker-library/python/blob/152874b138f0b1c4699fbdcb52b174608cfbfff5/3.13/slim-trixie/Dockerfile
[python-support]: https://devguide.python.org/versions/
[python-pip]: https://github.com/python/cpython/blob/v3.13.18/Lib/ensurepip/__init__.py
[pip-support]: https://pip.pypa.io/en/stable/development/release-process/
[node-download]: https://nodejs.org/dist/v24.22.0/SHASUMS256.txt
[node-support]: https://github.com/nodejs/Release/blob/main/schedule.json
[node-npm]: https://github.com/nodejs/node/blob/v24.22.0/deps/npm/package.json
[npm-support]: https://docs.npmjs.com/about-npm-versions/
[node-corepack]: https://github.com/nodejs/node/blob/v24.22.0/deps/corepack/package.json
[corepack-support]: https://github.com/nodejs/corepack/security/policy
[typescript-package]: https://registry.npmjs.org/typescript/7.0.2
[typescript-support]: https://github.com/microsoft/TypeScript/wiki/TypeScript's-Release-Process
[tsx-package]: https://registry.npmjs.org/tsx/4.23.15
[tsx-support]: https://github.com/privatenumber/tsx/security/policy
[go-download]: https://go.dev/dl/?mode=json&include=all
[go-support]: https://go.dev/doc/devel/release#policy
[rust-download]: https://static.rust-lang.org/dist/channel-rust-1.99.1.toml
[rust-support]: https://doc.rust-lang.org/book/appendix-07-nightly-rust.html
[rustup-download]: https://static.rust-lang.org/rustup/archive/1.29.1/x86_64-unknown-linux-gnu/rustup-init.sha256
[rustup-support]: https://github.com/rust-lang/rustup/security/policy
[bun-download]: https://github.com/oven-sh/bun/releases/tag/bun-v1.4.3
[bun-support]: https://github.com/oven-sh/bun/security/policy
[java-package]: https://packages.debian.org/trixie/openjdk-21-jdk-headless
[kotlin-download]: https://github.com/JetBrains/kotlin/releases/tag/v2.4.0
[kotlin-support]: https://kotlinlang.org/docs/releases.html
[ruby-package]: https://packages.debian.org/trixie/ruby
[rubygems-package]: https://packages.debian.org/trixie/ruby-rubygems
[ruby-support]: https://www.ruby-lang.org/en/downloads/branches/
[php-package]: https://packages.debian.org/trixie/php-cli
[php-support]: https://www.php.net/supported-versions.php
[gcc-package]: https://packages.debian.org/trixie/gcc
[gpp-package]: https://packages.debian.org/trixie/g++
[binutils-package]: https://packages.debian.org/trixie/binutils
[make-package]: https://packages.debian.org/trixie/make
[bash-package]: https://packages.debian.org/trixie/bash
[pnpm-package]: https://registry.npmjs.org/pnpm/12.14.0
[pnpm-support]: https://github.com/pnpm/pnpm/security/policy
[yarn-package]: https://registry.npmjs.org/yarn/1.22.22
[yarn-support]: https://classic.yarnpkg.com/en/docs/install/
[debian-support]: https://www.debian.org/releases/trixie/
[debian-security]: https://www.debian.org/security/faq

### Reproducibility caveat (Alpine)

The proxy's `apk` version pins target the live Alpine v3.24 repository.
A pin may need bumping
when Alpine ships a patch release; the digest-pinned base keeps the rest
of the toolchain fixed in the meantime. This is inherent to `apk` and is
noted at the pin site.

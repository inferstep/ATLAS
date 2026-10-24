# python:3.13-slim — digest-pinned for reproducible builds (Dependabot maintains).
# Kept current deliberately: the sandbox is what executes and syntax-checks
# agent output, so a Python older than the one models write for turns valid
# code into a rejection. On 3.11 a model burned a whole session re-emitting
# f"{d["k"]}" — a SyntaxError there, valid from 3.12 (PEP 701).
FROM docker.io/python:3.13-slim@sha256:70729b46c69b4f1e97c4822c1af3df53a1476cf5ddc6c087c0c10bc3a5678c2f
WORKDIR /app

# Install system dependencies and language runtimes
RUN apt-get update && apt-get install -y --no-install-recommends \
    # C/C++
    gcc g++ make \
    # Bash/Shell
    bash \
    # Downloads and archives
    curl ca-certificates gnupg unzip \
    # Common CLI tools. The sandbox runs non-root on a read-only fs, so
    # nothing can be apt-installed at runtime — anything a coding task
    # commonly shells out to must be baked in. git and sqlite3 were the
    # two observed gaps (TB2 bench 2026-07-18: `git clone` and
    # `sqlite3 .recover` both dead-ended with "command not found");
    # jq/patch/zip/xz are the same class: small, ubiquitous in build
    # scripts, impossible to add later.
    git sqlite3 jq patch zip xz-utils \
    # Binary-inspection tools. binutils (strings/objdump/readelf/nm)
    # rides in with gcc, but `file` (type detection) and `xxd` (hex dump)
    # are separate and are what the binary-file read guard points at.
    file xxd \
    # Archive tools a coding task commonly shells out to and can't
    # apt-install at runtime (7z was the observed gap — crack-7z-hash
    # dead-ended on "7z: command not found", and 7z is not pip-installable).
    p7zip-full \
    && rm -rf /var/lib/apt/lists/*

# Install Node.js 24 LTS (supported to 2028-04-30) from its release archive,
# checked against the published SHA-256
ARG NODE_VERSION=24.22.0
ARG NODE_SHA256_AMD64=cc2f601cee82d9a5a233302fe307abc600727035fd7d2c3a3571755989e95008
ARG NODE_SHA256_ARM64=fef00bb4d36e1c50104a9775a10359beee98f162b4104dd12472c75065c6a143
RUN case "$(dpkg --print-architecture)" in \
      amd64) arch=x64; sha="${NODE_SHA256_AMD64}" ;; \
      arm64) arch=arm64; sha="${NODE_SHA256_ARM64}" ;; \
      *) echo "no Node.js archive for $(dpkg --print-architecture)" >&2; exit 1 ;; \
    esac \
    && curl --proto '=https' --tlsv1.2 -fsSL -o /tmp/node.tar.xz "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${arch}.tar.xz" \
    && echo "${sha}  /tmp/node.tar.xz" | sha256sum -c - \
    && tar -xJf /tmp/node.tar.xz -C /usr/local --strip-components=1 --no-same-owner \
    && rm /tmp/node.tar.xz \
    && node --version && npm --version

# Install Go, checked against the published SHA-256
ARG GO_VERSION=1.27.2
ARG GO_SHA256_AMD64=ecbadb99091a3f46e31f5f934b068b1864eafa7995211b39eaddf76996045fe5
ARG GO_SHA256_ARM64=94f3e30b8e374bc285e7dadc11e0865726b9bc6e85b841ccceaabc0214c6b7c8
RUN arch="$(dpkg --print-architecture)" \
    && case "${arch}" in \
      amd64) sha="${GO_SHA256_AMD64}" ;; \
      arm64) sha="${GO_SHA256_ARM64}" ;; \
      *) echo "no Go archive for ${arch}" >&2; exit 1 ;; \
    esac \
    && curl --proto '=https' --tlsv1.2 -fsSL -o /tmp/go.tar.gz "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" \
    && echo "${sha}  /tmp/go.tar.gz" | sha256sum -c - \
    && tar -C /usr/local -xzf /tmp/go.tar.gz \
    && rm /tmp/go.tar.gz
ENV PATH="/usr/local/go/bin:${PATH}"
RUN go version

# Install Java 21 (JDK headless — javac + java, no GUI libs)
RUN apt-get update && apt-get install -y --no-install-recommends \
    openjdk-21-jdk-headless \
    && rm -rf /var/lib/apt/lists/*
RUN java --version && javac --version

# Install Kotlin 2.4.0 (JVM target — requires Java, installed above)
ARG KOTLIN_VERSION=2.4.0
ARG KOTLIN_SHA256=ba1b9e6eb6ddc3275079224f2e9ea4a2b02eef7d59ce2d38404f04b22613c20a

RUN curl -fsSL -o /tmp/kotlin.zip \
    "https://github.com/JetBrains/kotlin/releases/download/v${KOTLIN_VERSION}/kotlin-compiler-${KOTLIN_VERSION}.zip" \
    && echo "${KOTLIN_SHA256}  /tmp/kotlin.zip" | sha256sum -c - \
    && unzip -q /tmp/kotlin.zip -d /opt \
    && rm /tmp/kotlin.zip \
    && /opt/kotlinc/bin/kotlinc -version

ENV PATH="/opt/kotlinc/bin:${PATH}"

# Install Ruby (interpreter, no compile step)
RUN apt-get update && apt-get install -y --no-install-recommends \
    ruby \
    && rm -rf /var/lib/apt/lists/*
RUN ruby --version

# Install PHP (CLI, no compile step)
RUN apt-get update && apt-get install -y --no-install-recommends \
    php-cli \
    && rm -rf /var/lib/apt/lists/*
RUN php --version

# Install Rust (rustup, a fixed toolchain) under /opt so the runtime
# user (non-root) can read it. RUSTUP_HOME stays /opt/rustup at runtime
# (read-only toolchain); CARGO_HOME is repointed to the user's tmpfs
# home by compose so registry/cache writes work on the read-only root.
# rustup-init is checked against its published SHA-256; rustup checks the
# toolchain it downloads.
ENV RUSTUP_HOME=/opt/rustup CARGO_HOME=/opt/cargo
ARG RUSTUP_VERSION=1.29.1
ARG RUSTUP_SHA256_AMD64=dda7234360b7f578ca8b0ddcb80145646fa61a67c1720a5abc7051b35c9fcb71
ARG RUSTUP_SHA256_ARM64=15f6e4ce9f583b929c996c91562bad6d4454f3281de858b02cdfdef615fac433
ARG RUST_VERSION=1.99.1
RUN case "$(dpkg --print-architecture)" in \
      amd64) target=x86_64-unknown-linux-gnu; sha="${RUSTUP_SHA256_AMD64}" ;; \
      arm64) target=aarch64-unknown-linux-gnu; sha="${RUSTUP_SHA256_ARM64}" ;; \
      *) echo "no rustup-init for $(dpkg --print-architecture)" >&2; exit 1 ;; \
    esac \
    && curl --proto '=https' --tlsv1.2 -fsSL -o /tmp/rustup-init \
      "https://static.rust-lang.org/rustup/archive/${RUSTUP_VERSION}/${target}/rustup-init" \
    && echo "${sha}  /tmp/rustup-init" | sha256sum -c - \
    && chmod +x /tmp/rustup-init \
    && /tmp/rustup-init -y --default-toolchain "${RUST_VERSION}" --profile minimal \
    && rm /tmp/rustup-init \
    && chmod -R a+rX /opt/rustup /opt/cargo
ENV PATH="/opt/cargo/bin:${PATH}"
RUN rustc --version && cargo --version

# Install JavaScript/TypeScript verification tools and package managers.
# Project detection may select npm, pnpm, yarn, or bun from lockfiles; the
# sandbox needs all of them available for build-command verification to be
# accurate.
RUN npm install -g --ignore-scripts typescript@7.0.2 tsx@4.23.15 pnpm@12.14.0 yarn@1.22.22
RUN tsc --version && pnpm --version && yarn --version

# Install Bun for bun.lockb projects (under /opt for non-root reads),
# checked against the published SHA-256.
ARG BUN_VERSION=1.4.3
ARG BUN_SHA256_AMD64=3e730528d89775f03f87f05c06787c4df4099d0a37f3f252e7b829cb23816e18
ARG BUN_SHA256_ARM64=efa9813da5ed72423bf847f916e8d2c47c0d776add972354026a75e10da9aa21
RUN case "$(dpkg --print-architecture)" in \
      amd64) target=x64; sha="${BUN_SHA256_AMD64}" ;; \
      arm64) target=aarch64; sha="${BUN_SHA256_ARM64}" ;; \
      *) echo "no Bun archive for $(dpkg --print-architecture)" >&2; exit 1 ;; \
    esac \
    && curl --proto '=https' --tlsv1.2 -fsSL -o /tmp/bun.zip \
      "https://github.com/oven-sh/bun/releases/download/bun-v${BUN_VERSION}/bun-linux-${target}.zip" \
    && echo "${sha}  /tmp/bun.zip" | sha256sum -c - \
    && unzip -q /tmp/bun.zip -d /tmp/bun \
    && mkdir -p /opt/bun/bin \
    && mv "/tmp/bun/bun-linux-${target}/bun" /opt/bun/bin/bun \
    && ln -s /opt/bun/bin/bun /opt/bun/bin/bunx \
    && rm -rf /tmp/bun.zip /tmp/bun \
    && chmod -R a+rX /opt/bun
ENV PATH="/opt/bun/bin:${PATH}"
RUN bun --version

# Pre-baked Python deps — kept INTENTIONALLY MINIMAL after PC-191.
#
# Two groups only:
#   1. executor_server runtime (fastapi/uvicorn/pydantic) — the
#      sandbox API itself depends on these.
#   2. Universal verify helpers (pytest/requests/ruff/mypy) — these
#      are language-agnostic enough that almost every Python project
#      uses one of them, and they're slow to install on demand.
#
# Application libraries (flask/django/numpy/pandas/...) are NOT
# pre-baked. PC-190 originally baked them in to work around the
# read-only fs issue, but that was the wrong layer — PC-191's
# universal tmpfs mounts (see docker-compose.yml) make `pip install`
# work fine, and the project's own venv / requirements.txt is the
# right source of truth for application deps anyway. Keeping the
# image lean means: smaller pull, smaller attack surface, no drift
# between sandbox-baked-flask and user-pinned-flask, and the same
# pattern works for Node/Rust/Go/Ruby/Java/PHP without favoring
# Python. Keep the API runtime dependencies in one shared file so the
# container and the unit-test environment cannot drift apart.
COPY requirements-runtime.txt /tmp/sandbox-requirements-runtime.txt
COPY requirements-verify.txt /tmp/sandbox-requirements-verify.txt
RUN pip install --no-cache-dir \
    -r /tmp/sandbox-requirements-runtime.txt \
    -r /tmp/sandbox-requirements-verify.txt

# Create sandbox workspace
RUN mkdir -p /tmp/sandbox && chmod 777 /tmp/sandbox

# Non-root runtime user. Compose may override the uid/gid at runtime
# (ATLAS_SANDBOX_UID/GID, written by `atlas init` to match the host
# user so /workspace writes keep the right owner); HOME comes from the
# compose environment because an overridden uid has no passwd entry.
RUN groupadd -g 1000 sandbox && useradd -u 1000 -g 1000 -d /home/sandbox -s /bin/bash sandbox \
    && mkdir -p /home/sandbox && chown 1000:1000 /home/sandbox

COPY structured_log.py .
COPY private_values.py .
# The resource contract every untrusted command runs under. Imported by
# executor_server at module load, so the executor does not start without it.
COPY resource_contract.py .
COPY executor_models.py .
COPY executor_parsers.py .
COPY executor_server.py .

EXPOSE 8020

USER sandbox
ENV HOME=/home/sandbox

CMD ["python", "executor_server.py"]

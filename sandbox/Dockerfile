# python:3.13-slim — digest-pinned for reproducible builds (Dependabot maintains).
# Kept current deliberately: the sandbox is what executes and syntax-checks
# agent output, so a Python older than the one models write for turns valid
# code into a rejection. On 3.11 a model burned a whole session re-emitting
# f"{d["k"]}" — a SyntaxError there, valid from 3.12 (PEP 701).
FROM docker.io/python:3.13-slim@sha256:6771159cd4fa5d9bba1258caf0b82e6b73458c694d178ad97c5e925c2d0e1a91
WORKDIR /app

# Install system dependencies and language runtimes
RUN apt-get update && apt-get install -y --no-install-recommends \
    # C/C++
    gcc g++ make \
    # Bash/Shell
    bash \
    # Node.js (via nodesource)
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

# Install Node.js 20 LTS
RUN curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/* \
    && node --version && npm --version

# Install Go 1.22
RUN ARCH=$(dpkg --print-architecture) && \
    curl -fsSL https://go.dev/dl/go1.22.10.linux-${ARCH}.tar.gz | tar -C /usr/local -xz
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

# Install Rust (rustup, stable toolchain) under /opt so the runtime
# user (non-root) can read it. RUSTUP_HOME stays /opt/rustup at runtime
# (read-only toolchain); CARGO_HOME is repointed to the user's tmpfs
# home by compose so registry/cache writes work on the read-only root.
ENV RUSTUP_HOME=/opt/rustup CARGO_HOME=/opt/cargo
RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --default-toolchain stable --profile minimal \
    && chmod -R a+rX /opt/rustup /opt/cargo
ENV PATH="/opt/cargo/bin:${PATH}"
RUN rustc --version && cargo --version

# Install JavaScript/TypeScript verification tools and package managers.
# Project detection may select npm, pnpm, yarn, or bun from lockfiles; the
# sandbox needs all of them available for build-command verification to be
# accurate.
RUN npm install -g typescript tsx pnpm yarn
RUN tsc --version && pnpm --version && yarn --version

# Install Bun for bun.lockb projects (under /opt for non-root reads).
ENV BUN_INSTALL=/opt/bun
RUN curl -fsSL https://bun.sh/install | bash \
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
COPY executor_server.py .

EXPOSE 8020

USER sandbox
ENV HOME=/home/sandbox

CMD ["python", "executor_server.py"]

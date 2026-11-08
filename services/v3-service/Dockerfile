# python:3.13-slim, the same digest-pinned base the sandbox runs (Dependabot
# maintains). Every compile() and ast.parse verdict this service gives --
# structural_edit's post-splice check, the call graph, the lints -- is about
# code the sandbox then executes, so it has to use that interpreter's grammar.
# On 3.11 it rejected valid 3.12+ code (PEP 701 f"{d["k"]}", PEP 695 type
# parameters) that the sandbox runs fine.
FROM docker.io/python:3.13-slim@sha256:ae14d4ce8771606477a81446e7ef429edd6762305fc50c84507c1041fa8285d1
WORKDIR /app

# The week of the commit, which build-images.yml passes for a push to dev and
# for a PR build (a release build passes none). A commit of a new week runs
# the package installs below again, so they take the distribution's fixes
# between base bumps; within a week they come from the cache.
ARG PACKAGE_REFRESH

# Install system deps
RUN apt-get update && apt-get install -y --no-install-recommends curl && rm -rf /var/lib/apt/lists/*

# Install Python deps before copying source so code edits do not invalidate
# the tree-sitter layer during local image rebuilds. This service parses and
# orchestrates; it never scores — all tensor work lives in geometric-lens,
# so no torch here (it shipped ~900 MB of it unimported until 2026-08-01).
COPY services/v3-service/requirements-lock.txt /tmp/v3-requirements.txt
RUN pip install --no-cache-dir --require-hashes --only-binary=:all: -r /tmp/v3-requirements.txt

# V3 pipeline stages (candidate generation, selection, repair)
COPY services/v3-service/stages/ /app/stages/
COPY services/v3-service/main.py /app/main.py
COPY services/v3-service/adapters.py /app/adapters.py
COPY services/v3-service/scoring.py /app/scoring.py
COPY services/v3-service/symbols.py /app/symbols.py
COPY services/v3-service/grammars.py /app/grammars.py
COPY services/v3-service/edit_checks.py /app/edit_checks.py
COPY services/v3-service/embedded_script.py /app/embedded_script.py
COPY services/v3-service/node_splice.py /app/node_splice.py
COPY services/v3-service/planning.py /app/planning.py
COPY services/v3-service/pipeline.py /app/pipeline.py
COPY services/v3-service/pool_capture.py /app/pool_capture.py
COPY services/v3-service/candidate_cases.py /app/candidate_cases.py
COPY services/v3-service/stage_telemetry.py /app/stage_telemetry.py
COPY services/v3-service/candidate_records.py /app/candidate_records.py
COPY services/v3-service/vetoes.py /app/vetoes.py
COPY services/v3-service/runtime_verification.py /app/runtime_verification.py
# contract.py was added and the image kept building without it: this COPY
# list enumerates modules, so a new one is silently excluded and the service
# dies at import with the whole suite green. A DELETED one breaks the build
# instead, which is how evidence.py's retirement surfaced here.
COPY services/v3-service/contract.py /app/contract.py
COPY services/v3-service/private_values.py /app/private_values.py
COPY services/v3-service/structured_log.py /app/structured_log.py
# Structural call-graph reasoning (issue #39): tree-sitter-backed call graph.
# Pure stdlib + the tree-sitter grammars installed above; no extra deps.
COPY services/v3-service/graph/ /app/graph/

# Prove the image can import what it ships. A missing module is a build
# failure here rather than a service that starts, fails at import and is
# discovered by a container that will not come up.
RUN cd /app && python -c "import main, adapters, pipeline, contract"

# Non-root application account created inside this image. The V3 service
# writes only to /data/telemetry (stage telemetry, fail-soft) and HOME
# (library caches) — everything else it does is proxied to
# llama/lens/sandbox — so /app stays root-owned + world-readable and the
# app user needs no writable app directory. Bytecode caching is disabled
# so Python never tries to write .pyc into the read-only /app.
ENV PYTHONDONTWRITEBYTECODE=1
RUN groupadd --gid 1001 appuser && useradd --create-home --uid 1001 --gid 1001 appuser && \
    # Pre-create the telemetry path: a named volume copies its owner and
    # mode on first mount, so stage telemetry is writable without a root
    # init step. docker-compose.yml runs the service as the installing user,
    # not as appuser, and adds group 1001 to it: the folder is writable for
    # its owner and that group only.
    mkdir -p /data/telemetry && chown appuser:appuser /data/telemetry && chmod 770 /data/telemetry
USER appuser
ENV HOME=/home/appuser

EXPOSE 8070

CMD ["python", "main.py"]

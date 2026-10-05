# python:3.13-slim, the same digest-pinned base the sandbox runs (Dependabot
# maintains). Every compile() and ast.parse verdict this service gives --
# structural_edit's post-splice check, the call graph, the lints -- is about
# code the sandbox then executes, so it has to use that interpreter's grammar.
# On 3.11 it rejected valid 3.12+ code (PEP 701 f"{d["k"]}", PEP 695 type
# parameters) that the sandbox runs fine.
FROM docker.io/python:3.13-slim@sha256:7c61056e61ac89e852de05f3dc6fa51a6dd2181797bceed46aa725dd7cb2cd3b
WORKDIR /app

# Install system deps
RUN apt-get update && apt-get install -y --no-install-recommends curl && rm -rf /var/lib/apt/lists/*

# Install Python deps before copying source so code edits do not invalidate
# the tree-sitter layer during local image rebuilds. This service parses and
# orchestrates; it never scores — all tensor work lives in geometric-lens,
# so no torch here (it shipped ~900 MB of it unimported until 2026-08-01).
COPY v3-service/requirements.txt /tmp/v3-requirements.txt
RUN pip install --no-cache-dir -r /tmp/v3-requirements.txt

# V3 pipeline stages (candidate generation, selection, repair)
COPY v3-service/stages/ /app/stages/
COPY v3-service/main.py /app/main.py
COPY v3-service/adapters.py /app/adapters.py
COPY v3-service/scoring.py /app/scoring.py
COPY v3-service/symbols.py /app/symbols.py
COPY v3-service/node_splice.py /app/node_splice.py
COPY v3-service/planning.py /app/planning.py
COPY v3-service/pipeline.py /app/pipeline.py
COPY v3-service/runtime_verification.py /app/runtime_verification.py
# contract.py was added and the image kept building without it: this COPY
# list enumerates modules, so a new one is silently excluded and the service
# dies at import with the whole suite green. A DELETED one breaks the build
# instead, which is how evidence.py's retirement surfaced here.
COPY v3-service/contract.py /app/contract.py
COPY v3-service/private_values.py /app/private_values.py
COPY v3-service/structured_log.py /app/structured_log.py
# Structural call-graph reasoning (issue #39): tree-sitter-backed call graph.
# Pure stdlib + the tree-sitter grammars installed above; no extra deps.
COPY v3-service/graph/ /app/graph/

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
RUN useradd --create-home --uid 1001 --user-group appuser && \
    # Pre-create the telemetry path owned by appuser: a named volume
    # copies this ownership on first mount, so stage telemetry is
    # writable without a root init step.
    mkdir -p /data/telemetry && chown appuser:appuser /data/telemetry
USER appuser
ENV HOME=/home/appuser

EXPOSE 8070

CMD ["python", "main.py"]

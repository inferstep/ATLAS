# python:3.11-slim — digest-pinned for reproducible builds (Dependabot maintains)
FROM docker.io/python:3.11-slim@sha256:da047cb8f9d1d98e5c070f5300ba9f7274e33b8fc0e5be5ed88740aed1b95ba9
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
COPY v3-service/planning.py /app/planning.py
COPY v3-service/pipeline.py /app/pipeline.py
COPY v3-service/private_values.py /app/private_values.py
COPY v3-service/structured_log.py /app/structured_log.py
# Structural call-graph reasoning (issue #39): tree-sitter-backed call graph.
# Pure stdlib + the tree-sitter grammars installed above; no extra deps.
COPY v3-service/graph/ /app/graph/

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

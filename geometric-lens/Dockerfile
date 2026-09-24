# python:3.11-slim — digest-pinned for reproducible builds (Dependabot maintains)
FROM docker.io/python:3.11-slim@sha256:da047cb8f9d1d98e5c070f5300ba9f7274e33b8fc0e5be5ed88740aed1b95ba9
WORKDIR /app

# Install curl for healthcheck
RUN apt-get update && apt-get install -y --no-install-recommends curl && rm -rf /var/lib/apt/lists/*

# Install dependencies
COPY requirements.txt .
# Lens runs on CPU; all model inference stays in llama-server. Install torch
# from the CPU-only index first so pip cannot choose PyPI's CUDA/NCCL wheel,
# then resolve the ordinary application dependencies from PyPI. The version
# here MUST match requirements.txt's torch pin (contract-tested): a mismatch
# makes the second pip "upgrade" torch from PyPI, silently dragging the ~8 GB
# nvidia/cu* dependency stack into a CPU-only image.
RUN pip install --no-cache-dir torch==2.13.0 \
        --index-url https://download.pytorch.org/whl/cpu && \
    pip install --no-cache-dir -r requirements.txt

# Copy application code
COPY *.py .
COPY models/ models/
COPY cache/ cache/
COPY geometric_lens/ geometric_lens/

# Create directories for config and data. The serving path writes to
# /data/state (the SQLite state store, on the lens-state volume);
# config + secrets are read-only at runtime and the models dir is
# mounted read-only, so only /data/state and the app user's HOME need
# to be writable.
RUN mkdir -p /app/config /app/secrets /data/state

# Non-root application account created inside this image. /app files stay
# root-owned + world-readable (COPY default); /data/state is chowned so a
# freshly-created lens-state volume inherits writable ownership. HOME is
# a writable home for library caches (numpy/torch config, tiktoken);
# bytecode caching is disabled so Python never tries to write .pyc into
# /app.
#
# NOTE: an EXISTING lens-state volume was created root-owned by a prior
# image; after upgrading, chown it once —
#   docker compose run --rm --user root geometric-lens chown -R lens:lens /data/state
# — or recreate the volume. Fresh installs need nothing.
ENV PYTHONDONTWRITEBYTECODE=1
RUN groupadd --gid 1001 lens \
    && useradd --create-home --uid 1001 --gid 1001 lens \
    && chown -R lens:lens /data/state
USER lens
ENV HOME=/home/lens

EXPOSE 8099

CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8099"]

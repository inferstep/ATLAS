# python:3.11-slim — digest-pinned for reproducible builds (Dependabot maintains)
FROM docker.io/python:3.11-slim@sha256:db3ff2e1800a8581e2c48a27c3995339d47bdf046da21c7627accd3d51053a93
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
RUN pip install --no-cache-dir torch==2.14.0 \
        --index-url https://download.pytorch.org/whl/cpu && \
    pip install --no-cache-dir -r requirements.txt

# Copy application code
COPY *.py .
COPY geometric_lens/ geometric_lens/

# Create directories for config. Config + secrets are read-only at runtime
# and the models dir is mounted read-only, so only the app user's HOME
# needs to be writable.
RUN mkdir -p /app/config /app/secrets

# Non-root application account created inside this image. /app files stay
# root-owned + world-readable (COPY default). HOME is a writable home for
# library caches (numpy/torch config, tiktoken); bytecode caching is
# disabled so Python never tries to write .pyc into /app.
ENV PYTHONDONTWRITEBYTECODE=1
RUN groupadd --gid 1001 lens \
    && useradd --create-home --uid 1001 --gid 1001 lens
USER lens
ENV HOME=/home/lens

EXPOSE 8099

CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8099"]

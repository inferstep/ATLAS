# python:3.11-slim — digest-pinned for reproducible builds (Dependabot maintains)
FROM docker.io/python:3.11-slim@sha256:e88e9763f943ec1834f992a4b51e0f24500486803e8bc534e5767af9ea65f6ce
WORKDIR /app

# Install curl for healthcheck
RUN apt-get update && apt-get install -y --no-install-recommends curl && rm -rf /var/lib/apt/lists/*

COPY requirements.txt requirements-lock.txt ./
# Lens runs on CPU; all model inference stays in llama-server. Install all
# dependencies from the hashed lock, which emits PyTorch's CPU-only index
# so pip cannot choose PyPI's CUDA/NCCL wheel.
RUN pip install --no-cache-dir --require-hashes --only-binary=:all: -r requirements-lock.txt

# Copy application code
COPY *.py .
COPY geometric_lens/ geometric_lens/

# Create directories for config. Config + secrets are read-only at runtime
# and the models dir is mounted read-only, so only the app user's HOME
# needs to be writable.
RUN mkdir -p /app/config /app/secrets

# Non-root application account created inside this image. /app files stay
# root-owned + world-readable (COPY default). HOME is a writable home for
# library caches (numpy/torch config); bytecode caching is
# disabled so Python never tries to write .pyc into /app.
ENV PYTHONDONTWRITEBYTECODE=1
RUN groupadd --gid 1001 lens \
    && useradd --create-home --uid 1001 --gid 1001 lens
USER lens
ENV HOME=/home/lens

EXPOSE 8099

CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8099"]

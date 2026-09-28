"""Embedding extraction for the benchmark runner.

This module also carried a V2-era learning-curve tracker, a Spearman rank
correlation and an epoch shuffler. Nothing in the repository referenced
them -- no caller, no test, no document -- and the V3 lens pipeline does
its own calibration, so they were removed rather than left as surface that
reads like capability.
"""

import json
import urllib.request
import urllib.error
from typing import List, Optional

# Embedding dimensionality is read from the selected model's response.


# --- Embedding extraction -----------------------------------------------------

def extract_embedding_urllib(text: str, llama_url: str) -> Optional[List[float]]:
    """
    Extract embedding from LLM server.

    Supports llama.cpp /embedding.

    Args:
        text: Input text to embed.
        llama_url: Base URL for server (e.g. "http://localhost:8080").

    Returns:
        List of floats, or None on failure.
    """
    # Ask for raw vectors and pool below. `--pooling` is server-global in
    # llama.cpp and the lens per-step path needs `none`, so this has to
    # produce the same vector under either mode. Scale carries signal —
    # the cost field is fitted on unnormalized pooled vectors — so the
    # request pins normalization off rather than taking the server default.
    body = json.dumps({"content": text, "embd_normalize": -1}).encode("utf-8")
    endpoint = f"{llama_url}/embedding"

    req = urllib.request.Request(
        endpoint,
        data=body,
        headers={"Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            data = json.loads(resp.read().decode("utf-8"))
    except (urllib.error.URLError, urllib.error.HTTPError, OSError, ValueError):
        return None

    # llama.cpp response: [{"index": 0, "embedding": [[d0, ...], ...]}]
    try:
        token_vectors = data[0]["embedding"]
    except (KeyError, IndexError, TypeError):
        return None

    if not token_vectors:
        return None

    if not isinstance(token_vectors[0], list):
        return token_vectors

    n_tokens = len(token_vectors)
    n_dims = len(token_vectors[0])
    pooled = [0.0] * n_dims
    for vec in token_vectors:
        for i, v in enumerate(vec):
            pooled[i] += v
    for i in range(n_dims):
        pooled[i] /= n_tokens

    return pooled

"""The training samples `atlas lens build` reads: a labeled JSON or JSONL
file (--samples), the per-task results of a bench run (--from-results), and
the candidate embeddings that run banked in its telemetry.

atlas.commands.lens imports these loaders back, so `lens._load_training_samples`
still works.
"""

import json as jsonlib
import os
import sys
from typing import Dict, List, Optional

from atlas import env as cli_env


def _load_telemetry_embeddings(emb_path: str,
                                expect_dim: int) -> "tuple[List[Dict], int]":
    """Load labeled embeddings banked by v3_runner during a bench run.

    Every sandbox-tested candidate (probe + PlanSearch fan-out + repair
    iterations) gets its embedding + PASS/FAIL label appended to
    telemetry/embeddings.emb as the bench runs — in V3 mode that is several
    labeled samples per task, not one. Returns ({"embedding", "label"}
    dicts, n_skipped) with UNKNOWN labels and dim mismatches dropped.
    """
    # stages/ (the V3 pipeline stages) lives in v3-service — same pattern
    # as the geometric-lens sys.path setup above.
    v3_dir = os.path.join(cli_env.atlas_root(), "v3-service")
    if v3_dir not in sys.path:
        sys.path.insert(0, v3_dir)
    from stages.embedding_store import EmbeddingReader

    out: List[Dict] = []
    skipped = 0
    for rec in EmbeddingReader(emb_path).read_all():
        label = {"PASS": 1, "FAIL": 0}.get(rec.get("label"))
        emb = rec.get("embedding")
        if label is None or not emb or (expect_dim
                                        and len(emb) != expect_dim):
            skipped += 1
            continue
        out.append({"embedding": emb, "label": label})
    return out, skipped


def _load_training_samples(path: Optional[str]) -> List[Dict]:
    """Load training samples from a JSON or JSONL file.

    Format: list of {"text": str, "label": 0|1} or {"content": str, "label": 0|1}.
    JSONL is detected by .jsonl extension or by leading whitespace check.
    """
    if not path or not os.path.isfile(path):
        return []
    with open(path) as fh:
        content = fh.read()
    if path.endswith(".jsonl") or content.lstrip().startswith("{\""):
        # JSONL
        samples = []
        for line in content.splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                samples.append(jsonlib.loads(line))
            except jsonlib.JSONDecodeError:
                continue
        return samples
    # JSON array
    try:
        parsed = jsonlib.loads(content)
        if isinstance(parsed, list):
            return parsed
    except jsonlib.JSONDecodeError:
        return []
    return []


def _load_results_samples(results_dir: str) -> List[Dict]:
    """Load training samples from a benchmark results directory — the per-task
    JSONs written by `atlas bench` (each has `code` + `passed`). Maps to the
    same {text, label} shape `_extract_training_embeddings` consumes, so the
    model trains C(x) on its own pass/fail candidates."""
    import glob
    samples: List[Dict] = []
    for fp in sorted(glob.glob(os.path.join(results_dir, "*.json"))):
        try:
            with open(fp) as fh:
                d = jsonlib.load(fh)
        except (OSError, jsonlib.JSONDecodeError):
            continue
        if not isinstance(d, dict):
            continue  # stray non-result JSON (array/scalar) in the dir
        code = d.get("code")
        if not code:
            continue
        samples.append({"text": code, "label": 1 if d.get("passed") else 0})
    return samples

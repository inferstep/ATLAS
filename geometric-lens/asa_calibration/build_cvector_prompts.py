#!/usr/bin/env python3
"""Build positive.txt / negative.txt for llama-cvector-generator from a
JSONL contrast-pair file.

May 2026 BiasBusters #4 — ASA-style activation steering. The contrast
pairs encode the structural_edit-vs-edit_file decision as positive/negative
examples; cvector-generator extracts the residual-stream difference
between them; llama-server applies the difference at inference time
via --control-vector-scaled.

Usage:
    python build_cvector_prompts.py \\
        --pairs contrast_pairs.jsonl \\
        --positive structural_edit_positive.txt \\
        --negative structural_edit_negative.txt

Then run upstream cvector-generator (built from llama.cpp tools/) with the
same selected model used to render these prompts:
    llama-cvector-generator \\
        -m /models/your-model.gguf \\
        --positive-file structural_edit_positive.txt \\
        --negative-file structural_edit_negative.txt \\
        --method mean \\
        -o ast_edit_steering.gguf \\
        -ngl 99

And add to inference/entrypoint-v3.1.sh (or set the env var the
entrypoint reads):
    --control-vector-scaled /path/to/ast_edit_steering.gguf:0.5
"""

import argparse
import json
import os
import sys
from pathlib import Path
from urllib.request import Request, urlopen


_SYSTEM_PROMPT = (
    "You are ATLAS, a coding assistant. Choose the right tool for the job."
)


def _service_token() -> str:
    """The install's service token, found as atlas/token.py finds it; "" when it has none."""
    explicit = os.environ.get("ATLAS_SERVICE_TOKEN_FILE", "").strip()
    path = Path(explicit) if explicit else Path(__file__).resolve().parents[2] / "secrets" / "service-token"
    try:
        return path.read_text(encoding="utf-8").strip()
    except FileNotFoundError:
        return ""


def render(pair: dict, llama_url: str) -> str:
    """Use the loaded model's template, then escape newlines for the tool."""
    payload = json.dumps({
        "messages": [
            {"role": "system", "content": _SYSTEM_PROMPT},
            {"role": "user", "content": pair["user"]},
        ],
    }).encode()
    req = Request(
        f"{llama_url.rstrip('/')}/apply-template",
        data=payload,
        headers={"Content-Type": "application/json"},
    )
    token = _service_token()
    if token:
        # Unredirected: urllib copies a plain header to a redirect's address.
        req.add_unredirected_header("Authorization", f"Bearer {token}")
    try:
        with urlopen(req, timeout=30) as resp:
            result = json.loads(resp.read())
    except Exception as exc:
        raise RuntimeError(
            "llama-server /apply-template is required for model-agnostic "
            f"ASA calibration: {exc}"
        ) from exc
    prompt = result.get("prompt") if isinstance(result, dict) else None
    if not isinstance(prompt, str) or not prompt:
        raise RuntimeError("unexpected llama-server /apply-template response")
    return (prompt + pair["assistant_prefix"]).replace("\n", "\\n")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--pairs", required=True, type=Path,
                    help="JSONL file with one pair per line "
                         "({label, user, assistant_prefix, tool})")
    ap.add_argument("--positive", required=True, type=Path,
                    help="output file for label==structural_edit prompts")
    ap.add_argument("--negative", required=True, type=Path,
                    help="output file for label==edit_file prompts")
    ap.add_argument(
        "--llama-url",
        default=os.environ.get("LLAMA_URL", "http://localhost:8080"),
        help="loaded llama-server used to apply the selected model's template",
    )
    args = ap.parse_args()

    pos: list[str] = []
    neg: list[str] = []
    with args.pairs.open() as f:
        for lineno, raw in enumerate(f, start=1):
            raw = raw.strip()
            if not raw:
                continue
            try:
                pair = json.loads(raw)
            except json.JSONDecodeError as exc:
                print(f"line {lineno}: bad JSON: {exc}", file=sys.stderr)
                return 1
            for key in ("label", "user", "assistant_prefix"):
                if key not in pair:
                    print(f"line {lineno}: missing field {key!r}", file=sys.stderr)
                    return 1
            try:
                rendered = render(pair, args.llama_url)
            except RuntimeError as exc:
                print(f"line {lineno}: {exc}", file=sys.stderr)
                return 2
            if pair["label"] == "structural_edit":
                pos.append(rendered)
            elif pair["label"] == "edit_file":
                neg.append(rendered)
            else:
                print(f"line {lineno}: unknown label {pair['label']!r} "
                      f"(expected 'structural_edit' or 'edit_file')", file=sys.stderr)
                return 1

    if not pos or not neg:
        print(f"need both structural_edit and edit_file pairs; "
              f"got {len(pos)} positive, {len(neg)} negative", file=sys.stderr)
        return 1
    if len(pos) != len(neg):
        # cvector-generator pairs them positionally — line N of positive
        # is contrasted against line N of negative. Mismatched counts
        # silently truncate the longer side, which biases the vector.
        print(f"warning: {len(pos)} positive vs {len(neg)} negative — "
              f"cvector-generator will use min(N) pairs, biasing the result",
              file=sys.stderr)

    args.positive.write_text("\n".join(pos) + "\n")
    args.negative.write_text("\n".join(neg) + "\n")
    print(f"wrote {len(pos)} positive prompts to {args.positive}")
    print(f"wrote {len(neg)} negative prompts to {args.negative}")
    print()
    print("Next: run cvector-generator (see header docstring for the command).")
    return 0


if __name__ == "__main__":
    sys.exit(main())

"""V3 Embedding Store — binary storage for candidate embeddings.

Stores float32 embeddings in compact binary format (.emb files)
for post-hoc analysis, Lens retraining, and embedding space visualization.

Format: sequence of records, each:
  - 4 bytes: task_id length (uint32 LE)
  - N bytes: task_id (utf-8)
  - 4 bytes: candidate_index (uint32 LE)
  - 1 byte: label (0=FAIL, 1=PASS, 2=UNKNOWN)
  - 4 bytes: embedding dim (uint32 LE)
  - dim*4 bytes: embedding (float32 LE)

No external dependencies (stdlib only).
"""

import struct
import threading
from pathlib import Path
from typing import List

LABEL_FAIL = 0
LABEL_PASS = 1
LABEL_UNKNOWN = 2


def label_to_byte(label: str) -> int:
    if label == "PASS":
        return LABEL_PASS
    elif label == "FAIL":
        return LABEL_FAIL
    return LABEL_UNKNOWN


def byte_to_label(b: int) -> str:
    if b == LABEL_PASS:
        return "PASS"
    elif b == LABEL_FAIL:
        return "FAIL"
    return "UNKNOWN"


class EmbeddingWriter:
    """Append-only binary writer for embeddings (thread-safe)."""

    def __init__(self, path: Path):
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._file = open(self.path, "ab")
        self._lock = threading.Lock()

    def write(self, task_id: str, candidate_index: int,
              label: str, embedding: List[float]) -> None:
        """Append one embedding record."""
        tid_bytes = task_id.encode("utf-8")
        dim = len(embedding)
        header = struct.pack("<I", len(tid_bytes))
        header += tid_bytes
        header += struct.pack("<IBi", candidate_index, label_to_byte(label), dim)
        data = struct.pack(f"<{dim}f", *embedding)
        with self._lock:
            self._file.write(header + data)

    def close(self):
        self._file.close()

    def __enter__(self):
        return self

    def __exit__(self, *args):
        self.close()


class EmbeddingReader:
    """Reader for binary embedding files."""

    def __init__(self, path: Path):
        self.path = Path(path)

    def read_all(self) -> List[dict]:
        """Read all embedding records from file."""
        records = []
        with open(self.path, "rb") as f:
            while True:
                # Read task_id length
                buf = f.read(4)
                if not buf or len(buf) < 4:
                    break
                tid_len = struct.unpack("<I", buf)[0]
                tid_bytes = f.read(tid_len)
                task_id = tid_bytes.decode("utf-8")

                # Read candidate index, label, dim
                meta = f.read(9)  # 4 + 1 + 4
                candidate_index, label_byte, dim = struct.unpack("<IBi", meta)

                # Read embedding
                emb_bytes = f.read(dim * 4)
                embedding = list(struct.unpack(f"<{dim}f", emb_bytes))

                records.append({
                    "task_id": task_id,
                    "candidate_index": candidate_index,
                    "label": byte_to_label(label_byte),
                    "embedding": embedding,
                })
        return records

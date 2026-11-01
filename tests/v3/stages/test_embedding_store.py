"""Tests for V3 binary embedding store."""

from stages.embedding_store import EmbeddingReader, EmbeddingWriter


def test_records_read_back_as_written(tmp_path):
    path = tmp_path / "candidates.emb"
    small = [float(i) for i in range(64)]
    real_size = [i * 0.001 for i in range(4096)]  # the real dim (Qwen3.5-9B)
    with EmbeddingWriter(path) as writer:
        writer.write("task_001", 0, "PASS", small)
        writer.write("task_001", 1, "FAIL", small)
        writer.write("task_002", 0, "UNKNOWN", small)
        writer.write("LCB_3033", 0, "PASS", real_size)
        writer.write("LCB_café", 7, "PASS", [1.0, 2.0, 3.0])  # é is two bytes in UTF-8

    records = EmbeddingReader(path).read_all()

    assert [(r["task_id"], r["candidate_index"], r["label"]) for r in records] == [
        ("task_001", 0, "PASS"),
        ("task_001", 1, "FAIL"),
        ("task_002", 0, "UNKNOWN"),
        ("LCB_3033", 0, "PASS"),
        ("LCB_café", 7, "PASS"),
    ]
    assert records[0]["embedding"] == small  # whole numbers are exact in float32
    assert len(records[3]["embedding"]) == 4096
    assert abs(records[3]["embedding"][4095] - 4.095) < 0.001
    assert records[4]["embedding"] == [1.0, 2.0, 3.0]


def test_an_empty_file_has_no_records(tmp_path):
    path = tmp_path / "empty.emb"
    path.write_bytes(b"")

    assert EmbeddingReader(path).read_all() == []

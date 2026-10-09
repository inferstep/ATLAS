"""Deterministic local concurrency + recovery tests.

No Docker/network: exercises the atomic-write, stateless-filter, and
crash-recovery properties that must hold under concurrent access. The Go
services carry their own `-race` coverage; these cover the Python side.
"""

import threading

from atlas import upgrade_engine as eng
from atlas import redact


def _root(tmp_path, tag="v1.0.0"):
    (tmp_path / "docker-compose.yml").write_text("services: {}\n")
    (tmp_path / ".env").write_text(f"ATLAS_IMAGE_TAG={tag}\nK=v\n")
    return str(tmp_path)


def test_concurrent_restore_point_writes_never_corrupt(tmp_path):
    """os.replace makes the restore point atomic — concurrent writers
    must leave a fully-valid file, never a truncated one."""
    root = _root(tmp_path)
    errors = []

    def writer(i):
        try:
            for _ in range(20):
                eng.write_restore_point(
                    root, f"v{i}", "vX", {"svc": f"sha{i}"}, f"stamp{i}")
        except Exception as e:
            errors.append(e)

    threads = [threading.Thread(target=writer, args=(i,)) for i in range(8)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()

    assert not errors, errors
    # The final file is always complete + parseable (never a torn write).
    point = eng.read_restore_point(root)
    assert point is not None
    assert point["target_tag"] == "vX"
    assert "previous_digests" in point


def test_concurrent_reads_during_writes(tmp_path):
    """Readers concurrent with writers always see a valid file or None,
    never a partial parse error."""
    root = _root(tmp_path)
    eng.write_restore_point(root, "v1", "v2", {}, "s0")
    bad = []
    stop = threading.Event()

    def reader():
        while not stop.is_set():
            p = eng.read_restore_point(root)
            if p is not None and "target_tag" not in p:
                bad.append(p)

    def writer():
        for i in range(50):
            eng.write_restore_point(root, f"v{i}", "v2", {}, f"s{i}")

    r = threading.Thread(target=reader)
    r.start()
    w = threading.Thread(target=writer)
    w.start()
    w.join()
    stop.set()
    r.join()
    assert not bad, "reader observed a torn restore point"


def test_filter_is_stateless_under_threads():
    """The private-value filter must be pure — concurrent calls with
    different inputs never bleed into each other."""
    inputs = [f"TOKEN=fake-value-{i}" for i in range(200)]
    results = {}
    lock = threading.Lock()

    def work(i):
        out = redact.filter_private_values(inputs[i])
        with lock:
            results[i] = out

    threads = [threading.Thread(target=work, args=(i,))
               for i in range(len(inputs))]
    for t in threads:
        t.start()
    for t in threads:
        t.join()

    # Every result is masked and none leaked another input's value.
    for i, out in results.items():
        assert "[FILTERED]" in out
        assert f"fake-value-{i}" not in out


def test_env_tag_write_is_atomic(tmp_path):
    """Concurrent _set_env_tag-style writes leave a valid .env."""
    from atlas.commands.upgrade import _set_env_tag
    root = _root(tmp_path)
    errs = []

    def writer(tag):
        try:
            for _ in range(30):
                _set_env_tag(root, tag)
        except Exception as e:
            errs.append(e)

    threads = [threading.Thread(target=writer, args=(f"v{i}",))
               for i in range(6)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert not errs, errs
    # .env is intact and still has the other key
    env = (tmp_path / ".env").read_text()
    assert "K=v" in env
    assert "ATLAS_IMAGE_TAG=" in env

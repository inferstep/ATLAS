"""The LiveCodeBench loader: its download cache and how it reads test cases.

No network: the rows API is answered from fixed pages.
"""

import json
import urllib.parse

import pytest

from atlas.bench.datasets import livecodebench as lcb
from atlas.bench.datasets.livecodebench import LiveCodeBenchDataset

PRIMARY, MIRROR = LiveCodeBenchDataset.DATASET_IDS
CACHE = LiveCodeBenchDataset.FILENAME
MARKER = CACHE + ".partial"


class _Reply:
    def __init__(self, rows):
        self.body = json.dumps({"rows": rows}).encode()

    def __enter__(self):
        return self

    def __exit__(self, *_args):
        return False

    def read(self):
        return self.body


def _serve(monkeypatch, pages):
    """Answer the rows API from pages: {source: [rows or an exception, one per page of 100]}.
    Returns the (source, offset) of each request."""
    asked = []

    def urlopen(req, timeout):
        query = urllib.parse.parse_qs(urllib.parse.urlparse(req.full_url).query)
        source, offset = query["dataset"][0], int(query["offset"][0])
        asked.append((source, offset))
        page = pages[source][offset // 100]
        if isinstance(page, Exception):
            raise page
        return _Reply(page)

    monkeypatch.setattr(lcb.urllib.request, "urlopen", urlopen)
    return asked


def _rows(start, n):
    return [{"row": {"question_id": f"q{i}", "url": "dropped from the cache"}} for i in range(start, start + n)]


def _cached(tmp_path):
    return [json.loads(line) for line in (tmp_path / CACHE).read_text().splitlines()]


def test_a_full_download_pages_until_a_short_page_and_caches_slim_rows(tmp_path, monkeypatch):
    # The last row comes without the API's wrapper; it is cached as it is.
    asked = _serve(monkeypatch, {PRIMARY: [_rows(0, 100), _rows(100, 29) + [{"question_id": "q129"}]]})

    assert LiveCodeBenchDataset(cache_dir=tmp_path).download() == tmp_path / CACHE

    assert asked == [(PRIMARY, 0), (PRIMARY, 100)]
    assert _cached(tmp_path) == [{"question_id": f"q{i}"} for i in range(130)]
    assert not (tmp_path / MARKER).exists()


def test_a_source_that_fails_outright_gives_way_to_the_mirror(tmp_path, monkeypatch):
    asked = _serve(monkeypatch, {PRIMARY: [OSError("down")], MIRROR: [_rows(0, 30)]})

    LiveCodeBenchDataset(cache_dir=tmp_path).download()

    assert asked == [(PRIMARY, 0), (MIRROR, 0)]
    assert len(_cached(tmp_path)) == 30


def test_a_download_cut_midway_is_cached_and_marked_partial(tmp_path, monkeypatch):
    asked = _serve(monkeypatch, {PRIMARY: [_rows(0, 100), OSError("reset")]})

    LiveCodeBenchDataset(cache_dir=tmp_path).download()

    assert asked == [(PRIMARY, 0), (PRIMARY, 100)]  # the mirror is not asked
    assert len(_cached(tmp_path)) == 100
    assert (tmp_path / MARKER).exists()


def test_a_partial_cache_is_replaced_by_a_full_download_and_unmarked(tmp_path, monkeypatch):
    (tmp_path / CACHE).write_text("{}\n" * 150)
    (tmp_path / MARKER).touch()
    _serve(monkeypatch, {PRIMARY: [_rows(0, 30)]})

    LiveCodeBenchDataset(cache_dir=tmp_path).download()

    assert len(_cached(tmp_path)) == 30
    assert not (tmp_path / MARKER).exists()


def test_a_smaller_partial_download_never_replaces_the_cache(tmp_path, monkeypatch, capsys):
    (tmp_path / CACHE).write_text("{}\n" * 150)
    (tmp_path / MARKER).touch()
    _serve(monkeypatch, {PRIMARY: [_rows(0, 100), OSError("reset")], MIRROR: [OSError("down")]})

    LiveCodeBenchDataset(cache_dir=tmp_path).download()

    assert _cached(tmp_path) == [{}] * 150
    assert (tmp_path / MARKER).exists()
    assert "using the existing partial cache (150 tasks)" in capsys.readouterr().out


def _cases(n):
    return [{"input": str(i), "output": str(i + 1)} for i in range(n)]


@pytest.mark.parametrize("row, slim", [
    pytest.param({"question_id": "q", "url": "u"}, {"question_id": "q"}, id="fields-the-parser-never-reads-go"),
    pytest.param({"public_test_cases": json.dumps(_cases(51))}, {"public_test_cases": json.dumps(_cases(50))},
                 id="text-stays-text-cut-to-50"),
    pytest.param({"private_test_cases": _cases(51)}, {"private_test_cases": _cases(50)}, id="a-list-stays-a-list-cut-to-50"),
    pytest.param({"question_id": "q", "public_test_cases": "not json"}, {"question_id": "q"}, id="text-that-is-not-json-goes"),
    pytest.param({"public_test_cases": json.dumps({"input": "1"})}, {}, id="json-that-is-not-a-list-goes"),
])
def test_a_cached_row_keeps_what_the_parser_reads(row, slim):
    assert lcb._slim_row(row) == slim


@pytest.mark.parametrize("row, inputs, outputs", [
    pytest.param({"private_test_cases": _cases(1), "public_test_cases": [{"input": "p", "output": "q"}]},
                 ["0"], ["1"], id="private-first"),
    pytest.param({"private_test_cases": "", "public_test_cases": json.dumps([{"input": "p", "output": "q"}])},
                 ["p"], ["q"], id="public-when-private-is-empty"),
    pytest.param({"private_test_cases": "not json", "public_test_cases": [{"input": "p", "output": "q"}]},
                 ["p"], ["q"], id="public-when-private-is-not-json"),
    pytest.param({"private_test_cases": json.dumps({"input": "1"}), "public_test_cases": [{"input": "p", "output": "q"}]},
                 ["p"], ["q"], id="public-when-private-is-not-a-list"),
    pytest.param({"private_test_cases": [{"input": None, "output": "x"}], "public_test_cases": [{"input": "p", "output": "q"}]},
                 ["p"], ["q"], id="public-when-no-private-case-is-whole"),
    pytest.param({"public_test_cases": [{"input": "1", "expected_output": "2"}, {"input": "3", "output": None}, {"input": 5, "output": 6}]},
                 ["1", "5"], ["2", "6"], id="expected-output-taken-a-case-without-output-skipped-numbers-as-text"),
    pytest.param({"public_test_cases": _cases(51)}, [str(i) for i in range(50)], [str(i + 1) for i in range(50)],
                 id="cut-to-50"),
])
def test_test_cases_come_from_the_first_field_that_has_any(tmp_path, row, inputs, outputs):
    assert LiveCodeBenchDataset(cache_dir=tmp_path)._extract_test_cases(row) == (inputs, outputs)

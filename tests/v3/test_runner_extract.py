from stages.llm_client import extract_code, extract_code_for_problem


def test_extract_code_accepts_non_python_language_fence():
    response = """Here is the file:
```javascript
const result = `${2 + 3}`;
```
"""

    assert extract_code(response) == "const result = `${2 + 3}`;\n"


def test_extract_code_accepts_punctuation_in_language_label():
    response = """```c++
int main() { return 0; }
```"""

    assert extract_code(response) == "int main() { return 0; }\n"


def test_extract_code_chooses_longest_fenced_block_across_languages():
    response = """```text
short
```
```typescript
export function add(a: number, b: number): number { return a + b; }
```"""

    assert extract_code(response).startswith("export function add")


# --- exact bytes -------------------------------------------------------------
#
# The extractor names the artifact. Every candidate hash, selection record,
# authorization identity and disk write downstream is computed from the bytes
# it returns, so the bytes inside the fence must come back exactly as the model
# wrote them: a final newline when there is one, none when there is not, and
# every trailing blank line in between. The Markdown fence is framing and is
# not part of the artifact; the artifact's own line terminator is.

import pytest

from stages.plan_search import extract_code_from_response
from stages.pr_cot import extract_code_from_repair

EXTRACTORS = [
    pytest.param(extract_code, id="llm_client.extract_code"),
    pytest.param(extract_code_from_response, id="plan_search.extract_code_from_response"),
    pytest.param(extract_code_from_repair, id="pr_cot.extract_code_from_repair"),
]


@pytest.mark.parametrize("extract", EXTRACTORS)
def test_fenced_code_keeps_its_final_newline(extract):
    assert extract("```python\ndef f():\n    return 1\n```") == "def f():\n    return 1\n"


@pytest.mark.parametrize("extract", EXTRACTORS)
def test_fenced_code_without_a_final_newline_gets_none(extract):
    assert extract("```python\ndef f():\n    return 1```") == "def f():\n    return 1"


@pytest.mark.parametrize("extract", EXTRACTORS)
def test_fenced_code_keeps_every_trailing_blank_line(extract):
    assert extract("```python\ndef f():\n    return 1\n\n\n```") == "def f():\n    return 1\n\n\n"


@pytest.mark.parametrize("extract", EXTRACTORS)
def test_fenced_code_keeps_its_line_endings(extract):
    assert extract("```python\r\nx = 1\r\ny = 2\r\n```") == "x = 1\r\ny = 2\r\n"


@pytest.mark.parametrize("extract", EXTRACTORS)
def test_inline_code_keeps_its_final_newline(extract):
    # No fence: the response is the artifact, and its terminator stays.
    assert extract("def f():\n    return 1\n") == "def f():\n    return 1\n"


@pytest.mark.parametrize("extract", EXTRACTORS)
def test_inline_and_fenced_forms_share_bytes_only_when_the_bytes_agree(extract):
    fenced = extract("```\ndef f():\n    return 1\n```")
    inline = extract("def f():\n    return 1\n")
    bare = extract("def f():\n    return 1")
    assert fenced == inline
    assert bare != fenced
    assert bare + "\n" == fenced


def test_extract_code_prose_before_a_fence_is_not_part_of_the_artifact():
    response = "Here is the file:\n```javascript\nconst x = 1;\n```\nThat should do it.\n"
    assert extract_code(response) == "const x = 1;\n"


def test_problem_extractor_prefers_requested_artifact_over_later_tests():
    problem = "Create solution.py. Implement exactly:\ndef transpose_rows(rows):"
    response = (
        "```python\ndef transpose_rows(rows):\n    return rows\n```\n"
        "Supplemental checks:\n"
        "```python\nfrom solution import transpose_rows\n\n"
        "def test_rows():\n    assert transpose_rows([]) == []\n```"
    )
    assert extract_code_for_problem(response, problem, fallback="last") == (
        "def transpose_rows(rows):\n    return rows\n"
    )


def test_problem_extractor_keeps_historical_fallback_without_exact_target():
    response = "```python\nx = 1\n```\n```python\nx = 2\ny = 3\n```"
    assert extract_code_for_problem(response, "Create a Python module") == (
        "x = 2\ny = 3\n"
    )


def test_problem_extractor_repairs_only_unmatched_closer_when_file_parses():
    problem = "Create solution.py. Implement exactly:\ndef merge_intervals(items):"
    response = "```python\ndef merge_intervals(items):\n    return list(items))\n```"
    assert extract_code_for_problem(response, problem) == (
        "def merge_intervals(items):\n    return list(items)\n"
    )


def test_problem_extractor_repairs_unexpected_top_level_indent_when_file_parses():
    problem = "Create solution.py. Implement class TokenBucket"
    response = "```python\nimport logging\n logger = logging.getLogger(__name__)\n\nclass TokenBucket:\n    pass\n```"
    assert extract_code_for_problem(response, problem) == (
        "import logging\nlogger = logging.getLogger(__name__)\n\n"
        "class TokenBucket:\n    pass\n"
    )


def test_problem_extractor_leaves_ambiguous_syntax_failure_unchanged():
    problem = "Create solution.py. Implement exactly:\ndef solve(value):"
    code = "def solve(value)\n    return value\n"
    assert extract_code_for_problem(f"```python\n{code}```", problem) == code


def test_problem_extractor_restores_exact_requested_signature_only():
    problem = (
        "Create solution.py. Implement exactly:\n"
        "def round_robin(iterables, *, stop_shortest: bool = False):"
    )
    response = (
        "```python\n"
        "def round_robin(iterables: list, *, stop_shortest=False) -> object:\n"
        "    return iter(iterables)\n"
        "```"
    )
    assert extract_code_for_problem(response, problem) == (
        "def round_robin(iterables, *, stop_shortest: bool = False):\n"
        "    return iter(iterables)\n"
    )


def test_problem_extractor_restores_builtin_annotations_from_exact_contract():
    problem = (
        "Create solution.py. Implement exactly:\n"
        "def merge_intervals(intervals: list[tuple[int, int]], *, "
        "merge_touching: bool = True) -> list[tuple[int, int]]:"
    )
    response = (
        "```python\n"
        "from typing import List, Tuple\n\n"
        "def merge_intervals(intervals: List[Tuple[int, int]], *, "
        "merge_touching: bool = True) -> List[Tuple[int, int]]:\n"
        "    return intervals\n"
        "```"
    )
    assert extract_code_for_problem(response, problem) == (
        "from typing import List, Tuple\n\n"
        "def merge_intervals(intervals: list[tuple[int, int]], *, "
        "merge_touching: bool = True) -> list[tuple[int, int]]:\n"
        "    return intervals\n"
    )


def test_problem_extractor_uses_request_contract_not_later_reference_signature():
    problem = (
        "## The request\n\n"
        "Create solution.py. Implement exactly:\n"
        "def merge_intervals(intervals: list[tuple[int, int]], *, "
        "merge_touching: bool = True) -> list[tuple[int, int]]:\n\n"
        "Create the file `solution.py`.\n\n"
        "## Reference implementation:\n"
        "Improve upon this baseline if possible.\n\n"
        "```\n"
        "from typing import List, Tuple\n\n"
        "def merge_intervals(intervals: List[Tuple[int, int]], *, "
        "merge_touching: bool = True) -> List[Tuple[int, int]]:\n"
        "    return intervals\n"
        "```\n"
    )
    response = (
        "```python\n"
        "from typing import List, Tuple\n\n"
        "def merge_intervals(intervals: List[Tuple[int, int]], *, "
        "merge_touching: bool = True) -> List[Tuple[int, int]]:\n"
        "    return intervals\n"
        "```"
    )
    assert extract_code_for_problem(response, problem) == (
        "from typing import List, Tuple\n\n"
        "def merge_intervals(intervals: list[tuple[int, int]], *, "
        "merge_touching: bool = True) -> list[tuple[int, int]]:\n"
        "    return intervals\n"
    )


def test_problem_extractor_ignores_same_named_project_context_before_request():
    problem = (
        "The following files already exist in the project:\n\n"
        "### helpers.py\n```\n"
        "def solve(value: str) -> str:\n    return value\n```\n\n"
        "---\n\nTask:\n"
        "Create solution.py. Implement exactly:\n"
        "def solve(value: int, *, strict: bool = False) -> int:\n"
    )
    response = "```python\ndef solve(value):\n    return value\n```"
    assert extract_code_for_problem(response, problem) == (
        "def solve(value: int, *, strict: bool = False) -> int:\n"
        "    return value\n"
    )


def test_problem_extractor_does_not_rewrite_ambiguous_duplicate_target():
    problem = "Implement exactly:\ndef solve(value, *, strict: bool = False):"
    code = (
        "def solve(value: int, *, strict=False):\n    return value\n\n"
        "def solve(value: int, *, strict=False):\n    return value\n"
    )
    assert extract_code_for_problem(f"```python\n{code}```", problem) == code

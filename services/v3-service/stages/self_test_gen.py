"""V3 Self-Test Generation — Model-Generated Test Cases for Internal Verification.

When Phase 3 refinement needs to verify candidate solutions, this module generates
test cases from the problem statement alone — no access to real benchmark tests.
The model reasons about the problem and produces input/output pairs covering
simple cases, edge cases, and boundary conditions.

Config: [self_test_gen] in atlas.conf
Telemetry: telemetry/self_test_gen_events.jsonl

This replaces the answer-key feedback loop with genuine internal verification:
the model checks its own work using tests it generated from its understanding
of the problem, not from the benchmark's ground truth.
"""

import json
import re
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Callable, Dict, List, Optional, Tuple


# Type alias for LLM callable
LLMCallable = Callable[[str, float, int, Optional[int]], Tuple[str, int, float]]


# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

@dataclass
class SelfTestGenConfig:
    """Configuration for Self-Test Generation."""
    enabled: bool = False
    num_test_cases: int = 5
    generation_temperature: float = 0.3
    generation_max_tokens: int = 2048
    majority_threshold: float = 0.6  # Pass if 60%+ self-tests pass


# ---------------------------------------------------------------------------
# Data structures
# ---------------------------------------------------------------------------

# Where a case's expected output came from. The model wrote both halves of
# every case this module produces, from the problem statement alone, so the
# expected output is a second guess at the same problem rather than an
# answer key. Measured on the captured pool: 21 of 36 valid generated keys
# disagreed with the task's own reference, 4 of them containing the model's
# reasoning transcript instead of an answer. A case carries where it came
# from so no consumer has to infer it.
PROVENANCE_GENERATED = "generated"
PROVENANCE_TRUSTED = "trusted"


@dataclass
class GeneratedTestCase:
    """A single model-generated test case.

    `provenance` defaults to generated and this module never sets anything
    else: nothing here can produce a trusted case. A consumer that cannot
    read a provenance must treat the case as untrusted.
    """
    input_str: str
    expected_output: str
    description: str = ""
    provenance: str = PROVENANCE_GENERATED

    def to_dict(self) -> Dict:
        return {
            "input_str": self.input_str,
            "expected_output": self.expected_output,
            "description": self.description,
            "provenance": self.provenance,
        }

    @classmethod
    def from_dict(cls, d: Dict) -> "GeneratedTestCase":
        return cls(
            input_str=d.get("input_str", ""),
            expected_output=d.get("expected_output", ""),
            description=d.get("description", ""),
        )


@dataclass
class SelfTestResult:
    """Result of self-test generation."""
    test_cases: List[GeneratedTestCase] = field(default_factory=list)
    generation_tokens: int = 0
    generation_time_ms: float = 0.0
    reason: str = ""
    task_id: str = ""

    def to_dict(self) -> Dict:
        return {
            "num_test_cases": len(self.test_cases),
            "generation_tokens": self.generation_tokens,
            "generation_time_ms": self.generation_time_ms,
            "reason": self.reason,
            "task_id": self.task_id,
        }


@dataclass
class SelfTestGenEvent:
    """Telemetry event for self-test generation."""
    task_id: str
    num_cases_generated: int = 0
    generation_tokens: int = 0
    generation_time_ms: float = 0.0
    reason: str = ""
    timestamp: str = ""

    def to_dict(self) -> Dict:
        return {
            "task_id": self.task_id,
            "num_cases_generated": self.num_cases_generated,
            "generation_tokens": self.generation_tokens,
            "generation_time_ms": self.generation_time_ms,
            "reason": self.reason,
            "timestamp": self.timestamp or datetime.now(timezone.utc).isoformat(),
        }


# ---------------------------------------------------------------------------
# Prompt
# ---------------------------------------------------------------------------

SELF_TEST_PROMPT = """\
Given this programming problem, generate {num_cases} test cases to verify a solution's correctness.

Problem:
{problem}

For each test case, reason about what input would test an important aspect of the problem:
- At least one simple/trivial case
- At least one edge case (empty input, zero, single element, negative numbers)
- At least one boundary condition (large input, maximum values)

IMPORTANT: Manually trace through the expected computation for each test case to ensure your expected output is correct. Do not guess.

Output format — use EXACTLY this structure for each test case:
TEST CASE 1:
DESCRIPTION: <what this tests>
INPUT: <the stdin input, exactly as it would be typed>
OUTPUT: <the expected stdout output, exactly as it should appear>

TEST CASE 2:
DESCRIPTION: <what this tests>
INPUT: <the stdin input>
OUTPUT: <the expected stdout output>

Only output the test cases, nothing else."""


# ---------------------------------------------------------------------------
# Response parsing helpers
# ---------------------------------------------------------------------------

def _strip_think_tags(response: str) -> str:
    """Strip <think>...</think> blocks, including unclosed tags.

    If the model hits the token limit mid-think, the closing </think> is
    never emitted. Handle both closed and unclosed cases.
    """
    # First strip properly closed tags
    response = re.sub(r'<think>.*?</think>', '', response,
                      flags=re.DOTALL)
    # Then strip unclosed <think> (everything from <think> to end of string)
    response = re.sub(r'<think>.*$', '', response, flags=re.DOTALL)
    return response.strip()


def _strip_markdown_fences(text: str) -> str:
    """Remove markdown code fences wrapping a value.

    Handles ```python ... ```, ``` ... ```, and inline `...` wrapping.
    """
    text = text.strip()
    # Multi-line fenced code blocks: ```lang\n...\n```
    fenced = re.match(r'^```\w*\s*\n?(.*?)\n?\s*```$', text, re.DOTALL)
    if fenced:
        return fenced.group(1).strip()
    # Inline backtick wrapping: `value`
    inline = re.match(r'^`(.*)`$', text, re.DOTALL)
    if inline:
        return inline.group(1).strip()
    return text


# Patterns for splitting response into test case blocks.
# Matches: "TEST CASE 1:", "Test Case 1:", "Test 1:", "test case 1:",
#          "Test case #1:", etc.
_BLOCK_HEADER = re.compile(
    r'(?:TEST\s+(?:CASE\s+)?|Test\s+(?:[Cc]ase\s+)?|test\s+(?:case\s+)?)#?\d+\s*[:\-]',
    re.IGNORECASE,
)

# Patterns for field extraction within a block.
# INPUT variants: "INPUT:", "Input:", "Stdin:", "STDIN:", "input:"
_INPUT_LABEL = re.compile(
    r'^(?:input|stdin)\s*[:\-]\s*',
    re.MULTILINE | re.IGNORECASE,
)

# OUTPUT variants: "OUTPUT:", "EXPECTED OUTPUT:", "Expected Output:",
# "Expected:", "Stdout:", "STDOUT:", "output:"
_OUTPUT_LABEL = re.compile(
    r'^(?:(?:expected\s+)?output|expected|stdout)\s*[:\-]\s*',
    re.MULTILINE | re.IGNORECASE,
)

# DESCRIPTION variants: "DESCRIPTION:", "Desc:", "Note:", "Explanation:"
_DESC_LABEL = re.compile(
    r'^(?:description|desc|note|explanation)\s*[:\-]\s*',
    re.MULTILINE | re.IGNORECASE,
)


def _extract_field(block: str, label_pattern: re.Pattern,
                   stop_patterns: List[re.Pattern]) -> str:
    """Extract a field value from a block, reading until the next label or end.

    Returns the stripped value, with markdown fences removed.
    """
    match = label_pattern.search(block)
    if not match:
        return ""

    start = match.end()
    # Find where the next field label begins (or end of block)
    end = len(block)
    for stop in stop_patterns:
        stop_match = stop.search(block, pos=start)
        if stop_match and stop_match.start() < end:
            end = stop_match.start()

    value = block[start:end].strip()
    return _strip_markdown_fences(value)


def _parse_block_fields(block: str) -> Optional[GeneratedTestCase]:
    """Parse description, input, and output from a single test case block."""
    desc = _extract_field(block, _DESC_LABEL,
                          [_INPUT_LABEL, _OUTPUT_LABEL])
    input_str = _extract_field(block, _INPUT_LABEL,
                               [_OUTPUT_LABEL, _DESC_LABEL])
    output_str = _extract_field(block, _OUTPUT_LABEL,
                                [_INPUT_LABEL, _DESC_LABEL])

    if not input_str or not output_str:
        return None

    return GeneratedTestCase(
        input_str=input_str,
        expected_output=output_str,
        description=desc,
    )


def _parse_structured_blocks(response: str) -> List[GeneratedTestCase]:
    """Parse test cases from structured 'TEST CASE N:' blocks."""
    # Find all header positions
    headers = list(_BLOCK_HEADER.finditer(response))
    if not headers:
        return []

    cases: List[GeneratedTestCase] = []
    for i, header in enumerate(headers):
        block_start = header.end()
        block_end = headers[i + 1].start() if i + 1 < len(headers) else len(response)
        block = response[block_start:block_end]

        case = _parse_block_fields(block)
        if case is not None:
            cases.append(case)

    return cases


# Fallback pattern: numbered items like "1.", "1)", "1:"
_NUMBERED_HEADER = re.compile(
    r'(?:^|\n)\s*(\d+)\s*[.):\-]\s*',
)


def _parse_numbered_blocks(response: str) -> List[GeneratedTestCase]:
    """Fallback parser for numbered blocks without 'TEST CASE' headers.

    Matches patterns like:
        1. Input: 5  Output: 10
        2) Input: 0  Output: 0
    """
    headers = list(_NUMBERED_HEADER.finditer(response))
    if not headers:
        return []

    cases: List[GeneratedTestCase] = []
    for i, header in enumerate(headers):
        block_start = header.end()
        block_end = headers[i + 1].start() if i + 1 < len(headers) else len(response)
        block = response[block_start:block_end]

        case = _parse_block_fields(block)
        if case is not None:
            cases.append(case)

    return cases


# ---------------------------------------------------------------------------
# Self-Test Generator
# ---------------------------------------------------------------------------

class SelfTestGen:
    """Generates test cases from problem statements for internal verification.

    When enabled, asks the LLM to reason about the problem and generate
    input/output pairs. These are used during Phase 3 refinement instead
    of real benchmark test cases.

    When disabled, returns empty results (noop).
    """

    def __init__(self, config: SelfTestGenConfig,
                 telemetry_dir: Optional[Path] = None):
        self.config = config
        self.telemetry_dir = telemetry_dir
        self._events_file: Optional[Path] = None
        if telemetry_dir is not None:
            telemetry_dir.mkdir(parents=True, exist_ok=True)
            self._events_file = telemetry_dir / "self_test_gen_events.jsonl"

    def generate(self, problem: str,
                 llm_call: Optional[LLMCallable] = None,
                 task_id: str = "") -> SelfTestResult:
        """Generate test cases from a problem statement.

        Args:
            problem: The problem description text.
            llm_call: LLM callable for generation.
            task_id: Task identifier for telemetry.

        Returns:
            SelfTestResult with generated test cases.
        """
        if not self.config.enabled:
            return SelfTestResult(task_id=task_id, reason="disabled")

        if llm_call is None:
            return SelfTestResult(task_id=task_id, reason="missing_callable")

        start_time = time.time()

        user_content = SELF_TEST_PROMPT.format(
            num_cases=self.config.num_test_cases,
            problem=problem,
        )

        # Wrap in ChatML so the shared client's normalizer
        # (chatml_to_messages) sees distinct system/user roles — the system
        # prompt is what keeps this structured step concise (no
        # think-language, so the adapter leaves thinking off).
        prompt = (
            "<|im_start|>system\n"
            "You are an expert programmer generating test cases."
            "<|im_end|>\n"
            "<|im_start|>user\n"
            f"{user_content}<|im_end|>\n"
            "<|im_start|>assistant\n"
        )

        try:
            response, tokens, _ = llm_call(
                prompt, self.config.generation_temperature,
                self.config.generation_max_tokens, None,
            )
        except Exception as e:
            return SelfTestResult(
                task_id=task_id, reason=f"llm_error: {e}",
            )

        test_cases = self.parse_test_cases(response)
        gen_time = (time.time() - start_time) * 1000

        result = SelfTestResult(
            test_cases=test_cases,
            generation_tokens=tokens,
            generation_time_ms=gen_time,
            task_id=task_id,
        )

        self._log_event(SelfTestGenEvent(
            task_id=task_id,
            num_cases_generated=len(test_cases),
            generation_tokens=tokens,
            generation_time_ms=gen_time,
        ))

        return result

    def parse_test_cases(self, response: str) -> List[GeneratedTestCase]:
        """Parse test cases from LLM response.

        Handles multiple formatting variations the model might produce:
        - "TEST CASE 1:", "Test Case 1:", "Test 1:", "test case 1:" etc.
        - "INPUT:", "Input:", "EXPECTED OUTPUT:", "Expected:", "Stdout:" etc.
        - Markdown code blocks wrapping values
        - Unclosed <think> tags (model hit token limit mid-think)
        - Fallback: numbered blocks ("1.", "1)") with input/output pairs
        """
        response = _strip_think_tags(response)

        # Primary: structured "TEST CASE N:" blocks
        cases = _parse_structured_blocks(response)

        # Fallback: numbered blocks like "1.", "1)", "1:" with input/output
        if not cases:
            cases = _parse_numbered_blocks(response)

        # Cap to configured limit
        return cases[:self.config.num_test_cases]

    def _log_event(self, event: SelfTestGenEvent) -> None:
        """Append event to JSONL telemetry file."""
        if self._events_file is None:
            return
        try:
            with open(self._events_file, "a") as f:
                f.write(json.dumps(event.to_dict()) + "\n")
        except OSError:
            # best-effort: swallow on failure (caller continues)
            pass

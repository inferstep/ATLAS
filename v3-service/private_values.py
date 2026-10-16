"""Private-value filtering: masks credential-shaped values before they
reach a serialized sink (logs, error details, diagnostics).

CANONICAL COPY NOTICE: this file exists byte-identically in four
places — atlas/redact.py, geometric-lens/geometric_lens/, sandbox/,
and v3-service/ — because each service ships as a separate container
without a shared package, and the CLI ships as a pip package no
service image installs. tests/contracts/test_private_value_filtering.py
enforces that the copies stay identical and that each passes the
shared fixture corpus (tests/fixtures/private_value_fixtures.json),
which the Go implementation (proxy/private_values.go) also passes.
Edit all four copies together.

Patterns are deliberately conservative (assignment/header/key-block
shapes with secret-ish key names) so ordinary content — "timeout=30",
token counts, health URLs — passes through untouched.
"""

import logging
import re

PLACEHOLDER = "[FILTERED]"

_ASSIGNMENT = re.compile(
    r'(?i)([A-Z0-9_.-]{0,64}(?:api[_-]?key|apikey|token|secret|password'
    r'|passwd|credential|access[_-]?key)[A-Z0-9_.-]{0,64}["\']?\s*[=:]\s*["\']?)'
    r'([^\s"\',;&]+)')
_BEARER = re.compile(r'(?i)(bearer\s+)([A-Za-z0-9._~+/=-]+)')
_URL_PASSWORD = re.compile(r'(://[^/:@\s]{0,64}:)([^@\s]{1,256})(@)')
_PRIVATE_KEY_BLOCK = re.compile(
    r'-----BEGIN [A-Z ]{0,40}PRIVATE KEY-----.*?-----END [A-Z ]{0,40}PRIVATE KEY-----',
    re.S)


def filter_private_values(text: str) -> str:
    """Mask credential-shaped substrings in text."""
    if not text:
        return text
    text = _PRIVATE_KEY_BLOCK.sub(PLACEHOLDER, text)
    text = _ASSIGNMENT.sub(r'\g<1>' + PLACEHOLDER, text)
    text = _BEARER.sub(r'\g<1>' + PLACEHOLDER, text)
    text = _URL_PASSWORD.sub(r'\g<1>' + PLACEHOLDER + r'\g<3>', text)
    return text


class PrivateValueLogFilter(logging.Filter):
    """Attach to a logger (or root) so every record is filtered before
    any handler serializes it: logger.addFilter(PrivateValueLogFilter())
    """

    def filter(self, record: logging.LogRecord) -> bool:
        try:
            msg = record.getMessage()
            filtered = filter_private_values(msg)
            if filtered != msg:
                record.msg = filtered
                record.args = ()
            # Tracebacks reach the sink via exc_info/exc_text, not msg —
            # and their last line is `ExceptionType: <message>`, which can
            # embed credentials (e.g. a connection error quoting a URL
            # with a password). Pre-format and mask here: the stdlib
            # formatter honors a pre-set exc_text, and JsonFormatter
            # prefers it too.
            if record.exc_info and not record.exc_text:
                record.exc_text = filter_private_values(
                    logging.Formatter().formatException(record.exc_info))
        except Exception:  # noqa: S110
            pass  # a filtering failure must never suppress the log line
        return True

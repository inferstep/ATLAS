# A copy of cognitive_complexity 1.3.0 from PyPI (sdist cognitive_complexity-1.3.0.tar.gz,
# sha256 a0cfbd47dee0b19f4056f892389f501694b205c3af69fb703cc744541e03dde5), by Ilya
# Lebedev, under the MIT licence in LICENSE beside this file. Its release date is in
# docs/project/THIRD_PARTY_NOTICES.md.
# scripts/ci/code_health.py measures Python's cognitive complexity with it. It is
# copied because PyPI has it only as an sdist, which pip cannot install with every
# file checked by hash. The code is as released, but for two changes that keep
# every score: the print call in api.py lost its bare lint-suppression comment,
# which suppresses nothing under this repository's ruff rules (RUF100), and
# process_control_flow_breaker in utils/ast.py has its three branches of the same
# body as one (SonarQube's S1871). tests/infrastructure/pipeline/test_code_health.py
# pins its scores, so an edit here that changes one fails.
__version__ = '1.3.0'

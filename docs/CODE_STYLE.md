# Code style: how ATLAS code should read

These rules apply to new code and to every refactor. Much of today's code
predates them. The code-health check (`scripts/code_health.py`) stops it
from growing, and the Code health epic (#262) brings it in line step by
step. The formatting and lint rules per language stay in
[CONTRIBUTING](../CONTRIBUTING.md#code-style).

The goal: a new contributor can open a file, see what it is for, and
change one thing without reading everything else.

## Size

- **A function does one job.** Aim for under 50 lines. **Over 100 lines
  fails the code-health check** for new functions. A long function becomes
  named steps, each with one job.
- **A file holds one concern.** New files stay under 1,500 lines, or the
  check fails.
- **Oversized code doesn't grow.** A function or file on the baseline
  (`.github/code-health-baseline.json`) may shrink but not grow. If your
  change makes one shorter, lower its number in the same pull request.

## Structure

- **One package or module per concern.** In Go, a package groups the
  code for one thing (the agent loop, the tools, verification, the model
  client, the HTTP server) and exposes a small surface. In Python, one
  module per concern.
- **Dependencies point one way.** Lower layers (tools, the model client)
  never import higher ones (the agent loop).
- **Extend the design, don't layer on it.** A fix belongs where the
  behaviour lives. A new check that repeats an existing one with a special
  case means the existing one needs the change.

## Abstractions

- **Add an abstraction for a second real use, not an imagined one.** An
  interface with one implementation, a wrapper that only forwards, or a
  factory for one type makes the code harder to follow.
- **Extract repeated logic on the third copy,** not the first.
- **No switches for core behaviour.** ATLAS has one behaviour for V3, the
  lens and verification. A flag that turns a core part off is a second
  product to test. Configuration is for the environment (paths, ports,
  models), not for the design.

## Names

- Names say what a thing is or does: `repairOpenFiles`, not `handle2` or
  `doIt`.
- Use the project's words: *session*, *candidate*, *deliverable*,
  *verification*, *lens*. The same thing keeps the same name everywhere.
- No abbreviations beyond common ones (`ctx`, `err`, `id`, `url`).

## Comments

- **Say why, in one to three lines.** The code says what. A comment
  explains an intent or a reason the code can't show.
- **History belongs in git, not in comments.** "Round 3 of the TB2 fix",
  "added after incident X", or a paragraph of past attempts goes in the
  commit message. A comment may point to an issue: `// see #214`.
- **Delete dead code.** Don't comment it out; git keeps it.

## Errors

- Handle an error where you can act on it; otherwise return it with
  context.
- **Never swallow an error silently** (an empty `except`, `_ = err` with
  no reason). If ignoring is right, say why in one line.
- Messages name the cause and the next step, not only "failed".

## Tests

- A fix comes with a test that fails without it: break the fix once and
  check.
- A test name says the behaviour: `TestDeletionNeedsItsOwnApproval`, not
  `TestCase7`.
- Tests check behaviour through the public surface, not private details
  that a refactor would change.

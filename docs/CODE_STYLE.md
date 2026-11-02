# Code style: how ATLAS code should read

These rules apply to new code and to every refactor. Much of today's code
predates them. The code-health check (`scripts/ci/code_health.py`) stops it
from growing, and the Code health epic (#262) brings it in line step by
step. The formatting and lint rules per language stay in
[CONTRIBUTING](../.github/CONTRIBUTING.md#code-style).

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
- **A folder stays short enough to read.** Over 25 entries, files and
  sub-folders together, the check prints a note and passes. Split a folder
  by subject when that helps a reader, and leave it flat when a sub-folder
  would make it harder to follow.

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

Tests are code, and the goal above holds for them: a person reads one test
from top to bottom, sees what it sets up, what it does and what it expects,
and can still change it in a year.

- A fix comes with a test that fails without it: break the fix once and
  check.
- A test name says the behaviour: `TestDeletionNeedsItsOwnApproval`, not
  `TestCase7`.
- Tests check behaviour through the public surface, not private details
  that a refactor would change.
- **A test that would be a copy of another but for its values is a row of
  a table.** One body, and one row for each case, with a name that says
  what the case is. In Go: a list of cases and `t.Run`. In Python:
  `pytest.mark.parametrize`, with the name beside its row
  (`pytest.param(..., id="...")`). The body has no condition that picks
  between rows.
- **A failure names the input, the result and what was wanted**, so that
  nobody has to run the test again to see what went wrong.
- **Writing the inputs out in each test is right**, also when two tests
  then look alike: the reader sees them without opening another place. Some
  repetition is the price of a test that says everything in itself, and it
  is worth it.
- **A new helper only when it removes real repetition**, and only when the
  test still reads from top to bottom. A helper sets things up.
- **No check is hidden in a helper.** The comparison stands in the test.
- **A count of tests or of lines is no merit.** The text of a pull request
  does not give one as such. What counts is that a planted fault turns a
  test red.

An example. Five tests of one function, each a copy of the others but for
its values. Two of the five:

```go
func TestTierMaxTurnsEnvOverride(t *testing.T) {
	t.Setenv("ATLAS_MAX_TURNS", "150")
	if got := TierMaxTurns(Tier2Medium); got != 150 {
		t.Errorf("env override = %d, want 150 (operator's call, no upper clamp)", got)
	}
}

func TestTierMaxTurnsInvalidEnvFallsThrough(t *testing.T) {
	t.Setenv("ATLAS_MAX_TURNS", "garbage")
	if got := TierMaxTurns(Tier2Medium); got != 0 {
		t.Errorf("invalid env, T2 = %d, want 0 (uncapped default)", got)
	}
}
```

The five as one table, 26 lines where there were 50. Every case of the rule
is in one place, and a failure names its row, the input, the result and
what was wanted:

```go
func TestTierMaxTurns(t *testing.T) {
	cases := []struct {
		name string
		env  string // the value of ATLAS_MAX_TURNS
		tier Tier
		want int // 0 is "no limit" for the agent loop
	}{
		{"no setting: a conversation gets 12 turns, room to read a few files", "", Tier0Conversational, 12},
		{"no setting: simple work has no limit", "", Tier1Simple, 0},
		{"no setting: medium work has no limit", "", Tier2Medium, 0},
		{"no setting: hard work has no limit", "", Tier3Hard, 0},
		{"the operator's number is taken", "150", Tier2Medium, 150},
		{"a very high number is taken too: there is no upper bound", "10000", Tier2Medium, 10000},
		{"0 means the tier's own number: no limit for medium work", "0", Tier2Medium, 0},
		{"0 means the tier's own number: 12 for a conversation", "0", Tier0Conversational, 12},
		{"a setting that is no number means the tier's own number", "garbage", Tier2Medium, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("ATLAS_MAX_TURNS", c.env)
			if got := TierMaxTurns(c.tier); got != c.want {
				t.Errorf("TierMaxTurns(%v) with ATLAS_MAX_TURNS=%q = %d, want %d", c.tier, c.env, got, c.want)
			}
		})
	}
}
```

These rules are for new tests and for a test that is changed anyway. The
tests that are there are not rewritten for them.

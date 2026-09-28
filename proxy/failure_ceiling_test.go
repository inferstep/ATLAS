package main

import "testing"

// The locked benchmark (redteam/runs/locked, dev head 78d345c) recorded two
// sessions that never stopped: one bounced 19 identical calls off a tool ban
// and ended only when the model deleted its own accepted artifact, another
// bounced 85 and ran 102 turns over two hours. A third session, bouncing the
// same way off the identical-resend branch, stopped correctly.
//
// The difference was not the counters. Both branches incremented
// totalFailures and consecutiveErrors; only one of them read the result.
// These tests pin the stopping DECISION so every rejection branch has to
// share it, which is what makes the ban path and the resend path agree.

func TestFailureCeilingStopsAtMaxTotalFailures(t *testing.T) {
	if !shouldStopForFailures(maxTotalFailures, 0, nil) {
		t.Fatalf("expected a stop at totalFailures=%d", maxTotalFailures)
	}
	if !shouldStopForFailures(maxTotalFailures+1, 0, nil) {
		t.Fatal("expected a stop above the ceiling")
	}
}

func TestFailureCeilingStopsWhenStuckOnOnePath(t *testing.T) {
	stuck := []string{"snake_game.py", "snake_game.py", "snake_game.py"}
	if !shouldStopForFailures(3, 3, stuck) {
		t.Fatal("expected a stop: 3 consecutive failures on one path")
	}
}

// Recovery before the threshold has to keep working, or the fix trades a
// runaway loop for a harness that quits on the first bad turn.
func TestFailureCeilingAllowsRecoveryBelowThreshold(t *testing.T) {
	if shouldStopForFailures(1, 1, []string{"a.py"}) {
		t.Fatal("must not stop after a single failure")
	}
	if shouldStopForFailures(maxTotalFailures-1, 2,
		[]string{"a.py", "b.py", "c.py"}) {
		t.Fatal("must not stop below the ceiling on varied paths")
	}
}

// The ban branch is the one the benchmark caught counting without reading.
// Pair 1 reached 22 failures and pair 2 reached 96 against a ceiling of 12.
func TestBanBounceCountsReachTheCeiling(t *testing.T) {
	total, consecutive := 0, 0
	paths := []string{}
	stoppedAt := -1
	for bounce := 1; bounce <= 19; bounce++ { // pair 1's observed run
		consecutive++
		total++
		paths = append(paths, "snake_game.py")
		if len(paths) > 3 {
			paths = paths[len(paths)-3:]
		}
		if shouldStopForFailures(total, consecutive, paths) {
			stoppedAt = bounce
			break
		}
	}
	if stoppedAt == -1 {
		t.Fatal("19 identical ban bounces never reached a stop")
	}
	if stoppedAt > maxTotalFailures {
		t.Fatalf("stopped at bounce %d, later than the ceiling %d",
			stoppedAt, maxTotalFailures)
	}
}

// Tier1/2/3 run with MaxTurns == 0 (uncapped, types.go TierMaxTurns), so the
// failure ceiling is the only bound that exists for them.
func TestUncappedTurnsStillHitTheFailureCeiling(t *testing.T) {
	// Premise assertion, not a skip. If tier routing ever caps Tier2 this
	// must fail visibly so the test is updated deliberately, rather than
	// quietly removing the coverage that the failure ceiling is the ONLY
	// bound these tiers have.
	if got := TierMaxTurns(Tier2Medium); got != 0 {
		t.Fatalf("premise changed: Tier2Medium is now capped at %d turns; this "+
			"test exists because uncapped tiers rely solely on the failure "+
			"ceiling -- update it intentionally", got)
	}
	total := 0
	for turn := 0; turn < 200; turn++ {
		total++
		if shouldStopForFailures(total, 1, nil) {
			return
		}
	}
	t.Fatal("an uncapped tier ran 200 failing turns without a stop")
}

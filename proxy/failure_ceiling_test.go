package main

import "testing"

// Every rejection branch of the loop shares one stopping DECISION. Counting
// failures is not enough: a branch that increments the counters and never
// reads them bounces the same call off a tool ban until the session
// deadline. These tests pin the decision itself.

// Recovery before the threshold has to keep working, or the fix trades a
// runaway loop for a loop that quits on the first bad turn.
func TestShouldStopForFailures(t *testing.T) {
	for _, tc := range []struct {
		name               string
		total, consecutive int
		paths              []string
		wantStop           bool
	}{
		{"at the ceiling", maxTotalFailures, 0, nil, true},
		{"above the ceiling", maxTotalFailures + 1, 0, nil, true},
		{"3 consecutive failures on one path", 3, 3, []string{"snake_game.py", "snake_game.py", "snake_game.py"}, true},
		{"a single failure", 1, 1, []string{"a.py"}, false},
		{"below the ceiling on varied paths", maxTotalFailures - 1, 2, []string{"a.py", "b.py", "c.py"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldStopForFailures(tc.total, tc.consecutive, tc.paths); got != tc.wantStop {
				t.Errorf("shouldStopForFailures(%d, %d, %v) = %v, want %v",
					tc.total, tc.consecutive, tc.paths, got, tc.wantStop)
			}
		})
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

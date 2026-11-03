package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// v3CallTimeout is the interactive wall-clock cap for a single V3 pipeline
// call. It bounds a long-tail repair stall. Set ATLAS_V3_TIMEOUT to a second
// count to override, or 0 to disable the cap for offline bench runs.
//
// 300s because a lower cap cannot contain the pipeline it is capping: PlanSearch
// spends two LLM calls per candidate, so k=3 costs about 162s at about 22s per
// call, before the probe and the self-test that precede it, and phase-3 repair
// then runs out of time. The budget cap responds by cutting k to 1, which
// removes the candidates candidate-agreement needs to compare.
func v3CallTimeout() time.Duration {
	if v := os.Getenv("ATLAS_V3_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 300 * time.Second
}

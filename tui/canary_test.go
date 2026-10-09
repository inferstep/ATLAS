package main

import "testing"

// Planted by scripts/canary.py for the canary pull request. Never merge it.
func TestCanaryMustFail(t *testing.T) {
	t.Fatal("canary: this test fails on purpose")
}

// canaryNeverCalled is for the linter to report: nothing calls it.
func canaryNeverCalled() {}

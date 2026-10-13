package main

import "testing"

// objectHoldSkipReason is what a skipped test prints. One reason for all of
// them, so a run on a system without the hold says the same thing each time.
const objectHoldSkipReason = "this test goes through the deletion approval, which holds the file it asks about. " +
	"This system has no such hold (it exists on Linux only), so delete_file is refused here before anyone is asked"

// needsObjectHold skips a test that goes through the deletion approval on a
// system where the proxy cannot hold the object it asks about. Where the
// hold exists it does nothing, so the test runs.
func needsObjectHold(t *testing.T) {
	t.Helper()
	if !objectHoldSupported {
		t.Skip(objectHoldSkipReason)
	}
}

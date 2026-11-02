.PHONY: verify verify-full

# Run the checks this change needs. Prints only what failed, with the fix.
verify:
	@python3 scripts/ci/verify.py

# The same, plus the slow suites that CI runs.
verify-full:
	@python3 scripts/ci/verify.py --full

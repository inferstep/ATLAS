package main

import (
	"os"
	"strconv"
)

// How an untrusted command ended, as a fact rather than as an exit code.
//
// A staged verification command took the deployed inference server down. Its
// family seeds a loop that appends without bound, its own failing test calls
// it, and its client-declared verification is `python3 -m pytest -q`. pytest
// reached 5.9 GB in seconds and the kernel ran a host-global out-of-memory
// kill, choosing the largest resident process on the box. The executor bounded
// TIME at sixty seconds per command and nothing else, so the time bound never
// got a chance.
//
// The missing ceiling is only half of it. The other half is that a command
// stopped at a ceiling exits non-zero exactly like a failing test: a python
// process that hits an address-space limit raises MemoryError and exits 1.
// Read as an exit code, a verification that never completed becomes a
// behavioural failure of the candidate -- this machine asserting something it
// did not observe. So the executor says how the command ended, and only one
// member of this vocabulary means it reached its own end.

type ExecutionOutcome string

const (
	// ExecutionCompleted is the only member that means the command ran to its
	// own conclusion. Its exit code is then a fact about the command.
	ExecutionCompleted ExecutionOutcome = "completed"
	// Stopped at a ceiling. The exit code belongs to whoever stopped it.
	ExecutionTimedOut        ExecutionOutcome = "timed_out"
	ExecutionMemoryExhausted ExecutionOutcome = "memory_exhausted"
	ExecutionProcessLimit    ExecutionOutcome = "process_limit_exceeded"
	ExecutionOutputLimit     ExecutionOutcome = "output_limit_exceeded"
	ExecutionCancelled       ExecutionOutcome = "cancelled"
	ExecutionSpawnFailed     ExecutionOutcome = "spawn_failed"
	// ExecutionUnclassified is the fail-closed member. An executor that does
	// not speak this vocabulary, a response that omits the field, and a
	// stopped command nobody could name all land here -- and it is never
	// complete.
	ExecutionUnclassified ExecutionOutcome = "internal_unclassified"
)

func knownExecutionOutcome(o ExecutionOutcome) bool {
	switch o {
	case ExecutionCompleted, ExecutionTimedOut, ExecutionMemoryExhausted,
		ExecutionProcessLimit, ExecutionOutputLimit, ExecutionCancelled,
		ExecutionSpawnFailed, ExecutionUnclassified:
		return true
	}
	return false
}

// canonicalExecutionOutcome keeps an unknown value out of every decision.
//
// An empty field is an executor older than this vocabulary. It is read as
// unclassified rather than as completed, so a deployment skew fails toward
// refusing evidence instead of toward inventing it.
func canonicalExecutionOutcome(raw string) ExecutionOutcome {
	o := ExecutionOutcome(raw)
	if raw == "" || !knownExecutionOutcome(o) {
		return ExecutionUnclassified
	}
	return o
}

// executionCompleted reports whether the command reached its own end.
//
// THE predicate. Nothing else may ask "was it a resource kill" by listing
// members, because a member added later would then be silently treated as a
// completion by every site that forgot to update its list.
func executionCompleted(o ExecutionOutcome) bool {
	// Canonicalised first, because a RunCommandOutput built in Go -- a
	// transport failure, a refusal, a fixture -- carries the zero value and
	// never passed the decode. An empty outcome is an unclassified one
	// everywhere, and it is never a completion.
	return canonicalExecutionOutcome(string(o)) == ExecutionCompleted
}

// executionKnownIncomplete reports whether this build can SEE that the command
// did not reach its own end.
//
// It is the weaker of the two questions, and it exists because the two callers
// carry different consequences. Candidate staging mints authorization from what
// it observes, so it demands a positive completion and treats an executor it
// cannot read as having observed nothing. Ordinary run_command retires mutation
// debt and reports to the model, and an executor too old to speak this
// vocabulary cannot report a resource kill at all -- so refusing everything it
// says would disable verification wholesale for no safety gained, while
// refusing what it CAN report keeps the fix.
//
// Candidate staging holds a stricter line on the same field: there an
// unclassified outcome becomes `unobservable`, because staging is minting
// authorization evidence over bytes about to be delivered and an unread
// executor cannot support that. A tool result the model reads and an
// authorization the machine acts on are different bars, on purpose.
func executionKnownIncomplete(o ExecutionOutcome) bool {
	c := canonicalExecutionOutcome(string(o))
	return c != ExecutionCompleted && c != ExecutionUnclassified
}

// executionStoppedByResource reports whether a ceiling ended it. Used only for
// wording and telemetry; no decision keys off it, because "not completed" is
// already the whole answer.
func executionStoppedByResource(o ExecutionOutcome) bool {
	switch canonicalExecutionOutcome(string(o)) {
	case ExecutionMemoryExhausted, ExecutionProcessLimit, ExecutionOutputLimit:
		return true
	}
	return false
}

// executionOutcomeMessage is what the MODEL is told.
//
// Bounded and actionable, and deliberately free of host limits, pids, cgroup
// paths, addresses and deployment detail: the model can act on "this used too
// much memory to finish", and can do nothing with the number except try to
// stay under it, which is not a thing it can reason about reliably.
func executionOutcomeMessage(o ExecutionOutcome) string {
	switch o {
	case ExecutionMemoryExhausted:
		return "the command was stopped because it used too much memory to finish. " +
			"It did NOT fail — it never got to an answer. Look for unbounded growth " +
			"(a loop whose condition can never become false, a list appended to " +
			"forever, a recursion with no base case) and fix that before running it again."
	case ExecutionProcessLimit:
		return "the command was stopped because it started too many processes to finish. " +
			"It did NOT fail — it never got to an answer. Look for a loop that spawns " +
			"without waiting, or a recursive invocation of the command itself."
	case ExecutionOutputLimit:
		return "the command was stopped because it produced too much output to finish. " +
			"It did NOT fail — it never got to an answer. Quiet it down (redirect or " +
			"filter the output) and run it again."
	case ExecutionSpawnFailed:
		return "the command could not be started at all, so nothing ran."
	case ExecutionUnclassified:
		return "the command stopped for a reason this build cannot name, so its result " +
			"says nothing about the code."
	}
	return ""
}

// hostAddressSpaceKiB is the per-command address-space ceiling for host
// execution, in the KiB `ulimit -v` speaks, read from the same operator value
// the sandbox uses so the two cannot drift into separate memory policies.
func hostAddressSpaceKiB() int64 {
	bytes := int64(2 * 1024 * 1024 * 1024)
	if raw := os.Getenv("ATLAS_EXEC_MEMORY_BYTES"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 64*1024*1024 {
			bytes = n
		}
	}
	// Address space, not resident memory, so it is read as a multiple of the
	// operator's memory value rather than as that value: a mapping is
	// reserved before it is touched, and a ceiling set AT the resident number
	// refuses reservations a command never grows into.
	return int64(float64(bytes) * 1.5 / 1024)
}

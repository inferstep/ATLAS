package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Whether the memory this deployment may simultaneously demand fits the host.
//
// A per-command ceiling stops one runaway command. It does not stop a
// deployment whose configured maxima add up to more RAM than the machine has:
// the executor was already free to ask for eleven gigabytes on a fifteen-
// gigabyte host where the inference server held nine, and every process
// involved was inside its own limit at the moment the kernel went looking for
// something to kill. It chose the largest resident process, which was the
// model.
//
// So the sum is checked, once, at startup, against a host size the operator
// states. Nothing is inferred: an envelope nobody declared is not silently
// assumed to be fine, it is reported as unvalidated, and the per-command
// contract carries the safety on its own until someone declares one.

// memoryBudget is one component's budget, as the operator declared it.
type memoryBudget struct {
	Name  string
	Bytes int64
	// Enforced is whether a cgroup limit actually holds this component to
	// Bytes. An unenforced budget is an EXPECTATION, and the difference
	// decides what the sum means: adding an unenforced number into a
	// "the maxima fit" check would be inventing a ceiling nobody set.
	Enforced bool
}

// memoryEnvelope is the whole declaration.
type memoryEnvelope struct {
	HostBytes int64
	// Reserve is what must remain for the kernel, the container daemon,
	// logging, monitoring and an orderly shutdown. It is a budget like any
	// other and it is never the leftover.
	ReserveBytes int64
	Budgets      []memoryBudget
	// PerCommandBytes is the ceiling one untrusted command may reach. It must
	// leave room inside the sandbox's own budget for the executor and the
	// output it buffers, so a single command cannot consume the container.
	PerCommandBytes int64
	// SandboxBytes is the container budget the per-command ceiling lives in.
	SandboxBytes int64
	// Concurrency is how many untrusted commands may run at once.
	Concurrency int
}

func (e memoryEnvelope) declared() bool { return e.HostBytes > 0 }

// total is every simultaneously reachable HARD maximum, plus the reserve.
//
// Unenforced budgets are excluded on purpose. A component with no cgroup limit
// has no maximum to add: its real ceiling is the host, and folding an expected
// figure in here would turn a measurement into a promise.
func (e memoryEnvelope) total() int64 {
	sum := e.ReserveBytes
	for _, b := range e.Budgets {
		if b.Enforced {
			sum += b.Bytes
		}
	}
	return sum
}

// remainder is what is left for everything that is NOT held to a limit.
func (e memoryEnvelope) remainder() int64 { return e.HostBytes - e.total() }

// unenforcedOverruns names each unbounded component whose declared expectation
// does not fit in what the enforced budgets leave.
//
// Reported, never refused. The refusal exists for maxima that over-commit --
// a configuration error an operator can fix by editing a number. An unbounded
// component that outgrows the remainder is a different problem with a
// different fix (validate a limit for it, or give it a smaller model), and
// disabling execution would not make the machine any safer.
func (e memoryEnvelope) unenforcedOverruns() []string {
	var out []string
	rem := e.remainder()
	for _, b := range e.Budgets {
		if b.Enforced || b.Bytes <= 0 {
			continue
		}
		if b.Bytes > rem {
			out = append(out, fmt.Sprintf(
				"%s is not held to a limit and is expected to reach %s, which is "+
					"more than the %s the enforced budgets leave",
				b.Name, humanBytes(b.Bytes), humanBytes(rem)))
		}
	}
	return out
}

// validate reports every way the declaration cannot hold. All of them, not the
// first: an operator fixing one number wants to see the others.
func (e memoryEnvelope) validate() []string {
	var problems []string
	if e.HostBytes <= 0 {
		return []string{"host memory is not declared"}
	}
	if e.ReserveBytes <= 0 {
		problems = append(problems, "no host reserve is declared: the kernel, the "+
			"container daemon and an orderly shutdown are not free")
	}
	if e.Concurrency <= 0 {
		problems = append(problems, fmt.Sprintf("concurrency is %d", e.Concurrency))
	}
	for _, b := range e.Budgets {
		if b.Bytes <= 0 {
			problems = append(problems,
				fmt.Sprintf("%s has no budget", b.Name))
		}
	}
	if total := e.total(); total > e.HostBytes {
		problems = append(problems, fmt.Sprintf(
			"the declared maxima total %s on a %s host, over by %s",
			humanBytes(total), humanBytes(e.HostBytes), humanBytes(total-e.HostBytes)))
	}
	if e.SandboxBytes > 0 && e.PerCommandBytes > 0 {
		// One command may not consume the container it shares with the
		// executor process and the output that process buffers.
		want := e.PerCommandBytes * int64(max(1, e.Concurrency))
		if want >= e.SandboxBytes {
			problems = append(problems, fmt.Sprintf(
				"%d concurrent commands at %s each need %s, which is not less than "+
					"the sandbox's own %s: the executor and its buffers share it",
				e.Concurrency, humanBytes(e.PerCommandBytes), humanBytes(want),
				humanBytes(e.SandboxBytes)))
		}
	}
	if e.PerCommandBytes > 0 && e.SandboxBytes <= 0 {
		problems = append(problems, "a per-command ceiling is declared with no "+
			"container budget to hold it")
	}
	return problems
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return strconv.FormatFloat(float64(n)/float64(1<<30), 'f', 2, 64) + " GiB"
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/float64(1<<20), 'f', 0, 64) + " MiB"
	}
	return strconv.FormatInt(n, 10) + " B"
}

// envBytes reads a size that may carry a k/m/g suffix, the way compose writes
// them, so one value can be shared by the compose file and this check.
func envBytes(name string) int64 {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	if raw == "" {
		return 0
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(raw, "g"), strings.HasSuffix(raw, "gb"):
		mult, raw = 1<<30, strings.TrimRight(raw, "gb")
	case strings.HasSuffix(raw, "m"), strings.HasSuffix(raw, "mb"):
		mult, raw = 1<<20, strings.TrimRight(raw, "mb")
	case strings.HasSuffix(raw, "k"), strings.HasSuffix(raw, "kb"):
		mult, raw = 1<<10, strings.TrimRight(raw, "kb")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n * mult
}

// envelopeFromEnv reads the operator's declaration.
//
// Absent ATLAS_HOST_MEMORY_BYTES nothing is declared and nothing is checked:
// an envelope this build guessed at would be this build asserting something
// the operator never said.
func envelopeFromEnv() memoryEnvelope {
	e := memoryEnvelope{
		HostBytes:       envBytes("ATLAS_HOST_MEMORY_BYTES"),
		ReserveBytes:    envBytes("ATLAS_HOST_RESERVE_BYTES"),
		PerCommandBytes: envBytes("ATLAS_EXEC_MEMORY_BYTES"),
		SandboxBytes:    envBytes("ATLAS_SANDBOX_MEM"),
		Concurrency:     1,
	}
	if n, err := strconv.Atoi(os.Getenv("ATLAS_EXEC_CONCURRENCY")); err == nil && n > 0 {
		e.Concurrency = n
	}
	for name, key := range map[string]string{
		"lens": "ATLAS_LENS_MEM", "v3-service": "ATLAS_V3_MEM",
		"proxy": "ATLAS_PROXY_MEM", "sandbox": "ATLAS_SANDBOX_MEM",
	} {
		if b := envBytes(key); b > 0 {
			e.Budgets = append(e.Budgets, memoryBudget{Name: name, Bytes: b, Enforced: true})
		}
	}
	// The inference service is declared in two parts, because what it is
	// EXPECTED to use and what it is HELD to are different facts and only one
	// of them exists today. ATLAS_LLAMA_MEM is the cgroup limit and ships
	// unset: the deployed server's own peak resident set is above the value
	// that was proposed for it, and a hard limit under the peak turns an
	// occasional host-pressure event into a certain kill of the one process
	// the whole system depends on. ATLAS_LLAMA_BUDGET_BYTES is the measured
	// expectation, used for accounting so the arithmetic stays honest about
	// the largest term rather than omitting it.
	if enforced := envBytes("ATLAS_LLAMA_MEM"); enforced > 0 {
		e.Budgets = append(e.Budgets, memoryBudget{
			Name: "inference", Bytes: enforced, Enforced: true})
	} else if expected := envBytes("ATLAS_LLAMA_BUDGET_BYTES"); expected > 0 {
		e.Budgets = append(e.Budgets, memoryBudget{
			Name: "inference", Bytes: expected, Enforced: false})
	}
	sort.Slice(e.Budgets, func(i, j int) bool { return e.Budgets[i].Name < e.Budgets[j].Name })
	return e
}

// executionEnvelopeRefusal is the message the execution path answers with when
// the declared envelope cannot hold, or "" when execution may proceed.
//
// Refusing the EXECUTION PATH rather than the process: a proxy that will not
// start leaves an operator with no way to read the diagnosis, and reading a
// file or answering a question was never the unsafe part.
func executionEnvelopeRefusal(e memoryEnvelope) string {
	if !e.declared() {
		return ""
	}
	problems := e.validate()
	if len(problems) == 0 {
		return ""
	}
	return "command execution is disabled: the declared memory envelope cannot " +
		"hold — " + strings.Join(problems, "; ")
}

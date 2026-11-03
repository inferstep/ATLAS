package memoryenvelope

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

// captureLog returns what fn wrote to the standard logger.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	fn()
	return buf.String()
}

// Every process can be inside its own limit while the sum is not. An envelope
// whose enforced maxima over-commit the host is refused.
func TestAnEnvelopeThatDoesNotFitTheHostIsRefused(t *testing.T) {
	const GiB = int64(1) << 30
	fits := Envelope{
		HostBytes: 15*GiB + GiB/3, ReserveBytes: 3 * GiB / 2,
		Budgets: []Budget{
			{"lens", 1792 * (1 << 20), true}, {"v3-service", 512 * (1 << 20), true},
			{"proxy", 256 * (1 << 20), true}, {"sandbox", 1536 * (1 << 20), true},
		},
		PerCommandBytes: GiB, SandboxBytes: 1536 * (1 << 20), Concurrency: 1,
	}
	if problems := fits.validate(); len(problems) != 0 {
		t.Fatalf("the shipped envelope does not validate: %v", problems)
	}
	if ExecutionRefusal(fits) != "" {
		t.Error("a fitting envelope refused execution")
	}

	// Enforced maxima that over-commit are a number an operator can edit, so
	// they are refused: an inference limit beside an 11 GiB sandbox.
	enforcedOver := fits
	enforcedOver.Budgets = append(append([]Budget{}, fits.Budgets...),
		Budget{"inference", 9728 * (1 << 20), true})
	for i := range enforcedOver.Budgets {
		if enforcedOver.Budgets[i].Name == "sandbox" {
			enforcedOver.Budgets[i] = Budget{"sandbox", 11 * GiB, true}
		}
	}
	enforcedOver.SandboxBytes = 11 * GiB
	if !strings.Contains(ExecutionRefusal(enforcedOver), "over by") {
		t.Errorf("an over-committed enforced set was not refused: %q",
			ExecutionRefusal(enforcedOver))
	}

	// Concurrency has to fit too, or raising it is a silent over-commit.
	crowded := fits
	crowded.Concurrency = 2
	if len(crowded.validate()) == 0 {
		t.Error("two concurrent commands fit in a container that holds one")
	}

	// Nothing declared is not the same as everything fine: no refusal, and no
	// claim either.
	if ExecutionRefusal(Envelope{}) != "" {
		t.Error("an undeclared envelope refused execution")
	}
	var undeclared Envelope
	if undeclared.declared() {
		t.Error("an empty envelope reports itself as declared")
	}
}

func TestTheEnvelopeReadsTheSizesComposeWrites(t *testing.T) {
	for raw, want := range map[string]int64{
		"1536m": 1536 << 20, "9728M": 9728 << 20, "11g": 11 << 30,
		"2G": 2 << 30, "1073741824": 1073741824, "512k": 512 << 10,
		"": 0, "lots": 0, "-5": 0,
	} {
		if got := parseBytes(raw); got != want {
			t.Errorf("%q read as %d, want %d", raw, got, want)
		}
	}
}

// A declaration nobody can act on fails closed rather than being treated as
// unlimited.
func TestAMalformedEnvelopeFailsClosed(t *testing.T) {
	const GiB = int64(1) << 30
	base := Envelope{
		HostBytes: 16 * GiB, ReserveBytes: GiB,
		Budgets:      []Budget{{"inference", 8 * GiB, true}, {"sandbox", GiB, true}},
		SandboxBytes: GiB, PerCommandBytes: GiB / 2, Concurrency: 1,
	}
	if len(base.validate()) != 0 {
		t.Fatalf("the base envelope does not validate: %v", base.validate())
	}
	for _, tc := range []struct {
		name   string
		change func(*Envelope)
	}{
		{"no host reserve", func(e *Envelope) { e.ReserveBytes = 0 }},
		{"a component with no budget", func(e *Envelope) {
			e.Budgets = []Budget{{"inference", 0, true}}
		}},
		{"a per-command ceiling with no container to hold it", func(e *Envelope) { e.SandboxBytes = 0 }},
		{"zero concurrency", func(e *Envelope) { e.Concurrency = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := base
			tc.change(&e)
			if len(e.validate()) == 0 {
				t.Error("validated")
			}
		})
	}
}

// An unbounded component is not a maximum.
//
// The inference service may run without a cgroup limit, because a hard limit
// under its peak resident set produces a certain kill rather than reclaim. Its
// budget is then an EXPECTATION. It is left out of the maxima sum, since an
// unbounded component has no maximum to add, and it is reported, with its
// size, when the expectation does not fit what the enforced budgets leave.
func TestAnUnenforcedBudgetIsReportedAndNotSummed(t *testing.T) {
	const GiB = int64(1) << 30
	e := Envelope{
		HostBytes: 15*GiB + GiB/3, ReserveBytes: 3 * GiB / 2,
		Budgets: []Budget{
			{"lens", 1792 * (1 << 20), true}, {"v3-service", 512 * (1 << 20), true},
			{"proxy", 256 * (1 << 20), true}, {"sandbox", 1536 * (1 << 20), true},
			{"inference", 11 * GiB, false},
		},
		PerCommandBytes: GiB, SandboxBytes: 1536 * (1 << 20), Concurrency: 1,
	}
	// The enforced maxima fit, so execution is not refused: a configuration an
	// operator can fix by editing a number is what the refusal is for.
	if problems := e.validate(); len(problems) != 0 {
		t.Fatalf("the enforced maxima do not fit: %v", problems)
	}
	if ExecutionRefusal(e) != "" {
		t.Error("an unbounded component refused execution")
	}
	// But it IS reported: 11 GiB expected against what four enforced budgets
	// and the reserve leave.
	over := e.unenforcedOverruns()
	if len(over) != 1 || !strings.Contains(over[0], "inference") {
		t.Fatalf("the unbounded overrun is not reported: %v", over)
	}
	if !strings.Contains(over[0], "not held to a limit") {
		t.Errorf("the report does not say it is unbounded: %q", over[0])
	}
	if !strings.Contains(over[0], "11.00 GiB") {
		t.Errorf("the report does not give the expected size: %q", over[0])
	}
	// Enforcing it instead moves it into the sum, where it does not fit.
	enforced := e
	enforced.Budgets = append([]Budget{}, e.Budgets...)
	enforced.Budgets[4].Enforced = true
	if len(enforced.validate()) == 0 {
		t.Error("enforcing an 11 GiB inference budget on a 15 GiB host validated")
	}
	if len(enforced.unenforcedOverruns()) != 0 {
		t.Error("an enforced budget is still reported as an overrun")
	}
	// And a fitting expectation is silent.
	small := e
	small.Budgets = append([]Budget{}, e.Budgets...)
	small.Budgets[4].Bytes = 5 * GiB
	if len(small.unenforcedOverruns()) != 0 {
		t.Errorf("a fitting expectation was reported: %v", small.unenforcedOverruns())
	}
}

func TestAnOverrunIsLoggedAtStartWhenTheEnvelopeIsDeclared(t *testing.T) {
	const GiB = int64(1) << 30
	declared := Envelope{HostBytes: 4 * GiB, Budgets: []Budget{{"inference", 11 * GiB, false}}}
	logged := captureLog(t, func() { LogOverruns(declared) })
	if !strings.Contains(logged, "inference is not held to a limit") {
		t.Errorf("the overrun was not logged: %q", logged)
	}

	undeclared := declared
	undeclared.HostBytes = 0
	logged = captureLog(t, func() { LogOverruns(undeclared) })
	if logged != "" {
		t.Errorf("an envelope nobody declared was reported: %q", logged)
	}
}

// ATLAS_LLAMA_MEM enforces; ATLAS_LLAMA_BUDGET_BYTES only accounts. Setting
// the first must not leave the second silently shadowing it.
func TestTheInferenceBudgetIsEnforcedOnlyWhenTheLimitIsSet(t *testing.T) {
	t.Setenv("ATLAS_HOST_MEMORY_BYTES", "16495915008")
	t.Setenv("ATLAS_LLAMA_BUDGET_BYTES", "11811160064")
	t.Setenv("ATLAS_LLAMA_MEM", "")
	e := FromSettings(os.Getenv)
	var inference *Budget
	for i := range e.Budgets {
		if e.Budgets[i].Name == "inference" {
			inference = &e.Budgets[i]
		}
	}
	if inference == nil {
		t.Fatal("the inference term is missing from the envelope entirely")
	}
	if inference.Enforced {
		t.Error("an unset ATLAS_LLAMA_MEM produced an enforced budget")
	}
	if inference.Bytes != 11811160064 {
		t.Errorf("expectation %d", inference.Bytes)
	}
	t.Setenv("ATLAS_LLAMA_MEM", "9728m")
	e = FromSettings(os.Getenv)
	inference = nil
	for i := range e.Budgets {
		if e.Budgets[i].Name == "inference" {
			inference = &e.Budgets[i]
		}
	}
	if inference == nil {
		t.Fatal("a set ATLAS_LLAMA_MEM dropped the inference term")
	}
	if !inference.Enforced {
		t.Error("a set ATLAS_LLAMA_MEM did not enforce")
	}
	if inference.Bytes != 9728*(1<<20) {
		t.Errorf("the expectation shadowed the limit: %d", inference.Bytes)
	}
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Optional candidate generation owns its budget (STABILIZATION_CYCLE_1, C2).
//
// Measured on c5927b3: 32 V3 activations on writes took 3866 s, about half of
// all session time, 7 were cut at the cap with their work discarded, and none
// delivered a candidate -- under the default strict policy with no declared
// outputs, nothing the producer returns can reach disk. Generation now runs
// only where its result could be delivered and its cap leaves the declared
// allowance. The checks on the write itself run either way.

type budgetWorld struct {
	ctx *AgentContext
	dir string

	mu       sync.Mutex
	v3       int
	embedded string // compared with a previous version, a source containing this gets a finding
	unknown  string // a source containing this has an unresolved call
}

func (w *budgetWorld) v3Calls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.v3
}

func newBudgetWorld(t *testing.T) *budgetWorld {
	t.Helper()
	w := &budgetWorld{dir: t.TempDir()}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var in struct {
			Source, Code, Content, Previous string
		}
		_ = json.Unmarshal(raw, &in)
		body := in.Source + in.Code + in.Content
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"):
			w.mu.Lock()
			w.v3++
			w.mu.Unlock()
			http.Error(rw, "generation stub", http.StatusTeapot)
		case r.URL.Path == "/internal/cyclomatic_complexity":
			json.NewEncoder(rw).Encode(map[string]interface{}{"functions": []interface{}{}})
		case r.URL.Path == "/internal/embedded_script_check":
			out := map[string]interface{}{"ok": true, "findings": []interface{}{}}
			// A comparative finding (a loop the change stopped driving): only
			// visible against the previous version, which the final-byte syntax
			// check does not send.
			if w.embedded != "" && in.Previous != "" && strings.Contains(body, w.embedded) {
				out["findings"] = []map[string]interface{}{{"line": 3, "column": 1,
					"kind": "javascript", "where": "the <script> block", "message": "unexpected `)`"}}
			}
			json.NewEncoder(rw).Encode(out)
		case r.URL.Path == "/internal/structural_check":
			unresolved := []string{}
			if w.unknown != "" && strings.Contains(body, w.unknown) {
				unresolved = []string{w.unknown}
			}
			json.NewEncoder(rw).Encode(map[string]interface{}{"ok": true, "unresolved": unresolved})
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			valid := !strings.Contains(body, "def broken(")
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{"SyntaxError: invalid syntax"}
			}
			json.NewEncoder(rw).Encode(out)
		default:
			json.NewEncoder(rw).Encode(map[string]interface{}{"ok": true})
		}
	}))
	t.Cleanup(srv.Close)
	ctx := NewAgentContext(w.dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.Ctx = context.Background()
	ctx.V3URL, ctx.SandboxURL = srv.URL, srv.URL
	ctx.Messages = []AgentMessage{{Role: "user", Content: "write the module"}}
	w.ctx = ctx
	return w
}

// A Tier 2 module: large enough that the write route consults the producer.
const budgetModule = `import math


def area(radius):
    if radius < 0:
        raise ValueError("negative radius")
    return math.pi * radius * radius


def perimeter(radius):
    for _ in range(1):
        pass
    return 2 * math.pi * radius
`

func (w *budgetWorld) write(t *testing.T, rel, content string) *ToolResult {
	t.Helper()
	if tier := classifyFileTier(rel, content); tier < Tier2Medium {
		t.Fatalf("fixture must be Tier 2+, got %v", tier)
	}
	args, _ := json.Marshal(map[string]string{"path": rel, "content": content})
	return executeToolCall("write_file", args, w.ctx)
}

func (w *budgetWorld) seed(t *testing.T, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(w.dir, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// (i) No contract: no target is grounded, so no generation request, and the
// model's bytes land.
func TestNoGenerationWhenNoCandidateCouldBeDelivered(t *testing.T) {
	w := newBudgetWorld(t)
	res := w.write(t, "solve.py", budgetModule)
	if !res.Success {
		t.Fatalf("the write did not land: %s", res.Error)
	}
	if n := w.v3Calls(); n != 0 {
		t.Errorf("%d generation request(s) in a session where no candidate could be delivered", n)
	}
	if got := writeGenerationBypass(w.ctx, Tier2Medium, false); got != bypassCandidateUndeliverable {
		t.Errorf("bypass reason = %q", got)
	}
	if got := editGenerationBypass(w.ctx, Tier2Medium, true, false); got != bypassCandidateUndeliverable {
		t.Errorf("edit bypass reason = %q", got)
	}
}

// (ii) Where a candidate could be delivered, generation is still requested:
// declared outputs, or declared work whose own call grounds the target.
func TestGenerationStillRunsWhereACandidateCouldBeDelivered(t *testing.T) {
	t.Run("declared outputs", func(t *testing.T) {
		w := newBudgetWorld(t)
		w.ctx.TaskContract = declaredOutputs("solve.py")
		w.write(t, "solve.py", budgetModule)
		if w.v3Calls() == 0 {
			t.Error("no generation request although declared outputs make a candidate deliverable")
		}
	})
	t.Run("declared work", func(t *testing.T) {
		w := newBudgetWorld(t)
		w.ctx.TaskContract = &TaskContract{TaskMode: TaskModeWork,
			OutputKnowledge: KnowledgeUnspecified, VerificationKnowledge: KnowledgeUnspecified}
		w.write(t, "solve.py", budgetModule)
		if w.v3Calls() == 0 {
			t.Error("no generation request for declared work")
		}
	})
}

// (iii) Deliverable, but the cap would leave less than the allowance.
func TestNoGenerationWhenItsCapWouldBreachTheWorkAllowance(t *testing.T) {
	t.Setenv("ATLAS_V3_TIMEOUT", "300")
	allowance := workAllowance()
	if total, reserve := sessionBudget(); allowance != time.Duration(float64(total-reserve)*0.35) &&
		allowance != 120*time.Second {
		t.Fatalf("allowance %s is not max(120s, 35%% of the work budget)", allowance)
	}
	for _, c := range []struct {
		left     time.Duration
		generate bool
	}{
		// cap = min(300s, left/2); generation needs left - cap >= allowance
		{2*allowance + 20*time.Second, true},
		{2*allowance - 20*time.Second, false},
		{90 * time.Second, false},
	} {
		w := newBudgetWorld(t)
		w.ctx.TaskContract = declaredOutputs("solve.py")
		work, cancel := context.WithTimeout(context.Background(), c.left)
		w.ctx.Ctx = work
		got := writeGenerationBypass(w.ctx, Tier2Medium, false)
		if (got == bypassNone) != c.generate {
			t.Errorf("%s left (allowance %s): bypass %q, want generate=%v", c.left, allowance, got, c.generate)
		}
		if !c.generate {
			if got != bypassWorkAllowance {
				t.Errorf("%s left: reason %q, want %q", c.left, got, bypassWorkAllowance)
			}
			w.write(t, "solve.py", budgetModule)
			if n := w.v3Calls(); n != 0 {
				t.Errorf("%s left: %d generation request(s) breached the allowance", c.left, n)
			}
		}
		cancel()
	}
	// Uncapped generation can run to the deadline, so it never leaves one.
	t.Setenv("ATLAS_V3_TIMEOUT", "0")
	w := newBudgetWorld(t)
	w.ctx.TaskContract = declaredOutputs("solve.py")
	work, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	w.ctx.Ctx = work
	if got := writeGenerationBypass(w.ctx, Tier2Medium, false); got != bypassWorkAllowance {
		t.Errorf("uncapped generation with a deadline: %q, want %q", got, bypassWorkAllowance)
	}
}

// (iv) Every mandatory check still refuses its case on the direct path, with no
// generation request.
func TestMandatoryChecksRefuseOnTheDirectPath(t *testing.T) {
	healthyPage := strings.Repeat("<!-- layout -->\n", 60) +
		"<html><body>\n<script>\nlet x = 1;\n</script>\n</body></html>\n"
	for _, c := range []struct {
		name, rel, before, after string
		setup                    func(w *budgetWorld)
		want                     string
	}{
		{"healthy to broken syntax", "solve.py", budgetModule,
			strings.Replace(budgetModule, "def area(radius):", "def broken(radius:", 1), nil, "syntax error"},
		{"unresolved name introduced", "solve.py", budgetModule,
			strings.Replace(budgetModule, "return math.pi * radius * radius", "return frobnicate(radius)", 1),
			func(w *budgetWorld) { w.unknown = "frobnicate" }, "frobnicate"},
		{"embedded script broken", "index.html", healthyPage,
			strings.Replace(healthyPage, "let x = 1;", "let x = 1;);", 1),
			func(w *budgetWorld) { w.embedded = "let x = 1;);" }, "<script>"},
		{"duplicate entrypoint", "solve.py",
			budgetModule + "\n\nif __name__ == \"__main__\":\n    print(area(1))\n",
			budgetModule + "\n\nif __name__ == \"__main__\":\n    print(area(1))\n\nif __name__ == \"__main__\":\n    print(area(2))\n",
			nil, "__main__"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newBudgetWorld(t)
			if c.setup != nil {
				c.setup(w)
			}
			w.seed(t, c.rel, c.before)
			res := w.write(t, c.rel, c.after)
			if res.Success {
				t.Fatalf("the direct path landed a write its mandatory check refuses")
			}
			if !strings.Contains(res.Error, c.want) {
				t.Errorf("refusal does not name the problem (%q):\n%s", c.want, res.Error)
			}
			if b, _ := os.ReadFile(filepath.Join(w.dir, c.rel)); string(b) != c.before {
				t.Error("the file on disk changed")
			}
			if n := w.v3Calls(); n != 0 {
				t.Errorf("%d generation request(s)", n)
			}
		})
	}
}

// Where generation does run and the producer fails, the fallback writer applies
// the same comparative checks before landing the model's bytes.
func TestTheProducerFallbackAppliesTheComparativeChecks(t *testing.T) {
	w := newBudgetWorld(t)
	w.ctx.TaskContract = declaredOutputs("solve.py")
	before := budgetModule + "\n\nif __name__ == \"__main__\":\n    print(area(1))\n"
	w.seed(t, "solve.py", before)
	w.ctx.RecordFileRead(filepath.Join(w.dir, "solve.py"), before)
	after := before + "\nif __name__ == \"__main__\":\n    print(area(2))\n"
	res := w.write(t, "solve.py", after)
	if w.v3Calls() == 0 {
		t.Fatal("fixture did not reach the producer")
	}
	if res.Success || !strings.Contains(res.Error, "__main__") {
		t.Errorf("the fallback landed a duplicated entrypoint: success=%v %s", res.Success, res.Error)
	}
	if b, _ := os.ReadFile(filepath.Join(w.dir, "solve.py")); string(b) != before {
		t.Error("the file on disk changed")
	}
}

// Skipping generation must not remove a delivery that could have happened.
// For every contract/capture configuration the skip predicate calls
// undeliverable, the real candidate route is forced to run with the strongest
// candidate the fixture can offer; the caller's baseline must be what lands.
// Where the predicate says deliverable, the same fixture must be able to
// deliver in at least one configuration, or the matrix proves nothing.
func TestSkippingGenerationLosesNoDelivery(t *testing.T) {
	contracts := map[string]string{
		"no contract":                        "",
		"question":                           `{"task_mode":"question"}`,
		"work only (the reliability runner)": `{"task_mode":"work"}`,
		"work, an older client's policy":     `{"task_mode":"work","candidate_policy":"advisory"}`,
		"declared output":                    workContract,
		"verification declared, no outputs":  `{"task_mode":"work","verification_knowledge":"declared","verification":["python3 solve.py"]}`,
	}
	delivered := 0
	for name, contract := range contracts {
		for _, supported := range []bool{true, false} {
			for _, capture := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/supported=%v/capture=%v", name, supported, capture), func(t *testing.T) {
					if capture {
						t.Setenv(CandidateCaptureOnlyEnv, "1")
					}
					w := newAutomaticWorld(t, workContract, routeWinner, nil, supported)
					w.ctx.TaskContract = nil
					if contract != "" {
						w.ctx.TaskContract = mustContract(t, w.dir, contract)
					}
					deliverable := candidateDeliverable(w.ctx)
					if _, err := w.write(t); err != nil {
						t.Fatalf("write failed: %v", err)
					}
					got := w.disk(t)
					if !deliverable && got != routeBaseline {
						t.Fatalf("the skip predicate said undeliverable, but the forced route delivered %q", got)
					}
					if got == routeWinner {
						delivered++
					}
					t.Logf("deliverable=%v delivered=%v", deliverable, got == routeWinner)
				})
			}
		}
	}
	if delivered == 0 {
		t.Fatal("no configuration delivered: the fixture cannot show that a skip loses nothing")
	}
}

// With generation skipped, an unreachable check service is still visible: the
// client is told once, and every write's gate log carries the count.
func TestACheckServiceOutageIsVisibleWhenGenerationIsSkipped(t *testing.T) {
	w := newBudgetWorld(t)
	var notices []string
	w.ctx.StreamFn = func(event string, data interface{}) {
		if m, ok := data.(map[string]string); ok && event == "text" {
			notices = append(notices, m["content"])
		}
	}
	w.seed(t, "solve.py", budgetModule)
	// The check service is down; the sandbox (syntax) still answers.
	sandbox := w.ctx.SandboxURL
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	w.ctx.V3URL = dead.URL
	w.ctx.SandboxURL = sandbox
	for i := 0; i < 2; i++ {
		changed := strings.Replace(budgetModule, "return 2 * math.pi * radius", fmt.Sprintf("return %d * math.pi * radius", 3+i), 1)
		if res := w.write(t, "solve.py", changed); !res.Success {
			t.Fatalf("write %d did not land: %s", i, res.Error)
		}
	}
	if got := writeGenerationBypass(w.ctx, Tier2Medium, false); got != bypassCandidateUndeliverable {
		t.Fatalf("fixture must skip generation, got %q", got)
	}
	shown := 0
	for _, n := range notices {
		if strings.Contains(n, "unresolved-name check unavailable") {
			shown++
		}
	}
	if shown != 1 {
		t.Errorf("the outage was shown %d times, want exactly once: %v", shown, notices)
	}
	if s := w.ctx.checkServiceFailureSummary(); !strings.Contains(s, "unresolved-name=") {
		t.Errorf("the session does not count the outage: %q", s)
	}
}

// A check the session itself cancelled is not an outage.
func TestACancelledCheckIsNotReportedAsAnOutage(t *testing.T) {
	w := newBudgetWorld(t)
	var notices int
	w.ctx.StreamFn = func(event string, _ interface{}) {
		if event == "text" {
			notices++
		}
	}
	c, cancel := context.WithCancel(context.Background())
	cancel()
	w.ctx.Ctx = c
	noteCheckServiceUnavailable(w.ctx, "unresolved-name", "service unreachable")
	if notices != 0 || w.ctx.checkServiceFailureSummary() != "" {
		t.Errorf("a cancelled request was reported as an outage (notices=%d, summary=%q)",
			notices, w.ctx.checkServiceFailureSummary())
	}
}

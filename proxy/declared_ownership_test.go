package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Who owns the answer about a candidate.
//
// The typed path used to claim ownership when it had derived some obligations.
// That reads the wrong thing. A contract declaring `expected_outputs: []` is a
// client stating authoritatively that this request produces nothing, and it
// derives no obligations for exactly that reason -- so counting them made the
// most explicit statement a client can make indistinguishable from silence,
// and handed the candidate to the legacy decision on the strength of it.
//
// Ownership is presence-aware: did the client STATE what this request
// produces. Everything here is a way of asking that question and checking the
// answer travels intact from the wire to the disk.

// --- the wire ---------------------------------------------------------------------

// decodeContractOverHTTP puts a body through the real request decoder and the
// real validator, so `[]` versus absent is decided where production decides it
// and not by a test constructing a struct.
func decodeContractOverHTTP(t *testing.T, dir, body string) *TaskContract {
	t.Helper()
	var captured *TaskContract
	var decodeErr error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Message      string        `json:"message"`
			TaskContract *TaskContract `json:"task_contract,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			decodeErr = err
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		if req.TaskContract == nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		tc, err := validateTaskContract(req.TaskContract, dir)
		if err != nil {
			decodeErr = err
			http.Error(w, "invalid", http.StatusBadRequest)
			return
		}
		captured = tc
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if decodeErr != nil {
		t.Fatalf("%s: %v", body, decodeErr)
	}
	return captured
}

func TestExplicitEmptyAndUnspecifiedSurviveTheWire(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		name, body      string
		wantContract    bool
		wantDeclared    bool
		wantPathsCount  int
		wantOutputsSeen bool
	}{
		{name: "no contract at all",
			body: `{"message":"go"}`},
		{name: "contract with no output knowledge",
			body:         `{"message":"go","task_contract":{"task_mode":"work"}}`,
			wantContract: true},
		{name: "output knowledge explicitly unspecified",
			body: `{"message":"go","task_contract":{"task_mode":"work",` +
				`"output_knowledge":"unspecified"}}`,
			wantContract: true},
		{name: "declared with paths",
			body: `{"message":"go","task_contract":{"task_mode":"work",` +
				`"output_knowledge":"declared","expected_outputs":["solve.py"]}}`,
			wantContract: true, wantDeclared: true, wantPathsCount: 1,
			wantOutputsSeen: true},
		{name: "declared with an authoritative empty set",
			body: `{"message":"go","task_contract":{"task_mode":"work",` +
				`"output_knowledge":"declared","expected_outputs":[]}}`,
			wantContract: true, wantDeclared: true, wantPathsCount: 0,
			wantOutputsSeen: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := decodeContractOverHTTP(t, dir, c.body)
			if (tc != nil) != c.wantContract {
				t.Fatalf("contract present=%v, want %v", tc != nil, c.wantContract)
			}
			ctx := NewAgentContext(dir, Tier2Medium)
			ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-wire")
			ctx.HumanTask = "go"
			ctx.TaskContract = tc

			if got := outputKnowledgeDeclared(ctx); got != c.wantDeclared {
				t.Errorf("declared=%v, want %v", got, c.wantDeclared)
			}
			if tc != nil {
				if got := tc.OutputsPresent(); got != c.wantOutputsSeen {
					t.Errorf("outputs present=%v, want %v", got, c.wantOutputsSeen)
				}
				if got := len(tc.OutputPaths()); got != c.wantPathsCount {
					t.Errorf("%d declared paths, want %d", got, c.wantPathsCount)
				}
			}
			// The obligation owner agrees, and the count alone cannot tell the
			// last two apart -- which is the whole reason ownership does not
			// read it.
			d := resolveOutputObligation(ctx, "go")
			if d.KnowledgeSpecified != c.wantDeclared {
				t.Errorf("obligation owner says specified=%v, want %v",
					d.KnowledgeSpecified, c.wantDeclared)
			}
		})
	}
	// The two that matter: both derive zero obligations, and only one is owned.
	empty := decodeContractOverHTTP(t, dir,
		`{"message":"go","task_contract":{"task_mode":"work","output_knowledge":"declared","expected_outputs":[]}}`)
	unspec := decodeContractOverHTTP(t, dir,
		`{"message":"go","task_contract":{"task_mode":"work","output_knowledge":"unspecified"}}`)
	for _, c := range []struct {
		name     string
		tc       *TaskContract
		declared bool
	}{{"explicit empty", empty, true}, {"unspecified", unspec, false}} {
		ctx := NewAgentContext(dir, Tier2Medium)
		ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-wire")
		ctx.HumanTask = "go"
		ctx.TaskContract = c.tc
		if n := len(requestObligations(ctx)); n != 0 {
			t.Errorf("%s derived %d obligations, want 0", c.name, n)
		}
		if got := outputKnowledgeDeclared(ctx); got != c.declared {
			t.Errorf("%s: declared=%v, want %v", c.name, got, c.declared)
		}
	}
}

func TestQuestionModeDeclaresNeitherClass(t *testing.T) {
	// The boundary refuses it outright, which is the strongest form of the
	// rule: a question-mode contract that declares an output class never
	// becomes a stored contract at all, so nothing downstream has to remember
	// to ignore it.
	dir := t.TempDir()
	for _, body := range []string{
		`{"task_mode":"question","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		`{"task_mode":"question","output_knowledge":"declared","expected_outputs":[]}`,
		`{"task_mode":"question","verification_knowledge":"declared","verification":["pytest -q"]}`,
	} {
		var req struct {
			TaskContract *TaskContract `json:"task_contract,omitempty"`
		}
		if err := json.Unmarshal([]byte(`{"task_contract":`+body+`}`), &req); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if _, err := validateTaskContract(req.TaskContract, dir); err == nil {
			t.Errorf("%s was accepted; question mode may declare neither class", body)
		}
	}
	// And a plain question-mode contract declares nothing either way.
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-q")
	ctx.HumanTask = "what does this do"
	ctx.TaskContract = mustContract(t, dir, `{"task_mode":"question"}`)
	if outputKnowledgeDeclared(ctx) {
		t.Error("question mode declared an output class")
	}
	if resolveVerificationObligation(ctx).KnowledgeSpecified {
		t.Error("question mode declared a verification class")
	}
}

func TestVerificationKnowledgeIsIndependentOfOutputKnowledge(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		name, contract    string
		outputs, verifies bool
	}{
		{"outputs only", `{"task_mode":"work","output_knowledge":"declared",` +
			`"expected_outputs":["solve.py"]}`, true, false},
		{"verification only", `{"task_mode":"work","verification_knowledge":"declared",` +
			`"verification":["pytest -q"]}`, false, true},
		{"both", `{"task_mode":"work","output_knowledge":"declared",` +
			`"expected_outputs":["solve.py"],"verification_knowledge":"declared",` +
			`"verification":["pytest -q"]}`, true, true},
		{"neither", `{"task_mode":"work"}`, false, false},
	} {
		ctx := NewAgentContext(dir, Tier2Medium)
		ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-ind")
		ctx.HumanTask = "go"
		ctx.TaskContract = mustContract(t, dir, c.contract)
		if got := outputKnowledgeDeclared(ctx); got != c.outputs {
			t.Errorf("%s: output declared=%v, want %v", c.name, got, c.outputs)
		}
		if got := resolveVerificationObligation(ctx).KnowledgeSpecified; got != c.verifies {
			t.Errorf("%s: verification declared=%v, want %v", c.name, got, c.verifies)
		}
	}
}

// --- the route ---------------------------------------------------------------------

func TestDeclaredEmptyOwnsTheRouteAndAuthorizesNothing(t *testing.T) {
	w := newRouteWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":[]}`, nil)
	recs := captureShadow(t, func() {
		if _, err := w.write(t); err != nil {
			t.Fatalf("write failed: %v", err)
		}
	})
	// The typed path owns it, and the answer is that nothing is authorized.
	got := recordsOfKind(recs, "candidate_authorization_decision")
	if len(got) != 1 {
		t.Fatalf("%d decisions, want one", len(got))
	}
	if got[0]["influences_live_decision"] != true {
		t.Error("an explicitly empty declaration was not owned by the typed path")
	}
	if got[0]["reason"] != string(ReasonTargetNotDeclared) {
		t.Errorf("reason %v, want target_not_declared", got[0]["reason"])
	}
	// The candidate does not land, and does not fall back to legacy delivery.
	onDisk, err := os.ReadFile(w.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) == routeWinner {
		t.Error("an explicitly empty declaration delivered a V3 candidate")
	}
	if consumedGrants(recs) != 0 {
		t.Error("an explicitly empty declaration spent an authorization")
	}
}

// A work request that stated no outputs is grounded by the model's own call:
// the typed path owns it, the selected candidate lands on one grant, and a
// declared command still binds. A request with no contract names nothing, so
// the model's own bytes land and the typed path has no opinion.
func TestUnspecifiedOutputsAreGroundedByTheModelsCall(t *testing.T) {
	for _, c := range []struct {
		contract string
		owned    bool
	}{
		{"", false},
		{`{"task_mode":"work"}`, true},
		{`{"task_mode":"work","output_knowledge":"unspecified"}`, true},
		{`{"task_mode":"work","verification_knowledge":"declared","verification":["pytest -q"]}`, true},
	} {
		w := newRouteWorld(t, c.contract, map[string]stubEffect{"pytest -q": {ExitCode: 0}})
		recs := captureShadow(t, func() {
			if _, err := w.write(t); err != nil {
				t.Fatalf("%q: %v", c.contract, err)
			}
		})
		onDisk, err := os.ReadFile(w.path)
		if err != nil {
			t.Fatal(err)
		}
		want, grants := routeBaseline, 0
		if c.owned {
			want, grants = routeWinner, 1
		}
		if string(onDisk) != want {
			t.Errorf("%q: disk holds %q, want %q", c.contract, string(onDisk), want)
		}
		for _, r := range recordsOfKind(recs, "candidate_authorization_decision") {
			if r["influences_live_decision"] != c.owned {
				t.Errorf("%q: influences_live_decision=%v, want %v",
					c.contract, r["influences_live_decision"], c.owned)
			}
		}
		if got := consumedGrants(recs); got != grants {
			t.Errorf("%q: %d grants spent, want %d", c.contract, got, grants)
		}
	}
}

func TestADeclaredDocumentIsNotAnUndeclaredTarget(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		name, contract, file string
		want                 AuthorizationReason
	}{
		{"declared document", `{"task_mode":"work","output_knowledge":"declared",` +
			`"expected_outputs":["notes.md"]}`, "notes.md",
			ReasonNoAuthorizationPrerequisite},
		{"undeclared target", `{"task_mode":"work","output_knowledge":"declared",` +
			`"expected_outputs":["something_else.md"]}`, "notes.md",
			ReasonTargetNotDeclared},
		{"declared code with evidence", `{"task_mode":"work","output_knowledge":"declared",` +
			`"expected_outputs":["solve.py"]}`, "solve.py", ReasonAuthorized},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := "# notes\n"
			if strings.HasSuffix(c.file, ".py") {
				body = authPy
			}
			w := newAuthWorld(t, c.contract, c.file, body, true)
			ev, evID, ok := w.observe(t)
			var evidence []proxyEvidence
			if ok {
				evidence = append(evidence, ev)
			}
			a := w.authorize(evID, nil, evidence...)
			if a.Decision.Reason != c.want {
				t.Errorf("reason %q, want %q", a.Decision.Reason, c.want)
			}
			if !a.Typed {
				t.Error("a declared request was not owned by the typed path")
			}
			if (a.Grant != nil) != (c.want == ReasonAuthorized) {
				t.Errorf("grant=%v for reason %q", a.Grant != nil, a.Decision.Reason)
			}
		})
	}
	_ = dir
}

func TestADeclaredCodeTargetWithoutEvidenceIsNotAuthorized(t *testing.T) {
	w := newAuthWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		"solve.py", authPy, true)
	_, evID, ok := w.observe(t)
	if !ok {
		t.Fatal("the producer did not run")
	}
	// The obligation exists and nothing spoke for it.
	a := w.authorize(evID, nil)
	if a.Decision.Reason != ReasonEvidenceMissing {
		t.Errorf("reason %q, want evidence_missing", a.Decision.Reason)
	}
	if a.Grant != nil {
		t.Error("a code target with no evidence minted a grant")
	}
}

// --- what a refusal must not do -----------------------------------------------------

func TestATypedRefusalNeverReachesLegacyCandidateBytes(t *testing.T) {
	for _, contract := range []string{
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":[]}`,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["other.py"]}`,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],` +
			`"verification_knowledge":"declared","verification":["pytest -q"]}`,
	} {
		w := newRouteWorld(t, contract, map[string]stubEffect{"pytest -q": {ExitCode: 1}})
		res, err := w.write(t)
		if err != nil {
			t.Fatalf("%q: %v", contract, err)
		}
		onDisk, err := os.ReadFile(w.path)
		if err != nil {
			t.Fatal(err)
		}
		if string(onDisk) == routeWinner {
			t.Errorf("%q: a refused candidate landed", contract)
		}
		if res.V3Used {
			t.Errorf("%q: a refused candidate reported V3 provenance", contract)
		}
		if res.AuthorizedDeliveryHash != "" {
			t.Errorf("%q: a refused candidate named an authorization", contract)
		}
	}
}

// TestDeclaredEmptyDoesNotManufactureCompletion is the honesty half. Owning the
// route and refusing is not the same as saying the run is finished, and every
// other gate keeps its own say.
func TestDeclaredEmptyDoesNotManufactureCompletion(t *testing.T) {
	w := newRouteWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":[]}`, nil)
	if _, err := w.write(t); err != nil {
		t.Fatal(err)
	}
	// Nothing was delivered through the typed path, so nothing is settled.
	if owed, _ := postDeliverySettlementOwed(w.ctx); owed {
		t.Error("an undelivered request owes settlement")
	}
	// And the terminal is not persuaded that a request which wrote nothing of
	// its own is complete.
	status, reason := finalizeCompletion(w.ctx, &runState{}, "make it fast", "")
	if status == TerminalCompleted && reason == "no_file_obligation" {
		// Acceptable only when the run genuinely owed nothing; the write above
		// wrote the caller's own baseline, so the ledger has an entry.
		t.Errorf("status %q reason %q: a refused delivery read as no obligation",
			status, reason)
	}
	// The independent gates still exist and still answer for themselves.
	if _, why := terminalCompletionAllowed(w.ctx, nil); why == "" {
		t.Error("the deliverable gate stopped answering")
	}
}

// TestDirectModelWritesAreNotTypedTargetAuthorization pins that ordinary tool
// behaviour is untouched: a write that never went through the candidate route
// mints nothing and settles nothing.
func TestDirectModelWritesAreNotTypedTargetAuthorization(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-direct")
	ctx.HumanTask = "write it"
	ctx.TaskContract = mustContract(t, dir,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`)
	path := filepath.Join(dir, "solve.py")

	res, err := writeFileRecorded(path, authPy, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.AuthorizedDeliveryHash != "" {
		t.Error("a direct write claimed an authorization")
	}
	if liveGrantCount(ctx) != 0 {
		t.Error("a direct write minted a grant")
	}
	if deliverySettlementFor(ctx, path) != nil {
		t.Error("a direct write recorded a settlement")
	}
}

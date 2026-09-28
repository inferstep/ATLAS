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

// The evidence matrix: eight shapes a real request can take, each run through
// the real obligation derivation and the real producers against a stubbed
// sandbox, and each checked for the same two things.
//
//	no content leak     no candidate byte, command string or source line
//	                    appears in any binding that leaves a producer
//	a direct join       every record names the request, invocation and
//	                    candidate it is about, with no inference in between
//
// Nothing here delivers. The producers have no production caller, so a matrix
// row that produces authorizing evidence is describing what WOULD close an
// obligation if a consumer existed.

type matrixWorld struct {
	ctx     *AgentContext
	path    string
	hash    string
	code    string
	sandbox *httptest.Server
	// shell scripts what a staged command does. The defaults are a clean
	// pass; a row that needs a failure sets them before staging.
	shellExit    int
	shellMutate  bool
	shellInput   bool
	shellTimeout bool
	shellRuns    int
	// shellFail names the commands this executor fails, when a row needs some
	// to pass and others not.
	shellFail map[string]bool
}

// newMatrixWorld builds a workspace with one candidate on disk, a sandbox
// whose structural check answers `valid`, and whatever contract the row needs.
func newMatrixWorld(t *testing.T, contract, filename, code string, valid bool) *matrixWorld {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	world := &matrixWorld{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{"SyntaxError: invalid syntax"}
			}
			json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(r.URL.Path, "/shell"):
			// The executor's half of staging: overlay in, hashes either side
			// out. It draws no conclusion, exactly as the real one does not.
			var in struct {
				Command      string            `json:"command"`
				Files        map[string]string `json:"files"`
				ObservePaths []string          `json:"observe_paths"`
			}
			if json.NewDecoder(r.Body).Decode(&in) != nil || len(in.ObservePaths) == 0 {
				http.Error(w, "staging requires an overlay and an observation",
					http.StatusBadRequest)
				return
			}
			world.shellRuns++
			exit := world.shellExit
			if world.shellFail[in.Command] {
				exit = 1
			}
			observed := in.ObservePaths[0]
			before := contentSHA256(in.Files[observed])
			after, ws := before, "ws-before"
			if world.shellMutate {
				after = contentSHA256("rewritten\n")
				ws = "ws-after"
			}
			if world.shellInput {
				// The candidate is untouched; something else in the workspace
				// is not.
				ws = "ws-after"
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success":   exit == 0 && !world.shellTimeout,
				"exit_code": exit,
				"outcome":   "completed",
				"stdout":    "", "stderr": "", "timed_out": world.shellTimeout,
				"observation": map[string]interface{}{
					"target_before":    map[string]string{observed: before},
					"target_after":     map[string]string{observed: after},
					"workspace_before": "ws-before", "workspace_after": ws,
					"workspace_files": 2, "digest_truncated": false,
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.SandboxURL = srv.URL
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-matrix")
	if contract != "" {
		ctx.TaskContract = mustContract(t, dir, contract)
	}
	resolved := resolveAgentPath(ctx, filename)
	world.ctx, world.path, world.code, world.sandbox = ctx, resolved, code, srv
	world.hash = fileSHA256(ctx, resolved)
	return world
}

func (w *matrixWorld) obligations() []taskObligation {
	return deriveTaskObligations(w.ctx,
		resolveOutputObligation(w.ctx, "Create it."),
		resolveVerificationObligation(w.ctx))
}

// assertNoLeak is the invariant every row shares.
func assertNoLeak(t *testing.T, row string, ev proxyEvidence, secrets ...string) {
	t.Helper()
	blob, err := json.Marshal(ev.Provenance)
	if err != nil {
		t.Fatalf("%s: %v", row, err)
	}
	for _, s := range secrets {
		if s == "" {
			continue
		}
		if strings.Contains(string(blob), s) {
			t.Errorf("%s: the binding carries %q", row, s)
		}
	}
}

// assertDirectJoin is the other: the record names what it is about, without
// anything downstream having to infer it.
func assertDirectJoin(t *testing.T, row string, ev proxyEvidence,
	requestID, invocationID, candidateID, candidateHash string) {
	t.Helper()
	p := ev.Provenance
	for _, c := range []struct{ name, got, want string }{
		{"request_id", p.RequestID, requestID},
		{"invocation_id", p.InvocationID, invocationID},
		{"candidate_instance_id", p.CandidateInstanceID, candidateID},
		{"candidate_hash", p.CandidateHash, candidateHash},
	} {
		if c.got != c.want {
			t.Errorf("%s: %s is %q, want %q", row, c.name, c.got, c.want)
		}
	}
	if p.ObligationID == "" || p.WorkspaceStateHash == "" {
		t.Errorf("%s: incomplete join %+v", row, p)
	}
}

// stagedRun builds the request the wiring builds: one declared command, one
// staged observation of it, bound to this world's candidate. The shape is the
// point -- the producer is handed an OBSERVATION, never a conclusion.
func (w *matrixWorld) stagedRun(obl taskObligation, outcome stagingCommandOutcome,
	mutatedTarget, mutatedWorkspace bool) verificationEvidenceRequest {
	generation, stateHash := workspaceIdentity(w.ctx)
	return verificationEvidenceRequest{
		Obligation: obl,
		Result: stagingCommandResult{
			CommandIdentity: contentSHA256(obl.Subject), ObligationID: obl.ID,
			Index: 0, Count: 1, Outcome: outcome,
			TargetHashBefore: w.hash, TargetHashAfter: w.hash,
			WorkspaceHashBefore: "ws-before", WorkspaceHashAfter: "ws-before",
			MutatedTarget: mutatedTarget, MutatedWorkspace: mutatedWorkspace,
		},
		Identity: stagingIdentity{
			RequestID: "req-matrix", InvocationID: "inv-1",
			CandidateInstanceID: "cand-1", CandidateHash: w.hash,
			TargetPath: w.path, BaselineIdentity: baselineIdentityFor(w.ctx, w.path),
			WorkspaceGeneration: generation, WorkspaceStateHash: stateHash,
		},
	}
}

func TestEvidenceMatrix(t *testing.T) {
	const secretCode = "TOKEN = 'hunter2'\nprint(7)\n"

	t.Run("syntax-only structured task", func(t *testing.T) {
		w := newMatrixWorld(t,
			`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
			"solve.py", secretCode, true)
		obs := w.obligations()
		var syn taskObligation
		for _, o := range obs {
			if o.Kind == ObligationSyntacticValidity {
				syn = o
			}
		}
		if syn.ID == "" {
			t.Fatalf("no syntax obligation derived: %v", obs)
		}
		ev, ok := produceSyntaxEvidence(w.ctx, syntaxEvidenceRequest{
			Obligation: syn, Path: w.path, CandidateBytes: w.code,
			CandidateHash: w.hash,
			Outcome:       fallbackSyntaxOutcomeFor(w.ctx, w.path, w.code).aggregate(),
			InvocationID:  "inv-1", CandidateInstanceID: "cand-1",
		})
		if !ok {
			t.Fatal("a structured syntax task produced no evidence")
		}
		if authorized, why := ev.Authorizes(); !authorized {
			t.Errorf("proxy syntax evidence did not close a syntax obligation: %s", why)
		}
		assertNoLeak(t, "syntax-only", ev, secretCode, "hunter2", "TOKEN")
		assertDirectJoin(t, "syntax-only", ev, "req-matrix", "inv-1", "cand-1", w.hash)
	})

	t.Run("behavioral requirement on a syntax-only producer", func(t *testing.T) {
		w := newMatrixWorld(t,
			`{"task_mode":"work","verification_knowledge":"declared","verification":["pytest -q"]}`,
			"solve.py", secretCode, true)
		var cmd taskObligation
		for _, o := range w.obligations() {
			if o.Kind == ObligationDeclaredCommand {
				cmd = o
			}
		}
		if cmd.ID == "" {
			t.Fatal("no declared-command obligation derived")
		}
		// The structural gate cannot speak for it.
		if _, ok := produceSyntaxEvidence(w.ctx, syntaxEvidenceRequest{
			Obligation: cmd, Path: w.path, CandidateBytes: w.code,
			CandidateHash: w.hash,
			Outcome:       fallbackSyntaxOutcomeFor(w.ctx, w.path, w.code).aggregate(),
			InvocationID:  "inv-1", CandidateInstanceID: "cand-1",
		}); ok {
			t.Error("the syntax producer described a behavioural obligation")
		}
		// And nothing ran it, so the obligation is still owed.
		_, missing := declaredVerificationCoverage([]taskObligation{cmd}, nil)
		if len(missing) != 1 {
			t.Error("an unrun declared command was not reported as owed")
		}
	})

	t.Run("one declared command", func(t *testing.T) {
		w := newMatrixWorld(t,
			`{"task_mode":"work","verification_knowledge":"declared","verification":["python3 solve.py"]}`,
			"solve.py", secretCode, true)
		var cmd taskObligation
		for _, o := range w.obligations() {
			if o.Kind == ObligationDeclaredCommand {
				cmd = o
			}
		}
		ev, ok := produceDeclaredVerificationEvidence(w.ctx,
			w.stagedRun(cmd, stagingExitedZero, false, false))
		if !ok {
			t.Fatal("an exact declared command produced no evidence")
		}
		// A command declared without a type is a runtime fact: it ran against
		// these bytes and exited zero. Reaching behavioral takes a typed
		// declaration and assets the client owns.
		if ev.Provenance.ObservedStrength != VerificationKindRuntime {
			t.Errorf("strength %q, want runtime", ev.Provenance.ObservedStrength)
		}
		met, missing := declaredVerificationCoverage([]taskObligation{cmd}, []proxyEvidence{ev})
		if len(met) != 1 || len(missing) != 0 {
			t.Errorf("met %v missing %v, want the one command met", met, missing)
		}
		assertNoLeak(t, "one command", ev, secretCode, "hunter2", "python3 solve.py")
		assertDirectJoin(t, "one command", ev, "req-matrix", "inv-1", "cand-1", w.hash)
	})

	t.Run("multiple declared commands", func(t *testing.T) {
		w := newMatrixWorld(t,
			`{"task_mode":"work","verification_knowledge":"declared",`+
				`"verification":["python3 solve.py","ruff check ."]}`,
			"solve.py", secretCode, true)
		var cmds []taskObligation
		for _, o := range w.obligations() {
			if o.Kind == ObligationDeclaredCommand {
				cmds = append(cmds, o)
			}
		}
		if len(cmds) != 2 {
			t.Fatalf("derived %d command obligations, want 2", len(cmds))
		}
		var evidence []proxyEvidence
		ev, ok := produceDeclaredVerificationEvidence(w.ctx,
			w.stagedRun(cmds[0], stagingExitedZero, false, false))
		if !ok {
			t.Fatal("the first command produced no evidence")
		}
		evidence = append(evidence, ev)
		met, missing := declaredVerificationCoverage(cmds, evidence)
		if len(met) != 1 || len(missing) != 1 {
			t.Errorf("met %v missing %v, want one of each", met, missing)
		}
		assertNoLeak(t, "multiple commands", ev, secretCode, "hunter2",
			cmds[0].Subject, cmds[1].Subject)
	})

	t.Run("stale workspace", func(t *testing.T) {
		w := newMatrixWorld(t,
			`{"task_mode":"work","verification_knowledge":"declared","verification":["python3 solve.py"]}`,
			"solve.py", secretCode, true)
		var cmd taskObligation
		for _, o := range w.obligations() {
			if o.Kind == ObligationDeclaredCommand {
				cmd = o
			}
		}
		req := w.stagedRun(cmd, stagingExitedZero, false, false)
		// The workspace moves while the staged command is running.
		if err := os.WriteFile(w.path, []byte("print(8)\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		bumpWorkspace(w.ctx, w.path, contentSHA256("print(8)\n"))
		liveGen, liveHash := workspaceIdentity(w.ctx)
		if liveHash == req.Identity.WorkspaceStateHash {
			t.Fatal("the workspace did not actually move")
		}

		ev, ok := produceDeclaredVerificationEvidence(w.ctx, req)
		if !ok {
			t.Fatal("the row produced no evidence")
		}
		// The record is stamped with the workspace the staging run was bound
		// to, not with whatever the workspace becomes afterwards. That is what
		// makes staleness DETECTABLE.
		if ev.Provenance.WorkspaceStateHash != req.Identity.WorkspaceStateHash {
			t.Error("the record followed the workspace instead of recording it")
		}
		asked := ev.Provenance
		asked.WorkspaceGeneration, asked.WorkspaceStateHash = liveGen, liveHash
		if ok, _ := ev.Provenance.BindsTo(asked); ok {
			t.Error("evidence from a superseded workspace still binds to the live one")
		}
	})

	t.Run("existing validated baseline", func(t *testing.T) {
		w := newMatrixWorld(t,
			`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
			"solve.py", secretCode, true)
		if w.ctx.Ledger == nil {
			w.ctx.Ledger = map[string]*DeliverableState{}
		}
		w.ctx.Ledger[w.path] = &DeliverableState{
			Path: w.path, CurrentHash: w.hash, Generation: 1,
			ValidationKind: ValidationKindSyntax, ValidationStatus: ValidationPassed,
			ValidatedHash: w.hash,
		}
		var base taskObligation
		for _, o := range w.obligations() {
			if o.Kind == ObligationBaselinePreserved {
				base = o
			}
		}
		if base.ID == "" {
			t.Fatal("a validated baseline owed no preservation")
		}
		if base.RequiredStrength != "syntax" {
			t.Errorf("baseline floor %q, want the syntax it has", base.RequiredStrength)
		}
		// No producer here owns preservation, so nothing may speak for it.
		if _, ok := produceSyntaxEvidence(w.ctx, syntaxEvidenceRequest{
			Obligation: base, Path: w.path, CandidateBytes: w.code,
			CandidateHash: w.hash,
			Outcome:       fallbackSyntaxOutcomeFor(w.ctx, w.path, w.code).aggregate(),
			InvocationID:  "inv-1", CandidateInstanceID: "cand-1",
		}); ok {
			t.Error("the syntax producer claimed a baseline survived")
		}
		// The identity a replacement would be bound against names no bytes.
		id := baselineIdentityFor(w.ctx, w.path)
		if id == "" {
			t.Error("a validated baseline has no identity")
		}
		if strings.Contains(id, "hunter2") || strings.Contains(id, secretCode) {
			t.Errorf("baseline identity %q carries content", id)
		}
	})

	t.Run("model-generated self-test", func(t *testing.T) {
		// The client declared nothing. The model wrote its own test and ran it
		// green against the exact bytes.
		w := newMatrixWorld(t, `{"task_mode":"work"}`, "solve.py", secretCode, true)
		obl, ok := newTaskObligation(ObligationDeclaredCommand, "python3 test_solve.py",
			VerificationKindRuntime, true)
		if !ok {
			t.Fatal("obligation refused")
		}
		if _, ok := produceDeclaredVerificationEvidence(w.ctx,
			w.stagedRun(obl, stagingExitedZero, false, false)); ok {
			t.Error("a model-generated self-test produced trusted evidence")
		}
		if len(w.obligations()) != 0 {
			t.Error("a contractless request produced structured obligations")
		}
	})

	t.Run("unsupported artifact", func(t *testing.T) {
		// A class the structural gate does not govern. The obligation still
		// exists for existence; nothing can speak for its structure.
		w := newMatrixWorld(t,
			`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["notes.md"]}`,
			"notes.md", "# notes\n", true)
		obs := w.obligations()
		for _, o := range obs {
			if o.Kind == ObligationSyntacticValidity {
				t.Error("a class the gate does not govern owed syntax")
			}
		}
		unsup, ok := newTaskObligation(ObligationUnsupported, "something we cannot name", "", true)
		if !ok {
			t.Fatal("an unsupported obligation could not be represented")
		}
		if _, ok := produceSyntaxEvidence(w.ctx, syntaxEvidenceRequest{
			Obligation: unsup, Path: w.path, CandidateBytes: w.code,
			CandidateHash: w.hash,
			Outcome:       fallbackSyntaxOutcomeFor(w.ctx, w.path, w.code).aggregate(),
			InvocationID:  "inv-1", CandidateInstanceID: "cand-1",
		}); ok {
			t.Error("a producer spoke for an unsupported obligation")
		}
		if got := authorizationFloor([]taskObligation{unsup}); got != "oracle" {
			t.Errorf("an unsupported obligation left the floor reachable at %q", got)
		}
	})
}

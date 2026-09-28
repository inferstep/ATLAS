package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// The observe-only feasibility matrix, and the Stage-A replay it exists for.

func feasibilityFor(t *testing.T, contract string, files map[string]string) FeasibilityDecision {
	t.Helper()
	ctx, obs := rolesFixture(t, contract, files)
	_ = ctx
	return decideInvocationFeasibility(feasibilityInput{
		Obligations:       obs,
		AuthorizedTargets: authorizedTargets(obs),
		Producible:        producibleStrengths(),
	})
}

func expectFeasibility(t *testing.T, row string, d FeasibilityDecision,
	feasible bool, want FeasibilityReason) {
	t.Helper()
	if d.Feasible != feasible {
		t.Errorf("%s: feasible=%v, want %v (reason %q, unreachable %v)",
			row, d.Feasible, feasible, d.Reason, d.Unreachable)
	}
	if d.Reason != want {
		t.Errorf("%s: reason %q, want %q", row, d.Reason, want)
	}
	if !feasibilityReasons[d.Reason] {
		t.Errorf("%s: %q is outside the closed vocabulary", row, d.Reason)
	}
	if d.InfluencesLiveDecision {
		t.Errorf("%s: the decision claims to influence a live one", row)
	}
}

func TestFeasibilityMatrix(t *testing.T) {
	t.Run("declared code output", func(t *testing.T) {
		// The proxy's own gate is wired and reaches the syntax floor.
		d := feasibilityFor(t,
			`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`, nil)
		expectFeasibility(t, "code output", d, true, FeasibilityClosurePathAvailable)
		if d.Floor != "syntax" {
			t.Errorf("floor %q", d.Floor)
		}
		if len(d.Unreachable) != 0 {
			t.Errorf("unreachable %v", d.Unreachable)
		}
	})

	t.Run("declared document with no verification", func(t *testing.T) {
		// A target and nothing to demonstrate about it. Naming a path is not
		// a closure path.
		d := feasibilityFor(t,
			`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["notes.md"]}`, nil)
		expectFeasibility(t, "document", d, false, FeasibilityNoTrustedSource)
		if d.Floor != "" {
			t.Errorf("floor %q, want none stated", d.Floor)
		}
	})

	t.Run("declared command with a staging producer", func(t *testing.T) {
		d := feasibilityFor(t,
			`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
				`"verification_knowledge":"declared","verification":["pytest -q"]}`, nil)
		expectFeasibility(t, "declared command", d, true, FeasibilityClosurePathAvailable)
		if len(d.Unreachable) != 0 {
			t.Errorf("unreachable %v, want nothing owed with no path", d.Unreachable)
		}
	})

	t.Run("several declared commands, all producible", func(t *testing.T) {
		d := feasibilityFor(t,
			`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
				`"verification_knowledge":"declared","verification":["pytest -q","ruff check ."]}`, nil)
		expectFeasibility(t, "two commands", d, true, FeasibilityClosurePathAvailable)
	})

	t.Run("syntax baseline is reachable from what syntax evidence produces", func(t *testing.T) {
		ctx, _ := rolesFixture(t, "", map[string]string{"solve.py": "print(7)\n"})
		resolved := resolveAgentPath(ctx, "solve.py")
		disk := fileSHA256(ctx, resolved)
		ctx.LedgerMu.Lock()
		if ctx.Ledger == nil {
			ctx.Ledger = map[string]*DeliverableState{}
		}
		ctx.Ledger[resolved] = &DeliverableState{
			Path: resolved, CurrentHash: disk, Generation: 1,
			ValidationKind: ValidationKindSyntax, ValidationStatus: ValidationPassed,
			ValidatedHash: disk,
		}
		ctx.LedgerMu.Unlock()
		ctx.TaskContract = mustContract(t, ctx.WorkingDir,
			`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`)
		obs := requestObligations(ctx)
		d := decideInvocationFeasibility(feasibilityInput{
			Obligations:       obs,
			AuthorizedTargets: authorizedTargets(obs),
			Producible:        producibleStrengths(),
		})
		// Preservation has no producer and never will. It is reachable because
		// the syntax evidence that speaks for the candidate reaches the same
		// strength the baseline holds -- the same rule the authorization
		// decision applies afterwards.
		expectFeasibility(t, "syntax baseline", d, true, FeasibilityClosurePathAvailable)
	})

	t.Run("behavioural baseline is unreachable without behavioural evidence", func(t *testing.T) {
		ctx, _ := rolesFixture(t, "", map[string]string{"solve.py": "print(7)\n"})
		resolved := resolveAgentPath(ctx, "solve.py")
		ctx.VerificationEvidence = append(ctx.VerificationEvidence, VerificationRecord{
			Command: "python3 solve.py",
			Covered: map[string]string{resolved: fileSHA256(ctx, resolved)}, Turn: 1,
		})
		ctx.TaskContract = mustContract(t, ctx.WorkingDir,
			`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`)
		obs := requestObligations(ctx)
		in := feasibilityInput{
			Obligations:       obs,
			AuthorizedTargets: authorizedTargets(obs),
			Producible:        map[string]string{ObligationSyntacticValidity: "syntax"},
		}
		d := decideInvocationFeasibility(in)
		expectFeasibility(t, "behavioural baseline, syntax only", d, false,
			FeasibilityBaselineFloorUnreachable)

		// With behavioural evidence producible, the same floor is reachable.
		in.Producible = producibleStrengths()
		expectFeasibility(t, "behavioural baseline, staging wired",
			decideInvocationFeasibility(in), true, FeasibilityClosurePathAvailable)
	})

	t.Run("declared command on a build without staging", func(t *testing.T) {
		_, obs := rolesFixture(t,
			`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
				`"verification_knowledge":"declared","verification":["pytest -q"]}`, nil)
		d := decideInvocationFeasibility(feasibilityInput{
			Obligations:       obs,
			AuthorizedTargets: authorizedTargets(obs),
			Producible:        map[string]string{ObligationSyntacticValidity: "syntax"},
		})
		expectFeasibility(t, "declared command", d, false, FeasibilityAdapterCannotMeasure)
		if len(d.Unreachable) != 1 ||
			!strings.HasPrefix(d.Unreachable[0], ObligationDeclaredCommand+":") {
			t.Errorf("unreachable %v, want the declared command", d.Unreachable)
		}
	})

	t.Run("existing validated baseline", func(t *testing.T) {
		ctx, _ := rolesFixture(t, "", map[string]string{"solve.py": "print(7)\n"})
		resolved := resolveAgentPath(ctx, "solve.py")
		disk := fileSHA256(ctx, resolved)
		ctx.VerificationEvidence = append(ctx.VerificationEvidence, VerificationRecord{
			Command: "python3 solve.py",
			Covered: map[string]string{resolved: disk}, Turn: 1,
		})
		ctx.TaskContract = mustContract(t, ctx.WorkingDir,
			`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`)
		obs := requestObligations(ctx)
		d := decideInvocationFeasibility(feasibilityInput{
			Obligations:       obs,
			AuthorizedTargets: authorizedTargets(obs),
			Producible:        producibleStrengths(),
		})
		// The floor is the baseline's, and staging can reach it. Preservation
		// itself is still derived rather than produced -- what changed is that
		// something producible now gets that high.
		if d.Floor != "behavioral" {
			t.Errorf("floor %q, want the baseline's behavioral", d.Floor)
		}
		expectFeasibility(t, "behavioral baseline", d, true,
			FeasibilityClosurePathAvailable)
	})

	t.Run("unsupported obligation", func(t *testing.T) {
		exists, _ := newTaskObligation(ObligationArtifactExists, "a.py", "", true)
		unsup, _ := newTaskObligation(ObligationUnsupported, "a thing", "", true)
		d := decideInvocationFeasibility(feasibilityInput{
			Obligations:       []taskObligation{exists, unsup},
			AuthorizedTargets: []string{"a.py"},
			Producible:        producibleStrengths(),
		})
		expectFeasibility(t, "unsupported", d, false, FeasibilityUnsupportedObligation)
	})

	t.Run("unspecified contract", func(t *testing.T) {
		for _, body := range []string{
			"", `{"task_mode":"work"}`,
			`{"task_mode":"work","output_knowledge":"unspecified","verification_knowledge":"unspecified"}`,
		} {
			d := feasibilityFor(t, body, nil)
			expectFeasibility(t, body, d, false, FeasibilityUnspecifiedContract)
			if len(d.Unreachable) != 0 {
				t.Errorf("%q named %v unreachable", body, d.Unreachable)
			}
		}
	})

	t.Run("a prerequisite with no target", func(t *testing.T) {
		// Authoritative none for outputs, a declared command for verification.
		// Even if the command had a producer there would be nothing to deliver.
		cmd, _ := newTaskObligation(ObligationSyntacticValidity, "a.py", "", true)
		d := decideInvocationFeasibility(feasibilityInput{
			Obligations:       []taskObligation{cmd},
			AuthorizedTargets: nil,
			Producible:        producibleStrengths(),
		})
		expectFeasibility(t, "no target", d, false, FeasibilityNoTrustedSource)
	})

	t.Run("unknown obligation kind", func(t *testing.T) {
		forged := taskObligation{ID: "forged:0", Kind: "forged", Subject: "a.py",
			RequiredStrength: "syntax", Required: true}
		exists, _ := newTaskObligation(ObligationArtifactExists, "a.py", "", true)
		d := decideInvocationFeasibility(feasibilityInput{
			Obligations:       []taskObligation{exists, forged},
			AuthorizedTargets: []string{"a.py"},
			Producible:        producibleStrengths(),
		})
		// A kind with no role is not a prerequisite, so there is nothing left
		// to demonstrate and no closure path.
		if d.Feasible {
			t.Errorf("an unclassified obligation was called feasible: %q", d.Reason)
		}
	})
}

// --- a later invocation recomputes; this one does not gain authority ---------

func TestALaterInvocationRecomputesFromWhatIsAvailableThen(t *testing.T) {
	_, obs := rolesFixture(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
			`"verification_knowledge":"declared","verification":["pytest -q"]}`, nil)
	in := feasibilityInput{
		Obligations:       obs,
		AuthorizedTargets: authorizedTargets(obs),
		Producible:        producibleStrengths(),
	}
	now := decideInvocationFeasibility(in)
	expectFeasibility(t, "with the staging producer wired", now, true,
		FeasibilityClosurePathAvailable)

	// The same obligations on a build where the producer is not available. The
	// answer is different, and that is the point: feasibility is a statement
	// about what THIS invocation could produce, recomputed from what exists
	// then, never a property carried forward.
	without := in
	without.Producible = map[string]string{ObligationSyntacticValidity: "syntax"}
	other := decideInvocationFeasibility(without)
	if other.Feasible {
		t.Error("a declared command with no producer was called feasible")
	}
	if !now.Feasible {
		t.Error("the earlier invocation's answer changed")
	}
}

// --- the Stage-A replay ------------------------------------------------------

// TestTheStageARequestShapeHasNoClosurePath replays the shape the sealed
// acquisition actually sent and reaches the same conclusion it took 103
// candidate evaluations to reach, without reading a single outcome.
//
// The historical evidence is untouched: this constructs the REQUEST shape, not
// the run, and asserts nothing about what the run produced.
func TestTheStageARequestShapeHasNoClosurePath(t *testing.T) {
	// The acquisition's own senders declared task_mode and nothing else --
	// pinned by the owned-sender tests and by tests/e2e/conftest.py.
	d := feasibilityFor(t, `{"task_mode":"work"}`, nil)
	expectFeasibility(t, "stage-a shape", d, false, FeasibilityUnspecifiedContract)

	// The counterfactual the run would have needed: a structured Python task
	// whose closure floor is behavioural. At acquisition time its route could
	// not reach that floor at all. On this build it can, because staging runs
	// the declared command against the candidate -- so the same shape that was
	// unreachable then is feasible now.
	structured := feasibilityFor(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
			`"verification_knowledge":"declared","verification":["python3 solve.py"],`+
			`"verification_requirements_version":1,"verification_requirements":[`+
			`{"command":"python3 solve.py","kind":"behavioral","expects":"exit_zero",`+
			`"asset_authority":"client_supplied"}]}`, nil)
	if structured.Floor != "behavioral" {
		t.Errorf("floor %q, want behavioral", structured.Floor)
	}
	expectFeasibility(t, "structured behavioural task", structured, true,
		FeasibilityClosurePathAvailable)
	// What has NOT changed is the shape Stage A actually sent. A contract that
	// declared nothing has nothing to close, and staging cannot fix that: the
	// missing thing is the declaration, not the executor.
	if d.Feasible {
		t.Error("the acquisition's own shape gained a closure path")
	}

	// Quantify what would have been avoided, without changing anything: the
	// acquisition ran 21 tasks x 3 arms, and the sealed record's own count of
	// candidate evaluations under the route that could not close is the number
	// this answer would have skipped had it been consulted.
	const stageACandidateEvaluations = 103
	const underTheUnclosableRoute = 100
	if underTheUnclosableRoute >= stageACandidateEvaluations {
		t.Fatal("the pinned counts are inconsistent")
	}
	t.Logf("observe-only: %d of %d Stage-A candidate evaluations ran under a "+
		"route that, as sent, had no closure path; consulted, this decision "+
		"would have avoided them. Nothing was skipped, and the historical "+
		"record is unchanged.",
		underTheUnclosableRoute, stageACandidateEvaluations)
}

// --- inputs are closed -------------------------------------------------------

// TestFeasibilityReadsNoOutcome enumerates what the decision may not read.
func TestFeasibilityReadsNoOutcome(t *testing.T) {
	const file = "feasibility_decision.go"
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	// The forbidden list itself lives in the file; strip its declaration so
	// naming a symbol in order to ban it does not count as reading it.
	if i := strings.Index(body, "var feasibilityForbiddenInputs"); i >= 0 {
		end := strings.Index(body[i:], "\n}")
		body = body[:i] + body[i+end:]
	}
	for _, banned := range feasibilityForbiddenInputs {
		if strings.Contains(body, banned) {
			t.Errorf("the feasibility owner reads %q", banned)
		}
	}
	if len(feasibilityForbiddenInputs) == 0 {
		t.Fatal("the forbidden-input list is empty, so this guard proves nothing")
	}
}

func TestFeasibilityCallsNothingThatGenerates(t *testing.T) {
	const file = "feasibility_decision.go"
	fset := token.NewFileSet()
	tree, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	banned := map[string]bool{
		"callV3Generate": true, "callV3GenerateStreaming": true,
		"writeFileWithV3": true, "improveContentWithV3": true,
		"proposedV3Candidate": true, "v3DeliveryAuthorized": true,
		"writeFileRecorded": true, "finalizeCompletion": true,
		"WriteFile": true, "StreamFn": true,
	}
	ast.Inspect(tree, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		if banned[name] {
			t.Errorf("the feasibility owner calls %s", name)
		}
		return true
	})
}

func TestTheFeasibilityRecordCarriesNoContent(t *testing.T) {
	d := feasibilityFor(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
			`"verification_knowledge":"declared","verification":["pytest --token=hunter2"]}`, nil)
	blob, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"hunter2", "pytest", "solve.py"} {
		if strings.Contains(string(blob), needle) {
			t.Errorf("the feasibility answer carries %q", needle)
		}
	}
}

// TestTheTwoBaselineDerivationsAgree pins the pair that must not drift.
//
// Feasibility says before generation whether a baseline COULD be preserved;
// authorization says afterwards whether it WAS. They apply the same strength
// rule, in two files, and a build where one says unreachable and the other
// preserves anyway is a build whose two answers are about different systems.
func TestTheTwoBaselineDerivationsAgree(t *testing.T) {
	const witness = "command-identity"
	for _, required := range []string{"syntax", "behavioral", "oracle"} {
		o, ok := newTaskObligation(ObligationBaselinePreserved, "/w/solve.py", required, true)
		if !ok {
			t.Fatalf("%s baseline obligation refused", required)
		}
		for _, producible := range []map[string]string{
			{},
			{ObligationSyntacticValidity: "syntax"},
			{ObligationSyntacticValidity: "syntax", ObligationDeclaredCommand: "behavioral"},
			{ObligationDeclaredExample: "oracle"},
		} {
			// The strongest thing this build could put in front of the
			// authorization decision, named by the baseline's own witness so
			// the command check cannot be what decides it.
			var evidence []proxyEvidence
			for _, strength := range producible {
				evidence = append(evidence, proxyEvidence{
					Outcome: ValidationPassed,
					Provenance: V3EvidenceProvenance{
						ObservedStrength: strength, CommandIdentity: witness,
					},
				})
			}
			before := baselineFloorReachable(required, producible)
			after, _ := baselinePreservedBy(o, witness, evidence)
			if before != after {
				t.Errorf("%s baseline with %v: feasibility says %v, authorization says %v",
					required, producible, before, after)
			}
		}
	}
}

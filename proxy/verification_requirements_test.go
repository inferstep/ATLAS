package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Strength comes from what the client TYPED and from nothing else.
//
// Every row here is a pair: the same command, declared two ways, reaching two
// different floors. If any of them ever agreed, something would be reading the
// command instead of the declaration.

func typedCommandObligation(t *testing.T, w *matrixWorld) taskObligation {
	t.Helper()
	for _, o := range w.obligations() {
		if o.Kind == ObligationDeclaredCommand {
			return o
		}
	}
	t.Fatal("no declared-command obligation derived")
	return taskObligation{}
}

func TestDeclaredStrengthComesFromTheDeclaration(t *testing.T) {
	const code = "print(7)\n"
	// The identical command text, four declarations.
	for _, tc := range []struct {
		name     string
		contract string
		want     string
	}{
		{
			name: "untyped list is runtime",
			contract: `{"task_mode":"work","verification_knowledge":"declared",` +
				`"verification":["pytest -q"]}`,
			want: VerificationKindRuntime,
		},
		{
			name: "typed syntax is syntax",
			contract: `{"task_mode":"work","verification_knowledge":"declared",` +
				`"verification_requirements_version":1,"verification_requirements":[` +
				`{"command":"pytest -q","kind":"syntax","expects":"exit_zero",` +
				`"asset_authority":"client_supplied"}]}`,
			want: VerificationKindSyntax,
		},
		{
			name: "typed runtime is runtime",
			contract: `{"task_mode":"work","verification_knowledge":"declared",` +
				`"verification_requirements_version":1,"verification_requirements":[` +
				`{"command":"pytest -q","kind":"runtime","expects":"exit_zero",` +
				`"asset_authority":"client_supplied"}]}`,
			want: VerificationKindRuntime,
		},
		{
			name: "typed behavioral is behavioral",
			contract: `{"task_mode":"work","verification_knowledge":"declared",` +
				`"verification_requirements_version":1,"verification_requirements":[` +
				`{"command":"pytest -q","kind":"behavioral","expects":"exit_zero",` +
				`"asset_authority":"client_supplied"}]}`,
			want: VerificationKindBehavioral,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newMatrixWorld(t, tc.contract, "solve.py", code, true)
			o := typedCommandObligation(t, w)
			if o.RequiredStrength != tc.want {
				t.Errorf("required strength %q, want %q", o.RequiredStrength, tc.want)
			}
			if got := authorizationFloor(w.obligations()); got != tc.want {
				t.Errorf("floor %q, want %q", got, tc.want)
			}
		})
	}
}

// A command that looks like a test suite and one that looks like a parse check
// reach the same floor when they are declared the same way. Shape decides
// nothing.
func TestCommandTextDoesNotDecideStrength(t *testing.T) {
	const code = "print(7)\n"
	for _, cmd := range []string{
		"pytest -q tests/",
		`python3 -c "import ast;ast.parse(open('solve.py').read())"`,
		"true",
	} {
		w := newMatrixWorld(t, `{"task_mode":"work","verification_knowledge":"declared",`+
			`"verification":[`+mustJSONString(cmd)+`]}`, "solve.py", code, true)
		o := typedCommandObligation(t, w)
		if o.RequiredStrength != VerificationKindRuntime {
			t.Errorf("%q reached %q untyped, want runtime", cmd, o.RequiredStrength)
		}
	}
}

// A passing run is recorded at the weaker of what the client declared and what
// its assets can support.
func TestObservedStrengthIsCappedByAssetAuthority(t *testing.T) {
	const code = "print(7)\n"

	t.Run("client-supplied assets support behavioral", func(t *testing.T) {
		w := newMatrixWorld(t, `{"task_mode":"work","verification_knowledge":"declared",`+
			`"verification_requirements_version":1,"verification_requirements":[`+
			`{"command":"python3 solve.py","kind":"behavioral","expects":"exit_zero",`+
			`"asset_authority":"client_supplied"}]}`, "solve.py", code, true)
		o := typedCommandObligation(t, w)
		ev, ok := produceDeclaredVerificationEvidence(w.ctx,
			w.stagedRun(o, stagingExitedZero, false, false))
		if !ok {
			t.Fatal("a typed behavioral command produced no evidence")
		}
		if ev.Provenance.ObservedStrength != VerificationKindBehavioral {
			t.Errorf("observed %q, want behavioral", ev.Provenance.ObservedStrength)
		}
		if authorized, why := ev.Authorizes(); !authorized {
			t.Errorf("behavioral evidence did not close its own obligation: %s", why)
		}
	})

	t.Run("an asset this session wrote caps at runtime", func(t *testing.T) {
		w := newMatrixWorld(t, `{"task_mode":"work","verification_knowledge":"declared",`+
			`"verification_requirements_version":1,"verification_requirements":[`+
			`{"command":"python3 solve.py","kind":"behavioral","expects":"exit_zero",`+
			`"assets":["test_solve.py"],"asset_authority":"client_supplied"}]}`,
			"solve.py", code, true)
		// The model wrote the file the command is measured against. The
		// contract's claim about it is not the fact about it.
		asset := resolveAgentPath(w.ctx, "test_solve.py")
		if w.ctx.Ledger == nil {
			w.ctx.Ledger = map[string]*DeliverableState{}
		}
		w.ctx.Ledger[ledgerKey(w.ctx, asset)] = &DeliverableState{
			Path: asset, CurrentHash: contentSHA256("assert True\n"), Generation: 1,
		}
		o := typedCommandObligation(t, w)
		ev, ok := produceDeclaredVerificationEvidence(w.ctx,
			w.stagedRun(o, stagingExitedZero, false, false))
		if !ok {
			t.Fatal("the run produced no evidence at all")
		}
		if ev.Provenance.ObservedStrength != VerificationKindRuntime {
			t.Errorf("observed %q, want runtime", ev.Provenance.ObservedStrength)
		}
		// And it does not reach the floor the client declared, so it cannot
		// authorize. A request may not write its own oracle.
		if authorized, _ := ev.Authorizes(); authorized {
			t.Error("a self-authored oracle produced behavioral authority")
		}
	})
}

// The two lists are independent, and a command in both is answered by its
// typed declaration.
func TestTypedAndUntypedCommandsCoexist(t *testing.T) {
	w := newMatrixWorld(t, `{"task_mode":"work","verification_knowledge":"declared",`+
		`"verification":["ruff check .","python3 solve.py"],`+
		`"verification_requirements_version":1,"verification_requirements":[`+
		`{"command":"python3 solve.py","kind":"behavioral","expects":"exit_zero",`+
		`"asset_authority":"client_supplied"}]}`, "solve.py", "print(7)\n", true)
	strengths := map[string]string{}
	for _, o := range w.obligations() {
		if o.Kind == ObligationDeclaredCommand {
			strengths[o.Subject] = o.RequiredStrength
		}
	}
	if len(strengths) != 2 {
		t.Fatalf("derived %d command obligations, want 2: %v", len(strengths), strengths)
	}
	if strengths["python3 solve.py"] != VerificationKindBehavioral {
		t.Errorf("the typed command reached %q", strengths["python3 solve.py"])
	}
	if strengths["ruff check ."] != VerificationKindRuntime {
		t.Errorf("the untyped command reached %q", strengths["ruff check ."])
	}
	// Both are owed: the floor is the strongest of them.
	if got := authorizationFloor(w.obligations()); got != VerificationKindBehavioral {
		t.Errorf("floor %q, want behavioral", got)
	}
}

// The boundary refuses anything it cannot read, rather than defaulting it.
func TestTypedVerificationBoundaryRefusesTheUnreadable(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ name, contract string }{
		{"unknown kind", `{"task_mode":"work","verification_knowledge":"declared",` +
			`"verification_requirements_version":1,"verification_requirements":[` +
			`{"command":"x","kind":"semantic","expects":"exit_zero",` +
			`"asset_authority":"client_supplied"}]}`},
		{"oracle is not declarable", `{"task_mode":"work","verification_knowledge":"declared",` +
			`"verification_requirements_version":1,"verification_requirements":[` +
			`{"command":"x","kind":"oracle","expects":"exit_zero",` +
			`"asset_authority":"client_supplied"}]}`},
		{"unknown expectation", `{"task_mode":"work","verification_knowledge":"declared",` +
			`"verification_requirements_version":1,"verification_requirements":[` +
			`{"command":"x","kind":"runtime","expects":"prints_ok",` +
			`"asset_authority":"client_supplied"}]}`},
		{"unknown asset authority", `{"task_mode":"work","verification_knowledge":"declared",` +
			`"verification_requirements_version":1,"verification_requirements":[` +
			`{"command":"x","kind":"runtime","expects":"exit_zero",` +
			`"asset_authority":"trust_me"}]}`},
		{"unknown version", `{"task_mode":"work","verification_knowledge":"declared",` +
			`"verification_requirements_version":2,"verification_requirements":[` +
			`{"command":"x","kind":"runtime","expects":"exit_zero",` +
			`"asset_authority":"client_supplied"}]}`},
		{"no version at all", `{"task_mode":"work","verification_knowledge":"declared",` +
			`"verification_requirements":[` +
			`{"command":"x","kind":"runtime","expects":"exit_zero",` +
			`"asset_authority":"client_supplied"}]}`},
		{"version without requirements", `{"task_mode":"work",` +
			`"verification_requirements_version":1}`},
		{"empty command", `{"task_mode":"work","verification_knowledge":"declared",` +
			`"verification_requirements_version":1,"verification_requirements":[` +
			`{"command":"  ","kind":"runtime","expects":"exit_zero",` +
			`"asset_authority":"client_supplied"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var in TaskContract
			if err := json.Unmarshal([]byte(tc.contract), &in); err != nil {
				t.Fatal(err)
			}
			if _, err := validateTaskContract(&in, dir); err == nil {
				t.Fatal("the boundary accepted a contract it cannot read")
			}
		})
	}
}

// A question declares nothing, and typed requirements are not a way in.
func TestQuestionModeCannotDeclareTypedVerification(t *testing.T) {
	var in TaskContract
	if err := json.Unmarshal([]byte(`{"task_mode":"question",`+
		`"verification_requirements_version":1,"verification_requirements":[`+
		`{"command":"pytest -q","kind":"behavioral","expects":"exit_zero",`+
		`"asset_authority":"client_supplied"}]}`), &in); err != nil {
		t.Fatal(err)
	}
	if _, err := validateTaskContract(&in, t.TempDir()); err == nil {
		t.Fatal("question mode declared a verification obligation")
	}
}

// The declaration is metadata about a command, and none of it travels into
// evidence: no command text, no asset path, no kind prose beyond the closed
// strength value.
func TestTypedRequirementCarriesNoContentIntoEvidence(t *testing.T) {
	w := newMatrixWorld(t, `{"task_mode":"work","verification_knowledge":"declared",`+
		`"verification_requirements_version":1,"verification_requirements":[`+
		`{"command":"python3 solve.py --secret hunter2","kind":"behavioral",`+
		`"expects":"exit_zero","assets":["fixtures/golden_hunter2.json"],`+
		`"asset_authority":"client_supplied"}]}`, "solve.py", "print(7)\n", true)
	o := typedCommandObligation(t, w)
	ev, ok := produceDeclaredVerificationEvidence(w.ctx,
		w.stagedRun(o, stagingExitedZero, false, false))
	if !ok {
		t.Fatal("no evidence produced")
	}
	blob, err := json.Marshal(ev.Provenance)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"hunter2", "golden_hunter2.json", "--secret"} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("the evidence carries %q", secret)
		}
	}
}

// Nothing but a passing run produces authority, whatever the declaration says.
func TestTypedBehavioralStillNeedsACleanRun(t *testing.T) {
	contract := `{"task_mode":"work","verification_knowledge":"declared",` +
		`"verification_requirements_version":1,"verification_requirements":[` +
		`{"command":"python3 solve.py","kind":"behavioral","expects":"exit_zero",` +
		`"asset_authority":"client_supplied"}]}`
	for _, tc := range []struct {
		name             string
		outcome          stagingCommandOutcome
		mutatedTarget    bool
		mutatedWorkspace bool
	}{
		{"non-zero exit", stagingExitedNonZero, false, false},
		{"timeout", stagingTimedOut, false, false},
		{"budget exceeded", stagingBudgetExceeded, false, false},
		{"cancelled", stagingCancelled, false, false},
		{"refused", stagingRefused, false, false},
		{"unavailable", stagingUnavailable, false, false},
		{"unobservable", stagingUnobservable, false, false},
		{"mutated the candidate", stagingExitedZero, true, false},
		{"mutated the workspace", stagingExitedZero, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newMatrixWorld(t, contract, "solve.py", "print(7)\n", true)
			o := typedCommandObligation(t, w)
			ev, ok := produceDeclaredVerificationEvidence(w.ctx,
				w.stagedRun(o, tc.outcome, tc.mutatedTarget, tc.mutatedWorkspace))
			if !ok {
				return // no record at all is the strongest form of no authority
			}
			if ev.Outcome == ValidationPassed {
				t.Errorf("%s was recorded as a pass", tc.name)
			}
			if authorized, _ := ev.Authorizes(); authorized {
				t.Errorf("%s produced authority", tc.name)
			}
		})
	}
}

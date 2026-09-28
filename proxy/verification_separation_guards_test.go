package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// Structural guards for the separation. Each one is about the SHAPE of the
// decision, not about one outcome, so a later change that quietly re-merges
// the two obligations fails here rather than in a canary six weeks later.

// guardNames is every identifier and selector the file's CODE mentions.
// Comments are excluded on purpose: a guard that fires on the sentence
// explaining why something is banned is a guard nobody can write honestly.
func guardNames(t *testing.T, file string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	tree, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	ast.Inspect(tree, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.Ident:
			out[e.Name] = true
		case *ast.SelectorExpr:
			out[e.Sel.Name] = true
		}
		return true
	})
	return out
}

func guardCalls(t *testing.T, file, fn string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	tree, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, d := range tree.Decls {
		f, ok := d.(*ast.FuncDecl)
		if !ok || f.Name.Name != fn {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch e := call.Fun.(type) {
			case *ast.Ident:
				out[e.Name] = true
			case *ast.SelectorExpr:
				out[e.Sel.Name] = true
			}
			return true
		})
	}
	if len(out) == 0 {
		t.Fatalf("%s not found in %s", fn, file)
	}
	return out
}

func TestGuardNoShellOrProsePathParserWasAdded(t *testing.T) {
	// commandNamesPath is the one place a path is read off a command line, it
	// predates this work, and nothing in the new decision may call it or
	// re-implement it.
	for _, fn := range []string{"pathCoverageSatisfied", "commandObligationSatisfied",
		"commandEvidenceCurrent", "stagedCommandSatisfied", "stagedCoverageSatisfied",
		"stagedFulfillmentCurrent", "recordStagedCommandFulfillment"} {
		file := "guardrails.go"
		if strings.HasPrefix(fn, "staged") || strings.HasPrefix(fn, "record") {
			file = "staged_command_evidence.go"
		}
		calls := guardCalls(t, file, fn)
		for _, banned := range []string{"commandNamesPath", "Fields", "Split",
			"Contains", "HasPrefix", "HasSuffix", "Trim", "TrimSpace", "Index"} {
			if calls[banned] {
				t.Errorf("%s calls %s — that is shell text being read for a decision", fn, banned)
			}
		}
	}
	names := guardNames(t, "staged_command_evidence.go")
	for _, banned := range []string{"strings", "filepath", "regexp", "commandNamesPath"} {
		if names[banned] {
			t.Errorf("the staged consumer uses %s; identities are hashes, not prose", banned)
		}
	}
}

func TestGuardCommandAndCoverageAreDistinctPredicates(t *testing.T) {
	cover := guardCalls(t, "guardrails.go", "pathCoverageSatisfied")
	command := guardCalls(t, "guardrails.go", "commandObligationSatisfied")
	if cover["commandObligationSatisfied"] || cover["commandEvidenceCurrent"] {
		t.Error("path coverage consults the command predicate")
	}
	if command["pathCoverageSatisfied"] {
		t.Error("the command predicate consults path coverage")
	}
	// Coverage reads Covered, through the replay that lets a later failure
	// take back an earlier pass; the command predicate must not be able to.
	replay := guardCalls(t, "guardrails.go", "coverageRecord")
	if !cover["coverageRecord"] || !replay["evidenceIsCurrent"] {
		t.Error("path coverage no longer reads the coverage map")
	}
	if replay["commandObligationSatisfied"] || replay["commandEvidenceCurrent"] {
		t.Error("the coverage replay consults the command predicate")
	}
	demand := guardCalls(t, "guardrails.go", "decideVerificationDemand")
	if !demand["pathCoverageSatisfied"] || !demand["commandObligationSatisfied"] {
		t.Error("the demand no longer evaluates both sets")
	}
}

func TestGuardVerificationNeverRequiresSettlement(t *testing.T) {
	banned := map[string]bool{
		"deliverySettlementFor": true, "settleExistence": true,
		"postDeliverySettlementOwed": true, "settlementStatus": true,
		"recordDeliverySettlement": true,
	}
	for file, fns := range map[string][]string{
		"guardrails.go": {"decideVerificationDemand", "pathCoverageSatisfied",
			"commandObligationSatisfied", "commandEvidenceCurrent"},
		"staged_command_evidence.go": {"stagedCommandSatisfied", "stagedCoverageSatisfied",
			"stagedFulfillmentCurrent", "recordStagedCommandFulfillment"},
	} {
		for _, fn := range fns {
			for name := range guardCalls(t, file, fn) {
				if banned[name] {
					t.Errorf("%s calls %s — verification would wait on settlement, "+
						"which waits on verification", fn, name)
				}
			}
		}
	}
}

func TestGuardSettlementStaysAfterVerificationAndStaysMandatory(t *testing.T) {
	body, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	i := strings.Index(s, "func finalizeCompletion(")
	if i < 0 {
		t.Fatal("finalizeCompletion is gone")
	}
	fn := s[i:]
	if j := strings.Index(fn[1:], "\nfunc "); j > 0 {
		fn = fn[:j]
	}
	verification := strings.Index(fn, "decideVerificationDemand(")
	settlement := strings.Index(fn, "postDeliverySettlementOwed(")
	if verification < 0 || settlement < 0 {
		t.Fatal("the completion order lost one of its two gates")
	}
	if verification > settlement {
		t.Error("settlement is now decided before verification")
	}
	if !strings.Contains(fn, `"post_delivery_settlement_pending"`) {
		t.Error("an unsettled delivery no longer blocks completion")
	}
}

func TestGuardSuccessfulExecutionAloneIsNeverArtifactProof(t *testing.T) {
	// The coverage predicate must compare a hash. A version that returned true
	// on "some green record exists" would pass every other test here.
	body, err := os.ReadFile("guardrails.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	i := strings.Index(s, "func coverageRecord(")
	fn := s[i:]
	if j := strings.Index(fn[1:], "\nfunc "); j > 0 {
		fn = fn[:j]
	}
	if !strings.Contains(fn, "covered[path] == hash") {
		t.Error("path coverage no longer binds a path to its bytes")
	}
	staged, err := os.ReadFile("staged_command_evidence.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(staged), "f.CandidateHash == hash") {
		t.Error("staged coverage no longer binds the target to the exact bytes")
	}
}

func TestGuardStagedEvidenceIsNotDowngradedIntoTheWeakerRecord(t *testing.T) {
	names := guardNames(t, "staged_command_evidence.go")
	for _, banned := range []string{"VerificationRecord", "VerificationEvidence",
		"evidenceIsCurrent", "commandEvidenceCurrent"} {
		if names[banned] {
			t.Errorf("the staged consumer touches %s — its evidence is being "+
				"downgraded into the weaker record", banned)
		}
	}
	// And the strong store is its own field, not a second list of the weak one.
	if !guardNames(t, "types.go")["stagedCommandFulfillment"] {
		t.Error("staged fulfillments no longer have their own typed store")
	}
}

func TestGuardNoModelOwnedFieldCanCreateOrWeakenAnObligation(t *testing.T) {
	names := guardNames(t, "staged_command_evidence.go")
	for _, banned := range []string{
		"V3GenerateResponse", "WinningScore", "PhaseSolved", "CandidatesTested",
		"HumanTask", "latestUserMessage", "Envelope", "Selection", "SelectedCandidateID",
	} {
		if names[banned] {
			t.Errorf("the staged consumer reads %s, which the model or the service owns", banned)
		}
	}
	// The obligation identity it matches on comes from the one owner, derived
	// from the client's own declared text.
	if !names["obligationID"] || !names["ObligationDeclaredCommand"] {
		t.Error("declared-command identity is no longer derived from the obligation owner")
	}
	// And the list of declared commands still comes from the request's own
	// obligations, never from anything the service returned.
	wiring := guardCalls(t, "evidence_wiring.go", "observeCandidateVerification")
	if wiring["recordStagedCommandFulfillment"] && !wiring["requestObligations"] {
		t.Error("staged fulfillment is recorded outside the request's obligation set")
	}
}

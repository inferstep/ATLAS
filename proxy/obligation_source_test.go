package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// One owner decides where an obligation came from.
//
// Two sources can name the files a run must produce, and merging them is what
// broke the fifty-task benchmark: the prose heuristic reads a seventy-character
// window before a filename, so "Write solve.py that reads input.txt" made the
// INPUT file a deliverable. A path the session only reads owns nothing and can
// never be demonstrated, so every one of those runs was refused completion
// before any verification, action-demand, hazard or debt logic was consulted.
//
// The rule is not "prefer the contract". It is: a caller that SAYS it knows is
// the authority, and a caller that says nothing gets the heuristic unchanged.
// The two are alternatives, never a union, and the source travels with the
// decision so a later change cannot quietly re-merge them.

func ctxWith(tc *TaskContract) *AgentContext {
	c := NewAgentContext("/tmp", Tier2Medium)
	c.TaskContract = tc
	return c
}

func declaredOutputs(paths ...string) *TaskContract {
	p := append([]string{}, paths...)
	return &TaskContract{TaskMode: TaskModeWork,
		OutputKnowledge: KnowledgeDeclared, ExpectedOutputs: &p,
		VerificationKnowledge: KnowledgeUnspecified}
}

func declaredVerification(cmds ...string) *TaskContract {
	c := append([]string{}, cmds...)
	return &TaskContract{TaskMode: TaskModeWork,
		OutputKnowledge:       KnowledgeUnspecified,
		VerificationKnowledge: KnowledgeDeclared, Verification: &c}
}

// --- outputs ----------------------------------------------------------------

const proseNamesTwo = "Write solve.py that reads input.txt"

func TestNoContractKeepsTheLegacyHeuristicExactly(t *testing.T) {
	d := resolveOutputObligation(ctxWith(nil), proseNamesTwo)
	if d.Source != ObligationSourceLegacy {
		t.Fatalf("source=%q", d.Source)
	}
	if d.KnowledgeSpecified {
		t.Error("a missing contract claimed specified knowledge")
	}
	want := expectedOutputPaths(proseNamesTwo)
	if strings.Join(d.Items, "|") != strings.Join(want, "|") {
		t.Fatalf("legacy set changed: %v want %v", d.Items, want)
	}
}

func TestUnspecifiedKnowledgeKeepsTheLegacyHeuristicExactly(t *testing.T) {
	tc := &TaskContract{TaskMode: TaskModeWork,
		OutputKnowledge: KnowledgeUnspecified, VerificationKnowledge: KnowledgeUnspecified}
	d := resolveOutputObligation(ctxWith(tc), proseNamesTwo)
	if d.Source != ObligationSourceLegacy || d.KnowledgeSpecified {
		t.Fatalf("source=%q specified=%v", d.Source, d.KnowledgeSpecified)
	}
	if strings.Join(d.Items, "|") != strings.Join(expectedOutputPaths(proseNamesTwo), "|") {
		t.Fatalf("legacy set changed: %v", d.Items)
	}
}

func TestDeclaredOutputsAreUsedExactlyAndNeverUnioned(t *testing.T) {
	d := resolveOutputObligation(ctxWith(declaredOutputs("solve.py")), proseNamesTwo)
	if d.Source != ObligationSourceContractDeclared || !d.KnowledgeSpecified {
		t.Fatalf("source=%q specified=%v", d.Source, d.KnowledgeSpecified)
	}
	if strings.Join(d.Items, "|") != "solve.py" {
		t.Fatalf("declared set is %v, want exactly [solve.py]", d.Items)
	}
	// The prose names input.txt too. A union would bring it back.
	for _, p := range d.Items {
		if p == "input.txt" {
			t.Fatal("the prose heuristic was unioned into a declared set")
		}
	}
}

func TestDeclaredEmptyOutputsCreateNoObligation(t *testing.T) {
	d := resolveOutputObligation(ctxWith(declaredOutputs()), proseNamesTwo)
	if d.Source != ObligationSourceContractDeclared || !d.KnowledgeSpecified {
		t.Fatalf("source=%q specified=%v", d.Source, d.KnowledgeSpecified)
	}
	if len(d.Items) != 0 {
		t.Fatalf("authoritative none produced %v", d.Items)
	}
}

// --- verification -----------------------------------------------------------

func TestVerificationKnowledgeIsIndependentOfOutputs(t *testing.T) {
	o := resolveOutputObligation(ctxWith(declaredVerification("pytest")), proseNamesTwo)
	if o.Source != ObligationSourceLegacy {
		t.Errorf("declaring verification changed the OUTPUT source to %q", o.Source)
	}
	v := resolveVerificationObligation(ctxWith(declaredOutputs("a.py")))
	if v.Source != ObligationSourceLegacy {
		t.Errorf("declaring outputs changed the VERIFICATION source to %q", v.Source)
	}
}

func TestDeclaredVerificationIsUsedExactly(t *testing.T) {
	d := resolveVerificationObligation(ctxWith(declaredVerification("go test ./...", "pytest")))
	if d.Source != ObligationSourceContractDeclared || !d.KnowledgeSpecified {
		t.Fatalf("source=%q specified=%v", d.Source, d.KnowledgeSpecified)
	}
	if strings.Join(d.Items, "|") != "go test ./...|pytest" {
		t.Fatalf("commands=%v", d.Items)
	}
}

func TestDeclaredEmptyVerificationCreatesNoObligation(t *testing.T) {
	d := resolveVerificationObligation(ctxWith(declaredVerification()))
	if !d.KnowledgeSpecified || len(d.Items) != 0 {
		t.Fatalf("specified=%v items=%v", d.KnowledgeSpecified, d.Items)
	}
}

func TestNoContractLeavesVerificationLegacy(t *testing.T) {
	d := resolveVerificationObligation(ctxWith(nil))
	if d.Source != ObligationSourceLegacy || d.KnowledgeSpecified || len(d.Items) != 0 {
		t.Fatalf("%+v", d)
	}
}

// --- fail closed ------------------------------------------------------------

func TestAnUnrecognisedKnowledgeValueFallsBackToLegacy(t *testing.T) {
	// Cannot arrive through validation; if it ever did, it must not grant
	// declared authority.
	tc := &TaskContract{TaskMode: TaskModeWork, OutputKnowledge: "sideways"}
	d := resolveOutputObligation(ctxWith(tc), proseNamesTwo)
	if d.Source != ObligationSourceLegacy || d.KnowledgeSpecified {
		t.Fatalf("an unknown knowledge value granted authority: %+v", d)
	}
}

func TestDeclaredWithNoListFallsBackToLegacy(t *testing.T) {
	// Also unreachable through validation. Contradictory state must not be
	// read as authoritative none.
	tc := &TaskContract{TaskMode: TaskModeWork, OutputKnowledge: KnowledgeDeclared}
	d := resolveOutputObligation(ctxWith(tc), proseNamesTwo)
	if d.Source != ObligationSourceLegacy || d.KnowledgeSpecified {
		t.Fatalf("declared-without-a-list was treated as authority: %+v", d)
	}
}

// --- request-bound identity -------------------------------------------------

func TestADecisionCarriesItsRequestIdentity(t *testing.T) {
	c := ctxWith(declaredOutputs("a.py"))
	c.Ctx = context.WithValue(context.Background(), requestIDKey, "req-42")
	if d := resolveOutputObligation(c, ""); d.RequestID != "req-42" {
		t.Fatalf("request identity %q", d.RequestID)
	}
	if d := resolveVerificationObligation(c); d.RequestID != "req-42" {
		t.Fatalf("request identity %q", d.RequestID)
	}
}

// --- structure: one owner, no shadow, no second decision --------------------

func proxyFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		out[n] = f
	}
	return out
}

func callSites(files map[string]*ast.File, fn string) map[string]int {
	hits := map[string]int{}
	for name, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == fn {
					hits[name+":"+fd.Name.Name]++
				}
				return true
			})
		}
	}
	return hits
}

func TestEachObligationClassHasExactlyOneLiveDecision(t *testing.T) {
	// Exactly one LIVE POLICY caller per class. The rule is about deciding, not
	// about reading: two gates each deriving the obligation is how they start
	// to disagree, which is what this pins.
	//
	// A reader is admitted by name and only where reading the owner is the
	// alternative to duplicating it. The evidence producer asks the owner
	// whether the client declared a command precisely so it does not re-derive
	// that answer from the raw contract -- and it reaches no live policy, which
	// evidence_inertness_test.go proves separately.
	readers := map[string]bool{
		"verification_evidence.go:requestDeclaredCommand": true,
		// The typed-declaration reader. It is inside the obligation owner and
		// answers one question about one command, from the same decision every
		// other reader sees.
		"obligations.go:requestVerificationRequirement": true,
		// The one place a caller asks what a request obliges. It derives from
		// what the owners said and decides nothing itself.
		"obligation_kinds.go:requestObligations": true,
		// The one place a caller asks whether output knowledge was STATED.
		// It reads KnowledgeSpecified and nothing else -- the alternative is
		// every caller re-deriving `declared` from raw contract fields, which
		// is exactly how declared-and-empty came to be mistaken for silence.
		"obligations.go:outputKnowledgeDeclared": true,
	}
	files := proxyFiles(t)
	for _, fn := range []string{"resolveOutputObligation", "resolveVerificationObligation"} {
		sites := callSites(files, fn)
		deciders := map[string]int{}
		total := 0
		for site, n := range sites {
			if readers[site] {
				continue
			}
			deciders[site] = n
			total += n
		}
		if total != 1 {
			t.Errorf("%s is decided %d times from %v, want exactly one live decision",
				fn, total, deciders)
		}
	}
}

// TestOnlyNamedReadersReachTheObligationOwner keeps the exemption above
// honest: a reader may exist, and it must be one this file knows about.
func TestOnlyNamedReadersReachTheObligationOwner(t *testing.T) {
	known := map[string]bool{
		"agent.go:runAgentLoop":                           true,
		"guardrails.go:decideVerificationDemand":          true,
		"verification_evidence.go:requestDeclaredCommand": true,
		// The typed-declaration reader. It is inside the obligation owner and
		// answers one question about one command, from the same decision every
		// other reader sees.
		"obligations.go:requestVerificationRequirement": true,
		"obligation_kinds.go:requestObligations":        true,
		"obligations.go:outputKnowledgeDeclared":        true,
	}
	files := proxyFiles(t)
	for _, fn := range []string{"resolveOutputObligation", "resolveVerificationObligation"} {
		for site := range callSites(files, fn) {
			if !known[site] {
				t.Errorf("%s reaches %s and is not a named reader of the "+
					"obligation owner", site, fn)
			}
		}
	}
}

func TestTheLegacyOutputHeuristicIsReachedOnlyThroughTheOwner(t *testing.T) {
	// The shadow snapshot computes the legacy answer too, on purpose: its
	// whole job is to record what ATLAS would have inferred beside what the
	// client declared. A capture that read production's decision would be
	// comparing production with itself.
	allowed := map[string]bool{
		"obligations.go:resolveOutputObligation": true,
		"agent.go:emitShadowRequestSnapshot":     true,
	}
	sites := callSites(proxyFiles(t), "expectedOutputPaths")
	for k := range sites {
		if !allowed[k] {
			t.Errorf("%s calls the legacy output heuristic directly", k)
		}
	}
	if len(sites) != len(allowed) {
		t.Errorf("the legacy heuristic is evaluated in %d places: %v", len(sites), sites)
	}
}

func TestTheObligationOwnerReadsNoShadowSymbol(t *testing.T) {
	files := proxyFiles(t)
	for name, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			if !strings.HasPrefix(fd.Name.Name, "resolve") ||
				!strings.Contains(fd.Name.Name, "Obligation") {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if ok && shadowProductionSymbols[id.Name] {
					t.Errorf("%s:%s reads shadow state %q", name, fd.Name.Name, id.Name)
				}
				return true
			})
		}
	}
}

// The knowledge fields may be read by the owner and by nothing else.
func TestOnlyTheOwnerReadsTheKnowledgeFields(t *testing.T) {
	files := proxyFiles(t)
	for name, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			owner := strings.HasPrefix(fd.Name.Name, "resolve") &&
				strings.Contains(fd.Name.Name, "Obligation")
			if owner || fd.Name.Name == "validateTaskContract" ||
				fd.Name.Name == "normalizeKnowledge" ||
				fd.Name.Name == "emitShadowRequestSnapshot" {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if sel.Sel.Name == "OutputKnowledge" || sel.Sel.Name == "VerificationKnowledge" {
					t.Errorf("%s:%s reads %s outside the obligation owner",
						name, fd.Name.Name, sel.Sel.Name)
				}
				return true
			})
		}
	}
}

// --- the V3 delivery graph is untouched by this slice -----------------------

func TestNoNewConsumerInTheDeliveryGraph(t *testing.T) {
	files := proxyFiles(t)
	for _, fn := range []string{"EvidenceSupportsProvenanceFor",
		"v3DeliveryAuthorized"} {
		for site := range callSites(files, fn) {
			allowed := map[string]bool{
				"v3_bridge.go:v3DeliveryAuthorized":          true,
				"v3_bridge.go:EvidenceSupportsProvenanceFor": true,
				// The service's own verdict has exactly one reader and two
				// callers: the contractless delivery rule, and the write and
				// edit routes that apply it to a request which declared
				// nothing. A request that DID declare its outputs never
				// reaches it.
				"tools.go:writeFileWithV3":      true,
				"tools.go:improveContentWithV3": true,
			}
			if !allowed[site] {
				t.Errorf("%s gained a new caller of %s", site, fn)
			}
		}
	}
	// The obligation owner must not reach the delivery graph at all.
	for _, fn := range []string{"EvidenceSupportsProvenanceFor",
		"v3DeliveryAuthorized", "MayAuthorize", "BindsTo"} {
		for site := range callSites(files, fn) {
			if strings.Contains(site, "Obligation") {
				t.Errorf("the obligation owner calls %s", fn)
			}
		}
	}
}

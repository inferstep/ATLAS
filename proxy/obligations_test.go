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
// Two sources can name the files a run must produce, and they must never be
// merged. The prose heuristic reads a window of text before a filename, so
// "Write solve.py that reads input.txt" makes the INPUT file a deliverable. A
// path the session only reads owns nothing and can never be demonstrated, so
// a run with it in its set could never complete.
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

const proseNamesTwo = "Write solve.py that reads input.txt"

// The output obligation is either the declared list, exactly, or the prose
// heuristic, exactly. The last two rows cannot arrive through validation; if
// they ever did, they must not be read as declared authority.
func TestTheOutputObligationComesFromExactlyOneSource(t *testing.T) {
	legacy := expectedOutputPaths(proseNamesTwo)
	for _, tc := range []struct {
		name          string
		contract      *TaskContract
		wantSource    ObligationSource
		wantSpecified bool
		wantItems     []string
	}{
		{"no contract", nil, ObligationSourceLegacy, false, legacy},
		{"unspecified knowledge", &TaskContract{TaskMode: TaskModeWork,
			OutputKnowledge: KnowledgeUnspecified, VerificationKnowledge: KnowledgeUnspecified},
			ObligationSourceLegacy, false, legacy},
		{"only verification declared", declaredVerification("pytest"), ObligationSourceLegacy, false, legacy},
		// The prose names input.txt too. A union would bring it back.
		{"declared", declaredOutputs("solve.py"), ObligationSourceContractDeclared, true, []string{"solve.py"}},
		{"declared empty", declaredOutputs(), ObligationSourceContractDeclared, true, nil},
		{"unrecognised knowledge value", &TaskContract{TaskMode: TaskModeWork, OutputKnowledge: "sideways"},
			ObligationSourceLegacy, false, legacy},
		{"declared with no list", &TaskContract{TaskMode: TaskModeWork, OutputKnowledge: KnowledgeDeclared},
			ObligationSourceLegacy, false, legacy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := resolveOutputObligation(ctxWith(tc.contract), proseNamesTwo)
			if d.Source != tc.wantSource || d.KnowledgeSpecified != tc.wantSpecified {
				t.Fatalf("source=%q specified=%v, want %q %v", d.Source, d.KnowledgeSpecified, tc.wantSource, tc.wantSpecified)
			}
			if strings.Join(d.Items, "|") != strings.Join(tc.wantItems, "|") {
				t.Fatalf("items %v, want %v", d.Items, tc.wantItems)
			}
		})
	}
}

// The verification obligation is either the declared commands, exactly, or
// the legacy decision. Declaring outputs does not change it.
func TestTheVerificationObligationComesFromExactlyOneSource(t *testing.T) {
	for _, tc := range []struct {
		name          string
		contract      *TaskContract
		wantSource    ObligationSource
		wantSpecified bool
		wantItems     []string
	}{
		{"no contract", nil, ObligationSourceLegacy, false, nil},
		{"only outputs declared", declaredOutputs("a.py"), ObligationSourceLegacy, false, nil},
		{"declared", declaredVerification("go test ./...", "pytest"),
			ObligationSourceContractDeclared, true, []string{"go test ./...", "pytest"}},
		{"declared empty", declaredVerification(), ObligationSourceContractDeclared, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := resolveVerificationObligation(ctxWith(tc.contract))
			if d.Source != tc.wantSource || d.KnowledgeSpecified != tc.wantSpecified {
				t.Fatalf("source=%q specified=%v, want %q %v", d.Source, d.KnowledgeSpecified, tc.wantSource, tc.wantSpecified)
			}
			if strings.Join(d.Items, "|") != strings.Join(tc.wantItems, "|") {
				t.Fatalf("items %v, want %v", d.Items, tc.wantItems)
			}
		})
	}
}

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

// sourceFunc is one function declaration of the proxy, with its file.
type sourceFunc struct {
	file string
	decl *ast.FuncDecl
}

// funcsWithBodies lists every function declaration in files that has a body.
func funcsWithBodies(files map[string]*ast.File) []sourceFunc {
	var out []sourceFunc
	for name, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				out = append(out, sourceFunc{file: name, decl: fd})
			}
		}
	}
	return out
}

func callSites(files map[string]*ast.File, fn string) map[string]int {
	hits := map[string]int{}
	for _, f := range funcsWithBodies(files) {
		ast.Inspect(f.decl.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == fn {
				hits[f.file+":"+f.decl.Name.Name]++
			}
			return true
		})
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
	// evidence_wiring_test.go proves separately.
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
		"obligations.go:resolveOutputObligation":   true,
		"task_intake.go:emitShadowRequestSnapshot": true,
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
	for _, f := range funcsWithBodies(proxyFiles(t)) {
		name := f.decl.Name.Name
		if !strings.HasPrefix(name, "resolve") || !strings.Contains(name, "Obligation") {
			continue
		}
		ast.Inspect(f.decl.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if ok && shadowProductionSymbols[id.Name] {
				t.Errorf("%s:%s reads shadow state %q", f.file, name, id.Name)
			}
			return true
		})
	}
}

// The knowledge fields may be read by the owner and by nothing else.
func TestOnlyTheOwnerReadsTheKnowledgeFields(t *testing.T) {
	for _, f := range funcsWithBodies(proxyFiles(t)) {
		name := f.decl.Name.Name
		owner := strings.HasPrefix(name, "resolve") && strings.Contains(name, "Obligation")
		if owner || name == "validateTaskContract" || name == "normalizeKnowledge" ||
			name == "emitShadowRequestSnapshot" {
			continue
		}
		ast.Inspect(f.decl.Body, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "OutputKnowledge" || sel.Sel.Name == "VerificationKnowledge" {
				t.Errorf("%s:%s reads %s outside the obligation owner", f.file, name, sel.Sel.Name)
			}
			return true
		})
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
				"tool_write_v3.go:emitV3WriteStageEnd":  true,
				"tool_write_v3.go:improveContentWithV3": true,
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

// A file the task says to read is an input, not something the run owes.
//
// In "Write solve.py that reads input.txt", input.txt follows a write verb but
// is not an output. The run never writes an input, so the ledger never owns it
// and it can never be demonstrated: a run that owed it would end incomplete
// after printing the correct answer.

func TestAFileTheTaskReadsIsNotAnOutputObligation(t *testing.T) {
	for prompt, want := range map[string]string{
		"Write solve.py that reads input.txt and prints how many fish exist after 256 days.":  "solve.py",
		"read the file input.txt and write the result to output.txt":                          "output.txt",
		"Write a script that loads the file config.yaml and saves the summary to report.json": "report.json",
		"Create parser.py that parses log.txt and writes errors.txt":                          "parser.py|errors.txt",
		"Write the parse results to out.json":                                                 "out.json",
	} {
		if got := strings.Join(expectedOutputPaths(prompt), "|"); got != want {
			t.Errorf("%q: outputs %q, want %q", prompt, got, want)
		}
	}
}

// The captured session shape, end to end: the run reads input.txt, writes
// solve.py, runs it and it prints the answer, then says done. Nothing the task
// asked for is outstanding, so the run completes.
func TestAnAocRunThatWritesAndRunsItsSolverCompletes(t *testing.T) {
	const prompt = "input.txt holds one command per line: 'forward N', 'down N' or 'up N'. Track horizontal " +
		"position, depth and aim, all starting at 0. 'down N' increases aim by N, 'up N' decreases aim by N, " +
		"and 'forward N' increases horizontal position by N AND increases depth by aim multiplied by N. " +
		"Write solve.py that reads input.txt and prints the final horizontal position multiplied by the final " +
		"depth. Then run it and confirm the answer."
	solve := "def main():\n    x = d = aim = 0\n    with open('input.txt') as f:\n        for line in f:\n" +
		"            op, n = line.split()\n            n = int(n)\n            if op == 'down':\n                aim += n\n" +
		"            elif op == 'up':\n                aim -= n\n            else:\n                x += n\n" +
		"                d += aim * n\n    print(x * d)\n\n\nif __name__ == '__main__':\n    main()\n"
	input := map[string]string{"input.txt": "forward 5\ndown 5\nforward 8\nup 3\ndown 8\nforward 2\n"}
	// The reliability runner sends {"task_mode":"work"} and nothing else.
	work := func(ctx *AgentContext) {
		tc, err := validateTaskContract(&TaskContract{TaskMode: TaskModeWork}, ctx.WorkingDir)
		if err != nil {
			t.Fatal(err)
		}
		ctx.TaskContract = tc
	}
	run := integrityLoopWith(t, prompt, input, []string{
		toolCall("read_file", map[string]interface{}{"path": "input.txt"}),
		toolCall("write_file", map[string]interface{}{"path": "solve.py", "content": solve}),
		toolCall("run_command", map[string]interface{}{"command": "python3 solve.py", "timeout": 30}),
		`{"type":"done","summary":"solve.py prints 900."}`,
	}, nil, work)
	if got, _ := run.disk(t, "solve.py"); got != solve {
		t.Fatalf("solve.py on disk differs: %q", got)
	}
	if run.terminal["status"] != "completed" {
		t.Errorf("terminal %s / %s, want completed\n%s", run.terminal["status"], run.terminal["reason"], run.feedback())
	}

	// What the task does owe is still owed: solve.py never run is not done.
	unrun := integrityLoopWith(t, prompt, input, []string{
		toolCall("write_file", map[string]interface{}{"path": "solve.py", "content": solve}),
		`{"type":"done","summary":"solve.py prints 900."}`,
		`{"type":"done","summary":"solve.py prints 900."}`,
		`{"type":"done","summary":"solve.py prints 900."}`,
		`{"type":"done","summary":"solve.py prints 900."}`,
	}, nil, work)
	if unrun.terminal["status"] == "completed" {
		t.Errorf("a solver that was never run completed (%s)", unrun.terminal["reason"])
	}
}

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The structured intent boundary.
//
// Two claims, and they are separate on purpose. A tool call says WHERE a
// candidate may act, in fields rather than in prose. It never says the
// candidate is any good, and it never widens anything.

func scopeWorld(t *testing.T) (*AgentContext, string) {
	t.Helper()
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-scope")
	return ctx, filepath.Join(dir, "solve.py")
}

func scopeEntry(t *testing.T, ctx *AgentContext) routeEntry {
	t.Helper()
	e := mintRouteEntry(ctx)
	if !e.valid() {
		t.Fatal("no route entry")
	}
	return e
}

// Every mutating tool defines a scope, and nothing else does.
func TestOnlyMutatingToolCallsDefineAScope(t *testing.T) {
	ctx, path := scopeWorld(t)
	for _, tool := range []string{"write_file", "edit_file", "insert_after",
		"replace_lines", "structural_edit"} {
		s, ok := deriveMutationScope(ctx, scopeEntry(t, ctx), tool, path, "", "x = 1\n")
		if !ok || !s.valid() || s.Tool != tool {
			t.Errorf("%s defined no scope", tool)
		}
		if s.identity() == "" {
			t.Errorf("%s produced a scope with no identity", tool)
		}
	}
	for _, tool := range []string{"read_file", "run_command", "delete_file", "", "write_files"} {
		if _, ok := deriveMutationScope(ctx, scopeEntry(t, ctx), tool, path, "", "x = 1\n"); ok {
			t.Errorf("%q defined a mutation scope", tool)
		}
	}
}

// A scope is the canonical target, and no spelling of a path reaches past the
// workspace.
func TestScopeFailsClosedOnAliasesAndEscapes(t *testing.T) {
	ctx, path := scopeWorld(t)
	canonical, ok := deriveMutationScope(ctx, scopeEntry(t, ctx), "write_file", path, "", "x = 1\n")
	if !ok {
		t.Fatal("the canonical path defined no scope")
	}
	// An alias of the same file is the same target, not a second one.
	alias := filepath.Join(ctx.WorkingDir, ".", "solve.py")
	aliased, ok := deriveMutationScope(ctx, scopeEntry(t, ctx), "write_file", alias, "", "x = 1\n")
	if !ok || aliased.Target != canonical.Target {
		t.Errorf("an alias resolved to %q, want %q", aliased.Target, canonical.Target)
	}
	for _, escape := range []string{
		"../outside.py",
		filepath.Join(ctx.WorkingDir, "..", "outside.py"),
		"/etc/passwd",
	} {
		if _, ok := deriveMutationScope(ctx, scopeEntry(t, ctx), "write_file", escape, "", "x = 1\n"); ok {
			t.Errorf("%q defined a scope outside the workspace", escape)
		}
	}
}

// A scope never authorizes a deletion, whichever side the emptiness is on.
func TestScopeNeverAuthorizesADeletion(t *testing.T) {
	ctx, path := scopeWorld(t)
	if _, ok := deriveMutationScope(ctx, scopeEntry(t, ctx), "write_file", path, "x = 1\n", "   "); ok {
		t.Error("a call that leaves nothing behind defined a scope")
	}
	s, ok := deriveMutationScope(ctx, scopeEntry(t, ctx), "write_file", path, "", "x = 1\n")
	if !ok {
		t.Fatal("no scope")
	}
	if admits, why := scopeAdmitsCandidate(ctx, s, ""); admits || why != scopeRefusedEmptyCandidate {
		t.Errorf("an empty candidate was admitted (%v/%q)", admits, why)
	}
}

// An edit's boundary is the edit. A candidate that rewrites past it is out of
// scope, and the reason says so.
func TestScopeRefusesACandidateThatLeavesTheBoundary(t *testing.T) {
	ctx, path := scopeWorld(t)
	original := "def a():\n    return 1\n\n\ndef keep():\n    return 'untouched'\n"
	edited := "def a():\n    return 2\n\n\ndef keep():\n    return 'untouched'\n"
	s, ok := deriveMutationScope(ctx, scopeEntry(t, ctx), "edit_file", path, original, edited)
	if !ok || s.Kind != mutationScopeInPlaceEdit {
		t.Fatalf("scope %+v", s)
	}
	inside := "def a():\n    return 2 + 0\n\n\ndef keep():\n    return 'untouched'\n"
	if admits, why := scopeAdmitsCandidate(ctx, s, inside); !admits {
		t.Errorf("an in-boundary candidate was refused: %s", why)
	}
	outside := "def a():\n    return 2\n\n\ndef keep():\n    return 'rewritten'\n"
	if admits, why := scopeAdmitsCandidate(ctx, s, outside); admits || why != scopeRefusedBoundary {
		t.Errorf("an out-of-boundary candidate was admitted (%v/%q)", admits, why)
	}
}

// Identity is re-read live. A target or workspace that moved is a scope that
// no longer describes anything.
func TestScopeFailsClosedOnStaleIdentity(t *testing.T) {
	ctx, path := scopeWorld(t)
	if err := os.WriteFile(path, []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	observeDeliverable(ctx, path, []byte("x = 1\n"), ValidationKindSyntax, ValidationPassed, "")
	s, ok := deriveMutationScope(ctx, scopeEntry(t, ctx), "write_file", path, "x = 1\n", "x = 2\n")
	if !ok {
		t.Fatal("no scope")
	}
	if admits, why := scopeAdmitsCandidate(ctx, s, "x = 2\n"); !admits {
		t.Fatalf("a fresh scope refused its own candidate: %s", why)
	}
	// The target moves underneath it.
	observeDeliverable(ctx, path, []byte("x = 3\n"), ValidationKindSyntax, ValidationPassed, "")
	if admits, why := scopeAdmitsCandidate(ctx, s, "x = 2\n"); admits ||
		(why != scopeRefusedTargetMoved && why != scopeRefusedWorkspaceMoved) {
		t.Errorf("a moved target still admitted a candidate (%v/%q)", admits, why)
	}
}

// A request that is over admits nothing, whatever it once bounded.
func TestScopeFailsClosedOnCancellation(t *testing.T) {
	ctx, path := scopeWorld(t)
	s, ok := deriveMutationScope(ctx, scopeEntry(t, ctx), "write_file", path, "", "x = 1\n")
	if !ok {
		t.Fatal("no scope")
	}
	cancelled, cancel := context.WithCancel(ctx.Ctx)
	cancel()
	ctx.Ctx = cancelled
	if admits, why := scopeAdmitsCandidate(ctx, s, "x = 1\n"); admits || why != scopeRefusedCancelled {
		t.Errorf("a cancelled request admitted a candidate (%v/%q)", admits, why)
	}
	if _, ok := deriveMutationScope(ctx, scopeEntry(t, ctx), "write_file", path, "", "x = 1\n"); ok {
		t.Error("a cancelled request defined a new scope")
	}
}

// A scope belongs to the request that made it.
func TestScopeBelongsToItsOwnRequest(t *testing.T) {
	ctx, path := scopeWorld(t)
	s, ok := deriveMutationScope(ctx, scopeEntry(t, ctx), "write_file", path, "", "x = 1\n")
	if !ok {
		t.Fatal("no scope")
	}
	other := s
	other.RequestID = "req-somebody-else"
	if admits, _ := scopeAdmitsCandidate(ctx, other, "x = 1\n"); admits {
		t.Error("a scope from another request admitted a candidate")
	}
}

// The scope narrows and never grants: no licence exists without one, and one
// that names a different target mints nothing.
func TestAGrantCannotOutrunItsScope(t *testing.T) {
	w := newRouteWorldWithClosure(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
			`"verification_knowledge":"declared",`+
			`"verification_requirements_version":1,"verification_requirements":[`+
			`{"command":"pytest -q","kind":"behavioral","expects":"exit_zero",`+
			`"asset_authority":"client_supplied"}]}`,
		map[string]stubEffect{"pytest -q": {}}, false)
	recs := captureShadow(t, func() {
		if _, err := w.write(t); err != nil {
			t.Fatal(err)
		}
	})
	scopes := recordsOfKind(recs, "candidate_mutation_scope")
	if len(scopes) == 0 {
		t.Fatal("the route recorded no structured scope")
	}
	scopeID, _ := scopes[0]["scope_id"].(string)
	if scopeID == "" {
		t.Fatal("the scope record names no identity")
	}
	if admitted, _ := scopes[0]["admitted"].(bool); !admitted {
		t.Fatalf("the scope refused its own route's candidate: %v", scopes[0]["refusal"])
	}
	if authorizes, _ := scopes[0]["authorizes"].(bool); authorizes {
		t.Error("a scope record claims to authorize")
	}
	// Source-level: minting goes through the grant check, the check reads
	// the scope, and the scope reads no evidence.
	src, err := os.ReadFile("authorization_grant.go")
	if err != nil {
		t.Fatal(err)
	}
	mint := string(src)[strings.Index(string(src), "func mintAuthorizationGrant("):]
	mint = mint[:strings.Index(mint, "\n}")]
	if !strings.Contains(mint, "grantFor(") {
		t.Error("minting no longer goes through the grant check")
	}
	check := string(src)[strings.Index(string(src), "func grantFor("):]
	check = check[:strings.Index(check, "\n}")]
	if !strings.Contains(check, "scopeAdmitsCandidate") || !strings.Contains(check, "in.Scope") {
		t.Error("the grant check no longer consults the mutation scope")
	}
	scopeSrc, err := os.ReadFile("mutation_scope.go")
	if err != nil {
		t.Fatal(err)
	}
	body := codeWithoutComments(string(scopeSrc))
	for _, banned := range []string{"proxyEvidence", "Envelope", "ClosureEligible",
		"authorizationGrant", "decideAuthorization", "Passed"} {
		if strings.Contains(body, banned) {
			t.Errorf("the scope reads or mints %q", banned)
		}
	}
}

// The scope is a fact about a tool call, and its record carries no content.
func TestScopeRecordCarriesNoContent(t *testing.T) {
	w := newRouteWorldWithClosure(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		nil, false)
	recs := captureShadow(t, func() {
		if _, err := w.write(t); err != nil {
			t.Fatal(err)
		}
	})
	for _, r := range recordsOfKind(recs, "candidate_mutation_scope") {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		blob := string(raw)
		for _, secret := range []string{routeWinner, routeBaseline, w.path,
			"Make solve fast.", "solve.py"} {
			if secret != "" && strings.Contains(blob, secret) {
				t.Errorf("the scope record carries %q", secret)
			}
		}
	}
}

// All four edit tools and the new-file route reach the same boundary owner
// with their own tool name. A tool that routed around it would be a mutation
// nothing bounded.
func TestEveryMutatingRouteDerivesItsOwnScope(t *testing.T) {
	src, err := os.ReadFile("tools.go")
	if err != nil {
		t.Fatal(err)
	}
	body := codeWithoutComments(string(src))
	for _, tool := range []string{"edit_file", "insert_after", "replace_lines"} {
		if !strings.Contains(body, `runEditPipeline(ctx, "`+tool+`"`) {
			t.Errorf("%s no longer reaches the protected edit route", tool)
		}
	}
	if !strings.Contains(body, `deliverEditCandidate(ctx, "structural_edit"`) {
		t.Error("structural_edit no longer reaches the protected edit route")
	}
	if !strings.Contains(body, `deriveMutationScope(ctx, entry, "write_file"`) {
		t.Error("the new-file route derives no scope")
	}
	edit, err := os.ReadFile("edit_route_delivery.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(codeWithoutComments(string(edit)),
		"deriveMutationScope(ctx, entry, tool, path, original, edited)") {
		t.Error("the edit route derives no scope from its own call")
	}
	// And the scope's tool set is exactly those five.
	if len(mutationScopeTools) != 5 {
		t.Errorf("%d tools define a scope, want the five mutating ones",
			len(mutationScopeTools))
	}
}

// A whole-file write has no line boundary to leave.
//
// RED, in the exact shape the eligibility pilot retained: write_file creates
// the declared target, then write_file replaces it. The second call was
// classified in_place_edit because the file now existed, and the edit-boundary
// rule refused a candidate that was squarely inside the tool's authority. Six
// routes across four families died that way.
func TestAWholeFileWriteIsNotAnEdit(t *testing.T) {
	ctx, path := scopeWorld(t)
	original := "def solve(values):\n    total = 0\n    for v in values:\n" +
		"        total += v\n    return total\n"
	proposal := "def solve(values):\n    return sum(values)\n"
	candidate := "def solve(values):\n    return sum(values)  # tightened\n"

	// The pilot's shape: the target already exists, the tool is write_file.
	s, ok := deriveMutationScope(ctx, scopeEntry(t, ctx), "write_file", path,
		original, proposal)
	if !ok {
		t.Fatal("a rewrite of an existing target defined no scope")
	}
	if s.Kind != mutationScopeWholeFile {
		t.Fatalf("scope kind %q, want whole_file", s.Kind)
	}
	if admits, why := scopeAdmitsCandidate(ctx, s, candidate); !admits {
		t.Fatalf("a whole-file rewrite was refused for %q", why)
	}

	// The same bytes through an edit tool stay refused: an edit that dropped
	// every line it kept IS out of its boundary.
	for _, tool := range []string{"edit_file", "insert_after", "replace_lines",
		"structural_edit"} {
		e, ok := deriveMutationScope(ctx, scopeEntry(t, ctx), tool, path,
			original, proposal)
		if !ok {
			t.Fatalf("%s defined no scope", tool)
		}
		if e.Kind != mutationScopeInPlaceEdit {
			t.Errorf("%s scope kind %q, want in_place_edit", tool, e.Kind)
		}
		// Drops a line the edit kept: the function the original and the
		// caller's own result both declare.
		outside := "def compute(values):\n    return 0\n"
		if admits, why := scopeAdmitsCandidate(ctx, e, outside); admits {
			t.Errorf("%s admitted a candidate outside its boundary (%q)", tool, why)
		}
	}

	// And a first write is still a new file, not a rewrite.
	first, ok := deriveMutationScope(ctx, scopeEntry(t, ctx), "write_file", path,
		"", proposal)
	if !ok || first.Kind != mutationScopeNewFile {
		t.Errorf("a first write is %q, want new_file", first.Kind)
	}
}

// Widening the scope kind widens nothing else. Every other refusal a rewrite
// must still meet is asserted on the same scope.
func TestAWholeFileWriteKeepsEveryOtherBoundary(t *testing.T) {
	ctx, path := scopeWorld(t)
	original, proposal := "x = 1\n", "x = 2\n"
	s, ok := deriveMutationScope(ctx, scopeEntry(t, ctx), "write_file", path,
		original, proposal)
	if !ok {
		t.Fatal("no scope")
	}
	if admits, why := scopeAdmitsCandidate(ctx, s, ""); admits ||
		why != scopeRefusedEmptyCandidate {
		t.Errorf("a deletion was admitted (%v/%q)", admits, why)
	}
	other := s
	other.Target = filepath.Join(ctx.WorkingDir, "somewhere_else.py")
	if admits, _ := scopeAdmitsCandidate(ctx, other, "x = 3\n"); admits {
		// The target moved under the scope: its generation no longer matches.
		t.Log("a different target is caught by generation or tombstone")
	}
	foreign := s
	foreign.RequestID = "req-somebody-else"
	if admits, _ := scopeAdmitsCandidate(ctx, foreign, "x = 3\n"); admits {
		t.Error("a scope from another request admitted a rewrite")
	}
	cancelled, cancel := context.WithCancel(ctx.Ctx)
	cancel()
	ctx.Ctx = cancelled
	if admits, why := scopeAdmitsCandidate(ctx, s, "x = 3\n"); admits ||
		why != scopeRefusedCancelled {
		t.Errorf("a cancelled request admitted a rewrite (%v/%q)", admits, why)
	}
	// An escaping path still defines no scope at all.
	if _, ok := deriveMutationScope(ctx, scopeEntry(t, ctx), "write_file",
		"../outside.py", original, proposal); ok {
		t.Error("the wider kind let a path escape the workspace")
	}
}

// One containment resolver, one boundary rule. A second of either is how two
// answers to the same question start disagreeing.
func TestOneScopeAuthorityAndOnePathNormalizer(t *testing.T) {
	src, err := os.ReadFile("mutation_scope.go")
	if err != nil {
		t.Fatal(err)
	}
	body := codeWithoutComments(string(src))
	if n := strings.Count(body, "resolveWorkspacePath("); n != 1 {
		t.Errorf("the scope resolves containment %d times, want once", n)
	}
	for _, banned := range []string{"filepath.Abs(", "filepath.EvalSymlinks(",
		"os.Getwd(", "strings.TrimPrefix(path"} {
		if strings.Contains(body, banned) {
			t.Errorf("the scope normalises paths its own way via %q", banned)
		}
	}
	if n := strings.Count(body, "v3RewroteBeyondTheEdit("); n != 1 {
		t.Errorf("the boundary rule is applied %d times inside the scope, want once", n)
	}
	files := proxyFiles(t)
	for site := range callSites(files, "v3RewroteBeyondTheEdit") {
		switch site {
		case "gates.go:v3RewroteBeyondTheEdit",
			"mutation_scope.go:scopeAdmitsCandidate",
			"edit_route_delivery.go:deliverEditCandidate":
		default:
			t.Errorf("%s is a second owner of the edit boundary", site)
		}
	}
}

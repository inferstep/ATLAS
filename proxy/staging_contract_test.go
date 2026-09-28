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

// The staging transport: what may be asked, what may come back, and every way
// a malformed answer fails closed.
//
// Nothing here executes anything. The contract is validation only.

func stagingIdent() stagingIdentity {
	return stagingIdentity{
		RequestID: "req-1", InvocationID: "req-1:inv:1",
		CandidateInstanceID: "req-1:inv:1:abcdef0123456789",
		CandidateHash:       contentSHA256("print(7)\n"),
		TargetPath:          "/w/solve.py",
		BaselineIdentity:    "syntax:base",
		WorkspaceGeneration: 3,
		WorkspaceStateHash:  contentSHA256("ws"),
	}
}

func stagingReq(commands ...string) stagingRequest {
	req := stagingRequest{
		WireVersion:    stagingWireVersion,
		Identity:       stagingIdent(),
		CandidateBytes: "print(7)\n",
		Budget:         defaultStagingBudget(),
	}
	for i, text := range commands {
		req.Commands = append(req.Commands, stagingCommand{
			Text: text, Identity: contentSHA256(text),
			ObligationID: ObligationDeclaredCommand + ":" + contentSHA256(text)[:32],
			Index:        i, Count: len(commands),
		})
	}
	return req
}

func stagingRes(req stagingRequest, outcomes ...stagingCommandOutcome) stagingResult {
	res := stagingResult{
		WireVersion: stagingWireVersion, Identity: req.Identity,
		Complete: len(outcomes) == len(req.Commands), WorkspaceDestroyed: true,
	}
	for i, o := range outcomes {
		c := req.Commands[i]
		res.Commands = append(res.Commands, stagingCommandResult{
			CommandIdentity: c.Identity, ObligationID: c.ObligationID,
			Index: c.Index, Count: c.Count, Outcome: o,
			TargetHashBefore:    req.Identity.CandidateHash,
			TargetHashAfter:     req.Identity.CandidateHash,
			WorkspaceHashBefore: req.Identity.WorkspaceStateHash,
			WorkspaceHashAfter:  req.Identity.WorkspaceStateHash,
		})
	}
	return res
}

// --- the request ---------------------------------------------------------------

func TestAWellFormedStagingRequestValidates(t *testing.T) {
	req := stagingReq("pytest -q", "ruff check .")
	if ok, why := req.validate(); !ok {
		t.Fatalf("a well-formed request was refused: %s", why)
	}
}

func TestAStagingRequestFailsClosed(t *testing.T) {
	for _, c := range []struct {
		name string
		mut  func(*stagingRequest)
	}{
		{"unknown wire version", func(r *stagingRequest) { r.WireVersion = "99" }},
		{"no wire version", func(r *stagingRequest) { r.WireVersion = "" }},
		{"no request id", func(r *stagingRequest) { r.Identity.RequestID = "" }},
		{"no invocation id", func(r *stagingRequest) { r.Identity.InvocationID = "" }},
		{"no candidate instance", func(r *stagingRequest) { r.Identity.CandidateInstanceID = "" }},
		{"no candidate hash", func(r *stagingRequest) { r.Identity.CandidateHash = "" }},
		{"no target path", func(r *stagingRequest) { r.Identity.TargetPath = "" }},
		{"no workspace state", func(r *stagingRequest) { r.Identity.WorkspaceStateHash = "" }},
		{"negative generation", func(r *stagingRequest) { r.Identity.WorkspaceGeneration = -1 }},
		{"bytes are not the candidate", func(r *stagingRequest) { r.CandidateBytes = "print(8)\n" }},
		{"no commands", func(r *stagingRequest) { r.Commands = nil }},
		{"command with no text", func(r *stagingRequest) { r.Commands[0].Text = "" }},
		{"identity does not name the command", func(r *stagingRequest) {
			r.Commands[0].Identity = contentSHA256("something else")
		}},
		{"command names no obligation", func(r *stagingRequest) { r.Commands[0].ObligationID = "" }},
		{"index contradicts the set", func(r *stagingRequest) { r.Commands[0].Index = 7 }},
		{"count contradicts the set", func(r *stagingRequest) { r.Commands[0].Count = 9 }},
		{"budget out of range", func(r *stagingRequest) { r.Budget.MaxCommands = 0 }},
		{"a command may outlast the set", func(r *stagingRequest) {
			r.Budget.PerCommandTimeoutSec = r.Budget.TotalTimeoutSec + 1
		}},
	} {
		req := stagingReq("pytest -q", "ruff check .")
		c.mut(&req)
		if ok, _ := req.validate(); ok {
			t.Errorf("%s was accepted", c.name)
		}
	}
}

func TestADuplicateCommandIdentityIsRefused(t *testing.T) {
	req := stagingReq("pytest -q", "pytest -q")
	if ok, why := req.validate(); ok {
		t.Error("one command declared twice was accepted as two obligations")
	} else if !strings.Contains(why, "duplicate") {
		t.Errorf("refused for %q, want the duplicate", why)
	}
}

func TestTheDeclaredSetMayNotExceedTheStagingBudget(t *testing.T) {
	// maxTaskContractEntries is an input ceiling, not an execution policy.
	req := stagingReq("a", "b", "c", "d", "e")
	if ok, why := req.validate(); ok {
		t.Error("five commands ran against a four-command budget")
	} else if !strings.Contains(why, "budget") {
		t.Errorf("refused for %q, want the budget", why)
	}
	if defaultStagingBudget().MaxCommands >= maxTaskContractEntries {
		t.Error("the staging budget is not smaller than the validation ceiling")
	}
}

// --- the result ----------------------------------------------------------------

func TestAWellFormedStagingResultValidates(t *testing.T) {
	req := stagingReq("pytest -q", "ruff check .")
	res := stagingRes(req, stagingExitedZero, stagingExitedZero)
	if ok, why := res.validateAgainst(req); !ok {
		t.Fatalf("a well-formed result was refused: %s", why)
	}
	got := res.authorizingOutcomes()
	if len(got) != 2 {
		t.Errorf("authorizing %v, want both", got)
	}
}

func TestAStagingResultFailsClosed(t *testing.T) {
	base := stagingReq("pytest -q", "ruff check .")
	for _, c := range []struct {
		name string
		mut  func(*stagingResult)
	}{
		{"unknown wire version", func(r *stagingResult) { r.WireVersion = "99" }},
		{"another request", func(r *stagingResult) { r.Identity.RequestID = "req-2" }},
		{"another invocation", func(r *stagingResult) { r.Identity.InvocationID = "req-1:inv:2" }},
		{"another candidate", func(r *stagingResult) { r.Identity.CandidateInstanceID = "other" }},
		{"another hash", func(r *stagingResult) { r.Identity.CandidateHash = contentSHA256("x") }},
		{"another target", func(r *stagingResult) { r.Identity.TargetPath = "/w/other.py" }},
		{"another baseline", func(r *stagingResult) { r.Identity.BaselineIdentity = "syntax:other" }},
		{"another workspace", func(r *stagingResult) { r.Identity.WorkspaceStateHash = contentSHA256("moved") }},
		{"a later generation", func(r *stagingResult) { r.Identity.WorkspaceGeneration++ }},
		{"unknown outcome", func(r *stagingResult) { r.Commands[0].Outcome = "something_new" }},
		{"a command never asked for", func(r *stagingResult) {
			r.Commands[0].CommandIdentity = contentSHA256("whoami")
		}},
		{"a different obligation", func(r *stagingResult) { r.Commands[0].ObligationID = "declared_command:other" }},
		{"index contradicts the request", func(r *stagingResult) { r.Commands[0].Index = 5 }},
		{"count contradicts the request", func(r *stagingResult) { r.Commands[0].Count = 5 }},
		{"mutation flag contradicts the hashes", func(r *stagingResult) {
			r.Commands[0].MutatedTarget = true
		}},
		{"workspace flag contradicts the hashes", func(r *stagingResult) {
			r.Commands[0].MutatedWorkspace = true
		}},
	} {
		res := stagingRes(base, stagingExitedZero, stagingExitedZero)
		c.mut(&res)
		if ok, _ := res.validateAgainst(base); ok {
			t.Errorf("%s was accepted", c.name)
		}
	}
}

func TestADuplicateResultCommandIsRefused(t *testing.T) {
	req := stagingReq("pytest -q", "ruff check .")
	res := stagingRes(req, stagingExitedZero, stagingExitedZero)
	res.Commands[1].CommandIdentity = res.Commands[0].CommandIdentity
	res.Commands[1].ObligationID = res.Commands[0].ObligationID
	res.Commands[1].Index = res.Commands[0].Index
	if ok, _ := res.validateAgainst(req); ok {
		t.Error("one command reported twice was accepted")
	}
}

func TestAPartialSetIsNeverComplete(t *testing.T) {
	req := stagingReq("pytest -q", "ruff check .")
	res := stagingRes(req, stagingExitedZero)
	res.Complete = true
	if ok, why := res.validateAgainst(req); ok {
		t.Error("half a set claimed to be complete")
	} else if !strings.Contains(why, "complete") {
		t.Errorf("refused for %q", why)
	}
	res.Complete = false
	if ok, why := res.validateAgainst(req); !ok {
		t.Errorf("an honestly partial result was refused: %s", why)
	}
	if got := res.authorizingOutcomes(); len(got) != 1 {
		t.Errorf("authorizing %v, want only the one that ran", got)
	}
}

// --- only a clean, non-mutating pass authorizes ---------------------------------

func TestOnlyACleanNonMutatingPassAuthorizes(t *testing.T) {
	req := stagingReq("pytest -q")
	for _, o := range []stagingCommandOutcome{
		stagingExitedNonZero, stagingTimedOut, stagingCancelled, stagingRefused,
		stagingMutatedTarget, stagingMutatedWorkspace, stagingUnobservable,
		stagingBudgetExceeded, stagingUnavailable,
	} {
		res := stagingRes(req, o)
		if got := res.authorizingOutcomes(); len(got) != 0 {
			t.Errorf("%s authorized %v", o, got)
		}
	}
	clean := stagingRes(req, stagingExitedZero)
	if got := clean.authorizingOutcomes(); len(got) != 1 {
		t.Errorf("a clean pass authorized %v", got)
	}
}

func TestAPassThatChangedSomethingAuthorizesNothing(t *testing.T) {
	req := stagingReq("pytest -q")
	t.Run("changed the candidate", func(t *testing.T) {
		res := stagingRes(req, stagingExitedZero)
		res.Commands[0].TargetHashAfter = contentSHA256("rewritten")
		res.Commands[0].MutatedTarget = true
		if ok, why := res.validateAgainst(req); !ok {
			t.Fatalf("a consistent mutation report was refused: %s", why)
		}
		if got := res.authorizingOutcomes(); len(got) != 0 {
			t.Errorf("a command that rewrote its own subject authorized %v", got)
		}
	})
	t.Run("changed the workspace", func(t *testing.T) {
		res := stagingRes(req, stagingExitedZero)
		res.Commands[0].WorkspaceHashAfter = contentSHA256("moved")
		res.Commands[0].MutatedWorkspace = true
		if ok, why := res.validateAgainst(req); !ok {
			t.Fatalf("a consistent mutation report was refused: %s", why)
		}
		if got := res.authorizingOutcomes(); len(got) != 0 {
			t.Errorf("a command that changed its inputs authorized %v", got)
		}
	})
}

// --- nothing on the wire carries content ---------------------------------------

func TestTheStagingWireCarriesNoCommandTextOrBytes(t *testing.T) {
	const secretCmd = "pytest --token=hunter2 -q"
	const secretCode = "TOKEN = 'hunter2'\nprint(7)\n"
	req := stagingReq(secretCmd)
	req.CandidateBytes = secretCode
	req.Identity.CandidateHash = contentSHA256(secretCode)
	if ok, why := req.validate(); !ok {
		t.Fatalf("fixture is wrong: %s", why)
	}
	res := stagingRes(req, stagingExitedZero)

	for name, v := range map[string]interface{}{"request": req, "result": res} {
		blob, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		for _, needle := range []string{secretCmd, secretCode, "hunter2", "TOKEN", "pytest"} {
			if strings.Contains(string(blob), needle) {
				t.Errorf("the serialised %s carries %q", name, needle)
			}
		}
	}
}

// TestTheResultTypeCannotCarryOutput pins the shape: no field on a staging
// result may hold stdout, stderr, a diagnostic or a path's contents.
func TestTheResultTypeCannotCarryOutput(t *testing.T) {
	fset := token.NewFileSet()
	tree, err := parser.ParseFile(fset, "staging_contract.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	banned := map[string]bool{
		"Stdout": true, "Stderr": true, "Output": true, "Detail": true,
		"Message": true, "Contents": true, "Body": true, "Log": true,
		"Diagnostic": true, "Text": true,
	}
	ast.Inspect(tree, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || !strings.HasPrefix(ts.Name.Name, "staging") {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, f := range st.Fields.List {
			for _, name := range f.Names {
				if !banned[name.Name] {
					continue
				}
				// One exception, and it must be un-serialisable: the command
				// text has to reach the executor somehow.
				if ts.Name.Name == "stagingCommand" && name.Name == "Text" {
					if f.Tag == nil || !strings.Contains(f.Tag.Value, `json:"-"`) {
						t.Error("stagingCommand.Text is serialisable; command text must never travel")
					}
					continue
				}
				t.Errorf("%s.%s can hold content", ts.Name.Name, name.Name)
			}
		}
		return true
	})
}

// --- the executor declares no authority ----------------------------------------

// TestTheExecutorReportsFactsAndNotConclusions pins the trust boundary at the
// sandbox: it may report hashes, an exit status and whether it timed out. It
// may not name a provenance source, decide a mutation was permitted, or say
// anything about a client contract it cannot see.
func TestTheExecutorReportsFactsAndNotConclusions(t *testing.T) {
	src, err := os.ReadFile("../sandbox/executor_server.py")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	// Vocabulary that would mean the executor had made a trust claim. Each is
	// specific to this stack's provenance language: none of them can appear by
	// accident, which is what makes their absence meaningful.
	for _, banned := range []string{
		"client_declared", "proxy_owned_validation", "provenance",
		"TaskContract", "task_contract", "obligation",
		"influences_live_decision", "may_authorize",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("the executor names %q; it reports observations and declares "+
				"no authority", banned)
		}
	}
	// And what it does report is hashes and counts. Checked over the FIELD
	// declarations rather than the whole block: the docstring says "no command
	// text" and a prose match would fail on the sentence promising the thing
	// it is checking for.
	if !strings.Contains(body, "class ShellObservation") {
		t.Fatal("the executor no longer reports a staging observation")
	}
	start := strings.Index(body, "class ShellObservation")
	end := strings.Index(body[start:], "\n\n\nclass ")
	if end < 0 {
		end = len(body) - start
	}
	for _, line := range strings.Split(body[start:start+end], "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, ":") {
			continue
		}
		field := strings.TrimSpace(strings.SplitN(line, ":", 2)[0])
		if !isPythonFieldName(field) {
			continue // docstring prose, not a declaration
		}
		for _, banned := range []string{"stdout", "stderr", "content", "text", "output"} {
			if strings.Contains(strings.ToLower(field), banned) {
				t.Errorf("the staging observation declares %q, which can carry content", field)
			}
		}
	}
}

// isPythonFieldName reports whether a token is a bare identifier, so a
// docstring sentence containing a colon is not read as a field declaration.
func isPythonFieldName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// TestTheStagingOutcomeVocabularyIsClosed pins that every way a command can
// end has its own name, so no two failures collapse into one answer.
func TestTheStagingOutcomeVocabularyIsClosed(t *testing.T) {
	required := []stagingCommandOutcome{
		stagingExitedZero, stagingExitedNonZero, stagingTimedOut,
		stagingCancelled, stagingRefused, stagingMutatedTarget,
		stagingMutatedWorkspace, stagingUnobservable, stagingBudgetExceeded,
		stagingUnavailable, stagingResourceExhausted,
	}
	for _, o := range required {
		if !stagingCommandOutcomes[o] {
			t.Errorf("%q is required and not in the closed set", o)
		}
	}
	if stagingCommandOutcomes["something_new"] {
		t.Error("an unclassified outcome is accepted")
	}
	if len(stagingCommandOutcomes) != len(required) {
		t.Errorf("the set has %d members, the contract names %d",
			len(stagingCommandOutcomes), len(required))
	}
}

// TestTheContractExecutesNothing pins that this file is transport only.
func TestTheContractExecutesNothing(t *testing.T) {
	fset := token.NewFileSet()
	tree, err := parser.ParseFile(fset, "staging_contract.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	banned := map[string]bool{
		"runViaSandbox": true, "runLocally": true, "Do": true, "Post": true,
		"Command": true, "CommandContext": true, "Start": true, "Run": true,
		"WriteFile": true, "Remove": true, "RemoveAll": true, "MkdirTemp": true,
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
			t.Errorf("the transport contract calls %s", name)
		}
		return true
	})
}

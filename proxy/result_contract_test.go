package main

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"regexp"
	"strings"
	"testing"
)

// A ToolResult reports separate facts: whether bytes changed (MutationStatus),
// what was checked (ValidationKind) and how the check came out
// (ValidationStatus). `Success` alone mixes these up: a warned invalid write
// and a validated one look the same, and a delete looks like productive work.
//
// Every field fails closed at its zero value. A result that nobody filled in,
// or that carries a value this build does not know, never reads as applied or
// as passed.

// Only "applied" counts as applied. Only the named states count as
// classified: Unknown means that no producer set the field.
func TestMutationStatusValues(t *testing.T) {
	for _, tc := range []struct {
		status              MutationStatus
		wire                string
		applied, classified bool
	}{
		{MutationUnknown, "", false, false},
		{MutationNone, "none", false, true},
		{MutationApplied, "applied", true, true},
		{MutationRefused, "refused", false, true},
		{MutationFailed, "failed", false, true},
		// A command tool can change the workspace without looking. Unobserved
		// says so: a producer spoke, and nothing was shown to be applied.
		{MutationUnobserved, "unobserved", false, true},
	} {
		if string(tc.status) != tc.wire || tc.status.Applied() != tc.applied || tc.status.Classified() != tc.classified {
			t.Errorf("MutationStatus %q: applied %v, classified %v; want wire %q, applied %v, classified %v",
				tc.status, tc.status.Applied(), tc.status.Classified(), tc.wire, tc.applied, tc.classified)
		}
	}
	// A value from a producer of another version, or with its case or spacing
	// changed, fails closed.
	for _, s := range []MutationStatus{"APPLIED", "Applied", " applied", "applied ", "applied_maybe", "yes", "unknown"} {
		if s.Applied() || s.Classified() {
			t.Errorf("MutationStatus(%q) reads as applied or classified", s)
		}
	}
}

// Only "passed" counts as passed. not_run ("could not check") and
// not_applicable ("nothing to check") are separate states, and neither is a
// pass.
func TestValidationStatusValues(t *testing.T) {
	for _, tc := range []struct {
		status             ValidationStatus
		wire               string
		passed, classified bool
	}{
		{ValidationUnknown, "", false, false},
		{ValidationNotRun, "not_run", false, true},
		{ValidationNotApplicable, "not_applicable", false, true},
		{ValidationPassed, "passed", true, true},
		{ValidationFailed, "failed", false, true},
	} {
		if string(tc.status) != tc.wire || tc.status.Passed() != tc.passed || tc.status.Classified() != tc.classified {
			t.Errorf("ValidationStatus %q: passed %v, classified %v; want wire %q, passed %v, classified %v",
				tc.status, tc.status.Passed(), tc.status.Classified(), tc.wire, tc.passed, tc.classified)
		}
	}
	for _, s := range []ValidationStatus{"PASSED", "Passed", " passed", "passed_ish", "ok", "unknown", "not-run"} {
		if s.Passed() || s.Classified() {
			t.Errorf("ValidationStatus(%q) reads as passed or classified", s)
		}
	}
}

// A syntax pass is not a verified task: the kind keeps the two apart.
// Structural is its own kind. The write_file handler runs the syntax check
// first, so a structural refusal means that the syntax check passed.
func TestValidationKindValues(t *testing.T) {
	for _, tc := range []struct {
		kind       ValidationKind
		wire       string
		classified bool
	}{
		{ValidationKindUnknown, "", false},
		{ValidationKindNone, "none", true},
		{ValidationKindSyntax, "syntax", true},
		{ValidationKindStructural, "structural", true},
		{ValidationKindExecution, "execution", true},
	} {
		if string(tc.kind) != tc.wire || tc.kind.Classified() != tc.classified {
			t.Errorf("ValidationKind %q: classified %v; want wire %q, classified %v",
				tc.kind, tc.kind.Classified(), tc.wire, tc.classified)
		}
	}
	for _, k := range []ValidationKind{"STRUCTURAL", "Structural", " structural"} {
		if k.Classified() {
			t.Errorf("ValidationKind(%q) reads as classified", k)
		}
	}
}

// A result is classified only when all three facts are set. A payload from an
// older producer has none of them and decodes to Unknown, not to "nothing was
// done". A check with no outcome is not classified either.
func TestAResultIsClassifiedOnlyWhenEveryFactIsSet(t *testing.T) {
	var legacy ToolResult
	if err := json.Unmarshal([]byte(`{"success":true}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.MutationStatus != MutationUnknown || legacy.ValidationKind != ValidationKindUnknown ||
		legacy.ValidationStatus != ValidationUnknown {
		t.Errorf("a legacy payload decoded to %+v, want every fact unknown", legacy)
	}
	for _, tc := range []struct {
		name       string
		res        ToolResult
		classified bool
	}{
		{"zero value", ToolResult{}, false},
		{"legacy payload", legacy, false},
		{"check without outcome", ToolResult{MutationStatus: MutationApplied, ValidationKind: ValidationKindSyntax}, false},
		{"explicit none", ToolResult{MutationStatus: MutationNone, ValidationKind: ValidationKindNone,
			ValidationStatus: ValidationNotRun}, true},
		{"structural refusal", ToolResult{MutationStatus: MutationRefused, ValidationKind: ValidationKindStructural,
			ValidationStatus: ValidationFailed}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.res.Classified() != tc.classified {
				t.Errorf("Classified() = %v, want %v: %+v", tc.res.Classified(), tc.classified, tc.res)
			}
		})
	}
}

// `success` keeps its shape on the wire. The new fields are omitted when
// unset, and explicit none and not_run go out under their own names.
func TestToolResultJSON(t *testing.T) {
	for _, tc := range []struct {
		name          string
		res           ToolResult
		want, omitted []string
	}{
		{"legacy success", ToolResult{Success: true},
			[]string{`"success":true`},
			[]string{"mutation_status", "validation_status", "validation_kind"}},
		{"explicit none", ToolResult{Success: true, MutationStatus: MutationNone,
			ValidationKind: ValidationKindNone, ValidationStatus: ValidationNotRun},
			[]string{`"mutation_status":"none"`, `"validation_kind":"none"`, `"validation_status":"not_run"`},
			nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.res)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(b), want) {
					t.Errorf("missing %s in %s", want, b)
				}
			}
			for _, unwanted := range tc.omitted {
				if strings.Contains(string(b), unwanted) {
					t.Errorf("unset %s is on the wire: %s", unwanted, b)
				}
			}
		})
	}
}

// The SSE projections are explicit key maps, not ToolResult serialization, so
// internal classification cannot reach the wire by accident. This pins the
// legacy key sets: once producers start populating the new fields, a leak
// would show up here rather than in a consumer.
// The wire boundary, checked structurally rather than by text matching.
//
// ToolResult is never serialized: every SSE emitter builds an explicit map
// literal. So the internal classification states round-trip perfectly inside
// the process, while production SSE deliberately does not expose them. This
// test parses the agent loop's files and inspects the executable map literals, so a key
// added inside a comment or a string cannot fool it, and a key added in real
// code cannot hide from it.
func TestSSEProjectionsExposeOnlyLegacyKeys(t *testing.T) {
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range agentFiles {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, file)
	}
	inspectAll := func(visit func(ast.Node) bool) {
		for _, file := range files {
			ast.Inspect(file, visit)
		}
	}

	classification := map[string]bool{
		"mutation_status": true, "validation_kind": true,
		"validation_status": true, "validation_detail": true,
	}
	wantLegacy := []string{"tool", "success", "data", "error", "elapsed"}

	var sites int
	inspectAll(func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Stream" {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || lit.Value != `"tool_result"` {
			return true
		}
		composite, ok := call.Args[1].(*ast.CompositeLit)
		if !ok {
			t.Errorf("%s: tool_result payload is not a map literal; this guard "+
				"can no longer prove what reaches the wire",
				fset.Position(call.Pos()))
			return true
		}
		sites++

		keys := map[string]bool{}
		for _, elt := range composite.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				t.Errorf("%s: non key-value element in the tool_result payload",
					fset.Position(elt.Pos()))
				continue
			}
			k, ok := kv.Key.(*ast.BasicLit)
			if !ok || k.Kind != token.STRING {
				t.Errorf("%s: computed key in the tool_result payload; the wire "+
					"schema must stay statically inspectable",
					fset.Position(kv.Pos()))
				continue
			}
			name := strings.Trim(k.Value, `"`)
			keys[name] = true
			if classification[name] {
				t.Errorf("%s: emitter exposes internal classification key %q; "+
					"this phase must not expand the wire schema",
					fset.Position(kv.Pos()), name)
			}
		}
		// The canonical emitter carries the full legacy set. Others are
		// narrower by design, so only check the superset case.
		if keys["data"] || keys["elapsed"] {
			for _, want := range wantLegacy {
				if !keys[want] {
					t.Errorf("%s: legacy key %q disappeared from the tool_result "+
						"projection", fset.Position(call.Pos()), want)
				}
			}
		}
		return true
	})

	// Emit(Envelope{Type: EvtToolResult, Payload: ...}) is a SEPARATE emitter
	// on a different SSE stream (Emit -> defaultBroker.emit -> handleEvents),
	// not a delegate of the ctx.Stream calls above. EvtToolResult is the
	// literal "tool_result" (events.go), so this path publishes the same event
	// type and needs the same protection.
	var envelopes int
	inspectAll(func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok || ident.Name != "Emit" {
			return true
		}
		env, ok := call.Args[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		var isToolResult bool
		var payload *ast.CompositeLit
		for _, elt := range env.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			field, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			switch field.Name {
			case "Type":
				if id, ok := kv.Value.(*ast.Ident); ok && id.Name == "EvtToolResult" {
					isToolResult = true
				}
			case "Payload":
				if cl, ok := kv.Value.(*ast.CompositeLit); ok {
					payload = cl
				}
			}
		}
		if !isToolResult {
			return true
		}
		envelopes++
		if payload == nil {
			t.Errorf("%s: tool_result Envelope payload is not an inspectable "+
				"literal; this guard cannot prove what it publishes",
				fset.Position(call.Pos()))
			return true
		}
		for _, elt := range payload.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			k, ok := kv.Key.(*ast.BasicLit)
			if !ok || k.Kind != token.STRING {
				t.Errorf("%s: computed key in a tool_result Envelope payload",
					fset.Position(kv.Pos()))
				continue
			}
			if name := strings.Trim(k.Value, `"`); classification[name] {
				t.Errorf("%s: Envelope emitter exposes internal classification "+
					"key %q", fset.Position(kv.Pos()), name)
			}
		}
		return true
	})

	if sites < 3 {
		t.Fatalf("found %d ctx.Stream tool_result emitters via AST, expected at "+
			"least 3; if one was removed, update this guard deliberately", sites)
	}
	if envelopes < 1 {
		t.Fatalf("found %d Envelope tool_result emitters, expected at least 1; "+
			"if the path was removed, update this guard deliberately", envelopes)
	}
	t.Logf("structurally inspected %d ctx.Stream and %d Envelope tool_result emitters (%d total)",
		sites, envelopes, sites+envelopes)
}

// --- The model-facing boundary ----------------------------------------------
//
// A tool result reaches the model as the text of a tool message.
// ToolResult.MarshalText builds that text from an allowlist (ModelFacing), so
// the classification facts stay inside the process. These tests pin what
// crosses the allowlist.

// representativeResults holds one result of each mutation outcome, plus a
// warned write: a write that landed with a defect notice, where `data` and
// `error` are both set and the shape is most likely to drift.
var representativeResults = []struct {
	name string
	res  ToolResult
	want string
}{
	{"applied", ToolResult{Success: true, Data: json.RawMessage(`{"bytes_written":6}`),
		MutationStatus: MutationApplied, ValidationKind: ValidationKindSyntax,
		ValidationStatus: ValidationPassed, ValidationDetail: "parsed"},
		`{"success":true,"data":{"bytes_written":6}}`},
	{"refused", ToolResult{Success: false, Error: "edit_file: would not parse",
		MutationStatus: MutationRefused, ValidationKind: ValidationKindSyntax,
		ValidationStatus: ValidationFailed, ValidationDetail: "SyntaxError"},
		`{"success":false,"error":"edit_file: would not parse"}`},
	{"structural refusal", ToolResult{Success: false, Error: "write_file: unresolved call",
		MutationStatus: MutationRefused, ValidationKind: ValidationKindStructural,
		ValidationStatus: ValidationFailed, ValidationDetail: "unresolved call: render_template"},
		`{"success":false,"error":"write_file: unresolved call"}`},
	{"none", ToolResult{Success: false, Error: "file not found: gone.py",
		MutationStatus: MutationNone, ValidationKind: ValidationKindNone,
		ValidationStatus: ValidationNotApplicable},
		`{"success":false,"error":"file not found: gone.py"}`},
	{"not run", ToolResult{Success: true, Data: json.RawMessage(`{"listed":2}`),
		MutationStatus: MutationNone, ValidationKind: ValidationKindNone,
		ValidationStatus: ValidationNotRun},
		`{"success":true,"data":{"listed":2}}`},
	{"failed", ToolResult{Success: false, Error: "delete_file: permission denied",
		MutationStatus: MutationFailed, ValidationKind: ValidationKindNone,
		ValidationStatus: ValidationNotApplicable},
		`{"success":false,"error":"delete_file: permission denied"}`},
	{"unobserved", ToolResult{Success: true, Data: json.RawMessage(`{"exit_code":0}`),
		MutationStatus: MutationUnobserved, ValidationKind: ValidationKindNone,
		ValidationStatus: ValidationNotApplicable},
		`{"success":true,"data":{"exit_code":0}}`},
	{"warned write", ToolResult{Success: true,
		Data:  json.RawMessage(`{"bytes_written":41,"warning":"does not parse"}`),
		Error: "", MutationStatus: MutationApplied, ValidationKind: ValidationKindSyntax,
		ValidationStatus: ValidationFailed, ValidationDetail: "SyntaxError: invalid syntax"},
		`{"success":true,"data":{"bytes_written":41,"warning":"does not parse"}}`},
	{"v3 provenance survives", ToolResult{Success: true, Data: json.RawMessage(`{"ok":true}`),
		MutationStatus: MutationApplied, ValidationKind: ValidationKindSyntax,
		ValidationStatus: ValidationPassed,
		V3Used:           true, CandidatesTested: 4, WinningScore: 0.75, PhaseSolved: "phase1"},
		`{"success":true,"data":{"ok":true},"v3_used":true,"candidates_tested":4,"winning_score":0.75,"phase_solved":"phase1"}`},
}

func TestModelFacingTextCarriesNoClassification(t *testing.T) {
	banned := []string{"mutation_status", "validation_kind", "validation_status", "validation_detail"}
	for _, c := range representativeResults {
		t.Run(c.name, func(t *testing.T) {
			got := c.res.MarshalText()
			for _, key := range banned {
				if strings.Contains(got, key) {
					t.Errorf("model-facing text leaks %q: %s", key, got)
				}
			}
			if got != c.want {
				t.Errorf("model-facing text changed:\n got %s\nwant %s", got, c.want)
			}
			// The text is the plain JSON of the same result with the
			// classification fields left unset.
			legacy := ToolResult{Success: c.res.Success, Data: c.res.Data, Error: c.res.Error,
				V3Used: c.res.V3Used, CandidatesTested: c.res.CandidatesTested,
				WinningScore: c.res.WinningScore, PhaseSolved: c.res.PhaseSolved,
				VerificationEvidence: c.res.VerificationEvidence}
			legacyJSON, _ := json.Marshal(legacy)
			if got != string(legacyJSON) {
				t.Errorf("diverged from the legacy shape:\n got %s\nwant %s", got, legacyJSON)
			}
		})
	}
}

// The projection narrows the conversation, not the contract. The same result
// still marshals with every fact intact for internal use.
func TestInternalResultsStayFullyClassifiedBehindTheProjection(t *testing.T) {
	for _, c := range representativeResults {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(c.res)
			if err != nil {
				t.Fatal(err)
			}
			var back ToolResult
			if err := json.Unmarshal(b, &back); err != nil {
				t.Fatal(err)
			}
			if back.MutationStatus != c.res.MutationStatus ||
				back.ValidationKind != c.res.ValidationKind ||
				back.ValidationStatus != c.res.ValidationStatus ||
				back.ValidationDetail != c.res.ValidationDetail {
				t.Errorf("internal round trip lost classification: %+v", back)
			}
			if !back.Classified() {
				t.Errorf("internal result is not fully classified: %+v", back)
			}
		})
	}
}

// Structural inventory of every path a tool result can take into model
// context. Model context is ctx.Messages, so the guard is on AgentMessage
// construction plus any direct marshal of a result anywhere in the package.
// A new direct marshal, or a new tool message built from a result by some
// other route, fails here rather than silently reaching the prompt.
func TestEveryModelFacingSerializationSiteIsInventoried(t *testing.T) {
	fset := token.NewFileSet()

	// The approved ways a result becomes a tool message. Anything else must
	// be added here deliberately.
	approvedContent := map[string]bool{
		// The one path that serialises a ToolResult, now via ModelFacing.
		"result.MarshalText()": true,
		// Two bounces that never build a ToolResult at all: the legacy
		// two-key shape, which bounceContent extends with open_repair while
		// a file the session wrote does not parse (#214).
		"bounceContent(rejection, note)": true,
		"deniedToolMessage(denial)":      true,
	}
	resultish := regexp.MustCompile(`^&?\*?(result|res|toolResult|tr)$`)

	var toolMessages, marshals int
	for _, name := range nonTestGoFiles(t) {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CompositeLit:
				id, ok := node.Type.(*ast.Ident)
				if !ok || id.Name != "AgentMessage" {
					return true
				}
				var role, content string
				for _, elt := range node.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.Ident)
					if !ok {
						continue
					}
					var buf bytes.Buffer
					printer.Fprint(&buf, fset, kv.Value)
					switch key.Name {
					case "Role":
						role = strings.Trim(buf.String(), `"`)
					case "Content":
						content = buf.String()
					}
				}
				if role != "tool" {
					return true
				}
				toolMessages++
				if !approvedContent[content] {
					t.Errorf("%s: a tool message is built from an uninventoried "+
						"expression %s — every path into model context must be "+
						"listed in this guard", fset.Position(node.Pos()), content)
				}
			case *ast.CallExpr:
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if !ok || len(node.Args) != 1 {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "json" {
					return true
				}
				if sel.Sel.Name != "Marshal" && sel.Sel.Name != "MarshalIndent" {
					return true
				}
				var buf bytes.Buffer
				printer.Fprint(&buf, fset, node.Args[0])
				if !resultish.MatchString(buf.String()) {
					return true
				}
				marshals++
				t.Errorf("%s: json.%s(%s) marshals a tool result directly; use "+
					"ModelFacing() for the conversation, and name the variable "+
					"something else if this is internal",
					fset.Position(node.Pos()), sel.Sel.Name, buf.String())
			}
			return true
		})
	}

	// The transport half of the inventory. toWireMessages is where messages
	// become the request body, and it must stay a string passthrough: if it
	// ever reached for a ToolResult again, the projection above would be
	// bypassed at the last step.
	agentFile, err := parser.ParseFile(fset, "llm_call.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sawWire bool
	ast.Inspect(agentFile, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "toWireMessages" {
			return true
		}
		sawWire = true
		var buf bytes.Buffer
		printer.Fprint(&buf, fset, fn.Body)
		for _, banned := range []string{"ToolResult", "MarshalText", "json.Marshal", "ModelFacing"} {
			if strings.Contains(buf.String(), banned) {
				t.Errorf("toWireMessages references %s; the wire conversion must "+
					"stay a passthrough of already-projected content", banned)
			}
		}
		return false
	})
	if !sawWire {
		t.Fatal("toWireMessages not found; the transport path moved and this " +
			"guard no longer inventories it")
	}

	if toolMessages < 3 {
		t.Fatalf("found %d tool-role AgentMessage sites, expected at least 3; "+
			"if one was removed, update this guard deliberately", toolMessages)
	}
	t.Logf("inventoried %d tool-role message sites and %d direct result marshals",
		toolMessages, marshals)
}

// MarshalText must project rather than marshal the receiver. Checking the
// source keeps this true through a refactor that reintroduces the leak by
// changing one expression.
func TestMarshalTextProjectsRatherThanMarshallingTheReceiver(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "types.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "MarshalText" || fn.Recv == nil {
			return true
		}
		found = true
		var buf bytes.Buffer
		printer.Fprint(&buf, fset, fn.Body)
		body := buf.String()
		if !strings.Contains(body, "ModelFacing()") {
			t.Error("MarshalText no longer projects through ModelFacing")
		}
		if strings.Contains(body, "json.Marshal(r)") {
			t.Error("MarshalText marshals the whole receiver again")
		}
		return false
	})
	if !found {
		t.Fatal("MarshalText not found in types.go")
	}
}

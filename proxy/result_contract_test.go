package main

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Stage B contract tests. These pin the FACTS a ToolResult reports, before
// any consumer is changed.
//
// The defect they guard against is the one the locked benchmark found:
// `Success` conflated "the tool ran", "bytes changed" and "the content is
// valid", so a warned invalid write and a validated one were
// indistinguishable, and a successful delete looked like productive work.
//
// Every new field must FAIL CLOSED at its zero value. A result that was
// never populated, or that carries a value this build does not recognise,
// must never read as applied or as passed.

func TestZeroValueResultIsNeitherAppliedNorValidated(t *testing.T) {
	var r ToolResult
	if r.MutationStatus.Applied() {
		t.Error("zero-value MutationStatus reads as applied")
	}
	if r.ValidationStatus.Passed() {
		t.Error("zero-value ValidationStatus reads as passed")
	}
	if r.ValidationKind != ValidationKindUnknown {
		t.Errorf("zero-value ValidationKind = %q, want unknown", r.ValidationKind)
	}
	if r.MutationStatus != MutationUnknown {
		t.Errorf("zero-value MutationStatus = %q, want unknown", r.MutationStatus)
	}
	if r.ValidationStatus != ValidationUnknown {
		t.Errorf("zero-value ValidationStatus = %q, want unknown", r.ValidationStatus)
	}
	if r.Classified() {
		t.Error("a zero-value result reports itself as fully classified")
	}
}

// An unrecognised value is not a success. This matters across a version skew
// where one side emits a status the other does not know.
func TestUnknownEnumValuesAreNotSuccess(t *testing.T) {
	for _, v := range []MutationStatus{"", "APPLIED", "applied_maybe", "yes", "unknown"} {
		if v == MutationApplied {
			continue
		}
		if v.Applied() {
			t.Errorf("MutationStatus(%q) reads as applied", v)
		}
	}
	for _, v := range []ValidationStatus{"", "PASSED", "passed_ish", "ok", "unknown"} {
		if v == ValidationPassed {
			continue
		}
		if v.Passed() {
			t.Errorf("ValidationStatus(%q) reads as passed", v)
		}
	}
}

// not_run and not_applicable are distinct and neither is passed. Collapsing
// them is how "we could not check" silently becomes "we checked".
func TestNotRunAndNotApplicableAreNotPassed(t *testing.T) {
	if ValidationNotRun.Passed() {
		t.Error("not_run reads as passed")
	}
	if ValidationNotApplicable.Passed() {
		t.Error("not_applicable reads as passed")
	}
	if ValidationFailed.Passed() {
		t.Error("failed reads as passed")
	}
	if ValidationNotRun == ValidationNotApplicable {
		t.Error("not_run and not_applicable must stay distinguishable")
	}
}

// Legacy wire compatibility: `success` keeps its meaning and its shape, and
// the new fields are additive and omitted when unset.
func TestLegacyWireSuccessUnchanged(t *testing.T) {
	b, err := json.Marshal(ToolResult{Success: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"success":true`) {
		t.Fatalf("legacy success field changed shape: %s", b)
	}
	for _, unwanted := range []string{"mutation_status", "validation_status", "validation_kind"} {
		if strings.Contains(string(b), unwanted) {
			t.Errorf("unset %s must be omitted from the wire, got: %s", unwanted, b)
		}
	}
}

func TestNewFieldsRoundTrip(t *testing.T) {
	in := ToolResult{
		Success:          false,
		MutationStatus:   MutationRefused,
		ValidationKind:   ValidationKindSyntax,
		ValidationStatus: ValidationFailed,
		ValidationDetail: "SyntaxError: invalid syntax (line 4)",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out ToolResult
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.MutationStatus != MutationRefused || out.ValidationStatus != ValidationFailed ||
		out.ValidationKind != ValidationKindSyntax || out.ValidationDetail != in.ValidationDetail {
		t.Fatalf("round trip lost fields: %+v", out)
	}
}

// A payload from an older producer carries no new fields at all. It must
// decode to the fail-closed zero values rather than to applied/passed.
func TestLegacyPayloadDecodesFailClosed(t *testing.T) {
	var out ToolResult
	if err := json.Unmarshal([]byte(`{"success":true}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.MutationStatus.Applied() || out.ValidationStatus.Passed() {
		t.Fatalf("a legacy payload decoded as applied/validated: %+v", out)
	}
	// The distinction the producer audit depends on: an unmigrated producer
	// must NOT look like one that intentionally did nothing.
	if out.MutationStatus != MutationUnknown {
		t.Errorf("legacy payload MutationStatus = %q, want unknown", out.MutationStatus)
	}
	if out.ValidationStatus != ValidationUnknown {
		t.Errorf("legacy payload ValidationStatus = %q, want unknown", out.ValidationStatus)
	}
	if out.Classified() {
		t.Error("a legacy payload reports itself as fully classified")
	}
}

// Syntax validation is labelled as such. It is not evidence that the task
// was verified, and the kind is what keeps those separable.
func TestSyntaxPassIsLabelledSyntaxNotTaskVerification(t *testing.T) {
	r := ToolResult{
		Success: true, MutationStatus: MutationApplied,
		ValidationKind: ValidationKindSyntax, ValidationStatus: ValidationPassed,
	}
	if r.ValidationKind != ValidationKindSyntax {
		t.Fatalf("ValidationKind = %q, want syntax", r.ValidationKind)
	}
	if !r.ValidationStatus.Passed() {
		t.Fatal("an explicit syntax pass must read as passed")
	}
}

// Explicit none/not_run are intentional states and must survive the wire as
// their named strings, distinguishable from Unknown in both directions.
func TestExplicitNoneIsDistinctFromUnknown(t *testing.T) {
	in := ToolResult{
		Success: true, MutationStatus: MutationNone,
		ValidationKind: ValidationKindNone, ValidationStatus: ValidationNotRun,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"mutation_status":"none"`, `"validation_kind":"none"`,
		`"validation_status":"not_run"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in %s", want, b)
		}
	}
	var out ToolResult
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.MutationStatus == MutationUnknown || out.ValidationStatus == ValidationUnknown {
		t.Fatal("explicit none/not_run collapsed to unknown on the round trip")
	}
	if !out.Classified() {
		t.Fatal("an explicitly-none result must count as classified")
	}
}

// A claimed check with no outcome is not a classified result.
func TestSyntaxKindWithUnknownStatusIsUnclassified(t *testing.T) {
	r := ToolResult{MutationStatus: MutationApplied, ValidationKind: ValidationKindSyntax}
	if r.Classified() {
		t.Fatal("syntax kind with an unknown status must not count as classified")
	}
}

func TestMalformedValuesAreUnclassifiedAndNotSuccess(t *testing.T) {
	for _, m := range []MutationStatus{"", "APPLIED", "Applied", " applied", "applied "} {
		if m.Applied() || m.Classified() {
			t.Errorf("MutationStatus(%q) leaked through", m)
		}
	}
	for _, v := range []ValidationStatus{"", "PASSED", "Passed", " passed", "not-run"} {
		if v.Passed() || v.Classified() {
			t.Errorf("ValidationStatus(%q) leaked through", v)
		}
	}
}

// MutationUnobserved exists because command tools can change the workspace
// without the harness ever looking. run_command performs no pre/post state
// comparison, so `sed -i`, a build step, or `python fix.py` mutates the tree
// invisibly. Reporting that as MutationNone would be a false claim -- "nothing
// changed" is a fact this producer does not have. Unobserved says the true
// thing: side effects were not measured.
func TestUnobservedIsClassifiedButNotApplied(t *testing.T) {
	if !MutationUnobserved.Classified() {
		t.Error("Unobserved must be Classified: it is a current producer speaking")
	}
	if MutationUnobserved.Applied() {
		t.Error("Unobserved must not satisfy Applied: nothing was measured")
	}
	if MutationUnobserved == MutationUnknown {
		t.Error("Unobserved must be distinct from Unknown (unmigrated)")
	}
	if MutationUnobserved == MutationNone {
		t.Error("Unobserved must be distinct from None (measured, nothing changed)")
	}
}

// All four states stay mutually distinguishable across the wire.
func TestUnknownNoneAppliedUnobservedAreFourDistinctStates(t *testing.T) {
	seen := map[MutationStatus]bool{}
	for _, m := range []MutationStatus{MutationUnknown, MutationNone, MutationApplied, MutationUnobserved} {
		if seen[m] {
			t.Fatalf("duplicate wire value for %q", m)
		}
		seen[m] = true
	}
	if MutationUnobserved != "unobserved" {
		t.Fatalf("MutationUnobserved = %q, want \"unobserved\"", MutationUnobserved)
	}
	var out ToolResult
	if err := json.Unmarshal([]byte(`{"success":true,"mutation_status":"unobserved"}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.MutationStatus != MutationUnobserved || out.MutationStatus.Applied() {
		t.Fatalf("unobserved did not survive decode as a non-applied state: %+v", out)
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
// test parses agent.go and inspects the executable map literals, so a key
// added inside a comment or a string cannot fool it, and a key added in real
// code cannot hide from it.
func TestSSEProjectionsExposeOnlyLegacyKeys(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "agent.go", nil, 0)
	if err != nil {
		t.Fatalf("parse agent.go: %v", err)
	}

	classification := map[string]bool{
		"mutation_status": true, "validation_kind": true,
		"validation_status": true, "validation_detail": true,
	}
	wantLegacy := []string{"tool", "success", "data", "error", "elapsed"}

	var sites int
	ast.Inspect(file, func(n ast.Node) bool {
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
	ast.Inspect(file, func(n ast.Node) bool {
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

// The states themselves round-trip inside the process. Wire non-exposure is a
// deliberate boundary, not a limitation of the contract.
func TestClassificationRoundTripsInternallyEvenThoughSSEOmitsIt(t *testing.T) {
	for _, m := range []MutationStatus{MutationNone, MutationApplied,
		MutationRefused, MutationFailed, MutationUnobserved} {
		b, err := json.Marshal(ToolResult{MutationStatus: m,
			ValidationKind: ValidationKindSyntax, ValidationStatus: ValidationFailed})
		if err != nil {
			t.Fatal(err)
		}
		var out ToolResult
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		if out.MutationStatus != m || !out.Classified() {
			t.Fatalf("internal round trip lost %q: %+v", m, out)
		}
	}
}

// Structural validation is a distinct kind, not a flavour of syntax. In the
// write_file handler the syntax gate runs FIRST and a structural rejection
// therefore means syntax PASSED on those exact bytes -- labelling it
// syntax/failed would assert the opposite of what happened.
func TestStructuralIsARecognizedValidationKind(t *testing.T) {
	if !ValidationKindStructural.Classified() {
		t.Error("structural must be a classified validation kind")
	}
	if ValidationKindStructural == ValidationKindSyntax {
		t.Error("structural must be distinct from syntax")
	}
	if ValidationKindStructural != "structural" {
		t.Errorf("ValidationKindStructural = %q, want \"structural\"",
			ValidationKindStructural)
	}
	r := ToolResult{
		MutationStatus: MutationRefused,
		ValidationKind: ValidationKindStructural, ValidationStatus: ValidationFailed,
	}
	if !r.Classified() {
		t.Error("refused + structural/failed must be a classified result")
	}
	if r.ValidationStatus.Passed() {
		t.Error("structural/failed must never read as passed")
	}
	if r.MutationStatus.Applied() {
		t.Error("a structural refusal must never read as applied")
	}
}

func TestMalformedStructuralKindFailsClosed(t *testing.T) {
	for _, k := range []ValidationKind{"", "STRUCTURAL", "Structural", " structural"} {
		if k.Classified() {
			t.Errorf("ValidationKind(%q) leaked through as classified", k)
		}
	}
}

func TestStructuralRoundTripsInternally(t *testing.T) {
	in := ToolResult{
		MutationStatus: MutationRefused,
		ValidationKind: ValidationKindStructural, ValidationStatus: ValidationFailed,
		ValidationDetail: "unresolved call: render_template",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out ToolResult
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.ValidationKind != ValidationKindStructural ||
		out.ValidationStatus != ValidationFailed ||
		out.ValidationDetail != in.ValidationDetail {
		t.Fatalf("structural round trip lost fields: %+v", out)
	}
}

// --- The model-facing boundary ----------------------------------------------
//
// The SSE guard above proved the wire never carried classification. The
// conversation did: ToolResult.MarshalText marshalled the whole struct into
// the tool message, so every branch that classified itself also put four new
// keys in front of the model. Phase 3A's claim of unchanged external
// behaviour held for disk and for SSE, and did not hold here.
//
// The projection is an allowlist. These tests pin what crosses it.

// representativeResults covers one of each mutation outcome plus the warning
// shape -- a write that landed with a defect notice, which is the case where
// `data` and `error` are both populated and most likely to drift.
func representativeResults() []struct {
	name string
	res  ToolResult
	want string
} {
	return []struct {
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
		{"none", ToolResult{Success: false, Error: "file not found: gone.py",
			MutationStatus: MutationNone, ValidationKind: ValidationKindNone,
			ValidationStatus: ValidationNotApplicable},
			`{"success":false,"error":"file not found: gone.py"}`},
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
}

func TestModelFacingTextCarriesNoClassification(t *testing.T) {
	banned := []string{"mutation_status", "validation_kind", "validation_status", "validation_detail"}
	for _, c := range representativeResults() {
		t.Run(c.name, func(t *testing.T) {
			got := c.res.MarshalText()
			for _, key := range banned {
				if strings.Contains(got, key) {
					t.Errorf("model-facing text leaks %q: %s", key, got)
				}
			}
			// Byte-identical to what the parent produced for the same
			// success/data/error, which is the same struct with the
			// classification fields at their zero values.
			if got != c.want {
				t.Errorf("model-facing text changed:\n got %s\nwant %s", got, c.want)
			}
			legacy := ToolResult{Success: c.res.Success, Data: c.res.Data, Error: c.res.Error,
				V3Used: c.res.V3Used, CandidatesTested: c.res.CandidatesTested,
				WinningScore: c.res.WinningScore, PhaseSolved: c.res.PhaseSolved,
				VerificationEvidence: c.res.VerificationEvidence}
			parent, _ := json.Marshal(legacy)
			if got != string(parent) {
				t.Errorf("diverged from the parent shape:\n got %s\nwant %s", got, parent)
			}
		})
	}
}

// The projection narrows the conversation, not the contract. The same result
// still marshals with every fact intact for internal use.
func TestInternalResultsStayFullyClassifiedBehindTheProjection(t *testing.T) {
	for _, c := range representativeResults() {
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
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}

	// The approved ways a result becomes a tool message. Anything else must
	// be added here deliberately.
	approvedContent := map[string]bool{
		// The one path that serialises a ToolResult, now via ModelFacing.
		"result.MarshalText()": true,
		// Two bounces that never build a ToolResult at all: they emit the
		// legacy two-key shape directly.
		"fmt.Sprintf(`{\"success\":false,\"error\":%q}`, rejection)":    true,
		"`{\"success\":false,\"error\":\"permission denied by user\"}`": true,
	}
	resultish := regexp.MustCompile(`^&?\*?(result|res|toolResult|tr)$`)

	var toolMessages, marshals int
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
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
	agentFile, err := parser.ParseFile(fset, "agent.go", nil, 0)
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

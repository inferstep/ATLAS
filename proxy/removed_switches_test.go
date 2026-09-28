package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// V3 runs on every request, and the feasibility answer is recorded, never
// enforced. bypass_v3, v3_mode and feasibility_mode existed to measure
// without V3 or to skip it; a measured configuration that differs from the
// shipped one is the drift the rule against switches exists to stop.

// A request that asks for a removed switch is refused, not silently run under
// the system it did not ask for. Values that ask for what always happens pass.
func TestARemovedSwitchIsRefused(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		bypass           *bool
		v3Mode, feasMode string
		refused          bool
	}{
		{nil, "", "", false},
		{&no, "full", "observe", false},
		{&yes, "", "", true},
		{nil, "off", "", true},
		{nil, "planner_only", "", true},
		{nil, "", "enforce", true},
	}
	for _, c := range cases {
		if got := removedSwitchRefusal(c.bypass, c.v3Mode, c.feasMode) != ""; got != c.refused {
			t.Errorf("bypass=%v v3_mode=%q feasibility_mode=%q: refused=%v, want %v",
				c.bypass, c.v3Mode, c.feasMode, got, c.refused)
		}
	}

	dir := t.TempDir()
	t.Setenv("ATLAS_WORKSPACE_DIR", dir)
	body, _ := json.Marshal(map[string]interface{}{
		"message": "add a flag", "working_dir": dir, "bypass_v3": true})
	rec := httptest.NewRecorder()
	handleAgent(rec, httptest.NewRequest(http.MethodPost, "/v1/agent", strings.NewReader(string(body))))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "removed") {
		t.Errorf("bypass_v3=true got %d %q, want a 400 that says it was removed", rec.Code, rec.Body.String())
	}
}

// Nothing in the product reads a switch that turns V3 or its gates off.
func TestNoProductionCodeTurnsV3Off(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	banned := map[string]bool{
		"BypassV3": true, "V3Mode": true, "V3Bypassed": true, "V3GenerationEnabled": true,
		"V3PlanningEnabled": true, "FeasibilityMode": true, "generationSkipped": true,
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(node ast.Node) bool {
			if id, ok := node.(*ast.Ident); ok && banned[id.Name] {
				t.Errorf("%s: %s reads %s", n, fset.Position(id.Pos()), id.Name)
			}
			return true
		})
	}
}

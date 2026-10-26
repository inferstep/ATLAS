package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func lensHealthServer(t *testing.T, lens map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":     "healthy",
			"subsystems": map[string]any{"lens": lens},
		})
	}))
}

func compatibleLensHealth() map[string]any {
	return map[string]any{
		"cost_field_loaded": true,
		"cost_field_dim":    3840,
		"embed_dim":         3840,
		"gx_loaded":         true,
		"cx_calibrated":     true,
		"gx_calibrated":     true,
		"self_test_pass":    true,
	}
}

// A lens that is not ready says why: no calibration for the selected model,
// G(x) artifacts missing, or artifacts built for another model, whose
// self-test error is passed on as the hint.
func TestProbeLensStatusReportsALensThatIsNotReady(t *testing.T) {
	for _, tc := range []struct {
		name        string
		change      map[string]any
		wantVerdict string // empty: not checked
		wantHint    string // empty: not checked
	}{
		{"not calibrated", map[string]any{"cx_calibrated": false}, "uncalibrated", ""},
		{"G(x) missing", map[string]any{"gx_loaded": false}, "incomplete-artifacts", ""},
		{"built for another model", map[string]any{
			"cost_field_loaded": false,
			"self_test_error":   "artifacts are for model-a, selected model is model-b",
		}, "", "artifacts are for model-a, selected model is model-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			health := compatibleLensHealth()
			for k, v := range tc.change {
				health[k] = v
			}
			srv := lensHealthServer(t, health)
			defer srv.Close()

			got := probeLensStatus(context.Background(), srv.URL)
			if tc.wantVerdict != "" && got.Verdict != tc.wantVerdict {
				t.Errorf("verdict = %q, want %q", got.Verdict, tc.wantVerdict)
			}
			if tc.wantHint != "" && got.Hint != tc.wantHint {
				t.Errorf("hint = %q, want %q", got.Hint, tc.wantHint)
			}
		})
	}
}

func TestProbeASAStatusRequiresMatchingModelMarker(t *testing.T) {
	dir := t.TempDir()
	vector := filepath.Join(dir, "ast_edit_steering.gguf")
	if err := os.WriteFile(vector, []byte("vector"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATLAS_CONTROL_VECTOR", vector)
	t.Setenv("ATLAS_MODEL_NAME", "selected-model")

	if got := probeASAStatus(); got.Verdict != "unverified" {
		t.Fatalf("without marker verdict = %q, want unverified", got.Verdict)
	}
	if err := os.WriteFile(vector+".model", []byte("other-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := probeASAStatus(); got.Verdict != "incompatible" {
		t.Fatalf("wrong marker verdict = %q, want incompatible", got.Verdict)
	}
	if err := os.WriteFile(vector+".model", []byte("selected-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := probeASAStatus(); got.Verdict != "active" {
		t.Fatalf("matching marker verdict = %q, want active", got.Verdict)
	}
}

// Threshold resolution is fail-closed: only the selected model's calibrated
// values may drive interventions.
func TestLensThresholdResolution(t *testing.T) {
	var bare lensPerStepResult
	if _, _, ok := bare.calibratedThresholds(); ok {
		t.Fatal("missing calibration must not produce intervention thresholds")
	}
	withT := lensPerStepResult{Thresholds: &lensThresholds{OffRails: 0.6, Low: 0.45, Severe: 0.3}}
	low, severe, ok := withT.calibratedThresholds()
	if !ok || low != 0.45 || severe != 0.3 {
		t.Fatalf("calibrated thresholds = (%v, %v, %v)", low, severe, ok)
	}
	invalid := lensPerStepResult{Thresholds: &lensThresholds{Low: 0.2, Severe: 0.3}}
	if _, _, ok := invalid.calibratedThresholds(); ok {
		t.Fatal("severe > low must be rejected")
	}
}

// The same scores are interpreted only against the selected model's own
// calibration, for a run of writes and for one severe write alike.
func TestAgentLensRegressionUsesPerModelThresholds(t *testing.T) {
	for _, tc := range []struct {
		name        string
		scores      []float64
		low, severe float64
		wantFired   bool
	}{
		{"a run below low", []float64{0.40, 0.39}, 0.45, 0.30, true},
		{"one write below severe", []float64{0.32}, 0.45, 0.35, true},
		{"one write above severe", []float64{0.32}, 0.2, 0.1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, fired := agentLensRegression(tc.scores, tc.low, tc.severe); fired != tc.wantFired {
				t.Errorf("fired = %v, want %v", fired, tc.wantFired)
			}
		})
	}
}

func dimByName(dims []StatusDimension, name string) StatusDimension {
	for _, d := range dims {
		if d.Name == name {
			return d
		}
	}
	return StatusDimension{}
}

func TestBuildDimensionsSevenRows(t *testing.T) {
	dims := buildDimensions(LensStatus{}, ASAStatus{Verdict: "missing"})
	want := []string{"model_runtime", "direct_agent", "lens_identity",
		"lens_scoring", "lens_calibration", "lens_intervention", "asa"}
	if len(dims) != len(want) {
		t.Fatalf("expected %d dimensions, got %d", len(want), len(dims))
	}
	for i, n := range want {
		if dims[i].Name != n {
			t.Errorf("dimension %d = %q, want %q", i, dims[i].Name, n)
		}
	}
}

// The agent runs only while the lens can score (lens_required.go), and the
// status says so.
func TestDirectAgentIsBlockedWhileTheLensCannotScore(t *testing.T) {
	for verdict, want := range map[string]string{
		"supported": "supported", "uncalibrated": "supported",
		"unreachable": "blocked", "no-artifacts": "blocked",
		"dim-mismatch": "blocked", "incomplete-artifacts": "blocked", "drifted": "blocked",
		"self-test-failed": "blocked", "model-server-unreachable": "blocked",
	} {
		dims := buildDimensions(LensStatus{Verdict: verdict}, ASAStatus{Verdict: "missing"})
		d := dimByName(dims, "direct_agent")
		if d.Status != want {
			t.Errorf("%s: direct_agent %q, want %q", verdict, d.Status, want)
		}
		if want == "blocked" && !strings.Contains(d.Detail, verdict) {
			t.Errorf("%s: the detail does not name why: %q", verdict, d.Detail)
		}
	}
}

// A lens that loaded its artifacts can still be unable to score: drifted,
// failing its self-test, or without llama-server.
func TestProbeLensStatusNamesALensThatCannotScore(t *testing.T) {
	for _, c := range []struct {
		name, verdict string
		mutate        func(lens map[string]any, top map[string]any)
		// The model server as the lens reports it: down only when llama-server is.
		modelRuntime string
	}{
		{"drifted", "drifted", func(l, _ map[string]any) { l["fingerprint_ok"] = false }, "supported"},
		{"self-test failed", "self-test-failed", func(l, _ map[string]any) { l["self_test_pass"] = false }, "supported"},
		{"llama down", "model-server-unreachable", func(_, top map[string]any) {
			top["llama_server"] = map[string]any{"reachable": false}
		}, "unreachable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			lens := compatibleLensHealth()
			subsystems := map[string]any{"lens": lens}
			c.mutate(lens, subsystems)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "degraded", "subsystems": subsystems})
			}))
			defer srv.Close()

			got := probeLensStatus(context.Background(), srv.URL)
			if got.Verdict != c.verdict || got.CanScore {
				t.Errorf("verdict %q can_score=%v, want %q and false", got.Verdict, got.CanScore, c.verdict)
			}
			dims := buildDimensions(got, ASAStatus{Verdict: "missing"})
			if d := dimByName(dims, "direct_agent"); d.Status != "blocked" {
				t.Errorf("direct_agent %q", d.Status)
			}
			if d := dimByName(dims, "model_runtime"); d.Status != c.modelRuntime {
				t.Errorf("model_runtime %q, want %q", d.Status, c.modelRuntime)
			}
		})
	}
	srv := lensHealthServer(t, compatibleLensHealth())
	defer srv.Close()
	if got := probeLensStatus(context.Background(), srv.URL); got.Verdict != "supported" || !got.CanScore {
		t.Errorf("a healthy lens: %q can_score=%v", got.Verdict, got.CanScore)
	}
}

// Interventions act only on the selected model's calibration. A lens that
// scores without it gives raw telemetry only ("neutral").
func TestLensInterventionIsActiveOnlyWhenCalibrated(t *testing.T) {
	for _, tc := range []struct {
		name             string
		lens             LensStatus
		wantCalibration  string
		wantIntervention string
	}{
		{"calibrated", LensStatus{
			Verdict: "supported", CostFieldLoaded: true, GxLoaded: true,
			CostFieldDim: 3840, EmbedDim: 3840, CxCalibrated: true, GxCalibrated: true,
		}, "calibrated", "active"},
		{"not calibrated", LensStatus{
			Verdict: "uncalibrated", CostFieldLoaded: true, GxLoaded: true,
			CostFieldDim: 3840, EmbedDim: 3840, CxCalibrated: false, GxCalibrated: false,
		}, "uncalibrated", "neutral"},
		{"no artifacts", LensStatus{Verdict: "no-artifacts"}, "disabled", "disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dims := buildDimensions(tc.lens, ASAStatus{Verdict: "missing"})
			if d := dimByName(dims, "lens_calibration"); d.Status != tc.wantCalibration {
				t.Errorf("calibration = %q, want %q", d.Status, tc.wantCalibration)
			}
			if d := dimByName(dims, "lens_intervention"); d.Status != tc.wantIntervention {
				t.Errorf("intervention = %q, want %q", d.Status, tc.wantIntervention)
			}
		})
	}
}

func TestDimMismatchSurfaced(t *testing.T) {
	lens := LensStatus{
		Verdict: "dim-mismatch", CostFieldLoaded: true,
		CostFieldDim: 4096, EmbedDim: 3840,
	}
	dims := buildDimensions(lens, ASAStatus{Verdict: "missing"})
	if d := dimByName(dims, "lens_identity"); d.Status != "dim-mismatch" {
		t.Fatalf("identity = %q, want dim-mismatch", d.Status)
	}
}

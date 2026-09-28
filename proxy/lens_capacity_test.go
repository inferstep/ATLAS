package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A per-step answer the Lens gives when llama-server refused the embedding
// because the input exceeded the physical batch. It is a transport limit,
// typed as such, and never a score.
func typedCapacityFailureServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"enabled":      true,
			"scored":       false,
			"gx_available": false,
			"per_step":     []any{},
			"aggregate":    map[string]any{},
			"n_tokens":     0,
			"failure": map[string]any{
				"kind":            "embed_capacity",
				"input_tokens":    2055,
				"capacity_tokens": 2048,
			},
			"error": "EmbeddingCapacityError: per-step evaluation failed",
		})
	}))
}

func TestScoreContentForAgentTypedFailureIsNotAScore(t *testing.T) {
	srv := typedCapacityFailureServer(t)
	defer srv.Close()

	score, scored, down := scoreContentForAgent(context.Background(), srv.URL, "content")
	if scored {
		t.Fatalf("a typed capacity failure was treated as a score: %+v", score)
	}
	// An input the lens declines is not the lens being down: the run goes on.
	if down != "" {
		t.Fatalf("a capacity failure stopped the run: %s", down)
	}
	if score.Failure == nil || score.Failure.Kind != "embed_capacity" ||
		score.Failure.InputTokens != 2055 || score.Failure.CapacityTokens != 2048 {
		t.Fatalf("failure not carried through: %+v", score.Failure)
	}
}

func TestProbeLensStatusReadsTheEmbedCapacity(t *testing.T) {
	health := compatibleLensHealth()
	health["embed_capacity_tokens"] = 2048
	health["embed_capacity_source"] = "declared"
	srv := lensHealthServer(t, health)
	defer srv.Close()

	got := probeLensStatus(context.Background(), srv.URL)
	if got.EmbedCapacityTokens != 2048 || got.EmbedCapacitySource != "declared" {
		t.Fatalf("capacity not read: %+v", got)
	}
	if got.Verdict != "supported" {
		t.Fatalf("capacity must not change the artifact verdict, got %q", got.Verdict)
	}
}

func TestProbeLensStatusToleratesAnUnknownCapacity(t *testing.T) {
	health := compatibleLensHealth()
	health["embed_capacity_tokens"] = nil
	srv := lensHealthServer(t, health)
	defer srv.Close()

	got := probeLensStatus(context.Background(), srv.URL)
	if got.EmbedCapacityTokens != 0 {
		t.Fatalf("unknown capacity must read as 0, got %d", got.EmbedCapacityTokens)
	}
}

func supportedLens() LensStatus {
	return LensStatus{
		Verdict: "supported", CostFieldLoaded: true, GxLoaded: true,
		CostFieldDim: 3840, EmbedDim: 3840,
		CxCalibrated: true, GxCalibrated: true,
	}
}

func TestBuildDimensionsCapacityBelowTheGenerationCeilingIsPartial(t *testing.T) {
	t.Setenv("ATLAS_MAX_TOKENS", "8192")
	lens := supportedLens()
	lens.EmbedCapacityTokens = 2048
	lens.EmbedCapacitySource = "declared"

	d := dimByName(buildDimensions(lens, ASAStatus{Verdict: "supported"}), "lens_scoring")
	if d.Status != "partial" {
		t.Fatalf("lens_scoring = %q, want partial (%s)", d.Status, d.Detail)
	}
	for _, want := range []string{"2048", "8192", "ATLAS_UBATCH"} {
		if !strings.Contains(d.Detail, want) {
			t.Errorf("detail %q does not mention %s", d.Detail, want)
		}
	}
}

func TestBuildDimensionsCapacityCoveringTheCeilingStaysSupported(t *testing.T) {
	t.Setenv("ATLAS_MAX_TOKENS", "4096")
	lens := supportedLens()
	lens.EmbedCapacityTokens = 4096

	d := dimByName(buildDimensions(lens, ASAStatus{Verdict: "supported"}), "lens_scoring")
	if d.Status != "supported" {
		t.Fatalf("lens_scoring = %q, want supported (%s)", d.Status, d.Detail)
	}
}

func TestBuildDimensionsUnknownCapacityStaysSupported(t *testing.T) {
	t.Setenv("ATLAS_MAX_TOKENS", "8192")
	d := dimByName(buildDimensions(supportedLens(), ASAStatus{Verdict: "supported"}), "lens_scoring")
	if d.Status != "supported" {
		t.Fatalf("lens_scoring = %q, want supported when capacity is unknown", d.Status)
	}
}

func TestBuildDimensionsCapacityDoesNotTouchIntervention(t *testing.T) {
	t.Setenv("ATLAS_MAX_TOKENS", "8192")
	lens := supportedLens()
	lens.EmbedCapacityTokens = 2048
	dims := buildDimensions(lens, ASAStatus{Verdict: "supported"})
	if d := dimByName(dims, "lens_intervention"); d.Status != "active" {
		t.Fatalf("intervention = %q; a capacity limit is not a calibration defect", d.Status)
	}
	if d := dimByName(dims, "lens_calibration"); d.Status != "calibrated" {
		t.Fatalf("calibration = %q; a capacity limit is not a calibration defect", d.Status)
	}
}

// A per-step answer whose aggregate carries the non-standard token NaN:
// the shape an older Lens's json.dumps emits for a degenerate forward.
// encoding/json rejects the token, so the answer never becomes a score.
func TestScoreContentForAgentRejectsANonFiniteScore(t *testing.T) {
	for _, token := range []string{"NaN", "Infinity", "-Infinity"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"enabled":true,"scored":true,"gx_available":true,"n_tokens":3,` +
				`"aggregate":{"gx_score_min":` + token + `,"gx_score_mean":0.5},` +
				`"thresholds":{"off_rails":0.3,"low":0.4,"severe":0.2}}`))
		}))
		score, scored, _ := scoreContentForAgent(context.Background(), srv.URL, "content")
		srv.Close()
		if scored {
			t.Fatalf("%s was accepted as a score: %+v", token, score)
		}
	}
}

// Tests for the calibration badge renderer (PC-059).

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRenderCalibrationBadge_NilStatus_ShowsPlaceholder(t *testing.T) {
	got := renderCalibrationBadge(nil)
	if !strings.Contains(got, "cal") {
		t.Errorf("expected 'cal' placeholder, got %q", got)
	}
}

func TestRenderCalibrationBadge_BothSupported_ShowsCheck(t *testing.T) {
	s := &calibrationStatus{}
	s.Lens.Verdict = "supported"
	s.ASA.Verdict = "supported"
	got := renderCalibrationBadge(s)
	// ANSI codes wrap each badge — assert on the visible text content
	if !strings.Contains(got, "Lens ✓") {
		t.Errorf("expected 'Lens ✓' badge, got %q", got)
	}
	if !strings.Contains(got, "ASA ✓") {
		t.Errorf("expected 'ASA ✓' badge, got %q", got)
	}
}

// A lens with no artifacts cannot score, and the lens is required: requests
// are refused, so the badge is a failure, not a warning.
func TestRenderCalibrationBadge_NoArtifacts_ShowsFailure(t *testing.T) {
	s := &calibrationStatus{}
	s.Lens.Verdict = "no-artifacts"
	s.ASA.Verdict = "supported"
	got := renderCalibrationBadge(s)
	if !strings.Contains(got, "Lens ✗") {
		t.Errorf("expected 'Lens ✗' failure badge, got %q", got)
	}
	if !strings.Contains(got, "ASA ✓") {
		t.Errorf("expected 'ASA ✓' ok badge alongside the failure, got %q", got)
	}
}

func TestRenderCalibrationBadge_Uncalibrated_ShowsWarning(t *testing.T) {
	s := &calibrationStatus{}
	s.Lens.Verdict = "uncalibrated"
	s.ASA.Verdict = "unverified"
	got := renderCalibrationBadge(s)
	if !strings.Contains(got, "Lens ⚠") || !strings.Contains(got, "ASA ⚠") {
		t.Errorf("expected truthful warnings, got %q", got)
	}
}

func TestBadgeActionHint_UnverifiedASA_PointsAtCheck(t *testing.T) {
	s := &calibrationStatus{}
	s.Lens.Verdict = "supported"
	s.ASA.Verdict = "unverified"
	if got := badgeActionHint(s); !strings.Contains(got, "atlas asa check") {
		t.Errorf("expected verification hint, got %q", got)
	}
}

func TestRenderCalibrationBadge_Unreachable_ShowsFail(t *testing.T) {
	s := &calibrationStatus{}
	s.Lens.Verdict = "unreachable"
	s.ASA.Verdict = "missing"
	got := renderCalibrationBadge(s)
	if !strings.Contains(got, "Lens ✗") {
		t.Errorf("expected 'Lens ✗' fail badge, got %q", got)
	}
	if !strings.Contains(got, "ASA ⚠") {
		t.Errorf("expected 'ASA ⚠' warn badge for missing, got %q", got)
	}
}

func TestFetchCalibrationStatusCmd_RoundTripsRealResponse(t *testing.T) {
	// Spin up a mock proxy that returns a realistic CalibrationStatus
	// payload. The cmd should parse it into the right struct.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/calibration/status", func(w http.ResponseWriter, r *http.Request) {
		payload := map[string]any{
			"lens": map[string]any{
				"verdict":           "supported",
				"cost_field_loaded": true,
				"cost_field_dim":    4096,
				"embed_dim":         4096,
				"gx_loaded":         true,
				"hint":              "ready",
			},
			"asa": map[string]any{
				"verdict":        "missing",
				"vector_path":    "/models/ast_edit_steering.gguf",
				"vector_present": false,
				"hint":           "no control vector",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	msg := fetchCalibrationStatusCmd(srv.URL)()
	cmsg, ok := msg.(calibrationStatusMsg)
	if !ok {
		t.Fatalf("expected calibrationStatusMsg, got %T", msg)
	}
	if cmsg.err != nil {
		t.Fatalf("unexpected err: %v", cmsg.err)
	}
	if cmsg.status == nil {
		t.Fatal("expected non-nil status")
	}
	if cmsg.status.Lens.Verdict != "supported" {
		t.Errorf("Lens.Verdict = %q, want supported", cmsg.status.Lens.Verdict)
	}
	if cmsg.status.Lens.CostFieldDim != 4096 {
		t.Errorf("CostFieldDim = %d, want 4096", cmsg.status.Lens.CostFieldDim)
	}
	if cmsg.status.ASA.Verdict != "missing" {
		t.Errorf("ASA.Verdict = %q, want missing", cmsg.status.ASA.Verdict)
	}
	if cmsg.status.ASA.VectorPresent {
		t.Errorf("VectorPresent = true, want false")
	}
}

func TestFetchCalibrationStatusCmd_ProxyDown_ReturnsErr(t *testing.T) {
	// Point at a definitely-closed port.
	msg := fetchCalibrationStatusCmd("http://127.0.0.1:1")()
	cmsg, ok := msg.(calibrationStatusMsg)
	if !ok {
		t.Fatalf("expected calibrationStatusMsg, got %T", msg)
	}
	if cmsg.err == nil {
		t.Error("expected err for unreachable proxy, got nil")
	}
	if cmsg.status != nil {
		t.Error("expected nil status on err")
	}
}

func TestBadgeActionHint_BothSupported_Empty(t *testing.T) {
	s := &calibrationStatus{}
	s.Lens.Verdict = "supported"
	s.ASA.Verdict = "supported"
	if got := badgeActionHint(s); got != "" {
		t.Errorf("expected no hint when both supported, got %q", got)
	}
}

func TestBadgeActionHint_LensWarn_PointsAtBuildAndDocs(t *testing.T) {
	s := &calibrationStatus{}
	s.Lens.Verdict = "no-artifacts"
	s.ASA.Verdict = "supported"
	got := badgeActionHint(s)
	if !strings.Contains(got, "atlas lens build") {
		t.Errorf("lens-warn hint should suggest `atlas lens build`, got %q", got)
	}
	if !strings.Contains(got, "PUBLISHING.md") {
		t.Errorf("hint should reference docs/PUBLISHING.md, got %q", got)
	}
	if strings.Contains(got, "atlas asa build") {
		t.Errorf("ASA was supported, asa-build should NOT appear: %q", got)
	}
}

func TestBadgeActionHint_ASAWarn_PointsAtASABuild(t *testing.T) {
	s := &calibrationStatus{}
	s.Lens.Verdict = "supported"
	s.ASA.Verdict = "missing"
	got := badgeActionHint(s)
	if !strings.Contains(got, "atlas asa build") {
		t.Errorf("asa-warn hint should suggest `atlas asa build`, got %q", got)
	}
	if strings.Contains(got, "atlas lens build") {
		t.Errorf("Lens was supported, lens-build should NOT appear: %q", got)
	}
}

func TestBadgeActionHint_BothWarn_SuggestsBoth(t *testing.T) {
	s := &calibrationStatus{}
	s.Lens.Verdict = "dim-mismatch"
	s.ASA.Verdict = "missing"
	got := badgeActionHint(s)
	if !strings.Contains(got, "atlas lens build") || !strings.Contains(got, "atlas asa build") {
		t.Errorf("both-warn hint should mention both commands, got %q", got)
	}
}

func TestBadgeActionHint_Unreachable_NoBuildSuggestion(t *testing.T) {
	// When the lens is down the artifact may be fine — the service is.
	// Telling the user to "build" would be misleading; the lens is
	// required, so the pointer is atlas doctor.
	for _, v := range []string{"unreachable", "drifted", "self-test-failed",
		"model-server-unreachable"} {
		s := &calibrationStatus{}
		s.Lens.Verdict = v
		s.ASA.Verdict = "incompatible"
		got := badgeActionHint(s)
		if strings.Contains(got, "build") || !strings.Contains(got, "atlas doctor") {
			t.Errorf("%s: hint %q, want atlas doctor and no build suggestion", v, got)
		}
		if badge := renderOneBadge("Lens", v); !strings.Contains(badge, "✗") {
			t.Errorf("%s: badge %q, want a failure mark", v, badge)
		}
	}
}

// Missing or mismatched artifacts leave the lens unable to score, so requests
// are refused: a failure mark, a build pointer, and the reason. An
// uncalibrated lens scores, so it stays a warning.
func TestBadgeActionHint_MissingArtifactsSayRequestsAreRefused(t *testing.T) {
	for _, v := range []string{"no-artifacts", "incomplete-artifacts", "dim-mismatch"} {
		s := &calibrationStatus{}
		s.Lens.Verdict = v
		s.ASA.Verdict = "active"
		got := badgeActionHint(s)
		if !strings.Contains(got, "atlas lens build") || !strings.Contains(got, "refused") {
			t.Errorf("%s: hint %q, want atlas lens build and that requests are refused", v, got)
		}
		if badge := renderOneBadge("Lens", v); !strings.Contains(badge, "✗") {
			t.Errorf("%s: badge %q, want a failure mark", v, badge)
		}
	}
	s := &calibrationStatus{}
	s.Lens.Verdict = "uncalibrated"
	s.ASA.Verdict = "active"
	if got := badgeActionHint(s); strings.Contains(got, "refused") {
		t.Errorf("uncalibrated: hint %q says requests are refused; an uncalibrated lens scores", got)
	}
	if badge := renderOneBadge("Lens", "uncalibrated"); !strings.Contains(badge, "⚠") {
		t.Errorf("uncalibrated: badge %q, want a warning mark", badge)
	}
}

func TestRenderCalibrationBadge_WarnIncludesHint(t *testing.T) {
	s := &calibrationStatus{}
	s.Lens.Verdict = "no-artifacts"
	s.ASA.Verdict = "supported"
	got := renderCalibrationBadge(s)
	if !strings.Contains(got, "atlas lens build") {
		t.Errorf("rendered badge should append actionable hint when lens warn, got %q", got)
	}
	if !strings.Contains(got, "PUBLISHING.md") {
		t.Errorf("rendered badge should reference docs, got %q", got)
	}
}

// PC-061 round-2 fix: badge retry tick must converge once we have a
// real status and must stop after the retry cap.
func TestScheduleCalibrationRetry_FiresRetryMsgAfterDelay(t *testing.T) {
	// 1ms tick lets us assert the message type without slowing the
	// suite down. The shape of the message is what we care about; the
	// model decides what to do with it.
	cmd := scheduleCalibrationRetry(time.Millisecond)
	if cmd == nil {
		t.Fatal("scheduleCalibrationRetry returned nil cmd")
	}
	msg := cmd()
	if _, ok := msg.(calibrationRetryMsg); !ok {
		t.Errorf("expected calibrationRetryMsg, got %T", msg)
	}
}

func TestCalibrationRetryConstants_BoundedAndReasonable(t *testing.T) {
	// Lock the retry budget so changes are intentional. Too low and a
	// slow proxy startup never converges; too high and a permanently-
	// down proxy keeps pinging forever.
	if maxCalibrationRetries < 3 || maxCalibrationRetries > 10 {
		t.Errorf("maxCalibrationRetries (%d) outside reasonable range [3,10]",
			maxCalibrationRetries)
	}
	if calibrationRetryInterval < 2*time.Second ||
		calibrationRetryInterval > 30*time.Second {
		t.Errorf("calibrationRetryInterval (%v) outside reasonable range",
			calibrationRetryInterval)
	}
}

// New: regression test for the badge-frozen bug. The original
// implementation locked in the first response from the proxy forever —
// users who downloaded lens/asa artifacts mid-session and restarted
// the lens container saw the warn badge persist until they restarted
// the TUI. The fix: scheduleCalibrationRefresh produces a refresh msg
// that fires periodically forever, separate from the bounded retry
// mechanism that handles startup races.
func TestScheduleCalibrationRefresh_FiresRefreshMsgAfterDelay(t *testing.T) {
	cmd := scheduleCalibrationRefresh(time.Millisecond)
	if cmd == nil {
		t.Fatal("scheduleCalibrationRefresh returned nil cmd")
	}
	msg := cmd()
	if _, ok := msg.(calibrationRefreshMsg); !ok {
		t.Errorf("expected calibrationRefreshMsg, got %T", msg)
	}
}

func TestCalibrationRefreshInterval_LongerThanRetry(t *testing.T) {
	// Refresh runs forever after first success — must be slow enough
	// that the cost is trivial (~120 calls/hour at 30s). Also must be
	// longer than retry interval so the two mechanisms don't overlap.
	if calibrationRefreshInterval < calibrationRetryInterval {
		t.Errorf("refresh (%v) should be longer than retry (%v) — "+
			"refresh is the steady-state long-poll, retry is the "+
			"fast startup-race handler",
			calibrationRefreshInterval, calibrationRetryInterval)
	}
	if calibrationRefreshInterval < 15*time.Second ||
		calibrationRefreshInterval > 5*time.Minute {
		t.Errorf("calibrationRefreshInterval (%v) outside reasonable range "+
			"[15s, 5m] — too short hammers proxy, too long defeats the "+
			"point of auto-converging",
			calibrationRefreshInterval)
	}
}

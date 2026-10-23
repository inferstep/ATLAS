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

// The badge shows one mark per subsystem: ✓ ready, ⚠ a warning, ✗ requests
// are refused (the lens is required). When something needs attention, the
// hint follows on the same line.
func TestRenderCalibrationBadge(t *testing.T) {
	if got := renderCalibrationBadge(nil); !strings.Contains(got, "cal") {
		t.Errorf("before the first answer the badge is %q, want the 'cal' placeholder", got)
	}
	cases := []struct {
		name, lens, asa string
		want            []string
	}{
		{"both supported", "supported", "supported", []string{"Lens ✓", "ASA ✓"}},
		// A lens with no artifacts cannot score: a failure, not a warning.
		{"lens without artifacts", "no-artifacts", "supported",
			[]string{"Lens ✗", "ASA ✓", "atlas lens build", "PUBLISHING.md"}},
		{"lens with incomplete artifacts", "incomplete-artifacts", "active", []string{"Lens ✗"}},
		{"lens with mismatched dimensions", "dim-mismatch", "active", []string{"Lens ✗"}},
		{"uncalibrated lens, unverified ASA", "uncalibrated", "unverified", []string{"Lens ⚠", "ASA ⚠"}},
		{"unreachable lens, missing ASA", "unreachable", "missing", []string{"Lens ✗", "ASA ⚠"}},
		{"drifted lens", "drifted", "incompatible", []string{"Lens ✗"}},
		{"lens failed its self-test", "self-test-failed", "incompatible", []string{"Lens ✗"}},
		{"lens cannot reach the model server", "model-server-unreachable", "incompatible", []string{"Lens ✗"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &calibrationStatus{}
			s.Lens.Verdict = c.lens
			s.ASA.Verdict = c.asa
			got := renderCalibrationBadge(s)
			for _, want := range c.want {
				if !strings.Contains(got, want) {
					t.Errorf("badge %q, want it to contain %q", got, want)
				}
			}
		})
	}
}

// The hint says what to do: build what is missing, check what is unverified,
// or run atlas doctor when the lens is down (its artifacts may be fine, so a
// build would mislead). While the lens cannot score, requests are refused and
// the hint says so; an uncalibrated lens still scores.
func TestBadgeActionHint(t *testing.T) {
	cases := []struct {
		name, lens, asa string
		want            []string // texts the hint contains; none means no hint at all
		notWant         []string
	}{
		{"both supported", "supported", "supported", nil, nil},
		{"ASA unverified", "supported", "unverified", []string{"atlas asa check"}, nil},
		{"ASA missing", "supported", "missing", []string{"atlas asa build"}, []string{"atlas lens build"}},
		{"lens without artifacts", "no-artifacts", "supported",
			[]string{"atlas lens build", "PUBLISHING.md", "refused"}, []string{"atlas asa build"}},
		{"lens with incomplete artifacts", "incomplete-artifacts", "active", []string{"atlas lens build", "refused"}, nil},
		{"lens with mismatched dimensions", "dim-mismatch", "active", []string{"atlas lens build", "refused"}, nil},
		{"lens and ASA both need a build", "dim-mismatch", "missing", []string{"atlas lens build", "atlas asa build"}, nil},
		{"uncalibrated lens", "uncalibrated", "active", []string{"atlas lens build"}, []string{"refused"}},
		{"unreachable lens", "unreachable", "incompatible", []string{"atlas doctor"}, []string{"build"}},
		{"drifted lens", "drifted", "incompatible", []string{"atlas doctor"}, []string{"build"}},
		{"lens failed its self-test", "self-test-failed", "incompatible", []string{"atlas doctor"}, []string{"build"}},
		{"lens cannot reach the model server", "model-server-unreachable", "incompatible", []string{"atlas doctor"}, []string{"build"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &calibrationStatus{}
			s.Lens.Verdict = c.lens
			s.ASA.Verdict = c.asa
			got := badgeActionHint(s)
			if len(c.want) == 0 && got != "" {
				t.Errorf("hint %q, want none", got)
			}
			for _, want := range c.want {
				if !strings.Contains(got, want) {
					t.Errorf("hint %q, want it to contain %q", got, want)
				}
			}
			for _, notWant := range c.notWant {
				if strings.Contains(got, notWant) {
					t.Errorf("hint %q, want it without %q", got, notWant)
				}
			}
		})
	}
}

func TestFetchCalibrationStatusCmd_RoundTripsRealResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/calibration/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
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
		})
	}))
	defer srv.Close()

	got := fetchCalibrationStatusCmd(srv.URL)()
	msg, ok := got.(calibrationStatusMsg)
	if !ok {
		t.Fatalf("got %T, want calibrationStatusMsg", got)
	}
	if msg.err != nil || msg.status == nil {
		t.Fatalf("got status %v and error %v, want a status and no error", msg.status, msg.err)
	}
	if msg.status.Lens.Verdict != "supported" || msg.status.Lens.CostFieldDim != 4096 {
		t.Errorf("lens = %+v, want verdict supported and cost field dim 4096", msg.status.Lens)
	}
	if msg.status.ASA.Verdict != "missing" || msg.status.ASA.VectorPresent {
		t.Errorf("ASA = %+v, want verdict missing and no vector present", msg.status.ASA)
	}
}

func TestFetchCalibrationStatusCmd_ProxyDown_ReturnsErr(t *testing.T) {
	// A closed port on this machine stands in for a proxy that is down.
	got := fetchCalibrationStatusCmd("http://127.0.0.1:1")()
	msg, ok := got.(calibrationStatusMsg)
	if !ok {
		t.Fatalf("got %T, want calibrationStatusMsg", got)
	}
	if msg.err == nil || msg.status != nil {
		t.Errorf("got status %v and error %v, want no status and an error", msg.status, msg.err)
	}
}

// The retry tick (fast, bounded, while the proxy starts) and the refresh tick
// (slow, forever, so the badge follows the truth) each send their own message.
func TestCalibrationTicksSendTheirOwnMessage(t *testing.T) {
	retry := scheduleCalibrationRetry(time.Millisecond)()
	if _, ok := retry.(calibrationRetryMsg); !ok {
		t.Errorf("the retry tick sent %T, want calibrationRetryMsg", retry)
	}
	refresh := scheduleCalibrationRefresh(time.Millisecond)()
	if _, ok := refresh.(calibrationRefreshMsg); !ok {
		t.Errorf("the refresh tick sent %T, want calibrationRefreshMsg", refresh)
	}
}

// The bounds are deliberate. Too few or too slow retries never catch a slow
// proxy start; too many keep pinging a proxy that is down. The refresh must
// be cheap (about 120 calls an hour at 30s), fast enough to follow a fixed
// lens, and slower than the retry so the two do not overlap.
func TestCalibrationRetryAndRefreshBounds(t *testing.T) {
	if maxCalibrationRetries < 3 || maxCalibrationRetries > 10 {
		t.Errorf("maxCalibrationRetries = %d, want 3 to 10", maxCalibrationRetries)
	}
	if calibrationRetryInterval < 2*time.Second || calibrationRetryInterval > 30*time.Second {
		t.Errorf("calibrationRetryInterval = %v, want 2s to 30s", calibrationRetryInterval)
	}
	if calibrationRefreshInterval < 15*time.Second || calibrationRefreshInterval > 5*time.Minute {
		t.Errorf("calibrationRefreshInterval = %v, want 15s to 5m", calibrationRefreshInterval)
	}
	if calibrationRefreshInterval < calibrationRetryInterval {
		t.Errorf("refresh (%v) is faster than retry (%v)", calibrationRefreshInterval, calibrationRetryInterval)
	}
}

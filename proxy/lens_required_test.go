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

// The lens is required (lens_required.go, docs/adr/0011-the-lens-is-required.md).
// A request is not started while it cannot score, and a run whose lens stops
// scoring ends there, saying why.

// healthyLensHealth is the /health body of a lens that can score.
func healthyLensHealth() map[string]interface{} {
	return map[string]interface{}{
		"service": "geometric-lens", "status": "healthy",
		"subsystems": map[string]interface{}{
			"llama_server": map[string]interface{}{"reachable": true},
			"lens": map[string]interface{}{
				"cost_field_loaded": true, "gx_loaded": true,
				"cx_calibrated": true, "gx_calibrated": true,
				"self_test_pass": true, "fingerprint_ok": nil,
			},
		},
	}
}

func fakeLensServer(t *testing.T, readyStatus int, ready interface{}, health map[string]interface{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ready":
			w.WriteHeader(readyStatus)
			json.NewEncoder(w).Encode(ready)
		case "/health":
			json.NewEncoder(w).Encode(health)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLensReadinessNamesWhyTheLensCannotScore(t *testing.T) {
	lensField := func(k string, v interface{}) map[string]interface{} {
		h := healthyLensHealth()
		h["subsystems"].(map[string]interface{})["lens"].(map[string]interface{})[k] = v
		return h
	}
	llamaDown := healthyLensHealth()
	llamaDown["subsystems"].(map[string]interface{})["llama_server"] =
		map[string]interface{}{"reachable": false}
	for _, c := range []struct {
		name        string
		readyStatus int
		ready       interface{}
		health      map[string]interface{}
		want        string // "" = ready
	}{
		{"healthy", 200, map[string]bool{"ready": true}, healthyLensHealth(), ""},
		{"uncalibrated is up", 200, map[string]bool{"ready": true},
			lensField("cx_calibrated", false), ""},
		{"not ready, with a reason", 503,
			map[string]interface{}{"detail": map[string]interface{}{
				"reason": "lens model files missing — run `atlas lens build`"}},
			healthyLensHealth(), "not ready: lens model files missing"},
		{"not ready, llama down", 503,
			map[string]interface{}{"detail": map[string]interface{}{"llama_server": false}},
			healthyLensHealth(), "cannot reach llama-server"},
		{"no G(x)", 200, map[string]bool{"ready": true},
			lensField("gx_loaded", false), "no G(x) model"},
		{"drifted", 200, map[string]bool{"ready": true},
			lensField("fingerprint_ok", false), "drifted"},
		{"self-test failed", 200, map[string]bool{"ready": true},
			lensField("self_test_pass", false), "self-test failed"},
		{"llama down after boot", 200, map[string]bool{"ready": true},
			llamaDown, "cannot reach llama-server"},
	} {
		srv := fakeLensServer(t, c.readyStatus, c.ready, c.health)
		ok, why := probeLensReadiness(srv.URL)
		if c.want == "" {
			if !ok {
				t.Errorf("%s: not ready (%s), want ready", c.name, why)
			}
			continue
		}
		if ok || !strings.Contains(why, c.want) {
			t.Errorf("%s: ok=%v why=%q, want not ready naming %q", c.name, ok, why, c.want)
		}
	}
	if ok, why := probeLensReadiness("http://127.0.0.1:9"); ok || !strings.Contains(why, "unreachable") {
		t.Errorf("an unreachable lens: ok=%v why=%q", ok, why)
	}
}

func TestLensReadinessIsCachedBriefly(t *testing.T) {
	calls := 0
	prev := lensReadiness
	lensReadiness = func(string) (bool, string) { calls++; return true, "" }
	t.Cleanup(func() { lensReadiness = prev })
	lensReadyCache.Lock()
	lensReadyCache.url = ""
	lensReadyCache.Unlock()
	for i := 0; i < 3; i++ {
		lensReady("http://lens.test")
	}
	if calls != 1 {
		t.Errorf("%d probes for three requests inside the TTL, want 1", calls)
	}
}

// A request is not started while the lens cannot score: an ordinary 503 with
// the reason and what to run, before any work or any streamed byte.
func TestARequestIsRefusedWhileTheLensCannotScore(t *testing.T) {
	prev := lensReadiness
	lensReadiness = func(string) (bool, string) { return false, "it is unreachable at http://lens" }
	t.Cleanup(func() { lensReadiness = prev })
	lensReadyCache.Lock()
	lensReadyCache.url = ""
	lensReadyCache.Unlock()
	t.Setenv("ATLAS_WORKSPACE_DIR", t.TempDir())

	rec := httptest.NewRecorder()
	handleAgent(rec, httptest.NewRequest(http.MethodPost, "/v1/agent",
		strings.NewReader(`{"message":"add a pause toggle to app.py","task_contract":{"task_mode":"work"}}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503; body %s", rec.Code, rec.Body.String())
	}
	var env ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error != string(ErrDependencyDown) {
		t.Errorf("error code %q, want %q", env.Error, ErrDependencyDown)
	}
	for _, want := range []string{"geometric lens", "unreachable at http://lens", "atlas doctor"} {
		if !strings.Contains(env.Detail, want) {
			t.Errorf("detail %q does not say %q", env.Detail, want)
		}
	}
	if strings.Contains(rec.Header().Get("Content-Type"), "event-stream") {
		t.Error("the refusal was streamed: the client could not read it as an HTTP error")
	}
}

// /ready and /health apply the gate /v1/agent applies, so a client that asks
// first is not told "ready" for a request the proxy would refuse.
func TestReadyAgreesWithTheRequestGate(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(up.Close)
	prevURLs := []string{inferenceURL, sandboxURL, v3URL}
	inferenceURL, sandboxURL, v3URL = up.URL, up.URL, up.URL
	prev := lensReadiness
	lensReadiness = func(string) (bool, string) {
		return false, "it has no G(x) model loaded; run `atlas lens build` or `atlas model install-artifacts`"
	}
	t.Cleanup(func() {
		inferenceURL, sandboxURL, v3URL = prevURLs[0], prevURLs[1], prevURLs[2]
		lensReadiness = prev
	})
	lensReadyCache.Lock()
	lensReadyCache.url = ""
	lensReadyCache.Unlock()

	for _, c := range []struct {
		path   string
		h      http.HandlerFunc
		status int
	}{
		{"/ready", handleReady, http.StatusServiceUnavailable},
		{"/health", handleHealth, http.StatusOK},
	} {
		rec := httptest.NewRecorder()
		c.h(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		var body map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		if rec.Code != c.status || body["lens_ready"] != false {
			t.Errorf("%s: status %d lens_ready %v, want %d and false", c.path, rec.Code, body["lens_ready"], c.status)
		}
		if why, _ := body["lens_reason"].(string); !strings.Contains(why, "no G(x) model") {
			t.Errorf("%s: lens_reason %q does not say why", c.path, why)
		}
	}
}

func TestAScoreAnswerSaysWhetherTheLensItselfIsDown(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		down   bool
	}{
		{"no model loaded", 200, `{"enabled":false}`, true},
		{"model server down", 200, `{"enabled":true,"scored":false,"failure":{"kind":"model_server_unreachable","detail":"URLError"}}`, true},
		{"http error", 503, `{"detail":"x"}`, true},
		{"input too long", 200, `{"enabled":true,"scored":false,"failure":{"kind":"embed_capacity","input_tokens":2055,"capacity_tokens":2048}}`, false},
		{"empty input", 200, `{"enabled":true,"scored":false,"failure":{"kind":"empty_input"}}`, false},
		{"scored", 200, `{"enabled":true,"scored":true,"gx_available":true,"n_tokens":3,"aggregate":{"gx_score_min":0.7,"gx_score_mean":0.8,"first_off_rails_idx":-1}}`, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.status)
			w.Write([]byte(c.body))
		}))
		_, _, down := scoreContentForAgent(context.Background(), srv.URL, "x = 1\n")
		srv.Close()
		if (down != "") != c.down {
			t.Errorf("%s: down=%q, want down=%v", c.name, down, c.down)
		}
	}
	if _, _, down := scoreContentForAgent(context.Background(), "http://127.0.0.1:9", "x = 1\n"); down == "" {
		t.Error("an unreachable lens was not reported down")
	}
}

const lensFile = "def total(values):\n    acc = 0\n    for v in values:\n        acc += v\n    return acc\n\n\n" +
	"def mean(values):\n    return total(values) / len(values)\n\n\nprint(mean([1, 2, 3]))\n"

// A lens that stops scoring mid-run ends the run before the write it was
// asked to score, with the tool call answered and nothing written.
func TestALensThatStopsScoringEndsTheRun(t *testing.T) {
	r := editLoopFixture(t, nil, `{"task_mode":"work"}`, "Write stats.py.",
		script(stepWrite("stats.py", lensFile), stepDone("wrote stats.py")),
		editLoopOptions{lensOff: true})
	if r.terminal["status"] != string(TerminalFailed) || r.terminal["reason"] != "lens_unavailable" {
		t.Fatalf("terminal %q/%q, want failed/lens_unavailable: %s",
			r.terminal["status"], r.terminal["reason"], r.describe())
	}
	if !strings.Contains(r.terminal["summary"], "atlas doctor") {
		t.Errorf("the summary does not say what to run: %q", r.terminal["summary"])
	}
	if _, err := os.Stat(filepath.Join(r.dir, "stats.py")); err == nil {
		t.Error("the write landed although the lens could not score it")
	}
	if len(r.results) != 1 || r.results[0]["success"] != false {
		t.Errorf("the write call was not answered as not run: %v", r.results)
	}
}

// V3 reports that the lens could not score: the write is not applied (no
// fallback to the model's bytes) and the run ends with the reason.
func TestALensThatStopsScoringInsideV3EndsTheRun(t *testing.T) {
	r := editLoopFixture(t, nil, `{"task_mode":"work"}`, "Write stats.py.",
		script(stepWrite("stats.py", lensFile), stepDone("wrote stats.py")),
		editLoopOptions{v3LensUnavailable: "lens_unreachable: URLError"})
	if r.terminal["reason"] != "lens_unavailable" {
		t.Fatalf("terminal %q/%q, want lens_unavailable: %s",
			r.terminal["status"], r.terminal["reason"], r.describe())
	}
	if !strings.Contains(r.terminal["summary"], "lens_unreachable: URLError") {
		t.Errorf("the summary does not carry V3's reason: %q", r.terminal["summary"])
	}
	if _, err := os.Stat(filepath.Join(r.dir, "stats.py")); err == nil {
		t.Error("the model's bytes were written as a fallback")
	}
}

// The same on the edit route: the caller's edit is not applied either.
func TestALensThatStopsScoringInsideV3LeavesAnEditUnapplied(t *testing.T) {
	r := editLoopFixture(t, map[string]string{"mod.py": accountingSeed}, `{"task_mode":"work"}`,
		"Change helper in mod.py.",
		script(stepRead("mod.py"), stepEdit("mod.py", "    return 1\n", "    return 2\n"),
			stepDone("edited mod.py")),
		editLoopOptions{v3LensUnavailable: "model_server_error: 503"})
	if r.terminal["reason"] != "lens_unavailable" {
		t.Fatalf("terminal %q/%q, want lens_unavailable: %s",
			r.terminal["status"], r.terminal["reason"], r.describe())
	}
	if disk := r.disk(t, "mod.py"); disk != accountingSeed {
		t.Errorf("mod.py changed although the lens could not score the edit")
	}
}

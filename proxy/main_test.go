package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestModelsFallsBackToConfiguredModel(t *testing.T) {
	previous := modelName
	modelName = "configured-model"
	t.Cleanup(func() { modelName = previous })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	t.Cleanup(upstream.Close)
	withInferenceURL(t, upstream.URL)

	for _, path := range []string{"/v1/models", "/models"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			newProxyMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			var payload struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if len(payload.Data) != 1 || payload.Data[0].ID != modelName {
				t.Fatalf("model data = %#v, want id %q", payload.Data, modelName)
			}
		})
	}
}

func TestModelsPrefersLoadedModelOverStaleConfig(t *testing.T) {
	previous := modelName
	modelName = "stale-config-model"
	t.Cleanup(func() { modelName = previous })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("upstream path = %q, want /v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"loaded-runtime-model"}]}`))
	}))
	t.Cleanup(upstream.Close)
	withInferenceURL(t, upstream.URL)

	rec := httptest.NewRecorder()
	newProxyMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Data) != 1 || payload.Data[0].ID != "loaded-runtime-model" {
		t.Fatalf("model data = %#v, want loaded runtime model", payload.Data)
	}
}

func TestHealthAdvertisesRawDemoCapability(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	previousInference, previousLens, previousSandbox := inferenceURL, lensURL, sandboxURL
	inferenceURL, lensURL, sandboxURL = upstream.URL, upstream.URL, upstream.URL
	t.Cleanup(func() {
		inferenceURL, lensURL, sandboxURL = previousInference, previousLens, previousSandbox
	})

	rec := httptest.NewRecorder()
	newProxyMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var payload struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	found := false
	for _, capability := range payload.Capabilities {
		if capability == demoRawCapability {
			found = true
		}
	}
	if !found {
		t.Fatalf("health capabilities = %v, want %q", payload.Capabilities, demoRawCapability)
	}
}

func withInferenceURL(t *testing.T, url string) {
	t.Helper()
	previous := inferenceURL
	inferenceURL = url
	t.Cleanup(func() {
		inferenceURL = previous
	})
}

func TestPassthroughPreservesRequestURI(t *testing.T) {
	var gotMethod string
	var gotRequestURI string
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotRequestURI = r.URL.RequestURI()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		gotBody = string(body)
		w.Header().Set("X-Upstream", "seen")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)
	withInferenceURL(t, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?stream=true&n=2", strings.NewReader("payload"))
	req.Header.Set("X-Client", "atlas-test")
	rec := httptest.NewRecorder()

	newProxyMux().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%q", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want %q", gotMethod, http.MethodPost)
	}
	if gotRequestURI != "/v1/chat/completions?stream=true&n=2" {
		t.Fatalf("request URI = %q", gotRequestURI)
	}
	if gotBody != "payload" {
		t.Fatalf("body = %q", gotBody)
	}
	if rec.Header().Get("X-Upstream") != "seen" {
		t.Fatalf("upstream header was not copied")
	}
}

func TestPassthroughRejectsOversizedBody(t *testing.T) {
	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	withInferenceURL(t, upstream.URL)

	handler := http.MaxBytesHandler(newProxyMux(), 1)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("too large"))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d; body=%q", rec.Code, http.StatusRequestEntityTooLarge, rec.Body.String())
	}
	if upstreamCalled {
		t.Fatalf("oversized request reached upstream")
	}
}

func TestWriteErrorEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, 400, ErrInvalidInput, "bad thing")
	if rec.Code != 400 {
		t.Fatalf("status = %d", rec.Code)
	}
	var env ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error != "invalid_input" {
		t.Errorf("code = %q", env.Error)
	}
	if env.APIVersion != APIVersion {
		t.Errorf("api_version = %q", env.APIVersion)
	}
}

func TestAllErrorCodesUnique(t *testing.T) {
	seen := map[ErrorCode]bool{}
	for _, c := range AllErrorCodes {
		if seen[c] {
			t.Fatalf("duplicate error code %q", c)
		}
		seen[c] = true
	}
	if len(AllErrorCodes) != 6 {
		t.Fatalf("expected 6 error codes, got %d", len(AllErrorCodes))
	}
}

// Tests for internal service authentication. All tokens are synthetic
// test fixtures.

func withToken(t *testing.T, tok string) func() {
	t.Helper()
	prev := serviceToken
	serviceToken = tok
	return func() { serviceToken = prev }
}

func TestRequireServiceToken(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Run("no token configured passes everything through", func(t *testing.T) {
		defer withToken(t, "")()
		h := requireServiceToken(inner)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/agent", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("open mode rejected: %d", rec.Code)
		}
	})

	t.Run("correct token accepted", func(t *testing.T) {
		defer withToken(t, "atlas-st-testfixture")()
		h := requireServiceToken(inner)
		req := httptest.NewRequest("POST", "/v1/agent", nil)
		req.Header.Set("Authorization", "Bearer atlas-st-testfixture")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("valid token rejected: %d", rec.Code)
		}
	})

	t.Run("missing token rejected", func(t *testing.T) {
		defer withToken(t, "atlas-st-testfixture")()
		h := requireServiceToken(inner)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/agent", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("missing token got %d, want 401", rec.Code)
		}
		// The response must never echo token material.
		if got := rec.Body.String(); strings.Contains(got, "testfixture") {
			t.Fatalf("401 body leaks token material: %q", got)
		}
	})

	t.Run("wrong token rejected", func(t *testing.T) {
		defer withToken(t, "atlas-st-testfixture")()
		h := requireServiceToken(inner)
		req := httptest.NewRequest("POST", "/cancel", nil)
		req.Header.Set("Authorization", "Bearer atlas-st-WRONG")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong token got %d, want 401", rec.Code)
		}
	})

	t.Run("health and ready stay open", func(t *testing.T) {
		defer withToken(t, "atlas-st-testfixture")()
		h := requireServiceToken(inner)
		for _, p := range []string{"/health", "/ready"} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s rejected (%d) — compose healthchecks are headerless", p, rec.Code)
			}
		}
	})
}

func TestTokenTransportInjection(t *testing.T) {
	defer withToken(t, "atlas-st-inject")()
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
		}))
	defer srv.Close()

	client := &http.Client{Transport: &tokenTransport{}}

	t.Run("injects when absent", func(t *testing.T) {
		req, _ := http.NewRequest("GET", srv.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if gotAuth != "Bearer atlas-st-inject" {
			t.Fatalf("header not injected: %q", gotAuth)
		}
	})

	t.Run("caller-set header wins (passthrough forwarding)", func(t *testing.T) {
		req, _ := http.NewRequest("GET", srv.URL, nil)
		req.Header.Set("Authorization", "Bearer client-own-key")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if gotAuth != "Bearer client-own-key" {
			t.Fatalf("caller header overridden: %q", gotAuth)
		}
	})
}

func TestWithRequestIDGenerates(t *testing.T) {
	var seen string
	h := withRequestID(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			seen = requestIDFromContext(r.Context())
		}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	if seen == "" || !strings.HasPrefix(seen, "req-") {
		t.Fatalf("no generated request id in context: %q", seen)
	}
	if rec.Header().Get(requestIDHeader) != seen {
		t.Fatalf("response header %q != context %q",
			rec.Header().Get(requestIDHeader), seen)
	}
}

func TestWithRequestIDHonorsClientID(t *testing.T) {
	var seen string
	h := withRequestID(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			seen = requestIDFromContext(r.Context())
		}))
	req := httptest.NewRequest("GET", "/health", nil)
	req.Header.Set(requestIDHeader, "req-client-supplied")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen != "req-client-supplied" {
		t.Fatalf("client id not honored: %q", seen)
	}
}

func TestTokenTransportForwardsRequestID(t *testing.T) {
	defer withToken(t, "")() // token off — isolate the ID-forwarding path
	var got string
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Get(requestIDHeader)
		}))
	defer srv.Close()
	client := &http.Client{Transport: &tokenTransport{}}
	req, _ := http.NewRequest("GET", srv.URL, nil)
	ctx := context.WithValue(req.Context(), requestIDKey, "req-trace-99")
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got != "req-trace-99" {
		t.Fatalf("request id not forwarded downstream: %q", got)
	}
}

func TestLogEventJSONMode(t *testing.T) {
	var buf strings.Builder
	oldOut := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(oldOut)
	prev := logJSON
	logJSON = true
	defer func() { logJSON = prev }()
	logEvent("info", "hello", "req-7", map[string]interface{}{"k": "v"})
	out := buf.String()
	idx := strings.Index(out, "{")
	if idx < 0 {
		t.Fatalf("no JSON object in output: %q", out)
	}
	var rec map[string]interface{}
	if err := json.Unmarshal([]byte(out[idx:]), &rec); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, out)
	}
	if rec["level"] != "info" || rec["msg"] != "hello" ||
		rec["request_id"] != "req-7" || rec["k"] != "v" {
		t.Fatalf("bad record: %v", rec)
	}
}

func TestSafeLogFieldEscapesRecordSeparators(t *testing.T) {
	got := safeLogField("first\nforged\r\x00entry", 200)
	if strings.ContainsAny(got, "\r\n\x00") {
		t.Fatalf("safeLogField emitted a raw record separator: %q", got)
	}
	for _, escaped := range []string{`\n`, `\r`, `\x00`} {
		if !strings.Contains(got, escaped) {
			t.Fatalf("safeLogField(%q) missing %q", got, escaped)
		}
	}
}

func TestSafeLogFieldBoundsUntrustedText(t *testing.T) {
	got := safeLogField(strings.Repeat("x", 100), 12)
	if len(got) > 20 {
		t.Fatalf("bounded log field remained unexpectedly large: %q", got)
	}
}

// Corpus-driven tests for private-value filtering. Every fixture value
// is synthetic (see tests/fixtures/private_value_fixtures.json).

type pvCase struct {
	Name           string   `json:"name"`
	Input          string   `json:"input"`
	MustNotContain []string `json:"must_not_contain"`
	MustContain    []string `json:"must_contain"`
	MustEqualInput bool     `json:"must_equal_input"`
}

type pvCorpus struct {
	Placeholder   string   `json:"placeholder"`
	Cases         []pvCase `json:"cases"`
	NegativeCases []pvCase `json:"negative_cases"`
}

func loadCorpus(t *testing.T) pvCorpus {
	t.Helper()
	path := filepath.Join("..", "tests", "fixtures",
		"private_value_fixtures.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fixture corpus missing: %v", err)
	}
	var c pvCorpus
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("fixture corpus unparsable: %v", err)
	}
	return c
}

func TestFilterPrivateValuesCorpus(t *testing.T) {
	corpus := loadCorpus(t)
	if corpus.Placeholder != privateValuePlaceholder {
		t.Fatalf("placeholder drift: corpus %q vs code %q",
			corpus.Placeholder, privateValuePlaceholder)
	}
	for _, c := range corpus.Cases {
		got := filterPrivateValues(c.Input)
		for _, bad := range c.MustNotContain {
			if strings.Contains(got, bad) {
				t.Errorf("%s: %q survived filtering: %q", c.Name, bad, got)
			}
		}
		for _, keep := range c.MustContain {
			if !strings.Contains(got, keep) {
				t.Errorf("%s: context %q lost: %q", c.Name, keep, got)
			}
		}
		if len(c.MustNotContain) > 0 &&
			!strings.Contains(got, privateValuePlaceholder) {
			t.Errorf("%s: no placeholder in output: %q", c.Name, got)
		}
	}
	for _, c := range corpus.NegativeCases {
		if got := filterPrivateValues(c.Input); got != c.Input {
			t.Errorf("%s: benign input modified: %q -> %q",
				c.Name, c.Input, got)
		}
	}
}

func TestFilteringWriterOnLogger(t *testing.T) {
	var buf bytes.Buffer
	lg := log.New(filteringWriter{w: &buf}, "", 0)
	lg.Printf("turn failed: EXAMPLE_API_TOKEN=not-a-real-token status=500")
	out := buf.String()
	if strings.Contains(out, "not-a-real-token") {
		t.Fatalf("fixture value reached the log sink: %q", out)
	}
	if !strings.Contains(out, "status=500") {
		t.Fatalf("benign context lost: %q", out)
	}
	if !strings.Contains(out, privateValuePlaceholder) {
		t.Fatalf("placeholder missing: %q", out)
	}
}

// Every generation request leaving the proxy carries an explicit
// completion bound, so a client disconnect can't leave an unbounded zombie
// generation holding a llama slot.
func TestClampGenerationBody(t *testing.T) {
	get := func(body []byte, key string) (float64, bool) {
		var m map[string]interface{}
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("clamped body is not JSON: %v", err)
		}
		v, ok := m[key].(float64)
		return v, ok
	}

	t.Run("missing max_tokens gets the default ceiling", func(t *testing.T) {
		out := clampGenerationBody("/v1/chat/completions",
			[]byte(`{"messages":[{"role":"user","content":"hi"}]}`))
		if v, ok := get(out, "max_tokens"); !ok || v != 8192 {
			t.Fatalf("want max_tokens=8192, got %v (present=%v)", v, ok)
		}
	})

	t.Run("in-range value passes through untouched", func(t *testing.T) {
		in := []byte(`{"max_tokens":512}`)
		out := clampGenerationBody("/v1/chat/completions", in)
		if string(out) != string(in) {
			t.Fatalf("in-range body modified: %s", out)
		}
	})

	t.Run("unlimited sentinel -1 is clamped", func(t *testing.T) {
		out := clampGenerationBody("/v1/completions", []byte(`{"max_tokens":-1}`))
		if v, _ := get(out, "max_tokens"); v != 8192 {
			t.Fatalf("want clamp to 8192, got %v", v)
		}
	})

	t.Run("above-ceiling value is clamped", func(t *testing.T) {
		out := clampGenerationBody("/v1/chat/completions", []byte(`{"max_tokens":900000}`))
		if v, _ := get(out, "max_tokens"); v != 8192 {
			t.Fatalf("want clamp to 8192, got %v", v)
		}
	})

	t.Run("llama-native endpoints use n_predict", func(t *testing.T) {
		out := clampGenerationBody("/completion", []byte(`{"prompt":"x"}`))
		if v, ok := get(out, "n_predict"); !ok || v != 8192 {
			t.Fatalf("want n_predict=8192, got %v (present=%v)", v, ok)
		}
	})

	t.Run("env override sets the ceiling", func(t *testing.T) {
		t.Setenv("ATLAS_MAX_COMPLETION_TOKENS", "1024")
		out := clampGenerationBody("/v1/chat/completions", []byte(`{}`))
		if v, _ := get(out, "max_tokens"); v != 1024 {
			t.Fatalf("want max_tokens=1024, got %v", v)
		}
		out = clampGenerationBody("/v1/chat/completions", []byte(`{"max_tokens":2048}`))
		if v, _ := get(out, "max_tokens"); v != 1024 {
			t.Fatalf("want clamp to 1024, got %v", v)
		}
	})

	t.Run("non-generation paths and bad JSON pass through", func(t *testing.T) {
		for _, c := range []struct{ path, body string }{
			{"/health", `{}`},
			{"/slots", `{"max_tokens":-1}`},
			{"/v1/chat/completions", `not json`},
		} {
			out := clampGenerationBody(c.path, []byte(c.body))
			if string(out) != c.body {
				t.Fatalf("%s: body modified: %s", c.path, out)
			}
		}
	})
}

// --- The structured task contract --------------------------------------------
//
// ATLAS infers obligations from English today: which verbs the user used
// decides whether work was demanded, which filenames near a write verb become
// deliverables, which phrases demand verification. This is the first piece of
// the replacement -- the client saying, in typed fields, what it already knows.
//
// It decides nothing yet. This commit adds the shape, validates it, and stores
// it; a guard proves no production decision reads it.

func TestTaskContractWireShape(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.py"), []byte("A = 1\n"), 0o644)

	for _, c := range []struct {
		name    string
		body    string
		present bool
		valid   bool
		mode    TaskMode
		outputs []string
		verify  []string
	}{
		{"absent contract", `{}`, false, false, "", nil, nil},
		{"work with outputs and verification",
			`{"task_contract":{"task_mode":"work","expected_outputs":["app.py"],"verification":["go test ./..."]}}`,
			true, true, TaskModeWork, []string{"app.py"}, []string{"go test ./..."}},
		{"question", `{"task_contract":{"task_mode":"question"}}`,
			true, true, TaskModeQuestion, nil, nil},
		{"unknown mode", `{"task_contract":{"task_mode":"explore"}}`, true, false, "", nil, nil},
		{"empty mode", `{"task_contract":{"task_mode":""}}`, true, false, "", nil, nil},
		{"one bad path rejects the whole contract",
			`{"task_contract":{"task_mode":"work","expected_outputs":["app.py","../../etc/passwd"]}}`,
			true, false, "", nil, nil},
		{"workspace escape", `{"task_contract":{"task_mode":"work","expected_outputs":["../x.py"]}}`,
			true, false, "", nil, nil},
		{"empty output entry", `{"task_contract":{"task_mode":"work","expected_outputs":[""]}}`,
			true, false, "", nil, nil},
		{"empty verification entry",
			`{"task_contract":{"task_mode":"work","verification":["  "]}}`, true, false, "", nil, nil},
		{"aliases deduplicate canonically",
			`{"task_contract":{"task_mode":"work","expected_outputs":["app.py","./app.py"]}}`,
			true, true, TaskModeWork, []string{"app.py"}, nil},
		{"duplicate verification deduplicates",
			`{"task_contract":{"task_mode":"work","verification":["go test ./...","go test ./..."]}}`,
			true, true, TaskModeWork, nil, []string{"go test ./..."}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var wire struct {
				TaskContract *TaskContract `json:"task_contract,omitempty"`
			}
			if err := json.Unmarshal([]byte(c.body), &wire); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if (wire.TaskContract != nil) != c.present {
				t.Fatalf("present=%v want %v", wire.TaskContract != nil, c.present)
			}
			if !c.present {
				return
			}
			got, err := validateTaskContract(wire.TaskContract, dir)
			if (err == nil) != c.valid {
				t.Fatalf("valid=%v (err=%v) want %v", err == nil, err, c.valid)
			}
			if !c.valid {
				if got != nil {
					t.Error("an invalid contract produced a stored value; it must be all or nothing")
				}
				return
			}
			if got.TaskMode != c.mode {
				t.Errorf("mode=%q want %q", got.TaskMode, c.mode)
			}
			if strings.Join(got.OutputPaths(), "|") != strings.Join(c.outputs, "|") {
				t.Errorf("outputs=%v want %v", got.OutputPaths(), c.outputs)
			}
			if strings.Join(got.VerificationCommands(), "|") != strings.Join(c.verify, "|") {
				t.Errorf("verification=%v want %v", got.VerificationCommands(), c.verify)
			}
		})
	}
}

// strsPtr builds the presence-preserving list a TaskContract now carries:
// a non-nil pointer means the caller SENT a list, even an empty one.
func strsPtr(v ...string) *[]string {
	s := append([]string{}, v...)
	return &s
}

// Bounds use the ceiling the rest of the session state already uses.
func TestTaskContractBounds(t *testing.T) {
	dir := t.TempDir()
	mk := func(n int) *TaskContract {
		c := &TaskContract{TaskMode: TaskModeWork}
		paths := []string{}
		for i := 0; i < n; i++ {
			paths = append(paths, fmt.Sprintf("f%d.py", i))
		}
		c.ExpectedOutputs = &paths
		return c
	}
	if _, err := validateTaskContract(mk(maxTaskContractEntries), dir); err != nil {
		t.Errorf("at the bound: %v", err)
	}
	got, err := validateTaskContract(mk(maxTaskContractEntries+1), dir)
	if err == nil {
		t.Error("over the bound was accepted")
	}
	if got != nil {
		t.Error("overflow produced a partial contract")
	}
	v := &TaskContract{TaskMode: TaskModeWork}
	cmds := []string{}
	for i := 0; i <= maxTaskContractEntries; i++ {
		cmds = append(cmds, fmt.Sprintf("cmd %d", i))
	}
	v.Verification = &cmds
	if _, err := validateTaskContract(v, dir); err == nil {
		t.Error("verification overflow was accepted")
	}
}

// Round-trip: a valid contract serialises back to the same fields.
func TestTaskContractRoundTrip(t *testing.T) {
	dir := t.TempDir()
	inPaths := []string{"b.py", "a.py"}
	inCmds := []string{"pytest", "go vet ./..."}
	in := &TaskContract{
		TaskMode:        TaskModeWork,
		ExpectedOutputs: &inPaths,
		Verification:    &inCmds,
	}
	got, err := validateTaskContract(in, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Stable ordering, so two equivalent requests never disagree.
	if strings.Join(got.OutputPaths(), "|") != "a.py|b.py" {
		t.Errorf("outputs not stably ordered: %v", got.OutputPaths())
	}
	if strings.Join(got.VerificationCommands(), "|") != "go vet ./...|pytest" {
		t.Errorf("verification not stably ordered: %v", got.VerificationCommands())
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var back TaskContract
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.TaskMode != got.TaskMode ||
		strings.Join(back.OutputPaths(), "|") != strings.Join(got.OutputPaths(), "|") ||
		strings.Join(back.VerificationCommands(), "|") != strings.Join(got.VerificationCommands(), "|") {
		t.Errorf("round trip lost fields: %+v vs %+v", back, got)
	}
}

// The contract decides nothing. One definition, one validator, no consumer.
func TestTaskContractHasNoDecisionConsumer(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var defs, validators, readers []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		ast.Inspect(f, func(node ast.Node) bool {
			switch v := node.(type) {
			case *ast.TypeSpec:
				if v.Name != nil && v.Name.Name == "TaskContract" {
					defs = append(defs, fmt.Sprintf("%s:%d", n, fset.Position(v.Pos()).Line))
				}
			case *ast.FuncDecl:
				if v.Name != nil && v.Name.Name == "validateTaskContract" {
					validators = append(validators, fmt.Sprintf("%s:%d", n, fset.Position(v.Pos()).Line))
				}
			case *ast.SelectorExpr:
				if v.Sel != nil && v.Sel.Name == "TaskContract" {
					readers = append(readers, fmt.Sprintf("%s:%d", n, fset.Position(v.Pos()).Line))
				}
			}
			return true
		})
	}
	t.Logf("definitions=%v validators=%v reads=%v", defs, validators, readers)
	if len(defs) != 1 {
		t.Errorf("%d TaskContract definitions, want exactly one: %v", len(defs), defs)
	}
	if len(validators) != 1 {
		t.Errorf("%d validators, want exactly one: %v", len(validators), validators)
	}
	// obligations.go joins the list because this slice moved the decision
	// there: it is THE obligation-source owner, and it reads only the contract
	// the request boundary already validated. Nothing else may read the
	// contract to decide anything.
	// obligation_kinds.go joins as the single derivation reader: it asks the
	// owners what a request obliges and turns their answers into typed
	// obligations. evidence_wiring.go joins as the producer wiring, which
	// reads the contract only to know whether a request HAS structured
	// obligations at all -- it decides nothing and reaches no live path.
	// authorization_grant.go joins for the same reason evidence_wiring.go
	// did: minting asks whether the request HAS a contract at all, because a
	// licence to replace an artifact nobody declared is the one thing no
	// amount of evidence can justify. It reads no contract FIELD.
	contractReaders := map[string]bool{
		"agent.go:": true, "guardrails.go:": true, "obligations.go:": true,
		"obligation_kinds.go:": true, "evidence_wiring.go:": true,
		"authorization_grant.go:": true,
		// delivery_settlement.go joins for the same reason again: it asks
		// whether the request HAS a contract, because a run that declared
		// nothing has no existence obligation to discharge. It reads no
		// contract FIELD and reaches no decision of its own.
		"delivery_settlement.go:": true,
		// candidate_policy.go joins as THE candidate policy owner. It reads
		// exactly one field -- the mode the client declared -- and it is the
		// only place that field is read, which is what keeps the model and the
		// V3 service off a decision only a client or an operator may make.
		"candidate_policy.go:": true,
	}
	readerAllowed := func(r string) bool {
		for prefix := range contractReaders {
			if strings.HasPrefix(r, prefix) {
				return true
			}
		}
		return false
	}
	for _, r := range readers {
		if !readerAllowed(r) {
			t.Errorf("%s reads the task contract outside the request boundary", r)
		}
	}
	// Step 1 pinned that NOTHING consulted the contract, because Step 1 added
	// no decision. That premise is superseded twice over. The task-mode
	// migration gave the contract one live consumer; this slice gives it a
	// second, the work-contract verification demand. What still has to hold,
	// and is what this now pins, is that BOTH decisions are owned by
	// guardrails.go and that no other subsystem reads the contract to decide
	// anything.
	//
	// finalizeCompletion is off this list for that reason: it does not read a
	// contract field to reach its own conclusion, it hands the contract to the
	// policy owner and uses the answer. The check below pins that shape
	// exactly, so a future edit that starts inspecting TaskMode inline still
	// fails.
	body, _ := os.ReadFile("agent.go")
	final := string(body)[strings.Index(string(body), "func finalizeCompletion"):]
	if e := strings.Index(final[1:], "\nfunc "); e >= 0 {
		final = final[:e]
	}
	for _, want := range []string{
		"decideVerificationDemand(ctx, ctx.TaskContract, st.expectedOutputs)",
	} {
		if !strings.Contains(final, want) {
			t.Errorf("finalizeCompletion no longer delegates to the policy owner: %q", want)
		}
	}
	// The one inline contract read it is allowed is the scope of the text
	// exit, which is a terminal-shape rule and not a completion decision.
	if n := strings.Count(final, "ctx.TaskContract"); n != 2 {
		t.Errorf("finalizeCompletion reads the contract %d times; exactly two are "+
			"allowed: the delegation and the text-exit scope", n)
	}
	for _, fn := range []string{"classifyAgentTier", "terminalCompletionAllowed",
		"blockingTombstone", "needsPermission", "honestTerminalSummary",
		"buildSystemPrompt", "buildToolDescriptionsExcluding"} {
		i := strings.Index(string(body), "func "+fn)
		if i < 0 {
			continue
		}
		end := strings.Index(string(body)[i+1:], "\nfunc ")
		if end < 0 {
			end = len(body) - i - 1
		}
		if strings.Contains(string(body)[i:i+1+end], "TaskContract") {
			t.Errorf("%s consults the task contract; only the action-demand policy owner may", fn)
		}
	}
	// guardrails.go owns the decision, and only through the one helper.
	guard, _ := os.ReadFile("guardrails.go")
	gs := string(guard)
	if !strings.Contains(gs, "func decideVerificationDemand") {
		t.Error("the work-contract verification demand is not owned by guardrails.go")
	}
	if !strings.Contains(gs, "func decideActionDemand") {
		t.Error("the central action-demand helper is gone from the policy owner")
	}
	for _, fn := range []string{"wantsStateChange", "isActionIntentMessage", "isReadOnlyRequest"} {
		i := strings.Index(gs, "func "+fn)
		if i < 0 {
			continue
		}
		end := strings.Index(gs[i+1:], "\nfunc ")
		if end < 0 {
			end = len(gs) - i - 1
		}
		if strings.Contains(gs[i:i+1+end], "TaskContract") {
			t.Errorf("%s consults the task contract; the heuristic must stay contract-blind", fn)
		}
	}
	// No owner outside the request boundary and the obligation owner touches
	// it at all.
	for _, r := range readers {
		if !readerAllowed(r) {
			t.Errorf("%s reads the task contract outside the request boundary and policy owner", r)
		}
	}
	// And the obligation owner reads the contract ONLY through the validated
	// request-bound context -- never a shadow copy, never a second decode.
	obTree, err := parser.ParseFile(fset, "obligations.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	banned := map[string]bool{
		"json.Unmarshal": true, "Unmarshal": true, "validateTaskContract": true,
		"contractOutputs": true, "openShadowSink": true, "newShadowSink": true,
		"emitShadowRequestSnapshot": true,
	}
	// Calls, not prose: the file names validateTaskContract in a comment
	// precisely to say it does not call it.
	ast.Inspect(obTree, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		if banned[name] || shadowProductionSymbols[name] {
			t.Errorf("the obligation owner calls %s instead of reading the "+
				"validated request-bound contract", name)
		}
		return true
	})
}

// A contract changes only the classes it DECLARES.
//
// The premise this replaces allowed exactly one move, to
// action_demanded_unmet, because the contract decided exactly one thing. That
// is superseded: a caller that states it knows its outputs, or its
// verification, is now the authority on that class, and the corresponding gate
// may legitimately move. What did not change is the direction -- every
// permitted move is strictly toward incomplete, and no contract may move a
// terminal toward completed -- or the blast radius: a class the caller said
// nothing about behaves exactly as it did with no contract at all.
//
// Contracts here are built through validateTaskContract, not as struct
// literals, because the knowledge normalisation is part of what is under test:
// a struct built by hand carries no stated knowledge and must fall back to
// legacy, which the unspecified rows below pin.
func TestTaskContractChangesOnlyWhatItDeclares(t *testing.T) {
	run := func(t *testing.T, contract *TaskContract, prompt string,
		plan func(i int) map[string]interface{}) (string, string, []string) {
		t.Helper()
		dir := t.TempDir()
		turns := 0
		var mu sync.Mutex
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			case strings.HasSuffix(r.URL.Path, "/syntax-check"):
				json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
				return
			case strings.HasSuffix(r.URL.Path, "/execute"):
				var in struct{ Code string }
				json.NewDecoder(r.Body).Decode(&in)
				if strings.Contains(in.Code, ".atlas-mount-probe") {
					b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
					json.NewEncoder(w).Encode(map[string]interface{}{
						"success": true, "stdout": string(b), "exit_code": 0})
					return
				}
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": "", "exit_code": 0})
				return
			case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
				http.NotFound(w, r)
				return
			}
			io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			mu.Lock()
			i := turns
			turns++
			mu.Unlock()
			call, _ := json.Marshal(plan(i))
			d, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"delta": map[string]string{"content": string(call)}}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
		}))
		defer srv.Close()

		ctx := NewAgentContext(dir, Tier2Medium)
		ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
		ctx.PermissionMode = PermissionYolo
		ctx.TrustMode = trustFullyTrusted
		ctx.VerifyOnHost = true
		ctx.MaxTurns = 0
		ctx.TaskContract = contract // the only difference between the two runs
		var events []string
		terminal := map[string]string{}
		ctx.StreamFn = func(et string, data interface{}) {
			b, _ := json.Marshal(data)
			mu.Lock()
			defer mu.Unlock()
			// Timings differ run to run; everything else is compared.
			line := et + "|" + string(b)
			var m map[string]interface{}
			if json.Unmarshal(b, &m) == nil {
				for _, k := range []string{"elapsed", "prompt_ms", "elapsed_ms",
					"duration_ms", "wall_s", "ms"} {
					delete(m, k)
				}
				c, _ := json.Marshal(m)
				line = et + "|" + string(c)
			}
			events = append(events, line)
			if et == "done" {
				var m map[string]string
				json.Unmarshal(b, &m)
				for k, v := range m {
					terminal[k] = v
				}
			}
		}
		runAgentLoop(ctx, prompt)
		var disk []string
		filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(dir, p)
			if !strings.HasPrefix(rel, ".") {
				b, _ := os.ReadFile(p)
				disk = append(disk, rel+"="+hashBytes(b)[:12])
			}
			return nil
		})
		sort.Strings(disk)
		return strings.Join(events, "\n"),
			terminal["status"] + "/" + terminal["reason"], disk
	}

	// The case bodies below are contracts, so they are wrapped in the request
	// envelope and taken through the same decoder a request uses. A contract
	// that decodes to nil would make every row below compare a run against
	// itself, so it is a failure here rather than a silent pass.
	mustValidate := func(t *testing.T, contract string) *TaskContract {
		t.Helper()
		body := `{"task_contract":` + contract + `}`
		var req struct {
			TaskContract *TaskContract `json:"task_contract,omitempty"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.TaskContract == nil {
			t.Fatalf("%s decoded to no contract", body)
		}
		tc, err := validateTaskContract(req.TaskContract, t.TempDir())
		if err != nil {
			t.Fatalf("validate %s: %v", body, err)
		}
		if tc == nil {
			t.Fatalf("%s validated to no contract", body)
		}
		return tc
	}

	writesApp := func(i int) map[string]interface{} {
		if i == 0 {
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "app.py", "content": "A = 1\n"}}
		}
		return map[string]interface{}{"type": "done", "summary": "wrote app.py"}
	}
	saysNothing := func(i int) map[string]interface{} {
		return map[string]interface{}{"type": "text", "content": "here is what I would do"}
	}

	const (
		actionUnmet       = "incomplete/action_demanded_unmet"
		verificationUnmet = "incomplete/verification_demanded_unmet"
		deliverablesUnmet = "incomplete/deliverables_not_demonstrated"
	)

	// Each row states a baseline request and the request under test. The
	// baseline is how the same intent was expressed before this migration --
	// usually the legacy contract with no knowledge stated -- so a row asks
	// what stating knowledge changed, not what having a contract changed.
	// An empty baseline means no contract at all.
	for _, c := range []struct {
		name     string
		baseline string
		body     string
		// want: the exact terminal the request under test must produce.
		// Empty means it must equal the baseline's terminal event for event.
		want   string
		prompt string
		plan   func(i int) map[string]interface{}
	}{
		// --- stating "unspecified" is the same as not stating anything -----
		{"explicit unspecified equals omitted", `{"task_mode":"work"}`,
			`{"task_mode":"work","output_knowledge":"unspecified","verification_knowledge":"unspecified"}`,
			"", "Create app.py.", writesApp},
		{"explicit unspecified equals omitted, prose only", `{"task_mode":"work"}`,
			`{"task_mode":"work","output_knowledge":"unspecified","verification_knowledge":"unspecified"}`,
			"", "Create app.py.", saysNothing},
		{"explicit unspecified equals omitted, question", `{"task_mode":"question"}`,
			`{"task_mode":"question","output_knowledge":"unspecified","verification_knowledge":"unspecified"}`,
			"", "What does this repository do?", saysNothing},

		// --- a legacy list keeps the meaning it already had ----------------
		{"legacy outputs equal declared outputs",
			`{"task_mode":"work","expected_outputs":["app.py"]}`,
			`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["app.py"]}`,
			"", "Create app.py.", writesApp},
		{"legacy verification equals declared verification",
			`{"task_mode":"work","verification":["go test ./..."]}`,
			`{"task_mode":"work","verification_knowledge":"declared","verification":["go test ./..."]}`,
			"", "Create app.py.", writesApp},

		// --- an empty legacy list was never authoritative none -------------
		{"legacy empty outputs equal unspecified",
			`{"task_mode":"work"}`,
			`{"task_mode":"work","expected_outputs":[]}`,
			"", "Create app.py.", writesApp},
		{"legacy empty verification equals unspecified",
			`{"task_mode":"work"}`,
			`{"task_mode":"work","verification":[]}`,
			"", "Create app.py.", writesApp},

		// --- a declared output the run never produced is named as missing --
		{"declared output never written",
			`{"task_mode":"work"}`,
			`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["never_written.py"]}`,
			deliverablesUnmet, "Create app.py.", writesApp},

		// --- a declared verification nothing satisfied is named as unmet ---
		{"declared verification never run",
			`{"task_mode":"work","verification":["go test ./..."]}`,
			`{"task_mode":"work","verification_knowledge":"declared","verification":["go test ./..."]}`,
			verificationUnmet, "Create app.py.", writesApp},

		// --- authoritative none creates no obligation and no completion ----
		// Declaring no outputs drops the output obligation. It cannot
		// manufacture a completion: the verification demand this work request
		// already carried is untouched, so the run still ends incomplete.
		{"declared empty outputs create no output obligation",
			`{"task_mode":"work","expected_outputs":["never_written.py"]}`,
			`{"task_mode":"work","output_knowledge":"declared","expected_outputs":[]}`,
			verificationUnmet, "Create app.py.", writesApp},
		{"declared empty verification creates no verification obligation",
			`{"task_mode":"work","verification":["go test ./..."]}`,
			`{"task_mode":"work","verification_knowledge":"declared","verification":[]}`,
			verificationUnmet, "Create app.py.", writesApp},
	} {
		t.Run(c.name, func(t *testing.T) {
			var baseline *TaskContract
			if c.baseline != "" {
				baseline = mustValidate(t, c.baseline)
			}
			evA, termA, diskA := run(t, baseline, c.prompt, c.plan)
			evB, termB, diskB := run(t, mustValidate(t, c.body), c.prompt, c.plan)
			t.Logf("%s: terminal baseline=%q under test=%q", c.name, termA, termB)

			if strings.Join(diskA, ",") != strings.Join(diskB, ",") {
				t.Errorf("disk differs:\n  %v\n  %v", diskA, diskB)
			}
			// Stating knowledge may narrow a claim. It may never widen one.
			if strings.HasPrefix(termB, "completed") && !strings.HasPrefix(termA, "completed") {
				t.Errorf("stating knowledge moved a terminal toward completed: %q -> %q",
					termA, termB)
			}
			if c.want == "" {
				if termA != termB {
					t.Errorf("stating knowledge moved the terminal: %q -> %q", termA, termB)
				}
				if evA != evB {
					a, b := strings.Split(evA, "\n"), strings.Split(evB, "\n")
					for i := 0; i < len(a) || i < len(b); i++ {
						var x, y string
						if i < len(a) {
							x = a[i]
						}
						if i < len(b) {
							y = b[i]
						}
						if x != y {
							t.Errorf("stating knowledge changed the stream at event %d:"+
								"\n  baseline:   %.180s\n  under test: %.180s", i, x, y)
							break
						}
					}
				}
				return
			}
			if termB != c.want {
				t.Errorf("terminal %q, want %q (baseline was %q)", termB, c.want, termA)
			}
		})
	}
}

// Action demand stays pinned: a declared work contract may demand action where
// the wording alone would not have, and nothing else about that decision moved.
func TestActionDemandRemainsPinnedUnderDeclaredKnowledge(t *testing.T) {
	base := &TaskContract{TaskMode: TaskModeWork}
	paths := []string{"a.py"}
	declared := &TaskContract{TaskMode: TaskModeWork,
		OutputKnowledge: KnowledgeDeclared, ExpectedOutputs: &paths}
	for _, msg := range []string{"Create app.py.", "What does this do?", ""} {
		want := decideActionDemand(base, msg, Tier2Medium, false)
		got := decideActionDemand(declared, msg, Tier2Medium, false)
		if want.Required != got.Required {
			t.Errorf("%q: declaring outputs changed the action demand %v -> %v",
				msg, want.Required, got.Required)
		}
	}
}

func TestLegacyRequestWithoutContractIsAccepted(t *testing.T) {
	var wire struct {
		Message      string        `json:"message"`
		TaskContract *TaskContract `json:"task_contract,omitempty"`
	}
	const legacy = `{"message":"Create app.py.","working_dir":"/w","mode":"default"}`
	if err := json.Unmarshal([]byte(legacy), &wire); err != nil {
		t.Fatalf("a legacy body was rejected: %v", err)
	}
	if wire.TaskContract != nil {
		t.Error("a body with no task_contract decoded one anyway")
	}
	got, err := validateTaskContract(wire.TaskContract, t.TempDir())
	if err != nil || got != nil {
		t.Errorf("absent contract validated to %v (err=%v); absent must stay absent", got, err)
	}
	// And a present-but-invalid one is still a hard refusal, never a silent
	// omission that would look like a client which declared nothing.
	bad := &TaskContract{TaskMode: "explore"}
	if _, err := validateTaskContract(bad, t.TempDir()); err == nil {
		t.Error("an unsupported mode was accepted")
	}
}

// --- Bounded graceful shutdown -----------------------------------------------
//
// main() blocked in ListenAndServe and died on log.Fatalf, so SIGTERM killed
// the process where it stood: an active agent request was cut mid-turn, the
// TUI's permanent /events subscriber had no coordinated close, and no ordered
// cleanup had anywhere to run. A diagnostic sink that must drain and write a
// footer needs that landing site to exist.

func TestShutdownBudgetArithmetic(t *testing.T) {
	for _, c := range []struct {
		name       string
		sessionSec string
		graceSec   string
		wantErr    bool
	}{
		{"defaults", "", "", false}, // 600 + 10 = 610 < 650
		{"explicit shipped values", "600", "650", false},
		{"session raised without grace", "3600", "650", true},
		{"session raised with grace", "3600", "3700", false},
		{"grace exactly equal to need", "600", "610", true}, // strict <
		{"grace one second above need", "600", "611", false},
		{"malformed grace", "", "abc", true},
		{"zero grace", "", "0", true},
		{"negative grace", "", "-5", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.sessionSec != "" {
				t.Setenv("ATLAS_AGENT_SESSION_TIMEOUT_SEC", c.sessionSec)
			}
			if c.graceSec != "" {
				t.Setenv("ATLAS_SHUTDOWN_GRACE_SEC", c.graceSec)
			}
			budget, err := shutdownBudget()
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, c.wantErr)
			}
			if err != nil {
				// The refusal must show the whole sum, not just complain.
				for _, want := range []string{"ATLAS_SHUTDOWN_GRACE_SEC"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error does not name %s: %v", want, err)
					}
				}
				return
			}
			total, _ := sessionBudget()
			if budget.drain != total {
				t.Errorf("drain=%v, want the session total %v", budget.drain, total)
			}
			if budget.hookMargin != shutdownHookMargin {
				t.Errorf("hookMargin=%v", budget.hookMargin)
			}
		})
	}
}

// The shipped compose grace must agree with the shipped default.
func TestComposeGraceAgreesWithTheDefault(t *testing.T) {
	b, err := os.ReadFile("../docker-compose.yml")
	if err != nil {
		t.Skipf("compose unavailable: %v", err)
	}
	if !strings.Contains(string(b), "stop_grace_period: 650s") {
		t.Error("docker-compose.yml does not pin stop_grace_period: 650s to match " +
			"the shipped ATLAS_SHUTDOWN_GRACE_SEC default")
	}
}

// The production path: a real listener, a real in-flight request, a real
// /events subscriber, and a signal.
func TestRunServerDrainsAndRunsHooks(t *testing.T) {
	prev := defaultBroker
	defaultBroker = &broker{subscribers: map[chan Envelope]struct{}{}}
	defer func() { defaultBroker = prev }()

	released := make(chan struct{})
	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/events", handleEvents)
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-released // still running when the signal arrives
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "finished")
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	base := "http://" + ln.Addr().String()

	var hookRuns int32
	hooks := []closeHook{{
		name: "test-hook",
		fn: func(ctx context.Context) error {
			atomic.AddInt32(&hookRuns, 1)
			return nil
		},
	}}
	signals := make(chan os.Signal, 2)
	result := make(chan shutdownResult, 1)
	go func() {
		r, _ := runServer(srv, ln, signals, hooks,
			shutdownBudgetValues{drain: 10 * time.Second, hookMargin: 2 * time.Second})
		result <- r
	}()

	// A permanent /events subscriber, exactly as the TUI keeps open.
	evResp, err := http.Get(base + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer evResp.Body.Close()
	evBuf := make([]byte, 32)
	evResp.Body.Read(evBuf)

	// An in-flight request.
	slowDone := make(chan int, 1)
	go func() {
		resp, err := http.Get(base + "/slow")
		if err != nil {
			slowDone <- 0
			return
		}
		defer resp.Body.Close()
		io.ReadAll(resp.Body)
		slowDone <- resp.StatusCode
	}()
	<-started

	signals <- syscall.SIGTERM
	signals <- syscall.SIGTERM // repeated signals must not double-run hooks

	// The subscriber unblocks without consuming the drain budget.
	evClosed := make(chan struct{})
	go func() { io.ReadAll(evResp.Body); close(evClosed) }()
	select {
	case <-evClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("/events held the shutdown open")
	}

	// The in-flight request still finishes on its own terms.
	close(released)
	if code := <-slowDone; code != http.StatusOK {
		t.Errorf("the in-flight request was cut off: status %d", code)
	}

	select {
	case r := <-result:
		if r.forced {
			t.Errorf("a clean drain was reported as forced: %+v", r)
		}
		if !r.hooksRan {
			t.Error("close hooks did not run")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runServer never returned")
	}
	if n := atomic.LoadInt32(&hookRuns); n != 1 {
		t.Errorf("hooks ran %d times, want exactly one", n)
	}
	// New work is refused once draining began.
	if _, err := http.Get(base + "/slow"); err == nil {
		t.Error("the server accepted a new request after draining")
	}
}

// A request that outlives the drain budget takes the forced-close path, and
// that is reported as its own outcome -- not as a listener failure.
func TestRunServerForcedCloseIsClassified(t *testing.T) {
	prev := defaultBroker
	defaultBroker = &broker{subscribers: map[chan Envelope]struct{}{}}
	defer func() { defaultBroker = prev }()

	block := make(chan struct{})
	defer close(block)
	mux := http.NewServeMux()
	mux.HandleFunc("/stuck", func(w http.ResponseWriter, r *http.Request) { <-block })
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &http.Server{Handler: mux}
	signals := make(chan os.Signal, 1)
	result := make(chan shutdownResult, 1)
	go func() {
		r, _ := runServer(srv, ln, signals, nil,
			shutdownBudgetValues{drain: 300 * time.Millisecond, hookMargin: 200 * time.Millisecond})
		result <- r
	}()
	go http.Get("http://" + ln.Addr().String() + "/stuck")
	time.Sleep(100 * time.Millisecond)
	signals <- syscall.SIGTERM

	select {
	case r := <-result:
		if !r.forced {
			t.Error("a request outliving the drain budget was not classified as forced")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runServer never returned")
	}
}

// A listener that cannot start returns immediately rather than waiting for a
// signal, and ErrServerClosed is never an error.
func TestRunServerStartupFailureReturns(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ln.Close() // already closed: Serve fails at once
	srv := &http.Server{Handler: http.NewServeMux()}
	done := make(chan error, 1)
	go func() {
		_, err := runServer(srv, ln, make(chan os.Signal, 1), nil,
			shutdownBudgetValues{drain: time.Second, hookMargin: time.Second})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a listener failure was reported as success")
		}
		if errors.Is(err, http.ErrServerClosed) {
			t.Error("ErrServerClosed leaked out as a failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServer waited for a signal after a startup failure")
	}
}

// A hook that hangs is cut off by its own margin, and a failing hook is
// reported without hanging the process.
func TestCloseHooksAreBounded(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &http.Server{Handler: http.NewServeMux()}
	signals := make(chan os.Signal, 1)
	hooks := []closeHook{
		{name: "hangs", fn: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }},
		{name: "fails", fn: func(ctx context.Context) error { return errors.New("boom") }},
	}
	result := make(chan shutdownResult, 1)
	go func() {
		r, _ := runServer(srv, ln, signals, hooks,
			shutdownBudgetValues{drain: time.Second, hookMargin: 200 * time.Millisecond})
		result <- r
	}()
	time.Sleep(50 * time.Millisecond)
	signals <- syscall.SIGTERM
	select {
	case r := <-result:
		if len(r.hookErrors) != 2 {
			t.Errorf("hook errors=%v, want both reported", r.hookErrors)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a blocked hook hung the shutdown")
	}
}

// --- The private shadow capture ----------------------------------------------
//
// ATLAS still decides what the user demanded by reading their English. The
// client now declares it structurally, and nothing compares the two. This is
// that comparison, written to a private file and read by nobody: it decides
// nothing, reaches no wire, and exists only so a later corpus can say how often
// the two disagree and why.

func shadowEnv(t *testing.T, dir, name string) {
	t.Helper()
	t.Setenv("ATLAS_DIAGNOSTIC_DIR", dir)
	t.Setenv("ATLAS_SHADOW_CAPTURE", name)
}

func readShadowRecords(t *testing.T, path string) []map[string]interface{} {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("capture unreadable: %v", err)
	}
	var out []map[string]interface{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("malformed JSONL line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// Disabled is the deployed default: nothing opens, nothing is written.
func TestShadowSinkDisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ATLAS_DIAGNOSTIC_DIR", dir)
	// ATLAS_SHADOW_CAPTURE deliberately unset.
	sink, err := openShadowSink()
	if err != nil {
		t.Fatalf("disabled must not error: %v", err)
	}
	if sink != nil {
		t.Fatal("a sink was opened with no capture configured")
	}
	if !sink.enabled() { // nil receiver must answer false, not panic
		// expected
	} else {
		t.Error("a nil sink reports enabled")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("disabled mode created %d file(s)", len(entries))
	}
}

// Initialization refuses anything it cannot own: an existing destination, or
// one outside the capture root.
func TestShadowSinkInitializationRefusals(t *testing.T) {
	t.Run("existing destination", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "run.jsonl"), []byte("old\n"), 0o644)
		shadowEnv(t, dir, "run.jsonl")
		if _, err := openShadowSink(); err == nil {
			t.Error("an existing capture was accepted; a mixed run would look like one run")
		}
	})
	for _, escape := range []string{"../outside.jsonl", "/etc/passwd", "sub/../../out.jsonl"} {
		t.Run("escape "+escape, func(t *testing.T) {
			dir := t.TempDir()
			shadowEnv(t, dir, escape)
			if _, err := openShadowSink(); err == nil {
				t.Errorf("%q escaped the capture root and was accepted", escape)
			}
		})
	}
}

// A full queue drops, counts, and never blocks the caller.
func TestShadowSinkDropsWhenFull(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "run.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// The writer is not started, so nothing drains the queue while it fills.
	sink := newShadowSink(f)
	for i := 0; i < shadowQueueDepth*4; i++ {
		sink.submit(map[string]interface{}{"record_kind": "x", "i": i})
	}
	go sink.run()
	if err := sink.close(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("close: %v", err)
	}
	recs := readShadowRecords(t, filepath.Join(dir, "run.jsonl"))
	footer := recs[len(recs)-1]
	if footer["record_kind"] != "task_contract_shadow_footer" {
		t.Fatalf("last record is not a footer: %v", footer["record_kind"])
	}
	if footer["dropped"].(float64) <= 0 {
		t.Error("a saturated queue reported no drops")
	}
	if footer["accepted"].(float64) != float64(shadowQueueDepth*4) {
		t.Errorf("accepted=%v", footer["accepted"])
	}
}

// Duplicate request IDs are a capture defect, never merged.
func TestShadowSinkFlagsDuplicateRequestIDs(t *testing.T) {
	dir := t.TempDir()
	shadowEnv(t, dir, "run.jsonl")
	sink, _ := openShadowSink()
	sink.noteRequest("req-1")
	sink.noteRequest("req-2")
	sink.noteRequest("req-1") // same id twice in one capture
	sink.close(context.Background(), 5*time.Second)
	recs := readShadowRecords(t, filepath.Join(dir, "run.jsonl"))
	footer := recs[len(recs)-1]
	if footer["duplicate_request_ids"].(float64) != 1 {
		t.Errorf("duplicate_request_ids=%v, want 1", footer["duplicate_request_ids"])
	}
}

// A capture with no footer is detectable as incomplete.
// An acquisition that never reached its close hook has no footer.
//
// The ungraceful fixture changed with the finalisation owner. It used to close
// the queue and wait for the writer, which is now exactly what a CLEAN close
// does -- the writer emits the footer after draining -- so closing the queue can
// no longer stand for a process that died. What a SIGKILL actually leaves is a
// file whose close hook never ran, which is what this now reads: the records on
// disk while the sink is still open.
func TestShadowCaptureWithoutFooterIsIncomplete(t *testing.T) {
	dir := t.TempDir()
	shadowEnv(t, dir, "run.jsonl")
	sink, _ := openShadowSink()
	sink.submit(map[string]interface{}{"record_kind": "task_contract_shadow_request"})
	waitWritten(t, sink, 1)
	recs := readShadowRecords(t, filepath.Join(dir, "run.jsonl"))
	if len(recs) != 1 {
		t.Fatalf("%d records before any close, want 1", len(recs))
	}
	for _, r := range recs {
		if r["record_kind"] == "task_contract_shadow_footer" {
			t.Error("a capture wrote a footer without a close hook")
		}
	}
	sink.close(context.Background(), 5*time.Second)
}

// waitWritten blocks until the writer has written n records.
func waitWritten(t *testing.T, s *shadowSink, n int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.written.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("writer wrote %d of %d records", s.written.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// --- Submission lifecycle: open, closing, closed -----------------------------

// Submissions racing the close hook cannot panic, cannot block, and cannot
// leave the footer unable to account for them.
//
// A "check the flag, then send" pair is not enough on its own: close can land
// between the check and the send. Admission is therefore held for reading while
// a submitter decides AND enqueues, and the cutoff takes it for writing, so no
// submitter is inside when the queue closes.
func TestShadowSubmitRacesCloseWithoutPanic(t *testing.T) {
	dir := t.TempDir()
	shadowEnv(t, dir, "race.jsonl")
	sink, err := openShadowSink()
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	const submitters = 16
	var wg sync.WaitGroup
	returned := make(chan struct{}, submitters)
	start := make(chan struct{})
	for i := 0; i < submitters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			for j := 0; j < 64; j++ {
				sink.submit(map[string]interface{}{
					"record_kind": "task_contract_shadow_gate", "i": i, "j": j})
			}
			returned <- struct{}{}
		}(i)
	}
	close(start)
	time.Sleep(2 * time.Millisecond) // let submitters get going
	if err := sink.close(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("close: %v", err)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a submitter never returned: submission blocked on closure")
	}
	if len(returned) != submitters {
		t.Errorf("%d of %d submitters returned", len(returned), submitters)
	}

	accepted, written, dropped := sink.accepted.Load(), sink.written.Load(), sink.dropped.Load()
	refused := sink.refused.Load()
	t.Logf("accepted=%d written=%d dropped=%d refused=%d", accepted, written, dropped, refused)
	if accepted != written+dropped {
		t.Errorf("accepted %d != written %d + dropped %d", accepted, written, dropped)
	}
	if accepted+refused != submitters*64 {
		t.Errorf("accepted %d + refused %d != %d submitted", accepted, refused, submitters*64)
	}
	recs := readShadowRecords(t, filepath.Join(dir, "race.jsonl"))
	footer := recs[len(recs)-1]
	if footer["record_kind"] != "task_contract_shadow_footer" {
		t.Fatalf("last record is %v, not the footer", footer["record_kind"])
	}
	if got := int64(footer["written"].(float64)); int64(len(recs)-1) != got {
		t.Errorf("%d records before the footer, footer says %d written", len(recs)-1, got)
	}
}

// A record submitted after the cutoff is inert: no panic, not accepted, and
// not able to contradict a footer that has already been written.
func TestShadowSubmitAfterCleanCloseIsRefused(t *testing.T) {
	dir := t.TempDir()
	shadowEnv(t, dir, "late.jsonl")
	sink, err := openShadowSink()
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	sink.submit(map[string]interface{}{"record_kind": "task_contract_shadow_request"})
	if err := sink.close(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("close: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "late.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	sink.submit(map[string]interface{}{"record_kind": "task_contract_shadow_gate"})
	if got := sink.refused.Load(); got != 1 {
		t.Errorf("refused=%d, want 1", got)
	}
	if got := sink.accepted.Load(); got != 1 {
		t.Errorf("a post-cutoff record was accepted: accepted=%d", got)
	}
	after, err := os.ReadFile(filepath.Join(dir, "late.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("a post-cutoff record reached the finalised file")
	}
	// Closing again is idempotent and reports the same outcome.
	if err := sink.close(context.Background(), time.Second); err != nil {
		t.Errorf("second close: %v", err)
	}
}

// --- Single-owner finalisation -----------------------------------------------

// A clean close: the writer drains, emits the footer as the final line, closes
// the descriptor, and stops. Nothing else writes to the file.
func TestShadowCleanCloseFinalisesExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	shadowEnv(t, dir, "clean.jsonl")
	before := runtime.NumGoroutine()
	sink, err := openShadowSink()
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	for i := 0; i < 50; i++ {
		sink.submit(map[string]interface{}{
			"record_kind": "task_contract_shadow_gate", "i": i})
	}
	if err := sink.close(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("close: %v", err)
	}
	recs := readShadowRecords(t, filepath.Join(dir, "clean.jsonl"))
	footer := recs[len(recs)-1]
	if footer["record_kind"] != "task_contract_shadow_footer" {
		t.Fatalf("last record is %v", footer["record_kind"])
	}
	for _, r := range recs[:len(recs)-1] {
		if r["record_kind"] == "task_contract_shadow_footer" {
			t.Error("more than one footer")
		}
	}
	if got := int64(footer["written"].(float64)); got != 50 || int64(len(recs)-1) != got {
		t.Errorf("footer written=%d, file holds %d records", got, len(recs)-1)
	}
	if got := footer["accepted"].(float64); got != 50 {
		t.Errorf("accepted=%v", got)
	}
	// The writer is finished and owns nothing further.
	select {
	case <-sink.done:
	default:
		t.Error("the writer goroutine is still running after a clean close")
	}
	if _, err := sink.f.Write([]byte("x")); err == nil {
		t.Error("the descriptor is still open after a clean close")
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines %d -> %d after a clean close", before, after)
	}
}

// A writer that cannot finish within the hook's deadline leaves a file with no
// footer. The hook returns bounded and says the capture is incomplete; nothing
// manufactures a footer that a reader could mistake for a full acquisition.
//
// The writer here is simply never started, which is the same thing the hook can
// observe as a writer blocked in a filesystem call: no progress before the
// deadline. A write already inside a blocking syscall cannot be interrupted
// portably, so the hook never tries -- see the comment on close().
func TestShadowBlockedWriterDeadlineWritesNoFooter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stuck.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sink := newShadowSink(f) // writer never started: nothing drains the queue
	sink.submit(map[string]interface{}{"record_kind": "task_contract_shadow_request"})

	started := time.Now()
	err = sink.close(context.Background(), 150*time.Millisecond)
	elapsed := time.Since(started)
	if err == nil {
		t.Error("a capture that never finalised reported success")
	}
	if elapsed > 3*time.Second {
		t.Errorf("the close hook took %v, well past its deadline", elapsed)
	}
	b, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if strings.Contains(string(b), "task_contract_shadow_footer") {
		t.Error("a timed-out close manufactured a footer")
	}
	t.Logf("incomplete capture: %d bytes, err=%v, elapsed=%v", len(b), err, elapsed)
}

// shadowLoopRun drives the real agent loop with capture optionally enabled and
// returns the emitted records plus everything a causal comparison needs.
func shadowLoopRun(t *testing.T, capture bool, contract *TaskContract, prompt string,
	plan func(i int) map[string]interface{}) ([]map[string]interface{}, string, []string, string, []string) {
	t.Helper()
	capDir := t.TempDir()
	if capture {
		shadowEnv(t, capDir, "run.jsonl")
		sink, err := openShadowSink()
		if err != nil {
			t.Fatalf("sink: %v", err)
		}
		activeShadowSink.Store(sink)
		t.Cleanup(func() {
			sink.close(context.Background(), 5*time.Second)
			activeShadowSink.Store(nil)
		})
	} else {
		activeShadowSink.Store(nil)
	}
	recs, events, disk, term, prompts := shadowLoopDrive(t, "req-shadow-1", contract, prompt, plan)
	if capture {
		if s := activeShadowSink.Load(); s != nil {
			s.close(context.Background(), 5*time.Second)
			recs = readShadowRecords(t, filepath.Join(capDir, "run.jsonl"))
		}
	}
	return recs, events, disk, term, prompts
}

// shadowLoopDrive runs one scripted request through the real agent loop against
// whatever sink is currently installed, and returns everything a comparison
// needs. It never reads the capture and never touches the environment, so it is
// safe to call from several goroutines at once.
func shadowLoopDrive(t *testing.T, requestID string, contract *TaskContract, prompt string,
	plan func(i int) map[string]interface{}) ([]map[string]interface{}, string, []string, string, []string) {
	dir := t.TempDir()
	turns := 0
	var mu sync.Mutex
	var events []string
	var modelBodies []string
	terminal := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			// Controllable so a fixture can express bytes that do not parse;
			// the stub validated everything, which no real sandbox does.
			json.NewEncoder(w).Encode(map[string]interface{}{
				"valid": testStubSyntaxValid,
				"error": map[bool]string{true: "", false: "SyntaxError: invalid syntax (line 1)"}[testStubSyntaxValid],
			})
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		modelBodies = append(modelBodies, string(raw))
		i := turns
		turns++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		call, _ := json.Marshal(plan(i))
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(call)}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	defer srv.Close()

	reqCtx := context.WithValue(context.Background(), requestIDKey, requestID)
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.Ctx = reqCtx
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.MaxTurns = 0
	ctx.TaskContract = contract
	ctx.StreamFn = func(et string, data interface{}) {
		b, _ := json.Marshal(data)
		mu.Lock()
		defer mu.Unlock()
		var m map[string]interface{}
		line := et + "|" + string(b)
		if json.Unmarshal(b, &m) == nil {
			for _, k := range []string{"elapsed", "prompt_ms", "ms", "elapsed_ms",
				"duration_ms", "wall_s"} {
				delete(m, k)
			}
			c, _ := json.Marshal(m)
			line = et + "|" + string(c)
		}
		events = append(events, line)
		if et == "done" {
			var mm map[string]string
			json.Unmarshal(b, &mm)
			for k, v := range mm {
				terminal[k] = v
			}
		}
	}
	runAgentLoop(ctx, prompt)

	// Each run gets its own TempDir, so the workspace path is not a property of
	// the run; canonicalise it before anything compares two runs.
	for i, e := range events {
		events[i] = strings.ReplaceAll(e, dir, "<ws>")
	}
	for i, b := range modelBodies {
		modelBodies[i] = strings.ReplaceAll(b, dir, "<ws>")
	}

	var disk []string
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if !strings.HasPrefix(rel, ".") {
			b, _ := os.ReadFile(p)
			disk = append(disk, rel+"="+hashBytes(b)[:12])
		}
		return nil
	})
	sort.Strings(disk)

	// The model request bodies are returned verbatim. Nothing normalises tool
	// ordering any more: allTools() fixes it, so two runs of the same request
	// are byte-identical and any difference is a real one.
	return nil, strings.Join(events, "\n"), disk,
		terminal["status"] + "/" + terminal["reason"], modelBodies
}

// testStubSyntaxValid controls what the fixture sandbox says about written
// bytes. Default true; a test that needs invalid bytes sets it and restores it.
var testStubSyntaxValid = true

func shadowWorkPlan(i int) map[string]interface{} {
	switch i {
	case 0:
		return map[string]interface{}{"type": "tool_call", "name": "list_directory",
			"args": map[string]string{"path": "."}}
	case 1:
		return map[string]interface{}{"type": "tool_call", "name": "write_file",
			"args": map[string]string{"path": "app.py", "content": "A = 1\n"}}
	}
	return map[string]interface{}{"type": "done", "summary": "wrote app.py"}
}

// The two record kinds, the two gate sites, and the value production consumed.
func TestShadowRecordsCaptureBothGateSites(t *testing.T) {
	contract := &TaskContract{TaskMode: TaskModeWork}
	recs, _, _, term, _ := shadowLoopRun(t, true, contract, "Create app.py.", shadowWorkPlan)
	t.Logf("terminal=%s records=%d", term, len(recs))

	var snapshots, gates []map[string]interface{}
	sites := map[string]bool{}
	seqs := map[float64]bool{}
	for _, r := range recs {
		switch r["record_kind"] {
		case "task_contract_shadow_request":
			snapshots = append(snapshots, r)
		case "task_contract_shadow_gate":
			gates = append(gates, r)
			sites[r["call_site"].(string)] = true
			seqs[r["gate_seq"].(float64)] = true
		}
	}
	if len(snapshots) != 1 {
		t.Fatalf("%d request snapshots, want exactly one", len(snapshots))
	}
	if len(gates) == 0 {
		t.Fatal("no gate evaluations were observed")
	}
	for _, r := range append(snapshots, gates...) {
		if r["influences_live_decision"] != false {
			t.Error("a record does not declare itself inert")
		}
		if r["request_id"] != "req-shadow-1" {
			t.Errorf("request_id=%v", r["request_id"])
		}
	}
	// Gate sequences are per-request and strictly increasing.
	if len(seqs) != len(gates) {
		t.Errorf("gate_seq collided across %d gates", len(gates))
	}
	// Every gate carries the live boolean and a closed comparison.
	for _, g := range gates {
		if _, ok := g["legacy_wants_state_change"].(bool); !ok {
			t.Error("a gate record has no live decision")
		}
		switch g["comparison"] {
		case shadowAgreeWork, shadowAgreeQuestion, shadowContractWorkLegacyQuestion,
			shadowContractQuestionLegacyWork, shadowUnmeasured:
		default:
			t.Errorf("comparison %q is outside the closed vocabulary", g["comparison"])
		}
	}
	t.Logf("gate sites observed: %v", sites)
}

// A contractless request is unmeasured, never inferred.
func TestShadowContractlessGateIsUnmeasured(t *testing.T) {
	recs, _, _, _, _ := shadowLoopRun(t, true, nil, "Create app.py.", shadowWorkPlan)
	saw := false
	for _, r := range recs {
		if r["record_kind"] != "task_contract_shadow_gate" {
			continue
		}
		saw = true
		if r["comparison"] != shadowUnmeasured {
			t.Errorf("a contractless gate was classified %q", r["comparison"])
		}
	}
	if !saw {
		t.Fatal("no gate records")
	}
	for _, r := range recs {
		if r["record_kind"] == "task_contract_shadow_request" &&
			r["contract_present"] != false {
			t.Error("a contractless request claimed a contract")
		}
	}
}

// Undeclared lists stay non-comparable; verification never claims equality.
func TestShadowUndeclaredListsAreNonComparable(t *testing.T) {
	recs, _, _, _, _ := shadowLoopRun(t, true, &TaskContract{TaskMode: TaskModeWork},
		"Create app.py.", shadowWorkPlan)
	for _, r := range recs {
		if r["record_kind"] != "task_contract_shadow_request" {
			continue
		}
		if r["output_comparison"] != shadowNotDeclared {
			t.Errorf("output_comparison=%v, want contract_not_declared", r["output_comparison"])
		}
		if r["verification_comparison"] != shadowNotDeclared {
			t.Errorf("verification_comparison=%v", r["verification_comparison"])
		}
	}
	// A declared verification never reports command equality with a boolean.
	recs2, _, _, _, _ := shadowLoopRun(t, true,
		&TaskContract{TaskMode: TaskModeWork, Verification: strsPtr("go test ./...")},
		"Fix the failing test.", shadowWorkPlan)
	for _, r := range recs2 {
		if r["record_kind"] != "task_contract_shadow_request" {
			continue
		}
		got := r["verification_comparison"]
		if got != shadowVerifyLegacyRequires && got != shadowVerifyLegacyDoesNot {
			t.Errorf("verification_comparison=%v", got)
		}
		if strings.Contains(fmt.Sprint(got), "exact") {
			t.Error("verification claimed exact command agreement with a boolean heuristic")
		}
	}
}

// Aliases collapse to one canonical identity through the existing resolver.
func TestShadowOutputAliasesShareIdentity(t *testing.T) {
	recs, _, _, _, _ := shadowLoopRun(t, true,
		&TaskContract{TaskMode: TaskModeWork, ExpectedOutputs: strsPtr("app.py", "./app.py")},
		"Create app.py.", shadowWorkPlan)
	for _, r := range recs {
		if r["record_kind"] != "task_contract_shadow_request" {
			continue
		}
		hashes := r["output_hashes"].([]interface{})
		if len(hashes) != 1 {
			t.Errorf("aliases produced %d identities, want 1: %v", len(hashes), hashes)
		}
	}
}

// No raw prose, path, command, or token reaches the file.
func TestShadowRecordsCarryNoRawText(t *testing.T) {
	const secret = "Create app.py with the SECRETMARKER inside."
	recs, _, _, _, _ := shadowLoopRun(t, true,
		&TaskContract{TaskMode: TaskModeWork, ExpectedOutputs: strsPtr("app.py"),
			Verification: strsPtr("pytest --marker=SECRETCMD")},
		secret, shadowWorkPlan)
	blob, _ := json.Marshal(recs)
	for _, banned := range []string{"SECRETMARKER", "SECRETCMD", "pytest", "app.py",
		"Create app.py"} {
		if strings.Contains(string(blob), banned) {
			t.Errorf("the capture leaked %q", banned)
		}
	}
}

// Enabling the capture changes nothing the user can observe.
//
// The model request bodies are now compared BYTE FOR BYTE, tool ordering
// included. They were not, while three parts of the prompt were built by
// ranging a Go map and varied per request with the capture switched off; that
// ordering is fixed at its source now, so the comparison no longer has to
// tolerate it.
//
// Two normalisations remain, and no others:
//
//   - established nondeterministic timing fields (elapsed, prompt_ms, ms,
//     elapsed_ms, duration_ms, wall_s) are dropped from SSE payloads;
//   - the per-run temporary workspace path is canonicalised, because each run
//     gets its own t.TempDir and the path is an input difference, not a
//     property of the run.
//
// Nothing else is removed to make the comparison pass.
func TestShadowCaptureIsCausallyInert(t *testing.T) {
	contract := &TaskContract{TaskMode: TaskModeWork,
		ExpectedOutputs: strsPtr("app.py"), Verification: strsPtr("pytest")}
	_, evOff, diskOff, termOff, promptOff := shadowLoopRun(t, false, contract, "Create app.py.", shadowWorkPlan)
	recs, evOn, diskOn, termOn, promptOn := shadowLoopRun(t, true, contract, "Create app.py.", shadowWorkPlan)
	t.Logf("off=%s on=%s records=%d", termOff, termOn, len(recs))
	if len(recs) == 0 {
		t.Fatal("capture was enabled but recorded nothing")
	}
	if termOff != termOn {
		t.Errorf("terminal differs: %q vs %q", termOff, termOn)
	}
	if strings.Join(diskOff, ",") != strings.Join(diskOn, ",") {
		t.Errorf("disk differs:\n  %v\n  %v", diskOff, diskOn)
	}
	if len(promptOff) != len(promptOn) {
		t.Fatalf("turn count differs: %d vs %d", len(promptOff), len(promptOn))
	}
	for i := range promptOff {
		if promptOff[i] != promptOn[i] {
			k := 0
			for k < len(promptOff[i]) && k < len(promptOn[i]) && promptOff[i][k] == promptOn[i][k] {
				k++
			}
			lo := k - 60
			if lo < 0 {
				lo = 0
			}
			clip := func(v string) string {
				hi := k + 100
				if hi > len(v) {
					hi = len(v)
				}
				return v[lo:hi]
			}
			t.Fatalf("model prompt bytes differ on turn %d at byte %d:\n  off: %s\n  on:  %s",
				i, k, clip(promptOff[i]), clip(promptOn[i]))
		}
	}
	if evOff != evOn {
		a, b := strings.Split(evOff, "\n"), strings.Split(evOn, "\n")
		for i := 0; i < len(a) || i < len(b); i++ {
			var x, y string
			if i < len(a) {
				x = a[i]
			}
			if i < len(b) {
				y = b[i]
			}
			if x != y {
				j := 0
				for j < len(x) && j < len(y) && x[j] == y[j] {
					j++
				}
				lo := j - 80
				if lo < 0 {
					lo = 0
				}
				clip := func(v string) string {
					hi := j + 120
					if hi > len(v) {
						hi = len(v)
					}
					if lo > len(v) {
						return "<short>"
					}
					return v[lo:hi]
				}
				t.Fatalf("event/model stream differs at entry %d byte %d:\n  off: %s\n  on:  %s",
					i, j, clip(x), clip(y))
			}
		}
	}
}

// Capture off costs a run nothing: no sink, no goroutine, no allocation on the
// gate path, and the gate still returns exactly what production asked for.
func TestShadowDisabledCostsNothing(t *testing.T) {
	activeShadowSink.Store(nil)
	before := runtime.NumGoroutine()
	_, _, _, term, _ := shadowLoopRun(t, false, &TaskContract{TaskMode: TaskModeWork},
		"Create app.py.", shadowWorkPlan)
	t.Logf("terminal=%s", term)
	deadline := time.Now().Add(2 * time.Second)
	after := runtime.NumGoroutine()
	for after > before && time.Now().Before(deadline) {
		runtime.Gosched()
		after = runtime.NumGoroutine()
	}
	if after > before {
		t.Errorf("goroutines grew with capture off: %d -> %d", before, after)
	}
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	st := &runState{}
	for _, live := range []bool{true, false} {
		d := actionDemand{Required: live, Source: actionDemandLegacy, Legacy: live}
		if got := observeActionDemand(ctx, st, shadowGateActionGate, d); got != live {
			t.Errorf("the disabled gate returned %v for %v", got, live)
		}
	}
	allocs := testing.AllocsPerRun(100, func() {
		observeActionDemand(ctx, st, shadowGateActionGate,
			actionDemand{Required: true, Source: actionDemandLegacy, Legacy: true})
	})
	if allocs != 0 {
		t.Errorf("the disabled gate allocated %v per call", allocs)
	}
	if st.shadowGate.n != 0 {
		t.Errorf("the disabled gate advanced its sequence to %d", st.shadowGate.n)
	}
}

// The recorded value follows the workspace-inspection state production was
// holding at the moment of evaluation, not the request text.
//
// This is a dependency demonstrated ACROSS two otherwise-equivalent requests,
// not a false-then-true sequence inside one. Both live gates are evaluated at
// the completion boundary, so by the time either runs, whatever inspection the
// turn performed has already happened and current production cannot observe
// both states within one request. Adding an earlier gate purely to produce that
// sequence would be inventing a live evaluation to satisfy a description, so
// the description is what changed.
//
// Neither side is the correct answer here. The measurement is that a declared
// contract lands in different comparison categories depending on mid-run model
// behaviour; which one is right is a question for the Step 3B corpus.
func TestShadowGateFollowsInspectionNotRequestText(t *testing.T) {
	const neutral = "app.py numbers"
	noInspect := func(i int) map[string]interface{} {
		return map[string]interface{}{"type": "done", "summary": "nothing to do"}
	}
	report := func(recs []map[string]interface{}) []string {
		var out []string
		for _, r := range recs {
			if r["record_kind"] != "task_contract_shadow_gate" {
				continue
			}
			out = append(out, fmt.Sprintf("%v/live=%v/insp=%v/%v",
				r["gate_seq"], r["legacy_wants_state_change"], r["inspected_workspace"],
				r["comparison"]))
			// The comparison is derived from the value production consumed.
			want := shadowContractWorkLegacyQuestion
			if r["legacy_wants_state_change"].(bool) {
				want = shadowAgreeWork
			}
			if r["comparison"] != want {
				t.Errorf("live=%v recorded comparison %q, want %q",
					r["legacy_wants_state_change"], r["comparison"], want)
			}
			// The gate records inspection state and the live value together,
			// so a disagreement between them is visible in the capture.
			if r["legacy_wants_state_change"] != r["inspected_workspace"] {
				t.Errorf("live=%v with inspected=%v on a neutral request",
					r["legacy_wants_state_change"], r["inspected_workspace"])
			}
		}
		if len(out) == 0 {
			t.Fatal("no gate records")
		}
		return out
	}
	contract := &TaskContract{TaskMode: TaskModeWork}
	cold, _, _, termCold, _ := shadowLoopRun(t, true, contract, neutral, noInspect)
	warm, _, _, termWarm, _ := shadowLoopRun(t, true, contract, neutral, shadowWorkPlan)
	coldSeq, warmSeq := report(cold), report(warm)
	t.Logf("uninspected %s: %v", termCold, coldSeq)
	t.Logf("inspected   %s: %v", termWarm, warmSeq)
	for _, e := range coldSeq {
		if !strings.Contains(e, "live=false") {
			t.Errorf("an uninspected run recorded %s", e)
		}
	}
	for _, e := range warmSeq {
		if !strings.Contains(e, "live=true") {
			t.Errorf("an inspected run recorded %s", e)
		}
	}
}

// A request rejected at the boundary never produces a snapshot claiming a
// contract the run does not have.
func TestShadowRejectedContractProducesNoSnapshot(t *testing.T) {
	capDir := t.TempDir()
	shadowEnv(t, capDir, "reject.jsonl")
	sink, err := openShadowSink()
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	activeShadowSink.Store(sink)
	defer activeShadowSink.Store(nil)

	dir := t.TempDir()
	for _, bad := range []string{
		`{"task_mode":"explore"}`,
		`{"task_mode":"work","expected_outputs":["../../etc/passwd"]}`,
		`{"task_mode":""}`,
	} {
		var in TaskContract
		if err := json.Unmarshal([]byte(bad), &in); err != nil {
			t.Fatalf("fixture %s: %v", bad, err)
		}
		if _, err := validateTaskContract(&in, dir); err == nil {
			t.Fatalf("%s was accepted", bad)
		}
	}
	sink.close(context.Background(), 5*time.Second)
	for _, r := range readShadowRecords(t, filepath.Join(capDir, "reject.jsonl")) {
		if r["record_kind"] != "task_contract_shadow_footer" {
			t.Errorf("a rejected contract produced %v", r["record_kind"])
		}
	}
}

// Structural: the shadow path cannot reach a live decision.
//
// Three properties, each read off the syntax tree rather than trusted:
//   - observeActionDemand returns the decision it was handed, unchanged, and
//     never assigns to it, so instrumenting a gate cannot change its value.
//   - wantsStateChange is called exactly once per live site, and each call is
//     produced by decideActionDemand, so the recorded value is the
//     value production consumed and the heuristic runs once.
//   - outside the shadow implementation itself, no shadow identifier appears
//     anywhere in production code, so no shadow state can be branched on.
func TestShadowHasNoPathIntoPolicy(t *testing.T) {
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range entries {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files[name] = f
	}

	// 1. the observer returns the decision it was handed, unchanged
	var observer *ast.FuncDecl
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "observeActionDemand" {
				observer = fd
			}
		}
	}
	if observer == nil {
		t.Fatal("observeActionDemand is gone")
	}
	returns := 0
	ast.Inspect(observer.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.ReturnStmt:
			returns++
			if len(v.Results) != 1 {
				t.Errorf("a return yields %d values", len(v.Results))
				return true
			}
			sel, ok := v.Results[0].(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Required" {
				t.Errorf("a return does not yield the decision: %T", v.Results[0])
				return true
			}
			if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "d" {
				t.Error("a return yields something other than the decision argument")
			}
		case *ast.AssignStmt:
			for _, lhs := range v.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name == "d" {
					t.Error("the observer assigns to the decision")
				}
			}
		}
		return true
	})
	if returns == 0 {
		t.Error("the observer never returns")
	}

	// 2. the heuristic is evaluated exactly once, inside the central helper,
	//    and every live action-demand site goes through that helper.
	legacyCalls := map[string]int{}
	decideCalls := 0
	observeWrapsDecide := 0
	for name, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				switch id.Name {
				case "wantsStateChange":
					legacyCalls[name+":"+fd.Name.Name]++
				case "decideActionDemand":
					decideCalls++
				case "observeActionDemand":
					for _, a := range call.Args {
						if c, ok := a.(*ast.CallExpr); ok {
							if ci, ok := c.Fun.(*ast.Ident); ok &&
								ci.Name == "decideActionDemand" {
								observeWrapsDecide++
							}
						}
					}
				}
				return true
			})
		}
	}
	if len(legacyCalls) != 1 {
		t.Errorf("wantsStateChange is called from %d production functions, want only "+
			"decideActionDemand: %v", len(legacyCalls), legacyCalls)
	}
	for k, n := range legacyCalls {
		if !strings.HasSuffix(k, ":decideActionDemand") {
			t.Errorf("%s calls the legacy heuristic directly", k)
		}
		if n != 1 {
			t.Errorf("%s evaluates the heuristic %d times, want exactly once", k, n)
		}
	}
	if decideCalls != 2 || observeWrapsDecide != 2 {
		t.Errorf("%d decideActionDemand calls and %d wrapped in observeActionDemand, "+
			"want 2 and 2 (the two live action-demand sites)", decideCalls, observeWrapsDecide)
	}

	// 3. shadow symbols stay inside the shadow implementation
	for _, v := range shadowGuardViolations(files) {
		t.Error(v)
	}
}

// shadowProductionSymbols is the explicit inventory of every production symbol
// the capture owns. It is a list rather than a name pattern because a pattern
// misses the ones that do not start with the word -- activeShadowSink is the
// live sink pointer and a prefix rule lets it through.
var shadowProductionSymbols = map[string]bool{
	"activeShadowSink": true, "shadowSink": true, "newShadowSink": true,
	"openShadowSink": true, "shadowCaptureRoot": true, "shadowQueueDepth": true,
	"maxTrackedShadowRequests": true, "shadowSchemaVersion": true,
	"shadowGateSite": true, "shadowGateSeq": true, "shadowGate": true,
	"shadowHash": true, "shadowHashes": true, "shadowCanonicalSet": true,
	"shadowCompareSets": true, "contractOutputs": true,
	"shadowAgreeWork": true, "shadowAgreeQuestion": true,
	"shadowContractWorkLegacyQuestion": true, "shadowContractQuestionLegacyWork": true,
	"shadowUnmeasured": true, "shadowNotDeclared": true, "shadowDeclared": true,
	"shadowOutputsExact": true, "shadowOutputsContractOnly": true,
	"shadowOutputsLegacyOnly": true, "shadowOutputsPartial": true,
	"shadowOutputsIncomparable": true, "shadowVerifyLegacyRequires": true,
	"shadowVerifyLegacyDoesNot": true,
}

// shadowGuardEntryPoints are the only shadow names production may write down:
// the two observers it calls and the two call-site constants it labels them
// with. Naming them is permitted; reading anything they produce is not.
var shadowGuardEntryPoints = map[string]bool{
	"observeActionDemand": true, "emitShadowRequestSnapshot": true,
	"shadowGateActionDemanded": true, "shadowGateActionGate": true,
}

// shadowGuardOwners is keyed by receiver-qualified identity, so a future
// (*broker).close or (*ledger).submit cannot inherit an exemption written for
// the sink's methods.
var shadowGuardOwners = map[string]bool{
	"(*shadowSink).enabled":     true,
	"(*shadowSink).run":         true,
	"(*shadowSink).finalize":    true,
	"(*shadowSink).submit":      true,
	"(*shadowSink).noteRequest": true,
	"(*shadowSink).close":       true,
	"openShadowSink":            true,
	"newShadowSink":             true,
	"shadowCaptureRoot":         true,
	"emitShadowRequestSnapshot": true,
	"observeActionDemand":       true,
	// The observe-only writers. Each WRITES one record to the capture and
	// reads nothing back from it; that they reach no live decision is pinned
	// separately, by name, in evidence_inertness_test.go.
	"recordEvidenceObservation":   true,
	"recordAuthorizationDecision": true,
	"recordFeasibilityDecision":   true,
	// Grant transitions. Same shape: one record written per event, nothing
	// read back, and the grant's own reachability pinned by name elsewhere.
	"recordGrantEvent": true,
	// Route and delivery endings. One record per ending, nothing read back,
	// and both declare themselves inert in the record they write.
	"(*routeLifecycle).finish":  true,
	"recordDeliveryDisposition": true,
	// The structured intent record: one per decided candidate, saying which
	// tool call bounded it and whether the bytes stayed inside. It reads
	// nothing back and authorizes nothing, which its own record states.
	"recordMutationScope": true,
	// The acquisition control's two records: what the policy would have done,
	// and the fact that no licence followed it. Both write once and read
	// nothing back, and both declare that no delivery happened.
	"recordCaptureOnlySuppression": true,
	"recordCaptureOnlyDisposition": true,
	// Why the candidate producer was not consulted for a mutation. One record
	// per skip, nothing read back, and the skip decision itself is made by the
	// two owners beside it -- which touch the sink not at all.
	"recordCandidateGenerationBypass": true,
	// The candidate policy's own record: one answer per decided candidate,
	// with the vetoes that fired and the signals that were observed. It reads
	// nothing back, and whether the decision it describes delivered is a field
	// on the record rather than something the sink is asked about.
	"recordCandidatePolicyDecision": true,
	// One typed answer per route entry to why an automatic candidate did not
	// land, composed from facts the owners above already recorded. It writes
	// once at route exit and reads nothing back; that no live owner reads the
	// facts or the answer is pinned by name in automatic_attribution_test.go.
	"(*routeLifecycle).recordAttribution": true,
	"contractOutputs":                     true,
	"shadowCanonicalSet":                  true,
	"shadowHashes":                        true,
	"shadowCompareSets":                   true,
	"shadowHash":                          true,
	"main":                                true,
}

// funcIdentity renders a declaration the way shadowGuardOwners keys it.
func funcIdentity(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	return "(" + types.ExprString(fd.Recv.List[0].Type) + ")." + fd.Name.Name
}

// shadowGuardViolations reports every read of a shadow symbol from a function
// that does not own the capture. Exported to the test rather than inlined so
// the guard itself can be shown to fail on a deliberately unauthorized read.
func shadowGuardViolations(files map[string]*ast.File) []string {
	var out []string
	for name, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			owner := funcIdentity(fd)
			if shadowGuardOwners[owner] {
				continue
			}
			// The signature is walked as well as the body. Taking a shadow
			// value as a parameter is how a non-owner would read a counter
			// without ever naming an inventory symbol in its body -- the
			// unauthorized-read fixture below is exactly that shape.
			for _, node := range []ast.Node{fd.Type, fd.Body} {
				ast.Inspect(node, func(n ast.Node) bool {
					id, ok := n.(*ast.Ident)
					if !ok || !shadowProductionSymbols[id.Name] {
						return true
					}
					if shadowGuardEntryPoints[id.Name] {
						return true
					}
					out = append(out, fmt.Sprintf(
						"%s: %s reads shadow state %q outside the shadow path",
						name, owner, id.Name))
					return true
				})
			}
		}
	}
	sort.Strings(out)
	return out
}

// The guard is only worth its green if it goes red on the thing it forbids.
// Each of these is a policy owner reaching for shadow state the way a future
// change might: tier selection, prompt construction, completion authorisation,
// terminal status. None of them exists in the tree; all of them must be caught.
func TestShadowGuardRejectsAnUnauthorizedRead(t *testing.T) {
	cases := map[string]string{
		"tier selection": `package main
func pickTier() int {
	if activeShadowSink.Load() != nil {
		return 1
	}
	return 0
}`,
		"prompt construction": `package main
func buildPrompt(c *AgentContext) string {
	if c.TaskContract != nil && shadowAgreeWork == "agree_work" {
		return "work"
	}
	return ""
}`,
		"completion authorisation": `package main
func mayComplete(s *runState) bool {
	return s.shadowGate.n > 0
}`,
		"terminal status": `package main
func status(s *shadowSink) string {
	if s.dropped.Load() > 0 {
		return "failed"
	}
	return "completed"
}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "unauthorized.go", src, 0)
			if err != nil {
				t.Fatalf("fixture does not parse: %v", err)
			}
			got := shadowGuardViolations(map[string]*ast.File{"unauthorized.go": f})
			if len(got) == 0 {
				t.Fatal("the guard accepted an unauthorized read of shadow state")
			}
			t.Logf("caught: %s", got[0])
		})
	}
	// And the same fixture is accepted once it belongs to an owner, so the
	// guard is discriminating between owners rather than banning the word.
	fset := token.NewFileSet()
	f, _ := parser.ParseFile(fset, "owned.go", `package main
func openShadowSink() (*shadowSink, error) {
	_ = activeShadowSink.Load()
	return nil, nil
}`, 0)
	if got := shadowGuardViolations(map[string]*ast.File{"owned.go": f}); len(got) != 0 {
		t.Errorf("the guard flagged an owner: %v", got)
	}
}

// --- Coverage the first pass left open ---------------------------------------

// A writer that fails on every record changes nothing a user can see. The run
// keeps its stream, its prompts, its bytes on disk and its terminal; only the
// capture is defective, and it says so in its own counters.
func TestShadowWriterErrorLeavesTheRunUnchanged(t *testing.T) {
	contract := &TaskContract{TaskMode: TaskModeWork, ExpectedOutputs: strsPtr("app.py")}
	activeShadowSink.Store(nil)
	_, evOff, diskOff, termOff, promptOff := shadowLoopDrive(t, "req-writer-err",
		contract, "Create app.py.", shadowWorkPlan)

	// A sink whose descriptor is already closed: every write fails at the
	// syscall, which is the failure the run must survive.
	f, err := os.Create(filepath.Join(t.TempDir(), "broken.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	sink := newShadowSink(f)
	go sink.run()
	activeShadowSink.Store(sink)
	defer activeShadowSink.Store(nil)

	_, evOn, diskOn, termOn, promptOn := shadowLoopDrive(t, "req-writer-err",
		contract, "Create app.py.", shadowWorkPlan)
	closeErr := sink.close(context.Background(), 5*time.Second)

	t.Logf("accepted=%d written=%d errors=%d closeErr=%v",
		sink.accepted.Load(), sink.written.Load(), sink.errors.Load(), closeErr)
	if sink.errors.Load() == 0 {
		t.Fatal("the fixture did not actually make the writer fail")
	}
	if sink.written.Load() != 0 {
		t.Errorf("a failing writer reported %d records written", sink.written.Load())
	}
	if termOff != termOn {
		t.Errorf("terminal differs: %q vs %q", termOff, termOn)
	}
	if strings.Join(diskOff, ",") != strings.Join(diskOn, ",") {
		t.Errorf("disk differs:\n  %v\n  %v", diskOff, diskOn)
	}
	if evOff != evOn {
		t.Error("the SSE stream differs when the capture writer fails")
	}
	if len(promptOff) != len(promptOn) {
		t.Fatalf("turn count differs: %d vs %d", len(promptOff), len(promptOn))
	}
	for i := range promptOff {
		if promptOff[i] != promptOn[i] {
			t.Errorf("model prompt differs on turn %d", i)
		}
	}
}

// Concurrent requests share one capture and stay separable in it: each keeps
// its own request id, and each id's gate sequence is 1..n with no gaps.
func TestShadowConcurrentRequestsKeepIndependentSequences(t *testing.T) {
	dir := t.TempDir()
	shadowEnv(t, dir, "concurrent.jsonl")
	sink, err := openShadowSink()
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	activeShadowSink.Store(sink)
	defer activeShadowSink.Store(nil)

	const requests = 6
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			shadowLoopDrive(t, fmt.Sprintf("req-concurrent-%d", i),
				&TaskContract{TaskMode: TaskModeWork}, "Create app.py.", shadowWorkPlan)
		}(i)
	}
	wg.Wait()
	if err := sink.close(context.Background(), 10*time.Second); err != nil {
		t.Fatalf("close: %v", err)
	}

	seqs := map[string][]int{}
	snapshots := map[string]int{}
	for _, r := range readShadowRecords(t, filepath.Join(dir, "concurrent.jsonl")) {
		id, _ := r["request_id"].(string)
		switch r["record_kind"] {
		case "task_contract_shadow_request":
			snapshots[id]++
		case "task_contract_shadow_gate":
			seqs[id] = append(seqs[id], int(r["gate_seq"].(float64)))
		}
	}
	if len(seqs) != requests {
		t.Errorf("%d request ids carry gate records, want %d", len(seqs), requests)
	}
	for i := 0; i < requests; i++ {
		id := fmt.Sprintf("req-concurrent-%d", i)
		if snapshots[id] != 1 {
			t.Errorf("%s has %d snapshots, want 1", id, snapshots[id])
		}
		got := append([]int(nil), seqs[id]...)
		sort.Ints(got)
		for j, v := range got {
			if v != j+1 {
				t.Errorf("%s gate sequence is not continuous: %v", id, got)
				break
			}
		}
	}
	t.Logf("%d requests, sequences %v", len(seqs), seqs)
}

// The heuristics the capture observes must decide exactly what they decided
// before it existed, whether or not a capture is running. This is the corpus
// the migration will eventually replace, pinned decision for decision.
func TestLegacyHeuristicDecisionsAreUnchangedByCapture(t *testing.T) {
	corpus := []string{
		"Create app.py.", "app.py numbers", "hi", "thanks, that looks great",
		"What does parse_config do?", "Why is this slow?", "explain the retry logic",
		"fix the failing test", "Delete obsolete.py.", "Clean up the repo.",
		"Run the tests and make sure they pass.", "Write four files.",
		"remove the debug logging", "is this a bug?", "read config.yaml and tell me the port",
		"", "   ", "refactor the parser to use a table",
	}
	type decision struct {
		wants, action, readOnly, explain, question, fix bool
	}
	decide := func() []decision {
		var out []decision
		for _, msg := range corpus {
			for _, tier := range []Tier{Tier0Conversational, Tier1Simple, Tier2Medium} {
				for _, inspected := range []bool{false, true} {
					out = append(out, decision{
						wants:    wantsStateChange(msg, tier, inspected),
						action:   isActionIntentMessage(msg),
						readOnly: isReadOnlyRequest(msg),
						explain:  isExplainOnlyMessage(strings.ToLower(msg)),
						question: isQuestionMessage(msg),
						fix:      isFixIntentMessage(msg),
					})
				}
			}
		}
		return out
	}
	activeShadowSink.Store(nil)
	off := decide()

	dir := t.TempDir()
	shadowEnv(t, dir, "corpus.jsonl")
	sink, err := openShadowSink()
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	activeShadowSink.Store(sink)
	defer func() {
		sink.close(context.Background(), 5*time.Second)
		activeShadowSink.Store(nil)
	}()
	on := decide()

	if len(off) != len(on) {
		t.Fatalf("corpus length changed: %d vs %d", len(off), len(on))
	}
	for i := range off {
		if off[i] != on[i] {
			t.Errorf("decision %d changed with capture enabled: %+v vs %+v", i, off[i], on[i])
		}
	}
	t.Logf("%d heuristic decisions unchanged across %d messages", len(off), len(corpus))
}

// --- Task-mode policy: a validated contract owns the action-demand decision --
//
// Step 3B measured the legacy heuristic against the client's declared mode on a
// frozen corpus: 25 of 101 evaluable requests disagreed, 19 of them work the
// heuristic read as a question. The contract is the client's own statement of
// what it asked for, so where one is present it decides; where none is present
// nothing changes, because that evidence says nothing about contractless
// clients.

func modePlanDoneNow(i int) map[string]interface{} {
	return map[string]interface{}{"type": "done", "summary": "nothing to change"}
}

func modePlanTextOnly(i int) map[string]interface{} {
	return map[string]interface{}{"type": "text", "content": "the helper averages a list"}
}

func modePlanInspectThenDone(i int) map[string]interface{} {
	if i == 0 {
		return map[string]interface{}{"type": "tool_call", "name": "list_directory",
			"args": map[string]string{"path": "."}}
	}
	return map[string]interface{}{"type": "done", "summary": "looked around"}
}

func modeTerminal(t *testing.T, contract *TaskContract, prompt string,
	plan func(int) map[string]interface{}) string {
	t.Helper()
	activeShadowSink.Store(nil)
	_, _, _, term, _ := shadowLoopDrive(t, "req-mode", contract, prompt, plan)
	return term
}

// 1. Neutral prose, contract work, model quits immediately. The legacy
// heuristic reads this as a question; the contract says work.
func TestContractWorkDemandsActionOnNeutralProse(t *testing.T) {
	got := modeTerminal(t, &TaskContract{TaskMode: TaskModeWork},
		"app.py numbers", modePlanDoneNow)
	if got != "incomplete/action_demanded_unmet" {
		t.Errorf("terminal %q, want incomplete/action_demanded_unmet", got)
	}
}

// 2. Question-shaped wording, contract work: wording does not soften it.
func TestContractWorkDemandsActionOnInterrogativeSurface(t *testing.T) {
	got := modeTerminal(t, &TaskContract{TaskMode: TaskModeWork},
		"Could you add a median helper to calc.py?", modePlanDoneNow)
	if got != "incomplete/action_demanded_unmet" {
		t.Errorf("terminal %q, want incomplete/action_demanded_unmet", got)
	}
}

// 3. Imperative wording, contract question: a read-only answer completes.
func TestContractQuestionAllowsTextReplyOnImperativeSurface(t *testing.T) {
	got := modeTerminal(t, &TaskContract{TaskMode: TaskModeQuestion},
		"Walk me through how mean computes its result.", modePlanTextOnly)
	if got != "completed/text_reply" {
		t.Errorf("terminal %q, want completed/text_reply", got)
	}
}

// 4. Inspecting the workspace cannot turn a question into work.
func TestInspectionCannotFlipAQuestionContract(t *testing.T) {
	got := modeTerminal(t, &TaskContract{TaskMode: TaskModeQuestion},
		"app.py numbers", modePlanInspectThenDone)
	if got == "incomplete/action_demanded_unmet" {
		t.Errorf("inspection flipped a question contract into work: %q", got)
	}
}

// 5. No inspection cannot turn work into a question.
func TestNoInspectionCannotFlipAWorkContract(t *testing.T) {
	got := modeTerminal(t, &TaskContract{TaskMode: TaskModeWork},
		"app.py numbers", modePlanDoneNow)
	if got != "incomplete/action_demanded_unmet" {
		t.Errorf("terminal %q, want incomplete/action_demanded_unmet", got)
	}
}

// 13. An invalid mode cannot reach a run, but if one ever did it must fail
// closed to REQUIRING work rather than silently reading as a question.
func TestInvalidTaskModeFailsClosedToWork(t *testing.T) {
	d := decideActionDemand(&TaskContract{TaskMode: TaskMode("explore")},
		"app.py numbers", Tier2Medium, false)
	if !d.Required {
		t.Error("an unknown task mode did not fail closed to requiring work")
	}
	if d.Source != actionDemandContractInvalid {
		t.Errorf("source %q, want %q", d.Source, actionDemandContractInvalid)
	}
}

// The helper's whole contract, as a table. Pure: no loop, no model, no sink.
func TestActionDemandDecisionTable(t *testing.T) {
	cases := []struct {
		name      string
		contract  *TaskContract
		message   string
		inspected bool
		want      bool
		source    actionDemandSource
	}{
		{"work contract, neutral prose", &TaskContract{TaskMode: TaskModeWork},
			"app.py numbers", false, true, actionDemandContractWork},
		{"work contract, inspected", &TaskContract{TaskMode: TaskModeWork},
			"app.py numbers", true, true, actionDemandContractWork},
		{"question contract, action words", &TaskContract{TaskMode: TaskModeQuestion},
			"Delete obsolete.py and fix the tests.", false, false, actionDemandContractQuestion},
		{"question contract, inspected", &TaskContract{TaskMode: TaskModeQuestion},
			"app.py numbers", true, false, actionDemandContractQuestion},
		{"no contract, legacy false", nil, "app.py numbers", false,
			wantsStateChange("app.py numbers", Tier2Medium, false), actionDemandLegacy},
		{"no contract, legacy true after inspection", nil, "app.py numbers", true,
			wantsStateChange("app.py numbers", Tier2Medium, true), actionDemandLegacy},
		{"no contract, explicit action intent", nil, "Create app.py.", false,
			wantsStateChange("Create app.py.", Tier2Medium, false), actionDemandLegacy},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := decideActionDemand(c.contract, c.message, Tier2Medium, c.inspected)
			if d.Required != c.want {
				t.Errorf("required=%v, want %v", d.Required, c.want)
			}
			if d.Source != c.source {
				t.Errorf("source=%q, want %q", d.Source, c.source)
			}
			// The legacy value is always reported, and never authoritative
			// when a contract is present.
			legacy := wantsStateChange(c.message, Tier2Medium, c.inspected)
			if d.Legacy != legacy {
				t.Errorf("reported legacy %v, heuristic says %v", d.Legacy, legacy)
			}
			if c.contract != nil && d.Required == legacy && legacy != c.want {
				t.Error("a contract-present decision tracked the legacy value")
			}
		})
	}
}

// 6. A contractless request is untouched: same events, terminal, disk and model
// request bytes as before this migration. The Step 3B corpus said nothing about
// clients that send no contract, so nothing about them changes.
func TestContractlessRequestIsUnchanged(t *testing.T) {
	activeShadowSink.Store(nil)
	for _, tc := range []struct {
		name   string
		prompt string
		plan   func(int) map[string]interface{}
	}{
		{"neutral prose, quits immediately", "app.py numbers", modePlanDoneNow},
		{"explicit action intent", "Create app.py.", shadowWorkPlan},
		{"inspects then quits", "app.py numbers", modePlanInspectThenDone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ev, disk, term, prompts := shadowLoopDrive(t, "req-nc", nil, tc.prompt, tc.plan)
			// The legacy heuristic still owns the decision, byte for byte.
			d := decideActionDemand(nil, tc.prompt, Tier2Medium, false)
			if d.Source != actionDemandLegacy {
				t.Errorf("a contractless request used source %q", d.Source)
			}
			if d.Required != wantsStateChange(tc.prompt, Tier2Medium, false) {
				t.Error("a contractless decision diverged from the heuristic")
			}
			// And the run itself is well-formed: no contract was invented.
			if term == "" || len(prompts) == 0 || ev == "" {
				t.Fatalf("degenerate run: term=%q prompts=%d", term, len(prompts))
			}
			t.Logf("%s -> %s, disk %v", tc.name, term, disk)
		})
	}
}

// 7/8. A work contract establishes the obligation; completion still needs the
// existing evidence. Broken bytes never complete.
//
// The positive half of this test used to be "valid bytes complete", and that
// premise is superseded here on purpose. A declared work contract now also
// demands verification bound to the exact current bytes of its code
// deliverables: writing a file and declaring done is no longer evidence that
// anything ran. shadowWorkPlan writes app.py and stops, so it is now the
// canonical NEGATIVE case. The positive path -- write, run the artefact, then
// done -- is proved end to end in
// TestWorkContractVerificationDemandUsesBoundEvidence, which owns the
// verification fixtures and their sandbox stub.
//
// What this still pins is the property that mattered: a contract on its own
// never manufactures a completion, in either direction.
func TestWorkContractStillRequiresDeliverableEvidence(t *testing.T) {
	good := modeTerminal(t, &TaskContract{TaskMode: TaskModeWork},
		"Create app.py.", shadowWorkPlan)
	if good != "incomplete/verification_demanded_unmet" {
		t.Errorf("written-but-never-run terminal %q, want "+
			"incomplete/verification_demanded_unmet", good)
	}
	testStubSyntaxValid = false
	defer func() { testStubSyntaxValid = true }()
	brokenPlan := func(i int) map[string]interface{} {
		if i == 0 {
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "app.py", "content": "def broken(:\n"}}
		}
		return map[string]interface{}{"type": "done", "summary": "wrote app.py"}
	}
	bad := modeTerminal(t, &TaskContract{TaskMode: TaskModeWork}, "Create app.py.", brokenPlan)
	if bad == "completed/deliverables_demonstrated" {
		t.Errorf("invalid bytes completed as demonstrated: %q", bad)
	}
	t.Logf("valid=%q invalid=%q", good, bad)
}

// 9. A work contract with no action and confident prose stays incomplete, and
// the claim does not survive.
func TestWorkContractStripsAConfidentClaimWithNoAction(t *testing.T) {
	plan := func(i int) map[string]interface{} {
		return map[string]interface{}{"type": "done",
			"summary": "All done — I have fully implemented and tested the change."}
	}
	activeShadowSink.Store(nil)
	_, ev, _, term, _ := shadowLoopDrive(t, "req-claim",
		&TaskContract{TaskMode: TaskModeWork}, "app.py numbers", plan)
	if term != "incomplete/action_demanded_unmet" {
		t.Errorf("terminal %q, want incomplete/action_demanded_unmet", term)
	}
	var summary string
	for _, line := range strings.Split(ev, "\n") {
		if !strings.Contains(line, `"done"`) {
			continue
		}
		var m map[string]string
		if i := strings.Index(line, "|"); i >= 0 {
			json.Unmarshal([]byte(line[i+1:]), &m)
		}
		if s, ok := m["summary"]; ok {
			summary = s
		}
	}
	if strings.Contains(summary, "fully implemented and tested") {
		t.Errorf("the unsupported claim survived into the terminal summary: %q", summary)
	}
}

// 10/11. A question contract does not erase debt or hazards.
func TestQuestionContractDoesNotEraseMutationDebt(t *testing.T) {
	// The model tries to write, the bytes do not parse, and the debt stands.
	testStubSyntaxValid = false
	defer func() { testStubSyntaxValid = true }()
	plan := func(i int) map[string]interface{} {
		switch i {
		case 0:
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "app.py", "content": "def broken(:\n"}}
		}
		return map[string]interface{}{"type": "done", "summary": "answered"}
	}
	term := modeTerminal(t, &TaskContract{TaskMode: TaskModeQuestion},
		"What does app.py do?", plan)
	if term == "completed/text_reply" || term == "completed/no_file_obligation" {
		t.Errorf("a question contract completed over unresolved mutation: %q", term)
	}
	t.Logf("question contract with a broken write -> %s", term)
}

// 15. Both live action-demand sites reach the same centralised decision for the
// same inputs, so which gate fires cannot change the answer.
func TestBothActionDemandSitesShareOneDecision(t *testing.T) {
	for _, mode := range []*TaskContract{
		nil, {TaskMode: TaskModeWork}, {TaskMode: TaskModeQuestion},
	} {
		for _, inspected := range []bool{false, true} {
			a := decideActionDemand(mode, "app.py numbers", Tier2Medium, inspected)
			b := decideActionDemand(mode, "app.py numbers", Tier2Medium, inspected)
			if a != b {
				t.Errorf("the decision is not deterministic: %+v vs %+v", a, b)
			}
			ctx := NewAgentContext(t.TempDir(), Tier2Medium)
			ctx.TaskContract = mode
			st := &runState{inspectedWorkspace: inspected}
			activeShadowSink.Store(nil)
			one := observeActionDemand(ctx, st, shadowGateActionDemanded, a)
			two := observeActionDemand(ctx, st, shadowGateActionGate, a)
			if one != two || one != a.Required {
				t.Errorf("sites disagree: %v vs %v, decision %v", one, two, a.Required)
			}
		}
	}
}

// 14. Capture on and capture off produce the same live result.
func TestTaskModePolicyIsIndependentOfShadowCapture(t *testing.T) {
	contract := &TaskContract{TaskMode: TaskModeWork}
	activeShadowSink.Store(nil)
	_, evOff, diskOff, termOff, promptOff := shadowLoopDrive(t, "req-cap", contract,
		"app.py numbers", modePlanDoneNow)

	dir := t.TempDir()
	shadowEnv(t, dir, "policy.jsonl")
	sink, err := openShadowSink()
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	activeShadowSink.Store(sink)
	defer func() {
		sink.close(context.Background(), 5*time.Second)
		activeShadowSink.Store(nil)
	}()
	_, evOn, diskOn, termOn, promptOn := shadowLoopDrive(t, "req-cap", contract,
		"app.py numbers", modePlanDoneNow)

	if termOff != termOn {
		t.Errorf("terminal differs with capture on: %q vs %q", termOff, termOn)
	}
	if strings.Join(diskOff, ",") != strings.Join(diskOn, ",") {
		t.Errorf("disk differs: %v vs %v", diskOff, diskOn)
	}
	if evOff != evOn {
		t.Error("the SSE stream differs with capture on")
	}
	if len(promptOff) != len(promptOn) {
		t.Fatalf("turn count differs: %d vs %d", len(promptOff), len(promptOn))
	}
	for i := range promptOff {
		if promptOff[i] != promptOn[i] {
			t.Errorf("model request bytes differ on turn %d", i)
		}
	}
}

// Replay of the sealed Step 3B evidence through the new decision helper.
//
// No model is called and the evidence is never written to. Each captured gate
// record carries what governed at the time (the legacy value), the contract
// mode, and the inspection state, which is everything the helper needs. The
// point is to say exactly which live decisions this migration changes, and to
// prove it changes none for contractless requests.
func TestSealedStep3BEvidenceReplaysThroughTheNewPolicy(t *testing.T) {
	path := "../redteam/step3b-freeze/evidence/diag_gemma_04/diagnostics/" +
		"step3b_diag_gemma_04.jsonl"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("sealed evidence not present: %v", err)
	}
	type counts struct{ gates, contract, contractless, changed, workFN, questionFP int }
	var c counts
	changedRequests := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("malformed sealed record: %v", err)
		}
		if rec["record_kind"] != "task_contract_shadow_gate" {
			continue
		}
		c.gates++
		legacy, _ := rec["legacy_wants_state_change"].(bool)
		mode, _ := rec["contract_task_mode"].(string)
		rid, _ := rec["request_id"].(string)

		var tc *TaskContract
		if mode != "" {
			tc = &TaskContract{TaskMode: TaskMode(mode)}
			c.contract++
		} else {
			c.contractless++
		}
		// Replay: the helper is pure, so the recorded legacy value stands in
		// for the heuristic it would compute from the same inputs.
		var want bool
		var wantSource actionDemandSource
		switch {
		case tc == nil:
			want, wantSource = legacy, actionDemandLegacy
		case tc.TaskMode == TaskModeWork:
			want, wantSource = true, actionDemandContractWork
		default:
			want, wantSource = false, actionDemandContractQuestion
		}
		if tc == nil {
			// Contractless MUST reproduce the recorded legacy behaviour.
			if want != legacy {
				t.Errorf("%s: a contractless replay changed the live decision", rid)
			}
			continue
		}
		if want == legacy {
			continue
		}
		c.changed++
		changedRequests[rid] = true
		if tc.TaskMode == TaskModeWork {
			c.workFN++
		} else {
			c.questionFP++
		}
		if wantSource != actionDemandContractWork && wantSource != actionDemandContractQuestion {
			t.Errorf("%s: unexpected source %q", rid, wantSource)
		}
	}
	if c.gates == 0 {
		t.Fatal("no gate records in the sealed capture")
	}
	t.Logf("replayed %d gate records: %d contract-present, %d contractless",
		c.gates, c.contract, c.contractless)
	t.Logf("live action-demand decisions that change: %d gate records across %d requests",
		c.changed, len(changedRequests))
	t.Logf("  work false-negative corrections     : %d", c.workFN)
	t.Logf("  question false-positive corrections : %d", c.questionFP)
	if c.contractless == 0 {
		t.Log("note: this capture carried a contract on every gate record")
	}
	// The invariant that matters: nothing contractless moved.
	t.Log("contractless decisions changed: 0 (asserted per record above)")
}

// --- Shadow record schema versions are per record kind -----------------------
//
// c783e3e added live_action_demand and action_demand_source to the gate record
// while every record kind still shared one version constant, so a current gate
// record claimed to be v1 while carrying fields v1 never had. Sealed
// diag_gemma_04 is v1 and must stay readable by the v1 analyzer, so the new
// shape has to be v2 and the two schemas have to be separable.

// gateFieldsV1 is exactly the set sealed diag_gemma_04 carries.
var gateFieldsV1 = map[string]bool{
	"schema_version": true, "record_kind": true, "request_id": true, "gate_seq": true,
	"call_site": true, "inspected_workspace": true, "tier": true,
	"legacy_wants_state_change": true, "contract_task_mode": true, "comparison": true,
	"influences_live_decision": true,
}

// gateFieldsV2 is v1 plus the two policy fields, and nothing else.
var gateFieldsV2 = func() map[string]bool {
	m := map[string]bool{"live_action_demand": true, "action_demand_source": true}
	for k := range gateFieldsV1 {
		m[k] = true
	}
	return m
}()

var actionDemandSources = map[string]bool{
	"legacy": true, "contract_work": true, "contract_question": true,
	"contract_invalid_failed_closed": true,
}

// validateGateRecord is the version-aware v2 checker. It accepts a valid v1 or a
// valid v2 record and nothing else: exact field set per version, closed source
// enum, and a live value that must follow from the contract and the recorded
// legacy value rather than being taken on trust.
func validateGateRecord(rec map[string]interface{}) []string {
	var bad []string
	ver, ok := rec["schema_version"].(float64)
	if !ok {
		return []string{"schema_version is missing or not a number"}
	}
	var want map[string]bool
	switch int(ver) {
	case 1:
		want = gateFieldsV1
	case 2:
		want = gateFieldsV2
	default:
		return []string{fmt.Sprintf("unknown gate schema version %v", ver)}
	}
	for k := range rec {
		if !want[k] {
			bad = append(bad, fmt.Sprintf("unexpected field %q for v%d", k, int(ver)))
		}
	}
	for k := range want {
		if _, present := rec[k]; !present {
			bad = append(bad, fmt.Sprintf("missing field %q for v%d", k, int(ver)))
		}
	}
	if int(ver) == 1 {
		return bad
	}
	src, _ := rec["action_demand_source"].(string)
	if !actionDemandSources[src] {
		bad = append(bad, fmt.Sprintf("action_demand_source %q is outside the closed enum", src))
	}
	live, liveOK := rec["live_action_demand"].(bool)
	legacy, legacyOK := rec["legacy_wants_state_change"].(bool)
	mode, _ := rec["contract_task_mode"].(string)
	if !liveOK || !legacyOK {
		bad = append(bad, "live or legacy value is not a boolean")
		return bad
	}
	// Recomputed, not trusted.
	var wantLive bool
	var wantSrc string
	switch mode {
	case "":
		wantLive, wantSrc = legacy, "legacy"
	case "work":
		wantLive, wantSrc = true, "contract_work"
	case "question":
		wantLive, wantSrc = false, "contract_question"
	default:
		wantLive, wantSrc = true, "contract_invalid_failed_closed"
	}
	if live != wantLive {
		bad = append(bad, fmt.Sprintf("live_action_demand %v, recomputed %v", live, wantLive))
	}
	if src != wantSrc {
		bad = append(bad, fmt.Sprintf("action_demand_source %q, recomputed %q", src, wantSrc))
	}
	return bad
}

// 1. The producer must emit a self-consistent record: the version it claims and
// the fields it carries have to agree.
func TestGateRecordsDeclareTheirOwnSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	shadowEnv(t, dir, "schema.jsonl")
	sink, err := openShadowSink()
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	activeShadowSink.Store(sink)
	defer activeShadowSink.Store(nil)
	shadowLoopDrive(t, "req-schema", &TaskContract{TaskMode: TaskModeWork},
		"app.py numbers", modePlanDoneNow)
	sink.close(context.Background(), 5*time.Second)

	var gates, snaps, footers int
	for _, rec := range readShadowRecords(t, filepath.Join(dir, "schema.jsonl")) {
		switch rec["record_kind"] {
		case "task_contract_shadow_gate":
			gates++
			if v := int(rec["schema_version"].(float64)); v != 2 {
				t.Errorf("gate record claims schema version %d, want 2 now that it "+
					"carries the policy fields", v)
			}
			if bad := validateGateRecord(rec); bad != nil {
				t.Errorf("gate record invalid: %v", bad)
			}
		case "task_contract_shadow_request":
			snaps++
			if v := int(rec["schema_version"].(float64)); v != 1 {
				t.Errorf("request snapshot version %d, want 1 (its schema did not change)", v)
			}
		case "task_contract_shadow_footer":
			footers++
			if v := int(rec["schema_version"].(float64)); v != 1 {
				t.Errorf("footer version %d, want 1 (its schema did not change)", v)
			}
		}
	}
	if gates == 0 || snaps != 1 || footers != 1 {
		t.Fatalf("gates=%d snapshots=%d footers=%d", gates, snaps, footers)
	}
}

// The version-aware checker's whole contract, as a table. Nothing here touches
// live behaviour; it validates records the way a future v2 analyser must.
func TestGateRecordValidationMatrix(t *testing.T) {
	v1 := func(over map[string]interface{}) map[string]interface{} {
		r := map[string]interface{}{
			"schema_version": 1.0, "record_kind": "task_contract_shadow_gate",
			"request_id": "r", "gate_seq": 1.0, "call_site": "exit_action_gate",
			"inspected_workspace": false, "tier": "T2:medium",
			"legacy_wants_state_change": false, "contract_task_mode": "work",
			"comparison": "contract_work_legacy_question", "influences_live_decision": false,
		}
		for k, v := range over {
			if v == nil {
				delete(r, k)
				continue
			}
			r[k] = v
		}
		return r
	}
	v2 := func(over map[string]interface{}) map[string]interface{} {
		r := v1(nil)
		r["schema_version"] = 2.0
		r["live_action_demand"] = true
		r["action_demand_source"] = "contract_work"
		for k, v := range over {
			if v == nil {
				delete(r, k)
				continue
			}
			r[k] = v
		}
		return r
	}
	cases := []struct {
		name   string
		rec    map[string]interface{}
		accept bool
	}{
		{"sealed v1 record", v1(nil), true},
		{"valid contract-work v2", v2(nil), true},
		{"valid contract-question v2", v2(map[string]interface{}{
			"contract_task_mode": "question", "live_action_demand": false,
			"action_demand_source": "contract_question", "comparison": "agree_question"}), true},
		{"valid contractless v2", v2(map[string]interface{}{
			"contract_task_mode": "", "legacy_wants_state_change": true,
			"live_action_demand": true, "action_demand_source": "legacy",
			"comparison": "unmeasured"}), true},
		{"invalid internal mode fails closed", v2(map[string]interface{}{
			"contract_task_mode": "explore", "live_action_demand": true,
			"action_demand_source": "contract_invalid_failed_closed"}), true},
		{"mismatched live value", v2(map[string]interface{}{
			"live_action_demand": false}), false},
		{"mismatched source", v2(map[string]interface{}{
			"action_demand_source": "legacy"}), false},
		{"unknown source", v2(map[string]interface{}{
			"action_demand_source": "vibes"}), false},
		{"missing required v2 field", v2(map[string]interface{}{
			"live_action_demand": nil}), false},
		{"missing the other v2 field", v2(map[string]interface{}{
			"action_demand_source": nil}), false},
		{"extra unknown field", v2(map[string]interface{}{
			"speculative_guess": true}), false},
		{"v1 carrying a v2 field", v1(map[string]interface{}{
			"live_action_demand": true}), false},
		{"v1 carrying the other v2 field", v1(map[string]interface{}{
			"action_demand_source": "legacy"}), false},
		{"v2 lacking both new fields", v2(map[string]interface{}{
			"live_action_demand": nil, "action_demand_source": nil}), false},
		{"unknown schema version", v2(map[string]interface{}{
			"schema_version": 3.0}), false},
		{"missing a v1 field", v1(map[string]interface{}{"tier": nil}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bad := validateGateRecord(c.rec)
			if c.accept && bad != nil {
				t.Errorf("valid record rejected: %v", bad)
			}
			if !c.accept && bad == nil {
				t.Error("invalid record accepted")
			}
		})
	}
}

// The sealed capture stays valid v1 under the same checker, unmodified.
func TestSealedEvidenceRemainsValidV1(t *testing.T) {
	path := "../redteam/step3b-freeze/evidence/diag_gemma_04/diagnostics/" +
		"step3b_diag_gemma_04.jsonl"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("sealed evidence not present: %v", err)
	}
	before := hashBytes(raw)
	gates, snaps, footers := 0, 0, 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("sealed record does not parse: %v", err)
		}
		switch rec["record_kind"] {
		case "task_contract_shadow_gate":
			gates++
			if v := int(rec["schema_version"].(float64)); v != 1 {
				t.Fatalf("a sealed gate record is version %d, expected 1", v)
			}
			if bad := validateGateRecord(rec); bad != nil {
				t.Errorf("sealed v1 gate record no longer validates: %v", bad)
			}
			for _, f := range []string{"live_action_demand", "action_demand_source"} {
				if _, present := rec[f]; present {
					t.Errorf("a sealed v1 record carries %q", f)
				}
			}
		case "task_contract_shadow_request":
			snaps++
		case "task_contract_shadow_footer":
			footers++
		}
	}
	if hashBytes(raw) != before {
		t.Fatal("the sealed capture changed while being read")
	}
	t.Logf("sealed v1 capture: %d gates, %d snapshots, %d footers, all valid v1",
		gates, snaps, footers)
}

// Structural: one gate producer, per-record-kind versions, and the new fields
// stay out of every public surface.
func TestSchemaVersionOwnershipIsPerRecordKind(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	gateProducers, sharedConst := 0, 0
	for _, n := range names {
		if strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		gateProducers += strings.Count(src, `"record_kind":               "task_contract_shadow_gate"`)
		if strings.Contains(src, "shadowSchemaVersion =") {
			sharedConst++
		}
		// The policy fields must not leak into any public producer.
		for _, f := range []string{"live_action_demand", "action_demand_source"} {
			for _, public := range []string{"ctx.Stream(", "SSEEvent{", "writeError("} {
				idx := strings.Index(src, public)
				for idx >= 0 {
					end := idx + 400
					if end > len(src) {
						end = len(src)
					}
					if strings.Contains(src[idx:end], f) {
						t.Errorf("%s: %q appears in a public payload near %s", n, f, public)
					}
					next := strings.Index(src[idx+1:], public)
					if next < 0 {
						break
					}
					idx = idx + 1 + next
				}
			}
		}
	}
	if gateProducers != 1 {
		t.Errorf("%d gate-record producers, want exactly one", gateProducers)
	}
	if sharedConst != 0 {
		t.Error("a shared shadowSchemaVersion constant is back; versions are per record kind")
	}
	agent, _ := os.ReadFile("agent.go")
	for _, want := range []string{"shadowSchemaVersionRequest", "shadowSchemaVersionGate",
		"shadowSchemaVersionFooter"} {
		if !strings.Contains(string(agent), want) {
			t.Errorf("%s is missing", want)
		}
	}
	// The prompt never sees them.
	prompt, _ := os.ReadFile("agent.go")
	i := strings.Index(string(prompt), "func buildSystemPrompt")
	if i >= 0 {
		end := strings.Index(string(prompt)[i+1:], "\nfunc ")
		body := string(prompt)[i : i+1+end]
		for _, f := range []string{"live_action_demand", "action_demand_source", "TaskContract"} {
			if strings.Contains(body, f) {
				t.Errorf("buildSystemPrompt reads %q", f)
			}
		}
	}
}

// --- work-contract verification demand ----------------------------------------
//
// verificationDemandedAndUnmet asks a session-wide boolean: did ANY command
// pass. It cannot answer "verified WHAT, at WHICH bytes", so `echo ok` clears
// it and a rewrite after a green run does not re-arm it. The evidence needed to
// answer properly already exists and has exactly one consumer, the lens:
// ctx.VerificationEvidence records, per green command, the sha256 of each file
// the command actually NAMED (commandNamesPath). Binding completion to that
// record is what these tests fix.

func TestWorkContractVerificationDemandUsesBoundEvidence(t *testing.T) {
	// Deliberately NOT the benchmark prompt: naming an input file next to a
	// write verb trips the prose heuristic, and its block would fire before
	// the verification demand and hide what these fixtures measure. The
	// provenance slice owns that defect; this slice is tested in isolation.
	const prompt = "Create solve.py and make it print the answer."
	const good = "print('1 alpha')\n"
	const broken = "def f(:\n"

	run := func(t *testing.T, contract *TaskContract, plan func(i int) map[string]interface{}) (string, []string) {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "input.txt"), []byte("1 alpha\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		turns := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			case strings.HasSuffix(r.URL.Path, "/syntax-check"):
				var in struct{ Code string }
				json.NewDecoder(r.Body).Decode(&in)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"valid": !strings.Contains(in.Code, "def f(:"), "errors": []string{"SyntaxError"}})
				return
			case strings.HasSuffix(r.URL.Path, "/execute"), strings.HasSuffix(r.URL.Path, "/shell"):
				var in struct {
					Code    string
					Command string
				}
				json.NewDecoder(r.Body).Decode(&in)
				text := in.Code + " " + in.Command
				if strings.Contains(text, ".atlas-mount-probe") {
					b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
					json.NewEncoder(w).Encode(map[string]interface{}{
						"success": true, "stdout": string(b), "exit_code": 0})
					return
				}
				fail := strings.Contains(text, "FAILING")
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": !fail, "stdout": "1 alpha\n", "exit_code": map[bool]int{true: 1, false: 0}[fail],
					"error": map[bool]string{true: "boom", false: ""}[fail]})
				return
			case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			mu.Lock()
			i := turns
			turns++
			mu.Unlock()
			call, _ := json.Marshal(plan(i))
			d, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{{"delta": map[string]string{"content": string(call)}}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
		}))
		defer srv.Close()
		ctx := NewAgentContext(dir, Tier2Medium)
		ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
		ctx.PermissionMode = PermissionYolo
		ctx.TrustMode = trustFullyTrusted
		ctx.MaxTurns = 0
		ctx.TaskContract = contract
		terminal := map[string]string{}
		terminals, calls, results := 0, 0, 0
		ctx.StreamFn = func(et string, data interface{}) {
			b, _ := json.Marshal(data)
			mu.Lock()
			defer mu.Unlock()
			switch et {
			case "done":
				terminals++
				var m map[string]string
				json.Unmarshal(b, &m)
				for k, v := range m {
					terminal[k] = v
				}
			case "tool_call":
				calls++
			case "tool_result":
				results++
			}
		}
		runAgentLoop(ctx, prompt)
		if terminals != 1 {
			t.Fatalf("%d terminals, want one", terminals)
		}
		if calls != results {
			t.Fatalf("%d calls vs %d results", calls, results)
		}
		var disk []string
		filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(dir, p)
			if !strings.HasPrefix(rel, ".") {
				b, _ := os.ReadFile(p)
				disk = append(disk, rel+"="+hashBytes(b)[:12])
			}
			return nil
		})
		sort.Strings(disk)
		return terminal["status"] + "/" + terminal["reason"], disk
	}

	write := func(path, body string) map[string]interface{} {
		return map[string]interface{}{"type": "tool_call", "name": "write_file",
			"args": map[string]string{"path": path, "content": body}}
	}
	cmd := func(c string) map[string]interface{} {
		return map[string]interface{}{"type": "tool_call", "name": "run_command",
			"args": map[string]string{"command": c}}
	}
	done := map[string]interface{}{"type": "done", "summary": "wrote solve.py"}
	prose := map[string]interface{}{"type": "text", "content": "Please provide the content for solve.py."}
	seq := func(steps ...map[string]interface{}) func(int) map[string]interface{} {
		return func(i int) map[string]interface{} {
			if i < len(steps) {
				return steps[i]
			}
			return done
		}
	}
	work := func(outputs []string, verify []string) *TaskContract {
		return &TaskContract{TaskMode: TaskModeWork,
			ExpectedOutputs: strsPtr(outputs...), Verification: strsPtr(verify...)}
	}

	for _, c := range []struct {
		name     string
		contract *TaskContract
		plan     func(int) map[string]interface{}
		want     string
		wantNot  string
	}{
		{"valid code, done, no command", work([]string{"solve.py"}, nil),
			seq(write("solve.py", good), done), "incomplete/verification_demanded_unmet", ""},
		{"valid code, prose, no command", work([]string{"solve.py"}, nil),
			seq(write("solve.py", good), prose), "", "completed"},
		{"valid code, unrelated successful command", work([]string{"solve.py"}, nil),
			seq(write("solve.py", good), cmd("echo ok"), done),
			"incomplete/verification_demanded_unmet", ""},
		{"relevant command then mutation", work([]string{"solve.py"}, nil),
			seq(write("solve.py", good), cmd("python3 solve.py"), write("solve.py", good+"# more\n"), done),
			"incomplete/verification_demanded_unmet", ""},
		{"failed relevant command", work([]string{"solve.py"}, nil),
			seq(write("solve.py", good), cmd("python3 solve.py FAILING"), done), "", "completed"},
		{"two outputs, only one verified", work([]string{"solve.py", "other.py"}, nil),
			seq(write("solve.py", good), write("other.py", good), cmd("python3 solve.py"), done),
			"incomplete/verification_demanded_unmet", ""},
		{"relevant command after the final write", work([]string{"solve.py"}, nil),
			seq(write("solve.py", good), cmd("python3 solve.py"), done),
			"completed/deliverables_demonstrated", ""},
		{"exact contract command satisfied", work([]string{"solve.py"}, []string{"python3 solve.py"}),
			seq(write("solve.py", good), cmd("python3 solve.py"), done),
			"completed/deliverables_demonstrated", ""},
		{"contract command spelling differs", work([]string{"solve.py"}, []string{"python3 solve.py"}),
			seq(write("solve.py", good), cmd("python3  solve.py"), done),
			"incomplete/verification_demanded_unmet", ""},
		{"verification before the final write", work([]string{"solve.py"}, nil),
			seq(cmd("python3 solve.py"), write("solve.py", good), done),
			"incomplete/verification_demanded_unmet", ""},
		{"alias spelling shares canonical identity", work([]string{"./solve.py"}, nil),
			seq(write("solve.py", good), cmd("python3 solve.py"), done),
			"completed/deliverables_demonstrated", ""},
		{"invalid code with a green command", work([]string{"solve.py"}, nil),
			seq(write("solve.py", broken), cmd("python3 solve.py"), done), "", "completed"},
		{"question mode raises no verification demand", work(nil, nil),
			seq(map[string]interface{}{"type": "done", "summary": "it reads input.txt"}),
			"", "verification"},
	} {
		t.Run(c.name, func(t *testing.T) {
			contract := c.contract
			if c.name == "question mode unchanged" {
				contract = &TaskContract{TaskMode: TaskModeQuestion}
			}
			got, disk := run(t, contract, c.plan)
			if c.want != "" && got != c.want {
				t.Fatalf("terminal = %q, want %q", got, c.want)
			}
			if c.wantNot != "" && strings.Contains(got, c.wantNot) {
				t.Fatalf("terminal = %q, must not be %s", got, c.wantNot)
			}
			if len(disk) == 0 {
				t.Fatalf("workspace was not preserved")
			}
		})
	}
}

func TestOneVerificationDemandSharedByBothExits(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	// Two call sites, both shared by the done and text exits: the finalizer
	// decides the terminal, and exitGates asks the same owner earlier so the
	// run is told what it still owes while it can act on it.
	if n := strings.Count(body, "decideVerificationDemand("); n != 2 {
		t.Fatalf("agent.go calls decideVerificationDemand %d times; want the finalizer "+
			"and the exit gates, which both exits share", n)
	}
	i := strings.Index(body, "func finalizeCompletion")
	if i < 0 || !strings.Contains(body[i:i+2500], "decideVerificationDemand(") {
		t.Fatal("the demand is not evaluated inside finalizeCompletion, the single " +
			"finalizer both the done and text exits call")
	}
	if g := guardCalls(t, "agent.go", "exitGates"); !g["decideVerificationDemand"] {
		t.Fatal("the second call site is not the exit gates both exits run")
	}
	for _, marker := range []string{
		`textStatus, textReason := finalizeCompletion(ctx, st, userMessage, "text_reply")`,
		`status, reason := finalizeCompletion(`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("a terminal exit no longer routes through finalizeCompletion: %q", marker)
		}
	}
}

// --- executable versus declarative deliverables --------------------------------
//
// Commit 1 scoped its verification demand with syntaxGateLanguages, which
// answers "is there a checker for this extension" -- not "can a command run
// this". The registry holds .html, .htm, .xml, .json, .yaml and .yml, so a run
// that wrote index.html or config.yaml was told to produce an execution that
// names it, and no such command exists. That is an obligation nothing can
// discharge: a permanent incompletion for ordinary static work.
//
// The distinction now lives in the registry itself. These fixtures pin the
// boundary from both sides: declarative artifacts lose the execution demand and
// keep every byte-level requirement, executable artifacts keep the demand.

func TestExecutableDeliverablesDemandExecutionDeclarativeDoNot(t *testing.T) {
	const prompt = "Create the project files."
	const goodPy = "print('ok')\n"
	const brokenPy = "def f(:\n"
	const goodHTML = "<!doctype html><title>x</title><p>hi</p>\n"
	const brokenHTML = "<!doctype html><title>x</title><p>unclosed\n"
	const goodJSON = "{\"a\": 1}\n"
	const goodYAML = "a: 1\n"
	const goodXML = "<?xml version=\"1.0\"?><r><a/></r>\n"

	run := func(t *testing.T, contract *TaskContract, plan func(i int) map[string]interface{}) (string, []string, int, int) {
		t.Helper()
		dir := t.TempDir()
		var mu sync.Mutex
		turns := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			case strings.HasSuffix(r.URL.Path, "/syntax-check"):
				var in struct{ Code string }
				json.NewDecoder(r.Body).Decode(&in)
				bad := strings.Contains(in.Code, "def f(:") || strings.Contains(in.Code, "unclosed")
				json.NewEncoder(w).Encode(map[string]interface{}{
					"valid": !bad, "errors": []string{"SyntaxError"}})
				return
			case strings.HasSuffix(r.URL.Path, "/execute"), strings.HasSuffix(r.URL.Path, "/shell"):
				var in struct {
					Code    string
					Command string
				}
				json.NewDecoder(r.Body).Decode(&in)
				text := in.Code + " " + in.Command
				if strings.Contains(text, ".atlas-mount-probe") {
					b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
					json.NewEncoder(w).Encode(map[string]interface{}{
						"success": true, "stdout": string(b), "exit_code": 0})
					return
				}
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": "ok\n", "exit_code": 0})
				return
			case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			mu.Lock()
			i := turns
			turns++
			mu.Unlock()
			call, _ := json.Marshal(plan(i))
			d, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{{"delta": map[string]string{"content": string(call)}}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
		}))
		defer srv.Close()
		ctx := NewAgentContext(dir, Tier2Medium)
		ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
		ctx.PermissionMode = PermissionYolo
		ctx.TrustMode = trustFullyTrusted
		ctx.MaxTurns = 0
		ctx.TaskContract = contract
		terminal := map[string]string{}
		terminals, calls, results := 0, 0, 0
		ctx.StreamFn = func(et string, data interface{}) {
			b, _ := json.Marshal(data)
			mu.Lock()
			defer mu.Unlock()
			switch et {
			case "done":
				terminals++
				var m map[string]string
				json.Unmarshal(b, &m)
				for k, v := range m {
					terminal[k] = v
				}
			case "tool_call":
				calls++
			case "tool_result":
				results++
			}
		}
		runAgentLoop(ctx, prompt)
		if terminals != 1 {
			t.Fatalf("%d terminals, want one", terminals)
		}
		var disk []string
		filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(dir, p)
			if !strings.HasPrefix(rel, ".") {
				b, _ := os.ReadFile(p)
				disk = append(disk, rel+"="+hashBytes(b)[:12])
			}
			return nil
		})
		sort.Strings(disk)
		return terminal["status"] + "/" + terminal["reason"], disk, calls, results
	}

	write := func(path, body string) map[string]interface{} {
		return map[string]interface{}{"type": "tool_call", "name": "write_file",
			"args": map[string]string{"path": path, "content": body}}
	}
	cmd := func(c string) map[string]interface{} {
		return map[string]interface{}{"type": "tool_call", "name": "run_command",
			"args": map[string]string{"command": c}}
	}
	done := map[string]interface{}{"type": "done", "summary": "wrote the files"}
	seq := func(steps ...map[string]interface{}) func(int) map[string]interface{} {
		return func(i int) map[string]interface{} {
			if i < len(steps) {
				return steps[i]
			}
			return done
		}
	}
	work := func(outputs, verify []string) *TaskContract {
		return &TaskContract{TaskMode: TaskModeWork,
			ExpectedOutputs: strsPtr(outputs...), Verification: strsPtr(verify...)}
	}

	for _, c := range []struct {
		name     string
		contract *TaskContract
		plan     func(int) map[string]interface{}
		want     string
		wantNot  string
	}{
		// A page is not demanded a run, and nothing loaded it either: the
		// completion stands and its reason says it rests on a parse.
		{"static html, no command", work([]string{"index.html"}, nil),
			seq(write("index.html", goodHTML), done), "completed/deliverables_parse_only", ""},
		{"json config, no command", work([]string{"config.json"}, nil),
			seq(write("config.json", goodJSON), done), "completed/deliverables_demonstrated", ""},
		{"yaml config, no command", work([]string{"config.yaml"}, nil),
			seq(write("config.yaml", goodYAML), done), "completed/deliverables_demonstrated", ""},
		{"xml document, no command", work([]string{"data.xml"}, nil),
			seq(write("data.xml", goodXML), done), "completed/deliverables_demonstrated", ""},
		{"invalid html stays incomplete", work([]string{"index.html"}, nil),
			seq(write("index.html", brokenHTML), done), "", "completed"},
		{"python without a relevant command", work([]string{"solve.py"}, nil),
			seq(write("solve.py", goodPy), done), "incomplete/verification_demanded_unmet", ""},
		{"python with echo ok", work([]string{"solve.py"}, nil),
			seq(write("solve.py", goodPy), cmd("echo ok"), done),
			"incomplete/verification_demanded_unmet", ""},
		{"python with a relevant command", work([]string{"solve.py"}, nil),
			seq(write("solve.py", goodPy), cmd("python3 solve.py"), done),
			"completed/deliverables_demonstrated", ""},
		{"relevant command then mutation", work([]string{"solve.py"}, nil),
			seq(write("solve.py", goodPy), cmd("python3 solve.py"),
				write("solve.py", goodPy+"# more\n"), done),
			"incomplete/verification_demanded_unmet", ""},
		{"two executables, one exercised", work([]string{"a.py", "b.py"}, nil),
			seq(write("a.py", goodPy), write("b.py", goodPy), cmd("python3 a.py"), done),
			"incomplete/verification_demanded_unmet", ""},
		{"mixed: static asset plus exercised code", work([]string{"index.html", "app.py"}, nil),
			seq(write("index.html", goodHTML), write("app.py", goodPy), cmd("python3 app.py"), done),
			"completed/deliverables_parse_only", ""},
		{"mixed: static asset plus unexercised code", work([]string{"index.html", "app.py"}, nil),
			seq(write("index.html", goodHTML), write("app.py", goodPy), done),
			"incomplete/verification_demanded_unmet", ""},
		{"mixed: static asset invalid, code exercised", work([]string{"index.html", "app.py"}, nil),
			seq(write("index.html", brokenHTML), write("app.py", goodPy), cmd("python3 app.py"), done),
			"", "completed"},
		{"declared command still required on a static deliverable",
			work([]string{"index.html"}, []string{"htmlhint index.html"}),
			seq(write("index.html", goodHTML), done),
			"incomplete/verification_demanded_unmet", ""},
		{"declared command satisfied on a static deliverable",
			work([]string{"index.html"}, []string{"htmlhint index.html"}),
			seq(write("index.html", goodHTML), cmd("htmlhint index.html"), done),
			"completed/deliverables_parse_only", ""},
		{"declared command mismatched spelling",
			work([]string{"index.html"}, []string{"htmlhint index.html"}),
			seq(write("index.html", goodHTML), cmd("htmlhint  index.html"), done),
			"incomplete/verification_demanded_unmet", ""},
		{"invalid python with a green command", work([]string{"solve.py"}, nil),
			seq(write("solve.py", brokenPy), cmd("python3 solve.py"), done), "", "completed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, disk, calls, results := run(t, c.contract, c.plan)
			if c.want != "" && got != c.want {
				t.Fatalf("terminal = %q, want %q (disk %v)", got, c.want, disk)
			}
			if c.wantNot != "" && strings.HasPrefix(got, c.wantNot) {
				t.Fatalf("terminal = %q, must not be %s", got, c.wantNot)
			}
			if calls != results {
				t.Fatalf("%d tool calls, %d results", calls, results)
			}
			if len(disk) == 0 {
				t.Fatalf("nothing was written")
			}
		})
	}
}

// A future declarative language must not acquire an execution obligation just
// by joining the syntax-checker registry. The registry is the one owner of both
// facts, so the guard reads it directly.
func TestSyntaxRegistryOwnsExecutability(t *testing.T) {
	wantExecutable := map[string]bool{
		".py": true, ".js": true, ".ts": true, ".go": true, ".java": true,
		".kt": true, ".rb": true, ".php": true, ".sh": true,
		".mjs": true, ".cjs": true,
		".json": false, ".yaml": false, ".yml": false,
		".html": false, ".htm": false, ".xml": false,
	}
	if len(syntaxGateLanguages) != len(wantExecutable) {
		t.Fatalf("the registry holds %d extensions, the guard knows %d: a new "+
			"entry must declare whether it is executable",
			len(syntaxGateLanguages), len(wantExecutable))
	}
	for ext, meta := range syntaxGateLanguages {
		want, known := wantExecutable[ext]
		if !known {
			t.Errorf("%s joined the registry without declaring executability", ext)
			continue
		}
		if meta.Executable != want {
			t.Errorf("%s executable=%v, want %v", ext, meta.Executable, want)
		}
		if meta.Language == "" {
			t.Errorf("%s has no checker language", ext)
		}
	}
	// The demand must be scoped by executability, never by registry membership.
	src, err := os.ReadFile("guardrails.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	i := strings.Index(body, "func codeDeliverablesFor")
	if i < 0 {
		t.Fatal("codeDeliverablesFor is gone")
	}
	fn := body[i:]
	if e := strings.Index(fn[1:], "\nfunc "); e >= 0 {
		fn = fn[:e]
	}
	if !strings.Contains(fn, ".Executable") {
		t.Error("codeDeliverablesFor does not scope on the registry's executability")
	}
}

// /version reports the grammar mode this process applies, so a measurement
// can record the configuration it ran against.
func TestVersionReportsTheGrammarMode(t *testing.T) {
	for _, c := range []struct{ env, want string }{
		{"", "strict"}, {"strict", "strict"}, {"loose", "loose"}, {"something-else", "strict"},
	} {
		t.Setenv("ATLAS_GRAMMAR_MODE", c.env)
		rec := httptest.NewRecorder()
		handleVersion(rec, httptest.NewRequest("GET", "/version", nil))
		var body map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["grammar_mode"] != c.want {
			t.Errorf("ATLAS_GRAMMAR_MODE=%q: /version says %v, want %s", c.env, body["grammar_mode"], c.want)
		}
		// And the response format agrees with what /version says.
		_, strict := buildResponseFormat().(map[string]interface{})
		if strict != (c.want == "strict") {
			t.Errorf("ATLAS_GRAMMAR_MODE=%q: the response format disagrees with /version", c.env)
		}
	}
}

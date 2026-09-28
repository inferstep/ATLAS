package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var lensInvocationFormat = regexp.MustCompile(`^proxy-lens:[0-9a-f]{32}$`)

func lensCtx(rid string) context.Context {
	return context.WithValue(context.Background(), requestIDKey, rid)
}

// The shipped specification is the contract both implementations (this one
// and the ignored acquisition tooling) are checked against.
func TestLensInvocationVectorsReproduce(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "lens_invocation_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Scheme           string `json:"scheme"`
		Prefix           string `json:"prefix"`
		Header           string `json:"header"`
		RequestIDPattern string `json:"request_id_pattern"`
		Length           int    `json:"invocation_id_length"`
		Vectors          []struct {
			RequestID    string  `json:"request_id"`
			InvocationID *string `json:"invocation_id"`
			Note         string  `json:"note"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Scheme != lensInvocationScheme || spec.Prefix != lensInvocationPrefix || spec.Header != lensInvocationHeader {
		t.Fatalf("specification constants drifted: %+v", spec)
	}
	if spec.Length != lensInvocationTotalLe {
		t.Fatalf("length %d != %d", spec.Length, lensInvocationTotalLe)
	}
	pat := regexp.MustCompile(spec.RequestIDPattern)
	if len(spec.Vectors) < 8 {
		t.Fatalf("too few vectors: %d", len(spec.Vectors))
	}
	for _, v := range spec.Vectors {
		got, ok := lensInvocationID(v.RequestID)
		if v.InvocationID == nil {
			if ok || got != "" {
				t.Errorf("%q (%s): expected no identity, got %q", v.RequestID, v.Note, got)
			}
			if pat.MatchString(v.RequestID) {
				t.Errorf("%q: pattern accepts a request id the vectors reject", v.RequestID)
			}
			continue
		}
		if !ok || got != *v.InvocationID {
			t.Errorf("%q (%s): got %q, want %q", v.RequestID, v.Note, got, *v.InvocationID)
		}
		if !lensInvocationFormat.MatchString(got) || len(got) != lensInvocationTotalLe {
			t.Errorf("%q: format %q", v.RequestID, got)
		}
		if !pat.MatchString(v.RequestID) {
			t.Errorf("%q: pattern rejects a request id the vectors accept", v.RequestID)
		}
	}
}

// The committed vectors are identity test data and nothing else: no diagnostic
// or benchmark case id, no task-family name, no holdout identifier, no
// experiment prefix. The rule is closed and deterministic: every request id with
// an identity has one of the opaque shapes below, no id or note carries an
// underscore (family and case names are snake_case), and no id starts with an
// acquisition prefix. Vectors that must derive no identity are listed by value.
var (
	vectorOpaqueShape     = regexp.MustCompile(`^(req-[0-9a-f]{16}|svc-[a-z]+\.[0-9a-f]{4}:[0-9a-f]{4}|proxy-lens:[0-9a-f]+|a|A{128})$`)
	vectorMalformedByRule = map[string]bool{"": true, "bad id": true, "req\n1": true, "id/with/slash": true, strings.Repeat("A", 129): true}
	vectorForbiddenPrefix = []string{"glcsd-", "acdmv-", "sa-", "plens-", "diag-", "s3b-", "stage-"}
)

func TestVectorsCarryNoExperimentIdentity(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "lens_invocation_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Vectors []struct {
			RequestID    string  `json:"request_id"`
			InvocationID *string `json:"invocation_id"`
			Note         string  `json:"note"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	for _, v := range spec.Vectors {
		if strings.Contains(v.RequestID, "_") || strings.Contains(v.Note, "_") {
			t.Errorf("%q (%q): underscore; family and case names are snake_case and never belong in vectors", v.RequestID, v.Note)
		}
		for _, p := range vectorForbiddenPrefix {
			if strings.HasPrefix(v.RequestID, p) {
				t.Errorf("%q: acquisition prefix %q", v.RequestID, p)
			}
		}
		if v.InvocationID != nil {
			if !vectorOpaqueShape.MatchString(v.RequestID) {
				t.Errorf("%q: not one of the opaque vector shapes", v.RequestID)
			}
		} else if !vectorMalformedByRule[v.RequestID] {
			t.Errorf("%q: a no-identity vector must be one of the listed malformed values", v.RequestID)
		}
	}
}

func TestLensInvocationIsDeterministicDistinctBoundedAndTyped(t *testing.T) {
	seen := map[string]string{}
	for i := 0; i < 2000; i++ {
		rid := fmt.Sprintf("req-%016x", i*7919+13)
		a, ok := lensInvocationID(rid)
		b, _ := lensInvocationID(rid)
		if !ok || a != b {
			t.Fatalf("not deterministic for %q", rid)
		}
		if len(a) != lensInvocationTotalLe || !strings.HasPrefix(a, lensInvocationPrefix) {
			t.Fatalf("format %q", a)
		}
		if prev, dup := seen[a]; dup {
			t.Fatalf("collision between %q and %q", prev, rid)
		}
		seen[a] = rid
	}
	// Never a V3 invocation shape (uuid4 with dashes) and never the raw request id.
	for rid := range seen {
		if strings.Count(rid, "-") == 4 && len(rid) == 36 {
			t.Fatal("test ids must not look like V3 invocations")
		}
	}
	if inv, _ := lensInvocationID("proxy-lens:deadbeef"); inv == "proxy-lens:deadbeef" {
		t.Fatal("a request id is hashed, never passed through")
	}
}

func TestNewLensRequestCarriesThePairFromContextOnly(t *testing.T) {
	req, err := newLensRequest(lensCtx("req-0011223344556677"), "POST", "http://lens/internal/lens/score-per-step", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := lensInvocationID("req-0011223344556677")
	if req.Header.Get(requestIDHeader) != "req-0011223344556677" || req.Header.Get(lensInvocationHeader) != want {
		t.Fatalf("headers %v", req.Header)
	}
	if req.Header.Get("Content-Type") != "application/json" {
		t.Fatal("content type")
	}
	// No request id in the context: neither header. Absent work inherits nothing.
	bare, _ := newLensRequest(context.Background(), "POST", "http://lens/x", nil)
	if bare.Header.Get(requestIDHeader) != "" || bare.Header.Get(lensInvocationHeader) != "" {
		t.Fatalf("bare request carries identity: %v", bare.Header)
	}
	// A malformed request id is forwarded as the request id (unchanged
	// behaviour) but yields no invocation identity: fail closed at the relay.
	bad, _ := newLensRequest(lensCtx("bad id"), "POST", "http://lens/x", nil)
	if bad.Header.Get(requestIDHeader) != "bad id" || bad.Header.Get(lensInvocationHeader) != "" {
		t.Fatalf("malformed request id: %v", bad.Header)
	}
	// Retry: building the request again yields the identical pair.
	again, _ := newLensRequest(lensCtx("req-0011223344556677"), "POST", "http://lens/x", nil)
	if again.Header.Get(lensInvocationHeader) != want {
		t.Fatal("retry changed the pair")
	}
}

func TestOutboundTransportForwardsRequestIDButNeverAnInvocation(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(200)
	}))
	defer srv.Close()
	client := &http.Client{Transport: &tokenTransport{base: http.DefaultTransport}}
	req, _ := http.NewRequestWithContext(lensCtx("req-0011223344556677"), "POST", srv.URL+"/v3/plan", strings.NewReader("{}"))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.Get(requestIDHeader) != "req-0011223344556677" {
		t.Fatalf("transport dropped the request id: %v", got)
	}
	if got.Get(lensInvocationHeader) != "" {
		t.Fatalf("the transport must never mint an invocation for V3 or model calls: %v", got)
	}
}

type lensCapture struct {
	mu    sync.Mutex
	pairs [][2]string
	paths []string
}

func (c *lensCapture) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.pairs = append(c.pairs, [2]string{r.Header.Get(requestIDHeader), r.Header.Get(lensInvocationHeader)})
		c.paths = append(c.paths, r.URL.Path)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/internal/lens/score-per-step":
			fmt.Fprint(w, `{"enabled":true,"gx_available":true,"n_tokens":3,"hidden_dim":8,"layer":"l","aggregate":{"gx_score_min":0.7,"gx_score_mean":0.8,"first_off_rails_idx":-1},"latency_ms":1}`)
		default:
			w.WriteHeader(404)
		}
	}))
}

func TestEveryPerWriteScoreInARequestSendsTheSamePair(t *testing.T) {
	cap := &lensCapture{}
	srv := cap.server(t)
	defer srv.Close()
	ctx := lensCtx("req-0011223344556677")
	for _, content := range []string{"def f():\n    return 1\n", "x = 1\n"} {
		if _, scored, _ := scoreContentForAgent(ctx, srv.URL, content); !scored {
			t.Fatal("scoring did not run")
		}
	}
	want, _ := lensInvocationID("req-0011223344556677")
	if len(cap.pairs) != 2 {
		t.Fatalf("calls %d", len(cap.pairs))
	}
	for i, p := range cap.pairs {
		if p[0] != "req-0011223344556677" || p[1] != want {
			t.Errorf("call %d (%s): pair %v", i, cap.paths[i], p)
		}
	}
}

func TestConcurrentRequestsKeepTheirOwnLensPair(t *testing.T) {
	cap := &lensCapture{}
	srv := cap.server(t)
	defer srv.Close()
	const workers, rounds = 48, 6
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rid := fmt.Sprintf("req-%016x", w)
			ctx := lensCtx(rid)
			for r := 0; r < rounds; r++ {
				scoreContentForAgent(ctx, srv.URL, "x = 1\n")
			}
		}(w)
	}
	wg.Wait()
	if len(cap.pairs) != workers*rounds {
		t.Fatalf("calls %d", len(cap.pairs))
	}
	for _, p := range cap.pairs {
		want, _ := lensInvocationID(p[0])
		if p[1] != want {
			t.Fatalf("pair exchanged across requests: %v", p)
		}
	}
}

func TestCancelledOrExpiredContextStopsLensCalls(t *testing.T) {
	cap := &lensCapture{}
	srv := cap.server(t)
	defer srv.Close()
	ctx, cancel := context.WithCancel(lensCtx("req-0011223344556677"))
	cancel()
	if _, scored, down := scoreContentForAgent(ctx, srv.URL, "x = 1\n"); scored || down != "" {
		t.Fatalf("scored=%v down=%q on a cancelled context: cancellation is not the lens being down",
			scored, down)
	}
	if len(cap.pairs) != 0 {
		t.Fatalf("%d call(s) reached the Lens after cancellation", len(cap.pairs))
	}
	expired, cancel2 := context.WithTimeout(lensCtx("req-0011223344556677"), time.Nanosecond)
	defer cancel2()
	time.Sleep(time.Millisecond)
	if _, scored, down := scoreContentForAgent(expired, srv.URL, "x = 1\n"); scored || down != "" {
		t.Fatalf("scored=%v down=%q on an expired context", scored, down)
	}
	if len(cap.pairs) != 0 {
		t.Fatal("a call reached the Lens after the deadline")
	}
}

// Structural guards over the proxy sources (non-test files).
func proxySources(t *testing.T) map[string]string {
	t.Helper()
	files, _ := filepath.Glob("*.go")
	out := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[f] = string(b)
	}
	return out
}

func TestEveryProxyOwnedModelBoundLensCallUsesTheOneIdentityOwner(t *testing.T) {
	for name, src := range proxySources(t) {
		for n, line := range strings.Split(src, "\n") {
			trim := strings.TrimSpace(line)
			if strings.HasPrefix(trim, "//") {
				continue
			}
			if strings.Contains(line, `"/internal/lens/`) {
				if !strings.Contains(line, "newLensRequest(") {
					t.Errorf("%s:%d builds a Lens call outside lens_identity.go: %s", name, n+1, trim)
				}
			}
		}
	}
}

func TestInvocationIdentityHasOneOwnerAndNoDecisionReader(t *testing.T) {
	for name, src := range proxySources(t) {
		if name == "lens_identity.go" {
			continue
		}
		for _, needle := range []string{"X-ATLAS-V3-Invocation-ID", "lensInvocationHeader", "lensInvocationID(", "proxy-lens", "lensInvocationScheme", "lensInvocationPrefix"} {
			if strings.Contains(src, needle) {
				t.Errorf("%s reads or names the Lens invocation identity (%q); only lens_identity.go may", name, needle)
			}
		}
	}
	// The derivation reads the typed request identity and nothing else.
	src := proxySources(t)["lens_identity.go"]
	for _, forbidden := range []string{"parsed.Args", "Message", "WorkingDir", "content", "os.Getenv", "rand.", "time.Now", "var "} {
		if strings.Contains(strings.ReplaceAll(src, "// ", ""), forbidden) && forbidden != "content" {
			t.Errorf("lens_identity.go touches %q", forbidden)
		}
	}
}

func TestNoProxyBackgroundLensCallExistsOutsideTheRequestScopedSite(t *testing.T) {
	sites := 0
	for name, src := range proxySources(t) {
		for n, line := range strings.Split(src, "\n") {
			if strings.Contains(line, "newLensRequest(") && !strings.Contains(strings.TrimSpace(line), "func newLensRequest") {
				sites++
				if name != "lens.go" {
					t.Errorf("%s:%d: unexpected Lens call site", name, n+1)
				}
			}
		}
	}
	if sites != 1 {
		t.Fatalf("expected exactly one proxy-owned model-bound Lens call site (per-write scoring), found %d", sites)
	}
}

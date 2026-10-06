// atlas-proxy: ATLAS's local inference proxy.
//
// Hosts the structured agent endpoint (`/v1/agent`), the typed event
// broker (`/events`), and the cancel hook (`/cancel`) that the TUI
// drives. Plain OpenAI traffic on `/v1/chat/completions` and unmatched
// paths are passed through to llama-server via the catch-all handler
// in main(). The verify-repair pipeline (lens scoring + sandbox +
// V3 stages) lives behind the agent loop's `write_file` tool.
//
// Usage:
//
//	atlas-proxy                  (default port 8090)
//	ATLAS_LLAMA_URL=http://localhost:8080 atlas-proxy
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

var (
	inferenceURL = envOr("ATLAS_INFERENCE_URL", "http://localhost:8080")
	lensURL      = envOr("ATLAS_LENS_URL", "http://localhost:8099")
	sandboxURL   = envOr("ATLAS_SANDBOX_URL", "http://localhost:30820")
	v3URL        = envOr("ATLAS_V3_URL", "http://localhost:8070")
	proxyPort    = envOr("ATLAS_PROXY_PORT", "8090")
	modelName    = envOr("ATLAS_MODEL_NAME", "local-model")
	healthClient = &http.Client{Timeout: 3 * time.Second}
	// v3-service can take longer to answer when a pipeline run is in
	// flight; keep its readiness probe on a shorter leash so /ready
	// stays snappy.
	v3HealthClient = &http.Client{Timeout: 2 * time.Second}
)

const (
	demoRawCapability   = "demo_raw_completion_v1"
	maxRequestBodyBytes = 16 << 20
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// resolveVerifyTarget returns "host" when run_command should bypass
// the sandbox and execute on the host, or "sandbox" otherwise.
//
// Resolution order (later wins):
//  1. ATLAS_VERIFY_IN env var ("host" or "sandbox")
//  2. Per-project .atlas/config.toml — looks for `target = "host"` or
//     `target = "sandbox"` under an [execution] header. Trivially
//     parsed (no real TOML lib) so we don't take a dep just for one
//     setting; refuse to be clever about quoting.
//
// Default: "sandbox" (the safer path). Per-project config is the
// usual customization point for working codebases that need host
// execution; the env var is for one-off sessions and CI.
func resolveVerifyTarget(workingDir string) string {
	target := strings.ToLower(os.Getenv("ATLAS_VERIFY_IN"))
	if target != "host" && target != "sandbox" {
		target = "sandbox"
	}
	if workingDir == "" {
		return target
	}
	cfg, err := readConfined(workingDir, filepath.Join(".atlas", "config.toml"))
	if err != nil {
		return target
	}
	inExecution := false
	for _, raw := range strings.Split(string(cfg), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inExecution = strings.EqualFold(strings.Trim(line, "[]"), "execution")
			continue
		}
		if !inExecution {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) != "target" {
			continue
		}
		val := strings.ToLower(strings.Trim(strings.TrimSpace(parts[1]), `"'`))
		if val == "host" || val == "sandbox" {
			return val
		}
	}
	return target
}

// ---------------------------------------------------------------------------
// HTTP server setup
// ---------------------------------------------------------------------------

func handleModels(w http.ResponseWriter, r *http.Request) {
	// Prefer llama-server's loaded model over our configured fallback. This
	// keeps the API (and /demo title) truthful when a local launch overrides
	// ATLAS_MODEL_NAME or the local .env lags behind the running server.
	id := modelName
	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
		strings.TrimRight(inferenceURL, "/")+"/v1/models", nil)
	if err == nil {
		if upstream, upstreamErr := healthClient.Do(upstreamReq); upstreamErr == nil {
			defer upstream.Body.Close()
			if upstream.StatusCode == http.StatusOK {
				var loaded struct {
					Data []struct {
						ID string `json:"id"`
					} `json:"data"`
				}
				if decodeErr := json.NewDecoder(io.LimitReader(upstream.Body, 1<<20)).Decode(&loaded); decodeErr == nil {
					for _, candidate := range loaded.Data {
						if candidate.ID = strings.TrimSpace(candidate.ID); candidate.ID != "" {
							id = candidate.ID
							break
						}
					}
				}
			}
		}
	}
	resp := map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"id": id, "object": "model", "owned_by": "atlas"},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	llmOK, lensOK, sandboxOK := false, false, false

	if resp, err := healthClient.Get(inferenceURL + "/health"); err == nil {
		resp.Body.Close()
		llmOK = resp.StatusCode == 200
	}
	if resp, err := healthClient.Get(lensURL + "/health"); err == nil {
		resp.Body.Close()
		lensOK = resp.StatusCode == 200
	}
	// lens_ready is the gate /v1/agent applies: whether the lens can score
	// (lens_required.go). /health stays informational.
	lensCanScore, lensWhy := lensReady(lensURL)
	if resp, err := healthClient.Get(sandboxURL + "/health"); err == nil {
		resp.Body.Close()
		sandboxOK = resp.StatusCode == 200
	}

	overall := llmOK && lensOK && sandboxOK && lensCanScore
	overallStatus := "ok"
	if !overall {
		overallStatus = "degraded"
	}

	status := map[string]any{
		"status":       overallStatus,
		"inference":    llmOK,
		"lens":         lensOK,
		"lens_ready":   lensCanScore,
		"sandbox":      sandboxOK,
		"port":         proxyPort,
		"capabilities": []string{demoRawCapability},
	}
	if !lensCanScore {
		status["lens_reason"] = lensWhy
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func handleReady(w http.ResponseWriter, r *http.Request) {
	llmOK, sandboxOK := false, false

	if resp, err := healthClient.Get(inferenceURL + "/health"); err == nil {
		resp.Body.Close()
		llmOK = resp.StatusCode == 200
	}
	// The same gate /v1/agent applies, so a client that asks /ready first
	// is not told "ready" for a request the proxy would refuse.
	lensCanScore, lensWhy := lensReady(lensURL)
	if resp, err := healthClient.Get(sandboxURL + "/health"); err == nil {
		resp.Body.Close()
		sandboxOK = resp.StatusCode == 200
	}
	// T2/T3 writes route through v3-service, so readiness includes it
	// whenever a V3 URL is configured.
	v3OK := true
	if v3URL != "" {
		v3OK = false
		if resp, err := v3HealthClient.Get(v3URL + "/health"); err == nil {
			resp.Body.Close()
			v3OK = resp.StatusCode == 200
		}
	}

	ready := llmOK && lensCanScore && sandboxOK && v3OK
	body := map[string]any{
		"ready":      ready,
		"inference":  llmOK,
		"lens_ready": lensCanScore,
		"sandbox":    sandboxOK,
		"v3":         v3OK,
	}
	if !lensCanScore {
		body["lens_reason"] = lensWhy
	}
	w.Header().Set("Content-Type", "application/json")
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(body)
}

func newProxyMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/models", handleModels)
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/ready", handleReady)
	mux.HandleFunc("/v1/agent", handleAgent)           // tool-based agent endpoint
	mux.HandleFunc("/events", handleEvents)            // typed SSE event stream
	mux.HandleFunc("/cancel", handleCancel)            // TUI abort hook
	mux.HandleFunc("/v1/permission", handlePermission) // interactive approve/deny for destructive tools
	// TUI calls this on connect to render a Lens/ASA compat badge.
	mux.HandleFunc("/v1/calibration/status", handleCalibrationStatus)
	mux.HandleFunc("/version", handleVersion)
	mux.HandleFunc("/workspace", handleWorkspace)

	// Catch-all: proxy to llama-server
	mux.HandleFunc("/", handlePassthrough)
	return mux
}

// maxCompletionTokens is the ceiling every generation request leaving the
// proxy must carry (ATLAS_MAX_COMPLETION_TOKENS, default 8192).
func maxCompletionTokens() int {
	return envIntOr("ATLAS_MAX_COMPLETION_TOKENS", 8192)
}

// clampGenerationBody guarantees an explicit completion bound on a
// passthrough generation request. Without one, llama-server generates
// with its default n_predict=-1 (until the context fills); a client
// that disconnects mid-stream then leaves a zombie generation holding
// the slot — the H200 ops data showed these saturating every slot.
// The agent loop's own calls already carry max_tokens (agentMaxTokens);
// this closes the passthrough path.
//
// Missing, non-positive (-1 means "unlimited" to llama), or
// above-ceiling values are set to the ceiling. OpenAI-style endpoints
// carry the bound as max_tokens; llama-native /completion(s) and
// /infill as n_predict. Non-generation paths and unparseable bodies
// pass through unchanged — this is a guarantee, not a validator.
func clampGenerationBody(path string, body []byte) []byte {
	var key string
	switch path {
	case "/v1/chat/completions", "/v1/completions":
		key = "max_tokens"
	case "/completion", "/completions", "/infill":
		key = "n_predict"
	default:
		return body
	}
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil || req == nil {
		return body
	}
	ceiling := maxCompletionTokens()
	if v, ok := req[key].(float64); ok && v > 0 && v <= float64(ceiling) {
		return body
	}
	req[key] = ceiling
	clamped, err := json.Marshal(req)
	if err != nil {
		return body
	}
	return clamped
}

func handlePassthrough(w http.ResponseWriter, r *http.Request) {
	// %q on the path quotes + escapes CR/LF so a crafted URL can't
	// fake additional log entries (go/log-injection).
	logEvent("info", fmt.Sprintf("passthrough: %s %q", r.Method, r.URL.Path),
		requestIDFromContext(r.Context()), nil)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, ErrResourceLimit, "request body exceeds the configured limit")
		return
	}
	if r.Method == http.MethodPost {
		body = clampGenerationBody(r.URL.Path, body)
	}
	upstreamURL := inferenceURL + r.URL.RequestURI()
	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrInternal, err.Error())
		return
	}
	proxyReq.Header = r.Header.Clone()
	resp, err := http.DefaultClient.Do(proxyReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, ErrDependencyDown, err.Error())
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		for _, vv := range v {
			w.Header().Add(k, vv)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// --- Private shadow capture --------------------------------------------------
//
// ATLAS still works out what the user demanded by reading their English, and
// the client now declares it structurally. Nothing compares the two. This sink
// exists so a later corpus can measure how often they disagree and why -- and
// nothing else: no record it writes is read by any decision, and none reaches a
// wire. It is off unless an operator names a capture file.
//
// Deliberately not /events: that stream is a documented public contract with a
// permanently connected TUI subscriber and no per-session filter, so anything
// emitted there would be both a schema expansion and a disclosure. Deliberately
// not the lens corpus either -- that is training data.

// shadowQueueDepth bounds what a wedged or slow disk can cost. A full queue
// drops and counts rather than pushing back on an agent request: a diagnostic
// that can stall a user's run is worse than a diagnostic with a hole in it.
const shadowQueueDepth = 1024

// maxTrackedShadowRequests bounds duplicate detection. Beyond it, new ids stop
// being remembered and the footer says so, rather than growing without limit.
const maxTrackedShadowRequests = 100000

// shadowCaptureRoot is the only directory a capture may live in, following the
// same envOr convention as the lens data dir.
func shadowCaptureRoot() string {
	return envOr("ATLAS_DIAGNOSTIC_DIR", "/data/diagnostics")
}

// A sink is open, then closing, then closed, and submission is synchronised
// with that transition rather than merely checking it.
//
// Checking a flag and then sending on the queue cannot be made safe by making
// the flag atomic: the close can land between the check and the send, and a
// send on a closed channel panics -- inside an agent request, which is the one
// thing a diagnostic must never be able to do. So admission is a read lock held
// across the decision AND the enqueue, and the cutoff takes the same lock for
// writing. When the queue closes, no submitter is inside it and no submitter
// can enter and find it open. panic/recover is not used as synchronisation.
type shadowSink struct {
	queue chan []byte
	done  chan struct{}
	f     *os.File

	accepted  atomic.Int64
	written   atomic.Int64
	dropped   atomic.Int64
	errors    atomic.Int64
	duplicate atomic.Int64
	refused   atomic.Int64 // arrived after the cutoff, outside the acquisition
	overflow  atomic.Bool  // duplicate tracking stopped growing

	admit   sync.RWMutex
	closing bool // guarded by admit

	mu   sync.Mutex
	seen map[string]bool

	closeOnce sync.Once
	closeErr  error // the outcome every close() caller reports
	finalErr  error // written by the writer before it closes done
}

// activeShadowSink is written once before the listener opens and never again,
// so every later access is a read of an immutable value.
var activeShadowSink atomic.Pointer[shadowSink]

// openShadowSink prepares the capture, or returns nil when none is configured.
//
// Refuses anything it cannot own: a destination outside the capture root, or
// one that already exists. Appending to a previous capture would silently merge
// two runs into what looks like one, and a corpus cannot tell them apart later.
func openShadowSink() (*shadowSink, error) {
	name := strings.TrimSpace(os.Getenv("ATLAS_SHADOW_CAPTURE"))
	if name == "" {
		return nil, nil
	}
	root, err := filepath.Abs(shadowCaptureRoot())
	if err != nil {
		return nil, fmt.Errorf("shadow capture root: %w", err)
	}
	target, err := filepath.Abs(filepath.Join(root, name))
	if err != nil {
		return nil, fmt.Errorf("shadow capture path: %w", err)
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) ||
		filepath.IsAbs(name) {
		return nil, fmt.Errorf("ATLAS_SHADOW_CAPTURE=%q resolves outside %s", name, root)
	}
	if _, err := os.Stat(target); err == nil {
		return nil, fmt.Errorf("ATLAS_SHADOW_CAPTURE=%q already exists; a capture must be "+
			"fresh so two runs cannot merge into one file", target)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, fmt.Errorf("shadow capture dir: %w", err)
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("shadow capture: %w", err)
	}
	s := newShadowSink(f)
	go s.run()
	return s, nil
}

// newShadowSink builds a sink whose writer has not started, so nothing drains
// the queue until run() is called.
func newShadowSink(f *os.File) *shadowSink {
	return &shadowSink{
		queue: make(chan []byte, shadowQueueDepth),
		done:  make(chan struct{}),
		f:     f,
		seen:  map[string]bool{},
	}
}

func (s *shadowSink) enabled() bool { return s != nil }

// run is the only writer. Records, footer, sync and descriptor all belong to
// it, so nothing can ever write the file concurrently with it -- in particular
// not the close hook, which only ever asks it to stop and waits.
func (s *shadowSink) run() {
	defer close(s.done)
	for line := range s.queue {
		if _, err := s.f.Write(line); err != nil {
			// Counted, never retried, never surfaced: a capture failure is a
			// defective capture, not a failed user run.
			s.errors.Add(1)
			continue
		}
		s.written.Add(1)
	}
	// The queue is closed and drained, and admission stopped before it closed,
	// so no further record exists for this file. Finalise.
	s.finalize()
}

// finalize writes the footer after every record its counters describe, then
// releases the descriptor. Only run() calls it, exactly once, which is what
// makes a footer's presence mean "this acquisition completed".
func (s *shadowSink) finalize() {
	footer, err := json.Marshal(map[string]interface{}{
		"schema_version":            shadowSchemaVersionFooter,
		"record_kind":               "task_contract_shadow_footer",
		"accepted":                  s.accepted.Load(),
		"written":                   s.written.Load(),
		"dropped":                   s.dropped.Load(),
		"errors":                    s.errors.Load(),
		"duplicate_request_ids":     s.duplicate.Load(),
		"request_tracking_overflow": s.overflow.Load(),
		"influences_live_decision":  false,
	})
	if err != nil {
		s.finalErr = err
	} else if _, werr := s.f.Write(append(footer, '\n')); werr != nil {
		s.finalErr = werr
	}
	if serr := s.f.Sync(); serr != nil && s.finalErr == nil {
		s.finalErr = serr
	}
	if cerr := s.f.Close(); cerr != nil && s.finalErr == nil {
		s.finalErr = cerr
	}
}

// submit enqueues without blocking. A full queue drops and counts; a record
// arriving after the cutoff is refused and counted separately, so it can never
// appear in an accepted total the finalised footer is unable to account for.
func (s *shadowSink) submit(rec map[string]interface{}) {
	if s == nil {
		return
	}
	// Marshal outside the admission hold: it is the expensive part and it
	// cannot touch the queue.
	line, err := json.Marshal(rec)
	if err != nil {
		s.errors.Add(1)
		return
	}
	line = append(line, '\n')

	s.admit.RLock()
	defer s.admit.RUnlock()
	if s.closing {
		s.refused.Add(1)
		return
	}
	s.accepted.Add(1)
	select {
	case s.queue <- line:
	default:
		s.dropped.Add(1)
	}
}

// noteRequest records a request id and reports a duplicate within one capture.
func (s *shadowSink) noteRequest(id string) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[id] {
		s.duplicate.Add(1)
		return
	}
	if len(s.seen) >= maxTrackedShadowRequests {
		s.overflow.Store(true)
		return
	}
	s.seen[id] = true
}

// close stops admission, then waits for the writer to finalise, bounded.
//
// It never writes the footer and never closes the descriptor: doing either here
// could race the writer, and a footer racing a record is a file that looks
// complete and is not. The writer owns finalisation, so a footer exists only
// when every record its counters describe was already written.
//
// If the writer does not finish within the deadline the capture is left with no
// footer and the error says so. That is deliberate. A write already inside a
// blocking filesystem syscall cannot be interrupted portably from another
// goroutine -- Go offers no such guarantee for a regular file, and neither
// closing the descriptor nor cancelling a context unblocks it -- so there is no
// safe cutoff to write a footer after. The process is exiting once this hook
// returns; an acquisition with no footer is correctly readable as defective,
// which is better than one that reads as complete and is not.
func (s *shadowSink) close(ctx context.Context, wait time.Duration) error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		// The cutoff and the channel close happen under the same exclusive
		// hold, so no submitter is inside and none can enter to find it open.
		s.admit.Lock()
		s.closing = true
		close(s.queue)
		s.admit.Unlock()

		select {
		case <-s.done:
			s.closeErr = s.finalErr
		case <-time.After(wait):
			s.closeErr = fmt.Errorf("shadow capture did not finalise within %v; "+
				"the capture has no footer and is incomplete", wait)
		case <-ctx.Done():
			s.closeErr = fmt.Errorf("shadow capture finalisation cancelled (%w); "+
				"the capture has no footer and is incomplete", ctx.Err())
		}
	})
	return s.closeErr
}

// --- Bounded graceful shutdown ----------------------------------------------
//
// main() blocked in ListenAndServe and died on log.Fatalf, so a SIGTERM cut the
// process where it stood: an agent request lost its turn mid-write, the TUI's
// permanent /events subscriber had no coordinated close, and ordered cleanup
// had nowhere to run. Anything that must flush before exit -- a diagnostic
// capture, a drained buffer -- needs that landing site to exist first.
//
// The budget is derived, not chosen. An agent request is already bounded by its
// own session context, so the drain window is exactly that session total: a
// signal arriving late in a session does not grant it a fresh 600 seconds, it
// only means the server will wait up to that long for whatever remains. On top
// sits a small margin for the close hooks.
const shutdownHookMargin = 10 * time.Second

// defaultShutdownGraceSec is what the shipped compose file allows. It is an
// operator DECLARATION of the grace the environment will give this process --
// the proxy cannot read an orchestrator's real termination budget, so the two
// are pinned together and validated against the session configuration instead.
const defaultShutdownGraceSec = 650

type shutdownBudgetValues struct {
	drain      time.Duration
	hookMargin time.Duration
}

// shutdownBudget derives the drain window and validates it against the grace
// the operator says the environment allows.
//
// Strict: the required window must be LESS than the declared grace, so there is
// real headroom between the process finishing and the environment killing it.
// A session raised beyond what the declared grace supports refuses to serve
// rather than quietly running with a shutdown that cannot complete -- and
// rather than quietly shortening the session the operator asked for.
func shutdownBudget() (shutdownBudgetValues, error) {
	total, _ := sessionBudget() // the 600s total already contains its reserve
	need := total + shutdownHookMargin

	graceSec := defaultShutdownGraceSec
	if raw := strings.TrimSpace(os.Getenv("ATLAS_SHUTDOWN_GRACE_SEC")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return shutdownBudgetValues{}, fmt.Errorf(
				"ATLAS_SHUTDOWN_GRACE_SEC=%q is not a positive number of seconds", raw)
		}
		graceSec = n
	}
	grace := time.Duration(graceSec) * time.Second
	if need >= grace {
		return shutdownBudgetValues{}, fmt.Errorf(
			"shutdown budget does not fit: session total %v + hook margin %v = %v, "+
				"which is not less than ATLAS_SHUTDOWN_GRACE_SEC=%v. Raise "+
				"ATLAS_SHUTDOWN_GRACE_SEC and the container/orchestrator grace period "+
				"together, or lower ATLAS_AGENT_SESSION_TIMEOUT_SEC",
			total, shutdownHookMargin, need, grace)
	}
	return shutdownBudgetValues{drain: total, hookMargin: shutdownHookMargin}, nil
}

// closeHook is ordered cleanup that runs once, after the listener has stopped
// and request draining has been classified. A hook cannot extend shutdown and
// cannot turn a completed request into a failure.
type closeHook struct {
	name string
	fn   func(context.Context) error
}

// shutdownResult says what actually happened, so a forced close is never
// mistaken for a clean one -- or for a listener failure.
type shutdownResult struct {
	signalled  bool
	forced     bool // the drain deadline expired with requests still running
	hooksRan   bool
	hookErrors []string
}

// runServer serves until the listener fails or a signal arrives, then drains
// within the budget and runs the hooks. Separated from main so a test can drive
// a real listener, a real in-flight request and a real signal.
func runServer(srv *http.Server, ln net.Listener, signals <-chan os.Signal,
	hooks []closeHook, budget shutdownBudgetValues) (shutdownResult, error) {
	var res shutdownResult
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		// A listener that never started must not sit waiting for a signal.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return res, err
		}
		return res, nil
	case <-signals:
		res.signalled = true
	}

	// New connections stop here. Active handlers keep their own deadlines --
	// an agent request is bounded by its session context, not by this.
	log.Printf("[lifecycle] shutdown signal received — draining for up to %v", budget.drain)

	// The infrastructure stream would otherwise hold the drain open for its
	// whole budget: nothing ends /events but the client leaving.
	defaultBroker.drain()

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), budget.drain)
	defer cancelDrain()
	if err := srv.Shutdown(drainCtx); err != nil {
		// Requests outlived the window. Say so as its own outcome.
		res.forced = true
		log.Printf("[lifecycle] drain deadline reached with requests still active — forcing close")
		_ = srv.Close()
	}

	hookCtx, cancelHooks := context.WithTimeout(context.Background(), budget.hookMargin)
	defer cancelHooks()
	res.hooksRan = true
	for _, h := range hooks {
		done := make(chan error, 1)
		go func(h closeHook) { done <- h.fn(hookCtx) }(h)
		select {
		case err := <-done:
			if err != nil {
				res.hookErrors = append(res.hookErrors, h.name+": "+err.Error())
			}
		case <-hookCtx.Done():
			res.hookErrors = append(res.hookErrors, h.name+": "+hookCtx.Err().Error())
		}
	}
	for _, e := range res.hookErrors {
		log.Printf("[lifecycle] close hook failed: %s", e)
	}
	// Drain whatever Serve reports after Shutdown; ErrServerClosed is normal.
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return res, err
		}
	case <-time.After(time.Second):
	}
	return res, nil
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	// Private-value filtering: every log line passes through the
	// filter before it reaches stderr (filteringWriter, below).
	// In json mode the filtered line is then wrapped into a JSON
	// record; the record stamps its own ts, so the log package's
	// time prefix is dropped to keep it out of msg.
	out := io.Writer(os.Stderr)
	if logJSON {
		log.SetFlags(0)
		out = jsonLineWriter{w: os.Stderr}
	}
	log.SetOutput(filteringWriter{w: out})

	addr := ":" + proxyPort
	log.Printf("ATLAS Proxy v3.1.6 starting on %s", addr)
	log.Printf("  Inference: %s", inferenceURL)
	log.Printf("  Geometric Lens: %s", lensURL)
	log.Printf("  Sandbox: %s", sandboxURL)
	log.Printf("  Pipeline: agent loop (/v1/agent) + V3 candidate pipeline in v3-service for T2/T3 writes")

	// Probe geometric-lens + ASA calibration so operators see the
	// same verdict the TUI's header badge will render. The old "ASA
	// steering: present at X" banner is folded into logCalibrationStatusAtStartup
	// below (which also adds the corresponding Lens line) so the proxy
	// surfaces a unified calibration view at startup.
	installTokenTransport()

	logCalibrationStatusAtStartup()

	if envOr("ATLAS_KEEP_LLAMA_WARM", "1") != "0" {
		go keepLlamaWarm()
		log.Printf("  Keep-warm: pinging %s every 45s (set ATLAS_KEEP_LLAMA_WARM=0 to disable)", inferenceURL)
	}

	// Validated before the listener opens: a shutdown that cannot finish
	// inside the environment's grace is a configuration error, not something
	// to discover during a deploy.
	budget, err := shutdownBudget()
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}
	log.Printf("  Shutdown: drain up to %v, then %v for close hooks", budget.drain, budget.hookMargin)

	server := &http.Server{
		Addr:              addr,
		Handler:           http.MaxBytesHandler(withRequestID(requireServiceToken(newProxyMux())), maxRequestBodyBytes),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	// Opened before the listener: a capture that cannot be created is a
	// configuration error to discover now, not mid-acquisition.
	sink, err := openShadowSink()
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}
	var hooks []closeHook
	if sink != nil {
		activeShadowSink.Store(sink)
		log.Printf("  Shadow capture: enabled (%s)", os.Getenv("ATLAS_SHADOW_CAPTURE"))
		hooks = append(hooks, closeHook{
			name: "shadow-capture",
			fn: func(hctx context.Context) error {
				return sink.close(hctx, budget.hookMargin)
			},
		})
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listen %s: %v", addr, err)
	}
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	// hooks is empty unless a diagnostic capture is configured, which is the
	// ordinary production path and costs nothing.
	res, serveErr := runServer(server, ln, signals, hooks, budget)
	if serveErr != nil {
		log.Printf("server error: %v", serveErr)
		os.Exit(1)
	}
	switch {
	case res.forced:
		log.Printf("[lifecycle] shutdown complete (forced: requests outlived the drain window)")
	case res.signalled:
		log.Printf("[lifecycle] shutdown complete")
	}
}

// keepLlamaWarm pings llama-server with a 1-token completion every 45s. Keeps
// the model loaded in VRAM, the slot's prompt cache live, and the TCP keepalive
// fresh — avoiding the cold-start path that fires after 1-2 min idle.
// Disable with ATLAS_KEEP_LLAMA_WARM=0.
func keepLlamaWarm() {
	const interval = 45 * time.Second
	// Wait for llama-server to come up before starting the loop.
	time.Sleep(15 * time.Second)
	body, _ := json.Marshal(map[string]any{
		"messages":    []map[string]string{{"role": "user", "content": "."}},
		"max_tokens":  1,
		"temperature": 0.0,
	})
	client := &http.Client{Timeout: 60 * time.Second}
	for {
		req, err := http.NewRequest("POST", inferenceURL+"/v1/chat/completions", bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}
		time.Sleep(interval)
	}
}

// ---------------------------------------------------------------------------
// Model-based intent classification (Section 1 of production checklist)
// ---------------------------------------------------------------------------

// Tier represents the complexity classification of a request
type Tier int

const (
	Tier0Conversational Tier = 0 // instant response, no pipeline
	Tier1Simple         Tier = 1 // single file, obvious intent
	Tier2Medium         Tier = 2 // multi-file awareness, spec + verify
	Tier3Hard           Tier = 3 // full pipeline, best-of-K, multi-step verify
)

func (t Tier) String() string {
	switch t {
	case Tier0Conversational:
		return "T0:chat"
	case Tier1Simple:
		return "T1:simple"
	case Tier2Medium:
		return "T2:medium"
	case Tier3Hard:
		return "T3:hard"
	}
	return "T?:unknown"
}

// Correlation IDs + structured logging.
//
// Every inbound request gets an X-ATLAS-Request-ID (read from the client
// or generated), echoed in the response and stored in the request
// context. Outbound calls to llama/v3/lens/sandbox forward the same ID
// (tokenTransport reads it from the request context), so one turn is
// traceable across services.
//
// Log format is line-oriented by default; ATLAS_LOG_FORMAT=json emits
// one JSON object per line with stable fields. Both paths still pass
// through the private-value filter (main() wraps the log writer).

const requestIDHeader = "X-ATLAS-Request-ID"

type ctxKey string

const requestIDKey ctxKey = "atlas-request-id"

func newRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "req-unknown"
	}
	return "req-" + hex.EncodeToString(b)
}

func requestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// withRequestID wraps a handler so every request carries a correlation
// ID (client-provided or generated), echoed back and put in the context.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(requestIDHeader))
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// --- structured logging --------------------------------------------------

var logJSON = strings.EqualFold(os.Getenv("ATLAS_LOG_FORMAT"), "json")

// logEvent emits one structured record. In json mode it's a JSON object
// with stable fields; otherwise a readable line. request_id is included
// when present. Fields beyond the standard set are passed as kv pairs.
func logEvent(level, msg, requestID string, kv map[string]interface{}) {
	if logJSON {
		rec := map[string]interface{}{
			"ts":      time.Now().UTC().Format(time.RFC3339Nano),
			"level":   level,
			"service": "atlas-proxy",
			"version": APIVersion,
			"msg":     msg,
		}
		if requestID != "" {
			rec["request_id"] = requestID
		}
		for k, v := range kv {
			rec[k] = v
		}
		b, err := json.Marshal(rec)
		if err != nil {
			log.Printf("%s: %s", level, msg)
			return
		}
		log.Printf("%s", b)
		return
	}
	// line mode
	if requestID != "" {
		log.Printf("[%s] [%s] %s", level, requestID, msg)
	} else {
		log.Printf("[%s] %s", level, msg)
	}
}

// jsonLineWriter converts each (already private-value-filtered) log line
// into the same JSON record shape logEvent emits, so ATLAS_LOG_FORMAT=json
// covers every log call in the process, not only logEvent call sites.
// Lines that are already JSON objects (logEvent's json-mode output) pass
// through unchanged.
type jsonLineWriter struct {
	w io.Writer
}

func (j jsonLineWriter) Write(p []byte) (int, error) {
	line := bytes.TrimRight(p, "\n")
	if len(line) > 0 && line[0] == '{' && json.Valid(line) {
		return j.w.Write(p)
	}
	rec := map[string]interface{}{
		"ts":      time.Now().UTC().Format(time.RFC3339Nano),
		"level":   "info",
		"service": "atlas-proxy",
		"version": APIVersion,
		"msg":     string(line),
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return j.w.Write(p)
	}
	b = append(b, '\n')
	if _, err := j.w.Write(b); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Internal service authentication (per-installation token).
//
// One random token, generated by `atlas init` into
// secrets/service-token (0600) and mounted read-only into every
// container at /run/atlas-secrets/service-token, authenticates
// internal and client requests as `Authorization: Bearer <token>`.
//
// Enforcement is enabled iff a token file is configured and readable —
// an install that never ran `atlas init` keeps today's open-localhost
// behavior, and `atlas doctor` flags it. /health and /ready stay
// unauthenticated (compose/K8s probes are headerless curl).
//
// The token value must never be logged, placed in argv, or echoed in
// error bodies.

// serviceToken is loaded once at startup. Rotation = rewrite the file
// (atlas init --rotate-token) + restart the stack.
var serviceToken = loadServiceToken()

func loadServiceToken() string {
	path := os.Getenv("ATLAS_SERVICE_TOKEN_FILE")
	if path == "" {
		path = "/run/atlas-secrets/service-token"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "" // unconfigured => auth disabled (doctor warns)
	}
	return strings.TrimSpace(string(data))
}

// authOpenPaths never require the token: health probes are headerless
// curl in compose/K8s, and /ready gates orchestration.
func authOpenPath(path string) bool {
	return path == "/health" || path == "/ready" || path == "/version"
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if strings.HasPrefix(h, prefix) {
		return h[len(prefix):]
	}
	return ""
}

// requireServiceToken wraps a handler with token enforcement.
func requireServiceToken(next http.Handler) http.Handler {
	if serviceToken == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authOpenPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		got := bearerToken(r)
		if subtle.ConstantTimeCompare([]byte(got), []byte(serviceToken)) != 1 {
			// No token material in the response or the log line.
			writeError(w, http.StatusUnauthorized, ErrUnauthorized,
				"internal service auth is enabled; send Authorization: "+
					"Bearer <service-token> (secrets/service-token)")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// tokenTransport injects the service token into outbound requests
// (proxy -> llama/v3/lens/sandbox) unless the caller already set an
// Authorization header (e.g. the /v1/chat passthrough forwarding a
// client's own header).
type tokenTransport struct {
	base http.RoundTripper
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Forward the correlation ID (if the request carries one in context)
	// so downstream service logs join the same trace.
	if id := requestIDFromContext(req.Context()); id != "" &&
		req.Header.Get(requestIDHeader) == "" {
		req = req.Clone(req.Context())
		req.Header.Set(requestIDHeader, id)
	}
	if serviceToken != "" && req.Header.Get("Authorization") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+serviceToken)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// installTokenTransport wires outbound injection through the two
// transport choke points: the process default (covers every client
// with a nil Transport — http.DefaultClient, &http.Client{} literals,
// http.Post) and the dedicated LLM streaming client.
// The transport is installed even when auth is unconfigured: RoundTrip
// guards token injection on serviceToken, and correlation-ID forwarding
// must work on open-localhost installs too.
func installTokenTransport() {
	http.DefaultTransport = &tokenTransport{base: http.DefaultTransport}
	llmStreamClient.Transport = &tokenTransport{base: llmStreamClient.Transport}
	if serviceToken != "" {
		log.Printf("  Internal auth: enabled (token file configured)")
	}
}

// API / protocol versioning and a stable error-code taxonomy.
//
// APIVersion is the contract version for the proxy's HTTP + SSE surface.
// Clients read it from GET /version (and it rides on error envelopes) so
// a breaking change is a visible version bump, not a silent shape change.
//
// ErrorCode is a CLOSED set of machine-readable codes. Clients switch on
// the code, never on the human message — the message can change freely;
// the code is the contract. New failure modes get a new code; existing
// codes keep their meaning.

// APIVersion follows semver; bump minor for additive, major for breaking.
const APIVersion = "1.0.0"

// ProtocolVersion is the SSE event-envelope contract version (see
// proxy/events.go / atlas.cli.events).
const ProtocolVersion = 1

type ErrorCode string

const (
	ErrUnauthorized   ErrorCode = "unauthorized"
	ErrInvalidInput   ErrorCode = "invalid_input"
	ErrUnsupported    ErrorCode = "unsupported_operation"
	ErrDependencyDown ErrorCode = "dependency_unavailable"
	ErrResourceLimit  ErrorCode = "resource_limit"
	ErrInternal       ErrorCode = "internal_error"
)

// AllErrorCodes is the canonical closed set (asserted by the contract
// test against the documented taxonomy). Every code here is emitted by
// a live writeError call — aspirational codes were pruned 2026-08-05.
var AllErrorCodes = []ErrorCode{
	ErrUnauthorized, ErrInvalidInput, ErrUnsupported,
	ErrDependencyDown, ErrResourceLimit, ErrInternal,
}

// ErrorEnvelope is the stable error shape: a code (switch on this), a
// human message, and the API version.
type ErrorEnvelope struct {
	Error      string `json:"error"`  // the ErrorCode
	Detail     string `json:"detail"` // human message (may change)
	APIVersion string `json:"api_version"`
}

func writeError(w http.ResponseWriter, status int, code ErrorCode,
	detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorEnvelope{
		Error:      string(code),
		Detail:     detail,
		APIVersion: APIVersion,
	})
}

func handleVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	total, _ := sessionBudget()
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"api_version":      APIVersion,
		"protocol_version": ProtocolVersion,
		"error_codes":      AllErrorCodes,
		// The tool-call grammar mode this process applies. A run records
		// it: two installs of one model can differ here and behave
		// differently.
		"grammar_mode": effectiveGrammarMode(),
		// The session limit a measurement's budget has to equal.
		"session_timeout_s": int(total / time.Second),
	})
}

// containerMarkers are files a container runtime creates in every
// container: Docker writes /.dockerenv, Podman writes /run/.containerenv.
// A var so tests can point it at temp paths.
var containerMarkers = []string{"/.dockerenv", "/run/.containerenv"}

// isContainerized reports whether this process runs in a container.
// ATLAS_WORKSPACE_DIR is no signal: the local (non-Docker) proxy launcher
// sets it too (runtime.py:283). Kubernetes, and so K3s, sets
// KUBERNETES_SERVICE_HOST in every pod, where containerd creates neither
// marker file.
func isContainerized() bool {
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return true
	}
	for _, m := range containerMarkers {
		if _, err := os.Stat(m); err == nil {
			return true
		}
	}
	return false
}

func handleWorkspace(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed,
			ErrUnsupported, "method not allowed")
		return
	}
	hostPath := os.Getenv("ATLAS_PROJECT_DIR")
	containerPath := os.Getenv("ATLAS_WORKSPACE_DIR")
	containerized := isContainerized()
	if !filepath.IsAbs(hostPath) {
		hostPath = ""
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"project_dir":   hostPath,
		"working_dir":   containerPath,
		"containerized": containerized,
	})
}

// safeLogField encodes untrusted text as one quoted ASCII log field. Newlines,
// carriage returns, and control bytes become escape sequences, so model/user
// data cannot forge additional log records.
func safeLogField(value string, maxLen int) string {
	if maxLen > 0 {
		value = truncateStr(value, maxLen)
	}
	return strconv.QuoteToASCII(value)
}

// Private-value filtering: masks values that look like credentials
// before they reach a serialized sink. The proxy installs this on the
// standard logger's output (one choke point covers every log.Printf),
// so an error that happens to embed an env assignment or a header
// never lands in the log verbatim.
//
// The pattern spec is shared with the Python services via the fixture
// corpus at tests/fixtures/private_value_fixtures.json — change the
// patterns here and there together; the contract test runs the corpus
// against every implementation.
//
// Patterns are deliberately conservative (assignment/header/key-block
// shapes with secret-ish key names) so ordinary log content —
// "timeout=30", token counts, health URLs — passes through untouched.

const privateValuePlaceholder = "[FILTERED]"

var privateValuePatterns = []*regexp.Regexp{
	// KEY=value / key: value / "key": "value" assignments where the key
	// smells like a credential. Value part is masked, key kept.
	regexp.MustCompile(`(?i)([A-Z0-9_.-]{0,64}(?:api[_-]?key|apikey|token|secret|password|passwd|credential|access[_-]?key)[A-Z0-9_.-]{0,64}["']?\s*[=:]\s*["']?)([^\s"',;&]+)`),
	// Authorization / bearer values.
	regexp.MustCompile(`(?i)(bearer\s+)([A-Za-z0-9._~+/=-]+)`),
	// URL userinfo passwords: scheme://user:pass@host
	regexp.MustCompile(`(://[^/:@\s]{0,64}:)([^@\s]{1,256})(@)`),
	// Private-key blocks (any BEGIN ... PRIVATE KEY variant), body inclusive.
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]{0,40}PRIVATE KEY-----.*?-----END [A-Z ]{0,40}PRIVATE KEY-----`),
}

// filterPrivateValues masks credential-shaped substrings in s.
func filterPrivateValues(s string) string {
	// Key-block pattern replaces the whole match; assignment patterns
	// keep the key and mask the value.
	s = privateValuePatterns[3].ReplaceAllString(s, privateValuePlaceholder)
	s = privateValuePatterns[0].ReplaceAllString(s, "${1}"+privateValuePlaceholder)
	s = privateValuePatterns[1].ReplaceAllString(s, "${1}"+privateValuePlaceholder)
	s = privateValuePatterns[2].ReplaceAllString(s, "${1}"+privateValuePlaceholder+"${3}")
	return s
}

// filteringWriter applies the filter to every write — installed as the
// standard logger's output in main(), so all proxy log lines pass
// through it. Line-buffered writes from log.Printf arrive whole, so
// per-write filtering is sound for the standard logger.
type filteringWriter struct {
	w io.Writer
}

func (f filteringWriter) Write(p []byte) (int, error) {
	filtered := filterPrivateValues(string(p))
	if _, err := f.w.Write([]byte(filtered)); err != nil {
		return 0, err
	}
	// Report the original length so log.Printf never sees a short write.
	return len(p), nil
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Code inside a JSON string must escape every quote and newline, and dense
// lines are where the model fails: the same debounce solution parses 6 of 6
// times in a fenced block and 0 of 6 times as a JSON string. "@fenced" routes
// the file body around the JSON channel through one unconstrained sub-call.

func TestFencedContentExtraction(t *testing.T) {
	markdown := "# readme\n\n```\nexample\n```\n\ntail line"
	for _, tc := range []struct {
		name, reply, want string
	}{
		{"tagged block in prose", "Here is the file:\n```python\nx = 1\nprint(x)\n```\nDone.", "x = 1\nprint(x)\n"},
		{"bare fence", "```\ny = 2\n```", "y = 2\n"},
		{"c++ tag", "```c++\nint x;\n```", "int x;\n"},
		{"c# tag", "```c#\nint x;\n```", "int x;\n"},
		{"objective-c tag", "```objective-c\nint x;\n```", "int x;\n"},
		// A file's own leading blank lines are its content, not part of the
		// fence line. (Whitespace-only content is not a case: an empty block
		// is what makes the sub-call ask again.)
		{"one leading blank line", "```python\n\nx = 1\n```", "\nx = 1\n"},
		{"three leading blank lines", "```python\n\n\n\nx = 1\n```", "\n\n\nx = 1\n"},
		{"blank lines before a def", "```python\n\n\ndef f():\n    pass\n```", "\n\ndef f():\n    pass\n"},
		{"CRLF fence line", "```python\r\nx = 1\n```", "x = 1\n"},
		// Inside a ``` wrapper, the file's own ``` lines look the same as a
		// second block, so nothing is returned and the sub-call asks again
		// with a four-backtick note. Inside a ```` wrapper the whole file
		// comes back.
		{"file with ``` lines in a ``` wrapper", "```markdown\n" + markdown + "\n```", ""},
		{"file with ``` lines in a ```` wrapper", "````markdown\n" + markdown + "\n````", markdown + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractFencedContent(tc.reply); got != tc.want {
				t.Errorf("extractFencedContent(%q) = %q, want %q", tc.reply, got, tc.want)
			}
		})
	}
}

func TestFenceTagFollowsExtension(t *testing.T) {
	for path, want := range map[string]string{"app.html": "html", "main.go": "go", "solve.py": "python"} {
		if got := fenceTagForPath(path); got != want {
			t.Errorf("fenceTagForPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestRawResponseForFenceRoundTrips(t *testing.T) {
	parsed := ModelResponse{Type: "tool_call", Name: "write_file",
		Args: json.RawMessage(`{"path":"solve.py","content":"@fenced"}`)}
	raw := rawResponseForFence(parsed)
	var back ModelResponse
	if json.Unmarshal([]byte(raw), &back) != nil || back.Name != "write_file" {
		t.Fatalf("round trip failed: %s", raw)
	}
}

// An inline body reaches disk only when its framing proves it complete: a
// closed fence. A bare body carries no framing either way, so it may have
// stopped mid-emission, and it is resolved through the sub-call instead.
func TestInlineFencedBodyRequiresFramingToProveCompletion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		inline   string
		wantBody string // "": falls back to the sub-call
	}{
		{"truncated fence", "```python\nprint(", ""},
		{"opener only", "```python", ""},
		{"complete fence", "```python\nprint(\"hi\")\n```", "print(\"hi\")\n"},
		{"bare body, however finished it looks", "print(\"hi\")\n", ""},
		{"bare body cut mid-emission", "print(", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, _ := resolveInlineFencedBody(tc.inline)
			if !ok {
				got = ""
			}
			if got != tc.wantBody {
				t.Errorf("resolved %q, want %q", got, tc.wantBody)
			}
		})
	}
}

// stubInference serves /v1/chat/completions with the given replies in order,
// each stamped with tokensPer usage tokens. Other paths (the /slots prompt
// poller) get 404s and are not counted as calls.
func stubInference(t *testing.T, replies []string, tokensPer int, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.NotFound(w, r)
			return
		}
		io.Copy(io.Discard, r.Body) // the whole request first, as a real model server reads it
		i := *calls
		*calls++
		if i >= len(replies) {
			i = len(replies) - 1
		}
		writeStubSSE(w, replies[i], tokensPer)
	}))
}

// Every fenced sub-call attempt is a generation, and the run totals carry
// each one. A reply with no block is not content, and neither is a block that
// holds only the sentinel (it would land a file whose whole content is the
// word @fenced): the sub-call asks again.
func TestFencedFetchAccountsEveryAttempt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replies []string
		want    string
		calls   int
	}{
		{"block on the first attempt", []string{"```python\nx = 1\n```"}, "x = 1\n", 1},
		{"no block, then a block", []string{"no fence here, sorry", "```python\ny = 2\n```"}, "y = 2\n", 2},
		{"sentinel only, then a block", []string{"```python\n@fenced\n```", "```python\nprint(\"real\")\n```"},
			"print(\"real\")\n", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := stubInference(t, tc.replies, 100, &calls)
			defer srv.Close()
			t.Setenv("ATLAS_LLAMA_URL", srv.URL)

			ctx := &AgentContext{}
			got, err := fetchFencedContent(ctx, `{"type":"tool_call"}`, "a.py")
			if err != nil || got != tc.want {
				t.Fatalf("fetch = %q, %v; want %q", got, err, tc.want)
			}
			if calls != tc.calls || ctx.FencedCalls != tc.calls ||
				ctx.FencedTokens != 100*tc.calls || ctx.TotalTokens != 100*tc.calls {
				t.Errorf("calls=%d fenced=%d/%d total=%d; want %d calls of 100 tokens each",
					calls, ctx.FencedCalls, ctx.FencedTokens, ctx.TotalTokens, tc.calls)
			}
		})
	}
}

// serveLoopServices answers the requests of an agent-loop test that are not
// model calls: V3 is unavailable, the syntax check rejects code that holds
// "]]", and every command succeeds (the mount probe reads its file from dir).
// It returns false for a model call, which the caller answers.
func serveLoopServices(w http.ResponseWriter, r *http.Request, dir string) bool {
	switch {
	case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	case strings.HasSuffix(r.URL.Path, "/syntax-check"):
		var in struct{ Code string }
		json.NewDecoder(r.Body).Decode(&in)
		if strings.Contains(in.Code, "]]") {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"valid": false, "errors": []string{"SyntaxError: unmatched ']' (line 2)"}})
		} else {
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
		}
	case strings.HasSuffix(r.URL.Path, "/execute"):
		var in struct{ Code string }
		json.NewDecoder(r.Body).Decode(&in)
		stdout := ""
		if strings.Contains(in.Code, ".atlas-mount-probe") {
			b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
			stdout = string(b)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true, "stdout": stdout, "exit_code": 0})
	case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
		http.NotFound(w, r)
	default:
		return false
	}
	return true
}

// --- The fenced allowance ---------------------------------------------------
//
// A path may fail maxFencedFailuresPerPath fenced fetches in a session. The
// count lives in session state, keyed by the canonical path, so a new
// write_file call does not restore it.

// fencedFailStub counts its requests and answers each with a reply that holds
// no block, except request number succeedOnCall (from 0; -1: none), which
// gets one.
func fencedFailStub(t *testing.T, calls *int, succeedOnCall int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := *calls
		*calls++
		if i == succeedOnCall {
			serveReply(w, "```python\nx = 1\n```")
			return
		}
		serveReply(w, "I am thinking about it.")
	}))
}

func fencedCtx(t *testing.T, url string) *AgentContext {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.InferenceURL = url
	ctx.Ctx = context.Background()
	return ctx
}

// The allowance is spent once per file. A new write_file call for the same
// file, under any spelling, starts no generation; another file has its own
// allowance.
func TestTheFencedAllowanceIsPerCanonicalPath(t *testing.T) {
	calls := 0
	srv := fencedFailStub(t, &calls, -1)
	defer srv.Close()
	ctx := fencedCtx(t, srv.URL)

	if _, err := fetchFencedContent(ctx, "raw", "solve.py"); err == nil {
		t.Fatal("expected failure")
	}
	if calls != maxFencedFailuresPerPath {
		t.Fatalf("the first call spent %d generations, want the whole allowance (%d)",
			calls, maxFencedFailuresPerPath)
	}
	for _, spelling := range []string{"solve.py", "./solve.py"} {
		if _, err := fetchFencedContent(ctx, "raw", spelling); err == nil {
			t.Fatalf("expected a refusal for %s", spelling)
		}
	}
	if calls != maxFencedFailuresPerPath {
		t.Errorf("a new write_file call restored the allowance: %d generations, want %d",
			calls, maxFencedFailuresPerPath)
	}
	if _, err := fetchFencedContent(ctx, "raw", "other.py"); err == nil {
		t.Fatal("expected other.py to fail on its own allowance")
	}
	if calls != 2*maxFencedFailuresPerPath {
		t.Errorf("other.py spent %d generations, want its own allowance (%d)",
			calls-maxFencedFailuresPerPath, maxFencedFailuresPerPath)
	}
	if len(ctx.FencedFailures) != 2 ||
		ctx.FencedFailures[fencedKey(ctx, "solve.py")] != maxFencedFailuresPerPath ||
		ctx.FencedFailures[fencedKey(ctx, "other.py")] != maxFencedFailuresPerPath {
		t.Errorf("failure counts = %v, want %d for each of the two files",
			ctx.FencedFailures, maxFencedFailuresPerPath)
	}
}

// One retry may follow a zero-byte failure, and a success clears the path's
// failures, so the path keeps its whole allowance.
func TestOneConstrainedRetryMayFollowAZeroByteFailure(t *testing.T) {
	calls := 0
	srv := fencedFailStub(t, &calls, 1) // first attempt empty, retry succeeds
	defer srv.Close()
	ctx := fencedCtx(t, srv.URL)

	out, err := fetchFencedContent(ctx, "raw", "solve.py")
	if err != nil {
		t.Fatalf("the constrained retry should have succeeded: %v", err)
	}
	if !strings.Contains(out, "x = 1") {
		t.Errorf("retry payload lost: %q", out)
	}
	if calls != 2 {
		t.Errorf("expected one failure plus one retry, got %d generations", calls)
	}
	if n := ctx.FencedFailures[fencedKey(ctx, "solve.py")]; n != 0 {
		t.Errorf("success must clear the consecutive-failure state, got %d", n)
	}
	if fencedBudgetExhausted(ctx, "solve.py") {
		t.Error("a healthy path stays exhausted")
	}
}

// A fetch starts only when it fits before the work deadline with the reserve
// left over to check the result. With no deadline (a direct caller), only the
// allowance bounds it.
func TestFencedAdmissionObservesTheWorkDeadline(t *testing.T) {
	for _, tc := range []struct {
		name string
		left time.Duration // time to the work deadline; 0: no deadline
	}{
		{"no deadline", 0},
		{"ten minutes left", 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := fencedFailStub(t, &calls, -1)
			defer srv.Close()
			ctx := fencedCtx(t, srv.URL)
			if tc.left > 0 {
				session, cancel := context.WithTimeout(context.Background(), tc.left)
				defer cancel()
				ctx.Ctx = session
			}

			if !fencedFitsRemainingBudget(ctx) {
				t.Fatal("fencedFitsRemainingBudget = false, want true")
			}
			_, err := fetchFencedContent(ctx, "raw", "solve.py")
			if err == nil {
				t.Fatal("the stub never sends a block, so the fetch must fail")
			}
			if calls != maxFencedFailuresPerPath {
				t.Errorf("%d generations, want the allowance (%d)", calls, maxFencedFailuresPerPath)
			}
		})
	}
}

// A fetch that does not fit before the work deadline is refused before it
// starts a generation, and the refusal names the budget.
func TestAFencedFetchThatDoesNotFitTheWorkDeadlineIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		left time.Duration // time to the work deadline
	}{
		{"two seconds left", 2 * time.Second},
		{"half a second left", 500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := fencedFailStub(t, &calls, -1)
			defer srv.Close()
			ctx := fencedCtx(t, srv.URL)
			session, cancel := context.WithTimeout(context.Background(), tc.left)
			defer cancel()
			ctx.Ctx = session

			if fencedFitsRemainingBudget(ctx) {
				t.Fatal("fencedFitsRemainingBudget = true, want false")
			}
			_, err := fetchFencedContent(ctx, "raw", "solve.py")
			if err == nil {
				t.Fatal("the stub never sends a block, so the fetch must fail")
			}
			if calls != 0 || !strings.Contains(err.Error(), "session budget") {
				t.Errorf("a refused fetch spent %d generations or did not name the budget: %v", calls, err)
			}
		})
	}
}

// A fetch the session refuses reads nothing from disk: the file's current
// bytes are read only for an attempt that is made. The target is a named pipe
// with no writer, so a read of it would wait there and the fetch would not
// return.
func TestARefusedFencedFetchReadsNoFile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stalls   int           // the session's fenced stalls so far
		failures int           // the attempts the path has spent
		left     time.Duration // time to the work deadline
	}{
		{"the channel is off for the session", fencedSessionStallLimit, 0, 10 * time.Minute},
		{"the path's allowance is spent", 0, maxFencedFailuresPerPath, 10 * time.Minute},
		{"the session has no time for a fetch", 0, 0, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := fencedFailStub(t, &calls, -1)
			defer srv.Close()
			ctx := fencedCtx(t, srv.URL)
			pipe := filepath.Join(ctx.WorkingDir, "pipe.py")
			if err := syscall.Mkfifo(pipe, 0o600); err != nil {
				t.Fatalf("make the named pipe: %v", err)
			}
			ctx.FencedStalls = tc.stalls
			ctx.FencedFailures = map[string]int{fencedKey(ctx, "pipe.py"): tc.failures}
			session, cancel := context.WithTimeout(context.Background(), tc.left)
			defer cancel()
			ctx.Ctx = session

			refused := make(chan error, 1)
			go func() {
				_, err := fetchFencedContent(ctx, "raw", "pipe.py")
				refused <- err
			}()
			select {
			case err := <-refused:
				if err == nil {
					t.Fatal("the fetch was not refused")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the fetch read the file before it was refused")
			}
			if calls != 0 {
				t.Errorf("a refused fetch started %d generations", calls)
			}
		})
	}
}

// The default budget (600s, 30s reserve) leaves room for a fetch.
func TestTheDefaultWorkDeadlineAdmitsAFencedFetch(t *testing.T) {
	t.Setenv("ATLAS_AGENT_SESSION_TIMEOUT_SEC", "")
	total, reserve := sessionBudget()
	if total-reserve <= fencedFirstContentTimeout()+fencedReserve {
		t.Errorf("the default work deadline (%v) cannot admit a single fenced "+
			"fetch", total-reserve)
	}
}

// A failed resolution must not touch the file.
func TestFailedFencedResolutionLeavesDiskBytesUnchanged(t *testing.T) {
	calls := 0
	srv := fencedFailStub(t, &calls, -1)
	defer srv.Close()
	ctx := fencedCtx(t, srv.URL)
	target := filepath.Join(ctx.WorkingDir, "solve.py")
	original := "original = True\n"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	fetchFencedContent(ctx, "raw", "solve.py")
	after, _ := os.ReadFile(target)
	if string(after) != original {
		t.Errorf("failed resolution modified disk: %q", string(after))
	}
}

func TestNoPersistentStateOrWireFieldWasAdded(t *testing.T) {
	types, err := os.ReadFile("types.go")
	if err != nil {
		t.Fatal(err)
	}
	// Session-scoped, in-memory only: no file, no DB, no wire field.
	if !strings.Contains(string(types), "FencedFailures map[string]int") {
		t.Fatal("the allowance is not session state")
	}
	for _, f := range append([]string{"types.go", "tool_io.go"}, agentFiles...) {
		src := readSourceForTest(t, f)
		for _, leak := range []string{`json:"fenced_failures`, "os.WriteFile(fencedState"} {
			if strings.Contains(src, leak) {
				t.Errorf("%s: %q suggests a wire field or persistent state", f, leak)
			}
		}
	}
}

// The allowance and the repeat detector count one call that the model sends
// again, so they read the raw intent. An executed write is identified by the
// bytes it proposes instead (fenced_proposal_test.go).

// The repeat detector and the fenced allowance both count the model re-sending
// ONE call, and both still read the raw intent. Moving them to the proposal
// would make a channel that returns different bytes look like progress.
func TestChannelIdentityStaysRaw(t *testing.T) {
	src := agentSources(t)
	for _, line := range strings.Split(src, "\n") {
		l := strings.TrimSpace(line)
		if strings.Contains(l, "recordToolCall(ctx,") && !strings.Contains(l, "intentArgs") {
			t.Errorf("the repeat detector no longer reads the raw intent: %s", l)
		}
		if strings.Contains(l, "fencedKey(ctx,") && strings.Contains(l, "retryIdentityArgs") {
			t.Errorf("the fenced allowance was moved off the raw target: %s", l)
		}
	}
	// And the ban's three operations agree with each other.
	var lookup, record, cleared int
	for _, line := range strings.Split(src, "\n") {
		l := strings.TrimSpace(line)
		switch {
		case strings.Contains(l, "identicalRetryRefusal(ctx"):
			lookup++
		case strings.Contains(l, "recordFailedToolCall(ctx, parsed.Name,"):
			record++
		case strings.Contains(l, "clearFailedToolCall(ctx"):
			cleared++
		}
	}
	t.Logf("FailedToolCalls operations: %d lookup, %d record, %d clear", lookup, record, cleared)
	if lookup != 1 || cleared != 1 {
		t.Errorf("expected one lookup and one clear, got %d/%d", lookup, cleared)
	}
}

// Nothing about the bounds moved. The identity change is an identity change:
// no threshold, no counter reset, no allowance was introduced with it.
func TestC4IdentityChangeIntroducedNoAllowance(t *testing.T) {
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"maxTotalFailures", maxTotalFailures, 12},
		{"toolRepeatThreshold", toolRepeatThreshold, 3},
		{"toolRepeatWindow", toolRepeatWindow, 8},
		{"maxFencedFailuresPerPath", maxFencedFailuresPerPath, 2},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d — the C4 identity change must not move a bound",
				c.name, c.got, c.want)
		}
	}
	// The streak reset that lets a converging model keep going predates this
	// commit and is the existing changed-rejection-kind rule, not a new
	// epoch. It has exactly one site.
	// Four, all predating this commit: the changed-rejection-kind reset, the
	// two path-aware-breaker resets, and the one on a successful call.
	resets := strings.Count(agentSources(t), "consecutiveErrors = 0")
	t.Logf("consecutiveErrors reset sites: %d", resets)
	if resets != 4 {
		t.Errorf("%d reset sites; a new one would be an undeclared allowance", resets)
	}
}

// Each fenced attempt is recorded in a fenced_fetch event: what the wire
// showed and what became of the attempt. A channel can be lost when the first
// attempt streams the file and cannot close, and the raw retry sends no
// content: these events make that visible in the event stream.

type fencedEvents struct {
	mu  sync.Mutex
	evs []map[string]interface{}
}

func (f *fencedEvents) record(et string, data interface{}) {
	if et != "fenced_fetch" {
		return
	}
	b, _ := json.Marshal(data)
	var m map[string]interface{}
	json.Unmarshal(b, &m)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evs = append(f.evs, m)
}

func (f *fencedEvents) all() []map[string]interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]interface{}{}, f.evs...)
}

func fencedEventCtx(t *testing.T, url string) (*AgentContext, *fencedEvents) {
	t.Helper()
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.InferenceURL = url
	ctx.Ctx = context.Background()
	fe := &fencedEvents{}
	ctx.StreamFn = fe.record
	return ctx, fe
}

const fencedEventCall = `{"type":"tool_call","name":"write_file","args":{"path":"app.py","content":"@fenced"}}`

// isChatCompletion reports a model request. Every other path, the prompt
// progress poller's /slots among them, must answer at once: the call waits
// for the poller when it returns.
func isChatCompletion(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions")
}

func TestAFencedAttemptThatIsUsedIsRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveReply(w, "````python\nprint(1)\n````")
	}))
	defer srv.Close()
	ctx, fe := fencedEventCtx(t, srv.URL)
	if _, err := fetchFencedContent(ctx, fencedEventCall, "app.py"); err != nil {
		t.Fatal(err)
	}
	evs := fe.all()
	if len(evs) != 1 {
		t.Fatalf("%d fenced_fetch events, want 1: %v", len(evs), evs)
	}
	e := evs[0]
	if e["outcome"] != "used" || e["grammar"] != "fence" || e["attempt"] != float64(1) || e["cut"] != nil {
		t.Errorf("event = %v, want attempt 1, fence, used, no cut", e)
	}
	if n, _ := e["content_chars"].(float64); n == 0 {
		t.Errorf("no content recorded: %v", e)
	}
}

// The fence attempt streams the file and cannot close, and the raw retry
// reasons without sending content.
func TestASilentRetryIsRecordedWithWhatTheWireShowed(t *testing.T) {
	t.Setenv("ATLAS_FENCED_FIRST_CONTENT_SEC", "5")
	t.Setenv("ATLAS_FENCED_IDLE_SEC", "1")
	t.Setenv("ATLAS_FENCED_STALL_SEC", "1")
	stop := make(chan struct{})
	defer close(stop)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isChatCompletion(r) {
			http.NotFound(w, r)
			return
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, withGrammar := body["grammar"]; withGrammar {
			// The file, a closer the grammar did not end on, and the model's
			// next call on the same line: nothing the fetch can salvage.
			serveStuckStream(w, r, stop, "````python\n", "print(1)\n", "```{\"type\":\"done\"}\n")
			return
		}
		serveReasoningOnly(w, r, stop)
	}))
	defer srv.Close()
	ctx, fe := fencedEventCtx(t, srv.URL)
	if _, err := fetchFencedContent(ctx, fencedEventCall, "app.py"); err == nil {
		t.Fatal("the fetch succeeded")
	}
	evs := fe.all()
	if len(evs) != 2 {
		t.Fatalf("%d fenced_fetch events, want 2: %v", len(evs), evs)
	}
	first, retry := evs[0], evs[1]
	if n, _ := first["content_chars"].(float64); first["grammar"] != "fence" ||
		first["outcome"] != "unusable" || first["cut"] != "idle" || n == 0 {
		t.Errorf("attempt 1 = %v, want fence, unusable, cut idle, with content", first)
	}
	content, _ := retry["content_chars"].(float64)
	reasoning, _ := retry["reasoning_chars"].(float64)
	if retry["grammar"] != "raw" || retry["outcome"] != "stalled" || retry["cut"] != "stalled" ||
		content != 0 || reasoning == 0 {
		t.Errorf("attempt 2 = %v, want raw, stalled, cut stalled, no content, some reasoning", retry)
	}
	if ctx.FencedStalls != 1 {
		t.Errorf("FencedStalls = %d, want 1", ctx.FencedStalls)
	}
}

// A server that never answers is cut by the first-content watchdog, and the
// event says no frame came.
func TestAFetchWithNoResponseIsRecorded(t *testing.T) {
	t.Setenv("ATLAS_FENCED_FIRST_CONTENT_SEC", "1")
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isChatCompletion(r) {
			http.NotFound(w, r)
			return
		}
		// The server sees a client leave only once the body is read.
		io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	defer srv.Close()
	defer close(stop)
	ctx, fe := fencedEventCtx(t, srv.URL)
	fetchFencedContent(ctx, fencedEventCall, "app.py")
	evs := fe.all()
	if len(evs) != 1 {
		t.Fatalf("%d events, want 1 (the stall turns the channel off): %v", len(evs), evs)
	}
	if e := evs[0]; e["outcome"] != "stalled" || e["cut"] != "first_content" || e["wire_lines"] != float64(0) {
		t.Errorf("event = %v, want stalled, cut first_content, no wire lines", e)
	}
}

// The refusal that turns the channel off says where the stall was: this
// file's own fetch, or an earlier one.
func TestTheChannelOffRefusalSaysWhereTheStallWas(t *testing.T) {
	t.Setenv("ATLAS_FENCED_FIRST_CONTENT_SEC", "5")
	t.Setenv("ATLAS_FENCED_STALL_SEC", "1")
	stop := make(chan struct{})
	defer close(stop)
	srv := fencedLoopStub(t, stop)
	defer srv.Close()
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.MaxTurns = 2
	ctx.Ctx = context.Background()

	errs := writeFileErrors(ctx, "Create solve.py.")
	if len(errs) < 2 {
		t.Fatalf("%d write results, want 2: %q", len(errs), errs)
	}
	if !strings.Contains(errs[0], "stalled on this file just now") {
		t.Errorf("the stall in this call is not named as this call's: %q", errs[0])
	}
	if !strings.Contains(errs[1], "stalled earlier in this run") {
		t.Errorf("a later write does not say the stall was earlier: %q", errs[1])
	}
}

// A request that fails before any response (here, a refused connection) has
// no stream of its own: its event must not carry the stats of the call
// before it.
func TestAFailedRequestCarriesNoStaleStats(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	ctx, fe := fencedEventCtx(t, url)
	ctx.LastFencedStream = fencedStreamStats{WireLines: 7, ReasoningChars: 90, Cut: "idle"}
	fetchFencedContent(ctx, fencedEventCall, "app.py")
	evs := fe.all()
	if len(evs) == 0 {
		t.Fatal("no fenced_fetch event")
	}
	if e := evs[0]; e["wire_lines"] != float64(0) || e["reasoning_chars"] != float64(0) || e["cut"] != nil {
		t.Errorf("event = %v, carries the previous call's stream", e)
	}
}

// The fence grammar reserves four backticks for the closer (fenceBlockGrammar),
// and the model closes code with three, which the grammar takes as a body
// line. A grammar-constrained attempt then cannot end on its own: the fenced
// watchdog must cut it, and a retry without the grammar lands the file.

// serveStuckStream streams parts, then holds the stream open with frames that
// carry no content until the client gives up.
func serveStuckStream(w http.ResponseWriter, r *http.Request, stop <-chan struct{}, parts ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	frame := func(delta map[string]string) {
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{{"delta": delta}},
		})
		fmt.Fprintf(w, "data: %s\n\n", d)
		if fl != nil {
			fl.Flush()
		}
	}
	for _, part := range parts {
		frame(map[string]string{"content": part})
	}
	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-stop:
			return
		case <-deadline:
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		default:
		}
		frame(map[string]string{})
		time.Sleep(20 * time.Millisecond)
	}
}

// serveReasoningOnly streams reasoning_content frames and no content until
// the client gives up.
func serveReasoningOnly(w http.ResponseWriter, r *http.Request, stop <-chan struct{}) {
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-stop:
			return
		default:
		}
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{{"delta": map[string]string{"reasoning_content": "thinking "}}},
		})
		fmt.Fprintf(w, "data: %s\n\n", d)
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// serveReply streams body as one content frame and ends the stream.
func serveReply(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/event-stream")
	d, _ := json.Marshal(map[string]interface{}{
		"choices": []map[string]interface{}{{"delta": map[string]string{"content": body}}},
	})
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
}

func TestAGrammarConstrainedFencedAttemptIsWatched(t *testing.T) {
	t.Setenv("ATLAS_FENCED_FIRST_CONTENT_SEC", "5")
	t.Setenv("ATLAS_FENCED_IDLE_SEC", "1")
	stop := make(chan struct{})
	defer close(stop)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The block closed the way the model closes it (three backticks),
		// then frames with no content.
		serveStuckStream(w, r, stop, "````python\n", "print(1)\n", "```\n")
	}))
	defer srv.Close()

	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.Ctx = context.Background()
	start := time.Now()
	out, _, err := callLLMOnceWithGrammar(ctx, []AgentMessage{{Role: "user", Content: "x"}},
		0.2, fenceBlockGrammar("python"))
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("the grammar-constrained attempt was not watched: %v", el)
	}
	if err == nil {
		t.Error("the stuck attempt ended without a cut")
	}
	if !strings.Contains(out, "print(1)") {
		t.Errorf("the content written before the cut was lost: %q", out)
	}
}

// End to end: the grammar attempt is cut before the block closes, the cut is
// not a stall (the model was writing), and the retry without the grammar
// lands the file.
func TestAnUnclosedFenceIsRetriedWithoutTheGrammar(t *testing.T) {
	t.Setenv("ATLAS_FENCED_FIRST_CONTENT_SEC", "5")
	t.Setenv("ATLAS_FENCED_IDLE_SEC", "1")
	stop := make(chan struct{})
	defer close(stop)
	var mu sync.Mutex
	var grammarUsed []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isChatCompletion(r) {
			http.NotFound(w, r)
			return
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, withGrammar := body["grammar"]
		mu.Lock()
		grammarUsed = append(grammarUsed, withGrammar)
		mu.Unlock()
		if withGrammar {
			serveStuckStream(w, r, stop, "````python\n", "print(1)\n")
			return
		}
		serveReply(w, "````python\nprint(1)\n````")
	}))
	defer srv.Close()

	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.Ctx = context.Background()
	start := time.Now()
	content, err := fetchFencedContent(ctx,
		`{"type":"tool_call","name":"write_file","args":{"path":"app.py","content":"@fenced"}}`, "app.py")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("the file was not fetched: %v", err)
	}
	if strings.TrimSpace(content) != "print(1)" {
		t.Errorf("content %q, want print(1)", content)
	}
	if elapsed > 15*time.Second {
		t.Errorf("the fetch took %v", elapsed)
	}
	if ctx.FencedStalls != 0 {
		t.Errorf("a cut after content counted as a stall (%d): the channel would be off for the session",
			ctx.FencedStalls)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(grammarUsed) != 2 || !grammarUsed[0] || grammarUsed[1] {
		t.Errorf("attempts used the grammar as %v, want [true false]", grammarUsed)
	}
}

// fencedLoopStub is the model and services for a session whose one write is
// "@fenced": every turn asks to write solve.py with the sentinel, and the
// fenced sub-call reasons forever (it never sends the file).
func fencedLoopStub(t *testing.T, stop <-chan struct{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "single fenced block") {
			serveReasoningOnly(w, r, stop)
			return
		}
		call, _ := json.Marshal(map[string]interface{}{
			"type": "tool_call", "name": "write_file",
			"args": map[string]string{"path": "solve.py", "content": "@fenced"},
		})
		serveReply(w, string(call))
	}))
}

// writeFileErrors runs the agent loop on task and returns the error text of
// each write_file result, in order.
func writeFileErrors(ctx *AgentContext, task string) []string {
	var mu sync.Mutex
	var errs []string
	ctx.StreamFn = func(et string, data interface{}) {
		m, ok := data.(map[string]interface{})
		if et != "tool_result" || !ok || m["tool"] != "write_file" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		e, _ := m["error"].(string)
		errs = append(errs, e)
	}
	runAgentLoop(ctx, task)
	mu.Lock()
	defer mu.Unlock()
	return errs
}

// The session stops the fetch: it is cancelled while the file is being
// fetched, or too little session time is left to fetch the file and check it.
// The model sent nothing wrong, so the write result gives that reason, not
// "no fenced block followed", and no file is written.
func TestAFetchStoppedByTheSessionSaysWhy(t *testing.T) {
	t.Setenv("ATLAS_FENCED_FIRST_CONTENT_SEC", "30")
	for _, tc := range []struct {
		name        string
		maxTurns    int
		cancelAfter time.Duration // cancel the session after this long
		timeout     time.Duration // or give the session this deadline
		want        string
	}{
		{"cancelled during the fetch", 4, 1500 * time.Millisecond, 0, "was cancelled"},
		{"too little time left", 2, 0, 8 * time.Second, "too little session time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stop := make(chan struct{})
			defer close(stop)
			srv := fencedLoopStub(t, stop)
			defer srv.Close()

			dir := t.TempDir()
			ctx := NewAgentContext(dir, Tier2Medium)
			ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
			ctx.PermissionMode = PermissionYolo
			ctx.MaxTurns = tc.maxTurns
			var session context.Context
			var cancel context.CancelFunc
			if tc.timeout > 0 {
				session, cancel = context.WithTimeout(context.Background(), tc.timeout)
			} else {
				session, cancel = context.WithCancel(context.Background())
				time.AfterFunc(tc.cancelAfter, cancel)
			}
			defer cancel()
			ctx.Ctx = session

			errs := writeFileErrors(ctx, "Create solve.py.")
			if len(errs) == 0 {
				t.Fatal("no write_file result")
			}
			if strings.Contains(errs[0], "no fenced block followed") || !strings.Contains(errs[0], tc.want) {
				t.Errorf("the write result does not give the true reason: %q", errs[0])
			}
			if _, err := os.Stat(filepath.Join(dir, "solve.py")); err == nil {
				t.Error("solve.py was written")
			}
		})
	}
}

// The server owns each session's budget: a work deadline, then a reserve in
// which the loop still ends with a terminal. The loop tests below give the
// work a short deadline, so it passes inside production code.

func TestSessionBudgetBounds(t *testing.T) {
	for _, c := range []struct {
		raw       string
		wantTotal time.Duration
	}{
		{"", 600 * time.Second},
		{"abc", 600 * time.Second},
		{"0", 600 * time.Second},
		{"-1", 600 * time.Second},
		{"119", 600 * time.Second},
		{"3601", 600 * time.Second},
		{"999999", 600 * time.Second},
		{"120", 120 * time.Second},
		{"300", 300 * time.Second},
		{"3600", 3600 * time.Second},
	} {
		t.Run("override="+c.raw, func(t *testing.T) {
			t.Setenv("ATLAS_AGENT_SESSION_TIMEOUT_SEC", c.raw)
			total, reserve := sessionBudget()
			if total != c.wantTotal {
				t.Errorf("total = %v, want %v", total, c.wantTotal)
			}
			if reserve != 30*time.Second {
				t.Errorf("reserve = %v, want 30s", reserve)
			}
			if total-reserve <= 0 {
				t.Error("the work deadline is not positive")
			}
		})
	}
}

// timeoutFixture drives a real agent loop whose work context is already
// expiring, so the deadline lands inside production code rather than being
// simulated. hang controls where: mid-LLM-stream or mid-tool-call.
func timeoutFixture(t *testing.T, dir string, hang string) (*AgentContext, map[string]string, map[string]int) {
	t.Helper()
	stop := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/shell") {
			// A tool call that never returns until the request is cancelled.
			if hang == "tool" {
				select {
				case <-r.Context().Done():
				case <-stop:
				}
				http.Error(w, "cancelled", http.StatusGatewayTimeout)
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"stdout": "", "stderr": "", "exit_code": 0})
			return
		}
		if serveLoopServices(w, r, dir) {
			return
		}
		if hang == "llm" {
			// Stream forever: the deadline has to be what stops this.
			serveDotsForever(w, r, stop)
			return
		}
		call, _ := json.Marshal(map[string]interface{}{
			"type": "tool_call", "name": "run_command",
			"args": map[string]string{"command": "sleep 60"}})
		serveReply(w, string(call))
	}))
	// Release the hanging handlers before the server waits for them:
	// httptest.Server.Close blocks until every handler has returned, and the
	// "tool" handler returns only when its request dies or `stop` closes.
	// Cleanups run last-registered-first, so a `stop` registered on its own
	// before the server would close only after Close had blocked.
	t.Cleanup(func() {
		close(stop)
		srv.Close()
	})

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.V3URL = srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.MaxTurns = 20

	// The two lifetimes the handler builds, with a short work deadline
	// instead of ten minutes.
	reqCtx, cancelReq := context.WithCancel(context.Background())
	t.Cleanup(cancelReq)
	workCtx, cancelWork := context.WithTimeout(reqCtx, 900*time.Millisecond)
	t.Cleanup(cancelWork)
	ctx.RequestCtx = reqCtx
	ctx.Ctx = workCtx
	ctx.cancelWork = cancelWork

	terminal := map[string]string{}
	census := map[string]int{}
	var mu sync.Mutex
	ctx.StreamFn = func(eventType string, data interface{}) {
		mu.Lock()
		defer mu.Unlock()
		census[eventType]++
		if eventType == "done" {
			b, _ := json.Marshal(data)
			json.Unmarshal(b, &terminal)
		}
	}
	return ctx, terminal, census
}

// serveDotsForever streams "." content frames until the client gives up.
func serveDotsForever(w http.ResponseWriter, r *http.Request, stop <-chan struct{}) {
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-stop:
			return
		default:
		}
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": "."}}}})
		fmt.Fprintf(w, "data: %s\n\n", d)
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWorkDeadlineEmitsExactlyOneTimedOutTerminal(t *testing.T) {
	for _, where := range []string{"llm", "tool"} {
		t.Run("deadline during the "+where, func(t *testing.T) {
			dir := t.TempDir()
			ctx, terminal, census := timeoutFixture(t, dir, where)

			if err := runAgentLoop(ctx, "Do something long."); err != nil {
				t.Fatalf("agent loop error: %v", err)
			}

			if terminal["status"] != string(TerminalTimedOut) {
				t.Errorf("status = %q, want timed_out (summary=%q)",
					terminal["status"], terminal["summary"])
			}
			if terminal["reason"] != "work_deadline" {
				t.Errorf("reason = %q", terminal["reason"])
			}
			if census["done"] != 1 {
				t.Errorf("%d terminal events", census["done"])
			}
			if strings.Contains(terminal["summary"], "is on disk and parses") {
				t.Errorf("a timeout implied completion: %q", terminal["summary"])
			}
			t.Logf("terminal=%v", terminal)
		})
	}
}

// The terminal arrives AFTER the work context is dead, which is the whole
// point of the split, while the response lifetime lives on, and well inside
// the reserve.
func TestTheWorkDeadlineEndsTheWorkButNotTheResponse(t *testing.T) {
	for _, where := range []string{"llm", "tool"} {
		t.Run("deadline during the "+where, func(t *testing.T) {
			dir := t.TempDir()
			ctx, _, _ := timeoutFixture(t, dir, where)

			start := time.Now()
			if err := runAgentLoop(ctx, "Do something long."); err != nil {
				t.Fatalf("agent loop error: %v", err)
			}
			elapsed := time.Since(start)

			if ctx.Ctx.Err() == nil {
				t.Error("work context outlived the deadline")
			}
			if ctx.RequestCtx.Err() != nil {
				t.Error("the response lifetime died with the work")
			}
			if elapsed > 20*time.Second {
				t.Errorf("finalisation took %v", elapsed)
			}
			t.Logf("elapsed=%v", elapsed)
		})
	}
}

// A timeout restores a checkpoint only by the usual rules: a checkpoint that
// is shown to be safer is put back, and anything short of that is left alone.
// Whatever is on disk, a timeout is never a completion.
func TestTimeoutRestorationObeysTheSameEligibilityRules(t *testing.T) {
	for _, c := range []struct {
		name      string
		seed      func(t *testing.T, ctx *AgentContext, dir string)
		wantBytes string
	}{
		{name: "demonstrably safer checkpoint is restored",
			seed: func(t *testing.T, ctx *AgentContext, dir string) {
				args, _ := json.Marshal(map[string]string{
					"path": "solve.py", "content": "def f():\n    return [1]\n"})
				executeToolCall("write_file", args, ctx)
				os.WriteFile(filepath.Join(dir, "solve.py"),
					[]byte("def f():\n    return [1]]\n"), 0o644)
			},
			wantBytes: "def f():\n    return [1]\n"},
		{name: "invalid bytes with no checkpoint are kept",
			seed: func(t *testing.T, ctx *AgentContext, dir string) {
				// Written broken from the start, so nothing was ever valid.
				args, _ := json.Marshal(map[string]string{
					"path": "solve.py", "content": "def f():\n    return [1]]\n"})
				executeToolCall("write_file", args, ctx)
			},
			wantBytes: "def f():\n    return [1]]\n"},
		{name: "unknown validation is not a reason to act",
			seed: func(t *testing.T, ctx *AgentContext, dir string) {
				args, _ := json.Marshal(map[string]string{
					"path": "solve.py", "content": "def f():\n    return [1]\n"})
				executeToolCall("write_file", args, ctx)
				os.WriteFile(filepath.Join(dir, "solve.py"),
					[]byte("def f():\n    return [2]\n"), 0o644)
				// Checker gone: the current bytes have no verdict at all.
				ctx.SandboxURL = "http://127.0.0.1:1"
			},
			wantBytes: "def f():\n    return [2]\n"},
		{name: "a valid file the session never wrote is left alone",
			seed: func(t *testing.T, ctx *AgentContext, dir string) {
				os.WriteFile(filepath.Join(dir, "solve.py"), []byte("A = 1\n"), 0o644)
			},
			wantBytes: "A = 1\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			ctx, terminal, _ := timeoutFixture(t, dir, "llm")
			c.seed(t, ctx, dir)

			if err := runAgentLoop(ctx, "Do something long."); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
			if string(got) != c.wantBytes {
				t.Errorf("disk = %q, want %q", got, c.wantBytes)
			}
			if terminal["status"] != string(TerminalTimedOut) {
				t.Errorf("status = %q, want timed_out", terminal["status"])
			}
		})
	}
}

// When a timeout puts back a demonstrably safer checkpoint, the terminal says
// so, and the ledger describes the bytes it put back.
func TestATimeoutRestoreIsDisclosedAndInTheLedger(t *testing.T) {
	dir := t.TempDir()
	ctx, terminal, _ := timeoutFixture(t, dir, "llm")
	args, _ := json.Marshal(map[string]string{
		"path": "solve.py", "content": "def f():\n    return [1]\n"})
	executeToolCall("write_file", args, ctx)
	os.WriteFile(filepath.Join(dir, "solve.py"),
		[]byte("def f():\n    return [1]]\n"), 0o644)

	if err := runAgentLoop(ctx, "Do something long."); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
	if !strings.Contains(terminal["summary"], "Put back the last version") {
		t.Errorf("restore was not disclosed: %q", terminal["summary"])
	}
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
	if d == nil || d.CurrentHash != hashBytes(got) {
		t.Error("the ledger does not describe the restored bytes")
	}
}

// The reaper stops this session's jobs and leaves everything else alone.
func TestTimeoutReapsOnlyThisSessionsBackgroundJobs(t *testing.T) {
	var stopped []string
	var mu sync.Mutex
	zero := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stop") {
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/jobs/"), "/stop")
			mu.Lock()
			stopped = append(stopped, id)
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]interface{}{
				"job_id": id, "killed": true, "exit_code": zero,
				"stdout": []string{}, "stderr": []string{}})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.SandboxURL = srv.URL
	ctx.StreamFn = func(string, interface{}) {}
	ctx.BackgroundJobs = map[string]string{
		"mine-1": "python app.py", "mine-2": "npm start",
	}
	// Hazards are owned by the job identity they came from.
	raiseWorkspaceHazard(ctx, "mine-1")
	raiseWorkspaceHazard(ctx, "mine-2")

	reapSessionBackgroundJobs(ctx)

	mu.Lock()
	got := append([]string(nil), stopped...)
	mu.Unlock()
	sort.Strings(got)
	if len(got) != 2 || got[0] != "mine-1" || got[1] != "mine-2" {
		t.Errorf("reaped %v, want exactly this session's two jobs", got)
	}
	if len(ctx.BackgroundJobs) != 0 {
		t.Errorf("%d jobs still tracked after reaping", len(ctx.BackgroundJobs))
	}
	// Confirmed exits clear the hazard, which is what lets restoration run.
	if workspaceHazardous(ctx) {
		t.Error("hazard still raised after two confirmed exits")
	}
}

// A job that cannot be confirmed gone keeps the hazard raised, and the hazard
// is what stops restoration touching a file something may still be writing.
func TestUnconfirmedJobKeepsTheHazardAndBlocksRestore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No exit_code: signalled, not reaped.
		json.NewEncoder(w).Encode(map[string]interface{}{
			"job_id": "j1", "killed": true, "stdout": []string{}, "stderr": []string{}})
	}))
	defer srv.Close()

	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.SandboxURL = srv.URL
	ctx.StreamFn = func(string, interface{}) {}
	ctx.BackgroundJobs = map[string]string{"j1": "python app.py"}
	raiseWorkspaceHazard(ctx, "j1")

	reapSessionBackgroundJobs(ctx)
	if !workspaceHazardous(ctx) {
		t.Fatal("an unconfirmed exit cleared the hazard")
	}
	if len(restoreSaferDeliverables(ctx)) != 0 {
		t.Error("restoration ran while a job may still be writing")
	}
}

// A client that goes away is not a server timeout.
func TestClientDisconnectIsNotATimeout(t *testing.T) {
	dir := t.TempDir()
	ctx, terminal, census := timeoutFixture(t, dir, "llm")
	// Kill the response lifetime first, as a dropped connection does.
	reqCtx, cancelReq := context.WithCancel(context.Background())
	workCtx, cancelWork := context.WithCancel(reqCtx)
	ctx.RequestCtx = reqCtx
	ctx.Ctx = workCtx
	ctx.cancelWork = cancelWork
	cancelReq()

	err := runAgentLoop(ctx, "Do something long.")
	if err == nil {
		t.Error("a disconnect should surface as the context error")
	}
	if terminal["status"] == string(TerminalTimedOut) {
		t.Error("a disconnect was reported as a server timeout")
	}
	if census["done"] != 0 {
		t.Errorf("%d terminal events emitted into a closed response", census["done"])
	}
}

// A caller that sets only ctx.Ctx, with no separate response lifetime, still
// gets one terminal when the work deadline passes, and nothing is left
// running behind it.
func TestASessionThatRunsOutOfTimeStillEndsWithATerminal(t *testing.T) {
	dir := t.TempDir()
	stop := make(chan struct{})
	defer close(stop)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveLoopServices(w, r, dir) {
			return
		}
		serveDotsForever(w, r, stop)
	}))
	defer srv.Close()

	before := runtime.NumGoroutine()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.V3URL = srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.MaxTurns = 20
	workCtx, cancelWork := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancelWork()
	ctx.Ctx = workCtx

	var mu sync.Mutex
	terminals := 0
	var payload map[string]string
	ctx.StreamFn = func(eventType string, data interface{}) {
		if eventType != "done" {
			return
		}
		b, _ := json.Marshal(data)
		mu.Lock()
		terminals++
		json.Unmarshal(b, &payload)
		mu.Unlock()
	}

	start := time.Now()
	runAgentLoop(ctx, "Do something long.")
	elapsed := time.Since(start)

	mu.Lock()
	n, got := terminals, payload
	mu.Unlock()

	if n != 1 {
		t.Fatalf("%d terminal events; a session that runs out of time must "+
			"still tell the client what happened", n)
	}
	if got["status"] != "timed_out" {
		t.Errorf("status = %q, want timed_out", got["status"])
	}
	if elapsed > 20*time.Second {
		t.Errorf("the deadline did not bound the run: %v", elapsed)
	}

	// Nothing left running behind it.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Errorf("goroutines leaked: %d before, %d after", before, after)
	}
	t.Logf("elapsed=%v terminals=%d status=%q", elapsed, n, got["status"])
}

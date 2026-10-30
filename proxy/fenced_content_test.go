package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	for _, f := range []string{"types.go", "tool_io.go", "agent.go"} {
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
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(src), "\n") {
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
	for _, line := range strings.Split(string(src), "\n") {
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
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	// Four, all predating this commit: the changed-rejection-kind reset, the
	// two path-aware-breaker resets, and the one on a successful call.
	resets := strings.Count(string(src), "consecutiveErrors = 0")
	t.Logf("consecutiveErrors reset sites: %d", resets)
	if resets != 4 {
		t.Errorf("%d reset sites; a new one would be an undeclared allowance", resets)
	}
}

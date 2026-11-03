package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A black-box check that a model re-sending a refused write cannot loop past
// the failure ceiling. It drives the real agent loop with a model that makes
// one accepted write, then an invalid rewrite, then the same invalid rewrite
// forever. Without a bound, that sequence is rejected, banned (write_file,
// path), and then bounces off the ban without end.
//
// Nothing here calls shouldStopForFailures or appendRecentFailurePath, and
// nothing restates their conditions. The assertion is on what the loop
// actually does: how many tool calls it streams before it terminates.

// Tier2Medium requires a recognized code extension at >= 10 lines
// (classifyFileTier). Both bodies clear that, so the write takes the V3-gated
// branch rather than the ungated T0/T1 direct path.
const banLoopValidBody = `import sys


def alpha(n):
    if n > 0:
        return n * 2
    return 0


def beta(items):
    total = 0
    for item in items:
        total += alpha(item)
    return total


def main():
    print(beta([1, 2, 3]))


if __name__ == "__main__":
    main()
`

// Same shape, one deliberate syntax error, so the regression gate sees a
// healthy file on disk being replaced by broken content.
const banLoopInvalidBody = `import sys


def alpha(n:
    if n > 0:
        return n * 2
    return 0


def beta(items):
    total = 0
    for item in items:
        total += alpha(item)
    return total


def main():
    print(beta([1, 2, 3]))


if __name__ == "__main__":
    main()
`

func banLoopStubs(t *testing.T, path string, calls *int) *httptest.Server {
	t.Helper()
	write := func(content string) string {
		b, _ := json.Marshal(map[string]interface{}{
			"type": "tool_call", "name": "write_file",
			"args": map[string]string{"path": path, "content": content},
		})
		return string(b)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stand-in sandbox syntax checker: the gate that rejects the invalid
		// rewrite. Python-only is enough for a .py fixture.
		// V3 is required only so the write takes the gated branch; making
		// generation unavailable sends writeFileWithV3 down its fallback,
		// which still lands the baseline bytes. Invalid content never gets
		// this far -- the syntax gate runs before V3.
		if strings.HasPrefix(r.URL.Path, "/v3/") || strings.HasPrefix(r.URL.Path, "/internal/") {
			http.Error(w, "v3 unavailable in this test", http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/syntax-check") {
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			valid := !strings.Contains(in.Code, "def alpha(n:")
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{"SyntaxError: invalid syntax (line 1)"}
			}
			json.NewEncoder(w).Encode(out)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.NotFound(w, r)
			return
		}
		i := *calls
		*calls++
		// Turn 0 lands a good file. Every turn after re-sends the SAME
		// invalid rewrite, which is what produces the rejection, then the
		// ban, then an unbounded run of ban bounces.
		reply := write(banLoopInvalidBody)
		if i == 0 {
			reply = write(banLoopValidBody)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		delta, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": reply}}},
		})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", delta)
	}))
}

// banLoopStream counts what a ban-loop run streams: its tool calls, its
// terminal events and summary, and the gate events of the ban branch and of the
// refused-replacement recovery.
type banLoopStream struct {
	toolCalls   int
	doneEvents  int
	banEntries  int
	recoveries  int
	doneSummary string
}

func (s *banLoopStream) observe(eventType string, data interface{}) {
	switch eventType {
	case "tool_call":
		s.toolCalls++
	case "gate":
		// Proves the ban branch specifically, not merely that some
		// breaker stopped the session.
		b, _ := json.Marshal(data)
		if strings.Contains(string(b), "no longer available") {
			s.banEntries++
		}
		if strings.Contains(string(b), "The working version is still there") {
			s.recoveries++
		}
	case "done":
		s.doneEvents++
		b, _ := json.Marshal(data)
		s.doneSummary = string(b)
	}
}

func TestBannedWriteLoopTerminatesAtTheFailureCeiling(t *testing.T) {
	dir := t.TempDir()
	rel := "mod.py"
	inferenceCalls := 0
	srv := banLoopStubs(t, rel, &inferenceCalls)
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.V3URL = srv.URL
	ctx.PermissionMode = PermissionYolo

	var stream banLoopStream
	ctx.StreamFn = stream.observe

	// A hard stop far above the ceiling. If the loop is unbounded this is
	// what ends the test instead of hanging; exceeding it IS the failure.
	const hardStop = 60
	ctx.MaxTurns = hardStop

	if err := runAgentLoop(ctx, "Create mod.py with VALUE = 1."); err != nil {
		t.Fatalf("agent loop returned an error: %v", err)
	}

	t.Logf("tool_calls=%d ban_entries=%d recoveries=%d done_events=%d inference_calls=%d ceiling=%d summary=%s",
		stream.toolCalls, stream.banEntries, stream.recoveries, stream.doneEvents, inferenceCalls, maxTotalFailures, stream.doneSummary)

	// Routing precondition. If a future tier threshold or V3 setup routes
	// around the intended branch this fails loudly instead of silently
	// measuring a different breaker.
	if tier := classifyFileTier(rel, banLoopValidBody); tier < Tier2Medium {
		t.Fatalf("fixture no longer classifies Tier2Medium (got %v); the write "+
			"would take the ungated direct path and this test would not "+
			"exercise the ban branch", tier)
	}
	// A valid file on disk, then the same invalid rewrite re-sent against it:
	// the refused-replacement recovery answers the re-send before the ban
	// withdraws the tool. Either may answer; the property here is that the
	// loop stops. TestBannedEditLoopEntersTheBanBranch covers the ban branch
	// itself, on a shape the recovery does not own.
	if stream.recoveries == 0 && stream.banEntries == 0 {
		t.Fatal("neither the refused-replacement recovery nor the ban branch " +
			"answered the re-send, so this run measured some other breaker")
	}

	if stream.toolCalls >= hardStop {
		t.Fatalf("loop never terminated on its own: %d tool calls (hard stop %d); "+
			"the ban branch is counting failures without reading the ceiling",
			stream.toolCalls, hardStop)
	}
	// One accepted write plus at most `maxTotalFailures` rejected ones, with a
	// small margin for the turn that emits the terminal event.
	if limit := maxTotalFailures + 3; stream.toolCalls > limit {
		t.Fatalf("loop ran %d tool calls, past the failure ceiling %d (limit %d)",
			stream.toolCalls, maxTotalFailures, limit)
	}
	if stream.doneEvents == 0 {
		t.Fatal("loop ended without streaming a terminal done event")
	}

	// The accepted turn-0 bytes must still be the ones on disk: every later
	// write was invalid and must have been refused, not landed.
	got, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("accepted artifact missing: %v", err)
	}
	if string(got) != banLoopValidBody {
		t.Fatalf("accepted artifact was overwritten: %q", string(got))
	}
}

func banEditLoopStubs(t *testing.T) *httptest.Server {
	t.Helper()
	calls := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v3/") || strings.HasPrefix(r.URL.Path, "/internal/") {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/syntax-check") {
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.NotFound(w, r)
			return
		}
		io.Copy(io.Discard, r.Body) // the whole request first, as a real model server reads it
		i := calls
		calls++
		var reply string
		if i == 0 {
			b, _ := json.Marshal(map[string]interface{}{
				"type": "tool_call", "name": "read_file",
				"args": map[string]string{"path": "mod.py"}})
			reply = string(b)
		} else {
			// old_str is not in the file, and never will be. Not a no-op, not
			// a write_file proposal: nothing but the ban answers this.
			b, _ := json.Marshal(map[string]interface{}{
				"type": "tool_call", "name": "edit_file",
				"args": map[string]string{"path": "mod.py",
					"old_str": "def gamma(q):", "new_str": "def gamma(r):"}})
			reply = string(b)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		delta, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": reply}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", delta)
	}))
}

// The ban branch itself, on a shape the refused-replacement recovery does not
// own: an edit_file rejected for a reason that is not a no-op and not a
// write_file proposal, re-sent unchanged. The ban must count failures
// against the ceiling wherever it is the branch that answers.
func TestBannedEditLoopEntersTheBanBranch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mod.py"),
		[]byte(banLoopValidBody), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := banEditLoopStubs(t)
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	var stream banLoopStream
	ctx.StreamFn = stream.observe
	const hardStop = 60
	ctx.MaxTurns = hardStop
	if err := runAgentLoop(ctx, "Rename gamma's parameter in mod.py."); err != nil {
		t.Fatalf("agent loop returned an error: %v", err)
	}
	t.Logf("tool_calls=%d ban_entries=%d done_events=%d ceiling=%d",
		stream.toolCalls, stream.banEntries, stream.doneEvents, maxTotalFailures)

	if stream.banEntries == 0 {
		t.Fatal("never entered the ban branch: no gate event said the tool was " +
			"no longer available, so this run measured some other breaker")
	}
	if stream.toolCalls >= hardStop {
		t.Fatalf("loop never terminated on its own: %d tool calls (hard stop %d); "+
			"the ban branch is counting failures without reading the ceiling",
			stream.toolCalls, hardStop)
	}
	if limit := maxTotalFailures + 3; stream.toolCalls > limit {
		t.Errorf("%d tool calls for a ceiling of %d (limit %d)",
			stream.toolCalls, maxTotalFailures, limit)
	}
	if stream.doneEvents != 1 {
		t.Errorf("%d terminal events", stream.doneEvents)
	}
	// The file is untouched: every edit was refused before disk.
	if got, _ := os.ReadFile(filepath.Join(dir, "mod.py")); string(got) != banLoopValidBody {
		t.Errorf("a refused edit reached disk: %q", got)
	}
}

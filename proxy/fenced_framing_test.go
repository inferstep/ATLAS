package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// writeStubSSE emits one content delta plus a usage frame, the shape the
// real inference stream uses.
func writeStubSSE(w http.ResponseWriter, content string, tokens int) {
	w.Header().Set("Content-Type", "text/event-stream")
	delta, _ := json.Marshal(map[string]interface{}{
		"choices": []map[string]interface{}{
			{"delta": map[string]string{"content": content}}},
	})
	usage, _ := json.Marshal(map[string]interface{}{
		"choices": []interface{}{},
		"usage":   map[string]int{"total_tokens": tokens},
	})
	fmt.Fprintf(w, "data: %s\n\ndata: %s\n\ndata: [DONE]\n\n", delta, usage)
}

// Framing, not language.
//
// "@fenced" routes a file body around the JSON channel. The parent then has
// to decide whether what came back is a whole file. It used to decide that
// from one signal -- an opening fence with no closing fence -- and treat
// everything else as complete. Everything else includes a bare body with no
// fence at all, which carries no framing evidence in either direction.
//
// Measured on the sealed Stage-A acquisition: of 210 inline payloads, 168
// were the sentinel alone and 42 inlined a body. ZERO of those 42 opened a
// fence. The guard that existed could not fire, and the shape that actually
// occurred was accepted unconditionally -- including one that stopped
// mid-emission and landed 1106 bytes that parse cleanly and do nothing.
//
// Nothing below reads the payload as source. No test here depends on a
// language, a comment, a final line, indentation, a token count, or any
// benchmark task.

// --- the classifier both paths share --------------------------------------

func TestFramingIsDecidedByFencesAlone(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    fenceFraming
		body    string
	}{
		{"complete block", "```lang\nBODY\n```", fenceFramingComplete, "BODY\n"},
		{"complete bare-tag block", "```\nBODY\n```", fenceFramingComplete, "BODY\n"},
		{"opener then body, never closed", "```lang\nBODY", fenceFramingUnterminated, ""},
		{"opener only", "```lang", fenceFramingUnterminated, ""},
		{"closing fence only", "BODY\n```", fenceFramingUnterminated, ""},
		{"no fence at all", "BODY\n", fenceFramingAbsent, ""},
		{"empty", "", fenceFramingAbsent, ""},
		{"whitespace only", "   \n\t\n", fenceFramingAbsent, ""},
		{"fenced but empty body", "```lang\n\n```", fenceFramingUnterminated, ""},
	}
	for _, c := range cases {
		got, body := classifyFencedPayload(c.payload)
		if got != c.want {
			t.Errorf("%s: framing %v, want %v", c.name, got, c.want)
		}
		if body != c.body {
			t.Errorf("%s: body %q, want %q", c.name, body, c.body)
		}
		if got != fenceFramingComplete && body != "" {
			t.Errorf("%s: returned a body without proving completion", c.name)
		}
	}
}

// The whole point: content that looks unfinished but is properly framed is a
// FILE, and content that looks finished but is unframed is not evidence.
func TestFramingIgnoresWhatTheBodyLooksLike(t *testing.T) {
	// Framed, and the last line is a comment, a dangling clause, an open
	// bracket -- all complete, because the fence closed.
	for _, body := range []string{
		"x = 1\n# trailing note about the rule",
		"prose that simply stops mid senten",
		"data = [\n  1,\n  2,",
		"こんにちは",
	} {
		framing, got := classifyFencedPayload("```lang\n" + body + "\n```")
		if framing != fenceFramingComplete {
			t.Errorf("framed payload rejected: %q -> %v", body, framing)
		}
		if got != body+"\n" {
			t.Errorf("framed payload altered: got %q want %q", got, body+"\n")
		}
	}
	// Unframed, and the body looks like a finished program. Still not
	// evidence of completion.
	for _, body := range []string{
		"x = 1\nprint(x)\n",
		"COMPLETE\n",
	} {
		if framing, _ := classifyFencedPayload(body); framing != fenceFramingAbsent {
			t.Errorf("unframed payload accepted: %q -> %v", body, framing)
		}
	}
}

// A file that legitimately contains fences must never be cut at an interior
// one. Under a ``` wrapper an interior closing fence makes the reply ambiguous
// (it is also the shape of a file plus a second block), so nothing is returned;
// under a ```` wrapper the whole body is the file.
func TestFramingKeepsInteriorFences(t *testing.T) {
	body := "# doc\n```lang\ninner\n```\nmore\n"
	if framing, got := classifyFencedPayload("```markdown\n" + body + "```"); framing != fenceFramingAmbiguous || got != "" {
		t.Fatalf("``` wrapper: framing %v body %q, want ambiguous and no bytes", framing, got)
	}
	framing, got := classifyFencedPayload("````markdown\n" + body + "````")
	if framing != fenceFramingComplete || got != body {
		t.Fatalf("```` wrapper: framing %v body %q, want the whole body", framing, got)
	}
	if !strings.Contains(fencedRetryNote(fenceFramingAmbiguous, "markdown"), "four backticks") {
		t.Fatalf("the ambiguous retry note does not give the four-backtick path")
	}
}

func TestFramingClassificationsAreTruthful(t *testing.T) {
	for _, c := range []struct {
		f    fenceFraming
		want string
	}{
		{fenceFramingComplete, "complete"},
		{fenceFramingUnterminated, "fence opened, never closed"},
		{fenceFramingAbsent, "no fence at all"},
		{fenceFramingAmbiguous, "more than one fence closes the block"},
	} {
		if got := c.f.String(); got != c.want {
			t.Errorf("%v: %q, want %q", int(c.f), got, c.want)
		}
	}
}

// --- inline and fetched must agree ----------------------------------------

// The sub-call already refused everything the classifier calls incomplete.
// This pins that the two paths ask the SAME question, so a later change to
// one cannot drift from the other.
func TestInlineAndFetchedShareFramingSemantics(t *testing.T) {
	payloads := []string{
		"```lang\nBODY\n```",
		"```lang\nBODY",
		"BODY\n",
		"",
	}
	for _, p := range payloads {
		framing, body := classifyFencedPayload(p)
		fetched := extractFencedContent(p)
		if (framing == fenceFramingComplete) != (fetched != "") {
			t.Errorf("%q: inline says %v, fetched-path extraction says %q",
				p, framing, fetched)
		}
		if framing == fenceFramingComplete && body != fetched {
			t.Errorf("%q: inline body %q != fetched body %q", p, body, fetched)
		}
	}
}

// --- the failure class, end to end ----------------------------------------

// stubMarkerModel serves replies chosen by what the request contains, so the
// test can prove the model only corrects itself AFTER it receives the
// bounded recovery -- not merely that a retry happened.
func stubMarkerModel(t *testing.T, marker, before, after string, seen *int, sawMarker *bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.NotFound(w, r)
			return
		}
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
		}
		*seen++
		reply := before
		if strings.Contains(string(body), marker) {
			*sawMarker = true
			reply = after
		}
		writeStubSSE(w, reply, 10)
	}))
}

// An unframed body that stops mid-emission must never be handed on as a
// file. This is the Stage-A class, stated without any of its content.
func TestUnframedInlineBodyIsNotAcceptedAsAFile(t *testing.T) {
	// Bytes chosen to be inert: no language, no comment, no syntax.
	truncated := "AAAA\nBBBB\nCCC"
	framing, body := classifyFencedPayload(truncated)
	if framing != fenceFramingAbsent {
		t.Fatalf("an unframed body classified as %v", framing)
	}
	if body != "" {
		t.Fatalf("an unframed body yielded %q; nothing may be handed on without framing", body)
	}
}

// The same bytes, properly framed, land unchanged.
func TestTheSameBytesFramedDoLand(t *testing.T) {
	truncated := "AAAA\nBBBB\nCCC"
	framing, body := classifyFencedPayload("```lang\n" + truncated + "\n```")
	if framing != fenceFramingComplete {
		t.Fatalf("framing %v, want complete", framing)
	}
	if body != truncated+"\n" {
		t.Fatalf("body %q, want %q", body, truncated+"\n")
	}
}

// --- the sub-call refuses the same shapes, and is bounded -----------------

// An unframed reply is not a block: the fetch charges the attempt, retries
// within the session allowance, and errors rather than returning bytes.
func TestFetchRefusesAnUnframedReply(t *testing.T) {
	calls := 0
	srv := stubInference(t, []string{"AAAA\nBBBB\nCCC"}, 7, &calls)
	defer srv.Close()
	t.Setenv("ATLAS_LLAMA_URL", srv.URL)

	ctx := &AgentContext{}
	got, err := fetchFencedContent(ctx, `{"type":"tool_call"}`, "a.txt")
	if err == nil {
		t.Fatalf("an unframed reply was accepted as a file: %q", got)
	}
	if got != "" {
		t.Fatalf("bytes returned despite the error: %q", got)
	}
	if calls != maxFencedFailuresPerPath {
		t.Fatalf("attempts %d, want the session allowance %d", calls, maxFencedFailuresPerPath)
	}
	if ctx.FencedCalls != calls {
		t.Fatalf("accounting: %d generations charged for %d calls", ctx.FencedCalls, calls)
	}
}

// The correction must be caused by the bounded recovery, not by any retry.
// The stub only emits a framed file once it has SEEN the sub-call's note, so
// a pass here cannot come from retrying the same request.
func TestCorrectionFollowsTheBoundedRecoveryAndIsCountedOnce(t *testing.T) {
	seen := 0
	sawMarker := false
	// The marker is the sub-call's own instruction, not a task word.
	const marker = "No JSON, no commentary, no partial file."
	srv := stubMarkerModel(t, marker, "AAAA\nBBBB\nCCC", "```lang\nWHOLE\n```", &seen, &sawMarker)
	defer srv.Close()
	t.Setenv("ATLAS_LLAMA_URL", srv.URL)

	ctx := &AgentContext{}
	got, err := fetchFencedContent(ctx, `{"type":"tool_call"}`, "a.txt")
	if err != nil {
		t.Fatalf("the framed reply after the recovery was refused: %v", err)
	}
	if !sawMarker {
		t.Fatal("the model corrected itself without ever receiving the recovery")
	}
	if got != "WHOLE\n" {
		t.Fatalf("body %q, want the framed file", got)
	}
	if seen != 1 {
		t.Fatalf("the recovery was spent %d times, want exactly once", seen)
	}
	if ctx.FencedCalls != seen {
		t.Fatalf("accounting: %d generations charged for %d calls", ctx.FencedCalls, seen)
	}
}

// Ignoring the recovery stays bounded: a model that never frames anything
// cannot spend more than the session allowance.
func TestIgnoringTheRecoveryRemainsBounded(t *testing.T) {
	calls := 0
	srv := stubInference(t, []string{"still unframed"}, 3, &calls)
	defer srv.Close()
	t.Setenv("ATLAS_LLAMA_URL", srv.URL)

	ctx := &AgentContext{}
	if _, err := fetchFencedContent(ctx, `{"type":"tool_call"}`, "a.txt"); err == nil {
		t.Fatal("an endlessly unframed model was not refused")
	}
	if calls > maxFencedFailuresPerPath {
		t.Fatalf("%d attempts exceeded the allowance %d", calls, maxFencedFailuresPerPath)
	}
	// A second write_file for the same path must not restore the allowance.
	before := calls
	if _, err := fetchFencedContent(ctx, `{"type":"tool_call"}`, "a.txt"); err == nil {
		t.Fatal("the refusal did not persist for the path")
	}
	if calls != before {
		t.Fatalf("a new call bought %d more generations", calls-before)
	}
}

// Nothing unframed may reach disk. The write never happens, so whatever was
// there before is still there byte-for-byte.
func TestUnframedResolutionLeavesDiskUnchanged(t *testing.T) {
	calls := 0
	srv := stubInference(t, []string{"AAAA\nBBBB\nCCC"}, 5, &calls)
	defer srv.Close()
	t.Setenv("ATLAS_LLAMA_URL", srv.URL)

	dir := t.TempDir()
	target := dir + "/a.txt"
	const before = "ORIGINAL\n"
	if err := os.WriteFile(target, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := &AgentContext{}
	if _, err := fetchFencedContent(ctx, `{"type":"tool_call"}`, "a.txt"); err == nil {
		t.Fatal("expected a refusal")
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if after := string(raw); after != before {
		t.Fatalf("disk changed: %q -> %q", before, after)
	}
}

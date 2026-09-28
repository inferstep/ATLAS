package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A program run as `prog < data` is verified as a stdin filter, which is not
// how its caller will run it.
//
// Measured on the AoC tasks, whose prompt says the program must read
// input.txt: 7 of 10 failures wrote a stdin-reading program, ran it as
// `python3 solve.py < input.txt`, and got a SUCCESSFUL result — so the model
// had every reason to believe it was finished. The checker then ran
// `python solve.py` with no redirect and got 0. Not one session that
// verified this way passed.
//
// The same model with no shell never produces this shape; it writes code that
// opens the file, because piping is not available to it. The tool is what
// makes the wrong contract reachable, so the harness has to notice.

func TestARedirectIsDetected(t *testing.T) {
	for _, cmd := range []string{
		"python3 solve.py < input.txt",
		"python solve.py <input.txt",
		"./run < data/in.txt",
	} {
		if got := stdinRedirectSource(cmd); got == "" {
			t.Errorf("no redirect detected in %q", cmd)
		}
	}
}

func TestTheRedirectedFileIsNamed(t *testing.T) {
	if got := stdinRedirectSource("python3 solve.py < input.txt"); got != "input.txt" {
		t.Errorf("want input.txt, got %q", got)
	}
}

func TestOrdinaryCommandsAreNotRedirects(t *testing.T) {
	for _, cmd := range []string{
		"python3 solve.py",
		"pytest -q",
		"go build ./...",
		"echo hi > out.txt",
		"python3 - <<'EOF'\nprint(1)\nEOF",
		"diff <(sort a) <(sort b)",
		"",
	} {
		if got := stdinRedirectSource(cmd); got != "" {
			t.Errorf("%q should not read as a stdin redirect, got %q", cmd, got)
		}
	}
}

func TestTheRejectionSaysWhatToDoInstead(t *testing.T) {
	msg := redirectOnlyVerificationMessage("input.txt")
	for _, want := range []string{"input.txt", "standalone", "no `<`", "open"} {
		if !strings.Contains(msg, want) {
			t.Errorf("rejection should mention %q: %s", want, msg)
		}
	}
}

// Widened contract detection: a redirect anywhere in the segment and the
// cat-pipe idiom are the same stdin contract the trailing-only rule caught.
func TestStdinRedirectSourceWiderShapes(t *testing.T) {
	cases := []struct {
		cmd  string
		want string
	}{
		{"python3 solve.py < input.txt > out.txt", "input.txt"},
		{"python3 solve.py <input.txt 2>err.log", "input.txt"},
		{"cat input.txt | python3 solve.py", "input.txt"},
		{"cd /w && python3 solve.py < data.txt", "data.txt"},
		// Not stdin contracts:
		{"python3 solve.py", ""},
		{"python3 solve.py <<EOF\n1 2\nEOF", ""},
		{"diff <(sort a) <(sort b)", ""},
		{"cat notes.txt || echo missing", ""},
	}
	for _, c := range cases {
		if got := stdinRedirectSource(c.cmd); got != c.want {
			t.Errorf("stdinRedirectSource(%q) = %q, want %q", c.cmd, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// The V3 evidence wire contract
// ---------------------------------------------------------------------------

// Cross-language golden fixtures. Every file under
// ../v3-service/testdata/evidence_wire is a complete /v3/generate response
// body produced by the REAL Python serialiser. These tests decode those exact
// bytes; nothing here rebuilds an equivalent struct by hand, because two
// hand-written shapes agreeing with each other proves nothing about the wire.

const evidenceCaseFile = "../v3-service/testdata/evidence_wire_cases.json"

// evidenceCase is one cross-language case: the exact response bytes the
// service produces, and what BOTH sides must conclude from them. The
// expectations are declared by the producer and checked here independently, so
// the two languages agree with the contract rather than with each other.
type evidenceCase struct {
	ID          string          `json:"id"`
	Description string          `json:"description"`
	Response    json.RawMessage `json:"response"`
	Expect      struct {
		Availability                string `json:"availability"`
		EvidenceStrength            string `json:"evidence_strength"`
		SelectionStatus             string `json:"selection_status"`
		DescribesDeliveredCandidate bool   `json:"describes_delivered_candidate"`
		ReasonContains              string `json:"reason_contains"`
	} `json:"expect"`
}

func evidenceCases(t *testing.T) []evidenceCase {
	t.Helper()
	raw, err := os.ReadFile(evidenceCaseFile)
	if err != nil {
		t.Fatalf("golden cases missing (regenerate with "+
			"ATLAS_WRITE_EVIDENCE_FIXTURES=1 pytest "+
			"tests/v3-service/test_contract_genericity.py): %v", err)
	}
	var doc struct {
		Schema string         `json:"schema"`
		Cases  []evidenceCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("golden cases undecodable: %v", err)
	}
	if doc.Schema != "atlas.evidence_wire.cases/1" {
		t.Fatalf("unknown case-document schema %q", doc.Schema)
	}
	if len(doc.Cases) != 13 {
		t.Fatalf("case count = %d, want 13; cross-language coverage shrank",
			len(doc.Cases))
	}
	return doc.Cases
}

func evidenceCaseByID(t *testing.T, id string) evidenceCase {
	t.Helper()
	for _, c := range evidenceCases(t) {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no golden case %q", id)
	return evidenceCase{}
}

func readFixture(t *testing.T, id string) []byte {
	t.Helper()
	return evidenceCaseByID(t, id).Response
}

func decodeFixture(t *testing.T, id string) V3GenerateResponse {
	t.Helper()
	var got V3GenerateResponse
	if err := json.Unmarshal(readFixture(t, id), &got); err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	return got
}

// The regression that exposed the gap. The response type as it stood decoded
// the service's reply happily and dropped the whole envelope on the floor: no
// error, no warning, just absent evidence. Half of this test still describes
// that type, so the loss stays visible rather than becoming folklore.
func TestUnknownFieldsAreLostWithoutAnExplicitWireType(t *testing.T) {
	raw := readFixture(t, "01_verified_winner")

	// The pre-envelope shape, reproduced exactly.
	type legacyResponse struct {
		Code                 string                   `json:"code"`
		Passed               bool                     `json:"passed"`
		PhaseSolved          string                   `json:"phase_solved"`
		CandidatesTested     int                      `json:"candidates_tested"`
		WinningScore         float64                  `json:"winning_score"`
		TotalTokens          int                      `json:"total_tokens"`
		TotalTimeMs          float64                  `json:"total_time_ms"`
		VerificationEvidence []V3VerificationEvidence `json:"verification_evidence"`
	}
	var legacy legacyResponse
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatalf("legacy decode failed: %v", err)
	}
	if !legacy.Passed || legacy.Code == "" {
		t.Fatal("fixture did not carry the fields the legacy type does read")
	}
	round, _ := json.Marshal(legacy)
	if string(round) == string(raw) {
		t.Fatal("fixture does not exercise the loss: nothing was dropped")
	}

	// The explicit wire type keeps it.
	got := decodeFixture(t, "01_verified_winner")
	if got.Evidence == nil {
		t.Fatal("the evidence envelope was dropped by V3GenerateResponse")
	}
	if got.Evidence.Identity.ContractID == "" ||
		got.Evidence.Evaluation.EvidenceStrength == "" ||
		len(got.Evidence.Coverage.Required) == 0 ||
		got.Evidence.Selection.Status == "" ||
		got.Evidence.Delivery.DeliveredContentHash == "" {
		t.Fatalf("envelope decoded with empty sections: %+v", got.Evidence)
	}
}

// Field-by-field equality against the JSON the service produced. A struct tag
// typo drops one field silently, which is exactly the class of bug the whole
// fixture apparatus exists to catch.
func TestEnvelopeSurvivesDecodeWithoutFieldLoss(t *testing.T) {
	raw := readFixture(t, "01_verified_winner")
	var asMap struct {
		Evidence map[string]interface{} `json:"evidence"`
	}
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatal(err)
	}
	got := decodeFixture(t, "01_verified_winner")

	// Re-marshal the decoded struct and compare to the original sub-object.
	reencoded, err := json.Marshal(got.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]interface{}
	if err := json.Unmarshal(reencoded, &back); err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(asMap.Evidence)
	var want map[string]interface{}
	json.Unmarshal(original, &want)

	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(back)
	if string(wantJSON) != string(gotJSON) {
		t.Errorf("envelope changed crossing the boundary:\n got %s\nwant %s",
			gotJSON, wantJSON)
	}
}

// Every golden case, and what a strict consumer must conclude from it. The
// expectations travel with the bytes, so adding a case on the Python side
// automatically binds this side to it.
func TestGoldenCaseAvailability(t *testing.T) {
	for _, c := range evidenceCases(t) {
		c := c
		t.Run(c.ID, func(t *testing.T) {
			var got V3GenerateResponse
			if err := json.Unmarshal(c.Response, &got); err != nil {
				t.Fatalf("%s (%s): %v", c.ID, c.Description, err)
			}
			availability, reason := got.Evidence.Validate()
			if string(availability) != c.Expect.Availability {
				t.Fatalf("availability = %q (%s), want %q -- %s",
					availability, reason, c.Expect.Availability, c.Description)
			}
			if c.Expect.ReasonContains != "" &&
				!strings.Contains(reason, c.Expect.ReasonContains) {
				t.Errorf("reason = %q, want it to name %q", reason, c.Expect.ReasonContains)
			}
			if availability != EvidenceAvailable {
				// Unavailable and absent are never verdicts about the candidate.
				if got.Evidence.Available() {
					t.Error("a non-available envelope must not read as available")
				}
				if reason == "" {
					t.Error("a non-available envelope must say why")
				}
				return
			}
			if got.Evidence.Evaluation.EvidenceStrength != c.Expect.EvidenceStrength {
				t.Errorf("strength = %q, want %q",
					got.Evidence.Evaluation.EvidenceStrength, c.Expect.EvidenceStrength)
			}
			if got.Evidence.Selection.Status != c.Expect.SelectionStatus {
				t.Errorf("selection = %q, want %q",
					got.Evidence.Selection.Status, c.Expect.SelectionStatus)
			}
			if got.Evidence.DescribesBytes(got.Code) != c.Expect.DescribesDeliveredCandidate {
				t.Errorf("DescribesBytes(delivered) = %v, want %v",
					got.Evidence.DescribesBytes(got.Code),
					c.Expect.DescribesDeliveredCandidate)
			}
		})
	}
}

// A producer that predates the envelope decodes cleanly and reports absence --
// not a failure, and not silently "available with zero values".
func TestLegacyResponseDecodesAsAbsentEvidence(t *testing.T) {
	got := decodeFixture(t, "10_legacy_no_envelope")
	if got.Evidence != nil {
		t.Fatal("a legacy response must not synthesise an envelope")
	}
	availability, reason := got.Evidence.Validate()
	if availability != EvidenceAbsent {
		t.Errorf("availability = %q, want absent", availability)
	}
	if reason == "" {
		t.Error("absence must state itself")
	}
	if !got.Passed || got.Code == "" {
		t.Error("the legacy fields must still decode")
	}
}

// Evidence about other bytes may not support provenance for the delivered
// ones. Nothing calls this for a decision yet; the rule is pinned so the later
// slice cannot invent a looser one at the call site.
func TestProvenanceRequiresMatchingHashes(t *testing.T) {
	winner := decodeFixture(t, "01_verified_winner")
	if ok, why := EvidenceSupportsProvenanceFor(winner.Evidence, winner.Code); !ok {
		t.Errorf("matching evidence rejected: %s", why)
	}
	if ok, _ := EvidenceSupportsProvenanceFor(winner.Evidence, "different bytes\n"); ok {
		t.Error("evidence about other bytes supported provenance")
	}

	stale := decodeFixture(t, "09_evidence_for_other_candidate")
	ok, why := EvidenceSupportsProvenanceFor(stale.Evidence, stale.Code)
	if ok {
		t.Error("stale evidence supported provenance for the delivered bytes")
	}
	if why != "evidence describes a different candidate" {
		t.Errorf("reason = %q, want the hash mismatch", why)
	}

	unknown := decodeFixture(t, "11_unknown_wire_version")
	if ok, _ := EvidenceSupportsProvenanceFor(unknown.Evidence, unknown.Code); ok {
		t.Error("an unreadable envelope supported provenance")
	}
}

// Nothing in the envelope is inferred from the legacy fields, and nothing in
// the legacy fields is inferred from the envelope. The two travel side by side
// precisely because `passed` cannot express what the envelope carries.
func TestEnvelopeIsNotDerivedFromLegacyFields(t *testing.T) {
	// A passing legacy response whose evidence is explicitly not closure-eligible.
	got := decodeFixture(t, "09_evidence_for_other_candidate")
	if !got.Passed {
		t.Fatal("fixture must carry passed=true")
	}
	if got.Evidence.Evaluation.EvidenceStrength == "" {
		t.Fatal("fixture must carry a strength")
	}
	// The unsupported fixture is the mirror: legacy passed=false, and the
	// envelope still reports what was measured rather than a failure.
	unsup := decodeFixture(t, "05_unsupported_candidate")
	if unsup.Passed {
		t.Fatal("fixture must carry passed=false")
	}
	if unsup.Evidence.Evaluation.Supported {
		t.Error("fixture must carry supported=false")
	}
	if unsup.Evidence.Evaluation.ExecutionStatus == "error" {
		t.Error("unsupported must not be reported as an execution error")
	}
	if a, _ := unsup.Evidence.Validate(); a != EvidenceAvailable {
		t.Error("an unsupported-but-well-formed envelope is still readable")
	}
}

// End to end: the Python bytes reach telemetry with every field intact, and
// reach the tool result with none of them. The guarded tool-result projection
// is the model's view; research fields that leak there become an interface
// nobody meant to publish.
func TestEnvelopeReachesTelemetryAndNotTheToolResult(t *testing.T) {
	raw := readFixture(t, "01_verified_winner")
	var golden struct {
		Code     string                 `json:"code"`
		Evidence map[string]interface{} `json:"evidence"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}

	// SSE frames are single-line; the committed fixtures are indented for
	// review. Compacting changes no content -- the assertions below compare
	// the decoded structure, not the whitespace.
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		t.Fatal(err)
	}
	raw = compact.Bytes()

	sub := defaultBroker.subscribe()
	defer defaultBroker.unsubscribe(sub)

	dir := t.TempDir()
	var streamed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v3/generate":
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			// The service's own bytes, forwarded verbatim.
			for _, line := range []string{"event: result", "data: " + string(raw), "", "data: [DONE]", ""} {
				fmt.Fprint(w, line+"\n")
				if fl != nil {
					fl.Flush()
				}
			}
		case "/internal/cyclomatic_complexity":
			json.NewEncoder(w).Encode(map[string]interface{}{"functions": []interface{}{}})
		case "/internal/structural_check":
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "unresolved": []string{}})
		default:
			if strings.HasSuffix(r.URL.Path, "/syntax-check") {
				json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
				return
			}
			http.Error(w, "unexpected "+r.URL.Path, http.StatusTeapot)
		}
	}))
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(event string, _ interface{}) { streamed = append(streamed, event) }
	ctx.Ctx = context.Background()
	ctx.V3URL = srv.URL
	ctx.SandboxURL = srv.URL

	res, err := writeFileWithV3(filepath.Join(dir, "game.js"), "const a = 1;\n", ctx)
	if err != nil {
		t.Fatalf("writeFileWithV3: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("delivery failed: %+v", res)
	}

	// --- telemetry: whole envelope, no field loss ---------------------------
	var payload map[string]interface{}
	for {
		select {
		case ev := <-sub:
			if ev.Type == EvtStageEnd && ev.Stage == "v3" {
				payload = ev.Payload
			}
			if payload != nil {
				goto found
			}
			continue
		default:
			goto found
		}
	}
found:
	if payload == nil {
		t.Fatal("no v3 stage_end envelope was emitted")
	}
	evidence, ok := payload["evidence"].(map[string]interface{})
	if !ok {
		t.Fatalf("telemetry carries no evidence section: %v", payload)
	}
	if evidence["availability"] != string(EvidenceAvailable) {
		t.Errorf("availability = %v, want available", evidence["availability"])
	}
	// Re-serialise what telemetry holds and compare it to the Python bytes.
	viaTelemetry, err := json.Marshal(evidence["envelope"])
	if err != nil {
		t.Fatal(err)
	}
	fromPython, _ := json.Marshal(golden.Evidence)
	var a, b map[string]interface{}
	json.Unmarshal(viaTelemetry, &a)
	json.Unmarshal(fromPython, &b)
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if string(aj) != string(bj) {
		t.Errorf("evidence changed between Python and telemetry:\n got %s\nwant %s", aj, bj)
	}

	// --- tool result and SSE: nothing new -----------------------------------
	blob, _ := json.Marshal(res)
	if strings.Contains(string(blob), "wire_version") ||
		strings.Contains(string(blob), "closure_eligible") ||
		strings.Contains(string(blob), "evidence_strength") {
		t.Errorf("the envelope leaked into the tool result: %s", blob)
	}
	// Local validation is untouched by the envelope's arrival.
	if res.ValidationKind != ValidationKindSyntax || res.ValidationStatus != ValidationPassed {
		t.Errorf("local validation = %q/%q, want syntax/passed -- the service's "+
			"evidence must not displace what THIS process checked",
			res.ValidationKind, res.ValidationStatus)
	}
	for _, e := range streamed {
		if e != "v3_progress" && e != "v3_token" && e != "text" {
			t.Errorf("unexpected SSE projection %q", e)
		}
	}
}

// ---------------------------------------------------------------------------
// The envelope authorizes delivery
// ---------------------------------------------------------------------------
//
// `passed` collapses a compile smoke, a partial oracle score and a complete one
// into one boolean. It is kept on the wire for compatibility and telemetry, and
// it authorizes nothing: replacement and provenance require an available,
// self-consistent envelope whose SELECTION concluded a verified winner, whose
// record is closure-eligible, and whose hash names the exact bytes that would
// be written.

// envelopeFor renders the golden verified-winner envelope onto other bytes,
// optionally damaged. The shape stays the one the real serialiser produced.
func envelopeFor(t *testing.T, code string, mutate func(map[string]interface{})) map[string]interface{} {
	t.Helper()
	var payload struct {
		Evidence map[string]interface{} `json:"evidence"`
	}
	if err := json.Unmarshal(readFixture(t, "01_verified_winner"), &payload); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(code))
	h := hex.EncodeToString(sum[:])
	payload.Evidence["identity"].(map[string]interface{})["candidate_content_hash"] = h
	payload.Evidence["delivery"].(map[string]interface{})["delivered_content_hash"] = h
	if mutate != nil {
		mutate(payload.Evidence)
	}
	return payload.Evidence
}

func responseWith(t *testing.T, code string, passed bool,
	envelope map[string]interface{}) *V3GenerateResponse {
	t.Helper()
	body := map[string]interface{}{
		"code": code, "passed": passed, "phase_solved": "phase1",
		"candidates_tested": 3, "winning_score": 0.87,
		"verification_evidence": []map[string]interface{}{
			{"verifier": "sandbox", "status": "passed"}},
	}
	if envelope != nil {
		body["evidence"] = envelope
	}
	raw, _ := json.Marshal(body)
	var out V3GenerateResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

const authCandidate = "def improved():\n    return 2\n"
const authBaseline = "def original():\n    return 1\n"

func TestOnlyAVerifiedEnvelopeAuthorizesDelivery(t *testing.T) {
	for _, c := range []struct {
		name       string
		passed     bool
		mutate     func(map[string]interface{})
		omit       bool
		code       string
		authorized bool
		reason     string
	}{
		{name: "exact verified winner", passed: true, authorized: true},
		{name: "passed with partial evidence", passed: true, mutate: func(e map[string]interface{}) {
			ev := e["evaluation"].(map[string]interface{})
			ev["requirements_complete"] = false
			ev["closure_eligible"] = false
			e["selection"].(map[string]interface{})["status"] = "best_not_closure_eligible"
		}, reason: "selection status best_not_closure_eligible"},
		{name: "passed with best but not closure eligible", passed: true, mutate: func(e map[string]interface{}) {
			e["evaluation"].(map[string]interface{})["closure_eligible"] = false
			e["selection"].(map[string]interface{})["status"] = "best_not_closure_eligible"
		}, reason: "selection status best_not_closure_eligible"},
		{name: "passed with a tied pool", passed: true, mutate: func(e map[string]interface{}) {
			e["selection"].(map[string]interface{})["status"] = "tied"
		}, reason: "selection status tied"},
		{name: "passed with an incomparable pool", passed: true, mutate: func(e map[string]interface{}) {
			e["selection"].(map[string]interface{})["status"] = "incomparable"
		}, reason: "selection status incomparable"},
		{name: "passed with no verified winner", passed: true, mutate: func(e map[string]interface{}) {
			e["selection"].(map[string]interface{})["status"] = "no_verified_winner"
		}, reason: "selection status no_verified_winner"},
		{name: "passed with a hash mismatch", passed: true, mutate: func(e map[string]interface{}) {
			e["identity"].(map[string]interface{})["candidate_content_hash"] =
				"0000000000000000000000000000000000000000000000000000000000000000"
		}, reason: "evidence describes a different candidate"},
		{name: "passed with no envelope at all", passed: true, omit: true,
			reason: "no evidence envelope"},
		{name: "passed with an unknown wire version", passed: true, mutate: func(e map[string]interface{}) {
			e["wire_version"] = "99.0.0"
		}, reason: "unsupported wire version 99.0.0"},
		{name: "passed with a malformed identity", passed: true, mutate: func(e map[string]interface{}) {
			e["identity"].(map[string]interface{})["evaluation_context_hash"] = ""
		}, reason: "identity incomplete"},
		{name: "passed with closure contradicting execution", passed: true, mutate: func(e map[string]interface{}) {
			e["evaluation"].(map[string]interface{})["execution_status"] = "timeout"
		}, reason: "closure claimed over execution status timeout"},
		// The envelope is authoritative in BOTH directions: a service that
		// verified a winner has said so, whatever the legacy boolean carries.
		{name: "not passed with a verified envelope", passed: false, authorized: true},
		{name: "verified envelope with empty code", passed: true, code: " ",
			reason: "evidence describes a different candidate"},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			code := authCandidate
			if c.code != "" {
				code = c.code
			}
			var env map[string]interface{}
			if !c.omit {
				env = envelopeFor(t, authCandidate, c.mutate)
			}
			res := responseWith(t, code, c.passed, env)

			got, why := v3DeliveryAuthorized(res, res.Code)
			if got != c.authorized {
				t.Fatalf("authorized = %v (%s), want %v", got, why, c.authorized)
			}
			if !c.authorized && c.reason != "" && why != c.reason {
				t.Errorf("reason = %q, want %q", why, c.reason)
			}
			// The PROPOSAL boundary is a different question, and answers it
			// differently on purpose: bytes materially different from the
			// caller's own are a proposal whatever the service concluded, and
			// what happens to them is decided after they have been staged.
			proposal, proposed := proposedV3Candidate(res, authBaseline)
			if strings.TrimSpace(code) == "" || code == authBaseline {
				if proposed {
					t.Errorf("empty or identical bytes were proposed")
				}
			} else if !proposed || proposal != code {
				t.Errorf("proposal = %q/%v, want the candidate offered", proposal, proposed)
			}
		})
	}
}

// An empty candidate authorizes nothing, whatever the envelope says.
func TestEmptyCandidateIsNeverAuthorized(t *testing.T) {
	res := responseWith(t, "", true, envelopeFor(t, "", nil))
	if ok, why := v3DeliveryAuthorized(res, res.Code); ok {
		t.Fatalf("empty code was authorized: %s", why)
	}
	delivered, proposed := proposedV3Candidate(res, authBaseline)
	if proposed || delivered != authBaseline {
		t.Fatal("empty code was proposed as a candidate")
	}
}

// Source-level proof that the legacy boolean cannot authorize anything: the
// authorization path never reads it.
func TestPassedNoLongerAuthorizesDelivery(t *testing.T) {
	src, err := os.ReadFile("v3_bridge.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	fn := body[strings.Index(body, "func EvidenceSupportsProvenanceFor("):]
	fn = fn[:strings.Index(fn, "\n// v3DeliveryAuthorized")]
	auth := body[strings.Index(body, "func v3DeliveryAuthorized("):]
	auth = auth[:strings.Index(auth, "\n}")]
	for _, banned := range []string{".Passed", ".PhaseSolved", ".WinningScore",
		".VerificationEvidence"} {
		if strings.Contains(fn, banned) || strings.Contains(auth, banned) {
			t.Errorf("authorization reads the legacy field %s", banned)
		}
	}

	tools, err := os.ReadFile("tools.go")
	if err != nil {
		t.Fatal(err)
	}
	// There is no service-certification helper at all any more: a request that
	// declared nothing retains its own bytes rather than delivering on the
	// producer's opinion of them.
	if strings.Contains(string(tools), "func serviceCertifiedCandidate(") {
		t.Error("the self-certification path is back")
	}
	// And the proposal boundary reads no verdict at all: it compares bytes.
	proposal := string(tools)[strings.Index(string(tools), "func proposedV3Candidate("):]
	proposal = proposal[:strings.Index(proposal, "\n}")]
	for _, banned := range []string{"Passed", "Evidence", "ClosureEligible", "Selection"} {
		if strings.Contains(proposal, banned) {
			t.Errorf("the proposal boundary reads the service verdict field %s", banned)
		}
	}
	// The field itself stays on the wire for compatibility and telemetry.
	if !strings.Contains(string(src), "Passed") && !strings.Contains(body, "Passed") {
		t.Log("Passed no longer appears in the bridge at all")
	}
}

// Candidate zero, PR-CoT and refinement winners are deliverable on the same
// terms as any other: the phase name is not consulted, only the evidence.
func TestEveryPhaseIsDeliverableOnTheSameTerms(t *testing.T) {
	for _, phase := range []string{"probe", "phase1", "pr_cot", "refinement", "budget"} {
		phase := phase
		t.Run(phase, func(t *testing.T) {
			env := envelopeFor(t, authCandidate, nil)
			res := responseWith(t, authCandidate, true, env)
			res.PhaseSolved = phase
			if ok, why := v3DeliveryAuthorized(res, res.Code); !ok {
				t.Fatalf("%s winner refused: %s", phase, why)
			}
			// And the same phase without closure is refused, so no phase name
			// buys an exemption.
			weak := envelopeFor(t, authCandidate, func(e map[string]interface{}) {
				e["evaluation"].(map[string]interface{})["closure_eligible"] = false
				e["selection"].(map[string]interface{})["status"] = "best_not_closure_eligible"
			})
			weakRes := responseWith(t, authCandidate, true, weak)
			weakRes.PhaseSolved = phase
			if ok, _ := v3DeliveryAuthorized(weakRes, weakRes.Code); ok {
				t.Fatalf("%s delivered without closure eligibility", phase)
			}
		})
	}
}

// A local syntax or structural pass says what THIS process checked. It cannot
// promote the service's evidence into a verified winner.
func TestLocalValidationCannotUpgradeServiceEvidence(t *testing.T) {
	weak := envelopeFor(t, authCandidate, func(e map[string]interface{}) {
		e["evaluation"].(map[string]interface{})["closure_eligible"] = false
		e["selection"].(map[string]interface{})["status"] = "best_not_closure_eligible"
	})
	res := responseWith(t, authCandidate, true, weak)
	if ok, _ := v3DeliveryAuthorized(res, res.Code); ok {
		t.Fatal("a non-closure record was authorized")
	}
	// The local checkers have no input to this decision at all: the predicate
	// takes a response and bytes, nothing else.
	src, err := os.ReadFile("v3_bridge.go")
	if err != nil {
		t.Fatal(err)
	}
	auth := string(src)[strings.Index(string(src), "func v3DeliveryAuthorized("):]
	auth = auth[:strings.Index(auth, "\n}")]
	for _, banned := range []string{"checkFallbackSyntax", "fallbackSyntaxOutcomeFor",
		"editIntroducesUnresolved", "ValidationPassed"} {
		if strings.Contains(auth, banned) {
			t.Errorf("authorization consults the local checker %s", banned)
		}
	}
}

// A live round trip: the real Python serialiser writes the envelope, HTTP
// carries it, Go decodes it and authorizes on it. Nothing in this test builds
// a Go-side envelope by hand. The record is the stylesheet adapter's: its
// contract floor is syntax, so an accepted stylesheet is a closure-eligible
// verified winner, which no other adapter's record can be.
func TestLivePythonEnvelopeAuthorizesDelivery(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	code := "body { color: red; }\n"
	script := `
import json, sys
sys.path.insert(0, "../v3-service")
import adapters, contract
code = json.loads(sys.stdin.read())["code"]
record = adapters.contract_record(
    adapter=adapters.ADAPTER_CSS_SYNTAX, accepted=True,
    contract_id="generate:css", contract_version="1", artifact_scope="theme.css",
    evaluation_context_hash=contract.content_hash("style it"),
    candidate_content_hash=contract.content_hash(code))
selection = contract.select([record], record)
print(json.dumps({
    "code": code, "passed": False, "phase_solved": "probe",
    "candidates_tested": 1, "winning_score": 1.0,
    "evidence": contract.envelope(record, selection, contract.content_hash(code)),
}))
`
	cmd := exec.Command("python3", "-c", script)
	cmd.Stdin = strings.NewReader(`{"code":` + strconv.Quote(code) + `}`)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python producer failed: %v", err)
	}

	// Serve those exact bytes over HTTP and read them back through the bridge.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		var compact bytes.Buffer
		json.Compact(&compact, out)
		for _, line := range []string{"event: result", "data: " + compact.String(), "", "data: [DONE]", ""} {
			fmt.Fprint(w, line+"\n")
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer srv.Close()

	got, err := callV3GenerateStreaming(context.Background(), srv.URL,
		V3GenerateRequest{FilePath: "theme.css", BaselineCode: "body {}\n"}, nil)
	if err != nil {
		t.Fatalf("bridge call failed: %v", err)
	}
	if got.Evidence == nil {
		t.Fatal("the live envelope did not survive the round trip")
	}
	if availability, why := got.Evidence.Validate(); availability != EvidenceAvailable {
		t.Fatalf("live envelope unusable: %s (%s)", availability, why)
	}
	// Authorized on the envelope alone: the producer set passed=false.
	if got.Passed {
		t.Fatal("fixture must carry passed=false to prove the envelope decides")
	}
	if ok, why := v3DeliveryAuthorized(got, got.Code); !ok {
		t.Fatalf("live verified winner refused: %s", why)
	}
	delivered, proposed := proposedV3Candidate(got, "body {}\n")
	if !proposed || delivered != code {
		t.Fatalf("live winner not proposed: %q proposed=%v", delivered, proposed)
	}
}

// The authorization decision and its reason are recorded where a durable
// record belongs -- telemetry -- and nowhere the model can read them.
func TestAuthorizationReasonIsRecordedInTelemetryOnly(t *testing.T) {
	sub := defaultBroker.subscribe()
	defer defaultBroker.unsubscribe(sub)

	dir := t.TempDir()
	candidate := "def improved():\n    return 2\n"
	baseline := "import math\n\n\ndef area(r):\n    if r < 0:\n        raise ValueError('neg')\n    return math.pi * r * r\n\n\ndef p(r):\n    for _ in range(1):\n        pass\n    return 2 * math.pi * r\n"
	// A best record that is not closure-eligible: passed=true, and nothing is
	// authorized by it.
	env := envelopeFor(t, candidate, func(e map[string]interface{}) {
		e["evaluation"].(map[string]interface{})["closure_eligible"] = false
		e["selection"].(map[string]interface{})["status"] = "best_not_closure_eligible"
	})
	body, _ := json.Marshal(map[string]interface{}{
		"code": candidate, "passed": true, "phase_solved": "phase1",
		"candidates_tested": 3, "winning_score": 0.9, "evidence": env,
	})

	var streamed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v3/generate":
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			for _, line := range []string{"event: result", "data: " + string(body), "", "data: [DONE]", ""} {
				fmt.Fprint(w, line+"\n")
				if fl != nil {
					fl.Flush()
				}
			}
		case "/internal/structural_check":
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "unresolved": []string{}})
		default:
			if strings.HasSuffix(r.URL.Path, "/syntax-check") {
				json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
				return
			}
			http.Error(w, "unexpected "+r.URL.Path, http.StatusTeapot)
		}
	}))
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(event string, _ interface{}) { streamed = append(streamed, event) }
	ctx.Ctx = context.Background()
	ctx.V3URL = srv.URL
	ctx.SandboxURL = srv.URL

	res, err := writeFileWithV3(filepath.Join(dir, "mod.py"), baseline, ctx)
	if err != nil {
		t.Fatalf("writeFileWithV3: %v", err)
	}
	// The baseline is delivered, with no candidate metadata to feed the
	// agent's "V3 verified this edit" nudge.
	onDisk, _ := os.ReadFile(filepath.Join(dir, "mod.py"))
	if string(onDisk) != baseline {
		t.Fatalf("unauthorized candidate reached disk: %q", onDisk)
	}
	if res.V3Used || res.CandidatesTested != 0 || res.WinningScore != 0 ||
		res.PhaseSolved != "" || len(res.VerificationEvidence) != 0 {
		t.Errorf("baseline fallback carries candidate metadata: %+v", res)
	}

	var payload map[string]interface{}
	for done := false; !done; {
		select {
		case ev := <-sub:
			if ev.Type == EvtStageEnd && ev.Stage == "v3" {
				payload = ev.Payload
				done = true
			}
		default:
			done = true
		}
	}
	if payload == nil {
		t.Fatal("no v3 stage_end envelope was emitted")
	}
	auth, ok := payload["authorization"].(map[string]interface{})
	if !ok {
		t.Fatalf("telemetry carries no authorization section: %v", payload)
	}
	if auth["authorized"] != false {
		t.Errorf("authorized = %v, want false", auth["authorized"])
	}
	if reason, _ := auth["reason"].(string); reason != "selection status best_not_closure_eligible" {
		t.Errorf("reason = %q, want the selection status", reason)
	}

	blob, _ := json.Marshal(res)
	for _, leaked := range []string{"authorization", "closure_eligible", "wire_version"} {
		if strings.Contains(string(blob), leaked) {
			t.Errorf("%q leaked into the guarded tool result: %s", leaked, blob)
		}
	}
	for _, e := range streamed {
		if e != "v3_progress" && e != "v3_token" && e != "text" {
			t.Errorf("unexpected SSE projection %q", e)
		}
	}
}

// A live round trip: bytes produced by the running Python service, decoded and
// authorized here. Set ATLAS_LIVE_RESPONSE to a /v3/generate response body to
// run it; without it there is nothing live to check and the test says so.
func TestLiveServiceResponseAuthorizesAsExpected(t *testing.T) {
	path := os.Getenv("ATLAS_LIVE_RESPONSE")
	if path == "" {
		t.Skip("set ATLAS_LIVE_RESPONSE to a response body from the running service")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got V3GenerateResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("live response did not decode: %v", err)
	}
	if got.Evidence == nil {
		t.Fatal("live response carried no evidence envelope")
	}
	availability, why := got.Evidence.Validate()
	if availability != EvidenceAvailable {
		t.Fatalf("live envelope unusable: %s (%s)", availability, why)
	}
	if !got.Evidence.DescribesBytes(got.Code) {
		t.Fatal("live envelope does not describe the code it shipped with")
	}
	authorized, reason := v3DeliveryAuthorized(&got, got.Code)
	t.Logf("live: passed=%v phase=%s selection=%s closure=%v -> authorized=%v (%s)",
		got.Passed, got.PhaseSolved, got.Evidence.Selection.Status,
		got.Evidence.Evaluation.ClosureEligible, authorized, reason)
	if got.Evidence.Selection.Status == "verified_winner" &&
		got.Evidence.Evaluation.ClosureEligible && !authorized {
		t.Errorf("a live verified winner was refused: %s", reason)
	}
}

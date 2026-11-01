package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The ordinary existing-file write: the model rewrites a file this session
// wrote, and it is not in an edit-test-fix loop. Tier1 content keeps it off
// V3 even with V3 configured, so a /v3/ request fails the test.
//
// The shared existing-file regression gate makes the observation once and the
// result carries it. The final write block runs no check again: a second check
// would show up as an extra syntax-check call on the same bytes.

const directBaselineHealthy = writeRouteBaselineMark + `
import math

RADIUS = 2.0
AREA = math.pi * RADIUS * RADIUS
`

const directBaselineBroken = writeRouteBaselineMark + `
import math

` + writeRouteSyntaxErrorMark + `
`

const directProposalHealthy = `import math

RADIUS = 2.0
AREA = math.pi * RADIUS * RADIUS
CIRCUMFERENCE = 2 * math.pi * RADIUS
`

const directProposalBroken = `import math

` + writeRouteSyntaxErrorMark + `
    return math.pi * radius * radius
`

// Parses, and calls a name the file neither imports nor defines.
const directProposalUnresolved = `import math

RADIUS = 2.0
AREA = missing_helper(RADIUS)
`

func TestDirectWriteRoute(t *testing.T) {
	if _, gated := syntaxGateLanguages[".txt"]; gated {
		t.Fatal(".txt gained a sandbox checker; pick a type neither check applies to")
	}
	if embeddedScriptExts[".txt"] {
		t.Fatal(".txt gained an embedded-script check; pick a type neither check applies to")
	}

	for _, tc := range []struct {
		name     string
		rel      string // the path as the model sends it
		baseline string // the file on disk before the write
		proposal string // the content the model sends

		unavailableFor string       // the syntax check answers 500 for content holding this
		introduces     string       // the structural check reports this name as unresolved
		lastTool       AgentMessage // the last tool message; the zero value means a read_file
		readOnlyDir    bool         // the folder is read-only, so the write itself fails

		wantSuccess         bool
		wantDisk            string
		wantMutation        MutationStatus
		wantKind            ValidationKind
		wantStatus          ValidationStatus
		wantSyntaxCalls     int
		wantStructuralCalls int
		wantError           string // the exact error, when set
		errorHas            string // a part of the error, when set
		wantDetail          string // the exact validation detail, when set
		detailHas           string // a part of the validation detail, when set
	}{
		{name: "proposal parses",
			rel: "draft.py", baseline: directBaselineHealthy, proposal: directProposalHealthy,
			wantSuccess: true, wantDisk: directProposalHealthy,
			wantMutation: MutationApplied, wantKind: ValidationKindSyntax, wantStatus: ValidationPassed,
			wantSyntaxCalls: 1, wantStructuralCalls: 1},
		// One attempt, and its failure to run is the observation. The write
		// fails open, and the result says not_run.
		{name: "checker unavailable",
			rel: "draft.py", baseline: directBaselineHealthy, proposal: directProposalHealthy,
			unavailableFor: "CIRCUMFERENCE",
			wantSuccess:    true, wantDisk: directProposalHealthy,
			wantMutation: MutationApplied, wantKind: ValidationKindSyntax, wantStatus: ValidationNotRun,
			wantSyntaxCalls: 1, wantStructuralCalls: 1},
		// Applicability is decided locally, so no service is asked anything.
		{name: "no check applies",
			rel: "NOTES.txt", baseline: "release notes\n", proposal: "release notes\nsecond line\n",
			wantSuccess: true, wantDisk: "release notes\nsecond line\n",
			wantMutation: MutationApplied, wantKind: ValidationKindNone, wantStatus: ValidationNotApplicable,
			wantSyntaxCalls: 0, wantStructuralCalls: 0},
		// A repair of a file that already fails may land, and the result
		// says the landed bytes do not parse.
		{name: "failed proposal over a broken baseline lands",
			rel: "draft.py", baseline: directBaselineBroken, proposal: directProposalBroken,
			wantSuccess: true, wantDisk: directProposalBroken,
			wantMutation: MutationApplied, wantKind: ValidationKindSyntax, wantStatus: ValidationFailed,
			wantSyntaxCalls: 2, wantStructuralCalls: 1},
		{name: "failed proposal over a healthy baseline is refused",
			rel: "draft.py", baseline: directBaselineHealthy, proposal: directProposalBroken,
			wantSuccess: false, wantDisk: directBaselineHealthy,
			wantMutation: MutationRefused, wantKind: ValidationKindSyntax, wantStatus: ValidationFailed,
			wantSyntaxCalls: 2, wantStructuralCalls: 0,
			wantError:  fallbackSyntaxRejection("draft.py", directProposalBroken, writeRouteSyntaxDetail),
			wantDetail: writeRouteSyntaxDetail},
		// No evidence about the baseline is not evidence that it was broken,
		// so the failed proposal is refused.
		{name: "unavailable baseline check refuses a failed proposal",
			rel: "draft.py", baseline: directBaselineHealthy, proposal: directProposalBroken,
			unavailableFor: writeRouteBaselineMark,
			wantSuccess:    false, wantDisk: directBaselineHealthy,
			wantMutation: MutationRefused, wantKind: ValidationKindSyntax, wantStatus: ValidationFailed,
			wantSyntaxCalls: 2, wantStructuralCalls: 0},
		// Bytes that were checked and passed can still fail to land, and the
		// file keeps its old bytes.
		{name: "check passes, then the write fails",
			rel: "ro/draft.py", baseline: directBaselineHealthy, proposal: directProposalHealthy,
			readOnlyDir: true,
			wantSuccess: false, wantDisk: directBaselineHealthy,
			wantMutation: MutationFailed, wantKind: ValidationKindSyntax, wantStatus: ValidationPassed,
			wantSyntaxCalls: 1, wantStructuralCalls: 1, errorHas: "cannot write"},
		// Syntax passed on these bytes and the structural check refused them,
		// so structural is the deciding verdict. The structural check needs
		// both sides to refuse.
		{name: "structural refusal outranks a passing syntax check",
			rel: "draft.py", baseline: directBaselineHealthy, proposal: directProposalUnresolved,
			introduces:  "missing_helper",
			wantSuccess: false, wantDisk: directBaselineHealthy,
			wantMutation: MutationRefused, wantKind: ValidationKindStructural, wantStatus: ValidationFailed,
			wantSyntaxCalls: 1, wantStructuralCalls: 2, errorHas: "missing_helper", detailHas: "missing_helper"},
		// The last run failed, but it names webapp.py, and app.py is not a
		// whole file name inside it. So the session is not in the
		// edit-test-fix loop, and this ordinary route runs.
		{name: "failed run names another file",
			rel: "app.py", baseline: directBaselineHealthy, proposal: directProposalHealthy,
			lastTool: AgentMessage{Role: "tool", ToolName: "run_command",
				Content: `{"success":false,"data":{"stderr":"  File \"./webapp.py\", line 3\nSyntaxError"}}`},
			wantSuccess: true, wantDisk: directProposalHealthy,
			wantMutation: MutationApplied, wantKind: ValidationKindSyntax, wantStatus: ValidationPassed,
			wantSyntaxCalls: 1, wantStructuralCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			services := newWriteRouteServices(t, tc.unavailableFor, tc.introduces)
			dir := t.TempDir()
			target := filepath.Join(dir, tc.rel)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, []byte(tc.baseline), 0o644); err != nil {
				t.Fatal(err)
			}
			var events []string
			ctx := NewAgentContext(dir, Tier2Medium)
			ctx.PermissionMode = PermissionYolo
			ctx.StreamFn = func(event string, _ interface{}) { events = append(events, event) }
			// V3 is configured and the outputs are declared, so the tier
			// alone keeps V3 off.
			ctx.V3URL = services.URL
			ctx.TaskContract = declaredOutputs(tc.rel)
			ctx.SandboxURL = services.URL
			ctx.SessionWrites[tc.rel] = true
			last := tc.lastTool
			if last.Role == "" {
				last = AgentMessage{Role: "tool", ToolName: "read_file", Content: tc.baseline}
			}
			ctx.Messages = []AgentMessage{
				{Role: "user", Content: "extend the module"},
				{Role: "assistant", Content: "rewriting it"},
				last,
			}
			if tc.readOnlyDir {
				folder := filepath.Dir(target)
				if err := os.Chmod(folder, 0o555); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(folder, 0o755) })
			}
			if isActiveDebugIteration(ctx, tc.rel) {
				t.Fatal("the fixture is on the active-debug route, not the ordinary one")
			}
			if tier := classifyFileTier(tc.rel, tc.proposal); tier >= Tier2Medium {
				t.Fatalf("the fixture must be Tier1 so the ordinary route is chosen with V3 configured, got %v", tier)
			}

			args, _ := json.Marshal(map[string]string{"path": tc.rel, "content": tc.proposal})
			res := executeToolCall("write_file", args, ctx)

			calls := services.calls()
			if len(calls.unexpected) > 0 {
				t.Fatalf("unexpected endpoints were called: %v", calls.unexpected)
			}
			if calls.v3 != 0 {
				t.Fatalf("V3 generation ran (%d calls); this is not the ordinary direct route", calls.v3)
			}
			// Only the V3 branch streams events, so this route sends none.
			if len(events) != 0 {
				t.Errorf("the route streamed events: %v", events)
			}
			if res.Success != tc.wantSuccess {
				t.Fatalf("success = %v, want %v (error %q)", res.Success, tc.wantSuccess, res.Error)
			}
			disk, _ := os.ReadFile(target)
			if string(disk) != tc.wantDisk {
				t.Errorf("disk holds %q, want %q", disk, tc.wantDisk)
			}
			entries, _ := os.ReadDir(filepath.Dir(target))
			for _, e := range entries {
				if strings.Contains(e.Name(), ".atlas.tmp") {
					t.Errorf("a temporary file survived: %s", e.Name())
				}
			}
			if res.MutationStatus != tc.wantMutation ||
				res.ValidationKind != tc.wantKind ||
				res.ValidationStatus != tc.wantStatus {
				t.Errorf("got %q/%q/%q, want %q/%q/%q",
					res.MutationStatus, res.ValidationKind, res.ValidationStatus,
					tc.wantMutation, tc.wantKind, tc.wantStatus)
			}
			if !res.Classified() {
				t.Error("result not fully classified")
			}
			// The gate's observation is carried to the result, never made again.
			if calls.syntax != tc.wantSyntaxCalls {
				t.Errorf("syntax-check calls = %d, want %d", calls.syntax, tc.wantSyntaxCalls)
			}
			if calls.structural != tc.wantStructuralCalls {
				t.Errorf("structural_check calls = %d, want %d", calls.structural, tc.wantStructuralCalls)
			}
			if calls.embedded != 0 {
				t.Errorf("embedded_script_check calls = %d, want 0", calls.embedded)
			}
			if tc.wantError != "" && res.Error != tc.wantError {
				t.Errorf("error text:\n got %q\nwant %q", res.Error, tc.wantError)
			}
			if !strings.Contains(res.Error, tc.errorHas) {
				t.Errorf("error %q does not name %q", res.Error, tc.errorHas)
			}
			if tc.wantDetail != "" && res.ValidationDetail != tc.wantDetail {
				t.Errorf("validation detail = %q, want %q", res.ValidationDetail, tc.wantDetail)
			}
			if !strings.Contains(res.ValidationDetail, tc.detailHas) {
				t.Errorf("validation detail %q does not name %q", res.ValidationDetail, tc.detailHas)
			}
		})
	}
}

// Structural pin on the producers themselves. Eight structured evaluations
// exist in tool_write.go, and every one of them is a case some other gate did not
// already answer:
//
//	regression gate     proposal + baseline   existing destination
//	V3 preflight        proposal + baseline   new destination / unread baseline
//	active-debug branch proposal + baseline   destination gone since the write
//	new-file gate       proposal              sub-Tier2 or unconfigured V3
//	V3 service fallback baseline              after generation failed, deliberately
//	                                          fresh: minutes may have passed
//	V3 delivery         final bytes           the artifact the pipeline delivers,
//	                                          after sanitisation
//	V3 delivery         baseline              the alternative, when the candidate
//	                                          fails its own final-byte check
//	V3 revocations      baseline              structural and embedded, before the
//	                                          baseline they restore is written
//	V3 terminal guard   final bytes           re-observes only if some branch
//	                                          changed the bytes without saying so
//	edit gate           proposal + baseline   shared by edit_file,
//	                                          structural_edit, insert_after and
//	                                          replace_lines, once each
//
// None is in the final direct-write block. Growth beyond this means a route
// recomputed an observation it already held.
func TestWriteRoutesDoNotRecomputeTheirObservation(t *testing.T) {
	structured, legacy := syntaxCallCounts(t)
	// 13 since the write route's gates share one check of the baseline. Before
	// that 17, since structural_edit moved onto the shared edit gate (#214). It
	// had one site of its own, after the write, to observe the bytes it
	// spliced (before that it recorded not_run, and a file it edited carried
	// mutation debt nothing could discharge). The gate now observes the
	// proposed bytes before the write, and that observation is the verdict
	// it keeps: asking again after the write would be the recomputation this
	// counts against. Every site is beside the bytes it describes.
	if structured != 13 {
		t.Errorf("fallbackSyntaxOutcomeFor call sites = %d, want 13; a new one on a "+
			"route that already holds an observation is a recomputation", structured)
	}
	// The migration is complete: no route calls the legacy wrapper any more.
	// It survives as one definition in gates.go, whose own tests still cover
	// it, and TestLegacyCheckerWrapperInventory names what is left.
	if legacy != 0 {
		t.Errorf("checkFallbackSyntax call sites = %d, want 0 -- every route now "+
			"uses the structured producer", legacy)
	}
}

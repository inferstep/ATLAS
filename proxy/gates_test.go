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
	"strings"
	"sync"
	"testing"
	"time"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func findingsContaining(findings []string, substr string) bool {
	for _, f := range findings {
		if strings.Contains(f, substr) {
			return true
		}
	}
	return false
}

func TestAssetLintFlagsSnakeGameShape(t *testing.T) {
	// The 2026-07-18 session verbatim: template and static script
	// written, then an app.py that inlines everything and references
	// neither.
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"app.py": "from flask import Flask, render_template_string\n" +
			"app = Flask(__name__)\n" +
			"@app.route('/')\ndef index():\n" +
			"    return render_template_string(\"<html>inline</html>\")\n",
		"templates/index.html": "<html><body>snake</body></html>",
		"static/game.js":       "console.log('game');",
	})
	findings := assetLintFindings(root)
	if !findingsContaining(findings, "templates/index.html is referenced by nothing") {
		t.Fatalf("orphan template not flagged: %v", findings)
	}
	if !findingsContaining(findings, "render_template_string") {
		t.Fatalf("inline-template hint missing: %v", findings)
	}
	if !findingsContaining(findings, "static/game.js is referenced by nothing") {
		t.Fatalf("orphan static not flagged: %v", findings)
	}
}

func TestAssetLintQuietOnWiredProject(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"app.py": "from flask import Flask, render_template\n" +
			"@app.route('/')\ndef index():\n" +
			"    return render_template('index.html')\n",
		"templates/index.html": "<html><script src=\"/static/game.js\"></script></html>",
		"static/game.js":       "console.log('game');",
	})
	if findings := assetLintFindings(root); len(findings) != 0 {
		t.Fatalf("wired project should be quiet, got: %v", findings)
	}
}

func TestAssetLintDanglingReferences(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"templates/index.html": "<html>" +
			"<script src=\"/static/missing.js\"></script>" +
			"<link href=\"https://cdn.example.com/x.css\">" + // external: skip
			"<a href=\"#top\">top</a>" + // anchor: skip
			"<img src=\"{{ asset_path }}\">" + // unresolvable templated value: skip
			"</html>",
		"app.py": "from flask import render_template\n" +
			"render_template('index.html')\n" +
			"x = url_for('static', filename='also-missing.css')\n",
	})
	findings := assetLintFindings(root)
	if !findingsContaining(findings, `"/static/missing.js"`) {
		t.Fatalf("dangling src not flagged: %v", findings)
	}
	if !findingsContaining(findings, "also-missing.css") {
		t.Fatalf("dangling url_for not flagged: %v", findings)
	}
	for _, f := range findings {
		if strings.Contains(f, "cdn.example.com") || strings.Contains(f, "#top") ||
			strings.Contains(f, "asset_path") {
			t.Fatalf("external/anchor/templated ref flagged: %q", f)
		}
	}
}

func TestAssetLintSkipsLargeProjects(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{"templates/orphan.html": "<html></html>"}
	for i := 0; i < assetLintMaxFiles+5; i++ {
		files[fmt.Sprintf("pkg/f%d.py", i)] = "x = 1\n"
	}
	writeTree(t, root, files)
	if findings := assetLintFindings(root); findings != nil {
		t.Fatalf("large project must be skipped, got: %v", findings)
	}
}

func TestAssetLintNoteDedupes(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"templates/index.html": "<html></html>",
		"app.py":               "print('no render')\n",
	})
	ctx := &AgentContext{WorkingDir: root}
	first := assetLintNote(ctx)
	if first == "" || !strings.Contains(first, "templates/index.html") {
		t.Fatalf("first note should carry the finding: %q", first)
	}
	if again := assetLintNote(ctx); again != "" {
		t.Fatalf("unchanged findings must not repeat, got: %q", again)
	}
}

func TestSessionManifestNoteAnnouncesOncePerFile(t *testing.T) {
	ctx := &AgentContext{SessionWrites: map[string]bool{"app.py": true}}
	if note := sessionManifestNote(ctx); note != "" {
		t.Fatalf("single file needs no manifest, got %q", note)
	}
	ctx.SessionWrites["templates/index.html"] = true
	note := sessionManifestNote(ctx)
	if !strings.Contains(note, "app.py") || !strings.Contains(note, "templates/index.html") {
		t.Fatalf("manifest should list both files: %q", note)
	}
	if again := sessionManifestNote(ctx); again != "" {
		t.Fatalf("no new files → no repeat, got %q", again)
	}
	ctx.SessionWrites["static/game.js"] = true
	if third := sessionManifestNote(ctx); !strings.Contains(third, "static/game.js") {
		t.Fatalf("new file should re-announce full set: %q", third)
	}
}

func TestAssetLintFlatLayoutOrphan(t *testing.T) {
	// Mini-bench t03/t07: flat layout (no templates/static), html inlines a
	// duplicate <script> and orphans the companion .js.
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"index.html": "<html><script>function calculate(){}</script></html>",
		"calc.js":    "function calculate(expression) { return 1; }",
		"style.css":  "body { margin: 0; }",
	})
	findings := assetLintFindings(root)
	if !findingsContaining(findings, "calc.js is referenced by nothing") {
		t.Fatalf("flat orphan js not flagged: %v", findings)
	}
	if !findingsContaining(findings, "style.css is referenced by nothing") {
		t.Fatalf("flat orphan css not flagged: %v", findings)
	}
}

func TestAssetLintFlatLayoutQuietWhenWiredOrNoHTML(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"index.html": "<html><script src=\"calc.js\"></script><link href=\"style.css\"></html>",
		"calc.js":    "function calculate(){}",
		"style.css":  "body{}",
	})
	if f := assetLintFindings(root); len(f) != 0 {
		t.Fatalf("wired flat project should be quiet: %v", f)
	}
	// A pure library with no HTML must not flag its entry file.
	root2 := t.TempDir()
	writeTree(t, root2, map[string]string{"index.js": "module.exports = 1;"})
	if f := assetLintFindings(root2); len(f) != 0 {
		t.Fatalf("no-HTML project should be quiet: %v", f)
	}
}

func TestAssetLintMissingTemplateReference(t *testing.T) {
	// Snake fix session: errorhandler renders templates/404.html which
	// doesn't exist — every 404 became a 500.
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"app.py": "from flask import Flask, render_template\n" +
			"@app.route('/')\ndef i(): return render_template('index.html')\n" +
			"@app.errorhandler(404)\ndef nf(e): return render_template('404.html'), 404\n",
		"templates/index.html": "<html>{% extends \"base.html\" %}</html>",
	})
	findings := assetLintFindings(root)
	if !findingsContaining(findings, `references template "404.html"`) {
		t.Fatalf("missing py-referenced template not flagged: %v", findings)
	}
	if !findingsContaining(findings, `references template "base.html"`) {
		t.Fatalf("missing jinja extends target not flagged: %v", findings)
	}
}

func TestAssetLintRouteContractMismatch(t *testing.T) {
	// Mini-bench t01: JS calls REST endpoints the backend half never
	// declared; page loads, halves can't talk.
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"app.py": "from flask import Flask, render_template\n" +
			"@app.route('/')\ndef i(): return render_template('index.html')\n" +
			"@app.route('/add', methods=['POST'])\ndef a(): return ''\n" +
			"@app.route('/delete/<int:todo_id>', methods=['POST'])\ndef d(todo_id): return ''\n",
		"templates/index.html": "<html><script src=\"/static/todo.js\"></script>" +
			"<form action=\"/add\"></form></html>",
		"static/todo.js": "fetch('/api/todos');\nfetch(`/delete/${id}`, {method:'POST'});\n",
	})
	findings := assetLintFindings(root)
	if !findingsContaining(findings, `"/api/todos"`) {
		t.Fatalf("unrouted fetch target not flagged: %v", findings)
	}
	for _, f := range findings {
		if strings.Contains(f, `"/add"`) || strings.Contains(f, `"/delete/`) {
			t.Fatalf("legitimately routed target flagged: %q", f)
		}
	}
}

func TestClaimsUniversalCatchesGlobalAssertions(t *testing.T) {
	yes := []string{
		"All routes are functioning properly.",
		"Fixed all bugs in the routing layer.",
		"Everything works as expected.",
		"No errors remaining.",
		"Tested all endpoints — all green.",
		"App is fully functional.",
	}
	for _, s := range yes {
		if !claimsUniversal(s) {
			t.Errorf("claimsUniversal(%q) = false, want true", s)
		}
	}
	no := []string{
		"Added /admin route. Run the test suite to confirm the rest still works.",
		"Created the missing template for /pricing.",
		"Updated the readme.",
		"",
	}
	for _, s := range no {
		if claimsUniversal(s) {
			t.Errorf("claimsUniversal(%q) = true, want false", s)
		}
	}
}

// A22 parity: verifyCompletionClaims now consumes assetLintFindings'
// dangling-template findings, but a missing render_template target must
// still bounce a universal-claim done — universal summary trips
// claimsUniversal AND the structural check reports the gap.
func TestVerifyCompletionClaimsCatchesMissingFlaskTemplates(t *testing.T) {
	dir := t.TempDir()
	// app.py references 4 templates; only index.html exists.
	app := `from flask import Flask, render_template
app = Flask(__name__)
@app.route('/')
def index(): return render_template('index.html')
@app.route('/pricing')
def pricing(): return render_template('pricing.html')
@app.route('/contact')
def contact(): return render_template('contact.html')
@app.route('/admin')
def admin(): return render_template('admin.html')
`
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte(app), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "templates", "index.html"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !claimsUniversal("All routes are functioning properly.") {
		t.Fatal("test premise broken: universal summary not detected")
	}
	got := verifyCompletionClaims(dir)
	if got == "" {
		t.Fatal("expected gap report, got empty")
	}
	if !strings.Contains(got, "before declaring done") {
		t.Errorf("gap report should be shaped as a done-bounce directive:\n%s", got)
	}
	for _, want := range []string{"pricing.html", "contact.html", "admin.html"} {
		if !strings.Contains(got, want) {
			t.Errorf("gap report missing %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "index.html") {
		t.Errorf("gap report includes index.html (which exists):\n%s", got)
	}
}

func TestVerifyCompletionClaimsAllPresentReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	app := `from flask import Flask, render_template
app = Flask(__name__)
@app.route('/')
def index(): return render_template('index.html')
`
	os.WriteFile(filepath.Join(dir, "app.py"), []byte(app), 0o644)
	os.MkdirAll(filepath.Join(dir, "templates"), 0o755)
	os.WriteFile(filepath.Join(dir, "templates", "index.html"), []byte("ok"), 0o644)

	if got := verifyCompletionClaims(dir); got != "" {
		t.Errorf("expected empty (no gaps), got: %s", got)
	}
}

func TestVerifyCompletionClaimsSkipsNoiseDirs(t *testing.T) {
	dir := t.TempDir()
	// A render_template inside venv/ should NOT be parsed.
	os.MkdirAll(filepath.Join(dir, "venv", "lib"), 0o755)
	os.WriteFile(filepath.Join(dir, "venv", "lib", "junk.py"),
		[]byte(`render_template('ghost.html')`), 0o644)
	if got := verifyCompletionClaims(dir); got != "" {
		t.Errorf("noise dir tripped check: %s", got)
	}
}

func TestVerifyCompletionClaimsHandlesDynamicRenderArgs(t *testing.T) {
	// `render_template(name)` — variable arg, can't statically check.
	// Should NOT produce a gap report.
	dir := t.TempDir()
	app := `from flask import render_template
def view(name): return render_template(name)
`
	os.WriteFile(filepath.Join(dir, "app.py"), []byte(app), 0o644)
	if got := verifyCompletionClaims(dir); got != "" {
		t.Errorf("dynamic render tripped check: %s", got)
	}
}

func TestPromptIsMultiIssueCatchesPlurals(t *testing.T) {
	yes := []string{
		"there are LOTS of issues with the flask app",
		"a ton of bugs in this code",
		"fix all the bugs",
		"the routes don't work — fix everything",
		"multiple problems here",
		"it doesn't work",
		"all routes are broken",
		"nothing works",
		"can you fix the bugs?",
	}
	for _, m := range yes {
		if !promptIsMultiIssue(m) {
			t.Errorf("promptIsMultiIssue(%q) = false, want true", m)
		}
	}
	no := []string{
		"add a /admin route to the flask app",
		"fix the typo on line 42",
		"create a new endpoint for /health",
		"why does index.html return 500?",
		"what does this function do?",
	}
	for _, m := range no {
		if promptIsMultiIssue(m) {
			t.Errorf("promptIsMultiIssue(%q) = true, want false", m)
		}
	}
}

func TestPromptMultiIssueTriggersClaimCheck(t *testing.T) {
	// Smoke: a multi-issue prompt + narrow done summary +
	// missing templates → gap fires. Without the prompt-side
	// multi-issue trigger, a narrow summary would skip the check.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.py"), []byte(
		`from flask import render_template
def x(): return render_template('a.html')
def y(): return render_template('b.html')`), 0o644)
	os.MkdirAll(filepath.Join(dir, "templates"), 0o755)
	os.WriteFile(filepath.Join(dir, "templates", "a.html"), []byte("ok"), 0o644)

	narrow := "Fixed the /a route."
	if claimsUniversal(narrow) {
		t.Fatal("test premise broken: narrow summary should not be universal")
	}
	if !promptIsMultiIssue("LOTS of issues with the flask app, fix the bugs") {
		t.Fatal("test premise broken: multi-issue prompt not detected")
	}
	if got := verifyCompletionClaims(dir); got == "" || !strings.Contains(got, "b.html") {
		t.Errorf("gap report missing b.html, got: %q", got)
	}
}

func mkPlan(steps ...PlanStep) *Plan {
	p := &Plan{Steps: steps}
	if len(steps) > 0 {
		p.VerifyStep = steps[len(steps)-1].ID
	}
	return p
}

func mkArgs(t *testing.T, v interface{}) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestActionMatchesTool(t *testing.T) {
	cases := []struct {
		action, tool string
		want         bool
	}{
		// Direct + prefix-stem matching against the canonical tool
		// names. The planner is prompted to produce tool names
		// verbatim, so we don't need to handle freeform descriptive
		// actions ("verify with curl") — those count as off-plan.
		{"read_file", "read_file", true},
		{"read", "read_file", true},
		{"write_file", "write_file", true},
		{"edit", "edit_file", true},
		{"run_command", "run_command", true},
		{"run", "run_command", true},
		{"investigate the bug", "read_file", false},
		{"verify with curl", "run_command", false},
		{"write_file", "edit_file", false},
		{"", "read_file", false},
		{"read", "", false},
	}
	for _, tc := range cases {
		if got := actionMatchesTool(tc.action, tc.tool); got != tc.want {
			t.Errorf("actionMatchesTool(%q, %q) = %v, want %v",
				tc.action, tc.tool, got, tc.want)
		}
	}
}

func TestTargetsOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"app.py", "app.py", true},
		{"app.py", "/workspace/app.py", true},
		{"templates/index.html", "/workspace/templates/index.html", true},
		{"./app.py", "app.py", true},
		{"app.py", "tests/app.py", true},
		{"app.py", "src/main.go", false},
		{"curl http://localhost:5000/", "curl http://localhost:5000/hello", true},
		{"", "app.py", false},
	}
	for _, tc := range cases {
		if got := targetsOverlap(tc.a, tc.b); got != tc.want {
			t.Errorf("targetsOverlap(%q, %q) = %v, want %v",
				tc.a, tc.b, got, tc.want)
		}
	}
}

func TestMatchPlanStepFirstUnsatisfied(t *testing.T) {
	plan := mkPlan(
		PlanStep{ID: "s1", Action: "read_file", Target: "templates/index.html"},
		PlanStep{ID: "s2", Action: "edit_file", Target: "templates/index.html"},
		PlanStep{ID: "s3", Action: "run_command", Target: "curl http://localhost:5000/"},
	)
	satisfied := []bool{false, false, false}

	// read_file on the templates path matches s1.
	args := mkArgs(t, map[string]string{"path": "/workspace/templates/index.html"})
	if got := matchPlanStep(plan, satisfied, "read_file", args); got != 0 {
		t.Errorf("matchPlanStep first call = %d, want 0", got)
	}

	// Mark s1 satisfied. read_file again should NOT re-match s1 (already done).
	satisfied[0] = true
	if got := matchPlanStep(plan, satisfied, "read_file", args); got != -1 {
		t.Errorf("matchPlanStep after s1 satisfied = %d, want -1", got)
	}

	// edit_file on the same path matches s2.
	editArgs := mkArgs(t, map[string]string{"path": "templates/index.html"})
	if got := matchPlanStep(plan, satisfied, "edit_file", editArgs); got != 1 {
		t.Errorf("matchPlanStep edit_file = %d, want 1", got)
	}

	// run_command with curl matches s3 even with extra path components.
	runArgs := mkArgs(t, map[string]string{"command": "curl http://localhost:5000/"})
	if got := matchPlanStep(plan, satisfied, "run_command", runArgs); got != 2 {
		t.Errorf("matchPlanStep run_command = %d, want 2", got)
	}
}

func TestMatchPlanStepNoMatchOffPlan(t *testing.T) {
	plan := mkPlan(
		PlanStep{ID: "s1", Action: "read_file", Target: "app.py"},
	)
	satisfied := []bool{false}

	// list_directory isn't in the plan — should not match.
	args := mkArgs(t, map[string]string{"path": "."})
	if got := matchPlanStep(plan, satisfied, "list_directory", args); got != -1 {
		t.Errorf("off-plan list_directory matched step %d, want -1", got)
	}

	// read_file on a different file shouldn't match a target-specific step.
	args = mkArgs(t, map[string]string{"path": "tests/test_app.py"})
	if got := matchPlanStep(plan, satisfied, "read_file", args); got != -1 {
		t.Errorf("read_file on wrong file matched step %d, want -1", got)
	}
}

func TestMatchPlanStepNilPlanReturnsMinusOne(t *testing.T) {
	if got := matchPlanStep(nil, nil, "read_file", nil); got != -1 {
		t.Errorf("nil plan = %d, want -1", got)
	}
}

func TestRecordPlanAdherenceUpdatesState(t *testing.T) {
	plan := mkPlan(
		PlanStep{ID: "s1", Action: "read_file", Target: "app.py"},
		PlanStep{ID: "s2", Action: "edit_file", Target: "app.py"},
	)
	ctx := &AgentContext{Plan: plan}
	ctx.StreamFn = func(string, interface{}) {} // /dev/null sink

	// On-plan read → satisfied[0], streak resets.
	revise := recordPlanAdherence(ctx, "read_file",
		mkArgs(t, map[string]string{"path": "app.py"}), true)
	if revise {
		t.Error("first call shouldn't trigger revise")
	}
	if !ctx.PlanStepsSatisfied[0] {
		t.Error("step 0 not marked satisfied")
	}
	if ctx.PlanOffStreak != 0 {
		t.Errorf("off_streak = %d, want 0 after on-plan call", ctx.PlanOffStreak)
	}

	// Recon tool (list_directory) is NEUTRAL — exploration doesn't
	// violate a plan, so it must NOT increment off_streak. Verifies
	// the isReconTool gate added with planAutoReviseThreshold 3→5.
	revise = recordPlanAdherence(ctx, "list_directory",
		mkArgs(t, map[string]string{"path": "."}), true)
	if revise {
		t.Error("recon call shouldn't trigger revise")
	}
	if ctx.PlanOffStreak != 0 {
		t.Errorf("off_streak = %d after recon, want 0 (recon is neutral)", ctx.PlanOffStreak)
	}

	// Off-plan run_command (non-recon) → streak goes to 1.
	revise = recordPlanAdherence(ctx, "run_command",
		mkArgs(t, map[string]string{"command": "echo hi"}), true)
	if revise {
		t.Error("streak=1 shouldn't trigger revise")
	}
	if ctx.PlanOffStreak != 1 {
		t.Errorf("off_streak = %d after first off-plan run_command, want 1", ctx.PlanOffStreak)
	}

	// Need planAutoReviseThreshold (=5) total off-plan calls to revise.
	// Already at 1, fire 4 more.
	for i := 0; i < 3; i++ {
		revise = recordPlanAdherence(ctx, "run_command",
			mkArgs(t, map[string]string{"command": "echo " + string(rune('a'+i))}), true)
		if revise {
			t.Errorf("revise fired early at streak=%d (threshold=%d)",
				ctx.PlanOffStreak, planAutoReviseThreshold)
		}
	}
	revise = recordPlanAdherence(ctx, "run_command",
		mkArgs(t, map[string]string{"command": "echo final"}), true)
	if !revise {
		t.Errorf("streak=%d should trigger revise (threshold=%d)",
			ctx.PlanOffStreak, planAutoReviseThreshold)
	}
}

func TestRecordPlanAdherenceFailedCallsDontSatisfy(t *testing.T) {
	plan := mkPlan(
		PlanStep{ID: "s1", Action: "run_command", Target: "pytest"},
	)
	ctx := &AgentContext{Plan: plan}
	ctx.StreamFn = func(string, interface{}) {}

	// Failed run_command shouldn't tick off the verify step.
	recordPlanAdherence(ctx, "run_command",
		mkArgs(t, map[string]string{"command": "pytest"}), false)
	if ctx.PlanStepsSatisfied[0] {
		t.Error("failed call shouldn't satisfy plan step")
	}
	if ctx.PlanOffStreak != 1 {
		t.Errorf("failed call should extend streak, got %d", ctx.PlanOffStreak)
	}
}

func TestRecordPlanAdherenceNoOpWithoutPlan(t *testing.T) {
	ctx := &AgentContext{}
	if recordPlanAdherence(ctx, "read_file", nil, true) {
		t.Error("nil plan shouldn't trigger revise")
	}
	if ctx.PlanStepsSatisfied != nil {
		t.Error("nil plan shouldn't allocate satisfied tracking")
	}
}

func TestRecordPlanAdherenceCapsRevisions(t *testing.T) {
	plan := mkPlan(PlanStep{ID: "s1", Action: "read_file", Target: "a.py"})
	ctx := &AgentContext{
		Plan:          plan,
		PlanRevisions: planMaxRevisions, // already at the cap
	}
	ctx.StreamFn = func(string, interface{}) {}

	// Hammer with off-plan calls — past the cap, recordPlanAdherence
	// must NOT request a revise (we'd thrash forever otherwise).
	for i := 0; i < 10; i++ {
		if recordPlanAdherence(ctx, "list_directory",
			mkArgs(t, map[string]string{"path": "."}), true) {
			t.Fatalf("revise triggered past cap at i=%d", i)
		}
	}
}

func TestBuildSystemPromptIncludesPlan(t *testing.T) {
	plan := mkPlan(
		PlanStep{ID: "s1", Action: "read_file", Target: "app.py", Why: "inspect current routes"},
		PlanStep{ID: "s2", Action: "edit_file", Target: "app.py", Why: "add /hello route"},
		PlanStep{ID: "s3", Action: "run_command", Target: "curl http://localhost:5000/hello", Why: "verify"},
	)
	plan.Rationale = "investigate, change, verify."
	ctx := &AgentContext{
		WorkingDir: "/workspace",
		Plan:       plan,
	}
	prompt := buildSystemPrompt(ctx)

	// Plan section header present.
	if !strings.Contains(prompt, "## Plan") {
		t.Error("system prompt missing ## Plan header")
	}
	// All three step actions surfaced.
	for _, s := range []string{"read_file", "edit_file", "run_command"} {
		if !strings.Contains(prompt, s) {
			t.Errorf("system prompt missing step action %q", s)
		}
	}
	// Verify step marker present so model knows which step is "done"-gate.
	if !strings.Contains(prompt, "verify step (s3)") {
		t.Error("system prompt doesn't call out the verify step")
	}
	if !strings.Contains(prompt, "investigate, change, verify") {
		t.Error("system prompt doesn't include rationale")
	}
}

func TestBuildSystemPromptOmitsPlanSectionWhenNoPlan(t *testing.T) {
	ctx := &AgentContext{WorkingDir: "/workspace"}
	prompt := buildSystemPrompt(ctx)
	if strings.Contains(prompt, "## Plan") {
		t.Error("system prompt has Plan section when ctx.Plan is nil")
	}
}

func TestExtractToolTargetReadsCommonShapes(t *testing.T) {
	cases := []struct {
		tool string
		args interface{}
		want string
	}{
		{"read_file", map[string]string{"path": "app.py"}, "app.py"},
		{"write_file", map[string]string{"path": "x.py", "content": "..."}, "x.py"},
		{"edit_file", map[string]string{"path": "y.py", "old_str": "a", "new_str": "b"}, "y.py"},
		{"run_command", map[string]string{"command": "pytest tests/"}, "pytest tests/"},
		{"list_directory", map[string]string{"path": "src"}, "src"},
		{"plan_revise", map[string]string{"reason": "x"}, ""}, // unknown tool → empty
	}
	for _, tc := range cases {
		args := mkArgs(t, tc.args)
		if got := extractToolTarget(tc.tool, args); got != tc.want {
			t.Errorf("extractToolTarget(%s) = %q, want %q", tc.tool, got, tc.want)
		}
	}
}

// fakeV3Structural returns the given unresolved names for source containing
// `trigger`, else clean. Lets a test model "original clean, edited broken".
func fakeV3Structural(t *testing.T, trigger string, unresolved []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/structural_check" {
			http.Error(w, "not found", 404)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Source string `json:"source"`
		}
		_ = json.Unmarshal(raw, &body)
		out := map[string]interface{}{"ok": true, "unresolved": []string{}}
		if strings.Contains(body.Source, trigger) {
			out["unresolved"] = unresolved
		}
		b, _ := json.Marshal(out)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
}

func structCtx(url string) *AgentContext {
	return &AgentContext{V3URL: url, Ctx: context.Background(), WorkingDir: "/workspace"}
}

// An edit that introduces render_template (not in the original) is blocked.
func TestEditIntroducesUnresolvedBlocks(t *testing.T) {
	srv := fakeV3Structural(t, "render_template(", []string{"render_template"})
	defer srv.Close()
	ctx := structCtx(srv.URL)
	orig := "from flask import render_template_string\n@app.route('/')\ndef i(): return 'x'\n"
	edited := "from flask import render_template_string\n@app.route('/')\ndef i(): return render_template('i.html')\n"
	introduced := editIntroducesUnresolved(ctx, "app.py", orig, edited)
	if len(introduced) != 1 || introduced[0] != "render_template" {
		t.Fatalf("expected [render_template] introduced, got %v", introduced)
	}
	msg := structuralRejection("app.py", introduced)
	if !strings.Contains(msg, "`render_template`") || !strings.Contains(msg, "NameError") {
		t.Errorf("rejection should name the call + NameError: %q", msg)
	}
}

// A pre-existing unresolved name (present in BOTH original and edited) is a
// repair-in-progress and must NOT be blocked.
func TestPreexistingUnresolvedAllowed(t *testing.T) {
	srv := fakeV3Structural(t, "render_template(", []string{"render_template"})
	defer srv.Close()
	ctx := structCtx(srv.URL)
	// Both call render_template -> it's not newly introduced.
	orig := "def a(): return render_template('a')\n"
	edited := "def a(): return render_template('a')\ndef b(): return render_template('b')\n"
	if introduced := editIntroducesUnresolved(ctx, "app.py", orig, edited); len(introduced) != 0 {
		t.Errorf("pre-existing unresolved must be allowed, got %v", introduced)
	}
}

// Non-.py files and an unreachable V3 fail open (no block).
func TestStructuralGateFailsOpen(t *testing.T) {
	ctx := structCtx("http://127.0.0.1:0") // unreachable
	if introduced := editIntroducesUnresolved(ctx, "app.py", "x", "y render_template("); introduced != nil {
		t.Errorf("unreachable V3 must fail open, got %v", introduced)
	}
	ctx2 := structCtx("http://example.invalid")
	if _, ok := checkStructuralUnresolved(ctx2, "notes.txt", "render_template()"); ok {
		t.Error("non-.py must not be checked")
	}
}

// fakeV3StructuralResolving actually resolves `name` against the posted
// source: unresolved iff the source calls it AND lacks importLine. Faithful
// enough to express both directions of the #147 regression pair (blocked
// without the import, credited by an import anywhere in the composed file).
func fakeV3StructuralResolving(t *testing.T, name, importLine string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Source string `json:"source"`
		}
		_ = json.Unmarshal(raw, &body)
		out := map[string]interface{}{"ok": true, "unresolved": []string{}}
		if strings.Contains(body.Source, name+"(") && !strings.Contains(body.Source, importLine) {
			out["unresolved"] = []string{name}
		}
		b, _ := json.Marshal(out)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
}

// #147 scope item 3(b) at gate level: an edit whose new call is satisfied by
// an import living ELSEWHERE in the composed file (outside the edited
// fragment) must pass — the gate checks the whole post-edit file.
func TestImportElsewhereInFilePasses(t *testing.T) {
	srv := fakeV3StructuralResolving(t, "helper_util", "from utils import helper_util")
	defer srv.Close()
	ctx := structCtx(srv.URL)
	orig := "from utils import helper_util\n\ndef old():\n    return 1\n"
	edited := "from utils import helper_util\n\ndef old():\n    return helper_util()\n"
	if introduced := editIntroducesUnresolved(ctx, "app.py", orig, edited); len(introduced) != 0 {
		t.Errorf("import elsewhere in the file must credit the call, got %v", introduced)
	}
}

// Deleting an import a remaining direct call needs is a newly-introduced
// unresolved name and must be blocked.
func TestDeleteImportBlocked(t *testing.T) {
	srv := fakeV3StructuralResolving(t, "helper_util", "from utils import helper_util")
	defer srv.Close()
	ctx := structCtx(srv.URL)
	orig := "from utils import helper_util\n\ndef index():\n    return helper_util()\n"
	edited := "def index():\n    return helper_util()\n"
	introduced := editIntroducesUnresolved(ctx, "app.py", orig, edited)
	if len(introduced) != 1 || introduced[0] != "helper_util" {
		t.Fatalf("deleting the import must block, got %v", introduced)
	}
}

// When the ORIGINAL-side check can't run — a transient service failure on
// the second back-to-back call; note malformed Python is NOT this trigger,
// tree-sitter parses it tolerantly and returns ok:true — the healthy->
// broken comparison has no baseline: the gate must fail open (after one
// retry), not count every unresolved name as newly introduced.
func TestOriginalCheckFailureFailsOpen(t *testing.T) {
	origCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Source string `json:"source"`
		}
		_ = json.Unmarshal(raw, &body)
		// The fake models the service failing on the ORIGINAL-side
		// requests (marker comment), succeeding on the edited side.
		if strings.Contains(body.Source, "# original") {
			origCalls++
			_, _ = w.Write([]byte(`{"ok":false,"error":"transient failure"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"unresolved":["helper_x"]}`))
	}))
	defer srv.Close()
	ctx := structCtx(srv.URL)
	orig := "# original\ndef a():\n    return helper_x()\n"
	edited := "def fixed():\n    return helper_x()\n"
	if introduced := editIntroducesUnresolved(ctx, "app.py", orig, edited); introduced != nil {
		t.Errorf("original-side check failure must fail open, got %v", introduced)
	}
	if origCalls != 2 {
		t.Errorf("expected one retry of the original-side check (2 calls), got %d", origCalls)
	}
}

// The write_file variant of the rejection must name the operation the
// model actually issued — an "edit" steer on a blocked NEW-file write
// sends the model to edit_file against a file that doesn't exist.
func TestWriteRejectionNamesWrite(t *testing.T) {
	msg := structuralWriteRejection("app.py", []string{"render_template"})
	if !strings.Contains(msg, "write_file for app.py") || !strings.Contains(msg, "`render_template`") ||
		!strings.Contains(msg, "NameError") || !strings.Contains(msg, "re-issue the write_file") {
		t.Errorf("write rejection must be write-flavored and name the call: %q", msg)
	}
	if strings.Contains(msg, "re-issue the edit") {
		t.Errorf("write rejection must not steer toward edit tools: %q", msg)
	}
}

// readOriginalForGate: missing file = first write (empty original, gate
// runs); any other read failure = unknowable original (gate must skip).
func TestReadOriginalForGate(t *testing.T) {
	dir := t.TempDir()
	if content, ok := readOriginalForGate(dir + "/nope.py"); !ok || content != "" {
		t.Errorf("missing file must be (\"\", true), got (%q, %v)", content, ok)
	}
	if _, ok := readOriginalForGate(dir); ok {
		t.Error("unreadable original (a directory) must report not-ok so callers skip the gate")
	}
}

// A nil ctx.Ctx (paths constructed without a request context) must not
// panic — and the gate keeps working via a background context.
func TestNilRequestContextStillGates(t *testing.T) {
	srv := fakeV3Structural(t, "render_template(", []string{"render_template"})
	defer srv.Close()
	ctx := &AgentContext{V3URL: srv.URL, WorkingDir: "/workspace"} // Ctx nil
	orig := "def i(): return 'x'\n"
	edited := "def i(): return render_template('i.html')\n"
	introduced := editIntroducesUnresolved(ctx, "app.py", orig, edited)
	if len(introduced) != 1 || introduced[0] != "render_template" {
		t.Fatalf("nil ctx.Ctx must still gate via background context, got %v", introduced)
	}
}

// #147 review #2: the edited file's own (pre-edit) content must be excluded
// from the project_context sent, so a deleted top-level def isn't credited
// from stale state.
func TestStructuralCheckExcludesEditedSelf(t *testing.T) {
	var gotCtx map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		_ = json.Unmarshal(raw, &body)
		if pc, ok := body["project_context"].(map[string]interface{}); ok {
			gotCtx = pc
		} else {
			gotCtx = map[string]interface{}{}
		}
		_, _ = w.Write([]byte(`{"ok":true,"unresolved":[]}`))
	}))
	defer srv.Close()
	ctx := &AgentContext{
		V3URL: srv.URL, Ctx: context.Background(), WorkingDir: "/workspace",
		FilesRead:     map[string]string{"/workspace/app.py": "def gone(): pass", "/workspace/util.py": "def helper(): pass"},
		FileReadTimes: map[string]time.Time{"/workspace/app.py": time.Now(), "/workspace/util.py": time.Now()},
	}
	_, _ = checkStructuralUnresolved(ctx, "/workspace/app.py", "x = gone()")
	if _, present := gotCtx["app.py"]; present {
		t.Error("edited file app.py must be excluded from project_context")
	}
	if _, present := gotCtx["util.py"]; !present {
		t.Error("other read files should still be included")
	}
}

// A genuine syntax bug in complete content must NOT be blamed on truncation,
// must quote the offending line, and must forbid an identical resend
// (observed 2026-07-20, pytorch-model-recovery: an f-string resent 5×).
func TestFallbackSyntaxRejectionSyntaxBug(t *testing.T) {
	content := "import torch\nx = 1\ny = f\"{d[\"k[\"]}\"\n"
	msg := fallbackSyntaxRejection("a.py", content, "SyntaxError: f-string: unmatched '[' (a.py, line 3)")
	if strings.Contains(msg, "cut off") || strings.Contains(msg, "COMPLETE file content") {
		t.Errorf("must not give truncation advice for a real syntax bug: %q", msg)
	}
	if !strings.Contains(msg, "Do NOT resend") {
		t.Errorf("must forbid identical resend: %q", msg)
	}
	if !strings.Contains(msg, "line 3") || !strings.Contains(msg, `y = f`) {
		t.Errorf("must quote the offending line 3: %q", msg)
	}
}

// A genuinely truncated write keeps the "resend complete content" advice.
func TestFallbackSyntaxRejectionTruncation(t *testing.T) {
	msg := fallbackSyntaxRejection("a.py", "def f(", "SyntaxError: '(' was never closed (a.py, line 1)")
	if !strings.Contains(msg, "COMPLETE") {
		t.Errorf("truncation shape should advise resending complete content: %q", msg)
	}
}

// --- embedded-script gate -------------------------------------------------

// The keydown handler from the 2026-08-01 dogfooding failure: a Flask app whose
// UI is one HTML string, with one paren too many at the end of the last line.
const strayParenLine = `            else if(key === 'ArrowDown' && direction !== 'UP') nextDirection = 'DOWN');`

func flaskWithScript(handlerLine string) string {
	return "from flask import Flask, render_template_string\n" +
		"app = Flask(__name__)\n" +
		"HTML_TEMPLATE = \"\"\"\n<html><body>\n  <canvas id=\"gameCanvas\"></canvas>\n" +
		"  <script>\n    let nextDirection = 'RIGHT';\n" + handlerLine + "\n  </script>\n" +
		"</body></html>\n\"\"\"\n" +
		"@app.route('/')\ndef index():\n    return render_template_string(HTML_TEMPLATE)\n"
}

// fakeV3Embedded serves /internal/embedded_script_check: a finding for sources
// containing `trigger`, clean otherwise. Counts requests so the tests can
// assert the local pre-filter suppresses the call entirely.
func fakeV3Embedded(t *testing.T, trigger string, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/embedded_script_check" {
			http.Error(w, "not found", 404)
			return
		}
		if calls != nil {
			*calls++
		}
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Source string `json:"source"`
		}
		_ = json.Unmarshal(raw, &body)
		out := map[string]interface{}{"ok": true, "findings": []interface{}{}}
		if trigger != "" && strings.Contains(body.Source, trigger) {
			out["findings"] = []map[string]interface{}{{
				"line": 7, "column": 86, "kind": "javascript",
				"where":   "the <script> block inside the Python string HTML_TEMPLATE",
				"message": "unexpected `)`",
				"hint":    "Nothing opened a `(` for it to close — delete the stray `)`, or add the `(` it was meant to close.",
				"text":    strings.TrimSpace(strayParenLine),
			}}
		}
		b, _ := json.Marshal(out)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
}

// The gate consumes the finding and produces a rejection that names the file,
// the line, the offending construct and the fix — and says why running the app
// did not catch it.
func TestEmbeddedScriptGateBlocksStrayParen(t *testing.T) {
	srv := fakeV3Embedded(t, "'DOWN');", nil)
	defer srv.Close()
	ctx := structCtx(srv.URL)
	original := flaskWithScript("            let x = 1;")
	edited := flaskWithScript(strayParenLine)

	msg := embeddedScriptGate(ctx, "app.py", original, edited)
	if msg == "" {
		t.Fatal("gate must block a write that breaks the embedded script")
	}
	for _, want := range []string{
		"app.py",                     // the file
		"line 7",                     // the line
		"unexpected `)`",             // the offending construct
		"delete the stray `)`",       // how to fix it
		"HTML_TEMPLATE",              // which string to go fix
		"it was NOT written",         // nothing landed
		"still returns 200",          // why `curl` did not catch it
		"do NOT resend it unchanged", // no verbatim retry
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("rejection missing %q:\n%s", want, msg)
		}
	}
}

// Already-broken embedded script + still-broken result = repair-in-progress,
// same healthy->broken rule the syntax and structural gates use.
func TestEmbeddedScriptPreexistingBreakageAllowed(t *testing.T) {
	srv := fakeV3Embedded(t, "'DOWN');", nil)
	defer srv.Close()
	ctx := structCtx(srv.URL)
	broken := flaskWithScript(strayParenLine)
	if msg := embeddedScriptGate(ctx, "app.py", broken, broken+"\n# one more try\n"); msg != "" {
		t.Errorf("pre-existing breakage must be allowed:\n%s", msg)
	}
}

// A fix must not be blocked by the gate that flagged the bug.
func TestEmbeddedScriptGateAllowsTheFix(t *testing.T) {
	srv := fakeV3Embedded(t, "'DOWN');", nil)
	defer srv.Close()
	ctx := structCtx(srv.URL)
	broken := flaskWithScript(strayParenLine)
	fixed := flaskWithScript(strings.Replace(strayParenLine, "'DOWN');", "'DOWN';", 1))
	if msg := embeddedScriptGate(ctx, "app.py", broken, fixed); msg != "" {
		t.Errorf("the corrected content must pass:\n%s", msg)
	}
}

// Fail-soft: nothing about an unavailable checker may block a write.
func TestEmbeddedScriptGateFailsSoft(t *testing.T) {
	broken := flaskWithScript(strayParenLine)

	// v3-service unreachable
	if msg := embeddedScriptGate(structCtx("http://127.0.0.1:0"), "app.py", "", broken); msg != "" {
		t.Errorf("unreachable v3 must fail soft: %s", msg)
	}
	// no V3 URL configured
	if msg := embeddedScriptGate(structCtx(""), "app.py", "", broken); msg != "" {
		t.Errorf("empty V3 URL must fail soft: %s", msg)
	}
	// nil ctx
	if msg := embeddedScriptGate(nil, "app.py", "", broken); msg != "" {
		t.Errorf("nil ctx must fail soft: %s", msg)
	}
	// grammar missing on the far side -> ok:false
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"tree-sitter javascript grammar not installed in this build"}`))
	}))
	defer unavailable.Close()
	if msg := embeddedScriptGate(structCtx(unavailable.URL), "app.py", "", broken); msg != "" {
		t.Errorf("ok:false must fail soft: %s", msg)
	}
	// 5xx from the service
	broke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", 500)
	}))
	defer broke.Close()
	if msg := embeddedScriptGate(structCtx(broke.URL), "app.py", "", broken); msg != "" {
		t.Errorf("5xx must fail soft: %s", msg)
	}
}

// File types that cannot carry an embedded script, and content with no script
// tag at all, must never reach the service.
func TestEmbeddedScriptSkipsWithoutScriptTag(t *testing.T) {
	calls := 0
	srv := fakeV3Embedded(t, "'DOWN');", &calls)
	defer srv.Close()
	ctx := structCtx(srv.URL)

	if _, ok := checkEmbeddedScript(ctx, "notes.md", flaskWithScript(strayParenLine), ""); !ok {
		t.Error("an extension that cannot carry a script must pass")
	}
	if _, ok := checkEmbeddedScript(ctx, "app.py", "def f():\n    return 1\n", ""); !ok {
		t.Error("python with no markup must pass")
	}
	if calls != 0 {
		t.Errorf("expected no network calls for unscripted content, got %d", calls)
	}
	// ...and the real thing still does call.
	if _, ok := checkEmbeddedScript(ctx, "app.py", flaskWithScript(strayParenLine), ""); ok {
		t.Error("a broken embedded script must be reported")
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 network call, got %d", calls)
	}
}

// checkFallbackSyntax carries the embedded check, so every write path already
// wired to it inherits the gate — with the sandbox reporting the file itself
// as perfectly valid Python.
func TestCheckFallbackSyntaxCarriesEmbeddedCheck(t *testing.T) {
	v3 := fakeV3Embedded(t, "'DOWN');", nil)
	defer v3.Close()
	sandbox := fakeSyntaxSandbox(t, "") // everything parses
	defer sandbox.Close()
	ctx := structCtx(v3.URL)
	ctx.SandboxURL = sandbox.URL

	synErr, ok := checkFallbackSyntax(ctx, "app.py", flaskWithScript(strayParenLine))
	if ok {
		t.Fatal("checkFallbackSyntax must report the embedded-script break")
	}
	if !strings.HasPrefix(synErr, embeddedScriptErrPrefix) {
		t.Errorf("embedded finding must be tagged for the rejection formatter: %q", synErr)
	}
	if _, clean := checkFallbackSyntax(ctx, "app.py", flaskWithScript("            let x = 1;")); !clean {
		t.Error("clean embedded script must pass checkFallbackSyntax")
	}
}

// The generic syntax rejection would tell the model to resend complete content
// or check its old_str — both wrong for a stray paren in embedded JavaScript.
// fallbackSyntaxRejection must hand the pre-formatted finding through instead.
func TestFallbackSyntaxRejectionPassesEmbeddedFindingThrough(t *testing.T) {
	v3 := fakeV3Embedded(t, "'DOWN');", nil)
	defer v3.Close()
	ctx := structCtx(v3.URL)
	synErr, _ := checkEmbeddedScript(ctx, "app.py", flaskWithScript(strayParenLine), "")

	msg := fallbackSyntaxRejection("app.py", flaskWithScript(strayParenLine), synErr)
	if strings.Contains(msg, "COMPLETE file content") || strings.Contains(msg, "cut off") {
		t.Errorf("must not give truncation advice for an embedded-script bug:\n%s", msg)
	}
	if !strings.Contains(msg, "unexpected `)`") || strings.Contains(msg, embeddedScriptErrPrefix) {
		t.Errorf("must hand back the unwrapped finding:\n%s", msg)
	}
	if _, isEmbedded := embeddedScriptRejectionFor("SyntaxError: invalid syntax"); isEmbedded {
		t.Error("a plain syntax error must not be treated as an embedded finding")
	}
}

// The CSS variant speaks to the stylesheet, not the script.
func TestEmbeddedStyleRejectionWording(t *testing.T) {
	msg := formatEmbeddedScriptRejection("templates/index.html", embeddedScriptFinding{
		Line: 4, Kind: "css", Where: "the <style> block",
		Message: "a `{` that is never closed",
		Hint:    "Close the rule with `}`.",
		Text:    "body { margin: 0;",
	})
	if !strings.Contains(msg, "CSS syntax error") || !strings.Contains(msg, "<style>") {
		t.Errorf("css finding must be described as css:\n%s", msg)
	}
	if !strings.Contains(msg, "unstyled") || !strings.Contains(msg, "HTML syntax is valid") {
		t.Errorf("css rejection must explain the browser-side breakage:\n%s", msg)
	}
}

// A CRLF file is normal and must not be flagged; bare or repeated CRs in an
// old_str are the signature of a model that degenerated partway through
// copying a block. One observed old_str carried runs of \r plus a literal
// `\rVert` — a LaTeX fragment — in the middle of JavaScript.
func TestStrayCarriageReturnsIgnoresWellFormedCRLF(t *testing.T) {
	crlf := "line one\r\nline two\r\nline three\r\n"
	if n := strayCarriageReturns(crlf); n != 0 {
		t.Errorf("CRLF text must not be flagged, got %d", n)
	}
}

func TestStrayCarriageReturnsCountsDegenerateRuns(t *testing.T) {
	degenerate := "        let gameActive = true;\n        \r\r\r\r\r\r\n        \rVert"
	if n := strayCarriageReturns(degenerate); n < 3 {
		t.Errorf("degenerate old_str must be flagged, got %d", n)
	}
}

func TestStrayCarriageReturnsCleanTextIsZero(t *testing.T) {
	if n := strayCarriageReturns("def f():\n    return 1\n"); n != 0 {
		t.Errorf("clean text must be 0, got %d", n)
	}
}

// Given a 1-line puzzle input, a model recognised the puzzle from training,
// wrote the canonical textbook example over the real input WITHOUT reading
// it, and solved the wrong data — honestly reporting "created input.txt with
// sample data". The >5-line rule did not apply and nothing else asked whether
// the file should be replaced at all.
func TestUnreadOverwriteIsRefusedRegardlessOfSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "input.txt")
	if err := os.WriteFile(path, []byte("3,4,3,1,2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier2Medium)

	if !isUnreadOverwrite(ctx, path, false, false) {
		t.Error("an existing, unread, not-session-owned file must be protected")
	}
	// Once read, replacing it is the model's call again.
	ctx.RecordFileRead(path, "3,4,3,1,2\n")
	if isUnreadOverwrite(ctx, path, false, false) {
		t.Error("after read_file the overwrite must be allowed through")
	}
}

func TestUnreadOverwriteAllowsSessionOwnedAndCorrupted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "draft.py")
	ctx := NewAgentContext(dir, Tier2Medium)

	// The agent's own draft: it must be able to correct its first pass.
	if isUnreadOverwrite(ctx, path, false, true) {
		t.Error("a session-owned file must stay overwritable")
	}
	// A corrupted file can only be repaired by full replacement.
	if isUnreadOverwrite(ctx, path, true, false) {
		t.Error("a corrupted file must stay overwritable")
	}
}

// The sandbox checks Python on 3.13, where `f"{d["k"]}"` (PEP 701) is valid,
// so an f-string error is a real problem inside the braces. The message used
// to say "this environment runs an older Python" and send the model after the
// quotes (audit P-tests-3/TEST#2). It still leads with the diagnosis, because
// a session once hit the wall clock when the advice sat in a parenthetical.
func TestFStringRejectionPointsInsideTheBraces(t *testing.T) {
	msg := fallbackSyntaxRejection("todo.py",
		"print(f\"{i}: {item[}\")\n",
		"SyntaxError: f-string: closing parenthesis '}' does not match opening parenthesis '[' (line 1)")
	if !strings.HasPrefix(msg, "Your content for todo.py has an f-string error") {
		t.Errorf("must lead with the diagnosis, got %q", msg)
	}
	for _, false_ := range []string{"3.12", "older Python", "the other quote"} {
		if strings.Contains(msg, false_) {
			t.Errorf("claims %q, which is not true on the 3.13 sandbox: %q", false_, msg)
		}
	}
	for _, want := range []string{"does not match opening parenthesis", "NOT written", "Do NOT resend"} {
		if !strings.Contains(msg, want) {
			t.Errorf("must carry %q, got %q", want, msg)
		}
	}
	general := fallbackSyntaxRejection("a.py", "def f(:\n    pass\n", "SyntaxError: invalid syntax (line 1)")
	if strings.Contains(general, "f-string") {
		t.Errorf("the general message still blames f-string quoting: %q", general)
	}
}

// A non-f-string syntax error keeps the general message.
func TestNonFStringSyntaxErrorKeepsGeneralAdvice(t *testing.T) {
	msg := fallbackSyntaxRejection("a.py", "def f(:\n    pass\n",
		"SyntaxError: invalid syntax (line 1)")
	if strings.Contains(msg, "f-string error") {
		t.Errorf("plain syntax error must not claim an f-string problem: %q", msg)
	}
}

// "unexpected character after line continuation character" names the
// mechanism, not the mistake. A model that has started emitting stray
// backslashes cannot act on it — observed three times in one session, all on
// the same file, until the wall clock ran out.
func TestStrayBackslashRejectionNamesTheCharacter(t *testing.T) {
	msg := fallbackSyntaxRejection("todo.py", "print(\"hi\") \\ x\n",
		"SyntaxError: unexpected character after line continuation character (line 1)")
	if !strings.Contains(msg, "stray backslash") {
		t.Errorf("must name the character, got %q", msg)
	}
	if !strings.Contains(msg, "LAST character on its line") {
		t.Errorf("must say when a backslash is legal, got %q", msg)
	}
}

// The f-string branch must survive the refactor that introduced lowerErr.
func TestFStringBranchStillFiresAfterBackslashBranch(t *testing.T) {
	msg := fallbackSyntaxRejection("a.py", "x=1\n",
		"SyntaxError: f-string: unmatched '['")
	if !strings.Contains(msg, "has an f-string error") {
		t.Errorf("f-string diagnosis lost: %q", msg)
	}
}

func TestPlainSyntaxErrorGetsNeitherSpecialCase(t *testing.T) {
	msg := fallbackSyntaxRejection("a.py", "def f(:\n", "SyntaxError: invalid syntax")
	if strings.Contains(msg, "stray backslash") || strings.Contains(msg, "f-string error") {
		t.Errorf("plain error must keep the general message: %q", msg)
	}
}

// planVerifyHint is what the verification gate shows the model when it
// refuses a `done`. With no plan, or a plan whose verify_step the planner
// left empty, the gate still has to name something concrete to run — an
// empty hint would render as "run  to prove it" and tell the model nothing.
func TestPlanVerifyHintFallsBackWhenThereIsNothingToName(t *testing.T) {
	generic := planVerifyHint(nil)
	if generic == "" {
		t.Fatal("nil plan must still yield a hint, not an empty string")
	}
	if got := planVerifyHint(&Plan{VerifyStep: ""}); got != generic {
		t.Errorf("empty verify_step must fall back to the generic hint, got %q", got)
	}
	if got := planVerifyHint(&Plan{VerifyStep: "pytest -q test_app.py"}); got != "pytest -q test_app.py" {
		t.Errorf("a real verify_step must be returned verbatim, got %q", got)
	}
}

// revisePlan's guards run before it ever reaches the planner. Both must
// return without emitting plan_revise: a nil plan has nothing to revise,
// and past the cap the loop would re-plan forever. The event matters
// because the TUI renders it — a stream event with no revision behind it
// shows the user work that did not happen.
func TestRevisePlanGuardsEmitNoEvent(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  *AgentContext
	}{
		{"nil plan", &AgentContext{Plan: nil}},
		{"at the revision cap", &AgentContext{
			Plan:          mkPlan(PlanStep{ID: "s1", Action: "read_file", Target: "a.py"}),
			PlanRevisions: planMaxRevisions,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []string
			tc.ctx.StreamFn = func(kind string, _ interface{}) { events = append(events, kind) }
			revisePlan(tc.ctx, "add a health endpoint", "off-plan thrash")
			if len(events) != 0 {
				t.Errorf("guard must return before streaming, got events %v", events)
			}
			if tc.ctx.PlanRevisions > planMaxRevisions {
				t.Errorf("revision counter advanced past the cap: %d", tc.ctx.PlanRevisions)
			}
		})
	}
}

// The failure this came from: a model read snake_app.py, then sent
// old_str="76\t const ctx = canvas.getContext('2d');" — the correct line with
// read_file's display prefix still attached. It got a generic "not found",
// tried again with the prefix still on, and gave up after three failures.
func TestLineNumberPrefixIsDetectedInOldStr(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want int
	}{
		{"the observed failure", "\n76\t const ctx = canvas.getContext('2d');", 1},
		{"bare prefix at start", "12\tfoo()", 1},
		{"several numbered lines", "10\ta\n11\tb\n12\tc", 3},
		{"indented before the number", "  7\tx = 1", 1},

		// Must NOT fire: these are legitimate file content.
		{"clean single line", "const ctx = canvas.getContext('2d');", 0},
		{"tab-indented code", "\tif (x) {", 0},
		{"a number with no tab", "76 const ctx", 0},
		{"digits mid-line", "const x = 76;\tconst y = 2;", 0},
		{"number after real text", "foo 12\tbar", 0},
		{"empty", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := lineNumberPrefixedLines(tc.in); got != tc.want {
				t.Errorf("lineNumberPrefixedLines(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// The live failure: the model went off-plan for five consecutive calls, the
// adherence gate re-planned, and the planner returned the SAME three steps
// (read_file app.py / structural_edit app.py / run_command python app.py).
// The off-plan streak reset and the same unfollowable plan kept bouncing it.
func TestAnIdenticalRevisionDropsThePlan(t *testing.T) {
	same := func() *Plan {
		return mkPlan(
			PlanStep{ID: "s1", Action: "read_file", Target: "app.py"},
			PlanStep{ID: "s2", Action: "structural_edit", Target: "app.py"},
		)
	}
	if !samePlanSteps(same(), same()) {
		t.Error("identical step lists must compare equal")
	}

	// Reworded rationale, same work — still the same plan.
	a, b := same(), same()
	a.Rationale, b.Rationale = "fix it", "repair the thing"
	if !samePlanSteps(a, b) {
		t.Error("prose differences must not read as a different plan")
	}

	// A genuinely different plan must not be swallowed.
	diff := mkPlan(
		PlanStep{ID: "s1", Action: "edit_file", Target: "app.py"},
		PlanStep{ID: "s2", Action: "run_command", Target: "pytest"},
	)
	if samePlanSteps(same(), diff) {
		t.Error("different actions must compare unequal")
	}
	if samePlanSteps(same(), mkPlan(PlanStep{ID: "s1", Action: "read_file", Target: "app.py"})) {
		t.Error("different step counts must compare unequal")
	}
	if samePlanSteps(nil, same()) || samePlanSteps(same(), nil) {
		t.Error("a nil plan is not the same as a plan")
	}
}

// The observed corruption: V3 regenerated the whole file after a
// structural_edit and, while retyping the parts the edit never touched,
// wrote `id="msg"` as `id=" msg"`. getElementById('msg') then returns null
// at runtime, and no syntax check can see it — a space inside an attribute
// value is valid HTML.
func TestV3CandidateThatRetypesUntouchedLinesIsRejected(t *testing.T) {
	original := "<h1 id=\"msg\">GAME OVER</h1>\nlet speed = 100;\n"
	edited := "<h1 id=\"msg\">GAME OVER</h1>\nlet speed = 120;\n"
	improved := "<h1 id=\" msg\">GAME OVER</h1>\nlet speed = 120;\n"

	drift := v3RewroteBeyondTheEdit(original, edited, improved)
	if drift == "" {
		t.Fatal("V3 rewrote a line outside the edit and it was accepted")
	}
	if !strings.Contains(drift, "id=") {
		t.Errorf("drift message should name the casualty, got %q", drift)
	}
}

func TestV3CandidateThatOnlyTouchesTheEditIsKept(t *testing.T) {
	original := "<h1 id=\"msg\">GAME OVER</h1>\nlet speed = 100;\n"
	edited := "<h1 id=\"msg\">GAME OVER</h1>\nlet speed = 120;\n"
	// V3 improving the edited line itself is the whole point of the pipeline.
	improved := "<h1 id=\"msg\">GAME OVER</h1>\nconst speed = 120;\n"

	if drift := v3RewroteBeyondTheEdit(original, edited, improved); drift != "" {
		t.Errorf("in-scope improvement rejected: %s", drift)
	}
}

func TestV3DriftGateFailsSoft(t *testing.T) {
	same := "a\nb\n"
	if v3RewroteBeyondTheEdit(same, same, "totally different\n") != "" {
		t.Error("a no-op edit should not engage the gate")
	}
	if v3RewroteBeyondTheEdit(same, "a\nc\n", "") != "" {
		t.Error("an empty candidate should not engage the gate")
	}
	// Blank lines carry no content; reflowing them is not a rewrite.
	if drift := v3RewroteBeyondTheEdit("a\n\nb\n", "a\n\nc\n", "a\nc\n"); drift != "" {
		t.Errorf("blank-line reflow flagged as drift: %s", drift)
	}
}

// The gate's second job: a render loop the edit stopped driving. Not a syntax
// error — the JavaScript parses, the server starts, the page returns 200, and
// the game draws exactly one frame. The finding only exists when the service
// gets the pre-edit file to compare against, so the gate must send it.
func TestEmbeddedScriptGateBlocksAStoppedRenderLoop(t *testing.T) {
	var sawPrevious string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct{ Source, Previous string }
		_ = json.Unmarshal(raw, &body)
		out := map[string]interface{}{"ok": true, "findings": []interface{}{}}
		if body.Previous != "" && strings.Contains(body.Source, "setTimeout(draw") {
			sawPrevious = body.Previous
			out["findings"] = []map[string]interface{}{{
				"line": 42, "column": 8, "kind": "javascript", "defect": "stopped_loop",
				"where":   "the <script> block inside the Python string HTML_TEMPLATE",
				"message": "`draw` used to run on a repeating timer and now runs once",
				"hint":    "Nothing schedules the next call, so the loop stops after one frame.",
				"text":    "setTimeout(draw, delay);",
			}}
		}
		b, _ := json.Marshal(out)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	ctx := structCtx(srv.URL)
	original := flaskWithScript("            setInterval(draw, 100);")
	edited := flaskWithScript("            setTimeout(draw, delay);")

	msg := embeddedScriptGate(ctx, "app.py", original, edited)
	if msg == "" {
		t.Fatal("gate must block an edit that leaves a render loop scheduled once")
	}
	if sawPrevious == "" {
		t.Error("the pre-edit file was never sent — the comparison cannot run")
	}
	for _, want := range []string{
		"stops a render loop", // named as what it is, not a syntax error
		"line 42",             // where
		"`draw`",              // which function
		"it was NOT written",  // nothing landed
		"still returns 200",   // why the server check missed it
		"freezes after one frame",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("rejection missing %q:\n%s", want, msg)
		}
	}
	// The syntax wording must not leak into it.
	if strings.Contains(msg, "syntax error") {
		t.Errorf("stopped-loop finding described as a syntax error:\n%s", msg)
	}
}

// A repeated let/const parses fine and then refuses to run, so the rejection
// must not read like a syntax error the model can hunt for with a parser.
func TestRedeclarationRejectionSaysTheWholeScriptIsDead(t *testing.T) {
	msg := formatEmbeddedScriptRejection("app.py", embeddedScriptFinding{
		Line: 207, Column: 4, Kind: "javascript", Defect: "redeclaration",
		Where:   "the <script> block inside the Python string HTML_TEMPLATE",
		Message: "`score` is declared twice in the same scope",
		Hint:    "Drop this declaration and use the existing `score`, or rename one of them.",
		Text:    "let score = 0;",
	})
	for _, want := range []string{
		"declares the same name twice",
		"line 207",
		"`score`",
		"it was NOT written",
		"refuses the script", // why nothing on the page works
		"still returns 200",  // why the server check missed it
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("rejection missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "syntax error") {
		t.Errorf("redeclaration described as a syntax error:\n%s", msg)
	}
}

// structural_edit on a 3-line index() was handed content carrying the
// module's `if __name__` block, so the splice appended a second one and the
// file went 209 -> 388 lines. Parses, runs, and the second block is dead code
// under a blocking app.run() — the signature of a whole-file blob smuggled
// through a node selector.
func TestDuplicateEntrypointIsRefused(t *testing.T) {
	original := "import flask\n\n\nif __name__ == \"__main__\":\n    app.run()\n"
	edited := original + "\n\nif __name__ == \"__main__\":\n    app.run(port=5001)\n"

	msg := duplicateMainGuard("app.py", original, edited)
	if msg == "" {
		t.Fatal("a second module entrypoint must be refused")
	}
	for _, want := range []string{"app.py", "2 `if __name__", "it was NOT written", "Only the first one ever runs"} {
		if !strings.Contains(msg, want) {
			t.Errorf("rejection missing %q:\n%s", want, msg)
		}
	}
}

func TestDuplicateEntrypointGuardLeavesEverythingElseAlone(t *testing.T) {
	one := "if __name__ == '__main__':\n    main()\n"
	two := one + "if __name__ == '__main__':\n    main()\n"

	if duplicateMainGuard("app.py", "x = 1\n", one) != "" {
		t.Error("adding the first entrypoint was refused")
	}
	if duplicateMainGuard("app.py", two, two+"print(1)\n") != "" {
		t.Error("a file that already had two was refused — not healthy->broken")
	}
	if duplicateMainGuard("app.js", "x\n", two) != "" {
		t.Error("fired on a non-Python file")
	}
	// Indented: a guard inside a function is not a module entrypoint.
	nested := "def go():\n    if __name__ == '__main__':\n        main()\n"
	if duplicateMainGuard("app.py", one, one+nested) != "" {
		t.Error("counted an indented guard as a second module entrypoint")
	}
}

// The gate must not tell a model to re-run a server in the foreground: it
// never exits, so "confirm it exits clean" can never be satisfied. An
// observed session burned all three bounces re-sending `done` against that
// advice after correctly starting the server with run_background.
func TestBlockedServerStartGetsProbeAdviceNotFixAdvice(t *testing.T) {
	wrote := func(stderr string) *ToolResult {
		data, _ := json.Marshal(RunCommandOutput{Stderr: stderr, ExitCode: 1, Outcome: ExecutionCompleted})
		return &ToolResult{Error: stderr, Data: data}
	}
	if !blockedServerStart(wrote("Address already in use\nPort 5001 is in use by another program.")) {
		t.Error("a port that is taken was not recognised as a blocked server start")
	}
	if blockedServerStart(wrote("AssertionError: expected 3, got 4")) {
		t.Error("a genuinely red test was mistaken for a server start")
	}

	msg := verificationRejection(true, true, "")
	for _, want := range []string{"run_background", "curl", "servers do not exit"} {
		if !strings.Contains(msg, want) {
			t.Errorf("advice missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "re-run the same command") {
		t.Errorf("still telling the model to re-run a blocking command:\n%s", msg)
	}

	// Already started in the background: point at the job, don't start another.
	withJob := verificationRejection(true, true, "abc123")
	if !strings.Contains(withJob, "abc123") || !strings.Contains(withJob, "do not start another copy") {
		t.Errorf("did not name the running job:\n%s", withJob)
	}

	// A real red test keeps the fix-it advice.
	red := verificationRejection(true, false, "")
	if !strings.Contains(red, "FAILED") || strings.Contains(red, "servers do not exit") {
		t.Errorf("red-test advice changed:\n%s", red)
	}
}

// tree-sitter reports a missing `}` where the parser gave up, which is past
// the end of the block that needs it. Observed live: "line 202: a `}` is
// missing" against `setInterval(draw, 100);` — a line the edit never touched.
// The model tried to fix that line, twice, and the breaker stopped the run.
func TestMissingCloserPointsAtTheBlockNotTheParserStop(t *testing.T) {
	msg := formatEmbeddedScriptRejection("app.py", embeddedScriptFinding{
		Line: 202, Kind: "javascript",
		Where:      "the <script> block inside the Python string HTML_TEMPLATE",
		Message:    "a `}` is missing",
		Text:       "setInterval(draw, 100); // Slightly faster speed for better playability",
		OpenedLine: 143,
		OpenedText: "function draw() {",
	})
	for _, want := range []string{
		"143 | function draw() {",
		"this block is never closed",
		"the parser gave up here",
		"belongs at the end of the block that opens on line 143",
		"not where it goes",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("rejection missing %q:\n%s", want, msg)
		}
	}
}

// Without an opener the old single-line rendering stands — a stray `)` is
// reported exactly where it is.
func TestSyntaxRejectionWithoutAnOpenerIsUnchanged(t *testing.T) {
	msg := formatEmbeddedScriptRejection("app.py", embeddedScriptFinding{
		Line: 7, Kind: "javascript",
		Where:   "the <script> block inside the Python string HTML_TEMPLATE",
		Message: "unexpected `)`",
		Hint:    "Nothing opened a `(` for it to close — delete the stray `)`.",
		Text:    "nextDirection = 'DOWN');",
	})
	if !strings.Contains(msg, "delete the stray `)`") {
		t.Errorf("hint dropped:\n%s", msg)
	}
	if strings.Contains(msg, "never closed") {
		t.Errorf("opener wording leaked into an unrelated finding:\n%s", msg)
	}
}

// The message has to explain WHY an uncalled function is a defect, because
// the model's own evidence said otherwise: it ran the new command, got exit
// 0, and concluded the feature worked.
func TestOrphanedAdditionsMessageExplainsWhyExitZeroLied(t *testing.T) {
	msg := orphanedAdditionsMessage(map[string][]orphanedSymbol{
		"todo.py": {{Name: "done_task", Line: 38}},
	})
	for _, want := range []string{
		"todo.py:38", "done_task",
		"A function nothing references cannot run",
		"still exits 0",
		"dispatch",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
}

// Both containers see the split as `/workspace`, so no value either holds can
// reveal it — the divergence is in the host bind. The only detection from
// inside is to write on one side and read from the other.
func TestWorkspaceAlignmentProbeIsFunctionalNotConfigural(t *testing.T) {
	dir := t.TempDir()

	// Sandbox reads back exactly what the proxy wrote: aligned.
	aligned := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// The probe writes a token file; echo whatever is on disk.
		data, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
		_ = body
		w.Header().Set("Content-Type", "application/json")
		out, _ := json.Marshal(map[string]string{"stdout": string(data)})
		_, _ = w.Write(out)
	}))
	defer aligned.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.SandboxURL = aligned.URL
	resetWorkspaceAlignmentCache()
	if problem := verifyWorkspaceAlignment(ctx); problem != "" {
		t.Errorf("aligned mounts reported as split: %s", problem)
	}

	// Sandbox sees a different filesystem: split.
	split := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stdout":""}`))
	}))
	defer split.Close()
	ctx2 := NewAgentContext(dir, Tier2Medium)
	ctx2.SandboxURL = split.URL
	resetWorkspaceAlignmentCache()
	problem := verifyWorkspaceAlignment(ctx2)
	if problem == "" {
		t.Fatal("a split workspace was not detected")
	}
	for _, want := range []string{"different directories", "atlas workspace align"} {
		if !strings.Contains(problem, want) {
			t.Errorf("message missing %q:\n%s", want, problem)
		}
	}

	// An unreachable sandbox is not evidence of a split — fail soft.
	ctx3 := NewAgentContext(dir, Tier2Medium)
	ctx3.SandboxURL = "http://127.0.0.1:1"
	resetWorkspaceAlignmentCache()
	if problem := verifyWorkspaceAlignment(ctx3); problem != "" {
		t.Errorf("an unreachable sandbox was reported as a split: %s", problem)
	}

	// The probe file must not be left behind.
	if _, err := os.Stat(filepath.Join(dir, ".atlas-mount-probe")); !os.IsNotExist(err) {
		t.Error("probe file left in the user's workspace")
	}
}

// The probe must be read at the path the proxy WROTE it to. Hardcoding
// /workspace made every session running in a sandbox_subdir look split:
// the proxy wrote /workspace/e2e/.atlas-mount-probe and the sandbox looked in
// the root. It refused 28 of 28 benchmark sessions before any of them ran.
func TestAlignmentProbeCarriesTheSubdirectory(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "e2e")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}

	var askedFor string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Code string }
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		askedFor = req.Code
		// Only the true location has the token; a read of the wrong path
		// returns nothing, which is what a real split looks like.
		out := map[string]string{"stdout": ""}
		if strings.Contains(req.Code, filepath.Join(sub, ".atlas-mount-probe")) {
			data, _ := os.ReadFile(filepath.Join(sub, ".atlas-mount-probe"))
			out["stdout"] = string(data)
		}
		enc, _ := json.Marshal(out)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(enc)
	}))
	defer srv.Close()

	ctx := NewAgentContext(sub, Tier2Medium)
	ctx.SandboxURL = srv.URL
	resetWorkspaceAlignmentCache()

	if problem := verifyWorkspaceAlignment(ctx); problem != "" {
		t.Errorf("aligned mounts under a subdirectory reported as split: %s", problem)
	}
	if !strings.Contains(askedFor, sub) {
		t.Errorf("sandbox was asked for the wrong path: %s", askedFor)
	}
}

// The measured chain: the model called write_file on a 2000-line input
// fixture, retyping it from memory; it degenerated into repeating one line
// ~50 times; the content-loop detector cut the stream mid-JSON at 601 chars;
// the parse failed; three identical retries ended the run. A sibling session
// got further and corrupted the fixture outright.
func TestEchoedWritesAreRefused(t *testing.T) {
	fixture := strings.Repeat("199\n202\n201\n203\n", 200) // ~3 KB of data

	if !echoesExistingFile(fixture, fixture) {
		t.Error("an exact rewrite of the file on disk was allowed")
	}
	// The collapse truncates, so by the time the stream is cut the content is
	// a partial copy — still an echo.
	if !echoesExistingFile(fixture, fixture[:1200]) {
		t.Error("a truncated retype was allowed")
	}
	// Genuine edits and unrelated content must pass.
	edited := fixture + "\n# a real change\n"
	if echoesExistingFile(fixture, edited) {
		t.Error("an append was mistaken for an echo")
	}
	if echoesExistingFile(fixture, strings.Repeat("def solve():\n    pass\n", 40)) {
		t.Error("unrelated content was mistaken for an echo")
	}
	// Short files prove nothing either way — a two-line config legitimately
	// gets rewritten with the same content.
	if echoesExistingFile("a = 1\n", "a = 1\n") {
		t.Error("fired on a file too short to be evidence")
	}
	// A short prefix of a long file is not evidence of a retype.
	if echoesExistingFile(fixture, fixture[:80]) {
		t.Error("fired on a prefix too short to be evidence")
	}

	msg := echoedWriteRejection("input.txt", false)
	for _, want := range []string{"input.txt", "already has", "read it at runtime", "replace_lines"} {
		if !strings.Contains(msg, want) {
			t.Errorf("rejection missing %q:\n%s", want, msg)
		}
	}
}

// "Failed to parse model response" was a symptom of the proxy cutting its own
// stream. The classifier inferred "truncated_tool — you hit the token cap,
// make the call smaller", which is the wrong diagnosis and the wrong
// instruction, so the model retried the same thing until the run died.
func TestAStreamCutIsDiagnosedFromTheCutNotTheWreckage(t *testing.T) {
	truncated := `{"type":"tool_call","name":"write_file","args":{"path":"input.txt","content":"199\n202\n201`

	cat, feedback := classifyParseFailure(truncated, "content_loop")
	if cat != "loop_cut" {
		t.Errorf("category = %q, want loop_cut", cat)
	}
	for _, want := range []string{"repeating itself", "NOT too long", "not the data"} {
		if !strings.Contains(feedback, want) {
			t.Errorf("feedback missing %q:\n%s", want, feedback)
		}
	}
	if strings.Contains(feedback, "token cap") && !strings.Contains(feedback, "NOT too long") {
		t.Errorf("still blaming the token cap:\n%s", feedback)
	}

	// Reasoning-budget cuts get their own diagnosis.
	if cat, _ := classifyParseFailure(truncated, "reasoning_budget"); cat != "reasoning_cut" {
		t.Errorf("category = %q, want reasoning_cut", cat)
	}
	// With no cut, the shape-based classification still applies.
	if cat, _ := classifyParseFailure(truncated, ""); cat != "truncated_tool" {
		t.Errorf("category = %q, want truncated_tool when the model stopped on its own", cat)
	}
}

// The gate refuses the submission, so the file on disk is unchanged — and a
// message saying "app.py has a JavaScript syntax error" sends the model
// hunting a bug in a file that is fine. Measured as an H2 false rejection on
// flask_pause rep 2: the refusal was correct, the wording was not.
func TestRejectionsBlameTheSubmissionNotTheFile(t *testing.T) {
	for _, f := range []embeddedScriptFinding{
		{Line: 104, Kind: "javascript", Where: "the <script> block", Message: "unexpected `else`", Text: "else if(x) {}"},
		{Line: 42, Kind: "javascript", Defect: "stopped_loop", Where: "the <script> block",
			Message: "`draw` used to run on a repeating timer and now runs once"},
		{Line: 7, Kind: "javascript", Defect: "redeclaration", Where: "the <script> block",
			Message: "`score` is declared twice in the same scope"},
	} {
		msg := formatEmbeddedScriptRejection("app.py", f)
		if !strings.HasPrefix(msg, "Your content for app.py") {
			t.Errorf("defect=%q blames the file rather than the submission:\n%s", f.Defect, msg)
		}
		if !strings.Contains(msg, "on disk is unchanged") {
			t.Errorf("defect=%q does not say the file is untouched:\n%s", f.Defect, msg)
		}
	}
}

// V3 generates candidates for the TASK, and on a multi-file job the task is
// not the file. Measured on "build me a snake game": the model wrote a
// correct 18-line index.html, V3 activated and replaced it with 149 lines of
// JavaScript containing zero HTML tags, and the session reported success.
// The in-pipeline smoke check could not see it — for .html it runs an HTML
// parser, and an HTML parser accepts any text at all.
func TestV3LanguageSwapIsRefused(t *testing.T) {
	html := "<!DOCTYPE html>\n<html lang=\"en\">\n<body><canvas id=\"c\"></canvas></body>\n</html>\n"
	js := "const canvas = document.getElementById(\"c\");\nconst ctx = canvas.getContext(\"2d\");\n"

	if why := v3SwappedTheLanguage("index.html", html, js); why == "" {
		t.Fatal("JavaScript replacing an HTML document must be refused")
	}
	// A legitimate HTML improvement passes.
	better := html + "<script src=\"game.js\"></script>\n"
	if why := v3SwappedTheLanguage("index.html", html, better); why != "" {
		t.Fatalf("a real HTML improvement was refused: %s", why)
	}
	// CSS losing all its rule blocks.
	if why := v3SwappedTheLanguage("style.css", "body { color: red; }", "not css at all"); why == "" {
		t.Fatal("CSS replaced by non-CSS must be refused")
	}
	// A file that never had the markers is left alone (no healthy->broken).
	if why := v3SwappedTheLanguage("index.html", "just text", js); why != "" {
		t.Fatalf("must only fire on a healthy->broken transition: %s", why)
	}
	// Languages without a rule are untouched.
	if why := v3SwappedTheLanguage("solve.py", "x = 1\n", "y = 2\n"); why != "" {
		t.Fatalf("unrelated extensions must not be gated: %s", why)
	}
	// An empty candidate is someone else's problem.
	if why := v3SwappedTheLanguage("index.html", html, ""); why != "" {
		t.Fatalf("empty candidate must not trip this gate: %s", why)
	}
}

// A form action or fetch() target with no declared route is a page that
// cannot work, and it must be answerable on its own so the exit gate can ask
// for it -- not buried among advisories about orphaned files. The single-list
// callers keep seeing everything.
func TestRouteContractFindingsNameTheUnmatchedTarget(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"app.py": "from flask import Flask, render_template\n" +
			"app = Flask(__name__)\n" +
			"@app.route('/', methods=['GET', 'POST'])\ndef index():\n" +
			"    return render_template('index.html')\n" +
			"@app.route('/update/<int:book_id>', methods=['POST'])\ndef update(book_id):\n" +
			"    return ''\n",
		"templates/index.html": "<form method=\"POST\" action=\"/add_book\"><input name=\"title\"></form>",
	})
	contract := routeContractFindings(root)
	if len(contract) != 1 || !strings.Contains(contract[0], `"/add_book"`) ||
		!strings.Contains(contract[0], "/update/<int:book_id>") {
		t.Fatalf("the unmatched submit target must be named with the routes that exist: %v", contract)
	}
	// The combined list still carries it, so existing callers see no change.
	if !findingsContaining(assetLintFindings(root), `"/add_book"`) {
		t.Error("assetLintFindings dropped the route-contract finding")
	}
	// The mutation-time note calls it a defect, not advice.
	ctx := NewAgentContext(root, Tier2Medium)
	note := assetLintNote(ctx)
	if !strings.Contains(note, "cannot submit") || !strings.Contains(note, "/add_book") {
		t.Errorf("the note must name the defect as one: %q", note)
	}

	// A matching target is not a finding.
	writeTree(t, root, map[string]string{
		"templates/index.html": "<form method=\"POST\" action=\"/\"><input name=\"title\"></form>",
	})
	if c := routeContractFindings(root); len(c) != 0 {
		t.Errorf("a target the routes serve was flagged: %v", c)
	}
	// With no declared routes the check cannot judge, and says nothing.
	writeTree(t, root, map[string]string{
		"app.py":               "print('no routes here')\n",
		"templates/index.html": "<form action=\"/anything\"></form>",
	})
	if c := routeContractFindings(root); len(c) != 0 {
		t.Errorf("flagged a target with no routes to judge against: %v", c)
	}
}

// --- the sandbox only accepts a workspace-relative filename -----------------
//
// Production's _safe_overlay_path refuses an absolute path, a "..", or a
// backslash name with HTTP 400, and this layer maps any non-200 to
// ValidationNotRun -- which is never ValidationPassed. The completion check
// passes a RESOLVED absolute path, so sending the caller's spelling unchanged
// made every deliverable's verdict NotRun and deliverablesDemonstrablyValid
// could never be satisfied: 35 consecutive sessions reported correct artifacts
// as unverified. The stubs below REJECT absolute paths exactly as the sandbox
// does; the pre-existing doubles accepted any filename, which is why this
// shipped green.

// sandboxLikeSyntaxStub mimics the real /syntax-check contract: it refuses a
// filename the sandbox would refuse, and records what it was sent.
func sandboxLikeSyntaxStub(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/syntax-check") {
			http.NotFound(w, r)
			return
		}
		var in struct {
			Code     string `json:"code"`
			Language string `json:"language"`
			Filename string `json:"filename"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		*seen = append(*seen, in.Filename)
		mu.Unlock()
		// _safe_overlay_path: absolute, "..", or backslash -> 400.
		if filepath.IsAbs(in.Filename) || strings.Contains(in.Filename, "..") ||
			strings.Contains(in.Filename, `\`) {
			http.Error(w, "unsafe overlay file path", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"valid": true, "errors": []string{}})
	}))
}

func TestWorkspaceRelativeNameMatchesWhatTheSandboxAccepts(t *testing.T) {
	ctx := NewAgentContext("/workspace/_reliability", Tier2Medium)
	for _, c := range []struct {
		in       string
		want     string
		wantOK   bool
	}{
		{"solve.py", "solve.py", true},                                        // already relative
		{"templates/index.html", "templates/index.html", true},                // scoping preserved
		{"/workspace/_reliability/solve.py", "solve.py", true},                // the completion path's spelling
		{"/workspace/_reliability/templates/index.html", "templates/index.html", true},
		{"/etc/passwd", "", false},                                            // outside the workspace
		{"../escape.py", "", false},                                           // escapes
	} {
		got, ok := workspaceRelativeName(ctx, c.in)
		if ok != c.wantOK || got != c.want {
			t.Errorf("workspaceRelativeName(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

// The production-path replay: a run whose deliverable is valid must be able to
// claim completion, even though the completion check addresses it absolutely.
func TestAValidDeliverableCanBeDemonstratedThroughAnAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "solve.py"), []byte("x = 1\nprint(x)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var seen []string
	srv := sandboxLikeSyntaxStub(t, &seen)
	defer srv.Close()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.SandboxURL = srv.URL

	// Claimable, and the reason says what the claim rests on: nothing ran
	// solve.py, so it is current and parses (deliverables_parse_only).
	ok, why := terminalCompletionAllowed(ctx, []string{"solve.py"})
	if !ok || why != "deliverables_parse_only" {
		t.Fatalf("terminalCompletionAllowed = (%v,%q), want (true,\"deliverables_parse_only\") — "+
			"a correct artifact must be claimable; sandbox saw filenames %q", ok, why, seen)
	}
	// And it must still be SCOPED: the filename has to arrive, relativised,
	// or the path-dependent checks (the Jinja parse) silently stop applying.
	if len(seen) == 0 || seen[0] != "solve.py" {
		t.Errorf("sandbox received filenames %q, want the relative \"solve.py\" — "+
			"dropping the filename would fix the 400 but disable path-scoped checks", seen)
	}
}

// A path with no safe relative form must not be sent: the check still runs
// (whole-file syntax is what completion needs) rather than failing as NotRun.
func TestAnUnrelativisablePathOmitsTheFilenameInsteadOfBeingRefused(t *testing.T) {
	var seen []string
	srv := sandboxLikeSyntaxStub(t, &seen)
	defer srv.Close()
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.SandboxURL = srv.URL

	out := sandboxSyntaxOutcome(ctx, "/etc/passwd.py", "x = 1\n")
	if out.Status != ValidationPassed {
		t.Errorf("status = %q, want passed — an unscopable path must still get a whole-file check", out.Status)
	}
	if len(seen) != 1 || seen[0] != "" {
		t.Errorf("sandbox received filenames %q, want one empty (omitted) filename", seen)
	}
}

// --- editing a file this session just wrote ---------------------------------
//
// aoc_slope (38eaa0a): the session wrote solve.py, the syntax gate quoted the
// offending line, and the model's correct one-line edit_file repair was refused
// "file not read yet". With no accepted way to change one line it re-sent the
// whole file, tripped the identical-call detector, and the run died without
// repairing the line. Ownership alone must NOT open the gate; identity must.

func TestAFileThisSessionWroteCanBeEditedWithoutARereadWhileUnchanged(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	target := filepath.Join(dir, "solve.py")
	const body = "def solve():\n    return 1\n"
	if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// The session's own record of that write (what writeFileRecorded leaves).
	observeDeliverable(ctx, ledgerKey(ctx, target), []byte(body), ValidationKindSyntax, ValidationPassed, "")

	if !editViewIsCurrent(ctx, target) {
		t.Fatal("a file this session wrote, unchanged on disk, must be editable without a re-read")
	}

	// STALENESS IS PRESERVED: change the bytes underneath (as a shell command
	// this run issued could) and the view is no longer current.
	if err := os.WriteFile(target, []byte("def solve():\n    return 2  # changed elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if editViewIsCurrent(ctx, target) {
		t.Error("the view must go stale once the bytes on disk differ from what the session wrote")
	}
	// A real read makes it current again.
	ctx.RecordFileRead(target, "def solve():\n    return 2  # changed elsewhere\n")
	if !editViewIsCurrent(ctx, target) {
		t.Error("a fresh read must make the view current")
	}
}

func TestAFileTheSessionNeverTouchedStillRequiresARead(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	target := filepath.Join(dir, "preexisting.py")
	if err := os.WriteFile(target, []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if editViewIsCurrent(ctx, target) {
		t.Error("a pre-existing file the session never read or wrote must still require read_file")
	}
	if editViewIsCurrent(ctx, filepath.Join(dir, "absent.py")) {
		t.Error("an absent file is not a current view")
	}
}

// A step that says "edit stats.py" is satisfied by editing stats.py.
//
// Measured on add_function, both reps: the plan's s1 was `edit_file` on
// stats.py, the model added the function with insert_after, the test it wrote
// passed -- and the exit gate still said "2 of 3 planned steps have landed,
// and these have not: s1: edit_file". The file HAD been edited. Two causes,
// both fixed here: the edit tools other than edit_file reported no target at
// all, and the matcher treated their names as unrelated operations.
func TestAnEditStepIsSatisfiedByAnyEditTool(t *testing.T) {
	plan := &Plan{Steps: []PlanStep{
		{ID: "s1", Action: "edit_file", Target: "stats.py"},
		{ID: "s2", Action: "write_file", Target: "test_stats.py"},
		{ID: "s3", Action: "run_command", Target: "python3 test_stats.py"},
	}}
	args := func(v interface{}) json.RawMessage { b, _ := json.Marshal(v); return b }

	for _, tool := range []string{"insert_after", "structural_edit", "replace_lines", "edit_file"} {
		satisfied := make([]bool, len(plan.Steps))
		got := matchPlanStep(plan, satisfied, tool, args(map[string]string{"path": "stats.py"}))
		if got != 0 {
			t.Errorf("%s on stats.py matched step %d, want s1 (0)", tool, got)
		}
	}

	// The target half still has to agree: the same tool on a different file
	// satisfies nothing. This is what stops the fix from loosening the gate.
	satisfied := make([]bool, len(plan.Steps))
	if got := matchPlanStep(plan, satisfied, "insert_after",
		args(map[string]string{"path": "unrelated.py"})); got != -1 {
		t.Errorf("insert_after on an unrelated file matched step %d, want no match", got)
	}

	// And a whole-file write is still not an edit: pinned by
	// TestActionMatchesTool, restated here because this fix is next to it.
	satisfied = make([]bool, len(plan.Steps))
	if got := matchPlanStep(plan, satisfied, "write_file",
		args(map[string]string{"path": "stats.py", "content": "x = 1\n"})); got == 0 {
		t.Error("write_file satisfied an edit_file step; that contract must hold")
	}
}

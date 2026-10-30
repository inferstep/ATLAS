package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- the structural-unresolved gate -------------------------------------------

// fakeStructuralCheck serves /internal/structural_check as v3-service answers
// it for the names these tests use: a call to one of them is unresolved unless
// the source imports it ("import <name>" ending a line).
func fakeStructuralCheck(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/structural_check" {
			http.NotFound(w, r)
			return
		}
		var body struct{ Source string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		unresolved := []string{}
		for _, name := range []string{"render_template", "helper_util"} {
			if strings.Contains(body.Source, name+"(") && !strings.Contains(body.Source, "import "+name+"\n") {
				unresolved = append(unresolved, name)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "unresolved": unresolved})
	}))
}

// The gate checks the whole file after the change and blocks only the names
// the change made unresolved: a name already unresolved before it is a repair
// in progress, the same healthy->broken rule as the syntax gate.
func TestEditIntroducesUnresolved(t *testing.T) {
	srv := fakeStructuralCheck(t)
	defer srv.Close()
	// No request context (Ctx nil), as on paths built without one: the gate
	// runs on a background context rather than panic.
	ctx := &AgentContext{V3URL: srv.URL, WorkingDir: "/workspace"}
	for _, c := range []struct {
		name, path, original, edited string
		want                         []string
	}{
		{"a new call to a name the file never imports", "app.py",
			"from flask import render_template_string\ndef i(): return 'x'\n",
			"from flask import render_template_string\ndef i(): return render_template('i.html')\n",
			[]string{"render_template"}},
		{"a name unresolved before the edit", "app.py",
			"def a(): return render_template('a')\n",
			"def a(): return render_template('a')\ndef b(): return render_template('b')\n",
			nil},
		{"an import elsewhere in the file credits the new call", "app.py",
			"from utils import helper_util\n\ndef old():\n    return 1\n",
			"from utils import helper_util\n\ndef old():\n    return helper_util()\n",
			nil},
		{"deleting an import a remaining call needs", "app.py",
			"from utils import helper_util\n\ndef index():\n    return helper_util()\n",
			"def index():\n    return helper_util()\n",
			[]string{"helper_util"}},
		{"a file that is not Python is not checked", "notes.txt",
			"x\n", "y = render_template('i.html')\n",
			nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := editIntroducesUnresolved(ctx, c.path, c.original, c.edited); !slices.Equal(got, c.want) {
				t.Errorf("introduced %v, want %v", got, c.want)
			}
		})
	}
}

// The gate blocks only on a positive answer. With the service unreachable it
// lets the edit through, and so it does when the check of the original fails
// twice (once, then one retry): with no baseline, counting every unresolved
// name as new would block a model that fixes one error at a time.
func TestTheStructuralGateFailsOpen(t *testing.T) {
	unreachable := &AgentContext{V3URL: "http://127.0.0.1:0"}
	if got := editIntroducesUnresolved(unreachable, "app.py", "x", "y = render_template('i.html')"); got != nil {
		t.Errorf("an unreachable service blocked the edit: %v", got)
	}

	originalChecks := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Source string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.Contains(body.Source, "# original") {
			originalChecks++
			_, _ = w.Write([]byte(`{"ok":false,"error":"transient failure"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"unresolved":["helper_x"]}`))
	}))
	defer srv.Close()
	ctx := &AgentContext{V3URL: srv.URL}
	got := editIntroducesUnresolved(ctx, "app.py", "# original\ndef a():\n    return helper_x()\n", "def fixed():\n    return helper_x()\n")
	if got != nil {
		t.Errorf("introduced %v, want nothing", got)
	}
	if originalChecks != 2 {
		t.Errorf("%d checks of the original, want 2 (one retry)", originalChecks)
	}
}

// The project context sent with the check leaves out the file being checked:
// its pre-edit body would credit a top-level def the edit just deleted.
func TestStructuralCheckExcludesEditedSelf(t *testing.T) {
	var sent map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ProjectContext map[string]interface{} `json:"project_context"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		sent = body.ProjectContext
		_, _ = w.Write([]byte(`{"ok":true,"unresolved":[]}`))
	}))
	defer srv.Close()
	ctx := &AgentContext{
		V3URL: srv.URL, Ctx: context.Background(), WorkingDir: "/workspace",
		FilesRead:     map[string]string{"/workspace/app.py": "def gone(): pass", "/workspace/util.py": "def helper(): pass"},
		FileReadTimes: map[string]time.Time{"/workspace/app.py": time.Now(), "/workspace/util.py": time.Now()},
	}
	_, _ = checkStructuralUnresolved(ctx, "/workspace/app.py", "x = gone()")
	if _, present := sent["app.py"]; present {
		t.Error("the checked file app.py was sent as its own project context")
	}
	if _, present := sent["util.py"]; !present {
		t.Errorf("the other file read, util.py, was not sent: %v", sent)
	}
}

// The refusal names the calls and the recovery, in the operation the model
// issued: an "edit" steer on a refused new-file write sends the model to
// edit_file a file that does not exist.
func TestStructuralRejections(t *testing.T) {
	for _, c := range []struct {
		name, msg, opening      string
		recovery, otherRecovery string
	}{
		{"edit", structuralRejection("app.py", []string{"render_template"}),
			"edit for app.py calls `render_template`, which", "re-issue the edit.", "re-issue the write_file"},
		{"write_file", structuralWriteRejection("app.py", []string{"render_template"}),
			"write_file for app.py calls `render_template`, which", "re-issue the write_file", "re-issue the edit"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !strings.HasPrefix(c.msg, c.opening) {
				t.Errorf("must open with %q:\n%s", c.opening, c.msg)
			}
			if !strings.Contains(c.msg, "NameError") {
				t.Errorf("must name the NameError:\n%s", c.msg)
			}
			if !strings.Contains(c.msg, c.recovery) {
				t.Errorf("must say %q:\n%s", c.recovery, c.msg)
			}
			if strings.Contains(c.msg, c.otherRecovery) {
				t.Errorf("must not say %q:\n%s", c.otherRecovery, c.msg)
			}
		})
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

// --- the embedded-script gate -------------------------------------------------

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

// The gate refuses a change that newly breaks the JavaScript inside a file,
// which the Python check, the server start and a 200 from curl all miss. The
// refusal carries the service's line, message and hint, unwrapped.
func TestEmbeddedScriptGateRefusesAnEditThatBreaksTheScript(t *testing.T) {
	srv := fakeV3Embedded(t, "'DOWN');", nil)
	defer srv.Close()

	msg := embeddedScriptGate(&AgentContext{V3URL: srv.URL}, "app.py",
		flaskWithScript("            let x = 1;"), flaskWithScript(strayParenLine))
	opening := "Your content for app.py has a JavaScript syntax error in the <script> block inside the Python string HTML_TEMPLATE"
	if !strings.HasPrefix(msg, opening) {
		t.Errorf("the refusal must open with %q:\n%s", opening, msg)
	}
	for _, want := range []string{
		"line 7: unexpected `)`",
		"  7 | " + strings.TrimSpace(strayParenLine),
		"delete the stray `)`",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal missing %q:\n%s", want, msg)
		}
	}
}

// Everything else goes through: a script already broken before the change is a
// repair in progress, and a checker that cannot answer never blocks a write.
func TestEmbeddedScriptGateLetsTheWriteThrough(t *testing.T) {
	withFinding := fakeV3Embedded(t, "'DOWN');", nil)
	defer withFinding.Close()
	notAnalysable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"tree-sitter javascript grammar not installed in this build"}`))
	}))
	defer notAnalysable.Close()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", 500)
	}))
	defer failing.Close()

	broken := flaskWithScript(strayParenLine)
	fixed := flaskWithScript(strings.Replace(strayParenLine, "'DOWN');", "'DOWN';", 1))
	for _, c := range []struct {
		name             string
		ctx              *AgentContext
		original, edited string
	}{
		{"a script that was already broken", &AgentContext{V3URL: withFinding.URL}, broken, broken + "\n# one more try\n"},
		{"the fix of a broken script", &AgentContext{V3URL: withFinding.URL}, broken, fixed},
		{"an unreachable service", &AgentContext{V3URL: "http://127.0.0.1:0"}, "", broken},
		{"no service configured", &AgentContext{}, "", broken},
		{"no agent context", nil, "", broken},
		{"a service that cannot analyse the content", &AgentContext{V3URL: notAnalysable.URL}, "", broken},
		{"a service that fails", &AgentContext{V3URL: failing.URL}, "", broken},
	} {
		t.Run(c.name, func(t *testing.T) {
			if msg := embeddedScriptGate(c.ctx, "app.py", c.original, c.edited); msg != "" {
				t.Errorf("refused the write:\n%s", msg)
			}
		})
	}
}

// A render loop the edit stopped driving parses, the server starts, the page
// returns 200, and the game draws one frame. The service finds it only by
// comparing with the file before the edit, so the gate sends that file along.
func TestEmbeddedScriptGateBlocksAStoppedRenderLoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Source, Previous string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		out := map[string]interface{}{"ok": true, "findings": []interface{}{}}
		if strings.Contains(body.Previous, "setInterval(draw") && strings.Contains(body.Source, "setTimeout(draw") {
			out["findings"] = []map[string]interface{}{{
				"line": 42, "kind": "javascript", "defect": "stopped_loop",
				"where":   "the <script> block inside the Python string HTML_TEMPLATE",
				"message": "`draw` used to run on a repeating timer and now runs once",
			}}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	msg := embeddedScriptGate(&AgentContext{V3URL: srv.URL}, "app.py",
		flaskWithScript("            setInterval(draw, 100);"), flaskWithScript("            setTimeout(draw, delay);"))
	if !strings.HasPrefix(msg, "Your content for app.py stops a render loop") {
		t.Errorf("the stopped loop was not refused as one:\n%s", msg)
	}
}

// File types that cannot carry an embedded script, and content with no script
// tag at all, must never reach the service.
func TestEmbeddedScriptSkipsWithoutScriptTag(t *testing.T) {
	calls := 0
	srv := fakeV3Embedded(t, "'DOWN');", &calls)
	defer srv.Close()
	ctx := &AgentContext{V3URL: srv.URL}

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
	ctx := &AgentContext{V3URL: v3.URL, SandboxURL: sandbox.URL}

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

// --- the V3 candidate checks --------------------------------------------------

// V3 regenerates the whole file an edit composed, and a model retyping the
// lines the edit never touched drifts: `<h1 id="msg">` came back as
// `<h1 id=" msg">`, valid HTML that makes getElementById('msg') return null.
// A line the edit kept must survive intact.
func TestV3RewroteBeyondTheEdit(t *testing.T) {
	for _, c := range []struct {
		name, original, edited, improved string
		want                             string
	}{
		{"a candidate that retyped a line the edit never touched",
			"<h1 id=\"msg\">GAME OVER</h1>\nlet speed = 100;\n",
			"<h1 id=\"msg\">GAME OVER</h1>\nlet speed = 120;\n",
			"<h1 id=\" msg\">GAME OVER</h1>\nlet speed = 120;\n",
			`it rewrote a line the edit never touched: "<h1 id=\"msg\">GAME OVER</h1>"`},
		{"a candidate that improved the edited line itself",
			"<h1 id=\"msg\">GAME OVER</h1>\nlet speed = 100;\n",
			"<h1 id=\"msg\">GAME OVER</h1>\nlet speed = 120;\n",
			"<h1 id=\"msg\">GAME OVER</h1>\nconst speed = 120;\n",
			""},
		{"an edit that changed nothing", "a\nb\n", "a\nb\n", "totally different\n", ""},
		{"an empty candidate", "a\nb\n", "a\nc\n", "", ""},
		{"blank lines carry no content: a candidate that drops them", "a\n\nb\n", "a\n\nc\n", "a\nc", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := v3RewroteBeyondTheEdit(c.original, c.edited, c.improved); got != c.want {
				t.Errorf("v3RewroteBeyondTheEdit = %q, want %q", got, c.want)
			}
		})
	}
}

// V3 generates for the task, and on a multi-file job the task is not the file:
// a correct 18-line index.html was once replaced with 149 lines of JavaScript,
// which an HTML parser accepts. A candidate must still be in its file's
// language; only a change from having the markers to lacking them is refused.
func TestV3SwappedTheLanguage(t *testing.T) {
	html := "<!DOCTYPE html>\n<html lang=\"en\">\n<body><canvas id=\"c\"></canvas></body>\n</html>\n"
	js := "const canvas = document.getElementById(\"c\");\nconst ctx = canvas.getContext(\"2d\");\n"
	for _, c := range []struct {
		name, path, original, improved string
		want                           string
	}{
		{"JavaScript in place of an HTML document", "index.html", html, js,
			"the candidate contains no HTML tags at all, so it is not an HTML document"},
		{"a better HTML document", "index.html", html, html + "<script src=\"game.js\"></script>\n", ""},
		{"CSS that lost all its rule blocks", "style.css", "body { color: red; }", "not css at all",
			"the candidate contains no CSS rule blocks"},
		{"a file that never had the markers", "index.html", "just text", js, ""},
		{"a language with no rule", "solve.py", "x = 1\n", "y = 2\n", ""},
		{"an empty candidate", "index.html", html, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := v3SwappedTheLanguage(c.path, c.original, c.improved); got != c.want {
				t.Errorf("v3SwappedTheLanguage = %q, want %q", got, c.want)
			}
		})
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

// --- the sandbox only accepts a workspace-relative filename -----------------
//
// Production's _safe_overlay_path refuses an absolute path, a "..", or a
// backslash name with HTTP 400, and this layer maps any non-200 to
// ValidationNotRun -- which is never ValidationPassed. The completion check
// passes a RESOLVED absolute path, so sending the caller's spelling unchanged
// made every deliverable's verdict NotRun and deliverablesDemonstrablyValid
// could never be satisfied: 35 consecutive sessions reported correct artifacts
// as unverified. The stub below REJECTS absolute paths exactly as the sandbox
// does.

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
		_ = json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		*seen = append(*seen, in.Filename)
		mu.Unlock()
		// _safe_overlay_path: absolute, "..", or backslash -> 400.
		if filepath.IsAbs(in.Filename) || strings.Contains(in.Filename, "..") ||
			strings.Contains(in.Filename, `\`) {
			http.Error(w, "unsafe overlay file path", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"valid": true, "errors": []string{}})
	}))
}

func TestWorkspaceRelativeNameMatchesWhatTheSandboxAccepts(t *testing.T) {
	ctx := NewAgentContext("/workspace/_reliability", Tier2Medium)
	for _, c := range []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"solve.py", "solve.py", true},                         // already relative
		{"templates/index.html", "templates/index.html", true}, // scoping preserved
		{"/workspace/_reliability/solve.py", "solve.py", true}, // the completion path's spelling
		{"/workspace/_reliability/templates/index.html", "templates/index.html", true},
		{"/etc/passwd", "", false},  // outside the workspace
		{"../escape.py", "", false}, // escapes
	} {
		t.Run(c.in, func(t *testing.T) {
			got, ok := workspaceRelativeName(ctx, c.in)
			if ok != c.wantOK || got != c.want {
				t.Errorf("workspaceRelativeName(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.wantOK)
			}
		})
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

// --- code outside the gates files ---------------------------------------------

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

func TestBuildSystemPromptIncludesPlan(t *testing.T) {
	plan := &Plan{
		Steps: []PlanStep{
			{ID: "s1", Action: "read_file", Target: "app.py", Why: "inspect current routes"},
			{ID: "s2", Action: "edit_file", Target: "app.py", Why: "add /hello route"},
			{ID: "s3", Action: "run_command", Target: "curl http://localhost:5000/hello", Why: "verify"},
		},
		VerifyStep: "s3",
		Rationale:  "investigate, change, verify.",
	}
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

// postOrphanCheck asks the V3 service which added functions nothing calls.
// Only an answer counts: a refused or unreachable service gives no orphans,
// so a service that did not answer never stops the agent at its exit.
func TestPostOrphanCheckTrustsOnlyAnAnswer(t *testing.T) {
	answered := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var asked map[string]string
		_ = json.NewDecoder(r.Body).Decode(&asked)
		if r.URL.Path != "/internal/orphaned_symbols" ||
			!reflect.DeepEqual(asked, map[string]string{"path": "todo.py", "previous": "old", "source": "new"}) {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"orphans": [{"name": "done_task", "line": 12}]}`))
	}))
	defer answered.Close()
	// The refusal carries orphans in its body: they must not be read.
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"orphans": [{"name": "done_task", "line": 12}]}`))
	}))
	defer refused.Close()
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachable.Close()

	for _, c := range []struct {
		name string
		url  string
		want []orphanedSymbol
	}{
		{"answered", answered.URL, []orphanedSymbol{{Name: "done_task", Line: 12}}},
		{"refused with 503", refused.URL, nil},
		{"unreachable", unreachable.URL, nil},
	} {
		got := postOrphanCheck(&AgentContext{V3URL: c.url}, "todo.py", "old", "new")
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

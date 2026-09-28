package main

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// A declared output whose syntax check could not run is not a check that
// passed. The hard vetoes were computed before the authorization decision and
// before the structural classification that records "not run", so the veto
// for it could never fire, and an automatic delivery landed on the declared
// output anyway (audit P-delivery/INTEGRITY#4). They are computed after both
// now, once, for the automatic eligibility and the policy alike.
func TestAnAutomaticCandidateDoesNotLandWhenItsDeclaredCheckNeverRan(t *testing.T) {
	w := newAutomaticWorld(t, workContract, routeWinner, nil, true)
	origin, err := url.Parse(w.ctx.SandboxURL)
	if err != nil {
		t.Fatal(err)
	}
	forward := httputil.NewSingleHostReverseProxy(origin)
	checkerDown := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/syntax-check") {
			http.Error(rw, "unavailable", http.StatusServiceUnavailable)
			return
		}
		forward.ServeHTTP(rw, r)
	}))
	t.Cleanup(checkerDown.Close)
	w.ctx.SandboxURL = checkerDown.URL

	w.write(t)
	if got := w.disk(t); got == routeWinner {
		t.Fatal("the candidate landed although its declared syntax check never ran")
	}
}

// A declared command was staged only after a syntax obligation had been
// observed, and only extensions the syntax gate checks carry one. A declared
// main.rs with a declared command therefore never ran it: the command was
// owed, the route never staged it, and the candidate could never be
// authorized (audit P-evidence/DEAD#2). Staging no longer waits for a syntax
// obligation.
func TestADeclaredCommandIsStagedForAnUncheckedFileType(t *testing.T) {
	const command = "cargo run -q"
	const rustWinner = "fn main() {\n    println!(\"7\");\n}\n"
	const rustBaseline = "fn main() {\n    println!(\"6\");\n}\n"
	w := newAutomaticWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["main.rs"],`+
			`"verification_knowledge":"declared","verification":["`+command+`"]}`,
		rustWinner, map[string]stubEffect{command: {ExitCode: 0}}, true)
	w.path = filepath.Join(w.dir, "main.rs")

	if _, err := writeFileWithV3(w.path, rustBaseline, w.ctx); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if n := w.shell.runsOf(command); n != 1 {
		t.Fatalf("the declared command ran %d times, want once", n)
	}
	if got := w.disk(t); got != rustWinner {
		t.Errorf("the candidate did not land on its passing declared command; disk holds %q", got)
	}
}

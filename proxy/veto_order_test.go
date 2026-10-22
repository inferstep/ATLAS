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
// passed. The hard vetoes are computed after the authorization decision and
// after the structural classification that records "not run", once, for the
// automatic eligibility and the policy alike; computed before them, the veto
// could never fire.
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

// A declared command is staged even for a file type the syntax gate does not
// check. Only checked extensions carry a syntax obligation, so if staging
// waited for one, a declared main.rs would never run its declared command and
// its candidate could never be authorized.
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

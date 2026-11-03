package main

import (
	"slices"
	"strings"
	"testing"
)

func TestClaimsUniversal(t *testing.T) {
	for _, c := range []struct {
		summary string
		want    bool
	}{
		{"All routes are functioning properly.", true},
		{"Fixed all bugs in the routing layer.", true},
		{"Everything works as expected.", true},
		{"No errors remaining.", true},
		{"Tested all endpoints — all green.", true},
		{"App is fully functional.", true},

		{"Added /admin route. Run the test suite to confirm the rest still works.", false},
		{"Created the missing template for /pricing.", false},
		{"Updated the readme.", false},
		{"Fixed the /a route.", false},
		{"", false},
	} {
		t.Run(c.summary, func(t *testing.T) {
			if got := claimsUniversal(c.summary); got != c.want {
				t.Errorf("claimsUniversal(%q) = %v, want %v", c.summary, got, c.want)
			}
		})
	}
}

func TestPromptIsMultiIssue(t *testing.T) {
	for _, c := range []struct {
		prompt string
		want   bool
	}{
		{"there are LOTS of issues with the flask app", true},
		{"a ton of bugs in this code", true},
		{"fix all the bugs", true},
		{"the routes don't work — fix everything", true},
		{"multiple problems here", true},
		{"it doesn't work", true},
		{"all routes are broken", true},
		{"nothing works", true},
		{"can you fix the bugs?", true},

		{"add a /admin route to the flask app", false},
		{"fix the typo on line 42", false},
		{"create a new endpoint for /health", false},
		{"why does index.html return 500?", false},
		{"what does this function do?", false},
	} {
		t.Run(c.prompt, func(t *testing.T) {
			if got := promptIsMultiIssue(c.prompt); got != c.want {
				t.Errorf("promptIsMultiIssue(%q) = %v, want %v", c.prompt, got, c.want)
			}
		})
	}
}

// The claim check bounces a `done` on a template a .py file renders that does
// not exist: a guaranteed 500 at runtime. The bounce lists each gap.
func TestVerifyCompletionClaimsBouncesAMissingTemplate(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"app.py": "@app.route('/')\ndef index(): return render_template('index.html')\n" +
			"@app.route('/pricing')\ndef pricing(): return render_template('pricing.html')\n" +
			"@app.route('/contact')\ndef contact(): return render_template('contact.html')\n" +
			"@app.route('/admin')\ndef admin(): return render_template('admin.html')\n",
		"templates/index.html": "ok",
	})
	gaps := []string{
		`app.py references template "admin.html", but templates/admin.html does not exist.`,
		`app.py references template "contact.html", but templates/contact.html does not exist.`,
		`app.py references template "pricing.html", but templates/pricing.html does not exist.`,
	}
	if got := completionClaimGaps(dir); !slices.Equal(got, gaps) {
		t.Errorf("gaps:\n%q\nwant:\n%q", got, gaps)
	}
	report := verifyCompletionClaims(dir)
	if !strings.Contains(report, strings.Join(gaps, "\n")) {
		t.Errorf("the bounce does not list the gaps:\n%s", report)
	}
	if !strings.Contains(report, "before declaring done") {
		t.Errorf("the bounce does not ask for them before done:\n%s", report)
	}
}

// Only that gap bounces a `done`.
func TestVerifyCompletionClaimsPassesWithoutAMissingTemplate(t *testing.T) {
	for _, c := range []struct {
		name  string
		files map[string]string
	}{
		{"every rendered template exists", map[string]string{
			"app.py":               "@app.route('/')\ndef index(): return render_template('index.html')\n",
			"templates/index.html": "ok",
		}},
		{"a render call inside venv/ is not the project's",
			map[string]string{"venv/lib/junk.py": "render_template('ghost.html')"}},
		{"a template named by a variable cannot be checked",
			map[string]string{"app.py": "def view(name): return render_template(name)\n"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTree(t, dir, c.files)
			if report := verifyCompletionClaims(dir); report != "" {
				t.Errorf("no gap, but the done was bounced:\n%s", report)
			}
		})
	}
}

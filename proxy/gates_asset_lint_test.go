package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeTree writes each file of files under root, creating its directories.
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

// findingsContaining reports whether any finding contains substr.
func findingsContaining(findings []string, substr string) bool {
	for _, f := range findings {
		if strings.Contains(f, substr) {
			return true
		}
	}
	return false
}

// Each workspace below gives exactly the findings listed: advisory ones sorted,
// then the route-contract ones. A quiet workspace gives none.
func TestAssetLintFindings(t *testing.T) {
	for _, c := range []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"a template and a static script nothing loads, beside a page built inline",
			map[string]string{
				"app.py": "from flask import Flask, render_template_string\n" +
					"@app.route('/')\ndef index():\n    return render_template_string(\"<html>inline</html>\")\n",
				"templates/index.html": "<html><body>snake</body></html>",
				"static/game.js":       "console.log('game');",
			},
			[]string{
				`static/game.js is referenced by nothing (no <script src>, <link href>, or url_for('static', ...) names it).`,
				`templates/index.html is referenced by nothing (no render_template call or include names "index.html").` +
					` A .py file builds its page inline with render_template_string instead — either render this template or delete it.`,
			}},
		{"files that reference each other",
			map[string]string{
				"app.py":               "@app.route('/')\ndef index():\n    return render_template('index.html')\n",
				"templates/index.html": "<html><script src=\"/static/game.js\"></script></html>",
				"static/game.js":       "console.log('game');",
			},
			nil},
		{"references to files that do not exist; external, anchor and templated values are not files",
			map[string]string{
				"templates/index.html": "<html><script src=\"/static/missing.js\"></script>" +
					"<link href=\"https://cdn.example.com/x.css\"><a href=\"#top\">top</a><img src=\"{{ asset_path }}\"></html>",
				"app.py": "render_template('index.html')\nx = url_for('static', filename='also-missing.css')\n",
			},
			[]string{
				`app.py references url_for('static', filename="also-missing.css"), but static/also-missing.css does not exist.`,
				`templates/index.html references "/static/missing.js", which does not exist in the workspace.`,
			}},
		{"a flat layout: a script and a stylesheet no page loads",
			map[string]string{
				"index.html": "<html><script>function calculate(){}</script></html>",
				"calc.js":    "function calculate(expression) { return 1; }",
				"style.css":  "body { margin: 0; }",
			},
			[]string{
				"calc.js is referenced by nothing — if a page should load it, add the <script src>/<link href>; if its content was inlined instead, delete the file.",
				"style.css is referenced by nothing — if a page should load it, add the <script src>/<link href>; if its content was inlined instead, delete the file.",
			}},
		{"a flat layout whose page loads its script and stylesheet",
			map[string]string{
				"index.html": "<html><script src=\"calc.js\"></script><link href=\"style.css\"></html>",
				"calc.js":    "function calculate(){}",
				"style.css":  "body{}",
			},
			nil},
		{"with no HTML at all, a library's entry file is not an orphan",
			map[string]string{"index.js": "module.exports = 1;"},
			nil},
		{"a template that a .py file renders, or a template extends, is missing",
			map[string]string{
				"app.py": "@app.route('/')\ndef i(): return render_template('index.html')\n" +
					"@app.errorhandler(404)\ndef nf(e): return render_template('404.html'), 404\n",
				"templates/index.html": "<html>{% extends \"base.html\" %}</html>",
			},
			[]string{
				`app.py references template "404.html", but templates/404.html does not exist.`,
				`templates/index.html references template "base.html", but templates/base.html does not exist.`,
			}},
		{"a fetch() target no route serves; a routed fetch and form action are not findings",
			map[string]string{
				"app.py": "@app.route('/')\ndef i(): return render_template('index.html')\n" +
					"@app.route('/add', methods=['POST'])\ndef a(): return ''\n" +
					"@app.route('/delete/<int:todo_id>', methods=['POST'])\ndef d(todo_id): return ''\n",
				"templates/index.html": "<html><script src=\"/static/todo.js\"></script><form action=\"/add\"></form></html>",
				"static/todo.js":       "fetch('/api/todos');\nfetch(`/delete/${id}`, {method:'POST'});\n",
			},
			[]string{`static/todo.js calls "/api/todos", but no Flask route matches it (routes: /, /add, /delete/<int:todo_id>).`}},
		{"a form action no route serves is named with the routes that exist; one to / is served",
			map[string]string{
				"app.py": "@app.route('/', methods=['GET', 'POST'])\ndef index():\n    return render_template('index.html')\n" +
					"@app.route('/update/<int:book_id>', methods=['POST'])\ndef update(book_id):\n    return ''\n",
				"templates/index.html": "<form method=\"POST\" action=\"/add_book\"></form><form method=\"POST\" action=\"/\"></form>",
			},
			[]string{`templates/index.html calls "/add_book", but no Flask route matches it (routes: /, /update/<int:book_id>).`}},
		{"with no declared routes the route check cannot judge, and says nothing",
			map[string]string{
				"app.py":               "print('no routes here')\n",
				"templates/index.html": "<form action=\"/anything\"></form>",
			},
			[]string{`templates/index.html is referenced by nothing (no render_template call or include names "index.html").`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, c.files)
			if got := assetLintFindings(root); !slices.Equal(got, c.want) {
				t.Errorf("findings:\n%q\nwant:\n%q", got, c.want)
			}
		})
	}
}

// Past assetLintMaxFiles the project is no longer small, and the lint returns
// nil rather than judge it: the orphaned template here is not reported.
func TestAssetLintSkipsLargeProjects(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{"templates/orphan.html": "<html></html>"}
	for i := 0; i < assetLintMaxFiles+5; i++ {
		files[fmt.Sprintf("pkg/f%d.py", i)] = "x = 1\n"
	}
	writeTree(t, root, files)
	if findings := assetLintFindings(root); findings != nil {
		t.Errorf("a large project must be skipped, got: %q", findings)
	}
}

// A form target with no handler is a page that cannot work. The exit gate asks
// for those findings by themselves, and the mutation-time note calls them a
// defect, not advice. The note names each finding once per session.
func TestRouteContractFindingsStandApartFromTheAdvice(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"app.py":               "@app.route('/')\ndef index():\n    return 'home'\n",
		"templates/index.html": "<form method=\"POST\" action=\"/add_book\"></form>",
	})
	orphan := `templates/index.html is referenced by nothing (no render_template call or include names "index.html").`
	unserved := `templates/index.html calls "/add_book", but no Flask route matches it (routes: /).`

	if got := routeContractFindings(root); !slices.Equal(got, []string{unserved}) {
		t.Errorf("routeContractFindings = %q, want only the unserved target", got)
	}
	if got := assetLintFindings(root); !slices.Equal(got, []string{orphan, unserved}) {
		t.Errorf("assetLintFindings = %q, want the advice and then the unserved target", got)
	}

	ctx := NewAgentContext(root, Tier2Medium)
	note := assetLintNote(ctx)
	for _, want := range []string{
		"Project structure check: " + orphan + " This is advisory",
		"Route check: " + unserved + " This is a defect, not advice: that page cannot submit.",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("note missing %q:\n%s", want, note)
		}
	}
	if again := assetLintNote(ctx); again != "" {
		t.Errorf("findings the note already named must not repeat, got %q", again)
	}
}

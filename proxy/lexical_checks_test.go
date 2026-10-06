package main

import (
	"path/filepath"
	"reflect"
	"testing"
)

// A reference that names a place outside its own folder is reported as
// dangling by its text alone. Each case below puts a real file where such a
// name would lead, so a check that looked at the disk would find it and
// stay quiet.
func TestAReferenceThatLeavesItsFolderIsDanglingByItsTextAlone(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "project")
	writeTree(t, workspace, map[string]string{
		"app.py": "from flask import render_template, url_for\n" +
			"render_template('index.html')\n" +
			"render_template('../shared/page.html')\n" +
			"x = url_for('static', filename='../other/app.css')\n",
		"templates/index.html": "<html><script src=\"../sibling.js\"></script></html>",
		// Where the three names above would lead:
		"shared/page.html": "<html></html>",
		"other/app.css":    "body {}\n",
	})
	writeTree(t, base, map[string]string{"sibling.js": "// placeholder\n"})

	findings := assetLintFindings(workspace)
	for _, name := range []string{"../shared/page.html", "../other/app.css", "../sibling.js"} {
		if !findingsContaining(findings, name) {
			t.Errorf("the reference %q was not reported as dangling: %v", name, findings)
		}
	}
}

// The same three kinds of reference, with a name that stays inside and a
// file that is there, are not reported.
func TestAReferenceThatStaysInsideAndExistsIsNotDangling(t *testing.T) {
	workspace := t.TempDir()
	writeTree(t, workspace, map[string]string{
		"app.py": "from flask import render_template, url_for\n" +
			"render_template('index.html')\n" +
			"x = url_for('static', filename='app.css')\n",
		"templates/index.html": "<html><script src=\"/static/app.js\"></script>" +
			"<link href=\"{{ url_for('static', filename='app.css') }}\"></html>",
		"static/app.js":  "// placeholder\n",
		"static/app.css": "body {}\n",
	})
	for _, finding := range assetLintFindings(workspace) {
		for _, name := range []string{"index.html", "app.js", "app.css"} {
			if findingsContaining([]string{finding}, "does not exist") && findingsContaining([]string{finding}, name) {
				t.Errorf("a reference that exists was reported as dangling: %q", finding)
			}
		}
	}
}

// An expected output is looked for only under the workspace and the system
// temporary folder. A path under neither is skipped: it is not looked for,
// so it is not reported as missing either.
func TestAnExpectedOutputOutsideTheKnownRootsIsSkipped(t *testing.T) {
	workspace := t.TempDir()
	writeTree(t, workspace, map[string]string{"present.txt": "x\n"})
	ctx := NewAgentContext(workspace, Tier2Medium)

	outside := filepath.Join(string(filepath.Separator), "placeholder-not-a-root", "report.txt")
	got := missingExpectedOutputs(ctx, []string{"present.txt", "absent.txt", outside})
	if want := []string{"absent.txt"}; !reflect.DeepEqual(got, want) {
		t.Errorf("missingExpectedOutputs = %v, want %v: the file that is there is not missing, "+
			"and the path under neither root is not judged", got, want)
	}
}

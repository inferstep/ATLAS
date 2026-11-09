package assetlint

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Findings is every finding, advisory and contract alike, in one
// list. The mutation-time note and the exit gate ask the two halves
// separately; this keeps the single-list callers unchanged.
//
// It walks the project under workingDir, and returns nil for a project too
// large to judge.
func Findings(workingDir string) []string {
	return FindingsWithin(workingDir, time.Time{})
}

// FindingsWithin is Findings with a deadline, for a gate that must not hold a
// session on a deep tree. A zero deadline means no limit.
func FindingsWithin(workingDir string, deadline time.Time) []string {
	files, ok := assetLintFilesWithin(workingDir, deadline)
	if !ok {
		return nil
	}
	advisory, contract := findingsFor(workingDir, files)
	return append(advisory, contract...)
}

// RouteContractFindings is only the findings that describe a page which
// cannot work: a form action or fetch() target that no declared route serves.
// These are not advice. A user clicking that form gets a 404 whatever else the
// project does right, so the exit gate asks for this list by itself.
func RouteContractFindings(workingDir string) []string {
	return RouteContractFindingsWithin(workingDir, time.Time{})
}

// RouteContractFindingsWithin is RouteContractFindings with a deadline. A zero
// deadline means no limit.
func RouteContractFindingsWithin(workingDir string, deadline time.Time) []string {
	files, ok := assetLintFilesWithin(workingDir, deadline)
	if !ok {
		return nil
	}
	_, contract := findingsFor(workingDir, files)
	return contract
}

// assetLintFilesWithin is the walk with the deadline it stops at.
func assetLintFilesWithin(workingDir string, deadline time.Time) ([]lintFile, bool) {
	return collectLintFiles(workingDir, func() bool {
		return !deadline.IsZero() && time.Now().After(deadline)
	})
}

// findingsFor separates what is advice (an orphaned template, a
// dangling href) from what is a broken contract between two files the model
// wrote (a submit target with no handler).
func findingsFor(workingDir string, files []lintFile) (advisory, contract []string) {
	var findings []string
	allOther := func(self string) string {
		var b strings.Builder
		for _, f := range files {
			if f.rel != self {
				b.WriteString(f.content)
				b.WriteByte('\n')
			}
		}
		return b.String()
	}
	htmlCount, routeSet, routeRaw := pagesAndRoutes(files)

	for _, f := range files {
		switch {
		case strings.HasPrefix(f.rel, "templates/"):
			findings = append(findings, orphanedTemplateFindings(f, allOther)...)
		case strings.HasPrefix(f.rel, "static/"):
			findings = append(findings, orphanedStaticFindings(f, allOther)...)
		default:
			findings = append(findings, orphanedFlatFindings(f, allOther, htmlCount)...)
		}

		ext := strings.ToLower(filepath.Ext(f.rel))
		findings = append(findings, missingTemplateFindings(workingDir, f, ext)...)

		// Route-contract check: fetch()/form-action URLs must correspond
		// to a declared Flask route. Mini-bench t01 generated a JS
		// frontend calling REST endpoints in a style the backend half
		// implemented differently — page loads, halves can't talk.
		if len(routeSet) > 0 && (ext == ".js" || ext == ".html" || ext == ".htm") {
			contract = append(contract, unservedRouteFindings(f, routeSet, routeRaw)...)
		}
	}

	// Dangling local references: src/href/url_for pointing at files that
	// don't exist in the workspace. A file reports each target once.
	seenDangling := map[string]bool{}
	for _, f := range files {
		findings = append(findings, danglingHrefFindings(workingDir, f, seenDangling)...)
		findings = append(findings, danglingStaticFindings(workingDir, f, seenDangling)...)
	}

	sort.Strings(findings)
	return findings, contract
}

// missingTemplateFindings reports the templates a file refers to that do not
// exist: render_template('x') in .py, {% extends/include %} in templates. The
// snake fix session shipped an errorhandler rendering templates/404.html that
// did not exist — every 404 became a 500.
func missingTemplateFindings(workingDir string, f lintFile, ext string) []string {
	var out []string
	for _, name := range templateReferences(f, ext) {
		if strings.Contains(name, "{{") {
			continue
		}
		// A name escaping templates/ is dangling by definition (Jinja loaders
		// refuse traversal).
		if missingUnder(filepath.Join(workingDir, "templates"), filepath.FromSlash(name)) {
			out = append(out, fmt.Sprintf(
				"%s references template %q, but templates/%s does not exist.",
				f.rel, name, name))
		}
	}
	return out
}

// danglingHrefFindings reports the local src/href targets of a file that do
// not exist in the workspace. seen holds the targets already reported.
func danglingHrefFindings(workingDir string, f lintFile, seen map[string]bool) []string {
	var out []string
	for _, m := range reSrcHref.FindAllStringSubmatch(f.content, -1) {
		target := m[1]
		if notALocalFile(target) {
			continue
		}
		target = strings.SplitN(target, "?", 2)[0]
		target = strings.SplitN(target, "#", 2)[0]
		if target == "" || target == "/" {
			continue
		}
		// A target escaping the workspace can't be served from it.
		if missingUnder(workingDir, filepath.FromSlash(strings.TrimPrefix(target, "/"))) {
			key := f.rel + "→" + target
			if !seen[key] {
				seen[key] = true
				out = append(out, fmt.Sprintf(
					"%s references %q, which does not exist in the workspace.", f.rel, target))
			}
		}
	}
	return out
}

// danglingStaticFindings reports the url_for('static', filename=...) files of
// a file that do not exist under static/. seen holds the targets already
// reported.
func danglingStaticFindings(workingDir string, f lintFile, seen map[string]bool) []string {
	var out []string
	for _, m := range reURLFor.FindAllStringSubmatch(f.content, -1) {
		// A filename escaping static/ 404s at runtime (Flask refuses traversal).
		if missingUnder(filepath.Join(workingDir, "static"), filepath.FromSlash(m[1])) {
			key := f.rel + "→" + m[1]
			if !seen[key] {
				seen[key] = true
				out = append(out, fmt.Sprintf(
					"%s references url_for('static', filename=%q), but static/%s does not exist.",
					f.rel, m[1], m[1]))
			}
		}
	}
	return out
}

// notALocalFile reports a src/href target that names no file of the
// workspace: a URL, an anchor, inline data, a mail link or a template
// expression.
func notALocalFile(target string) bool {
	return strings.Contains(target, "://") || strings.HasPrefix(target, "//") ||
		strings.HasPrefix(target, "#") || strings.HasPrefix(target, "data:") ||
		strings.HasPrefix(target, "mailto:") || strings.Contains(target, "{{") ||
		strings.Contains(target, "{%")
}

// missingUnder reports whether rel names no file under root. A rel that
// escapes root is missing without a look at the disk, so the probe stays
// contained.
func missingUnder(root, rel string) bool {
	if !filepath.IsLocal(rel) {
		return true
	}
	_, err := os.Stat(filepath.Join(root, rel))
	return err != nil
}

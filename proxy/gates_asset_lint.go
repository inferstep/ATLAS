// Asset-graph lint: cross-file coherence for small web projects — a template
// no route renders, an href to a file that isn't there, a fetch to a route
// that doesn't exist. Advisory notes, deduped per session. A form or fetch()
// target that no route serves is a defect, and the exit gate asks about it.
//
// The findings come from assetLintFindings, routeContractFindings and
// assetLintFindingsFor, in gates.go. The walk that loads the project's files
// and the steps assetLintFindingsFor runs over them are here.

package main

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
)

// Asset-graph lint: cross-file coherence checks for small web projects.
// The sandbox verifies each file in isolation (compile, run, HTTP 200),
// so a project can pass every check while its files ignore each other —
// a template no route renders, a static script no page loads, an href
// to a file that doesn't exist. All three shapes appeared in the
// 2026-07-18 snake-game session. Findings are advisory text handed back
// to the model as [system note]s; nothing here blocks a write or a
// done — the stuck-pattern detectors stay the only loop-breakers.

const (
	// assetLintMaxFiles bounds the workspace walk. Past this the project
	// is no longer "small", reference search gets quadratic-ish, and a
	// framework's asset pipeline makes textual matching wrong anyway.
	assetLintMaxFiles = 400
	// assetLintMaxFileBytes skips huge files during content search.
	assetLintMaxFileBytes = 256 * 1024
)

var (
	// src/href values worth resolving as local paths. Skips externals,
	// anchors, data URIs, protocol-relative, and templated values.
	reSrcHref = regexp.MustCompile(`(?i)\b(?:src|href)\s*=\s*["']([^"']+)["']`)
	// url_for('static', filename='x.js') — Flask's canonical static ref.
	reURLFor = regexp.MustCompile(`url_for\(\s*['"]static['"]\s*,\s*filename\s*=\s*['"]([^'"]+)['"]`)
	// render_template('name.html') — referenced template must exist.
	reRenderTemplate = regexp.MustCompile(`render_template\(\s*['"]([^'"]+)['"]`)
	// {% extends "base.html" %} / {% include "nav.html" %}.
	reJinjaRef = regexp.MustCompile(`\{%\s*(?:extends|include)\s+['"]([^'"]+)['"]`)
	// fetch('/path'...) with a local absolute path (quote or backtick).
	reFetchURL = regexp.MustCompile("fetch\\(\\s*[`'\"](/[^`'\"?#{]*)")
	// <form action="/path">.
	reFormAction = regexp.MustCompile(`(?i)\baction\s*=\s*["'](/[^"'?#{]*)["']`)
	// @app.route('/path') / @bp.route(...).
	reFlaskRoute = regexp.MustCompile(`@\w+\.route\(\s*['"]([^'"]+)['"]`)
)

// lintFile is one workspace source file the asset lint reasons about.
type lintFile struct {
	rel     string
	content string
}

// assetLintFiles loads the files the lint reasons about. false means the
// project is too large to judge, and every lint answer is then "nothing".
func assetLintFiles(workingDir string) ([]lintFile, bool) {
	var files []lintFile
	count := 0
	dir := openConfinedDir(workingDir) // reads stay inside the workspace
	defer dir.Close()
	_ = dir.Walk(func(rel string, entry fs.DirEntry, err error) error { // its one error, "too large", is in count
		if err != nil {
			return nil
		}
		name := entry.Name()
		if entry.IsDir() {
			return lintWalkDir(name)
		}
		count++
		if count > assetLintMaxFiles {
			return fmt.Errorf("project too large")
		}
		if info, ierr := entry.Info(); ierr != nil || info.Size() > assetLintMaxFileBytes {
			return nil
		}
		switch strings.ToLower(filepath.Ext(name)) {
		case ".py", ".html", ".htm", ".js", ".css", ".jinja", ".jinja2":
			data, rerr := dir.ReadFile(rel)
			if rerr != nil {
				return nil
			}
			files = append(files, lintFile{rel: filepath.ToSlash(rel), content: string(data)})
		}
		return nil
	})
	if count > assetLintMaxFiles {
		return nil, false
	}
	return files, true
}

// lintWalkDir is the lint walk's answer for a directory: skip a hidden one and
// the folders that hold installed packages or caches, and enter any other.
func lintWalkDir(name string) error {
	if strings.HasPrefix(name, ".") || name == "node_modules" ||
		name == "venv" || name == "__pycache__" {
		return fs.SkipDir
	}
	return nil
}

// pagesAndRoutes counts the project's HTML pages and collects the routes its
// .py files declare: each as a pattern a request path is matched against,
// and as written, for the finding that names them.
func pagesAndRoutes(files []lintFile) (int, []*regexp.Regexp, []string) {
	htmlCount := 0
	routeSet := []*regexp.Regexp{}
	routeRaw := []string{}
	for _, f := range files {
		ext := strings.ToLower(filepath.Ext(f.rel))
		if ext == ".html" || ext == ".htm" {
			htmlCount++
		}
		if ext == ".py" {
			for _, m := range reFlaskRoute.FindAllStringSubmatch(f.content, -1) {
				if re, err := regexp.Compile(flaskRoutePattern(m[1])); err == nil {
					routeSet = append(routeSet, re)
					routeRaw = append(routeRaw, m[1])
				}
			}
		}
	}
	return htmlCount, routeSet, routeRaw
}

// flaskRoutePattern is the regular expression for the request paths one
// declared Flask route serves, such as "/items/<int:id>".
func flaskRoutePattern(route string) string {
	raw := strings.TrimSuffix(route, "/")
	if raw == "" {
		raw = "/"
	}
	// '<int:id>'-style segments match any single path segment.
	var b strings.Builder
	b.WriteString("^")
	for _, seg := range strings.Split(raw, "/") {
		if seg == "" {
			continue
		}
		b.WriteString("/")
		if strings.HasPrefix(seg, "<") && strings.HasSuffix(seg, ">") {
			b.WriteString("[^/]+")
		} else {
			b.WriteString(regexp.QuoteMeta(seg))
		}
	}
	if b.String() == "^" {
		b.WriteString("/")
	}
	b.WriteString("/?$")
	return b.String()
}

// routeMatches reports whether a declared route serves target.
func routeMatches(routeSet []*regexp.Regexp, target string) bool {
	t := strings.TrimSuffix(target, "/")
	if t == "" {
		t = "/"
	}
	for _, re := range routeSet {
		if re.MatchString(t) {
			return true
		}
	}
	return false
}

// orphanedTemplateFindings reports a template in templates/ that no other file
// names: no render_template call and no include.
func orphanedTemplateFindings(f lintFile, allOther func(self string) string) []string {
	var findings []string
	base := filepath.Base(f.rel)
	if others := allOther(f.rel); !strings.Contains(others, base) {
		msg := fmt.Sprintf(
			"%s is referenced by nothing (no render_template call or include names %q).",
			f.rel, base)
		// render_template_string elsewhere is the smell that
		// pairs with an orphaned template (2026-07-18 snake
		// session: model inlined the page and orphaned both
		// the template and its static script).
		if strings.Contains(others, "render_template_string") {
			msg += " A .py file builds its page inline with render_template_string instead — either render this template or delete it."
		}
		findings = append(findings, msg)
	}
	return findings
}

// orphanedStaticFindings reports a file in static/ that no other file names:
// no <script src>, no <link href> and no url_for('static', ...).
func orphanedStaticFindings(f lintFile, allOther func(self string) string) []string {
	var findings []string
	base := filepath.Base(f.rel)
	relUnderStatic := strings.TrimPrefix(f.rel, "static/")
	others := allOther(f.rel)
	if !strings.Contains(others, base) && !strings.Contains(others, relUnderStatic) {
		findings = append(findings, fmt.Sprintf(
			"%s is referenced by nothing (no <script src>, <link href>, or url_for('static', ...) names it).",
			f.rel))
	}
	return findings
}

// orphanedFlatFindings reports a script or stylesheet outside templates/ and
// static/ that no other file names, in a project that has HTML pages.
func orphanedFlatFindings(f lintFile, allOther func(self string) string, htmlCount int) []string {
	var findings []string
	// Flat-layout orphans: a .js/.css living beside .html files
	// (no templates/static dirs) is subject to the same rule —
	// three mini-bench tasks inlined a duplicate <script> and
	// orphaned the companion file, invisible to the prefix-keyed
	// rules above. Only fires when the project has HTML at all
	// (a pure node/python lib's entry file is legitimately
	// unreferenced).
	ext := strings.ToLower(filepath.Ext(f.rel))
	if (ext == ".js" || ext == ".css") && htmlCount > 0 {
		if !strings.Contains(allOther(f.rel), filepath.Base(f.rel)) {
			findings = append(findings, fmt.Sprintf(
				"%s is referenced by nothing — if a page should load it, add the <script src>/<link href>; if its content was inlined instead, delete the file.",
				f.rel))
		}
	}
	return findings
}

// templateReferences are the template names f renders (a .py file) or
// extends and includes (a template).
func templateReferences(f lintFile, ext string) []string {
	var tmplRefs []string
	if ext == ".py" {
		for _, m := range reRenderTemplate.FindAllStringSubmatch(f.content, -1) {
			tmplRefs = append(tmplRefs, m[1])
		}
	}
	if ext == ".html" || ext == ".htm" || ext == ".jinja" || ext == ".jinja2" {
		for _, m := range reJinjaRef.FindAllStringSubmatch(f.content, -1) {
			tmplRefs = append(tmplRefs, m[1])
		}
	}
	return tmplRefs
}

// unservedRouteFindings reports each fetch() or form-action URL in f that no
// declared Flask route serves.
func unservedRouteFindings(f lintFile, routeSet []*regexp.Regexp, routeRaw []string) []string {
	var contract []string
	seen := map[string]bool{}
	for _, re := range []*regexp.Regexp{reFetchURL, reFormAction} {
		for _, m := range re.FindAllStringSubmatch(f.content, -1) {
			target := m[1]
			if seen[target] || strings.HasPrefix(target, "/static/") {
				continue
			}
			seen[target] = true
			if !routeMatches(routeSet, target) {
				contract = append(contract, fmt.Sprintf(
					"%s calls %q, but no Flask route matches it (routes: %s).",
					f.rel, target, strings.Join(routeRaw, ", ")))
			}
		}
	}
	return contract
}

// routeContractMessage is the exit gate's instruction when a page submits to
// a route the server never defines. It carries the lint's own finding, which
// already names the target and lists the routes that do exist.
func routeContractMessage(findings []string) string {
	var sb strings.Builder
	sb.WriteString("Before finishing: a page you wrote submits to a route the server does not define, " +
		"so that part of the site cannot work.\n\n")
	for _, f := range findings {
		sb.WriteString("  " + f + "\n")
	}
	// How, not just what. Answering this with a whole-file write_file of a
	// file the session created is allowed, and it is the expensive path: the
	// fenced channel, then the full candidate pipeline again on a file that
	// already passed it and ran. Measured 2026-09-15: one added route that way
	// cost 183 s of a 570 s budget and introduced a 500 the run had 7 s left to
	// find. Adding a handler is an insert; changing an action is an edit.
	sb.WriteString("\nEither add a handler for that path -- with insert_after or structural_edit on the " +
		"existing file, not a whole-file write_file -- or change the form action / fetch URL to a " +
		"route that exists with edit_file. Then verify it with a request against the running " +
		"server. If the route is registered in a way this check cannot see, say so in your " +
		"summary and finish.")
	return sb.String()
}

// assetLintNote runs the lint and formats findings the model has not
// seen yet as one [system note] body ("" when quiet). Dedup state lives
// in ctx.AssetLintSeen so a persistent orphan is mentioned once, not
// after every subsequent write.
func assetLintNote(ctx *AgentContext) string {
	files, ok := assetLintFiles(ctx.WorkingDir)
	if !ok {
		return ""
	}
	advisory, contract := assetLintFindingsFor(ctx.WorkingDir, files)
	if len(advisory)+len(contract) == 0 {
		return ""
	}
	if ctx.AssetLintSeen == nil {
		ctx.AssetLintSeen = make(map[string]bool)
	}
	fresh := func(in []string) []string {
		var out []string
		for _, f := range in {
			if !ctx.AssetLintSeen[f] {
				ctx.AssetLintSeen[f] = true
				out = append(out, f)
			}
		}
		return out
	}
	advisory, contract = fresh(advisory), fresh(contract)
	if len(advisory)+len(contract) == 0 {
		return ""
	}
	var sb strings.Builder
	if len(advisory) > 0 {
		sb.WriteString("Project structure check: " + strings.Join(advisory, " ") +
			" This is advisory — fix it if these files are meant to work together.")
	}
	// A submit target with no handler was delivered under the same
	// "advisory" wording as an orphaned file, and a model reading it as
	// advice shipped a form that 404s (acceptance run, 2026-09-15). It is
	// a defect, and the run will be asked about it before it can finish.
	if len(contract) > 0 {
		if sb.Len() > 0 {
			sb.WriteString(" ")
		}
		sb.WriteString("Route check: " + strings.Join(contract, " ") +
			" This is a defect, not advice: that page cannot submit. Add the route or " +
			"fix the target before finishing.")
	}
	return sb.String()
}

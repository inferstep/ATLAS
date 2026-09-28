package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every event the proxy streams must have a case in appendChatEvent.
//
// There was no test for this, and an event type shipped that the TUI could not
// render: a content-loop recovery streamed `agent_loop_recovery`, the user saw
// a stream stop and a new turn begin with nothing said in between, and the
// reliability harness flagged it as a protocol defect on 2 of 28 sessions.
// Located by scanning source, the same way the harness does, so a file move
// cannot silently empty either side.
func TestEveryStreamedEventTypeIsRenderable(t *testing.T) {
	emitted := map[string]string{} // type -> where
	streamRE := regexp.MustCompile(`ctx\.Stream\(\s*"([a-z0-9_]+)"`)
	proxyDir := filepath.Join("..", "proxy")
	entries, err := os.ReadDir(proxyDir)
	if err != nil {
		t.Skipf("proxy source not available: %v", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(proxyDir, e.Name()))
		if err != nil {
			continue
		}
		for _, m := range streamRE.FindAllStringSubmatch(string(src), -1) {
			if _, seen := emitted[m[1]]; !seen {
				emitted[m[1]] = e.Name()
			}
		}
	}
	if len(emitted) == 0 {
		t.Fatal("found no ctx.Stream call sites — the scan is broken, not the contract")
	}

	handled := map[string]bool{}
	caseRE := regexp.MustCompile(`(?m)^\s*case ((?:"[a-z0-9_]+"(?:,\s*)?)+):`)
	litRE := regexp.MustCompile(`"([a-z0-9_]+)"`)
	tuiFiles, _ := filepath.Glob("*.go")
	for _, f := range tuiFiles {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil || !strings.Contains(string(src), "appendChatEvent") {
			continue
		}
		for _, m := range caseRE.FindAllStringSubmatch(string(src), -1) {
			for _, lit := range litRE.FindAllStringSubmatch(m[1], -1) {
				handled[lit[1]] = true
			}
		}
	}

	// Types deliberately not rendered as chat, with the reason. Anything else
	// missing is the defect this test exists to catch.
	exempt := map[string]string{
		"token": "token deltas are streamed for throughput accounting, never shown as a chat line",
	}
	var missing []string
	for typ, where := range emitted {
		if handled[typ] || exempt[typ] != "" {
			continue
		}
		missing = append(missing, typ+" (emitted in proxy/"+where+")")
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the proxy streams %d event type(s) the TUI cannot render:\n  %s\n"+
			"Add a case in appendChatEvent, or exempt it here with the reason.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

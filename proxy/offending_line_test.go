package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A parse warning shows the line it names. A bare line number is little use
// to a model that reproduces the whole file from its own memory: it can
// rewrite the file again and again with the same error at the same line.
//
// This is information, not a gate: nothing is rejected, retried or repaired,
// and the write lands with its warning.

func TestTheParseWarningQuotesTheLineItNames(t *testing.T) {
	broken := "from flask import Flask\n\napp = Flask(__name__)\n\n\n@app.route('/items', methods=['GET']))\n" +
		"def list_items():\n    return []\n"
	// A sandbox whose checker reports the real error, as the deployed one does.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/syntax-check") {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"valid": false, "errors": []string{"SyntaxError: unmatched ')' (line 6)"}})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	ctx, _ := exactEditWorld(t, "placeholder.txt", "x\n")
	ctx.SandboxURL = srv.URL
	res := exactEditCall(t, ctx, "write_file", map[string]interface{}{"path": "app.py", "content": broken})
	if !res.Success {
		t.Fatalf("the warned write did not land: %s", res.Error)
	}
	var out struct{ Warning string }
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Warning, "does not parse") {
		t.Fatalf("no parse warning: %s", out.Warning)
	}
	if !strings.Contains(out.Warning, "methods=['GET']))") {
		t.Errorf("the warning does not show the offending line:\n%s", out.Warning)
	}
	if !strings.Contains(out.Warning, "> 6\t") {
		t.Errorf("the offending line is not marked with its number:\n%s", out.Warning)
	}
}

// The note is derived from the reported line and nothing else: no line number,
// no note; a number past the end of the file, no note.
func TestTheOffendingLineNoteInventsNothing(t *testing.T) {
	content := "a = 1\nb = 2\n"
	if note := offendingLineNote(content, "SyntaxError: something is wrong"); note != "" {
		t.Errorf("a note without a line number: %q", note)
	}
	if note := offendingLineNote(content, "SyntaxError: bad (line 99)"); note != "" {
		t.Errorf("a note past the end of the file: %q", note)
	}
	note := offendingLineNote(content, "SyntaxError: bad (line 2)")
	if !strings.Contains(note, "> 2\tb = 2") || strings.Contains(note, "line 3") {
		t.Errorf("unexpected note: %q", note)
	}
}

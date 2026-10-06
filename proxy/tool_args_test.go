package main

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// These tests fix the behaviour the path check must have: one reading of a tool
// call's arguments, shared by the check and the handler. They drive the real
// dispatcher (executeToolCall) and assert the check's own refusal, so they fail
// on code that lets the check and a handler read a call two different ways and
// pass once the check judges and forwards a single reading.

// argOutsideMarker is the content of a file placed outside the workspace. A call
// that reads, writes, moves or removes it has left the workspace.
const argOutsideMarker = "contents of a file outside the workspace"

// Substrings of the refusals the check gives for a call it cannot read as one
// value. Kept here so the tests and the refusal wording stay in step.
const (
	refusalRepeatedField = "more than once"
	refusalNotAString    = "must be a string"
)

func argWorkspace(t *testing.T) (*AgentContext, string) {
	t.Helper()
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "OUTSIDE")
	if err := os.WriteFile(marker, []byte(argOutsideMarker), 0o600); err != nil {
		t.Fatalf("write the marker file outside the workspace: %v", err)
	}
	ctx := NewAgentContext(root, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustUntrusted
	return ctx, marker
}

func argMarkerUntouched(t *testing.T, marker string, res *ToolResult) {
	t.Helper()
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != argOutsideMarker {
		t.Errorf("the file outside the workspace was changed or removed (err=%v)", err)
	}
	if res != nil && strings.Contains(string(res.Data), argOutsideMarker) {
		t.Error("the call returned the content of a file outside the workspace")
	}
}

// argObject builds a JSON object from ordered key/value fragments already
// encoded. Written by hand so a key can be repeated, which a Go map cannot do.
func argObject(pairs ...string) json.RawMessage {
	return json.RawMessage("{" + strings.Join(pairs, ",") + "}")
}

func argField(key, value string) string { return jsonQuote(key) + ":" + jsonQuote(value) }

// TestToolCallWithARepeatedPathArgumentIsRefused: when a path argument is given
// more than once the call is ambiguous -- the check and the handler can keep
// different copies -- so it is refused before any handler runs.
func TestToolCallWithARepeatedPathArgumentIsRefused(t *testing.T) {
	for name, fields := range workspacePathFields {
		name, fields := name, fields
		t.Run(name, func(t *testing.T) {
			pairs := []string{argField(fields[0], "a.txt"), argField(fields[0], "b.txt")}
			for _, f := range fields[1:] {
				pairs = append(pairs, argField(f, "c.txt"))
			}
			ctx, marker := argWorkspace(t)
			res := executeToolCall(name, argObject(pairs...), ctx)
			if res == nil || res.Success || !strings.Contains(res.Error, refusalRepeatedField) {
				t.Errorf("%s with a repeated %q = %+v, want a refusal naming the repeated argument",
					name, fields[0], res)
			}
			argMarkerUntouched(t, marker, res)
		})
	}
}

// TestToolCallWithANonTextPathArgumentIsRefused: a path argument that is not a
// string is refused, where the check used to skip it and leave the handler to
// read it alone.
func TestToolCallWithANonTextPathArgumentIsRefused(t *testing.T) {
	for name, fields := range workspacePathFields {
		name, fields := name, fields
		for _, bad := range fields {
			bad := bad
			t.Run(name+"/"+bad, func(t *testing.T) {
				pairs := make([]string, 0, len(fields))
				for _, f := range fields {
					if f == bad {
						pairs = append(pairs, jsonQuote(f)+":123")
					} else {
						pairs = append(pairs, argField(f, "ok.txt"))
					}
				}
				ctx, marker := argWorkspace(t)
				res := executeToolCall(name, argObject(pairs...), ctx)
				if res == nil || res.Success || !strings.Contains(res.Error, refusalNotAString) {
					t.Errorf("%s with %q as a number = %+v, want a refusal that the field must be a string",
						name, bad, res)
				}
				argMarkerUntouched(t, marker, res)
			})
		}
	}
}

// TestMalformedArgumentsKeepTheHandlerMessage pins the behaviour the pass-through
// relies on: arguments that are not a JSON object are not refused with workspace
// wording. Every registered tool fails on them through its own decode, and a null
// is read as "no arguments"; nothing outside the workspace is touched.
func TestMalformedArgumentsKeepTheHandlerMessage(t *testing.T) {
	notObjects := map[string]json.RawMessage{
		"array":   json.RawMessage("[1,2,3]"),
		"string":  json.RawMessage(`"x"`),
		"number":  json.RawMessage("5"),
		"boolean": json.RawMessage("true"),
	}
	for _, tool := range allTools() {
		tool := tool
		t.Run(tool.Name, func(t *testing.T) {
			for kind, args := range notObjects {
				ctx, marker := argWorkspace(t)
				res := executeToolCall(tool.Name, args, ctx)
				assertFailedWithoutWorkspaceWording(t, tool.Name, kind, res)
				argMarkerUntouched(t, marker, res)
			}
			ctx, marker := argWorkspace(t)
			res := executeToolCall(tool.Name, json.RawMessage("null"), ctx)
			if res == nil || res.Error != missingArgsHint(tool.Name) {
				t.Errorf("%s on null = %+v, want the shape hint for no arguments", tool.Name, res)
			}
			argMarkerUntouched(t, marker, res)
		})
	}
}

func assertFailedWithoutWorkspaceWording(t *testing.T, tool, kind string, res *ToolResult) {
	t.Helper()
	if res == nil || res.Success {
		t.Fatalf("%s on a top-level %s = %+v, want a failed result", tool, kind, res)
	}
	workspaceWording := []string{refusalRepeatedField, refusalNotAString,
		"outside the workspace", "argument names are lowercase"}
	for _, w := range workspaceWording {
		if strings.Contains(res.Error, w) {
			t.Errorf("%s refused a top-level %s with workspace wording %q: %q", tool, kind, w, res.Error)
		}
	}
}

// TestMalformedArgumentMessagesAreTheOnesTheModelReads pins the three messages a
// model reads for a malformed call, so the pass-through does not change them.
func TestMalformedArgumentMessagesAreTheOnesTheModelReads(t *testing.T) {
	ctx, _ := argWorkspace(t)
	if res := executeToolCall("read_file", json.RawMessage(""), ctx); res == nil ||
		res.Error != missingArgsHint("read_file") {
		t.Errorf("empty arguments = %+v, want the shape hint", res)
	}
	if res := executeToolCall("read_file", json.RawMessage("{"), ctx); res == nil ||
		res.Success || strings.Contains(res.Error, "truncated") {
		t.Errorf("short invalid JSON = %+v, want the parser error", res)
	}
	long := json.RawMessage(`{"path":"` + strings.Repeat("a", 300))
	if res := executeToolCall("write_file", long, ctx); res == nil ||
		res.Success || !strings.Contains(res.Error, "truncated") {
		t.Errorf("a long unterminated object = %+v, want the truncation hint", res)
	}
}

// TestEveryStringArgumentIsClassifiedAndPathsAreRegistered forces a decision on
// every string argument of every registered tool's input schema: it is a
// registered workspace path, or listed as not a path. A path argument that is
// unregistered, named like a path but not registered, newly added, or nested
// below the top level fails here instead of silently escaping the check.
func TestEveryStringArgumentIsClassifiedAndPathsAreRegistered(t *testing.T) {
	for _, tool := range allTools() {
		tool := tool
		t.Run(tool.Name, func(t *testing.T) {
			assertStringArgsClassified(t, tool.Name, tool.InputSchema)
			assertRegisteredFieldsAreStringArgs(t, tool.Name, tool.InputSchema)
			if nested := nestedPathLikeArg(tool.InputSchema); nested != "" {
				t.Errorf("%s has a path-like argument %q below the top level, which the check does not judge", tool.Name, nested)
			}
		})
	}
}

var argPathLikeNames = map[string]bool{
	"path": true, "source": true, "destination": true, "cwd": true,
}

var argNotAPath = map[string]bool{
	"content": true, "old_str": true, "new_str": true,
	"selector": true, "command": true, "job_id": true,
	"pattern": true, "glob": true,
}

func assertStringArgsClassified(t *testing.T, name string, schema any) {
	t.Helper()
	registered := map[string]bool{}
	for _, f := range workspacePathFields[name] {
		registered[f] = true
	}
	for _, arg := range stringArgNames(schema) {
		switch {
		case registered[arg], argNotAPath[arg]:
		case argPathLikeNames[arg]:
			t.Errorf("%s takes a path argument %q that workspacePathFields does not list", name, arg)
		default:
			t.Errorf("%s has a string argument %q that is neither a registered workspace path nor listed as not-a-path; decide and add it", name, arg)
		}
	}
}

func assertRegisteredFieldsAreStringArgs(t *testing.T, name string, schema any) {
	t.Helper()
	has := map[string]bool{}
	for _, arg := range stringArgNames(schema) {
		has[arg] = true
	}
	for _, f := range workspacePathFields[name] {
		if !has[f] {
			t.Errorf("workspacePathFields lists %q for %s but its schema has no such string argument", f, name)
		}
	}
}

func stringArgNames(schema any) []string {
	st := structType(reflect.TypeOf(schema))
	if st == nil {
		return nil
	}
	var out []string
	for i := 0; i < st.NumField(); i++ {
		if st.Field(i).Type.Kind() == reflect.String {
			if name := jsonName(st.Field(i)); name != "" && name != "-" {
				out = append(out, name)
			}
		}
	}
	return out
}

func nestedPathLikeArg(schema any) string {
	st := structType(reflect.TypeOf(schema))
	if st == nil {
		return ""
	}
	for i := 0; i < st.NumField(); i++ {
		if name := nestedPathLikeInType(st.Field(i).Type); name != "" {
			return name
		}
	}
	return ""
}

func nestedPathLikeInType(t reflect.Type) string {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return ""
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Type.Kind() == reflect.String && argPathLikeNames[jsonName(f)] {
			return jsonName(f)
		}
		if deeper := nestedPathLikeInType(f.Type); deeper != "" {
			return deeper
		}
	}
	return ""
}

func jsonName(f reflect.StructField) string { return strings.Split(f.Tag.Get("json"), ",")[0] }

func structType(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	return t
}

// TestCanonicalArgumentsPreserveValuesWithoutEscaping checks the encoder: decoded
// values survive unchanged - including a large whole number and a nested value -
// no HTML escape is added, and a value is carried as its raw bytes, never widened
// into a float.
func TestCanonicalArgumentsPreserveValuesWithoutEscaping(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	args := json.RawMessage(`{"path":"in.txt","content":"<b> & </b>","big":123456789012345678,"meta":{"k":[1,2,3]}}`)
	canonical, rejection := canonicalToolArgs("write_file", args, ctx)
	if rejection != "" {
		t.Fatalf("a well-formed call was refused: %s", rejection)
	}
	if strings.Contains(string(canonical), `\u00`) {
		t.Errorf("canonical form added an escape: %s", canonical)
	}
	if !strings.Contains(string(canonical), "<b> & </b>") {
		t.Errorf("canonical form did not carry the markup literally: %s", canonical)
	}
	if !strings.Contains(string(canonical), "123456789012345678") {
		t.Errorf("canonical form did not carry the large number exactly: %s", canonical)
	}
	var from, to map[string]any
	if err := json.Unmarshal(args, &from); err != nil {
		t.Fatalf("seed does not parse: %v", err)
	}
	if err := json.Unmarshal(canonical, &to); err != nil {
		t.Fatalf("canonical form does not parse: %v", err)
	}
	if !reflect.DeepEqual(from, to) {
		t.Errorf("decoded values changed: %v vs %v", from, to)
	}
}

// TestCanonicalFormIsStableAndRebuiltCallPasses checks idempotence: the canonical
// form of a canonical form is the same bytes, and a write_file rebuilt from a
// typed struct passes the check and dispatches the same decoded call.
func TestCanonicalFormIsStableAndRebuiltCallPasses(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	once, rej := canonicalToolArgs("write_file", json.RawMessage(`{"content":"x","path":"in.txt"}`), ctx)
	if rej != "" {
		t.Fatalf("a well-formed call was refused: %s", rej)
	}
	twice, rej2 := canonicalToolArgs("write_file", once, ctx)
	if rej2 != "" || !bytes.Equal(once, twice) {
		t.Errorf("canonical of canonical changed: %q -> %q (rejection %q)", once, twice, rej2)
	}
	var wf WriteFileInput
	if err := json.Unmarshal(once, &wf); err != nil {
		t.Fatalf("canonical write_file does not decode: %v", err)
	}
	rebuilt, err := json.Marshal(wf)
	if err != nil {
		t.Fatalf("rebuild failed: %v", err)
	}
	got, rej3 := canonicalToolArgs("write_file", json.RawMessage(rebuilt), ctx)
	if rej3 != "" {
		t.Fatalf("a rebuilt write_file was refused: %s", rej3)
	}
	var before, after WriteFileInput
	_ = json.Unmarshal(once, &before)
	_ = json.Unmarshal(got, &after)
	if before != after {
		t.Errorf("a rebuilt write_file changed the decoded call: %+v vs %+v", before, after)
	}
}

// TestEveryPathToolRefusesAnOutsideWorkspacePath is the plain per-tool refusal
// through the real dispatcher: a well-formed path outside the workspace is
// refused for every tool and every path field, and the marker file is untouched.
func TestEveryPathToolRefusesAnOutsideWorkspacePath(t *testing.T) {
	for name, fields := range workspacePathFields {
		name, fields := name, fields
		for _, bad := range fields {
			bad := bad
			t.Run(name+"/"+bad, func(t *testing.T) {
				pairs := make([]string, 0, len(fields))
				for _, f := range fields {
					if f == bad {
						pairs = append(pairs, argField(f, "../escape.txt"))
					} else {
						pairs = append(pairs, argField(f, "ok.txt"))
					}
				}
				ctx, marker := argWorkspace(t)
				res := executeToolCall(name, argObject(pairs...), ctx)
				if res == nil || res.Success || !strings.Contains(res.Error, "outside the workspace") {
					t.Errorf("%s with %q outside the workspace = %+v, want a refusal", name, bad, res)
				}
				argMarkerUntouched(t, marker, res)
			})
		}
	}
}

// TestUnknownToolAnswersUnknownWhateverItsArguments pins that an unknown tool is
// answered "unknown tool" for any arguments -- well-formed, repeated, non-text,
// or not an object -- because no handler reads them.
func TestUnknownToolAnswersUnknownWhateverItsArguments(t *testing.T) {
	ctx, _ := argWorkspace(t)
	for _, args := range []string{
		`{"path":"a.txt"}`,
		`{"path":"a.txt","path":"b.txt"}`,
		`{"path":123}`,
		`[1,2,3]`,
	} {
		res := executeToolCall("a_tool_that_does_not_exist", json.RawMessage(args), ctx)
		if res == nil || res.Success || !strings.Contains(res.Error, "unknown tool") {
			t.Errorf("unknown tool with %s = %+v, want an \"unknown tool\" answer", args, res)
		}
	}
}

// TestHandlerDispatchHasOneGuardedCallSite reads the package source and fails if a
// tool handler could run outside the one dispatcher the check guards: .Execute is
// called only in executeToolCall, which judges the arguments once before it. If a
// second call site is added, the single-reading guarantee would rest on nothing,
// and this fails.
func TestHandlerDispatchHasOneGuardedCallSite(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		tree, perr := parser.ParseFile(fset, file, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", file, perr)
		}
		for _, decl := range tree.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok &&
					sel.Sel.Name == "Execute" && fn.Name.Name != "executeToolCall" {
					t.Errorf("%s: a tool handler is run (.Execute) in %s, outside executeToolCall", file, fn.Name.Name)
				}
				return true
			})
		}
	}
}

// FuzzToolArgumentsAreOneReading is the property: for any arguments the check
// accepts, the path the handler decodes from the forwarded form equals the path
// the check judged, and the reader that runs before the check (the approval
// description) agrees with the forwarded form. The target takes raw argument
// bytes so it reaches spellings an encoder never produces. The TUI tool line is a
// separate module and is not exercised here.
func FuzzToolArgumentsAreOneReading(f *testing.F) {
	f.Add("read_file", `{"path":"a.txt"}`)
	f.Add("write_file", `{"path":"a.txt","content":"x"}`)
	f.Add("move_file", `{"source":"a.txt","destination":"b.txt"}`)
	f.Add("run_command", `{"command":"ls","cwd":"sub"}`)
	f.Add("read_file", `{"path":"a.txt","path":"b.txt"}`)
	f.Add("read_file", `{"path":123}`)
	f.Add("read_file", `{"PATH":"a.txt"}`)
	f.Add("read_file", `null`)
	f.Add("read_file", `[1,2,3]`)
	f.Fuzz(func(t *testing.T, name, raw string) {
		ctx := NewAgentContext(t.TempDir(), Tier2Medium)
		canonical, rejection := canonicalToolArgs(name, json.RawMessage(raw), ctx)
		if rejection != "" {
			return
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(canonical, &m) != nil || m == nil {
			return
		}
		handlerPaths := argHandlerPaths(name, canonical)
		for i, field := range workspacePathFields[name] {
			var judged string
			_ = json.Unmarshal(m[field], &judged)
			if i < len(handlerPaths) && handlerPaths[i] != judged {
				t.Errorf("%s %s: handler reads %q, check judged %q", name, field, handlerPaths[i], judged)
			}
		}
		if describeToolCall(name, json.RawMessage(raw)) != describeToolCall(name, canonical) {
			t.Errorf("%s: the pre-check description differs from the forwarded form", name)
		}
	})
}

// argHandlerPaths decodes a tool's arguments with the handler's own input type
// and returns the path values the handler would use, so the property compares the
// handler's reading to the check's rather than a second parser.
func argHandlerPaths(name string, b json.RawMessage) []string {
	switch name {
	case "read_file":
		var in ReadFileInput
		_ = json.Unmarshal(b, &in)
		return []string{in.Path}
	case "outline_file":
		var in OutlineInput
		_ = json.Unmarshal(b, &in)
		return []string{in.Path}
	case "write_file":
		var in WriteFileInput
		_ = json.Unmarshal(b, &in)
		return []string{in.Path}
	case "edit_file":
		var in EditFileInput
		_ = json.Unmarshal(b, &in)
		return []string{in.Path}
	case "structural_edit":
		var in StructuralEditInput
		_ = json.Unmarshal(b, &in)
		return []string{in.Path}
	case "insert_after":
		var in InsertAfterInput
		_ = json.Unmarshal(b, &in)
		return []string{in.Path}
	case "delete_file":
		var in DeleteFileInput
		_ = json.Unmarshal(b, &in)
		return []string{in.Path}
	case "move_file":
		var in MoveFileInput
		_ = json.Unmarshal(b, &in)
		return []string{in.Source, in.Destination}
	case "search_files":
		var in SearchFilesInput
		_ = json.Unmarshal(b, &in)
		return []string{in.Path}
	case "find_file":
		var in FindFileInput
		_ = json.Unmarshal(b, &in)
		return []string{in.Path}
	case "list_directory":
		var in ListDirectoryInput
		_ = json.Unmarshal(b, &in)
		return []string{in.Path}
	case "run_command":
		var in RunCommandInput
		_ = json.Unmarshal(b, &in)
		return []string{in.Cwd}
	case "run_background":
		var in RunBackgroundInput
		_ = json.Unmarshal(b, &in)
		return []string{in.Cwd}
	default:
		return nil
	}
}

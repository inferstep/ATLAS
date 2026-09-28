package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// One entry of the candidate-generation route is one attributable thing.
//
// A request may enter that route many times: a model that writes, is refused,
// and writes again enters it once per attempt. Every record those attempts
// produce used to carry only the request id, so several feasibility decisions
// arrived for "one invocation" and nothing could say which downstream work
// belonged to which attempt. The join is by identity, never by ordinal,
// timestamp or nearest-event.

func routeCtx(t *testing.T) *AgentContext {
	t.Helper()
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-route")
	return ctx
}

func TestEachRouteEntryGetsItsOwnIdentity(t *testing.T) {
	ctx := routeCtx(t)
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		e := mintRouteEntry(ctx)
		if e.ID == "" {
			t.Fatal("an entry with no identity is unattributable")
		}
		if seen[e.ID] {
			t.Fatalf("entry %d reused %q", i, e.ID)
		}
		seen[e.ID] = true
		if !strings.HasPrefix(e.ID, "req-route:") {
			t.Fatalf("identity does not name its request: %q", e.ID)
		}
	}
	if len(seen) != 5 {
		t.Fatalf("five entries produced %d identities", len(seen))
	}
}

func TestARouteEntryIdentityIsNotGuessable(t *testing.T) {
	a := mintRouteEntry(routeCtx(t))
	b := mintRouteEntry(routeCtx(t))
	if a.ID == b.ID {
		t.Fatal("two requests minted the same first identity; a collision is a theft")
	}
}

func TestARouteEntryWithoutARequestFailsClosed(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	if e := mintRouteEntry(ctx); e.ID != "" {
		t.Fatalf("an identity that binds to no request was minted: %q", e.ID)
	}
}

func TestTheInvocationIdentityIsTheRouteEntry(t *testing.T) {
	ctx := routeCtx(t)
	e := mintRouteEntry(ctx)
	id := nextInvocationIdentity(ctx, e, contentSHA256("candidate"))
	if id.InvocationID != e.ID {
		t.Fatalf("invocation %q is not route entry %q", id.InvocationID, e.ID)
	}
	if !strings.HasPrefix(id.CandidateInstanceID, e.ID+":") {
		t.Fatalf("candidate instance %q does not belong to its entry", id.CandidateInstanceID)
	}
	other := mintRouteEntry(ctx)
	if nextInvocationIdentity(ctx, other, contentSHA256("candidate")).InvocationID == id.InvocationID {
		t.Fatal("two entries produced one invocation; downstream work would merge")
	}
}

func TestAnEntryWithoutIdentityMintsNoInvocation(t *testing.T) {
	ctx := routeCtx(t)
	if id := nextInvocationIdentity(ctx, routeEntry{}, contentSHA256("x")); id.InvocationID != "" {
		t.Fatal("an unattributable entry produced an invocation identity")
	}
}

func TestTheFeasibilityRecordCarriesItsRouteEntry(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-route")
	ctx.SandboxURL = "http://127.0.0.1:1"
	outputs := []string{"solve.py"}
	ctx.TaskContract = &TaskContract{TaskMode: TaskModeWork,
		OutputKnowledge: KnowledgeDeclared, ExpectedOutputs: &outputs}

	ids := []string{}
	recs := captureShadow(t, func() {
		for i := 0; i < 3; i++ {
			e := mintRouteEntry(ctx)
			ids = append(ids, e.ID)
			observeInvocationFeasibility(ctx, e)
		}
	})
	records := recordsOfKind(recs, "shadow_invocation_feasibility")
	if len(records) != 3 {
		t.Fatalf("three entries produced %d feasibility records", len(records))
	}
	got := map[string]bool{}
	for _, r := range records {
		id, _ := r["route_entry_id"].(string)
		if id == "" {
			t.Fatal("a feasibility record carries no route entry; it is unattributable")
		}
		got[id] = true
	}
	for _, want := range ids {
		if !got[want] {
			t.Fatalf("no feasibility record for entry %q", want)
		}
	}
}

// --- the identity is plumbing, not policy --------------------------------

func TestTheRouteEntryNeverReachesTheModel(t *testing.T) {
	// Model-visible text is built from prompts, tool schemas and tool results.
	// None of them may mention the identity: a model that could read it could
	// echo it, and an identity the model can supply is not an identity.
	for _, file := range []string{"prompts.go", "tools.go", "agent.go"} {
		body, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(body), "\n") {
			if !strings.Contains(line, "entry.ID") && !strings.Contains(line, "routeEntry") {
				continue
			}
			for _, modelFacing := range []string{
				"StreamFn(", "ctx.Messages = append", "Content:", "system_prompt",
				"toolResult", "fmt.Sprintf(\"You ",
			} {
				if strings.Contains(line, modelFacing) {
					t.Errorf("%s: the route entry reaches model-visible text: %s",
						file, strings.TrimSpace(line))
				}
			}
		}
	}
}

func TestTheRouteEntryIsNeverDecodedFromInput(t *testing.T) {
	// Nothing reads it off the wire. It is minted by mintRouteEntry and by
	// nothing else, so a caller cannot hand one in.
	tree, err := parser.ParseFile(token.NewFileSet(), "route_entry.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var mints int
	ast.Inspect(tree, func(n ast.Node) bool {
		if f, ok := n.(*ast.FuncDecl); ok && f.Name.Name == "mintRouteEntry" {
			mints++
		}
		return true
	})
	if mints != 1 {
		t.Fatalf("mintRouteEntry declared %d times", mints)
	}
	for _, file := range []string{"types.go", "agent.go", "tools.go"} {
		body, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		if strings.Contains(string(body), `json:"route_entry_id"`) ||
			strings.Contains(string(body), `json:"route_entry"`) {
			t.Errorf("%s decodes a route entry from input", file)
		}
	}
}

func TestTheV3StageEnvelopeGainedOneAdditiveField(t *testing.T) {
	body, err := os.ReadFile("tools.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	i := strings.Index(s, `Emit(NewEnvelope(EvtStageStart, "v3"`)
	if i < 0 {
		t.Fatal("the v3 stage envelope is gone")
	}
	block := s[i : i+400]
	if !strings.Contains(block, `"detail"`) {
		t.Error("the existing detail field was removed; consumers would break")
	}
	if !strings.Contains(block, `"route_entry"`) {
		t.Error("the envelope does not carry its route entry")
	}
}

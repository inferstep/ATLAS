package main

import (
	"go/ast"
	"os"
	"slices"
	"strings"
	"testing"
)

// The registry is the list of ways service-authored bytes can reach an
// artifact. A new one has to join it before it can exist.

// v3DeliveryRouteSteps names, for a registered route whose body is split into
// steps, each step in the order the route calls it. The guards that ask what a
// route calls read these steps as part of their route, and no other function:
// a step's only caller is its route, the route calls its steps in this order,
// and each sink has one caller within the route (all three pinned below).
var v3DeliveryRouteSteps = map[string][]string{
	"edit_route_delivery.go:deliverEditCandidate": {
		"edit_route_delivery.go:proposeEditCandidate",
		"edit_route_delivery.go:collectEditCandidateEvidence",
		"edit_route_delivery.go:decideEditCandidateDelivery",
	},
}

// routeOf names the route a call site answers for: the route whose step the
// site is, or else the site itself.
func routeOf(site string) string {
	for route, steps := range v3DeliveryRouteSteps {
		if slices.Contains(steps, site) {
			return route
		}
	}
	return site
}

// A step is called by its route, once, and by nothing else, so it cannot become
// a second way for service bytes to reach disk.
func TestEachRouteStepIsCalledOnlyByItsRoute(t *testing.T) {
	files := proxyFiles(t)
	for route, steps := range v3DeliveryRouteSteps {
		for _, step := range steps {
			_, name, _ := strings.Cut(step, ":")
			sites := callSites(files, name)
			if len(sites) != 1 || sites[route] != 1 {
				t.Errorf("%s is called from %v, want once from %s", step, sites, route)
			}
		}
	}
}

// A route calls its steps in their order, and the shared delivery owner after
// the last of them: no byte is delivered before the decision.
func TestARouteCallsItsStepsInTheirOrder(t *testing.T) {
	files := proxyFiles(t)
	for route, steps := range v3DeliveryRouteSteps {
		var want []string
		for _, step := range steps {
			_, name, _ := strings.Cut(step, ":")
			want = append(want, name)
		}
		want = append(want, "deliverCandidateBytes")
		if got := callsInOrder(files, route, want); !slices.Equal(got, want) {
			t.Errorf("route %s calls %v, want %v in that order", route, got, want)
		}
	}
}

// v3DeliveryRouteSinkCallers names, for each split route, the one function that
// may call each sink: the authorization owner and the shared delivery owner.
// A step holds its route's right to a sink only where this table names it.
var v3DeliveryRouteSinkCallers = map[string]map[string]string{
	"edit_route_delivery.go:deliverEditCandidate": {
		"authorizeCandidateDelivery": "edit_route_delivery.go:decideEditCandidateDelivery",
		"deliverCandidateBytes":      "edit_route_delivery.go:deliverEditCandidate",
	},
}

// Within a split route, each sink is called once, from the function the table
// names, so no step asks for authorization or delivers out of its turn.
func TestEachSinkOfASplitRouteHasOneCaller(t *testing.T) {
	files := proxyFiles(t)
	for route, sinks := range v3DeliveryRouteSinkCallers {
		for sink, caller := range sinks {
			calls := map[string]int{}
			for site, n := range callSites(files, sink) {
				if routeOf(site) == route {
					calls[site] = n
				}
			}
			if len(calls) != 1 || calls[caller] != 1 {
				t.Errorf("within %s, %s is called from %v, want once from %s", route, sink, calls, caller)
			}
		}
	}
}

// callsInOrder lists the calls from the body of site ("file:function") to the
// functions in names, in the order they stand in that body.
func callsInOrder(files map[string]*ast.File, site string, names []string) []string {
	file, name, _ := strings.Cut(site, ":")
	var got []string
	for _, d := range files[file].Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || funcIdentity(fd) != name {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && slices.Contains(names, id.Name) {
					got = append(got, id.Name)
				}
			}
			return true
		})
	}
	return got
}

func TestEveryRegisteredRouteAnswersForTheWholeChain(t *testing.T) {
	files := proxyFiles(t)
	// Each registered route must reach authorization, and the shared owner
	// must reach consumption, the exact-byte write, settlement and
	// restoration. Read from the call graph, not from prose.
	owner := "candidate_delivery.go:deliverCandidateBytes"
	for _, fn := range []string{"consumeAuthorizationGrant", "recordDeliverySettlement",
		"restoreDeliverable", "markGrantDelivery"} {
		sites := callSites(files, fn)
		if _, ok := sites[owner]; !ok {
			t.Errorf("the delivery owner does not reach %s (called from %v)", fn, sites)
		}
	}
	for route := range v3DeliveryRoutes {
		if route == owner || strings.HasSuffix(route, "deliverAuthorizedCandidate") {
			continue
		}
		// The route asks itself, or in one of its named steps.
		reaches := false
		for site := range callSites(files, "authorizeCandidateDelivery") {
			if routeOf(site) == route {
				reaches = true
			}
		}
		if !reaches {
			t.Errorf("registered route %s never asks for authorization", route)
		}
	}
}

func TestARouteWithoutRouteIdentityIsNotRegistrable(t *testing.T) {
	files := proxyFiles(t)
	for route := range v3DeliveryRoutes {
		if strings.HasSuffix(route, "deliverAuthorizedCandidate") {
			continue
		}
		if _, ok := callSites(files, "mintRouteEntry")[route]; !ok {
			t.Errorf("registered route %s mints no route identity", route)
		}
		if _, ok := callSites(files, "newRouteLifecycle")[route]; !ok {
			t.Errorf("registered route %s records no route ending", route)
		}
	}
}

func TestNoBareWriteLandsServiceAuthoredBytes(t *testing.T) {
	// The four edit tools used to write a service proposal with a bare
	// os.WriteFile. Any function that both consumes an improved candidate and
	// writes it itself is that bypass returning.
	for _, fn := range functionCalls(proxyFiles(t)) {
		if !fn.calls["improveContentWithV3"] {
			continue
		}
		if !registeredV3DeliveryRoute(routeOf(fn.owner)) {
			t.Errorf("%s consumes a service proposal and is not a registered route or one of its steps", fn.owner)
		}
		if fn.calls["WriteFile"] || fn.calls["atomicReplaceFile"] {
			t.Errorf("%s writes a service proposal itself instead of delivering it", fn.owner)
		}
	}
}

// funcCalls is one function of the package and the names it calls.
type funcCalls struct {
	owner string          // "file.go:identity", as the registry names routes
	calls map[string]bool // each called name, plain or as a selector
}

// functionCalls lists every function with a body in files, with the names it
// calls.
func functionCalls(files map[string]*ast.File) []funcCalls {
	var out []funcCalls
	for name, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			calls := map[string]bool{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch e := c.Fun.(type) {
				case *ast.Ident:
					calls[e.Name] = true
				case *ast.SelectorExpr:
					calls[e.Sel.Name] = true
				}
				return true
			})
			out = append(out, funcCalls{owner: name + ":" + funcIdentity(fd), calls: calls})
		}
	}
	return out
}

func TestTheRegistryNamesOnlyRealRoutes(t *testing.T) {
	files := proxyFiles(t)
	for route := range v3DeliveryRoutes {
		file := strings.SplitN(route, ":", 2)[0]
		fn := strings.SplitN(route, ":", 2)[1]
		f, ok := files[file]
		if !ok {
			t.Errorf("registry names missing file %s", file)
			continue
		}
		found := false
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && funcIdentity(fd) == fn {
				found = true
			}
		}
		if !found {
			t.Errorf("registry names missing function %s", route)
		}
	}
	if len(v3DeliveryToolIdentities()) < 5 {
		t.Error("the registry lost a tool identity")
	}
}

func TestProseCannotSatisfyTheRegistryGuard(t *testing.T) {
	// A comment naming a route is not a registration: the guard reads the
	// call graph and the registry map, never the file's text.
	body, err := os.ReadFile("v3_delivery_registry.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "var v3DeliveryRoutes = map[string]string{") {
		t.Error("the registry is no longer a typed map")
	}
	if registeredV3DeliveryRoute("tools.go:someFunctionMentionedInAComment") {
		t.Error("an unregistered site was accepted")
	}
}

func TestASecondGrantOrSettlementOwnerFails(t *testing.T) {
	files := proxyFiles(t)
	for _, fn := range []string{"consumeAuthorizationGrant", "recordDeliverySettlement"} {
		if sites := callSites(files, fn); len(sites) != 1 {
			t.Errorf("%s has %d owners %v, want exactly one", fn, len(sites), sites)
		}
	}
}

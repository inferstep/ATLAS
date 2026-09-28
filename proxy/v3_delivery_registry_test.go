package main

import (
	"go/ast"
	"os"
	"strings"
	"testing"
)

// The registry is the list of ways service-authored bytes can reach an
// artifact. A new one has to join it before it can exist.

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
		reaches := false
		for _, fn := range []string{"authorizeCandidateDelivery"} {
			if _, ok := callSites(files, fn)[route]; ok {
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
	files := proxyFiles(t)
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
			if !calls["improveContentWithV3"] {
				continue
			}
			owner := name + ":" + funcIdentity(fd)
			if !registeredV3DeliveryRoute(owner) {
				t.Errorf("%s consumes a service proposal and is not a registered route", owner)
			}
			if calls["WriteFile"] || calls["atomicReplaceFile"] {
				t.Errorf("%s writes a service proposal itself instead of delivering it", owner)
			}
		}
	}
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

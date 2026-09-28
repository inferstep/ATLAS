package main

import "strings"

// Every production route that can place service-authored bytes on disk.
//
// The registry exists because the ways a candidate can reach an artifact grew
// one at a time and nothing failed when a new one appeared unprotected: four
// edit tools landed service proposals for months with no evidence, no
// authorization, no licence and no settlement, and every test stayed green
// because no test knew to look. A route that is not named here cannot ask for
// authorization, and a route that is named here has to answer for the whole
// chain.
//
// Ordinary model writes, restoration and non-V3 mutations are deliberately
// outside it: they carry no service-authored bytes and owe no candidate
// lifecycle.
var v3DeliveryRoutes = map[string]string{
	// route -> the tool identity whose result it must answer with.
	"tools.go:writeFileWithV3":                    "write_file",
	"edit_route_delivery.go:deliverEditCandidate": "edit_file|structural_edit|insert_after|replace_lines",
	// The shared owner itself: it delivers on behalf of a route above and is
	// listed so a caller cannot reach it from anywhere else.
	"candidate_delivery.go:deliverAuthorizedCandidate": "write_file",
}

// registeredV3DeliveryRoute reports whether a call site is a route allowed to
// carry service-authored bytes toward disk.
func registeredV3DeliveryRoute(site string) bool {
	if _, ok := v3DeliveryRoutes[site]; ok {
		return true
	}
	// The owner may call itself through its own helpers.
	return strings.HasPrefix(site, "candidate_delivery.go:deliverCandidateBytes")
}

// v3DeliveryToolIdentities is every originating tool a registered route may
// answer as. A delivery that returns another tool's result would break the
// loop's tool-call accounting.
func v3DeliveryToolIdentities() []string {
	out := []string{}
	for _, tools := range v3DeliveryRoutes {
		out = append(out, strings.Split(tools, "|")...)
	}
	return out
}

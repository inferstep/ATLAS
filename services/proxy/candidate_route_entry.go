package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// One entry of the candidate-generation route, named so its own work can be
// told apart from the next attempt's.
//
// A request enters that route once per write_file call that reaches it, and a
// model that writes, is refused and writes again enters it several times. Every
// record those attempts produced carried only the request id, so a reader saw
// "four feasibility decisions for one invocation" and could not say which
// generation, candidate, grant or delivery belonged to which attempt. Measured
// live: four of ten canary cells were unattributable for exactly this reason.
//
// The identity is minted here and nowhere else. It is not derived from a
// count the reader could recompute, not from a timestamp, and not from
// position in a list -- an ordinal join is what this exists to make
// unnecessary. The model never sees it, never supplies it and cannot reach it:
// it is minted after the tool call is decoded and appears only in private
// telemetry and in the additive field on the v3 stage envelope.
type routeEntry struct {
	// ID is empty when there is nothing to bind to. An entry with no identity
	// mints no invocation and satisfies nothing, rather than borrowing one.
	ID string
}

// mintRouteEntry names one entry of the route.
//
// The sequence keeps entries ordered within a request and the random suffix
// keeps them unguessable, so an identity cannot be predicted, reused or
// reconstructed by anything that did not mint it.
func mintRouteEntry(ctx *AgentContext) routeEntry {
	if ctx == nil {
		return routeEntry{}
	}
	request := requestIDOf(ctx)
	if strings.TrimSpace(request) == "" {
		// An identity that binds to no request is worse than none: it would
		// join records across requests.
		return routeEntry{}
	}
	ctx.routeEntryMu.Lock()
	ctx.routeEntrySeq++
	seq := ctx.routeEntrySeq
	ctx.routeEntryMu.Unlock()

	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// No entropy, no identity. A predictable one would let a later entry
		// be mistaken for this one.
		return routeEntry{}
	}
	return routeEntry{ID: fmt.Sprintf("%s:entry:%d:%s", request, seq, hex.EncodeToString(b))}
}

// valid reports whether this entry can carry attribution at all.
func (e routeEntry) valid() bool { return strings.TrimSpace(e.ID) != "" }

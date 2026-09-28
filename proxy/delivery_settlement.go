package main

import (
	"path/filepath"
	"sort"
	"strings"
)

// Discharging the obligation that could only be answered after the bytes
// landed.
//
// `artifact_exists` is the one obligation nothing can evidence in advance:
// requiring it before delivery made every structured task unclosable, which is
// why it became post-delivery settlement rather than an authorization
// prerequisite. What was missing was the other half -- something that actually
// discharges it once a delivery has happened.
//
// Settlement is not a second completion rule. It answers exactly one question,
// about exactly one target: is the artifact this run authorized still there, at
// the bytes it was authorized at, with the session's own record agreeing?
// Everything else a run owes -- mutation debt, verification debt, background
// hazards, deletion rules, action demand, other declared outputs -- is owed by
// its existing owner and untouched by this.

// deliverySettlement is what one delivery left behind for settlement to check.
//
// Identities and hashes. It records the state BEFORE the delivery as well as
// what was delivered, because "the ledger generation this delivery produced"
// is only checkable against what the generation was beforehand.
type deliverySettlement struct {
	GrantID             string
	RequestID           string
	InvocationID        string
	CandidateInstanceID string
	CandidateHash       string
	TargetPath          string

	// PriorGeneration is the target's ledger generation immediately before
	// the write. The ledger observes the write afterwards, through its own
	// owner, so settlement requires the generation to have MOVED past this
	// rather than to equal a number this side guessed.
	PriorGeneration int

	// MetCommands are the declared-command obligations that had current
	// trusted evidence when the delivery was authorized. Settlement requires
	// the task's current set to still be covered by these: a stronger
	// obligation that appeared afterwards is not discharged by an older
	// delivery.
	MetCommands []string

	// BaselinePreserved records that the preservation requirement was
	// satisfied at authorization. Settlement will not discharge existence for
	// a delivery that replaced something it was not entitled to replace.
	BaselinePreserved bool
}

// recordDeliverySettlement is written by the delivery owner, and only for a
// delivery that actually settled on disk.
func recordDeliverySettlement(ctx *AgentContext, s deliverySettlement) {
	if ctx == nil || strings.TrimSpace(s.TargetPath) == "" {
		return
	}
	ctx.grantMu.Lock()
	defer ctx.grantMu.Unlock()
	if ctx.settlements == nil {
		ctx.settlements = map[string]*deliverySettlement{}
	}
	sort.Strings(s.MetCommands)
	rec := s
	ctx.settlements[filepath.Clean(s.TargetPath)] = &rec
}

// deliverySettlementFor returns what a delivery to this target left behind, if
// anything did.
func deliverySettlementFor(ctx *AgentContext, resolved string) *deliverySettlement {
	if ctx == nil {
		return nil
	}
	ctx.grantMu.Lock()
	defer ctx.grantMu.Unlock()
	if s := ctx.settlements[filepath.Clean(resolved)]; s != nil {
		copied := *s
		return &copied
	}
	return nil
}

// settleExistence answers whether one post-delivery existence obligation is
// discharged, and says why when it is not.
//
// Every condition holds simultaneously or none of it counts. They are checked
// in the order in which the answer stops being possible: was there a delivery
// at all, is the artifact still there, is it the same artifact, does the
// session's own record agree, and was the delivery entitled to what it did.
func settleExistence(ctx *AgentContext, o taskObligation,
	obs []taskObligation) (bool, string) {
	resolved := resolveAgentPath(ctx, o.Subject)

	// A settlement record exists only where a grant was validly consumed and
	// the exact authorized bytes were confirmed on disk afterwards. Nothing
	// else writes one, so a successful tool result, a selection label or a
	// piece of prose cannot manufacture it.
	s := deliverySettlementFor(ctx, resolved)
	if s == nil {
		return false, "no authorized delivery to settle"
	}
	// It settles the target it was about and no other.
	if s.TargetPath != resolved {
		return false, "the settlement is about another artifact"
	}
	if s.RequestID != requestIDOf(ctx) {
		return false, "the settlement is from another request"
	}

	// The artifact is still there, at the bytes that were authorized.
	disk := fileSHA256(ctx, resolved)
	if disk == "" {
		return false, "artifact is not on disk"
	}
	if disk != s.CandidateHash {
		return false, "bytes on disk are not the delivered bytes"
	}

	// The session's own record agrees, and it moved: a ledger that still
	// holds the pre-delivery generation never saw this write, and one that
	// holds other bytes is a record about something that is no longer there.
	ctx.LedgerMu.Lock()
	d := ctx.Ledger[filepath.Clean(resolved)]
	var current string
	var generation int
	var tombstoned bool
	var kind ValidationKind
	var status ValidationStatus
	var validatedHash string
	if d != nil {
		current, generation, tombstoned = d.CurrentHash, d.Generation, d.Tombstoned
		kind, status, validatedHash = d.ValidationKind, d.ValidationStatus, d.ValidatedHash
	}
	ctx.LedgerMu.Unlock()

	if d == nil {
		return false, "the ledger does not confirm the bytes on disk"
	}
	if tombstoned {
		return false, "the artifact was deliberately removed"
	}
	if current != disk {
		return false, "the ledger does not confirm the bytes on disk"
	}
	if generation <= s.PriorGeneration {
		return false, "the ledger never recorded this delivery"
	}

	// The structural verdict is about THESE bytes, not an older set. A
	// verdict whose hash has moved on is historical, and a class the gate
	// does not govern owes none.
	if _, gated := syntaxGateLanguages[strings.ToLower(filepath.Ext(resolved))]; gated {
		if status != ValidationPassed || validatedHash != disk {
			return false, "the structural verdict is not current"
		}
		if kind != ValidationKindSyntax && kind != ValidationKindStructural {
			return false, "the structural verdict is not current"
		}
	}

	// Every declared command the task requires must have been covered by the
	// delivery that is being settled. One that appeared afterwards is a
	// stronger obligation than the delivery answered, and an older delivery
	// does not discharge it.
	met := map[string]bool{}
	for _, id := range s.MetCommands {
		met[id] = true
	}
	for _, req := range authorizationPrerequisites(obs) {
		if req.Kind != ObligationDeclaredCommand || !req.Required {
			continue
		}
		if !met[req.ID] {
			return false, "a declared command is not covered by the delivery"
		}
	}
	if !s.BaselinePreserved {
		return false, "the delivery did not preserve the baseline it replaced"
	}
	return true, ""
}

// settlementStatus is the whole-request answer: which post-delivery
// obligations are discharged, and which are still owed.
//
// Sorted, so a summary and a decision never disagree on order.
// `obs` is the request's WHOLE obligation set, not a pre-filtered subset:
// settling existence has to read the declared commands the task states, and a
// caller that handed over only the existence obligations would be asking the
// question with the answer already removed.
func settlementStatus(ctx *AgentContext, obs []taskObligation,
	only map[string]bool) (settled, owed []string, why string) {
	for _, o := range postDeliverySettlement(obs) {
		if !o.Required || (only != nil && !only[o.ID]) {
			continue
		}
		if ok, reason := settleExistence(ctx, o, obs); ok {
			settled = append(settled, o.ID)
		} else {
			owed = append(owed, o.ID)
			if why == "" {
				why = reason
			}
		}
	}
	sort.Strings(settled)
	sort.Strings(owed)
	return settled, owed, why
}

// postDeliverySettlementOwed is what the terminal asks. It is a question about
// this run's own authorized deliveries and nothing else: a request that never
// delivered through the typed path owes no settlement, because there is no
// delivery to settle.
func postDeliverySettlementOwed(ctx *AgentContext) (bool, string) {
	if ctx == nil || ctx.TaskContract == nil {
		return false, ""
	}
	obs := requestObligations(ctx)
	if len(obs) == 0 {
		return false, ""
	}
	// Only targets this run actually delivered to are in scope. An output the
	// run never produced is owed by missingExpectedOutputs, which already owns
	// that question -- claiming it here too would be a second rule for one
	// obligation.
	delivered := map[string]bool{}
	for _, o := range postDeliverySettlement(obs) {
		if !o.Required {
			continue
		}
		if deliverySettlementFor(ctx, resolveAgentPath(ctx, o.Subject)) != nil {
			delivered[o.ID] = true
		}
	}
	if len(delivered) == 0 {
		return false, ""
	}
	_, owed, why := settlementStatus(ctx, obs, delivered)
	if len(owed) == 0 {
		return false, ""
	}
	return true, why
}

// What a request declares: its task contract, the shadow record of that contract, and what it demands.

package main

import (
	"fmt"
	"sort"
	"strings"
)

// validateTaskContract checks a client contract and returns the stored form,
// or an error and nothing at all.
//
// All or nothing, deliberately. A contract with one unusable path is a client
// that thinks it asked for something it did not, and honouring the half that
// parsed would produce obligations the user never stated. Normalising an
// invalid contract into an empty valid one would be worse still: it would look
// like a client that declared nothing.
//
// Paths go through resolveWorkspacePath, the same resolver every tool uses, so
// containment and canonical identity are decided in one place. That needs the
// request's working directory, which is why validation happens at the request
// boundary where the directory has already been resolved -- not in the decoder,
// which has no workspace to check against.
func validateTaskContract(in *TaskContract, workingDir string) (*TaskContract, error) {
	if in == nil {
		return nil, nil
	}
	switch in.TaskMode {
	case TaskModeWork, TaskModeQuestion:
	case "":
		return nil, fmt.Errorf("task_contract.task_mode is required")
	default:
		// Never coerced. An unrecognised mode is a client asking for something
		// this build does not implement.
		return nil, fmt.Errorf("task_contract.task_mode %q is not supported", in.TaskMode)
	}
	if len(in.OutputPaths()) > maxTaskContractEntries ||
		len(in.VerificationCommands()) > maxTaskContractEntries {
		return nil, fmt.Errorf("task_contract exceeds %d entries", maxTaskContractEntries)
	}
	// The typed requirements are checked before knowledge is normalised,
	// because they are part of what the caller declared about verification and
	// a refusal here must not depend on how the older list was spelled.
	typedReqs, err := validateVerificationRequirements(in)
	if err != nil {
		return nil, err
	}
	// candidate_policy is accepted and not read: there is one delivery rule
	// (candidate_policy.go), and a client that still sends a mode -- an
	// older TUI sends "strict" on every request -- keeps working.
	outKnow, err := normalizeKnowledge("output_knowledge", in.OutputKnowledge,
		in.OutputsPresent(), len(in.OutputPaths()))
	if err != nil {
		return nil, err
	}
	verKnow, err := normalizeKnowledge("verification_knowledge", in.VerificationKnowledge,
		in.VerificationPresent(), len(verificationCommandsOf(in)))
	if err != nil {
		return nil, err
	}
	// A question declares nothing to produce and nothing to run. Letting it
	// would make question mode a way in for obligations -- and, later, for the
	// authority attached to them -- through a door work mode does not have.
	if in.TaskMode == TaskModeQuestion {
		if outKnow == KnowledgeDeclared {
			return nil, fmt.Errorf(
				"task_contract: task_mode %q cannot declare output obligations",
				TaskModeQuestion)
		}
		if verKnow == KnowledgeDeclared {
			return nil, fmt.Errorf(
				"task_contract: task_mode %q cannot declare verification obligations",
				TaskModeQuestion)
		}
	}
	out := &TaskContract{TaskMode: in.TaskMode,
		OutputKnowledge: outKnow, VerificationKnowledge: verKnow}
	paths, err := distinctOutputPaths(in, workingDir)
	if err != nil {
		return nil, err
	}
	cmds, err := distinctVerificationCommands(in)
	if err != nil {
		return nil, err
	}
	// Stable order, so two equivalent requests never disagree downstream.
	sort.Strings(paths)
	sort.Strings(cmds)
	// Presence is STORED, not re-derived. A declared-empty list must survive
	// as a present, empty list; rebuilding by append is exactly how the old
	// validator turned it back into "absent".
	storeExpectedOutputs(out, outKnow, paths)
	storeVerification(out, in, verKnow, cmds, typedReqs)
	return out, nil
}

// distinctOutputPaths checks each declared output path against the workspace
// and keeps one spelling of each file, in the order declared.
func distinctOutputPaths(in *TaskContract, workingDir string) ([]string, error) {
	probe := &AgentContext{WorkingDir: workingDir}
	seen := map[string]bool{}
	var paths []string
	for _, p := range in.OutputPaths() {
		if strings.TrimSpace(p) == "" {
			return nil, fmt.Errorf("task_contract.expected_outputs contains an empty path")
		}
		canon, err := resolveWorkspacePath(probe, p)
		if err != nil {
			return nil, fmt.Errorf("task_contract.expected_outputs: %w", err)
		}
		if seen[canon] {
			continue // the same file spelled two ways is one obligation
		}
		seen[canon] = true
		paths = append(paths, p)
	}
	return paths, nil
}

// distinctVerificationCommands checks each declared verification command and
// keeps one of each, by its exact text, in the order declared.
func distinctVerificationCommands(in *TaskContract) ([]string, error) {
	vseen := map[string]bool{}
	var cmds []string
	for _, v := range in.VerificationCommands() {
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("task_contract.verification contains an empty entry")
		}
		if vseen[v] {
			continue // deduplicated by exact identity, not by resemblance
		}
		vseen[v] = true
		cmds = append(cmds, v)
	}
	return cmds, nil
}

// storeExpectedOutputs stores the declared output paths on out: a declared
// list, even an empty one, is present.
func storeExpectedOutputs(out *TaskContract, outKnow ObligationKnowledge, paths []string) {
	if outKnow == KnowledgeDeclared {
		if paths == nil {
			paths = []string{}
		}
		out.ExpectedOutputs = &paths
	}
}

// storeVerification stores the declared verification commands on out, present
// even when empty, with the typed requirements that came with them.
func storeVerification(out, in *TaskContract, verKnow ObligationKnowledge, cmds []string,
	typedReqs []VerificationRequirement) {
	if verKnow == KnowledgeDeclared {
		if cmds == nil {
			cmds = []string{}
		}
		out.Verification = &cmds
		if typedReqs != nil {
			out.VerificationRequirements = &typedReqs
			out.VerificationRequirementsVersion = in.VerificationRequirementsVersion
		}
	}
}

// normalizeKnowledge turns what the caller sent into a stated knowledge value,
// or refuses.
//
// The compatibility rules are asymmetric on purpose. A legacy NON-EMPTY list
// always meant "these are the obligations", so it normalises to declared and
// keeps its meaning. A legacy EMPTY or absent list cannot be promoted: the
// storage those clients were written against could not tell [] from omitted,
// so reading one as "authoritatively none" would invent an authority the
// caller never expressed.
func normalizeKnowledge(field string, stated ObligationKnowledge,
	present bool, count int) (ObligationKnowledge, error) {
	switch stated {
	case "":
		if present && count > 0 {
			return KnowledgeDeclared, nil // legacy non-empty keeps its meaning
		}
		return KnowledgeUnspecified, nil
	case KnowledgeUnspecified:
		if count > 0 {
			return "", fmt.Errorf(
				"task_contract.%s is %q but %d entries were sent; a caller "+
					"that knows its obligations must say so", field, stated, count)
		}
		return KnowledgeUnspecified, nil
	case KnowledgeDeclared:
		if !present {
			return "", fmt.Errorf(
				"task_contract.%s is %q but no list was sent; declaring "+
					"authority over an absent list says nothing", field, stated)
		}
		return KnowledgeDeclared, nil
	default:
		// Never coerced: an unrecognised value is a client asking for
		// something this build does not implement.
		return "", fmt.Errorf("task_contract.%s %q is not supported", field, stated)
	}
}

// --- Shadow comparison: what the client declared vs what ATLAS inferred ------
//
// Two records, because one cannot represent both. The request snapshot holds
// the immutable inputs -- the contract, the tier production already chose, each
// heuristic's own answer. The gate record holds an actual live decision, and
// there may be zero, one or several of those in a run: wantsStateChange reads
// inspectedWorkspace, which flips true once a read-only tool succeeds, so the
// same request legitimately answers false early and true later. A request-start
// approximation would be a different number from the one production used.
//
// Nothing here decides anything. Every function is called for its existing
// answer and the answer is recorded, not consulted.
// Record kinds version INDEPENDENTLY. Adding a field to one must not silently
// redefine another, and a sealed capture must stay readable by the analyzer
// written for the schema it was captured under.
//
// Gate v1 was legacy-observation only: it recorded what the heuristic said and
// nothing about what governed. Gate v2 adds the live action demand and the
// authority that produced it, so it is a different closed contract and carries
// a different number. The request snapshot and the footer did not change, so
// they stay at 1 rather than being bumped for tidiness.
const (
	shadowSchemaVersionRequest = 1
	shadowSchemaVersionGate    = 2
	shadowSchemaVersionFooter  = 1
	// One candidate evidence observation, written by the wired producer.
	shadowSchemaVersionEvidence = 1
	// One observe-only authorization decision.
	shadowSchemaVersionAuthorization = 1
	// One observe-only invocation feasibility answer.
	shadowSchemaVersionFeasibility = 1
	// How one route entry ended, and what became of the licence it minted.
	shadowSchemaVersionRouteDisposition    = 1
	shadowSchemaVersionDeliveryDisposition = 1
	// One candidate policy answer: which rule owned the decision, what
	// disqualified the candidate, and what was observed in its favour.
	shadowSchemaVersionCandidatePolicy = 1
	// One tool call's structured mutation scope, and whether the candidate
	// stayed inside it.
	shadowSchemaVersionMutationScope = 1
	// One acquisition-control record: what the policy would have done, and the
	// fact that no delivery followed it.
	shadowSchemaVersionCaptureOnly = 1
	// One mutation the candidate producer was never consulted for, and the
	// predicate that turned it away.
	shadowSchemaVersionGenerationBypass = 1
	// One typed answer per route entry to why an automatic candidate did not
	// land, copied from the decisions the live owners recorded.
	shadowSchemaVersionAutomaticAttribution = 1
)

// canonicalSource keeps an unknown decision source out of the record. The
// enum is closed and decideActionDemand can only produce its four members; if
// one ever escaped, it is written as the fail-closed member rather than as
// arbitrary prose.
func (s actionDemandSource) canonicalSource() string {
	switch s {
	case actionDemandLegacy, actionDemandContractWork,
		actionDemandContractQuestion, actionDemandContractInvalid:
		return string(s)
	default:
		return string(actionDemandContractInvalid)
	}
}

// shadowGateSite names the two live call sites, as a closed set.
type shadowGateSite string

const (
	shadowGateActionDemanded shadowGateSite = "action_demanded_and_unmet"
	shadowGateActionGate     shadowGateSite = "exit_action_gate"
)

// shadowComparison is the closed task-mode vocabulary. Only a gate record gets
// one, because only a gate record holds a live legacy decision.
const (
	shadowAgreeWork                  = "agree_work"
	shadowAgreeQuestion              = "agree_question"
	shadowContractWorkLegacyQuestion = "contract_work_legacy_question"
	shadowContractQuestionLegacyWork = "contract_question_legacy_work"
	shadowUnmeasured                 = "unmeasured"
)

// Declaration state and the two set-comparison vocabularies. There is no
// "invalid" state: an invalid contract is rejected at the request boundary and
// never reaches a run, so a record can only describe a contract that was
// declared or one that was absent.
const (
	shadowNotDeclared = "contract_not_declared"
	shadowDeclared    = "contract_declared"

	shadowOutputsExact        = "exact_agreement"
	shadowOutputsContractOnly = "contract_only"
	shadowOutputsLegacyOnly   = "legacy_only"
	shadowOutputsPartial      = "partial_overlap"
	shadowOutputsIncomparable = "incomparable"

	shadowVerifyLegacyRequires = "contract_declared_legacy_requires_verification"
	shadowVerifyLegacyDoesNot  = "contract_declared_legacy_does_not_require_verification"
)

// shadowHash is the stable identity used for joining and set comparison. It is
// never authority and never reversible to the original text.
func shadowHash(s string) string { return hashBytes([]byte(s))[:16] }

// shadowGateSeq is bounded per-request diagnostic state. Structurally unable to
// reach policy: nothing but the emitter reads it.
type shadowGateSeq struct{ n int }

// emitShadowRequestSnapshot records the immutable comparison inputs once per
// validated request. Every heuristic below is the existing function, called for
// the answer it already gives; no word list, regex or path rule is duplicated.
func emitShadowRequestSnapshot(ctx *AgentContext, userMessage string) {
	sink := activeShadowSink.Load()
	if !sink.enabled() {
		return // disabled: no hashing, no resolution, no heuristic calls
	}
	requestID := ""
	if ctx.Ctx != nil {
		requestID = requestIDFromContext(ctx.Ctx)
	}
	sink.noteRequest(requestID)

	tc := ctx.TaskContract
	rec := map[string]interface{}{
		"schema_version":           shadowSchemaVersionRequest,
		"record_kind":              "task_contract_shadow_request",
		"request_id":               requestID,
		"user_message_sha256":      hashBytes([]byte(userMessage)),
		"contract_present":         tc != nil,
		"tier":                     ctx.Tier.String(),
		"heuristic_action_intent":  isActionIntentMessage(userMessage),
		"heuristic_read_only":      isReadOnlyRequest(userMessage),
		"heuristic_explain_only":   isExplainOnlyMessage(strings.ToLower(userMessage)),
		"heuristic_question":       isQuestionMessage(userMessage),
		"heuristic_fix_intent":     isFixIntentMessage(userMessage),
		"influences_live_decision": false,
		"build_version":            APIVersion,
	}
	if tc != nil {
		rec["contract_provenance"] = "client"
		rec["task_mode"] = string(tc.TaskMode)
	}

	// Legacy deliverables, canonicalised through the resolver every tool uses.
	legacy := expectedOutputPaths(userMessage)
	legacyCanon, legacyFails := shadowCanonicalSet(ctx, legacy)
	rec["legacy_output_count"] = len(legacy)
	rec["legacy_output_hashes"] = shadowHashes(legacyCanon)

	declared := tc != nil && len(tc.OutputPaths()) > 0
	contractCanon, contractFails := shadowCanonicalSet(ctx, contractOutputs(tc))
	rec["canonicalization_failures"] = legacyFails + contractFails
	if declared {
		rec["output_declaration_state"] = shadowDeclared
		rec["output_count"] = len(tc.OutputPaths())
		rec["output_hashes"] = shadowHashes(contractCanon)
		rec["output_comparison"] = shadowCompareSets(contractCanon, legacyCanon,
			contractFails+legacyFails > 0)
	} else {
		rec["output_declaration_state"] = shadowNotDeclared
		rec["output_count"] = 0
		rec["output_comparison"] = shadowNotDeclared
	}

	// Verification: the legacy side is a boolean demand and never a command,
	// so exact agreement is not a claim this can make.
	if tc != nil && len(tc.VerificationCommands()) > 0 {
		rec["verification_declaration_state"] = shadowDeclared
		rec["verification_count"] = len(tc.VerificationCommands())
		rec["verification_hashes"] = shadowHashes(tc.VerificationCommands())
		if isFixIntentMessage(userMessage) {
			rec["verification_comparison"] = shadowVerifyLegacyRequires
		} else {
			rec["verification_comparison"] = shadowVerifyLegacyDoesNot
		}
	} else {
		rec["verification_declaration_state"] = shadowNotDeclared
		rec["verification_count"] = 0
		rec["verification_comparison"] = shadowNotDeclared
	}
	sink.submit(rec)
}

func contractOutputs(tc *TaskContract) []string {
	if tc == nil {
		return nil
	}
	return tc.OutputPaths()
}

// shadowCanonicalSet resolves each path through resolveWorkspacePath -- the one
// canonicalisation rule -- and counts what could not be resolved.
func shadowCanonicalSet(ctx *AgentContext, paths []string) ([]string, int) {
	seen := map[string]bool{}
	var out []string
	fails := 0
	for _, p := range paths {
		canon, err := resolveWorkspacePath(ctx, p)
		if err != nil {
			fails++
			continue
		}
		if !seen[canon] {
			seen[canon] = true
			out = append(out, canon)
		}
	}
	sort.Strings(out)
	return out, fails
}

func shadowHashes(items []string) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, shadowHash(it))
	}
	return out
}

// shadowCompareSets classifies two canonical sets.
func shadowCompareSets(contract, legacy []string, failed bool) string {
	if failed {
		return shadowOutputsIncomparable
	}
	inLegacy := map[string]bool{}
	for _, l := range legacy {
		inLegacy[l] = true
	}
	overlap := 0
	for _, c := range contract {
		if inLegacy[c] {
			overlap++
		}
	}
	switch {
	case overlap == len(contract) && overlap == len(legacy):
		return shadowOutputsExact
	case overlap == 0 && len(legacy) == 0:
		return shadowOutputsContractOnly
	case overlap == 0 && len(contract) == 0:
		return shadowOutputsLegacyOnly
	case overlap == 0:
		return shadowOutputsIncomparable
	default:
		return shadowOutputsPartial
	}
}

// answerOnlyRequest reports a request whose deliverable is an answer: the
// client declared a question, or the user forbade any change. Steers read it;
// the exit gates keep deciding the action demand (decideActionDemand).
func answerOnlyRequest(ctx *AgentContext) bool {
	return mutationForbidden(ctx) || (ctx.TaskContract != nil && ctx.TaskContract.TaskMode == TaskModeQuestion)
}

// observeActionDemand records one live action-demand decision and returns it
// unchanged. The observer cannot alter the answer: it receives a decision that
// has already been made and hands the same value back.
//
// The record carries BOTH the legacy heuristic's answer and the live decision,
// so a capture shows what the old signal would have said next to what actually
// governed. influences_live_decision stays false because it describes this
// observer and its sink, not whether the contract is authoritative.
func observeActionDemand(ctx *AgentContext, st *runState, site shadowGateSite,
	d actionDemand) bool {
	sink := activeShadowSink.Load()
	if !sink.enabled() {
		return d.Required
	}
	st.shadowGate.n++
	requestID := ""
	if ctx.Ctx != nil {
		requestID = requestIDFromContext(ctx.Ctx)
	}
	comparison := shadowUnmeasured
	mode := ""
	if tc := ctx.TaskContract; tc != nil {
		mode = string(tc.TaskMode)
		switch {
		case tc.TaskMode == TaskModeWork && d.Legacy:
			comparison = shadowAgreeWork
		case tc.TaskMode == TaskModeQuestion && !d.Legacy:
			comparison = shadowAgreeQuestion
		case tc.TaskMode == TaskModeWork && !d.Legacy:
			comparison = shadowContractWorkLegacyQuestion
		case tc.TaskMode == TaskModeQuestion && d.Legacy:
			comparison = shadowContractQuestionLegacyWork
		}
	}
	sink.submit(map[string]interface{}{
		"schema_version":            shadowSchemaVersionGate,
		"record_kind":               "task_contract_shadow_gate",
		"request_id":                requestID,
		"gate_seq":                  st.shadowGate.n,
		"call_site":                 string(site),
		"inspected_workspace":       st.inspectedWorkspace,
		"tier":                      ctx.Tier.String(),
		"legacy_wants_state_change": d.Legacy,
		"live_action_demand":        d.Required,
		"action_demand_source":      d.Source.canonicalSource(),
		"contract_task_mode":        mode,
		"comparison":                comparison,
		"influences_live_decision":  false,
	})
	return d.Required
}

// removedSwitchRefusal names a request field that asks for a switch this build
// no longer has, or returns "". Values that ask for what always happens are
// accepted: bypass_v3 false, v3_mode full and feasibility_mode observe.
func removedSwitchRefusal(bypassV3 *bool, v3Mode, feasibilityMode string) string {
	switch {
	case bypassV3 != nil && *bypassV3,
		v3Mode != "" && v3Mode != "full",
		feasibilityMode != "" && feasibilityMode != "observe":
		return "bypass_v3, v3_mode and feasibility_mode were removed: V3 runs on every " +
			"request, and feasibility is recorded, never enforced. Measuring without " +
			"V3 takes a research build, not a request field."
	}
	return ""
}

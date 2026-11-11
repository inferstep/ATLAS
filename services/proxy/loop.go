package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/itigges22/atlas-proxy/internal/fencing"
	"github.com/itigges22/atlas-proxy/internal/wire"
)

// checkCategory records a known cause or failed operation, separately from turn control.
type checkCategory uint8

const (
	checkPass checkCategory = iota + 1
	checkGuardRefusal
	checkToolError
	checkModelError
	checkInfrastructureError
)

type checkResult struct {
	category checkCategory
	outcome  turnOutcome
}

type orderedCheck[T any] struct {
	name string
	run  func(*T) checkResult
}

func runCheckPhase[T any](checks []orderedCheck[T], input *T) checkResult {
	for _, check := range checks {
		result := check.run(input)
		if result.outcome != turnContinues {
			return result
		}
	}
	return checkResult{checkPass, turnContinues}
}

// proxyChecks owns the order within each existing execution phase.
// Preparation rows preserve the boundaries between checks with shared state.
var proxyChecks struct {
	request              []orderedCheck[requestCheckInput]
	requestDirectory     []orderedCheck[requestServiceCheckInput]
	requestConfiguration []orderedCheck[requestServiceCheckInput]
	requestServices      []orderedCheck[requestServiceCheckInput]
	stream               []orderedCheck[streamCheckInput]
	conversation         []orderedCheck[AgentContext]
	modelStart           []orderedCheck[modelCheckInput]
	modelInference       []orderedCheck[modelCheckInput]
	modelReply           []orderedCheck[modelCheckInput]
	beforeWorkspace      []orderedCheck[toolCheckInput]
	workspace            []orderedCheck[toolCheckInput]
	afterWorkspace       []orderedCheck[toolCheckInput]
	writePreparation     []orderedCheck[writePreparationCheckInput]
	warnedWrite          []orderedCheck[warnedWriteCheckInput]
	fencedCall           []orderedCheck[fencedCallCheckInput]
	fencedFetch          []orderedCheck[fencedFetchCheckInput]
	fencedResolution     []orderedCheck[writePreparationCheckInput]
	existingWrite        []orderedCheck[existingWriteCheckInput]
	beforeDispatch       []orderedCheck[toolCheckInput]
	toolHealth           []orderedCheck[toolHealthCheckInput]
	dispatchSelection    []orderedCheck[dispatchSelectionCheckInput]
	dispatch             []orderedCheck[dispatchCheckInput]
	dispatchAfter        []orderedCheck[dispatchSelectionCheckInput]
	toolReported         []orderedCheck[toolHealthCheckInput]
	failure              []orderedCheck[failureCheckInput]
	readFile             []orderedCheck[readFileCheckInput]
	outlineInput         []orderedCheck[outlineInputCheckInput]
	searchInput          []orderedCheck[searchInputCheckInput]
	findInput            []orderedCheck[findInputCheckInput]
	writeInput           []orderedCheck[writeEarlyCheckInput]
	writeEarly           []orderedCheck[writeEarlyCheckInput]
	fastWrite            []orderedCheck[fastWriteCheckInput]
	directWrite          []orderedCheck[fastWriteCheckInput]
	directLanding        []orderedCheck[directLandingCheckInput]
	editInput            []orderedCheck[editInputCheckInput]
	editRead             []orderedCheck[editReadCheckInput]
	matchedEdit          []orderedCheck[matchedEditCheckInput]
	composedEdit         []orderedCheck[composedEditCheckInput]
	editLanding          []orderedCheck[editLandingCheckInput]
	insertInput          []orderedCheck[insertInputCheckInput]
	replaceInput         []orderedCheck[replaceInputCheckInput]
	lineRead             []orderedCheck[lineReadCheckInput]
	insertion            []orderedCheck[insertionCheckInput]
	lineReplacement      []orderedCheck[lineReplacementCheckInput]
	lineLanding          []orderedCheck[lineLandingCheckInput]
	structuralInput      []orderedCheck[structuralInputCheckInput]
	structuralRead       []orderedCheck[structuralReadCheckInput]
	structuralSplice     []orderedCheck[structuralSpliceCheckInput]
	structuralLanding    []orderedCheck[structuralLandingCheckInput]
	deleteInput          []orderedCheck[deleteCheckInput]
	deleteTarget         []orderedCheck[deleteCheckInput]
	deleteApproval       []orderedCheck[deleteCheckInput]
	move                 []orderedCheck[moveCheckInput]
	commandTrust         []orderedCheck[commandCheckInput]
	commandMemory        []orderedCheck[commandCheckInput]
	background           []orderedCheck[backgroundCheckInput]
	tailInput            []orderedCheck[tailInputCheckInput]
	stopInput            []orderedCheck[stopInputCheckInput]
	editCandidate        []orderedCheck[editCandidateCheckInput]
	candidate            []orderedCheck[candidateCheckInput]
	delivery             []orderedCheck[deliveryCheckInput]
	settlement           []orderedCheck[settlementCheckInput]
	exit                 []orderedCheck[exitCheckInput]
	completion           []orderedCheck[completionCheckInput]
}

func init() {
	proxyChecks.request = []orderedCheck[requestCheckInput]{
		{"request.method", (*requestCheckInput).method},
		{"request.decode", (*requestCheckInput).decode},
		{"request.message", (*requestCheckInput).message},
	}
}

func init() {
	proxyChecks.requestDirectory = []orderedCheck[requestServiceCheckInput]{
		{"requestDirectory.directory", (*requestServiceCheckInput).directory},
	}
}

func init() {
	proxyChecks.requestConfiguration = []orderedCheck[requestServiceCheckInput]{
		{"requestConfiguration.configuration", (*requestServiceCheckInput).configuration},
	}
}

func init() {
	proxyChecks.requestServices = []orderedCheck[requestServiceCheckInput]{
		{"requestServices.contract", (*requestServiceCheckInput).contract},
		{"requestServices.lens", (*requestServiceCheckInput).lens},
	}
}

func init() {
	proxyChecks.stream = []orderedCheck[streamCheckInput]{
		{"stream.supported", (*streamCheckInput).supported},
	}
}

func init() {
	proxyChecks.conversation = []orderedCheck[AgentContext]{
		{"conversation.verifyWorkspaceAlignment", func(ctx *AgentContext) checkResult {

			if problem := verifyWorkspaceAlignment(ctx); problem != "" {
				log.Printf("[agent] refusing to start — proxy and sandbox workspaces are not aligned")
				emitTerminal(ctx, nil, wire.TerminalFailed, "workspace_misaligned", problem)
				return checkResult{checkInfrastructureError, runEnds}
			}
			return checkResult{checkPass, turnContinues}
		}},
	}
}

func init() {
	proxyChecks.modelStart = []orderedCheck[modelCheckInput]{
		{"modelStart.cancellation", (*modelCheckInput).cancellation},
	}
}

func init() {
	proxyChecks.modelInference = []orderedCheck[modelCheckInput]{
		{"modelInference.inference", (*modelCheckInput).inference},
	}
}

func init() {
	proxyChecks.modelReply = []orderedCheck[modelCheckInput]{
		{"modelReply.parse", (*modelCheckInput).parse},
		{"modelReply.swallowedContent", (*modelCheckInput).swallowedContent},
		{"modelReply.ambiguousContent", (*modelCheckInput).ambiguousContent},
	}
}

func init() {
	proxyChecks.beforeWorkspace = []orderedCheck[toolCheckInput]{
		// Check permissions. In default and accept-edits modes a
		// destructive tool pauses the loop until the client approves or
		// denies it (via POST /v1/permission). Yolo mode and pre-approved
		// tools short-circuit needsPermission and never reach here. The
		// legacy PermissionFn is still honored for non-interactive callers.
		//
		// Deletion runs this LATER -- see below. It has to be asked after
		// the path is canonicalised and its structured intent is on the
		// record, or a refusal leaves nothing owed and a run whose only
		// act was a refused deletion reports completed.
		{"beforeWorkspace.permission", func(input *toolCheckInput) checkResult {
			if input.parsed.Name != "delete_file" {
				return input.permission()
			}
			return checkResult{checkPass, turnContinues}
		}},
		{"beforeWorkspace.refuseTruncatedArgs", func(input *toolCheckInput) checkResult {
			return turnCheckResult(checkModelError, input.loop.refuseTruncatedArgs(input.turn, *input.parsed))
		}},
	}
}

func init() {
	proxyChecks.workspace = []orderedCheck[toolCheckInput]{
		{"workspace.workspace", (*toolCheckInput).workspace},
	}
}

func init() {
	proxyChecks.afterWorkspace = []orderedCheck[toolCheckInput]{
		// The intent is on the record from here: the arguments parse, the
		// path is inside the workspace, and for every tool but delete_file
		// permission was granted (deletion asks just below). Every later
		// refusal -- a gate, a failed fenced resolution, a failed write --
		// leaves the debt standing, so these pre-dispatch failures do not
		// disappear from the record.
		{"afterWorkspace.noteMutationIntent", func(input *toolCheckInput) checkResult {
			noteMutationIntent(input.loop.ctx, input.loop.st, input.parsed.Name, input.parsed.Args)
			return checkResult{checkPass, turnContinues}
		}},
		// Deletion asks here, not with the others. By this point the path
		// has parsed, canonicalised and cleared the workspace boundary,
		// and the structured intent is recorded -- so a refusal, a denial,
		// a timeout or a cancel all leave the debt standing and the run
		// cannot call itself finished. Asked any earlier, a refused deletion
		// would owe nothing, and a run whose only act was that refused
		// deletion would report completed.
		{"afterWorkspace.permission", func(input *toolCheckInput) checkResult {
			if input.parsed.Name == "delete_file" {
				return input.permission()
			}
			return checkResult{checkPass, turnContinues}
		}},
		{"afterWorkspace.runFirstGateForEdits", func(input *toolCheckInput) checkResult {
			return turnCheckResult(checkGuardRefusal, input.loop.runFirstGateForEdits(*input.parsed))
		}},
		{"afterWorkspace.prepareWriteFile", func(input *toolCheckInput) checkResult {
			return input.loop.prepareWriteFile(input.turn, input.parsed, input.intentArgs)
		}},
	}
}

func init() {
	proxyChecks.writePreparation = []orderedCheck[writePreparationCheckInput]{
		{"writePreparation.runFirstGateForWrite", func(input *writePreparationCheckInput) checkResult {
			return turnCheckResult(checkGuardRefusal, input.loop.runFirstGateForWrite(input.write))
		}},
		{"writePreparation.resolveFencedContent", func(input *writePreparationCheckInput) checkResult {
			return input.loop.resolveFencedContent(input.turn, input.parsed, input.intentArgs, input.write)
		}},
	}
}

func init() {
	proxyChecks.warnedWrite = []orderedCheck[warnedWriteCheckInput]{
		{"warnedWrite.recovery", (*warnedWriteCheckInput).recovery},
		{"warnedWrite.bounce", (*warnedWriteCheckInput).bounce},
	}
}

func init() {
	proxyChecks.fencedCall = []orderedCheck[fencedCallCheckInput]{
		{"fencedCall.arguments", (*fencedCallCheckInput).arguments},
		{"fencedCall.decode", (*fencedCallCheckInput).decode},
		{"fencedCall.path", (*fencedCallCheckInput).path},
		{"fencedCall.workspace", (*fencedCallCheckInput).workspace},
		{"fencedCall.denyList", (*fencedCallCheckInput).denyList},
	}
}

func init() {
	proxyChecks.fencedFetch = []orderedCheck[fencedFetchCheckInput]{
		{"fencedFetch.channel", (*fencedFetchCheckInput).channel},
		{"fencedFetch.allowance", (*fencedFetchCheckInput).allowance},
		{"fencedFetch.budget", (*fencedFetchCheckInput).budget},
	}
}

func init() {
	proxyChecks.fencedResolution = []orderedCheck[writePreparationCheckInput]{
		{"fencedResolution.recoverChannel", (*writePreparationCheckInput).recoverChannel},
		{"fencedResolution.replaceFencedSentinel", func(input *writePreparationCheckInput) checkResult {
			return input.loop.replaceFencedSentinel(input.turn, input.parsed, input.intentArgs, input.write, input.trimmed)
		}},
	}
}

func init() {
	proxyChecks.existingWrite = []orderedCheck[existingWriteCheckInput]{
		{"existingWrite.unread", (*existingWriteCheckInput).unread},
		{"existingWrite.rewrite", (*existingWriteCheckInput).rewrite},
	}
}

func init() {
	proxyChecks.beforeDispatch = []orderedCheck[toolCheckInput]{
		{"beforeDispatch.redirectForegroundServer", func(input *toolCheckInput) checkResult {
			return turnCheckResult(checkGuardRefusal, input.loop.redirectForegroundServer(*input.parsed, input.intentArgs))
		}},
		{"beforeDispatch.refuseUnsafeRunCommand", func(input *toolCheckInput) checkResult {
			return turnCheckResult(checkGuardRefusal, input.loop.refuseUnsafeRunCommand(*input.parsed, input.intentArgs))
		}},
		{"beforeDispatch.refuseUnsafeRunBackground", func(input *toolCheckInput) checkResult {
			return turnCheckResult(checkGuardRefusal, input.loop.refuseUnsafeRunBackground(*input.parsed, input.intentArgs))
		}},
		{"beforeDispatch.countWholeFileWrites", func(input *toolCheckInput) checkResult {
			input.runawayWrite = input.loop.countWholeFileWrites(*input.parsed)
			return checkResult{checkPass, turnContinues}
		}},
		{"beforeDispatch.refuseBannedTool", func(input *toolCheckInput) checkResult {
			return turnCheckResult(checkGuardRefusal, input.loop.refuseBannedTool(input.turn, *input.parsed))
		}},
		{"beforeDispatch.answerRejectedProposal", func(input *toolCheckInput) checkResult {
			return turnCheckResult(checkGuardRefusal, input.loop.answerRejectedProposal(*input.parsed, input.intentArgs))
		}},
		{"beforeDispatch.answerBrokenArtifact", func(input *toolCheckInput) checkResult {
			return turnCheckResult(checkGuardRefusal, input.loop.answerBrokenArtifact(*input.parsed, input.intentArgs))
		}},
		{"beforeDispatch.refuseIdenticalRetry", func(input *toolCheckInput) checkResult {
			return turnCheckResult(checkGuardRefusal, input.loop.refuseIdenticalRetry(input.turn, *input.parsed, input.intentArgs))
		}},
	}
}

func init() {
	proxyChecks.toolHealth = []orderedCheck[toolHealthCheckInput]{
		{"toolHealth.repetition", (*toolHealthCheckInput).repetition},
		{"toolHealth.reasoning", (*toolHealthCheckInput).reasoning},
		{"toolHealth.lens", (*toolHealthCheckInput).lens},
	}
}

func init() {
	proxyChecks.dispatchSelection = []orderedCheck[dispatchSelectionCheckInput]{
		{"dispatchSelection.cachedRead", (*dispatchSelectionCheckInput).cachedRead},
		{"dispatchSelection.rerun", (*dispatchSelectionCheckInput).rerun},
	}
}

func init() {
	proxyChecks.dispatch = []orderedCheck[dispatchCheckInput]{
		{"dispatch.knownTool", (*dispatchCheckInput).knownTool},
		{"dispatch.arguments", (*dispatchCheckInput).arguments},
		{"dispatch.denyList", (*dispatchCheckInput).denyList},
		{"dispatch.readOnly", (*dispatchCheckInput).readOnly},
	}
}

func init() {
	proxyChecks.dispatchAfter = []orderedCheck[dispatchSelectionCheckInput]{
		{"dispatchAfter.cancelled", (*dispatchSelectionCheckInput).cancelled},
	}
}

func init() {
	proxyChecks.toolReported = []orderedCheck[toolHealthCheckInput]{
		{"toolReported.reportedLensFailure", (*toolHealthCheckInput).reportedLensFailure},
	}
}

func init() {
	proxyChecks.failure = []orderedCheck[failureCheckInput]{
		{"failure.ceiling", (*failureCheckInput).ceiling},
		{"failure.samePath", (*failureCheckInput).samePath},
	}
}

func init() {
	proxyChecks.readFile = []orderedCheck[readFileCheckInput]{
		{"readFile.pathCheck", (*readFileCheckInput).pathCheck},
		{"readFile.read", (*readFileCheckInput).read},
		{"readFile.binary", (*readFileCheckInput).binary},
	}
}

func init() {
	proxyChecks.outlineInput = []orderedCheck[outlineInputCheckInput]{
		{"outlineInput.required", (*outlineInputCheckInput).required},
	}
}

func init() {
	proxyChecks.searchInput = []orderedCheck[searchInputCheckInput]{
		{"searchInput.required", (*searchInputCheckInput).required},
	}
}

func init() {
	proxyChecks.findInput = []orderedCheck[findInputCheckInput]{
		{"findInput.required", (*findInputCheckInput).required},
	}
}

func init() {
	proxyChecks.writeInput = []orderedCheck[writeEarlyCheckInput]{
		{"writeInput.pathCheck", (*writeEarlyCheckInput).pathCheck},
	}
}

func init() {
	proxyChecks.writeEarly = []orderedCheck[writeEarlyCheckInput]{
		{"writeEarly.sibling", (*writeEarlyCheckInput).sibling},
		{"writeEarly.echo", (*writeEarlyCheckInput).echo},
		{"writeEarly.stub", (*writeEarlyCheckInput).stub},
	}
}

func init() {
	proxyChecks.fastWrite = []orderedCheck[fastWriteCheckInput]{
		{"fastWrite.syntax", (*fastWriteCheckInput).syntax},
		{"fastWrite.unresolved", (*fastWriteCheckInput).unresolved},
	}
}

func init() {
	proxyChecks.directWrite = []orderedCheck[fastWriteCheckInput]{
		{"directWrite.directUnresolved", (*fastWriteCheckInput).directUnresolved},
	}
}

func init() {
	proxyChecks.directLanding = []orderedCheck[directLandingCheckInput]{
		{"directLanding.newFileWarning", (*directLandingCheckInput).newFileWarning},
		{"directLanding.rewriteWarning", (*directLandingCheckInput).rewriteWarning},
		// The two comparative gates the V3 route applies to the bytes it
		// writes (writeFileWithV3). They are checks on the write, not part
		// of candidate generation, so a write that does not generate --
		// small, mid-iteration, or with nothing deliverable -- must not skip
		// them. Healthy->broken and fail-soft, as there.
		{"directLanding.readBaseline", (*directLandingCheckInput).readBaseline},
		{"directLanding.embedded", (*directLandingCheckInput).embedded},
		{"directLanding.entrypoint", (*directLandingCheckInput).entrypoint},
	}
}

func init() {
	proxyChecks.editInput = []orderedCheck[editInputCheckInput]{
		{"editInput.path", (*editInputCheckInput).path},
		{"editInput.oldString", (*editInputCheckInput).oldString},
	}
}

func init() {
	proxyChecks.editRead = []orderedCheck[editReadCheckInput]{
		{"editRead.currentView", (*editReadCheckInput).currentView},
		{"editRead.read", (*editReadCheckInput).read},
		{"editRead.staleness", (*editReadCheckInput).staleness},
	}
}

func init() {
	proxyChecks.matchedEdit = []orderedCheck[matchedEditCheckInput]{
		{"matchedEdit.unique", (*matchedEditCheckInput).unique},
		{"matchedEdit.changed", (*matchedEditCheckInput).changed},
	}
}

func init() {
	proxyChecks.composedEdit = []orderedCheck[composedEditCheckInput]{
		{"composedEdit.shrinkage", (*composedEditCheckInput).shrinkage},
		{"composedEdit.unchanged", (*composedEditCheckInput).unchanged},
		{"composedEdit.alreadyApplied", (*composedEditCheckInput).alreadyApplied},
	}
}

func init() {
	proxyChecks.editLanding = []orderedCheck[editLandingCheckInput]{
		{"editLanding.syntax", (*editLandingCheckInput).syntax},
		{"editLanding.unresolvedCalls", (*editLandingCheckInput).unresolvedCalls},
		{"editLanding.embeddedScript", (*editLandingCheckInput).embeddedScript},
		{"editLanding.entrypoint", (*editLandingCheckInput).entrypoint},
	}
}

func init() {
	proxyChecks.insertInput = []orderedCheck[insertInputCheckInput]{
		{"insertInput.path", (*insertInputCheckInput).path},
		{"insertInput.content", (*insertInputCheckInput).content},
	}
}

func init() {
	proxyChecks.replaceInput = []orderedCheck[replaceInputCheckInput]{
		{"replaceInput.path", (*replaceInputCheckInput).path},
	}
}

func init() {
	proxyChecks.lineRead = []orderedCheck[lineReadCheckInput]{
		{"lineRead.currentView", (*lineReadCheckInput).currentView},
		{"lineRead.read", (*lineReadCheckInput).read},
	}
}

func init() {
	proxyChecks.insertion = []orderedCheck[insertionCheckInput]{
		{"insertion.bounds", (*insertionCheckInput).bounds},
		{"insertion.reparenting", (*insertionCheckInput).reparenting},
	}
}

func init() {
	proxyChecks.lineReplacement = []orderedCheck[lineReplacementCheckInput]{
		{"lineReplacement.boundsCheck", (*lineReplacementCheckInput).boundsCheck},
		{"lineReplacement.spanCheck", (*lineReplacementCheckInput).spanCheck},
		{"lineReplacement.relocateCheck", (*lineReplacementCheckInput).relocateCheck},
		{"lineReplacement.firstLineCheck", (*lineReplacementCheckInput).firstLineCheck},
		{"lineReplacement.lastLineCheck", (*lineReplacementCheckInput).lastLineCheck},
		{"lineReplacement.replacementCheck", (*lineReplacementCheckInput).replacementCheck},
	}
}

func init() {
	proxyChecks.lineLanding = []orderedCheck[lineLandingCheckInput]{
		{"lineLanding.syntax", (*lineLandingCheckInput).syntax},
		{"lineLanding.unresolved", (*lineLandingCheckInput).unresolved},
		{"lineLanding.embedded", (*lineLandingCheckInput).embedded},
		{"lineLanding.entrypoint", (*lineLandingCheckInput).entrypoint},
	}
}

func init() {
	proxyChecks.structuralInput = []orderedCheck[structuralInputCheckInput]{
		{"structuralInput.path", (*structuralInputCheckInput).path},
		{"structuralInput.selector", (*structuralInputCheckInput).selector},
	}
}

func init() {
	proxyChecks.structuralRead = []orderedCheck[structuralReadCheckInput]{
		{"structuralRead.currentView", (*structuralReadCheckInput).currentView},
		{"structuralRead.read", (*structuralReadCheckInput).read},
		{"structuralRead.empty", func(c *structuralReadCheckInput) checkResult { return c.empty() }},
		{"structuralRead.runaway", func(c *structuralReadCheckInput) checkResult { return c.runaway() }},
		{"structuralRead.staleness", (*structuralReadCheckInput).staleness},
	}
}

func init() {
	proxyChecks.structuralSplice = []orderedCheck[structuralSpliceCheckInput]{
		{"structuralSplice.service", (*structuralSpliceCheckInput).service},
		{"structuralSplice.shrinkage", (*structuralSpliceCheckInput).shrinkage},
		{"structuralSplice.unchanged", (*structuralSpliceCheckInput).unchanged},
	}
}

func init() {
	proxyChecks.structuralLanding = []orderedCheck[structuralLandingCheckInput]{
		{"structuralLanding.syntax", (*structuralLandingCheckInput).syntax},
		{"structuralLanding.unresolvedCalls", (*structuralLandingCheckInput).unresolvedCalls},
		{"structuralLanding.embeddedScript", (*structuralLandingCheckInput).embeddedScript},
		{"structuralLanding.entrypoint", (*structuralLandingCheckInput).entrypoint},
	}
}

func init() {
	proxyChecks.deleteInput = []orderedCheck[deleteCheckInput]{
		{"deleteInput.pathCheck", (*deleteCheckInput).pathCheck},
	}
}

func init() {
	proxyChecks.deleteTarget = []orderedCheck[deleteCheckInput]{
		{"deleteTarget.exists", (*deleteCheckInput).exists},
		{"deleteTarget.empty", (*deleteCheckInput).empty},
	}
}

func init() {
	proxyChecks.deleteApproval = []orderedCheck[deleteCheckInput]{
		{"deleteApproval.identity", (*deleteCheckInput).identity},
		{"deleteApproval.capacity", (*deleteCheckInput).capacity},
	}
}

func init() {
	proxyChecks.move = []orderedCheck[moveCheckInput]{
		{"move.paths", (*moveCheckInput).paths},
		{"move.source", (*moveCheckInput).source},
		{"move.destination", (*moveCheckInput).destination},
		{"move.different", (*moveCheckInput).different},
		{"move.absent", (*moveCheckInput).absent},
	}
}

func init() {
	proxyChecks.commandTrust = []orderedCheck[commandCheckInput]{
		{"commandTrust.trust", (*commandCheckInput).trust},
	}
}

func init() {
	proxyChecks.commandMemory = []orderedCheck[commandCheckInput]{
		{"commandMemory.memory", (*commandCheckInput).memory},
	}
}

func init() {
	proxyChecks.background = []orderedCheck[backgroundCheckInput]{
		{"background.trust", (*backgroundCheckInput).trust},
		{"background.command", (*backgroundCheckInput).command},
		{"background.shell", (*backgroundCheckInput).shell},
		{"background.host", (*backgroundCheckInput).host},
	}
}

func init() {
	proxyChecks.tailInput = []orderedCheck[tailInputCheckInput]{
		{"tailInput.required", (*tailInputCheckInput).required},
	}
}

func init() {
	proxyChecks.stopInput = []orderedCheck[stopInputCheckInput]{
		{"stopInput.required", (*stopInputCheckInput).required},
	}
}

func init() {
	proxyChecks.editCandidate = []orderedCheck[editCandidateCheckInput]{
		{"editCandidate.empty", (*editCandidateCheckInput).empty},
		{"editCandidate.unchanged", (*editCandidateCheckInput).unchanged},
		{"editCandidate.rewrite", (*editCandidateCheckInput).rewrite},
		{"editCandidate.language", (*editCandidateCheckInput).language},
	}
}

func init() {
	proxyChecks.candidate = []orderedCheck[candidateCheckInput]{
		{"candidate.withdrawRefusedCandidate", func(input *candidateCheckInput) checkResult {
			return input.accept(checkGuardRefusal, withdrawRefusedCandidate(input.write))
		}},
		{"candidate.refuseContentThatDoesNotParse", func(input *candidateCheckInput) checkResult {
			return input.accept(checkGuardRefusal, refuseContentThatDoesNotParse(input.write))
		}},
		{"candidate.refuseUnresolvedCalls", func(input *candidateCheckInput) checkResult {
			return input.accept(checkGuardRefusal, refuseUnresolvedCalls(input.write))
		}},
		{"candidate.refuseBrokenEmbeddedScript", func(input *candidateCheckInput) checkResult {
			return input.accept(checkGuardRefusal, refuseBrokenEmbeddedScript(input.write))
		}},
		{"candidate.refuseDuplicateEntrypoint", func(input *candidateCheckInput) checkResult {
			return input.accept(checkGuardRefusal, refuseDuplicateEntrypoint(input.write))
		}},
		{"candidate.refuseCancelledWrite", func(input *candidateCheckInput) checkResult {
			return input.accept(checkInfrastructureError, refuseCancelledWrite(input.write))
		}},
	}
}

func init() {
	proxyChecks.delivery = []orderedCheck[deliveryCheckInput]{
		{"delivery.authorization", (*deliveryCheckInput).authorization},
		{"delivery.hash", (*deliveryCheckInput).hash},
		{"delivery.target", (*deliveryCheckInput).target},
	}
}

func init() {
	proxyChecks.settlement = []orderedCheck[settlementCheckInput]{
		{"settlement.read", (*settlementCheckInput).read},
		{"settlement.identity", (*settlementCheckInput).identity},
		{"settlement.validation", (*settlementCheckInput).validation},
	}
}

func init() {
	proxyChecks.exit = []orderedCheck[exitCheckInput]{
		{"exit.repairGate", func(input *exitCheckInput) checkResult {
			return input.result(input.state.repairGate(input.ctx))
		}},
		// The intent rows hold a reply that is not an answer yet: one that promises the
		// content, announces a tool call it never made, or closes by deferring the
		// work. A reply that says it cannot do the work is marked, not sent back.
		{"exit.promisedContentGate", func(input *exitCheckInput) checkResult {
			return input.result(input.state.promisedContentGate(input.ctx, input.claimText))
		}},
		{"exit.announcedToolGate", func(input *exitCheckInput) checkResult {
			return input.result(input.state.announcedToolGate(input.ctx, input.claimText))
		}},
		{"exit.deferredReplyGate", func(input *exitCheckInput) checkResult {
			return input.result(input.state.deferredReplyGate(input.ctx, input.claimText))
		}},
		{"exit.handoffGate", func(input *exitCheckInput) checkResult {
			return input.result(input.state.handoffGate(input.ctx, input.claimText))
		}},
		// The evidence rows hold a reply that cites a file the run never read, or code
		// that no read showed.
		{"exit.unreadCitationGate", func(input *exitCheckInput) checkResult {
			return input.result(input.state.unreadCitationGate(input.ctx, input.claimText))
		}},
		{"exit.unshownSymbolGate", func(input *exitCheckInput) checkResult {
			return input.result(input.state.unshownSymbolGate(input.ctx, input.claimText))
		}},
		{"exit.scopeGate", func(input *exitCheckInput) checkResult {
			return input.result(input.state.scopeGate(input.ctx, input.userMessage, input.claimText))
		}},
		{"exit.runFirstGateAtExit", func(input *exitCheckInput) checkResult {
			return input.result(input.state.runFirstGateAtExit(input.ctx))
		}},
		{"exit.routeContractGate", func(input *exitCheckInput) checkResult {
			return input.result(input.state.routeContractGate(input.ctx))
		}},
		{"exit.prepareContract", (*exitCheckInput).prepareContract},
		{"exit.backgroundGate", func(input *exitCheckInput) checkResult {
			return input.result(input.state.backgroundGate(input.ctx, input.contractOwed))
		}},
		// The verification rows hold an exit whose verification is missing, out of date
		// or partial, in this order: a run only ever fed through stdin, bytes that
		// changed after the run that verified them, no verifying run at all, a probe
		// of older code, and a work contract that nothing current shows working.
		{"exit.redirectGate", func(input *exitCheckInput) checkResult {
			return input.result(input.state.redirectGate(input.ctx))
		}},
		{"exit.artifactGate", func(input *exitCheckInput) checkResult {
			return input.result(input.state.artifactGate(input.ctx))
		}},
		{"exit.verificationGate", func(input *exitCheckInput) checkResult {
			return input.result(input.state.verificationGate(input.ctx))
		}},
		{"exit.staleProbeGate", func(input *exitCheckInput) checkResult {
			return input.result(input.state.staleProbeGate(input.ctx))
		}},
		{"exit.contractVerificationGate", func(input *exitCheckInput) checkResult {
			return input.result(input.state.contractVerificationGate(input.ctx, input.contractDemand, input.contractOwed))
		}},
		// The completion rows hold an exit that leaves the work unfinished, in this
		// order: code nothing calls, plan steps never carried out, no change where
		// the request asked for one, a named deliverable not on disk, and a claim of
		// completeness the files do not bear out.
		{"exit.orphanGate", func(input *exitCheckInput) checkResult {
			return input.result(input.state.orphanGate(input.ctx))
		}},
		{"exit.planGate", func(input *exitCheckInput) checkResult {
			return input.result(input.state.planGate(input.ctx))
		}},
		{"exit.actionGate", func(input *exitCheckInput) checkResult {
			return input.result(input.state.actionGate(input.ctx, input.userMessage))
		}},
		{"exit.outputGate", func(input *exitCheckInput) checkResult {
			return input.result(input.state.outputGate(input.ctx))
		}},
		{"exit.claimCheckGate", func(input *exitCheckInput) checkResult {
			return input.result(input.state.claimCheckGate(input.ctx, input.userMessage, input.claimText))
		}},
	}
}

func init() {
	proxyChecks.completion = []orderedCheck[completionCheckInput]{
		{"completion.repairs", (*completionCheckInput).repairs},
		{"completion.deliverables", (*completionCheckInput).deliverables},
		{"completion.action", (*completionCheckInput).action},
		{"completion.verification", (*completionCheckInput).verification},
		{"completion.contract", (*completionCheckInput).contract},
		{"completion.deliverySettlement", (*completionCheckInput).deliverySettlement},
		{"completion.declaredCompletion", (*completionCheckInput).declaredCompletion},
		{"completion.reply", (*completionCheckInput).reply},
		{"completion.background", (*completionCheckInput).background},
		{"completion.mutationDebt", (*completionCheckInput).mutationDebt},
		{"completion.unresolvedGates", (*completionCheckInput).unresolvedGates},
	}
}

// ---------------------------------------------------------------------------
// Agent loop — iterative tool-calling loop between model and executors
// ---------------------------------------------------------------------------

// runAgentLoop runs the agent loop for a single user request.
// The model emits tool calls (constrained by grammar), the proxy executes them,
// and returns results. Continues until the model emits "done" or max turns hit.
func runAgentLoop(ctx *AgentContext, userMessage string) error {
	loopStart := beginAgentRun(ctx, userMessage)
	defer endAgentStage(ctx, loopStart)
	if !openConversation(ctx, userMessage) {
		return nil
	}

	// edit_file old_str-mismatch failures per path. A successful read_file
	// between attempts resets consecutiveErrors/RecentFailurePaths, which
	// masks the classic read→edit-miss→read loop (smaller models can't
	// reproduce old_str byte-for-byte). This counter survives interleaved
	// reads so we can force the structural_edit steer after the second miss.
	editMissByPath := map[string]int{}
	// THE output-obligation decision, made once, from the contract the
	// request boundary already validated. Nothing the model emits later can
	// change which source spoke.
	outputObligation := resolveOutputObligation(ctx, userMessage)
	loop := newAgentLoop(ctx, userMessage, outputObligation)
	st := loop.st

	for turn := 0; ctx.MaxTurns <= 0 || turn < ctx.MaxTurns; turn++ {
		response, parsed, outcome, err := loop.takeModelTurn(turn)
		switch outcome {
		case turnEnds:
			continue
		case runEnds:
			return err
		}

		switch parsed.Type {
		case "done":
			if loop.endOnDone(turn, parsed) {
				return nil
			}

		case "text":
			if loop.endOnText(turn, parsed) {
				return nil
			}

		case "tool_call":
			st.toolsRun++
			st.pendingToolCall = parsed.Name
			intentArgs := announceToolCall(ctx, turn, parsed)
			switch loop.screenBeforeWorkspaceCheck(turn, response, parsed, intentArgs) {
			case turnEnds:
				continue
			case runEnds:
				return nil
			}

			// Enforce the workspace boundary before any pre-execution gate reads
			// a path. executeToolCall repeats this check for parallel dispatch.
			workspaceCheck := toolCheckInput{loop: loop, turn: turn, parsed: &parsed, intentArgs: intentArgs}
			switch runCheckPhase(proxyChecks.workspace, &workspaceCheck).outcome {
			case turnEnds:
				continue
			case runEnds:
				return nil
			}

			switch loop.screenAfterWorkspaceCheck(turn, response, &parsed, intentArgs) {
			case turnEnds:
				continue
			case runEnds:
				return nil
			}
			switch loop.screenExistingWrite(parsed, intentArgs) {
			case turnEnds:
				continue
			case runEnds:
				return nil
			}

			result, steers, callOutcome, callErr := loop.runToolCall(turn, parsed, intentArgs)
			switch callOutcome {
			case turnEnds:
				continue
			case runEnds:
				return callErr
			}
			if result.Success && len(ctx.LiteralBlocks) > 0 &&
				(parsed.Name == "write_file" || parsed.Name == "edit_file" ||
					parsed.Name == "structural_edit" ||
					parsed.Name == "insert_after" || parsed.Name == "replace_lines") {
				// Literal-contract enforcement: the user's own bytes are the
				// authoritative rendering of any content they spelled out, and
				// the model measurably cannot transcribe bytes (space-prefixed
				// BPE tokens win after quotes — `BANNER = "ready"` arrives as
				// `BANNER = " ready"` deterministically). Spacing drift within
				// lines of a stated literal is repaired in place; indentation
				// is never changed (repairLiteralDrift).
				var wp struct {
					Path string `json:"path"`
				}
				if json.Unmarshal(parsed.Args, &wp) == nil && wp.Path != "" {
					lp := resolveAgentPath(ctx, wp.Path)
					if body, rerr := os.ReadFile(lp); rerr == nil {
						if fixed, repairedLits, changed := repairLiteralDrift(string(body), ctx.LiteralBlocks); changed {
							if werr := os.WriteFile(lp, []byte(fixed), 0o644); werr == nil {
								reportLiteralRepair(ctx, result, wp.Path, lp, fixed, repairedLits)
							}
						}
					}
				}
			}
			loop.recordCallEffects(turn, parsed, result)

			// edit_file old_str miss: count per path independently of the
			// consecutiveErrors reset an interleaved read causes. On the
			// second miss for the same structured file, force the structural_edit
			// steer as a [system note] (the inline tool-error hint alone
			// doesn't reliably move a small model off edit_file).
			if !result.Success && parsed.Name == "edit_file" &&
				strings.Contains(result.Error, "string to replace not found") {
				mp := extractFailurePath(parsed.Name, parsed.Args)
				editMissByPath[mp]++
				ext := strings.ToLower(filepath.Ext(mp))
				// Force the structural_edit steer on the FIRST miss for structured
				// files — small models bail to run_command after a single
				// edit_file miss rather than retrying, so waiting for a
				// second miss never fires.
				if editMissByPath[mp] >= 1 && (ext == ".py" || ext == ".html" || ext == ".htm") {
					var source string
					if b, rerr := os.ReadFile(resolveAgentPath(ctx, mp)); rerr == nil {
						source = string(b)
					}
					steers.repeat = editMissSteer(mp, source)
					log.Printf("[agent] edit_file miss on %q — forcing structural_edit steer", mp)
				}
			}

			if loop.breakErrorLoop(turn, parsed, result) {
				return nil
			}
			loop.answerToolCall(turn, response, parsed, result, steers)

		default:
			askForAKnownResponseType(ctx, parsed)
		}
	}

	loop.endOutOfTurns()
	return nil
}

// fetchFencedContent asks the model for a file's contents in its native
// channel: a single fenced block (attempt 0 constrained by the fence grammar, a
// retry as free text).
//
// Code emitted INSIDE a JSON string pays escaping pressure on every dense line,
// and slips such as a missing close-paren, a literal \n fusing a statement into
// a comment, or list joins losing their spaces are this one channel problem.
// The envelope stays under the JSON constraint; only the file body moves to
// plain text.
//
// The sub-call is ephemeral: nothing is appended to ctx.Messages, so from the
// main conversation's view the model wrote "@fenced" and the write simply
// happened.
func fetchFencedContent(ctx *AgentContext, rawCall, path string) (string, error) {
	if err := fencedFetchRefusal(ctx, path); err != nil {
		return "", &fencedCheckError{err, checkGuardRefusal}
	}
	tag := fenceTagForPath(path)
	note := wire.AgentMessage{Role: "user", Content: fmt.Sprintf(
		"[system note]: Now provide ONLY the complete contents of %s, as plain "+
			"code in a single fenced block, fenced with FOUR backticks (````%s ... ````) "+
			"so a ``` line inside the file stays inside it. No JSON, no "+
			"commentary, no partial file.", path, tag)}
	msgs := append(append([]wire.AgentMessage{}, ctx.Messages...),
		wire.AgentMessage{Role: "assistant", Content: rawCall}, note)
	fetch := &fencedFetch{ctx: ctx, path: path, tag: tag, msgs: msgs}
	// Attempts remaining are what the SESSION still allows for this path, not
	// a fresh two. Requirement: a new write_file call must not restore the
	// allowance a previous turn spent.
	remaining := maxFencedFailuresPerPath - ctx.FencedFailures[fencedKey(ctx, path)]
	for attempt := 0; attempt < remaining; attempt++ {
		if attempt > 0 && !fencedFitsRemainingBudget(ctx) {
			break // a retry that cannot finish and still be validated
		}
		// A stall on attempt 0 already raised FencedStalls to the disable
		// limit; a retry would only pay another watchdog cut for a channel
		// the session has given up on. Stop here and let the caller steer the
		// model inline, reclaiming that ~25s on the file that first stalls.
		if attempt > 0 && fencedChannelDisabledForSession(ctx) {
			break
		}
		attemptStart := time.Now()
		// Attempt 0 constrains decoding to the contract the note describes:
		// one fenced block with the requested tag. That returns clean usable
		// content far more often than free text, with fewer empty replies, in
		// less time. A later attempt drops the grammar, so a server that
		// refuses it still gets a free-text request.
		grammar := fencing.BlockGrammar(tag)
		current, _ := os.ReadFile(resolveAgentPath(ctx, path)) // absent: a new file
		if attempt > 0 || !fencing.GrammarFits(string(current)) {
			grammar = rawEmissionSentinel
		}
		answer := fetch.ask(grammar, attempt, attemptStart)
		if content, done, err := fetch.settle(answer, attempt); done {
			return content, err
		}
	}
	if fetch.lastErr != nil {
		return "", &fencedCheckError{fmt.Errorf("fenced resolution for %s was cut after %d attempt(s) "+
			"this session (%v); send the file inline or edit it instead",
			path, ctx.FencedFailures[fencedKey(ctx, path)], fetch.lastErr), checkInfrastructureError}
	}
	return "", &fencedCheckError{fmt.Errorf("no fenced block after %d attempt(s) for %s this session; "+
		"send the file inline or edit it instead",
		ctx.FencedFailures[fencedKey(ctx, path)], path), checkModelError}
}

// Toolchain describes one language ecosystem detected in the project.
// The fields are surfaced into the system prompt so the model knows
// which runner to invoke and how to install deps if needed.
//
// Detection is manifest-driven: presence of pyproject.toml means
// Python, package.json means Node, Cargo.toml means Rust, etc. A
// polyglot project (React frontend + Django backend + deploy scripts)
// returns multiple Toolchains so the model can pick the right one
// per file edit.
type Toolchain struct {
	Name           string   // canonical key: "python", "node", "rust", "go", "ruby", "java-maven", "java-gradle", "php", "dotnet", "dart"
	Manifests      []string // manifest files found relative to workingDir (e.g. ["pyproject.toml", "requirements.txt"])
	Runner         string   // command to run the project's main entry (e.g. "/workspace/venv/bin/python", "node", "cargo run", "go run .")
	PackageManager string   // detected pkg manager when ambiguous (npm vs pnpm vs yarn vs bun for Node)
	InstallCommand string   // command to install deps from lockfile (e.g. "npm ci", "pip install -r requirements.txt")
	TestCommand    string   // best-guess test runner ("pytest", "npm test", "cargo test", ...)
}

// detectProjectToolchains scans workingDir for language manifests and
// returns one Toolchain per detected ecosystem. Polyglot projects
// (e.g. React + Django) produce multiple entries. Empty slice means
// no recognized manifest was found at the root.
//
// We deliberately only look ONE level deep at the root — most
// monorepos have manifests in subdirs (apps/web/package.json,
// services/api/pyproject.toml) but probing deeper here would be
// expensive and noisy. The model can still discover deep manifests
// via list_directory / read_file when it needs to.
func detectProjectToolchains(workingDir string) []Toolchain {
	if workingDir == "" {
		return nil
	}
	var out []Toolchain

	// Python — venv-aware so the runner points at the project's
	// pinned interpreter when one exists.
	pyManifests := pickExisting(workingDir, "pyproject.toml", "requirements.txt", "setup.py", "Pipfile", "poetry.lock")
	if len(pyManifests) > 0 || detectProjectVenvPython(workingDir) != "" {
		out = append(out, pythonToolchain(workingDir, pyManifests))
	}

	// Node / TypeScript — pkg manager picked from lockfile.
	if hasFile(workingDir, "package.json") {
		pm, install := nodePackageManager(workingDir)
		runner := "node"
		if hasFile(workingDir, "tsconfig.json") {
			runner = "tsx" // ts/jsx-aware launcher; falls back to node for plain .js
		}
		out = append(out, Toolchain{
			Name: "node", Manifests: pickExisting(workingDir, "package.json", "tsconfig.json"),
			Runner: runner, PackageManager: pm, InstallCommand: install,
			TestCommand: pm + " test",
		})
	}

	// Rust
	if hasFile(workingDir, "Cargo.toml") {
		out = append(out, Toolchain{
			Name: "rust", Manifests: pickExisting(workingDir, "Cargo.toml", "Cargo.lock"),
			Runner: "cargo run", InstallCommand: "cargo fetch",
			TestCommand: "cargo test",
		})
	}

	// Go
	if hasFile(workingDir, "go.mod") {
		out = append(out, Toolchain{
			Name: "go", Manifests: pickExisting(workingDir, "go.mod", "go.sum"),
			Runner: "go run .", InstallCommand: "go mod download",
			TestCommand: "go test ./...",
		})
	}

	// Ruby
	if hasFile(workingDir, "Gemfile") {
		out = append(out, Toolchain{
			Name: "ruby", Manifests: pickExisting(workingDir, "Gemfile", "Gemfile.lock"),
			Runner: "bundle exec ruby", InstallCommand: "bundle install",
			TestCommand: "bundle exec rspec",
		})
	}

	// Java — Maven
	if hasFile(workingDir, "pom.xml") {
		out = append(out, Toolchain{
			Name: "java-maven", Manifests: []string{"pom.xml"},
			Runner: "mvn exec:java", InstallCommand: "mvn install -DskipTests",
			TestCommand: "mvn test",
		})
	}

	// Java/Kotlin — Gradle (prefer wrapper if present)
	if hasFile(workingDir, "build.gradle") || hasFile(workingDir, "build.gradle.kts") {
		runner, install, test := gradleCommands(workingDir)
		out = append(out, Toolchain{
			Name: "java-gradle", Manifests: pickExisting(workingDir, "build.gradle", "build.gradle.kts", "settings.gradle", "gradlew"),
			Runner: runner, InstallCommand: install, TestCommand: test,
		})
	}

	// PHP / Composer
	if hasFile(workingDir, "composer.json") {
		out = append(out, Toolchain{
			Name: "php", Manifests: pickExisting(workingDir, "composer.json", "composer.lock"),
			Runner: "php", InstallCommand: "composer install",
			TestCommand: "vendor/bin/phpunit",
		})
	}

	// .NET — pick the first project file we find
	if csproj := firstMatchingGlob(workingDir, "*.csproj", "*.fsproj", "*.sln"); csproj != "" {
		out = append(out, Toolchain{
			Name: "dotnet", Manifests: []string{csproj},
			Runner: "dotnet run", InstallCommand: "dotnet restore",
			TestCommand: "dotnet test",
		})
	}

	// Dart / Flutter
	if hasFile(workingDir, "pubspec.yaml") {
		out = append(out, dartToolchain(workingDir))
	}

	return out
}

// pythonToolchain is the Python entry: the project's own venv interpreter when
// it has one, and the install command its lockfile or manifest calls for.
func pythonToolchain(workingDir string, pyManifests []string) Toolchain {
	runner := detectProjectVenvPython(workingDir)
	if runner == "" {
		runner = "python"
	}
	install := "pip install -r requirements.txt"
	if hasFile(workingDir, "poetry.lock") {
		install = "poetry install"
	} else if hasFile(workingDir, "Pipfile.lock") {
		install = "pipenv install"
	} else if hasFile(workingDir, "pyproject.toml") && !hasFile(workingDir, "requirements.txt") {
		install = "pip install -e ."
	}
	return Toolchain{
		Name: "python", Manifests: pyManifests,
		Runner: runner, InstallCommand: install,
		TestCommand: "pytest",
	}
}

// nodePackageManager picks Node's package manager, and the command that
// installs with it, from the lockfile the project has.
func nodePackageManager(workingDir string) (string, string) {
	pm, install := "npm", "npm install"
	switch {
	case hasFile(workingDir, "pnpm-lock.yaml"):
		pm, install = "pnpm", "pnpm install --frozen-lockfile"
	case hasFile(workingDir, "yarn.lock"):
		pm, install = "yarn", "yarn install --frozen-lockfile"
	case hasFile(workingDir, "bun.lockb"):
		pm, install = "bun", "bun install --frozen-lockfile"
	case hasFile(workingDir, "package-lock.json"):
		pm, install = "npm", "npm ci"
	}
	return pm, install
}

// gradleCommands picks Gradle's run, install and test commands, through the
// project's own wrapper when it has one.
func gradleCommands(workingDir string) (string, string, string) {
	runner := "gradle run"
	install := "gradle build -x test"
	test := "gradle test"
	if hasFile(workingDir, "gradlew") {
		runner = "./gradlew run"
		install = "./gradlew build -x test"
		test = "./gradlew test"
	}
	return runner, install, test
}

// dartToolchain is the Dart entry, run through Flutter when the project is a
// Flutter app.
func dartToolchain(workingDir string) Toolchain {
	runner, install := "dart run", "dart pub get"
	if hasFile(workingDir, ".flutter-plugins") || hasFile(workingDir, "flutter.yaml") {
		runner, install = "flutter run", "flutter pub get"
	}
	return Toolchain{
		Name: "dart", Manifests: pickExisting(workingDir, "pubspec.yaml", "pubspec.lock"),
		Runner: runner, InstallCommand: install,
		TestCommand: "dart test",
	}
}

// probeToolchainReady returns a short status string for a Toolchain
// that's safe to run from buildSystemPrompt — meaning: it MUST be
// purely filesystem-based (no shelling out, no network). The model
// uses this to decide whether to install deps or skip straight to
// verification.
//
// We can't actually invoke `python -c "import flask"` here without
// running a subprocess in the sandbox, which is too expensive for
// every system-prompt build. Instead we look for filesystem evidence
// that deps are installed: venv with site-packages populated,
// node_modules present, target/debug/ for Rust, vendor/ for Ruby/Go,
// etc. False positives are fine ("looks installed but isn't" — the
// model will discover that on first verify and install). False
// negatives are bad — they push the model toward unnecessary
// reinstalls. Bias toward "ready" when the evidence is ambiguous.
func probeToolchainReady(workingDir string, tc Toolchain) string {
	switch tc.Name {
	case "python":
		for _, vd := range []string{"venv", ".venv", "env", ".env-py"} {
			sp := filepath.Join(workingDir, vd, "lib")
			if entries, err := os.ReadDir(sp); err == nil {
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), "python") && e.IsDir() {
						if hasUserPackages(filepath.Join(sp, e.Name(), "site-packages")) {
							return "ready"
						}
					}
				}
			}
		}
		if hasFile(workingDir, "requirements.txt") || hasFile(workingDir, "pyproject.toml") {
			return "needs install"
		}
		return "no manifest"

	case "node":
		if entries, err := os.ReadDir(filepath.Join(workingDir, "node_modules")); err == nil && len(entries) > 0 {
			return "ready"
		}
		return "needs install"

	case "rust":
		if info, err := os.Stat(filepath.Join(workingDir, "target")); err == nil && info.IsDir() {
			return "warm"
		}
		return "cold"

	case "go":
		if info, err := os.Stat(filepath.Join(workingDir, "vendor")); err == nil && info.IsDir() {
			return "vendored"
		}
		if hasFile(workingDir, "go.sum") {
			return "ready"
		}
		return "needs `go mod tidy`"

	case "ruby":
		if info, err := os.Stat(filepath.Join(workingDir, "vendor", "bundle")); err == nil && info.IsDir() {
			return "ready"
		}
		return "needs install"

	case "java-maven", "java-gradle":
		dir := "target"
		if tc.Name == "java-gradle" {
			dir = "build"
		}
		if info, err := os.Stat(filepath.Join(workingDir, dir)); err == nil && info.IsDir() {
			return "warm"
		}
		return "cold"

	case "php":
		if info, err := os.Stat(filepath.Join(workingDir, "vendor")); err == nil && info.IsDir() {
			return "ready"
		}
		return "needs install"

	case "dotnet":
		if info, err := os.Stat(filepath.Join(workingDir, "bin")); err == nil && info.IsDir() {
			return "warm"
		}
		return "cold"

	case "dart":
		if info, err := os.Stat(filepath.Join(workingDir, ".dart_tool")); err == nil && info.IsDir() {
			return "ready"
		}
		return "needs install"
	}
	return ""
}

// displayRelativeRunner converts an absolute runner path to its
// project-relative form when it lives under workingDir. Compresses
// `/workspace/venv/bin/python` to `venv/bin/python` in prompt output —
// matches the existing "use relative paths" rule and stops the model
// confusing itself into emitting `workspace/app.py`.
func displayRelativeRunner(runner, workingDir string) string {
	if !filepath.IsAbs(runner) {
		return runner
	}
	if rel, err := filepath.Rel(workingDir, runner); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return runner
}

// hasUserPackages returns true when site-packages contains anything
// beyond pip/setuptools/wheel — i.e. the user has installed real
// project deps. Empty / pip-only venvs return false.
func hasUserPackages(sitePackages string) bool {
	entries, err := os.ReadDir(sitePackages)
	if err != nil {
		return false
	}
	skip := map[string]bool{
		"pip": true, "setuptools": true, "wheel": true,
		"pkg_resources": true, "_distutils_hack": true,
		"__pycache__": true,
	}
	for _, e := range entries {
		name := e.Name()
		// Strip dist-info / egg-info suffixes for the skip check.
		if i := strings.Index(name, "-"); i > 0 {
			name = name[:i]
		}
		if strings.HasSuffix(e.Name(), ".dist-info") || strings.HasSuffix(e.Name(), ".egg-info") {
			continue
		}
		if !skip[name] && !strings.HasPrefix(name, "_") {
			return true
		}
	}
	return false
}

// hasFile returns true when workingDir/name exists as a file.
func hasFile(workingDir, name string) bool {
	info, err := os.Stat(filepath.Join(workingDir, name))
	return err == nil && !info.IsDir()
}

// pickExisting returns the subset of names that exist as files in workingDir.
func pickExisting(workingDir string, names ...string) []string {
	var out []string
	for _, n := range names {
		if hasFile(workingDir, n) {
			out = append(out, n)
		}
	}
	return out
}

// firstMatchingGlob returns the first filename matching any of the
// glob patterns at the workingDir root, or "" if none match.
func firstMatchingGlob(workingDir string, patterns ...string) string {
	for _, p := range patterns {
		matches, _ := filepath.Glob(filepath.Join(workingDir, p))
		if len(matches) > 0 {
			return filepath.Base(matches[0])
		}
	}
	return ""
}

// detectProjectVenvPython returns the container-side path to the
// project's venv python (e.g. "/workspace/venv/bin/python") if the
// working directory has a recognisable Python virtual environment.
// Returns "" when no venv is found.
//
// The agent's working_dir is the container-internal /workspace, so
// we resolve against that. Common venv directory names: venv, .venv,
// env, .env-py — we probe in priority order and stop at the first hit.
// Inside each, look for bin/python, bin/python3, or Scripts/python.exe
// (Windows-emitted venvs occasionally end up bind-mounted on Linux).
//
// Caller passes workingDir from ctx.WorkingDir; the returned path is
// what the model should literally invoke via run_command — e.g.
// "/workspace/venv/bin/python app.py" — and what gets surfaced in the
// system prompt's venv hint.
func detectProjectVenvPython(workingDir string) string {
	if workingDir == "" {
		return ""
	}
	venvDirs := []string{"venv", ".venv", "env", ".env-py"}
	pythonRels := []string{"bin/python", "bin/python3", "Scripts/python.exe"}
	for _, vd := range venvDirs {
		for _, py := range pythonRels {
			abs := filepath.Join(workingDir, vd, py)
			if info, err := os.Stat(abs); err == nil && !info.IsDir() {
				// Return container-relative path (workingDir is already
				// the container-side /workspace), so caller can paste
				// it into a run_command argument unchanged.
				return abs
			}
		}
	}
	return ""
}

func listWorkspaceFiles(workingDir string, limit int) []string {
	if workingDir == "" {
		return nil
	}
	var out []string
	skip := map[string]bool{".git": true, "node_modules": true,
		"__pycache__": true, ".venv": true, "venv": true, "dist": true}
	_ = filepath.WalkDir(workingDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skip[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if len(out) >= limit {
			return fs.SkipAll
		}
		if rel, err := filepath.Rel(workingDir, path); err == nil {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

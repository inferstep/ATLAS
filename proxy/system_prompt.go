// The system prompt, and the budget that decides what else fits in a request.

package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// System prompt construction
// ---------------------------------------------------------------------------

func buildSystemPrompt(ctx *AgentContext) string {
	var sb strings.Builder

	sb.WriteString("You are ATLAS, a coding assistant that creates and modifies code by calling tools. ")
	sb.WriteString("You have access to the filesystem and can run commands to verify your work.\n")
	sb.WriteString("You MUST respond with ONLY a single valid JSON object, no other text.\n\n")

	// Pick-the-right-shape guidance — this is what keeps "hi" out of the
	// tool-call rabbit hole. Without it the model treats every input as a
	// task and starts read_file'ing random paths.
	sb.WriteString("## Choosing your response shape\n\n")
	sb.WriteString("- **Conversational input** (greetings, small talk, questions about YOU, status checks): emit `{\"type\":\"text\",\"content\":\"...\"}` — the turn ends after one text reply, and the user can follow up. Do NOT call tools to answer \"hi\" or \"what can you do\".\n")
	sb.WriteString("- **Questions about the CODE** (\"what does X do\", \"why is this slow\", \"is this a bug\") are NOT that case: read the file first, then answer in one `text` reply. You have read_file and outline_file — use them. Never ask the user to paste a file that is already in the workspace, and never answer from a guess about code you have not opened. Reading to answer a question is not \"starting work\": make no edits unless the user asked for one.\n")
	sb.WriteString("- **Coding tasks** (\"fix the bug\", \"add a feature\", \"refactor X\"): emit `{\"type\":\"tool_call\",...}` to make progress, repeat as needed, then emit `{\"type\":\"done\",\"summary\":\"...\"}` when finished.\n")
	sb.WriteString("- **Don't use `text` mid-task.** Roll narration into the done.summary at the end, or skip it entirely. Mid-task `text` ends the turn early.\n")
	sb.WriteString("- **When unsure** whether the user wants chat or work: ask in a single `text` reply. Don't speculatively start tool-calling — but reading a file the user named is never speculative.\n\n")

	sb.WriteString(buildToolDescriptionsExcluding(nil))

	sb.WriteString("## Rules\n\n")
	sb.WriteString("- **Writing a whole file**: in write_file, set content to EXACTLY the 7 characters `@fenced` and NOTHING else — do NOT put the file itself in the JSON. After the tool call you will be asked for the file; reply then with ONE fenced code block containing the complete file. Code inside a JSON string gets corrupted by escaping (lost parens, broken newlines); the fenced reply is the reliable channel. Only trivially short content (under 5 lines) may be inlined in the JSON.\n")
	sb.WriteString("- To work on an EXISTING file, `read_file` it. Reading is the default and the context window is large; `read_file` caps itself on a file too big to load and tells you how to narrow the range, so you do not need to ration reads. Reach for `outline_file` only to locate a target inside a file that large — it returns line ranges and no code, so it can tell you where to read but never what the code does. Never re-read the same file in a loop; if a read's content is already in the conversation, act on it.\n")
	sb.WriteString("- Always read the relevant code before editing it (outline_file → read_file, then edit_file/structural_edit).\n")
	sb.WriteString("- MANDATORY: Use `edit_file` (targeted old_str/new_str) for any change to a file that already exists, no matter how small. `write_file` is ONLY for creating brand-new files. The agent layer rejects every `write_file` call against an existing file >5 lines — your call won't execute and you'll get a tool error directing you to edit_file. Don't re-emit a whole file to change a few lines.\n")
	sb.WriteString("  Example — to add a None check to one branch, use:\n")
	sb.WriteString("    edit_file {\"path\":\"src/foo.py\",\"old_str\":\"if x == 0:\\n        return None\",\"new_str\":\"if x is None or x == 0:\\n        return None\"}\n")
	sb.WriteString("  NOT write_file with the entire file's new contents.\n")
	sb.WriteString("- For WHOLE-FUNCTION or WHOLE-ELEMENT rewrites, prefer `structural_edit` over `edit_file`. structural_edit takes a structural selector (`function:NAME`, `class:NAME`, `<tag>` for HTML) and replaces that one whole named block — no need to copy the existing function as old_str. Selector must match exactly one node; ambiguous selectors return an error so you can be more specific. Decorators are included automatically when selecting a Python function. Works on `.py`, `.html`/`.htm`, `.go`, `.ts`/`.tsx` and `.js`/`.jsx`; for Go `function:NAME` matches a func or a method and `type:NAME` a type, and for JS/TS `function:NAME` matches a declaration, an arrow or a class method.\n")
	sb.WriteString("    structural_edit {\"path\":\"app.py\",\"selector\":\"function:dashboard\",\"content\":\"@app.route('/dashboard')\\ndef dashboard():\\n    return render_template('dashboard.html')\"}\n")
	sb.WriteString("    structural_edit {\"path\":\"templates/index.html\",\"selector\":\"<body>\",\"content\":\"<body>\\n  <h1>Welcome</h1>\\n  ...\\n</body>\"}\n")
	sb.WriteString("- WHEN write_file IS REJECTED for an existing file: if the file is `.py`, `.html`, or `.htm` and you're replacing the whole thing (e.g. swapping the entire body, replacing the dashboard function), use `structural_edit` next, not edit_file. structural_edit doesn't need `old_str` so it doesn't hit the max_tokens truncation that kills long edit_file calls. Use edit_file ONLY for surgical inline string changes (one line, one expression). For a change that spans several lines but is not a whole node, use `replace_lines` with the line numbers read_file printed — you assert only the FIRST and LAST line of the range, so there is no multi-line old_str to reproduce. This rule applies even when conversation trimming has dropped the original rejection message — re-derive the intent from the file extension and the size of your replacement.\n")
	sb.WriteString("- JSON strings in tool args contain LITERAL characters: write `<` not `&lt;`, `>` not `&gt;`, `&` not `&amp;`. The file content goes verbatim onto disk — `&lt;!DOCTYPE&gt;` would write the literal text `&lt;!DOCTYPE&gt;` instead of `<!DOCTYPE>`. NEVER HTML-encode angle brackets inside `content`, `old_str`, or `new_str`.\n")
	sb.WriteString("- The `content` you put in write_file / edit_file goes verbatim onto disk. **No markdown fences. No prose preamble (\"Looking at the task...\", \"Here's the file:\"). No trailing explanation.** Just the raw file contents. The agent layer strips fenced wrappers before writing, but the right move is to never emit them in the first place.\n")
	sb.WriteString("- For CONTENT changes, prefer the dedicated tools — `edit_file` (one line), `replace_lines` (a line range), `insert_after` (adding at a line), `structural_edit` (a whole node), `write_file` (new files) — they go through the validation pipeline. The last three need no old_str at all, which is why they hold up on changes edit_file loses. For moving / renaming / reorganizing files you may use either `move_file` or shell `mv`/`cp` via run_command; both work. `run_command` runs a real shell (in an isolated sandbox confined to this project), so ordinary file operations (mv, cp, mkdir, rm of a file you created, chmod) are fine. To delete a file that was already here, use `delete_file`, which asks the user: a shell `rm` of one leaves the task unfinished. Only catastrophic commands are blocked: wiping the whole project (`rm -rf /`, `rm -rf .`, `rm -rf *`), fork bombs, and device/filesystem destruction.\n")
	sb.WriteString("- Verify your changes by running them: run the program, its tests, or fetch the page with curl. A build, lint or syntax check (for example `python -m py_compile`) shows the code is well formed, not that it works, and does not count. For \"fix\"/\"isn't working\" prompts, verify before `done`.\n")
	sb.WriteString("- For LONG-RUNNING commands (servers): `run_background(cmd)` → `run_command(\"curl -sf http://localhost:<port>/\")` → `stop_background(job_id)`. Don't use `timeout 5 ... || true` — server dies before probe hits.\n")
	sb.WriteString("- When creating a project from scratch: create config/build files FIRST, verify they work (e.g., npm install, cargo check), THEN create feature code\n")
	sb.WriteString("- Respond with {\"type\":\"done\",\"summary\":\"...\"} when the task is complete\n")
	sb.WriteString("- If a command fails, read the error output, fix the issue, and try again\n")
	sb.WriteString("- Do not guess at file contents — read first, then edit\n")
	sb.WriteString("- ALWAYS use relative file paths (`app.py`, `src/main.rs`), NEVER absolute paths and NEVER prefix with `workspace/` — that's the parent dir, not your project root.\n")
	sb.WriteString("- When adding features to an existing project, read at most 2-3 files to understand the structure, then immediately write your changes. Do not explore the entire directory tree. Prioritize writing code over reading code.\n\n")

	writeProjectContext(&sb, ctx.Project)
	sb.WriteString(fmt.Sprintf("Working directory: %s\n\n", ctx.WorkingDir))
	writeToolchains(&sb, ctx.WorkingDir)
	if ctx.VerifyOnHost {
		sb.WriteString("`run_command` targets the host (not sandbox). Sees host env/services/paths.\n\n")
	}
	writeProjectFiles(&sb, ctx.SnapshotFilesRead())
	writePlan(&sb, ctx.Plan)
	return sb.String()
}

// writeProjectContext names what the project runs, when it was detected.
func writeProjectContext(sb *strings.Builder, project *ProjectInfo) {
	if project == nil {
		return
	}
	sb.WriteString("## Project Context\n\n")
	sb.WriteString(fmt.Sprintf("Language: %s\n", project.Language))
	if project.Framework != "" {
		sb.WriteString(fmt.Sprintf("Framework: %s\n", project.Framework))
	}
	if project.BuildCommand != "" {
		if classifyCommandEvidence(project.BuildCommand).Kind.verifies() {
			sb.WriteString(fmt.Sprintf("Build command: %s\n", project.BuildCommand))
		} else {
			sb.WriteString(fmt.Sprintf("Build/syntax check: %s (does not run the program)\n", project.BuildCommand))
		}
	}
	if project.DevCommand != "" {
		sb.WriteString(fmt.Sprintf("Dev command: %s\n", project.DevCommand))
	}
	if len(project.ConfigFiles) > 0 {
		sb.WriteString(fmt.Sprintf("Config files: %s\n", strings.Join(project.ConfigFiles, ", ")))
	}
	sb.WriteString("\n")
}

// writeToolchains detects every recognized language manifest in the project
// and surfaces the runners + install commands so the model picks the right
// tool per file edit. Polyglot projects (React + Django + deploy scripts) get
// one entry per ecosystem. Covers every toolchain, not just Python's venv.
// Probe-first hints — whether the deps are already importable — are added
// per-toolchain when the evidence is on disk.
func writeToolchains(sb *strings.Builder, workingDir string) {
	tcs := detectProjectToolchains(workingDir)
	if len(tcs) == 0 {
		return
	}
	sb.WriteString("## Toolchains\n")
	for _, tc := range tcs {
		line := fmt.Sprintf("- **%s** — runner `%s`", tc.Name, displayRelativeRunner(tc.Runner, workingDir))
		if tc.InstallCommand != "" {
			line += fmt.Sprintf(", install `%s`", tc.InstallCommand)
		}
		if tc.TestCommand != "" {
			line += fmt.Sprintf(", tests `%s`", tc.TestCommand)
		}
		if probe := probeToolchainReady(workingDir, tc); probe != "" {
			line += " [" + probe + "]"
		}
		sb.WriteString(line + "\n")
	}
	sb.WriteString("Skip install when status is `ready`; install only what's missing.\n\n")
}

// writeProjectFiles shows which files are in the project (names only, not full
// content). Full content is available via read_file if needed. This avoids
// consuming context window with pre-injected file dumps.
func writeProjectFiles(sb *strings.Builder, filesRead map[string]string) {
	if len(filesRead) == 0 {
		return
	}
	sb.WriteString("## Project Files Available\n")
	for _, path := range sortedKeys(filesRead) {
		sb.WriteString(fmt.Sprintf("- %s\n", path))
	}
	sb.WriteString("\nUse read_file to inspect these files if needed. To MODIFY any of them, use edit_file — write_file against an existing file (>5 lines) is rejected at the agent layer.\n\n")
}

// writePlan surfaces the planner's plan, when it returned one, so the model has
// explicit step guidance instead of having to infer the right shape from the
// user message alone. Plans are advisory (the agent layer doesn't hard-block
// off-plan calls), but having them in the system prompt visibly improves
// first-call accuracy.
func writePlan(sb *strings.Builder, plan *Plan) {
	if plan == nil || len(plan.Steps) == 0 {
		return
	}
	sb.WriteString("## Plan\n\n")
	sb.WriteString("A planner has proposed these steps for the user's request. ")
	sb.WriteString("Follow them in order when sensible. ")
	sb.WriteString("Deviate only if a step's premise is wrong (file doesn't exist, command unavailable, etc.) — the agent layer notices repeated off-plan calls and will silently revise the plan with what you've discovered.\n\n")
	for i, step := range plan.Steps {
		marker := " "
		if step.ID == plan.VerifyStep {
			marker = "✓" // verify step
		}
		sb.WriteString(fmt.Sprintf("%d. [%s] **%s** %s — %s\n", i+1, marker, step.Action, step.Target, step.Why))
	}
	if plan.Rationale != "" {
		sb.WriteString(fmt.Sprintf("\n_%s_\n", plan.Rationale))
	}
	if plan.VerifyStep != "" {
		sb.WriteString(fmt.Sprintf("\nThe verify step (%s) is your evidence the fix worked — don't emit `done` until it has run successfully.\n", plan.VerifyStep))
	}
	sb.WriteString("\n")
}

// estTokens is a cheap, model-agnostic token estimate: ~4 chars/token plus
// a small per-message framing overhead. Good enough for budgeting; we leave
// generous headroom so the estimate never has to be exact.
func estTokens(content string) int {
	return len(content)/4 + 8
}

// conversationTokenBudget is how many prompt tokens the agent loop will let
// the conversation grow to before trimming. Derived from the deployment's
// per-slot context (ATLAS_CTX_SIZE / ATLAS_PARALLEL_SLOTS), reserving ~35%
// for the response. Model-agnostic: keys off the context the deploy gives,
// not the model identity. Falls back to a safe default when env is absent.
// perSlotContext is the token window one llama.cpp slot actually has:
// the server's context divided by the parallel slots it was started with.
// Shared so the history budget and the restatement agree on the limit they
// are both spending against.
func perSlotContext() int {
	ctxSize := 131072
	if v := envOr("ATLAS_CTX_SIZE", ""); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			ctxSize = n
		}
	}
	return ctxSize / parallelSlots()
}

// numberedLineOverhead is the cost of the "%d\t" prefix the restatement adds
// to every line — on a 2000-line file that is most of its size.
func numberedLineOverhead(content string) int {
	return estTokens(strings.Repeat("0000\t", strings.Count(content, "\n")+1))
}

func conversationTokenBudget() int {
	perSlot := perSlotContext()
	// Sliding window sized to the actual slot: reserve room for the model's
	// reply (max_tokens) plus a margin for system-prompt growth and tokenizer
	// slack, and give the REST of the slot to the conversation. The previous
	// flat 14k cap was too aggressive — on a 32k slot it left ~10k unused AND
	// dropped the file the model was editing, so weak models hallucinated
	// symbols/lines they could no longer see. The active file is additionally
	// pinned in trimMessages so it survives the window regardless. The
	// model-agnostic re-encode cost (SWA models re-process the prompt each
	// turn) is bounded by the slot itself; deploys that need it smaller can
	// still set ATLAS_AGENT_HISTORY_BUDGET.
	// Reserve: the model's reply (max_tokens), a fixed margin for
	// system-prompt growth, and a proportional tokenizer-slack margin.
	// estTokens is chars/4, which UNDER-counts dense content (code,
	// JSON-escaped tool results run closer to 3 chars/token) — without
	// the proportional slack the estimate can pass while the real
	// prompt exceeds the slot (observed: 32844 real vs 32768 slot).
	budget := perSlot - agentMaxTokens() - 2048 - perSlot/8
	if budget < 4000 {
		budget = 4000 // floor: tiny-context deploys still keep a usable window
	}
	// Optional hard ceiling — unset by default. Only set
	// ATLAS_AGENT_HISTORY_BUDGET to bound per-turn re-encode cost below the
	// slot capacity (trades retained context for faster turns on SWA models).
	if v := envOr("ATLAS_AGENT_HISTORY_BUDGET", ""); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 && n < budget {
			budget = n
		}
	}
	return budget
}

// loopingTailCount is how many times the stream's 48-char tail appears in
// the whole buffer. The threshold belongs to the CALLER because it depends
// on what is streaming. Prose degeneration ("...I'll just say X. Wait, I
// can't...") repeats until max_tokens, so 3 occurrences is already strong
// evidence. CODE is legitimately self-similar: a grid walker's four
// elif-direction branches, a debouncer's run_ bookkeeping lines — 48-char
// windows repeat 3-4 times in perfectly healthy files. With the threshold
// at 3 for everything, the detector cut healthy write_file drafts at the
// same structural spot every time: measured across one 50-task run, 17
// cuts, 10 of them truncating write_file code, and the two families whose
// code is most self-similar (walk, debounce) went 0/5 each — the cut
// stump landed, the model patched the cut line instead of rewriting, and
// the patch drifted (a comma in the print, spaces lost from a join).
func loopingTailCount(s string) int {
	if len(s) < loopProbe*3 {
		return 0
	}
	tail := s[len(s)-loopProbe:]
	if strings.TrimSpace(tail) == "" {
		return 0
	}
	return strings.Count(s, tail)
}

// toolCallLoopThreshold is the repeat count that counts as degeneration
// inside a tool_call stream. A real spiral runs to max_tokens — hundreds
// of repeats — so demanding 10 keeps the guard while making 3-4 branch-
// shaped repeats of healthy code invisible to it.
const toolCallLoopThreshold = 10

// loopProbe is the length of the tail a loop check looks for earlier in the
// stream.
const loopProbe = 48

// streamLooping is the content-loop decision for what a stream has produced
// so far.
func streamLooping(buffered string) bool {
	threshold := 3
	if strings.Contains(buffered, `"tool_call"`) || strings.Contains(buffered, "```") {
		// Code is legitimately self-similar; only spiral-grade
		// repetition is degeneration there. Covers both channels
		// code streams through: tool_call JSON args, and the
		// fenced block of the @fenced sub-call — without the
		// fence case the prose threshold would re-cut healthy
		// code in the channel built to avoid exactly that.
		threshold = toolCallLoopThreshold
	}
	return loopingTailCount(buffered) >= threshold || countedLoop(buffered)
}

// countedLoopThreshold is the repeat count, with numbers masked, at which a
// prose reply counts as a loop.
const countedLoopThreshold = 5

// proseReplyRe matches the start of a text or done reply.
var proseReplyRe = regexp.MustCompile(`^\s*\{\s*"type"\s*:\s*"(text|done)"`)

// digitRunRe matches one run of digits.
var digitRunRe = regexp.MustCompile(`[0-9]+`)

// countedLoop reports a prose reply that loops while it counts ("29. I'll
// check planning.py's end. 30. I'll check planning.py's end. ..."). Each
// repeat carries a new number, so no tail repeats verbatim, loopingTailCount
// never sees it, and the reply runs to the token cap. With each run of digits
// masked, the repeats are the same text.
//
// Only a text or done reply with no code block in it: a file body can count
// legitimately (CSV rows, a numbered test table, a migration list), and it
// streams in a tool call, a fenced sub-call, or a malformed call whose type
// names the tool, none of which starts as a prose reply. The probe must also
// hold words, so a numeric table in an answer is not a loop.
func countedLoop(s string) bool {
	if !proseReplyRe.MatchString(s) || strings.Contains(s, "```") {
		return false
	}
	masked := digitRunRe.ReplaceAllString(s, "#")
	if loopingTailCount(masked) < countedLoopThreshold {
		return false
	}
	letters := 0
	for _, b := range []byte(masked[len(masked)-loopProbe:]) {
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') {
			letters++
		}
	}
	return letters >= 16
}

// agentMaxTokens is the per-turn generation ceiling (ATLAS_MAX_TOKENS,
// default 8192). Shared by the LLM request and conversationTokenBudget so the
// window and the reply reservation stay consistent.
func agentMaxTokens() int {
	maxTokens := 8192
	if v := envOr("ATLAS_MAX_TOKENS", ""); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			maxTokens = n
		}
	}
	return maxTokens
}

// parallelSlots returns the llama-server --parallel slot count for this
// deployment (ATLAS_PARALLEL_SLOTS), defaulting to 4 to match the
// entrypoint. Used both for KV-slot isolation and per-slot context math.
func parallelSlots() int {
	slots := 4
	if v := envOr("ATLAS_PARALLEL_SLOTS", ""); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			slots = n
		}
	}
	return slots
}

// pinnedIndices returns the two message indices trimMessages will keep
// regardless of the tail window: the most recent user message (the task)
// and the most recent file-content tool result (the file being edited).
// Either is -1 when absent. Shared by trimMessages (which re-injects
// them) and budgetedKeepLast (which must COUNT them — see below).
func pinnedIndices(msgs []AgentMessage) (pinIdx, filePinIdx int) {
	pinIdx, filePinIdx = -1, -1
	for i := len(msgs) - 1; i >= 1; i-- {
		if msgs[i].Role == "user" {
			pinIdx = i
			break
		}
	}
	for i := len(msgs) - 1; i >= 1; i-- {
		if msgs[i].Role == "tool" && (msgs[i].ToolName == "read_file" || msgs[i].ToolName == "outline_file") &&
			!strings.Contains(msgs[i].Content, "You already read") { // skip dedup pointers — they carry no content
			filePinIdx = i
			break
		}
	}
	return pinIdx, filePinIdx
}

// budgetedKeepLast returns how many trailing messages trimMessages should
// keep so the kept set (system + pinned user + pinned file + tail) fits the
// token budget. Floored at 8; when the whole conversation fits it returns
// len(msgs)-1, so nothing is trimmed.
//
// The pinned messages MUST be pre-counted here: trimMessages re-injects
// them even when they fall outside the tail window, so a budget that
// ignored them would under-count the real prompt by the size of the pinned
// read_file, and the request could exceed the per-slot context (a
// llama-server 400 exceed_context_size).
func budgetedKeepLast(msgs []AgentMessage) int {
	if len(msgs) == 0 {
		return 0
	}
	budget := conversationTokenBudget()
	used := estTokens(msgs[0].Content) // system prompt is always kept
	pinIdx, filePinIdx := pinnedIndices(msgs)
	if pinIdx >= 1 {
		used += estTokens(msgs[pinIdx].Content)
	}
	if filePinIdx >= 1 && filePinIdx != pinIdx {
		used += estTokens(msgs[filePinIdx].Content)
	}
	keep := 0
	for i := len(msgs) - 1; i >= 1; i-- {
		t := 0
		if i != pinIdx && i != filePinIdx { // already counted above
			t = estTokens(msgs[i].Content)
		}
		if used+t > budget && keep >= 8 {
			break
		}
		used += t
		keep++
	}
	if keep > len(msgs)-1 {
		keep = len(msgs) - 1
	}
	return keep
}

// trimMessages caps a conversation at roughly 1 (system) + 1 (pinned user) +
// keepLast tail messages, dropping the middle. The pin is the most recent
// role=="user" message — the user's current task. Without the pin, long agent
// loops (5+ tool calls) push the user's instruction off the end of the
// keepLast window, the model loses the task, and replies generically
// ("Hi! I'm ATLAS..."). If the pinned message already lives inside the tail
// window we don't duplicate it.
//
// Assumes msgs[0] is the system prompt.
func trimMessages(msgs []AgentMessage, keepLast int) []AgentMessage {
	if len(msgs) <= keepLast+1 {
		return msgs
	}

	// Pins (shared scan with budgetedKeepLast, which counts them): the
	// most-recent user message — the task — and the most-recent
	// file-content tool result (read_file / outline_file), so the file the
	// model is working on never gets trimmed out from under it. Without
	// the file pin, a long agent loop drops the file content, the model
	// edits BLIND, and a weak model then hallucinates symbols and old_str
	// that aren't in the file (observed live: structural_edit
	// function:count_items and edit_file old_str="return len(items)"
	// against a file containing neither, with the model literally
	// reasoning "I don't see the file content"). The exploration-budget
	// breaker compounds it by telling the model it "has full project
	// context" when the content was already trimmed.
	pinIdx, filePinIdx := pinnedIndices(msgs)

	tailStart := len(msgs) - keepLast
	out := make([]AgentMessage, 0, keepLast+3)
	out = append(out, msgs[0])
	if pinIdx >= 1 && pinIdx < tailStart {
		out = append(out, msgs[pinIdx])
	}
	// Re-inject the pinned file content (as a user-role note so it survives
	// templates that reject orphan tool messages) when it falls outside the
	// kept tail.
	if filePinIdx >= 1 && filePinIdx < tailStart && filePinIdx != pinIdx {
		out = append(out, AgentMessage{
			Role:    "user",
			Content: "[system note]: current contents of the file you are editing (do not invent symbols or lines not shown here):\n" + msgs[filePinIdx].Content,
		})
	}
	out = append(out, msgs[tailStart:]...)
	return out
}

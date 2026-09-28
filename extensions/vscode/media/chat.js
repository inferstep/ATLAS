// Webview-side renderer for the ATLAS chat panel. Deliberately dumb: all
// state lives in the extension host (src/ui/chatView.ts); this script only
// renders the messages it is posted, in order. On load it sends {type:
// "ready"} and the host replays the full transcript.

/* global acquireVsCodeApi */

(function () {
	'use strict';

	const vscode = acquireVsCodeApi();

	const messagesEl = document.getElementById('messages');
	const inputEl = document.getElementById('input');
	const sendEl = document.getElementById('send');
	const stopEl = document.getElementById('stop');

	/** The assistant bubble currently receiving streamed text, if any. */
	let openAssistantEl = null;
	/** The collapsible "Thinking…" section currently receiving reasoning
	 * deltas, if any. Closed alongside the assistant bubble. */
	let openReasoningEl = null;
	/** Tool chips awaiting a tool_result, keyed by tool name (FIFO per name). */
	const pendingChips = new Map();
	/** Open permission cards keyed by prompt id. */
	const permissionCards = new Map();
	/** Current plan checklist: step id -> row element. A new planLoaded
	 * (revision) replaces the map — old checklists stay in the log, frozen. */
	let planSteps = new Map();

	/** Single transient progress line pinned above the composer. */
	const progressEl = document.getElementById('progress');

	function scrollToBottom() {
		messagesEl.scrollTop = messagesEl.scrollHeight;
	}

	function appendBlock(className, text) {
		const el = document.createElement('div');
		el.className = className;
		if (text !== undefined) {
			el.textContent = text;
		}
		messagesEl.appendChild(el);
		scrollToBottom();
		return el;
	}

	function closeAssistantBubble() {
		openAssistantEl = null;
		openReasoningEl = null;
	}

	function assistantBubble() {
		if (!openAssistantEl) {
			openAssistantEl = appendBlock('msg assistant', '');
		}
		return openAssistantEl;
	}

	function reasoningSection() {
		if (!openReasoningEl) {
			const details = document.createElement('details');
			details.className = 'reasoning';
			const summary = document.createElement('summary');
			summary.textContent = 'Thinking…';
			details.appendChild(summary);
			const body = document.createElement('div');
			body.className = 'reasoning-body';
			details.appendChild(body);
			messagesEl.appendChild(details);
			scrollToBottom();
			openReasoningEl = body;
		}
		return openReasoningEl;
	}

	function addPlanChecklist(steps, revision) {
		planSteps = new Map();
		const card = document.createElement('details');
		card.className = 'plan-card';
		card.open = true;
		const summary = document.createElement('summary');
		summary.textContent = revision > 0 ? 'Plan (revision ' + revision + ')' : 'Plan';
		card.appendChild(summary);
		const list = document.createElement('div');
		list.className = 'plan-steps';
		for (const step of steps) {
			const row = document.createElement('div');
			row.className = 'plan-step';
			const box = document.createElement('span');
			box.className = 'plan-box';
			box.textContent = '☐';
			row.appendChild(box);
			const label = document.createElement('span');
			label.textContent = step.label;
			row.appendChild(label);
			list.appendChild(row);
			planSteps.set(step.id, row);
		}
		card.appendChild(list);
		messagesEl.appendChild(card);
		scrollToBottom();
	}

	function checkPlanStep(stepId) {
		const row = planSteps.get(stepId);
		if (!row) {
			return;
		}
		row.classList.add('done');
		const box = row.querySelector('.plan-box');
		if (box) {
			box.textContent = '☑';
		}
	}

	function setProgress(text) {
		progressEl.textContent = text;
		progressEl.hidden = text === '';
	}

	function addToolChip(name, detail) {
		const chip = document.createElement('div');
		chip.className = 'chip pending';
		const label = document.createElement('span');
		label.className = 'chip-label';
		label.textContent = detail ? name + ' ' + detail : name;
		const status = document.createElement('span');
		status.className = 'chip-status';
		status.textContent = '⋯'; // ⋯ spinner placeholder
		chip.appendChild(status);
		chip.appendChild(label);
		messagesEl.appendChild(chip);
		scrollToBottom();

		if (!pendingChips.has(name)) {
			pendingChips.set(name, []);
		}
		pendingChips.get(name).push(chip);
	}

	function resolveToolChip(tool, success, elapsed, error, diffId, summary) {
		const queue = pendingChips.get(tool);
		let chip = queue && queue.length > 0 ? queue.shift() : null;
		if (!chip) {
			// Result without a rendered call (e.g. replay edge) — show standalone.
			chip = appendBlock('chip ' + (success ? 'ok' : 'fail'), (success ? '✓ ' : '✗ ') + tool + (error ? ': ' + error : ''));
		} else {
			chip.classList.remove('pending');
			chip.classList.add(success ? 'ok' : 'fail');
			const status = chip.querySelector('.chip-status');
			status.textContent = success ? '✓' : '✗';
			if (elapsed) {
				const time = document.createElement('span');
				time.className = 'chip-elapsed';
				time.textContent = elapsed;
				chip.appendChild(time);
			}
			if (!success && error) {
				const detail = document.createElement('div');
				detail.className = 'chip-error';
				detail.textContent = error;
				chip.appendChild(detail);
			}
			// For a read-only ask the tool output IS the answer; a chip that
			// shows only a checkmark answers nothing.
			if (success && summary) {
				const out = document.createElement('div');
				out.className = 'chip-summary';
				out.textContent = summary;
				chip.appendChild(out);
			}
		}
		if (diffId !== undefined && diffId !== null) {
			const view = document.createElement('button');
			view.type = 'button';
			view.className = 'chip-diff';
			view.textContent = 'View change';
			view.addEventListener('click', () => {
				vscode.postMessage({ type: 'viewAppliedDiff', id: diffId });
			});
			chip.appendChild(view);
		}
	}

	/** A denied call never gets a tool_result — consume its pending chip so
	 * the next allowed call of the same tool pairs with its own chip. */
	function denyToolChip(tool) {
		const queue = pendingChips.get(tool);
		const chip = queue && queue.length > 0 ? queue.shift() : null;
		if (!chip) {
			return;
		}
		chip.classList.remove('pending');
		chip.classList.add('fail');
		const status = chip.querySelector('.chip-status');
		status.textContent = '✗';
		const detail = document.createElement('div');
		detail.className = 'chip-error';
		detail.textContent = 'denied';
		chip.appendChild(detail);
	}

	/** Turn-end backstop: calls rejected before execution (truncated args,
	 * workspace-boundary gate) emit neither tool_result nor
	 * permission_denied, so their chips would spin forever. */
	function settleLeftoverChips() {
		for (const queue of pendingChips.values()) {
			for (const chip of queue) {
				chip.classList.remove('pending');
				const status = chip.querySelector('.chip-status');
				if (status) {
					status.textContent = '–';
				}
			}
		}
		pendingChips.clear();
	}

	function addPermissionCard(id, tool, detail, message, canDiff, note, oneTimeOnly) {
		const card = document.createElement('div');
		card.className = 'permission-card';

		const title = document.createElement('div');
		title.className = 'permission-title';
		title.textContent = 'Permission: ' + tool;
		card.appendChild(title);

		if (message) {
			const body = document.createElement('div');
			body.className = 'permission-message';
			body.textContent = message;
			card.appendChild(body);
		}
		if (detail) {
			const args = document.createElement('div');
			args.className = 'permission-args';
			args.textContent = detail;
			card.appendChild(args);
		}
		if (note) {
			const hint = document.createElement('div');
			hint.className = 'permission-note';
			hint.textContent = note;
			card.appendChild(hint);
		}

		const actions = document.createElement('div');
		actions.className = 'permission-actions';
		if (canDiff) {
			const view = document.createElement('button');
			view.type = 'button';
			view.className = 'view-diff';
			view.textContent = 'View Diff';
			view.addEventListener('click', () => {
				vscode.postMessage({ type: 'viewPermissionDiff', id: id });
			});
			actions.appendChild(view);
		}
		// A deletion is approved one file at a time: no session-wide answer.
		const buttons = oneTimeOnly
			? [['Allow Once', 'allow-once'], ['Deny', 'deny']]
			: [['Allow Once', 'allow-once'], ['Allow for Session', 'allow-session'], ['Deny', 'deny']];
		for (const pair of buttons) {
			const button = document.createElement('button');
			button.type = 'button';
			button.textContent = pair[0];
			if (pair[1] === 'deny') {
				button.className = 'deny';
			}
			button.addEventListener('click', () => {
				vscode.postMessage({ type: 'permissionAnswer', id: id, choice: pair[1] });
			});
			actions.appendChild(button);
		}
		card.appendChild(actions);

		messagesEl.appendChild(card);
		permissionCards.set(id, card);
		scrollToBottom();
	}

	function resolvePermissionCard(id, outcome) {
		const card = permissionCards.get(id);
		permissionCards.delete(id);
		if (!card) {
			// Replay path: the prompt message was recorded before its resolution —
			// both replay in order, so a missing card only means a pruned DOM.
			return;
		}
		const actions = card.querySelector('.permission-actions');
		if (actions) {
			actions.remove();
		}
		const result = document.createElement('div');
		result.className = 'permission-outcome';
		result.textContent = outcome;
		card.classList.add('resolved');
		card.appendChild(result);
	}

	function setBusy(busy) {
		sendEl.hidden = busy;
		stopEl.hidden = !busy;
		inputEl.disabled = busy;
	}

	function resetAll() {
		messagesEl.textContent = '';
		pendingChips.clear();
		permissionCards.clear();
		planSteps.clear();
		openAssistantEl = null;
		openReasoningEl = null;
		setProgress('');
	}

	window.addEventListener('message', (event) => {
		const message = event.data;
		switch (message.type) {
			case 'userMessage':
				closeAssistantBubble();
				appendBlock('msg user', message.text);
				break;
			case 'assistantDelta':
				assistantBubble().textContent += message.text;
				scrollToBottom();
				break;
			case 'reasoningDelta':
				reasoningSection().textContent += message.text;
				scrollToBottom();
				break;
			case 'toolCall':
				closeAssistantBubble();
				addToolChip(message.name, message.detail);
				break;
			case 'toolResult':
				resolveToolChip(message.tool, message.success, message.elapsed, message.error, message.diffId, message.summary);
				break;
			case 'toolDenied':
				denyToolChip(message.tool);
				break;
			case 'doneSummary': {
				// Final answer of a tool-shaped turn (done.summary) — assistant-
				// style bubble with a "done" marker, distinct from streamed text.
				closeAssistantBubble();
				const bubble = appendBlock('msg assistant done-summary', '');
				const marker = document.createElement('span');
				marker.className = 'done-marker';
				marker.textContent = 'done';
				bubble.appendChild(marker);
				const body = document.createElement('span');
				body.textContent = message.text;
				bubble.appendChild(body);
				break;
			}
			case 'note':
				closeAssistantBubble();
				appendBlock('note', message.text);
				break;
			case 'badge':
				appendBlock('badge', message.text);
				break;
			case 'planLoaded':
				closeAssistantBubble();
				addPlanChecklist(message.steps, message.revision);
				break;
			case 'planStep':
				checkPlanStep(message.stepId);
				break;
			case 'progress':
				setProgress(message.text);
				break;
			case 'permissionPrompt':
				closeAssistantBubble();
				addPermissionCard(message.id, message.tool, message.detail, message.message, message.canDiff, message.note, message.oneTimeOnly);
				break;
			case 'permissionResolved':
				resolvePermissionCard(message.id, message.outcome);
				break;
			case 'turnDone':
				closeAssistantBubble();
				settleLeftoverChips();
				setProgress('');
				break;
			case 'turnError':
				closeAssistantBubble();
				settleLeftoverChips();
				setProgress('');
				appendBlock('error-card', message.message);
				break;
			case 'busy':
				setBusy(message.value);
				break;
			case 'reset':
				resetAll();
				break;
		}
	});

	function submit() {
		const text = inputEl.value.trim();
		if (text === '') {
			return;
		}
		inputEl.value = '';
		vscode.postMessage({ type: 'submit', text: text });
	}

	sendEl.addEventListener('click', submit);
	stopEl.addEventListener('click', () => vscode.postMessage({ type: 'cancel' }));
	inputEl.addEventListener('keydown', (event) => {
		if (event.key === 'Enter' && !event.shiftKey) {
			event.preventDefault();
			submit();
		}
	});

	vscode.postMessage({ type: 'ready' });
})();

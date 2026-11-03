// Virtual documents for diff rendering. Registers the `atlas-diff:` scheme
// (TextDocumentContentProvider — read-only by construction) and opens native
// side-by-side diffs via the built-in `vscode.diff` command. Used for
// permission-time predictions (editPreview.ts), post-result applied-change
// views, and edit_file's server-computed diff_preview text.

import * as vscode from 'vscode';

export class DiffProvider implements vscode.TextDocumentContentProvider {
	static readonly scheme = 'atlas-diff';

	private readonly docs = new Map<string, string>();
	private readonly order: string[] = [];
	private seq = 0;

	register(): vscode.Disposable {
		return vscode.workspace.registerTextDocumentContentProvider(DiffProvider.scheme, this);
	}

	provideTextDocumentContent(uri: vscode.Uri): string {
		return this.docs.get(uri.toString()) ?? '';
	}

	/** Open a native side-by-side diff. `filePath` is the tool's target path;
	 * its basename is kept in both virtual URIs so VS Code's language
	 * detection (and syntax highlighting) applies to both panes. */
	async openDiff(title: string, filePath: string, left: string, right: string): Promise<void> {
		const name = basename(filePath);
		const leftUri = this.stash(`before/${name}`, left);
		const rightUri = this.stash(`after/${name}`, right);
		await vscode.commands.executeCommand('vscode.diff', leftUri, rightUri, title);
	}

	/** Open a unified-diff text blob (edit_file's diff_preview) as a
	 * read-only virtual doc; the .diff suffix gets diff highlighting. */
	async openPreview(filePath: string, content: string): Promise<void> {
		const uri = this.stash(`preview/${basename(filePath)}.diff`, content);
		const doc = await vscode.workspace.openTextDocument(uri);
		await vscode.window.showTextDocument(doc, { preview: true });
	}

	private stash(name: string, content: string): vscode.Uri {
		const uri = vscode.Uri.from({ scheme: DiffProvider.scheme, path: `/${this.seq++}/${name}` });
		const key = uri.toString();
		this.docs.set(key, content);
		this.order.push(key);
		// Bounded memory: an editor still open on an evicted doc just
		// renders empty on the next provider read.
		while (this.order.length > 64) {
			this.docs.delete(this.order.shift() as string);
		}
		return uri;
	}
}

function basename(filePath: string): string {
	const clean = filePath.replace(/[\\/]+$/, '');
	const index = Math.max(clean.lastIndexOf('/'), clean.lastIndexOf('\\'));
	const name = index === -1 ? clean : clean.slice(index + 1);
	return name === '' ? 'file' : name;
}

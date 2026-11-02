# ATLAS JetBrains Plugin

A JetBrains IDE client for the [ATLAS](https://github.com/inferstep/ATLAS) agent proxy — a thin UI layer wrapping `atlas-proxy`'s agent loop with no agent logic in the plugin itself.

**Status: Stage 6 — hardening and release**, tracked in [issue #35](https://github.com/inferstep/ATLAS/issues/35), one pull request per stage.

## Compose and Jewel in the tool window

The chat panel is a Jewel Compose panel in a Swing tool window (`src/main/kotlin/com/inferstep/atlas/ui/AtlasToolWindowFactory.kt`). It follows the IDE theme through `SwingBridgeTheme`, and it renders the assistant's text with Jewel's Markdown renderer (`src/main/kotlin/com/inferstep/atlas/ui/AtlasChatPanel.kt`).

`composeUI()` only adds a *compile-time* dependency. Without the matching `<depends>com.intellij.modules.compose</depends>` in the descriptor the tool window throws `NoClassDefFoundError: androidx/compose/ui/awt/ComposePanel` on a product that does not enable Compose for other plugins, PyCharm 2026.1 among them, even though the class is in its distribution.

Jewel's Markdown renderer is reached by **module name, not by plugin id.** The `intellij.platform.jewel.markdown.*` modules ship in every 2026.1 product, but a `<depends>` takes a plugin id and these are content modules: `<depends>intellij.platform.jewel.markdown.core</depends>` names nothing and makes the platform refuse to load the plugin. The platform's own descriptor declares `intellij.platform.compose.markdown` with `visibility="public"` (in `lib/product-backend.jar` as `META-INF/plugin.xml` on IDEA 2026.1.3 and `META-INF/PythonPlugin.xml` on PyCharm 2026.1), and that one module pulls in Compose plus every Jewel Markdown module. So the descriptor says `<dependencies><module name="intellij.platform.compose.markdown"/></dependencies>`.

The build side names the modules too, because `bundledModule` attaches the jar of the module it is given and not that module's declared dependencies: the aggregator `intellij.platform.compose.markdown` carries no classes of its own, so `intellij.platform.jewel.markdown.core` and `intellij.platform.jewel.markdown.ideLafBridgeStyling` are declared for the compiler.

`AtlasToolWindowFactoryTest` guards the descriptor: it fails when the `<depends>` on `com.intellij.modules.compose` is missing, and when a `<depends>` names an `intellij.platform.jewel` module, which disables the plugin.

## How it works

The plugin is a thin client over the proxy HTTP API (see `docs/API.md`). The layout mirrors `extensions/vscode/`, which is the reference IDE client; the TUI (`tui/`) remains the reference client overall.

## The protocol/client layer (Stage 2)

Two packages, both deliberately free of any `com.intellij` import, so they compile and run on a plain JVM and are tested with plain JUnit:

```
src/main/kotlin/com/inferstep/atlas/protocol/Types.kt   # wire types, one file per direction
src/main/kotlin/com/inferstep/atlas/protocol/Sse.kt     # the SSE frame parser
src/main/kotlin/com/inferstep/atlas/client/AtlasClient.kt  # Ktor client for the seven endpoints
```

`ProtocolIsolationTest` enforces the no-IntelliJ rule mechanically: the plugin module compiles the platform onto the same classpath, so a stray `import com.intellij.…` would build, pass review, and quietly take the layer hostage.

**Events are decoded on demand.** A frame is `{"type":"<name>","data":{…}}`, and `data` stays a raw `JsonElement` until a caller asks for a payload shape with `event.payload<TextPayload>()`. The proxy documents event types this client does not render (every V3 stage, every detector intervention) and may add more, so an unknown type survives the envelope rather than failing the stream. Every payload field has a default, so a proxy that adds a field does not break a client that has never heard of it.

**`SseParser` is hand-written, and that is deliberate.** The wire format is `data:`-only with a `[DONE]` sentinel, and the client must skip a malformed frame rather than fail the turn — that permissiveness is the parser's, and a strict third-party SSE parser would silently change it. The parser buffers bytes and decodes a line only once its newline has arrived, because a `\n` byte can never occur inside a UTF-8 multi-byte sequence; that is what makes a character split across two socket reads work without an incremental decoder. `ktor-client-sse` is therefore not a dependency; Ktor is used for transport and the payload is read as a byte stream.

**No `HttpTimeout` plugin is installed**, on purpose. While a `permission_request` is open the proxy sends nothing on the agent stream, and the documented fail-safe is `ATLAS_PERMISSION_TIMEOUT_SEC` (600s). A read timeout shorter than that kills the turn mid-permission; the Java engine's defaults are no connect timeout and no read timeout, which is exactly what this needs.

### Why Ktor is pinned to 3.4.3

**Ktor must not be bumped past 3.4.3 while the plugin targets IntelliJ Platform 2026.1.** Ktor 3.5.0 moved to kotlinx-coroutines 1.11.0, which hoisted `Job.invokeOnCompletion$default` onto the `Job` interface (JVM default methods). IntelliJ Platform 2026.1 bundles kotlinx-coroutines 1.10.2 (as `1.10.2-intellij-1`), where that synthetic lives in `Job$DefaultImpls` — so the first request Ktor issues against the platform's coroutines fails with `NoSuchMethodError: 'kotlinx.coroutines.DisposableHandle kotlinx.coroutines.Job.invokeOnCompletion$default(...)'`.

The plugin must use the platform's coroutines: shipping a second copy inside the plugin jar would shadow the platform's and is exactly the two-runtime hazard the build excludes against. 3.4.3 is the newest Ktor built against coroutines 1.10.2, so it is the newest one that runs here. `.github/dependabot.yml` ignores `io.ktor:*` at `>= 3.5.0` for this tree, and the mock-proxy tests below fail loudly if the pin is ever moved.

That same incompatibility is why the tests declare no coroutines dependency of their own: the `test` task runs against the platform's coroutines, exactly as the plugin does in the IDE. A second, newer copy on the test classpath would have hidden the bug above instead of catching it.

## The chat (Stage 3)

```
src/main/kotlin/com/inferstep/atlas/session/TurnManager.kt     # one turn: session id, history, allowlist, cancel
src/main/kotlin/com/inferstep/atlas/session/PermissionFlow.kt  # the open prompts and their answers
src/main/kotlin/com/inferstep/atlas/session/ChatSession.kt     # the transcript and the controls
src/main/kotlin/com/inferstep/atlas/session/DoneOutcome.kt     # done.status and done.reason, as the banner shows them
src/main/kotlin/com/inferstep/atlas/session/ToolSummary.kt     # the one-line answer a tool chip shows
src/main/kotlin/com/inferstep/atlas/ui/AtlasChatPanel.kt       # the panel: a renderer of ChatSession's state
```

A port of `extensions/vscode/src/session/`, with the TUI as the reference where the two disagree. None of `session/` imports `com.intellij` or Compose, so the whole conversation is exercised under plain JUnit and the panel only lays it out — and `ProtocolIsolationTest` now enforces that for this package as well as `protocol/` and `client/`.

**Every turn declares a task contract.** `task_contract.task_mode` is `work` (the workspace should change) or `question` (nothing should), and it is a control the user operates — nothing reads the message text, so the same sentence can be sent either way. The obligation lists (`expected_outputs`, `verification`) stay absent: someone typing into a chat box has told the plugin nothing structured about which files must exist or which command must run, and the API refuses a `question` that declares them. This is the same rule the repo's contract test states for the other clients: absence is reserved for external and legacy callers, so an owned client always declares a mode.

**History and the session allowlist live on the client.** Each turn mints a fresh 24-character hex `session_id`, sends the rolling transcript (the most recent 40 entries, assistant turns re-wrapped in the `{"type":"text","content":…}` envelope the TUI uses) and re-sends `session_allowed_tools`, because the proxy does not persist either. A deleted file counts as one request: `delete_file` is never approved for the session, and its card offers no session-wide button.

**Cancellation stops the stream first.** `cancel()` aborts the local SSE request, which is the mechanism that actually ends the turn, and posts `/cancel` as a backstop. The panel's Cancel button routes there; a cancelled turn keeps whatever text it had produced in the history it replays.

**The connection is a setting with an environment fallback.** Settings → Tools → ATLAS holds the proxy URL, permission mode and status-bar interval; the token is stored in the platform credential store through `AtlasTokenStore` (`PasswordSafe`), not in `atlas.xml`. A blank URL or token falls back to `ATLAS_PROXY_URL` / `ATLAS_SERVICE_TOKEN_FILE`, the variables the TUI and `atlas doctor` use, so an existing install keeps working. `AtlasConnection.toString()` redacts the token, and the old relative `secrets/service-token` fallback is gone: it resolved against the IDE's working directory rather than the install, which turned "the token is elsewhere" into "no token configured". The diff review and the workspace checks that Stage 4 adds are described under [The diffs and the workspace](#the-diffs-and-the-workspace-stage-4), and the Stage 5 surfaces under [The IDE integration](#the-ide-integration-stage-5).

## The diffs and the workspace (Stage 4)

```
src/main/kotlin/com/inferstep/atlas/session/EditPreview.kt        # the change a prompt can predict
src/main/kotlin/com/inferstep/atlas/session/Workspace.kt          # reads, stats, and the GET /workspace verdict
src/main/kotlin/com/inferstep/atlas/session/WorkspaceMismatch.kt  # the passive post-edit mismatch check
src/main/kotlin/com/inferstep/atlas/ui/AtlasDiffViewer.kt         # the IDE's own side-by-side diff
```

A port of `extensions/vscode/src/session/editPreview.ts` and
`extensions/vscode/src/workspace/`, with the IDE half replaced by the platform's
own diff. Everything that decides what to show is in `session/`, so it runs
under plain JUnit; `ui/AtlasDiffViewer.kt` only opens the window.

**A change is shown twice, from two different sources.** Before a write is
approved, the card can offer the *predicted* change: `predictEdit` computes it
from the file as it is now — `write_file`, `insert_after` and `replace_lines`
exactly, `edit_file` exactly when `old_str` matches, and `structural_edit` as a
best-effort splice labelled approximate, because tree-sitter is the server-side
authority. After the call returns, the tool row offers the *exact* change
instead: `ChatSession` snapshots the target at `tool_call` time and diffs that
snapshot against the file on disk, which also compensates for an approximate
prediction. When the file is not readable in this workspace — the mismatch case
below — `edit_file`'s server-computed `diff_preview` is shown instead.

**The diff itself is the platform's.** `AtlasDiffViewer` builds a
`SimpleDiffRequest` from the two texts and hands it to `DiffManager`, so the
panes are the IDE's own diff viewer, with its theme and its whitespace and
collapse actions. The target's file name picks the `FileType`, so both sides get
the IDE's syntax highlighting; an extension the IDE does not know degrades to
plain text rather than failing.

**The proxy's writes reach the IDE's VFS.** On a bind-mounted install
IntelliJ's own watcher usually notices first, but the plugin still refreshes the
changed path after every successful file op, so a file the session just created
turns up even when nothing else is watching that directory.

**A wrong folder is said out loud**, by two checks — one exact, one passive:

- At panel start, `checkWorkspace` asks `GET /workspace` and compares the host
  path the proxy reports with the folder this window has open. A host path that
  does not cover it is worth interrupting for: every later edit lands where the
  user is not looking. An older proxy without the route, a relative path, or no
  folder open all read as "can't tell", never as a mismatch.
- After each successful file op, `MismatchDetector` compares a pre-op stat with
  a post-op one: a write must exist, a delete must not, a move must be at its
  destination. An unchanged mtime is inconclusive rather than a mismatch,
  because a coarse-mtime filesystem can stamp a real change with the same time.
  One contradiction is enough to warn once for the session.

`atlas workspace align` — the CLI that moves the containers' binds — is
deliberately **not** wired up. v1 is parity with the VS Code client's protocol,
session and permission behaviour, and the answer on #35 was to leave align for a
later stage, so the plugin can say the folder is wrong but not fix it.

**Not seen on screen.** The session suites cover the predictions, the
snapshots and both workspace checks, and `AtlasWorkspaceRefreshTest` covers
the VFS refresh. No test covers the diff window itself (`AtlasDiffViewer`);
a manual `runPyCharm` smoke run is the check that would cover it
([Smoke-test status](#smoke-test-status)).

## The IDE integration (Stage 5)

```
src/main/kotlin/com/inferstep/atlas/ide/AtlasProjectService.kt      # one session, one connection, one status refresh signal
src/main/kotlin/com/inferstep/atlas/settings/AtlasSettings.kt      # persisted URL, permission mode and poll interval
src/main/kotlin/com/inferstep/atlas/settings/AtlasTokenStore.kt    # PasswordSafe-backed service token
src/main/kotlin/com/inferstep/atlas/settings/AtlasConfigurable.kt  # Settings → Tools → ATLAS
src/main/kotlin/com/inferstep/atlas/ui/AtlasStatusBarWidget.kt     # /ready polling and the click menu
src/main/kotlin/com/inferstep/atlas/ui/AtlasNotifier.kt            # permission/error/workspace notifications
src/main/kotlin/com/inferstep/atlas/ui/AtlasActions.kt             # commands registered in plugin.xml
```

**One session per project.** `AtlasProjectService` owns the `ChatSession`, the Ktor client and the workspace view. The panel, the status bar and the actions all use that one session, so closing the tool window does not forget a conversation, and Cancel or New Conversation works from the Tools menu even when the panel is closed. A turn is launched on the service scope rather than the Compose scope, which is why closing the panel does not abort it.

**Settings are the source of the connection.** `AtlasSettings` persists the proxy URL, the permission mode and the status-bar poll interval in `atlas.xml`; the token is never there. A blank URL follows `ATLAS_PROXY_URL`, and a blank stored token follows `ATLAS_SERVICE_TOKEN_FILE`, so an existing install keeps working. Applying settings publishes a message-bus event; each project service swaps its client only when the URL or token actually changed, and the panel's header and the status bar observe the new value. The permission-mode chips in the panel write the same setting rather than a second, panel-local mode.

**The token is write-only in the UI.** Settings has a password field and a "Remove the stored service token" checkbox; an existing secret is never read back into the field. `ATLAS: Set Service Token` in the Tools menu is the same action without opening the Settings dialog. `PasswordSafe` delegates to the platform keychain (Keychain, Secret Service, Windows credential manager, or the in-memory store in tests), and `AtlasConnection.toString()` prints `<redacted>` instead of the token.

**The status bar polls `/ready` and probes a route that needs the token.** The item shows `ATLAS` when every gate passes, `ATLAS · starting` with the failing gates in its tooltip, `ATLAS · offline` when the proxy cannot be reached, and `ATLAS · token` when `/workspace` answers 401. The token probe is separate because `/ready` is open: a wrong or missing token would otherwise leave the item looking ready. It runs only after `/ready` answered, so an unreachable proxy reads `offline` rather than `token` and a poll waits for one time limit rather than two; a rejection belongs to the address and token it was learned on, so changing either starts over. Polling pauses while a turn streams and refreshes when it ends; the interval is settable from 5 to 3600 seconds. `/v1/calibration/status` is fetched at startup and on **ATLAS: Refresh Status & Calibration**, not on every poll, because each call re-probes the lens service. A 5-second timeout keeps a blackholed host from leaving the item on "starting" forever. Clicking the item opens the common actions in a popup.

**Progress and the plan are state, not a second transcript.** `ChatSession` reads the `llm_*`, `v3_*` and `plan_*` events: a transient line under the mode controls shows thinking, prompt-evaluation percentage, the current V3 stage, a veto or a lens intervention, and the panel draws the generated plan with the steps `plan_adherence` marked satisfied. The line is cleared at the end of the turn, so a replay never shows a stale spinner; the plan is cleared when the next turn starts or the conversation is reset.

**Notifications mirror the two things that can wait for the user.** A `permission_request` opens the inline card and a native notification with **View Diff** (when a prediction exists), **Allow Once**, **Allow for Session** (not for a deletion) and **Deny**; the first answer wins. A new error notice opens an error notification with **Open Chat** and **Settings**, and the workspace-mismatch notice opens a warning notification. The notifier observes `ChatSession.state`, not the panel, so a closed tool window does not silence a prompt.

**Actions are the VS Code commands, with JetBrains names.** **ATLAS: Open Chat**, **New Conversation**, **Cancel Current Turn**, **Set Service Token**, **Refresh Status & Calibration** and **Settings** are in the Tools menu; Cancel, New Conversation and Settings are also tool-window title actions, and the status-bar menu carries the useful subset. Each delegates to `AtlasProjectService`, so it operates the same session the panel shows.

**The tool window draws its own icon.** `plugin.xml`'s `<toolWindow>` names `icons/toolwindow.svg`, a 13 x 13 mark — the size the platform draws a stripe button at — with a `_dark` file beside it that the platform selects by suffix, the same light/dark split as the 40 x 40 plugin icons. `AtlasToolWindowIconTest` holds the descriptor entry, both files at 13 x 13 with the mark's four steps, stem and dot, and the pair's shared geometry, so an edit to one theme cannot leave the other drawing a different mark. `scripts/check-distribution.sh` audits only the two `META-INF` plugin icons, so this test is what stands between a renamed file and the platform's default square.

**Not seen on screen.** The descriptor test checks that the settings page, the status bar factory, the notification group and the actions are registered; `AtlasStatusTest` covers the `/ready` rules, and `AtlasConfigurableTest` and `AtlasProjectServiceTest` exercise the settings/service wiring under the platform test framework. The status bar's polling loop, the popup menus and notification actions themselves, and every surface's actual appearance are covered by no test in a running IDE. A `runPyCharm` session that opens Settings, saves a token, watches the status bar go ready, allows a prompt from the notification and opens its diff is the check that would close that gap.

## Hardening and release (Stage 6)

Stage 6 turns "works here" into a release candidate. The built ZIP is checked
against every target product's binary surface, the archive itself is audited,
and the same checks run in CI.

### Plugin Verifier across the products

[JetBrains' Plugin Verifier](https://github.com/JetBrains/intellij-plugin-verifier)
reads the built ZIP and checks every class, method and extension point the
plugin names against each product build, without launching the IDE:

```bash
./gradlew verifyPlugin                                  # all four products, both lines
./gradlew verifyPlugin -PatlasVerifyProducts=pycharm     # one product, for a small runner
```

The property exists because the verifier downloads a full IDE distribution per
product and line, and a standard CI runner's disk cannot hold eight of them. CI
runs a matrix of four jobs, one product each, so every job is two downloads; a
local release run leaves the property unset and checks all eight.

| Product | 2026.1.5 | 2026.2.3 |
|---|---|---|
| IntelliJ IDEA | IU-261.27258.48 | IU-262.10968.63 |
| PyCharm | PY-261.27258.51 | PY-262.10968.92 |
| WebStorm | WS-261.27258.45 | WS-262.10968.77 |
| GoLand | GO-261.27258.47 | GO-262.10968.67 |

All eight reports say **Compatible**. The 2026.1 reports carry two
experimental-API usages (`Markdown` and `SwingBridgeTheme`); the 2026.2
reports add one deprecated usage (`Markdown`) and carry the same two
experimental ones.

The plugin compiles against 2026.1.3 and declares `sinceBuild = 261`, so the
2026.1 line is the supported baseline and 2026.2 is the current stable line
whose removals matter. The compile platform is deliberately **not** moved to
2026.2: building against it would let a 2026.1 IDE meet bytecode that names a
newer API. Verifying against a newer build is the forward-compatibility check.

### What the verifier found, and the fixes

- **`statusBarWidgetFactory` had no `id`,** so it did not match the string
  `AtlasStatusBarWidgetFactory.getId()` returns. The descriptor now sets
  `id="AtlasStatus"`, and the verifier's plugin-structure warning is gone.
- **`SimpleListCellRenderer.create(…)` is deprecated in 2026.2,** the
  `Customizer` overload as well as the `String` one. The permission-mode box
  now uses a small subclass of the renderer and overrides `customize()`,
  which is the class's own abstraction and not deprecated on either line.
- **Kotlin's compatibility mode generated delegating methods** in
  `AtlasToolWindowFactory` for `ToolWindowFactory`'s default methods, and the
  verifier read each copy as the plugin's own experimental or deprecated API
  usage. Compiling with `-jvm-default=no-compatibility` lets the platform's
  implementation be the only one; the six delegating methods, each reported as
  an override and an invocation, disappeared from both reports.
- **Jewel's Markdown composable and `SwingBridgeTheme` remain reported.** Both
  are experimental API in 2026.2 and the Markdown composable is deprecated
  there too. No plugin-visible replacement exists (see
  [Compose and Jewel in the tool window](#compose-and-jewel-in-the-tool-window)), so they stay, are reported, and are read at release rather than
  muted.

The task fails on compatibility warnings and problems,
internal/override-only/non-extendable API, scheduled-for-removal API,
plugin-structure warnings, missing dependencies and an invalid plugin. Deprecated and experimental reports do not fail it: a new
deprecation should not block a release, while a structural defect or an API the
next line removes should.

### Packaging audit

`scripts/check-distribution.sh` reads the built ZIP the way a user installs it
and fails when it contains the wrong id, no numeric version, a missing plugin
icon or one that is not 40 x 40, a bundled coroutine or Kotlin stdlib jar, a
missing transport jar, a duplicate jar name, a test class, or a checkout file
such as `.env`, `secrets/` or a key. It is a script rather than a Gradle task so
it can also be pointed at a downloaded CI artifact:

```bash
bash scripts/check-distribution.sh
bash scripts/check-distribution.sh build/distributions/jetbrains-plugin-0.1.0.zip
```

### CI

`.github/workflows/jetbrains-plugin.yml` keeps the gate and lock jobs and adds:

- a `verify-plugin` matrix of four jobs, one product each (two IDE downloads
  per runner), with the verifier report uploaded whether it passed or failed;
- a distribution-contents step after the gate, and the built ZIP uploaded as
  an artifact.

### Dependency verification

The lock file fixes the version of every dependency; it does not prove that the
bytes behind a version are the bytes that were reviewed. A verification file was
not adopted because Dependabot does not maintain it, so every dependency or
platform bump would need a hand regeneration and review.

### What Stage 6 does not prove

Four-way binary compatibility is checked (Plugin Verifier, in
`.github/workflows/jetbrains-plugin.yml`). The **on-screen** behaviour is
unverified: no check launches an IDE against a live atlas-proxy, and Windows is
untried. A `runPyCharm` run against a live proxy is the check that would close
the UI half ([Smoke-test status](#smoke-test-status)).

## Tests

`./gradlew test` runs the suites in one JUnit Platform run: the `BasePlatformTestCase` suites (via the vintage engine) and the JVM suites (Jupiter). The platform suites — the tool window, the settings page and the project service — need the test framework but no IDE window; none of the suites needs a proxy:

| Class | What it covers |
|---|---|
| `SseTest` | comments, chunk splits at every byte boundary, multi-byte characters, >1MB frames, malformed frames, CRLF, the `[DONE]` sentinel, the end-of-stream flush |
| `TypesTest` | wire names and modes, absent optionals staying absent, unknown fields and unknown event types, the documented `done.status` rule, the six-code error set, and the task contract |
| `RealStreamTest` | a recorded real proxy turn, replayed at chunk sizes 1, 7, 64 and 1024 |
| `ProtocolIsolationTest` | the three IDE-free packages (`protocol`, `client`, `session`) import nothing from `com.intellij` |
| `AtlasClientTest` | the client against an in-process HTTP fixture: streaming, the permission pause and its resume, cancel, the structured 400/401/500 errors and a non-JSON envelope, a dropped connection, the token probe's 200/401/404 split, and every optional endpoint |
| `AtlasConnectionTest` | the documented default URL, a blank URL, the token file, an install with no token, URL normalization, and the redacted `toString` |
| `AtlasStatusTest` | every `/ready` gate combination, the status-bar order of the failing gates, and 401 vs. unreachable failures |
| `AtlasNotifierTest` | notification content escapes markup while keeping the command or path text intact |
| `AtlasStatusBarTooltipTest` | the status-bar tooltip escapes proxy text before it becomes HTML |
| `TurnManagerTest` | session-id minting, the request shape, the 40-entry cap and the assistant envelope, the allowlist re-send, the busy guard, the task mode, and cancel |
| `PermissionFlowTest` | auto-allow, allow-once/session/deny, first-answer-wins, a remote deny, turn-end cleanup, one-file-at-a-time deletions, and advisory post failures |
| `ChatSessionTest` | streaming text into one entry, tool chips with their summaries, the done banner, live prompts, auto-allowed tools, cancellation, the mode controls and reset, the change a successful file op made, the prediction a prompt carries, both workspace checks and their warning flag, a connection swap leaving the open prompt with its own turn, the transient progress line (including what clears it) and the generated plan with its satisfied steps |
| `EditPreviewTest` | the tools that name a path, then one table per tool: write_file, edit_file (exact, approximate, replace_all as a boolean, literal `$`), the python and html structural_edit splices, insert_after and replace_lines, and every case that is not previewable |
| `WorkspaceMismatchTest` | the paths a file op touches, the verdict for each kind of write/edit/delete/move, and the detector as one table of short sessions: FIFO pairing, the one-shot warning, silence on a failed or inconclusive op, and the failed result that answers a denial |
| `WorkspaceTest` | reads and stats inside the root, an absolute path or a `..` escape refused, a missing or empty root reading nothing, and the alignment verdict for each pair of host paths |
| `AtlasSettingsTest` | the URL and token precedence, the stored permission-mode wire names, and the poll-interval bounds |
| `AtlasConfigurableTest` | the settings page builds against the real application services and a memory password safe |
| `AtlasProjectServiceTest` | one service and one session per project, and a permission-mode write reaching both the setting and the session |
| `AtlasTokenStoreTest` | the token round trip through PasswordSafe, including the blank value that removes it |
| `AtlasWorkspaceRefreshTest` | a file written outside the IDE is found in the VFS after `refreshWorkspacePath` |
| `AtlasToolWindowFactoryTest` | the Compose panel mounts in a registered tool window, recreation keeps the project session, every action is registered, and the descriptor keeps the Compose module and the Stage 5 extensions |
| `AtlasToolWindowIconTest` | the descriptor gives the tool window an icon, both theme files are 13 x 13 with the mark's six shapes, and the pair differ only in fill |
| `DoneOutcomeTest` | the status/reason rules, including an absent status and a stream with no `done` at all |
| `ToolSummaryTest` | directory listings, capped listings, match counts, the generic fallbacks, and a shape it does not know |

The fixture (`MockProxy`) is a real server on `com.sun.net.httpserver`, so the transport is exercised for real — which is also what makes a Ktor bump past the pin fail here rather than in an IDE.

## Requirements

* JDK 21 (the Gradle toolchain pins 21; nothing else is installed for you)
* IntelliJ Platform 2026.1 or newer (`sinceBuild = 261`)

The plugin is written in Kotlin 2.3.20 against the IntelliJ Platform 2026.1.3 SDK. Kotlin language and API levels are pinned to 2.3 because the IDE bundles the 2.3.x standard library; compiling against a newer level produces bytecode the platform cannot load.

The plugin uses the Compose runtime and Jewel modules bundled with IntelliJ Platform 2026.1.3: Compose Multiplatform 1.10.0 and Jewel 0.37. `composeUI()` supplies the Compose modules (including the runtime split) and, transitively, the Jewel widgets the plugin uses, and `bundledModule(...)` supplies Jewel's Markdown modules; nothing is packaged into the plugin. The build-side declaration is only half of it, though: `plugin.xml` depends on `com.intellij.modules.compose` and names `intellij.platform.compose.markdown` as a module, and that is what puts the classes on the plugin's classloader at runtime. `SwingBridgeTheme` maps the active Swing Look and Feel into the Compose content (`build.gradle.kts`, `src/main/resources/META-INF/plugin.xml`).

## Building and running

All commands run from `extensions/jetbrains/`:

```bash
./gradlew build            # compile and assemble
./gradlew runIde           # launch a sandboxed IntelliJ IDEA with the plugin
./gradlew runPyCharm       # launch sandboxed PyCharm
./gradlew runWebStorm      # launch sandboxed WebStorm
./gradlew runGoLand        # launch sandboxed GoLand
./gradlew test             # the platform tool-window test and the protocol/client suite
./gradlew ktlintCheck      # Kotlin formatting and lint gate
./gradlew ktlintFormat     # apply the same rules
./gradlew buildPlugin      # build the distributable ZIP
./gradlew verifyPlugin     # Plugin Verifier, all four products on both lines
bash scripts/check-distribution.sh   # audit the built ZIP
```

`verifyPlugin` downloads two IDE distributions per product; pass
`-PatlasVerifyProducts=idea` (or `pycharm`, `webstorm`, `goland`) to check one
product, as the CI matrix does.

The ZIP in `build/distributions/` is installed with **Settings → Plugins →
Install Plugin from Disk**. It is not signed or published to JetBrains
Marketplace; publishing is a separate decision from this release check.

The four run tasks download a full IDE on first use, which is large and slow. They exist so the plugin can be smoke-tested against each product rather than assuming IDEA compatibility.

## Dependency locking

Every version the plugin is built and tested against is fixed in `gradle.lockfile`.
`build.gradle.kts` turns locking on, in strict mode, for the 26 configurations the gate
resolves — 26 of the 101 resolvable configurations of this build — so a version that moves
without a change to the lock file fails the build instead of being resolved quietly.

**The scope is those 26 configurations and no others, and that is deliberate.** Strict mode
fails a locked configuration that has no lock state, so a wider scope breaks tasks this gate
does not run. Measured on this tree with every resolvable configuration locked, resolving
`annotationProcessor` or `intellijPlatformLocal` — neither is resolved here, both are
resolved by other tasks — fails with `Locking strict mode: Configuration ':annotationProcessor'
is locked but does not have lock state.` Anything that makes the gate resolve one more
configuration has to name it in `gatedConfigurations` in `build.gradle.kts`; left out of that
list, it is left out of the lock.

**Five modules are ignored rather than locked:** `kotlinx-coroutines-bom`, `-core`,
`-core-jvm`, `-jdk8` and `-slf4j`. The build excludes them from every Ktor delivery (see [Why
Ktor is pinned to 3.4.3](#why-ktor-is-pinned-to-343)), and resolving a configuration's files
still resolves them, so the strict check demands a lock line for each — a line `--write-locks`
never writes, because the modules are not in the dependency graph. Without the ignore, one
`--write-locks` produces a lock the next strict build rejects with `Resolved
'org.jetbrains.kotlinx:kotlinx-coroutines-…' which is not part of the dependency lock state`.
The excluded Kotlin stdlib modules are not ignored: they are in the graph, so they are locked
like everything else.

**To renew the lock, run the gate form:**

```bash
./gradlew --no-daemon --write-locks ktlintCheck test verifyPluginProjectConfiguration buildPlugin
```

That resolves every locked configuration and writes `gradle.lockfile` without pulling the four
IDE distributions, which is why it is also the form the CI step runs: it renews the lock and
then holds the result against the committed file. The header inside the lock file itself names
`:dependencies --write-locks` instead — that resolves every resolvable configuration of the
build, including the four IDE downloads, and writes the same lock for everything the gate
builds.

A version that moves in `build.gradle.kts` has to carry the renewed lock with it. GitHub's
changelog of 24 June 2025 is that Dependabot can read and update `gradle.lockfile` in addition
to `build.gradle`, by default and with no command named; when one of its version bumps arrives
without the renewed lock, the CI lock step is what says so.

**What is not locked.** The configurations of the `runIde`-family tasks are outside the lock,
so which IDE build a sandbox run downloads is not fixed by it; the OS-specific installer never
reaches the lock file in any case, which records the OS-less line `idea:idea:2026.1.3`.
`verifyPlugin` and publishing have configurations of their own that are not locked either.
Locking has been built and renewed on Linux and on macOS 15.5 (arm64, OpenJDK 21), where the
strict gate passes and `--write-locks` writes the same lock file byte for byte. Windows is still
untried: no Windows build has been made. The IDE distributions Plugin Verifier downloads are
outside the lock entirely, so a verifier run resolves those from the platform's own repository.

## Smoke-test status

Each `runIde`-family task launches a sandboxed IDE with the built plugin, so the tool window can be checked in that product by hand. These are manual runs, not tests, and none of them is wired into CI.

The on-screen behaviour is unverified: the panel, its streaming, its prompt cards, its Cancel button, the diff window, the settings page, the status bar and the notification actions are covered by the platform, JVM and descriptor tests above and by nothing else. A `runPyCharm` session against a live proxy, with settings opened, a token saved, an edit allowed from the notification and its diff opened, is the check that would close that gap. Four-way **binary** compatibility is checked by Plugin Verifier. Treat the UI as unverified until each run task has been smoke-tested.

## Kotlin style

`ktlint` is the Kotlin gate, and `extensions/jetbrains/.editorconfig` is the single source of the rules it applies — there is no baseline file and no rule configuration in the Gradle build. Lines are limited to 100 characters, matching the other languages in this repository.

Kotlin is intentionally **not** covered by `scripts/ci/code_health.py`, which scans the Go and Python trees only. Formatting is ktlint's concern; the function- and file-size rules in `docs/CODE_STYLE.md` are not mechanically enforced for this language yet.

## Layout

```
build.gradle.kts        # plugin module: IPGP, bundled Compose/Jewel, Ktor, Kotlin toolchain, ktlint
settings.gradle.kts     # root project
src/main/kotlin/        # Compose chat panel, the protocol layer, the proxy client, the session layer,
                        # the diff prediction and workspace checks, the project service,
                        # settings/token storage, the status bar, notifications and actions
src/main/resources/     # META-INF/plugin.xml, the light/dark plugin icons and the tool window's pair
src/test/kotlin/        # tool-window and icon tests, protocol/client suite, session and workspace suites,
                        # mock proxy
src/test/resources/     # the recorded real proxy turn
scripts/                # the release ZIP audit
.editorconfig           # ktlint rules (single source)
```

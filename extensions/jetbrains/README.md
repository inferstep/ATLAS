# ATLAS JetBrains Plugin

A JetBrains IDE client for the [ATLAS](https://github.com/inferstep/ATLAS) agent proxy — a thin UI layer wrapping `atlas-proxy`'s agent loop with no agent logic in the plugin itself.

**Status: Stage 2 — the ATLAS protocol/client layer.** Tracking [issue #35](https://github.com/inferstep/ATLAS/issues/35), one pull request per stage. Stage 1 proved a Jewel Compose panel can mount in the ATLAS tool window, follow the IDE theme and recompose incrementally as frames arrive. Stage 2 adds the Kotlin client for the atlas-proxy HTTP API and nothing else: the tool window still renders the Stage 1 stub, deliberately unwired from the client until sessions arrive. Chat, permissions, diffs and workspace integration are later stages.

## Spike results

| Question | Result |
|---|---|
| Compose panel mounts in a Swing tool window | Works, once `plugin.xml` depends on `com.intellij.modules.compose` |
| Incremental streaming recomposition | Works — each stub frame appends and recomposes |
| IDE theme bridging | Works via `SwingBridgeTheme` |
| Jewel Markdown rendering | Works, through the `intellij.platform.compose.markdown` content module (see below) |

`composeUI()` only adds a *compile-time* dependency. Without the matching `<depends>com.intellij.modules.compose</depends>` in the descriptor the tool window throws `NoClassDefFoundError: androidx/compose/ui/awt/ComposePanel` on a product that does not enable Compose for other plugins — PyCharm 2026.1 was the one that caught it, even though the class is in its distribution.

Jewel's Markdown renderer is reached by **module name, not by plugin id.** The `intellij.platform.jewel.markdown.*` modules ship in every 2026.1 product, but a `<depends>` takes a plugin id and these are content modules: `<depends>intellij.platform.jewel.markdown.core</depends>` names nothing and makes the platform refuse to load the plugin. The platform's own descriptor declares `intellij.platform.compose.markdown` with `visibility="public"` (in `lib/product-backend.jar` as `META-INF/plugin.xml` on IDEA 2026.1.3 and `META-INF/PythonPlugin.xml` on PyCharm 2026.1), and that one module pulls in Compose plus every Jewel Markdown module. So the descriptor says `<dependencies><module name="intellij.platform.compose.markdown"/></dependencies>`.

The build side names the modules too, because `bundledModule` attaches the jar of the module it is given and not that module's declared dependencies: the aggregator `intellij.platform.compose.markdown` carries no classes of its own, so `intellij.platform.jewel.markdown.core` and `intellij.platform.jewel.markdown.ideLafBridgeStyling` are declared for the compiler. `ProvideMarkdownStyling(project)` paints the Markdown with the IDE's theme, the way `SwingBridgeTheme` does for the surrounding Compose content.

`AtlasToolWindowFactoryTest` still guards the `<depends>` half: a `<depends>` on an `intellij.platform.jewel` module disables the plugin, and that is the mistake the test fails on.

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

## Tests

`./gradlew test` runs two suites in one JUnit Platform run: the existing `BasePlatformTestCase` tool-window test (via the vintage engine) and the Stage 2 protocol/client suite (Jupiter). The protocol suite needs no IDE and no proxy:

| Class | What it covers |
|---|---|
| `SseTest` | comments, chunk splits at every byte boundary, multi-byte characters, >1MB frames, malformed frames, CRLF, the `[DONE]` sentinel, the end-of-stream flush |
| `TypesTest` | wire names and modes, absent optionals staying absent, unknown fields and unknown event types, the documented `done.status` rule, the six-code error set |
| `RealStreamTest` | a recorded real proxy turn, replayed at chunk sizes 1, 7, 64 and 1024 |
| `ProtocolIsolationTest` | the layer imports nothing from `com.intellij` |
| `AtlasClientTest` | the client against an in-process HTTP fixture: streaming, the permission pause and its resume, cancel, 401 and non-JSON envelopes, a dropped connection, and every optional endpoint |

The fixture (`MockProxy`) is a real server on `com.sun.net.httpserver`, so the transport is exercised for real — which is also what makes a Ktor bump past the pin fail here rather than in an IDE.

## Requirements

* JDK 21 (the Gradle toolchain pins 21; nothing else is installed for you)
* IntelliJ Platform 2026.1 or newer (`sinceBuild = 261`)

The plugin is written in Kotlin 2.3.20 against the IntelliJ Platform 2026.1.3 SDK. Kotlin language and API levels are pinned to 2.3 because the IDE bundles the 2.3.x standard library; compiling against a newer level produces bytecode the platform cannot load.

The spike uses the Compose runtime and Jewel modules bundled with IntelliJ Platform 2026.1.3: Compose Multiplatform 1.10.0 and Jewel 0.37. `composeUI()` supplies the Compose modules (including the runtime split) and, transitively, the Jewel widgets the plugin uses, and `bundledModule(...)` supplies Jewel's Markdown modules; nothing is packaged into the plugin. The build-side declaration is only half of it, though: `plugin.xml` depends on `com.intellij.modules.compose` and names `intellij.platform.compose.markdown` as a module, and that is what puts the classes on the plugin's classloader at runtime. `SwingBridgeTheme` maps the active Swing Look and Feel into the Compose content.

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
```

The four run tasks download a full IDE on first use, which is large and slow. They exist so the plugin can be smoke-tested against each product rather than assuming IDEA compatibility.

## Smoke-test status

Each `runIde`-family task launches a sandboxed IDE with the built plugin, so the tool window can be checked in that product by hand. These are manual runs, not tests, and none of them is wired into CI.

| Run task | Product | Smoke-tested by hand |
|---|---|---|
| `runIde` | IntelliJ IDEA | no |
| `runPyCharm` | PyCharm 2026.1 | **yes** — `PY-261.22158.340` |
| `runWebStorm` | WebStorm 2026.1 | no |
| `runGoLand` | GoLand 2026.1 | no |

The PyCharm run is the one that reproduced the Stage 1 `NoClassDefFoundError` above, and the one that confirmed the Markdown route. With the tool window forced visible so its content factory actually runs, the plugin loads (`Loaded custom plugins: ATLAS` in the sandbox log, with no `has dependency on … which is not installed` line for it), the tool window mounts, Jewel's Markdown renderer composes the assistant text, and no exception names the plugin. The other three have not been launched — their sandboxes have no project or tool-window state, so the tool window was never exercised. Treat four-way compatibility as unverified until each run task has been smoke-tested.

## Kotlin style

`ktlint` is the Kotlin gate, and `extensions/jetbrains/.editorconfig` is the single source of the rules it applies — there is no baseline file and no rule configuration in the Gradle build. Lines are limited to 100 characters, matching the other languages in this repository.

Kotlin is intentionally **not** covered by `scripts/code_health.py`, which scans the Go and Python trees only. Formatting is ktlint's concern; the function- and file-size rules in `docs/CODE_STYLE.md` are not mechanically enforced for this language yet.

## Layout

```
build.gradle.kts        # plugin module: IPGP, bundled Compose/Jewel, Ktor, Kotlin toolchain, ktlint
settings.gradle.kts     # root project
src/main/kotlin/        # Compose tool window, the protocol layer, the proxy client
src/main/resources/     # META-INF/plugin.xml
src/test/kotlin/        # tool-window test, protocol/client suite, mock proxy
src/test/resources/     # the recorded real proxy turn
.editorconfig           # ktlint rules (single source)
```

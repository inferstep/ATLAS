import org.gradle.api.artifacts.dsl.LockMode
import org.jetbrains.intellij.platform.gradle.IntelliJPlatformType
import org.jetbrains.intellij.platform.gradle.TestFrameworkType
import org.jetbrains.intellij.platform.gradle.tasks.VerifyPluginTask
import org.jetbrains.kotlin.gradle.dsl.KotlinVersion

plugins {
    // 2026.1.3 bundles Kotlin 2.3.20 and Compose 1.10.0. Matching its
    // compiler avoids Compose inline/runtime ABI mismatches in the IDE.
    kotlin("jvm") version "2.3.20"
    kotlin("plugin.compose") version "2.3.20"
    // The protocol layer parses the proxy's SSE envelopes with
    // kotlinx.serialization; the compiler plugin version must match Kotlin.
    kotlin("plugin.serialization") version "2.3.20"
    id("org.jetbrains.intellij.platform") version "2.19.0"
    id("org.jlleitschuh.gradle.ktlint") version "14.2.0"
}

// Ktor is the HTTP transport stage 2 is built on.
//
// PINNED DELIBERATELY. Ktor 3.5.0 moved to kotlinx-coroutines 1.11.0, which
// hoisted Job.invokeOnCompletion$default onto the Job interface (JVM default
// methods). IntelliJ Platform 2026.1 bundles kotlinx-coroutines 1.10.2 (as
// 1.10.2-intellij-1) and that synthetic lives in Job$DefaultImpls, so Ktor
// 3.5+ throws NoSuchMethodError the first time it issues a request against the
// IDE's coroutines. 3.4.3 is the newest release built against 1.10.2 and is
// therefore the newest one that can run on the platform's coroutines.
//
// The mock-proxy integration tests in AtlasClientTest are the guard: they
// issue real requests, so a bump past 3.4.3 fails them rather than shipping.
// .github/dependabot.yml also ignores io.ktor for this tree.
val ktorVersion = "3.4.3"
val serializationVersion = "1.11.0"

// Which products `verifyPlugin` checks. The CI matrix sets one per job so a
// runner downloads two IDE distributions rather than eight; a release run
// leaves it unset and checks all four.
val verifierProducts =
    providers
        .gradleProperty("atlasVerifyProducts")
        .orNull
        ?.split(',')
        ?.map { it.trim().lowercase() }
        ?.filter { it.isNotEmpty() }
        ?: listOf("idea", "pycharm", "webstorm", "goland")

// The IDE already ships kotlinx-coroutines and the Kotlin stdlib and puts them
// on a plugin's classloader through com.intellij.modules.platform, so every
// Ktor coordinate excludes both -- and every companion artifact Ktor reaches
// them through, not just the -core one. A second copy inside the plugin jar
// shadows the platform's, which is how a plugin ends up with two
// incompatible coroutine runtimes.
val platformCoroutines =
    listOf(
        "org.jetbrains.kotlinx:kotlinx-coroutines-bom",
        "org.jetbrains.kotlinx:kotlinx-coroutines-core",
        "org.jetbrains.kotlinx:kotlinx-coroutines-core-jvm",
        "org.jetbrains.kotlinx:kotlinx-coroutines-jdk8",
        "org.jetbrains.kotlinx:kotlinx-coroutines-slf4j",
    )
val platformProvided =
    platformCoroutines +
        listOf(
            "org.jetbrains.kotlin:kotlin-stdlib",
            "org.jetbrains.kotlin:kotlin-stdlib-jdk7",
            "org.jetbrains.kotlin:kotlin-stdlib-jdk8",
        )

fun DependencyHandler.proxyClient(notation: String) {
    add("implementation", notation) {
        platformProvided.forEach { coordinate ->
            val (group, module) = coordinate.split(":", limit = 2)
            exclude(group = group, module = module)
        }
    }
}

group = "com.inferstep.atlas"
version = "0.1.0"

repositories {
    mavenCentral()
    intellijPlatform {
        defaultRepositories()
    }
}

dependencies {
    // The protocol/client layer is deliberately free of IntelliJ imports, so
    // Ktor plus kotlinx.serialization is its whole dependency set. Request
    // bodies are small and the SSE stream is parsed by hand (protocol/Sse.kt,
    // mirroring the VS Code client), so content negotiation and
    // ktor-client-sse are deliberately absent: ktor-serialization-kotlinx-json
    // alone drags in ktor-openapi-schema and with it kaml, snakeyaml and okio,
    // none of which belong in an IDE plugin.
    proxyClient("io.ktor:ktor-client-core:$ktorVersion")
    proxyClient("io.ktor:ktor-client-java:$ktorVersion")
    proxyClient("org.jetbrains.kotlinx:kotlinx-serialization-json:$serializationVersion")

    // BasePlatformTestCase is JUnit 3/4, and the protocol layer's tests are
    // Jupiter; both run under the JUnit Platform (the vintage engine carries
    // the platform test). Managed by the BOM so the engines cannot drift.
    testImplementation(platform("org.junit:junit-bom:5.13.4"))
    testImplementation("junit:junit:4.13.2")
    testImplementation("org.junit.jupiter:junit-jupiter")
    testRuntimeOnly("org.junit.platform:junit-platform-launcher")
    testRuntimeOnly("org.junit.vintage:junit-vintage-engine")

    // Deliberately no kotlinx-coroutines dependency here either: the test task
    // runs against the platform's own coroutines, exactly as the plugin does
    // in the IDE. Adding a second, newer copy would hide the very
    // incompatibility the Ktor pin above exists to avoid.

    intellijPlatform {
        intellijIdea("2026.1.3")
        // Compose and Jewel are provided by the target IDE. Never package a
        // second copy: 2026.1.3 ships Compose Multiplatform 1.10.0 and Jewel
        // 0.37, including the runtime split. composeUI() adds the Compose
        // modules and transitively the Jewel widgets the plugin uses. This is
        // compile time only — plugin.xml carries the matching runtime
        // <depends>, which is what the classloader actually honours.
        composeUI()
        // Jewel's Markdown renderer is a content module of the platform, not a
        // plugin, so it is reached by module name rather than by id: the
        // platform declares intellij.platform.compose.markdown with
        // visibility="public", and the descriptor names that module, which
        // brings the rest with it. <depends> cannot name it (that takes a
        // plugin id). For the build, each module whose classes are compiled
        // against has to be named: the aggregator alone carries no classes.
        bundledModule("intellij.platform.compose.markdown")
        bundledModule("intellij.platform.jewel.markdown.core")
        bundledModule("intellij.platform.jewel.markdown.ideLafBridgeStyling")
        testFramework(TestFrameworkType.Platform)

        // Pin the verifier CLI. Without a version the plugin resolves the
        // newest one, so the gate would quietly become a different check
        // after a release. 1.411 is the CLI this matrix was measured with.
        pluginVerifier("1.411")
    }
}

// Dependency locking. The platform the plugin compiles against, the coroutines
// it must share with the IDE and the Ktor pin above are all versions this build
// may not move by accident, so they are fixed in gradle.lockfile.
//
// Locking is turned on for the configurations the gate resolves and for no
// others, because strict mode fails a locked configuration that has no lock
// state. Measured on this tree: with every resolvable configuration locked,
// resolving `annotationProcessor` or `intellijPlatformLocal` -- neither of which
// the gate touches, and both of which some other task does -- fails with
// "Locking strict mode: Configuration ':annotationProcessor' is locked but does
// not have lock state." A wider scope therefore breaks the per-product run tasks
// and the verifier on a lock the gate has written, and a lock that only the
// `dependencies` task could complete is one that pulls four IDE distributions
// (see the README).
//
// The 26 names below are exactly the configurations
// `ktlintCheck test verifyPluginProjectConfiguration buildPlugin` resolves: the
// configurations named in gradle.lockfile, plus the seven it records as empty.
// A dependency or plugin that makes the gate resolve one more configuration
// has to add its name here, or that configuration is left out of the lock.
val gatedConfigurations =
    listOf(
        "compileClasspath",
        "runtimeClasspath",
        "testCompileClasspath",
        "testRuntimeClasspath",
        "ktlint",
        "ktlintReporter",
        "ktlintRuleset",
        "kotlinBuildToolsApiClasspath",
        "kotlinCompilerPluginClasspathMain",
        "kotlinCompilerPluginClasspathTest",
        "kotlinScriptDefExtensions",
        "testKotlinScriptDefExtensions",
        "intellijPlatformClasspath",
        "intellijPlatformComposedJar",
        "intellijPlatformDependency",
        "intellijPlatformJavaCompiler",
        "intellijPlatformSandboxRuntimeClasspath",
        "intellijPlatformTestClasspath",
        "intellijPlatformTestRuntimeClasspath",
        "intellijPlatformTestRuntimeFixClasspath",
        "intellijPlatformTestSandboxRuntimeClasspath",
        "intellijPlatformBackendPluginModuleDependencies",
        "intellijPlatformPlugin",
        "intellijPlatformPluginComposedModule",
        "intellijPlatformPluginModule",
        "jetbrainsRuntime",
    )

configurations.configureEach {
    if (name in gatedConfigurations) {
        resolutionStrategy.activateDependencyLocking()
    }
}

dependencyLocking {
    // Strict: a configuration named above that is resolved and has no line in
    // the lock file fails the build. A listed configuration that is never
    // resolved is not checked, and keeps the lines it has.
    lockMode = LockMode.STRICT
    // The five coroutines modules are excluded from every Ktor delivery above,
    // and resolving a configuration's files still resolves them. The strict
    // check then demands a lock line for each, which --write-locks never writes
    // because the modules are not in the dependency graph: without this, one
    // --write-locks produces a lock the next strict build rejects. Ignored
    // modules are left out of the lock file and out of the check, so they may
    // move; nothing else may.
    ignoredDependencies.addAll(platformCoroutines)
}

intellijPlatform {
    pluginConfiguration {
        ideaVersion {
            sinceBuild = "261"
        }
    }

    // Plugin Verifier: the built ZIP against the four products the plugin
    // claims, on the 2026.1 line it compiles against and on the current
    // 2026.2 line. Verifying is not the same question as loading in a sandbox:
    // it checks every class and API the plugin uses against each product's
    // binary surface, which is what the four `runIde`-family tasks cannot do
    // on a machine without a display.
    //
    // One product per invocation is how CI keeps four jobs small enough for a
    // runner's disk: `-PatlasVerifyProducts=pycharm` verifies PyCharm only.
    // With no property, every product is verified, which is the local release
    // check.
    pluginVerification {
        ides {
            if ("idea" in verifierProducts) {
                create(IntelliJPlatformType.IntellijIdea, "2026.1.5")
                create(IntelliJPlatformType.IntellijIdea, "2026.2.3")
            }
            if ("pycharm" in verifierProducts) {
                create(IntelliJPlatformType.PyCharm, "2026.1.5")
                create(IntelliJPlatformType.PyCharm, "2026.2.3")
            }
            if ("webstorm" in verifierProducts) {
                create(IntelliJPlatformType.WebStorm, "2026.1.5")
                create(IntelliJPlatformType.WebStorm, "2026.2.3")
            }
            if ("goland" in verifierProducts) {
                create(IntelliJPlatformType.GoLand, "2026.1.5")
                create(IntelliJPlatformType.GoLand, "2026.2.3")
            }
        }

        // Fail on anything the release should not ship, and on the warnings
        // that name a binary or dependency problem. Deprecated-API and
        // experimental-API notes are reported but do not fail: the verifier
        // reports them by design on a platform this size, and the release
        // checklist reads the report for them. An ignored problem needs a
        // reason in the commit or the report, never a blanket mute.
        failureLevel =
            listOf(
                VerifyPluginTask.FailureLevel.COMPATIBILITY_WARNINGS,
                VerifyPluginTask.FailureLevel.COMPATIBILITY_PROBLEMS,
                VerifyPluginTask.FailureLevel.INTERNAL_API_USAGES,
                VerifyPluginTask.FailureLevel.OVERRIDE_ONLY_API_USAGES,
                VerifyPluginTask.FailureLevel.NON_EXTENDABLE_API_USAGES,
                VerifyPluginTask.FailureLevel.SCHEDULED_FOR_REMOVAL_API_USAGES,
                VerifyPluginTask.FailureLevel.PLUGIN_STRUCTURE_WARNINGS,
                VerifyPluginTask.FailureLevel.MISSING_DEPENDENCIES,
                VerifyPluginTask.FailureLevel.INVALID_PLUGIN,
            )

        verificationReportsFormats =
            listOf(
                VerifyPluginTask.VerificationReportsFormats.PLAIN,
                VerifyPluginTask.VerificationReportsFormats.HTML,
                VerifyPluginTask.VerificationReportsFormats.MARKDOWN,
            )
    }
}

tasks.test {
    useJUnitPlatform()
}

// One run task per IDE target, so the plugin can be smoke-tested against
// each product rather than assuming IDEA compatibility. The built-in
// runIde covers IntelliJ IDEA itself.
intellijPlatformTesting {
    runIde {
        register("runPyCharm") {
            type = IntelliJPlatformType.PyCharm
            version = "2026.1"
        }
        register("runWebStorm") {
            type = IntelliJPlatformType.WebStorm
            version = "2026.1"
        }
        register("runGoLand") {
            type = IntelliJPlatformType.GoLand
            version = "2026.1"
        }
    }
}

kotlin {
    jvmToolchain(21)
    compilerOptions {
        languageVersion.set(KotlinVersion.KOTLIN_2_3)
        apiVersion.set(KotlinVersion.KOTLIN_2_3)
        // Kotlin's compatibility mode writes a delegating method into the
        // implementing class for every platform interface default, so a class
        // that overrides nothing still carries `getIcon`, `getAnchor`,
        // `manage` and the rest of ToolWindowFactory's defaults. The verifier
        // reads those copies as the plugin's own usages of experimental and
        // deprecated API. JVM default methods let the platform's own
        // implementation be the only one, and the toolchain is 21, so no
        // older consumer needs the bridges.
        freeCompilerArgs.add("-jvm-default=no-compatibility")
    }
}

// The ktlint engine is pinned rather than left to the plugin's default,
// so the rules the gate enforces don't drift under us; .editorconfig is
// the single source for the rule settings themselves.
ktlint {
    version.set("1.8.0")
}

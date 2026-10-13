import org.jetbrains.intellij.platform.gradle.IntelliJPlatformType
import org.jetbrains.intellij.platform.gradle.TestFrameworkType
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

// The IDE already ships kotlinx-coroutines and the Kotlin stdlib and puts them
// on a plugin's classloader through com.intellij.modules.platform, so every
// Ktor coordinate excludes both -- and every companion artifact Ktor reaches
// them through, not just the -core one. A second copy inside the plugin jar
// shadows the platform's, which is how a plugin ends up with two
// incompatible coroutine runtimes.
val platformProvided =
    listOf(
        "org.jetbrains.kotlinx:kotlinx-coroutines-bom",
        "org.jetbrains.kotlinx:kotlinx-coroutines-core",
        "org.jetbrains.kotlinx:kotlinx-coroutines-core-jvm",
        "org.jetbrains.kotlinx:kotlinx-coroutines-jdk8",
        "org.jetbrains.kotlinx:kotlinx-coroutines-slf4j",
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
    }
}

intellijPlatform {
    pluginConfiguration {
        ideaVersion {
            sinceBuild = "261"
        }
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
    }
}

// The ktlint engine is pinned rather than left to the plugin's default,
// so the rules the gate enforces don't drift under us; .editorconfig is
// the single source for the rule settings themselves.
ktlint {
    version.set("1.8.0")
}

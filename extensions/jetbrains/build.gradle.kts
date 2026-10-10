import org.jetbrains.intellij.platform.gradle.IntelliJPlatformType
import org.jetbrains.intellij.platform.gradle.TestFrameworkType
import org.jetbrains.kotlin.gradle.dsl.KotlinVersion

plugins {
    // 2026.1.3 bundles Kotlin 2.3.20 and Compose 1.10.0. Matching its
    // compiler avoids Compose inline/runtime ABI mismatches in the IDE.
    kotlin("jvm") version "2.4.20"
    kotlin("plugin.compose") version "2.4.20"
    id("org.jetbrains.intellij.platform") version "2.19.0"
    id("org.jlleitschuh.gradle.ktlint") version "14.2.0"
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
    testImplementation("junit:junit:4.13.2")

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

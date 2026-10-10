// The protocol/client layer's whole reason for existing is that it can be
// tested with plain JUnit, without launching an IDE. That only holds while it
// stays free of IntelliJ imports, and nothing else enforces it: the plugin
// module compiles the platform onto the same classpath, so a stray
// `import com.intellij.…` would build, pass review, and quietly take the layer
// hostage. This test is the enforcement.
package com.inferstep.atlas.protocol

import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.io.File

class ProtocolIsolationTest {
    @Test
    fun `the protocol and client layers import nothing from the IntelliJ platform`() {
        val sources = layerSources()

        // Guard against passing vacuously if the walk finds nothing.
        assertTrue(sources.size >= 3, "expected the layer's sources, found ${sources.size}")

        val offenders =
            sources.filter { file ->
                file.readLines().any { it.trimStart().startsWith("import com.intellij") }
            }

        assertTrue(
            offenders.isEmpty(),
            "these files import the IntelliJ platform and break the IDE-free layer: " +
                offenders.joinToString { it.name },
        )
    }

    @Test
    fun `the layer is where this test thinks it is`() {
        val sources = layerSources()

        assertTrue(
            sources.any { it.name == "Types.kt" } &&
                sources.any { it.name == "Sse.kt" } &&
                sources.any { it.name == "AtlasClient.kt" },
            "expected Types.kt, Sse.kt and AtlasClient.kt in $sources",
        )
    }

    /**
     * Every Kotlin file under the protocol and client packages. The module root
     * is found by walking up from the test's working directory, so the test
     * does not depend on how Gradle was invoked.
     */
    private fun layerSources(): List<File> {
        var directory = File("").absoluteFile
        var root = File(directory, MAIN_SOURCES)
        while (!root.isDirectory) {
            directory =
                directory.parentFile
                    ?: error("could not find $MAIN_SOURCES from ${File("").absolutePath}")
            root = File(directory, MAIN_SOURCES)
        }
        return root
            .walkTopDown()
            .filter { it.isFile && it.extension == "kt" }
            .filter { it.parentFile.name == "protocol" || it.parentFile.name == "client" }
            .toList()
    }

    private companion object {
        const val MAIN_SOURCES = "src/main/kotlin/com/inferstep/atlas"
    }
}

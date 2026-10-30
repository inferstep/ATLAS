// The IDE-free layers' whole reason for existing is that they can be tested
// with plain JUnit, without launching an IDE. That only holds while they stay
// free of IntelliJ imports, and nothing else enforces it: the plugin module
// compiles the platform onto the same classpath, so a stray
// `import com.intellij.…` would build, pass review, and quietly take the layer
// hostage. This test is the enforcement.
//
// Three packages are covered: protocol/, client/ and session/. The last is the
// one that would slip: the chat's turn, permission and transcript types are
// what a later stage is most tempted to wire to the IDE from.
package com.inferstep.atlas.protocol

import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.io.File

class ProtocolIsolationTest {
    @Test
    fun `the protocol, client and session layers import nothing from the IntelliJ platform`() {
        // The module root is found by walking up from the test's working
        // directory, so the test does not depend on how Gradle was invoked.
        val mainSources = "src/main/kotlin/com/inferstep/atlas"
        var directory = File("").absoluteFile
        while (!File(directory, mainSources).isDirectory) {
            directory =
                directory.parentFile
                    ?: error("could not find $mainSources from ${File("").absolutePath}")
        }
        val sources =
            File(directory, mainSources)
                .walkTopDown()
                .filter { it.isFile && it.extension == "kt" }
                .filter { it.parentFile.name in setOf("protocol", "client", "session") }
                .toList()

        // Guards against passing because the walk found nothing.
        val names = sources.map { it.name }
        val expected =
            listOf("Types.kt", "Sse.kt", "AtlasClient.kt", "TurnManager.kt", "ChatSession.kt")
        assertTrue(names.containsAll(expected), "expected $expected in $names")

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
}

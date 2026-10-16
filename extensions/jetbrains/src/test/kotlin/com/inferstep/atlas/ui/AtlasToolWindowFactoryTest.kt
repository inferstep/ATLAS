package com.inferstep.atlas.ui

import androidx.compose.ui.awt.ComposePanel
import com.intellij.openapi.wm.RegisterToolWindowTask
import com.intellij.openapi.wm.ToolWindowAnchor
import com.intellij.openapi.wm.ToolWindowManager
import com.intellij.testFramework.fixtures.BasePlatformTestCase
import java.awt.Component
import java.awt.Container

/**
 * Two guards for the Stage 1 tool window.
 *
 * Neither can replace the four `runIde`-family smoke runs: the platform test
 * framework only ever runs against the IDEA SDK, where the Compose modules
 * happen to be visible, so it cannot reproduce a product that does not put
 * them on this plugin's classloader. The descriptor test below is the part
 * that can, and it fails if the runtime dependency is dropped again.
 */
class AtlasToolWindowFactoryTest : BasePlatformTestCase() {
    fun testAtlasToolWindowMountsComposePanel() {
        val toolWindow =
            ToolWindowManager.getInstance(project).registerToolWindow(
                RegisterToolWindowTask("ATLAS", ToolWindowAnchor.RIGHT),
            )
        AtlasToolWindowFactory().createToolWindowContent(project, toolWindow)

        val contents = toolWindow.contentManager.contents
        assertEquals("ATLAS tool window must have one content tab", 1, contents.size)
        assertNotNull(
            "ATLAS tool window must mount a ComposePanel",
            findComposePanel(contents.single().component),
        )
    }

    /**
     * The spike threw NoClassDefFoundError on PyCharm 2026.1 because
     * `composeUI()` only adds a compile-time dependency. Requesting the
     * module in the descriptor is what puts Compose and Jewel on the plugin's
     * classloader at runtime, so the declaration is asserted here.
     */
    fun testDescriptorRequestsTheComposeModule() {
        val descriptor = readDescriptor()

        assertTrue(
            "plugin.xml must depend on $COMPOSE_MODULE, or the tool window " +
                "fails to load on a product that does not enable Compose for other plugins",
            descriptor.contains("<depends>$COMPOSE_MODULE</depends>"),
        )
    }

    /**
     * Jewel's Markdown modules ship in every 2026.1 product, but they are
     * platform *content modules*: a `<depends>` takes a plugin id, so one on
     * an `intellij.platform.jewel` module names nothing and makes the
     * platform refuse to load the plugin. They are reached through the
     * `intellij.platform.compose.markdown` module instead, which is a
     * `<dependencies><module>` entry and not a `<depends>`. Naming a Jewel
     * module in a `<depends>` again means someone took the id route and needs
     * to read the README note first.
     */
    fun testDescriptorDependsOnNoUnresolvableJewelModule() {
        val descriptor = readDescriptor()

        assertFalse(
            "a <depends> on an intellij.platform.jewel module disables the plugin; " +
                "only $COMPOSE_MODULE is plugin-visible",
            descriptor.contains("<depends>intellij.platform.jewel"),
        )
    }

    private fun readDescriptor(): String {
        val stream = javaClass.classLoader.getResourceAsStream(DESCRIPTOR_PATH)
        assertNotNull("$DESCRIPTOR_PATH must be on the test classpath", stream)
        return stream!!.use { it.readBytes().decodeToString() }
    }

    private fun findComposePanel(component: Component): ComposePanel? =
        when (component) {
            is ComposePanel -> component
            is Container -> component.components.firstNotNullOfOrNull(::findComposePanel)
            else -> null
        }

    private companion object {
        const val DESCRIPTOR_PATH = "META-INF/plugin.xml"
        const val COMPOSE_MODULE = "com.intellij.modules.compose"
    }
}

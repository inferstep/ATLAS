package com.inferstep.atlas.ui

import androidx.compose.ui.awt.ComposePanel
import com.inferstep.atlas.ide.AtlasProjectService
import com.intellij.openapi.actionSystem.ActionManager
import com.intellij.openapi.wm.RegisterToolWindowTask
import com.intellij.openapi.wm.ToolWindowAnchor
import com.intellij.openapi.wm.ToolWindowManager
import com.intellij.testFramework.fixtures.BasePlatformTestCase
import java.awt.Component
import java.awt.Container

/**
 * Guards for the tool window and the descriptor that puts it on the classpath.
 *
 * None of these can replace the four `runIde`-family smoke runs: the platform
 * test framework only ever runs against the IDEA SDK, where the Compose modules
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

    fun testDescriptorDeclaresTheComposeModuleAndTheStageFiveExtensions() {
        val stream = javaClass.classLoader.getResourceAsStream("META-INF/plugin.xml")
        assertNotNull("META-INF/plugin.xml must be on the test classpath", stream)
        val descriptor = stream!!.use { it.readBytes().decodeToString() }

        // The spike threw NoClassDefFoundError on PyCharm 2026.1 because
        // `composeUI()` only adds a compile-time dependency. Requesting the
        // module in the descriptor is what puts Compose and Jewel on the
        // plugin's classloader at runtime.
        assertTrue(
            "plugin.xml must depend on com.intellij.modules.compose, or the tool window " +
                "fails to load on a product that does not enable Compose for other plugins",
            descriptor.contains("<depends>com.intellij.modules.compose</depends>"),
        )

        // Jewel's Markdown modules ship in every 2026.1 product, but they are
        // platform *content modules*: a `<depends>` takes a plugin id, so one
        // on an `intellij.platform.jewel` module names nothing and makes the
        // platform refuse to load the plugin. They are reached through the
        // `intellij.platform.compose.markdown` module instead, which is a
        // `<dependencies><module>` entry and not a `<depends>`. Naming a Jewel
        // module in a `<depends>` again means someone took the id route and
        // needs to read the README note first.
        assertFalse(
            "a <depends> on an intellij.platform.jewel module disables the plugin; " +
                "only com.intellij.modules.compose is plugin-visible",
            descriptor.contains("<depends>intellij.platform.jewel"),
        )

        // Stage 5's user-facing surfaces are extensions, not code the tool
        // window mounts. A missing registration fails here rather than in a
        // smoke run. The actions are checked through ActionManager below.
        assertTrue(
            "settings must be an applicationConfigurable",
            descriptor.contains("com.inferstep.atlas.settings.AtlasConfigurable"),
        )
        assertTrue(
            "the status bar widget factory must be registered",
            descriptor.contains("com.inferstep.atlas.ui.AtlasStatusBarWidgetFactory"),
        )
        assertTrue(
            "notifications need the ATLAS group",
            descriptor.contains("<notificationGroup id=\"ATLAS\""),
        )
    }

    /**
     * Tool-window recreation: the content is thrown away and built again, as
     * the platform does when the window is closed and reopened. The
     * conversation lives on the project service, so the second mount must use
     * the same session rather than a fresh one — the property that keeps a
     * running turn and its history alive across a close.
     */
    fun testRecreatingTheContentKeepsTheProjectSession() {
        val toolWindow =
            ToolWindowManager.getInstance(project).registerToolWindow(
                RegisterToolWindowTask("ATLAS", ToolWindowAnchor.RIGHT),
            )
        val factory = AtlasToolWindowFactory()
        factory.createToolWindowContent(project, toolWindow)
        val session = AtlasProjectService.getInstance(project).session

        toolWindow.contentManager.removeAllContents(true)
        factory.createToolWindowContent(project, toolWindow)

        assertEquals(
            "the rebuilt window must have one content tab",
            1,
            toolWindow.contentManager.contents.size,
        )
        assertSame(
            "the rebuilt window must keep the project's session",
            session,
            AtlasProjectService.getInstance(project).session,
        )
    }

    /**
     * Every action is looked up through ActionManager, not merely grepped out
     * of the descriptor: a class the platform cannot instantiate is registered
     * on paper and still fails to open from the menu.
     */
    fun testEveryActionIsRegisteredWithThePlatform() {
        val manager = ActionManager.getInstance()
        val actions =
            listOf(
                "Atlas.OpenChatAction",
                "Atlas.NewConversationAction",
                "Atlas.CancelTurnAction",
                "Atlas.SetServiceTokenAction",
                "Atlas.RefreshStatusAction",
                "Atlas.OpenAtlasSettingsAction",
            )

        actions.forEach { id ->
            assertNotNull("$id must be a registered action", manager.getAction(id))
        }
    }

    private fun findComposePanel(component: Component): ComposePanel? =
        when (component) {
            is ComposePanel -> component
            is Container -> component.components.firstNotNullOfOrNull(::findComposePanel)
            else -> null
        }
}

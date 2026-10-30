package com.inferstep.atlas.ui

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNotNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import javax.xml.parsers.DocumentBuilderFactory

/**
 * The tool window's own button icon. Nothing else looks at it: the platform
 * takes it from the descriptor's `icon` attribute, and
 * `scripts/check-distribution.sh` audits only the two META-INF plugin icons, so
 * a missing or wrongly sized file would reach a release as the platform's
 * default square.
 *
 * What this cannot check is how the mark looks on the stripe — that is the
 * `runPyCharm` pass the README lists with the rest of the on-screen gaps.
 */
class AtlasToolWindowIconTest {
    @Test
    fun `the descriptor gives the ATLAS tool window an icon`() {
        val descriptor = resource("META-INF/plugin.xml").decodeToString()
        val toolWindow = Regex("<toolWindow\\b[^>]*>").find(descriptor)

        assertNotNull(toolWindow, "plugin.xml must declare a toolWindow")
        assertTrue(
            toolWindow!!.value.contains("id=\"ATLAS\""),
            "the tool window in the descriptor must be ATLAS: ${toolWindow.value}",
        )
        assertTrue(
            toolWindow.value.contains("icon=\"$LIGHT\""),
            "the tool window must draw its own icon: ${toolWindow.value}",
        )
    }

    @Test
    fun `both theme variants ship at the size the stripe draws`() {
        listOf(LIGHT, DARK).forEach { path ->
            val root =
                DocumentBuilderFactory
                    .newInstance()
                    .newDocumentBuilder()
                    .parse(resource(path).inputStream())
                    .documentElement

            assertEquals("13", root.getAttribute("width"), "$path width")
            assertEquals("13", root.getAttribute("height"), "$path height")
            assertEquals("0 0 13 13", root.getAttribute("viewBox"), "$path viewBox")
            val shapes =
                root.getElementsByTagName("rect").length +
                    root.getElementsByTagName("circle").length
            assertEquals(6, shapes, "$path must hold the mark's four steps, its stem and its dot")
        }
    }

    /**
     * The platform picks the dark file by the `_dark` suffix, so the pair has
     * to stay the same drawing: only the fill may differ, or one theme shows a
     * different mark from the other.
     */
    @Test
    fun `the two variants differ only in their colour`() {
        val light = resource(LIGHT).decodeToString()
        val dark = resource(DARK).decodeToString()

        assertTrue(light.contains("fill=\"#121212\""), "$LIGHT must be the light theme's mark")
        assertTrue(dark.contains("fill=\"#f7f7f4\""), "$DARK must be the dark theme's mark")
        assertEquals(
            withoutCommentsAndFill(light),
            withoutCommentsAndFill(dark),
            "the two files must draw the same mark",
        )
    }

    private fun withoutCommentsAndFill(svg: String): String {
        val withoutComments = svg.replace(Regex("<!--.*?-->", RegexOption.DOT_MATCHES_ALL), "")
        return withoutComments.replace(Regex("fill=\"#[0-9a-fA-F]{6}\""), "fill=\"\"").trim()
    }

    private fun resource(path: String): ByteArray {
        val stream = javaClass.classLoader.getResourceAsStream(path.removePrefix("/"))
        assertNotNull(stream, "$path must be on the classpath")
        return stream!!.use { it.readBytes() }
    }

    private companion object {
        const val LIGHT = "/icons/toolwindow.svg"
        const val DARK = "/icons/toolwindow_dark.svg"
    }
}

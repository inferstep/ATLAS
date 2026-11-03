package com.inferstep.atlas.ui

import com.intellij.testFramework.fixtures.BasePlatformTestCase

/**
 * The tooltip is HTML, so a verdict or gate label with markup in it must read
 * as the text the proxy sent. `XmlStringUtil` needs the platform, hence the
 * fixture rather than plain JUnit.
 */
class AtlasStatusBarTooltipTest : BasePlatformTestCase() {
    fun testTooltipEscapesProxyText() {
        assertEquals(
            "<html>ATLAS proxy: waiting on: &lt;lens&gt; &amp; sandbox<br>" +
                "lens: &lt;b&gt; · asa: ok<br>Click for actions.</html>",
            statusTooltip(
                detail = "waiting on: <lens> & sandbox",
                calibration = "lens: <b> · asa: ok",
                running = false,
            ),
        )
    }
}

package com.inferstep.atlas.ui

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Test

class AtlasNotifierTest {
    @Test
    fun `notification content escapes markup but keeps the request text`() {
        val raw =
            "ATLAS wants to run 'run_command'.\n" +
                "echo <tag> && echo 'a'"

        assertEquals(
            "ATLAS wants to run 'run_command'.\n" +
                "echo &lt;tag&gt; &amp;&amp; echo 'a'",
            notificationContent(raw),
        )
    }
}

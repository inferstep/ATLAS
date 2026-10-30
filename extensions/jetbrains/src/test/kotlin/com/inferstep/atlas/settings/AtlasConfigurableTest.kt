package com.inferstep.atlas.settings

import com.intellij.testFramework.fixtures.BasePlatformTestCase

/**
 * The settings page is mostly wiring, but the wiring is the part that can fail
 * before the user sees it: two application services, the credential store and
 * the Swing form. The test framework supplies the application and a memory
 * password safe, so this runs without an IDE window.
 */
class AtlasConfigurableTest : BasePlatformTestCase() {
    fun testSettingsPageBuildsAndResets() {
        val configurable = AtlasConfigurable()

        assertNotNull("the settings component must build", configurable.createComponent())
        assertFalse("a freshly reset page has no changes", configurable.isModified())
        assertEquals("ATLAS", configurable.displayName)

        configurable.disposeUIResources()
    }
}

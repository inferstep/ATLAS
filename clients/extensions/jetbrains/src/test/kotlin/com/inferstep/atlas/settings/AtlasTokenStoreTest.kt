package com.inferstep.atlas.settings

import com.intellij.testFramework.fixtures.BasePlatformTestCase

/**
 * The service token goes through PasswordSafe and nowhere else. The test
 * framework installs a memory credential store, so the round trip can be
 * exercised without touching the machine's keychain.
 */
class AtlasTokenStoreTest : BasePlatformTestCase() {
    fun testTokenRoundTripsAndABlankValueRemovesIt() {
        val store = AtlasTokenStore.getInstance()
        try {
            store.setToken("  s3cret  ")
            assertEquals("s3cret", store.token())

            store.setToken("   ")
            assertNull("a blank token must remove the stored value", store.token())

            store.setToken("next")
            assertEquals("next", store.token())
        } finally {
            store.setToken(null)
        }
    }
}

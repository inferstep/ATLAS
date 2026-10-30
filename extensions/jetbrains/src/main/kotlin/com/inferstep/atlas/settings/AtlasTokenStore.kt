// The service token, stored where the IDE keeps secrets.
//
// PasswordSafe delegates to the platform's credential store (Keychain, the
// Secret Service, the Windows credential manager, or the in-memory store in a
// test). The token is never written to atlas.xml, never logged, and never
// included in a connection's toString.
package com.inferstep.atlas.settings

import com.intellij.credentialStore.CredentialAttributes
import com.intellij.credentialStore.generateServiceName
import com.intellij.ide.passwordSafe.PasswordSafe
import com.intellij.openapi.components.Service
import com.intellij.openapi.components.service

@Service(Service.Level.APP)
class AtlasTokenStore {
    private val attributes =
        CredentialAttributes(generateServiceName(SERVICE_NAME, TOKEN_ACCOUNT))

    /** The stored token, or null when none is configured. */
    fun token(): String? =
        PasswordSafe.instance
            .getPassword(attributes)
            ?.takeIf { it.isNotEmpty() }

    /** Store [token]; null or blank removes the stored token. */
    fun setToken(token: String?) {
        if (token.isNullOrBlank()) {
            PasswordSafe.instance.set(attributes, null)
        } else {
            PasswordSafe.instance.setPassword(attributes, token.trim())
        }
    }

    companion object {
        fun getInstance(): AtlasTokenStore = service()

        private const val SERVICE_NAME = "ATLAS"
        private const val TOKEN_ACCOUNT = "service-token"
    }
}

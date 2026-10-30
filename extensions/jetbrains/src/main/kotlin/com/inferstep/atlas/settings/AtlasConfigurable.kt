package com.inferstep.atlas.settings

import com.inferstep.atlas.protocol.PermissionMode
import com.intellij.openapi.options.Configurable
import com.intellij.openapi.ui.ComboBox
import com.intellij.ui.JBIntSpinner
import com.intellij.ui.SimpleListCellRenderer
import com.intellij.ui.components.JBCheckBox
import com.intellij.ui.components.JBLabel
import com.intellij.ui.components.JBPasswordField
import com.intellij.ui.components.JBTextField
import com.intellij.util.ui.FormBuilder
import javax.swing.JComponent
import javax.swing.JList
import javax.swing.JPanel

/**
 * Settings → Tools → ATLAS.
 *
 * The token field is write-only by design: an existing secret is never read
 * back into the UI. Leaving the field empty keeps whatever is stored; the
 * checkbox removes it. The field and the checkbox therefore do not compete:
 * clearing wins if both are set.
 */
class AtlasConfigurable : Configurable {
    private val settings = AtlasSettings.getInstance()
    private val tokens = AtlasTokenStore.getInstance()

    private val urlField = JBTextField()
    private val modeBox = ComboBox(PermissionMode.entries.toTypedArray())
    private val pollSpinner =
        JBIntSpinner(DEFAULT_STATUS_POLL_SECONDS, MIN_STATUS_POLL_SECONDS, MAX_STATUS_POLL_SECONDS)
    private val tokenField = JBPasswordField()
    private val clearToken = JBCheckBox("Remove the stored service token")
    private val tokenStatus = JBLabel()
    private var panel: JPanel? = null

    private var initialUrl = ""
    private var initialMode = PermissionMode.DEFAULT
    private var initialPoll = DEFAULT_STATUS_POLL_SECONDS

    override fun getDisplayName(): String = "ATLAS"

    override fun getPreferredFocusedComponent(): JComponent = urlField

    override fun createComponent(): JComponent {
        // A subclass rather than SimpleListCellRenderer.create: the factory
        // methods are deprecated in 2026.2 (the Customizer overload as well as
        // the String one), while customize() is the class's own abstraction.
        modeBox.renderer =
            object : SimpleListCellRenderer<PermissionMode>() {
                override fun customize(
                    list: JList<out PermissionMode>,
                    value: PermissionMode?,
                    index: Int,
                    selected: Boolean,
                    hasFocus: Boolean,
                ) {
                    text = value?.wireName().orEmpty()
                }
            }
        tokenField.emptyText.text = "Leave empty to keep the stored token"
        clearToken.addActionListener {
            tokenField.isEnabled = !clearToken.isSelected
            if (clearToken.isSelected) {
                tokenField.text = ""
            }
        }

        val built =
            FormBuilder
                .createFormBuilder()
                .addLabeledComponent("Proxy URL:", urlField)
                .addLabeledComponent("Permission mode:", modeBox)
                .addLabeledComponent("Status bar poll interval (seconds):", pollSpinner)
                .addSeparator()
                .addLabeledComponent("New service token:", tokenField)
                .addComponent(clearToken)
                .addComponent(tokenStatus)
                .addComponentFillVertically(JPanel(), 0)
                .panel
        panel = built
        reset()
        return built
    }

    override fun isModified(): Boolean {
        if (panel == null) {
            return false
        }
        return urlField.text.trim() != initialUrl ||
            modeBox.selectedItem != initialMode ||
            pollSpinner.number != initialPoll ||
            tokenField.password.isNotEmpty() ||
            clearToken.isSelected
    }

    override fun apply() {
        settings.proxyUrl = urlField.text
        settings.permissionMode = modeBox.selectedItem as PermissionMode
        settings.statusBarPollIntervalSec = pollSpinner.number

        if (clearToken.isSelected) {
            tokens.setToken(null)
        } else if (tokenField.password.isNotEmpty()) {
            tokens.setToken(String(tokenField.password))
        }

        settings.notifyChanged()
        reset()
    }

    override fun reset() {
        initialUrl = settings.resolvedProxyUrl()
        initialMode = settings.permissionMode
        initialPoll = settings.statusBarPollIntervalSec
        urlField.text = initialUrl
        modeBox.selectedItem = initialMode
        pollSpinner.number = initialPoll
        tokenField.text = ""
        clearToken.isSelected = false
        tokenField.isEnabled = true
        tokenStatus.text =
            if (tokens.token().isNullOrEmpty()) {
                "No service token is stored. ATLAS_SERVICE_TOKEN_FILE is used when it is set; " +
                    "otherwise the proxy answers only when internal auth is disabled."
            } else {
                "A service token is stored in the IDE credential store."
            }
    }

    override fun disposeUIResources() {
        panel = null
        tokenField.text = ""
    }
}

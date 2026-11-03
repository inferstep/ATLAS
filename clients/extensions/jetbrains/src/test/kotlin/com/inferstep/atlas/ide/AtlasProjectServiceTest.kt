package com.inferstep.atlas.ide

import com.inferstep.atlas.protocol.PermissionMode
import com.inferstep.atlas.settings.AtlasSettings
import com.intellij.openapi.util.Disposer
import com.intellij.testFramework.fixtures.BasePlatformTestCase

/**
 * The project service is the seam the panel, the status bar and the actions
 * share, so the wiring that makes them one conversation is worth a test: a
 * second instance, or a session that ignores the stored permission mode,
 * would show up here before a runPyCharm session.
 */
class AtlasProjectServiceTest : BasePlatformTestCase() {
    fun testOneServiceAndOneSessionPerProject() {
        val service = AtlasProjectService.getInstance(project)

        assertSame(service, AtlasProjectService.getInstance(project))
        assertFalse(service.session.state.value.running)
        assertTrue(
            service.connection.value.url
                .isNotEmpty(),
        )
    }

    fun testANewServiceStartsItsSessionInTheStoredPermissionMode() {
        val settings = AtlasSettings.getInstance()
        val previous = settings.permissionMode
        // A stored mode that is not the session's own default, so a session
        // that ignored the setting would show it. The service is built here
        // rather than taken from the project, whose one instance may already
        // exist from an earlier test: the stored mode is read when a service
        // starts.
        settings.permissionMode = PermissionMode.ACCEPT_EDITS
        val service = AtlasProjectService(project)
        try {
            assertEquals(
                PermissionMode.ACCEPT_EDITS,
                service.session.state.value.permissionMode,
            )
        } finally {
            Disposer.dispose(service)
            settings.permissionMode = previous
        }
    }

    fun testSettingThePermissionModeWritesTheSettingAndTheSession() {
        val settings = AtlasSettings.getInstance()
        val previous = settings.permissionMode
        try {
            AtlasProjectService
                .getInstance(project)
                .setPermissionMode(PermissionMode.YOLO)

            assertEquals(PermissionMode.YOLO, settings.permissionMode)
            assertEquals(
                PermissionMode.YOLO,
                AtlasProjectService
                    .getInstance(project)
                    .session.state.value.permissionMode,
            )
        } finally {
            // The settings service is application-wide, so a test must not
            // leave a mode behind for the next one.
            settings.permissionMode = previous
            settings.notifyChanged()
        }
    }
}

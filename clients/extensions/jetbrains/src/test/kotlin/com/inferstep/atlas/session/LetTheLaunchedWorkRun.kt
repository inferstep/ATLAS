// The one wait the session suites use.
//
// These suites drive real coroutines: a turn runs on the scope the test opened,
// and the work that outlives it — a fire-and-forget permission decision POST,
// the `/cancel` a cancelled turn posts, the cleanup in a turn's `finally` — is
// launched on that same scope. A test that asserts on any of it has to let it
// run first.
//
// A sleep, not a virtual clock, because there is deliberately no coroutines
// test library here: the build declares no coroutines dependency of its own
// (see the Ktor pin in build.gradle.kts), so kotlinx-coroutines-test is not on
// the classpath and these tests run against the platform's own coroutines,
// exactly as the plugin does in the IDE. A second copy, for the tests, is the
// thing that pin exists to prevent.
package com.inferstep.atlas.session

import kotlinx.coroutines.delay

/** How long the launched work is given. Every wait in these suites is this one. */
private const val LAUNCHED_WORK_MS = 50L

/**
 * Give the work a test launched a chance to run: a turn reaching its first
 * suspension point, a decision POST, a `/cancel`, or a turn's cleanup.
 *
 * Not a synchronisation primitive — the flow does not await a decision POST, so
 * there is nothing to join on; this is the wait that stands in for that.
 *
 * The name is the wait, not the prompt: `PendingPermission.settle` already
 * means "answer this request", and a suite answers a prompt on the line before
 * it waits.
 */
internal suspend fun letTheLaunchedWorkRun() = delay(LAUNCHED_WORK_MS)

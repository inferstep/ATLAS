package main

import "testing"

func TestADeniedPermissionRowGivesTheReasonWhenNobodyWasAsked(t *testing.T) {
	m := sized(80, 30)
	m.appendChatEvent(mkChatEvent("permission_denied", map[string]string{"tool": "run_command"}))
	if got := m.chat[len(m.chat)-1].Body; got != "permission denied for run_command" {
		t.Errorf("a denial by the user reads %q, want the row it had before", got)
	}
	const reason = "delete_file: the object could not be held for approval on this platform. Nobody was asked, and nothing was deleted."
	m.appendChatEvent(mkChatEvent("permission_denied", map[string]string{"tool": "delete_file", "reason": reason}))
	if got := m.chat[len(m.chat)-1].Body; got != reason {
		t.Errorf("a refusal before asking reads %q, want the reason %q", got, reason)
	}
}

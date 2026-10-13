package main

import "strings"

// replyLeadIns are words between an announcing subject ("i will", "let me")
// and its verb that leave the announcement unchanged: "i will try to read it"
// defers the read exactly as "i will read it" does. A #273 proof session closed
// that way after failed reads, and the reply was reported completed.
var replyLeadIns = []string{"now ", "first ", "next ", "then ", "try to ", "attempt to ",
	"try and ", "go ahead and "}

// trimReplyLeadIns drops leading lead-ins, in any order and number.
func trimReplyLeadIns(s string) string {
	for changed := true; changed; {
		changed = false
		for _, l := range replyLeadIns {
			if strings.HasPrefix(s, l) {
				s, changed = s[len(l):], true
			}
		}
	}
	return s
}

package main

import (
	"encoding/json"
	"strings"
)

// blockedServerStart reports whether a failed verification command failed
// because it is a long-running process rather than because the code is
// broken: it never exited (the time limit stopped it) or it could not bind
// because something is already serving that port.
//
// The distinction decides what the verification gate says next. Treating it
// as a red test tells the model to fix its code and re-run the command, and
// re-running a blocking server start can never exit clean — an observed
// session started the server correctly with run_background, was told to
// "re-run the same command and confirm it exits clean", and spent its three
// remaining bounces re-sending `done` because nothing it could do satisfied
// that.
//
// That the command never exited is read from the result's own field. The
// executor sets it when it stops a command at the time limit, and a command
// that is stopped writes nothing about it. That a port is taken is read from
// what the command wrote: only the command knows it.
func blockedServerStart(result *ToolResult) bool {
	var out RunCommandOutput
	if json.Unmarshal(result.Data, &out) == nil && out.TimedOut {
		return true
	}
	low := strings.ToLower(result.Error + string(result.Data))
	return strings.Contains(low, "address already in use") ||
		strings.Contains(low, "is in use by another program")
}

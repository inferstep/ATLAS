// PC-062: Bubbletea TUI for ATLAS — main entry point.
//
// Connects to atlas-proxy /events (typed envelope SSE stream from
// PC-061) and renders the canonical chat UI: pipeline progress, event
// log, stats + chat input. The default `atlas` command launches this
// in interactive mode; pipe mode falls back to the built-in REPL.
//
// main reads the flags and internal/app runs the program with them. The
// Bubbletea model is in internal/app/model.go; pane rendering in
// internal/app/panes.go; chat/agent client and the /events SSE consumer both
// in internal/proxyclient.

package main

import (
	"flag"

	"github.com/itigges22/atlas-tui/internal/app"
	"github.com/itigges22/atlas-tui/internal/proxyclient"
)

const (
	defaultProxyURL = "http://localhost:8090"
)

func main() {
	proxyclient.InstallTokenTransport()
	proxyURL := flag.String("proxy", app.EnvOr("ATLAS_PROXY_URL", defaultProxyURL),
		"atlas-proxy base URL (default: $ATLAS_PROXY_URL or http://localhost:8090)")
	logPath := flag.String("log", app.EnvOr("ATLAS_TUI_LOG", ""),
		"append-only debug log path (default: off; alt-screen makes copy hard, "+
			"so tail this file to see what the TUI saw)")
	mouseFlag := flag.String("mouse", app.EnvOr("ATLAS_TUI_MOUSE", "on"),
		"mouse capture: 'on' (wheel scrolls chat) or 'off' (lets you select/copy text). "+
			"Mid-session toggle: /mouse on|off")
	demoMode := flag.String("demo", "",
		"launch the split-pane recording demo directly (short|medium|long). "+
			"Skips the main TUI; same as typing /demo from inside it.")
	continueFlag := flag.Bool("continue", false,
		"resume the most recent saved session for the current directory")
	resumeFlag := flag.Bool("resume", false,
		"resume a saved session: 'atlas --resume <id>' by id, or 'atlas --resume' "+
			"to pick from a list")
	flag.Parse()

	app.Run(*proxyURL, *logPath, *mouseFlag, *demoMode, *resumeFlag, *continueFlag)
}

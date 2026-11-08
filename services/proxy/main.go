// atlas-proxy: ATLAS's local inference proxy.
//
// Hosts the structured agent endpoint (`/v1/agent`), the typed event
// broker (`/events`), and the cancel hook (`/cancel`) that the TUI
// drives. Plain OpenAI traffic on `/v1/chat/completions` and unmatched
// paths are passed through to llama-server. The verify-repair pipeline
// (lens scoring + sandbox + V3 stages) lives behind the agent loop's
// `write_file` tool.
//
// main only starts the program. The program is in internal/app: app.Run
// starts the HTTP server, in internal/app/server.go.
//
// Usage:
//
//	atlas-proxy                  (default port 8090)
//	ATLAS_LLAMA_URL=http://localhost:8080 atlas-proxy
package main

import "github.com/itigges22/atlas-proxy/internal/app"

func main() {
	app.Run()
}

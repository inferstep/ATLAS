package sessionstore

type chatRole int

const (
	RoleUser chatRole = iota
	RoleAssistant
	RoleTool
	RoleSystem
)

// ChatMessage is one row in the chat history.
type ChatMessage struct {
	Role chatRole
	Body string
	// Meta — for tool: the tool name; for system: severity tag.
	Meta string
	// Success — only meaningful for tool rows. Drives the icon color.
	Success bool
	// Status and Reason — for a done row: the run's terminal status and
	// why (#236). Empty on every other row.
	Status string
	Reason string
	// Echo — the row mirrors raw slash/bash input for display only.
	// buildChatHistory skips echo rows so they never reach /v1/agent
	// as fake user turns.
	Echo bool
}

// Package llm is a small, provider-neutral chat interface with tool calling.
// The agent only depends on Client, so swapping the model backend (local
// Ollama today, a hosted API later) doesn't touch the investigation logic.
package llm

import "context"

// Message is one turn in a conversation.
type Message struct {
	Role      string     // "system", "user", "assistant" or "tool"
	Content   string     // text; for role "tool", the tool's output
	ToolCalls []ToolCall // set on assistant turns that request tools
	ToolName  string     // set on "tool" turns: which tool produced Content
}

// ToolCall is the model asking to run one tool.
type ToolCall struct {
	Name string
	Args map[string]any
}

// ToolSpec describes a tool the model may call. Parameters is a JSON Schema
// object.
type ToolSpec struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// Client sends a conversation and returns the model's next turn. With no
// tools, the model must answer in text.
type Client interface {
	Chat(ctx context.Context, messages []Message, tools []ToolSpec) (Message, error)
	Name() string
}

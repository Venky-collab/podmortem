package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Ollama talks to a local Ollama server (https://ollama.com), so the agent
// runs fully offline at no cost.
type Ollama struct {
	BaseURL string // e.g. http://localhost:11434
	Model   string // e.g. qwen2.5:1.5b
	HTTP    *http.Client
}

func NewOllama(baseURL, model string) *Ollama {
	return &Ollama{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Model:   model,
		HTTP:    &http.Client{Timeout: 5 * time.Minute}, // small CPUs are slow on long prompts
	}
}

func (o *Ollama) Name() string { return "ollama/" + o.Model }

type ollamaToolCall struct {
	Function struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"function"`
}

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolName  string           `json:"tool_name,omitempty"`
}

type ollamaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

func (o *Ollama) Chat(ctx context.Context, messages []Message, tools []ToolSpec) (Message, error) {
	req := map[string]any{
		"model":    o.Model,
		"messages": toOllamaMessages(messages),
		"stream":   false,
		// Low temperature: we want the same diagnosis for the same evidence.
		"options": map[string]any{"temperature": 0.1, "num_ctx": 8192},
	}
	if len(tools) > 0 {
		req["tools"] = toOllamaTools(tools)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Message{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return Message{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := o.HTTP.Do(httpReq)
	if err != nil {
		return Message{}, fmt.Errorf("ollama: %w (is `ollama serve` running?)", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Message{}, err
	}

	var out struct {
		Message ollamaMessage `json:"message"`
		Error   string        `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Message{}, fmt.Errorf("ollama: bad response (%s): %w", resp.Status, err)
	}
	if out.Error != "" || resp.StatusCode != http.StatusOK {
		return Message{}, fmt.Errorf("ollama: %s %s", resp.Status, out.Error)
	}

	msg := Message{Role: "assistant", Content: out.Message.Content}
	for _, tc := range out.Message.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{Name: tc.Function.Name, Args: tc.Function.Arguments})
	}
	return msg, nil
}

func toOllamaMessages(messages []Message) []ollamaMessage {
	out := make([]ollamaMessage, 0, len(messages))
	for _, m := range messages {
		om := ollamaMessage{Role: m.Role, Content: m.Content, ToolName: m.ToolName}
		for _, tc := range m.ToolCalls {
			var c ollamaToolCall
			c.Function.Name = tc.Name
			c.Function.Arguments = tc.Args
			om.ToolCalls = append(om.ToolCalls, c)
		}
		out = append(out, om)
	}
	return out
}

func toOllamaTools(tools []ToolSpec) []ollamaTool {
	out := make([]ollamaTool, 0, len(tools))
	for _, t := range tools {
		var ot ollamaTool
		ot.Type = "function"
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		ot.Function.Parameters = t.Parameters
		out = append(out, ot)
	}
	return out
}

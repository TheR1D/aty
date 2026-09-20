package llm

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Turn is one completed terminal command, or an answer discarded before run.
type Turn struct {
	Question string
	Answer   string
	Text     string
	Tool     bool
}

const (
	roleSystem    = "system"
	roleUser      = "user"
	roleAssistant = "assistant"
	roleTool      = "tool"
)

type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	// Responses output items, including opaque reasoning, must be replayed
	// intact during a tool loop. Chat Completions uses the fields above.
	responseItems []json.RawMessage
}

type chatRequest struct {
	Model           string
	Messages        []message
	Stream          bool
	Temperature     *float64
	Thinking        bool
	ReasoningEffort string
	Tools           []toolSpec
	ToolChoice      string
	PromptCacheKey  string
	MaxOutputTokens *int
}

func transcriptMessages(transcript []Turn) []message {
	messages := make([]message, 0, len(transcript)*3)
	for index, turn := range transcript {
		if question := strings.TrimSpace(turn.Question); question != "" {
			messages = append(messages, message{Role: roleUser, Content: question})
		}
		if turn.Tool {
			if call, result, ok := toolHistory(index, turn); ok {
				messages = append(messages, call, result)
			}
			continue
		}
		if answer := strings.TrimSpace(turn.Answer); answer != "" {
			messages = append(messages, message{Role: roleAssistant, Content: answer})
		}
		if strings.TrimSpace(turn.Text) != "" {
			messages = append(messages, message{Role: roleUser, Content: turn.Text})
		}
	}
	return messages
}

func toolHistory(index int, turn Turn) (message, message, bool) {
	command := strings.TrimSpace(turn.Answer)
	if command == "" {
		return message{}, message{}, false
	}
	id := fmt.Sprintf("call_%d", index)
	arguments, _ := json.Marshal(shellCommandArgs{Command: command})
	call := message{
		Role: roleAssistant,
		ToolCalls: []toolCall{{
			ID: id, Type: "function",
			Function: toolCallArgs{Name: toolSuggestShellCommand, Arguments: string(arguments)},
		}},
	}
	result := turn.Text
	if strings.TrimSpace(result) == "" {
		result = "the user cancelled the command before it ran"
	}
	return call, message{Role: roleTool, ToolCallID: id, Content: result}, true
}

func contextMessages(system string, transcript []Turn) []message {
	return append([]message{{Role: roleSystem, Content: system}}, transcriptMessages(transcript)...)
}

func questionMessages(system, question string, transcript []Turn) []message {
	return append(contextMessages(system, transcript), message{
		Role: roleUser, Content: strings.TrimSpace(question),
	})
}

func (c *Client) rememberAnswer(messages []message, command string) {
	out := append([]message(nil), messages...)
	if command != "" {
		out = append(out, message{Role: roleAssistant, Content: command})
	}
	c.remember(out)
}

func (c *Client) remember(messages []message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chat = append([]message(nil), messages...)
	c.chatAt = time.Now()
}

func (c *Client) chatSnapshot() ([]message, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]message(nil), c.chat...), c.chatAt
}

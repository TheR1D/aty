package llm

import "encoding/json"

const openAINativeName = "openai-native"

// NewOpenAINative uses OpenAI's Responses API. The endpoint is the full
// /v1/responses URL, just as compatible providers take a full completions URL.
func NewOpenAINative(opts Options) *Client {
	return newClient(opts, providerProtocol{
		name: openAINativeName, encode: encodeResponses,
		warm: openAICompatibleCacheRequest, nativeCache: true, responses: true,
	})
}

type responsesBody struct {
	Model           string              `json:"model"`
	Input           []any               `json:"input"`
	Stream          bool                `json:"stream,omitempty"`
	Store           bool                `json:"store"`
	Reasoning       *responsesReasoning `json:"reasoning,omitempty"`
	Temperature     *float64            `json:"temperature,omitempty"`
	Tools           []responsesTool     `json:"tools,omitempty"`
	ToolChoice      string              `json:"tool_choice,omitempty"`
	PromptCacheKey  string              `json:"prompt_cache_key,omitempty"`
	MaxOutputTokens *int                `json:"max_output_tokens,omitempty"`
}

type responsesReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type responsesTool struct {
	Type string `json:"type"`
	toolSchema
	// Keep the existing shell and MCP schemas' optional fields optional.
	Strict bool `json:"strict"`
}

type responsesMessage struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responsesFunctionCall struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type responsesFunctionOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

func encodeResponses(req chatRequest) ([]byte, error) {
	body := responsesBody{
		Model: req.Model, Input: responsesInput(req.Messages), Stream: req.Stream,
		Store:       false,
		Temperature: req.Temperature, ToolChoice: req.ToolChoice,
		PromptCacheKey: req.PromptCacheKey, MaxOutputTokens: req.MaxOutputTokens,
	}
	if req.ReasoningEffort != "" {
		body.Reasoning = &responsesReasoning{Effort: req.ReasoningEffort}
		if req.Thinking && req.ReasoningEffort != reasoningNone {
			body.Reasoning.Summary = "auto"
		}
	}
	for _, tool := range req.Tools {
		body.Tools = append(body.Tools, responsesTool{Type: "function", toolSchema: tool.Function})
	}
	return json.Marshal(body)
}

func responsesInput(messages []message) []any {
	items := make([]any, 0, len(messages))
	for _, msg := range messages {
		if msg.responseItems != nil {
			// Preserve every output item in order, including encrypted reasoning
			// and function-call IDs, before appending the tool results.
			for _, item := range msg.responseItems {
				items = append(items, item)
			}
			continue
		}
		if msg.Role == roleTool {
			items = append(items, responsesFunctionOutput{
				Type: "function_call_output", CallID: msg.ToolCallID, Output: msg.Content,
			})
			continue
		}
		if msg.Content != "" || len(msg.ToolCalls) == 0 {
			items = append(items, responsesMessage{Type: "message", Role: msg.Role, Content: msg.Content})
		}
		for _, call := range msg.ToolCalls {
			items = append(items, responsesFunctionCall{
				Type: "function_call", CallID: call.ID,
				Name: call.Function.Name, Arguments: call.Function.Arguments,
			})
		}
	}
	return items
}

package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// newResponsesStream decodes one Responses SSE stream. Output indexes include
// reasoning and messages, so function calls need separate, dense tool indexes.
func newResponsesStream() func(string) (streamPiece, error) {
	type pendingCall struct {
		index  int
		itemID string
	}
	calls := make(map[int]pendingCall)
	var streamedText strings.Builder
	return func(line string) (streamPiece, error) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			return streamPiece{}, nil
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			return streamPiece{}, fmt.Errorf("llm: OpenAI response ended without response.completed")
		}
		var event responseEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return streamPiece{}, fmt.Errorf("llm: reading the OpenAI response: %w", err)
		}
		if event.Error.Message != "" {
			return streamPiece{}, fmt.Errorf("llm: OpenAI: %s", event.Error.Message)
		}
		switch event.Type {
		case "":
			return streamPiece{}, fmt.Errorf("llm: OpenAI stream event has no type")
		case "response.output_text.delta":
			streamedText.WriteString(event.Delta)
			return streamPiece{content: event.Delta}, nil
		case "response.reasoning_summary_text.delta":
			return streamPiece{reasoning: event.Delta}, nil
		case "response.refusal.delta", "response.refusal.done":
			return streamPiece{}, responseRefusal(firstNonEmpty(event.Delta, event.Refusal))
		case "response.content_part.added", "response.content_part.done":
			if event.Part.Type == "refusal" {
				return streamPiece{}, responseRefusal(event.Part.Refusal)
			}
		case "response.output_item.added":
			if event.Item.Type != "function_call" {
				return streamPiece{}, nil
			}
			if event.OutputIndex == nil || *event.OutputIndex < 0 {
				return streamPiece{}, fmt.Errorf("llm: OpenAI function call has no valid output_index")
			}
			if _, exists := calls[*event.OutputIndex]; exists {
				return streamPiece{}, fmt.Errorf("llm: OpenAI repeated function call output_index %d", *event.OutputIndex)
			}
			if len(calls) >= maxToolCalls {
				return streamPiece{}, fmt.Errorf("llm: OpenAI response exceeds the limit of %d tool calls", maxToolCalls)
			}
			index := len(calls)
			calls[*event.OutputIndex] = pendingCall{index: index, itemID: event.Item.ID}
			call := event.Item.toolCall()
			call.Index = index
			return streamPiece{toolCalls: []toolCall{call}}, nil
		case "response.function_call_arguments.delta":
			if event.OutputIndex == nil {
				return streamPiece{}, fmt.Errorf("llm: OpenAI function arguments have no output_index")
			}
			call, exists := calls[*event.OutputIndex]
			if !exists {
				return streamPiece{}, fmt.Errorf("llm: OpenAI sent arguments for unknown function call output_index %d", *event.OutputIndex)
			}
			if event.ItemID != "" && call.itemID != "" && event.ItemID != call.itemID {
				return streamPiece{}, fmt.Errorf("llm: OpenAI function arguments have a mismatched item_id")
			}
			return streamPiece{toolCalls: []toolCall{{
				Index: call.index, Function: toolCallArgs{Arguments: event.Delta},
			}}}, nil
		case "response.completed":
			piece, err := completedResponse(event.Response)
			if err != nil {
				return streamPiece{}, err
			}
			if !strings.HasPrefix(piece.content, streamedText.String()) {
				return streamPiece{}, fmt.Errorf("llm: OpenAI completed text does not match the streamed answer")
			}
			piece.content = piece.content[streamedText.Len():]
			return piece, nil
		case "response.failed", "response.incomplete":
			detail := firstNonEmpty(event.Response.Error.Message, event.Response.IncompleteDetails.Reason, event.Response.Status)
			return streamPiece{}, fmt.Errorf("llm: OpenAI %s: %s", event.Type, firstNonEmpty(detail, "no details provided"))
		case "error":
			return streamPiece{}, fmt.Errorf("llm: OpenAI: %s", firstNonEmpty(event.Message, event.Code, "unknown streaming error"))
		}
		return streamPiece{}, nil
	}
}

// completedResponse uses the full output as the source of truth, preserving all
// items (including opaque reasoning) for the next agent request.
func completedResponse(response responseSnapshot) (streamPiece, error) {
	if response.Status != "completed" {
		return streamPiece{}, fmt.Errorf("llm: OpenAI response.completed has status %q", response.Status)
	}
	if response.Error.Message != "" {
		return streamPiece{}, fmt.Errorf("llm: OpenAI: %s", response.Error.Message)
	}
	if response.Output == nil {
		return streamPiece{}, fmt.Errorf("llm: OpenAI response.completed has no output array")
	}
	piece := streamPiece{done: true, finish: finishStop, responseItems: response.Output}
	var text strings.Builder
	ids := make(map[string]bool)
	for _, raw := range response.Output {
		var item responseOutputItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return streamPiece{}, fmt.Errorf("llm: reading an OpenAI output item: %w", err)
		}
		if item.Type == "" {
			return streamPiece{}, fmt.Errorf("llm: OpenAI output item has no type")
		}
		for _, part := range item.Content {
			if part.Type == "refusal" {
				return streamPiece{}, responseRefusal(part.Refusal)
			}
			if item.Type == "message" && part.Type == "output_text" {
				text.WriteString(part.Text)
			}
		}
		if item.Type != "function_call" {
			continue
		}
		if len(piece.finalToolCalls) >= maxToolCalls {
			return streamPiece{}, fmt.Errorf("llm: OpenAI response exceeds the limit of %d tool calls", maxToolCalls)
		}
		if strings.TrimSpace(item.CallID) == "" || strings.TrimSpace(item.Name) == "" {
			return streamPiece{}, fmt.Errorf("llm: OpenAI function call is missing call_id or name")
		}
		if ids[item.CallID] {
			return streamPiece{}, fmt.Errorf("llm: OpenAI repeated function call_id %q", item.CallID)
		}
		ids[item.CallID] = true
		if item.Status != "" && item.Status != "completed" {
			return streamPiece{}, fmt.Errorf("llm: OpenAI function call %s has status %q", item.Name, item.Status)
		}
		if !json.Valid([]byte(item.Arguments)) {
			return streamPiece{}, fmt.Errorf("llm: OpenAI function call %s arguments were not JSON", item.Name)
		}
		piece.finalToolCalls = append(piece.finalToolCalls, item.toolCall())
		piece.finish = finishToolCalls
	}
	piece.content = text.String()
	return piece, nil
}

func responseRefusal(reason string) error {
	if reason == "" {
		return fmt.Errorf("llm: OpenAI refused the request")
	}
	return fmt.Errorf("llm: OpenAI refused the request: %s", reason)
}

type responseEvent struct {
	Type        string             `json:"type"`
	Delta       string             `json:"delta"`
	Refusal     string             `json:"refusal"`
	OutputIndex *int               `json:"output_index"`
	ItemID      string             `json:"item_id"`
	Item        responseOutputItem `json:"item"`
	Part        responseContent    `json:"part"`
	Response    responseSnapshot   `json:"response"`
	Error       chatError          `json:"error"`
	Message     string             `json:"message"`
	Code        string             `json:"code"`
}

type responseSnapshot struct {
	Status            string            `json:"status"`
	Output            []json.RawMessage `json:"output"`
	Error             chatError         `json:"error"`
	IncompleteDetails struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}

type responseContent struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

type responseOutputItem struct {
	Type      string            `json:"type"`
	ID        string            `json:"id"`
	CallID    string            `json:"call_id"`
	Name      string            `json:"name"`
	Arguments string            `json:"arguments"`
	Status    string            `json:"status"`
	Content   []responseContent `json:"content"`
}

func (item responseOutputItem) toolCall() toolCall {
	return toolCall{
		ID: item.CallID, Type: "function",
		Function: toolCallArgs{Name: item.Name, Arguments: item.Arguments},
	}
}

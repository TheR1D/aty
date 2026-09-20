package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	finishStop      = "stop"
	finishToolCalls = "tool_calls"
)

var errStreamDone = errors.New("chat stream done")

// stream owns the HTTP response and SSE decoding for command and agent requests.
// Only a completed stream is drained for connection reuse; callback errors stop
// reading immediately so cancellation does not wait for the model.
func (c *Client) stream(ctx context.Context, request chatRequest, consume func(streamPiece) error) error {
	body, err := c.post(ctx, request)
	if err != nil {
		return err
	}
	reusable := false
	defer func() { closeHTTP(body, reusable) }()

	scanner := bufio.NewScanner(body)
	read := readStream
	if c.protocol.responses {
		read = newResponsesStream()
		// Completed events include all output, including encrypted reasoning.
		scanner.Buffer(make([]byte, 64<<10), 16<<20)
	}
	for scanner.Scan() {
		piece, err := read(scanner.Text())
		if errors.Is(err, errStreamDone) {
			reusable = true
			break
		}
		if err != nil {
			return err
		}
		if err := consume(piece); err != nil {
			return err
		}
		if piece.done {
			reusable = true
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("llm: the answer from %s was cut short: %w", c.model, ctx.Err())
		}
		return fmt.Errorf("llm: reading the answer from %s: %w", c.model, err)
	}
	if c.protocol.responses {
		return fmt.Errorf("llm: the answer from %s ended before response.completed: %w", c.model, io.ErrUnexpectedEOF)
	}
	reusable = true
	return nil
}

type streamPiece struct {
	content        string
	reasoning      string
	toolCalls      []toolCall
	finish         string
	done           bool
	responseItems  []json.RawMessage
	finalToolCalls []toolCall
}

// readStream decodes one SSE line. Non-data lines are keep-alives and ignored.
func readStream(line string) (streamPiece, error) {
	line = strings.TrimSpace(line)
	if line == "" || !strings.HasPrefix(line, "data:") {
		return streamPiece{}, nil
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "[DONE]" {
		return streamPiece{}, errStreamDone
	}

	var chunk chatChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return streamPiece{}, fmt.Errorf("llm: reading the answer: %w", err)
	}
	if chunk.Error.Message != "" {
		return streamPiece{}, fmt.Errorf("llm: %s", chunk.Error.Message)
	}
	if len(chunk.Choices) == 0 {
		return streamPiece{}, nil
	}
	choice := chunk.Choices[0]
	return streamPiece{
		content: choice.Delta.Content,
		reasoning: firstNonEmpty(
			choice.Delta.ReasoningContent,
			choice.Delta.Reasoning,
			choice.Delta.Thinking,
		),
		toolCalls: choice.Delta.ToolCalls,
		finish:    choice.FinishReason,
	}, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content          string     `json:"content"`
			ReasoningContent string     `json:"reasoning_content"`
			Reasoning        string     `json:"reasoning"`
			Thinking         string     `json:"thinking"`
			ToolCalls        []toolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error chatError `json:"error"`
}

type chatError struct {
	Message string `json:"message"`
}

type chatFailure struct {
	Error chatError `json:"error"`
}

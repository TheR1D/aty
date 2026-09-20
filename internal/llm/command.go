package llm

import (
	"context"
	"errors"
	"strings"
)

var ErrNoCommand = errors.New("llm: the model answered with nothing that could be typed at a shell")

func (c *Client) Command(
	ctx context.Context,
	question string,
	transcript []Turn,
	thinking bool,
	emit func(string) error,
	think func(string) error,
) error {
	messages := questionMessages(c.system, question, transcript)
	c.remember(messages)
	answer := streamedAnswer{emit: emit, think: think}
	defer func() { c.rememberAnswer(messages, answer.command) }()

	err := c.stream(ctx, c.ask(messages, true, thinking), func(piece streamPiece) error {
		if err := answer.addReasoning(piece.reasoning); err != nil {
			return err
		}
		return answer.addContent(piece.content)
	})
	if err != nil {
		return err
	}
	if answer.command == "" {
		return ErrNoCommand
	}
	return nil
}

// streamedAnswer keeps model text separate from reasoning and publishes complete
// revisions. Both command and agent mode use the same sanitization rules.
type streamedAnswer struct {
	content   strings.Builder
	reasoning strings.Builder
	command   string
	emit      func(string) error
	think     func(string) error
}

func (a *streamedAnswer) addReasoning(piece string) error {
	if piece == "" {
		return nil
	}
	a.reasoning.WriteString(piece)
	return a.think(a.reasoning.String())
}

func (a *streamedAnswer) addContent(piece string) error {
	if piece == "" {
		return nil
	}
	a.content.WriteString(piece)
	next := sanitize(a.content.String())
	if next == a.command {
		return nil
	}
	a.command = next
	return a.emit(next)
}

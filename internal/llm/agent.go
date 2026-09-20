package llm

import "context"

func (c *Client) Agent(
	ctx context.Context,
	question string,
	transcript []Turn,
	emit func(string) error,
	think func(string) error,
	run func(context.Context, string) (string, error),
	approve ApproveTool,
) error {
	messages := questionMessages(c.agent, question, transcript)
	c.remember(messages)
	var final string
	defer func() { c.rememberAnswer(messages, final) }()

	for step := 0; ; step++ {
		request := c.ask(messages, true, true)
		if step < c.agentSteps {
			request = withTools(request, c.availableTools())
		} else {
			request = withoutTools(request)
		}
		reply, answer, err := c.agentStep(ctx, request, emit, think)
		if err != nil {
			return err
		}
		if answer != "" {
			final = answer
		}
		if len(reply.ToolCalls) == 0 || step >= c.agentSteps {
			if final == "" {
				return ErrNoCommand
			}
			return nil
		}
		messages = append(messages, reply)
		c.remember(messages)
		for _, call := range reply.ToolCalls {
			result, err := c.runTool(ctx, call, run, approve)
			if err != nil {
				return err
			}
			messages = append(messages, message{
				Role: roleTool, ToolCallID: call.ID, Content: result,
			})
			c.remember(messages)
		}
	}
}

func (c *Client) agentStep(
	ctx context.Context,
	request chatRequest,
	emit func(string) error,
	think func(string) error,
) (message, string, error) {
	reply := message{Role: roleAssistant}
	var calls toolAccum
	var typed string
	answer := streamedAnswer{
		emit:  func(line string) error { return emit(reportLine(line)) },
		think: think,
	}

	err := c.stream(ctx, request, func(piece streamPiece) error {
		if piece.done {
			reply.responseItems = piece.responseItems
			reply.ToolCalls = piece.finalToolCalls
		}
		if err := calls.add(piece.toolCalls); err != nil {
			return err
		}
		if err := answer.addReasoning(piece.reasoning); err != nil {
			return err
		}
		if len(calls.buf) > 0 && calls.buf[0].Function.Name == toolSuggestShellCommand {
			prefix := toolCommandPrefix(calls.buf[0].Function.Arguments)
			if prefix != "" && prefix != typed {
				typed = prefix
				if err := emit(prefix); err != nil {
					return err
				}
			}
		}
		return answer.addContent(piece.content)
	})
	if err != nil {
		return message{}, "", err
	}
	if !c.protocol.responses {
		reply.ToolCalls = calls.calls()
	}
	return reply, reportLine(answer.command), nil
}

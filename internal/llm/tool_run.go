package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/TheR1D/aty/internal/helpers"
	"strings"
)

func (c *Client) runTool(
	ctx context.Context,
	call toolCall,
	run func(context.Context, string) (string, error),
	approve ApproveTool,
) (string, error) {
	name := call.Function.Name
	if name != "" && name != toolSuggestShellCommand && !c.hasTool(name) {
		return fmt.Sprintf("llm: the model called %s, which is not a tool it was given", name), nil
	}
	return runToolCall(ctx, call, run, c.callTool, approve, c.toolOutputBytes)
}

func (c *Client) hasTool(name string) bool {
	for _, tool := range c.tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

// runToolCall returns validation and execution failures as model-visible results
// so the next step can recover. Cancellation and approval failures stop the query.
func runToolCall(
	ctx context.Context,
	call toolCall,
	run func(context.Context, string) (string, error),
	external ToolCaller,
	approve ApproveTool,
	outputLimit int,
) (string, error) {
	if call.Function.Name == "" || call.Function.Name == toolSuggestShellCommand {
		cmd, err := toolCommand(call)
		if err != nil {
			return err.Error(), nil
		}
		out, err := run(ctx, cmd)
		if err != nil {
			if ctx.Err() != nil {
				return "", err
			}
			return fmt.Sprintf("the command failed: %s", err), nil
		}
		return toolResult(out, outputLimit), nil
	}

	arguments := json.RawMessage(call.Function.Arguments)
	var object map[string]any
	if err := json.Unmarshal(arguments, &object); err != nil || object == nil {
		if err == nil {
			err = fmt.Errorf("value is not an object")
		}
		return fmt.Sprintf("llm: %s arguments were not a JSON object: %s", call.Function.Name, err), nil
	}
	if external == nil {
		return fmt.Sprintf("llm: the model called %s, but no external tool runner is available", call.Function.Name), nil
	}
	if approve == nil {
		return fmt.Sprintf("llm: the model called %s, but user approval is unavailable", call.Function.Name), nil
	}
	if err := approve(ctx, call.Function.Name, arguments); err != nil {
		return "", err
	}
	out, err := external(ctx, call.Function.Name, arguments)
	if err != nil {
		if ctx.Err() != nil {
			return "", err
		}
		return fmt.Sprintf("the MCP tool failed: %s", err), nil
	}
	return toolResult(out, outputLimit), nil
}

// toolResult bounds successful tool output and makes an empty result explicit.
func toolResult(output string, limit int) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return "the command printed nothing"
	}
	if len(output) <= limit {
		return output
	}
	return helpers.TruncatedOutput(output, output, limit)
}

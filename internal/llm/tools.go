package llm

import (
	"context"
	"encoding/json"
)

const (
	// toolSuggestShellCommand is the built-in tool that runs commands through
	// the user's terminal. Configured MCP tools are added beside it.
	toolSuggestShellCommand        = "suggest_shell_command"
	suggestShellCommandDescription = "Type one shell command into the user's terminal for them to confirm. They may edit it before it runs. You get what it printed. The command really runs, in the user's shell and working directory, so this is how the work gets done. It must be non-interactive and must exit on its own."

	toolChoiceAuto = "auto"
	toolChoiceNone = "none"
)

// Tool is an external function made available to an agent question.
type Tool struct {
	Name        string
	Description string
	Parameters  any
}

// ToolCaller invokes an external tool by its model-facing name.
type ToolCaller func(context.Context, string, json.RawMessage) (string, error)

// ApproveTool asks the user before an external tool is invoked.
type ApproveTool func(context.Context, string, json.RawMessage) error

// suggestShellCommandTool is the built-in tool advertised beside external MCP tools.
var suggestShellCommandTool = toolSpec{
	Type: "function",
	Function: toolSchema{
		Name:        toolSuggestShellCommand,
		Description: suggestShellCommandDescription,
		Parameters: objectSchema{
			Type: "object",
			Properties: map[string]stringSchema{
				"command": {
					Type:        "string",
					Description: "The shell command to run.",
				},
			},
			Required: []string{"command"},
		},
	},
}

type toolSpec struct {
	Type     string     `json:"type"`
	Function toolSchema `json:"function"`
}

type toolSchema struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

type objectSchema struct {
	Type       string                  `json:"type"`
	Properties map[string]stringSchema `json:"properties"`
	Required   []string                `json:"required"`
}

type stringSchema struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

// withTools lets the model call an available tool or answer in text.
func withTools(req chatRequest, available []toolSpec) chatRequest {
	req.Tools = available
	req.ToolChoice = toolChoiceAuto
	return req
}

// buildToolSpecs combines the built-in shell tool with configured MCP tools.
func buildToolSpecs(external []Tool) []toolSpec {
	tools := []toolSpec{suggestShellCommandTool}
	for _, tool := range external {
		parameters := tool.Parameters
		if parameters == nil {
			parameters = map[string]any{"type": "object"}
		}
		tools = append(tools, toolSpec{
			Type: "function",
			Function: toolSchema{
				Name: tool.Name, Description: tool.Description, Parameters: parameters,
			},
		})
	}
	return tools
}

func (c *Client) availableTools() []toolSpec {
	return buildToolSpecs(c.tools)
}

// formatTools renders the agent's tool list for the context dump.
func formatTools(tools []toolSpec) string {
	raw, err := json.MarshalIndent(tools, "", "  ")
	if err != nil {
		return ""
	}
	return string(raw) + "\n"
}

// withoutTools asks for a final report after the agent reaches its step limit.
func withoutTools(req chatRequest) chatRequest {
	req.Tools = nil
	req.ToolChoice = toolChoiceNone
	return req
}

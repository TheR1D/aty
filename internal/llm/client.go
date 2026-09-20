// Package llm turns terminal questions into streamed shell commands.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	appconfig "github.com/TheR1D/aty/internal/config"
)

type Options struct {
	ToolOutputBytes int // Zero uses the default result size.
	AgentSteps      int // Zero uses the default number of tool rounds.
	Endpoint        string
	Model           string
	APIKey          string
	ReasoningEffort string
	Temperature     *float64
	SystemPrompt    string
	AgentPrompt     string
	HTTP            *http.Client
	Tools           []Tool     // External tools advertised beside suggest_shell_command.
	CallTool        ToolCaller // Invokes one of Tools by name.
}

type Client struct {
	toolOutputBytes int
	agentSteps      int
	http            *http.Client
	endpoint        string
	model           string
	apiKey          string
	system          string
	agent           string
	protocol        providerProtocol
	reasoningEffort string
	temperature     *float64
	tools           []Tool
	callTool        ToolCaller

	cacheKey atomic.Pointer[string]

	mu     sync.Mutex
	chat   []message
	chatAt time.Time
}

// New creates a llama.cpp client. Prefer NewLlama in new code.
func New(opts Options) *Client {
	return NewLlama(opts)
}

func newClient(opts Options, protocol providerProtocol) *Client {
	if opts.ToolOutputBytes <= 0 {
		opts.ToolOutputBytes = appconfig.DefaultToolOutputBytes
	}
	if opts.AgentSteps <= 0 {
		opts.AgentSteps = appconfig.DefaultAgentSteps
	}
	client := &Client{
		toolOutputBytes: opts.ToolOutputBytes, agentSteps: opts.AgentSteps,
		http: opts.HTTP, endpoint: strings.TrimSuffix(opts.Endpoint, "/"),
		model: opts.Model, apiKey: strings.TrimSpace(opts.APIKey),
		system: opts.SystemPrompt, agent: opts.AgentPrompt,
		protocol: protocol, reasoningEffort: strings.TrimSpace(opts.ReasoningEffort),
		temperature: opts.Temperature, tools: append([]Tool(nil), opts.Tools...),
		callTool: opts.CallTool,
	}
	if client.http == nil {
		client.http = NewHTTPClient()
	}
	if client.system == "" || client.agent == "" {
		defaults := bundledPrompts()
		if client.system == "" {
			client.system = defaults.System
		}
		if client.agent == "" {
			client.agent = defaults.Agent
		}
	}
	if protocol.nativeCache {
		client.rotatePromptCacheKey()
	}
	return client
}

func (c *Client) Name() string     { return c.protocol.name }
func (c *Client) Model() string    { return c.model }
func (c *Client) Endpoint() string { return c.endpoint }

// SetPS1 binds the startup prompt before any request or prewarm starts.
func (c *Client) SetPS1(ps1 string) {
	prompts := (Prompts{System: c.system, Agent: c.agent}).WithPS1(ps1)
	c.system, c.agent = prompts.System, prompts.Agent
}

func (c *Client) Version(ctx context.Context) (string, error) {
	_, err := c.get(ctx)
	return "", err
}

func (c *Client) Models(ctx context.Context) ([]string, error) {
	body, err := c.get(ctx)
	if err != nil {
		return nil, err
	}
	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("llm: %s did not list models: %w", c.endpoint, err)
	}
	names := make([]string, 0, len(response.Data))
	for _, model := range response.Data {
		if model.ID != "" {
			names = append(names, model.ID)
		}
	}
	return names, nil
}

func Installed(name string, listed []string) bool {
	name = strings.TrimSpace(name)
	return name != "" && slices.Contains(listed, name)
}

func (c *Client) ask(messages []message, stream, thinking bool) chatRequest {
	return chatRequest{
		Model: c.model, Messages: messages, Stream: stream,
		Temperature: c.temperature, Thinking: thinking,
		ReasoningEffort: c.effort(thinking),
		PromptCacheKey:  c.promptCacheKey(),
	}
}

func (c *Client) effort(thinking bool) string {
	if c.reasoningEffort != "" && !thinking && !c.protocol.responses {
		return reasoningNone
	}
	return c.reasoningEffort
}

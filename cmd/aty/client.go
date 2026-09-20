package main

import (
	"fmt"
	"net/http"

	appconfig "github.com/TheR1D/aty/internal/config"
	"github.com/TheR1D/aty/internal/helpers"
	"github.com/TheR1D/aty/internal/llm"
	appmcp "github.com/TheR1D/aty/internal/mcp"
)

func clients(settings appconfig.Settings, prompts llm.Prompts, registry *appmcp.Registry) (llm.Asker, llm.Asker, error) {
	httpClient := llm.NewHTTPClient()
	ask, err := client(settings.Default, prompts, registry, httpClient, settings.Limits)
	if err != nil {
		return nil, nil, err
	}
	if appconfig.Equivalent(settings.Default, settings.Think) {
		return ask, ask, nil
	}
	think, err := client(settings.Think, prompts, registry, httpClient, settings.Limits)
	return ask, think, err
}

func client(backend appconfig.Backend, prompts llm.Prompts, registry *appmcp.Registry, httpClient *http.Client, limits appconfig.Limits) (llm.Asker, error) {
	tools, callTool := mcpTools(registry)
	opts := llm.Options{
		ToolOutputBytes: limits.ToolOutputBytes,
		AgentSteps:      limits.AgentSteps,
		HTTP:            httpClient,
		Model:           backend.Model,
		Endpoint:        backend.Endpoint,
		APIKey:          helpers.Value(backend.APIKey),
		ReasoningEffort: helpers.Value(backend.ReasoningEffort),
		Temperature:     backend.Temperature,
		SystemPrompt:    prompts.System,
		AgentPrompt:     prompts.Agent,
		Tools:           tools,
		CallTool:        callTool,
	}
	switch backend.Provider {
	case appconfig.Llama:
		return llm.NewLlama(opts), nil
	case appconfig.Ollama:
		return llm.NewOllama(opts), nil
	case appconfig.OpenAICompatible:
		return llm.NewOpenAICompatible(opts), nil
	case appconfig.OpenAINative:
		return llm.NewOpenAINative(opts), nil
	default:
		return nil, fmt.Errorf("aty: unsupported provider %q", backend.Provider)
	}
}

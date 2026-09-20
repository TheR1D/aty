package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/TheR1D/aty/internal/llm"
	appmcp "github.com/TheR1D/aty/internal/mcp"
)

type mcpLoader func(context.Context, string, string) (*appmcp.Registry, error)

func loadMCP(ctx context.Context, path, workspace string) (*appmcp.Registry, error) {
	config, err := appmcp.LoadOrCreate(path, workspace)
	if err != nil {
		return nil, err
	}
	return appmcp.Open(ctx, config), nil
}

func startMCP(loader mcpLoader, stderr io.Writer) (*appmcp.Registry, error) {
	workspace, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("finding working directory: %w", err)
	}
	registry, err := loader(context.Background(), mcpConfigPath(), workspace)
	if err != nil {
		return nil, err
	}
	writeMCPStartupFailures(stderr, registry)
	return registry, nil
}

func mcpTools(registry *appmcp.Registry) ([]llm.Tool, llm.ToolCaller) {
	if registry == nil {
		return nil, nil
	}
	var tools []llm.Tool
	for _, tool := range registry.Tools() {
		description := tool.Description
		if description == "" {
			description = fmt.Sprintf("Call %s from the %s MCP server.", tool.Original, tool.Server)
		}
		tools = append(tools, llm.Tool{
			Name: tool.Name, Description: description, Parameters: tool.InputSchema,
		})
	}
	return tools, registry.Call
}

func writeMCPStartupFailures(out io.Writer, registry *appmcp.Registry) {
	if registry == nil {
		return
	}
	for _, status := range registry.Statuses() {
		if status.Err != nil {
			_, _ = fmt.Fprintf(out, "aty: mcp %s unavailable: %s\n", status.Name, status.Err)
		}
	}
}

func writeMCPStatuses(out io.Writer, registry *appmcp.Registry) {
	if registry == nil {
		return
	}
	for _, status := range registry.Statuses() {
		if status.Err != nil {
			_, _ = fmt.Fprintf(out, "mcp     fail  %s (%s): %s\n", status.Name, status.Transport, status.Err)
		} else {
			_, _ = fmt.Fprintf(out, "mcp     ok    %s (%s, %d tools)\n", status.Name, status.Transport, status.Tools)
		}
	}
}

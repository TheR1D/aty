package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Status describes one configured server after connection.
type Status struct {
	Name      string
	Transport string
	Tools     int
	Err       error
}

type registeredTool struct {
	serverName string
	toolName   string
	server     Server
	session    *sdk.ClientSession
}

// Registry owns all MCP sessions and routes namespaced tool calls. Discovery
// finishes before Open returns, so tools, routes, and statuses are immutable.
type Registry struct {
	tools    []Tool
	routes   map[string]registeredTool
	statuses []Status

	closeMu  sync.Mutex
	sessions []*sdk.ClientSession
}

// Open connects configured servers concurrently, then registers their tools
// in config-name order. A failed server does not disable the others.
// TODO: Connect servers in the background so MCP startup never delays the terminal.
func Open(ctx context.Context, config Config) *Registry {
	registry := &Registry{routes: make(map[string]registeredTool)}
	names := slices.Sorted(maps.Keys(config.Servers))

	results := make([]serverResult, len(names))
	var wait sync.WaitGroup
	for index, name := range names {
		server := config.Servers[name]
		wait.Go(func() {
			results[index] = openServer(ctx, config.Workspace, server)
		})
	}
	wait.Wait()

	for index, name := range names {
		registry.registerServer(name, config.Servers[name], results[index])
	}
	return registry
}

// registerServer runs only during Open, before the registry is shared. Keeping
// registration serial makes tool names and diagnostic output deterministic.
func (registry *Registry) registerServer(name string, server Server, result serverResult) {
	status := Status{Name: name, Transport: server.Type}
	if result.err != nil {
		status.Err = redactConnectError(result.err, server)
		registry.statuses = append(registry.statuses, status)
		return
	}
	registry.sessions = append(registry.sessions, result.session)
	for _, tool := range result.tools {
		if tool == nil || strings.TrimSpace(tool.Name) == "" {
			continue
		}
		alias := registry.uniqueAlias(name, tool.Name)
		registry.tools = append(registry.tools, Tool{
			Name: alias, Server: name, Original: tool.Name,
			Description: tool.Description, InputSchema: tool.InputSchema,
		})
		registry.routes[alias] = registeredTool{
			serverName: name, toolName: tool.Name, server: server, session: result.session,
		}
		status.Tools++
	}
	registry.statuses = append(registry.statuses, status)
}

// Tools returns a stable copy of all tools from connected servers.
func (registry *Registry) Tools() []Tool {
	return append([]Tool(nil), registry.tools...)
}

// Statuses returns one status for every configured server.
func (registry *Registry) Statuses() []Status {
	return append([]Status(nil), registry.statuses...)
}

// Call invokes a discovered tool by its model-facing alias.
func (registry *Registry) Call(ctx context.Context, name string, arguments json.RawMessage) (string, error) {
	route, ok := registry.routes[name]
	if !ok {
		return "", fmt.Errorf("mcp: unknown tool %q", name)
	}

	args := make(map[string]any)
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &args); err != nil {
			return "", fmt.Errorf("mcp: arguments for %s are not a JSON object: %w", name, err)
		}
	}
	result, err := route.session.CallTool(ctx, &sdk.CallToolParams{
		Name: route.toolName, Arguments: args,
	})
	if err != nil {
		safe := redactConnectError(err, route.server)
		return "", fmt.Errorf("mcp: calling %s on %s: %w", route.toolName, route.serverName, safe)
	}
	return formatResult(result), nil
}

// Close ends every connected MCP session.
func (registry *Registry) Close() error {
	registry.closeMu.Lock()
	sessions := registry.sessions
	registry.sessions = nil
	registry.closeMu.Unlock()

	errs := make([]error, len(sessions))
	var wait sync.WaitGroup
	for index, session := range sessions {
		wait.Go(func() {
			errs[index] = session.Close()
		})
	}
	wait.Wait()
	return errors.Join(errs...)
}

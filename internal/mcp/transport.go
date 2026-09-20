package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	clientName     = "aty"
	clientVersion  = "v0.1.0"
	connectTimeout = 10 * time.Second
	cleanupTimeout = 2 * time.Second
)

type serverResult struct {
	session *sdk.ClientSession
	tools   []*sdk.Tool
	err     error
}

func openServer(ctx context.Context, workspace string, server Server) serverResult {
	session, err := connect(ctx, workspace, server)
	if err != nil {
		return serverResult{err: err}
	}
	tools, err := listTools(ctx, session)
	if err != nil {
		_ = session.Close()
		return serverResult{err: fmt.Errorf("listing tools: %w", err)}
	}
	return serverResult{session: session, tools: tools}
}

func redactConnectError(err error, server Server) error {
	message := err.Error()
	if server.URL != "" {
		if parsed, parseErr := http.NewRequest(http.MethodGet, server.URL, nil); parseErr == nil {
			safeURL := parsed.URL.Scheme + "://" + parsed.URL.Host
			message = strings.ReplaceAll(message, server.URL, safeURL)
		}
	}
	for _, value := range server.Headers {
		if value != "" {
			message = strings.ReplaceAll(message, value, "[redacted]")
		}
	}
	for _, value := range server.Env {
		if value != "" {
			message = strings.ReplaceAll(message, value, "[redacted]")
		}
	}
	return errors.New(message)
}

func connect(ctx context.Context, workspace string, server Server) (*sdk.ClientSession, error) {
	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	client := sdk.NewClient(&sdk.Implementation{Name: clientName, Version: clientVersion}, &sdk.ClientOptions{
		Capabilities: &sdk.ClientCapabilities{},
	})
	switch server.Type {
	case transportStdio:
		command := exec.Command(server.Command, server.Args...)
		command.Dir = workspace
		command.Env = environment(server.Env)
		command.Stderr = io.Discard
		return client.Connect(connectCtx, &sdk.CommandTransport{Command: command}, nil)
	case transportHTTP:
		httpClient := &http.Client{Transport: headerTransport{
			base: http.DefaultTransport, headers: server.Headers,
		}}
		return client.Connect(connectCtx, &sdk.StreamableClientTransport{
			Endpoint: server.URL, HTTPClient: httpClient, DisableStandaloneSSE: true,
		}, nil)
	default:
		return nil, fmt.Errorf("unsupported transport %q", server.Type)
	}
}

type headerTransport struct {
	base          http.RoundTripper
	headers       map[string]string
	deleteTimeout time.Duration
}

func (transport headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx := request.Context()
	if request.Method == http.MethodDelete {
		timeout := transport.deleteTimeout
		if timeout <= 0 {
			timeout = cleanupTimeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cloned := request.Clone(ctx)
	for key, value := range transport.headers {
		cloned.Header.Set(key, value)
	}
	return transport.base.RoundTrip(cloned)
}

func listTools(ctx context.Context, session *sdk.ClientSession) ([]*sdk.Tool, error) {
	listCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	var tools []*sdk.Tool
	for tool, err := range session.Tools(listCtx, nil) {
		if err != nil {
			return nil, err
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

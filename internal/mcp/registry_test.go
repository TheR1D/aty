package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const helperEnvironment = "ATY_MCP_TEST_HELPER"

func TestStdioHelperServer(t *testing.T) {
	if os.Getenv(helperEnvironment) != "1" {
		return
	}
	server := testMCPServer()
	if err := server.Run(context.Background(), &sdk.StdioTransport{}); err != nil {
		_, _ = io.WriteString(os.Stderr, err.Error())
		os.Exit(1)
	}
	os.Exit(0)
}

func TestRegistryConnectsToAStdioServerAndCallsItsTool(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	registry := Open(t.Context(), Config{
		Workspace: t.TempDir(),
		Servers: map[string]Server{
			"local demo": {
				Type: "stdio", Command: executable,
				Args: []string{"-test.run=^TestStdioHelperServer$"},
				Env:  map[string]string{helperEnvironment: "1"},
			},
		},
	})
	t.Cleanup(func() { _ = registry.Close() })

	statuses := registry.Statuses()
	if len(statuses) != 1 || statuses[0].Err != nil || statuses[0].Tools != 1 {
		t.Fatalf("statuses = %+v, want one connected server with one tool", statuses)
	}
	tools := registry.Tools()
	if len(tools) != 1 || tools[0].Name != "mcp_local_demo_echo" {
		t.Fatalf("tools = %+v, want namespaced echo tool", tools)
	}
	result, err := registry.Call(t.Context(), tools[0].Name, json.RawMessage(`{"value":"hello"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result != "echo: hello" {
		t.Errorf("result = %q, want echo: hello", result)
	}
}

func TestStdioEnvironmentDoesNotInheritCredentialsOrIdentity(t *testing.T) {
	t.Setenv("ATY_API_KEY", "provider-secret")
	t.Setenv("GITHUB_TOKEN", "github-secret")
	t.Setenv("USER", "local-identity")
	t.Setenv("HOME", "/safe-home")
	t.Setenv("PATH", "/parent-bin")

	entries := environment(map[string]string{
		"MCP_TOKEN": "explicit-secret",
		"PATH":      "/configured-bin",
	})
	values := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("invalid environment entry %q", entry)
		}
		values[key] = value
	}

	for _, key := range []string{"ATY_API_KEY", "GITHUB_TOKEN", "USER"} {
		if _, exists := values[key]; exists {
			t.Errorf("stdio environment inherited %s", key)
		}
	}
	for key, want := range map[string]string{
		"HOME":      "/safe-home",
		"MCP_TOKEN": "explicit-secret",
		"PATH":      "/configured-bin",
	} {
		if got := values[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestRegistryConnectsToStreamableHTTPWithConfiguredHeaders(t *testing.T) {
	mcpServer := testMCPServer()
	var sawHeader bool
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server {
		return mcpServer
	}, &sdk.StreamableHTTPOptions{Stateless: true})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer test-token" {
			sawHeader = true
		}
		if r.Body != nil {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			if bytes.Contains(body, []byte(`"roots"`)) ||
				bytes.Contains(body, []byte(`"sampling"`)) ||
				bytes.Contains(body, []byte(`"elicitation"`)) {
				t.Errorf("tools-only client advertised unsupported capabilities: %s", body)
			}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)

	registry := Open(t.Context(), Config{
		Workspace: t.TempDir(),
		Servers: map[string]Server{
			"remote": {
				Type: "http", URL: httpServer.URL,
				Headers: map[string]string{"Authorization": "Bearer test-token"},
			},
		},
	})
	t.Cleanup(func() { _ = registry.Close() })
	if statuses := registry.Statuses(); len(statuses) != 1 || statuses[0].Err != nil {
		t.Fatalf("statuses = %+v, want connected remote server", statuses)
	}
	if !sawHeader {
		t.Fatal("the configured header was not sent to the remote server")
	}
	tool := registry.Tools()[0]
	result, err := registry.Call(t.Context(), tool.Name, json.RawMessage(`{"value":"remote"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result != "echo: remote" {
		t.Errorf("result = %q, want echo: remote", result)
	}
}

func TestRegistryConnectsServersConcurrently(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	newBlockedServer := func(name string) *httptest.Server {
		handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server {
			return testMCPServer()
		}, &sdk.StreamableHTTPOptions{Stateless: true})
		var firstRequest sync.Once
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			firstRequest.Do(func() {
				started <- name
				<-release
			})
			handler.ServeHTTP(w, request)
		}))
	}

	first := newBlockedServer("first")
	t.Cleanup(first.Close)
	second := newBlockedServer("second")
	t.Cleanup(second.Close)

	workspace := t.TempDir()
	opened := make(chan *Registry, 1)
	go func() {
		opened <- Open(t.Context(), Config{
			Workspace: workspace,
			Servers: map[string]Server{
				"first":  {Type: transportHTTP, URL: first.URL},
				"second": {Type: transportHTTP, URL: second.URL},
			},
		})
	}()

	seen := make(map[string]bool)
	timer := time.NewTimer(time.Second)
	timedOut := false
	for len(seen) < 2 && !timedOut {
		select {
		case name := <-started:
			seen[name] = true
		case <-timer.C:
			timedOut = true
		}
	}
	timer.Stop()
	close(release)

	registry := <-opened
	t.Cleanup(func() { _ = registry.Close() })
	if timedOut {
		t.Fatalf("only started %v; MCP connections ran sequentially", seen)
	}
	statuses := registry.Statuses()
	if len(statuses) != 2 || statuses[0].Name != "first" || statuses[1].Name != "second" {
		t.Fatalf("statuses = %+v, want deterministic config-name order", statuses)
	}
}

func TestRegistryIsolatesFailedServersAndToolNameCollisions(t *testing.T) {
	registry := &Registry{routes: make(map[string]registeredTool)}
	first := registry.uniqueAlias("a b", "read")
	registry.routes[first] = registeredTool{}
	second := registry.uniqueAlias("a_b", "read")
	if first == second {
		t.Fatalf("colliding tool names both became %q", first)
	}
	if len(first) > 64 || len(second) > 64 {
		t.Fatalf("aliases exceed provider limit: %q %q", first, second)
	}

	failed := Open(t.Context(), Config{
		Workspace: t.TempDir(),
		Servers: map[string]Server{
			"offline": {Type: "http", URL: "http://127.0.0.1:1/mcp?token=do-not-print"},
		},
	})
	t.Cleanup(func() { _ = failed.Close() })
	if statuses := failed.Statuses(); len(statuses) != 1 || statuses[0].Err == nil {
		t.Fatalf("statuses = %+v, want isolated connection failure", statuses)
	} else if strings.Contains(statuses[0].Err.Error(), "do-not-print") {
		t.Fatalf("connection error leaked a URL secret: %v", statuses[0].Err)
	}
}

func TestFormatResultKeepsUsefulContentAndOmitsBinaryData(t *testing.T) {
	result := formatResult(&sdk.CallToolResult{
		Content: []sdk.Content{
			&sdk.TextContent{Text: "plain text"},
			&sdk.ImageContent{MIMEType: "image/png", Data: []byte("binary")},
		},
		StructuredContent: map[string]any{"ok": true},
		IsError:           true,
	})
	for _, want := range []string{"the MCP tool failed", "plain text", "image/png omitted", `"ok":true`} {
		if !strings.Contains(result, want) {
			t.Errorf("formatted result %q does not contain %q", result, want)
		}
	}
	if strings.Contains(result, "binary") {
		t.Errorf("formatted result included binary data: %q", result)
	}
}

func TestRemoteSessionCleanupHasABoundedRequest(t *testing.T) {
	transport := headerTransport{
		base: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, request.Context().Err()
		}),
		deleteTimeout: 20 * time.Millisecond,
	}
	request, err := http.NewRequest(http.MethodDelete, "https://example.com/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := transport.RoundTrip(request); err == nil {
		t.Fatal("stalled DELETE returned no timeout error")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stalled DELETE took %s, want bounded cleanup", elapsed)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func testMCPServer() *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: "test-server", Version: "v1.0.0"}, nil)
	server.AddTool(&sdk.Tool{
		Name: "echo", Description: "Echo a value.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`),
	}, func(_ context.Context, request *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		raw, _ := json.Marshal(request.Params.Arguments)
		var arguments struct {
			Value string `json:"value"`
		}
		_ = json.Unmarshal(raw, &arguments)
		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: "echo: " + arguments.Value}},
		}, nil
	})
	return server
}

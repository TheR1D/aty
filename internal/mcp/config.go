// Package mcp loads and connects the MCP servers available to an aty session.
package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	emptyConfig             = "{\n  \"mcpServers\": {}\n}\n"
	transportStdio          = "stdio"
	transportHTTP           = "http"
	cursorHTTPTransportType = "streamable-http"
)

// Config is the resolved MCP configuration for one aty session.
type Config struct {
	Workspace string
	Servers   map[string]Server
}

// Server is one local or remote MCP server.
type Server struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	EnvFile string            `json:"envFile,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

type configFile struct {
	Schema     string            `json:"$schema,omitempty"`
	MCPServers map[string]Server `json:"mcpServers"`
}

// LoadOrCreate creates an empty config when path is missing, then parses,
// resolves, and validates every configured server.
func LoadOrCreate(path, workspace string) (Config, error) {
	if strings.TrimSpace(path) == "" {
		return Config{}, fmt.Errorf("mcp: config path is empty")
	}
	if workspace == "" {
		var err error
		workspace, err = os.Getwd()
		if err != nil {
			return Config{}, fmt.Errorf("mcp: finding the working directory: %w", err)
		}
	}
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return Config{}, fmt.Errorf("mcp: resolving workspace: %w", err)
	}
	if err := ensureConfig(path); err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("mcp: reading %s: %w", path, err)
	}
	var file configFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return Config{}, fmt.Errorf("mcp: %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return Config{}, fmt.Errorf("mcp: %s: %w", path, err)
	}
	if file.MCPServers == nil {
		return Config{}, fmt.Errorf("mcp: %s: mcpServers is required", path)
	}

	resolved := Config{Workspace: workspace, Servers: make(map[string]Server, len(file.MCPServers))}
	for _, name := range slices.Sorted(maps.Keys(file.MCPServers)) {
		if strings.TrimSpace(name) == "" {
			return Config{}, fmt.Errorf("mcp: %s: server name is empty", path)
		}
		server, err := resolveServer(file.MCPServers[name], workspace)
		if err != nil {
			return Config{}, fmt.Errorf("mcp: %s: server %q: %w", path, name, err)
		}
		resolved.Servers[name] = server
	}
	return resolved, nil
}

func ensureConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mcp: creating config directory %s: %w", filepath.Dir(path), err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return fmt.Errorf("mcp: creating %s: %w", path, err)
	}
	complete := false
	defer func() {
		_ = file.Close()
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.WriteString(emptyConfig); err != nil {
		return fmt.Errorf("mcp: writing %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("mcp: closing %s: %w", path, err)
	}
	complete = true
	return nil
}

func resolveServer(server Server, workspace string) (Server, error) {
	server.Type = strings.ToLower(strings.TrimSpace(server.Type))
	if err := server.interpolate(workspace); err != nil {
		return Server{}, err
	}
	if server.EnvFile != "" && !filepath.IsAbs(server.EnvFile) {
		server.EnvFile = filepath.Join(workspace, server.EnvFile)
	}

	switch {
	case server.Command != "" && server.URL != "":
		return Server{}, fmt.Errorf("command and url cannot both be set")
	case server.Command != "":
		if server.Type == "" {
			server.Type = transportStdio
		}
		if server.Type != transportStdio {
			return Server{}, fmt.Errorf("type %q cannot use command", server.Type)
		}
		if len(server.Headers) > 0 {
			return Server{}, fmt.Errorf("headers are only supported for remote servers")
		}
		fromFile, err := readEnvFile(server.EnvFile)
		if err != nil {
			return Server{}, err
		}
		server.Env = mergeEnvironment(fromFile, server.Env)
	case server.URL != "":
		if server.Type == "" {
			server.Type = transportHTTP
		}
		if server.Type == cursorHTTPTransportType {
			server.Type = transportHTTP
		}
		if server.Type != transportHTTP {
			return Server{}, fmt.Errorf("unsupported remote type %q; use http", server.Type)
		}
		if server.EnvFile != "" {
			return Server{}, fmt.Errorf("envFile is only supported for stdio servers")
		}
		if len(server.Args) > 0 || len(server.Env) > 0 {
			return Server{}, fmt.Errorf("args and env are only supported for stdio servers")
		}
		parsed, err := url.Parse(server.URL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return Server{}, fmt.Errorf("url must be an absolute http or https URL")
		}
		if parsed.User != nil {
			return Server{}, fmt.Errorf("url credentials are not supported; use headers")
		}
		if parsed.Scheme == "http" && len(server.Headers) > 0 && !loopbackHost(parsed.Hostname()) {
			return Server{}, fmt.Errorf("headers require https except for loopback servers")
		}
	default:
		return Server{}, fmt.Errorf("command or url is required")
	}
	return server, nil
}

func loopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

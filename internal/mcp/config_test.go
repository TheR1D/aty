package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadCreatesAnEmptyConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp", "mcp.json")
	config, err := LoadOrCreate(path, t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(config.Servers) != 0 {
		t.Fatalf("servers = %+v, want none", config.Servers)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestLoadResolvesCursorCompatibleLocalAndRemoteServers(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "work")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".env"), []byte("FROM_FILE=file\nOVERRIDE=old\nQUOTED='hello world'\nLITERAL=${workspaceFolder}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCP_TOKEN", "secret-token")
	path := writeConfig(t, root, `{
  "$schema": "https://example.invalid/mcp.schema.json",
  "mcpServers": {
    "local": {
      "command": "${userHome}/bin/server",
      "args": ["${workspaceFolder}", "${workspaceFolderBasename}", "${pathSeparator}", "${/}"],
      "envFile": ".env",
      "env": {"OVERRIDE": "new", "TOKEN": "${env:MCP_TOKEN}"}
    },
    "remote": {
      "type": "http",
      "url": "https://example.com/${workspaceFolderBasename}",
      "headers": {"Authorization": "Bearer ${env:MCP_TOKEN}"}
    }
  }
}`)

	config, err := LoadOrCreate(path, workspace)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	local := config.Servers["local"]
	if local.Type != "stdio" || local.Command == "" {
		t.Errorf("local = %+v, want inferred stdio", local)
	}
	if local.Args[0] != workspace || local.Args[1] != "work" {
		t.Errorf("args = %q, want workspace interpolation", local.Args)
	}
	if local.Env["FROM_FILE"] != "file" || local.Env["OVERRIDE"] != "new" || local.Env["TOKEN"] != "secret-token" {
		t.Errorf("env = %#v, want file values overridden by env", local.Env)
	}
	if local.Env["QUOTED"] != "hello world" {
		t.Errorf("quoted env value = %q, want hello world", local.Env["QUOTED"])
	}
	if local.Env["LITERAL"] != "${workspaceFolder}" {
		t.Errorf("envFile content was interpolated: %q", local.Env["LITERAL"])
	}
	remote := config.Servers["remote"]
	if remote.Type != "http" || remote.URL != "https://example.com/work" {
		t.Errorf("remote = %+v, want resolved HTTP server", remote)
	}
	if remote.Headers["Authorization"] != "Bearer secret-token" {
		t.Errorf("authorization header was not resolved")
	}
}

func TestLoadRejectsInvalidConfigurationsWithoutLeakingSecrets(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "broken JSON", body: `{"mcpServers":`, want: "mcp.json"},
		{name: "missing servers", body: `{}`, want: "mcpServers is required"},
		{name: "both transports", body: `{"mcpServers":{"bad":{"command":"x","url":"https://example.com"}}}`, want: "cannot both"},
		{name: "SSE", body: `{"mcpServers":{"bad":{"type":"sse","url":"https://example.com"}}}`, want: "unsupported remote type"},
		{name: "relative URL", body: `{"mcpServers":{"bad":{"url":"/mcp"}}}`, want: "absolute"},
		{name: "URL credentials", body: `{"mcpServers":{"bad":{"url":"https://user:secret@example.com/mcp"}}}`, want: "credentials are not supported"},
		{name: "plaintext credentials", body: `{"mcpServers":{"bad":{"url":"http://example.com/mcp","headers":{"Authorization":"secret"}}}}`, want: "require https"},
		{name: "unknown field", body: `{"mcpServers":{"bad":{"command":"x","surprise":true}}}`, want: "unknown field"},
		{name: "command interpolation", body: `{"mcpServers":{"bad":{"command":"${unsupported}"}}}`, want: "unsupported interpolation"},
		{name: "argument interpolation", body: `{"mcpServers":{"bad":{"command":"x","args":["${unsupported}"]}}}`, want: "unsupported interpolation"},
		{name: "envFile interpolation", body: `{"mcpServers":{"bad":{"command":"x","envFile":"${unsupported}"}}}`, want: "unsupported interpolation"},
		{name: "environment interpolation", body: `{"mcpServers":{"bad":{"command":"x","env":{"KEY":"${unsupported}"}}}}`, want: "unsupported interpolation"},
		{name: "header interpolation", body: `{"mcpServers":{"bad":{"url":"https://example.com","headers":{"Authorization":"${unsupported}"}}}}`, want: "unsupported interpolation"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfig(t, t.TempDir(), test.body)
			_, err := LoadOrCreate(path, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load error = %v, want %q", err, test.want)
			}
		})
	}

	t.Setenv("VERY_SECRET_TOKEN", "do-not-print-me")
	path := writeConfig(t, t.TempDir(), `{"mcpServers":{"remote":{"url":"${env:MISSING_TOKEN}","headers":{"X-Secret":"${env:VERY_SECRET_TOKEN}"}}}}`)
	_, err := LoadOrCreate(path, t.TempDir())
	if err == nil {
		t.Fatal("missing interpolation was accepted")
	}
	if strings.Contains(err.Error(), "do-not-print-me") {
		t.Fatalf("error leaked a resolved secret: %v", err)
	}
}

func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

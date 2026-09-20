package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheR1D/aty/internal/llm"
	appmcp "github.com/TheR1D/aty/internal/mcp"
)

func TestDoctorReportsAReachableLoadedModel(t *testing.T) {
	client := doctorClient(t, "qwen3.5:9b", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("doctor requested %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"qwen3.5:9b"}]}`)
	})

	var out bytes.Buffer
	if code := check(&out, client); code != 0 {
		t.Fatalf("check exited %d, want 0:\n%s", code, out.String())
	}
	got := out.String()
	for _, want := range []string{"llama  ok", "model   ok", "qwen3.5:9b"} {
		if !strings.Contains(got, want) {
			t.Errorf("the report does not mention %q:\n%s", want, got)
		}
	}
}

func TestDoctorReportsAnUnreachableServer(t *testing.T) {
	client := llm.NewLlama(llm.Options{Endpoint: "http://127.0.0.1:1", Model: "qwen3.5:9b"})

	var out bytes.Buffer
	if code := check(&out, client); code != 1 {
		t.Fatalf("check exited %d, want 1:\n%s", code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "llama  fail") {
		t.Errorf("an unreachable completions server was not reported as a failure:\n%s", got)
	}
	if !strings.Contains(got, "model   skip") {
		t.Errorf("the model was checked against a llama-server that is down:\n%s", got)
	}
}

func TestDoctorReportsAMissingModel(t *testing.T) {
	client := doctorClient(t, "qwen3.5:9b", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("doctor requested %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"other"}]}`)
	})

	var out bytes.Buffer
	if code := check(&out, client); code != 1 {
		t.Fatalf("check exited %d, want 1:\n%s", code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "model   fail") || !strings.Contains(got, "qwen3.5:9b") {
		t.Errorf("a missing model was not named:\n%s", got)
	}
	if !strings.Contains(got, "other") {
		t.Errorf("the models that are loaded were not listed:\n%s", got)
	}
}

func TestDoctorUsesTheOllamaProvider(t *testing.T) {
	announcingShell(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"from-env"}]}`)
	}))
	t.Cleanup(server.Close)

	got, code := doctorOutput(t, "ollama", "from-env", server.URL+"/chat/completions")
	if code != 0 {
		t.Fatalf("doctor exited %d:\n%s", code, got)
	}
	if !strings.Contains(got, "ollama  ok") {
		t.Errorf("doctor did not label the backend as ollama:\n%s", got)
	}
	if !strings.Contains(got, "from-env") {
		t.Errorf("doctor did not check the model from the environment:\n%s", got)
	}
}

func TestDoctorUsesTheLlamaProvider(t *testing.T) {
	announcingShell(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"local-model"}]}`)
	}))
	t.Cleanup(server.Close)

	got, code := doctorOutput(t, "llama", "local-model", server.URL+"/chat/completions")
	if code != 0 {
		t.Fatalf("doctor exited %d:\n%s", code, got)
	}
	if !strings.Contains(got, "llama  ok") {
		t.Errorf("doctor did not label the backend as llama:\n%s", got)
	}
	if !strings.Contains(got, "local-model") {
		t.Errorf("doctor did not check the llama.cpp model:\n%s", got)
	}
}

func TestDoctorUsesTheOpenAIProviders(t *testing.T) {
	for _, test := range []struct{ provider, endpoint string }{
		{"openai-compatible", "/v1/chat/completions"},
		{"openai-native", "/v1/responses"},
	} {
		t.Run(test.provider, func(t *testing.T) {
			announcingShell(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer test-key" {
					t.Errorf("doctor requested %s with incorrect path or authorization", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				_, _ = io.WriteString(w, `{"data":[{"id":"gpt-4o-mini"}]}`)
			}))
			t.Cleanup(server.Close)

			got, code := doctorOutput(t, test.provider, "gpt-4o-mini", server.URL+test.endpoint)
			if code != 0 {
				t.Fatalf("doctor exited %d:\n%s", code, got)
			}
			if !strings.Contains(got, test.provider+"  ok") || !strings.Contains(got, "model   ok    gpt-4o-mini") {
				t.Errorf("doctor did not identify the provider and model:\n%s", got)
			}
		})
	}
}

func TestDoctorChecksTheThinkBackend(t *testing.T) {
	announcingShell(t)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"plain-model"}]}`)
	}))
	t.Cleanup(plain.Close)
	thinking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"think-model"}]}`)
	}))
	t.Cleanup(thinking.Close)

	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "aty")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), fmt.Appendf(nil, `
provider = "openai-compatible"
api_key = "test-key"
model = "plain-model"
endpoint = %q
`, plain.URL+"/chat/completions"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "think_config.toml"), fmt.Appendf(nil, `
provider = "openai-compatible"
api_key = "test-key"
model = "think-model"
endpoint = %q
`, thinking.URL+"/chat/completions"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, code := doctorOutput(t)
	if code != 0 {
		t.Fatalf("doctor exited %d:\n%s", code, got)
	}
	if !strings.Contains(got, "plain-model") {
		t.Errorf("doctor did not check the default model:\n%s", got)
	}
	if !strings.Contains(got, "think-model") {
		t.Errorf("doctor did not check the think model:\n%s", got)
	}
	if !strings.Contains(got, "??") {
		t.Errorf("doctor did not label the think backend:\n%s", got)
	}
}

func TestDoctorUsesTheParsedSettings(t *testing.T) {
	announcingShell(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"from-env"}]}`)
	}))
	t.Cleanup(server.Close)

	got, code := doctorOutput(t, "openai-compatible", "from-env", server.URL)
	if code != 0 {
		t.Fatalf("doctor exited %d:\n%s", code, got)
	}
	if !strings.Contains(got, "from-env") {
		t.Errorf("doctor did not check the model from the environment:\n%s", got)
	}
}

func TestDoctorReportsAShellThatAnnouncesAPrompt(t *testing.T) {
	path := announcingShell(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"from-env"}]}`)
	}))
	t.Cleanup(server.Close)

	got, code := doctorOutput(t, "openai-compatible", "from-env", server.URL)
	if code != 0 {
		t.Fatalf("doctor exited %d:\n%s", code, got)
	}
	if !strings.Contains(got, "shell   ok    "+path) {
		t.Errorf("doctor did not report the shell that announced a prompt:\n%s", got)
	}
}

func TestDoctorReportsAShellThatNeverAnnounces(t *testing.T) {
	path := silentShell(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"from-env"}]}`)
	}))
	t.Cleanup(server.Close)

	got, code := doctorOutput(t, "openai-compatible", "from-env", server.URL)
	if code != 1 {
		t.Fatalf("doctor exited %d, want 1 for a shell without paste marks:\n%s", code, got)
	}
	if !strings.Contains(got, "shell   fail  "+path) {
		t.Errorf("an unsupported shell was not named:\n%s", got)
	}
	if !strings.Contains(got, "need  zsh, bash 5.1+, or fish") {
		t.Errorf("the shells that do announce were not listed:\n%s", got)
	}
	if !strings.Contains(got, "from-env") {
		t.Errorf("the model was skipped because the shell check failed:\n%s", got)
	}
}

func TestDoctorReportsAMissingShell(t *testing.T) {
	isolateConfig(t)
	path := filepath.Join(t.TempDir(), "no-such-shell")
	t.Setenv("SHELL", path)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"from-env"}]}`)
	}))
	t.Cleanup(server.Close)

	got, code := doctorOutput(t, "openai-compatible", "from-env", server.URL)
	if code != 1 {
		t.Fatalf("doctor exited %d, want 1 for a missing shell:\n%s", code, got)
	}
	if !strings.Contains(got, "shell   fail") || !strings.Contains(got, path) {
		t.Errorf("a missing shell was not reported as a failure:\n%s", got)
	}
	if strings.Contains(got, "need  zsh") {
		t.Errorf("a missing binary was described as a shell without paste marks:\n%s", got)
	}
}

func TestDoctorDumpIncludesChecksAndMessages(t *testing.T) {
	announcingShell(t)
	client := doctorClient(t, "qwen3.5:9b", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("doctor requested %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"qwen3.5:9b"}]}`)
	})

	got := doctorDump(client, client, llm.Prompts{System: "be brief", Agent: "use the tool"}, "list the files", []llm.Turn{
		{Text: "$ pwd\n/tmp"},
	}, nil)
	if want := "ATY version " + version + "\n"; !strings.HasPrefix(got, want) {
		t.Errorf("?# dump should start with %q:\n%s", want, got)
	}
	for _, want := range []string{
		"======= provider =======",
		"shell   ok",
		"llama  ok",
		"model   ok",
		"qwen3.5:9b",
		"======= default prompt =======",
		"be brief",
		"======= agent prompt =======",
		"use the tool",
		"======= tools =======",
		"suggest_shell_command",
		"======= context =======",
		"list the files",
		"/tmp",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("?# dump does not mention %q:\n%s", want, got)
		}
	}
	if i, j := strings.Index(got, "======= provider ======="), strings.Index(got, "======= default prompt ======="); i < 0 || j < 0 || i > j {
		t.Errorf("provider checks should precede the prompts:\n%s", got)
	}
}

func TestDoctorDumpReportsUnavailableMCPServers(t *testing.T) {
	announcingShell(t)
	client := doctorClient(t, "qwen3.5:9b", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"qwen3.5:9b"}]}`)
	})
	registry := appmcp.Open(t.Context(), appmcp.Config{
		Workspace: t.TempDir(),
		Servers: map[string]appmcp.Server{
			"offline": {Type: "http", URL: "http://127.0.0.1:1/mcp"},
		},
	})
	t.Cleanup(func() { _ = registry.Close() })

	got := doctorDump(client, client, llm.Prompts{}, "", nil, registry)
	for _, want := range []string{"mcp     fail", "offline", "(http)"} {
		if !strings.Contains(got, want) {
			t.Errorf("?# dump does not mention %q:\n%s", want, got)
		}
	}
}

func doctorOutput(t *testing.T, backend ...string) (string, int) {
	t.Helper()
	stderr := ioDiscard(t)
	if len(backend) != 0 {
		if len(backend) != 3 {
			t.Fatalf("doctorOutput backend has %d values, want provider, model, and endpoint", len(backend))
		}
		t.Setenv("ATY_PROVIDER", backend[0])
		t.Setenv("ATY_API_KEY", "test-key")
		t.Setenv("ATY_MODEL", backend[1])
		t.Setenv("ATY_ENDPOINT", backend[2])
	}
	s, err := parse(nil, stderr)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ask, think, err := clients(s, llm.Prompts{}, nil)
	if err != nil {
		t.Fatalf("clients: %v", err)
	}
	var out bytes.Buffer
	code := doctor(&out, ask, think)
	return out.String(), code
}

func doctorClient(t *testing.T, model string, handle http.HandlerFunc) *llm.Client {
	t.Helper()
	server := httptest.NewServer(handle)
	t.Cleanup(server.Close)
	return llm.NewLlama(llm.Options{Model: model, Endpoint: server.URL + "/chat/completions", HTTP: server.Client()})
}

func announcingShell(t *testing.T) string {
	t.Helper()
	return fakeShell(t, "printf '\\033[?2004h'\n")
}

func silentShell(t *testing.T) string {
	t.Helper()
	return fakeShell(t, "printf 'sh$ '\n")
}

func isolateConfig(t *testing.T) {
	t.Helper()
	isolateConfigHome(t)
	clearConfigEnv(t)
}

func fakeShell(t *testing.T, body string) string {
	t.Helper()
	isolateConfig(t)
	path := filepath.Join(t.TempDir(), "shell")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", path)
	return path
}

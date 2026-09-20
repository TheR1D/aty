package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appconfig "github.com/TheR1D/aty/internal/config"
	"github.com/TheR1D/aty/internal/llm"
)

func TestConfiguredLimitsReachTerminalAndAgentRequests(t *testing.T) {
	const ps1 = "user@host {{.PS1}}$ "
	for _, separateBackend := range []bool{false, true} {
		t.Run(fmt.Sprint(separateBackend), func(t *testing.T) {
			isolateConfig(t)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []struct{ Role, Content string }
					Tools    []json.RawMessage
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if len(body.Messages) == 0 || body.Messages[0].Content != "agent "+ps1 {
					t.Errorf("agent request lost the startup prompt: %+v", body.Messages)
				}
				if requests.Add(1) == 1 {
					_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"suggest_shell_command","arguments":"{\"command\":\"cat file\"}"}}]}}]}`+"\n\ndata: [DONE]\n\n")
					return
				}
				if len(body.Tools) != 0 {
					t.Error("agent_steps = 1 did not remove tools after the first round")
				}
				last := body.Messages[len(body.Messages)-1]
				if last.Role != "tool" || last.Content != "aaaaa\n\n# --- truncated long output ---\n\n"+strings.Repeat("b", 24) {
					t.Errorf("tool_output_bytes = 64 produced %+v", last)
				}
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			backend := fmt.Sprintf("provider = 'ollama'\nmodel = 'local'\nendpoint = %q\n", server.URL)
			writeConfig(t, backend+"[limits]\ncommand_bytes = 7\ntool_output_bytes = 64\nagent_steps = 1\n")
			if separateBackend {
				writeConfigIn(t, isolateConfigHome(t), thinking, strings.Replace(backend, "'local'", "'thinking'", 1))
			}
			settings, err := parse(nil, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := compose(settings, llm.Prompts{System: "system {{.PS1}}", Agent: "agent {{.PS1}}"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Limits != settings.Limits || cfg.Limits.CommandBytes != 7 {
				t.Fatalf("terminal limits were not forwarded: %+v", cfg.Limits)
			}
			cfg.InitialPrompt(ps1)
			discard := func(string) error { return nil }
			if err := cfg.Agent(t.Context(), "read file", nil, discard, discard, func(context.Context, string) (string, error) {
				return strings.Repeat("a", 100) + strings.Repeat("b", 100), nil
			}, nil); err != nil {
				t.Fatal(err)
			}
			if requests.Load() != 2 {
				t.Fatalf("got %d requests, want one tool round and a final answer", requests.Load())
			}
		})
	}
}

func TestModesReuseConnectionsAndKeepProviderSettings(t *testing.T) {
	const ps1 = "user@host:~/project$ "
	for _, separateServers := range []bool{false, true} {
		name := "same server"
		if separateServers {
			name = "different servers"
		}
		t.Run(name, func(t *testing.T) {
			var connections, requests atomic.Int32
			handler := func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				var body struct {
					Messages        []struct{ Role, Content string }
					Model           string  `json:"model"`
					Temperature     float64 `json:"temperature"`
					Think           *bool   `json:"think"`
					ReasoningEffort string  `json:"reasoning_effort"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				switch r.URL.Path {
				case "/plain":
					if body.Model != "plain-model" || body.Temperature != 0.2 || body.Think != nil || body.ReasoningEffort != "none" || r.Header.Get("Authorization") != "Bearer plain-key" {
						t.Error("normal mode lost its provider settings")
					}
				case "/thinking":
					if body.Model != "thinking-model" || body.Temperature != 0.7 || body.Think == nil || !*body.Think || body.ReasoningEffort != "high" || r.Header.Get("Authorization") != "Bearer thinking-key" {
						t.Error("thinking/agent mode lost its provider settings")
					}
				default:
					t.Errorf("unexpected endpoint: %s", r.URL.Path)
				}
				if len(body.Messages) == 0 || (body.Messages[0].Content != "system "+ps1 && body.Messages[0].Content != "agent "+ps1) {
					t.Errorf("request lost the startup prompt: %+v", body.Messages)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"pwd\"}}]}\n\ndata: [DONE]\n\n")
			}
			startServer := func() *httptest.Server {
				server := httptest.NewUnstartedServer(http.HandlerFunc(handler))
				server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
					if state == http.StateNew {
						connections.Add(1)
					}
				}
				server.Start()
				t.Cleanup(server.Close)
				return server
			}
			plain := startServer()
			thinking := plain
			wantConnections := int32(1)
			if separateServers {
				thinking = startServer()
				wantConnections = 2
			}
			plainKey, thinkingKey := "plain-key", "thinking-key"
			plainTemp, thinkingTemp := 0.2, 0.7
			effort := "high"
			cfg, err := compose(appconfig.Settings{
				Default: appconfig.Backend{
					Provider: appconfig.OpenAICompatible, Model: "plain-model", Endpoint: plain.URL + "/plain",
					APIKey: &plainKey, Temperature: &plainTemp, ReasoningEffort: &effort,
				},
				Think: appconfig.Backend{
					Provider: appconfig.Ollama, Model: "thinking-model", Endpoint: thinking.URL + "/thinking",
					APIKey: &thinkingKey, Temperature: &thinkingTemp, ReasoningEffort: &effort,
				},
			}, llm.Prompts{System: "system {{.PS1}}", Agent: "agent {{.PS1}}"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			discard := func(string) error { return nil }
			cfg.InitialPrompt(ps1)
			for range 2 {
				if err := cfg.Ask(t.Context(), "where am I", nil, false, discard, discard); err != nil {
					t.Fatal(err)
				}
				if err := cfg.Ask(t.Context(), "where am I", nil, true, discard, discard); err != nil {
					t.Fatal(err)
				}
				if err := cfg.Agent(t.Context(), "where am I", nil, discard, discard, nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			if got := requests.Load(); got != 6 {
				t.Errorf("requests = %d, want 6", got)
			}
			if got := connections.Load(); got != wantConnections {
				t.Errorf("connections = %d, want %d across six mode requests", got, wantConnections)
			}
		})
	}
}

func TestSharedPoolDoesNotBlockAQueryBehindWarmup(t *testing.T) {
	warming := make(chan struct{})
	releaseWarmup := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/warm" {
			close(warming)
			select {
			case <-releaseWarmup:
			case <-r.Context().Done():
			}
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"pwd\"}}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(releaseWarmup) })
	ask, think, err := clients(appconfig.Settings{
		Default: appconfig.Backend{Provider: appconfig.Llama, Endpoint: server.URL + "/ask"},
		Think:   appconfig.Backend{Provider: appconfig.Llama, Endpoint: server.URL + "/warm"},
	}, llm.Prompts{System: "system", Agent: "agent"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	warmed := make(chan error, 1)
	go func() { warmed <- think.Prewarm(ctx, nil, true, false) }()
	defer func() {
		cancel()
		<-warmed
	}()
	select {
	case <-warming:
	case <-ctx.Done():
		t.Fatal("warmup did not start")
	}
	discard := func(string) error { return nil }
	if err := ask.Command(ctx, "where am I", nil, false, discard, discard); err != nil {
		t.Fatalf("query could not finish while the other mode was warming: %v", err)
	}
}

func TestNativeProviderRoutesEveryModeToResponses(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(fmt.Sprint(configured), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer native-key" {
					t.Errorf("native request lost its endpoint or authorization: %s", r.URL.Path)
				}
				var body struct {
					Model       string            `json:"model"`
					Messages    []json.RawMessage `json:"messages"`
					Input       []json.RawMessage `json:"input"`
					Store       *bool             `json:"store"`
					Temperature *float64          `json:"temperature"`
					Reasoning   *struct {
						Effort string `json:"effort"`
					} `json:"reasoning"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if body.Model != "native-model" || body.Store == nil || *body.Store || len(body.Input) == 0 || len(body.Messages) != 0 {
					t.Errorf("native request did not use stateless Responses: %+v", body)
				}
				if configured {
					if body.Temperature == nil || *body.Temperature != 0 || body.Reasoning == nil || body.Reasoning.Effort != "high" {
						t.Errorf("native request did not retain explicit options: %+v", body)
					}
				} else if body.Temperature != nil || (body.Reasoning != nil && body.Reasoning.Effort != "") {
					t.Errorf("native request supplied unconfigured options: %+v", body)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"pwd","output_index":0,"content_index":0}`+"\n\n")
				_, _ = io.WriteString(w, "data: "+`{"type":"response.completed","response":{"status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"pwd","annotations":[]}]}]}}`+"\n\n")
			}))
			t.Cleanup(server.Close)
			backend := appconfig.Backend{
				Provider: appconfig.OpenAINative, Model: "native-model",
				Endpoint: server.URL + "/v1/responses", APIKey: new("native-key"),
			}
			if configured {
				backend.Temperature = new(0.0)
				backend.ReasoningEffort = new("high")
			}
			cfg, err := compose(appconfig.Settings{Default: backend, Think: backend}, llm.Prompts{System: "system", Agent: "agent"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			discard := func(string) error { return nil }
			for _, thinking := range []bool{false, true} {
				var command string
				if err := cfg.Ask(t.Context(), "where am I", nil, thinking, func(s string) error { command = s; return nil }, discard); err != nil {
					t.Fatal(err)
				}
				if command != "pwd" {
					t.Errorf("command = %q, want pwd", command)
				}
			}
			if err := cfg.Agent(t.Context(), "where am I", nil, discard, discard, nil, nil); err != nil {
				t.Fatal(err)
			}
			if requests.Load() != 3 {
				t.Errorf("requests = %d, want one for each mode", requests.Load())
			}
		})
	}
}

package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestOllamaDoesNotInventConnectionOptions(t *testing.T) {
	client := NewOllama(Options{})

	if client.Name() != ollamaName {
		t.Errorf("Name = %q, want ollama", client.Name())
	}
	if client.endpoint != "" {
		t.Errorf("Endpoint = %q, want empty", client.endpoint)
	}
	if client.model != "" {
		t.Errorf("Model = %q, want empty", client.model)
	}
	if client.system == "" {
		t.Error("questions carry no system prompt, so nothing constrains the answer to a command")
	}
}

func TestOllamaPostsToTheConfiguredCompletionsURL(t *testing.T) {
	client := newOllamaTestClient(t, Options{Model: "qwen-test"}, func(w http.ResponseWriter, _ *http.Request) {
		send(t, w, token("ls"), token(" -la"), doneToken)
	})

	said := collect(t, client, "list everything", nil)
	if want := []string{"ls", "ls -la"}; !slices.Equal(said, want) {
		t.Errorf("the command arrived as %q, want %q", said, want)
	}
}

func TestOllamaFallbackPrewarmRequestsOneOutputToken(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	client := newOllamaTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("the warmup was not readable as JSON: %v", err)
		}
		bodies <- body
		_, _ = io.WriteString(w, `{"choices":[]}`)
	})

	if err := client.Prewarm(t.Context(), nil, false, false); err != nil {
		t.Fatalf("prewarming Ollama's fallback: %v", err)
	}
	sent := <-bodies
	if got := sent["max_tokens"]; got != float64(1) {
		t.Errorf("fallback max_tokens = %#v, want 1", got)
	}
	if _, ok := sent["prompt_cache_key"]; ok {
		t.Error("fallback invented a native cache key Ollama does not document")
	}
}

func TestOllamaAsksWithThinkNotAChatTemplate(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	client := newOllamaTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("the question was not readable as JSON: %v", err)
		}
		bodies <- body
		send(t, w, token("ls"), doneToken)
	})

	collect(t, client, "list everything in tmp", nil)

	sent := <-bodies
	if _, ok := sent["chat_template_kwargs"]; ok {
		t.Error("Ollama was sent chat_template_kwargs, which it does not use")
	}
	if _, ok := sent["add_generation_prompt"]; ok {
		t.Error("Ollama was sent add_generation_prompt, which it does not use")
	}
	if got, ok := sent["think"].(bool); !ok || got {
		t.Errorf("think = %#v, want false so a plain question is not billed for a thought", sent["think"])
	}
	if _, ok := sent["reasoning_effort"]; ok {
		t.Error("Ollama was sent reasoning_effort with none configured")
	}
	if _, ok := sent["temperature"]; ok {
		t.Error("Ollama was sent temperature with none configured")
	}
	for _, cap := range []string{"max_tokens", "max_completion_tokens"} {
		if _, ok := sent[cap]; ok {
			t.Errorf("Ollama was sent %s = %v; aty does not cap the answer", cap, sent[cap])
		}
	}
}

func TestOllamaThinksWhenTheQuestionAsks(t *testing.T) {
	questions := make(chan ollamaBody, 1)
	client := newOllamaTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		questions <- decode[ollamaBody](t, r)
		send(t, w, token("ls"), doneToken)
	})

	err := client.Command(t.Context(), "list everything in tmp", nil, true, func(string) error { return nil }, ignoreThought)
	if err != nil {
		t.Fatalf("asking a thinking question: %v", err)
	}

	asked := <-questions
	if asked.Think == nil || !*asked.Think {
		t.Errorf("think = %v, want true", asked.Think)
	}
	if asked.ReasoningEffort != "" {
		t.Errorf("reasoning_effort = %q, want empty when none was configured", asked.ReasoningEffort)
	}
}

func TestOllamaThinksAtTheConfiguredEffort(t *testing.T) {
	questions := make(chan ollamaBody, 1)
	client := newOllamaTestClient(t, Options{ReasoningEffort: "medium"}, func(w http.ResponseWriter, r *http.Request) {
		questions <- decode[ollamaBody](t, r)
		send(t, w, token("ls"), doneToken)
	})

	err := client.Command(t.Context(), "list everything in tmp", nil, true, func(string) error { return nil }, ignoreThought)
	if err != nil {
		t.Fatalf("asking a thinking question: %v", err)
	}

	asked := <-questions
	if asked.ReasoningEffort != "medium" {
		t.Errorf("reasoning_effort = %q, want the configured medium", asked.ReasoningEffort)
	}
}

func TestOllamaAgentDropsLlamaExtras(t *testing.T) {
	asked := make(chan ollamaBody, 2)
	extras := make(chan map[string]any, 2)
	client := newOllamaTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the question: %v", err)
			return
		}
		var req ollamaBody
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("the question was not readable as a chat request: %v", err)
		}
		var sent map[string]any
		if err := json.Unmarshal(raw, &sent); err != nil {
			t.Errorf("the question was not JSON: %v", err)
		}
		asked <- req
		extras <- sent
		if last := req.Messages[len(req.Messages)-1]; last.Role == roleTool {
			send(t, w, token("ls -la /tmp"), doneToken)
			return
		}
		send(t, w, toolCallStream(`{"command":"ls /tmp"}`), doneToken)
	})

	err := client.Agent(t.Context(), "what is taking space in tmp", nil,
		func(string) error { return nil },
		ignoreThought,
		func(_ context.Context, command string) (string, error) {
			return "a.txt", nil
		},
		nil)
	if err != nil {
		t.Fatalf("Agent: %v", err)
	}

	first := <-asked
	sent := <-extras
	if first.Think == nil || !*first.Think {
		t.Errorf("think = %v, want true: an agent question always thinks", first.Think)
	}
	if first.ReasoningEffort != "" {
		t.Errorf("reasoning_effort = %q, want empty when none was configured", first.ReasoningEffort)
	}
	if _, ok := sent["tool_choice"]; ok {
		t.Error("Ollama was sent tool_choice, which its Chat Completions surface does not take")
	}
	if _, ok := sent["chat_template_kwargs"]; ok {
		t.Error("Ollama was sent chat_template_kwargs, which it does not use")
	}
	if len(first.Tools) != 1 {
		t.Errorf("tools = %+v, want the shell command tool", first.Tools)
	}

	second := <-asked
	<-extras
	if second.Think == nil || !*second.Think {
		t.Errorf("the follow-up think = %v, want true", second.Think)
	}
}

func TestOllamaCommandStreamsTheThoughtFromReasoning(t *testing.T) {
	client := newOllamaTestClient(t, Options{}, func(w http.ResponseWriter, _ *http.Request) {
		send(t, w,
			ollamaReasoning("the user wants to "),
			ollamaReasoning("undo the last commit"),
			token("git reset --soft HEAD~1"),
			doneToken,
		)
	})

	var said, thoughts []string
	err := client.Command(t.Context(), "undo last commit", nil, true, func(command string) error {
		said = append(said, command)
		return nil
	}, func(thought string) error {
		thoughts = append(thoughts, thought)
		return nil
	})
	if err != nil {
		t.Fatalf("asking a thinking question: %v", err)
	}

	wantThoughts := []string{
		"the user wants to ",
		"the user wants to undo the last commit",
	}
	if !slices.Equal(thoughts, wantThoughts) {
		t.Errorf("the thought arrived as %q, want the whole of it so far each time", thoughts)
	}
	if want := []string{"git reset --soft HEAD~1"}; !slices.Equal(said, want) {
		t.Errorf("the command arrived as %q, want %q", said, want)
	}
}

func ollamaReasoning(content string) string {
	return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"reasoning\":%q}}]}\n\n", content)
}

func newOllamaTestClient(t *testing.T, opts Options, handle http.HandlerFunc) *Client {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != chatPath {
			t.Errorf("the question went to %q, want %q", r.URL.Path, chatPath)
		}
		handle(w, r)
	}))
	t.Cleanup(server.Close)
	opts.Endpoint = server.URL + chatPath
	opts.HTTP = server.Client()
	return NewOllama(opts)
}

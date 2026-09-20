package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestOpenAIDoesNotInventConnectionOptions(t *testing.T) {
	client := NewOpenAICompatible(Options{})

	if client.Name() != openAICompatibleName {
		t.Errorf("Name = %q, want openai-compatible", client.Name())
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

func TestOpenAIPostsToTheConfiguredCompletionsURL(t *testing.T) {
	client := newOpenAICompatibleTestClient(t, Options{Model: "gpt-4o"}, func(w http.ResponseWriter, _ *http.Request) {
		send(t, w, token("ls"), token(" -la"), doneToken)
	})

	said := collect(t, client, "list everything", nil)
	if want := []string{"ls", "ls -la"}; !slices.Equal(said, want) {
		t.Errorf("the command arrived as %q, want %q", said, want)
	}
}

func TestOpenAIPrewarmsAndReusesItsNativeCacheKey(t *testing.T) {
	bodies := make(chan map[string]any, 2)
	client := newOpenAICompatibleTestClient(t, Options{Model: "gpt-5-nano"}, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("the request was not readable as JSON: %v", err)
		}
		bodies <- body
		if stream, _ := body["stream"].(bool); stream {
			send(t, w, token("pwd"), doneToken)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[]}`))
	})

	transcript := []Turn{{Text: "$ pwd\n/tmp"}}
	if err := client.Prewarm(t.Context(), transcript, false, false); err != nil {
		t.Fatalf("prewarming OpenAI: %v", err)
	}
	collect(t, client, "where am i", transcript)

	warm, asked := <-bodies, <-bodies
	key, ok := warm["prompt_cache_key"].(string)
	if !ok || key == "" {
		t.Fatalf("the warmup cache key is %#v, want a stable non-empty key", warm["prompt_cache_key"])
	}
	if asked["prompt_cache_key"] != key {
		t.Errorf("the question cache key is %#v, want the warmup key %q", asked["prompt_cache_key"], key)
	}
	if got := warm["max_completion_tokens"]; got != float64(1024) {
		t.Errorf("warmup max_completion_tokens = %#v, want 1024", got)
	}
	if _, ok := warm["reasoning_effort"]; ok {
		t.Error("the warmup changed reasoning effort, which would change OpenAI's rendered cache prefix")
	}
	if _, ok := asked["max_completion_tokens"]; ok {
		t.Error("the real question kept the warmup's output limit")
	}
	for name, body := range map[string]map[string]any{"warmup": warm, "question": asked} {
		if _, ok := body["cache_prompt"]; ok {
			t.Errorf("OpenAI %s was sent llama.cpp's cache_prompt", name)
		}
	}
	messages, ok := warm["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("warmup messages = %#v, want system prompt and transcript only", warm["messages"])
	}
	last, _ := messages[len(messages)-1].(map[string]any)
	if last["content"] != transcript[0].Text {
		t.Errorf("the warmup ended in %#v, want the current transcript", last)
	}
}

func TestInvalidatePromptCacheRotatesOnlyNativeCacheKeys(t *testing.T) {
	openAI := NewOpenAICompatible(Options{})
	before := openAI.promptCacheKey()
	openAI.InvalidatePromptCache()
	after := openAI.promptCacheKey()
	if before == "" || after == "" || after == before {
		t.Errorf("OpenAI cache key rotated from %q to %q, want two distinct non-empty keys", before, after)
	}

	llama := NewLlama(Options{})
	llama.InvalidatePromptCache()
	if key := llama.promptCacheKey(); key != "" {
		t.Errorf("llama.cpp cache invalidation invented native cache key %q", key)
	}
}

func TestOpenAIAsksWithoutLlamaExtras(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	client := newOpenAICompatibleTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
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
		t.Error("OpenAI was sent chat_template_kwargs, which it rejects")
	}
	if _, ok := sent["add_generation_prompt"]; ok {
		t.Error("OpenAI was sent add_generation_prompt, which it rejects")
	}
	if _, ok := sent["think"]; ok {
		t.Error("OpenAI was sent think, which it rejects")
	}
	if _, ok := sent["cache_prompt"]; ok {
		t.Error("OpenAI was sent llama.cpp's cache_prompt")
	}
	for _, cap := range []string{"max_tokens", "max_completion_tokens"} {
		if _, ok := sent[cap]; ok {
			t.Errorf("OpenAI was sent %s = %v; aty does not cap the answer", cap, sent[cap])
		}
	}
	if _, ok := sent["reasoning_effort"]; ok {
		t.Error("OpenAI was sent reasoning_effort with none configured, which models that do not reason reject")
	}
	if _, ok := sent["temperature"]; ok {
		t.Error("OpenAI was sent temperature with none configured, which gpt-5-nano and the o-series reject")
	}
}

func TestOpenAISendsTheConfiguredTemperature(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	client := newOpenAICompatibleTestClient(t, Options{Temperature: new(0.2)}, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("the question was not readable as JSON: %v", err)
		}
		bodies <- body
		send(t, w, token("ls"), doneToken)
	})

	collect(t, client, "list everything in tmp", nil)

	sent := <-bodies
	if got, ok := sent["temperature"].(float64); !ok || got != 0.2 {
		t.Errorf("temperature = %#v, want the configured 0.2", sent["temperature"])
	}
}

func TestOpenAIStreamsTheThoughtFromReasoningContent(t *testing.T) {
	client := newOpenAICompatibleTestClient(t, Options{}, func(w http.ResponseWriter, _ *http.Request) {
		send(t, w,
			reasoning("the user wants to "),
			reasoning("list tmp"),
			token("ls -la /tmp"),
			doneToken,
		)
	})

	var said, thoughts []string
	err := client.Command(t.Context(), "list everything in tmp", nil, true, func(command string) error {
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
		"the user wants to list tmp",
	}
	if !slices.Equal(thoughts, wantThoughts) {
		t.Errorf("the thought arrived as %q, want the whole of it so far each time", thoughts)
	}
	if want := []string{"ls -la /tmp"}; !slices.Equal(said, want) {
		t.Errorf("the command arrived as %q, want %q", said, want)
	}
	for _, command := range said {
		for _, thought := range thoughts {
			if strings.Contains(command, thought) {
				t.Errorf("the thought %q leaked into the command: %q", thought, command)
			}
		}
	}
}

func TestOpenAIThinksWhenTheQuestionAsks(t *testing.T) {
	questions := make(chan compatibleBody, 1)
	client := newOpenAICompatibleTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		questions <- decode[compatibleBody](t, r)
		send(t, w, token("ls"), doneToken)
	})

	err := client.Command(t.Context(), "list everything in tmp", nil, true, func(string) error { return nil }, ignoreThought)
	if err != nil {
		t.Fatalf("asking a thinking question: %v", err)
	}

	asked := <-questions
	if asked.ReasoningEffort != "" {
		t.Errorf("reasoning_effort = %q, want empty when none was configured", asked.ReasoningEffort)
	}
}

func TestOpenAIAgentKeepsToolsAndDropsLlamaExtras(t *testing.T) {
	asked := make(chan compatibleBody, 2)
	client := newOpenAICompatibleTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		req := decode[compatibleBody](t, r)
		asked <- req
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
	if first.ReasoningEffort != "" {
		t.Errorf("reasoning_effort = %q, want empty when none was configured", first.ReasoningEffort)
	}
	if first.ToolChoice != toolChoiceAuto {
		t.Errorf("tool_choice = %q, want %q so the model may still answer in text", first.ToolChoice, toolChoiceAuto)
	}
	if len(first.Tools) != 1 {
		t.Errorf("tools = %+v, want the shell command tool", first.Tools)
	}

	second := <-asked
	if second.ReasoningEffort != "" {
		t.Errorf("the follow-up reasoning_effort = %q, want empty when none was configured", second.ReasoningEffort)
	}
}

func TestOpenAIThinksAtTheConfiguredEffort(t *testing.T) {
	questions := make(chan compatibleBody, 1)
	client := newOpenAICompatibleTestClient(t, Options{ReasoningEffort: "high"}, func(w http.ResponseWriter, r *http.Request) {
		questions <- decode[compatibleBody](t, r)
		send(t, w, token("ls"), doneToken)
	})

	err := client.Command(t.Context(), "list everything in tmp", nil, true, func(string) error { return nil }, ignoreThought)
	if err != nil {
		t.Fatalf("asking a thinking question: %v", err)
	}

	asked := <-questions
	if asked.ReasoningEffort != "high" {
		t.Errorf("reasoning_effort = %q, want the configured high", asked.ReasoningEffort)
	}
}

func TestOpenAISendsNoneWhenConfiguredButTheQuestionIsPlain(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	client := newOpenAICompatibleTestClient(t, Options{ReasoningEffort: "high"}, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("the question was not readable as JSON: %v", err)
		}
		bodies <- body
		send(t, w, token("ls"), doneToken)
	})

	collect(t, client, "list everything in tmp", nil)

	sent := <-bodies
	if got, ok := sent["reasoning_effort"].(string); !ok || got != reasoningNone {
		t.Errorf("reasoning_effort = %#v, want %q so a configured backend can disable thinking", sent["reasoning_effort"], reasoningNone)
	}
}

func TestOpenAISendsTheConfiguredKey(t *testing.T) {
	headers := make(chan string, 1)
	client := newOpenAICompatibleTestClient(t, Options{APIKey: "sk-openai"}, func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Get("Authorization")
		send(t, w, token("pwd"), doneToken)
	})

	collect(t, client, "where am i", nil)
	if got := <-headers; got != "Bearer sk-openai" {
		t.Errorf("Authorization = %q, want the configured bearer token", got)
	}
}

func newOpenAICompatibleTestClient(t *testing.T, opts Options, handle http.HandlerFunc) *Client {
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
	return NewOpenAICompatible(opts)
}

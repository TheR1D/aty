package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCommandStreamsTheAnswerAsItArrives(t *testing.T) {
	questions := make(chan compatibleBody, 1)
	client := newTestClient(t, Options{Model: "test-model"}, func(w http.ResponseWriter, r *http.Request) {
		questions <- read(t, r)
		send(t, w, token("ls"), token(" -la"), token(" /tmp"), doneToken)
	})

	said := collect(t, client, "list everything in tmp", nil)

	want := []string{"ls", "ls -la", "ls -la /tmp"}
	if !slices.Equal(said, want) {
		t.Errorf("the command arrived as %q, want %q", said, want)
	}
	asked := <-questions
	if asked.Model != "test-model" {
		t.Errorf("the question went to %q, want %q", asked.Model, "test-model")
	}
	if !asked.Stream {
		t.Error("the answer was not asked for as a stream, so the user waits for all of it before seeing any")
	}
	if len(asked.Messages) != 2 {
		t.Fatalf("the question carried %d messages, want the system prompt and the question", len(asked.Messages))
	}
	if asked.Messages[0].Role != roleSystem || asked.Messages[0].Content != client.system {
		t.Errorf("the first message is not the system prompt: %+v", asked.Messages[0])
	}
	if asked.Messages[1].Role != roleUser || asked.Messages[1].Content != "list everything in tmp" {
		t.Errorf("the second message is not the user's question: %+v", asked.Messages[1])
	}
}

func TestCommandDoesNotWaitForAnInFlightWarmup(t *testing.T) {
	requests := make(chan int, 2)
	releaseWarmup := make(chan struct{})
	var count atomic.Int32
	client := newTestClient(t, Options{}, func(w http.ResponseWriter, _ *http.Request) {
		n := int(count.Add(1))
		requests <- n
		if n == 1 {
			<-releaseWarmup
			_, _ = io.WriteString(w, `{"choices":[]}`)
			return
		}
		send(t, w, token("pwd"), doneToken)
	})

	warmed := make(chan error, 1)
	go func() {
		warmed <- client.Prewarm(t.Context(), nil, false, false)
	}()
	if got := <-requests; got != 1 {
		t.Fatalf("the first request is %d, want the warmup", got)
	}

	answered := make(chan error, 1)
	go func() {
		answered <- client.Command(t.Context(), "where am i", nil, false, func(string) error { return nil }, ignoreThought)
	}()
	select {
	case got := <-requests:
		if got != 2 {
			t.Fatalf("the concurrent request is %d, want the submitted question", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the submitted question waited for the cache request")
	}

	close(releaseWarmup)
	if err := <-warmed; err != nil {
		t.Fatalf("prewarming: %v", err)
	}
	if err := <-answered; err != nil {
		t.Fatalf("asking while the warmup was in flight: %v", err)
	}
}

func TestLlamaPrewarmEvaluatesThePromptIntoItsCache(t *testing.T) {
	body := make(chan map[string]any, 1)
	client := newTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		var sent map[string]any
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Errorf("the warmup was not readable as JSON: %v", err)
		}
		body <- sent
		_, _ = io.WriteString(w, `{"choices":[]}`)
	})

	if err := client.Prewarm(t.Context(), nil, false, false); err != nil {
		t.Fatalf("prewarming llama.cpp: %v", err)
	}
	sent := <-body
	if got := sent["max_tokens"]; got != float64(0) {
		t.Errorf("llama.cpp max_tokens = %#v, want 0 to prefill without generating", got)
	}
	if got, ok := sent["cache_prompt"].(bool); !ok || !got {
		t.Errorf("llama.cpp cache_prompt = %#v, want true", sent["cache_prompt"])
	}
	if messages, ok := sent["messages"].([]any); !ok || len(messages) != 1 {
		t.Errorf("warmup messages = %#v, want only the system prompt when the transcript is empty", sent["messages"])
	}
}

func TestLlamaQuestionsReuseThePromptCache(t *testing.T) {
	body := make(chan map[string]any, 1)
	client := newTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		var sent map[string]any
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Errorf("the question was not readable as JSON: %v", err)
		}
		body <- sent
		send(t, w, token("pwd"), doneToken)
	})

	collect(t, client, "where am i", nil)

	sent := <-body
	if got, ok := sent["cache_prompt"].(bool); !ok || !got {
		t.Errorf("llama.cpp cache_prompt = %#v, want true", sent["cache_prompt"])
	}
	if _, ok := sent["max_tokens"]; ok {
		t.Error("the real question kept the warmup's output limit")
	}
}

func TestCommandSendsEachCommandAsItsOwnUserTurn(t *testing.T) {
	questions := make(chan compatibleBody, 1)
	client := newTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		questions <- read(t, r)
		send(t, w, token("git push --force-with-lease"), doneToken)
	})

	transcript := []Turn{
		{Text: "$ git push\nerror: failed to push some refs"},
		{Text: "$ pwd\n/tmp"},
	}
	collect(t, client, "why did that fail", transcript)

	asked := <-questions
	if len(asked.Messages) != 4 {
		t.Fatalf("the question carried %d messages, want the system prompt, one turn per command and the question", len(asked.Messages))
	}
	if asked.Messages[0].Role != roleSystem {
		t.Errorf("the first message is not the system prompt: %+v", asked.Messages[0])
	}
	for i, turn := range transcript {
		if asked.Messages[i+1].Role != roleUser || asked.Messages[i+1].Content != turn.Text {
			t.Errorf("message %d is not the command's own turn: %+v", i+1, asked.Messages[i+1])
		}
	}
	if asked.Messages[3].Role != roleUser || asked.Messages[3].Content != "why did that fail" {
		t.Errorf("the last message is not the user's question: %+v", asked.Messages[3])
	}
}

func TestCommandKeepsTheAnswerAsAnAssistantTurn(t *testing.T) {
	questions := make(chan compatibleBody, 1)
	client := newTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		questions <- read(t, r)
		send(t, w, token("git push --force-with-lease"), doneToken)
	})

	transcript := []Turn{{
		Question: "push it anyway",
		Answer:   "git push",
		Text:     "$ git push\nerror: failed to push some refs",
	}}
	collect(t, client, "why did that fail", transcript)

	asked := <-questions
	if len(asked.Messages) != 5 {
		t.Fatalf("the question carried %d messages, want the system prompt, the question that produced the answer, the answer, what it printed and the follow-up", len(asked.Messages))
	}
	if asked.Messages[1].Role != roleUser || asked.Messages[1].Content != "push it anyway" {
		t.Errorf("the question that produced the answer is not a user turn of its own: %+v", asked.Messages[1])
	}
	if asked.Messages[2].Role != roleAssistant || asked.Messages[2].Content != "git push" {
		t.Errorf("the answer is not an assistant turn of its own: %+v", asked.Messages[2])
	}
	if asked.Messages[3].Role != roleUser || asked.Messages[3].Content != transcript[0].Text {
		t.Errorf("the command that ran is not the user turn after the answer: %+v", asked.Messages[3])
	}
	if asked.Messages[4].Role != roleUser || asked.Messages[4].Content != "why did that fail" {
		t.Errorf("the last message is not the user's question: %+v", asked.Messages[4])
	}
}

func TestCommandOmitsTheFieldsThatWereNotConfigured(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	client := newTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("the question was not readable as JSON: %v", err)
		}
		bodies <- body
		send(t, w, token("ls"), doneToken)
	})

	collect(t, client, "list everything in tmp", nil)

	sent := <-bodies
	for _, cap := range []string{"max_tokens", "max_completion_tokens"} {
		if _, ok := sent[cap]; ok {
			t.Errorf("the question carried %s = %v; aty does not cap the answer", cap, sent[cap])
		}
	}
	if _, ok := sent["reasoning_effort"]; ok {
		t.Error("the question named a reasoning effort with none configured")
	}
	if _, ok := sent["chat_template_kwargs"]; ok {
		t.Error("the question sent the removed llama-specific chat_template_kwargs")
	}
	if _, ok := sent["add_generation_prompt"]; ok {
		t.Error("the question suppressed the assistant header the model needs to answer")
	}
	if _, ok := sent["temperature"]; ok {
		t.Error("the question named a temperature with none configured")
	}
}

func TestCommandSendsTheConfiguredTemperature(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	client := newTestClient(t, Options{Temperature: new(0.2)}, func(w http.ResponseWriter, r *http.Request) {
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

func TestCommandSendsTemperatureZeroWhenConfigured(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	client := newTestClient(t, Options{Temperature: new(0.0)}, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("the question was not readable as JSON: %v", err)
		}
		bodies <- body
		send(t, w, token("ls"), doneToken)
	})

	collect(t, client, "list everything in tmp", nil)

	sent := <-bodies
	got, ok := sent["temperature"].(float64)
	if !ok || got != 0 {
		t.Errorf("temperature = %#v, want 0 so a configured zero is not treated as unset", sent["temperature"])
	}
}

func TestCommandThinksAtTheConfiguredEffort(t *testing.T) {
	questions := make(chan compatibleBody, 1)
	client := newTestClient(t, Options{ReasoningEffort: "high"}, func(w http.ResponseWriter, r *http.Request) {
		questions <- read(t, r)
		send(t, w, token("ls"), doneToken)
	})

	var said []string
	err := client.Command(t.Context(), "list everything in tmp", nil, true, func(command string) error {
		said = append(said, command)
		return nil
	}, ignoreThought)
	if err != nil {
		t.Fatalf("asking a thinking question: %v", err)
	}

	asked := <-questions
	if asked.ReasoningEffort != "high" {
		t.Errorf("reasoning_effort = %q, want high", asked.ReasoningEffort)
	}
}

// TestCommandKeepsEveryLineTheModelWrote is the end-of-stream protocol. A
// newline is part of a command rather than the end of one, so nothing is cut
// off the answer and nothing is scanned for: the command grows a line at a
// time and the stream ending is what finishes it. What keeps an explanation
// out of the user's prompt is the system prompt, not this.
func TestCommandKeepsEveryLineTheModelWrote(t *testing.T) {
	client := newTestClient(t, Options{}, func(w http.ResponseWriter, _ *http.Request) {
		send(t, w, token("cat <<'EOF' > note"), token("\nhello"), token("\nEOF"), doneToken)
	})

	said := collect(t, client, "write hello to note", nil)

	want := []string{
		"cat <<'EOF' > note",
		"cat <<'EOF' > note\nhello",
		"cat <<'EOF' > note\nhello\nEOF",
	}
	if !slices.Equal(said, want) {
		t.Errorf("the command arrived as %q, want %q", said, want)
	}
}

func TestCommandRefusesAnAnswerWithNothingInIt(t *testing.T) {
	client := newTestClient(t, Options{}, func(w http.ResponseWriter, _ *http.Request) {
		send(t, w, token("\n"), doneToken)
	})

	err := client.Command(t.Context(), "do something impossible", nil, false, func(string) error {
		t.Error("a command was typed at the shell for an answer that had none in it")
		return nil
	}, ignoreThought)
	if !errors.Is(err, ErrNoCommand) {
		t.Errorf("an empty answer failed with %v, want %v", err, ErrNoCommand)
	}
}

func TestCommandSaysWhatWentWrong(t *testing.T) {
	tests := []struct {
		name   string
		answer func(w http.ResponseWriter)
		want   string
	}{
		{
			name: "a model that is not loaded",
			answer: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":{"message":"model not found"}}`)
			},
			want: "model not found",
		},
		{
			name: "something on the port that is not llama-server",
			answer: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, "<html>proxy: upstream is down</html>")
			},
			want: "proxy: upstream is down",
		},
		{
			name: "a failure part way through the answer",
			answer: func(w http.ResponseWriter) {
				_, _ = io.WriteString(w, token("ls")+`data: {"error":{"message":"rate limited"}}`+"\n")
			},
			want: "rate limited",
		},
		{
			name: "an answer that is not the stream it claims to be",
			answer: func(w http.ResponseWriter) {
				_, _ = io.WriteString(w, "data: not json at all\n")
			},
			want: "reading the answer",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newTestClient(t, Options{Model: "test-model"}, func(w http.ResponseWriter, _ *http.Request) {
				test.answer(w)
			})

			err := client.Command(t.Context(), "list everything", nil, false, func(string) error { return nil }, ignoreThought)
			if err == nil {
				t.Fatalf("a failed question was reported as answered")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("the failure reads %q, which does not mention %q", err, test.want)
			}
		})
	}
}

func TestCommandStopsWhenTheQuestionIsTakenBack(t *testing.T) {
	client := newTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		send(t, w, token("git "))
		<-r.Context().Done()
	})

	ctx, takeBack := context.WithCancel(t.Context())
	defer takeBack()

	answered := make(chan error, 1)
	go func() {
		answered <- client.Command(ctx, "push the branch", nil, false, func(string) error {
			takeBack()
			return nil
		}, ignoreThought)
	}()

	select {
	case err := <-answered:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("a question taken back failed with %v, want it to report the cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a question taken back was still being answered")
	}
}

func TestCommandDoesNotBoundAnAnswerByADeadline(t *testing.T) {
	client := newTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Context().Deadline(); ok {
			t.Error("the answer carried a deadline, want the stream to run until llama finishes or the question is taken back")
		}
		send(t, w, token("git push"), doneToken)
	})

	err := client.Command(context.Background(), "push the branch", nil, false, func(string) error { return nil }, ignoreThought)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
}

func TestCommandStopsWhenTheAnswerIsRefused(t *testing.T) {
	tooLate := errors.New("the query is gone")
	client := newTestClient(t, Options{}, func(w http.ResponseWriter, _ *http.Request) {
		send(t, w, token("git"), token(" push"), doneToken)
	})

	typed := 0
	err := client.Command(t.Context(), "push the branch", nil, false, func(string) error {
		typed++
		return tooLate
	}, ignoreThought)
	if !errors.Is(err, tooLate) {
		t.Errorf("a refused answer failed with %v, want %v", err, tooLate)
	}
	if typed != 1 {
		t.Errorf("the answer went on being typed %d times after it was refused", typed-1)
	}
}

func TestTheHTTPSessionIsKeptAlive(t *testing.T) {
	client := New(Options{})
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok {
		t.Fatal("the session is not an HTTP transport, so keep-alives cannot be set")
	}
	if transport.DisableKeepAlives {
		t.Error("keep-alives are off, so each question pays for a new handshake")
	}
	if transport.IdleConnTimeout != 0 {
		t.Errorf("IdleConnTimeout = %v, want none so the session lasts the terminal", transport.IdleConnTimeout)
	}
	if transport.MaxIdleConnsPerHost != 1 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 1 so one connection is the session", transport.MaxIdleConnsPerHost)
	}
}

func TestQuestionsReuseOneHTTPConnection(t *testing.T) {
	var newConns atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != chatPath {
			t.Errorf("the question went to %q, want %q", r.URL.Path, chatPath)
		}
		send(t, w, token("pwd"), doneToken)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			newConns.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)

	client := New(Options{Endpoint: server.URL + chatPath})
	collect(t, client, "where am i", nil)
	collect(t, client, "where am i", nil)

	if got := newConns.Load(); got != 1 {
		t.Errorf("opened %d connections, want 1 so the second question reuses the session", got)
	}
}

func TestClientDoesNotInventConnectionOptions(t *testing.T) {
	client := New(Options{})

	if client.Name() != llamaName {
		t.Errorf("Name = %q, want %s", client.Name(), llamaName)
	}
	if client.endpoint != "" {
		t.Errorf("Endpoint = %q, want empty", client.endpoint)
	}
	if client.model != "" {
		t.Errorf("Model = %q, want empty", client.model)
	}
	if client.apiKey != "" {
		t.Error("a local llama-server was given a key nobody configured")
	}
	if client.system == "" {
		t.Error("questions carry no system prompt, so nothing constrains the answer to a command")
	}
}

func TestCommandAsksWithoutAKey(t *testing.T) {
	headers := make(chan string, 1)
	client := newTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Get("Authorization")
		send(t, w, token("pwd"), doneToken)
	})

	said := collect(t, client, "where am i", nil)
	if want := []string{"pwd"}; !slices.Equal(said, want) {
		t.Errorf("the command arrived as %q, want %q", said, want)
	}
	if got := <-headers; got != "" {
		t.Errorf("Authorization = %q, want none on a local llama-server", got)
	}
}

func TestCommandSendsAnExplicitKey(t *testing.T) {
	headers := make(chan string, 1)
	client := newTestClient(t, Options{APIKey: "sk-server"}, func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Get("Authorization")
		send(t, w, token("pwd"), doneToken)
	})

	collect(t, client, "where am i", nil)
	if got := <-headers; got != "Bearer sk-server" {
		t.Errorf("Authorization = %q, want the configured bearer token", got)
	}
}

func TestVersionIsAReachabilityCheck(t *testing.T) {
	client := newTestServer(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != modelsPath || r.Method != http.MethodGet {
			t.Errorf("reachability was checked with %s %s, want GET %s", r.Method, r.URL.Path, modelsPath)
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"qwen3.5:9b"}]}`)
	})

	got, err := client.Version(t.Context())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if got != "" {
		t.Errorf("Version = %q, want empty: llama-server has no version string", got)
	}
}

func TestVersionSaysWhenLlamaIsUnreachable(t *testing.T) {
	client := New(Options{Endpoint: "http://127.0.0.1:1"})

	_, err := client.Version(t.Context())
	if err == nil {
		t.Fatal("an unreachable llama-server was reported as reachable")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("the failure reads %q, which does not name the endpoint", err)
	}
}

func TestModelsListsWhatLlamaHasLoaded(t *testing.T) {
	client := newTestServer(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != modelsPath || r.Method != http.MethodGet {
			t.Errorf("loaded models were listed with %s %s, want GET %s", r.Method, r.URL.Path, modelsPath)
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"qwen3.5:9b"},{"id":"other"}]}`)
	})

	got, err := client.Models(t.Context())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	want := []string{"qwen3.5:9b", "other"}
	if !slices.Equal(got, want) {
		t.Errorf("Models = %q, want %q", got, want)
	}
}

func TestModelsURLIsTheSiblingOfChatCompletions(t *testing.T) {
	tests := []struct{ in, want string }{
		{in: "http://127.0.0.1:8081/v1/chat/completions", want: "http://127.0.0.1:8081/v1/models"},
		{in: "http://127.0.0.1:8081/v1/chat/completions/", want: "http://127.0.0.1:8081/v1/models"},
		{in: "http://127.0.0.1:1", want: "http://127.0.0.1:1/models"},
	}
	for _, test := range tests {
		if got := modelsURL(test.in); got != test.want {
			t.Errorf("modelsURL(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

func TestInstalledMatchesAListedName(t *testing.T) {
	listed := []string{"qwen3.5:9b", "other"}
	tests := []struct {
		name string
		want bool
	}{
		{name: "qwen3.5:9b", want: true},
		{name: "other", want: true},
		{name: "missing", want: false},
		{name: "", want: false},
	}
	for _, test := range tests {
		if got := Installed(test.name, listed); got != test.want {
			t.Errorf("Installed(%q) = %v, want %v", test.name, got, test.want)
		}
	}
}

func TestCommandStreamsTheThoughtAsItArrives(t *testing.T) {
	client := newTestClient(t, Options{}, func(w http.ResponseWriter, _ *http.Request) {
		send(t, w,
			reasoning("the user wants to "),
			reasoning("undo the last commit"),
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
	for _, command := range said {
		for _, thought := range thoughts {
			if strings.Contains(command, thought) {
				t.Errorf("the thought %q leaked into the command: %q", thought, command)
			}
		}
	}
}

func TestCommandIgnoresKeepAlivesAndRoleOnlyChunks(t *testing.T) {
	client := newTestClient(t, Options{}, func(w http.ResponseWriter, _ *http.Request) {
		send(t, w,
			": keep-alive\n",
			"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
			token("pwd"),
			doneToken,
		)
	})

	said := collect(t, client, "where am i", nil)
	if want := []string{"pwd"}; !slices.Equal(said, want) {
		t.Errorf("the command arrived as %q, want %q", said, want)
	}
}

func newTestClient(t *testing.T, opts Options, handle http.HandlerFunc) *Client {
	t.Helper()

	return newTestServer(t, opts, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != chatPath {
			t.Errorf("the question went to %q, want %q", r.URL.Path, chatPath)
		}
		handle(w, r)
	})
}

func newTestServer(t *testing.T, opts Options, handle http.HandlerFunc) *Client {
	t.Helper()

	server := httptest.NewServer(handle)
	t.Cleanup(server.Close)
	opts.Endpoint = server.URL + chatPath
	opts.HTTP = server.Client()
	return New(opts)
}

func ignoreThought(string) error { return nil }

func collect(t *testing.T, client Asker, question string, transcript []Turn) []string {
	t.Helper()

	var said []string
	if err := client.Command(t.Context(), question, transcript, false, func(command string) error {
		said = append(said, command)
		return nil
	}, ignoreThought); err != nil {
		t.Fatalf("asking %q: %v", question, err)
	}
	return said
}

func decode[T any](t *testing.T, r *http.Request) T {
	t.Helper()

	var asked T
	if err := json.NewDecoder(r.Body).Decode(&asked); err != nil {
		t.Errorf("the question was not readable as a chat request: %v", err)
	}
	return asked
}

func read(t *testing.T, r *http.Request) compatibleBody {
	t.Helper()
	return decode[compatibleBody](t, r)
}

func send(t *testing.T, w http.ResponseWriter, parts ...string) {
	t.Helper()

	flush := http.NewResponseController(w)
	for _, part := range parts {
		if _, err := io.WriteString(w, part); err != nil {
			t.Errorf("writing the answer: %v", err)
			return
		}
		if err := flush.Flush(); err != nil {
			t.Errorf("flushing the answer: %v", err)
			return
		}
	}
}

func token(content string) string {
	return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", content)
}

func reasoning(content string) string {
	return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"reasoning_content\":%q}}]}\n\n", content)
}

const doneToken = "data: [DONE]\n\n"

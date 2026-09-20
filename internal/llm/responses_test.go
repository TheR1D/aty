package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOpenAINativeSendsResponsesOptionsWithoutInventingModelSettings(t *testing.T) {
	for _, test := range []struct {
		name        string
		effort      string
		thinking    bool
		temperature *float64
	}{
		{name: "defaults"},
		{name: "thinking without effort", thinking: true},
		{name: "plain configured effort", effort: "high"},
		{name: "thinking configured effort", effort: "high", thinking: true},
		{name: "explicit none", effort: "none", thinking: true},
		{name: "explicit zero temperature", temperature: new(0.0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			bodies := make(chan map[string]any, 1)
			client := newNativeTestClient(t, Options{
				Model: "test-model", APIKey: "test-native-key",
				ReasoningEffort: test.effort, Temperature: test.temperature,
			}, func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Bearer test-native-key" {
					t.Errorf("Authorization = %q, want the configured key", got)
				}
				bodies <- decode[map[string]any](t, r)
				sendNativeText(t, w, "pwd")
			})

			if err := client.Command(t.Context(), "where am i", nil, test.thinking,
				ignoreThought, ignoreThought); err != nil {
				t.Fatal(err)
			}
			body := <-bodies
			if client.Name() != "openai-native" {
				t.Errorf("Name = %q, want openai-native", client.Name())
			}
			if body["model"] != "test-model" || body["stream"] != true || body["store"] != false {
				t.Errorf("Responses request model/stream/store = %#v/%#v/%#v", body["model"], body["stream"], body["store"])
			}
			for _, field := range []string{"messages", "reasoning_effort", "think", "cache_prompt", "max_tokens", "max_completion_tokens", "max_output_tokens", "previous_response_id"} {
				if _, ok := body[field]; ok {
					t.Errorf("native command unexpectedly sends %s", field)
				}
			}
			if test.effort == "" {
				if _, exists := body["reasoning"]; exists {
					t.Errorf("reasoning = %#v, want absent when not configured", body["reasoning"])
				}
			} else if reasoning, ok := body["reasoning"].(map[string]any); !ok || reasoning["effort"] != test.effort {
				t.Errorf("reasoning = %#v, want effort %q even for a plain question", body["reasoning"], test.effort)
			} else if test.thinking && test.effort != "none" {
				if reasoning["summary"] != "auto" {
					t.Errorf("reasoning.summary = %#v, want auto for configured thinking", reasoning["summary"])
				}
			} else if _, exists := reasoning["summary"]; exists {
				t.Errorf("reasoning.summary = %#v, want absent outside configured thinking", reasoning["summary"])
			}
			if test.temperature == nil {
				if _, exists := body["temperature"]; exists {
					t.Errorf("temperature = %#v, want absent", body["temperature"])
				}
			} else if body["temperature"] != *test.temperature {
				t.Errorf("temperature = %#v, want %v", body["temperature"], *test.temperature)
			}
			input := nativeInput(t, body)
			if len(input) != 2 || input[0]["role"] != "system" || input[1]["role"] != "user" || input[1]["content"] != "where am i" {
				t.Errorf("input = %#v, want the system prompt and user's question", input)
			}
		})
	}
}

func TestOpenAINativeStreamsCommandAndReasoningSeparately(t *testing.T) {
	client := newNativeTestClient(t, Options{ReasoningEffort: "high"}, func(w http.ResponseWriter, _ *http.Request) {
		send(t, w,
			nativeEvent("response.reasoning_summary_text.delta", map[string]any{"item_id": "rs_1", "output_index": 0, "summary_index": 0, "delta": "Check "}),
			nativeEvent("response.reasoning_summary_text.delta", map[string]any{"item_id": "rs_1", "output_index": 0, "summary_index": 0, "delta": "the directory."}),
			nativeTextDelta("ls"), nativeTextDelta(" -la"),
			nativeCompleted(nativeReasoningItem(), nativeMessageItem("msg_1", "ls -la")),
		)
	})
	var commands, thoughts []string
	if err := client.Command(t.Context(), "list files", nil, true,
		func(command string) error { commands = append(commands, command); return nil },
		func(thought string) error { thoughts = append(thoughts, thought); return nil }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(commands, []string{"ls", "ls -la"}) {
		t.Errorf("commands = %q, want streamed cumulative command", commands)
	}
	if !slices.Equal(thoughts, []string{"Check ", "Check the directory."}) {
		t.Errorf("thoughts = %q, want streamed cumulative summary", thoughts)
	}
}

func TestOpenAINativePrewarmReusesAndInvalidatesCacheKey(t *testing.T) {
	bodies := make(chan map[string]any, 3)
	client := newNativeTestClient(t, Options{ReasoningEffort: "high"}, func(w http.ResponseWriter, r *http.Request) {
		body := decode[map[string]any](t, r)
		bodies <- body
		if body["stream"] == true {
			sendNativeText(t, w, "pwd")
		} else {
			_, _ = w.Write([]byte(`{"id":"resp_warm","status":"completed","output":[]}`))
		}
	})
	transcript := []Turn{{Text: "$ pwd\n/tmp"}}
	if err := client.Prewarm(t.Context(), transcript, false, false); err != nil {
		t.Fatal(err)
	}
	collect(t, client, "where am i", transcript)
	client.InvalidatePromptCache()
	collect(t, client, "where am i", transcript)
	warm, command, invalidated := <-bodies, <-bodies, <-bodies
	key, _ := warm["prompt_cache_key"].(string)
	if key == "" || command["prompt_cache_key"] != key || invalidated["prompt_cache_key"] == key {
		t.Errorf("cache keys = %#v, %#v, %#v; want stable until invalidated", warm["prompt_cache_key"], command["prompt_cache_key"], invalidated["prompt_cache_key"])
	}
	if warm["max_output_tokens"] != float64(1024) {
		t.Errorf("warmup max_output_tokens = %#v, want 1024", warm["max_output_tokens"])
	}
	if _, exists := command["max_output_tokens"]; exists {
		t.Error("the real question retained the warmup's output cap")
	}
	if !reflect.DeepEqual(warm["reasoning"], command["reasoning"]) {
		t.Errorf("warmup reasoning differs from the submitted question: %#v / %#v", warm["reasoning"], command["reasoning"])
	}
	if input := nativeInput(t, warm); len(input) != 2 || input[1]["content"] != transcript[0].Text {
		t.Errorf("warmup input = %#v, want only system prompt and transcript", input)
	}
}

func TestOpenAINativeAgentReplaysOutputItemsAndUsesSharedToolApproval(t *testing.T) {
	requests := make(chan map[string]any, 3)
	var count atomic.Int32
	reasoningItem := nativeReasoningItem()
	messageItem := nativeMessageItem("msg_before_tool", "Inspecting.")
	shellItem := nativeCallItem("fc_shell", "call_shell", toolSuggestShellCommand, `{"command":"pwd"}`)
	externalItem := nativeCallItem("fc_mcp", "call_mcp", "inspect_file", `{"path":"/tmp/a"}`)
	var activity, emitted, thoughts []string
	client := newNativeTestClient(t, Options{
		ReasoningEffort: "high",
		Tools:           []Tool{{Name: "inspect_file", Description: "Inspect a path.", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"detail":{"type":"boolean"}},"required":["path"]}`)}},
		CallTool: func(_ context.Context, name string, arguments json.RawMessage) (string, error) {
			activity = append(activity, "call "+name+" "+string(arguments))
			return "file details", nil
		},
	}, func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		if n > 3 {
			http.Error(w, "unexpected extra agent request", http.StatusInternalServerError)
			return
		}
		requests <- decode[map[string]any](t, r)
		switch n {
		case 1:
			send(t, w,
				nativeEvent("response.output_item.added", map[string]any{"output_index": 0, "item": reasoningItem}),
				nativeEvent("response.reasoning_summary_text.delta", map[string]any{"item_id": "rs_1", "output_index": 0, "summary_index": 0, "delta": "Find the directory."}),
				nativeEvent("response.output_item.done", map[string]any{"output_index": 0, "item": reasoningItem}),
				nativeEvent("response.output_item.added", map[string]any{"output_index": 1, "item": messageItem}),
				nativeEvent("response.output_item.done", map[string]any{"output_index": 1, "item": messageItem}),
				nativeCallAdded(2, shellItem),
				nativeArgumentsDelta(2, "fc_shell", `{"command":"p`),
				nativeArgumentsDelta(2, "fc_shell", `wd"}`),
				nativeEvent("response.output_item.done", map[string]any{"output_index": 2, "item": shellItem}),
				nativeCompleted(reasoningItem, messageItem, shellItem),
			)
		case 2:
			send(t, w, nativeCallAdded(0, externalItem), nativeArgumentsDelta(0, "fc_mcp", externalItem["arguments"].(string)),
				nativeEvent("response.output_item.done", map[string]any{"output_index": 0, "item": externalItem}), nativeCompleted(externalItem))
		default:
			sendNativeText(t, w, "Done.")
		}
	})
	if err := client.Agent(t.Context(), "inspect the current folder", nil,
		func(command string) error { emitted = append(emitted, command); return nil },
		func(thought string) error { thoughts = append(thoughts, thought); return nil },
		func(_ context.Context, command string) (string, error) {
			activity = append(activity, "run "+command)
			return "/tmp", nil
		},
		func(_ context.Context, name string, arguments json.RawMessage) error {
			activity = append(activity, "approve "+name+" "+string(arguments))
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(activity, []string{"run pwd", `approve inspect_file {"path":"/tmp/a"}`, `call inspect_file {"path":"/tmp/a"}`}) {
		t.Errorf("activity = %q, want shell execution then approved MCP call", activity)
	}
	if !slices.Contains(emitted, "p") || !slices.Contains(emitted, "pwd") || emitted[len(emitted)-1] != "# Done." {
		t.Errorf("emitted = %q, want command fragments and a final shell comment", emitted)
	}
	if !slices.Contains(thoughts, "Find the directory.") {
		t.Errorf("thoughts = %q, want the separate reasoning summary", thoughts)
	}
	first, second, third := <-requests, <-requests, <-requests
	toolSpecs, ok := first["tools"].([]any)
	if !ok || len(toolSpecs) != 2 {
		t.Fatalf("tools = %#v, want shell and MCP tools", first["tools"])
	}
	for _, raw := range toolSpecs {
		tool := raw.(map[string]any)
		if tool["type"] != "function" || tool["strict"] != false || tool["name"] == nil || tool["parameters"] == nil {
			t.Errorf("tool = %#v, want flat Responses function schema with strict:false", tool)
		}
		if _, nested := tool["function"]; nested {
			t.Errorf("Responses tool retained a Chat Completions function wrapper: %#v", tool)
		}
		if tool["name"] == "inspect_file" {
			parameters := tool["parameters"].(map[string]any)
			if !reflect.DeepEqual(parameters["required"], []any{"path"}) {
				t.Errorf("MCP required fields = %#v, want optional detail left optional", parameters["required"])
			}
		}
	}
	secondInput := nativeInput(t, second)
	wantSecond := []map[string]any{reasoningItem, messageItem, shellItem, {"type": "function_call_output", "call_id": "call_shell", "output": "/tmp"}}
	if len(secondInput) != 6 || !reflect.DeepEqual(secondInput[2:], wantSecond) {
		t.Errorf("second input = %#v, want every output item preserved before its tool result", secondInput)
	}
	thirdInput := nativeInput(t, third)
	if len(thirdInput) != 8 || !reflect.DeepEqual(thirdInput[:6], secondInput) || !reflect.DeepEqual(thirdInput[6], externalItem) || !reflect.DeepEqual(thirdInput[7], map[string]any{"type": "function_call_output", "call_id": "call_mcp", "output": "file details"}) {
		t.Errorf("third input = %#v, want prior input plus the MCP call and result", thirdInput)
	}
}

func TestOpenAINativeAgentRequiresCompletedResponseBeforeRunningTools(t *testing.T) {
	call := nativeCallItem("fc_1", "call_1", toolSuggestShellCommand, `{"command":"pwd"}`)
	for _, test := range []struct {
		name   string
		suffix string
	}{
		{name: "truncated stream"},
		{name: "chat done marker", suffix: doneToken},
		{name: "failed", suffix: nativeEvent("response.failed", map[string]any{"response": map[string]any{"status": "failed", "error": map[string]any{"message": "model failed"}}})},
		{name: "incomplete", suffix: nativeEvent("response.incomplete", map[string]any{"response": map[string]any{"status": "incomplete", "incomplete_details": map[string]any{"reason": "max_output_tokens"}}})},
		{name: "error", suffix: nativeEvent("error", map[string]any{"message": "bad request", "code": "invalid_request"})},
		{name: "refusal", suffix: nativeEvent("response.refusal.delta", map[string]any{"item_id": "msg_refusal", "output_index": 1, "content_index": 0, "delta": "I cannot do that."}) + nativeCompleted(call)},
	} {
		t.Run(test.name, func(t *testing.T) {
			var ran bool
			client := newNativeTestClient(t, Options{}, func(w http.ResponseWriter, _ *http.Request) {
				send(t, w, nativeCallAdded(0, call), nativeArgumentsDelta(0, "fc_1", call["arguments"].(string)),
					nativeEvent("response.output_item.done", map[string]any{"output_index": 0, "item": call}), test.suffix)
			})
			err := client.Agent(t.Context(), "where am i", nil, ignoreThought, ignoreThought,
				func(context.Context, string) (string, error) { ran = true; return "", nil }, nil)
			if err == nil || ran {
				t.Errorf("Agent error = %v, ran = %v; want a rejected response and no execution", err, ran)
			}
		})
	}
}

func TestOpenAINativeCancellationStopsBeforeToolExecution(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	call := nativeCallItem("fc_1", "call_1", toolSuggestShellCommand, `{"command":"pwd"}`)
	client := newNativeTestClient(t, Options{}, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(nativeCallAdded(0, call) + nativeArgumentsDelta(0, "fc_1", call["arguments"].(string)) + nativeCompleted(call)))
	})
	var ran bool
	err := client.Agent(ctx, "where am i", nil, func(string) error { cancel(); return context.Canceled }, ignoreThought,
		func(context.Context, string) (string, error) { ran = true; return "", nil }, nil)
	if !errors.Is(err, context.Canceled) || ran {
		t.Errorf("Agent error = %v, ran = %v; want cancellation and no execution", err, ran)
	}
}

func TestOpenAINativeApprovalCancellationDoesNotCallMCP(t *testing.T) {
	denied := errors.New("approval cancelled")
	var ran bool
	call := nativeCallItem("fc_1", "call_1", "inspect_file", `{}`)
	client := newNativeTestClient(t, Options{
		Tools:    []Tool{{Name: "inspect_file", Parameters: json.RawMessage(`{"type":"object"}`)}},
		CallTool: func(context.Context, string, json.RawMessage) (string, error) { ran = true; return "", nil },
	}, func(w http.ResponseWriter, _ *http.Request) {
		send(t, w, nativeCallAdded(0, call), nativeArgumentsDelta(0, "fc_1", `{}`), nativeCompleted(call))
	})
	err := client.Agent(t.Context(), "inspect", nil, ignoreThought, ignoreThought, nil,
		func(context.Context, string, json.RawMessage) error { return denied })
	if !errors.Is(err, denied) || ran {
		t.Errorf("Agent error = %v, MCP called = %v; want approval cancellation without calling MCP", err, ran)
	}
}

func TestOpenAINativeConvertsTranscriptToolCalls(t *testing.T) {
	requests := make(chan map[string]any, 1)
	client := newNativeTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		requests <- decode[map[string]any](t, r)
		sendNativeText(t, w, "ls")
	})
	transcript := []Turn{{Question: "where am i", Answer: "pwd", Text: "/tmp", Tool: true}}
	collect(t, client, "list it", transcript)
	input := nativeInput(t, <-requests)
	if len(input) != 5 {
		t.Fatalf("input = %#v, want system, previous question, function call, result and current question", input)
	}
	if input[2]["type"] != "function_call" || input[2]["name"] != toolSuggestShellCommand || input[2]["arguments"] != `{"command":"pwd"}` {
		t.Errorf("historical call = %#v, want a native function_call", input[2])
	}
	if input[3]["type"] != "function_call_output" || input[3]["call_id"] != input[2]["call_id"] || input[3]["output"] != "/tmp" {
		t.Errorf("historical result = %#v, want a matching native function_call_output", input[3])
	}
}

func TestOpenAINativeAgentHonorsOutputLimitAndStepCap(t *testing.T) {
	requests := make(chan map[string]any, 2)
	var count atomic.Int32
	var ran int
	call := nativeCallItem("fc_1", "call_1", toolSuggestShellCommand, `{"command":"pwd"}`)
	client := newNativeTestClient(t, Options{AgentSteps: 1, ToolOutputBytes: 80}, func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		if n > 2 {
			http.Error(w, "unexpected extra agent request", http.StatusInternalServerError)
			return
		}
		requests <- decode[map[string]any](t, r)
		if n == 1 {
			send(t, w, nativeCallAdded(0, call), nativeArgumentsDelta(0, "fc_1", call["arguments"].(string)), nativeCompleted(call))
		} else {
			sendNativeText(t, w, "Finished.")
		}
	})
	if err := client.Agent(t.Context(), "inspect", nil, ignoreThought, ignoreThought,
		func(context.Context, string) (string, error) { ran++; return strings.Repeat("x", 1000), nil }, nil); err != nil {
		t.Fatal(err)
	}
	first, last := <-requests, <-requests
	if first["tools"] == nil || last["tools"] != nil || last["tool_choice"] != "none" || ran != 1 {
		t.Errorf("first tools = %#v, last tools = %#v, last choice = %#v, ran = %d; want one permitted tool round", first["tools"], last["tools"], last["tool_choice"], ran)
	}
	input := nativeInput(t, last)
	output, _ := input[len(input)-1]["output"].(string)
	if output != toolResult(strings.Repeat("x", 1000), 80) {
		t.Errorf("tool output = %q, want the shared bounded result", output)
	}
}

func TestResponsesDoNotChangeOtherProviders(t *testing.T) {
	for _, test := range []struct {
		name string
		new  func(Options) *Client
	}{
		{name: "llama", new: NewLlama},
		{name: "ollama", new: NewOllama},
		{name: "openai-compatible", new: NewOpenAICompatible},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := make(chan map[string]any, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					t.Errorf("path = %q, want the configured Chat Completions endpoint", r.URL.Path)
				}
				requests <- decode[map[string]any](t, r)
				send(t, w, token("pwd"), doneToken)
			}))
			t.Cleanup(server.Close)
			client := test.new(Options{Endpoint: server.URL + "/v1/chat/completions", HTTP: server.Client(), ReasoningEffort: "high"})
			collect(t, client, "where am i", nil)
			body := <-requests
			if body["reasoning_effort"] != "none" || body["messages"] == nil {
				t.Errorf("request = %#v, want the existing normal-mode Chat Completions behavior", body)
			}
			for _, field := range []string{"input", "store", "reasoning", "include", "max_output_tokens", "previous_response_id"} {
				if _, exists := body[field]; exists {
					t.Errorf("Responses field %s leaked to %s", field, test.name)
				}
			}
		})
	}
}

func newNativeTestClient(t *testing.T, opts Options, handle http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			t.Errorf("request = %s %s, want POST /v1/responses", r.Method, r.URL.Path)
		}
		handle(w, r)
	}))
	t.Cleanup(server.Close)
	opts.Endpoint, opts.HTTP = server.URL+"/v1/responses", server.Client()
	return NewOpenAINative(opts)
}

func nativeInput(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	items, ok := body["input"].([]any)
	if !ok {
		t.Fatalf("input = %#v, want an array of Responses items", body["input"])
	}
	result := make([]map[string]any, len(items))
	for i, item := range items {
		result[i], ok = item.(map[string]any)
		if !ok {
			t.Fatalf("input[%d] = %#v, want an item object", i, item)
		}
	}
	return result
}

func nativeEvent(kind string, fields map[string]any) string {
	fields["type"] = kind
	raw, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return "event: " + kind + "\ndata: " + string(raw) + "\n\n"
}

func nativeTextDelta(text string) string {
	return nativeEvent("response.output_text.delta", map[string]any{"item_id": "msg_1", "output_index": 0, "content_index": 0, "delta": text})
}

func nativeMessageItem(id, text string) map[string]any {
	return map[string]any{"id": id, "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
}

func nativeReasoningItem() map[string]any {
	return map[string]any{"id": "rs_1", "type": "reasoning", "summary": []any{}, "encrypted_content": "opaque-encrypted-reasoning"}
}

func nativeCallItem(id, callID, name, arguments string) map[string]any {
	return map[string]any{"id": id, "type": "function_call", "status": "completed", "call_id": callID, "name": name, "arguments": arguments}
}

func nativeCallAdded(index int, call map[string]any) string {
	item := make(map[string]any, len(call))
	for key, value := range call {
		item[key] = value
	}
	item["status"], item["arguments"] = "in_progress", ""
	return nativeEvent("response.output_item.added", map[string]any{"output_index": index, "item": item})
}

func nativeArgumentsDelta(index int, id, arguments string) string {
	return nativeEvent("response.function_call_arguments.delta", map[string]any{"item_id": id, "output_index": index, "delta": arguments})
}

func nativeCompleted(items ...map[string]any) string {
	return nativeEvent("response.completed", map[string]any{"response": map[string]any{"id": "resp_test", "status": "completed", "output": items}})
}

func sendNativeText(t *testing.T, w http.ResponseWriter, text string) {
	t.Helper()
	send(t, w, nativeTextDelta(text), nativeCompleted(nativeMessageItem("msg_1", text)))
}

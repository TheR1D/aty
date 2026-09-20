package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	appconfig "github.com/TheR1D/aty/internal/config"
)

func TestSuggestShellCommandToolIsTheSpecAnAgentQuestionSends(t *testing.T) {
	raw, err := json.Marshal(suggestShellCommandTool)
	if err != nil {
		t.Fatalf("marshaling the tool spec: %v", err)
	}

	var got struct {
		Type     string `json:"type"`
		Function struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Parameters  struct {
				Type       string   `json:"type"`
				Required   []string `json:"required"`
				Properties map[string]struct {
					Type string `json:"type"`
				} `json:"properties"`
			} `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the tool spec was not the JSON Chat Completions expects: %v\n%s", err, raw)
	}
	if got.Type != "function" {
		t.Errorf("type = %q, want function", got.Type)
	}
	if got.Function.Name != toolSuggestShellCommand {
		t.Errorf("name = %q, want %q", got.Function.Name, toolSuggestShellCommand)
	}
	if got.Function.Description != suggestShellCommandDescription {
		t.Errorf("description = %q, want %q", got.Function.Description, suggestShellCommandDescription)
	}
	if got.Function.Parameters.Type != "object" {
		t.Errorf("parameters.type = %q, want object", got.Function.Parameters.Type)
	}
	if _, ok := got.Function.Parameters.Properties["command"]; !ok {
		t.Error("the tool has no command parameter, so there is nothing to run")
	}
	if len(got.Function.Parameters.Required) != 1 || got.Function.Parameters.Required[0] != "command" {
		t.Errorf("required = %q, want [command]", got.Function.Parameters.Required)
	}
}

func TestWithToolsAttachesTheShellTool(t *testing.T) {
	req := withTools(chatRequest{Model: "test-model"}, buildToolSpecs(nil))
	if req.ToolChoice != toolChoiceAuto {
		t.Errorf("tool_choice = %q, want %q so the model may answer in text", req.ToolChoice, toolChoiceAuto)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != toolSuggestShellCommand {
		t.Errorf("tools = %+v, want the shell command tool", req.Tools)
	}

	raw, err := encodeOpenAICompatible(req)
	if err != nil {
		t.Fatalf("marshaling an agent request: %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatalf("the agent request was not JSON: %v", err)
	}
	if sent["tool_choice"] != toolChoiceAuto {
		t.Errorf("tool_choice = %#v, want %q", sent["tool_choice"], toolChoiceAuto)
	}
	if _, ok := sent["tools"]; !ok {
		t.Error("an agent request carried no tools")
	}
}

func TestWithoutToolsForcesATextAnswer(t *testing.T) {
	req := withoutTools(withTools(chatRequest{Model: "test-model"}, buildToolSpecs(nil)))
	if req.ToolChoice != toolChoiceNone {
		t.Errorf("tool_choice = %q, want %q so the overflow step cannot call the tool", req.ToolChoice, toolChoiceNone)
	}
	if req.Tools != nil {
		t.Errorf("tools = %+v, want none on the overflow step", req.Tools)
	}
}

// TestTheAgentsLastWordIsAlwaysAComment pins the guard that stands between
// a model that reported in prose and the user's line editor: an agent has
// already done the work, so nothing it says at the end may be a command
// sitting on the prompt waiting for Enter.
func TestTheAgentsLastWordIsAlwaysAComment(t *testing.T) {
	tests := []struct{ answer, want string }{
		{answer: "Done. Created notes/a.md, b.md and c.md.", want: "# Done. Created notes/a.md, b.md and c.md."},
		{answer: "# already a comment", want: "# already a comment"},
		{answer: "#no space", want: "#no space"},
		{answer: "rm -rf /tmp/scratch", want: "# rm -rf /tmp/scratch"},
		// Nothing said is nothing typed; ErrNoCommand is what an empty
		// answer becomes, and a bare "# " would hide it.
		{answer: "", want: ""},
	}

	for _, test := range tests {
		if got := reportLine(test.answer); got != test.want {
			t.Errorf("reportLine(%q) = %q, want %q", test.answer, got, test.want)
		}
	}
}

// TestAReportIsCommentedFromTheFirstPieceOfIt covers the streaming half:
// the line is typed as it arrives, so it has to be a comment from the
// first character rather than become one at the end, or the user watches a
// command appear and only then be rewritten.
func TestAReportIsCommentedFromTheFirstPieceOfIt(t *testing.T) {
	client := newTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		send(t, w, token("Done"), token(", wrote"), token(" three files"), doneToken)
	})

	var said []string
	err := client.Agent(t.Context(), "write three files", nil,
		func(command string) error {
			said = append(said, command)
			return nil
		},
		ignoreThought,
		func(context.Context, string) (string, error) {
			return "", nil
		},
		nil)
	if err != nil {
		t.Fatalf("Agent: %v", err)
	}

	want := []string{"# Done", "# Done, wrote", "# Done, wrote three files"}
	if !slices.Equal(said, want) {
		t.Errorf("the report was typed as %q, want %q", said, want)
	}
}

func TestToolAccumBuffersSplitArgumentFragments(t *testing.T) {
	var acc toolAccum
	for _, line := range []string{
		`data: {"choices":[{"delta":{"content":null,"tool_calls":[{"index":0,"id":"call_ls","type":"function","function":{"name":"suggest_shell_command","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"com"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"mand\":\""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ls -la"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	} {
		p, err := readStream(line)
		if err != nil {
			t.Fatalf("feeding %q: %v", line, err)
		}
		if err := acc.add(p.toolCalls); err != nil {
			t.Fatalf("accumulating %q: %v", line, err)
		}
		if p.finish != "" && p.finish != finishToolCalls {
			t.Errorf("finish_reason = %q, want %q", p.finish, finishToolCalls)
		}
	}

	calls := acc.calls()
	if len(calls) != 1 {
		t.Fatalf("accumulated %d tool calls, want 1: %+v", len(calls), calls)
	}
	got := calls[0]
	if got.ID != "call_ls" {
		t.Errorf("id = %q, want call_ls", got.ID)
	}
	if got.Type != "function" {
		t.Errorf("type = %q, want function", got.Type)
	}
	if got.Index != 0 {
		t.Errorf("index = %d, want it omitted on the assistant turn", got.Index)
	}
	if got.Function.Name != toolSuggestShellCommand {
		t.Errorf("name = %q, want %q", got.Function.Name, toolSuggestShellCommand)
	}
	if got.Function.Arguments != `{"command":"ls -la"}` {
		t.Errorf("arguments = %q, want the fragments joined into one JSON object", got.Function.Arguments)
	}

	cmd, err := toolCommand(got)
	if err != nil {
		t.Fatalf("toolCommand: %v", err)
	}
	if cmd != "ls -la" {
		t.Errorf("command = %q, want ls -la", cmd)
	}
}

func TestToolCommandPrefixReadsACommandFromPartialJSON(t *testing.T) {
	tests := []struct{ arguments, want string }{
		{`{"com`, ""},
		{`{"command"`, ""},
		{`{"command":`, ""},
		{`{"command": "`, ""},
		{`{"command":"`, ""},
		{`{"command":"p`, "p"},
		{`{"command":"pwd`, "pwd"},
		{`{"command":"pwd"}`, "pwd"},
		{`{"command":"ls -l`, "ls -l"},
		{`{"command" : "ls -la"}`, "ls -la"},
		{`{"command":"echo \"hi`, `echo "hi`},
		{`{"command":"echo \`, `echo `},
		{`{"command":"a\u0041`, "aA"},
		{`{"command":"a\u00`, "a"},
	}

	for _, test := range tests {
		if got := toolCommandPrefix(test.arguments); got != test.want {
			t.Errorf("toolCommandPrefix(%q) = %q, want %q", test.arguments, got, test.want)
		}
	}

	first, second := `{"com`, `mand":"pwd"}`
	if got := toolCommandPrefix(first); got != "" {
		t.Errorf("the first fragment %q was already a command %q", first, got)
	}
	if got := toolCommandPrefix(first + second); got != "pwd" {
		t.Errorf("the joined fragments were %q, want pwd", got)
	}
}

func TestToolAccumKeepsParallelCallsOnTheirIndex(t *testing.T) {
	var acc toolAccum
	if err := acc.add([]toolCall{
		{Index: 1, ID: "call_b", Function: toolCallArgs{Name: toolSuggestShellCommand, Arguments: `{"command":"pwd"}`}},
		{Index: 0, ID: "call_a", Function: toolCallArgs{Name: toolSuggestShellCommand, Arguments: `{"command":"ls"}`}},
	}); err != nil {
		t.Fatal(err)
	}

	calls := acc.calls()
	if len(calls) != 2 {
		t.Fatalf("accumulated %d tool calls, want 2: %+v", len(calls), calls)
	}
	if calls[0].ID != "call_a" || calls[1].ID != "call_b" {
		t.Errorf("calls arrived as %q then %q, want index order", calls[0].ID, calls[1].ID)
	}
}

func TestToolAccumFillsInAMissingIdSoAResultCanPointAtIt(t *testing.T) {
	var acc toolAccum
	if err := acc.add([]toolCall{{Function: toolCallArgs{Name: toolSuggestShellCommand, Arguments: `{"command":"ls"}`}}}); err != nil {
		t.Fatal(err)
	}
	calls := acc.calls()
	if len(calls) != 1 || calls[0].ID != "call_0" || calls[0].Type != "function" {
		t.Errorf("calls = %+v, want id call_0 and type=function", calls)
	}
}

func TestToolAccumRejectsUnboundedIndexes(t *testing.T) {
	var acc toolAccum
	err := acc.add([]toolCall{{Index: maxToolCalls}})
	if err == nil || !strings.Contains(err.Error(), "exceeds the limit") {
		t.Fatalf("large tool index error = %v", err)
	}
	if len(acc.buf) != 0 {
		t.Fatalf("large tool index allocated %d calls", len(acc.buf))
	}
}

func TestToolCommandFeedsInvalidArgumentsBackAsAnError(t *testing.T) {
	_, err := toolCommand(toolCall{Function: toolCallArgs{Name: toolSuggestShellCommand, Arguments: `{`}})
	if err == nil {
		t.Fatal("broken arguments were accepted as a command")
	}
	if !strings.Contains(err.Error(), "not JSON") {
		t.Errorf("the failure reads %q, which does not say the arguments were not JSON", err)
	}

	_, err = toolCommand(toolCall{Function: toolCallArgs{Name: toolSuggestShellCommand, Arguments: `{"command":""}`}})
	if err == nil {
		t.Fatal("an empty command was accepted")
	}

	_, err = toolCommand(toolCall{Function: toolCallArgs{Name: "other_tool", Arguments: `{"command":"ls"}`}})
	if err == nil {
		t.Fatal("a tool the model was not given was accepted")
	}
}

func TestAToolTurnRoundTripsOnTheChatRequest(t *testing.T) {
	msgs := []message{
		{Role: roleAssistant, ToolCalls: []toolCall{{
			ID:       "call_1",
			Type:     "function",
			Function: toolCallArgs{Name: toolSuggestShellCommand, Arguments: `{"command":"ls"}`},
		}}},
		{Role: roleTool, ToolCallID: "call_1", Content: "file.txt"},
	}
	raw, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshaling tool turns: %v", err)
	}

	var got []message
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("tool turns did not round-trip: %v\n%s", err, raw)
	}
	if len(got) != 2 || got[0].Role != roleAssistant || len(got[0].ToolCalls) != 1 {
		t.Fatalf("assistant turn = %+v, want a tool_calls array", got)
	}
	if got[1].Role != roleTool || got[1].ToolCallID != "call_1" || got[1].Content != "file.txt" {
		t.Errorf("tool turn = %+v, want tool_call_id call_1 and the command's output", got[1])
	}
	if !strings.Contains(string(raw), `"tool_calls"`) || !strings.Contains(string(raw), `"tool_call_id"`) {
		t.Errorf("the JSON dropped the tool fields:\n%s", raw)
	}
}

func TestReadStreamTreatsNullContentAsEmpty(t *testing.T) {
	p, err := readStream(`data: {"choices":[{"delta":{"content":null,"tool_calls":[{"index":0,"id":"call_1","function":{"name":"suggest_shell_command","arguments":"{"}}]}}]}`)
	if err != nil {
		t.Fatalf("a tool-call chunk with null content failed: %v", err)
	}
	if p.content != "" {
		t.Errorf("content = %q, want empty so a null does not become the word null", p.content)
	}
	if len(p.toolCalls) != 1 || p.toolCalls[0].ID != "call_1" {
		t.Errorf("tool_calls = %+v, want the fragment the accumulator needs", p.toolCalls)
	}

	p, err = readStream(`data: {"choices":[{"delta":{"content":"ls"},"finish_reason":"stop"}]}`)
	if err != nil {
		t.Fatalf("a stop chunk failed: %v", err)
	}
	if p.finish != finishStop || p.content != "ls" {
		t.Errorf("stop chunk = %+v, want content ls and finish %q", p, finishStop)
	}

	_, err = readStream("data: [DONE]")
	if !errors.Is(err, errStreamDone) {
		t.Errorf("the end of the stream failed with %v, want %v", err, errStreamDone)
	}
}

func TestAgentRunsTheToolThenAnswers(t *testing.T) {
	asked := make(chan compatibleBody, 2)
	client := newTestClient(t, Options{Model: "test-model"}, func(w http.ResponseWriter, r *http.Request) {
		req := read(t, r)
		asked <- req
		if last := req.Messages[len(req.Messages)-1]; last.Role == roleTool {
			send(t, w, token("ls -la /tmp"), doneToken)
			return
		}
		send(t, w, reasoning("see what is in tmp first"), toolCallStream(`{"command":"ls /tmp"}`), doneToken)
	})

	var said, thoughts, ran []string
	err := client.Agent(t.Context(), "what is taking space in tmp", nil,
		func(command string) error {
			said = append(said, command)
			return nil
		},
		func(thought string) error {
			thoughts = append(thoughts, thought)
			return nil
		},
		func(_ context.Context, command string) (string, error) {
			ran = append(ran, command)
			return "$ ls /tmp\na.txt b.txt", nil
		},
		nil)
	if err != nil {
		t.Fatalf("Agent: %v", err)
	}

	if want := []string{"ls /tmp"}; !slices.Equal(ran, want) {
		t.Errorf("the agent ran %q, want %q", ran, want)
	}
	if want := []string{"see what is in tmp first"}; !slices.Equal(thoughts, want) {
		t.Errorf("the thought arrived as %q, want %q", thoughts, want)
	}
	if want := []string{"ls /tmp", "# ls -la /tmp"}; !slices.Equal(said, want) {
		t.Errorf("emit received %q, want the command as it was typed then the report as a comment", said)
	}

	first := <-asked
	if len(first.Tools) != 1 || first.Tools[0].Function.Name != toolSuggestShellCommand {
		t.Errorf("the agent question carried tools = %+v, want the shell command tool", first.Tools)
	}
	if first.ToolChoice != toolChoiceAuto {
		t.Errorf("tool_choice = %q, want %q so the model may still answer in text", first.ToolChoice, toolChoiceAuto)
	}
	if first.ReasoningEffort != "" {
		t.Errorf("reasoning_effort = %q, want omitted when none was configured", first.ReasoningEffort)
	}
	if len(first.Messages) != 2 || !strings.Contains(first.Messages[0].Content, toolSuggestShellCommand) {
		t.Errorf("the agent question does not open with the agent system prompt: %+v", first.Messages)
	}

	second := <-asked
	if len(second.Messages) != 4 {
		t.Fatalf("the follow-up carried %d messages, want system, question, the tool call and its result: %+v", len(second.Messages), second.Messages)
	}
	assistant := second.Messages[2]
	if assistant.Role != roleAssistant || len(assistant.ToolCalls) != 1 {
		t.Fatalf("the tool call is not an assistant turn of its own: %+v", assistant)
	}
	if got := assistant.ToolCalls[0].Function.Arguments; got != `{"command":"ls /tmp"}` {
		t.Errorf("the assistant turn's arguments = %q, want the JSON the model streamed", got)
	}
	result := second.Messages[3]
	if result.Role != roleTool || result.ToolCallID != "call_1" {
		t.Errorf("the tool result is %+v, want a tool turn pointing at call_1", result)
	}
	if !strings.Contains(result.Content, "a.txt b.txt") {
		t.Errorf("the tool result = %q, want what the command printed", result.Content)
	}

	dump := DumpChat("", Prompts{}, "", nil, client, client)
	ctx := dump[strings.Index(dump, "======= context ======="):]
	for _, want := range []string{`"tool_calls"`, toolSuggestShellCommand, `"role": "tool"`, "a.txt b.txt"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("?# did not print the in-memory agent messages (%q):\n%s", want, ctx)
		}
	}
}

func TestAgentApprovesAndCallsAnExternalToolWithoutTypingItsArguments(t *testing.T) {
	const toolName = "mcp_demo__deploy"
	asked := make(chan compatibleBody, 2)
	var called bool
	client := newTestClient(t, Options{
		Model: "test-model",
		Tools: []Tool{{
			Name: toolName, Description: "Deploy a service.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"command": map[string]any{"type": "string"}},
			},
		}},
		CallTool: func(_ context.Context, name string, arguments json.RawMessage) (string, error) {
			called = true
			if name != toolName || string(arguments) != `{"command":"release"}` {
				t.Errorf("external call = %s %s", name, arguments)
			}
			return "deployed", nil
		},
	}, func(w http.ResponseWriter, r *http.Request) {
		req := read(t, r)
		asked <- req
		if last := req.Messages[len(req.Messages)-1]; last.Role == roleTool {
			send(t, w, token("finished"), doneToken)
			return
		}
		send(t, w,
			fmt.Sprintf(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_mcp","type":"function","function":{"name":%q,"arguments":"{\"command\":\"release\"}"}}]}}]}`+"\n\n", toolName),
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n",
			doneToken,
		)
	})

	var said, approved []string
	err := client.Agent(t.Context(), "deploy it", nil,
		func(text string) error {
			said = append(said, text)
			return nil
		},
		ignoreThought,
		func(context.Context, string) (string, error) {
			t.Fatal("an MCP tool call reached the shell runner")
			return "", nil
		},
		func(_ context.Context, name string, arguments json.RawMessage) error {
			approved = append(approved, name+" "+string(arguments))
			return nil
		},
	)
	if err != nil {
		t.Fatalf("Agent: %v", err)
	}
	if !called {
		t.Fatal("the approved MCP tool was not called")
	}
	if want := []string{toolName + ` {"command":"release"}`}; !slices.Equal(approved, want) {
		t.Errorf("approvals = %q, want %q", approved, want)
	}
	if want := []string{"# finished"}; !slices.Equal(said, want) {
		t.Errorf("typed text = %q, want only the final report %q", said, want)
	}

	first := <-asked
	if len(first.Tools) != 2 || first.Tools[1].Function.Name != toolName {
		t.Fatalf("tools = %+v, want shell and MCP tools", first.Tools)
	}
	schema, err := json.Marshal(first.Tools[1].Function.Parameters)
	if err != nil || !strings.Contains(string(schema), `"command"`) {
		t.Errorf("MCP schema = %s, want arbitrary input schema", schema)
	}
	second := <-asked
	if got := second.Messages[len(second.Messages)-1].Content; got != "deployed" {
		t.Errorf("tool result = %q, want deployed", got)
	}
	if dump := DumpChat("", Prompts{}, "", nil, client, client); !strings.Contains(dump, toolName) {
		t.Errorf("?# dump omitted the discovered MCP tool:\n%s", dump)
	}
}

func TestExternalToolCannotRunWithoutApproval(t *testing.T) {
	called := false
	result, err := runToolCall(t.Context(), toolCall{Function: toolCallArgs{
		Name: "mcp_demo__write", Arguments: `{}`,
	}}, func(context.Context, string) (string, error) {
		return "", nil
	}, func(context.Context, string, json.RawMessage) (string, error) {
		called = true
		return "", nil
	}, nil, appconfig.DefaultToolOutputBytes)
	if err != nil {
		t.Fatalf("runToolCall: %v", err)
	}
	if called {
		t.Fatal("external tool ran without approval")
	}
	if !strings.Contains(result, "approval is unavailable") {
		t.Errorf("result = %q, want an approval error", result)
	}
}

func TestAgentRejectsAnUnadvertisedTool(t *testing.T) {
	client := newTestClient(t, Options{
		CallTool: func(context.Context, string, json.RawMessage) (string, error) {
			t.Error("an unadvertised tool reached the external runner")
			return "", nil
		},
	}, func(w http.ResponseWriter, r *http.Request) {
		messages := read(t, r).Messages
		if last := messages[len(messages)-1]; last.Role == roleTool {
			if !strings.Contains(last.Content, "not a tool it was given") {
				t.Errorf("unknown-tool result = %q", last.Content)
			}
			send(t, w, token("tool unavailable"), doneToken)
			return
		}
		send(t, w, `data: {"choices":[{"delta":{"tool_calls":[{"function":{"name":"unknown_tool","arguments":"{}"}}]}}]}`+"\n\n", doneToken)
	})

	err := client.Agent(t.Context(), "use a tool", nil, ignoreThought, ignoreThought,
		func(context.Context, string) (string, error) {
			t.Error("an unadvertised tool reached the shell runner")
			return "", nil
		},
		func(context.Context, string, json.RawMessage) error {
			t.Error("an unadvertised tool requested approval")
			return nil
		})
	if err != nil {
		t.Fatalf("Agent: %v", err)
	}
}

func TestAgentAssemblesAToolCallFromSplitChunks(t *testing.T) {
	asked := make(chan compatibleBody, 2)
	client := newTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		req := read(t, r)
		asked <- req
		if last := req.Messages[len(req.Messages)-1]; last.Role == roleTool {
			send(t, w, token("pwd"), doneToken)
			return
		}
		send(t, w,
			`data: {"choices":[{"delta":{"content":null,"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"suggest_shell_command","arguments":""}}]}}]}`+"\n\n",
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"com"}}]}}]}`+"\n\n",
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"mand\":\"pw"}}]}}]}`+"\n\n",
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"d\"}"}}]}}]}`+"\n\n",
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n",
			doneToken,
		)
	})

	var said, ran []string
	err := client.Agent(t.Context(), "where am i", nil,
		func(command string) error {
			said = append(said, command)
			return nil
		},
		ignoreThought,
		func(_ context.Context, command string) (string, error) {
			ran = append(ran, command)
			return "/tmp", nil
		},
		nil)
	if err != nil {
		t.Fatalf("Agent: %v", err)
	}
	if want := []string{"pwd"}; !slices.Equal(ran, want) {
		t.Errorf("the agent ran %q, want the command assembled from the fragments", ran)
	}
	if want := []string{"pw", "pwd", "# pwd"}; !slices.Equal(said, want) {
		t.Errorf("emit received %q, want pwd as the fragments assembled and the report last", said)
	}

	<-asked
	assistant := (<-asked).Messages[2]
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].Function.Arguments != `{"command":"pwd"}` {
		t.Errorf("the follow-up carried %+v, want the fragments joined into one JSON object", assistant.ToolCalls)
	}
}

func TestAgentFeedsBrokenArgumentsBackAsTheToolResult(t *testing.T) {
	asked := make(chan compatibleBody, 2)
	client := newTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		req := read(t, r)
		asked <- req
		if last := req.Messages[len(req.Messages)-1]; last.Role == roleTool {
			send(t, w, token("ls"), doneToken)
			return
		}
		send(t, w, toolCallStream(`{"command":"ls`), doneToken)
	})

	ran := false
	var said []string
	err := client.Agent(t.Context(), "list the files", nil,
		func(command string) error {
			said = append(said, command)
			return nil
		},
		ignoreThought,
		func(_ context.Context, _ string) (string, error) {
			ran = true
			return "", nil
		},
		nil)
	if err != nil {
		t.Fatalf("Agent: %v", err)
	}

	if ran {
		t.Error("a call with broken arguments was run at the shell anyway")
	}
	if want := []string{"ls", "# ls"}; !slices.Equal(said, want) {
		t.Errorf("emit received %q, want the leftover command revised by the report", said)
	}
	<-asked
	result := (<-asked).Messages
	toolTurn := result[len(result)-1]
	if toolTurn.Role != roleTool || !strings.Contains(toolTurn.Content, "not JSON") {
		t.Errorf("the broken call was fed back as %+v, want a tool result saying the arguments were not JSON", toolTurn)
	}
}

func TestAgentTakesTheToolAwayAfterMaxToolSteps(t *testing.T) {
	for _, configured := range []int{0, 1, appconfig.DefaultAgentSteps + 1} {
		t.Run(fmt.Sprint(configured), func(t *testing.T) {
			limit := configured
			if limit == 0 {
				limit = appconfig.DefaultAgentSteps
			}

			asked := make(chan compatibleBody, max(appconfig.DefaultAgentSteps, limit)+2)
			client := newTestClient(t, Options{AgentSteps: configured}, func(w http.ResponseWriter, r *http.Request) {
				req := read(t, r)
				asked <- req
				if len(req.Tools) == 0 {
					send(t, w, token("# checked everywhere I can"), doneToken)
					return
				}
				send(t, w, toolCallStream(`{"command":"true"}`), doneToken)
			})

			ran := 0
			var said []string
			err := client.Agent(t.Context(), "find the missing file", nil,
				func(command string) error {
					said = append(said, command)
					return nil
				},
				ignoreThought,
				func(_ context.Context, _ string) (string, error) {
					ran++
					return "", nil
				},
				nil)
			if err != nil {
				t.Fatalf("Agent: %v", err)
			}

			if ran != limit {
				t.Errorf("the agent ran %d commands, want the %d cap", ran, limit)
			}
			if want := append(slices.Repeat([]string{"true"}, limit), "# checked everywhere I can"); !slices.Equal(said, want) {
				t.Errorf("emit received %q, want the command typed each step and the report at the end", said)
			}

			for step := 0; step < limit; step++ {
				if req := <-asked; len(req.Tools) == 0 {
					t.Errorf("step %d carried no tools, want the tool attached until the cap", step)
				}
			}
			last := <-asked
			if len(last.Tools) != 0 || last.ToolChoice != toolChoiceNone {
				t.Errorf("the overflow step carried tools = %+v and tool_choice = %q, want none so the model has to answer", last.Tools, last.ToolChoice)
			}
			if want := 2 + 2*limit; len(last.Messages) != want {
				t.Errorf("the overflow step carried %d messages, want %d: every tool call and its result", len(last.Messages), want)
			}
			toolTurns := 0
			for _, msg := range last.Messages {
				if msg.Role != roleTool {
					continue
				}
				toolTurns++
				if !strings.Contains(msg.Content, "printed nothing") {
					t.Errorf("a command that printed nothing was fed back as %q, want it to say so", msg.Content)
				}
			}
			if toolTurns != limit {
				t.Errorf("the overflow step carried %d tool results, want one per step", toolTurns)
			}
			select {
			case extra := <-asked:
				t.Errorf("the agent kept asking after the overflow step: %+v", extra)
			default:
			}

		})
	}
}

func TestAgentDoesNotBoundAStepByADeadline(t *testing.T) {
	client := newTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Context().Deadline(); ok {
			t.Error("an agent step carried a deadline, want the stream to run until llama finishes or the question is taken back")
		}
		send(t, w, token("looked around"), doneToken)
	})

	err := client.Agent(context.Background(), "look around", nil,
		func(string) error { return nil },
		ignoreThought,
		func(context.Context, string) (string, error) {
			return "", nil
		},
		nil)
	if err != nil {
		t.Fatalf("Agent: %v", err)
	}
}

func TestAgentStopsWhenTheQuestionIsTakenBack(t *testing.T) {
	client := newTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		send(t, w, token("still looking"))
		<-r.Context().Done()
	})

	ctx, takeBack := context.WithCancel(t.Context())
	defer takeBack()

	answered := make(chan error, 1)
	go func() {
		answered <- client.Agent(ctx, "look around", nil, func(string) error {
			takeBack()
			return nil
		}, ignoreThought, func(context.Context, string) (string, error) {
			return "", nil
		}, nil)
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

func TestAgentAnswersWithoutCallingTheTool(t *testing.T) {
	asked := make(chan compatibleBody, 1)
	client := newTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		asked <- read(t, r)
		send(t, w, token("ls -la /tmp"), doneToken)
	})

	ran := false
	var said []string
	err := client.Agent(t.Context(), "list everything in tmp", nil,
		func(command string) error {
			said = append(said, command)
			return nil
		},
		ignoreThought,
		func(context.Context, string) (string, error) {
			ran = true
			return "", nil
		},
		nil)
	if err != nil {
		t.Fatalf("Agent: %v", err)
	}

	if ran {
		t.Error("the model answered in text but a command was run anyway")
	}
	if want := []string{"# ls -la /tmp"}; !slices.Equal(said, want) {
		t.Errorf("the final answer arrived as %q, want %q", said, want)
	}
	req := <-asked
	if len(req.Tools) != 1 || req.ToolChoice != toolChoiceAuto {
		t.Errorf("a first-step answer carried tools = %+v tool_choice = %q, want the tool attached so the model could have used it", req.Tools, req.ToolChoice)
	}
	select {
	case extra := <-asked:
		t.Errorf("the agent asked again after a text answer: %+v", extra)
	default:
	}
}

// TestAgentCommentsEveryLineOfItsAnswer is what stands in for cutting the
// answer at its first newline. An agent's last word is typed at the user's
// prompt, so a second line of it that was not a comment would be a command
// waiting for Enter.
func TestAgentCommentsEveryLineOfItsAnswer(t *testing.T) {
	client := newTestClient(t, Options{}, func(w http.ResponseWriter, _ *http.Request) {
		send(t, w, token("started postgres"), token("\nit is listening on 5432"), doneToken)
	})

	ran := false
	var said []string
	err := client.Agent(t.Context(), "list everything", nil,
		func(command string) error {
			said = append(said, command)
			return nil
		},
		ignoreThought,
		func(context.Context, string) (string, error) {
			ran = true
			return "", nil
		},
		nil)
	if err != nil {
		t.Fatalf("Agent: %v", err)
	}

	if ran {
		t.Error("an explanation after the command was treated as a tool call")
	}
	want := []string{"# started postgres", "# started postgres\n# it is listening on 5432"}
	if !slices.Equal(said, want) {
		t.Errorf("the report arrived as %q, want %q", said, want)
	}
}

func TestAgentKeepsGoingAfterACommandFails(t *testing.T) {
	asked := make(chan compatibleBody, 2)
	client := newTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		req := read(t, r)
		asked <- req
		if last := req.Messages[len(req.Messages)-1]; last.Role == roleTool {
			send(t, w, token("# nothing to list"), doneToken)
			return
		}
		send(t, w, toolCallStream(`{"command":"ls /nope"}`), doneToken)
	})

	var said, ran []string
	err := client.Agent(t.Context(), "list the missing directory", nil,
		func(command string) error {
			said = append(said, command)
			return nil
		},
		ignoreThought,
		func(_ context.Context, command string) (string, error) {
			ran = append(ran, command)
			return "", errors.New("exit 2")
		},
		nil)
	if err != nil {
		t.Fatalf("Agent: %v", err)
	}

	if want := []string{"ls /nope"}; !slices.Equal(ran, want) {
		t.Errorf("the agent ran %q, want %q", ran, want)
	}
	if want := []string{"ls /nope", "# nothing to list"}; !slices.Equal(said, want) {
		t.Errorf("emit received %q, want the failed command then the report", said)
	}
	<-asked
	toolTurn := (<-asked).Messages
	result := toolTurn[len(toolTurn)-1]
	if result.Role != roleTool || !strings.Contains(result.Content, "the command failed") || !strings.Contains(result.Content, "exit 2") {
		t.Errorf("a failed command was fed back as %+v, want a tool result saying so", result)
	}
}

func TestRunToolCallStopsWhenTheQuestionIsTakenBack(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := runToolCall(ctx, toolCall{Function: toolCallArgs{Name: toolSuggestShellCommand, Arguments: `{"command":"ls"}`}},
		func(ctx context.Context, _ string) (string, error) {
			return "", ctx.Err()
		}, nil, nil, appconfig.DefaultToolOutputBytes)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled question failed with %v, want the cancellation", err)
	}
}

func TestToolResultTruncatesLongOutput(t *testing.T) {
	if got := toolResult("  \n", appconfig.DefaultToolOutputBytes); got != "the command printed nothing" {
		t.Errorf("empty output was fed back as %q, want it to say so", got)
	}
	if got := toolResult("  file.txt  ", appconfig.DefaultToolOutputBytes); got != "file.txt" {
		t.Errorf("short output = %q, want it trimmed", got)
	}

	exact := strings.Repeat("x", appconfig.DefaultToolOutputBytes)
	if got := toolResult(exact, appconfig.DefaultToolOutputBytes); got != exact {
		t.Errorf("a result that fills the limit was changed, len %d", len(got))
	}

	const marker = "\n\n# --- truncated long output ---\n\n"
	room := appconfig.DefaultToolOutputBytes - len(marker)
	long := strings.Repeat("a", appconfig.DefaultToolOutputBytes) + strings.Repeat("b", appconfig.DefaultToolOutputBytes)
	want := strings.Repeat("a", room/5) + marker + strings.Repeat("b", room-room/5)
	if got := toolResult(long, appconfig.DefaultToolOutputBytes); got != want {
		t.Fatalf("tool result did not keep the expected head, marker and tail: %q", got)
	}
	captured := "command\nhead" + marker + "tail\nexit code = 0"
	if got := toolResult(captured, appconfig.DefaultToolOutputBytes); got != captured {
		t.Fatal("an already bounded command result was changed")
	}
}

func TestToolResultCustomLimitsForShellAndMCP(t *testing.T) {
	for _, limit := range []int{1, 3, 35, 64, appconfig.DefaultToolOutputBytes + 10} {
		for _, name := range []string{toolSuggestShellCommand, "mcp_test"} {
			t.Run(fmt.Sprintf("%s/%d", name, limit), func(t *testing.T) {
				output := strings.Repeat("é", limit+1)
				result, err := runToolCall(t.Context(), toolCall{Function: toolCallArgs{
					Name: name, Arguments: `{"command":"cat"}`,
				}}, func(context.Context, string) (string, error) {
					return output, nil
				}, func(context.Context, string, json.RawMessage) (string, error) {
					return output, nil
				}, func(context.Context, string, json.RawMessage) error { return nil }, limit)
				want := ""
				const marker = "\n\n# --- truncated long output ---\n\n"
				if room := limit - len(marker); room >= 0 {
					want = output[:room/5] + marker + output[len(output)-(room-room/5):]
				}
				if err != nil || result != want {
					t.Fatalf("custom result = %q, %v; want %q", result, err, want)
				}
			})
		}
	}
}

// toolCallStream is one tool-call answer as SSE chunks: the call in a
// single piece — the fragment-by-fragment case belongs to the accumulator
// tests — then the finish_reason that ends it.
func toolCallStream(arguments string) string {
	return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%q}}]}}]}\n\n", toolSuggestShellCommand, arguments) +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"
}

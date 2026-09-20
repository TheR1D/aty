package llm

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestResponsesStreamTextAndReasoning(t *testing.T) {
	read := newResponsesStream()
	for _, tt := range []struct {
		line string
		want streamPiece
	}{
		{"", streamPiece{}},
		{": keep-alive", streamPiece{}},
		{"event: response.output_text.delta", streamPiece{}},
		{`data: {"type":"response.created","response":{"status":"in_progress"}}`, streamPiece{}},
		{`data: {"type":"response.output_text.delta","delta":"pwd"}`, streamPiece{content: "pwd"}},
		{`  data: {"type":"response.reasoning_summary_text.delta","delta":"Checking the directory."}  `, streamPiece{reasoning: "Checking the directory."}},
		{`data: {"type":"response.output_text.done","text":"pwd"}`, streamPiece{}},
		{`data: {"type":"response.reasoning_summary_text.done","text":"Checking the directory."}`, streamPiece{}},
		{`data: {"type":"response.future_event","data":{"unknown":true}}`, streamPiece{}},
		{`data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"pwd"}]}]}}`, streamPiece{done: true, finish: finishStop, responseItems: []json.RawMessage{json.RawMessage(`{"type":"message","content":[{"type":"output_text","text":"pwd"}]}`)}}},
	} {
		got, err := read(tt.line)
		if err != nil {
			t.Fatalf("read(%q): %v", tt.line, err)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("read(%q) = %+v, want %+v", tt.line, got, tt.want)
		}
	}
}

func TestResponsesStreamFunctionFragmentsAndCompletedItems(t *testing.T) {
	read := newResponsesStream()
	var accumulated toolAccum
	for _, line := range []string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1"}}`,
		`data: {"type":"response.output_text.delta","delta":"Checking."}`,
		`data: {"type":"response.output_item.added","output_index":33,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"suggest_shell_command","arguments":""}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":33,"item_id":"fc_1","delta":"{\"command\":\"pw"}`,
		`data: {"type":"response.output_item.added","output_index":38,"item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"external_tool","arguments":"{\"path\":"}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":38,"item_id":"fc_2","delta":"\"/tmp\"}"}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":33,"item_id":"fc_1","delta":"d\"}"}`,
		`data: {"type":"response.function_call_arguments.done","output_index":33,"item_id":"fc_1","arguments":"{\"command\":\"pwd\"}"}`,
		`data: {"type":"response.output_item.done","output_index":33,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"suggest_shell_command","arguments":"{\"command\":\"pwd\"}"}}`,
	} {
		piece, err := read(line)
		if err != nil {
			t.Fatalf("read(%s): %v", line, err)
		}
		if err := accumulated.add(piece.toolCalls); err != nil {
			t.Fatal(err)
		}
	}
	wantPreview := []toolCall{
		{ID: "call_1", Type: "function", Function: toolCallArgs{Name: toolSuggestShellCommand, Arguments: `{"command":"pwd"}`}},
		{ID: "call_2", Type: "function", Function: toolCallArgs{Name: "external_tool", Arguments: `{"path":"/tmp"}`}},
	}
	if got := accumulated.calls(); !reflect.DeepEqual(got, wantPreview) {
		t.Fatalf("accumulated calls = %+v, want %+v", got, wantPreview)
	}

	// Preserve opaque reasoning and future fields exactly; the final arguments
	// supersede the preview and call_id, not the output item id, links results.
	reasoning := `{"type": "reasoning", "id":"rs_1", "encrypted_content":"opaque+/=", "summary":[], "future":{"keep":true}}`
	firstCall := `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"suggest_shell_command","arguments":"{\"command\":\"ls /tmp\"}","status":"completed"}`
	secondCall := `{"type":"function_call","id":"fc_2","call_id":"call_2","name":"external_tool","arguments":"{\"path\":\"/tmp\"}"}`
	message := `{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Checking.","annotations":[]}]}`
	future := `{"type":"future_output","unfamiliar":{"nested":true}}`
	items := []string{reasoning, message, firstCall, secondCall, future}
	line := `data: {"type":"response.completed","response":{"status":"completed","output":[` + strings.Join(items, ",") + `]}}`
	completed, err := read(line)
	if err != nil {
		t.Fatal(err)
	}
	if !completed.done || completed.finish != finishToolCalls || completed.content != "" || completed.reasoning != "" || len(completed.toolCalls) != 0 {
		t.Fatalf("completion duplicated stream deltas or did not finish: %+v", completed)
	}
	wantPreview[0].Function.Arguments = `{"command":"ls /tmp"}`
	if !reflect.DeepEqual(completed.finalToolCalls, wantPreview) {
		t.Errorf("final calls = %+v, want %+v", completed.finalToolCalls, wantPreview)
	}
	if len(completed.responseItems) != len(items) {
		t.Fatalf("retained %d items, want %d", len(completed.responseItems), len(items))
	}
	for i, raw := range completed.responseItems {
		if string(raw) != items[i] {
			t.Errorf("item %d changed: %s, want %s", i, raw, items[i])
		}
	}
}

func TestResponsesStreamFailures(t *testing.T) {
	for _, tt := range []struct {
		name string
		line string
		want string
	}{
		{"done sentinel", "data: [DONE]", "without response.completed"},
		{"malformed JSON", `data: {`, "reading the OpenAI response"},
		{"null event", `data: null`, "no type"},
		{"missing type", `data: {}`, "no type"},
		{"wrong field type", `data: {"type":"response.output_text.delta","delta":5}`, "reading the OpenAI response"},
		{"top level error", `data: {"error":{"message":"Invalid API key"}}`, "Invalid API key"},
		{"error event", `data: {"type":"error","code":"server_error","message":"Server unavailable"}`, "Server unavailable"},
		{"error code", `data: {"type":"error","code":"server_error"}`, "server_error"},
		{"empty error", `data: {"type":"error"}`, "unknown streaming error"},
		{"failed", `data: {"type":"response.failed","response":{"status":"failed","error":{"message":"Server failed"}}}`, "Server failed"},
		{"incomplete", `data: {"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`, "max_output_tokens"},
		{"incomplete without details", `data: {"type":"response.incomplete"}`, "no details provided"},
		{"incomplete status on completed", `data: {"type":"response.completed","response":{"status":"incomplete","output":[]}}`, `status "incomplete"`},
		{"missing status on completed", `data: {"type":"response.completed","response":{"output":[]}}`, `status ""`},
		{"missing output on completed", `data: {"type":"response.completed","response":{"status":"completed"}}`, "no output array"},
		{"null output on completed", `data: {"type":"response.completed","response":{"status":"completed","output":null}}`, "no output array"},
		{"error on completed", `data: {"type":"response.completed","response":{"status":"completed","output":[],"error":{"message":"Server failed"}}}`, "Server failed"},
		{"null output item", `data: {"type":"response.completed","response":{"status":"completed","output":[null]}}`, "output item has no type"},
		{"wrong output item type", `data: {"type":"response.completed","response":{"status":"completed","output":[42]}}`, "reading an OpenAI output item"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			piece, err := newResponsesStream()(tt.line)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if !reflect.DeepEqual(piece, streamPiece{}) {
				t.Errorf("failure returned executable data: %+v", piece)
			}
		})
	}
}

func TestResponsesStreamReconcilesCompletedText(t *testing.T) {
	for _, tt := range []struct {
		name   string
		deltas []string
		final  string
		want   string
		failed bool
	}{
		{name: "final text only", final: "pwd", want: "pwd"},
		{name: "remaining suffix", deltas: []string{"pw"}, final: "pwd", want: "d"},
		{name: "already streamed", deltas: []string{"p", "wd"}, final: "pwd"},
		{name: "empty answer", final: ""},
		{name: "changed final answer", deltas: []string{"pwd"}, final: "ls", failed: true},
		{name: "truncated final answer", deltas: []string{"pwd"}, final: "pw", failed: true},
		{name: "missing final answer", deltas: []string{"pwd"}, failed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			read := newResponsesStream()
			for _, delta := range tt.deltas {
				encoded, err := json.Marshal(delta)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := read(`data: {"type":"response.output_text.delta","delta":` + string(encoded) + `}`); err != nil {
					t.Fatal(err)
				}
			}
			encoded, err := json.Marshal(tt.final)
			if err != nil {
				t.Fatal(err)
			}
			piece, err := read(`data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":` + string(encoded) + `}]}]}}`)
			if tt.failed {
				if err == nil || !strings.Contains(err.Error(), "does not match") {
					t.Fatalf("error = %v, want mismatched text", err)
				}
				if !reflect.DeepEqual(piece, streamPiece{}) {
					t.Fatalf("mismatched text returned executable data: %+v", piece)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !piece.done || piece.content != tt.want {
				t.Errorf("completion = %+v, want content %q", piece, tt.want)
			}
		})
	}
}

func TestResponsesStreamRefusals(t *testing.T) {
	for _, line := range []string{
		`data: {"type":"response.refusal.delta","delta":"I cannot help."}`,
		`data: {"type":"response.refusal.done","refusal":"I cannot help."}`,
		`data: {"type":"response.refusal.done","refusal":""}`,
		`data: {"type":"response.content_part.added","part":{"type":"refusal","refusal":""}}`,
		`data: {"type":"response.content_part.done","part":{"type":"refusal","refusal":"I cannot help."}}`,
		`data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"I cannot help."}]}]}}`,
	} {
		piece, err := newResponsesStream()(line)
		if err == nil || !strings.Contains(err.Error(), "refused the request") {
			t.Errorf("read(%s) = %+v, %v; want refusal error", line, piece, err)
		}
		if !reflect.DeepEqual(piece, streamPiece{}) {
			t.Errorf("refusal returned executable data: %+v", piece)
		}
	}
}

func TestResponsesStreamRejectsMalformedFunctions(t *testing.T) {
	for _, tt := range []struct {
		name  string
		items string
		want  string
	}{
		{"no call id", `{"type":"function_call","id":"fc_1","name":"tool","arguments":"{}"}`, "missing call_id or name"},
		{"no name", `{"type":"function_call","call_id":"call_1","arguments":"{}"}`, "missing call_id or name"},
		{"blank call id", `{"type":"function_call","call_id":" ","name":"tool","arguments":"{}"}`, "missing call_id or name"},
		{"blank name", `{"type":"function_call","call_id":"call_1","name":" ","arguments":"{}"}`, "missing call_id or name"},
		{"missing arguments", `{"type":"function_call","call_id":"call_1","name":"tool"}`, "arguments were not JSON"},
		{"invalid arguments", `{"type":"function_call","call_id":"call_1","name":"tool","arguments":"{"}`, "arguments were not JSON"},
		{"incomplete function", `{"type":"function_call","call_id":"call_1","name":"tool","arguments":"{}","status":"in_progress"}`, `status "in_progress"`},
		{"duplicate call id", `{"type":"function_call","call_id":"call_1","name":"tool","arguments":"{}"},{"type":"function_call","call_id":"call_1","name":"tool","arguments":"{}"}`, "repeated function call_id"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			line := `data: {"type":"response.completed","response":{"status":"completed","output":[` + tt.items + `]}}`
			piece, err := newResponsesStream()(line)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if !reflect.DeepEqual(piece, streamPiece{}) {
				t.Errorf("invalid function returned executable data: %+v", piece)
			}
		})
	}
}

func TestResponsesStreamRejectsMalformedFunctionDeltas(t *testing.T) {
	added := `data: {"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"tool","arguments":""}}`
	for _, tt := range []struct {
		line string
		want string
	}{
		{added, "repeated function call output_index"},
		{`data: {"type":"response.output_item.added","item":{"type":"function_call"}}`, "no valid output_index"},
		{`data: {"type":"response.output_item.added","output_index":-1,"item":{"type":"function_call"}}`, "no valid output_index"},
		{`data: {"type":"response.function_call_arguments.delta","delta":"{}"}`, "no output_index"},
		{`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}`, "unknown function call"},
		{`data: {"type":"response.function_call_arguments.delta","output_index":-1,"delta":"{}"}`, "unknown function call"},
		{`data: {"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc_wrong","delta":"{}"}`, "mismatched item_id"},
	} {
		read := newResponsesStream()
		if _, err := read(added); err != nil {
			t.Fatal(err)
		}
		_, err := read(tt.line)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("read(%s) error = %v, want %q", tt.line, err, tt.want)
		}
	}
}

func TestResponsesStreamLimitsFunctions(t *testing.T) {
	read := newResponsesStream()
	var items []string
	for i := 0; i <= maxToolCalls; i++ {
		item := fmt.Sprintf(`{"type":"function_call","id":"fc_%d","call_id":"call_%d","name":"tool","arguments":"{}"}`, i, i)
		items = append(items, item)
		line := fmt.Sprintf(`data: {"type":"response.output_item.added","output_index":%d,"item":%s}`, i+100, item)
		piece, err := read(line)
		if i < maxToolCalls {
			if err != nil {
				t.Fatalf("call %d: %v", i, err)
			}
			if len(piece.toolCalls) != 1 || piece.toolCalls[0].Index != i {
				t.Fatalf("call %d did not get a dense index: %+v", i, piece)
			}
		} else if err == nil || !strings.Contains(err.Error(), "limit of 32 tool calls") {
			t.Fatalf("extra streamed call error = %v", err)
		}
	}
	line := `data: {"type":"response.completed","response":{"status":"completed","output":[` + strings.Join(items, ",") + `]}}`
	if _, err := newResponsesStream()(line); err == nil || !strings.Contains(err.Error(), "limit of 32 tool calls") {
		t.Fatalf("extra completed call error = %v", err)
	}
}

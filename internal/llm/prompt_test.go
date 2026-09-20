package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDumpSendsTheQuestionAloneWhenNothingHasRun(t *testing.T) {
	got := Dump("", Prompts{System: "custom system prompt"}, "list the files", nil)

	if !strings.Contains(got, "list the files") {
		t.Errorf("Dump dropped the question:\n%s", got)
	}
	if !strings.Contains(got, "custom system prompt") {
		t.Errorf("Dump dropped the configured system prompt:\n%s", got)
	}
	if i := strings.Index(got, "======= context ======="); i < 0 || strings.Contains(got[i:], "custom system prompt") {
		t.Errorf("the system prompt was repeated in the context:\n%s", got)
	}
	if strings.Count(got, `"role": "user"`) != 1 {
		t.Errorf("a question with no transcript carried %d user turns, want 1:\n%s", strings.Count(got, `"role": "user"`), got)
	}
}

func TestDumpIsTheMessagesAQuestionWouldSend(t *testing.T) {
	system := "system from default_prompt.sh"
	question := "why did that fail"
	transcript := []Turn{
		{Text: "$ git push\nerror: failed to push some refs"},
		{Text: "$ pwd\n/tmp"},
	}
	got := Dump("", Prompts{System: system}, question, transcript)
	at := strings.Index(got, "======= context =======")
	if at < 0 {
		t.Fatalf("Dump has no context section:\n%s", got)
	}
	ctx := got[at:]
	for _, want := range []string{"git push", "failed to push", "/tmp", "why did that fail"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("the context dropped %q:\n%s", want, ctx)
		}
	}
	if strings.Contains(ctx, system) {
		t.Errorf("the context still carried the system prompt:\n%s", ctx)
	}
	if strings.Count(ctx, `"role": "user"`) != 3 {
		t.Errorf("Dump wrote %d user turns, want one per command and the question each as their own:\n%s", strings.Count(ctx, `"role": "user"`), ctx)
	}
	if !strings.Contains(ctx, "$ git push") || !strings.Contains(ctx, "error: failed to push some refs") {
		t.Errorf("command output was not dumped:\n%s", ctx)
	}
	if strings.Contains(got, "Question:") {
		t.Errorf("Dump still joined the transcript and the question into one turn:\n%s", got)
	}
}

func TestDumpUsesLabeledSections(t *testing.T) {
	got := Dump("chat  ok", Prompts{System: "be brief", Agent: "use the tool"}, "list the files", nil)

	order := []string{
		"======= provider =======",
		"chat  ok",
		"======= default prompt =======",
		"be brief",
		"======= agent prompt =======",
		"use the tool",
		"======= tools =======",
		toolSuggestShellCommand,
		"======= context =======",
		"list the files",
	}
	at := 0
	for _, want := range order {
		i := strings.Index(got[at:], want)
		if i < 0 {
			t.Fatalf("Dump is missing %q after %q:\n%s", want, got[:at], got)
		}
		at += i + len(want)
	}
}

func TestDumpIncludesTheAgentTools(t *testing.T) {
	got := Dump("", Prompts{System: "be brief"}, "list the files", nil)

	if i, j := strings.Index(got, "======= tools ======="), strings.Index(got, "======= context ======="); i < 0 || j < 0 || i > j {
		t.Errorf("the tools should precede the context:\n%s", got)
	}
	for _, want := range []string{
		"======= tools =======",
		toolSuggestShellCommand,
		"Type one shell command",
		`"command"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Dump dropped %q:\n%s", want, got)
		}
	}
}

func TestDumpPrintsInMemoryMessages(t *testing.T) {
	got := renderDump("", Prompts{}, "", nil, []message{
		{Role: roleSystem, Content: "agent system"},
		{Role: roleUser, Content: "list tmp"},
		{Role: roleAssistant, ToolCalls: []toolCall{{
			ID:   "call_1",
			Type: "function",
			Function: toolCallArgs{
				Name:      toolSuggestShellCommand,
				Arguments: `{"command":"ls /tmp"}`,
			},
		}}},
		{Role: roleTool, ToolCallID: "call_1", Content: "$ ls /tmp\nfile.txt"},
	}, formatTools(buildToolSpecs(nil)))
	ctx := got[strings.Index(got, "======= context ======="):]

	for _, want := range []string{
		`"role": "user"`,
		"list tmp",
		`"tool_calls"`,
		toolSuggestShellCommand,
		`{\"command\":\"ls /tmp\"}`,
		`"role": "tool"`,
		"file.txt",
	} {
		if !strings.Contains(ctx, want) {
			t.Errorf("in-memory messages were not dumped (%q):\n%s", want, ctx)
		}
	}
	if strings.Contains(ctx, "agent system") {
		t.Errorf("the system prompt was repeated in the context:\n%s", ctx)
	}
}

func TestQuestionMessagesKeepPriorCommandsAsToolCalls(t *testing.T) {
	transcript := []Turn{{
		Question: "list tmp",
		Answer:   "ls /tmp",
		Text:     "$ ls /tmp\nfile.txt",
		Tool:     true,
	}}
	msgs := questionMessages("system", "now summarize", transcript)
	if len(msgs) != 5 {
		t.Fatalf("follow-up has %d messages, want system, question, tool call, result, follow-up: %+v", len(msgs), msgs)
	}
	if len(msgs[2].ToolCalls) != 1 || msgs[2].ToolCalls[0].Function.Name != toolSuggestShellCommand {
		t.Errorf("the prior ??? command is not a tool call: %+v", msgs[2])
	}
	if msgs[2].Content != "" {
		t.Errorf("the tool call still has assistant content: %+v", msgs[2])
	}
	if msgs[3].Role != roleTool || !strings.Contains(msgs[3].Content, "file.txt") {
		t.Errorf("the tool result is %+v", msgs[3])
	}
}

func TestQuestionMessagesCompleteACancelledToolCall(t *testing.T) {
	transcript := []Turn{{
		Question: "say hi",
		Answer:   "echo hi",
		Tool:     true,
	}}
	msgs := questionMessages("system", "hello world", transcript)

	if len(msgs) != 5 {
		t.Fatalf("follow-up has %d messages, want system, question, tool call, cancellation result, follow-up: %+v", len(msgs), msgs)
	}
	call := msgs[2]
	result := msgs[3]
	if call.Role != roleAssistant || len(call.ToolCalls) != 1 {
		t.Fatalf("the cancelled command is not a tool call: %+v", call)
	}
	if result.Role != roleTool || result.ToolCallID != call.ToolCalls[0].ID {
		t.Fatalf("the cancelled tool call has no matching result: call=%+v result=%+v", call, result)
	}
	if !strings.Contains(result.Content, "cancelled") || !strings.Contains(result.Content, "before it ran") {
		t.Errorf("the cancelled tool result does not explain that the command never ran: %+v", result)
	}
	if msgs[4].Role != roleUser || msgs[4].Content != "hello world" {
		t.Errorf("the follow-up does not come after the completed tool interaction: %+v", msgs)
	}
}

func TestDumpKeepsPlainAnswersAsAssistantContent(t *testing.T) {
	got := Dump("", Prompts{}, "", []Turn{{
		Question: "list tmp",
		Answer:   "ls /tmp",
		Text:     "$ ls /tmp\nfile.txt",
	}})
	ctx := got[strings.Index(got, "======= context ======="):]
	if !strings.Contains(ctx, `"content": "ls /tmp"`) {
		t.Errorf("a ? answer was not dumped as assistant content:\n%s", ctx)
	}
	if strings.Contains(ctx, `"tool_calls"`) {
		t.Errorf("a ? answer was dumped as a tool call:\n%s", ctx)
	}
}

func TestDumpContextEncodesToolCallsAsJSON(t *testing.T) {
	got := prettyMessages([]message{
		{Role: roleAssistant, ToolCalls: []toolCall{{
			ID:   "call_1",
			Type: "function",
			Function: toolCallArgs{
				Name:      toolSuggestShellCommand,
				Arguments: `{"command":"ls /tmp"}`,
			},
		}}},
		{Role: roleTool, ToolCallID: "call_1", Content: "file.txt"},
	})

	for _, want := range []string{
		`"role": "assistant"`,
		`"tool_calls"`,
		toolSuggestShellCommand,
		`{\"command\":\"ls /tmp\"}`,
		`"role": "tool"`,
		`"tool_call_id": "call_1"`,
		"file.txt",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prettyMessages dropped %q:\n%s", want, got)
		}
	}
	if !json.Valid([]byte(got)) {
		t.Errorf("prettyMessages returned invalid JSON:\n%s", got)
	}
}

func TestAnAnswerThatNeverRanIsItsOwnAssistantTurn(t *testing.T) {
	transcript := []Turn{
		{Text: "$ pwd\n/tmp"},
		{Question: "show docker images", Answer: "docker image ls"},
	}
	msgs := questionMessages("system", "what does that command do", transcript)

	if len(msgs) != 5 {
		t.Fatalf("the question sends %d messages, want system, one command, the question that produced the answer, the answer, and the follow-up: %+v", len(msgs), msgs)
	}
	if msgs[2].Role != roleUser || msgs[2].Content != "show docker images" {
		t.Errorf("the question that produced the answer is not the user turn before it: %+v", msgs[2])
	}
	if msgs[3].Role != roleAssistant || msgs[3].Content != "docker image ls" {
		t.Errorf("the answer that never ran is not the assistant turn before the follow-up: %+v", msgs[3])
	}
	if msgs[4].Role != roleUser || msgs[4].Content != "what does that command do" {
		t.Errorf("the question does not follow the thrown-away answer: %+v", msgs[4])
	}
}

func TestAFollowUpKeepsTheQuestionThatProducedTheAnswer(t *testing.T) {
	transcript := []Turn{
		{Text: "$ pwd\n/tmp"},
		{
			Question: "what is my ip",
			Answer:   `ifconfig | grep "inet "`,
			Text:     "$ ifconfig\n192.0.2.1",
		},
	}
	msgs := questionMessages("system", "what is my public ip", transcript)

	if len(msgs) != 6 {
		t.Fatalf("the follow-up sends %d messages, want system, the typed command, the question, its answer, what ran, and the follow-up: %+v", len(msgs), msgs)
	}
	if msgs[2].Role != roleUser || msgs[2].Content != "what is my ip" {
		t.Errorf("the follow-up dropped the question that produced the answer: %+v", msgs[2])
	}
	if msgs[3].Role != roleAssistant || msgs[3].Content != `ifconfig | grep "inet "` {
		t.Errorf("the answer is not the assistant turn after its question: %+v", msgs[3])
	}
	if msgs[4].Role != roleUser || msgs[4].Content != transcript[1].Text {
		t.Errorf("the command that ran is not the user turn after the answer: %+v", msgs[4])
	}
	if msgs[5].Role != roleUser || msgs[5].Content != "what is my public ip" {
		t.Errorf("the last message is not the follow-up: %+v", msgs[5])
	}
}

func TestTheDefaultPromptDescribesTheTranscript(t *testing.T) {
	prompt := bundledPrompts().System

	for _, want := range []string{"PS1", "shell transcript", "shell command output"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the default system prompt never mentions %q:\n%s", want, prompt)
		}
	}
}

func TestTheAgentPromptTellsTheModelToDoTheWork(t *testing.T) {
	prompt := bundledPrompts().Agent

	for _, want := range []string{
		toolSuggestShellCommand,
		"Use only tool calls to complete the task", "Read each result",
		"Take the actions needed", "finish without further user input",
		"one command per shell tool call", "Every line must start with #",
		"or the shell prompt", "PS1",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the default agent prompt never mentions %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "read-only") {
		t.Errorf("the default agent prompt still limits the tool to read-only commands:\n%s", prompt)
	}
}

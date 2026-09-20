package llm

import (
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// Dump is what `?#` prints when no client has been asked yet: provider
// checks, both system prompts, the tools a ??? would attach, and the
// transcript as a pretty-printed messages array. System prompts are
// their own sections, so they are not repeated in the context.
func Dump(provider string, prompts Prompts, question string, transcript []Turn) string {
	return renderDump(provider, prompts, question, transcript, nil, formatTools(buildToolSpecs(nil)))
}

// DumpChat is `?#` after a question: the context is the messages the
// client last held, including tool_calls, not a rebuilt transcript.
func DumpChat(provider string, prompts Prompts, question string, transcript []Turn, ask, think Asker) string {
	return renderDump(provider, prompts, question, transcript, lastChat(ask, think), formatTools(toolsOf(think, ask)))
}

func renderDump(provider string, prompts Prompts, question string, transcript []Turn, chat []message, tools string) string {
	var b strings.Builder
	appendDumpSection(&b, "provider", provider)
	appendDumpSection(&b, "default prompt", prompts.System)
	appendDumpSection(&b, "agent prompt", prompts.Agent)
	appendDumpSection(&b, "tools", tools)
	appendDumpSection(&b, "context", formatDumpMessages(question, transcript, chat))
	return b.String()
}

func toolsOf(primary, fallback Asker) []toolSpec {
	type holder interface {
		availableTools() []toolSpec
	}
	if value, ok := primary.(holder); ok {
		return value.availableTools()
	}
	if value, ok := fallback.(holder); ok {
		return value.availableTools()
	}
	return buildToolSpecs(nil)
}

func lastChat(ask, think Asker) []message {
	msgs, at := chatOf(ask)
	if think == nil || ask == think {
		return msgs
	}
	other, otherAt := chatOf(think)
	if otherAt.After(at) {
		return other
	}
	return msgs
}

func chatOf(a Asker) ([]message, time.Time) {
	type holder interface {
		chatSnapshot() ([]message, time.Time)
	}
	h, ok := a.(holder)
	if !ok {
		return nil, time.Time{}
	}
	return h.chatSnapshot()
}

func appendDumpSection(b *strings.Builder, name, body string) {
	if b.Len() > 0 {
		b.WriteByte('\n')
	}
	b.WriteString("======= ")
	b.WriteString(name)
	b.WriteString(" =======\n\n")
	body = strings.TrimRight(body, "\n")
	if body != "" {
		b.WriteString(body)
		b.WriteByte('\n')
	}
}

func formatDumpMessages(question string, transcript []Turn, chat []message) string {
	msgs := dumpMessages(transcript, chat)
	if q := strings.TrimSpace(question); q != "" && !lastUserContent(msgs, q) {
		msgs = append(msgs, message{Role: roleUser, Content: q})
	}
	return prettyMessages(msgs)
}

// dumpMessages is the context `?#` prints. In-memory chat wins when present;
// otherwise the transcript is used.
func dumpMessages(transcript []Turn, chat []message) []message {
	if held := withoutSystem(chat); len(held) > 0 {
		return held
	}
	return transcriptMessages(transcript)
}

func withoutSystem(msgs []message) []message {
	out := make([]message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == roleSystem {
			continue
		}
		out = append(out, m)
	}
	return out
}

func lastUserContent(msgs []message, q string) bool {
	for _, msg := range slices.Backward(msgs) {
		if msg.Role == roleUser {
			return msg.Content == q
		}
	}
	return false
}

func prettyMessages(msgs []message) string {
	raw, _ := json.MarshalIndent(msgs, "", "  ")
	return string(raw) + "\n"
}

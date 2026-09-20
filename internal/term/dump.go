//go:build darwin || linux

package term

import (
	"fmt"
	"os"
	"strings"

	"github.com/TheR1D/aty/internal/llm"
)

const dumpPrefix = '#'

func dumpRequest(question string) (string, bool) {
	if question == "" || question[0] != dumpPrefix {
		return "", false
	}
	return strings.TrimSpace(question[1:]), true
}

func (k *keyboard) dumpSubmitted(plan queryRequest) error {
	q := k.query
	ctx := k.beginRequest(q)
	cancel := q.cancel
	dump := k.dump
	go func() {
		text := contextText(dump, plan.question, plan.transcript)
		if ctx.Err() != nil {
			return
		}
		path, err := writeContextFile(text)
		cancel()

		k.mu.Lock()
		defer k.mu.Unlock()
		if q.editor.finished() {
			if path != "" {
				_ = os.Remove(path)
			}
			return
		}
		if err != nil {
			_ = k.end()
			return
		}
		if k.end() == nil {
			_ = k.toShell([]byte(catCommand(path)))
		}
	}()
	return q.render(q.editor.snapshot())
}

func contextText(format func(string, []llm.Turn) string, question string, transcript []llm.Turn) string {
	if format != nil {
		return format(question, transcript)
	}
	var text strings.Builder
	for _, turn := range transcript {
		if question := strings.TrimSpace(turn.Question); question != "" {
			writeTurn(&text, "user", question)
			text.WriteByte('\n')
		}
		if turn.Answer != "" {
			writeTurn(&text, "assistant", turn.Answer)
			text.WriteByte('\n')
		}
		if turn.Text != "" {
			writeTurn(&text, "user", turn.Text)
			text.WriteByte('\n')
		}
	}
	writeTurn(&text, "user", strings.TrimSpace(question))
	return text.String()
}

func writeTurn(text *strings.Builder, role, content string) {
	text.WriteString(role)
	text.WriteString(":\n")
	text.WriteString(content)
	if content == "" || !strings.HasSuffix(content, "\n") {
		text.WriteByte('\n')
	}
}

func writeContextFile(text string) (string, error) {
	file, err := os.CreateTemp("", "aty-context-*")
	if err != nil {
		return "", fmt.Errorf("term: creating the context dump: %w", err)
	}
	path := file.Name()
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.WriteString(text); err != nil {
		return "", fmt.Errorf("term: writing the context dump: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("term: closing the context dump: %w", err)
	}
	ok = true
	return path, nil
}

func catCommand(path string) string {
	return "cat '" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

//go:build darwin || linux

package term

import (
	"os"
	"strings"
	"testing"

	"github.com/TheR1D/aty/internal/llm"
)

func TestContextDumpFormattingIsDeterministic(t *testing.T) {
	turns := []llm.Turn{{
		Question: "what changed",
		Answer:   "git status",
		Text:     "$ git status\nclean",
	}}
	got := contextText(nil, "what next", turns)
	want := "user:\nwhat changed\n\nassistant:\ngit status\n\nuser:\n$ git status\nclean\n\nuser:\nwhat next\n"
	if got != want {
		t.Fatalf("context dump =\n%q\nwant\n%q", got, want)
	}
}

func TestContextDumpShellCommandQuotesPath(t *testing.T) {
	command := catCommand("/tmp/user's context")
	if command != `cat '/tmp/user'\''s context'` {
		t.Fatalf("cat command = %q", command)
	}
	if strings.ContainsAny(command, "\r\n") {
		t.Fatalf("cat command contains a line ending: %q", command)
	}
}

func TestContextDumpFileIsPrivateAndUnique(t *testing.T) {
	first, err := writeContextFile("private context")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(first) })
	second, err := writeContextFile("other context")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(second) })

	if first == second {
		t.Fatalf("context dumps reused predictable path %q", first)
	}
	info, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("context dump mode = %o, want 600", mode)
	}
	content, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "private context" {
		t.Errorf("context dump = %q", content)
	}
}

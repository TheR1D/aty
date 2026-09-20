//go:build darwin || linux

package term

import (
	"errors"
	"strings"
	"testing"
)

func TestCommandInjectionSafety(t *testing.T) {
	// A newline is typable because it is pasted rather than typed, and a line
	// editor collecting a paste holds it as text.
	for _, command := range []string{"echo ok", "printf 'café'", "# explanation", "echo one\necho two"} {
		if !typable(command) {
			t.Errorf("typable(%q) = false", command)
		}
	}
	for _, command := range []string{"echo\rwhoami", "echo\x1b[A", "echo\x00"} {
		if typable(command) {
			t.Errorf("typable(%q) = true", command)
		}
	}
}

func TestFailedAnswerBecomesOneSafeComment(t *testing.T) {
	got := failedLine(errors.New("llm: first\nsecond\tthird"))
	if got != "# first second third" || !typable(got) {
		t.Fatalf("failedLine = %q, want one typable comment", got)
	}
}

func TestRevisionReconstructsUnicodeCommand(t *testing.T) {
	for _, test := range []struct{ typed, command string }{
		{"printf cafe", "printf café"},
		// Erasing back across a newline: the line editor takes one back for
		// the newline the same as for any other character.
		{"echo one\necho two", "echo one"},
		{"echo one", "echo one\necho two"},
	} {
		erase, insert := revision([]rune(test.typed), []rune(test.command))
		got := []rune(test.typed)
		for range erase {
			got = got[:len(got)-1]
		}
		got = append(got, []rune(insert)...)
		if string(got) != test.command {
			t.Errorf("revision(%q, %q) reconstructed %q", test.typed, test.command, string(got))
		}
	}
}

// TestRevisionKeepsBackspacesOutOfThePastedText is the one that has to hold:
// a backspace sent inside a bracketed paste is inserted as a literal ^? and
// ends up in the command that runs.
func TestRevisionSeparatesErasureFromText(t *testing.T) {
	erase, insert := revision([]rune("echo one\necho zwei"), []rune("echo one\necho two"))
	if strings.ContainsAny(insert, "\x7f\b") {
		t.Fatalf("revision put an erasure in the text to insert: %q", insert)
	}
	if len(erase) != 4 || insert != "two" {
		t.Fatalf("revision = %d backspaces, %q; want 4, %q", len(erase), insert, "two")
	}
}

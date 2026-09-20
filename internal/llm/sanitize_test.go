package llm

import "testing"

// TestSanitizeKeepsOnlyWhatMayBeTypedAtAShell covers the last thing between a
// model and a real prompt. The cases that matter are the ones where the model
// did not do as it was told, since the ones where it did need no sanitizing at
// all.
func TestSanitizeKeepsOnlyWhatMayBeTypedAtAShell(t *testing.T) {
	tests := []struct {
		name    string
		answer  string
		command string
	}{
		{
			name:   "a command still arriving is not finished",
			answer: "git switch -c ",
			// Trailing space would put the cursor a space away from where the
			// next piece of the command goes.
			command: "git switch -c",
		},
		{
			name:    "a newline is part of the command now, not the end of it",
			answer:  "cat <<'EOF' > f\nhello\nEOF",
			command: "cat <<'EOF' > f\nhello\nEOF",
		},
		{
			name: "so everything the model says after the command is typed too",
			// The system prompt is what stops this. A sanitizer that guessed
			// where the command ended would guess wrong on a heredoc.
			answer:  "ls -la\nThis lists every file, including the hidden ones.",
			command: "ls -la\nThis lists every file, including the hidden ones.",
		},
		{
			name: "a carriage return is dropped, because a terminal reads one as Enter",
			// It would submit the line even inside a paste on a terminal that
			// did not translate it.
			answer:  "df -h\r\ndu -h\r",
			command: "df -h\ndu -h",
		},
		{
			name:   "an answer that opens with blank space has not said anything yet",
			answer: "\n\n",
		},
		{
			name:   "and the command is what follows it",
			answer: "\n  echo hi",
			// Still unfinished: the stream is what ends a command, so this may
			// yet grow.
			command: "echo hi",
		},
		{
			name: "a command is never left holding a blank last line",
			// The next line of it is still arriving; typing the newline now
			// would move the cursor off the line the rest goes on.
			answer:  "for f in *; do\n",
			command: "for f in *; do",
		},
		{
			name:   "control characters are not typed, since a line editor would act on them",
			answer: "ls\x07 -l\x1b[A",
			// What is left of the escape sequence is its printable tail, which
			// is a wrong command rather than a keypress the user did not make.
			command: "ls -l[A",
		},
		{
			name:    "a tab between words separates them",
			answer:  "go\ttest ./...",
			command: "go test ./...",
		},
		{
			name:   "an answer made of nothing but control characters is no answer",
			answer: "\x00\x07",
		},
		{
			name:    "a fence is not unwrapped, because guessing what the model meant is worse",
			answer:  "```bash",
			command: "```bash",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if command := sanitize(test.answer); command != test.command {
				t.Errorf("sanitize(%q) = %q, want %q", test.answer, command, test.command)
			}
		})
	}
}

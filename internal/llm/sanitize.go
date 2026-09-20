package llm

import (
	"strings"
	"unicode"
)

// sanitize removes terminal controls from a complete command revision. Newlines
// remain for bracketed paste; tabs become spaces so adjacent words stay separate.
// Fences, prose, and shell syntax are left untouched: interpreting model output
// here could change the command. Outer whitespace is trimmed on every revision
// and naturally returns when later fragments make it interior whitespace.
func sanitize(answer string) string {
	answer = strings.TrimLeftFunc(answer, unicode.IsSpace)

	answer = strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r
		case r == '\t':
			return ' '
		case r < 0x20, r == 0x7f:
			return -1
		}
		return r
	}, answer)

	return strings.TrimRightFunc(answer, unicode.IsSpace)
}

// reportLine makes every line of an agent's final report a shell comment,
// including partial reports displayed while the answer is still streaming.
func reportLine(answer string) string {
	if answer == "" {
		return answer
	}
	lines := strings.Split(answer, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "#") {
			lines[i] = "# " + line
		}
	}
	return strings.Join(lines, "\n")
}

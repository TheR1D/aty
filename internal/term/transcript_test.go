//go:build darwin || linux

package term

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	appconfig "github.com/TheR1D/aty/internal/config"
	"github.com/TheR1D/aty/internal/helpers"
)

func (c *chunk) addLine(line string) {
	c.addOutput(textParts{head: line})
}

// expectedPreview is a whole-string reference, independent of streaming storage.
func expectedPreview(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	if limit < len(helpers.OutputTruncationMarker) {
		return ""
	}
	budget := limit - len(helpers.OutputTruncationMarker)
	head, tail := budget/5, len(text)-(budget-budget/5)
	return text[:head] + helpers.OutputTruncationMarker + text[tail:]
}

func TestOutputKeepsHeadAndTailWhileStreaming(t *testing.T) {
	for _, limit := range []int{1, 40, 200, appconfig.DefaultCommandBytes} {
		c := chunk{prompt: "$ ", command: "cat", answer: "answer", question: "question", maxBytes: limit}
		var all strings.Builder
		for i := range 1024 {
			line := fmt.Sprintf("%d: café 世界", i)
			all.WriteString("\n" + line)
			c.addLine(line)
			want := "$ cat" + expectedPreview(all.String(), max(0, limit-19))
			if c.text() != want || c.size() != len(want)+14 {
				t.Fatalf("limit %d, line %d: got %q, want %q", limit, i, c.text(), want)
			}
		}
		c.hasExit, c.exit = true, 123
		c.trim(limit)
		if strings.Count(c.text(), helpers.OutputTruncationMarker) > 1 {
			t.Fatalf("invalid truncated result: %q", c.text())
		}
	}
}

func TestOutputRingRetainsShortLinesWithinByteBudget(t *testing.T) {
	var want strings.Builder
	want.WriteString("cat")
	c := chunk{command: "cat"}
	for i := range 1000 {
		line := fmt.Sprint(i)
		c.addLine(line)
		want.WriteByte('\n')
		want.WriteString(line)
	}
	if got := c.text(); got != want.String() || c.size() != want.Len() {
		t.Fatal("ring did not preserve the latest lines and their exact size")
	}
	c.trim(len(c.command))
	if c.text() != "cat" || c.size() != 3 {
		t.Fatal("trimming all lines changed the command or left output behind")
	}
	c.maxBytes = appconfig.DefaultCommandBytes
	c.output = boundedText{}
	c.addLine("new")
	if c.text() != "cat\nnew" || c.size() != 7 {
		t.Fatal("adding after trimming all lines lost order or size")
	}
}

func TestChunkSizeIncludesOnlySerializedText(t *testing.T) {
	for _, command := range []string{"", " \t", "é世界"} {
		for _, exit := range []int{-123, 0, 255} {
			c := chunk{prompt: "$ ", command: command, answer: "答", question: "?", hasExit: true, exit: exit}
			c.addLine("output")
			c.trim(20)
			if c.size() != len(c.text())+len(c.answer)+len(c.question) || !utf8.ValidString(c.command) {
				t.Fatalf("wrong size or broken UTF-8 for %+v", c)
			}
		}
	}
	c := chunk{command: " ", answer: strings.Repeat("a", 100)}
	c.addLine("hidden output")
	c.trim(10)
	if c.output.length != 0 || c.size() != 100 || c.text() != "" {
		t.Fatal("unserialized output affected trimming an answer-only turn")
	}
}

//go:build darwin || linux

package term

import (
	"strconv"
	"strings"
	"unicode/utf8"

	appconfig "github.com/TheR1D/aty/internal/config"
	"github.com/TheR1D/aty/internal/llm"
)

const exitPrefix = "exit code = "

type transcriptStore struct {
	entries   []chunk
	bytes     int
	maxBytes  int
	keepBytes int
}

func (s *transcriptStore) append(entry chunk) {
	s.entries = append(s.entries, entry)
	s.bytes += entry.size()
	s.evict()
}

func (s *transcriptStore) turns() []llm.Turn {
	s.evict()
	if len(s.entries) == 0 {
		return nil
	}
	turns := make([]llm.Turn, 0, len(s.entries))
	for i := range s.entries {
		entry := &s.entries[i]
		turns = append(turns, llm.Turn{
			Question: entry.question,
			Answer:   entry.answer,
			Text:     entry.text(),
			Tool:     entry.tool,
		})
	}
	return turns
}

func (s *transcriptStore) clear() {
	clear(s.entries)
	s.entries = s.entries[:0]
	s.bytes = 0
}

func (s *transcriptStore) evict() {
	if s.bytes <= s.maxBytes {
		return
	}
	drop := 0
	for drop < len(s.entries)-1 && s.bytes > s.keepBytes {
		s.bytes -= s.entries[drop].size()
		drop++
	}
	clear(s.entries[:drop])
	s.entries = s.entries[drop:]
}

type chunk struct {
	prompt, command  string
	answer, question string
	tool             bool
	output           boundedText
	maxBytes         int
	hasExit          bool
	exit             int
}

func (c *chunk) outputBudget(limit int) int {
	overhead := len(c.prompt) + len(c.command) + len(c.answer) + len(c.question)
	if c.hasExit {
		overhead += 1 + len(exitPrefix) + len(strconv.Itoa(c.exit))
	}
	return max(0, limit-overhead)
}

func (c *chunk) limit() int {
	if c.maxBytes == 0 {
		return appconfig.DefaultCommandBytes
	}
	return c.maxBytes
}

func (c *chunk) addOutput(line textParts) {
	budget := c.outputBudget(c.limit())
	c.output.append('\n', budget)
	c.output.appendParts(line, budget)
}

func (c *chunk) empty() bool { return strings.TrimSpace(c.command) == "" }

func (c *chunk) size() int { return c.textSize() + len(c.answer) + len(c.question) }

func (c *chunk) textSize() int {
	if c.empty() {
		return 0
	}
	n := len(c.prompt) + len(c.command) + len(c.output.text(c.outputBudget(c.limit())))
	if c.hasExit {
		n += 1 + len(exitPrefix) + len(strconv.Itoa(c.exit))
	}
	return n
}

func (c *chunk) text() string {
	if c.empty() {
		return ""
	}
	var text strings.Builder
	text.Grow(c.textSize())
	text.WriteString(c.prompt)
	text.WriteString(c.command)
	text.WriteString(c.output.text(c.outputBudget(c.limit())))
	if c.hasExit {
		text.WriteByte('\n')
		text.WriteString(exitPrefix)
		text.WriteString(strconv.Itoa(c.exit))
	}
	return text.String()
}

func (c *chunk) trim(limit int) {
	size := c.trimOutput(limit)
	if extra := size - limit; extra > 0 && len(c.command) > extra {
		for extra < len(c.command) && !utf8.RuneStart(c.command[extra]) {
			extra++
		}
		c.command = c.command[extra:]
	}
}

// trimOutput bounds arriving output without changing the running command.
// Prompt, command and AI text share the budget; exit metadata is added later.
func (c *chunk) trimOutput(limit int) int {
	c.maxBytes = limit
	if c.empty() {
		c.output = boundedText{}
	} else {
		c.output.bound(c.outputBudget(limit))
	}
	return c.size()
}

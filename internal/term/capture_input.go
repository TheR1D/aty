//go:build darwin || linux

package term

import (
	"strings"
	"unicode/utf8"
)

// typed remembers printable input for prompt recovery and treats Ctrl-C as
// cancellation, even if the editor subsequently emits a command-start marker.
func (c *capture) typed(p []byte) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rememberInitialPromptLocked()
	for i := 0; i < len(p); i++ {
		b := p[i]
		switch {
		case b == ctrlC:
			c.keepPendingLocked()
			c.typedLine = c.typedLine[:0]
			c.seq = inputSequence{}
			if c.echoing {
				c.echo = c.echo[:0]
				c.aborted = true
				c.line.reset()
				c.outputLine.reset()
			}
		case !c.echoing:
		case c.seq.active():
			c.seq.feed(b)
		case b == esc:
			c.seq.start()
		case b == backspaceKey || b == del:
			if n := len(c.typedLine); n > 0 {
				c.typedLine = c.typedLine[:n-1]
			}
		case b == '\n':
			// Pasted newlines must remain part of the echoed command suffix.
			c.typedLine = append(c.typedLine, '\n')
		case b >= space:
			r, size := utf8.DecodeRune(p[i:])
			if r != utf8.RuneError || size != 1 {
				c.typedLine = append(c.typedLine, r)
				i += size - 1
			}
		}
	}
}

func (c *capture) resetEchoLocked() {
	c.echo = c.echo[:0]
	c.typedLine = c.typedLine[:0]
	c.seq = inputSequence{}
}

// Freeze the startup prompt before input can be echoed onto its line.
// Bash can draw its prompt after enabling bracketed paste, in a later read.
func (c *capture) rememberInitialPromptLocked() {
	if c.initialPromptSet || !c.echoing || c.alt {
		return
	}
	c.initialPrompt = c.prompt
	if c.initialPrompt == "" {
		if len(c.echo) > 0 {
			c.initialPrompt = strings.Join(c.echo, "\n") + "\n"
		}
		c.initialPrompt += c.line.text()
	}
	c.initialPromptSet = true
}

func (c *capture) initialPS1() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rememberInitialPromptLocked()
	return c.initialPrompt
}

// answered associates a generated command with its question until it runs or
// is discarded. An empty question preserves the one pending for an agent run.
// Manual edits retain this association; capture does not query the line editor.
func (c *capture) answered(question, command string, tool bool) {
	if c == nil || command == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pendingAnswer = command
	c.pendingTool = tool
	if q := strings.TrimSpace(question); q != "" {
		c.pendingQuestion = q
	}
}

// discardAnswer preserves an unused answer when a fresh query proves the
// previous input line was erased.
func (c *capture) discardAnswer() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keepPendingLocked()
}

// keepPendingLocked saves an unexecuted answer and clears its association.
func (c *capture) keepPendingLocked() {
	c.keepAnswerLocked(c.pendingAnswer, c.pendingQuestion, c.pendingTool)
	c.clearPendingLocked()
}

func (c *capture) clearPendingLocked() {
	c.pendingAnswer = ""
	c.pendingQuestion = ""
	c.pendingTool = false
}

// keepAnswerLocked stores an assistant answer without claiming it ran.
func (c *capture) keepAnswerLocked(answer, question string, tool bool) {
	if answer == "" {
		return
	}
	ch := chunk{answer: answer, question: question, tool: tool}
	c.history.append(ch)
}

// echoedCommandLocked separates PS1 from the command at editor shutdown.
// Prefer the saved prompt; if bash drew PS1 later, recover it from a matching
// typed suffix. Completion may change the echo, so typed input is only a fallback.
func (c *capture) echoedCommandLocked() (prompt, cmd string) {
	n := len(c.echo)
	partial := c.line.text()
	if n == 0 && partial == "" && len(c.typedLine) == 0 {
		return "", ""
	}
	lines := make([]string, n, n+1)
	copy(lines, c.echo)
	if partial != "" {
		lines = append(lines, partial)
	}
	raw := strings.Join(lines, "\n")
	if c.prompt != "" && len(lines) > 0 {
		stripped := strings.TrimPrefix(lines[0], c.prompt)
		if stripped != lines[0] {
			lines[0] = stripped
			return c.prompt, strings.Join(lines, "\n")
		}
	}
	typed := string(c.typedLine)
	if typed == "" {
		return c.prompt, raw
	}
	// Saved prompt was empty (or stripping it left the whole line), so
	// split on typed keys if they are a suffix of the echoed line.
	if c.prompt == "" || raw == partial {
		if strings.HasSuffix(raw, typed) {
			return raw[:len(raw)-len(typed)], typed
		}
		if strings.HasSuffix(partial, typed) {
			return partial[:len(partial)-len(typed)], typed
		}
	}
	return c.prompt, raw
}

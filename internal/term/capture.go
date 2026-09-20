//go:build darwin || linux

package term

import (
	"strings"
	"sync"

	appconfig "github.com/TheR1D/aty/internal/config"
	"github.com/TheR1D/aty/internal/llm"
)

// capture builds an in-memory transcript from terminal input and output.
// Bracketed-paste mode marks the transition between editing and running a
// command. All methods ending in Locked require mu.
type capture struct {
	mu         sync.Mutex
	line       linebuf
	outputLine boundedText
	open       *chunk
	history    transcriptStore
	limits     appconfig.Limits
	alt        bool

	initialPrompt    string
	initialPromptSet bool
	completed        uint64 // monotonic, independent of transcript eviction
	lastResult       string
	cleared          bool // a clear command committed during the current feed

	// A generated answer moves into the next command or becomes its own turn
	// if the user cancels or erases it. Agent calls keep their tool identity.
	pendingAnswer   string
	pendingQuestion string
	pendingTool     bool

	// prompt is the line at promptReady; typedLine recovers PS1 when the shell
	// prints it later. Input escape sequences must stay out of that fallback.
	prompt    string
	typedLine []rune
	seq       inputSequence
	aborted   bool // Ctrl-C cancelled the line before commandStart

	// Between promptReady and commandStart, flushed lines are input echoes.
	// Keep them because the newline and mode toggle can arrive in either order.
	echoing bool
	echo    []string
}

func newCapture(limits appconfig.Limits) *capture {
	limits = limits.WithDefaults()
	return &capture{
		history: transcriptStore{maxBytes: limits.TranscriptBytes, keepBytes: limits.TranscriptKeepBytes},
		line:    linebuf{maxCells: limits.CommandBytes},
		limits:  limits,
	}
}

// feed uses positioned mode events to separate prompt echoes from command
// output, even when both arrive in one read. Alternate-screen bytes are ignored.
// It reports a committed clear command; p is consumed without retaining it.
func (c *capture) feed(p []byte, events []modeEvent) bool {
	if c == nil {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.cleared = false

	prev := 0
	for _, ev := range events {
		if !c.alt {
			c.ingestBytesLocked(p[prev:ev.at])
		}
		switch ev.kind {
		case altOn, altOff:
			c.alt = ev.kind == altOn
			c.line.reset()
			c.outputLine.reset()
		case promptReady:
			if !c.alt {
				c.promptReadyLocked()
			}
		case commandStart:
			if !c.alt {
				c.commandStartLocked()
			}
		}
		prev = ev.at
	}
	if !c.alt {
		c.ingestBytesLocked(p[prev:])
	}
	return c.cleared
}

func (c *capture) ingestBytesLocked(p []byte) {
	for _, b := range p {
		// Preserve output in arrival order. The prompt parser consumes escape
		// sequences, but output never applies their cursor edits. UTF-8 bytes,
		// tabs, backspaces and standalone carriage returns pass through here.
		if !c.echoing && c.open != nil && (c.line.phase == lineUTF8 ||
			c.line.phase == lineGround && b != esc && b != '\n') {
			c.outputLine.append(b, c.limits.CommandBytes)
		}
		if line, ok := c.line.consume(b); ok {
			if c.echoing {
				c.echo = append(c.echo, line)
			} else if c.open != nil {
				c.outputLine.trimCR()
				if c.outputLine.length > 0 {
					c.open.addOutput(c.outputLine.parts())
				}
			}
			c.outputLine.reset()
		}
	}
	c.takeExitLocked()
}

// takeExitLocked attaches OSC 133 D to the current command before promptReady
// commits it. Always consume the exit status so later commands cannot inherit it.
func (c *capture) takeExitLocked() {
	n, ok := c.line.takeExit()
	if !ok || c.open == nil {
		return
	}
	c.open.hasExit = true
	c.open.exit = n
	c.open.trimOutput(c.limits.CommandBytes)
}

// promptReadyLocked commits the command and snapshots PS1 before input echoes.
// zsh draws its prompt before enabling bracketed paste; bash may draw it after.
func (c *capture) promptReadyLocked() {
	c.commitLocked()
	c.aborted = false
	c.prompt = c.line.text()
	c.resetEchoLocked()
	c.echoing = true
}

// commandStartLocked opens a command from the editor's echo. Ignore cancelled
// and empty lines, including remote-shell startup toggles, without closing an
// existing command such as ssh. Keep an unused answer as its own turn.
func (c *capture) commandStartLocked() {
	aborted := c.aborted
	c.aborted = false
	prompt, cmd := c.echoedCommandLocked()
	cmd = strings.TrimSpace(cmd)
	c.resetEchoLocked()
	c.echoing = false
	c.line.reset()
	c.outputLine.reset()

	if aborted || cmd == "" {
		c.keepPendingLocked()
		return
	}

	c.commitLocked()

	c.open = &chunk{
		maxBytes: c.limits.CommandBytes,
		prompt:   prompt,
		command:  cmd,
		answer:   c.pendingAnswer,
		question: c.pendingQuestion,
		tool:     c.pendingTool,
	}
	c.clearPendingLocked()
}

// recent returns completed commands and unused answers, oldest first. Each
// generated command retains the question and assistant answer that produced it.
func (c *capture) recent() []llm.Turn {
	if c == nil {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.history.turns()
}

// completion is independent of history length, which can shrink on eviction.
func (c *capture) completion() (uint64, string) {
	if c == nil {
		return 0, ""
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.completed, c.lastResult
}

func (c *capture) commitLocked() {
	ch := c.open
	c.open = nil
	if ch == nil {
		return
	}
	if ch.empty() {
		// An erased command can still carry an answer worth preserving.
		c.keepAnswerLocked(ch.answer, ch.question, ch.tool)
		return
	}
	cleared := isClearCommand(ch.command)
	ch.trim(c.limits.CommandBytes)
	c.completed++
	c.lastResult = ch.text()
	if cleared {
		// Clearing the terminal also clears model context; omit clear itself.
		c.history.clear()
		c.cleared = true
		return
	}
	c.history.append(*ch)
}

// isClearCommand recognizes clear, with an optional path and arguments.
// Compound commands that merely start with clear do not erase the transcript.
func isClearCommand(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" || strings.ContainsAny(command, ";&|") {
		return false
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	name := fields[0]
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name == "clear"
}

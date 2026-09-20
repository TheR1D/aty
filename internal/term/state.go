//go:build darwin || linux

// Package term hosts the user's shell in a pty and manages terminal interaction.
package term

import (
	"os"
	"sync"
	"unicode/utf8"

	appconfig "github.com/TheR1D/aty/internal/config"
	"github.com/TheR1D/aty/internal/llm"
)

const (
	triggerKey = '?'
	// Other termios interrupt characters are treated as ordinary editing keys.
	ctrlC = 0x03
)

// state combines terminal modes, input edits, and live process ownership to
// decide whether a query can start. Input and output run concurrently; mu guards
// modes, editor, and line. Process probing and transcript capture lock themselves.
type state struct {
	process processGate

	mu      sync.Mutex
	modes   decModes
	editor  bool // bracketed paste is enabled; processGate checks whose editor
	line    promptLine
	capture *capture
	// onClear invalidates model-side state after capture commits a clear.
	onClear func()
}

func newState(shellPid int, ptmx *os.File, limits appconfig.Limits) *state {
	return &state{
		process: processGate{shellPID: shellPid, ptmx: ptmx},
		capture: newCapture(limits),
	}
}

// observeOutput parses mode events once for both the gate and transcript.
// It reports whether a prompt announcement should restore the cursor tint.
func (s *state) observeOutput(p []byte) bool {
	if len(p) == 0 {
		return false
	}

	s.mu.Lock()
	announcedPrompt := false
	events := s.modes.feed(p)
	for _, ev := range events {
		switch ev.kind {
		case promptReady:
			s.editor = true
			s.line.atPrompt()
			announcedPrompt = true
		case commandStart:
			s.editor = false
		case altOff:
			s.line.screenHandedBack()
		}
	}
	alternate := s.modes.alt
	s.line.printed(p)
	s.mu.Unlock()

	if announcedPrompt {
		s.process.probe()
	}

	if s.capture.feed(p, events) && s.onClear != nil {
		s.onClear()
	}

	return announcedPrompt && !alternate
}

// typed records only the keystrokes forwarded to the shell.
func (s *state) typed(p []byte) {
	if len(p) == 0 {
		return
	}

	s.mu.Lock()
	s.line.typed(p)
	s.mu.Unlock()
	s.capture.typed(p)
}

// pasted counts content as characters, excluding bracketed-paste controls.
// Backspacing away an injected answer can then restore a fresh input line.
func (s *state) pasted(text string) {
	if text == "" {
		return
	}

	s.mu.Lock()
	s.line.inserted(utf8.RuneCountInString(text))
	s.mu.Unlock()
	s.capture.typed([]byte(text))
}

// pasteMode requires an editor on the main screen before inserting multiline text.
func (s *state) pasteMode() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.editor && !s.modes.alt
}

// gate reads process ownership afresh: a cached shell pgid could hide a child
// that just took the terminal. Relay recognition additionally checks argv.
func (s *state) gate() gate {
	s.mu.Lock()
	promptReady := s.editor
	altScreen := s.modes.alt
	freshLine := s.line.fresh()
	s.mu.Unlock()

	g := s.process.current(promptReady)
	g.altScreen = altScreen
	g.promptReady = promptReady
	g.freshLine = freshLine
	return g
}

// triggerState rejects impossible triggers before querying the foreground
// process. A terminal reply in progress must still be consumed byte by byte:
// completing it can restore a fresh input line.
func (s *state) triggerState() (ready, partial bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.editor && !s.modes.alt && s.line.fresh(), s.line.seq.active()
}

// transcript returns completed commands and unexecuted assistant answers.
func (s *state) transcript() []llm.Turn {
	if s == nil {
		return nil
	}
	return s.capture.recent()
}

// commandCompletion is independent of the bounded transcript: a newly
// completed command may evict old turns without increasing its length.
func (s *state) commandCompletion() (uint64, string) {
	if s == nil {
		return 0, ""
	}
	return s.capture.completion()
}

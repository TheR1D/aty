//go:build darwin || linux

package term

import "bytes"

// promptLine tracks whether the shell's editable input is empty without
// querying the shell. Printable characters and backspaces give an exact count;
// other editing keys make it unknown until the next prompt. Terminal replies
// restore the state from before their escape sequence began.
type promptLine struct {
	dirty bool
	ended bool // last key was Enter or Ctrl-C; wait for output to start a new line
	edits int  // known character count, or -1 after an unknown edit
	seq   inputSequence
	// An incomplete sequence counts as an unknown edit. A completed terminal
	// reply restores this snapshot because it did not change the input line.
	held struct {
		dirty bool
		ended bool
		edits int
	}
}

// typed records keystrokes on their way to the shell.
func (l *promptLine) typed(p []byte) {
	for _, b := range p {
		if l.seq.active() {
			if l.seq.feed(b) == sequenceReport {
				l.dirty, l.ended, l.edits = l.held.dirty, l.held.ended, l.held.edits
			}
			continue
		}
		if b == esc {
			l.held.dirty, l.held.ended, l.held.edits = l.dirty, l.ended, l.edits
			l.seq.start()
			l.dirty, l.ended, l.edits = true, false, -1
			continue
		}

		l.ended = b == '\r' || b == '\n' || b == ctrlC

		switch {
		case b == del || b == backspaceKey:
			// Backspace can restore an exactly counted line, never an unknown one.
			if l.edits > 0 {
				l.edits--
			}
			l.dirty = l.edits != 0
		case b == '\r' || b == '\n' || b == ctrlC:
			// The input buffer is gone, but a child may read keys until a prompt returns.
			l.dirty, l.edits = true, 0
		case b < space:
			// Completion, history, and other controls can make arbitrary edits.
			l.dirty, l.edits = true, -1
		case b&0xc0 == 0x80:
			// Count a UTF-8 character once, at its first byte.
			l.dirty = true
		default:
			l.dirty = true
			if l.edits >= 0 {
				l.edits++
			}
		}
	}
}

// inserted counts pasted characters as text, including embedded newlines.
func (l *promptLine) inserted(n int) {
	if n <= 0 {
		return
	}
	l.ended = false
	l.dirty = true
	if l.edits >= 0 {
		l.edits += n
	}
}

// atPrompt clears uncertainty from keys consumed by the previous program.
// Known printable type-ahead remains: the new editor may still read those keys.
func (l *promptLine) atPrompt() {
	if l.edits < 0 {
		l.edits = 0
	}
	l.reckon()
}

// screenHandedBack drops keys consumed by a full-screen program, such as a
// pager's q or vim's ZZ, before returning to the shell.
func (l *promptLine) screenHandedBack() {
	l.edits = 0
	l.reckon()
}

// reckon resets sequence parsing and derives freshness from the known edits.
func (l *promptLine) reckon() {
	l.dirty, l.ended, l.seq = l.edits != 0, false, inputSequence{}
}

// printed restores freshness after Enter or Ctrl-C only once a newline arrives.
// Earlier output may still be the delayed echo of a key typed before Enter.
func (l *promptLine) printed(p []byte) {
	if l.ended && bytes.IndexByte(p, '\n') >= 0 {
		l.dirty, l.ended, l.edits, l.seq = false, false, 0, inputSequence{}
	}
}

// fresh reports whether the line is one the user has not typed into.
func (l *promptLine) fresh() bool { return !l.dirty }

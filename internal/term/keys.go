//go:build darwin || linux

package term

import "unicode/utf8"

// action is the complete input alphabet understood by query editing.
type action uint8

const (
	nothing action = iota
	insertChar
	eraseChar
	moveLeft
	moveRight
	submitQuery
	cancelQuery
)

type keypress struct {
	action action
	char   rune
}

// plainTextLen finds complete text characters already available in this read.
// Control keys and a trailing partial rune go through the normal key decoder.
func plainTextLen(input []byte) int {
	i := 0
	for i < len(input) {
		b := input[i]
		if b < space || b == del {
			break
		}
		if b < utf8.RuneSelf {
			i++
			continue
		}
		if !utf8.FullRune(input[i:]) {
			break
		}
		_, size := utf8.DecodeRune(input[i:])
		i += size
	}
	return i
}

const (
	del          = 0x7f
	backspaceKey = 0x08
	space        = 0x20
	bel          = 0x07
)

// decodeKey consumes exactly one terminal key event. Incomplete UTF-8 and
// control sequences are retained by the caller until another read arrives.
func decodeKey(input []byte) (keypress, []byte, bool) {
	switch first := input[0]; {
	case first == esc:
		return decodeEscapeKey(input)
	case first == ctrlC:
		return keypress{action: cancelQuery}, input[1:], true
	case first == '\r' || first == '\n':
		return keypress{action: submitQuery}, input[1:], true
	case first == del || first == backspaceKey:
		return keypress{action: eraseChar}, input[1:], true
	case first < space:
		return keypress{action: nothing}, input[1:], true
	}

	char, size := utf8.DecodeRune(input)
	if !utf8.FullRune(input) {
		return keypress{}, input, false
	}
	return keypress{action: insertChar, char: char}, input[size:], true
}

// The terminal writes on the same stream the keyboard does. A program that
// asked it something — its colors, its identity, where the cursor is — reads
// the answer as input, and a terminal whose window gained or lost the focus
// says so without being asked. Those bytes have to reach whatever they belong
// to, but they are not the user typing: the line the shell's editor is holding
// is exactly as it was, and counting them as typing is what would leave an
// empty prompt looking written on, with nowhere for a question to be asked.
//
// The two are told apart by shape. Every string sequence — OSC, DCS, APC — is
// an answer, since no key sends one, and a CSI is an answer when it carries an
// intermediate byte or ends in a final no key produces.
type inputSequence struct {
	phase   inputPhase
	private byte
	// marked is whether a CSI intermediate byte has been seen. DECRPM's
	// replies end `$y`, and no key carries one.
	marked bool
	length int
}

type inputPhase uint8

const (
	inputGround inputPhase = iota
	inputEscaped
	inputCSI
	inputSS3
	// inputString is OSC, DCS, APC, PM, or SOS: a sequence with a payload,
	// which on the input stream is always the terminal answering.
	inputString
	// inputStringST is an escape inside one of those, which is the first
	// byte of the string terminator.
	inputStringST
)

type sequenceKind uint8

const (
	sequenceOngoing sequenceKind = iota
	// sequenceKey is a keystroke whose effect on the line cannot be seen from
	// here: a cursor key, a function key, the start of a paste.
	sequenceKey
	// sequenceReport is the terminal talking, which changes no line.
	sequenceReport
)

// maxSequence bounds a string sequence the terminal never terminated, so a
// stray introducer cannot swallow the typing that follows it.
const maxSequence = 512

func (s *inputSequence) start() { *s = inputSequence{phase: inputEscaped} }

func (s *inputSequence) active() bool { return s.phase != inputGround }

// feed takes the byte after the escape that started the sequence and reports
// what the sequence turned out to be, once it is complete.
func (s *inputSequence) feed(b byte) sequenceKind {
	if s.length++; s.length > maxSequence {
		s.phase = inputGround
		return sequenceKey
	}
	switch s.phase {
	case inputEscaped:
		switch b {
		case '[':
			s.phase = inputCSI
		case 'O':
			s.phase = inputSS3
		case ']', 'P', '_', '^', 'X':
			s.phase = inputString
		case esc:
			// Escape twice over: the sequence starts at the second one.
		default:
			// Escape and one character, which is a key with Alt held.
			s.phase = inputGround
			return sequenceKey
		}
	case inputSS3:
		s.phase = inputGround
		return sequenceKey
	case inputCSI:
		switch {
		case b == esc:
			s.phase = inputEscaped
		case b >= 0x3c && b <= 0x3f:
			if s.private == 0 {
				s.private = b
			}
		case b >= 0x20 && b <= 0x2f:
			s.marked = true
		case b >= 0x40 && b <= 0x7e:
			answer := s.answered(b)
			s.phase = inputGround
			if answer {
				return sequenceReport
			}
			return sequenceKey
		}
	case inputString:
		switch b {
		case bel:
			s.phase = inputGround
			return sequenceReport
		case esc:
			s.phase = inputStringST
		}
	case inputStringST:
		if b == esc {
			break
		}
		s.phase = inputGround
		if b == '\\' {
			return sequenceReport
		}
		// A payload the terminal ended with a bare escape. Whatever followed
		// is input in its own right, and counts as a key rather than being
		// taken for part of the answer.
		return sequenceKey
	}
	return sequenceOngoing
}

// answered reports whether a complete CSI is the terminal talking rather than
// a key. Anything not listed is a key, so an unfamiliar sequence still leaves
// the line looking written on.
func (s *inputSequence) answered(final byte) bool {
	if s.marked {
		return true
	}
	switch s.private {
	case '<':
		// SGR mouse: a click reported, not a line written.
		return final == 'M' || final == 'm'
	case '?':
		// The keyboard flags a program asked for. Keys sent under that
		// protocol also end in u, but carry no private byte.
		if final == 'u' {
			return true
		}
	}
	switch final {
	case 'c':
		// Device attributes, primary through tertiary.
		return true
	case 'R':
		// The cursor position something asked for.
		return true
	case 'n':
		// A status the terminal was asked to report.
		return true
	case 't':
		// The window's size or state.
		return true
	case 'I', 'O':
		// The window took or lost the focus.
		return true
	}
	return false
}

func decodeEscapeKey(input []byte) (keypress, []byte, bool) {
	if len(input) == 1 {
		return keypress{action: cancelQuery}, nil, true
	}
	if input[1] != '[' && input[1] != 'O' {
		return keypress{action: nothing}, input[2:], true
	}
	for i := 2; i < len(input); i++ {
		final := input[i]
		if final < 0x40 || final > 0x7e {
			continue
		}
		action := nothing
		switch final {
		case 'D':
			action = moveLeft
		case 'C':
			action = moveRight
		}
		return keypress{action: action}, input[i+1:], true
	}
	return keypress{}, input, false
}

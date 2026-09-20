//go:build darwin || linux

package term

import (
	"bytes"
	"unicode/utf8"
)

const osc133D = "133;D;"

type linePhase uint8

const (
	lineGround linePhase = iota
	lineEsc
	lineCSI
	lineOSC
	lineOSCEsc
	lineString
	lineStringEsc
	lineCharset
	lineUTF8
)

// linebuf recovers the shell prompt and echoed command, not command output.
// Its cell cap comes from CommandBytes; escape payloads are bounded separately.
type linebuf struct {
	maxCells int
	cells    []rune
	cursor   int
	phase    linePhase

	params  [maxParams]byte
	nparams int
	private byte
	invalid bool

	runeBytes [utf8.UTFMax]byte
	nrune     int

	exit    int
	hasExit bool
}

func (l *linebuf) text() string { return string(l.cells) }

func (l *linebuf) reset() {
	l.cells = l.cells[:0]
	l.cursor, l.phase = 0, lineGround
	l.clearSequence()
	l.nrune, l.exit, l.hasExit = 0, 0, false
}

func (l *linebuf) takeExit() (int, bool) {
	n, ok := l.exit, l.hasExit
	l.exit, l.hasExit = 0, false
	return n, ok
}

func (l *linebuf) consume(b byte) (string, bool) {
	switch l.phase {
	case lineGround:
		return l.ground(b)
	case lineEsc:
		l.escape(b)
	case lineCSI:
		l.csi(b)
	case lineOSC:
		l.stringByte(b, lineOSCEsc, true)
	case lineOSCEsc:
		l.stringEnd(b, lineOSC, true)
	case lineString:
		l.stringByte(b, lineStringEsc, false)
	case lineStringEsc:
		l.stringEnd(b, lineString, false)
	case lineCharset:
		l.phase = lineGround
	case lineUTF8:
		return l.utf8Byte(b)
	}
	return "", false
}

func (l *linebuf) ground(b byte) (string, bool) {
	switch {
	case b == esc:
		l.phase = lineEsc
	case b == '\n':
		return l.flush(), true
	case b == '\r':
		l.cursor = 0
	case b == '\b' || b == del:
		l.cursor = max(0, l.cursor-1)
	case b == '\t':
		for stop := min((l.cursor+8)/8*8, l.maxCells); l.cursor < stop; {
			l.put(' ')
		}
	case b < space:
	case b < utf8.RuneSelf:
		l.put(rune(b))
	default:
		l.runeBytes[0], l.nrune, l.phase = b, 1, lineUTF8
	}
	return "", false
}

func (l *linebuf) escape(b byte) {
	l.clearSequence()
	switch b {
	case '[':
		l.phase = lineCSI
	case ']':
		l.phase = lineOSC
	case 'P', '^', '_':
		l.phase = lineString
	case '(', ')', '*', '+', '-', '.', '/':
		l.phase = lineCharset
	case esc:
		l.phase = lineEsc
	default:
		l.phase = lineGround
	}
}

func (l *linebuf) csi(b byte) {
	switch {
	case b == esc:
		l.phase = lineEsc
	case b >= 0x3c && b <= 0x3f:
		if l.private != 0 || l.nparams != 0 {
			l.invalid = true
		} else {
			l.private = b
		}
	case b >= 0x30 && b <= 0x3b:
		if b == ';' || b >= '0' && b <= '9' {
			l.appendParam(b)
		} else {
			l.invalid = true
		}
	case b >= 0x20 && b <= 0x2f:
		l.invalid = true
	case b >= 0x40 && b <= 0x7e:
		if !l.invalid && l.private == 0 {
			l.applyCSI(b)
		}
		l.phase = lineGround
	}
}

func (l *linebuf) applyCSI(final byte) {
	switch final {
	case 'K':
		switch l.param(0, 0) {
		case 1:
			for i := 0; i < len(l.cells) && i <= l.cursor; i++ {
				l.cells[i] = ' '
			}
		case 2:
			l.cells = l.cells[:0]
		default:
			if l.cursor < len(l.cells) {
				l.cells = l.cells[:l.cursor]
			}
		}
	case 'C':
		l.cursor = min(l.cursor+l.param(0, 1), l.maxCells)
	case 'D':
		l.cursor = max(l.cursor-l.param(0, 1), 0)
	case 'G':
		l.cursor = min(max(l.param(0, 1)-1, 0), l.maxCells)
	case 'H', 'f':
		l.cursor = min(max(l.param(1, 1)-1, 0), l.maxCells)
	}
}

func (l *linebuf) param(want, fallback int) int {
	index, value, have := 0, 0, false
	for _, b := range l.params[:l.nparams] {
		if b == ';' {
			if index == want {
				return defaultParam(value, have, fallback)
			}
			index, value, have = index+1, 0, false
		} else {
			value, have = min(value*10+int(b-'0'), l.maxCells), true
		}
	}
	if index == want {
		return defaultParam(value, have, fallback)
	}
	return fallback
}

func defaultParam(value int, have bool, fallback int) int {
	if !have || value == 0 && fallback != 0 {
		return fallback
	}
	return value
}

func (l *linebuf) stringByte(b byte, escaped linePhase, osc bool) {
	switch b {
	case '\a':
		if osc {
			l.endOSC()
		}
		l.phase = lineGround
	case esc:
		l.phase = escaped
	default:
		if osc {
			l.appendParam(b)
		}
	}
}

func (l *linebuf) stringEnd(b byte, from linePhase, osc bool) {
	if b == '\\' {
		if osc {
			l.endOSC()
		}
		l.phase = lineGround
		return
	}
	l.phase = from
	l.escape(b)
}

func (l *linebuf) endOSC() {
	if !l.invalid {
		l.exit, l.hasExit = parseOSC133D(l.params[:l.nparams])
	}
	l.clearSequence()
}

func parseOSC133D(payload []byte) (int, bool) {
	if !bytes.HasPrefix(payload, []byte(osc133D)) {
		return 0, false
	}
	digits := payload[len(osc133D):]
	if len(digits) == 0 || len(digits) > 9 {
		return 0, false
	}
	n := 0
	for _, b := range digits {
		if b < '0' || b > '9' {
			return 0, false
		}
		n = n*10 + int(b-'0')
	}
	return n, true
}

func (l *linebuf) appendParam(b byte) {
	if l.nparams == len(l.params) {
		l.invalid = true
		return
	}
	l.params[l.nparams] = b
	l.nparams++
}

func (l *linebuf) clearSequence() {
	l.nparams, l.private, l.invalid = 0, 0, false
}

func (l *linebuf) utf8Byte(b byte) (string, bool) {
	if l.nrune == len(l.runeBytes) {
		l.nrune, l.phase = 0, lineGround
		return "", false
	}
	l.runeBytes[l.nrune], l.nrune = b, l.nrune+1
	if !utf8.FullRune(l.runeBytes[:l.nrune]) {
		return "", false
	}
	r, size := utf8.DecodeRune(l.runeBytes[:l.nrune])
	consumed := l.nrune
	l.nrune, l.phase = 0, lineGround
	if r == utf8.RuneError {
		if consumed > 1 {
			return l.ground(b)
		}
		return "", false
	}
	if size > 0 {
		l.put(r)
	}
	return "", false
}

func (l *linebuf) put(r rune) {
	if l.cursor >= l.maxCells {
		return
	}
	for len(l.cells) < l.cursor {
		l.cells = append(l.cells, ' ')
	}
	if l.cursor < len(l.cells) {
		l.cells[l.cursor] = r
	} else {
		l.cells = append(l.cells, r)
	}
	l.cursor++
}

func (l *linebuf) flush() string {
	text := string(l.cells)
	l.cells, l.cursor = l.cells[:0], 0
	return text
}

//go:build darwin || linux

package term

import (
	"bytes"
	"fmt"
)

const (
	esc       = 0x1b
	maxParams = 64
)

func (k eventKind) String() string {
	switch k {
	case promptReady:
		return "promptReady"
	case commandStart:
		return "commandStart"
	case altOn:
		return "altOn"
	case altOff:
		return "altOff"
	default:
		return fmt.Sprintf("eventKind(%d)", k)
	}
}

type csiPhase uint8

const (
	phaseGround csiPhase = iota
	phaseEscaped
	phaseCSI
)

type eventKind uint8

const (
	promptReady eventKind = iota
	commandStart
	altOn
	altOff
)

type modeEvent struct {
	at   int // byte offset immediately after the sequence in the current feed
	kind eventKind
}

// decModes is a bounded incremental parser for the two DEC private mode
// families that affect interception. It observes bytes only; relaying remains
// the caller's responsibility and is never delayed by an incomplete sequence.
type decModes struct {
	alt bool

	phase   csiPhase
	private byte
	params  [maxParams]byte
	n       int
	invalid bool
}

func (d *decModes) reset() { *d = decModes{} }

func (d *decModes) feed(p []byte) []modeEvent {
	var events []modeEvent
	for i := 0; i < len(p); i++ {
		b := p[i]
		switch d.phase {
		case phaseGround:
			at := bytes.IndexByte(p[i:], esc)
			if at < 0 {
				return events
			}
			i += at
			d.phase = phaseEscaped
		case phaseEscaped:
			if b == '[' {
				d.phase, d.private, d.n, d.invalid = phaseCSI, 0, 0, false
			} else if b != esc {
				d.phase = phaseGround
			}
		case phaseCSI:
			events = d.consume(b, i+1, events)
		}
	}
	return events
}

func (d *decModes) consume(b byte, at int, events []modeEvent) []modeEvent {
	switch {
	case b == esc:
		d.phase = phaseEscaped
	case b >= 0x3c && b <= 0x3f:
		if d.private != 0 || d.n != 0 {
			d.invalid = true
		} else {
			d.private = b
		}
	case b >= 0x30 && b <= 0x3b:
		if d.n == len(d.params) {
			d.invalid = true
		} else {
			d.params[d.n] = b
			d.n++
		}
	case b >= 0x20 && b <= 0x2f:
		d.invalid = true
	case b >= 0x40 && b <= 0x7e:
		d.phase = phaseGround
		return d.apply(b, at, events)
	}
	return events
}

func (d *decModes) apply(final byte, at int, events []modeEvent) []modeEvent {
	if d.invalid || d.private != '?' || (final != 'h' && final != 'l') {
		return events
	}
	on := final == 'h'
	for parameter := range bytes.SplitSeq(d.params[:d.n], []byte{';'}) {
		var kind eventKind
		switch string(parameter) {
		case "47", "1047", "1049":
			d.alt = on
			kind = altOff
			if on {
				kind = altOn
			}
		case "2004":
			kind = commandStart
			if on {
				kind = promptReady
			}
		default:
			continue
		}
		events = append(events, modeEvent{at: at, kind: kind})
	}
	return events
}

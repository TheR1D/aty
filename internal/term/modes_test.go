//go:build darwin || linux

package term

import (
	"bytes"
	"testing"
)

func TestDECModesReset(t *testing.T) {
	var modes decModes
	modes.feed([]byte("\x1b[?1049h\x1b[?2004"))
	modes.reset()
	if modes.alt || modes.phase != phaseGround || modes.n != 0 {
		t.Fatalf("reset left parser state behind: %+v", modes)
	}
	if events := modes.feed([]byte("h")); len(events) != 0 {
		t.Fatalf("suffix after reset produced events: %+v", events)
	}
}

func TestDECModesBoundMalformedSequenceAndRecover(t *testing.T) {
	var modes decModes
	overlong := append([]byte("\x1b[?"), bytes.Repeat([]byte{'1'}, maxParams+32)...)
	overlong = append(overlong, 'h')
	if events := modes.feed(overlong); len(events) != 0 || modes.alt {
		t.Fatalf("overlong sequence changed modes: events=%+v alt=%t", events, modes.alt)
	}
	events := modes.feed([]byte("\x1b[?1049h"))
	if len(events) != 1 || events[0].kind != altOn || !modes.alt {
		t.Fatalf("parser did not recover: events=%+v alt=%t", events, modes.alt)
	}
}

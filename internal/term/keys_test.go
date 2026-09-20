package term

import "testing"

func TestDecodeKeyEvents(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		action action
		char   rune
		rest   string
		whole  bool
	}{
		{name: "text", input: "éx", action: insertChar, char: 'é', rest: "x", whole: true},
		{name: "left", input: "\x1b[D?", action: moveLeft, rest: "?", whole: true},
		{name: "right", input: "\x1bOC", action: moveRight, whole: true},
		{name: "escape", input: "\x1b", action: cancelQuery, whole: true},
		{name: "enter", input: "\rnext", action: submitQuery, rest: "next", whole: true},
		{name: "backspace", input: "\x7f", action: eraseChar, whole: true},
		{name: "partial utf8", input: "\xc3", rest: "\xc3", whole: false},
		{name: "partial csi", input: "\x1b[", rest: "\x1b[", whole: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			press, rest, whole := decodeKey([]byte(test.input))
			if whole != test.whole || press.action != test.action || press.char != test.char || string(rest) != test.rest {
				t.Errorf("decodeKey(%q) = {%v %q}, %q, %v", test.input, press.action, press.char, rest, whole)
			}
		})
	}
}

// TestInputSequenceTellsAnAnswerFromAKey covers the one distinction the input
// parser exists to make. Both arrive as an escape and some bytes after it, and
// only one of them means the user changed the line the shell is holding.
func TestInputSequenceTellsAnAnswerFromAKey(t *testing.T) {
	reports := map[string]string{
		"background color, string terminator": "\x1b]11;rgb:1e1e/1e1e/1e1e\x1b\\",
		"foreground color, bell":              "\x1b]10;rgb:c0c0/c0c0/c0c0\a",
		"primary device attributes":           "\x1b[?62;4c",
		"secondary device attributes":         "\x1b[>1;95;0c",
		"terminal version":                    "\x1bP>|ghostty 1.0\x1b\\",
		"cursor position":                     "\x1b[24;80R",
		"device status":                       "\x1b[0n",
		"window size":                         "\x1b[8;40;120t",
		"mode setting":                        "\x1b[?2004;2$y",
		"keyboard flags":                      "\x1b[?1u",
		"mouse press":                         "\x1b[<0;10;5M",
		"focus gained":                        "\x1b[I",
		"focus lost":                          "\x1b[O",
	}
	for name, input := range reports {
		t.Run(name, func(t *testing.T) {
			if got := classify(input); got != sequenceReport {
				t.Errorf("%q read as %v, want the terminal answering", input, got)
			}
		})
	}

	keys := map[string]string{
		"up":              "\x1b[A",
		"application up":  "\x1bOA",
		"ctrl-right":      "\x1b[1;5C",
		"delete":          "\x1b[3~",
		"paste beginning": "\x1b[200~",
		"paste end":       "\x1b[201~",
		"shift-tab":       "\x1b[Z",
		"home":            "\x1b[H",
		"alt-b":           "\x1bb",
		"ctrl-a, kitty":   "\x1b[97;5u",
	}
	for name, input := range keys {
		t.Run(name, func(t *testing.T) {
			if got := classify(input); got != sequenceKey {
				t.Errorf("%q read as %v, want a key", input, got)
			}
		})
	}
}

// TestInputSequenceOutlastsAReadBoundary covers a sequence split between two
// reads, which is undecided until the byte that completes it arrives.
func TestInputSequenceOutlastsAReadBoundary(t *testing.T) {
	var seq inputSequence
	seq.start()
	for _, b := range []byte("]11;rgb:1e1e/") {
		if got := seq.feed(b); got != sequenceOngoing {
			t.Fatalf("the answer was decided at %q as %v, want to still be reading", b, got)
		}
	}
	for _, b := range []byte("1e1e/1e1e\x1b") {
		if got := seq.feed(b); got != sequenceOngoing {
			t.Fatalf("the answer was decided at %q as %v, want to still be reading", b, got)
		}
	}
	if got := seq.feed('\\'); got != sequenceReport {
		t.Errorf("the completed answer read as %v, want the terminal answering", got)
	}
	if seq.active() {
		t.Error("the parser is still reading a sequence that ended")
	}
}

// TestInputSequenceAbandonsAnUnterminatedPayload covers a string sequence with
// no terminator, which must not swallow the typing that follows it.
func TestInputSequenceAbandonsAnUnterminatedPayload(t *testing.T) {
	var seq inputSequence
	seq.start()
	seq.feed(']')
	for range maxSequence {
		if got := seq.feed('x'); got == sequenceKey {
			if seq.active() {
				t.Error("the abandoned sequence is still being read")
			}
			return
		}
	}
	t.Error("an unterminated payload is still being read past its bound")
}

// classify feeds one whole sequence, escape included, and reports what it was.
func classify(input string) sequenceKind {
	var seq inputSequence
	kind := sequenceOngoing
	for i, b := range []byte(input) {
		if i == 0 {
			seq.start()
			continue
		}
		if kind = seq.feed(b); kind != sequenceOngoing {
			break
		}
	}
	return kind
}

func (k sequenceKind) String() string {
	switch k {
	case sequenceKey:
		return "a key"
	case sequenceReport:
		return "the terminal answering"
	default:
		return "still reading"
	}
}

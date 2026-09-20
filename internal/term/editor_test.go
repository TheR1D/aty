//go:build darwin || linux

package term

import (
	"slices"
	"testing"
)

func TestQueryEditorUnicodeAndBoundaries(t *testing.T) {
	tests := []struct {
		name   string
		runs   [][]byte
		text   string
		cursor int
		end    action
	}{
		{name: "unicode split", runs: [][]byte{[]byte("caf"), {0xc3}, {0xa9}}, text: "café", cursor: 4},
		{name: "edit middle", runs: [][]byte{[]byte("ab界"), []byte("\x1b[D"), []byte("🙂")}, text: "ab🙂界", cursor: 3},
		{name: "left boundary", runs: [][]byte{[]byte("a"), []byte("\x1b[D\x1b[D")}, text: "a"},
		{name: "right boundary", runs: [][]byte{[]byte("a"), []byte("\x1b[C\x1b[C")}, text: "a", cursor: 1},
		{name: "submit leaves suffix", runs: [][]byte{[]byte("question\rcommand")}, text: "question", cursor: 8, end: submitQuery},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var editor queryEditor
			var ending action
			for _, run := range test.runs {
				_, ending = editor.feed(run)
				if ending != nothing {
					break
				}
			}
			if string(editor.text) != test.text || editor.cursor != test.cursor || ending != test.end {
				t.Fatalf("editor = text %q cursor %d ending %d; want %q, %d, %d",
					string(editor.text), editor.cursor, ending, test.text, test.cursor, test.end)
			}
		})
	}
}

func TestQueryEditorModes(t *testing.T) {
	tests := []struct {
		keys string
		mode queryMode
		text string
	}{
		{keys: "?", mode: queryMode{thinking: true}},
		{keys: "??", mode: queryMode{thinking: true, agent: true}},
		{keys: "???", mode: queryMode{thinking: true, agent: true}, text: "?"},
		{keys: "!", mode: queryMode{yolo: true}},
		{keys: "!x", mode: queryMode{yolo: true, noContext: true}},
		{keys: "x", mode: queryMode{noContext: true}},
		{keys: "#", text: "#"},
	}
	for _, test := range tests {
		var editor queryEditor
		for i := range len(test.keys) {
			if !editor.changeMode(test.keys[i]) {
				editor.feed([]byte{test.keys[i]})
			}
		}
		if editor.mode != test.mode || string(editor.text) != test.text {
			t.Errorf("keys %q produced mode %+v text %q; want %+v, %q",
				test.keys, editor.mode, string(editor.text), test.mode, test.text)
		}
	}
}

func TestEditorSnapshotIsImmutable(t *testing.T) {
	editor := queryEditor{
		text: []rune("first\nsecond"), cursor: 5,
		mode: queryMode{thinking: true, noContext: true},
	}
	snapshot := editor.snapshot()
	editor.text[0] = 'X'
	editor.mode.thinking = false
	editor.insert('!')

	if string(snapshot.text) != "first\nsecond" || snapshot.cursor != 5 ||
		snapshot.mode != (queryMode{thinking: true, noContext: true}) {
		t.Fatalf("snapshot changed with editor: %+v", snapshot)
	}
	if slices.Equal(snapshot.text, editor.text) {
		t.Fatal("snapshot aliases mutable editor text")
	}
}

func TestSubmittedEditorOnlyAcceptsCancellation(t *testing.T) {
	editor := queryEditor{text: []rune("question"), cursor: 8}
	editor.submit()
	if _, ending := editor.feed([]byte("ignored")); ending != nothing {
		t.Fatalf("typing ended submitted editor as %d", ending)
	}
	if string(editor.text) != "question" {
		t.Fatalf("submitted editor changed to %q", string(editor.text))
	}
	if _, ending := editor.feed([]byte{ctrlC}); ending != cancelQuery {
		t.Fatalf("interrupt ended submitted editor as %d", ending)
	}
	editor.finish()
	if !editor.finished() {
		t.Fatal("finished editor does not report finished")
	}
}

func TestConfirmationTransitions(t *testing.T) {
	editor := queryEditor{confirm: confirmLine}
	event, rest, whole := editor.nextConfirmation([]byte("\rnext"))
	if !whole || string(event.bytes) != "\r" || string(rest) != "next" || editor.confirm != confirmRun {
		t.Fatalf("Enter transition = event %+v rest %q phase %d", event, rest, editor.confirm)
	}

	editor.confirm = confirmLine
	event, _, whole = editor.nextConfirmation([]byte{ctrlC})
	if !whole || !event.cancel || !event.interrupt {
		t.Fatalf("Ctrl-C transition = %+v", event)
	}

	editor.confirm = confirmRun
	event, _, whole = editor.nextConfirmation([]byte{ctrlC})
	if !whole || event.cancel {
		t.Fatalf("Ctrl-C while running cancelled: %+v", event)
	}
}

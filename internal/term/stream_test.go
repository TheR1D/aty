//go:build darwin || linux

package term

import (
	"bytes"
	"errors"
	appconfig "github.com/TheR1D/aty/internal/config"
	"strings"
	"testing"
)

type streamWriter struct {
	bytes.Buffer
	writes int
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.Buffer.Write(p)
}

type streamForeground struct{ reads int }

func (f *streamForeground) Foreground() (int, error) {
	f.reads++
	return 42, nil
}

func TestInputKeepsAvailableTextTogether(t *testing.T) {
	for _, test := range []struct {
		name    string
		reads   []string
		confirm bool
		writes  int
	}{
		{"ordinary", []string{strings.Repeat("a", 1024)}, false, 1},
		{"question marks outside prompt", []string{strings.Repeat("?", 1024)}, false, 1},
		{"mixed text outside prompt", []string{strings.Repeat("abcdef?", 256)}, false, 2},
		{"confirmation text", []string{strings.Repeat("a", 1024)}, true, 1},
		{"confirmation unicode", []string{"café 世界"}, true, 1},
		{"split unicode", []string{"caf\xc3", "\xa9!"}, true, 3},
		{"split arrow", []string{"ab\x1b[", "Dcd"}, true, 3},
		{"enter then interrupt", []string{"ab\rcd\x03ef"}, true, 5},
		{"interrupt returns input", []string{"ab\x03cd"}, true, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newState(42, nil, appconfig.Limits{})
			foreground := &streamForeground{}
			s.process.pgrp = foreground
			out := &streamWriter{}
			k := &keyboard{state: s, master: out}
			if test.confirm {
				k.query = &query{editor: queryEditor{confirm: confirmLine}}
			}
			for _, read := range test.reads {
				if _, err := k.Write([]byte(read)); err != nil {
					t.Fatal(err)
				}
			}
			if got, want := out.String(), strings.Join(test.reads, ""); got != want {
				t.Fatalf("forwarded %q, want %q", got, want)
			}
			if out.writes != test.writes {
				t.Errorf("%d writes, want %d", out.writes, test.writes)
			}
			if foreground.reads != 0 {
				t.Errorf("%d unnecessary foreground checks", foreground.reads)
			}
		})
	}
}

func TestBatchedTextStopsBeforeLineCanBecomeFresh(t *testing.T) {
	s := atEmptyPrompt(t)
	view, _ := newQuery(t)
	out := &streamWriter{}
	k := &keyboard{state: s, master: out, screen: view.screen}
	// The first ? belongs to the shell; the second follows a backspace that
	// empties the line and must still be eligible to open a query.
	s.typed([]byte("a"))
	if _, err := k.Write([]byte("?\x7f\x7f?")); err != nil {
		t.Fatal(err)
	}
	if k.query == nil || out.String() != "?\x7f\x7f" {
		t.Fatal("batch swallowed the trigger after the edits restored an empty prompt")
	}
}

func TestSharedModeEventsAcrossOutputReads(t *testing.T) {
	stream := "$ " + pasteOn + "echo hi" + pasteOff + "\r\none\r\n" +
		"\x1b[?1049hhidden\x1b[?2004h\x1b[?1049l" +
		"two café\r\n\x1b]133;D;7\a$ " + pasteOn
	want := "$ echo hi\none\ntwo café\nexit code = 7"
	for split := 0; split <= len(stream); split++ {
		s := newState(0, nil, appconfig.Limits{})
		s.observeOutput([]byte(stream[:split]))
		if s.modes.alt != s.capture.alt {
			t.Fatalf("screen state disagrees at split %d", split)
		}
		s.observeOutput([]byte(stream[split:]))
		turns := s.transcript()
		if len(turns) != 1 || turns[0].Text != want {
			t.Fatalf("split %d: got %+v, want %q", split, turns, want)
		}
	}
}

func TestIncrementalInjectionPreservesRevisionsAndSafety(t *testing.T) {
	out := &streamWriter{}
	k := &keyboard{state: newState(0, nil, appconfig.Limits{}), master: out}
	q := &query{}
	var inject injection
	for _, command := range []string{
		"git", "git status", "git status", "git stash", "git",
		"printf café", "printf caf\xc3", "printf café",
		"echo \xff", "echo \xff!", "", "pwd",
	} {
		erase, insert := revision(inject.typed, []rune(command))
		out.Reset()
		if err := k.revise(q, &inject, command); err != nil {
			t.Fatal(err)
		}
		if got, want := out.String(), string(erase)+insert; got != want {
			t.Fatalf("revision to %q sent %q, want %q", command, got, want)
		}
		if got, want := string(inject.typed), string([]rune(command)); got != want {
			t.Fatalf("revision retained %q, want %q", got, want)
		}
	}
	for _, control := range []byte{0, '\r', '\t', ctrlC, esc, del, '\n'} {
		for _, command := range []string{"pwd" + string(control), string(control) + "pwd"} {
			out.Reset()
			if err := k.revise(q, &inject, command); !errors.Is(err, errUntypable) {
				t.Fatalf("unsafe revision %q returned %v", command, err)
			}
			if out.Len() != 0 || string(inject.typed) != "pwd" {
				t.Fatalf("unsafe revision %q changed the shell or cached command", command)
			}
		}
	}
	k.resetInjection(&inject)
	out.Reset()
	if err := k.revise(q, &inject, "pwd -P"); err != nil {
		t.Fatal(err)
	}
	if out.String() != "pwd -P" {
		t.Fatalf("reset reused a previous prefix: %q", out.String())
	}
}

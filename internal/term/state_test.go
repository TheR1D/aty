//go:build darwin || linux

package term

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	appconfig "github.com/TheR1D/aty/internal/config"
	"github.com/creack/pty"
)

func TestMain(m *testing.M) {
	if !relayName(filepath.Base(os.Args[0])) {
		os.Exit(m.Run())
	}
	busy := slices.Contains(os.Args[1:], "-aty-busy")
	if !busy {
		_, _ = os.Stdout.WriteString("\x1b[?2004hremote$ ")
		_ = os.Stdout.Sync()
	}
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

// TestPgrpFollowsForegroundJob is the portability check behind the interception
// gate. It drives a real shell through a real pty and asserts that the reading
// is exactly the terminal's foreground process group through every transition
// aty has to survive: idle prompt, a job taking the terminal, Ctrl-Z handing it
// back, the job taking it again, and Ctrl-C.
//
// The job's pid comes from the shell itself, via $!, so the expected value is
// never inferred from the source under test.
func TestPgrpFollowsForegroundJob(t *testing.T) {
	for _, sh := range installedShells() {
		t.Run(filepath.Base(sh.path), func(t *testing.T) {
			shell, ptmx, out := startShell(t, sh)
			shellPid := shell.Process.Pid

			waitForShellPrompt(t, out)
			pgrp, err := ProbePgrp(ptmx, shellPid)
			if err != nil {
				t.Fatalf("probing the foreground process group: %v", err)
			}
			if pgrp.UsesFallback() {
				t.Errorf("ioctl(ptmx, TIOCGPGRP) is unusable on %s/%s, so readings fell back to the process table",
					runtime.GOOS, runtime.GOARCH)
			}

			waitForPgrp(t, pgrp, shellPid, "at the prompt")
			assertSourcesAgree(t, pgrp, shellPid, "at the prompt")
			assertAtPrompt(t, pgrp, true, "at the prompt")

			writeLine(t, ptmx, "sleep 30 & echo CHILD=$!")
			childPid := waitForPid(t, out, regexp.MustCompile(`CHILD=(\d+)`))
			writeLine(t, ptmx, "fg")

			waitForPgrp(t, pgrp, childPid, "with a job in the foreground")
			assertSourcesAgree(t, pgrp, childPid, "with a job in the foreground")
			assertAtPrompt(t, pgrp, false, "with a job in the foreground")

			writeBytes(t, ptmx, "\x1a")
			waitForPgrp(t, pgrp, shellPid, "after suspending the job")
			assertAtPrompt(t, pgrp, true, "after suspending the job")

			writeLine(t, ptmx, "fg")
			waitForPgrp(t, pgrp, childPid, "after resuming the job")

			writeBytes(t, ptmx, "\x03")
			waitForPgrp(t, pgrp, shellPid, "after interrupting the job")
			assertAtPrompt(t, pgrp, true, "after interrupting the job")
		})
	}
}

// TestPgrpMissesForegroundJobWithoutJobControl pins down the one case the
// foreground process group cannot see. A shell with job control off never calls
// tcsetpgrp, so its children run in its own process group and the reading still
// names the shell while a child holds the terminal. This is why interception
// needs the alt-screen tracker as well: the programs that make a misread
// dangerous, vim and less, switch screen buffers whatever the shell does.
func TestPgrpMissesForegroundJobWithoutJobControl(t *testing.T) {
	for _, sh := range installedShells() {
		t.Run(filepath.Base(sh.path), func(t *testing.T) {
			shell, ptmx, out := startShell(t, sh)
			shellPid := shell.Process.Pid

			waitForShellPrompt(t, out)
			pgrp, err := ProbePgrp(ptmx, shellPid)
			if err != nil {
				t.Fatalf("probing the foreground process group: %v", err)
			}
			waitForPgrp(t, pgrp, shellPid, "at the prompt")

			writeLine(t, ptmx, "set +m")
			writeLine(t, ptmx, "sleep 30")

			// A shell blocked in a child never answers. The tty driver echoes the
			// request back either way, so only an answer with $$ expanded proves
			// the shell itself is reading.
			writeLine(t, ptmx, "echo ALIVE=$$")
			answered := regexp.MustCompile(`ALIVE=(\d+)`)
			for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
				if answered.MatchString(out.String()) {
					t.Fatalf("shell answered while a foreground child should have blocked it, output was:\n%s", out.String())
				}
				fg, err := pgrp.Foreground()
				if err != nil {
					t.Fatalf("reading the foreground process group: %v", err)
				}
				if fg != shellPid {
					t.Fatalf("foreground process group is %d, want the shell (%d): this shell puts jobs in their own process group even with job control off, so the blind spot this test documents no longer exists", fg, shellPid)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

// TestPgrpFallbackPath exercises the process-table reader on its own, since a
// machine where the master ioctl misbehaves would run entirely on it.
func TestPgrpFallbackPath(t *testing.T) {
	shell, ptmx, out := startShell(t, shellUnderTest{path: "/bin/sh", args: []string{"-i"}})
	shellPid := shell.Process.Pid

	pgrp := &Pgrp{ptmx: ptmx, shellPid: shellPid, fallback: true}
	waitForPgrp(t, pgrp, shellPid, "at the prompt")

	writeLine(t, ptmx, "sleep 30 & echo CHILD=$!")
	childPid := waitForPid(t, out, regexp.MustCompile(`CHILD=(\d+)`))
	writeLine(t, ptmx, "fg")

	waitForPgrp(t, pgrp, childPid, "with a job in the foreground")
}

func TestProbePgrpWithoutAnySource(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})

	// A pipe has no foreground process group, and pid 0 is not a process whose
	// controlling terminal can be inspected.
	if pgrp, err := ProbePgrp(reader, 0); err == nil {
		fg, _ := pgrp.Foreground()
		t.Errorf("ProbePgrp on a pipe succeeded and reports %d, want an error", fg)
	}
}

// TestGatesFollowARealShell drives the gates through the transitions a session
// makes, against a real shell writing real bytes. Each step waits for the one
// signal it is about and then checks that interception is shut, so a step that
// passes for the wrong reason still fails.
func TestGatesFollowARealShell(t *testing.T) {
	for _, sh := range installedShells() {
		t.Run(filepath.Base(sh.path), func(t *testing.T) {
			if !lineEditorAnnounces(sh) {
				t.Skip("this shell does not announce its line editor")
			}
			w := watchShell(t, sh)

			awaitGate(t, w, gate.open, "at a fresh prompt")

			// Typing is echoed back, and the echo must not make the line it
			// echoed look untouched: the shell is still holding what was typed.
			w.press(t, "ec")
			awaitOutput(t, w, "ec")
			typed := w.state.gate()
			if typed.freshLine || typed.open() {
				t.Errorf("a half-typed command that was echoed back leaves the line looking fresh: %+v", typed)
			}

			// The interrupt character throws the line away, and the prompt the
			// shell prints for it is where a fresh line begins again.
			w.press(t, "\x03")
			awaitGate(t, w, gate.open, "at the prompt after the line was interrupted")

			w.press(t, "sleep 30\n")
			busy := awaitGate(t, w, func(g gate) bool { return !g.atPrompt }, "with a job in the foreground")
			if busy.open() {
				t.Errorf("interception is open while a job owns the terminal: %+v", busy)
			}

			w.press(t, "\x03")
			awaitGate(t, w, gate.open, "at the prompt after the job was interrupted")

			// The screen is the gate that holds when the foreground process
			// group cannot: this is the switch vim and less make, without vim
			// or less having to be installed.
			w.press(t, `printf '\033[?1049h'`+"\n")
			hidden := awaitGate(t, w, func(g gate) bool { return g.altScreen }, "on the alternate screen")
			if hidden.open() {
				t.Errorf("interception is open on the alternate screen: %+v", hidden)
			}
			if !hidden.atPrompt {
				t.Errorf("the shell that switched screens is not the one owning the terminal: %+v", hidden)
			}

			w.press(t, `printf '\033[?1049l'`+"\n")
			awaitGate(t, w, gate.open, "back on the normal screen")
		})
	}
}

func TestGateOpensOverSshAtARemotePrompt(t *testing.T) {
	w := watchShell(t, interactiveShell(t))
	awaitGate(t, w, gate.open, "at a fresh prompt")

	w.press(t, installRelay(t)+"\n")
	remote := awaitGate(t, w, func(g gate) bool { return g.relay && g.open() }, "over ssh at a remote prompt")
	if remote.atPrompt {
		t.Errorf("ssh still looks like the local shell: %+v", remote)
	}
	if remote.altScreen {
		t.Errorf("a remote prompt was taken for the alternate screen: %+v", remote)
	}
}

func TestGateStaysShutForSshWithoutALineEditor(t *testing.T) {
	w := watchShell(t, interactiveShell(t))
	awaitGate(t, w, gate.open, "at a fresh prompt")

	w.press(t, installRelay(t)+" -aty-busy\n")
	busy := awaitGate(t, w, func(g gate) bool { return !g.atPrompt }, "ssh without a remote prompt")
	if busy.open() || busy.relay {
		t.Errorf("ssh with no line editor opened interception: %+v", busy)
	}
}

func TestGateStaysShutWhenALocalJobSendsLineEditorModes(t *testing.T) {
	w := watchShell(t, interactiveShell(t))
	awaitGate(t, w, gate.open, "at a fresh prompt")

	w.press(t, `printf '\033[?2004h'; sleep 30`+"\n")
	busy := awaitGate(t, w, func(g gate) bool {
		return !g.atPrompt && g.promptReady
	}, "a local job that turned on a line editor")
	if busy.open() || busy.relay {
		t.Errorf("line editor modes from a local job opened interception: %+v", busy)
	}
}

// TestGateOpensAtThePromptAProgramLeftBehind runs the same thing against a
// real shell: a program is ended with ctrl-D rather than with Enter, and the
// prompt it leaves behind is one a question can be asked at.
func TestGateOpensAtThePromptAProgramLeftBehind(t *testing.T) {
	w := watchShell(t, interactiveShell(t))
	awaitGate(t, w, gate.open, "at a fresh prompt")

	w.press(t, "cat\n")
	awaitGate(t, w, func(g gate) bool { return !g.atPrompt }, "with cat reading the keyboard")

	w.press(t, "\x04")
	awaitGate(t, w, gate.open, "at the prompt cat left behind")
}

func TestFreshLineTracksWhatTheShellWasSent(t *testing.T) {
	s := newState(os.Getpid(), nil, appconfig.Limits{})
	assertFresh(t, s, true, "before anything is typed")

	// The terminal echoes typing, so the shell's own output arrives a
	// keystroke behind the user. An echo must not make the line it echoed look
	// untouched.
	s.typed([]byte("ls"))
	assertFresh(t, s, false, "with half a command typed")
	s.observeOutput([]byte("ls"))
	assertFresh(t, s, false, "with that typing echoed back")

	s.typed([]byte("\r"))
	assertFresh(t, s, false, "between Enter and the prompt that follows it")
	s.observeOutput([]byte("\r\ndocs  go.mod\r\n$ "))
	assertFresh(t, s, true, "at the prompt after the command ran")

	s.typed([]byte("rm -rf /tm\x03"))
	assertFresh(t, s, false, "just after a line was interrupted")
	s.observeOutput([]byte("^C\r\n$ "))
	assertFresh(t, s, true, "at the prompt the interrupt produced")

	// A keystroke can outrun the echo of the one before it, so what arrives
	// after the key that ended a line can still be the echo of what was typed
	// before it, and the shell is not at a new prompt yet.
	s.typed([]byte("ec"))
	s.typed([]byte("\x03"))
	s.observeOutput([]byte("ec"))
	assertFresh(t, s, false, "with the echo of the typing arriving after the interrupt")
	s.observeOutput([]byte("\r\n$ "))
	assertFresh(t, s, true, "at the prompt that followed that interrupt")

	// Typing ahead of a prompt puts text in the line that prompt will read, so
	// the prompt arriving does not make that line an empty one.
	s.typed([]byte("\r"))
	s.typed([]byte("x"))
	s.observeOutput([]byte("\r\n$ x"))
	assertFresh(t, s, false, "at a prompt that was typed at before it appeared")

	// A trigger the shell received is typing like any other.
	s.typed([]byte("\r"))
	s.observeOutput([]byte("\r\n$ "))
	s.typed([]byte("?"))
	assertFresh(t, s, false, "after a trigger was passed through to the shell")

	s.typed([]byte("\r"))
	s.observeOutput([]byte("\r\n$ "))
	s.typed([]byte("\x7f\x08"))
	assertFresh(t, s, true, "after a backspace at an empty prompt")
	s.typed([]byte("ls\x7f"))
	assertFresh(t, s, false, "after a backspace on a line that already had typing")
}

// TestFreshLineIgnoresWhatTheTerminalSays covers the bytes on the input stream
// that are not the user: a program the answer ran asks the terminal for its
// colors or its identity and reads the reply as input, and a terminal says on
// its own account that its window took the focus. None of that touches the
// line the shell's editor is holding, and counting it as typing is what used
// to leave an empty prompt looking written on until the next Enter.
func TestFreshLineIgnoresWhatTheTerminalSays(t *testing.T) {
	s := newState(os.Getpid(), nil, appconfig.Limits{})

	s.typed([]byte("\x1b]11;rgb:1e1e/1e1e/1e1e\x1b\\"))
	assertFresh(t, s, true, "after the terminal reported its background color")

	s.typed([]byte("\x1b[?62;4c"))
	assertFresh(t, s, true, "after the terminal reported its device attributes")

	s.typed([]byte("\x1b[I"))
	assertFresh(t, s, true, "after the window took the focus")

	// The answer is a payload of printable text, and none of it is a
	// character the line editor is holding.
	s.typed([]byte("\x7f"))
	assertFresh(t, s, true, "after a backspace following those answers")

	// A report is undecided until it is complete, so a read that ends in the
	// middle of one leaves the line dirty rather than falsely fresh.
	s.typed([]byte("\x1b]11;rgb:1e1e"))
	assertFresh(t, s, false, "with only half of an answer read")
	s.typed([]byte("/1e1e/1e1e\x1b\\"))
	assertFresh(t, s, true, "once the rest of that answer arrived")

	// A command running is what usually asks, so the answer lands between
	// Enter and the prompt that follows it. That must not stop the prompt from
	// reading as a fresh line.
	s.typed([]byte("gh repo list"))
	s.typed([]byte("\r"))
	s.typed([]byte("\x1b[?62;4c"))
	s.observeOutput([]byte("\r\nThere are no repositories\r\n$ \x1b[?2004h"))
	assertFresh(t, s, true, "at the prompt after a command whose program asked the terminal something")

	// A key is still a key: what an arrow did to the line cannot be seen from
	// here, so the line stays dirty.
	s.typed([]byte("\x1b[A"))
	assertFresh(t, s, false, "after an arrow key recalled history")
}

func TestStateReportsClearAfterDroppingTheTranscript(t *testing.T) {
	s := newState(os.Getpid(), nil, appconfig.Limits{})
	cleared := 0
	s.onClear = func() { cleared++ }

	s.observeOutput([]byte("$ \x1b[?2004h"))
	s.observeOutput([]byte("echo hi\x1b[?2004l\nhi\n$ \x1b[?2004h"))
	if cleared != 0 {
		t.Fatalf("an ordinary command called the clear hook %d times", cleared)
	}

	s.observeOutput([]byte("clear\x1b[?2004l\n$ \x1b[?2004h"))
	if cleared != 1 {
		t.Errorf("clear hook calls = %d, want 1", cleared)
	}
}

// TestFreshLineFollowsBackspacesToAnEmptyLine covers the line that is erased
// back to nothing: a question asked, answered and cleared away, or simply a
// command rethought, leaves the line editor holding exactly what the shell
// gave it, so the trigger that follows is aty's again.
func TestFreshLineFollowsBackspacesToAnEmptyLine(t *testing.T) {
	s := newState(os.Getpid(), nil, appconfig.Limits{})

	s.typed([]byte("git status"))
	assertFresh(t, s, false, "with a command half typed")

	s.typed([]byte("\x7f\x7f\x7f\x7f\x7f\x7f\x7f\x7f\x7f\x7f"))
	assertFresh(t, s, true, "after the line was backspaced away")

	// A held-down backspace keeps arriving after the line is already empty.
	s.typed([]byte("\x7f\x7f"))
	assertFresh(t, s, true, "after backspacing an empty line")

	// A multi-byte character is one character to the line editor, typed as
	// several bytes and erased with one backspace.
	s.typed([]byte("echo caf\xc3\xa9"))
	s.typed([]byte("\x7f\x7f\x7f\x7f\x7f\x7f\x7f\x7f\x7f"))
	assertFresh(t, s, true, "after a line ending in an accented character was erased")

	// What completion puts in the line is not visible here, so a completed
	// line stays dirty however many erases follow, until the shell is handed
	// the line and prints the next prompt.
	s.typed([]byte("ec\t"))
	assertFresh(t, s, false, "with a completion in flight")
	s.typed([]byte("\x7f\x7f\x7f\x7f\x7f\x7f"))
	assertFresh(t, s, false, "even after the completion is erased")

	s.typed([]byte("\x03"))
	s.observeOutput([]byte("^C\r\n$ "))
	assertFresh(t, s, true, "at the prompt after the interrupt")

	// An arrow key can recall history, whose contents cannot be counted.
	s.typed([]byte("\x1b[A"))
	s.typed([]byte("\x7f"))
	assertFresh(t, s, false, "after history recall and an erase")
}

// TestFreshLineFollowsAKeyThatEndedAProgram covers the keystroke a line editor
// never saw: the EOF that closes a program, the key that suspends it, a key
// only that program understood. None of them is an Enter, so the prompt the
// program leaves behind is printed with no line ending in front of it, and the
// announcement is the only thing that says that line is an empty one.
func TestFreshLineFollowsAKeyThatEndedAProgram(t *testing.T) {
	s := newState(os.Getpid(), nil, appconfig.Limits{})
	s.observeOutput([]byte("$ \x1b[?2004h"))

	// The program is what the keyboard is reaching now, which the line editor
	// going quiet is what shuts the gate on; the line it left is an empty one.
	s.typed([]byte("cat\r"))
	s.observeOutput([]byte("\x1b[?2004l\r\n"))

	s.typed([]byte("\x04"))
	assertFresh(t, s, false, "just after the key that closed the program")
	s.observeOutput([]byte("$ \x1b[?2004h"))
	assertFresh(t, s, true, "at the prompt the program left behind")

	// A suspend is the same key to aty: one whose effect on a line cannot be
	// seen, pressed at something that was not a line.
	s.typed([]byte("less notes\r"))
	s.observeOutput([]byte("\x1b[?2004l\r\n"))
	s.typed([]byte("\x1a"))
	s.observeOutput([]byte("\r\nzsh: suspended  less notes\r\n$ \x1b[?2004h"))
	assertFresh(t, s, true, "at the prompt after a suspend")
}

// TestFreshLineFollowsAProgramLeavingTheAlternateScreen covers the other key a
// line editor never saw, and the only one that is a plain character: a pager's
// q, a vim ZZ. It is typing anywhere else, so what makes it not typing is the
// program it was typed at giving the screen back.
func TestFreshLineFollowsAProgramLeavingTheAlternateScreen(t *testing.T) {
	s := newState(os.Getpid(), nil, appconfig.Limits{})
	s.observeOutput([]byte("$ \x1b[?2004h"))

	s.typed([]byte("less notes\r"))
	s.observeOutput([]byte("\x1b[?2004l\r\n\x1b[?1049hnotes\r\n"))
	s.typed([]byte("q"))
	assertFresh(t, s, false, "with a character typed at the program on the screen")

	s.observeOutput([]byte("\x1b[?1049l$ \x1b[?2004h"))
	assertFresh(t, s, true, "at the prompt the pager left behind")
}

// TestFreshLineKeepsTypeAheadAcrossAPrompt is what the announcement does not
// excuse: keys typed before it arrived have not been read yet, so they are in
// the line the editor is about to read, and an answer typed there would be
// answered onto the end of them.
func TestFreshLineKeepsTypeAheadAcrossAPrompt(t *testing.T) {
	s := newState(os.Getpid(), nil, appconfig.Limits{})
	s.observeOutput([]byte("$ \x1b[?2004h"))

	s.typed([]byte("ls\r"))
	s.observeOutput([]byte("\x1b[?2004l\r\ndocs  go.mod\r\n"))
	s.typed([]byte("x"))
	s.observeOutput([]byte("$ \x1b[?2004hx"))
	assertFresh(t, s, false, "at a prompt that was typed at before it appeared")

	s.typed([]byte("\x7f"))
	assertFresh(t, s, true, "once that typing was erased")
}

func TestGatePromptReadyFollowsBracketedPaste(t *testing.T) {
	s := newState(os.Getpid(), nil, appconfig.Limits{})
	if g := s.gate(); g.promptReady {
		t.Errorf("a session that has not seen a line editor is already prompt-ready: %+v", g)
	}

	s.observeOutput([]byte("\x1b[?2004h"))
	if g := s.gate(); !g.promptReady {
		t.Errorf("CSI ? 2004 h did not open the prompt-ready signal: %+v", g)
	}

	s.observeOutput([]byte("\x1b[?2004l"))
	if g := s.gate(); g.promptReady {
		t.Errorf("CSI ? 2004 l left the prompt-ready signal set: %+v", g)
	}
}

// TestObserveOutputTintsAtAPrompt is when the orange cursor is asked for: a
// line editor announcing itself off the alternate screen, not vim doing the
// same thing on it, and not the line being handed to the shell.
func TestObserveOutputTintsAtAPrompt(t *testing.T) {
	s := newState(os.Getpid(), nil, appconfig.Limits{})

	if s.observeOutput([]byte("docs  go.mod\r\n$ ")) {
		t.Error("ordinary output asked for the cursor to be tinted")
	}
	if !s.observeOutput([]byte("\x1b[?2004h")) {
		t.Error("CSI ? 2004 h did not ask for the cursor to be tinted")
	}
	if s.observeOutput([]byte("\x1b[?2004l")) {
		t.Error("CSI ? 2004 l asked for the cursor to be tinted")
	}

	s = newState(os.Getpid(), nil, appconfig.Limits{})
	if s.observeOutput([]byte("\x1b[?1049h\x1b[?2004h")) {
		t.Error("a prompt on the alternate screen asked for the cursor to be tinted")
	}
	if s.observeOutput([]byte("\x1b[?2004h")) {
		t.Error("another prompt still on the alternate screen asked for the cursor to be tinted")
	}
	if !s.observeOutput([]byte("\x1b[?1049l\x1b[?2004h")) {
		t.Error("a prompt after leaving the alternate screen did not ask for the cursor to be tinted")
	}
}

// TestGateIsShutWithoutAForegroundReading covers the session where the reading
// interception is gated on could not be validated at all. aty carries on as a
// pty wrapper, which means never taking a keystroke the shell was sent.
func TestGateIsShutWithoutAForegroundReading(t *testing.T) {
	s := newState(os.Getpid(), nil, appconfig.Limits{})
	s.observeOutput([]byte("\x1b[?2004h"))

	g := s.gate()
	if g.open() {
		t.Errorf("interception is open with nothing saying who owns the terminal: %+v", g)
	}
	if g.err == nil {
		t.Error("a gate with no foreground reading does not say why")
	}
}

func TestInterceptionNeedsEverySignal(t *testing.T) {
	allowed := gate{atPrompt: true, promptReady: true, freshLine: true}
	if !allowed.open() {
		t.Fatalf("no signal refuses and interception is shut anyway: %+v", allowed)
	}
	relayed := gate{relay: true, promptReady: true, freshLine: true}
	if !relayed.open() {
		t.Fatalf("a relay at a remote line editor is shut: %+v", relayed)
	}

	for when, refused := range map[string]gate{
		"a job owns the terminal":              {atPrompt: false, promptReady: true, freshLine: true},
		"a full-screen program has the screen": {atPrompt: true, altScreen: true, promptReady: true, freshLine: true},
		"the user has typed on the line":       {atPrompt: true, promptReady: true, freshLine: false},
		"the line editor has not announced":    {atPrompt: true, freshLine: true},
		"a relay on the alternate screen":      {relay: true, altScreen: true, promptReady: true, freshLine: true},
		"a relay with a dirty line":            {relay: true, promptReady: true, freshLine: false},
		"a relay without a line editor":        {relay: true, freshLine: true},
	} {
		if refused.open() {
			t.Errorf("%s and interception is open anyway: %+v", when, refused)
		}
	}
}

func TestAltScreenFollowsScreenSwitches(t *testing.T) {
	tests := []struct {
		name   string
		output []string
		want   bool
	}{
		{name: "the switch vim makes", output: []string{"\x1b[?1049h"}, want: true},
		{name: "and the one it makes on the way out", output: []string{"\x1b[?1049h", "\x1b[?1049l"}, want: false},
		{name: "the older spelling", output: []string{"\x1b[?47h"}, want: true},
		{name: "the spelling in between", output: []string{"\x1b[?1047h"}, want: true},
		{name: "left by one spelling, returned by another", output: []string{"\x1b[?1049h", "\x1b[?47l"}, want: false},
		{name: "switched along with other modes", output: []string{"\x1b[?1049;2004h"}, want: true},
		{name: "bracketed paste is not a screen", output: []string{"\x1b[?2004h"}, want: false},
		{name: "nor is the same number without the private marker", output: []string{"\x1b[1049h"}, want: false},
		{name: "nor the text of the sequence rather than the sequence", output: []string{`\033[?1049h`}, want: false},
		{name: "buried in a burst of ordinary output", output: []string{"\x1b[32mgreen\x1b[0m\r\n\x1b[?1049h\x1b[2J$ "}, want: true},
		{name: "an escape abandons the sequence it interrupts", output: []string{"\x1b[?1049\x1b[?2004h"}, want: false},
		{name: "parameters that never end are not waited on forever",
			output: []string{"\x1b[?" + strings.Repeat("1;", maxParams) + "1049h"}, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var whole decModes
			for _, piece := range test.output {
				whole.feed([]byte(piece))
			}
			if whole.alt != test.want {
				t.Errorf("the alternate screen reads as %t, want %t", whole.alt, test.want)
			}

			// A copy loop hands over whatever a read returned, so the same
			// sequence can arrive one byte at a time.
			var split decModes
			for _, piece := range test.output {
				for i := range len(piece) {
					split.feed([]byte{piece[i]})
				}
			}
			if split.alt != test.want {
				t.Errorf("read a byte at a time, the alternate screen reads as %t, want %t", split.alt, test.want)
			}
		})
	}
}

func TestAltScreenReportsOnlyRealSwitches(t *testing.T) {
	var a decModes

	a.feed([]byte("$ ls\r\n"))
	if a.alt {
		t.Error("ordinary output was reported as a screen switch")
	}
	a.feed([]byte("\x1b[?1049h"))
	if !a.alt {
		t.Error("taking the alternate screen was not recorded")
	}
	a.feed([]byte("\x1b[?1049h"))
	if !a.alt {
		t.Error("taking a screen already in use cleared it")
	}
	a.feed([]byte("\x1b[?1049l"))
	if a.alt {
		t.Error("giving the screen back was not recorded")
	}
}

func TestDecModesEmitsPositionedEvents(t *testing.T) {
	tests := []struct {
		name   string
		output []string
		want   []eventKind
		alt    bool
	}{
		{name: "bash bracketed paste", output: []string{"\x1b[?2004h"}, want: []eventKind{promptReady}},
		{name: "readline leaving the prompt", output: []string{"\x1b[?2004h", "\x1b[?2004l"}, want: []eventKind{promptReady, commandStart}},
		{name: "zsh smkx is ignored", output: []string{"\x1b[?1h"}},
		{name: "smkx mixed into a paste sequence is ignored", output: []string{"\x1b[?1;2004h"}, want: []eventKind{promptReady}},
		{name: "the switch vim makes", output: []string{"\x1b[?1049h"}, want: []eventKind{altOn}, alt: true},
		{name: "and the one it makes on the way out", output: []string{"\x1b[?1049h", "\x1b[?1049l"}, want: []eventKind{altOn, altOff}},
		{name: "switched along with paste", output: []string{"\x1b[?1049;2004h"}, want: []eventKind{altOn, promptReady}, alt: true},
		{name: "the same number without the private marker", output: []string{"\x1b[2004h"}},
		{name: "buried in a burst of ordinary output", output: []string{"\x1b[32mgreen\x1b[0m\r\n\x1b[?2004h$ "}, want: []eventKind{promptReady}},
		{name: "an escape abandons the sequence it interrupts", output: []string{"\x1b[?1\x1b[?2004h"}, want: []eventKind{promptReady}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var whole decModes
			var events []modeEvent
			for _, piece := range test.output {
				events = append(events, whole.feed([]byte(piece))...)
			}
			assertModeEvents(t, events, test.want)
			if whole.alt != test.alt {
				t.Errorf("the alternate screen reads as %t, want %t", whole.alt, test.alt)
			}

			var split decModes
			var splitEvents []modeEvent
			for _, piece := range test.output {
				for i := range len(piece) {
					splitEvents = append(splitEvents, split.feed([]byte{piece[i]})...)
				}
			}
			assertModeEvents(t, splitEvents, test.want)
			if split.alt != test.alt {
				t.Errorf("read a byte at a time, the alternate screen reads as %t, want %t", split.alt, test.alt)
			}
		})
	}
}

func TestDecModesEventOffsetSplitsTheBuffer(t *testing.T) {
	const seq = "\x1b[?2004l"
	p := []byte("echo hi" + seq + "hi\n")

	var d decModes
	events := d.feed(p)
	if len(events) != 1 || events[0].kind != commandStart {
		t.Fatalf("events=%v, want one commandStart", events)
	}
	at := events[0].at
	if string(p[:at]) != "echo hi"+seq {
		t.Errorf("bytes before commandStart: %q", p[:at])
	}
	if string(p[at:]) != "hi\n" {
		t.Errorf("bytes after commandStart: %q", p[at:])
	}
}

func assertModeEvents(t *testing.T, events []modeEvent, want []eventKind) {
	t.Helper()
	if len(events) != len(want) {
		t.Errorf("events=%v, want %v", events, want)
		return
	}
	for i, ev := range events {
		if ev.kind != want[i] {
			t.Errorf("events=%v, want %v", events, want)
			return
		}
	}
}

func TestRelayArgvRecognizesRelays(t *testing.T) {
	for _, args := range [][]string{
		{"ssh"},
		{"/usr/bin/ssh"},
		{"/opt/homebrew/bin/ssh", "host"},
		{"ssh", "-t", "host"},
		{"mosh"},
		{"/usr/bin/mosh", "host"},
		{"docker"},
		{"/usr/bin/docker", "exec", "-it", "box", "bash"},
		{"podman"},
		{"kubectl"},
		{"/usr/local/bin/kubectl", "exec", "-it", "pod", "--", "bash"},
	} {
		if !relayArgv(args) {
			t.Errorf("relayArgv(%q) is false, want a relay recognised", args)
		}
	}
	for _, args := range [][]string{
		nil,
		{},
		{"sshd"},
		{"scp"},
		{"autossh"},
		{"mosh-server"},
		{"dockerd"},
		{"docker-compose"},
		{"python"},
		{"/bin/sleep"},
	} {
		if relayArgv(args) {
			t.Errorf("relayArgv(%q) is true, want it refused", args)
		}
	}
	if relayProcess(os.Getpid()) {
		t.Error("this test process was taken for a relay")
	}
}

type shellUnderTest struct {
	path string
	args []string
}

// installedShells lists the shells to run the gate against, skipping the ones
// this machine does not have. The arguments keep each shell interactive, which
// is what turns job control on, while ignoring the user's own startup files.
func installedShells() []shellUnderTest {
	candidates := []shellUnderTest{
		{path: "/bin/sh", args: []string{"-i"}},
		{path: "/bin/bash", args: []string{"--norc", "--noprofile", "-i"}},
		{path: "/bin/zsh", args: []string{"-f", "-i"}},
	}
	installed := make([]shellUnderTest, 0, len(candidates))
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate.path); err == nil {
			installed = append(installed, candidate)
		}
	}
	return installed
}

// waitForShellPrompt waits until the shell has printed something. ProbePgrp
// takes one reading and assumes the shell already owns the terminal, which a
// prompt is enough to prove; the session waits for promptReady instead.
func waitForShellPrompt(t *testing.T, out *ptyOutput) {
	t.Helper()

	for deadline := time.Now().Add(answerTimeout); ; {
		if out.String() != "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the shell never printed a prompt")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func startShell(t *testing.T, sh shellUnderTest) (*exec.Cmd, *os.File, *ptyOutput) {
	t.Helper()

	shell := exec.Command(sh.path, sh.args...)
	shell.Env = testEnv(t)
	ptmx, err := pty.StartWithSize(shell, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("starting %s in a pty: %v", sh.path, err)
	}

	out := &ptyOutput{}
	go func() {
		_, _ = io.Copy(out, ptmx)
	}()

	t.Cleanup(func() {
		// Closing the master hangs up the session, which takes the foreground
		// job with it.
		_ = ptmx.Close()
		_ = shell.Process.Kill()
		_ = shell.Wait()
	})
	return shell, ptmx, out
}

func writeLine(t *testing.T, ptmx *os.File, line string) {
	t.Helper()
	writeBytes(t, ptmx, line+"\n")
}

func writeBytes(t *testing.T, ptmx *os.File, keys string) {
	t.Helper()
	if _, err := ptmx.WriteString(keys); err != nil {
		t.Fatalf("writing %q to the pty: %v", keys, err)
	}
}

// waitForPgrp waits for the foreground process group to become want, reporting
// what it saw instead if it never does.
func waitForPgrp(t *testing.T, pgrp *Pgrp, want int, when string) {
	t.Helper()

	var (
		got int
		err error
	)
	for deadline := time.Now().Add(5 * time.Second); ; {
		if got, err = pgrp.Foreground(); err == nil && got == want {
			return
		}
		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("%s: reading the foreground process group: %v", when, err)
			}
			t.Fatalf("%s: foreground process group is %d, want %d", when, got, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// assertAtPrompt checks the question interception is gated on: whether the
// shell itself owns the pty, meaning no child is running and the keyboard is
// reaching the shell's own line editor.
func assertAtPrompt(t *testing.T, pgrp *Pgrp, want bool, when string) {
	t.Helper()

	fg, err := pgrp.Foreground()
	if err != nil {
		t.Errorf("%s: reading the foreground process group: %v", when, err)
		return
	}
	if atPrompt := fg == pgrp.shellPid; atPrompt != want {
		t.Errorf("%s: the shell owning the pty is %t, want %t", when, atPrompt, want)
	}
}

// assertSourcesAgree checks both readers against the same expected value, so a
// regression in either one is visible instead of hidden by the other.
func assertSourcesAgree(t *testing.T, pgrp *Pgrp, want int, when string) {
	t.Helper()

	if byIoctl, err := pgrp.foregroundIoctl(); err != nil {
		t.Errorf("%s: ioctl: %v", when, err)
	} else if byIoctl != want {
		t.Errorf("%s: ioctl(ptmx, TIOCGPGRP) reports %d, want %d", when, byIoctl, want)
	}

	if byProc, err := tpgid(pgrp.shellPid); err != nil {
		t.Errorf("%s: process table: %v", when, err)
	} else if byProc != want {
		t.Errorf("%s: process table reports tpgid %d, want %d", when, byProc, want)
	}
}

func interactiveShell(t *testing.T) shellUnderTest {
	t.Helper()
	var fallback shellUnderTest
	for _, sh := range installedShells() {
		if !lineEditorAnnounces(sh) {
			continue
		}
		if filepath.Base(sh.path) == "zsh" {
			return sh
		}
		if fallback.path == "" {
			fallback = sh
		}
	}
	if fallback.path != "" {
		return fallback
	}
	t.Skip("needs a shell that announces its line editor")
	return shellUnderTest{}
}

// lineEditorAnnounces reports whether sh is expected to emit CSI ? 2004 at a
// prompt, which is what the gates need. plain sh and bash before 5.1 never do.
func lineEditorAnnounces(sh shellUnderTest) bool {
	switch filepath.Base(sh.path) {
	case "zsh", "fish":
		return true
	case "bash":
		out, err := exec.Command(sh.path, "-c", `printf %s "${BASH_VERSINFO[0]}"`).Output()
		if err != nil {
			return false
		}
		major, err := strconv.Atoi(string(out))
		return err == nil && major >= 5
	default:
		return false
	}
}

func installRelay(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("this test binary: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ssh")
	if err := os.Symlink(exe, path); err != nil {
		t.Fatalf("linking a fake ssh: %v", err)
	}
	return path
}

// watched is a shell whose pty is wired to a state the way a session wires it:
// output reaches the state before the screen, and keystrokes reach it before
// the shell.
type watched struct {
	ptmx     *os.File
	out      *ptyOutput
	state    *state
	shellPid int
}

func watchShell(t *testing.T, sh shellUnderTest) *watched {
	t.Helper()

	shell, ptmx, out := startShell(t, sh)
	w := &watched{ptmx: ptmx, out: out, shellPid: shell.Process.Pid}
	w.state = newState(w.shellPid, ptmx, appconfig.Limits{})
	out.observe(func(p []byte) { _ = w.state.observeOutput(p) })
	return w
}

// press types at the shell the way a session does, showing the keys to the
// state on their way to the pty.
func (w *watched) press(t *testing.T, keys string) {
	t.Helper()

	w.state.typed([]byte(keys))
	writeBytes(t, w.ptmx, keys)
}

// awaitGate waits for the gates to reach the state want describes, since a
// shell arrives where it is being driven on its own schedule.
func awaitGate(t *testing.T, w *watched, want func(gate) bool, when string) gate {
	t.Helper()

	var g gate
	for deadline := time.Now().Add(answerTimeout); ; {
		if g = w.state.gate(); want(g) {
			return g
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: the gates never got there, they are %+v, and the session output was:\n%s",
				when, g, w.out.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// awaitOutput waits for the shell to print something containing want, which is
// how a test knows a keystroke reached it rather than its line editor's setup.
func awaitOutput(t *testing.T, w *watched, want string) {
	t.Helper()

	for deadline := time.Now().Add(answerTimeout); ; {
		if strings.Contains(w.out.String(), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the shell never printed anything containing %q, its output was:\n%s", want, w.out.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func assertFresh(t *testing.T, s *state, want bool, when string) {
	t.Helper()

	if got := s.gate().freshLine; got != want {
		t.Errorf("%s: the line reads as fresh=%t, want %t", when, got, want)
	}
}

// awaitQuiet waits for the shell to stop printing, so a keystroke is typed at
// a finished prompt rather than at a prompt still being drawn.
func awaitQuiet(t *testing.T, h *hosted, quiet time.Duration) {
	t.Helper()

	printed := len(h.out.String())
	for deadline := time.Now().Add(answerTimeout); ; {
		time.Sleep(quiet)
		now := len(h.out.String())
		if now == printed {
			return
		}
		printed = now

		if time.Now().After(deadline) {
			t.Fatalf("the shell never stopped printing, the session output was:\n%s", h.out.String())
		}
	}
}

// waitForPid waits for the shell to print a pid matching pattern, whose first
// submatch is the number.
func waitForPid(t *testing.T, out *ptyOutput, pattern *regexp.Regexp) int {
	t.Helper()

	for deadline := time.Now().Add(5 * time.Second); ; {
		if match := pattern.FindStringSubmatch(out.String()); match != nil {
			pid, err := strconv.Atoi(match[1])
			if err != nil {
				t.Fatalf("shell printed %q as a pid: %v", match[1], err)
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("shell output never matched %s, it was:\n%s", pattern, out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type ptyOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
	tap func([]byte)
}

// observe attaches a tap to the output, which is how a test drives session
// state with a real shell's real bytes, the way the session's own copy loop
// does. Bytes that arrived before the tap are shown to it too: the first
// prompt can land before the helper is wired, and that prompt is what
// probes the foreground process group.
func (o *ptyOutput) observe(tap func([]byte)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.tap = tap
	if n := o.buf.Len(); n > 0 {
		already := make([]byte, n)
		copy(already, o.buf.Bytes())
		tap(already)
	}
}

func (o *ptyOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.tap != nil {
		o.tap(p)
	}
	return o.buf.Write(p)
}

func (o *ptyOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

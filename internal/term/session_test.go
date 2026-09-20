//go:build darwin || linux

package term

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	appconfig "github.com/TheR1D/aty/internal/config"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	xterm "golang.org/x/term"
)

const (
	// answerTimeout is how long the shell gets to say something the test is
	// waiting for.
	answerTimeout = 5 * time.Second
	// attemptTimeout is how long a single question waits before being asked
	// again.
	attemptTimeout = 500 * time.Millisecond
)

// TestSessionPassesBytesThroughUnchanged is the transparency check. A hosted
// shell has to be indistinguishable from one the terminal started itself, so
// nothing aty copies may be rewritten in either direction.
func TestSessionPassesBytesThroughUnchanged(t *testing.T) {
	for _, sh := range installedShells() {
		t.Run(filepath.Base(sh.path), func(t *testing.T) {
			h := hostShell(t, sh, defaultSize())

			// Typing arrives at the shell and its output comes back. The answer
			// is arithmetic because the terminal echoes the question as well,
			// so only the result tells the two apart.
			ask(t, h, "echo SUM=$((19+23))", regexp.MustCompile(`SUM=42`))

			// Colors are escape sequences in the output stream, and they are
			// only colors if they reach the terminal as the bytes the shell
			// wrote. The echoed question holds the same sequence written out as
			// text, which is why the pattern insists on a real escape byte.
			writeLine(t, h.master, `printf 'COLOR=\033[31mred\033[0m\n'`)
			expect(t, h, regexp.MustCompile(`COLOR=\x1b\[31mred\x1b\[0m`))

			// Escape sequences travelling the other way have to survive too:
			// that is all an arrow key is inside vim. The pty is put in raw mode
			// first so the sequence is read as the three bytes it is, and the
			// readiness marker is printed after that, so the keys cannot be
			// typed while the terminal would still treat them as a line.
			writeLine(t, h.master, `stty raw -echo; echo KEYS=$((4+4)); head -c 3 | od -An -tx1; stty sane`)
			expect(t, h, regexp.MustCompile(`KEYS=8`))
			writeBytes(t, h.master, "\x1b[A")
			expect(t, h, regexp.MustCompile(`1b\s+5b\s+41`))
		})
	}
}

// TestSessionKeepsSignalKeysWorking checks that ^C and ^Z are still signals.
// aty's own terminal has to be the one place they are not acted on, since the
// byte has to reach the pty for its line discipline to raise the signal on the
// foreground job.
func TestSessionKeepsSignalKeysWorking(t *testing.T) {
	for _, sh := range installedShells() {
		t.Run(filepath.Base(sh.path), func(t *testing.T) {
			h := hostShell(t, sh, defaultSize())

			// The job is a single sleeping process and says nothing, because the
			// terminal handover is then the whole story: there is no window
			// between a job announcing itself and the fork of the process that
			// actually blocks, and a signal that arrives in such a window
			// changes nothing visible for as long as the sleep lasts.
			writeLine(t, h.master, "sleep 30")
			awaitForegroundJob(t, h)
			writeBytes(t, h.master, "\x03")
			// A sleep cannot decline SIGINT, so the shell getting the terminal
			// back is proof the byte became a signal rather than input.
			awaitPrompt(t, h)

			writeLine(t, h.master, "sleep 30")
			awaitForegroundJob(t, h)
			writeBytes(t, h.master, "\x1a")
			awaitPrompt(t, h)

			// Owning the terminal is not the same as being back at a prompt, and
			// a suspend that took the shell down with the job would still look
			// like the former, so the shell is asked something only a shell
			// reading its input can answer.
			ask(t, h, "echo BACK=$((5+5))", regexp.MustCompile(`BACK=10`))
		})
	}
}

// TestSessionForwardsTabCompletion covers the keystroke that only works if aty
// stays out of the way. A tab aty's own terminal acted on would never reach the
// shell's line editor, and the completion would silently not happen.
func TestSessionForwardsTabCompletion(t *testing.T) {
	for _, sh := range installedShells() {
		name := filepath.Base(sh.path)
		if name != "bash" && name != "zsh" {
			// Completion is a line editor feature, and a plain POSIX shell need
			// not have one.
			continue
		}
		t.Run(name, func(t *testing.T) {
			h := hostShell(t, sh, defaultSize())

			const (
				target = "aty_tab_completion_target"
				typed  = "aty_tab_completion_tar"
			)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, target), nil, 0o600); err != nil {
				t.Fatalf("creating the file to complete: %v", err)
			}
			ask(t, h, "cd "+dir+" && echo CD=$((1+1))", regexp.MustCompile(`CD=2`))

			// Only part of the name is typed, so the rest of it can only appear
			// because the line editor completed it and the terminal echoed what
			// it inserted.
			writeBytes(t, h.master, "echo "+typed+"\t")
			expect(t, h, regexp.MustCompile(target))
		})
	}
}

// TestSessionReportsShellExitStatus checks that aty exits the way the shell
// did, since anything reading aty's status is really asking about the shell.
func TestSessionReportsShellExitStatus(t *testing.T) {
	for _, sh := range installedShells() {
		t.Run(filepath.Base(sh.path), func(t *testing.T) {
			h := hostShell(t, sh, defaultSize())

			writeLine(t, h.master, "exit 7")
			if code := h.exitStatus(t); code != 7 {
				t.Errorf("aty exited %d after the shell exited 7", code)
			}
		})
	}
}

// TestSessionReportsShellKilledBySignal covers the shell dying without an exit
// code of its own. 128 plus the signal is the status a shell reports for a
// child killed that way, so it is the status aty reports for the shell.
func TestSessionReportsShellKilledBySignal(t *testing.T) {
	h := hostShell(t, shellUnderTest{path: "/bin/sh", args: []string{"-i"}}, defaultSize())

	// $$ is the shell itself, and SIGKILL is the one signal an interactive
	// shell cannot decline.
	writeLine(t, h.master, "kill -9 $$")
	if code, want := h.exitStatus(t), 128+int(unix.SIGKILL); code != want {
		t.Errorf("aty exited %d after the shell was killed, want %d", code, want)
	}
}

// TestSessionRestoresTerminalModes is the check that aty cannot leave a
// terminal unusable. That it displaces the modes at all is something every
// other test here rides on, since hosting a shell starts by waiting for it;
// what this one adds is that they come back exactly.
func TestSessionRestoresTerminalModes(t *testing.T) {
	h := hostShell(t, shellUnderTest{path: "/bin/sh", args: []string{"-i"}}, defaultSize())

	writeLine(t, h.master, "exit 0")
	if code := h.exitStatus(t); code != 0 {
		t.Errorf("aty exited %d after the shell exited 0", code)
	}

	if after := terminalModes(t, h.terminal); !reflect.DeepEqual(h.initialModes, after) {
		t.Errorf("aty left the terminal in different modes than it found:\nbefore: %+v\nafter:  %+v", h.initialModes, after)
	}
}

// TestSessionRestoresTerminalOnPanic covers the crash path. A panic in one of
// the copy goroutines takes the process down without running any deferred
// cleanup elsewhere, so the goroutine has to put the terminal back itself,
// before the panic carries on and is printed.
func TestSessionRestoresTerminalOnPanic(t *testing.T) {
	_, terminal := openPty(t)
	before := terminalModes(t, terminal)

	s := &session{cfg: Config{In: terminal, Out: terminal}}
	if err := s.enterRaw(); err != nil {
		t.Fatalf("entering raw mode: %v", err)
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic stopped at the terminal being restored instead of carrying on")
			}
		}()
		defer s.restoreOnPanic()
		panic("a copy goroutine failed")
	}()

	if after := terminalModes(t, terminal); !reflect.DeepEqual(before, after) {
		t.Errorf("a panic left the terminal in raw mode:\nbefore: %+v\nafter:  %+v", before, after)
	}
}

// TestSessionRestoresTerminalOnSignal covers being killed and being suspended
// from outside. Both end with someone else expecting the terminal back, and a
// signal that ends a process cannot be tested inside the process making the
// assertions, so aty here is this test binary run again as a child.
func TestSessionRestoresTerminalOnSignal(t *testing.T) {
	if os.Getenv(hostShellEnv) != "" {
		hostShellAsChild()
		return
	}

	master, terminal := openPty(t)
	go func() {
		// The pty fills up and blocks the shell otherwise.
		_, _ = io.Copy(io.Discard, master)
	}()

	before := terminalModes(t, terminal)

	child := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	child.Env = append(os.Environ(), hostShellEnv+"=1")
	child.Stdin, child.Stdout, child.Stderr = terminal, terminal, terminal
	if err := child.Start(); err != nil {
		t.Fatalf("starting aty as a child: %v", err)
	}

	var waitErr error
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		waitErr = child.Wait()
	}()
	t.Cleanup(func() {
		_ = child.Process.Kill()
		select {
		case <-finished:
		case <-time.After(answerTimeout):
			t.Error("aty outlived being killed")
		}
	})

	displaced := func(now *xterm.State) bool { return !reflect.DeepEqual(before, now) }
	restored := func(now *xterm.State) bool { return reflect.DeepEqual(before, now) }
	awaitModes(t, terminal, displaced, "aty never put the terminal in raw mode")

	signalChild(t, child, unix.SIGTSTP)
	awaitModes(t, terminal, restored, "a suspended aty left the terminal in raw mode")
	awaitStopped(t, child)

	signalChild(t, child, unix.SIGCONT)
	awaitModes(t, terminal, displaced, "a resumed aty did not take the terminal back")

	signalChild(t, child, unix.SIGTERM)
	select {
	case <-finished:
	case <-time.After(answerTimeout):
		t.Fatal("aty did not end when it was terminated")
	}
	awaitModes(t, terminal, restored, "a terminated aty left the terminal in raw mode")

	// Dying of the signal rather than exiting is what tells whoever started aty
	// what actually happened.
	var exit *exec.ExitError
	if !errors.As(waitErr, &exit) {
		t.Fatalf("aty returned %v after being terminated, want the signal", waitErr)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != unix.SIGTERM {
		t.Errorf("aty ended as %v after being terminated, want killed by SIGTERM", exit)
	}
}

// TestSessionTracksTerminalSize covers both halves of the size story: the shell
// starts out at the size of the terminal aty was launched in, and a later
// resize reaches it. Without either one, a full-screen program draws itself to
// the wrong shape.
func TestSessionTracksTerminalSize(t *testing.T) {
	h := hostShell(t, shellUnderTest{path: "/bin/sh", args: []string{"-i"}}, &pty.Winsize{Rows: 30, Cols: 100})

	// stty reads the size from the pty, which is where a full-screen program
	// reads it from too.
	ask(t, h, "stty size", regexp.MustCompile(`\b30 100\b`))

	if err := pty.Setsize(h.master, &pty.Winsize{Rows: 40, Cols: 120}); err != nil {
		t.Fatalf("resizing the terminal: %v", err)
	}
	// The kernel sends SIGWINCH to the session that owns the terminal, and the
	// test's pty has no session, so the signal aty would have been sent is
	// raised here instead.
	if err := unix.Kill(os.Getpid(), unix.SIGWINCH); err != nil {
		t.Fatalf("raising SIGWINCH: %v", err)
	}

	// A signal handler applies the new size, so it can land after the signal
	// itself; asking repeatedly is what waits for it instead of racing it.
	ask(t, h, "stty size", regexp.MustCompile(`\b40 120\b`))
}

const cursorOrange = "\x1b]12;#FFA500\a"

// TestTapTintsTheCursorAfterAPrompt is the orange cursor: a line editor
// announcing a prompt is followed by OSC 12, the shell's own bytes are not
// rewritten, and a prompt on the alternate screen is left alone.
func TestTapTintsTheCursorAfterAPrompt(t *testing.T) {
	var out bytes.Buffer
	s := newState(os.Getpid(), nil, appconfig.Limits{})
	tap := tap{
		observe:  s.observeOutput,
		to:       &out,
		onPrompt: func() { _, _ = out.Write([]byte(cursorOrange)) },
	}

	if _, err := tap.Write([]byte("$ ")); err != nil {
		t.Fatalf("writing ordinary output: %v", err)
	}
	if bytes.Contains(out.Bytes(), []byte(cursorOrange)) {
		t.Errorf("ordinary output painted the cursor: %q", out.Bytes())
	}

	out.Reset()
	prompt := []byte("$ \x1b[?2004h")
	if _, err := tap.Write(prompt); err != nil {
		t.Fatalf("writing a prompt: %v", err)
	}
	got := out.Bytes()
	if !bytes.HasPrefix(got, prompt) {
		t.Errorf("a prompt was rewritten, got %q", got)
	}
	if !bytes.HasSuffix(got, []byte(cursorOrange)) {
		t.Errorf("a prompt did not paint the cursor orange after itself, got %q", got)
	}

	out.Reset()
	if _, err := tap.Write([]byte("\x1b[?1049h\x1b[?2004h")); err != nil {
		t.Fatalf("writing an alternate-screen prompt: %v", err)
	}
	if bytes.Contains(out.Bytes(), []byte(cursorOrange)) {
		t.Errorf("an alternate-screen prompt painted the cursor: %q", out.Bytes())
	}
}

func TestScreenCursorColorSequences(t *testing.T) {
	for _, test := range []struct {
		color appconfig.Color
		want  string
	}{
		{color: "", want: cursorOrange + cursorDefault},
		{color: "orange", want: cursorOrange + cursorDefault},
		{color: "blue", want: "\x1b]12;#6495ED\a" + cursorDefault},
		{color: "none", want: ""},
	} {
		t.Run(string(test.color), func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatalf("pipe: %v", err)
			}
			t.Cleanup(func() {
				_ = reader.Close()
				_ = writer.Close()
			})

			sc := newScreen(writer, test.color)
			sc.tintCursor()
			sc.resetCursorColor()
			_ = writer.Close()

			got, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("reading cursor sequences: %v", err)
			}
			if string(got) != test.want {
				t.Errorf("cursor sequences are %q, want %q", got, test.want)
			}
		})
	}
}

// TestSessionNeedsATerminal covers being started with nothing to hand to a
// shell. aty has to say so rather than start one blind.
func TestSessionNeedsATerminal(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})

	if code, err := Run(Config{Shell: "/bin/sh", In: reader, Out: writer}); err == nil {
		t.Errorf("hosting a shell on a pipe returned %d and no error, want an error", code)
	}
}

func TestSessionCancellationRestoresTerminal(t *testing.T) {
	master, terminal, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = master.Close()
		_ = terminal.Close()
	}()
	before, err := xterm.GetState(int(terminal.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = RunContext(ctx, Config{
		Shell: "/bin/sh",
		Args:  []string{"-c", "sleep 30"},
		In:    terminal,
		Out:   terminal,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunContext returned %v, want context cancellation", err)
	}
	after, err := xterm.GetState(int(terminal.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Error("cancelled session did not restore terminal state")
	}
}

func TestSessionStartupFailureRestoresTerminal(t *testing.T) {
	master, terminal, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = master.Close()
		_ = terminal.Close()
	}()
	before, err := xterm.GetState(int(terminal.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Run(Config{
		Shell: filepath.Join(t.TempDir(), "missing-shell"),
		In:    terminal,
		Out:   terminal,
	})
	if err == nil {
		t.Fatal("missing shell started successfully")
	}
	after, err := xterm.GetState(int(terminal.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Error("failed startup did not restore terminal state")
	}
}

func TestSessionCloseIsIdempotent(t *testing.T) {
	master, peer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peer.Close() }()
	session := &session{ptmx: master}
	session.close()
	session.close()
	if _, err := master.Read(make([]byte, 1)); err == nil {
		t.Fatal("session close left its PTY open")
	}
}

func TestDefaultShellFollowsTheEnvironment(t *testing.T) {
	t.Setenv("SHELL", "/bin/ksh")
	if got := DefaultShell(); got != "/bin/ksh" {
		t.Errorf("default shell is %q, want the one in $SHELL", got)
	}

	t.Setenv("SHELL", "")
	if got := DefaultShell(); got != "/bin/sh" {
		t.Errorf("default shell without $SHELL is %q, want /bin/sh", got)
	}
}

// hosted is an aty session with a real terminal on either side of it. The test
// holds the master of the pty aty takes for the user's terminal, so keystrokes
// and output travel the path a real session uses: test, aty, pty, shell, and
// back again.
type hosted struct {
	master   *os.File
	terminal *os.File
	out      *ptyOutput

	// shellPid is the hosted shell, which leads its own process group, so it is
	// also what the pty reports as its foreground process group at a prompt.
	shellPid int

	// initialModes is what aty's terminal looked like before aty touched it.
	initialModes *xterm.State

	finished chan struct{}
	code     int
	err      error
}

func hostShell(t *testing.T, sh shellUnderTest, size *pty.Winsize) *hosted {
	t.Helper()
	return hostShellWithEnv(t, sh, size, testEnv(t))
}

func hostShellWithEnv(t *testing.T, sh shellUnderTest, size *pty.Winsize, env []string) *hosted {
	t.Helper()

	master, terminal := openPty(t)
	if err := pty.Setsize(master, size); err != nil {
		t.Fatalf("sizing the pty that stands in for the user's terminal: %v", err)
	}
	h := &hosted{
		master:       master,
		terminal:     terminal,
		out:          &ptyOutput{},
		initialModes: terminalModes(t, terminal),
		finished:     make(chan struct{}),
	}
	go func() {
		_, _ = io.Copy(h.out, master)
	}()
	go func() {
		defer close(h.finished)
		h.code, h.err = Run(Config{
			Shell: sh.path,
			Args:  sh.args,
			Env:   env,
			In:    terminal,
			Out:   terminal,
		})
	}()

	// Nothing can be typed at a shell that has not read its way to a prompt
	// yet, and the pid it answers with is the process group the cleanup kills.
	awaitRawMode(t, h)
	answer := ask(t, h, "echo SHELL=$$", regexp.MustCompile(`SHELL=(\d+)`))
	shellPid, err := strconv.Atoi(answer[1])
	if err != nil {
		t.Fatalf("the shell reported %q as its pid: %v", answer[1], err)
	}
	h.shellPid = shellPid

	t.Cleanup(func() {
		// A session outlives a failed test unless the shell is killed, since
		// that is the only thing aty waits for. The shell leads its own process
		// group, so the whole group goes with it.
		_ = unix.Kill(-h.shellPid, unix.SIGKILL)
		select {
		case <-h.finished:
		case <-time.After(answerTimeout):
			t.Errorf("aty did not return after the shell was killed, the session output was:\n%s", h.out.String())
		}
	})
	return h
}

// openPty returns a pty for aty to take as the user's terminal, master first.
func openPty(t *testing.T) (master, terminal *os.File) {
	t.Helper()

	master, terminal, err := pty.Open()
	if err != nil {
		t.Fatalf("opening the pty that stands in for the user's terminal: %v", err)
	}
	t.Cleanup(func() {
		_ = terminal.Close()
		_ = master.Close()
	})
	return master, terminal
}

// hostShellEnv marks the child half of TestSessionRestoresTerminalOnSignal.
const hostShellEnv = "ATY_TEST_HOST_SHELL"

// hostShellAsChild is aty: it hosts a shell on the terminal the parent handed
// down and exits the way Run says to, which is what the parent inspects.
func hostShellAsChild() {
	code, err := Run(Config{
		Shell: "/bin/sh",
		Args:  []string{"-i"},
		Env:   []string{"PATH=" + os.Getenv("PATH"), "TERM=dumb", "ENV="},
	})
	if err != nil {
		os.Exit(2)
	}
	os.Exit(code)
}

func signalChild(t *testing.T, child *exec.Cmd, sig unix.Signal) {
	t.Helper()

	if err := child.Process.Signal(sig); err != nil {
		t.Fatalf("sending %v to aty: %v", sig, err)
	}
}

func awaitModes(t *testing.T, terminal *os.File, want func(*xterm.State) bool, complaint string) {
	t.Helper()

	for deadline := time.Now().Add(answerTimeout); ; {
		if modes, err := xterm.GetState(int(terminal.Fd())); err == nil && want(modes) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(complaint)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// awaitStopped waits for aty to really be stopped, which is the half of a
// suspend that a restored terminal says nothing about: catching a signal is not
// obeying it, and a suspend that only looked like one would leave aty running
// with its terminal in the wrong modes.
func awaitStopped(t *testing.T, child *exec.Cmd) {
	t.Helper()

	var state string
	for deadline := time.Now().Add(answerTimeout); ; {
		if state = processState(child.Process.Pid); strings.HasPrefix(state, "T") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a suspended aty is in state %q, want stopped", state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// processState reads a process state the way both systems spell it, which is
// what ps is for: this is a test looking at a process from the outside.
func processState(pid int) string {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ask types a command at the shell and waits for the answer it prints. The
// question is repeated because it can go unanswered through no fault of aty's:
// a signal key flushes the terminal's input queue on its way through the line
// discipline, and a shell still starting up may discard whatever was typed
// before it took the terminal over.
func ask(t *testing.T, h *hosted, command string, answer *regexp.Regexp) []string {
	t.Helper()

	for deadline := time.Now().Add(answerTimeout); ; {
		writeLine(t, h.master, command)
		if match := h.await(answer, attemptTimeout); match != nil {
			return match
		}
		if time.Now().After(deadline) {
			t.Fatalf("the shell never answered %q with anything matching %s, the session output was:\n%s",
				command, answer, h.out.String())
		}
	}
}

// awaitForegroundJob waits until a job other than the shell owns the terminal,
// which is what a signal key needs in order to reach that job. A signal that
// arrives while the shell still owns the terminal is one an interactive shell
// ignores on its jobs' behalf, so the keystroke would look as though aty had
// swallowed it.
func awaitForegroundJob(t *testing.T, h *hosted) {
	t.Helper()

	if !h.awaitTerminalOwner(func(foreground int) bool { return foreground != h.shellPid }) {
		t.Fatalf("no job ever took the terminal from the shell, the session output was:\n%s", h.out.String())
	}
}

// awaitPrompt waits until the shell owns the terminal again, which is how the
// test sees that the foreground job stopped or died.
func awaitPrompt(t *testing.T, h *hosted) {
	t.Helper()

	if !h.awaitTerminalOwner(func(foreground int) bool { return foreground == h.shellPid }) {
		t.Fatalf("the shell never got the terminal back from its job, the session output was:\n%s", h.out.String())
	}
}

// awaitTerminalOwner polls the process table rather than the pty aty owns,
// because the test is on the outside of the session and this is the same reading
// from a source the test does not share with the code under test.
func (h *hosted) awaitTerminalOwner(owner func(foreground int) bool) bool {
	for deadline := time.Now().Add(answerTimeout); ; {
		if foreground, err := tpgid(h.shellPid); err == nil && isLivePgid(foreground) && owner(foreground) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// awaitRawMode waits for aty to take its terminal over, which it does before
// the shell exists. Typing earlier than that is typing at a terminal still
// echoing and line-editing on its own.
func awaitRawMode(t *testing.T, h *hosted) {
	t.Helper()

	for deadline := time.Now().Add(answerTimeout); ; {
		if modes, err := xterm.GetState(int(h.terminal.Fd())); err == nil && !reflect.DeepEqual(h.initialModes, modes) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("aty never put its own terminal in raw mode")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// expect waits for output matching pattern, which is how a test waits for the
// session to reach a known point instead of sleeping.
func expect(t *testing.T, h *hosted, pattern *regexp.Regexp) []string {
	t.Helper()

	match := h.await(pattern, answerTimeout)
	if match == nil {
		t.Fatalf("the session output never matched %s, it was:\n%s", pattern, h.out.String())
	}
	return match
}

func (h *hosted) await(pattern *regexp.Regexp, within time.Duration) []string {
	for deadline := time.Now().Add(within); ; {
		if match := pattern.FindStringSubmatch(h.out.String()); match != nil {
			return match
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// exitStatus waits for the session to end and reports the status aty returned.
func (h *hosted) exitStatus(t *testing.T) int {
	t.Helper()

	select {
	case <-h.finished:
	case <-time.After(answerTimeout):
		t.Fatalf("aty is still hosting the shell after it was told to end, the session output was:\n%s", h.out.String())
	}
	if h.err != nil {
		t.Fatalf("hosting the shell: %v", h.err)
	}
	return h.code
}

func terminalModes(t *testing.T, terminal *os.File) *xterm.State {
	t.Helper()

	modes, err := xterm.GetState(int(terminal.Fd()))
	if err != nil {
		t.Fatalf("reading the terminal modes: %v", err)
	}
	return modes
}

func defaultSize() *pty.Winsize {
	return &pty.Winsize{Rows: 24, Cols: 80}
}

// testEnv keeps the shells under test away from the developer's own startup
// files, which are free to print anything at all into the stream the assertions
// read. A short prompt keeps hostnames and working directories from wrapping
// command echoes in tests that inspect the raw output stream.
func testEnv(t *testing.T) []string {
	t.Helper()

	return []string{
		"PATH=" + os.Getenv("PATH"),
		"TERM=xterm-256color",
		"HOME=" + t.TempDir(),
		"PS1=$ ",
		"ENV=",
		"BASH_ENV=",
	}
}

func TestSessionEnablesZshInteractiveComments(t *testing.T) {
	path, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh is not installed")
	}
	h := hostShell(t, shellUnderTest{path: path, args: []string{"-f", "-i"}}, defaultSize())
	ask(t, h, "echo COMMENTS=$options[interactivecomments]", regexp.MustCompile(`COMMENTS=on`))
	ask(t, h, "echo COMMENT_RESULT=$((20+22)) # this is a comment", regexp.MustCompile(`COMMENT_RESULT=42\r?\n`))
	// Enabling an option already set by aty must remain harmless.
	ask(t, h, "setopt interactivecomments; echo STILL_ENABLED=$options[interactivecomments]", regexp.MustCompile(`STILL_ENABLED=on`))
}

func TestSessionExportsActiveMarkerBeforeZshStartup(t *testing.T) {
	path, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh is not installed")
	}
	dir := t.TempDir()
	// A missing marker would cause the startup hook to launch aty again.
	if err := os.WriteFile(filepath.Join(dir, ".zshrc"), []byte(`
if [[ "${ATY_ACTIVE:-}" != 1 ]]; then
  print -r -- "MISSING_SESSION_MARKER"
  exit 91
fi
`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(testEnv(t), "ZDOTDIR="+dir, "ATY_ACTIVE=0")
	h := hostShellWithEnv(t, shellUnderTest{path: path, args: []string{"-d", "-i"}}, defaultSize(), env)
	ask(t, h, `sh -c 'printf "CHILD_MARKER=%s\n" "$ATY_ACTIVE"'`, regexp.MustCompile(`CHILD_MARKER=1\r?\n`))
	if got := env[len(env)-1]; got != "ATY_ACTIVE=0" {
		t.Fatalf("session changed caller environment: %q", got)
	}
}

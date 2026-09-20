//go:build darwin || linux

package term

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// ErrNoPaste is the outcome of ProbePaste when the program started, wrote
// something, and never sent CSI ? 2004 h. That is the mark aty needs to
// intercept ? and to split the transcript, so a shell that never sends it
// is a terminal and nothing more.
var ErrNoPaste = errors.New("never emits CSI ? 2004 h")

// pasteProbeTimeout is how long a shell gets to announce a prompt.
// The mark arrives with the first prompt, so a shell that needs its
// startup files still has to finish them; one that never sends the
// mark is left sitting at a prompt until this bound.
const pasteProbeTimeout = 5 * time.Second

func ProbePaste(shell string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), pasteProbeTimeout)
	defer cancel()
	err := ProbePasteContext(ctx, shell, args...)
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s %w", resolvedShell(shell), ErrNoPaste)
	}
	return err
}

// ProbePasteContext runs a disposable PTY and observes its output without
// changing or interpreting any bytes beyond DEC mode tracking.
func ProbePasteContext(ctx context.Context, shell string, args ...string) error {
	shell = resolvedShell(shell)
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, shell, args...)
	cmd.Env = os.Environ()
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		return fmt.Errorf("starting %s in a pty: %w", shell, err)
	}

	err = waitForPaste(ctx, master)
	_ = master.Close()
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}

	if err == nil {
		return nil
	}
	if errors.Is(err, ErrNoPaste) {
		return fmt.Errorf("%s %w", shell, ErrNoPaste)
	}
	return fmt.Errorf("reading from %s: %w", shell, err)
}

func waitForPaste(ctx context.Context, reader io.ReadCloser) error {
	result := make(chan error, 1)
	go func() { result <- detectPaste(reader) }()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		// PTY reads cannot be deadline-bound on Darwin. Closing the master is
		// the cancellation mechanism; join the reader before returning.
		_ = reader.Close()
		<-result
		return ctx.Err()
	}
}

func resolvedShell(shell string) string {
	if shell == "" {
		return DefaultShell()
	}
	return shell
}

func detectPaste(reader io.Reader) error {
	var modes decModes
	buffer := make([]byte, 4096)
	for {
		n, err := reader.Read(buffer)
		for _, event := range modes.feed(buffer[:n]) {
			if event.kind == promptReady {
				return nil
			}
		}
		if err != nil {
			if pasteGone(err) {
				return ErrNoPaste
			}
			return err
		}
	}
}

// pasteGone is a read that ended because the shell went away, not because
// the pty itself failed. Linux reports a hangup as EIO; Darwin as EOF.
// Either way there was no 2004 h in what was written.
func pasteGone(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, unix.EIO)
}

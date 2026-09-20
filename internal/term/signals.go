//go:build darwin || linux

package term

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const signalExitTimeout = 250 * time.Millisecond

// watchSignals owns process-level terminal signals for one session. All
// terminal restoration runs through the session lock, so resize, suspend,
// shutdown, and normal cleanup cannot race terminal or PTY state.
func (s *session) watchSignals() func() {
	signals := make(chan os.Signal, 8)
	signal.Notify(
		signals,
		unix.SIGWINCH,
		unix.SIGHUP,
		unix.SIGTERM,
		unix.SIGINT,
		unix.SIGQUIT,
		unix.SIGTSTP,
		unix.SIGCONT,
	)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for received := range signals {
			switch received {
			case unix.SIGWINCH:
				_ = s.resize()
			case unix.SIGTSTP:
				s.restoreTerminal()
				_ = unix.Kill(os.Getpid(), unix.SIGSTOP)
			case unix.SIGCONT:
				_ = s.enterRaw()
				if s.screen != nil {
					s.screen.tintCursor()
				}
				_ = s.resize()
			default:
				s.restoreTerminal()
				die(received)
			}
		}
	}()

	return sync.OnceFunc(func() {
		signal.Stop(signals)
		close(signals)
		<-done
	})
}

func die(received os.Signal) {
	number, ok := received.(syscall.Signal)
	if !ok {
		os.Exit(1)
	}
	signal.Reset(number)
	_ = unix.Kill(os.Getpid(), number)
	time.Sleep(signalExitTimeout)
	os.Exit(128 + int(number))
}

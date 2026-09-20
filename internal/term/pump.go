//go:build darwin || linux

package term

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// inputPump makes a terminal read cancellable without closing the caller's
// terminal. stop wakes poll through an owned pipe, and run owns both pipe ends.
type inputPump struct {
	input *os.File
	to    io.Writer
	wake  [2]int
	once  sync.Once
}

func newInputPump(input *os.File, to io.Writer) (*inputPump, error) {
	pump := &inputPump{input: input, to: to}
	if err := unix.Pipe(pump.wake[:]); err != nil {
		return nil, fmt.Errorf("term: creating input wake pipe: %w", err)
	}
	return pump, nil
}

func (p *inputPump) stop() {
	p.once.Do(func() {
		_, _ = unix.Write(p.wake[1], []byte{1})
	})
}

func (p *inputPump) run() error {
	defer func() {
		_ = unix.Close(p.wake[0])
		_ = unix.Close(p.wake[1])
	}()
	poll := []unix.PollFd{
		{Fd: int32(p.input.Fd()), Events: unix.POLLIN},
		{Fd: int32(p.wake[0]), Events: unix.POLLIN},
	}
	buffer := make([]byte, 32*1024)
	for {
		if _, err := unix.Poll(poll, -1); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("term: polling input: %w", err)
		}
		if poll[1].Revents != 0 {
			return nil
		}
		if poll[0].Revents&unix.POLLIN == 0 {
			if poll[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
				return io.EOF
			}
			continue
		}
		n, err := p.input.Read(buffer)
		if n > 0 {
			written, writeErr := p.to.Write(buffer[:n])
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
		}
		if err != nil {
			return err
		}
	}
}

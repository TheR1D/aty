//go:build darwin || linux

package term

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

const outputDrainTimeout = 100 * time.Millisecond

// serve relays both terminal streams until the shell exits or either stream
// stops. It joins the input reader and drains shell output before returning.
func (s *session) serve(ctx context.Context) (int, error) {
	s.mu.Lock()
	master := s.ptmx
	s.mu.Unlock()
	input := &keyboard{
		initialPrompt: s.cfg.InitialPrompt,
		state:         s.state,
		master:        master,
		screen:        s.screen,
		ask:           s.cfg.Ask,
		warm:          s.cfg.Warm,
		agent:         s.cfg.Agent,
		dump:          s.cfg.Dump,
	}
	pump, err := newInputPump(s.cfg.In, input)
	if err != nil {
		s.terminate()
		_ = s.cmd.Wait()
		return 0, err
	}

	inputDone := make(chan error, 1)
	outputDone := make(chan error, 1)
	waitDone := make(chan error, 1)
	go func() { inputDone <- pump.run() }()
	go func() { outputDone <- s.copyOutput(master) }()
	go func() { waitDone <- s.cmd.Wait() }()

	var cause error
	select {
	case <-waitDone:
	case err := <-inputDone:
		inputDone = nil
		if err != nil {
			cause = fmt.Errorf("term: reading terminal input: %w", err)
		}
		s.terminate()
		<-waitDone
	case err := <-outputDone:
		outputDone = nil
		if err != nil && !pasteGone(err) {
			cause = fmt.Errorf("term: writing terminal output: %w", err)
		}
		s.terminate()
		<-waitDone
	case <-ctx.Done():
		cause = ctx.Err()
		s.terminate()
		<-waitDone
	}

	pump.stop()
	if inputDone != nil {
		<-inputDone
	}

	if outputDone != nil {
		if err := s.drainOutput(outputDone); cause == nil {
			cause = err
		}
	}
	if s.cmd.ProcessState == nil {
		if cause == nil {
			cause = errors.New("term: shell exited without process state")
		}
		return 0, cause
	}
	return exitStatus(s.cmd.ProcessState), cause
}

func (s *session) copyOutput(master io.Reader) error {
	defer s.restoreOnPanic()

	_, err := io.Copy(tap{
		observe:  s.state.observeOutput,
		to:       s.screen,
		onPrompt: s.screen.tintCursor,
	}, master)
	return err
}

// tap observes output without changing it.
type tap struct {
	observe  func([]byte) bool
	to       io.Writer
	onPrompt func()
}

func (t tap) Write(p []byte) (int, error) {
	prompt := t.observe(p)
	n, err := t.to.Write(p)
	if prompt && n == len(p) && err == nil && t.onPrompt != nil {
		t.onPrompt()
	}
	return n, err
}

// drainOutput gives buffered shell output time to reach the screen. A child
// may keep the PTY open after the shell exits, so close it after the grace period.
func (s *session) drainOutput(done <-chan error) error {
	timer := time.NewTimer(outputDrainTimeout)
	defer timer.Stop()

	var err error
	select {
	case err = <-done:
	case <-timer.C:
		s.closePTY()
		err = <-done
	}
	if err != nil && !pasteGone(err) {
		return fmt.Errorf("term: writing terminal output: %w", err)
	}
	return nil
}

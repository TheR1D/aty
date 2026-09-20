//go:build darwin || linux

package term

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	commandPoll      = 50 * time.Millisecond
	confirmHintDelay = 50 * time.Millisecond
)

func (k *keyboard) runner(q *query, inject *injection) func(context.Context, string) (string, error) {
	return func(ctx context.Context, command string) (string, error) {
		command = strings.TrimSpace(command)
		if command == "" {
			return "", fmt.Errorf("term: the command is empty")
		}
		if !typable(command) {
			return "", fmt.Errorf("%w: %q", errUntypable, command)
		}
		if err := k.revise(q, inject, command); err != nil {
			return "", err
		}
		output, err := k.waitConfirm(ctx, q, command)
		k.resetInjection(inject)
		if err != nil {
			return output, err
		}
		return output, k.resumeWait(q)
	}
}

func (k *keyboard) resumeWait(q *query) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if q.editor.finished() {
		return errTakenBack
	}
	if !q.waiting || !q.erased {
		return nil
	}
	q.thought = nil
	q.thoughtOnly = true
	q.erased = false
	q.anchored = false
	q.visible = visible(k.screen.columns(), 0)
	return q.render(q.editor.snapshot())
}

func (k *keyboard) waitConfirm(ctx context.Context, q *query, command string) (string, error) {
	k.mu.Lock()
	if q.editor.finished() || ctx.Err() != nil {
		k.mu.Unlock()
		return "", errTakenBack
	}
	before, _ := k.state.commandCompletion()
	autoRun := q.editor.startConfirmation()
	k.recordAnswer(q, command)
	if autoRun {
		if err := k.toShell([]byte{'\r'}); err != nil {
			q.editor.clearConfirmation()
			k.mu.Unlock()
			return "", err
		}
	}
	k.mu.Unlock()

	defer func() {
		k.mu.Lock()
		if q.editor.confirmingLine() {
			k.recordAnswer(q, command)
		}
		_ = q.wipeConfirmHint()
		q.editor.clearConfirmation()
		q.hintDone = false
		k.mu.Unlock()
	}()

	started := time.Now()
	ticker := time.NewTicker(commandPoll)
	defer ticker.Stop()
	hinted := false
	for {
		text, done, err := k.commandFinished(q, before)
		if done {
			return text, err
		}
		if !hinted && k.state.gate().promptReady && time.Since(started) >= confirmHintDelay {
			k.mu.Lock()
			err := q.paintConfirmHint(q.editor.snapshot())
			k.mu.Unlock()
			if err != nil {
				return "", err
			}
			hinted = true
		}
		select {
		case <-ctx.Done():
			return "", errTakenBack
		case <-ticker.C:
		}
	}
}

func (k *keyboard) confirmKeys(input []byte) ([]byte, error) {
	q := k.query
	for len(input) > 0 {
		toolConfirm := q.editor.confirm == confirmTool
		if !toolConfirm && len(q.editor.partial) == 0 {
			if n := plainTextLen(input); n > 0 {
				if err := q.wipeConfirmHint(); err != nil {
					return input, err
				}
				if err := k.toShell(input[:n]); err != nil {
					return input[n:], err
				}
				input = input[n:]
				continue
			}
		}
		event, left, whole := q.editor.nextConfirmation(input)
		if !whole {
			return nil, nil
		}
		input = left
		if err := q.wipeConfirmHint(); err != nil {
			return input, err
		}
		if toolConfirm && event.move != 0 {
			q.approvalAt = min(max(q.approvalAt+event.move*8, 0), max(len(q.approvalText)-1, 0))
			if err := q.render(q.editor.snapshot()); err != nil {
				return input, err
			}
			continue
		}
		if event.approve {
			q.decideApproval(true)
			return nil, nil
		}
		if event.cancel {
			q.decideApproval(false)
			if err := k.end(); err != nil {
				return input, err
			}
			if event.interrupt {
				q.editor.clearConfirmation()
				return input, k.toShell(event.bytes)
			}
			return input, nil
		}
		if toolConfirm {
			continue
		}
		if err := k.toShell(event.bytes); err != nil {
			return input, err
		}
	}
	return nil, nil
}

func (k *keyboard) commandFinished(q *query, before uint64) (string, bool, error) {
	k.mu.Lock()
	taken := q.editor.finished()
	k.mu.Unlock()
	if taken {
		return "", true, errTakenBack
	}
	if !k.state.gate().open() {
		return "", false, nil
	}
	completed, result := k.state.commandCompletion()
	if completed <= before {
		return "", false, nil
	}
	return result, true, nil
}

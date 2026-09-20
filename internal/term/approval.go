//go:build darwin || linux

package term

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/TheR1D/aty/internal/llm"
)

// decideApproval never blocks input if the tool has already stopped waiting.
func (q *query) decideApproval(approved bool) {
	select {
	case q.approval <- approved:
	default:
	}
}

func (k *keyboard) approver(q *query) llm.ApproveTool {
	return func(ctx context.Context, name string, arguments json.RawMessage) error {
		k.mu.Lock()
		if q.editor.finished() || ctx.Err() != nil {
			k.mu.Unlock()
			return errTakenBack
		}
		if q.editor.mode.yolo {
			k.mu.Unlock()
			return nil
		}

		decision := make(chan bool, 1)
		q.approval = decision
		q.editor.confirm = confirmTool
		q.approvalText = []rune(toolSummary(name, arguments))
		q.approvalAt = 0
		q.hintDone = false
		if err := q.render(q.editor.snapshot()); err != nil {
			q.approval = nil
			q.editor.clearConfirmation()
			q.approvalText = nil
			k.mu.Unlock()
			return err
		}
		k.mu.Unlock()

		approved := false
		select {
		case approved = <-decision:
		case <-ctx.Done():
		}

		k.mu.Lock()
		if q.approval == decision {
			q.approval = nil
		}
		if q.editor.confirm == confirmTool {
			q.editor.clearConfirmation()
		}
		q.approvalText = nil
		q.approvalAt = 0
		q.thought = nil
		q.thoughtOnly = true
		q.hintDone = false
		if !q.editor.finished() {
			_ = q.render(q.editor.snapshot())
		}
		k.mu.Unlock()

		if !approved || ctx.Err() != nil {
			return errTakenBack
		}
		return nil
	}
}

func toolSummary(name string, arguments json.RawMessage) string {
	var compact bytes.Buffer
	if err := json.Compact(&compact, arguments); err != nil {
		return name
	}
	if compact.Len() == 0 || compact.String() == "{}" {
		return name
	}
	return fmt.Sprintf("%s %s", name, compact.String())
}

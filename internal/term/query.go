//go:build darwin || linux

package term

import "context"

// query joins the editor, overlay, and asynchronous work for one question.
// The keyboard mutex guards all mutable query state.
type query struct {
	queryView
	editor queryEditor

	// cancel stops the assistant.
	cancel        context.CancelFunc
	warmCancel    context.CancelFunc
	executionDone <-chan struct{}

	// keptQuestion is whether this query's text has already been stored on
	// a transcript turn. An agent question may write several commands;
	// repeating the question in front of each would duplicate it in context.
	keptQuestion bool
	// submitted is the question sent to the model, without a prompt modifier
	// such as the x in `?x`.
	submitted string
	approval  chan bool
}

// feed returns any input after submission or cancellation to the keyboard router.
func (q *query) feed(p []byte) (rest []byte, ending action, err error) {
	rest, ending = q.editor.feed(p)
	if ending != nothing {
		return rest, ending, nil
	}
	return nil, nothing, q.render(q.editor.snapshot())
}

// finish stops background work. The caller decides whether to erase the overlay.
func (q *query) finish() {
	q.editor.finish()
	q.stopWarmup()
	if q.cancel != nil {
		q.cancel()
	}
	if q.stopped != nil {
		close(q.stopped)
		q.stopped = nil
	}
}

func (q *query) stopWarmup() {
	if q.warmCancel != nil {
		q.warmCancel()
		q.warmCancel = nil
	}
}

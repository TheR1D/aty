//go:build darwin || linux

package term

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/TheR1D/aty/internal/llm"
)

// keyboard forwards input unchanged except for queries opened at an empty
// prompt. Each Write may contain multiple keystrokes or a paste.
type keyboard struct {
	initialPrompt func(string)
	state         *state
	master        io.Writer
	screen        *screen
	// A nil ask leaves query editing available without model responses.
	ask assistant
	// warm populates the selected provider's stable prompt prefix once a space
	// after the query marker confirms the user is starting a question.
	warm warmupFunc
	// A nil agent falls back to asking in thinking mode.
	agent agentFunc
	// dump formats the messages a question would send, for `?#`. A nil dump
	// still writes the question and the transcript.
	dump func(question string, transcript []llm.Turn) string

	// mu guards query state shared by input, model callbacks, and the spinner.
	mu    sync.Mutex
	query *query
}

// assistant streams full command revisions and accumulated reasoning. Command
// revisions go to the shell line editor; reasoning stays in the overlay. The
// transcript contains recent commands, output, and the questions behind them.
// Only an explicit ! marker submits the generated command automatically.
type assistant func(ctx context.Context, question string, transcript []llm.Turn, thinking bool, typing func(command string) error, think func(thought string) error) error

// warmupFunc precomputes a provider's system-prompt and transcript prefix.
// thinking and agent select the same request settings submit will eventually
// use, without coupling the terminal package to provider API details.
type warmupFunc func(ctx context.Context, transcript []llm.Turn, thinking, agent bool) error

// agentFunc runs a multi-step query with thinking enabled. run presents shell
// commands for confirmation and returns their output; approve handles external
// tools without sending them to the PTY.
type agentFunc func(ctx context.Context, question string, transcript []llm.Turn, typing func(command string) error, think func(thought string) error, run func(ctx context.Context, command string) (string, error), approve llm.ApproveTool) error

// Cancellation leaves the partial command in the shell editor. Further writes
// could race the user who now owns that line, so callbacks return errTakenBack.
var errTakenBack = errors.New("term: the query was taken back")

// Reject the entire revision if it contains unsafe terminal characters; a
// truncated command could have a different meaning.
var errUntypable = errors.New("term: the answer holds a character that cannot be typed at a shell")

// Write consumes the entire input batch, including keys handled by the query.
func (k *keyboard) Write(p []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	written := len(p)
	for len(p) > 0 {
		var err error
		if p, err = k.route(p); err != nil {
			return written, err
		}
	}
	return written, nil
}

// route consumes input through the next query trigger or editor transition.
func (k *keyboard) route(p []byte) ([]byte, error) {
	if k.query != nil {
		return k.edit(p)
	}

	at := bytes.IndexByte(p, triggerKey)
	if at < 0 {
		return nil, k.toShell(p)
	}
	if err := k.toShell(p[:at]); err != nil {
		return nil, err
	}
	p = p[at:]
	ready, partial := k.state.triggerState()
	if !ready || !k.state.gate().open() {
		n := 1
		// Ordinary text cannot make a dirty line fresh. Stop at editing keys
		// and incomplete sequences, which can change the next trigger's owner.
		if !partial {
			n = plainTextLen(p)
		}
		return p[n:], k.toShell(p[:n])
	}
	return p[1:], k.trigger()
}

// trigger opens the query after route has confirmed an empty shell prompt.
func (k *keyboard) trigger() error {
	if k.initialPrompt != nil {
		k.initialPrompt(k.state.capture.initialPS1())
		k.initialPrompt = nil
	}

	// A pending answer on an empty line was erased rather than executed.
	// Keep it in the transcript before recording the next question.
	k.state.capture.discardAnswer()

	k.query = &query{
		screen: k.screen, visible: visible(k.screen.columns(), markerWidth)}
	return k.query.render(k.query.editor.snapshot())
}

// edit handles mode changes, speculative warmup, and query editor transitions.
func (k *keyboard) edit(p []byte) ([]byte, error) {
	if k.query.editor.confirming() {
		return k.confirmKeys(p)
	}

	if k.query.editor.changeMode(p[0]) {
		k.query.visible = visible(k.screen.columns(), k.query.editor.markerWidth())
		return p[1:], k.query.render(k.query.editor.snapshot())
	}

	// Only a space after the marker starts warmup. No-context modes never warm.
	if thinking, agent, ok := k.query.editor.warmupMode(); ok && p[0] == ' ' {
		k.startWarmup(thinking, agent)
	}

	rest, ending, err := k.query.feed(p)
	if err != nil {
		return rest, err
	}
	switch ending {
	case cancelQuery:
		return rest, k.end()
	case submitQuery:
		return rest, k.submit()
	}
	return rest, nil
}

// end takes the query off the screen and gives the keyboard back to the shell.
func (k *keyboard) end() error {
	q := k.query
	k.query = nil
	q.finish()
	return q.erase()
}

// toShell records keystrokes before forwarding them so query gating stays current.
func (k *keyboard) toShell(p []byte) error {
	if len(p) == 0 {
		return nil
	}

	k.state.typed(p)
	if _, err := k.master.Write(p); err != nil {
		return fmt.Errorf("term: typing at the shell: %w", err)
	}
	return nil
}

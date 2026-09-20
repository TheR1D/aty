//go:build darwin || linux

package term

import (
	"context"
	"errors"
	"sync"
)

// queryExecution owns one submitted request from dispatch through the single
// transition that releases keyboard ownership.
type queryExecution struct {
	keyboard *keyboard
	query    *query
	plan     queryRequest
	ctx      context.Context
	inject   *injection
	once     sync.Once
	done     chan struct{}
}

func (k *keyboard) startWarmup(thinking, agent bool) {
	if k.query == nil || k.warm == nil {
		return
	}
	k.query.stopWarmup()
	ctx, cancel := context.WithCancel(context.Background())
	k.query.warmCancel = cancel
	transcript := k.state.transcript()
	go func() { _ = k.warm(ctx, transcript, thinking, agent) }()
}

func (k *keyboard) submit() error {
	q := k.query
	q.stopWarmup()
	snapshot := q.editor.snapshot()
	plan, ended := planQuery(snapshot, k.state.transcript(), k.ask != nil, k.agent != nil)
	if ended != "" {
		return k.end()
	}
	if plan.route == routeDump {
		return k.dumpSubmitted(plan)
	}

	q.submitted = plan.question
	ctx := k.beginRequest(q)

	execution := &queryExecution{
		keyboard: k, query: q, plan: plan, ctx: ctx,
		inject: &injection{}, done: make(chan struct{}),
	}
	q.executionDone = execution.done
	go execution.run()
	return q.render(q.editor.snapshot())
}

// beginRequest transfers the editor to background work. The caller holds k.mu.
func (k *keyboard) beginRequest(q *query) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	q.cancel, q.waiting, q.stopped = cancel, true, make(chan struct{})
	q.editor.submit()
	k.spin(q)
	return ctx
}

func (e *queryExecution) run() {
	defer close(e.done)
	emit := func(command string) error { return e.keyboard.revise(e.query, e.inject, command) }
	think := func(thought string) error { return e.keyboard.rethink(e.query, thought) }
	var err error
	if e.plan.route == routeAgent {
		err = e.keyboard.agent(
			e.ctx, e.plan.question, e.plan.transcript,
			emit, think,
			e.keyboard.runner(e.query, e.inject),
			e.keyboard.approver(e.query),
		)
	} else {
		err = e.keyboard.ask(
			e.ctx, e.plan.question, e.plan.transcript, e.plan.thinking,
			emit, think,
		)
	}
	e.query.cancel()

	if err != nil && !errors.Is(err, errTakenBack) && !errors.Is(err, errUntypable) {
		_ = e.keyboard.revise(e.query, e.inject, failedLine(err))
	}
	e.complete(err)
}

func (e *queryExecution) complete(err error) {
	e.once.Do(func() {
		k, q := e.keyboard, e.query
		k.mu.Lock()
		defer k.mu.Unlock()

		command := string(e.inject.typed)
		if q.editor.finished() {
			k.recordAnswer(q, command)
			return
		}
		autoAccept := false
		if err == nil {
			k.recordAnswer(q, command)
			autoAccept = e.plan.yolo && command != ""
		}
		if k.end() != nil {
			return
		}
		if autoAccept {
			_ = k.toShell([]byte{'\r'})
		}
	})
}

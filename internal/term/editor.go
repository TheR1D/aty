//go:build darwin || linux

package term

import "slices"

const (
	yoloKey      = '!'
	noContextKey = 'x'
	queryLimit   = 512
)

type queryMode struct {
	thinking  bool
	agent     bool
	yolo      bool
	noContext bool
}

type confirmPhase uint8

const (
	confirmOff confirmPhase = iota
	confirmLine
	confirmRun
	confirmTool
)

type editorStatus uint8

const (
	editorOpen editorStatus = iota
	editorSubmitted
	editorFinished
)

type queryEditor struct {
	text    []rune
	cursor  int
	partial []byte
	edited  bool
	mode    queryMode
	status  editorStatus
	confirm confirmPhase
}

type editorSnapshot struct {
	text    []rune
	cursor  int
	mode    queryMode
	status  editorStatus
	confirm confirmPhase
}

func (e *queryEditor) snapshot() editorSnapshot {
	return editorSnapshot{
		text: slices.Clone(e.text), cursor: e.cursor, mode: e.mode,
		status: e.status, confirm: e.confirm,
	}
}

func (e *queryEditor) untouched() bool {
	return !e.edited && len(e.partial) == 0 && e.status == editorOpen
}

func (e *queryEditor) markerWidth() int {
	return len(e.mode.marker())
}

func (m queryMode) marker() string {
	marker := "?"
	if m.thinking {
		marker += "?"
	}
	if m.agent {
		marker += "?"
	}
	if m.yolo {
		marker += "!"
	}
	if m.noContext {
		marker += "x"
	}
	return marker
}

// changeMode reports whether a prefix key changed the mode instead of adding text.
func (e *queryEditor) changeMode(key byte) bool {
	if !e.untouched() {
		return false
	}
	switch key {
	case triggerKey:
		if e.mode.yolo || e.mode.noContext {
			return false
		}
		if !e.mode.thinking {
			e.mode.thinking = true
			return true
		}
		if !e.mode.agent {
			e.mode.agent = true
			return true
		}
	case yoloKey:
		if !e.mode.yolo && !e.mode.noContext {
			e.mode.yolo = true
			return true
		}
	case noContextKey:
		if !e.mode.noContext {
			e.mode.noContext = true
			return true
		}
	}
	return false
}

func (e *queryEditor) feed(input []byte) (rest []byte, ending action) {
	if len(e.partial) > 0 {
		input = append(e.partial, input...)
		e.partial = nil
	}
	for len(input) > 0 {
		press, left, whole := decodeKey(input)
		if !whole {
			e.partial = append(e.partial, input...)
			break
		}
		input = left
		if e.status != editorOpen {
			if e.status == editorSubmitted && press.action == cancelQuery {
				return input, cancelQuery
			}
			continue
		}
		switch press.action {
		case insertChar:
			e.insert(press.char)
		case eraseChar:
			if e.cursor == 0 {
				return skipErases(input), cancelQuery
			}
			e.text = slices.Delete(e.text, e.cursor-1, e.cursor)
			e.cursor--
			e.edited = true
		case moveLeft:
			e.cursor = max(e.cursor-1, 0)
		case moveRight:
			e.cursor = min(e.cursor+1, len(e.text))
		case submitQuery, cancelQuery:
			return input, press.action
		}
	}
	return nil, nothing
}

func (e *queryEditor) insert(char rune) {
	if len(e.text) == queryLimit {
		return
	}
	e.text = slices.Insert(e.text, e.cursor, char)
	e.cursor++
	e.edited = true
}

func (e *queryEditor) submit()        { e.status = editorSubmitted }
func (e *queryEditor) finish()        { e.status = editorFinished }
func (e *queryEditor) finished() bool { return e.status == editorFinished }
func (e *queryEditor) question() string {
	return string(e.text)
}

func (e *queryEditor) startConfirmation() bool {
	e.confirm = confirmLine
	if e.mode.yolo {
		e.confirm = confirmRun
		return true
	}
	return false
}

func (e *queryEditor) clearConfirmation() { e.confirm = confirmOff }
func (e *queryEditor) confirming() bool   { return e.confirm != confirmOff }
func (e *queryEditor) confirmingLine() bool {
	return e.confirm == confirmLine
}

func (e *queryEditor) warmupMode() (thinking, agent, ok bool) {
	return e.mode.thinking, e.mode.agent, e.untouched() && !e.mode.noContext
}

func (e *queryEditor) agentMode() bool { return e.mode.agent }

type confirmationKey struct {
	bytes     []byte
	cancel    bool
	interrupt bool
	approve   bool
	move      int
}

func (e *queryEditor) nextConfirmation(input []byte) (confirmationKey, []byte, bool) {
	if len(e.partial) > 0 {
		input = append(e.partial, input...)
		e.partial = nil
	}
	press, left, whole := decodeKey(input)
	if !whole {
		e.partial = append(e.partial, input...)
		return confirmationKey{}, nil, false
	}
	key := input[:len(input)-len(left)]
	event := confirmationKey{bytes: key}
	event.cancel = press.action == cancelQuery && (e.confirm == confirmLine || e.confirm == confirmTool || key[0] == esc)
	event.interrupt = event.cancel && e.confirm == confirmLine && key[0] == ctrlC
	event.approve = press.action == submitQuery && e.confirm == confirmTool
	if e.confirm == confirmTool {
		switch press.action {
		case moveLeft:
			event.move = -1
		case moveRight:
			event.move = 1
		}
	}
	if press.action == submitQuery && e.confirm != confirmTool {
		e.confirm = confirmRun
	}
	return event, left, true
}

func skipErases(input []byte) []byte {
	for len(input) > 0 {
		press, left, whole := decodeKey(input)
		if !whole || press.action != eraseChar {
			return input
		}
		input = left
	}
	return nil
}

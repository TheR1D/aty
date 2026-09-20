//go:build darwin || linux

package term

import (
	"fmt"
	"slices"
	"time"
	"unicode/utf8"
)

const (
	saveCursor      = "\x1b7"
	restoreCursor   = "\x1b8"
	clearLine       = "\x1b[K"
	cursorBack      = "\x1b[%dD"
	sgrDim          = "\x1b[2m"
	sgrReset        = "\x1b[0m"
	spinnerWidth    = 2
	thoughtMaxWidth = 48
	thoughtLabel    = "Thinking: "
	approvalLabel   = "Approve: "
	confirmHint     = " confirm?"
)

var spinnerFrames = []rune{'|', '/', '-', '\\'}

const spinnerInterval = 120 * time.Millisecond

// queryView owns every piece of mutable presentation state. The query model
// owns text and request semantics; rendering methods consume both.
type queryView struct {
	screen *screen

	visible int
	offset  int

	anchored bool
	erased   bool

	waiting      bool
	frame        int
	thought      []rune
	thoughtOnly  bool
	approvalText []rune
	approvalAt   int
	stopped      chan struct{}

	hinted   bool
	hintDone bool
}

func visible(columns, marker int) int {
	if room := columns - promptWidth - marker - spinnerWidth; room > minQueryWidth {
		return room
	}
	return minQueryWidth
}

func (v *queryView) render(model editorSnapshot) error {
	if model.status == editorFinished || v.erased {
		return nil
	}
	label := thoughtLabel
	content := v.thought
	contentOnly := v.thoughtOnly
	approving := model.confirm == confirmTool
	if approving {
		label = approvalLabel
		content = v.approvalText
		contentOnly = true
	}
	width, showStatus := 0, false
	if v.waiting && contentOnly {
		width = max(min(thoughtMaxWidth, len(content), v.visible-1-len(label)), 0)
		showStatus = true
	} else if v.waiting && len(content) > 0 {
		width = max(min(thoughtMaxWidth, len(content), v.visible/2-len(label)), 0)
		showStatus = width > 0
	}
	room := v.visible
	if !contentOnly {
		if showStatus {
			room -= 1 + len(label) + width
		}
		v.reframe(model.text, model.cursor, room)
	}

	frame := make([]byte, 0, 64)
	if v.anchored {
		frame = append(frame, restoreCursor...)
	} else {
		frame = append(frame, saveCursor...)
		v.anchored = true
	}
	trailing := 0
	if !contentOnly {
		frame = append(frame, v.screen.textColor...)
		frame = append(frame, model.mode.marker()...)
		window := model.text[v.offset:min(v.offset+room, len(model.text))]
		for _, char := range window {
			frame = utf8.AppendRune(frame, char)
		}
		if v.screen.textColor != "" {
			frame = append(frame, sgrReset...)
		}
		trailing = v.offset + len(window) - model.cursor
	}
	if v.waiting {
		frame = append(frame, ' ')
		frame = utf8.AppendRune(frame, spinnerFrames[v.frame%len(spinnerFrames)])
		trailing += spinnerWidth
		if showStatus {
			frame = append(frame, ' ')
			frame = append(frame, sgrDim...)
			frame = append(frame, label...)
			shown := content
			if approving {
				start := min(max(v.approvalAt, 0), max(len(shown)-width, 0))
				shown = slices.Clone(shown[start:min(start+width, len(shown))])
				if start > 0 && len(shown) > 0 {
					shown[0] = '‹'
				}
				if start+width < len(content) && len(shown) > 0 {
					shown[len(shown)-1] = '›'
				}
			} else if extra := len(shown) - width; extra > 0 {
				shown = shown[extra:]
			}
			for _, char := range shown {
				frame = utf8.AppendRune(frame, char)
			}
			frame = append(frame, sgrReset...)
			trailing += 1 + len(label) + width
		}
	}
	if model.confirm == confirmTool {
		frame = append(frame, sgrDim...)
		frame = append(frame, confirmHint...)
		frame = append(frame, sgrReset...)
		trailing += len(confirmHint)
	}
	frame = append(frame, clearLine...)
	if trailing > 0 {
		frame = fmt.Appendf(frame, cursorBack, trailing)
	}
	if showStatus {
		frame = append(frame, sgrReset...)
	}
	_, err := v.screen.Write(frame)
	return err
}

func (v *queryView) reframe(text []rune, cursor, room int) {
	if cursor < v.offset {
		v.offset = cursor
	}
	if cursor > v.offset+room {
		v.offset = cursor - room
	}
	if end := len(text) - room; v.offset > end {
		v.offset = max(end, 0)
	}
}

func (v *queryView) erase() error {
	if v.erased {
		return nil
	}
	v.erased = true
	if !v.anchored {
		return nil
	}
	_, err := v.screen.Write([]byte(restoreCursor + clearLine + sgrReset))
	return err
}

func (v *queryView) paintConfirmHint(model editorSnapshot) error {
	if model.status == editorFinished || v.hintDone || v.hinted || model.confirm != confirmLine {
		return nil
	}
	frame := make([]byte, 0, len(sgrDim)+len(confirmHint)+len(sgrReset)+len(clearLine)+8)
	frame = append(frame, sgrDim...)
	frame = append(frame, confirmHint...)
	frame = append(frame, sgrReset...)
	frame = append(frame, clearLine...)
	frame = fmt.Appendf(frame, cursorBack, len(confirmHint))
	frame = append(frame, sgrReset...)
	if _, err := v.screen.Write(frame); err != nil {
		return err
	}
	v.hinted = true
	return nil
}

func (v *queryView) wipeConfirmHint() error {
	v.hintDone = true
	if !v.hinted {
		return nil
	}
	v.hinted = false
	_, err := v.screen.Write([]byte(clearLine + sgrReset))
	return err
}

func (k *keyboard) spin(q *query) {
	stopped := q.stopped
	go func() {
		ticker := time.NewTicker(spinnerInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopped:
				return
			case <-ticker.C:
				k.mu.Lock()
				q.frame++
				err := q.render(q.editor.snapshot())
				k.mu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()
}

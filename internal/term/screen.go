//go:build darwin || linux

package term

import (
	"fmt"
	"os"
	"sync"

	appconfig "github.com/TheR1D/aty/internal/config"
	"github.com/creack/pty"
)

// screen serializes shell output, query frames, and cursor updates so their
// bytes cannot interleave.
type screen struct {
	out *os.File
	// Immutable escape sequences resolved once for the whole session.
	textColor   string
	cursorColor string

	mu sync.Mutex
}

func newScreen(out *os.File, color appconfig.Color) *screen {
	if color == "" {
		color = appconfig.DefaultColor
	}
	s := &screen{out: out}
	if rgb, ok := color.RGB(); ok {
		s.textColor = fmt.Sprintf("\x1b[38;2;%d;%d;%dm", rgb>>16, (rgb>>8)&0xff, rgb&0xff)
		s.cursorColor = fmt.Sprintf("\x1b]12;#%06X\a", rgb)
	}
	return s
}

func (s *screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.out.Write(p)
}

// OSC 112 restores the terminal default without querying the input stream.
const cursorDefault = "\x1b]112\a"

// tintCursor is best effort for terminals that do not support OSC 12.
// Reapply at each prompt because programs may reset the cursor color.
func (s *screen) tintCursor() {
	if s.cursorColor != "" {
		_, _ = s.Write([]byte(s.cursorColor))
	}
}

// resetCursorColor restores the terminal default when the session ends.
func (s *screen) resetCursorColor() {
	if s.cursorColor != "" {
		_, _ = s.Write([]byte(cursorDefault))
	}
}

// columns bounds the overlay so it can be erased without crossing a line wrap.
func (s *screen) columns() int {
	size, err := pty.GetsizeFull(s.out)
	if err != nil || size.Cols == 0 {
		return defaultColumns
	}
	return int(size.Cols)
}

const (
	// Use a conventional width when the terminal cannot report one.
	defaultColumns = 80
	// Estimate prompt width instead of querying the cursor: replies share the
	// keyboard stream and some terminals never answer.
	promptWidth = 24
	// Keep the query usable on narrow terminals.
	minQueryWidth = 16
	// Each mode-marker character reduces the available query width.
	markerWidth = 1
	// Retain only the tail of the displayed reasoning.
	thoughtLimit = 240
)

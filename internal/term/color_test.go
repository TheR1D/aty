//go:build darwin || linux

package term

import (
	"context"
	"testing"

	appconfig "github.com/TheR1D/aty/internal/config"
	"github.com/TheR1D/aty/internal/llm"
)

func TestQueryColorStaysOnScreen(t *testing.T) {
	for _, test := range []struct {
		color appconfig.Color
		start string
		reset string
	}{
		{color: "", start: "\x1b[38;2;255;165;0m", reset: sgrReset},
		{color: "blue", start: "\x1b[38;2;100;149;237m", reset: sgrReset},
		{color: "none"},
	} {
		for _, marker := range []string{"?", "??", "???"} {
			t.Run(string(test.color)+"/"+marker, func(t *testing.T) {
				ty := newTyping(t, atEmptyPrompt(t))
				ty.keyboard.screen, ty.drawn = newDrawnOnWithColor(t, test.color)
				ty.keyboard.ask = func(_ context.Context, question string, _ []llm.Turn, _ bool, emit func(string) error, _ func(string) error) error {
					if question != "show cwd" {
						t.Errorf("model received %q, want plain query text", question)
					}
					return emit("pwd")
				}

				ty.press(t, "?")
				awaitDrawn(t, ty.drawn, exactly(saveCursor+test.start+"?"+test.reset+clearLine), "color at interception")
				ty.press(t, marker[1:]+"show cwd")
				awaitDrawn(t, ty.drawn, endingWith(restoreCursor+test.start+marker+"show cwd"+test.reset+clearLine), "colored query with reset before shell output")
				ty.press(t, "\r")
				awaitPending(t, ty, false, "query completion")
				if got := ty.shell.String(); got != "pwd" {
					t.Errorf("shell received %q, want an uncolored command", got)
				}
			})
		}
	}
}

func TestColoredQueryResetsOnCancel(t *testing.T) {
	for _, cancel := range []string{"\x1b", "\x03", "\x7f"} {
		t.Run(cancel, func(t *testing.T) {
			ty := newTyping(t, atEmptyPrompt(t))
			ty.keyboard.screen, ty.drawn = newDrawnOnWithColor(t, appconfig.DefaultColor)
			ty.press(t, "?"+cancel)
			awaitPending(t, ty, false, "query cancellation")
			awaitDrawn(t, ty.drawn, endingWith(restoreCursor+clearLine+sgrReset), "color reset on cancellation")
			if got := ty.shell.String(); got != "" {
				t.Errorf("cancelled query reached shell: %q", got)
			}
		})
	}
}

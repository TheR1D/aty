//go:build darwin || linux

package term

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	appconfig "github.com/TheR1D/aty/internal/config"
	"github.com/TheR1D/aty/internal/llm"
)

// TestQueryReadsWhatWasTypedAtIt covers the line editor on its own: what the
// keys do to the question, and where they leave the cursor in it. The runs are
// separate because a run is one read from the terminal, and a keystroke is free
// to arrive split across two of them.
func TestQueryReadsWhatWasTypedAtIt(t *testing.T) {
	tests := []struct {
		name   string
		runs   []string
		text   string
		cursor int
		ending action
	}{
		{name: "a question is text", runs: []string{"why did that fail"}, text: "why did that fail", cursor: 17},
		{name: "backspace takes the last character back", runs: []string{"ls -la", "\x7f\x7f"}, text: "ls -", cursor: 4},
		{name: "and so does the backspace some keyboards send", runs: []string{"ls", "\x08"}, text: "l", cursor: 1},
		{
			name: "the arrow keys move along the question",
			runs: []string{"git psh", "\x1b[D\x1b[D", "u"},
			text: "git push", cursor: 6,
		},
		{
			name: "and stop at either end of it",
			runs: []string{"go", "\x1b[D\x1b[D\x1b[D", "\x1b[C\x1b[C\x1b[C\x1b[C", "!"},
			text: "go!", cursor: 3,
		},
		{
			name: "a character split across two reads is still one character",
			runs: []string{"caf", "\xc3", "\xa9"},
			text: "café", cursor: 4,
		},
		{
			name: "the keys a shell's line editor answers to mean nothing here",
			runs: []string{"ls\t\x12\x1b[A\x1b[3~\x1bf"},
			text: "ls", cursor: 2,
		},
		{name: "Enter submits", runs: []string{"deploy\r"}, text: "deploy", cursor: 6, ending: submitQuery},
		{name: "and so does the newline a paste can carry", runs: []string{"deploy\n"}, text: "deploy", cursor: 6, ending: submitQuery},
		{name: "Esc cancels", runs: []string{"deploy\x1b"}, text: "deploy", cursor: 6, ending: cancelQuery},
		{name: "and so does the interrupt character", runs: []string{"deploy\x03"}, text: "deploy", cursor: 6, ending: cancelQuery},
		{
			name:   "backspacing off the front of an empty question abandons it",
			runs:   []string{"\x7f"},
			ending: cancelQuery,
		},
		{
			name:   "and so does backspacing a written question away",
			runs:   []string{"hi", "\x7f\x7f\x7f"},
			ending: cancelQuery,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			q, _ := newQuery(t)

			var ending action
			for _, run := range test.runs {
				rest, reached, err := q.feed([]byte(run))
				if err != nil {
					t.Fatalf("typing %q at the query: %v", run, err)
				}
				if ending = reached; ending != nothing {
					if len(rest) != 0 {
						t.Errorf("typing %q ended the query with %q left over, want nothing after it", run, rest)
					}
					break
				}
			}

			if ending != test.ending {
				t.Errorf("typing %v ended the query as %d, want %d", test.runs, ending, test.ending)
			}
			if got := string(q.editor.text); got != test.text {
				t.Errorf("typing %v left the question as %q, want %q", test.runs, got, test.text)
			}
			if q.editor.cursor != test.cursor {
				t.Errorf("typing %v left the cursor at %d, want %d", test.runs, q.editor.cursor, test.cursor)
			}
		})
	}
}

// TestQueryStopsAtTheKeyThatEndsIt covers the boundary between the query and
// the shell: whatever was typed after the key that ended a query is the shell's
// again, which matters because a paste can hold both.
func TestQueryStopsAtTheKeyThatEndsIt(t *testing.T) {
	q, _ := newQuery(t)

	rest, ending, err := q.feed([]byte("why did that fail\rls -la\r"))
	if err != nil {
		t.Fatalf("typing at the query: %v", err)
	}
	if ending != submitQuery {
		t.Fatalf("a question with Enter in the middle of it ended as %d, want submitted", ending)
	}
	if got, want := string(rest), "ls -la\r"; got != want {
		t.Errorf("the query kept %q of what followed Enter, want %q left for the shell", got, want)
	}
}

// TestQueryKeepsExtraBackspacesWhenItIsAbandoned covers a held-down key: the
// erase that takes the trigger off ends the query, and the ones behind it are
// not handed to the shell, because they were meant for the question that just
// vanished.
func TestQueryKeepsExtraBackspacesWhenItIsAbandoned(t *testing.T) {
	q, _ := newQuery(t)

	rest, ending, err := q.feed([]byte("hi\x7f\x7f\x7f\x7f?"))
	if err != nil {
		t.Fatalf("typing at the query: %v", err)
	}
	if ending != cancelQuery {
		t.Fatalf("backspacing a question away ended as %d, want cancelled", ending)
	}
	if got, want := string(rest), "?"; got != want {
		t.Errorf("the query left %q after it was abandoned, want the %q that followed the extra erases", got, want)
	}
}

// TestQueryIsDrawnWhereThePromptLeftTheCursor is the drawing check. Every frame
// is one write, so a frame is asserted whole: the position the query is anchored
// to, the question, and where the cursor ends up in it.
func TestQueryIsDrawnWhereThePromptLeftTheCursor(t *testing.T) {
	q, drawn := newQuery(t)

	feed(t, q, "git psh")
	// The first frame saves the cursor position the shell's prompt left, since
	// every frame after it, and the erase, go back to that spot.
	awaitDrawn(t, drawn, exactly(saveCursor+"?git psh"+clearLine), "the first frame of a question")

	feed(t, q, "\x1b[D\x1b[Du")
	awaitDrawn(t, drawn, exactly(saveCursor+"?git psh"+clearLine+
		restoreCursor+"?git push"+clearLine+"\x1b[2D"),
		"a question edited in the middle")

	if err := q.erase(); err != nil {
		t.Fatalf("erasing the query: %v", err)
	}
	awaitDrawn(t, drawn, endingWith(restoreCursor+clearLine+sgrReset),
		"the erase that takes a question off the screen")
}

// TestQueryScrollsRatherThanWrapping covers the question too long for the line
// it is drawn on. Wrapping would move the cursor position the erase goes back
// to, so the window scrolls over the question instead and what is drawn stays
// within the line.
func TestQueryScrollsRatherThanWrapping(t *testing.T) {
	q, drawn := newQuery(t)

	// A pipe reports no width, so the query is drawn for the width a terminal
	// that says nothing is assumed to have.
	window := visible(defaultColumns, markerWidth)
	long := strings.Repeat("x", window*2)
	feed(t, q, long)

	awaitDrawn(t, drawn, exactly(saveCursor+"?"+strings.Repeat("x", window)+clearLine),
		"the tail of a question longer than the line")

	if q.offset != len(long)-window {
		t.Errorf("the window over a question of %d characters starts at %d, want %d",
			len(long), q.offset, len(long)-window)
	}
	if len(q.editor.text) != len(long) {
		t.Errorf("the question is %d characters, want the %d that were typed", len(q.editor.text), len(long))
	}
}

// TestQueryIsBounded covers a key held down, or a paste of something that was
// never a question: the buffer stops growing and the session carries on.
func TestQueryIsBounded(t *testing.T) {
	q, _ := newQuery(t)

	feed(t, q, strings.Repeat("x", queryLimit+100))
	if len(q.editor.text) != queryLimit {
		t.Errorf("a question typed past the limit is %d characters, want %d", len(q.editor.text), queryLimit)
	}
}

// TestTriggerAtAnEmptyPromptIsAtysAndNotTheShells is the interception check.
// The shell has to be told nothing at all: not the trigger, not the question,
// and not the Enter that submits it.
func TestTriggerAtAnEmptyPromptIsAtysAndNotTheShells(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))

	// The trigger is drawn the moment it is taken, so that pressing it looks
	// like typing rather than like a keystroke that went nowhere.
	ty.press(t, "?")
	awaitDrawn(t, ty.drawn, exactly(saveCursor+"?"+clearLine), "the trigger aty took")

	ty.press(t, "deploy the staging branch")
	awaitDrawn(t, ty.drawn, exactly(saveCursor+"?"+clearLine+
		restoreCursor+"?deploy the staging branch"+clearLine),
		"the question drawn where the prompt left the cursor")
	if got := ty.shell.String(); got != "" {
		t.Errorf("the shell was sent %q while the question was being typed, want nothing", got)
	}

	// Enter is the one key aty never passes on: the user is the only one who
	// runs anything.
	ty.press(t, "\r")
	if got := ty.shell.String(); got != "" {
		t.Errorf("the shell was sent %q when the question was submitted, want nothing", got)
	}
	if ty.pending() != nil {
		t.Error("a submitted question with nothing to answer it still has the keyboard")
	}
}

func TestSpaceAfterTriggerPrewarmsTheCurrentTranscriptWithoutBlockingTyping(t *testing.T) {
	s := atEmptyPrompt(t)
	s.observeOutput([]byte("$ " + pasteOn + "git status" + pasteOff + "\nclean\n$ " + pasteOn))
	ty := newTyping(t, s)

	type call struct {
		transcript []llm.Turn
		thinking   bool
		agent      bool
	}
	warmed := make(chan call, 1)
	release := make(chan struct{})
	ty.keyboard.warm = func(_ context.Context, transcript []llm.Turn, thinking, agent bool) error {
		warmed <- call{transcript: transcript, thinking: thinking, agent: agent}
		<-release
		return nil
	}

	ty.press(t, "?")
	select {
	case <-warmed:
		t.Fatal("the trigger alone started a cache warmup before the required space")
	case <-time.After(20 * time.Millisecond):
	}

	ty.press(t, " ")
	// A blocked provider warmup does not hold the keyboard lock.
	ty.press(t, "what changed")
	if q := ty.pending(); q == nil || string(q.editor.text) != " what changed" {
		t.Fatalf("typing during warmup left query %#v, want the full question", q)
	}

	select {
	case got := <-warmed:
		if got.thinking || got.agent {
			t.Errorf("the trigger-space warmed thinking=%t agent=%t, want normal mode", got.thinking, got.agent)
		}
		if !turnsContain(got.transcript, "clean") {
			t.Errorf("the warmup omitted the current transcript: %+v", got.transcript)
		}
	case <-time.After(answerTimeout):
		t.Fatal("the space after the trigger never started a cache warmup")
	}
	close(release)
}

func TestInitialPromptIsBoundOnceBeforePrewarm(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))
	bound := make(chan string, 2)
	ty.keyboard.initialPrompt = func(ps1 string) { bound <- ps1 }
	warmed := make(chan bool, 1)
	ty.keyboard.warm = func(context.Context, []llm.Turn, bool, bool) error {
		select {
		case <-bound:
			warmed <- true
		default:
			warmed <- false
		}
		return nil
	}
	ty.press(t, "? ")
	select {
	case ok := <-warmed:
		if !ok {
			t.Fatal("prewarm ran before the startup prompt was bound")
		}
	case <-time.After(answerTimeout):
		t.Fatal("prewarm did not start")
	}
	ty.press(t, "\x1b")
	ty.press(t, "?")
	select {
	case <-bound:
		t.Fatal("startup prompt was rebound on a later query")
	default:
	}
	ty.press(t, "\x1b")
}

func TestNoContextMarkerDoesNotPrewarmTheTranscript(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))
	warmed := make(chan struct{}, 1)
	ty.keyboard.warm = func(context.Context, []llm.Turn, bool, bool) error {
		warmed <- struct{}{}
		return nil
	}

	ty.press(t, "?x ")
	select {
	case <-warmed:
		t.Fatal("a no-context question started a cache warmup with the transcript")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestSubmitCancelsWarmupAndAsksWithoutWaiting(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))
	warming := make(chan context.Context, 1)
	ty.keyboard.warm = func(ctx context.Context, _ []llm.Turn, _, _ bool) error {
		warming <- ctx
		<-ctx.Done()
		return ctx.Err()
	}
	asked := make(chan string, 1)
	ty.keyboard.ask = func(_ context.Context, question string, _ []llm.Turn, _ bool, _ func(string) error, _ func(string) error) error {
		asked <- question
		return nil
	}

	ty.press(t, "? ")
	warmCtx := <-warming
	ty.press(t, "list files\r")

	select {
	case <-warmCtx.Done():
	case <-time.After(answerTimeout):
		t.Fatal("submitting did not cancel the cache request")
	}
	select {
	case got := <-asked:
		if got != " list files" {
			t.Errorf("submitted question = %q, want %q", got, " list files")
		}
	case <-time.After(answerTimeout):
		t.Fatal("the real request waited for the cancelled cache request")
	}
}

// TestQueryIsErasedWhenItIsCancelled covers what a cancelled question leaves
// behind, which has to be nothing: the shell's prompt is exactly as it drew it,
// and the next keystroke is the shell's again.
func TestQueryIsErasedWhenItIsCancelled(t *testing.T) {
	for name, key := range map[string]string{"Esc": "\x1b", "the interrupt character": "\x03"} {
		t.Run(name, func(t *testing.T) {
			ty := newTyping(t, atEmptyPrompt(t))

			ty.press(t, "?why did that fail")
			ty.press(t, key)
			awaitDrawn(t, ty.drawn, exactly(saveCursor+"?"+clearLine+
				restoreCursor+"?why did that fail"+clearLine+
				restoreCursor+clearLine+sgrReset),
				"a question that was cancelled")
			if got := ty.shell.String(); got != "" {
				t.Errorf("the shell was sent %q by a cancelled question, want nothing", got)
			}

			ty.press(t, "ls")
			if got := ty.shell.String(); got != "ls" {
				t.Errorf("after a cancelled question the shell was sent %q, want the typing that followed it", got)
			}
		})
	}
}

// TestQueryClearedWithBackspaceLeavesTheNextTriggerAtys is the case of typing
// a question, holding backspace until the line is gone, and asking again. The
// extra erases must not reach the shell: that would make the empty prompt look
// typed-in, and the next trigger would be forwarded instead of taken.
func TestQueryClearedWithBackspaceLeavesTheNextTriggerAtys(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))

	asked := make(chan string, 1)
	ty.keyboard.ask = func(_ context.Context, question string, _ []llm.Turn, _ bool, typing func(string) error, _ func(string) error) error {
		asked <- question
		return typing("git branch --show-current")
	}

	ty.press(t, "?what is current active branch"+strings.Repeat("\x7f", 40))
	if ty.pending() != nil {
		t.Fatal("backspacing the question away left a query open")
	}
	if got := ty.shell.String(); got != "" {
		t.Errorf("the shell was sent %q by a question that was backspaced away, want nothing", got)
	}
	if !ty.keyboard.state.gate().freshLine {
		t.Fatal("backspacing a question away left the shell's line looking typed-in")
	}

	// A backspace of its own after the query is gone is the same held-down
	// key arriving in the next read, and must not shut the next trigger either.
	ty.press(t, "\x7f\x7f")
	if !ty.keyboard.state.gate().freshLine {
		t.Fatal("a backspace at the empty prompt left the line looking typed-in")
	}

	ty.press(t, "?what branch\r")
	select {
	case got := <-asked:
		if got != "what branch" {
			t.Errorf("the next question was %q, want %q", got, "what branch")
		}
	case <-time.After(answerTimeout):
		t.Fatal("the next trigger after a backspaced question was not taken")
	}
}

// TestAnswerClearedAwayLeavesTheNextTriggerAtys is the case of asking,
// rethinking the command that came back, and asking again: the answer was
// typed into the shell's line editor and the user backspaced all of it away,
// so the line is exactly as empty as the shell left it and the next trigger
// is aty's. Forwarding it is how the question lands at the shell as a glob.
func TestAnswerClearedAwayLeavesTheNextTriggerAtys(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))

	asked := make(chan string, 1)
	ty.keyboard.ask = func(_ context.Context, question string, _ []llm.Turn, _ bool, typing func(string) error, _ func(string) error) error {
		asked <- question
		return typing("git status")
	}

	ty.press(t, "?show the status\r")
	if got, want := <-asked, "show the status"; got != want {
		t.Fatalf("the question asked was %q, want %q", got, want)
	}
	awaitShell(t, ty, "git status")
	awaitPending(t, ty, false, "the question was never answered")

	// The command the model wrote is cleared with backspace, every character
	// of it, and the erases that overshoot an already-empty line.
	ty.press(t, strings.Repeat("\x7f", len("git status")+2))
	if !ty.keyboard.state.gate().freshLine {
		t.Fatal("a command backspaced away left the shell's line looking typed-in")
	}

	ty.press(t, "?what changed\r")
	select {
	case got := <-asked:
		if got != "what changed" {
			t.Errorf("the next question was %q, want %q", got, "what changed")
		}
	case <-time.After(answerTimeout):
		t.Error("the trigger after a cleared-away answer was handed to the shell")
	}
}

// TestAnAnswerClearedAwayIsInTheNextQuestionsTranscript is the follow-up half
// of clearing an answer away: the command the model wrote never ran, but the
// next question can be about it — "what does that command do" — so the
// transcript the question is asked with keeps the answer as the assistant's
// own turn.
func TestAnAnswerClearedAwayIsInTheNextQuestionsTranscript(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))

	asked := make(chan []llm.Turn, 1)
	ty.keyboard.ask = func(_ context.Context, _ string, transcript []llm.Turn, _ bool, typing func(string) error, _ func(string) error) error {
		asked <- transcript
		return typing("docker image ls")
	}

	ty.press(t, "?show docker images\r")
	<-asked
	awaitShell(t, ty, "docker image ls")
	awaitPending(t, ty, false, "the question was never answered")

	// The command the model wrote is cleared with backspace, every
	// character of it.
	ty.press(t, strings.Repeat("\x7f", len("docker image ls")))
	if !ty.keyboard.state.gate().freshLine {
		t.Fatal("an answer backspaced away left the shell's line looking typed-in")
	}

	ty.press(t, "?what does that command do\r")
	select {
	case transcript := <-asked:
		if len(transcript) != 1 {
			t.Fatalf("the follow-up was asked with %d turns, want the answer that never ran: %+v", len(transcript), transcript)
		}
		if transcript[0].Question != "show docker images" || transcript[0].Answer != "docker image ls" || transcript[0].Text != "" {
			t.Errorf("the follow-up's transcript does not keep the cleared-away question and answer as their own turn: %+v", transcript[0])
		}
	case <-time.After(answerTimeout):
		t.Fatal("the follow-up question was never asked")
	}
}

// TestTriggerAfterErasingAQuestionIsPartOfTheQuestion covers clearing the
// text but leaving the trigger: that query is no longer the one that just
// opened, so a ? typed there is a character, not the escape that types one
// at the shell.
func TestTriggerAfterErasingAQuestionIsPartOfTheQuestion(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))

	ty.press(t, "?hello")
	ty.press(t, strings.Repeat("\x7f", len("hello")))
	if ty.pending() == nil {
		t.Fatal("erasing the question abandoned the query, want it still open")
	}

	ty.press(t, "?again")
	if got := ty.shell.String(); got != "" {
		t.Errorf("a trigger typed after the question was erased was sent to the shell as %q", got)
	}
	q := ty.pending()
	if q == nil {
		t.Fatal("a trigger typed after the question was erased closed the query")
	}
	if got := string(q.editor.text); got != "?again" {
		t.Errorf("the question is %q, want the trigger kept as text", got)
	}
}

// TestDoubledTriggerAsksAThinkingQuestion covers the second trigger: the
// marker doubles, and the question it heads is one the model thinks through
// before answering, which is what the ask is told.
func TestDoubledTriggerAsksAThinkingQuestion(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))

	thought := make(chan bool, 1)
	ty.keyboard.ask = func(_ context.Context, _ string, _ []llm.Turn, thinking bool, typing func(string) error, _ func(string) error) error {
		thought <- thinking
		return typing("git push --force-with-lease")
	}

	ty.press(t, "??")
	if got := ty.shell.String(); got != "" {
		t.Errorf("a doubled trigger sent the shell %q, want nothing: the question is aty's", got)
	}
	q := ty.pending()
	if q == nil {
		t.Fatal("a doubled trigger closed the query, want it open for the question")
	}
	if !q.editor.mode.thinking {
		t.Error("a doubled trigger left the query not thinking")
	}
	awaitDrawn(t, ty.drawn, exactly(saveCursor+"?"+clearLine+restoreCursor+"??"+clearLine),
		"the marker doubling for a thinking question")

	ty.press(t, "push the branch safely\r")
	if !<-thought {
		t.Error("a doubled-trigger question was asked without thinking")
	}
	awaitShell(t, ty, "git push --force-with-lease")
}

func TestBangAfterModeMarkerEnablesYolo(t *testing.T) {
	tests := []struct {
		marker   string
		thinking bool
		agent    bool
	}{
		{marker: "?!"},
		{marker: "??!", thinking: true},
		{marker: "???!", thinking: true, agent: true},
	}

	for _, test := range tests {
		t.Run(test.marker, func(t *testing.T) {
			ty := newTyping(t, atEmptyPrompt(t))
			ty.press(t, test.marker)

			q := ty.pending()
			if q == nil {
				t.Fatal("the yolo marker closed the query")
			}
			if !q.editor.mode.yolo || q.editor.mode.thinking != test.thinking || q.editor.mode.agent != test.agent {
				t.Errorf("marker %q left yolo=%t thinking=%t agent=%t", test.marker, q.editor.mode.yolo, q.editor.mode.thinking, q.editor.mode.agent)
			}
			if got := string(q.editor.text); got != "" {
				t.Errorf("marker %q became question text %q", test.marker, got)
			}
			if q.visible != visible(ty.keyboard.screen.columns(), len(test.marker)) {
				t.Errorf("marker %q sized the query window to %d, want %d", test.marker, q.visible, visible(ty.keyboard.screen.columns(), len(test.marker)))
			}
			awaitDrawn(t, ty.drawn, endingWith(restoreCursor+test.marker+clearLine), "the complete yolo marker")
			ty.press(t, "\x1b")
		})
	}
}

func TestXAfterModeMarkerDisablesContext(t *testing.T) {
	tests := []struct {
		marker   string
		thinking bool
		agent    bool
		yolo     bool
	}{
		{marker: "?x"},
		{marker: "??x", thinking: true},
		{marker: "???x", thinking: true, agent: true},
		{marker: "?!x", yolo: true},
		{marker: "??!x", thinking: true, yolo: true},
		{marker: "???!x", thinking: true, agent: true, yolo: true},
	}

	for _, test := range tests {
		t.Run(test.marker, func(t *testing.T) {
			ty := newTyping(t, atEmptyPrompt(t))
			ty.press(t, test.marker)

			q := ty.pending()
			if q == nil {
				t.Fatal("the no-context marker closed the query")
			}
			if !q.editor.mode.noContext || q.editor.mode.thinking != test.thinking || q.editor.mode.agent != test.agent || q.editor.mode.yolo != test.yolo {
				t.Errorf("marker %q left withoutContext=%t yolo=%t thinking=%t agent=%t",
					test.marker, q.editor.mode.noContext, q.editor.mode.yolo, q.editor.mode.thinking, q.editor.mode.agent)
			}
			if got := string(q.editor.text); got != "" {
				t.Errorf("marker %q became question text %q", test.marker, got)
			}
			if q.visible != visible(ty.keyboard.screen.columns(), len(test.marker)) {
				t.Errorf("marker %q sized the query window to %d, want %d", test.marker, q.visible, visible(ty.keyboard.screen.columns(), len(test.marker)))
			}
			awaitDrawn(t, ty.drawn, endingWith(restoreCursor+test.marker+clearLine), "the complete no-context marker")
			ty.press(t, "\x1b")
		})
	}
}

func TestBangAfterQuestionTextIsOrdinaryText(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))
	ty.press(t, "?explain this!")

	q := ty.pending()
	if q == nil {
		t.Fatal("typing a bang in the question closed it")
	}
	if q.editor.mode.yolo {
		t.Error("a bang after question text enabled yolo mode")
	}
	if got := string(q.editor.text); got != "explain this!" {
		t.Errorf("the question is %q, want its bang kept as text", got)
	}
}

func TestYoloAcceptsPlainAndThinkingAnswersAfterStreaming(t *testing.T) {
	for _, test := range []struct {
		marker   string
		thinking bool
	}{
		{marker: "?!"},
		{marker: "??!", thinking: true},
	} {
		t.Run(test.marker, func(t *testing.T) {
			ty := newTyping(t, atEmptyPrompt(t))
			streamed := make(chan error, 1)
			finish := make(chan struct{})
			ty.keyboard.ask = func(_ context.Context, question string, _ []llm.Turn, thinking bool, typing func(string) error, _ func(string) error) error {
				if question != "run it" {
					t.Errorf("the model was asked %q, want the yolo marker removed", question)
				}
				if thinking != test.thinking {
					t.Errorf("marker %q asked with thinking=%t", test.marker, thinking)
				}
				err := typing("echo YOLO")
				streamed <- err
				<-finish
				return err
			}

			ty.press(t, test.marker+"run it\r")
			if err := <-streamed; err != nil {
				t.Fatalf("streaming the command: %v", err)
			}
			awaitShell(t, ty, "echo YOLO")
			close(finish)

			awaitShell(t, ty, "echo YOLO\r")
			awaitPending(t, ty, false, "the yolo query did not finish after accepting its answer")
		})
	}
}

func TestSpaceAfterFinalMarkerStartsTheMatchingWarmup(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))
	type warm struct {
		ctx      context.Context
		thinking bool
		agent    bool
	}
	started := make(chan warm, 1)
	ty.keyboard.warm = func(ctx context.Context, _ []llm.Turn, thinking, agent bool) error {
		started <- warm{ctx: ctx, thinking: thinking, agent: agent}
		<-ctx.Done()
		return ctx.Err()
	}

	ty.press(t, "???")
	select {
	case got := <-started:
		t.Fatalf("the marker alone started thinking=%t agent=%t warmup before its space", got.thinking, got.agent)
	case <-time.After(20 * time.Millisecond):
	}

	ty.press(t, " ")
	agent := <-started
	if !agent.thinking || !agent.agent {
		t.Fatalf("agent trigger-space warmed thinking=%t agent=%t, want both true", agent.thinking, agent.agent)
	}

	ty.press(t, "\x1b")
	select {
	case <-agent.ctx.Done():
	case <-time.After(answerTimeout):
		t.Fatal("cancelling the query did not cancel its agent warmup")
	}
}

// TestPlainTriggerAsksWithoutThinking is the other half: one trigger is a
// plain question, which a server launched with reasoning enabled is told to
// answer without the thought.
func TestPlainTriggerAsksWithoutThinking(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))

	thought := make(chan bool, 1)
	ty.keyboard.ask = func(_ context.Context, _ string, _ []llm.Turn, thinking bool, _ func(string) error, _ func(string) error) error {
		thought <- thinking
		return nil
	}

	ty.press(t, "?list the files\r")
	if <-thought {
		t.Error("a plain question was asked with thinking")
	}
}

// TestTripledTriggerAsksAnAgentQuestion covers the third trigger: the
// marker widens again, thinking stays on, and the question is handed to
// the agent rather than asked as a plain one.
func TestTripledTriggerAsksAnAgentQuestion(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))

	asked := make(chan struct{}, 1)
	agented := make(chan string, 1)
	ty.keyboard.ask = func(_ context.Context, _ string, _ []llm.Turn, _ bool, _ func(string) error, _ func(string) error) error {
		asked <- struct{}{}
		return nil
	}
	ty.keyboard.agent = func(_ context.Context, question string, _ []llm.Turn, _ func(string) error, _ func(string) error, _ func(context.Context, string) (string, error), _ llm.ApproveTool) error {
		agented <- question
		return nil
	}

	ty.press(t, "???")
	if got := ty.shell.String(); got != "" {
		t.Errorf("a tripled trigger sent the shell %q, want nothing: the question is aty's", got)
	}
	q := ty.pending()
	if q == nil {
		t.Fatal("a tripled trigger closed the query, want it open for the question")
	}
	if !q.editor.mode.thinking || !q.editor.mode.agent {
		t.Errorf("a tripled trigger left thinking=%t agent=%t, want both set", q.editor.mode.thinking, q.editor.mode.agent)
	}
	if q.visible != visible(ty.keyboard.screen.columns(), 3*markerWidth) {
		t.Errorf("an agent query's window is %d, want %d for a marker three wide",
			q.visible, visible(ty.keyboard.screen.columns(), 3*markerWidth))
	}
	awaitDrawn(t, ty.drawn, exactly(saveCursor+"?"+clearLine+restoreCursor+"??"+clearLine+restoreCursor+"???"+clearLine),
		"the marker tripling for an agent question")

	ty.press(t, "look around\r")
	select {
	case got := <-agented:
		if got != "look around" {
			t.Errorf("the agent was asked %q, want %q", got, "look around")
		}
	case <-asked:
		t.Error("a tripled trigger was asked as a plain question")
	case <-time.After(answerTimeout):
		t.Fatal("a tripled trigger was never handed to the agent")
	}
}

// TestTripledTriggerWithoutAnAgentThinksInstead is the nil-safe half: a
// session that has no agent still accepts `???`, and the question is
// asked as a thinking one.
func TestTripledTriggerWithoutAnAgentThinksInstead(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))

	thought := make(chan bool, 1)
	ty.keyboard.ask = func(_ context.Context, _ string, _ []llm.Turn, thinking bool, _ func(string) error, _ func(string) error) error {
		thought <- thinking
		return nil
	}

	ty.press(t, "???look around\r")
	if !<-thought {
		t.Error("a tripled trigger with no agent was asked without thinking")
	}
}

// TestFourthTriggerIsPartOfTheQuestion covers the trigger past agent: the
// marker is already three wide, so another ? is a character of the
// question rather than a fourth mode.
func TestFourthTriggerIsPartOfTheQuestion(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))

	ty.press(t, "????")
	if got := ty.shell.String(); got != "" {
		t.Errorf("a fourth trigger sent the shell %q, want nothing: it is still aty's question", got)
	}
	q := ty.pending()
	if q == nil {
		t.Fatal("a fourth trigger closed the query, want it open with the extra ? as text")
	}
	if !q.editor.mode.thinking || !q.editor.mode.agent {
		t.Errorf("a fourth trigger left thinking=%t agent=%t, want both still set", q.editor.mode.thinking, q.editor.mode.agent)
	}
	if got := string(q.editor.text); got != "?" {
		t.Errorf("the question is %q, want the fourth trigger kept as text", got)
	}
}

// TestAgentFinalAnswerIsTypedWithoutEnter is the other half of the runner:
// a tool command is typed and left for the user to confirm, but the line
// the model finishes on is typed into the editor and left there, the same
// way a plain answer is.
func TestAgentFinalAnswerIsTypedWithoutEnter(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))

	typed := make(chan error, 1)
	ty.keyboard.agent = func(_ context.Context, _ string, _ []llm.Turn, typing func(string) error, _ func(string) error, _ func(context.Context, string) (string, error), _ llm.ApproveTool) error {
		err := typing("ls -la /tmp")
		typed <- err
		return err
	}

	ty.press(t, "???what is in tmp\r")
	if err := <-typed; err != nil {
		t.Fatalf("typing the answer at the shell: %v", err)
	}
	awaitShell(t, ty, "ls -la /tmp")
	if strings.Contains(ty.shell.String(), "\r") {
		t.Errorf("the final answer was handed to the shell with Enter: %q", ty.shell.String())
	}
	awaitPending(t, ty, false, "the keyboard never went back to the shell after the question was answered")
}

func TestYoloAgentFinalAnswerIsAccepted(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))
	ty.keyboard.agent = func(_ context.Context, _ string, _ []llm.Turn, typing func(string) error, _ func(string) error, _ func(context.Context, string) (string, error), _ llm.ApproveTool) error {
		return typing("ls -la /tmp")
	}

	ty.press(t, "???!what is in tmp\r")
	awaitShell(t, ty, "ls -la /tmp\r")
	awaitPending(t, ty, false, "the yolo agent query did not finish after accepting its final answer")
}

// TestAgentRunnerInjectsTheCommandAndReturnsTheTurn is the tool's other
// end: the command is typed at the real prompt and left for the user to
// confirm. Enter is theirs; what it printed is the result the model reads
// next.
func TestAgentRunnerInjectsTheCommandAndReturnsTheTurn(t *testing.T) {
	ty := typingOnShell(t)
	q := &query{screen: ty.keyboard.screen}
	ty.keyboard.query = q
	inject := &injection{}

	type outcome struct {
		result string
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := ty.keyboard.runner(q, inject)(t.Context(), "echo AGENT_MARK=ok")
		done <- outcome{result, err}
	}()

	awaitShell(t, ty, "echo AGENT_MARK=ok")
	if strings.Contains(ty.shell.String(), "\r") {
		t.Errorf("the runner pressed Enter before the user did: %q", ty.shell.String())
	}
	awaitConfirm(t, ty, confirmLine, "the runner never opened the line for confirm")
	ty.press(t, "\r")

	var out outcome
	select {
	case out = <-done:
	case <-time.After(answerTimeout):
		t.Fatal("the runner never returned after Enter")
	}
	if out.err != nil {
		t.Fatalf("running the command: %v", out.err)
	}
	if got, want := ty.shell.String(), "echo AGENT_MARK=ok\r"; got != want {
		t.Errorf("the shell was sent %q, want %q", got, want)
	}
	if !q.erased {
		t.Error("the query line was still on the screen after the command ran")
	}
	if !strings.Contains(out.result, "AGENT_MARK=ok") {
		t.Errorf("the tool result is %q, want the command's output", out.result)
	}
	if inject.typed != nil {
		t.Errorf("inject.typed is %q after the command ran, want it forgotten so the next command does not backspace a line that is gone", string(inject.typed))
	}

	drawn := ty.drawn.String()
	if err := q.render(q.editor.snapshot()); err != nil {
		t.Fatalf("drawing after the command: %v", err)
	}
	if got := ty.drawn.String(); got != drawn {
		t.Errorf("the query was redrawn after the command ran, want the line left off: %q", got[min(len(drawn), len(got)):])
	}
}

func TestAgentRunnerReturnsAfterTranscriptEviction(t *testing.T) {
	ty := typingOnShell(t)
	q := &query{screen: ty.keyboard.screen}
	ty.keyboard.query = q
	inject := &injection{}

	capture := ty.keyboard.state.capture
	capture.mu.Lock()
	capture.history.maxBytes = 70
	capture.history.keepBytes = 35
	for _, entry := range []struct{ command, line string }{{"one", "A"}, {"two", "B"}} {
		ch := chunk{command: entry.command}
		ch.addLine(strings.Repeat(entry.line, 30))
		capture.history.append(ch)
	}
	before := len(capture.history.entries)
	capture.mu.Unlock()

	type outcome struct {
		result string
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := ty.keyboard.runner(q, inject)(t.Context(), "echo AGENT_EVICTION=ok")
		done <- outcome{result, err}
	}()

	awaitShell(t, ty, "echo AGENT_EVICTION=ok")
	awaitConfirm(t, ty, confirmLine, "the runner never opened the line for confirm")
	ty.press(t, "\r")

	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("running the command after transcript eviction: %v", out.err)
		}
		if !strings.Contains(out.result, "AGENT_EVICTION=ok") {
			t.Errorf("the tool result is %q, want the newly completed command", out.result)
		}
	case <-time.After(answerTimeout):
		t.Fatal("the runner mistook transcript eviction for an unfinished command")
	}

	after := len(ty.keyboard.state.transcript())
	if after >= before {
		t.Fatalf("transcript entries after command = %d, want fewer than %d to exercise eviction", after, before)
	}
}

func TestYoloAgentRunnerAcceptsToolCommand(t *testing.T) {
	ty := typingOnShell(t)
	q := &query{screen: ty.keyboard.screen, editor: queryEditor{mode: queryMode{yolo: true}}}
	ty.keyboard.query = q
	inject := &injection{}

	type outcome struct {
		result string
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := ty.keyboard.runner(q, inject)(t.Context(), "echo YOLO_AGENT=ok")
		done <- outcome{result, err}
	}()

	awaitShell(t, ty, "echo YOLO_AGENT=ok\r")
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("running the yolo tool command: %v", out.err)
		}
		if !strings.Contains(out.result, "YOLO_AGENT=ok") {
			t.Errorf("the tool result is %q, want the command's output", out.result)
		}
	case <-time.After(answerTimeout):
		t.Fatal("the yolo runner waited for the user's Enter")
	}
}

func TestMCPToolWaitsForLocalApprovalWithoutReachingTheShell(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))
	approved := make(chan error, 1)
	ty.keyboard.agent = func(
		ctx context.Context,
		_ string,
		_ []llm.Turn,
		_ func(string) error,
		_ func(string) error,
		_ func(context.Context, string) (string, error),
		approve llm.ApproveTool,
	) error {
		err := approve(ctx, "mcp_github__create_issue", json.RawMessage(`{"padding":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","final":"VISIBLE_END"}`))
		approved <- err
		return err
	}

	ty.press(t, "???create the issue\r")
	awaitConfirm(t, ty, confirmTool, "the MCP tool never asked for approval")
	awaitDrawn(t, ty.drawn, func(frames string) bool {
		return strings.Contains(frames, "Approve: mcp_github__create_issue")
	}, "the MCP approval summary")
	ty.press(t, strings.Repeat("\x1b[C", 20))
	awaitDrawn(t, ty.drawn, func(frames string) bool {
		return strings.Contains(frames, "VISIBLE_END")
	}, "the scrollable end of the MCP arguments")
	if got := ty.shell.String(); got != "" {
		t.Fatalf("MCP approval leaked to the shell: %q", got)
	}
	ty.press(t, "\r")

	select {
	case err := <-approved:
		if err != nil {
			t.Fatalf("approval returned %v", err)
		}
	case <-time.After(answerTimeout):
		t.Fatal("Enter did not approve the MCP tool")
	}
	if got := ty.shell.String(); got != "" {
		t.Fatalf("approval Enter reached the shell: %q", got)
	}
	awaitPending(t, ty, false, "the approved MCP query did not finish")
}

func TestYoloAgentAutoApprovesMCPTools(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))
	approved := make(chan error, 1)
	ty.keyboard.agent = func(
		ctx context.Context,
		_ string,
		_ []llm.Turn,
		_ func(string) error,
		_ func(string) error,
		_ func(context.Context, string) (string, error),
		approve llm.ApproveTool,
	) error {
		err := approve(ctx, "mcp_demo__read", json.RawMessage(`{}`))
		approved <- err
		return err
	}

	ty.press(t, "???!read it\r")
	select {
	case err := <-approved:
		if err != nil {
			t.Fatalf("yolo approval returned %v", err)
		}
	case <-time.After(answerTimeout):
		t.Fatal("yolo mode waited for MCP approval")
	}
	if got := ty.shell.String(); got != "" {
		t.Fatalf("yolo MCP approval reached the shell: %q", got)
	}
	awaitPending(t, ty, false, "the yolo MCP query did not finish")
}

func TestEscapeCancelsAnMCPApproval(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))
	cancelled := make(chan error, 1)
	ty.keyboard.agent = func(
		ctx context.Context,
		_ string,
		_ []llm.Turn,
		_ func(string) error,
		_ func(string) error,
		_ func(context.Context, string) (string, error),
		approve llm.ApproveTool,
	) error {
		err := approve(ctx, "mcp_demo__delete", json.RawMessage(`{"id":"one"}`))
		cancelled <- err
		return err
	}

	ty.press(t, "???delete it\r")
	awaitConfirm(t, ty, confirmTool, "the MCP tool never asked for approval")
	ty.press(t, "\x1b")
	select {
	case err := <-cancelled:
		if !errors.Is(err, errTakenBack) {
			t.Fatalf("Escape cancelled approval with %v, want %v", err, errTakenBack)
		}
	case <-time.After(answerTimeout):
		t.Fatal("Escape did not cancel the MCP approval")
	}
	if got := ty.shell.String(); got != "" {
		t.Fatalf("Escape during MCP approval reached the shell: %q", got)
	}
	awaitPending(t, ty, false, "the cancelled MCP query stayed open")
}

// TestResumeWaitDrawsFromANewSaveAndDropsTheOldThought is the overlay
// between agent steps: the query is already off, the old save is the
// prompt that just ran, and putting it back must save again at the
// prompt the shell is at now. The question stays off; only the spinner
// and Thinking: are drawn. The thought that produced the command is
// dropped so it does not linger as the next step's.
func TestResumeWaitDrawsFromANewSaveAndDropsTheOldThought(t *testing.T) {
	ty := newTyping(t, newState(os.Getpid(), nil, appconfig.Limits{}))
	question := "look around"
	q := &query{
		screen: ty.keyboard.screen, visible: visible(ty.keyboard.screen.columns(), 3*markerWidth),
		waiting: true, erased: true, anchored: true, thought: []rune("the last decision"),
		editor: queryEditor{
			text: []rune(question), cursor: len(question),
			mode: queryMode{thinking: true, agent: true},
		},
	}
	ty.keyboard.query = q

	if err := ty.keyboard.resumeWait(q); err != nil {
		t.Fatalf("resuming the wait: %v", err)
	}
	if q.erased {
		t.Error("resumeWait left the query erased, want it on the screen")
	}
	if !q.thoughtOnly {
		t.Error("resumeWait left thoughtOnly false, want the question kept off")
	}
	if !q.anchored {
		t.Error("resumeWait left the query unanchored after drawing, want the new save taken")
	}
	if got := string(q.thought); got != "" {
		t.Errorf("the thought is %q after resumeWait, want it cleared", got)
	}

	empty := spinnerWidth + 1 + len(thoughtLabel)
	want := saveCursor + " | " + sgrDim + thoughtLabel + sgrReset + clearLine +
		fmt.Sprintf(cursorBack, empty) + sgrReset
	awaitDrawn(t, ty.drawn, exactly(want), "Thinking: from a new save, without the question")
	if strings.Contains(ty.drawn.String(), question) {
		t.Errorf("the question %q was drawn between commands, want only Thinking:", question)
	}

	q.thought = []rune("now look closer")
	if err := q.render(q.editor.snapshot()); err != nil {
		t.Fatalf("drawing the next thought: %v", err)
	}
	shown := "now look closer"
	awaitDrawn(t, ty.drawn, endingWith(restoreCursor+" | "+
		sgrDim+thoughtLabel+shown+sgrReset+clearLine+
		fmt.Sprintf(cursorBack, empty+len(shown))+sgrReset),
		"the next thought beside the spinner, still without the question")
}

// TestResumeWaitIgnoresAQueryThatIsNotWaiting is the runner's other
// callers: a command injected without a submitted question has nothing
// to put back, and drawing there would plant a marker on a prompt the
// user owns.
func TestResumeWaitIgnoresAQueryThatIsNotWaiting(t *testing.T) {
	ty := newTyping(t, newState(os.Getpid(), nil, appconfig.Limits{}))
	q := &query{screen: ty.keyboard.screen, erased: true, anchored: true}
	ty.keyboard.query = q

	if err := ty.keyboard.resumeWait(q); err != nil {
		t.Fatalf("resuming a query that was not waiting: %v", err)
	}
	if !q.erased {
		t.Error("a query that was not waiting was put back on the screen")
	}
	if frames := ty.drawn.String(); frames != "" {
		t.Errorf("resumeWait drew %q for a query that was not waiting, want nothing", frames)
	}
}

// TestResumeWaitRefusesAQueryThatWasTakenBack covers the race with Esc:
// the command finished, but the question is already over, and putting
// the line back would draw on a prompt the shell has again.
func TestResumeWaitRefusesAQueryThatWasTakenBack(t *testing.T) {
	ty := newTyping(t, newState(os.Getpid(), nil, appconfig.Limits{}))
	q := &query{
		screen: ty.keyboard.screen, waiting: true, erased: true,
		editor: queryEditor{status: editorFinished},
	}
	ty.keyboard.query = q

	if err := ty.keyboard.resumeWait(q); !errors.Is(err, errTakenBack) {
		t.Errorf("resumeWait on a finished query returned %v, want %v", err, errTakenBack)
	}
	if !q.erased {
		t.Error("a finished query was put back on the screen")
	}
	if frames := ty.drawn.String(); frames != "" {
		t.Errorf("resumeWait drew %q for a finished query, want nothing", frames)
	}
}

// TestAgentWaitReturnsToTheQueryLineAfterACommand is the overlay on the
// agent's path: after the user runs a tool command, a spinner and
// Thinking: are drawn from a new save, the next thought appears there,
// and the line that follows takes it off the same way the first command
// did. The original question stays off.
func TestAgentWaitReturnsToTheQueryLineAfterACommand(t *testing.T) {
	ty := typingOnShell(t)

	ran := make(chan struct{})
	afterRun := make(chan struct{})
	thought := make(chan struct{})
	afterThought := make(chan struct{})
	ty.keyboard.agent = func(ctx context.Context, _ string, _ []llm.Turn, typing func(string) error, think func(string) error, run func(context.Context, string) (string, error), _ llm.ApproveTool) error {
		if err := think("listing tmp first"); err != nil {
			return err
		}
		if _, err := run(ctx, "echo AGENT_MARK=ok"); err != nil {
			return err
		}
		close(ran)
		<-afterRun
		if err := think("now look closer"); err != nil {
			return err
		}
		close(thought)
		<-afterThought
		return typing("# looked in tmp")
	}

	ty.press(t, "???what is in tmp\r")
	awaitShell(t, ty, "echo AGENT_MARK=ok")
	awaitConfirm(t, ty, confirmLine, "the runner never opened the line for confirm")
	ty.press(t, "\r")

	select {
	case <-ran:
	case <-time.After(answerTimeout):
		t.Fatal("the runner never returned after Enter")
	}

	q := ty.pending()
	if q == nil {
		t.Fatal("the query was gone after the command, want it waiting for the next step")
	}
	if q.erased {
		t.Error("the query was still off the screen after the command finished")
	}
	if !q.thoughtOnly {
		t.Error("the question was put back after the command, want only Thinking:")
	}
	if got := string(q.thought); got != "" {
		t.Errorf("the thought is %q after the command, want it cleared so the last step does not linger", got)
	}

	erase := restoreCursor + clearLine + sgrReset
	awaitDrawn(t, ty.drawn, func(frames string) bool {
		i := strings.LastIndex(frames, erase)
		if i < 0 {
			return false
		}
		after := frames[i:]
		return strings.Contains(after, saveCursor) &&
			strings.Contains(after, sgrDim+thoughtLabel) &&
			!strings.Contains(after, "???what is in tmp")
	}, "Thinking: from a new save after the command, without the question")

	close(afterRun)

	select {
	case <-thought:
	case <-time.After(answerTimeout):
		t.Fatal("the next step's thought was never handed to the query")
	}
	awaitDrawn(t, ty.drawn, func(frames string) bool {
		return strings.Contains(frames, sgrDim+thoughtLabel+"now look closer"+sgrReset)
	}, "the next step's thought on the query line")
	close(afterThought)

	awaitDrawn(t, ty.drawn, endingWith(erase), "the erase the next line begins with")
	awaitShell(t, ty, "echo AGENT_MARK=ok\r# looked in tmp")
	awaitPending(t, ty, false, "the keyboard never went back to the shell after the question was answered")
}

// TestAgentRunnerRefusesACommandThatCouldRunSomethingElse covers the same
// guard the final answer uses: a carriage return is Enter wherever it lands,
// even inside a paste, so a command holding one is two commands and the
// runner will not type the first of them.
func TestAgentRunnerRefusesACommandThatCouldRunSomethingElse(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))
	q := &query{screen: ty.keyboard.screen}
	ty.keyboard.query = q

	_, err := ty.keyboard.runner(q, &injection{})(t.Context(), "ls\rrm -rf /")
	if !errors.Is(err, errUntypable) {
		t.Errorf("a command with a carriage return in it was refused with %v, want %v", err, errUntypable)
	}
	if got := ty.shell.String(); got != "" {
		t.Errorf("the shell was sent %q by a command aty will not type, want nothing", got)
	}
}

// TestMultiLineAnswerIsPastedRatherThanTyped is the whole of how a command
// that needs more than one line reaches a line editor without running: the
// newline goes in between the brackets a terminal puts around a paste, where
// an editor collects it as text.
func TestMultiLineAnswerIsPastedRatherThanTyped(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))
	q := &query{screen: ty.keyboard.screen}
	ty.keyboard.query = q

	const command = "cat <<'EOF' > note\nhello\nEOF"
	inject := &injection{}
	if err := ty.keyboard.revise(q, inject, "cat <<'EOF' > note"); err != nil {
		t.Fatalf("typing the first line: %v", err)
	}
	if err := ty.keyboard.revise(q, inject, command); err != nil {
		t.Fatalf("typing the lines after it: %v", err)
	}

	want := "cat <<'EOF' > note" + pasteStart + "\nhello\nEOF" + pasteEnd
	if got := ty.shell.String(); got != want {
		t.Fatalf("the shell was sent %q, want %q", got, want)
	}

	// A pasted answer is still an answer the user can take back to an empty
	// line one backspace at a time, which is what lets the next question be
	// asked on it. Counting the brackets as keystrokes would lose that.
	for range []rune(command) {
		ty.keyboard.state.typed([]byte{del})
	}
	if !ty.keyboard.state.gate().freshLine {
		t.Error("a pasted answer erased back to nothing left the line dirty, so ? would not open on it")
	}
}

// TestAgentQueryTakenBackMidCommandStopsTheLoop covers cancelling while a
// tool command is still running: Enter starts it, Esc takes the question
// back, and Ctrl-C would have gone to the process instead.
func TestAgentQueryTakenBackMidCommandStopsTheLoop(t *testing.T) {
	ty := typingOnShell(t)

	refused := make(chan error, 1)
	ty.keyboard.agent = func(ctx context.Context, _ string, _ []llm.Turn, _ func(string) error, _ func(string) error, run func(context.Context, string) (string, error), _ llm.ApproveTool) error {
		_, err := run(ctx, "sleep 30")
		refused <- err
		return err
	}

	ty.press(t, "???look around\r")
	awaitShell(t, ty, "sleep 30")
	awaitConfirm(t, ty, confirmLine, "the runner never opened the line for confirm")
	ty.press(t, "\r")
	awaitConfirm(t, ty, confirmRun, "Enter never handed the line to the process")
	awaitCommandRunning(t, ty, "sleep never took the terminal, so the runner was never waiting on it")
	ty.press(t, "\x1b")

	select {
	case err := <-refused:
		if !errors.Is(err, errTakenBack) {
			t.Errorf("a command running when the question was taken back was refused with %v, want %v", err, errTakenBack)
		}
	case <-time.After(answerTimeout):
		t.Fatal("the runner never returned after the question was taken back")
	}
	if strings.Contains(ty.shell.String(), "\x1b") {
		t.Errorf("Esc after Enter reached the process, want it to abort the agent: %q", ty.shell.String())
	}
	awaitPending(t, ty, false, "the keyboard never went back to the shell after the question was taken back")
}

// TestAgentCtrlCAfterEnterGoesToTheProcess is the other half of that
// keyboard: once Enter has started the command, Ctrl-C is the process's
// to receive. The agent keeps waiting for the prompt, which is how an
// interrupted command still becomes a tool result.
func TestAgentCtrlCAfterEnterGoesToTheProcess(t *testing.T) {
	ty := typingOnShell(t)

	type outcome struct {
		result string
		err    error
	}
	done := make(chan outcome, 1)
	ty.keyboard.agent = func(ctx context.Context, _ string, _ []llm.Turn, _ func(string) error, _ func(string) error, run func(context.Context, string) (string, error), _ llm.ApproveTool) error {
		result, err := run(ctx, "sleep 30")
		done <- outcome{result, err}
		return err
	}

	ty.press(t, "???look around\r")
	awaitShell(t, ty, "sleep 30")
	awaitConfirm(t, ty, confirmLine, "the runner never opened the line for confirm")
	ty.press(t, "\r")
	awaitConfirm(t, ty, confirmRun, "Enter never handed the line to the process")
	awaitCommandRunning(t, ty, "sleep never took the terminal, so Ctrl-C had nothing to interrupt")
	ty.press(t, "\x03")
	if !strings.Contains(ty.shell.String(), "\x03") {
		t.Errorf("Ctrl-C after Enter was swallowed, want it forwarded to the process: %q", ty.shell.String())
	}

	var out outcome
	select {
	case out = <-done:
	case <-time.After(answerTimeout):
		t.Fatal("the runner never returned after Ctrl-C reached the process")
	}
	if errors.Is(out.err, errTakenBack) {
		t.Error("Ctrl-C after Enter took the question back, want it to interrupt the process and leave the agent waiting for the prompt")
	}
	if out.err != nil {
		t.Errorf("the interrupted command failed the runner: %v", out.err)
	}
	if out.result == "" {
		t.Error("the interrupted command produced no tool result, want what it printed")
	}
}

// TestConfirmHintIsDrawnDimBesideTheCommand is the overlay itself: dim
// confirm?, then the cursor walked back over it so the shell still thinks
// it is at the end of the command.
func TestConfirmHintIsDrawnDimBesideTheCommand(t *testing.T) {
	q, drawn := newQuery(t)
	q.editor.confirm = confirmLine
	if err := q.paintConfirmHint(q.editor.snapshot()); err != nil {
		t.Fatalf("painting confirm?: %v", err)
	}
	if !q.hinted {
		t.Error("painting confirm? left hinted false, want it on the screen")
	}
	awaitDrawn(t, drawn, exactly(confirmHintFrame()), "dim confirm? beside the command")

	if err := q.wipeConfirmHint(); err != nil {
		t.Fatalf("wiping confirm?: %v", err)
	}
	if q.hinted {
		t.Error("wiping confirm? left hinted true, want it off the screen")
	}
	awaitDrawn(t, drawn, endingWith(clearLine+sgrReset), "the erase that takes confirm? off")
}

// TestConfirmHintIsNotPaintedAfterTheFirstKey covers a keystroke that
// arrives before the overlay does: the paint that was about to happen
// must not land on top of the edit.
func TestConfirmHintIsNotPaintedAfterTheFirstKey(t *testing.T) {
	q, drawn := newQuery(t)
	q.editor.confirm = confirmLine
	if err := q.wipeConfirmHint(); err != nil {
		t.Fatalf("dismissing confirm? before it was painted: %v", err)
	}
	if err := q.paintConfirmHint(q.editor.snapshot()); err != nil {
		t.Fatalf("painting confirm? after it was dismissed: %v", err)
	}
	if q.hinted {
		t.Error("confirm? was painted after the first keystroke, want it skipped")
	}
	if frames := drawn.String(); frames != "" {
		t.Errorf("the screen was written %q after the first keystroke, want nothing", frames)
	}
}

// TestConfirmHintIsWipedBeforeTheKeyReachesTheShell is the first
// keystroke while confirm? is up: the overlay comes off the screen
// before that key is forwarded, or Enter would run `ls -la confirm?`.
func TestConfirmHintIsWipedBeforeTheKeyReachesTheShell(t *testing.T) {
	ty := newTyping(t, newState(os.Getpid(), nil, appconfig.Limits{}))
	q := &query{screen: ty.keyboard.screen, hinted: true, editor: queryEditor{confirm: confirmLine}}
	ty.keyboard.query = q

	ty.press(t, "a")
	awaitDrawn(t, ty.drawn, exactly(clearLine+sgrReset), "confirm? erased before the key was forwarded")
	if got := ty.shell.String(); got != "a" {
		t.Errorf("the shell was sent %q, want the key after the hint was gone", got)
	}
	if q.hinted {
		t.Error("the first keystroke left confirm? on the screen")
	}
}

// TestAgentConfirmHintAppearsOnTheScreenNotThePty is the overlay on the
// runner's path: after the command is typed and the line editor has
// announced a prompt, dim confirm? is on the user's screen, never in
// the PTY, and Esc takes it off before the question ends.
func TestAgentConfirmHintAppearsOnTheScreenNotThePty(t *testing.T) {
	ty := newTyping(t, promptReadyState())
	q := &query{screen: ty.keyboard.screen}
	ty.keyboard.query = q

	done := make(chan error, 1)
	go func() {
		_, err := ty.keyboard.runner(q, &injection{})(t.Context(), "ls -la")
		done <- err
	}()

	awaitShell(t, ty, "ls -la")
	if strings.Contains(ty.shell.String(), "confirm?") {
		t.Errorf("confirm? was typed at the shell: %q", ty.shell.String())
	}
	awaitDrawn(t, ty.drawn, endingWith(confirmHintFrame()), "dim confirm? beside the command")
	if strings.Contains(ty.shell.String(), "confirm?") {
		t.Errorf("confirm? reached the PTY after it was painted: %q", ty.shell.String())
	}

	ty.press(t, "\x1b")
	select {
	case err := <-done:
		if !errors.Is(err, errTakenBack) {
			t.Errorf("Esc during confirm was refused with %v, want %v", err, errTakenBack)
		}
	case <-time.After(answerTimeout):
		t.Fatal("the runner never returned after Esc")
	}
	awaitDrawn(t, ty.drawn, endingWith(clearLine+sgrReset), "the erase Esc takes confirm? off with")
	if got := ty.shell.String(); got != "ls -la" {
		t.Errorf("the shell was sent %q, want the command still on the line and Esc not forwarded", got)
	}
}

// TestConfirmHintStaysOffUntilAPromptMark is the overlay's other gate:
// waitConfirm will not paint confirm? until the line editor has
// announced a prompt, even after the short delay that lets an echo land.
func TestConfirmHintStaysOffUntilAPromptMark(t *testing.T) {
	ty := newTyping(t, newState(os.Getpid(), nil, appconfig.Limits{}))
	q := &query{screen: ty.keyboard.screen}
	ty.keyboard.query = q

	done := make(chan error, 1)
	go func() {
		_, err := ty.keyboard.runner(q, &injection{})(t.Context(), "ls -la")
		done <- err
	}()

	awaitShell(t, ty, "ls -la")
	awaitConfirm(t, ty, confirmLine, "the runner never opened the line for confirm")
	time.Sleep(confirmHintDelay + commandPoll)
	if frames := ty.drawn.String(); frames != "" {
		t.Errorf("confirm? was drawn before a prompt mark: %q", frames)
	}

	ty.press(t, "\x1b")
	select {
	case err := <-done:
		if !errors.Is(err, errTakenBack) {
			t.Errorf("Esc during confirm was refused with %v, want %v", err, errTakenBack)
		}
	case <-time.After(answerTimeout):
		t.Fatal("the runner never returned after Esc")
	}
}

// TestAgentCtrlCDuringConfirmClearsTheCommand is the interrupt while the
// command is still sitting on the line: Ctrl-C takes the agent question
// back and also reaches the shell's editor, which discards the command.
func TestAgentCtrlCDuringConfirmClearsTheCommand(t *testing.T) {
	ty := newTyping(t, promptReadyState())
	q := &query{screen: ty.keyboard.screen}
	ty.keyboard.query = q

	done := make(chan error, 1)
	go func() {
		_, err := ty.keyboard.runner(q, &injection{})(t.Context(), "ls -la")
		done <- err
	}()

	awaitShell(t, ty, "ls -la")
	awaitConfirm(t, ty, confirmLine, "the runner never opened the line for confirm")
	awaitDrawn(t, ty.drawn, endingWith(confirmHintFrame()), "dim confirm? beside the command")
	ty.press(t, "\x03")

	select {
	case err := <-done:
		if !errors.Is(err, errTakenBack) {
			t.Errorf("Ctrl-C during confirm was refused with %v, want %v", err, errTakenBack)
		}
	case <-time.After(answerTimeout):
		t.Fatal("the runner never returned after Ctrl-C")
	}
	awaitDrawn(t, ty.drawn, endingWith(clearLine+sgrReset), "the erase Ctrl-C takes confirm? off with")
	if got := ty.shell.String(); got != "ls -la\x03" {
		t.Errorf("the shell was sent %q, want Ctrl-C after the command so the line editor clears it", got)
	}
}

// TestAgentCommandTakenBackDuringConfirmIsInTheNextQuestionsTranscript
// is why Ctrl-C on a tool command and then `? revise that` works: the
// command never ran, but it was sitting on the line as the model's
// answer, so the follow-up is asked with that answer as a turn of its
// own rather than only the commands that did run.
func TestAgentCommandTakenBackDuringConfirmIsInTheNextQuestionsTranscript(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))

	cmd := "cat hello.txt world.text > combined.txt"
	tookBack := make(chan error, 1)
	ty.keyboard.agent = func(ctx context.Context, _ string, _ []llm.Turn, _ func(string) error, _ func(string) error, run func(context.Context, string) (string, error), _ llm.ApproveTool) error {
		_, err := run(ctx, cmd)
		tookBack <- err
		return err
	}

	ty.press(t, "???combine the txt files\r")
	awaitShell(t, ty, cmd)
	awaitConfirm(t, ty, confirmLine, "the runner never opened the line for confirm")
	ty.press(t, "\x03")
	select {
	case err := <-tookBack:
		if !errors.Is(err, errTakenBack) {
			t.Fatalf("Ctrl-C during confirm was refused with %v, want %v", err, errTakenBack)
		}
	case <-time.After(answerTimeout):
		t.Fatal("the runner never returned after Ctrl-C")
	}
	awaitPending(t, ty, false, "the keyboard never went back to the shell after the question was taken back")

	if got := ty.shell.String(); got != cmd+"\x03" {
		t.Fatalf("the shell was sent %q, want the refused command cleared by Ctrl-C", got)
	}
	ty.keyboard.state.observeOutput([]byte("\r\n" + pasteOn))
	if !ty.keyboard.state.gate().freshLine {
		t.Fatal("the prompt after the refused command was interrupted still looks typed-in")
	}

	asked := make(chan []llm.Turn, 1)
	ty.keyboard.ask = func(_ context.Context, _ string, transcript []llm.Turn, _ bool, typing func(string) error, _ func(string) error) error {
		asked <- transcript
		return typing("cat hello.txt world.text > merged.txt")
	}

	ty.press(t, "?change final file to merged.txt\r")
	select {
	case transcript := <-asked:
		if len(transcript) != 1 {
			t.Fatalf("the follow-up was asked with %d turns, want the command that never ran: %+v", len(transcript), transcript)
		}
		if transcript[0].Question != "combine the txt files" || transcript[0].Answer != cmd || transcript[0].Text != "" {
			t.Errorf("the follow-up's transcript does not keep the refused question and command as their own turn: %+v", transcript[0])
		}
	case <-time.After(answerTimeout):
		t.Fatal("the follow-up question was never asked")
	}
}

// TestAgentCommandTakenBackAfterEnterIsNotKeptAsAThrownAwayAnswer is
// the other half of that recording: Enter already opened a transcript
// turn, and Esc then is aborting a command that ran, not an answer that
// never did. Saving it again would show the same command twice.
func TestAgentCommandTakenBackAfterEnterIsNotKeptAsAThrownAwayAnswer(t *testing.T) {
	ty := typingOnShell(t)

	refused := make(chan error, 1)
	ty.keyboard.agent = func(ctx context.Context, _ string, _ []llm.Turn, _ func(string) error, _ func(string) error, run func(context.Context, string) (string, error), _ llm.ApproveTool) error {
		_, err := run(ctx, "sleep 30")
		refused <- err
		return err
	}

	ty.press(t, "???look around\r")
	awaitShell(t, ty, "sleep 30")
	awaitConfirm(t, ty, confirmLine, "the runner never opened the line for confirm")
	ty.press(t, "\r")
	awaitConfirm(t, ty, confirmRun, "Enter never handed the line to the process")
	awaitCommandRunning(t, ty, "sleep never took the terminal, so the runner was never waiting on it")
	ty.press(t, "\x1b")

	select {
	case err := <-refused:
		if !errors.Is(err, errTakenBack) {
			t.Fatalf("Esc after Enter was refused with %v, want %v", err, errTakenBack)
		}
	case <-time.After(answerTimeout):
		t.Fatal("the runner never returned after the question was taken back")
	}
	awaitPending(t, ty, false, "the keyboard never went back to the shell after the question was taken back")

	before := ty.keyboard.state.transcript()
	ty.keyboard.state.capture.discardAnswer()
	after := ty.keyboard.state.transcript()
	if len(after) != len(before) {
		t.Fatalf("Esc after Enter left a thrown-away answer in the transcript: %+v", after)
	}
	for _, turn := range after {
		if turn.Answer == "sleep 30" && turn.Text == "" {
			t.Errorf("the running command was also kept as an answer that never ran: %+v", turn)
		}
	}
}

// TestTriggerReachesTheShellWhenTheGatesAreShut is the other half of the
// decision. Nothing here says the shell is at an empty prompt, so the trigger is
// an ordinary byte and the screen is never touched.
func TestTriggerReachesTheShellWhenTheGatesAreShut(t *testing.T) {
	ty := newTyping(t, newState(os.Getpid(), nil, appconfig.Limits{}))

	ty.press(t, "ls ?.go\r")
	if got, want := ty.shell.String(), "ls ?.go\r"; got != want {
		t.Errorf("the shell was sent %q, want %q", got, want)
	}
	if drawn := ty.drawn.String(); drawn != "" {
		t.Errorf("aty drew %q on a line it had no business on, want nothing", drawn)
	}
}

// TestSubmittedQueryWaitsBehindASpinner covers the wait for an answer. The
// question stays on the screen with the spinner turning, the answer is typed
// into the shell's line editor, and the question comes off the screen as the
// first of it arrives rather than after all of it has.
func TestSubmittedQueryWaitsBehindASpinner(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))

	asked := make(chan string, 1)
	answer := make(chan struct{})
	failed := make(chan error, 1)
	ty.keyboard.ask = func(_ context.Context, question string, _ []llm.Turn, _ bool, typing func(string) error, _ func(string) error) error {
		asked <- question
		<-answer
		err := typing("kubectl rollout restart deploy/web")
		failed <- err
		return err
	}

	ty.press(t, "?restart the web deployment\r")
	if got, want := <-asked, "restart the web deployment"; got != want {
		t.Errorf("the question asked was %q, want %q", got, want)
	}

	// The first frame of the wait is drawn by the submit itself and the next
	// one by the spinner, so a second frame is the spinner turning.
	awaitDrawn(t, ty.drawn, func(frames string) bool {
		return strings.Contains(frames, "?restart the web deployment |") &&
			strings.Contains(frames, "?restart the web deployment /")
	}, "the spinner turning while the question waits")
	if got := ty.shell.String(); got != "" {
		t.Errorf("the shell was sent %q while the question was waiting, want nothing", got)
	}

	close(answer)
	if err := <-failed; err != nil {
		t.Fatalf("typing the answer at the shell: %v", err)
	}
	awaitShell(t, ty, "kubectl rollout restart deploy/web")

	// The question is replaced by the command rather than followed by it, so
	// the erase is on the screen before the command is at the shell.
	awaitDrawn(t, ty.drawn, endingWith(restoreCursor+clearLine+sgrReset), "the erase an answer begins with")
	awaitPending(t, ty, false, "the keyboard never went back to the shell after the question was answered")

	// An answer is typing, so the line the shell is holding is no longer empty
	// and the next trigger on it belongs to the shell.
	if ty.keyboard.state.gate().freshLine {
		t.Error("a command typed at the shell left its line looking empty")
	}
}

// TestWaitingQueryDrawsTheThoughtBesideTheSpinner is the thought on the
// query line: dim, after the spinner, and only while the question is still
// waiting. The cursor is walked back over it so it still sits at the end
// of the question, which is where a waiting query leaves it.
func TestWaitingQueryDrawsTheThoughtBesideTheSpinner(t *testing.T) {
	q, drawn := newQuery(t)

	feed(t, q, "undo last commit")
	awaitDrawn(t, drawn, exactly(saveCursor+"?undo last commit"+clearLine),
		"the question before it waits")

	thought := "the user wants to undo"
	q.waiting = true
	q.thought = []rune(thought)
	if err := q.render(q.editor.snapshot()); err != nil {
		t.Fatalf("drawing the thought: %v", err)
	}

	// The label is spent from the thought's half of the line, so with the
	// label shown the thought itself fits only as its tail.
	tw := min(len(q.thought), q.visible/2-len(thoughtLabel))
	shown := thought[len(thought)-tw:]
	awaitDrawn(t, drawn, endingWith(restoreCursor+"?undo last commit | "+
		sgrDim+thoughtLabel+shown+sgrReset+clearLine+
		fmt.Sprintf(cursorBack, spinnerWidth+1+len(thoughtLabel)+tw)+sgrReset),
		"the thought drawn dim beside the spinner, behind its label")
}

// TestWaitingQueryDrawsOnlyTheTailOfALongThought covers the width budget:
// a thought longer than the line keeps only its tail on the screen, and a
// question that already filled the window shrinks so the thought still fits.
func TestWaitingQueryDrawsOnlyTheTailOfALongThought(t *testing.T) {
	q, drawn := newQuery(t)

	window := q.visible
	feed(t, q, strings.Repeat("x", window))
	awaitDrawn(t, drawn, exactly(saveCursor+"?"+strings.Repeat("x", window)+clearLine),
		"a question that already fills the line")

	tw := min(thoughtMaxWidth, q.visible/2-len(thoughtLabel))
	q.waiting = true
	q.thought = []rune(strings.Repeat("y", tw+20))
	if err := q.render(q.editor.snapshot()); err != nil {
		t.Fatalf("drawing a long thought: %v", err)
	}

	shown := strings.Repeat("x", window-1-len(thoughtLabel)-tw)
	tail := strings.Repeat("y", tw)
	awaitDrawn(t, drawn, endingWith(restoreCursor+"?"+shown+" | "+
		sgrDim+thoughtLabel+tail+sgrReset+clearLine+
		fmt.Sprintf(cursorBack, spinnerWidth+1+len(thoughtLabel)+tw)+sgrReset),
		"the tail of a thought that is longer than the line")
	if strings.Contains(drawn.String(), sgrDim+strings.Repeat("y", tw+1)) {
		t.Error("a thought longer than the window was drawn in full, want only its tail")
	}
}

// TestSubmittedThoughtKeepsOnlyTheCappedTail covers the buffer itself: a
// thought longer than aty keeps is stored as its last thoughtLimit
// characters, and only the last thoughtMaxWidth of those are drawn.
func TestSubmittedThoughtKeepsOnlyTheCappedTail(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))

	head := strings.Repeat("h", 80)
	tail := strings.Repeat("t", thoughtLimit)
	thinking := make(chan struct{})
	hold := make(chan struct{})
	ty.keyboard.ask = func(_ context.Context, _ string, _ []llm.Turn, _ bool, _ func(string) error, think func(string) error) error {
		if err := think(head + tail); err != nil {
			return err
		}
		close(thinking)
		<-hold
		return nil
	}

	ty.press(t, "??undo last commit\r")
	select {
	case <-thinking:
	case <-time.After(answerTimeout):
		t.Fatal("the thought was never handed to the query")
	}

	q := ty.pending()
	if q == nil {
		t.Fatal("the query was gone before the thought was kept")
	}
	if got := string(q.thought); got != tail {
		t.Errorf("a long thought was kept as %q, want the last %d characters", got, thoughtLimit)
	}

	tw := min(thoughtMaxWidth, len(q.thought), q.visible/2-len(thoughtLabel))
	shown := strings.Repeat("t", tw)
	awaitDrawn(t, ty.drawn, func(frames string) bool {
		return strings.Contains(frames, sgrDim+thoughtLabel+shown+sgrReset) &&
			!strings.Contains(frames, strings.Repeat("h", 8)) &&
			!strings.Contains(frames, sgrDim+thoughtLabel+strings.Repeat("t", tw+1))
	}, "only the tail of a capped thought")

	close(hold)
}

// TestThoughtControlCharactersBecomeSpaces covers the one thing a thought
// must never carry to the screen: a paragraph thinks in newlines, and one
// drawn on the query line moves the cursor off it. Every control character
// is a space before the thought is kept, so the line stays one line.
func TestThoughtControlCharactersBecomeSpaces(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))

	thinking := make(chan struct{})
	hold := make(chan struct{})
	ty.keyboard.ask = func(_ context.Context, _ string, _ []llm.Turn, _ bool, _ func(string) error, think func(string) error) error {
		if err := think("line one\nline two\r\nand\x1b[31mcolour"); err != nil {
			return err
		}
		close(thinking)
		<-hold
		return nil
	}

	ty.press(t, "??undo last commit\r")
	select {
	case <-thinking:
	case <-time.After(answerTimeout):
		t.Fatal("the thought was never handed to the query")
	}

	q := ty.pending()
	if q == nil {
		t.Fatal("the query was gone before the thought was kept")
	}
	if got, want := string(q.thought), "line one line two  and [31mcolour"; got != want {
		t.Errorf("the thought was kept as %q, want %q", got, want)
	}

	close(hold)
}

// TestSubmittedThinkingQueryShowsTheThoughtUntilTheAnswerArrives covers
// the path a doubled trigger takes: the model thinks out loud on the query
// line, then the first of the command takes the thought off with the
// question, the same erase a plain answer uses.
func TestSubmittedThinkingQueryShowsTheThoughtUntilTheAnswerArrives(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))

	thinking := make(chan struct{})
	answer := make(chan struct{})
	failed := make(chan error, 1)
	ty.keyboard.ask = func(_ context.Context, _ string, _ []llm.Turn, _ bool, typing func(string) error, think func(string) error) error {
		if err := think("the user wants to undo"); err != nil {
			failed <- err
			return err
		}
		close(thinking)
		<-answer
		err := typing("git reset --soft HEAD~1")
		failed <- err
		return err
	}

	ty.press(t, "??undo last commit\r")
	select {
	case <-thinking:
	case <-time.After(answerTimeout):
		t.Fatal("the thought was never handed to the query")
	}

	awaitDrawn(t, ty.drawn, func(frames string) bool {
		return strings.Contains(frames, "??undo last commit") &&
			strings.Contains(frames, sgrDim+thoughtLabel+"er wants to undo"+sgrReset)
	}, "a submitted thinking question showing the dim thought behind its label")

	close(answer)
	if err := <-failed; err != nil {
		t.Fatalf("typing the answer at the shell: %v", err)
	}
	awaitShell(t, ty, "git reset --soft HEAD~1")
	awaitDrawn(t, ty.drawn, endingWith(restoreCursor+clearLine+sgrReset),
		"the erase that takes the thought off with the question")
	awaitPending(t, ty, false, "the keyboard never went back to the shell after the thought gave way to the answer")
}

func TestAFailedAnswerIsTypedAsAComment(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))
	ty.keyboard.ask = func(_ context.Context, _ string, _ []llm.Turn, _ bool, _ func(string) error, _ func(string) error) error {
		return errors.New("llm: https://api.example.com/v1/chat/completions answered 422 Unprocessable Entity: Extra inputs are not permitted")
	}

	ty.press(t, "?list files\r")
	awaitPending(t, ty, false, "a failed question never finished")
	awaitShell(t, ty, "# https://api.example.com/v1/chat/completions answered 422 Unprocessable Entity: Extra inputs are not permitted")
}

// TestAnswerIsTypedAsItIsGenerated covers the injection itself. A model revises
// what it has said, so each version of the command is measured against the line
// the shell is already holding and only the difference is typed: characters onto
// the end while the command grows, and backspaces for whatever the model took
// back.
func TestAnswerIsTypedAsItIsGenerated(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))

	versions := []string{"git", "git ps", "git push", "git push --force-with-lease"}
	typed := make(chan error, 1)
	ty.keyboard.ask = func(_ context.Context, _ string, _ []llm.Turn, _ bool, typing func(string) error, _ func(string) error) error {
		for _, version := range versions {
			if err := typing(version); err != nil {
				typed <- err
				return err
			}
		}
		typed <- nil
		return nil
	}

	ty.press(t, "?push the branch\r")
	if err := <-typed; err != nil {
		t.Fatalf("typing the answer at the shell: %v", err)
	}

	// `git`, then ` ps` onto the end of it, then the `s` taken back for `ush`,
	// and the rest added.
	awaitShell(t, ty, "git ps\x7fush --force-with-lease")

	// What the shell was sent is a means; what it means is the line its editor
	// is left holding, which is the command the model finished on.
	sent := ty.shell.String()
	if got, want := asEdited(sent), versions[len(versions)-1]; got != want {
		t.Errorf("the keys %q leave a line editor holding %q, want %q", sent, got, want)
	}
}

// TestAnswerThatCouldRunSomethingIsNeverTyped covers the one thing aty will not
// do. What the model said is stripped of everything a line editor acts on before
// it reaches the injection, so an answer still holding one of those means the
// stripping was not done, and typing it would be aty running a command nobody
// read. A newline is the exception, and only because it is pasted rather than
// typed; a carriage return is Enter even inside a paste.
func TestAnswerThatCouldRunSomethingIsNeverTyped(t *testing.T) {
	for name, command := range map[string]string{
		"a carriage return":  "ls\rrm -rf /",
		"an escape sequence": "ls\x1b[A",
	} {
		t.Run(name, func(t *testing.T) {
			ty := newTyping(t, atEmptyPrompt(t))

			refused := make(chan error, 1)
			ty.keyboard.ask = func(_ context.Context, _ string, _ []llm.Turn, _ bool, typing func(string) error, _ func(string) error) error {
				err := typing(command)
				refused <- err
				return err
			}

			ty.press(t, "?list the files\r")
			if err := <-refused; !errors.Is(err, errUntypable) {
				t.Errorf("an answer with %s in it was refused with %v, want %v", name, err, errUntypable)
			}
			// Not even the part before the character that was refused: a
			// command cut short is a different command, and it would be sitting
			// at the prompt as though a model had written it.
			if got := ty.shell.String(); got != "" {
				t.Errorf("the shell was sent %q by an answer aty will not type, want nothing", got)
			}
			awaitPending(t, ty, false, "the keyboard never went back to the shell after an answer that could not be typed")
		})
	}
}

// TestMultiLineAnswerWaitsForTheUsersEnter is the promise, checked against a
// real shell rather than against what aty believes about one: a command of
// several lines sits in the line editor, whole and unrun, until the user
// presses Enter, and then all of it runs.
func TestMultiLineAnswerWaitsForTheUsersEnter(t *testing.T) {
	w := watchShell(t, interactiveShell(t))
	awaitGate(t, w, gate.open, "at a fresh prompt")
	screen, drawn := newDrawnOn(t)
	sent := &ptyOutput{}
	ty := &typing{
		keyboard: &keyboard{
			state:  w.state,
			master: io.MultiWriter(sent, w.ptmx),
			screen: screen,
		},
		shell: sent,
		drawn: drawn,
	}
	ty.keyboard.ask = func(_ context.Context, _ string, _ []llm.Turn, _ bool, typing func(string) error, _ func(string) error) error {
		return typing("echo AAA\necho BBB")
	}

	ty.press(t, "?print two things\r")
	awaitPending(t, ty, false, "the keyboard never went back to the shell after the answer")
	awaitOutput(t, w, "echo BBB")

	// The echo of a pasted line and the output of a run one are told apart by
	// what is on the line: the shell echoes `echo AAA`, and running it prints
	// AAA on a line of its own.
	if out := w.out.String(); strings.Contains(out, "AAA\r\n") {
		t.Fatalf("the shell ran a line of the answer before the user pressed Enter, its output was:\n%s", out)
	}

	ty.press(t, "\r")
	awaitOutput(t, w, "AAA\r\n")
	awaitOutput(t, w, "BBB\r\n")
}

// TestNewlineIsRefusedWithNoEditorToCollectIt is the guard the paste rests on.
// The brackets only mean anything to a line editor that is holding a line, and
// a newline arriving anywhere else is Enter, so an answer that grew one after
// the editor let go of the line is refused rather than sent.
func TestNewlineIsRefusedWithNoEditorToCollectIt(t *testing.T) {
	s := newState(os.Getpid(), nil, appconfig.Limits{})
	s.observeOutput([]byte(pasteOn + pasteOff))
	ty := newTyping(t, s)
	q := &query{screen: ty.keyboard.screen}
	ty.keyboard.query = q

	err := ty.keyboard.revise(q, &injection{}, "ls\nrm -rf /")
	if !errors.Is(err, errUntypable) {
		t.Errorf("a newline with no editor collecting a paste was refused with %v, want %v", err, errUntypable)
	}
	if got := ty.shell.String(); got != "" {
		t.Errorf("the shell was sent %q, want nothing", got)
	}
}

// TestQueryTakenBackWhileItIsBeingTypedStopsThere covers the cancel that lands
// mid-command. Nothing more is typed, and what was typed already stays where the
// user can see it: the keyboard is the shell's again, so clearing the line is
// theirs to do with the line editor they already have.
func TestQueryTakenBackWhileItIsBeingTypedStopsThere(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))

	begun := make(chan struct{})
	refused := make(chan error, 1)
	ty.keyboard.ask = func(ctx context.Context, _ string, _ []llm.Turn, _ bool, typing func(string) error, _ func(string) error) error {
		if err := typing("git pu"); err != nil {
			refused <- err
			return err
		}
		close(begun)
		<-ctx.Done()
		err := typing("git push --force-with-lease")
		refused <- err
		return err
	}

	ty.press(t, "?push the branch\r")
	<-begun
	awaitShell(t, ty, "git pu")
	q := ty.pending()
	if q == nil || q.executionDone == nil {
		t.Fatal("submitted query has no execution completion event")
	}

	ty.press(t, "\x03")
	if err := <-refused; !errors.Is(err, errTakenBack) {
		t.Errorf("the rest of an answer to a question that was taken back was refused with %v, want %v", err, errTakenBack)
	}
	<-q.executionDone
	if got, want := ty.shell.String(), "git pu"; got != want {
		t.Errorf("the shell holds %q after the question was taken back, want the %q that had been typed", got, want)
	}
	awaitPending(t, ty, false, "the keyboard never went back to the shell after the question was taken back")

	// The interrupt that took the question back was aty's, so the next one is
	// the shell's, and that is what throws the half-typed command away.
	ty.press(t, "\x03")
	if got, want := ty.shell.String(), "git pu\x03"; got != want {
		t.Errorf("the shell was sent %q, want %q", got, want)
	}

	turns := ty.keyboard.state.transcript()
	if len(turns) != 1 {
		t.Fatalf("the transcript has %d turns, want the answer that was typed when the question was taken back: %+v", len(turns), turns)
	}
	if turns[0].Question != "push the branch" || turns[0].Answer != "git pu" {
		t.Errorf("the assistant turn was lost: %+v", turns[0])
	}
	if turns[0].Text != "" {
		t.Errorf("a command that never ran has a user turn: %q", turns[0].Text)
	}
}

// TestRevisionIsTheDifferenceFromWhatWasTyped covers the keystrokes on their
// own, which is where every case a stream can produce is cheap to state.
func TestRevisionIsTheDifferenceFromWhatWasTyped(t *testing.T) {
	tests := []struct {
		name    string
		typed   string
		command string
		keys    string
	}{
		{name: "the first version of a command is typed as it stands", command: "ls -la", keys: "ls -la"},
		{name: "a command that has grown is typed onto the end", typed: "ls", command: "ls -la", keys: " -la"},
		{name: "a word the model replaced is backspaced away", typed: "git ps", command: "git push", keys: "\x7fush"},
		{name: "a command that shrank is taken back to what is left", typed: "ls -la", command: "ls", keys: "\x7f\x7f\x7f\x7f"},
		{name: "a command that has not changed is not typed again", typed: "ls -la", command: "ls -la"},
		{name: "a command replaced outright is taken back in full", typed: "ls", command: "pwd", keys: "\x7f\x7fpwd"},
		{
			name:  "a character is one backspace however many bytes it took",
			typed: "echo café", command: "echo caf", keys: "\x7f",
		},
		{
			name:  "and is typed as all of its bytes",
			typed: "echo caf", command: "echo café", keys: "é",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			erase, insert := revision([]rune(test.typed), []rune(test.command))
			got := string(erase) + insert
			if got != test.keys {
				t.Errorf("revising %q into %q types %q, want %q", test.typed, test.command, got, test.keys)
			}
			if line := asEdited(test.typed + got); line != test.command {
				t.Errorf("revising %q into %q leaves a line editor holding %q", test.typed, test.command, line)
			}
		})
	}
}

// TestQueryTakenBackWhileWaitingIsNeverTyped is the cancel that matters most:
// once the model is answering, the only thing left to say is no, and it has to
// mean that nothing at all reaches the shell.
func TestQueryTakenBackWhileWaitingIsNeverTyped(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))

	asked := make(chan string, 1)
	refused := make(chan error, 1)
	ty.keyboard.ask = func(ctx context.Context, question string, _ []llm.Turn, _ bool, typing func(string) error, _ func(string) error) error {
		asked <- question
		<-ctx.Done()
		// An assistant is free to have a command in hand when it is stopped,
		// and the answer to that is that it is not typed.
		err := typing("rm -rf /")
		refused <- err
		return err
	}

	ty.press(t, "?something regrettable\r")
	<-asked
	ty.press(t, "\x03")

	if err := <-refused; !errors.Is(err, errTakenBack) {
		t.Errorf("an answer to a question that was taken back was refused with %v, want %v", err, errTakenBack)
	}
	if got := ty.shell.String(); got != "" {
		t.Errorf("the shell was sent %q by a question that was taken back, want nothing", got)
	}
	awaitPending(t, ty, false, "the keyboard never went back to the shell after the question was taken back")
	awaitDrawn(t, ty.drawn, endingWith(restoreCursor+clearLine+sgrReset), "the erase a question that was taken back leaves")
}

// TestSubmittedQueryCarriesRecentOutput is why a follow-up works: the model
// is given what just appeared on the screen, because the shell never says
// what it ran and never will.
func TestSubmittedQueryCarriesRecentOutput(t *testing.T) {
	s := atEmptyPrompt(t)
	s.observeOutput([]byte("$ " + pasteOn + "git push origin HEAD" + pasteOff + "\nerror: failed to push some refs\n$ " + pasteOn))

	saw := make(chan []llm.Turn, 1)
	ty := newTyping(t, s)
	ty.keyboard.ask = func(_ context.Context, question string, transcript []llm.Turn, _ bool, typing func(string) error, _ func(string) error) error {
		saw <- transcript
		return typing("git push --force-with-lease")
	}

	ty.press(t, "?why did that fail\r")
	var transcript []llm.Turn
	select {
	case transcript = <-saw:
	case <-time.After(answerTimeout):
		t.Fatal("the question was never asked")
	}
	if !turnsContain(transcript, "failed to push") {
		t.Errorf("the question was asked without the output that failed:\n%+v", transcript)
	}
}

// TestAnAnswerRunKeepsTheAssistantTurn is the other half of an answer's
// journey: the command the model wrote was run, so its transcript turn keeps
// the answer itself rather than only the command line, and the
// model reads its own words back as the assistant turn the output belongs to.
func TestAnAnswerRunKeepsTheAssistantTurn(t *testing.T) {
	s := atEmptyPrompt(t)
	ty := newTyping(t, s)
	ty.keyboard.ask = func(_ context.Context, _ string, _ []llm.Turn, _ bool, typing func(string) error, _ func(string) error) error {
		return typing("git push --force-with-lease")
	}

	ty.press(t, "?push it anyway\r")
	awaitPending(t, ty, false, "the question was never answered")

	ty.press(t, "\r")
	s.observeOutput([]byte("$ " + pasteOn + "git push --force-with-lease" + pasteOff + "\nEverything up-to-date\n$ " + pasteOn))

	turns := s.transcript()
	if len(turns) != 1 {
		t.Fatalf("the transcript has %d turns, want the one command that ran", len(turns))
	}
	if turns[0].Question != "push it anyway" {
		t.Errorf("the turn does not keep the question that produced the answer: %+v", turns[0])
	}
	if turns[0].Answer != "git push --force-with-lease" {
		t.Errorf("the turn does not keep the answer as the assistant's own: %+v", turns[0])
	}
	if !strings.Contains(turns[0].Text, "Everything up-to-date") {
		t.Errorf("the turn does not hold what the command printed:\n%s", turns[0].Text)
	}
}

func TestDumpQuestionPrintsTheContextInsteadOfAsking(t *testing.T) {
	s := atEmptyPrompt(t)
	s.observeOutput([]byte("$ " + pasteOn + "git push origin HEAD" + pasteOff + "\nerror: failed to push some refs\n$ " + pasteOn))

	asked := make(chan struct{}, 1)
	ty := newTyping(t, s)
	ty.keyboard.ask = func(_ context.Context, _ string, _ []llm.Turn, _ bool, _ func(string) error, _ func(string) error) error {
		asked <- struct{}{}
		return nil
	}
	ty.keyboard.dump = func(question string, transcript []llm.Turn) string {
		return "system:\nbe brief\n\nuser:\n" + question + "\n" + strings.Join(turnTexts(transcript), "\n") + "\n"
	}

	ty.press(t, "?#why did that fail\r")
	select {
	case <-asked:
		t.Error("a dump was sent to the model")
	default:
	}

	path := awaitDump(t, ty)
	if ty.pending() != nil {
		t.Error("a dump left the query open")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the dump: %v", err)
	}
	for _, want := range []string{"system:", "be brief", "why did that fail", "failed to push"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("the dump dropped %q:\n%s", want, got)
		}
	}
}

func TestXDumpQuestionOmitsRecentOutput(t *testing.T) {
	s := atEmptyPrompt(t)
	s.observeOutput([]byte("$ " + pasteOn + "git push origin HEAD" + pasteOff + "\nerror: failed to push some refs\n$ " + pasteOn))

	saw := make(chan []llm.Turn, 1)
	ty := newTyping(t, s)
	ty.keyboard.dump = func(question string, transcript []llm.Turn) string {
		if question != "why did that fail" {
			t.Errorf("the dump question is %q, want the ?x modifier removed", question)
		}
		saw <- transcript
		return "dumped\n"
	}

	ty.press(t, "?x#why did that fail\r")
	select {
	case transcript := <-saw:
		if len(transcript) != 0 {
			t.Errorf("a ?x dump included recent context:\n%+v", transcript)
		}
	case <-time.After(answerTimeout):
		t.Fatal("the dump was never formatted")
	}
}

func TestContextTextSplitsTranscriptAndQuestion(t *testing.T) {
	transcript := []llm.Turn{
		{Text: "$ git push\nerror: failed to push some refs"},
		{Text: "$ pwd\n/tmp"},
	}
	got := contextText(nil, "why did that fail", transcript)

	if strings.Count(got, "user:") != 3 {
		t.Errorf("a nil dump wrote %d user turns, want one per command and the question each as their own:\n%s", strings.Count(got, "user:"), got)
	}
	if strings.Contains(got, "Question:") {
		t.Errorf("a nil dump still joined the transcript and the question into one turn:\n%s", got)
	}
	if strings.Index(got, "git push") > strings.Index(got, "why did that fail") {
		t.Errorf("the question arrived before the output it is about:\n%s", got)
	}

	kept := contextText(nil, "what is my public ip", []llm.Turn{{
		Question: "what is my ip",
		Answer:   "ifconfig",
		Text:     "$ ifconfig\n192.0.2.1",
	}})
	if i, j := strings.Index(kept, "what is my ip"), strings.Index(kept, "ifconfig"); i < 0 || i > j {
		t.Errorf("the dump dropped the question that produced the answer or put it after it:\n%s", kept)
	}

	alone := contextText(nil, "list the files", nil)
	if strings.Count(alone, "user:") != 1 {
		t.Errorf("a nil dump with no transcript wrote %d user turns, want 1:\n%s", strings.Count(alone, "user:"), alone)
	}
}

func TestBareDumpPrintsTheContextWithoutAQuestion(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))
	ty.keyboard.dump = func(question string, _ []llm.Turn) string {
		if question != "" {
			t.Errorf("a bare dump carried question %q, want none", question)
		}
		return "system:\nbe brief\n\nuser:\n"
	}

	ty.press(t, "?#\r")
	path := awaitDump(t, ty)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the dump: %v", err)
	}
	if !strings.Contains(string(got), "system:") {
		t.Errorf("a bare dump has no system prompt:\n%s", got)
	}
}

func TestDumpQuestionWaitsBehindASpinner(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))

	started := make(chan struct{})
	release := make(chan struct{})
	ty.keyboard.dump = func(string, []llm.Turn) string {
		close(started)
		<-release
		return "dumped\n"
	}

	ty.press(t, "?#\r")
	<-started

	awaitDrawn(t, ty.drawn, func(frames string) bool {
		return strings.Contains(frames, "?# |") && strings.Contains(frames, "?# /")
	}, "the spinner turning while the dump waits")
	if got := ty.shell.String(); got != "" {
		t.Errorf("the shell was sent %q while the dump was waiting, want nothing", got)
	}

	close(release)
	_ = awaitDump(t, ty)
	awaitPending(t, ty, false, "the keyboard never went back to the shell after the dump")
}

func TestDumpTakenBackWhileChecksRunTypesNothing(t *testing.T) {
	ty := newTyping(t, atEmptyPrompt(t))

	started := make(chan struct{})
	release := make(chan struct{})
	ty.keyboard.dump = func(string, []llm.Turn) string {
		close(started)
		<-release
		return "dumped\n"
	}

	ty.press(t, "?#\r")
	<-started
	ty.press(t, "\x1b")
	awaitPending(t, ty, false, "taking the dump back left the query open")
	if got := ty.shell.String(); got != "" {
		t.Errorf("a dump that was taken back still typed %q", got)
	}
	close(release)
	time.Sleep(20 * time.Millisecond)
	if got := ty.shell.String(); got != "" {
		t.Errorf("a dump that was taken back typed %q after the checks finished", got)
	}
}

func catPath(sent string) (string, bool) {
	const prefix = "cat '"
	if !strings.HasPrefix(sent, prefix) || !strings.HasSuffix(sent, "'") {
		return "", false
	}
	return strings.TrimSuffix(strings.TrimPrefix(sent, prefix), "'"), true
}

func awaitDump(t *testing.T, ty *typing) string {
	t.Helper()

	var sent string
	for deadline := time.Now().Add(answerTimeout); ; {
		sent = ty.shell.String()
		if path, ok := catPath(sent); ok {
			return path
		}
		if time.Now().After(deadline) {
			t.Fatalf("the shell was sent %q, want a cat of the dump", sent)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestXQueryOmitsRecentOutputInEveryModeAndStripsTheModifier(t *testing.T) {
	type request struct {
		question   string
		transcript []llm.Turn
		thinking   bool
		agent      bool
	}

	for _, test := range []struct {
		name     string
		marker   string
		thinking bool
		agent    bool
	}{
		{name: "normal", marker: "?"},
		{name: "thinking", marker: "??", thinking: true},
		{name: "agent", marker: "???", thinking: true, agent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := atEmptyPrompt(t)
			s.observeOutput([]byte("$ " + pasteOn + "git push origin HEAD" + pasteOff + "\nerror: failed to push some refs\n$ " + pasteOn))

			saw := make(chan request, 1)
			ty := newTyping(t, s)
			ty.keyboard.ask = func(_ context.Context, question string, transcript []llm.Turn, thinking bool, _ func(string) error, _ func(string) error) error {
				saw <- request{question: question, transcript: transcript, thinking: thinking}
				return nil
			}
			ty.keyboard.agent = func(_ context.Context, question string, transcript []llm.Turn, _ func(string) error, _ func(string) error, _ func(context.Context, string) (string, error), _ llm.ApproveTool) error {
				saw <- request{question: question, transcript: transcript, thinking: true, agent: true}
				return nil
			}

			ty.press(t, test.marker+"x why did that fail\r")
			select {
			case got := <-saw:
				if got.question != "why did that fail" {
					t.Errorf("the question is %q, want the x modifier removed", got.question)
				}
				if len(got.transcript) != 0 {
					t.Errorf("%sx included recent context:\n%+v", test.marker, got.transcript)
				}
				if got.thinking != test.thinking || got.agent != test.agent {
					t.Errorf("%sx selected thinking=%t agent=%t, want thinking=%t agent=%t", test.marker, got.thinking, got.agent, test.thinking, test.agent)
				}
			case <-time.After(answerTimeout):
				t.Fatal("the question was never asked")
			}
			awaitPending(t, ty, false, "the keyboard never went back to the shell")
		})
	}
}

// TestSessionKeepsAQuestionAwayFromTheShell is the end-to-end check: a real
// session, hosting a real shell, with a question typed at a real prompt. The
// shell has to end up with an empty line, which is a thing only the shell can
// be asked about: if it were holding the question, Enter would run it.
func TestSessionKeepsAQuestionAwayFromTheShell(t *testing.T) {
	h := hostShell(t, interactiveShell(t), defaultSize())
	awaitQueryMode(t, h)

	// Arithmetic, because the terminal shows the question as it is typed either
	// way: only the shell can turn it into 7.
	writeBytes(t, h.master, "echo Q=$((3+4))")
	// The query is drawn on the user's terminal, which is where the position it
	// is anchored to is saved.
	expect(t, h, regexp.MustCompile(regexp.QuoteMeta(saveCursor)))

	writeBytes(t, h.master, "\x1b")
	writeBytes(t, h.master, "\r")
	ask(t, h, "echo A=$((1+1))", regexp.MustCompile(`A=2`))
	if strings.Contains(h.out.String(), "Q=7") {
		t.Errorf("the shell ran the question it was never sent, the session output was:\n%s", h.out.String())
	}
}

// TestSessionKeepsInterceptingAfterATerminalReport is the reported failure,
// end to end: a command runs, the program in it asks the terminal something,
// and the terminal answers on the input stream. The trigger typed at the
// prompt that follows is still aty's.
func TestSessionKeepsInterceptingAfterATerminalReport(t *testing.T) {
	h := hostShell(t, interactiveShell(t), defaultSize())
	awaitQueryMode(t, h)
	// awaitQueryMode leaves a query open. Escape gives the keyboard back, and
	// the command has to be typed after that: keys arriving while the query
	// still holds them would be read into the question instead.
	writeBytes(t, h.master, "\x1b")
	awaitQuiet(t, h, 100*time.Millisecond)

	writeLine(t, h.master, "sleep 0.6; echo DONE=$((2+2))")
	awaitForegroundJob(t, h)
	// The sleeping command owns the terminal, so this is what a program that
	// asked for the background color reads, not typing at a line editor.
	writeBytes(t, h.master, "\x1b]11;rgb:1e1e/1e1e/1e1e\x1b\\")
	expect(t, h, regexp.MustCompile(`DONE=4`))
	awaitQuiet(t, h, 100*time.Millisecond)

	before := len(h.out.String())
	writeBytes(t, h.master, "?")
	// The query is drawn on the user's terminal, which is where the position
	// it is anchored to is saved. A trigger the shell received instead is
	// echoed as an ordinary character.
	if !awaitPrinted(h, saveCursor, before) {
		t.Fatalf("a trigger after the terminal answered a command was forwarded to the shell, the session output was:\n%s",
			h.out.String())
	}
}

func awaitPrinted(h *hosted, want string, from int) bool {
	for deadline := time.Now().Add(answerTimeout); ; {
		if out := h.out.String(); from <= len(out) && strings.Contains(out[from:], want) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// newQuery is a query with a terminal a test can read back, and nothing else:
// the line editor and the frames it draws are the whole of what it is for.
func newQuery(t *testing.T) (*query, *ptyOutput) {
	t.Helper()

	screen, drawn := newDrawnOn(t)
	return &query{
		screen: screen, visible: visible(screen.columns(), markerWidth)}, drawn
}

func feed(t *testing.T, q *query, keys string) {
	t.Helper()

	if _, _, err := q.feed([]byte(keys)); err != nil {
		t.Fatalf("typing %q at the query: %v", keys, err)
	}
}

// asEdited is the line a line editor is left holding after being sent keys,
// which is what an injection is really claiming: the bytes are how it gets
// there, and a backspace among them is not a character on the line.
func asEdited(keys string) string {
	var line []rune
	for _, key := range keys {
		if key == del {
			line = line[:max(len(line)-1, 0)]
			continue
		}
		line = append(line, key)
	}
	return string(line)
}

// typing is a keyboard wired the way a session wires it, with both sides of it
// readable: what the shell was sent, and what was drawn on the terminal.
type typing struct {
	keyboard *keyboard
	shell    *ptyOutput
	drawn    *ptyOutput
}

func newTyping(t *testing.T, s *state) *typing {
	t.Helper()

	screen, drawn := newDrawnOn(t)
	shell := &ptyOutput{}
	return &typing{
		keyboard: &keyboard{state: s, master: shell, screen: screen},
		shell:    shell,
		drawn:    drawn,
	}
}

// promptReadyState is a state whose line editor has announced a prompt,
// which is what waitConfirm needs before it paints confirm?. It is not
// a real shell: the command never runs, and Esc is the way out.
func promptReadyState() *state {
	s := newState(os.Getpid(), nil, appconfig.Limits{})
	s.observeOutput([]byte(pasteOn))
	return s
}

// typingOnShell is a keyboard whose master is a real shell, which is what
// an agent command needs: forwarding Enter has to run something, and the
// output has to come back through the same state the gates already watch.
func typingOnShell(t *testing.T) *typing {
	t.Helper()

	w := watchShell(t, interactiveShell(t))
	awaitGate(t, w, gate.open, "at a fresh prompt")
	screen, drawn := newDrawnOn(t)
	sent := &ptyOutput{}
	return &typing{
		keyboard: &keyboard{
			state:  w.state,
			master: io.MultiWriter(sent, w.ptmx),
			screen: screen,
		},
		shell: sent,
		drawn: drawn,
	}
}

// press types at aty the way the copy loop does, one run of keystrokes at a
// time, since a run is one read from the user's terminal.
func (ty *typing) press(t *testing.T, keys string) {
	t.Helper()

	n, err := ty.keyboard.Write([]byte(keys))
	if err != nil {
		t.Fatalf("typing %q: %v", keys, err)
	}
	if n != len(keys) {
		t.Fatalf("typing %q accounted for %d of %d bytes", keys, n, len(keys))
	}
}

// pending is the query being typed, read the way the goroutines that finish one
// read it.
func (ty *typing) pending() *query {
	ty.keyboard.mu.Lock()
	defer ty.keyboard.mu.Unlock()

	return ty.keyboard.query
}

// newDrawnOn is a screen a test can read back. It is a pipe rather than a pty
// on purpose: aty draws into whatever it is given, and a pipe reports no width,
// which is the case the assumed width is there for.
func newDrawnOn(t *testing.T) (*screen, *ptyOutput) {
	t.Helper()
	return newDrawnOnWithColor(t, appconfig.NoColor)
}

func newDrawnOnWithColor(t *testing.T, color appconfig.Color) (*screen, *ptyOutput) {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	drawn := &ptyOutput{}
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(drawn, reader)
	}()
	t.Cleanup(func() {
		_ = writer.Close()
		<-copied
		_ = reader.Close()
	})
	return newScreen(writer, color), drawn
}

// atEmptyPrompt is a state that says the shell is sitting at a prompt with
// nothing typed on it, which is the only condition aty takes a keystroke in. It
// comes from a real shell in a real pty, because the gates read the kernel and
// nothing short of that opens them.
func atEmptyPrompt(t *testing.T) *state {
	t.Helper()

	w := watchShell(t, interactiveShell(t))
	awaitGate(t, w, gate.open, "at a fresh prompt")
	return w.state
}

// exactly matches the frames a test expects and nothing else, which is what
// makes a stray frame a failure rather than something to be waited past.
func exactly(want string) func(string) bool {
	return func(frames string) bool { return frames == want }
}

// endingWith matches frames whose last one is the one a test is waiting for,
// which is how a test asks what is on the screen now rather than what has been
// drawn on it since it started.
func endingWith(want string) func(string) bool {
	return func(frames string) bool { return strings.HasSuffix(frames, want) }
}

// confirmHintFrame is the overlay waitConfirm paints: dim confirm?, then
// the cursor walked back over it so the shell still thinks it is at the
// end of the command.
func confirmHintFrame() string {
	return sgrDim + confirmHint + sgrReset + clearLine + fmt.Sprintf(cursorBack, len(confirmHint)) + sgrReset
}

// awaitDrawn waits for the frames a test expects, since a frame reaches the
// terminal on its own way.
func awaitDrawn(t *testing.T, drawn *ptyOutput, want func(frames string) bool, when string) {
	t.Helper()

	var frames string
	for deadline := time.Now().Add(answerTimeout); ; {
		if frames = drawn.String(); want(frames) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was never drawn, what was drawn is:\n%q", when, frames)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// awaitShell waits for the shell to be sent something, which an answer to a
// question is on a goroutine of its own.
func awaitShell(t *testing.T, ty *typing, want string) {
	t.Helper()

	var sent string
	for deadline := time.Now().Add(answerTimeout); ; {
		if sent = ty.shell.String(); sent == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the shell was sent %q, want %q", sent, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// awaitPending waits for a query to be open, or to be over, which is a thing
// the goroutine an assistant runs on decides.
func awaitPending(t *testing.T, ty *typing, want bool, complaint string) {
	t.Helper()

	for deadline := time.Now().Add(answerTimeout); ; {
		if (ty.pending() != nil) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(complaint)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// awaitCommandRunning waits until something other than the shell owns the
// terminal, which is how a test knows Enter actually started the command
// it injected.
func awaitCommandRunning(t *testing.T, ty *typing, complaint string) {
	t.Helper()

	for deadline := time.Now().Add(answerTimeout); ; {
		if g := ty.keyboard.state.gate(); g.err == nil && !g.atPrompt {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(complaint)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// awaitConfirm waits for waitConfirm to have opened the line, which is
// when Enter reaches the shell instead of being swallowed.
func awaitConfirm(t *testing.T, ty *typing, want confirmPhase, complaint string) {
	t.Helper()

	for deadline := time.Now().Add(answerTimeout); ; {
		ty.keyboard.mu.Lock()
		got := confirmOff
		if ty.keyboard.query != nil {
			got = ty.keyboard.query.editor.confirm
		}
		ty.keyboard.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(complaint)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// awaitQueryMode types the trigger at a fresh prompt until aty takes it, which
// is how a test waits for a session to have validated the reading its gates
// stand on: a session does that while it is already running, so the first
// trigger typed can be too early.
func awaitQueryMode(t *testing.T, h *hosted) {
	t.Helper()

	for deadline := time.Now().Add(answerTimeout); ; {
		writeBytes(t, h.master, "\r")
		awaitQuiet(t, h, 50*time.Millisecond)
		before := len(h.out.String())
		writeBytes(t, h.master, "?")

		for attempt := time.Now().Add(attemptTimeout); time.Now().Before(attempt); {
			output := h.out.String()
			if before <= len(output) && strings.Contains(output[before:], saveCursor) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		if time.Now().After(deadline) {
			t.Fatalf("a trigger typed at a fresh prompt never opened a query.\nthe session output was:\n%s",
				h.out.String())
		}
	}
}

//go:build darwin || linux

package term

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	appconfig "github.com/TheR1D/aty/internal/config"
	"github.com/TheR1D/aty/internal/llm"
)

const (
	pasteOn  = "\x1b[?2004h"
	pasteOff = "\x1b[?2004l"
)

// turnTexts is each turn as the user message it is sent as, which is the part
// of the transcript most tests are about.
func turnTexts(turns []llm.Turn) []string {
	texts := make([]string, 0, len(turns))
	for _, turn := range turns {
		texts = append(texts, turn.Text)
	}
	return texts
}

// cmdTurn is one transcript turn as the model reads it: the prompt, the
// command, then what it printed, with no labels.
func cmdTurn(prompt, command, output string) string {
	s := prompt + command
	if output != "" {
		s += "\n" + output
	}
	return s
}

func turnsContain(turns []llm.Turn, text string) bool {
	return strings.Contains(strings.Join(turnTexts(turns), "\n"), text)
}

// captureStream supplies parsed mode events without involving process gating.
type captureStream struct {
	*capture
	modes decModes
}

func newCaptureStream() *captureStream {
	return &captureStream{capture: newCapture(appconfig.Limits{})}
}

func TestInitialPS1SurvivesTypingAndPromptChanges(t *testing.T) {
	for _, test := range []struct {
		name   string
		output []string
		want   string
	}{
		{"prompt before paste mode", []string{"banner\r\n\x1b[32muser@host\x1b[0m$ " + pasteOn}, "user@host$ "},
		{"prompt after paste mode", []string{pasteOn, "user@", "host$ "}, "user@host$ "},
		{"multiline bash prompt", []string{pasteOn, "user@host\r\n", "$ "}, "user@host\n$ "},
		{"empty prompt", []string{pasteOn}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := newCaptureStream()
			for _, output := range test.output {
				c.feed([]byte(output))
			}
			c.typed([]byte("pwd"))
			run(c, "pwd", "/tmp", "changed> ")
			if got := c.initialPS1(); got != test.want {
				t.Fatalf("startup prompt = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCaptureUsesConfiguredLimits(t *testing.T) {
	t.Run("command bytes", func(t *testing.T) {
		c := &captureStream{capture: newCapture(appconfig.Limits{CommandBytes: 13})}
		atPrompt(c, "$ ")
		run(c, "cat", "123456\nabc\ndef", "$ ")
		if got := turnTexts(c.recent()); !slices.Equal(got, []string{"$ cat"}) {
			t.Fatalf("byte trimming = %q", got)
		}
	})
	t.Run("transcript eviction", func(t *testing.T) {
		c := &captureStream{capture: newCapture(appconfig.Limits{TranscriptBytes: 25, TranscriptKeepBytes: 12})}
		atPrompt(c, "$ ")
		for _, command := range []string{"echo 1", "echo 2", "echo 3"} {
			run(c, command, "ok", "$ ")
		}
		if got := turnTexts(c.recent()); !slices.Equal(got, []string{"$ echo 3\nok"}) {
			t.Fatalf("eviction = %q, want only the newest command", got)
		}
	})
	t.Run("larger limits retain more output", func(t *testing.T) {
		c := &captureStream{capture: newCapture(appconfig.Limits{
			CommandBytes: 20000,
		})}
		atPrompt(c, "$ ")
		output := strings.Repeat("x\n", 299) + strings.Repeat("é", 4500)
		run(c, "cat", output, "$ ")
		if got := turnTexts(c.recent()); !slices.Equal(got, []string{"$ cat\n" + output}) {
			t.Fatal("larger limits still truncated output at the old defaults")
		}
	})
}

func (c *captureStream) feed(p []byte) bool {
	return c.capture.feed(p, c.modes.feed(p))
}

func TestCaptureBoundsOutputBeforeCommandFinishes(t *testing.T) {
	for _, limit := range []int{128, appconfig.DefaultCommandBytes} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			c := &captureStream{capture: newCapture(appconfig.Limits{CommandBytes: limit})}
			atPrompt(c, "$ ")
			c.answered("show logs", "cat", false)
			c.feed([]byte("cat" + pasteOff + "\n"))
			for i := range 5000 {
				line := fmt.Sprintf("%04d café", i)
				c.feed([]byte(line + "\n"))
				if c.open.size() > limit || !strings.HasSuffix(c.open.text(), "\n"+line) {
					t.Fatalf("running capture lost latest output or exceeded %d bytes: %q", limit, c.open.text())
				}
				if c.open.command != "cat" {
					t.Fatal("output trimming changed the command")
				}
			}
			if len(c.recent()) != 0 {
				t.Fatal("running command was committed early")
			}
			c.feed([]byte("\x1b]133;D;0\a"))
			if c.open.size() > limit || !strings.HasSuffix(c.open.text(), "\nexit code = 0") {
				t.Fatal("exit metadata was not included in the running byte budget")
			}
			want := c.open.text()
			c.feed([]byte("$ " + pasteOn))
			if got := c.recent(); len(got) != 1 || got[0].Text != want {
				t.Fatalf("completion changed captured output: %+v", got)
			}
		})
	}
}

func TestCaptureKeepsMoreThan256ShortLines(t *testing.T) {
	c := newCaptureStream()
	atPrompt(c, "$ ")
	output := strings.Repeat("OK\n", 999) + "OK"
	run(c, "cat", output, "$ ")
	if got := turnTexts(c.recent()); !slices.Equal(got, []string{"$ cat\n" + output}) {
		t.Fatal("short output was discarded despite fitting in the byte budget")
	}
}

func TestCaptureKeepsBothEndsOfLongUnbrokenOutput(t *testing.T) {
	const limit = 200
	c := &captureStream{capture: newCapture(appconfig.Limits{CommandBytes: limit})}
	atPrompt(c, "$ ")
	c.feed([]byte("cat" + pasteOff + "\n"))
	var output strings.Builder
	for i := range 10000 {
		part := fmt.Sprintf("%04d界", i)
		output.WriteString(part)
		c.feed([]byte(part))
		if len(c.outputLine.head)+c.outputLine.count > limit {
			t.Fatal("unfinished line exceeded the command byte budget")
		}
	}
	c.feed([]byte("\n$ " + pasteOn))
	want := "$ cat" + expectedPreview("\n"+output.String(), limit-len("$ cat"))
	if got := turnTexts(c.recent()); !slices.Equal(got, []string{want}) {
		t.Fatalf("long line capture = %q, want %q", got, want)
	}
}

func TestCaptureKeepsTranscriptAcrossReadBoundaries(t *testing.T) {
	stream := []byte("cat" + pasteOff + "\r\nbefore\r\n" +
		"\x1b[?1049;2004hhidden\x1b]133;D;9\a\x1b[?2004;1049l" +
		"after café\r\n\x1b]133;D;7\x1b\\remote$ " + pasteOn)
	want := llm.Turn{
		Question: "show output", Answer: "cat", Tool: true,
		Text: "remote$ cat\nbefore\nafter café\nexit code = 7",
	}
	for width := 1; width <= len(stream); width++ {
		c := newCaptureStream()
		c.feed([]byte(pasteOn + "remote$ "))
		c.answered(want.Question, want.Answer, want.Tool)
		c.typed([]byte("cat\r"))
		for start := 0; start < len(stream); start += width {
			c.feed(stream[start:min(start+width, len(stream))])
		}
		if got := c.recent(); !slices.Equal(got, []llm.Turn{want}) {
			t.Fatalf("read width %d: transcript = %+v, want %+v", width, got, want)
		}
		if sequence, result := c.completion(); sequence != 1 || result != want.Text {
			t.Fatalf("read width %d: completion = %d, %q", width, sequence, result)
		}
		if prompt := c.initialPS1(); prompt != "remote$ " {
			t.Fatalf("read width %d: startup prompt = %q", width, prompt)
		}
	}
}

func TestCaptureTruncationMatchesConcatenatedOutput(t *testing.T) {
	for _, output := range []string{
		strings.Repeat("x", appconfig.DefaultCommandBytes-len("$ cat")),   // The leading newline crosses the entry budget.
		strings.Repeat("x", appconfig.DefaultCommandBytes-len("$ cat\n")), // Exact fit: no marker.
		"first\n" + strings.Repeat("é界🙂", 5000) + "\nlast",
		strings.Repeat("header\n", 1000) + strings.Repeat("abcdef", 5000) + "\nlast",
	} {
		c := newCaptureStream()
		atPrompt(c, "$ ")
		run(c, "cat", output, "$ ")
		want := "$ cat" + expectedPreview("\n"+output, appconfig.DefaultCommandBytes-len("$ cat"))
		if got := turnTexts(c.recent()); !slices.Equal(got, []string{want}) {
			t.Fatalf("capture differs from a single head/tail cut: got %q, want %q", got, want)
		}
	}
}

func atPrompt(c *captureStream, prompt string) {
	c.feed([]byte(prompt + pasteOn))
}

func run(c *captureStream, command, output, prompt string) bool {
	seq := command + pasteOff + "\n"
	if output != "" {
		seq += output + "\n"
	}
	seq += prompt + pasteOn
	return c.feed([]byte(seq))
}

func TestLineProcessorKeepsWhatTheUserSaw(t *testing.T) {
	tests := []struct {
		name    string
		output  []string
		lines   []string
		current string
	}{
		{name: "plain text", output: []string{"hello\n"}, lines: []string{"hello"}},
		{name: "a carriage return rewrites the line", output: []string{"file.iso  10%\rfile.iso  50%\rfile.iso 100%\n"}, lines: []string{"file.iso 100%"}},
		{name: "backspace moves without deleting", output: []string{"hello\b\b\b"}, current: "hello"},
		{name: "then the next character overwrites", output: []string{"cat\b\bog\n"}, lines: []string{"cog"}},
		{name: "erase-line drops the tail", output: []string{"hello world\r\x1b[Khi\n"}, lines: []string{"hi"}},
		{name: "erase-line without a parameter is the same", output: []string{"abc\x1b[2D\x1b[K\n"}, lines: []string{"a"}},
		{name: "erase to the start of the line", output: []string{"abc\x1b[1K\n"}, lines: []string{"   "}},
		{name: "erase the whole line", output: []string{"abc\x1b[2K\rdef\n"}, lines: []string{"def"}},
		{name: "color is not text", output: []string{"\x1b[32mgreen\x1b[0m\n"}, lines: []string{"green"}},
		{name: "nor is a 256-color sequence", output: []string{"\x1b[38;5;244msuggestion\x1b[0m\n"}, lines: []string{"suggestion"}},
		{name: "nor a window title", output: []string{"\x1b]0;title\x07ls\n"}, lines: []string{"ls"}},
		{name: "nor the string terminator a title can use instead", output: []string{"\x1b]0;title\x1b\\ls\n"}, lines: []string{"ls"}},
		{name: "nor an OSC 133 exit status", output: []string{"\x1b]133;D;1\x07ls\n"}, lines: []string{"ls"}},
		{name: "an autosuggestion stays on the line until it is overwritten",
			output:  []string{"git pu\x1b[38;5;244msh origin\x1b[0m\x1b[9D"},
			current: "git push origin"},
		{name: "a cursor-back after a suggestion is how the cursor returns to the typed text",
			output:  []string{"git pu\x1b[38;5;244msh\x1b[0m\x1b[2D"},
			current: "git push"},
		{name: "UTF-8 is one character", output: []string{"café\n"}, lines: []string{"café"}},
		{name: "a character split across two writes is still one character", output: []string{"caf", "\xc3", "\xa9\n"}, lines: []string{"café"}},
		{name: "a sequence split across two writes is still one sequence", output: []string{"\x1b[3", "2mred\x1b[0m\n"}, lines: []string{"red"}},
		{name: "the text of a sequence is not a sequence", output: []string{"\\033[32m\n"}, lines: []string{"\\033[32m"}},
		{name: "a tab lands on the next stop", output: []string{"a\tb\n"}, lines: []string{"a       b"}},
		{name: "the screen-switch sequence is not text", output: []string{"ls\x1b[?1049h\n"}, lines: []string{"ls"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var whole linebuf
			var lines []string
			for _, piece := range test.output {
				lines = append(lines, whole.feed([]byte(piece))...)
			}
			assertLines(t, lines, test.lines, whole.text(), test.current)

			var split linebuf
			lines = nil
			for _, piece := range test.output {
				for i := range len(piece) {
					lines = append(lines, split.feed([]byte{piece[i]})...)
				}
			}
			if test.name == "a character split across two writes is still one character" ||
				test.name == "a sequence split across two writes is still one sequence" {
				return
			}
			assertLines(t, lines, test.lines, split.text(), test.current)
		})
	}
}

func assertLines(t *testing.T, got, want []string, current, wantCurrent string) {
	t.Helper()
	if len(want) == 0 {
		want = nil
	}
	if len(got) == 0 {
		got = nil
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("flushed lines are %q, want %q", got, want)
	}
	if current != wantCurrent {
		t.Errorf("the current line is %q, want %q", current, wantCurrent)
	}
}

func TestTranscriptGivesEachCommandItsOwnTurn(t *testing.T) {
	c := newCaptureStream()
	atPrompt(c, "% ")
	run(c, "echo hi", "hi", "% ")
	run(c, "git status", "On branch main", "% ")

	want := []string{
		cmdTurn("% ", "echo hi", "hi"),
		cmdTurn("% ", "git status", "On branch main"),
	}
	if got := turnTexts(c.recent()); !slices.Equal(got, want) {
		t.Errorf("the transcript is:\n%q\nwant:\n%q", got, want)
	}
}

func TestHalfTypedCommandIsNotMistakenForThePrompt(t *testing.T) {
	c := newCaptureStream()
	atPrompt(c, "$ ")
	run(c, "echo one", "one", "$ ")
	c.feed([]byte("gi"))
	run(c, "t log", "commit 0dcafe", "$ ")

	want := []string{
		cmdTurn("$ ", "echo one", "one"),
		cmdTurn("$ ", "git log", "commit 0dcafe"),
	}
	if got := turnTexts(c.recent()); !slices.Equal(got, want) {
		t.Errorf("the transcript is:\n%q\nwant:\n%q", got, want)
	}
}

func TestEnterOnAnEmptyLineLeavesNothingBehind(t *testing.T) {
	c := newCaptureStream()
	atPrompt(c, "$ ")
	run(c, "", "", "$ ")

	if got := c.recent(); len(got) != 0 {
		t.Errorf("an empty command left %+v in the transcript, want nothing", got)
	}
}

func TestEmptyCommandStartKeepsTheRunningCommandOpen(t *testing.T) {
	c := newCaptureStream()
	atPrompt(c, "$ ")
	c.feed([]byte("ssh host" + pasteOff + "\nLinux host 1.0\n" + pasteOff + "Last login: Tue Aug 18\n$ " + pasteOn))

	want := []string{cmdTurn("$ ", "ssh host", "Linux host 1.0\nLast login: Tue Aug 18")}
	if got := turnTexts(c.recent()); !slices.Equal(got, want) {
		t.Errorf("the transcript is:\n%q\nwant:\n%q", got, want)
	}
}

func TestEmptyCommandStartWithoutACommandDropsTheBanner(t *testing.T) {
	c := newCaptureStream()
	atPrompt(c, "$ ")
	c.feed([]byte(pasteOff + "\nLinux host 1.0\nLast login: Tue Aug 18\n$ " + pasteOn))

	if got := c.recent(); len(got) != 0 {
		t.Errorf("a login banner with no command left %+v in the transcript, want nothing", got)
	}
}

func TestCommandKeepsAPromptDrawnAfterBracketedPaste(t *testing.T) {
	c := newCaptureStream()
	c.feed([]byte(pasteOn))
	c.feed([]byte("user@host:~# "))
	c.typed([]byte("pwd"))
	c.feed([]byte("pwd" + pasteOff + "\n/root\n" + pasteOn))

	want := []string{cmdTurn("user@host:~# ", "pwd", "/root")}
	if got := turnTexts(c.recent()); !slices.Equal(got, want) {
		t.Errorf("the transcript is:\n%q\nwant:\n%q", got, want)
	}
}

// TestCommandKeepsTheTerminalsAnswersOutOfTheTypedKeys covers a reply landing
// while the line editor is at a prompt. Its payload is printable text, and
// appending it to the keys the user typed would put the terminal's words in
// front of the command, where the prompt is cut off the echoed line. What that
// costs is visible in clear: a command read as "user@host:~# clear" is not the
// program named clear, and the transcript the user asked to forget stays.
func TestCommandKeepsTheTerminalsAnswersOutOfTheTypedKeys(t *testing.T) {
	c := newCaptureStream()
	// The prompt mark arrives before PS1, the way bash over ssh sends it, so
	// the command is cut off the echoed line using the keys the user typed.
	c.feed([]byte(pasteOn + "user@host:~# "))
	c.typed([]byte("echo hi"))
	c.feed([]byte("echo hi" + pasteOff + "\nhi\n" + pasteOn + "user@host:~# "))
	if got := turnTexts(c.recent()); !slices.Equal(got, []string{cmdTurn("user@host:~# ", "echo hi", "hi")}) {
		t.Fatalf("the command before the answer is:\n%q", got)
	}

	c.typed([]byte("\x1b]11;rgb:1e1e/1e1e/1e1e\x1b\\"))
	c.typed([]byte("clear"))
	if !c.feed([]byte("clear" + pasteOff + "\n" + pasteOn)) {
		t.Error("clear typed after the terminal answered was not read as the program named clear")
	}
	if got := c.recent(); len(got) != 0 {
		t.Errorf("clear left %d turns in the transcript, want none", len(got))
	}
}

func TestTranscriptKeepsPS1WithTheCommand(t *testing.T) {
	c := newCaptureStream()
	ps1 := "dev@host ~/aty % "
	atPrompt(c, ps1)
	run(c, "pwd", "/Users/dev/aty", ps1)

	want := []string{cmdTurn(ps1, "pwd", "/Users/dev/aty")}
	if got := turnTexts(c.recent()); !slices.Equal(got, want) {
		t.Errorf("the transcript is:\n%q\nwant:\n%q", got, want)
	}
}

func TestClearDropsTheTranscript(t *testing.T) {
	c := newCaptureStream()
	atPrompt(c, "$ ")
	run(c, "echo hi", "hi", "$ ")

	if got := c.recent(); len(got) != 1 {
		t.Fatalf("the command before clear never landed: %+v", got)
	}

	if cleared := run(c, "clear", "", "$ "); !cleared {
		t.Error("committing clear did not report transcript invalidation")
	}

	if got := c.recent(); len(got) != 0 {
		t.Errorf("clear left %+v in the transcript, want nothing", got)
	}

	run(c, "echo after", "after", "$ ")

	want := []string{cmdTurn("$ ", "echo after", "after")}
	if got := turnTexts(c.recent()); !slices.Equal(got, want) {
		t.Errorf("the transcript after clear is:\n%q\nwant:\n%q", got, want)
	}
}

func TestIsClearCommand(t *testing.T) {
	tests := []struct {
		command string
		want    bool
	}{
		{"clear", true},
		{"  clear  ", true},
		{"/usr/bin/clear", true},
		{"clear -x", true},
		{"echo clear", false},
		{"clear; ls", false},
		{"clear && ls", false},
		{"clearance", false},
		{"", false},
	}
	for _, test := range tests {
		if got := isClearCommand(test.command); got != test.want {
			t.Errorf("isClearCommand(%q) = %v, want %v", test.command, got, test.want)
		}
	}
}

func TestChunkTextLabelsTheCommandAndItsOutput(t *testing.T) {
	var ch chunk
	ch.command = "echo hi"
	ch.prompt = "$ "
	if got := ch.text(); got != cmdTurn("$ ", "echo hi", "") {
		t.Errorf("a command with no output is %q, want the prompt and the command", got)
	}
	ch.addLine("hi")
	ch.addLine("there")
	want := cmdTurn("$ ", "echo hi", "hi\nthere")
	if got := ch.text(); got != want {
		t.Errorf("a command with output is:\n%s\nwant:\n%s", got, want)
	}
	if ch.size() != len(want) {
		t.Errorf("size is %d, want %d so the ring counts what the model is sent", ch.size(), len(want))
	}
	ch.hasExit = true
	ch.exit = 2
	want = cmdTurn("$ ", "echo hi", "hi\nthere") + "\n" + exitPrefix + "2"
	if got := ch.text(); got != want {
		t.Errorf("a command with an exit code is:\n%s\nwant:\n%s", got, want)
	}
	if ch.size() != len(want) {
		t.Errorf("size with an exit code is %d, want %d so the ring counts the extra line", ch.size(), len(want))
	}
}

func TestChunkTrimDoesNotCutThePrompt(t *testing.T) {
	prompt := "user@host:~/src % "
	ch := chunk{prompt: prompt, command: strings.Repeat("x", 200)}
	ch.trim(80)
	if got := ch.text(); !strings.HasPrefix(got, prompt) {
		t.Errorf("trim cut the prompt: %q", got)
	}
}

func TestChunkTrimKeepsValidUTF8(t *testing.T) {
	ch := chunk{prompt: "$ ", command: strings.Repeat("界", 40)}
	ch.trim(41)
	if !utf8.ValidString(ch.text()) {
		t.Fatalf("trim split a UTF-8 command: %q", ch.text())
	}
	if ch.size() > 41 {
		t.Fatalf("trimmed chunk size = %d, want at most 41", ch.size())
	}
}

func TestTranscriptKeepsWhatIsOld(t *testing.T) {
	c := newCaptureStream()
	atPrompt(c, "$ ")
	run(c, "old", "stale", "$ ")
	run(c, "new", "fresh", "$ ")

	got := strings.Join(turnTexts(c.recent()), "\n")
	if !strings.Contains(got, "stale") {
		t.Errorf("a chunk older than 15 minutes was dropped while under the cap:\n%s", got)
	}
	if !strings.Contains(got, "fresh") {
		t.Errorf("the recent chunk is gone:\n%s", got)
	}
}

func TestTranscriptDropsWhatIsTooLarge(t *testing.T) {
	c := newCaptureStream()
	c.history.maxBytes = 32
	c.history.keepBytes = 16

	atPrompt(c, "$ ")
	run(c, "one", strings.Repeat("A", 20), "$ ")
	run(c, "two", strings.Repeat("B", 20), "$ ")

	got := strings.Join(turnTexts(c.recent()), "\n")
	if strings.Contains(got, "AAAA") {
		t.Errorf("a chunk that overflowed the cap is still in the transcript:\n%s", got)
	}
	if !strings.Contains(got, "BBBB") {
		t.Errorf("the newest chunk was evicted with the overflow:\n%s", got)
	}
}

func TestTranscriptEvictsToTheLowWaterMark(t *testing.T) {
	c := newCaptureStream()
	c.history.maxBytes = 100
	c.history.keepBytes = 50

	atPrompt(c, "")
	run(c, "one", strings.Repeat("A", 36), "")
	run(c, "two", strings.Repeat("B", 36), "")
	run(c, "three", strings.Repeat("C", 36), "")

	got := strings.Join(turnTexts(c.recent()), "\n")
	if strings.Contains(got, "AAAA") {
		t.Errorf("the oldest chunk survived a low-water eviction:\n%s", got)
	}
	if strings.Contains(got, "BBBB") {
		t.Errorf("eviction stopped at the cap instead of the low-water mark:\n%s", got)
	}
	if !strings.Contains(got, "CCCC") {
		t.Errorf("the newest chunk was evicted:\n%s", got)
	}
}

func TestTranscriptIsEmptyUntilACommandHasFinished(t *testing.T) {
	c := newCaptureStream()
	if got := c.recent(); len(got) != 0 {
		t.Errorf("a new capture already has a transcript: %+v", got)
	}

	c.feed([]byte("$ " + pasteOn))
	if got := c.recent(); len(got) != 0 {
		t.Errorf("a prompt with no command is a transcript: %+v", got)
	}

	c.feed([]byte("echo hi" + pasteOff + "\nhi\n"))
	if got := c.recent(); len(got) != 0 {
		t.Errorf("a command that has not returned to a prompt is already in the transcript: %+v", got)
	}

	c.feed([]byte("$ " + pasteOn))
	want := []string{cmdTurn("$ ", "echo hi", "hi")}
	if got := turnTexts(c.recent()); !slices.Equal(got, want) {
		t.Errorf("the transcript is:\n%q\nwant:\n%q", got, want)
	}
}

func TestCaptureBuildsNoTranscriptWithoutBracketedPaste(t *testing.T) {
	c := newCaptureStream()
	c.feed([]byte("$ echo hi\nhi\n$ "))
	if got := c.recent(); len(got) != 0 {
		t.Errorf("output with no 2004 marks left %+v in the transcript, want nothing", got)
	}
}

func TestChunkOpensOnCommandStartAndCommitsOnPromptReady(t *testing.T) {
	c := newCaptureStream()

	c.feed([]byte("$ " + pasteOn))
	c.feed([]byte("echo hi"))
	c.feed([]byte(pasteOff + "\nhi\n$ " + pasteOn))

	want := []string{cmdTurn("$ ", "echo hi", "hi")}
	if got := turnTexts(c.recent()); !slices.Equal(got, want) {
		t.Errorf("the transcript is:\n%q\nwant:\n%q", got, want)
	}
}

func TestChunkTakesThePartialLineWhenNewlineArrivesAfterCommandStart(t *testing.T) {
	c := newCaptureStream()

	c.feed([]byte("$ " + pasteOn + "echo hi" + pasteOff + "\nhi\n$ " + pasteOn))

	want := []string{cmdTurn("$ ", "echo hi", "hi")}
	if got := turnTexts(c.recent()); !slices.Equal(got, want) {
		t.Errorf("the transcript is:\n%q\nwant:\n%q", got, want)
	}
}

func TestChunkTakesTheFlushedLineWhenNewlineArrivesBeforeCommandStart(t *testing.T) {
	c := newCaptureStream()

	c.feed([]byte("$ " + pasteOn + "echo hi\n" + pasteOff + "hi\n$ " + pasteOn))

	want := []string{cmdTurn("$ ", "echo hi", "hi")}
	if got := turnTexts(c.recent()); !slices.Equal(got, want) {
		t.Errorf("the transcript is:\n%q\nwant:\n%q", got, want)
	}
}

func TestChunkKeepsAMultilineEchoAsOneCommand(t *testing.T) {
	c := newCaptureStream()

	c.feed([]byte("$ " + pasteOn + "echo 'line1\nline2'" + pasteOff + "\nline1\nline2\n$ " + pasteOn))

	want := []string{cmdTurn("$ ", "echo 'line1\nline2'", "line1\nline2")}
	if got := turnTexts(c.recent()); !slices.Equal(got, want) {
		t.Errorf("the transcript is:\n%q\nwant:\n%q", got, want)
	}
}

func TestChunkKeepsAPipelineAsTheCommand(t *testing.T) {
	c := newCaptureStream()
	atPrompt(c, "$ ")
	run(c, "ls | grep x", "x.go", "$ ")

	want := []string{cmdTurn("$ ", "ls | grep x", "x.go")}
	if got := turnTexts(c.recent()); !slices.Equal(got, want) {
		t.Errorf("the transcript is:\n%q\nwant:\n%q", got, want)
	}
}

func TestChunkIgnoresBracketedPasteOnTheAlternateScreen(t *testing.T) {
	c := newCaptureStream()

	c.feed([]byte("$ " + pasteOn + "vim foo" + pasteOff))
	c.feed([]byte("\x1b[?1049h" + pasteOn + "THE WHOLE OF VIM" + pasteOff + "\x1b[?1049l"))
	c.feed([]byte("$ " + pasteOn))

	got := strings.Join(turnTexts(c.recent()), "\n")
	if strings.Contains(got, "THE WHOLE OF VIM") {
		t.Errorf("the alternate screen leaked into the transcript:\n%s", got)
	}
	if !strings.Contains(got, "vim foo") {
		t.Errorf("the command that took the alternate screen is missing:\n%s", got)
	}
}

func TestTranscriptKeepsANumericOSC133DExitCode(t *testing.T) {
	tests := []struct {
		name string
		d    string
		want string
	}{
		{name: "BEL", d: "\x1b]133;D;1\x07", want: cmdTurn("$ ", "false", "nope") + "\n" + exitPrefix + "1"},
		{name: "ST", d: "\x1b]133;D;1\x1b\\", want: cmdTurn("$ ", "false", "nope") + "\n" + exitPrefix + "1"},
		{name: "zero", d: "\x1b]133;D;0\x07", want: cmdTurn("$ ", "false", "nope") + "\n" + exitPrefix + "0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newCaptureStream()
			atPrompt(c, "$ ")
			c.feed([]byte("false" + pasteOff + "\nnope\n" + test.d + "$ " + pasteOn))
			if got := turnTexts(c.recent()); !slices.Equal(got, []string{test.want}) {
				t.Errorf("the transcript is:\n%q\nwant:\n%q", got, []string{test.want})
			}
		})
	}
}

func TestTranscriptKeepsOSC133DSplitAcrossWrites(t *testing.T) {
	c := newCaptureStream()
	atPrompt(c, "$ ")
	c.feed([]byte("false" + pasteOff + "\nnope\n"))
	c.feed([]byte("\x1b]133;D;1\x07$ " + pasteOn))

	want := []string{cmdTurn("$ ", "false", "nope") + "\n" + exitPrefix + "1"}
	if got := turnTexts(c.recent()); !slices.Equal(got, want) {
		t.Errorf("the transcript is:\n%q\nwant:\n%q", got, want)
	}
}

func TestTranscriptOmitsExitCodeWithoutOSC133D(t *testing.T) {
	c := newCaptureStream()
	atPrompt(c, "$ ")
	run(c, "echo hi", "hi", "$ ")

	want := []string{cmdTurn("$ ", "echo hi", "hi")}
	if got := turnTexts(c.recent()); !slices.Equal(got, want) {
		t.Errorf("the transcript is:\n%q\nwant:\n%q", got, want)
	}
}

func TestTranscriptOmitsExitCodeWhenOSC133DHasNoNumber(t *testing.T) {
	tests := []struct {
		name string
		osc  string
	}{
		{name: "bare D", osc: "\x1b]133;D\x07"},
		{name: "D with empty status", osc: "\x1b]133;D;\x07"},
		{name: "A", osc: "\x1b]133;A\x07"},
		{name: "B", osc: "\x1b]133;B\x07"},
		{name: "C", osc: "\x1b]133;C\x07"},
	}
	want := []string{cmdTurn("$ ", "echo hi", "hi")}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newCaptureStream()
			atPrompt(c, "$ ")
			c.feed([]byte("echo hi" + pasteOff + "\nhi\n" + test.osc + "$ " + pasteOn))
			if got := turnTexts(c.recent()); !slices.Equal(got, want) {
				t.Errorf("the transcript is:\n%q\nwant:\n%q", got, want)
			}
		})
	}
}

func TestTranscriptIgnoresOSC133DOnTheAlternateScreen(t *testing.T) {
	c := newCaptureStream()
	c.feed([]byte("$ " + pasteOn + "vim foo" + pasteOff))
	c.feed([]byte("\x1b[?1049h\x1b]133;D;1\x07THE WHOLE OF VIM\x1b[?1049l"))
	c.feed([]byte("$ " + pasteOn))

	got := strings.Join(turnTexts(c.recent()), "\n")
	if strings.Contains(got, "exit code =") {
		t.Errorf("an OSC 133 D on the alternate screen was kept:\n%s", got)
	}
	if strings.Contains(got, "THE WHOLE OF VIM") {
		t.Errorf("the alternate screen leaked into the transcript:\n%s", got)
	}
}

func TestTranscriptIgnoresOSC133DWithNoOpenChunk(t *testing.T) {
	c := newCaptureStream()
	c.feed([]byte("\x1b]133;D;1\x07$ " + pasteOn))
	run(c, "echo hi", "hi", "$ ")

	want := []string{cmdTurn("$ ", "echo hi", "hi")}
	if got := turnTexts(c.recent()); !slices.Equal(got, want) {
		t.Errorf("an OSC 133 D before any command labelled the first turn:\n%q\nwant:\n%q", got, want)
	}
}

// TestCaptureRecordsARealCommand is the end-to-end check for the
// bracketed-paste machine: a real shell writes 2004 h/l around a command,
// and the transcript holds the echoed line and what it printed.
func TestCaptureRecordsARealCommand(t *testing.T) {
	w := watchShell(t, interactiveShell(t))
	awaitGate(t, w, gate.open, "at a fresh prompt")

	w.press(t, "echo CAPTURE_MARK=ok\n")
	awaitOutput(t, w, "CAPTURE_MARK=ok")
	awaitGate(t, w, gate.open, "back at the prompt")

	var text string
	for deadline := time.Now().Add(answerTimeout); ; {
		text = strings.Join(turnTexts(w.state.transcript()), "\n")
		if strings.Contains(text, "echo CAPTURE_MARK=ok") && strings.Contains(text, "CAPTURE_MARK=ok") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the transcript never held the command and its output, it was:\n%s\nthe session output was:\n%s",
				text, w.out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestArgvReadsThisProcess(t *testing.T) {
	args, err := argv(os.Getpid())
	if err != nil {
		t.Fatalf("reading this process's argv: %v", err)
	}
	if len(args) == 0 {
		t.Fatal("this process has no argv")
	}
	if filepath.Base(args[0]) != filepath.Base(os.Args[0]) {
		t.Errorf("argv[0] is %q, want a path ending like %q", args[0], os.Args[0])
	}
	if len(args) != len(os.Args) {
		t.Errorf("argc is %d (%q), want %d (%q)", len(args), args, len(os.Args), os.Args)
		return
	}
	for i := 1; i < len(os.Args); i++ {
		if args[i] != os.Args[i] {
			t.Errorf("argv[%d] is %q, want %q", i, args[i], os.Args[i])
		}
	}
}

func TestParseProcargs2(t *testing.T) {
	buf := make([]byte, 4)
	binary.NativeEndian.PutUint32(buf, 2)
	buf = append(buf, "/bin/sleep"...)
	buf = append(buf, 0, 0, 0)
	buf = append(buf, "sleep"...)
	buf = append(buf, 0)
	buf = append(buf, "30"...)
	buf = append(buf, 0)

	args, err := parseProcargs2(buf)
	if err != nil {
		t.Fatalf("parseProcargs2: %v", err)
	}
	if got, want := strings.Join(args, " "), "sleep 30"; got != want {
		t.Errorf("argv is %q, want %q", got, want)
	}
}

func TestParseProcargs2RejectsGarbage(t *testing.T) {
	for _, data := range [][]byte{nil, {1, 2}, {0, 0, 0, 0}, append([]byte{1, 0, 0, 0}, "no-nul"...)} {
		if args, err := parseProcargs2(data); err == nil {
			t.Errorf("parseProcargs2(%q) = %q, want error", data, args)
		}
	}
}

func TestParseCmdline(t *testing.T) {
	args, err := parseCmdline([]byte("sleep\x0030\x00"))
	if err != nil {
		t.Fatalf("parseCmdline: %v", err)
	}
	if got, want := strings.Join(args, " "), "sleep 30"; got != want {
		t.Errorf("argv is %q, want %q", got, want)
	}

	args, err = parseCmdline([]byte("printf\x00\x00ok\x00"))
	if err != nil {
		t.Fatalf("parseCmdline with an empty argument: %v", err)
	}
	if len(args) != 3 || args[1] != "" || args[2] != "ok" {
		t.Errorf("empty argument was dropped: %q", args)
	}
}

func TestParseCmdlineRejectsEmpty(t *testing.T) {
	if args, err := parseCmdline(nil); err == nil {
		t.Errorf("parseCmdline(nil) = %q, want error", args)
	}
}

func TestAnAnsweredCommandKeepsTheAnswerItCameFrom(t *testing.T) {
	c := newCaptureStream()
	atPrompt(c, "$ ")
	c.answered("push it anyway", "git push --force-with-lease", false)
	run(c, "git push --force-with-lease", "Everything up-to-date", "$ ")

	turns := c.recent()
	if len(turns) != 1 {
		t.Fatalf("the transcript has %d turns, want the one command that ran", len(turns))
	}
	if turns[0].Question != "push it anyway" {
		t.Errorf("the turn does not keep the question that produced the answer: %+v", turns[0])
	}
	if turns[0].Answer != "git push --force-with-lease" {
		t.Errorf("the turn does not keep the answer the command came from: %+v", turns[0])
	}
	if !strings.Contains(turns[0].Text, "$ git push --force-with-lease") || !strings.Contains(turns[0].Text, "Everything up-to-date") {
		t.Errorf("the turn does not hold the prompt, the command that ran and what it printed:\n%s", turns[0].Text)
	}
	if turns[0].Tool {
		t.Error("a ? answer was marked as a tool call")
	}
}

func TestAnAgentAnswerIsMarkedAsAToolTurn(t *testing.T) {
	c := newCaptureStream()
	atPrompt(c, "$ ")
	c.answered("list tmp", "ls /tmp", true)
	run(c, "ls /tmp", "file.txt", "$ ")

	turns := c.recent()
	if len(turns) != 1 {
		t.Fatalf("the transcript has %d turns, want the one command that ran", len(turns))
	}
	if !turns[0].Tool {
		t.Errorf("a ??? command was not marked as a tool call: %+v", turns[0])
	}
	if turns[0].Answer != "ls /tmp" {
		t.Errorf("the turn lost the command: %+v", turns[0])
	}
}

// TestAnAnswerThrownAwayIsKeptAsItsOwnTurn is the interrupt-key half of an
// answer's journey: the line the answer was typed into was thrown away, so
// the command never ran, but the next question can still be about it, so the
// answer stays as a turn of its own rather than vanishing with the line.
func TestAnAnswerThrownAwayIsKeptAsItsOwnTurn(t *testing.T) {
	c := newCaptureStream()
	atPrompt(c, "$ ")
	c.answered("push it anyway", "git push --force-with-lease", false)
	c.typed([]byte{ctrlC})
	c.feed([]byte(pasteOff + "\n^C\n$ " + pasteOn))

	run(c, "echo hi", "hi", "$ ")

	turns := c.recent()
	if len(turns) != 2 {
		t.Fatalf("the transcript has %d turns, want the thrown-away answer and the command that ran", len(turns))
	}
	if turns[0].Question != "push it anyway" {
		t.Errorf("the turn lost the question that produced the thrown-away answer: %+v", turns[0])
	}
	if turns[0].Answer != "git push --force-with-lease" {
		t.Errorf("the turn lost the answer that was thrown away: %+v", turns[0])
	}
	if turns[0].Text != "" {
		t.Errorf("an answer that never ran has a command turn: %q", turns[0].Text)
	}
	if turns[1].Answer != "" || turns[1].Question != "" {
		t.Errorf("a command typed after the answer was thrown away kept the answer: %+v", turns[1])
	}
}

// TestCtrlCAfterAnAnswerDoesNotRecordACommandThatNeverRan is the 2004 l
// that follows the interrupt: zsh leaves zle, and that used to store
// the cancelled line as a command that ran. The
// assistant turn stays; only that user turn is dropped.
func TestCtrlCAfterAnAnswerDoesNotRecordACommandThatNeverRan(t *testing.T) {
	c := newCaptureStream()
	atPrompt(c, "$ ")
	c.answered("what is in current folder", "ls", false)
	c.feed([]byte("ls"))
	c.typed([]byte{ctrlC})
	c.feed([]byte(pasteOff + "\n^C\n$ " + pasteOn))

	turns := c.recent()
	if len(turns) != 1 {
		t.Fatalf("the transcript has %d turns, want only the answer that never ran: %+v", len(turns), turns)
	}
	if turns[0].Question != "what is in current folder" || turns[0].Answer != "ls" {
		t.Errorf("the assistant turn was lost: %+v", turns[0])
	}
	if turns[0].Text != "" {
		t.Errorf("a command that never ran has a user turn: %q", turns[0].Text)
	}
}

// TestCtrlCDoesNotRecordATypedLineAsACommandThatRan is the same 2004 l
// on a line the user typed themselves: cancelling it is not running it.
func TestCtrlCDoesNotRecordATypedLineAsACommandThatRan(t *testing.T) {
	c := newCaptureStream()
	atPrompt(c, "$ ")
	c.feed([]byte("ls"))
	c.typed([]byte{ctrlC})
	c.feed([]byte(pasteOff + "\n^C\n$ " + pasteOn))

	if got := c.recent(); len(got) != 0 {
		t.Errorf("a typed line cancelled with Ctrl-C landed in the transcript: %+v", got)
	}
}

// TestAnAnswerErasedBeforeEnterIsKeptAsItsOwnTurn is the other way an answer
// never runs: it was backspaced out of the line and Enter handed the shell
// an empty one. That 2004 l opens no chunk, but a pending answer is still
// kept as a turn rather than vanishing with the line.
func TestAnAnswerErasedBeforeEnterIsKeptAsItsOwnTurn(t *testing.T) {
	c := newCaptureStream()
	atPrompt(c, "$ ")
	c.answered("show docker images", "docker image ls", false)
	run(c, "", "", "$ ")

	turns := c.recent()
	if len(turns) != 1 {
		t.Fatalf("the transcript has %d turns, want the answer that never ran", len(turns))
	}
	if turns[0].Question != "show docker images" {
		t.Errorf("the turn lost the question that produced the thrown-away answer: %+v", turns[0])
	}
	if turns[0].Answer != "docker image ls" {
		t.Errorf("the turn lost the answer that was thrown away: %+v", turns[0])
	}
	if turns[0].Text != "" {
		t.Errorf("an answer that never ran has a command turn: %q", turns[0].Text)
	}
}

func TestACommandTheUserTypedHasNoAnswer(t *testing.T) {
	c := newCaptureStream()
	atPrompt(c, "$ ")
	run(c, "echo hi", "hi", "$ ")

	turns := c.recent()
	if len(turns) != 1 {
		t.Fatalf("the transcript has %d turns, want the one command that ran", len(turns))
	}
	if turns[0].Answer != "" || turns[0].Question != "" {
		t.Errorf("a typed command was labelled as the model's own: %+v", turns[0])
	}
}

func TestCaptureOutputDoesNotReplayCursorEdits(t *testing.T) {
	for _, test := range []struct{ output, want string }{
		{"10%\r20%", "10%\r20%"},
		{"abcdef\b\bXY", "abcdef\b\bXY"},
		{"abc\x1b[2DXY", "abcXY"},
		{"abc\r\x1b[Kdone", "abc\rdone"},
		{"a\tb", "a\tb"},
	} {
		c := newCaptureStream()
		atPrompt(c, "$ ")
		run(c, "cat", test.output, "$ ")
		if got := turnTexts(c.recent()); !slices.Equal(got, []string{"$ cat\n" + test.want}) {
			t.Fatalf("output %q: captured %q, want %q", test.output, got, test.want)
		}
	}
}

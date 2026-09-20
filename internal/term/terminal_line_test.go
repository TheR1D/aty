//go:build darwin || linux

package term

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	appconfig "github.com/TheR1D/aty/internal/config"
)

func (l *linebuf) feed(p []byte) (lines []string) {
	if l.maxCells <= 0 {
		l.maxCells = appconfig.DefaultCommandBytes
	}
	for _, b := range p {
		if line, ok := l.consume(b); ok {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestTerminalLineIsChunkInvariant(t *testing.T) {
	streams := [][]byte{
		[]byte("caf\xc3\xa9\n"),
		[]byte("progress 10%\rprogress 100%\n"),
		[]byte("\x1b[32mgreen\x1b[0m\n"),
		[]byte("abc\x1b[2Dxy\n"),
		[]byte("\x1b]133;D;17\x1b\\done\n"),
		{'x', 0xff, '\n'},
		append(append([]byte("\x1b["), bytes.Repeat([]byte{'1'}, maxParams+8)...), []byte("mplain\n")...),
	}
	for _, stream := range streams {
		var whole linebuf
		wantLines := whole.feed(stream)
		wantText := whole.text()
		wantExit, wantHasExit := whole.takeExit()

		for split := 0; split <= len(stream); split++ {
			var chunked linebuf
			gotLines := append(chunked.feed(stream[:split]), chunked.feed(stream[split:])...)
			gotExit, gotHasExit := chunked.takeExit()
			if !slices.Equal(gotLines, wantLines) || chunked.text() != wantText ||
				gotExit != wantExit || gotHasExit != wantHasExit {
				t.Fatalf("split %d of %q changed result: lines=%q text=%q exit=%d/%t; want lines=%q text=%q exit=%d/%t",
					split, stream, gotLines, chunked.text(), gotExit, gotHasExit,
					wantLines, wantText, wantExit, wantHasExit)
			}
		}
	}
}

func TestTerminalLineBoundsStorage(t *testing.T) {
	var line linebuf
	line.feed(bytes.Repeat([]byte{'x'}, appconfig.DefaultCommandBytes*2))
	if len(line.cells) != appconfig.DefaultCommandBytes {
		t.Fatalf("held %d cells, want bound %d", len(line.cells), appconfig.DefaultCommandBytes)
	}
	line.feed(append(append([]byte("\x1b]"), bytes.Repeat([]byte{'x'}, maxParams*2)...), '\a'))
	if line.nparams > maxParams {
		t.Fatalf("held %d control bytes, want at most %d", line.nparams, maxParams)
	}
}

func TestPromptLineStorageSurvivesControlsAndReset(t *testing.T) {
	line := linebuf{maxCells: 64}
	for _, text := range []string{strings.Repeat("abcdef", 1000), "\tXYZ", "\x1b[99999999999999999999Cxyz", "\x1b[999Gxyz", "\x1b[1;999Hxyz", strings.Repeat("é界🙂", 1000)} {
		line.reset()
		line.feed([]byte(text))
		if len(line.cells) > 64 || line.maxCells != 64 {
			t.Fatalf("%q exceeded or reset custom limit", text)
		}
	}
}

//go:build darwin || linux

package term

import (
	"bytes"
	"fmt"
	appconfig "github.com/TheR1D/aty/internal/config"
	"io"
	"strings"
	"testing"
)

func BenchmarkOutputObservation(b *testing.B) {
	for _, test := range []struct {
		name string
		data []byte
		alt  bool
	}{
		{"plain", bytes.Repeat([]byte(strings.Repeat("a", 80)+"\r\n"), 400), false},
		{"color", bytes.Repeat([]byte("\x1b[32mhello\x1b[0m world\r\n"), 1400), false},
		{"alternate", bytes.Repeat([]byte("\x1b[32mhello\x1b[0m world\r\n"), 1400), true},
	} {
		b.Run(test.name, func(b *testing.B) {
			s := newState(0, nil, appconfig.Limits{})
			s.capture.open = &chunk{command: "cat"}
			if test.alt {
				s.observeOutput([]byte("\x1b[?1049h"))
			}
			b.SetBytes(int64(len(test.data)))
			b.ReportAllocs()
			for b.Loop() {
				s.observeOutput(test.data)
			}
		})
	}
}

func BenchmarkTranscriptTrim(b *testing.B) {
	for _, width := range []int{80, 4096} {
		b.Run(fmt.Sprint(width), func(b *testing.B) {
			line := strings.Repeat("a", width)
			b.ReportAllocs()
			for b.Loop() {
				c := chunk{command: "cat"}
				for range 256 {
					c.addLine(line)
				}
				c.trim(appconfig.DefaultCommandBytes)
			}
		})
	}
}

func BenchmarkStreamingCommand(b *testing.B) {
	commands := make([]string, 256)
	for i := range commands {
		commands[i] = "echo " + strings.Repeat("a", (i+1)*8)
	}
	k := &keyboard{state: &state{}, master: io.Discard}
	b.ReportAllocs()
	for b.Loop() {
		q := &query{}
		var inject injection
		for _, command := range commands {
			if err := k.revise(q, &inject, command); err != nil {
				b.Fatal(err)
			}
		}
	}
}

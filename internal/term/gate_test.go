//go:build darwin || linux

package term

import (
	"errors"
	"testing"
)

type fixedForeground struct {
	pgid int
	err  error
}

func (f fixedForeground) Foreground() (int, error) { return f.pgid, f.err }

func TestProcessGateFailsClosed(t *testing.T) {
	readErr := errors.New("unreadable")
	tests := []struct {
		name       string
		reader     foregroundReader
		prompt     bool
		wantPrompt bool
		wantErr    bool
	}{
		{name: "shell owns terminal", reader: fixedForeground{pgid: 42}, prompt: true, wantPrompt: true},
		{name: "child owns terminal", reader: fixedForeground{pgid: 999999}, prompt: false},
		{name: "reading failed", reader: fixedForeground{err: readErr}, prompt: true, wantErr: true},
		{name: "not probed", prompt: true, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			process := processGate{shellPID: 42, pgrp: test.reader}
			got := process.current(test.prompt)
			if got.atPrompt != test.wantPrompt {
				t.Errorf("atPrompt = %t, want %t", got.atPrompt, test.wantPrompt)
			}
			if (got.err != nil) != test.wantErr {
				t.Errorf("error = %v, want error %t", got.err, test.wantErr)
			}
		})
	}
}

//go:build darwin || linux

package term

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// processGate owns the live process information used to decide whether input
// still belongs to the shell, or to a supported relay with a remote prompt.
type processGate struct {
	shellPID int
	ptmx     *os.File

	once sync.Once
	mu   sync.Mutex
	pgrp foregroundReader
}

type foregroundReader interface {
	Foreground() (int, error)
}

func (p *processGate) probe() {
	p.once.Do(func() {
		if p.ptmx == nil {
			return
		}
		reader, err := ProbePgrp(p.ptmx, p.shellPID)
		if err != nil {
			return
		}
		p.mu.Lock()
		p.pgrp = reader
		p.mu.Unlock()
	})
}

func (p *processGate) current(promptReady bool) gate {
	p.mu.Lock()
	reader := p.pgrp
	p.mu.Unlock()

	var result gate
	if reader == nil {
		result.err = errors.New("the foreground process group is not being read")
	} else {
		result.foreground, result.err = reader.Foreground()
		result.atPrompt = result.err == nil && result.foreground == p.shellPID
	}
	if !result.atPrompt && result.err == nil && promptReady {
		result.relay = relayProcess(result.foreground)
	}
	return result
}

// gate is a diagnostic snapshot. open is deliberately fail-closed: all
// process, terminal-mode, prompt, and line signals must agree.
type gate struct {
	foreground  int
	atPrompt    bool
	relay       bool
	promptReady bool
	altScreen   bool
	freshLine   bool
	err         error
}

func (g gate) open() bool {
	return g.promptReady && !g.altScreen && g.freshLine && (g.atPrompt || g.relay)
}

func relayName(name string) bool {
	switch name {
	case "ssh", "mosh", "docker", "podman", "kubectl":
		return true
	default:
		return false
	}
}

func relayArgv(args []string) bool {
	return len(args) > 0 && relayName(filepath.Base(args[0]))
}

func relayProcess(pid int) bool {
	args, err := argv(pid)
	return err == nil && relayArgv(args)
}

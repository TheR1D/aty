//go:build darwin || linux

package term

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	xterm "golang.org/x/term"

	appconfig "github.com/TheR1D/aty/internal/config"
	"github.com/TheR1D/aty/internal/llm"
)

// Config describes the shell session hosted by Run.
type Config struct {
	Limits appconfig.Limits
	Color  appconfig.Color
	Shell  string
	Args   []string
	Env    []string
	In     *os.File
	Out    *os.File

	// InitialPrompt runs once before the first query, with the startup prompt.
	InitialPrompt func(string)

	Ask   assistant
	Warm  warmupFunc
	Clear func()
	Agent agentFunc
	Dump  func(question string, transcript []llm.Turn) string
}

// Run hosts an interactive shell and returns the shell's exit status.
func Run(cfg Config) (int, error) {
	return RunContext(context.Background(), cfg)
}

// RunContext hosts an interactive shell until it exits or ctx is cancelled.
func RunContext(ctx context.Context, cfg Config) (int, error) {
	cfg = cfg.defaults()
	if !xterm.IsTerminal(int(cfg.In.Fd())) {
		return 0, errors.New("term: input is not a terminal")
	}

	s := &session{
		cfg:    cfg,
		screen: newScreen(cfg.Out, cfg.Color),
	}
	defer s.close()
	s.stopSignals = s.watchSignals()
	if err := s.enterRaw(); err != nil {
		return 0, err
	}
	if err := s.start(); err != nil {
		return 0, err
	}
	return s.serve(ctx)
}

type session struct {
	cfg    Config
	screen *screen

	mu          sync.Mutex
	ptmx        *os.File
	saved       *xterm.State
	raw         bool
	cmd         *exec.Cmd
	state       *state
	stopSignals func()
	closeOnce   sync.Once
}

func (s *session) start() error {
	args := s.cfg.Args
	if filepath.Base(s.cfg.Shell) == "zsh" {
		// Set the option in the child shell without modifying the user's startup files.
		args = append([]string{"-o", "interactivecomments"}, args...)
	}
	cmd := exec.Command(s.cfg.Shell, args...)
	// Set the marker only for the hosted shell and its descendants.
	cmd.Env = append(slices.Clone(s.cfg.Env), activeEnv+"=1")

	size := &pty.Winsize{Rows: 24, Cols: 80}
	if current, err := pty.GetsizeFull(s.cfg.In); err == nil {
		size = current
	}

	master, err := pty.StartWithSize(cmd, size)
	if err != nil {
		return fmt.Errorf("term: starting %s: %w", s.cfg.Shell, err)
	}

	s.mu.Lock()
	s.ptmx = master
	s.cmd = cmd
	s.mu.Unlock()

	state := newState(cmd.Process.Pid, master, s.cfg.Limits)
	state.onClear = s.cfg.Clear
	s.state = state
	return nil
}

func (s *session) resize() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ptmx == nil {
		return nil
	}
	if err := pty.InheritSize(s.cfg.In, s.ptmx); err != nil {
		return fmt.Errorf("term: resizing pty: %w", err)
	}
	return nil
}

func (s *session) closePTY() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ptmx == nil {
		return
	}
	_ = s.ptmx.Close()
	s.ptmx = nil
}

func (s *session) terminate() {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = unix.Kill(-s.cmd.Process.Pid, unix.SIGHUP)
	}
	s.closePTY()
}

func (s *session) close() {
	s.closeOnce.Do(func() {
		if s.stopSignals != nil {
			s.stopSignals()
		}
		s.closePTY()
		s.restoreTerminal()
	})
}

func (s *session) enterRaw() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := xterm.MakeRaw(int(s.cfg.In.Fd()))
	if err != nil {
		return fmt.Errorf("term: enabling raw mode: %w", err)
	}
	if s.saved == nil {
		s.saved = state
	}
	s.raw = true
	return nil
}

func (s *session) restoreTerminal() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.raw {
		return
	}
	if s.screen != nil {
		s.screen.resetCursorColor()
	}
	_ = xterm.Restore(int(s.cfg.In.Fd()), s.saved)
	s.raw = false
}

func (s *session) restoreOnPanic() {
	if value := recover(); value != nil {
		s.restoreTerminal()
		panic(value)
	}
}

func (cfg Config) defaults() Config {
	if cfg.Shell == "" {
		cfg.Shell = DefaultShell()
	}
	if cfg.Env == nil {
		cfg.Env = os.Environ()
	}
	if cfg.In == nil {
		cfg.In = os.Stdin
	}
	if cfg.Out == nil {
		cfg.Out = os.Stdout
	}
	return cfg
}

// DefaultShell returns $SHELL, falling back to /bin/sh.
func DefaultShell() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "/bin/sh"
}

func exitStatus(process *os.ProcessState) int {
	status := process.Sys().(syscall.WaitStatus)
	if status.Signaled() {
		return 128 + int(status.Signal())
	}
	return process.ExitCode()
}

//go:build darwin || linux

package term

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Pgrp reads a pty's foreground process group through its master ioctl or,
// when that source is unusable, the shell's process-table entry.
type Pgrp struct {
	ptmx *os.File
	// Setsid makes the shell its own process-group and session leader.
	shellPid int
	fallback bool
}

// ProbePgrp selects a usable foreground process-group reader. The shell must
// have been started with Setsid and the pty slave as its controlling terminal.
// Call after its first prompt, when it owns the terminal.
//
// Prefer the ioctl when the process table agrees or cannot be read. If both
// sources report live groups but disagree, trust the process table.
func ProbePgrp(ptmx *os.File, shellPid int) (*Pgrp, error) {
	p := &Pgrp{ptmx: ptmx, shellPid: shellPid}

	byIoctl, errIoctl := p.foregroundIoctl()
	byProc, errProc := tpgid(shellPid)
	ioctlOK := errIoctl == nil && isLivePgid(byIoctl)
	procOK := errProc == nil && isLivePgid(byProc)
	agreed := ioctlOK && procOK && byIoctl == byProc

	switch {
	case agreed, ioctlOK && !procOK:
		return p, nil
	case procOK:
		p.fallback = true
		return p, nil
	}
	return nil, fmt.Errorf("term: no usable foreground process group for pid %d: ioctl: %v; process table: %v",
		shellPid, readingErr(byIoctl, errIoctl), readingErr(byProc, errProc))
}

// Foreground returns the process group that currently owns the pty.
func (p *Pgrp) Foreground() (int, error) {
	if p.fallback {
		return tpgid(p.shellPid)
	}
	return p.foregroundIoctl()
}

// UsesFallback reports whether readings come from the process table.
func (p *Pgrp) UsesFallback() bool { return p.fallback }

func (p *Pgrp) foregroundIoctl() (int, error) {
	conn, err := p.ptmx.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("pty master syscall conn: %w", err)
	}
	var (
		pgid     int
		ioctlErr error
	)
	// TIOCGPGRP writes a four-byte pid_t into IoctlGetInt's int. On big-endian
	// machines ProbePgrp rejects an implausible result and uses the fallback.
	if err := conn.Control(func(fd uintptr) {
		pgid, ioctlErr = unix.IoctlGetInt(int(fd), unix.TIOCGPGRP)
	}); err != nil {
		return 0, fmt.Errorf("pty master control: %w", err)
	}
	if ioctlErr != nil {
		return 0, fmt.Errorf("ioctl(ptmx, TIOCGPGRP): %w", ioctlErr)
	}
	return pgid, nil
}

// A successful read can still return a placeholder when no group owns the tty.
func isLivePgid(pgid int) bool {
	if pgid <= 1 {
		return false
	}
	return unix.Kill(-pgid, 0) == nil
}

func readingErr(pgid int, err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("no such process group: %d", pgid)
}

package term

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// tpgid reads the controlling terminal's foreground process group without
// spawning ps; this fallback may be called for every intercepted keystroke.
func tpgid(pid int) (int, error) {
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, fmt.Errorf("term: sysctl kern.proc.pid.%d: %w", pid, err)
	}
	return int(proc.Eproc.Tpgid), nil
}

// ppid is the parent of pid, read from the process table.
func ppid(pid int) (int, error) {
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, fmt.Errorf("term: sysctl kern.proc.pid.%d: %w", pid, err)
	}
	return int(proc.Eproc.Ppid), nil
}

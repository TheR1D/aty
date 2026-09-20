package term

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// argv reads kernel process arguments for relay and ancestry checks.
func argv(pid int) ([]string, error) {
	data, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, fmt.Errorf("term: sysctl kern.procargs2.%d: %w", pid, err)
	}
	args, err := parseProcargs2(data)
	if err != nil {
		return nil, fmt.Errorf("term: parse kern.procargs2.%d: %w", pid, err)
	}
	return args, nil
}

package term

import (
	"fmt"
	"os"
)

// argv reads process arguments from /proc for relay and ancestry checks.
func argv(pid int) ([]string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, fmt.Errorf("term: read /proc/%d/cmdline: %w", pid, err)
	}
	args, err := parseCmdline(data)
	if err != nil {
		return nil, fmt.Errorf("term: parse /proc/%d/cmdline: %w", pid, err)
	}
	return args, nil
}

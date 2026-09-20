package term

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
)

// tpgid returns the foreground process group of the controlling terminal of
// pid, read from the process table.
func tpgid(pid int) (int, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, fmt.Errorf("term: read process stat: %w", err)
	}
	pgid, err := parseTpgid(stat)
	if err != nil {
		return 0, fmt.Errorf("term: parse /proc/%d/stat: %w", pid, err)
	}
	return pgid, nil
}

// parseTpgid returns field 8, tpgid, of a /proc/<pid>/stat line.
func parseTpgid(stat []byte) (int, error) {
	return parseProcStatField(stat, 8)
}

// ppid is the parent of pid, read from /proc.
func ppid(pid int) (int, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, fmt.Errorf("term: read process stat: %w", err)
	}
	parent, err := parsePpid(stat)
	if err != nil {
		return 0, fmt.Errorf("term: parse /proc/%d/stat ppid: %w", pid, err)
	}
	return parent, nil
}

// parsePpid returns field 4, ppid, of a /proc/<pid>/stat line.
func parsePpid(stat []byte) (int, error) {
	return parseProcStatField(stat, 4)
}

// The executable name (field 2) is parenthesized but unescaped, so it may
// contain spaces and parentheses. Numeric fields start after the final ')'.
func parseProcStatField(stat []byte, field int) (int, error) {
	_, tail, ok := bytes.CutLast(stat, []byte{')'})
	if !ok {
		return 0, errors.New("no comm field")
	}
	index := field - 3
	fields := bytes.Fields(tail)
	if len(fields) <= index {
		return 0, fmt.Errorf("want %d fields after comm, have %d", index+1, len(fields))
	}
	value, err := strconv.Atoi(string(fields[index]))
	if err != nil {
		return 0, fmt.Errorf("field %d: %w", field, err)
	}
	return value, nil
}

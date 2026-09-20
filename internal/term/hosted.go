//go:build darwin || linux

package term

import (
	"os"
	"path/filepath"
)

const maxProcessAncestors = 256
const activeEnv = "ATY_ACTIVE"

type processLookup func(pid int) (args []string, parent int, err error)

// Hosted reports whether this process was launched from inside an aty
// session: it inherits the session marker or some ancestor is aty itself.
// Nesting sessions would put the terminal in raw mode twice and intercept the
// same keystrokes.
func Hosted() bool {
	return os.Getenv(activeEnv) == "1" || hostedFrom(os.Getppid())
}

func hostedFrom(pid int) bool {
	return hostedAncestry(pid, lookupProcess)
}

func hostedAncestry(pid int, lookup processLookup) bool {
	for depth := 0; pid > 1 && depth < maxProcessAncestors; depth++ {
		args, parent, err := lookup(pid)
		if err != nil {
			return false
		}
		if atyArgv(args) {
			return true
		}
		if parent <= 0 || parent == pid {
			return false
		}
		pid = parent
	}
	return false
}

func lookupProcess(pid int) ([]string, int, error) {
	args, _ := argv(pid)
	parent, err := ppid(pid)
	if err != nil {
		return nil, 0, err
	}
	return args, parent, nil
}

func atyArgv(args []string) bool {
	return len(args) > 0 && filepath.Base(args[0]) == "aty"
}

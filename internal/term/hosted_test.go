//go:build darwin || linux

package term

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
)

const hostedEnv = "ATY_TEST_HOSTED"

type fakeProcess struct {
	args   []string
	parent int
	err    error
}

func TestHostedRecognizesActiveEnvironment(t *testing.T) {
	t.Setenv(activeEnv, "1")
	if !Hosted() {
		t.Fatal("inherited ATY_ACTIVE=1 was not recognized")
	}
}

func TestHostedAncestry(t *testing.T) {
	lookup := func(tree map[int]fakeProcess) processLookup {
		return func(pid int) ([]string, int, error) {
			process, ok := tree[pid]
			if !ok {
				return nil, 0, errors.New("missing process")
			}
			return process.args, process.parent, process.err
		}
	}

	tests := []struct {
		name string
		tree map[int]fakeProcess
		want bool
	}{
		{name: "direct parent", tree: map[int]fakeProcess{
			10: {args: []string{"/usr/bin/aty"}, parent: 1},
		}, want: true},
		{name: "shell between", tree: map[int]fakeProcess{
			10: {args: []string{"zsh"}, parent: 9},
			9:  {args: []string{"aty"}, parent: 1},
		}, want: true},
		{name: "ordinary ancestry", tree: map[int]fakeProcess{
			10: {args: []string{"zsh"}, parent: 1},
		}},
		{name: "lookup failure", tree: map[int]fakeProcess{
			10: {err: errors.New("gone")},
		}},
		{name: "cycle", tree: map[int]fakeProcess{
			10: {args: []string{"zsh"}, parent: 11},
			11: {args: []string{"sh"}, parent: 10},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hostedAncestry(10, lookup(test.tree)); got != test.want {
				t.Fatalf("hostedAncestry = %t, want %t", got, test.want)
			}
		})
	}
}

func TestAtyArgvRecognizesAty(t *testing.T) {
	for _, args := range [][]string{
		{"aty"},
		{"/usr/local/bin/aty"},
		{"/Users/me/Developer/personal/aty/bin/aty", "--help"},
	} {
		if !atyArgv(args) {
			t.Errorf("atyArgv(%q) is false, want aty recognised", args)
		}
	}
	for _, args := range [][]string{
		nil,
		{},
		{"zsh"},
		{"/bin/zsh"},
		{"aty.test"},
		{"/tmp/aty.test"},
		{"caty"},
		{"aty-helper"},
		{"/usr/bin/caty"},
	} {
		if atyArgv(args) {
			t.Errorf("atyArgv(%q) is true, want it refused", args)
		}
	}
}

func TestPpidReadsThisProcess(t *testing.T) {
	got, err := ppid(os.Getpid())
	if err != nil {
		t.Fatalf("reading this process's parent: %v", err)
	}
	if got != os.Getppid() {
		t.Errorf("ppid(%d) = %d, want %d", os.Getpid(), got, os.Getppid())
	}
}

// TestHostedSeesAParentAty is a process-tree check. The parent has to be a
// real process whose argv is aty, because that is what a nested `aty` typed
// at the hosted shell looks like: aty, then the shell, then aty again.
func TestHostedSeesAParentAty(t *testing.T) {
	t.Setenv(activeEnv, "")
	switch os.Getenv(hostedEnv) {
	case "parent":
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		child := exec.Command(exe, "-test.run=^"+t.Name()+"$")
		child.Env = append(os.Environ(), hostedEnv+"=child")
		child.Stderr = os.Stderr
		if err := child.Run(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	case "child":
		if !Hosted() {
			fmt.Fprintln(os.Stderr, "Hosted is false under a parent named aty")
			os.Exit(1)
		}
		os.Exit(0)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	parent := exec.Command(exe, "-test.run=^"+t.Name()+"$")
	parent.Args[0] = "aty"
	parent.Env = append(os.Environ(), hostedEnv+"=parent")
	parent.Stderr = os.Stderr
	if err := parent.Run(); err != nil {
		t.Fatalf("a child of a process named aty did not see itself as hosted: %v", err)
	}
}

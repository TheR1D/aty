package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/TheR1D/aty/internal/llm"
	appmcp "github.com/TheR1D/aty/internal/mcp"
	"github.com/TheR1D/aty/internal/term"
)

const nestedEnv = "ATY_TEST_NESTED"

// TestRunSkipsANestedSession is typing `aty` at a prompt aty already hosts:
// the new process is a grandchild of the session, and starting another copy
// would wrap the same terminal twice.
func TestRunSkipsANestedSession(t *testing.T) {
	switch os.Getenv(nestedEnv) {
	case "parent":
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		child := exec.Command(exe, "-test.run=^"+t.Name()+"$")
		child.Env = append(os.Environ(), nestedEnv+"=child")
		child.Stderr = os.Stderr
		if err := child.Run(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	case "child":
		var stderr bytes.Buffer
		code := run(nil, &stderr)
		if code != 0 {
			fmt.Fprintf(os.Stderr, "run exited %d, stderr=%q\n", code, stderr.String())
			os.Exit(1)
		}
		if !strings.Contains(stderr.String(), "already running") {
			fmt.Fprintf(os.Stderr, "stderr=%q, want already running\n", stderr.String())
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Configuration is validated before the ancestry check. Give the child
	// valid settings without relying on the developer's config files.
	isolateConfig(t)
	validArgs(t)
	t.Setenv("ATY_ACTIVE", "")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	parent := exec.Command(exe, "-test.run=^"+t.Name()+"$")
	parent.Args[0] = "aty"
	parent.Env = append(os.Environ(), nestedEnv+"=parent")
	parent.Stderr = os.Stderr
	if err := parent.Run(); err != nil {
		t.Fatalf("aty launched from aty started another session: %v", err)
	}
}

func TestExecutePropagatesTheShellExitCode(t *testing.T) {
	isolateConfig(t)
	t.Setenv("ATY_COLOR", "none")
	var started bool
	deps := dependencies{
		hosted: func() bool { return false },
		prompts: func(_, _ string, _ io.Writer) (llm.Prompts, error) {
			return llm.Prompts{System: "system", Agent: "agent"}, nil
		},
		mcp: loadMCP,
		terminal: func(cfg term.Config) (int, error) {
			started = cfg.Ask != nil && cfg.Agent != nil && cfg.Dump != nil
			if cfg.Color != "none" {
				t.Errorf("terminal color = %q, want none from ATY_COLOR", cfg.Color)
			}
			if _, err := os.Stat(mcpConfigPath()); err != nil {
				t.Errorf("MCP config was not created before terminal startup: %v", err)
			}
			return 37, nil
		},
	}

	var stderr bytes.Buffer
	if code := execute(validArgs(t), &stderr, deps); code != 37 {
		t.Fatalf("execute exited %d, want child status 37; stderr=%q", code, stderr.String())
	}
	if !started {
		t.Fatal("terminal session did not receive composed dependencies")
	}
}

func TestExecuteMapsUsageResults(t *testing.T) {
	isolateConfig(t)
	deps := dependencies{
		hosted: func() bool {
			t.Fatal("invalid arguments reached bootstrap")
			return false
		},
	}
	for _, test := range []struct {
		args []string
		code int
	}{
		{args: []string{"--help"}, code: 0},
		{args: nil, code: 2},
	} {
		var stderr bytes.Buffer
		if code := execute(test.args, &stderr, deps); code != test.code {
			t.Errorf("execute(%q) = %d, want %d", test.args, code, test.code)
		}
	}
}

func TestExecuteMapsStartupFailuresToOne(t *testing.T) {
	isolateConfig(t)
	tests := []struct {
		name string
		deps dependencies
		want string
	}{
		{
			name: "prompt",
			deps: dependencies{
				hosted: func() bool { return false },
				prompts: func(_, _ string, _ io.Writer) (llm.Prompts, error) {
					return llm.Prompts{}, errors.New("prompt failed")
				},
			},
			want: "aty: prompt failed",
		},
		{
			name: "terminal",
			deps: dependencies{
				hosted: func() bool { return false },
				prompts: func(_, _ string, _ io.Writer) (llm.Prompts, error) {
					return llm.Prompts{}, nil
				},
				mcp: loadMCP,
				terminal: func(term.Config) (int, error) {
					return 0, errors.New("terminal failed")
				},
			},
			want: "aty: terminal failed",
		},
		{
			name: "mcp config",
			deps: dependencies{
				hosted: func() bool { return false },
				prompts: func(_, _ string, _ io.Writer) (llm.Prompts, error) {
					return llm.Prompts{}, nil
				},
				mcp: func(context.Context, string, string) (*appmcp.Registry, error) {
					return nil, errors.New("mcp config failed")
				},
				terminal: func(term.Config) (int, error) {
					t.Fatal("terminal started after MCP config failed")
					return 0, nil
				},
			},
			want: "aty: mcp config failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := execute(validArgs(t), &stderr, test.deps); code != 1 {
				t.Fatalf("execute exited %d, want 1", code)
			}
			if !strings.Contains(stderr.String(), test.want) {
				t.Errorf("stderr = %q, want %q", stderr.String(), test.want)
			}
		})
	}
}

func TestExecuteStopsBeforeLoadingPromptsWhenHosted(t *testing.T) {
	isolateConfig(t)
	deps := dependencies{
		hosted: func() bool { return true },
		prompts: func(_, _ string, _ io.Writer) (llm.Prompts, error) {
			t.Fatal("hosted execution loaded prompts")
			return llm.Prompts{}, nil
		},
	}
	var stderr bytes.Buffer
	if code := execute(validArgs(t), &stderr, deps); code != 0 {
		t.Fatalf("execute exited %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "already running") {
		t.Errorf("stderr = %q, want already running", stderr.String())
	}
}

func validArgs(t *testing.T) []string {
	t.Helper()
	t.Setenv("ATY_PROVIDER", "openai-compatible")
	t.Setenv("ATY_API_KEY", "test-key")
	t.Setenv("ATY_MODEL", "test-model")
	t.Setenv("ATY_ENDPOINT", "http://127.0.0.1/chat/completions")
	return nil
}

func TestExecuteRejectsMissingCredentialsBeforeStartup(t *testing.T) {
	isolateConfig(t)
	validArgs(t)
	t.Setenv("ATY_ENDPOINT", "https://api.openai.com/v1/chat/completions")
	t.Setenv("ATY_API_KEY", "")
	deps := dependencies{
		hosted: func() bool {
			t.Fatal("missing credentials reached startup")
			return false
		},
	}
	var stderr bytes.Buffer
	if code := execute(nil, &stderr, deps); code != 2 || !strings.Contains(stderr.String(), "ATY_API_KEY") {
		t.Fatalf("execute = %d, stderr = %q", code, stderr.String())
	}
}

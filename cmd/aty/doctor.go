package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/TheR1D/aty/internal/llm"
	appmcp "github.com/TheR1D/aty/internal/mcp"
	"github.com/TheR1D/aty/internal/term"
)

const doctorTimeout = 5 * time.Second

func doctor(out io.Writer, ask, think llm.Asker) int {
	code := checkShell(out)
	if check(out, ask) != 0 {
		code = 1
	}
	if ask == think {
		return code
	}
	if checkPrefixed(out, "??  ", think) != 0 {
		return 1
	}
	return code
}

// doctorDump is what `?#` writes: ATY version, provider checks, both system prompts,
// the tools a ??? would attach, and the messages currently in memory.
func doctorDump(ask, think llm.Asker, prompts llm.Prompts, question string, transcript []llm.Turn, registry *appmcp.Registry) string {
	var checks strings.Builder
	_ = doctor(&checks, ask, think)
	writeMCPStatuses(&checks, registry)
	return "ATY version " + version + "\n\n" + llm.DumpChat(checks.String(), prompts, question, transcript, ask, think)
}

// checkShell is whether $SHELL emits CSI ? 2004 h, the mark aty needs to
// intercept ? and to split the transcript. A shell that never sends it
// (plain sh, busybox ash, bash before 5.1) is a terminal and nothing more;
// `?#` says so instead of leaving that silent until the first ?.
func checkShell(out io.Writer) int {
	shell := term.DefaultShell()
	if err := term.ProbePaste(shell); err != nil {
		_, _ = fmt.Fprintf(out, "shell   fail  %s\n", err)
		if errors.Is(err, term.ErrNoPaste) {
			_, _ = fmt.Fprintf(out, "        need  zsh, bash 5.1+, or fish\n")
		}
		return 1
	}
	_, _ = fmt.Fprintf(out, "shell   ok    %s\n", shell)
	return 0
}

// check is whether the configured backend is reachable, and whether the
// model questions will go to is available. It does not load a local GGUF.
// Starting the server is the user's to do; this only says that one is
// needed, or that it listed the model.
func check(out io.Writer, client llm.Asker) int {
	return checkPrefixed(out, "", client)
}

func checkPrefixed(out io.Writer, prefix string, client llm.Asker) int {
	ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
	defer cancel()

	name := client.Name()
	version, err := client.Version(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(out, "%s%s  fail  %s: %s\n", prefix, name, client.Endpoint(), displayErr(err))
		_, _ = fmt.Fprintf(out, "%smodel   skip  %s (%s is unreachable)\n", prefix, client.Model(), name)
		return 1
	}
	if version != "" {
		_, _ = fmt.Fprintf(out, "%s%s  ok    %s (%s)\n", prefix, name, client.Endpoint(), version)
	} else {
		_, _ = fmt.Fprintf(out, "%s%s  ok    %s\n", prefix, name, client.Endpoint())
	}

	names, err := client.Models(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(out, "%smodel   fail  %s: %s\n", prefix, client.Model(), displayErr(err))
		return 1
	}
	if !llm.Installed(client.Model(), names) {
		_, _ = fmt.Fprintf(out, "%smodel   fail  %s is not installed\n", prefix, client.Model())
		if len(names) == 0 {
			_, _ = fmt.Fprintf(out, "%s        have  none\n", prefix)
		} else {
			_, _ = fmt.Fprintf(out, "%s        have  %s\n", prefix, strings.Join(names, ", "))
		}
		return 1
	}
	_, _ = fmt.Fprintf(out, "%smodel   ok    %s\n", prefix, client.Model())
	return 0
}

func displayErr(err error) string {
	return strings.TrimPrefix(err.Error(), "llm: ")
}

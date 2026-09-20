package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	appconfig "github.com/TheR1D/aty/internal/config"
	"github.com/TheR1D/aty/internal/llm"
	appmcp "github.com/TheR1D/aty/internal/mcp"
	"github.com/TheR1D/aty/internal/term"
)

type dependencies struct {
	hosted   func() bool
	prompts  func(string, string, io.Writer) (llm.Prompts, error)
	mcp      mcpLoader
	terminal func(term.Config) (int, error)
}

func systemDependencies() dependencies {
	return dependencies{
		hosted: term.Hosted, prompts: llm.LoadPrompts, mcp: loadMCP, terminal: term.Run,
	}
}

func run(args []string, stderr io.Writer) int {
	return execute(args, stderr, systemDependencies())
}

func execute(args []string, stderr io.Writer, deps dependencies) int {
	settings, err := parse(args, stderr)
	if err != nil {
		if errors.Is(err, errHelp) {
			return 0
		}
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	if deps.hosted() {
		_, _ = fmt.Fprintln(stderr, "aty: already running")
		return 0
	}
	prompts, err := deps.prompts(configPath(defaultPrompt), configPath(agentPrompt), stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "aty: %v\n", err)
		return 1
	}
	registry, err := startMCP(deps.mcp, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "aty: %v\n", err)
		return 1
	}
	if registry != nil {
		defer func() { _ = registry.Close() }()
	}
	cfg, err := compose(settings, prompts, registry)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	code, err := deps.terminal(cfg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "aty: %v\n", err)
		return 1
	}
	return code
}

// compose keeps terminal callbacks together so mode routing and shared-client
// updates can be checked without following the terminal event loop.
func compose(settings appconfig.Settings, prompts llm.Prompts, registry *appmcp.Registry) (term.Config, error) {
	ask, think, err := clients(settings, prompts, registry)
	if err != nil {
		return term.Config{}, err
	}
	eachClient := func(update func(llm.Asker)) {
		update(ask)
		if think != ask {
			update(think)
		}
	}
	return term.Config{
		Limits: settings.Limits,
		Color:  settings.Color,
		InitialPrompt: func(ps1 string) {
			prompts = prompts.WithPS1(ps1)
			eachClient(func(client llm.Asker) { client.SetPS1(ps1) })
		},
		Ask: func(ctx context.Context, question string, transcript []llm.Turn, thinking bool, emit, thought func(string) error) error {
			asker := ask
			if thinking {
				asker = think
			}
			return asker.Command(ctx, question, transcript, thinking, emit, thought)
		},
		Warm: func(ctx context.Context, transcript []llm.Turn, thinking, agent bool) error {
			asker, backend := ask, settings.Default
			if thinking || agent {
				asker, backend = think, settings.Think
			}
			if !backend.PrewarmEnabled() {
				return nil
			}
			return asker.Prewarm(ctx, transcript, thinking, agent)
		},
		Clear: func() { eachClient(llm.Asker.InvalidatePromptCache) },
		Agent: think.Agent,
		Dump: func(question string, transcript []llm.Turn) string {
			return doctorDump(ask, think, prompts, question, transcript, registry)
		},
	}, nil
}

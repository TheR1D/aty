package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	appconfig "github.com/TheR1D/aty/internal/config"
)

const (
	defaultFile   = "config.toml"
	thinking      = "think_config.toml"
	defaultPrompt = "default_prompt.sh"
	agentPrompt   = "agent_prompt.sh"
	mcpDirectory  = "mcp"
	mcpFile       = "mcp.json"
)

var errHelp = errors.New("help")

const usageText = `aty runs your normal shell with optional AI command generation.
At a fresh, empty prompt, type ? followed by what you want to do. aty
sends the request and recent terminal context to the configured model,
then types its suggested command into the shell's editable input line.
Review or edit the command, then press Enter yourself. Without !, aty
never runs it automatically.

Usage:
  aty

`

const usageFooter = `
  ?    suggest a command: normal mode (fast).
  ??   suggest a command: thinking (reasoning) mode.
  ???  agent: proposes and runs commands until the goal is done.
  !    after a mode marker, automatically press Enter on generated commands.
  x    after a mode marker, omit recent terminal context.
  ?#   dump provider checks, prompts, tools, and context.

~/.config/aty/config.toml is ?. think_config.toml is ?? and ???.
Local and remote MCP tools are configured in ~/.config/aty/mcp/mcp.json.
ATY_* names match the file fields and override both model configurations.
Shared history and output settings go in config.toml's optional [limits] table.
`

func parse(args []string, stderr io.Writer) (appconfig.Settings, error) {
	return parseInput(args, os.Stdin, stderr)
}

func parseInput(args []string, stdin io.Reader, stderr io.Writer) (appconfig.Settings, error) {
	fs := flag.NewFlagSet("aty", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr, usageText)
		fs.PrintDefaults()
		_, _ = fmt.Fprint(stderr, usageFooter)
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return appconfig.Settings{}, errHelp
		}
		return appconfig.Settings{}, err
	}
	if fs.NArg() > 0 {
		return appconfig.Settings{}, fmt.Errorf("aty: unexpected argument %q", fs.Arg(0))
	}

	return appconfig.LoadOrCreate(appconfig.Paths{
		Default: configPath(defaultFile),
		Think:   configPath(thinking),
	}, stdin, stderr)
}

func configPath(name string) string {
	return filepath.Join(appconfig.Dir(), name)
}

func mcpConfigPath() string {
	return filepath.Join(appconfig.Dir(), mcpDirectory, mcpFile)
}

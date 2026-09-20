package llm

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

const (
	DefaultPromptFilename = "default_prompt.sh"
	AgentPromptFilename   = "agent_prompt.sh"
)

//go:embed prompts/default_prompt.sh
var defaultPromptScript string

//go:embed prompts/agent_prompt.sh
var agentPromptScript string

// Prompts are the system prompts printed by the user's prompt scripts.
type Prompts struct {
	System string
	Agent  string
}

// WithPS1 substitutes the rendered startup prompt without evaluating its contents.
func (p Prompts) WithPS1(ps1 string) Prompts {
	p.System = strings.ReplaceAll(p.System, "{{.PS1}}", ps1)
	p.Agent = strings.ReplaceAll(p.Agent, "{{.PS1}}", ps1)
	return p
}

// LoadPrompts creates any missing default scripts at the given paths,
// executes both with bash, and returns their stdout. Existing files are
// never overwritten. Created paths are reported to output. Both scripts run
// with their directory as cwd.
func LoadPrompts(systemPath, agentPath string, output io.Writer) (Prompts, error) {
	if strings.TrimSpace(systemPath) == "" || strings.TrimSpace(agentPath) == "" {
		return Prompts{}, fmt.Errorf("prompt script path is empty")
	}
	system, err := loadPrompt(systemPath, defaultPromptScript, output)
	if err != nil {
		return Prompts{}, err
	}
	agent, err := loadPrompt(agentPath, agentPromptScript, output)
	if err != nil {
		return Prompts{}, err
	}
	return Prompts{System: system, Agent: agent}, nil
}

func loadPrompt(path, contents string, output io.Writer) (string, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating prompt config directory %s: %w", dir, err)
	}
	if err := createPromptScript(path, contents, output); err != nil {
		return "", err
	}
	return runPromptScript(path, dir)
}

func createPromptScript(path, contents string, output io.Writer) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return fmt.Errorf("creating prompt script %s: %w", path, err)
	}

	complete := false
	defer func() {
		_ = f.Close()
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if _, err := f.WriteString(contents); err != nil {
		return fmt.Errorf("writing prompt script %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing prompt script %s: %w", path, err)
	}
	complete = true
	_, _ = fmt.Fprintf(output, "aty: created prompt script at %s\n", path)
	return nil
}

func runPromptScript(path, dir string) (string, error) {
	cmd := exec.Command("bash", path)
	cmd.Dir = dir
	return promptOutput(cmd, path)
}

func promptOutput(cmd *exec.Cmd, name string) (string, error) {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return "", fmt.Errorf("running prompt script %s: %w: %s", name, err, detail)
		}
		return "", fmt.Errorf("running prompt script %s: %w", name, err)
	}
	if strings.TrimSpace(string(out)) == "" {
		return "", fmt.Errorf("prompt script %s produced no output", name)
	}
	return string(out), nil
}

var bundledPrompts = sync.OnceValue(func() Prompts {
	system, err := runBundledPrompt(defaultPromptScript, DefaultPromptFilename)
	// TODO: Probably not cool to panic here.
	if err != nil {
		panic(fmt.Errorf("loading bundled system prompt: %w", err))
	}
	agent, err := runBundledPrompt(agentPromptScript, AgentPromptFilename)
	if err != nil {
		panic(fmt.Errorf("loading bundled agent prompt: %w", err))
	}
	return Prompts{System: system, Agent: agent}
})

func runBundledPrompt(script, name string) (string, error) {
	cmd := exec.Command("bash")
	cmd.Stdin = strings.NewReader(script)
	return promptOutput(cmd, name)
}

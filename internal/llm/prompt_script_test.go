package llm

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadPromptsCreatesAndRunsTheDefaultScripts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SHELL", "/opt/homebrew/bin/fish")
	systemPath := filepath.Join(dir, DefaultPromptFilename)
	agentPath := filepath.Join(dir, AgentPromptFilename)

	var output bytes.Buffer
	prompts, err := LoadPrompts(systemPath, agentPath, &output)
	if err != nil {
		t.Fatalf("LoadPrompts: %v", err)
	}
	wantOutput := fmt.Sprintf("aty: created prompt script at %s\naty: created prompt script at %s\n", systemPath, agentPath)
	if got := output.String(); got != wantOutput {
		t.Errorf("output = %q, want %q", got, wantOutput)
	}
	osName := map[string]string{"darwin": "macOS", "linux": "Linux"}[runtime.GOOS]
	for name, prompt := range map[string]string{"default": prompts.System, "agent": prompts.Agent} {
		if !strings.Contains(prompt, "fish") || !strings.Contains(prompt, osName) {
			t.Errorf("%s prompt did not resolve $SHELL and OS:\n%s", name, prompt)
		}
		if !strings.Contains(prompt, "{{.PS1}}") {
			t.Errorf("%s script expanded the startup prompt placeholder early", name)
		}
	}
	const ps1 = "user@host:$(echo untouched) `literal` {{.PS1}}$ "
	bound := prompts.WithPS1(ps1)
	for name, prompt := range map[string]string{"default": bound.System, "agent": bound.Agent} {
		if !strings.Contains(prompt, "PS1 prompt defined as "+ps1+"\n") {
			t.Errorf("%s prompt did not preserve the literal startup prompt: %s", name, prompt)
		}
	}
	if !strings.Contains(prompts.Agent, toolSuggestShellCommand) {
		t.Errorf("agent prompt did not describe its tool:\n%s", prompts.Agent)
	}
	for _, path := range []string{systemPath, agentPath} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was not created: %v", path, err)
		}
	}
	output.Reset()
	if _, err := LoadPrompts(systemPath, agentPath, &output); err != nil {
		t.Fatalf("reloading prompts: %v", err)
	}
	if output.Len() != 0 {
		t.Errorf("existing scripts produced creation notices: %q", output.String())
	}
}

func TestLoadPromptsPreservesAndCapturesCustomScripts(t *testing.T) {
	dir := t.TempDir()
	systemPath := filepath.Join(dir, DefaultPromptFilename)
	agentPath := filepath.Join(dir, AgentPromptFilename)
	custom := "#!/usr/bin/env bash\nprintf 'custom %s\\n' \"$ATY_PROMPT_TEST\"\n"
	if err := os.WriteFile(systemPath, []byte(custom), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATY_PROMPT_TEST", "stdout")

	var output bytes.Buffer
	prompts, err := LoadPrompts(systemPath, agentPath, &output)
	if err != nil {
		t.Fatalf("LoadPrompts: %v", err)
	}
	if prompts.System != "custom stdout\n" {
		t.Errorf("System = %q, want custom script stdout", prompts.System)
	}
	got, err := os.ReadFile(systemPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != custom {
		t.Errorf("custom prompt script was overwritten:\n%s", got)
	}
	wantOutput := fmt.Sprintf("aty: created prompt script at %s\n", agentPath)
	if got := output.String(); got != wantOutput {
		t.Errorf("output = %q, want %q", got, wantOutput)
	}
}

func TestLoadPromptsCreatesIndependentScriptDirectories(t *testing.T) {
	dir := t.TempDir()
	systemPath := filepath.Join(dir, "system", DefaultPromptFilename)
	agentPath := filepath.Join(dir, "agent", AgentPromptFilename)

	if _, err := LoadPrompts(systemPath, agentPath, io.Discard); err != nil {
		t.Fatalf("LoadPrompts: %v", err)
	}
	for _, path := range []string{systemPath, agentPath} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was not created: %v", path, err)
		}
	}
}

func TestLoadPromptsRejectsAnEmptyPrompt(t *testing.T) {
	dir := t.TempDir()
	systemPath := filepath.Join(dir, DefaultPromptFilename)
	agentPath := filepath.Join(dir, AgentPromptFilename)
	if err := os.WriteFile(systemPath, []byte("#!/usr/bin/env bash\n:\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	_, err := LoadPrompts(systemPath, agentPath, io.Discard)
	if err == nil {
		t.Fatal("an empty prompt was accepted")
	}
	if !strings.Contains(err.Error(), DefaultPromptFilename) {
		t.Errorf("error %q does not name the broken script", err)
	}
}

func TestClientUsesConfiguredPrompts(t *testing.T) {
	client := New(Options{
		SystemPrompt: "custom system",
		AgentPrompt:  "custom agent",
	})

	if got := client.system; got != "custom system" {
		t.Errorf("system prompt = %q, want configured stdout", got)
	}
	if got := client.agent; got != "custom agent" {
		t.Errorf("agent prompt = %q, want configured stdout", got)
	}
}

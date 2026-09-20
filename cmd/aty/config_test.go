package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	appconfig "github.com/TheR1D/aty/internal/config"
	"github.com/TheR1D/aty/internal/helpers"
)

func TestParseRequiresProviderEndpointAndModel(t *testing.T) {
	stderr := ioDiscard(t)
	_, err := parse(nil, stderr)
	if err == nil || !strings.Contains(err.Error(), "provider") {
		t.Fatalf("parse without provider = %v, want a provider error", err)
	}

	t.Setenv("ATY_PROVIDER", "openai-compatible")
	_, err = parse(nil, stderr)
	if err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("parse without model = %v, want a model error", err)
	}

	t.Setenv("ATY_MODEL", "test-model")
	_, err = parse(nil, stderr)
	if err == nil || !strings.Contains(err.Error(), "endpoint") {
		t.Fatalf("parse without endpoint = %v, want an endpoint error", err)
	}
}

func TestParseCreatesConfigOnlyForNormalLaunch(t *testing.T) {
	stderr := ioDiscard(t)
	path := configPath(defaultFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--help"}, {"--unknown"}, {"unexpected"}} {
		_, _ = parseInput(args, nil, stderr)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("parse(%q) created a config: %v", args, err)
		}
	}
	input := strings.NewReader("ollama\nhttp://localhost:11434/v1/chat/completions\n\nlocal-model\n\n")
	if _, err := parseInput(nil, input, stderr); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "[limits]") {
		t.Fatalf("normal launch did not create config defaults: %q, %v", data, err)
	}
}

func TestParseLetsTheFileOverrideTheDefaults(t *testing.T) {
	writeConfig(t, `
provider = "openai-compatible"
api_key = "test-key"
model = "qwen2.5-coder:7b"
endpoint = "http://127.0.0.1:8082/v1"
reasoning_effort = "medium"
`)

	s, err := parse(nil, ioDiscard(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.Default.Model != "qwen2.5-coder:7b" {
		t.Errorf("Model = %q, want qwen2.5-coder:7b", s.Default.Model)
	}
	if s.Default.Endpoint != "http://127.0.0.1:8082/v1" {
		t.Errorf("Endpoint = %q, want the one in the file", s.Default.Endpoint)
	}
	if helpers.Value(s.Default.ReasoningEffort) != "medium" {
		t.Errorf("ReasoningEffort = %q, want medium", helpers.Value(s.Default.ReasoningEffort))
	}
}

func TestParseLetsTheFileSetTemperature(t *testing.T) {
	writeConfig(t, `
provider = "openai-compatible"
api_key = "test-key"
model = "test-model"
endpoint = "http://127.0.0.1:8081/v1/chat/completions"
temperature = 0.2
`)

	s, err := parse(nil, ioDiscard(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.Default.Temperature == nil || *s.Default.Temperature != 0.2 {
		t.Errorf("Temperature = %v, want 0.2 from the file", s.Default.Temperature)
	}
}

func TestParseLetsTheFileSetTemperatureZero(t *testing.T) {
	writeConfig(t, `
provider = "openai-compatible"
api_key = "test-key"
model = "test-model"
endpoint = "http://127.0.0.1:8081/v1/chat/completions"
temperature = 0
`)

	s, err := parse(nil, ioDiscard(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.Default.Temperature == nil || *s.Default.Temperature != 0 {
		t.Errorf("Temperature = %v, want 0 from the file, not omitted", s.Default.Temperature)
	}
}

func TestParsePrewarmingSettings(t *testing.T) {
	for _, test := range []struct {
		name, setting string
		want          bool
	}{
		{"omitted", "", false},
		{"disabled", "prewarm = false", false},
		{"enabled", "prewarm = true", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			writeConfig(t, `
provider = "openai-compatible"
api_key = "test-key"
model = "test-model"
endpoint = "http://127.0.0.1:8081/v1/chat/completions"
`+test.setting)

			s, err := parse(nil, ioDiscard(t))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if s.Default.PrewarmEnabled() != test.want || s.Think.PrewarmEnabled() != test.want {
				t.Errorf("prewarming = %v/%v, want %v for both backends", s.Default.PrewarmEnabled(), s.Think.PrewarmEnabled(), test.want)
			}
		})
	}
}

func TestParseRejectsConfigurationFlags(t *testing.T) {
	for _, arg := range []string{
		"--provider=openai-compatible",
		"--model=test-model",
		"--endpoint=http://localhost",
		"--api-key=secret",
		"--reasoning-effort=high",
		"--temperature=0.5",
		"--prewarm=false",
	} {
		if _, err := parse([]string{arg}, ioDiscard(t)); err == nil {
			t.Errorf("%s was accepted; configuration should come from TOML or ATY_*", arg)
		}
	}
}

func TestParseLetsTheFilePickTheProvider(t *testing.T) {
	writeConfig(t, `
provider = "ollama"
model = "test-model"
endpoint = "http://127.0.0.1:11434/v1/chat/completions"
`)

	s, err := parse(nil, ioDiscard(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.Default.Provider != appconfig.Ollama {
		t.Errorf("Provider = %q, want ollama", s.Default.Provider)
	}
}

func TestParseLetsTheFilePickOpenAICompatible(t *testing.T) {
	writeConfig(t, `
provider = "openai-compatible"
model = "gpt-4o-mini"
endpoint = "https://api.openai.com/v1/chat/completions"
api_key = "test-key"
`)

	s, err := parse(nil, ioDiscard(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.Default.Provider != appconfig.OpenAICompatible {
		t.Errorf("Provider = %q, want openai-compatible", s.Default.Provider)
	}
	if s.Default.ReasoningEffort != nil {
		t.Errorf("ReasoningEffort = %q, want nil when the file omitted it", helpers.Value(s.Default.ReasoningEffort))
	}
}

func TestParseLetsTheEnvironmentOverrideTheFile(t *testing.T) {
	writeConfig(t, `
provider = "openai-compatible"
model = "from-file"
endpoint = "http://127.0.0.1:1"
api_key = "sk-file"
reasoning_effort = "low"
temperature = 0.1
`)
	stderr := ioDiscard(t)
	t.Setenv("ATY_PROVIDER", "ollama")
	t.Setenv("ATY_MODEL", "from-env")
	t.Setenv("ATY_ENDPOINT", "http://127.0.0.1:2")
	t.Setenv("ATY_API_KEY", "sk-env")
	t.Setenv("ATY_REASONING_EFFORT", "high")
	t.Setenv("ATY_TEMPERATURE", "0.7")

	s, err := parse(nil, stderr)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.Default.Provider != appconfig.Ollama {
		t.Errorf("Provider = %q, want ollama from ATY_PROVIDER", s.Default.Provider)
	}
	if s.Default.Model != "from-env" {
		t.Errorf("Model = %q, want from-env", s.Default.Model)
	}
	if s.Default.Endpoint != "http://127.0.0.1:2" {
		t.Errorf("Endpoint = %q, want the environment", s.Default.Endpoint)
	}
	if helpers.Value(s.Default.APIKey) != "sk-env" {
		t.Errorf("APIKey = %q, want sk-env", helpers.Value(s.Default.APIKey))
	}
	if helpers.Value(s.Default.ReasoningEffort) != "high" {
		t.Errorf("ReasoningEffort = %q, want high", helpers.Value(s.Default.ReasoningEffort))
	}
	if s.Default.Temperature == nil || *s.Default.Temperature != 0.7 {
		t.Errorf("Temperature = %v, want 0.7 from ATY_TEMPERATURE", s.Default.Temperature)
	}
}

func TestParseLetsTheEnvironmentOverrideAThinkConfig(t *testing.T) {
	dir := isolateConfigHome(t)
	writeConfigIn(t, dir, "config.toml", `
provider = "openai-compatible"
model = "plain-model"
endpoint = "http://127.0.0.1:8081/v1/chat/completions"
api_key = "sk-file"
`)
	writeConfigIn(t, dir, "think_config.toml", `
provider = "openai-compatible"
model = "think-model"
endpoint = "http://127.0.0.1:8081/v1/chat/completions"
api_key = "sk-think"
`)
	stderr := ioDiscard(t)
	t.Setenv("ATY_MODEL", "from-env")
	t.Setenv("ATY_API_KEY", "sk-env")

	s, err := parse(nil, stderr)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.Default.Model != "from-env" {
		t.Errorf("Model = %q, want from-env", s.Default.Model)
	}
	if s.Think.Model != "from-env" {
		t.Errorf("Think.Model = %q, want from-env so ATY_MODEL overrides think_config.toml", s.Think.Model)
	}
	if helpers.Value(s.Default.APIKey) != "sk-env" || helpers.Value(s.Think.APIKey) != "sk-env" {
		t.Errorf("APIKey = %q Think.APIKey = %q, want sk-env on both", helpers.Value(s.Default.APIKey), helpers.Value(s.Think.APIKey))
	}
}

func TestParseRejectsABrokenEnvironmentValue(t *testing.T) {
	stderr := ioDiscard(t)
	t.Setenv("ATY_TEMPERATURE", "nope")
	_, err := parse(nil, stderr)
	if err == nil {
		t.Fatal("a non-numeric ATY_TEMPERATURE was accepted")
	}
	if !strings.Contains(err.Error(), "ATY_TEMPERATURE") {
		t.Errorf("the failure reads %q, which does not name ATY_TEMPERATURE", err)
	}
}

func TestParseRejectsDoctorAsAnArgument(t *testing.T) {
	_, err := parse([]string{"doctor"}, ioDiscard(t))
	if err == nil {
		t.Fatal("parse accepted doctor as an argument")
	}
	if !strings.Contains(err.Error(), "doctor") {
		t.Errorf("parse = %v, want it to name doctor", err)
	}
}

func TestParseRejectsABrokenConfig(t *testing.T) {
	path := writeConfig(t, "model = [")

	_, err := parse(nil, ioDiscard(t))
	if err == nil {
		t.Fatal("a broken config was accepted")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the failure reads %q, which does not name the file", err)
	}
}

func TestParseRejectsAnUnexpectedArgument(t *testing.T) {
	_, err := parse([]string{"serve"}, ioDiscard(t))
	if err == nil {
		t.Fatal("an unknown command was accepted")
	}
}

func TestParseHelpIsNotAFailure(t *testing.T) {
	clearConfigEnv(t)
	var stderr bytes.Buffer
	_, err := parse([]string{"-h"}, &stderr)
	if err != errHelp {
		t.Fatalf("parse(-h) = %v, want errHelp", err)
	}
	help := stderr.String()
	for _, want := range []string{
		"never runs it",
		"???",
		"?#",
		"tools",
		"mcp/mcp.json",
		"ATY_",
		"both model configurations",
		"[limits]",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help does not mention %q:\n%s", want, help)
		}
	}
}

func TestConfigPathFollowsXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	if got, want := configPath(defaultFile), "/tmp/xdg/aty/config.toml"; got != want {
		t.Errorf("configPath(defaultFile) = %q, want %q", got, want)
	}
	if got, want := configPath(thinking), "/tmp/xdg/aty/think_config.toml"; got != want {
		t.Errorf("configPath(thinking) = %q, want %q", got, want)
	}
	if got, want := configPath(defaultPrompt), "/tmp/xdg/aty/default_prompt.sh"; got != want {
		t.Errorf("configPath(defaultPrompt) = %q, want %q", got, want)
	}
	if got, want := configPath(agentPrompt), "/tmp/xdg/aty/agent_prompt.sh"; got != want {
		t.Errorf("configPath(agentPrompt) = %q, want %q", got, want)
	}
	if got, want := mcpConfigPath(), "/tmp/xdg/aty/mcp/mcp.json"; got != want {
		t.Errorf("mcpConfigPath() = %q, want %q", got, want)
	}
}

func TestParseUsesTheThinkConfigBesideTheDefaultFile(t *testing.T) {
	dir := isolateConfigHome(t)
	writeConfigIn(t, dir, "config.toml", `
provider = "openai-compatible"
api_key = "test-key"
model = "plain-model"
endpoint = "http://127.0.0.1:8081/v1/chat/completions"
`)
	writeConfigIn(t, dir, "think_config.toml", `
provider = "ollama"
model = "think-model"
endpoint = "http://127.0.0.1:11434/v1/chat/completions"
reasoning_effort = "high"
`)

	s, err := parse(nil, ioDiscard(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.Default.Model != "plain-model" {
		t.Errorf("Model = %q, want plain-model", s.Default.Model)
	}
	if s.Think.Provider != appconfig.Ollama {
		t.Errorf("Think.Provider = %q, want ollama", s.Think.Provider)
	}
	if s.Think.Model != "think-model" {
		t.Errorf("Think.Model = %q, want think-model", s.Think.Model)
	}
	if s.Think.Endpoint != "http://127.0.0.1:11434/v1/chat/completions" {
		t.Errorf("Think.Endpoint = %q, want the configured endpoint", s.Think.Endpoint)
	}
	if helpers.Value(s.Think.ReasoningEffort) != "high" {
		t.Errorf("Think.ReasoningEffort = %q, want high", helpers.Value(s.Think.ReasoningEffort))
	}
}

func TestParseThinkConfigFallsBackToTheDefaultMode(t *testing.T) {
	writeConfig(t, `
provider = "openai-compatible"
api_key = "test-key"
model = "from-file"
endpoint = "http://127.0.0.1:8081/v1/chat/completions"
reasoning_effort = "medium"
`)

	s, err := parse(nil, ioDiscard(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.Think != s.Default {
		t.Errorf("Think = %+v, want the default mode when think_config.toml is absent", s.Think)
	}
	if s.Think.Model != "from-file" {
		t.Errorf("Think.Model = %q, want from-file from the default config", s.Think.Model)
	}
}

func TestParseRejectsABrokenThinkConfig(t *testing.T) {
	dir := isolateConfigHome(t)
	writeConfigIn(t, dir, "config.toml", `
provider = "openai-compatible"
api_key = "test-key"
model = "ok"
endpoint = "http://127.0.0.1:8081/v1/chat/completions"
`)
	think := writeConfigIn(t, dir, "think_config.toml", "model = [")

	_, err := parse(nil, ioDiscard(t))
	if err == nil {
		t.Fatal("a broken think config was accepted")
	}
	if !strings.Contains(err.Error(), think) {
		t.Errorf("the failure reads %q, which does not name the think file", err)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	return writeConfigIn(t, isolateConfigHome(t), string(defaultFile), body)
}

func writeConfigIn(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func ioDiscard(t *testing.T) *bytes.Buffer {
	t.Helper()
	clearConfigEnv(t)
	isolateConfigHome(t)
	return &bytes.Buffer{}
}

func isolateConfigHome(t *testing.T) string {
	t.Helper()
	if os.Getenv("ATY_TEST_CONFIG") == "1" {
		dir := filepath.Dir(configPath(defaultFile))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("ATY_TEST_CONFIG", "1")
	dir := filepath.Join(root, "aty")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Most tests exercise existing file/environment loading, not first-launch input.
	writeConfigIn(t, dir, defaultFile, "")
	return dir
}

func clearConfigEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ATY_COLOR", "")
	for _, typ := range []reflect.Type{reflect.TypeFor[appconfig.Backend](), reflect.TypeFor[appconfig.Limits]()} {
		for field := range typ.Fields() {
			if name := field.Tag.Get("env"); name != "" {
				t.Setenv(name, "")
			}
		}
	}
}

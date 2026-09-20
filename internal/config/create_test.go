package config

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestLoadOrCreateAsksForModelAndWritesPrivateDefaults(t *testing.T) {
	clearEnvironment(t)
	dir := filepath.Join(t.TempDir(), "aty")
	paths := Paths{Default: filepath.Join(dir, "config.toml"), Think: filepath.Join(dir, "think_config.toml")}
	var output bytes.Buffer
	input := strings.NewReader("ollama\nhttp://localhost:11434/v1/chat/completions\n\nlocal-model\n\n")
	settings, err := LoadOrCreate(paths, input, &output)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Default.Provider != Ollama || settings.Default.Model != "local-model" || settings.Default.Endpoint != "http://localhost:11434/v1/chat/completions" || !Equivalent(settings.Default, settings.Think) {
		t.Fatalf("setup settings = %+v", settings)
	}
	previous := -1
	for _, want := range []string{"provider (llama, ollama, openai-compatible, openai-native):", "endpoint (full Chat Completions URL):", "api_key (can be empty for local models):", "model:", "reasoning_effort (e.g. low, medium, high. Model-dependent, Enter to skip):", "aty: created config file at " + paths.Default} {
		index := strings.Index(output.String(), want)
		if index <= previous {
			t.Fatalf("setup output missing or out of order %q: %s", want, &output)
		}
		previous = index
	}
	for _, unwanted := range []string{"temperature:", "prewarm:", "color:", "limits:"} {
		if strings.Contains(output.String(), unwanted) {
			t.Errorf("setup asked for optional field %s", unwanted)
		}
	}
	var file fileConfig
	meta, err := toml.DecodeFile(paths.Default, &file)
	if err != nil {
		t.Fatal(err)
	}
	if file.Limits == nil || *file.Limits != DefaultLimits() || file.PrewarmEnabled() {
		t.Fatalf("generated defaults = %+v", file)
	}
	if file.Color == nil || *file.Color != DefaultColor {
		t.Fatalf("generated color = %v, want orange", file.Color)
	}
	if file.APIKey == nil || *file.APIKey != "" || file.Temperature != nil || file.ReasoningEffort != nil {
		t.Fatalf("unexpected optional model settings: %+v", file.Backend)
	}
	for _, field := range []string{"provider", "model", "endpoint", "api_key", "prewarm", "color"} {
		if !meta.IsDefined(field) {
			t.Errorf("generated config omitted %s", field)
		}
	}
	schema := reflect.TypeFor[Limits]()
	for field := range schema.Fields() {
		name := field.Tag.Get("toml")
		if !meta.IsDefined("limits", name) {
			t.Errorf("generated config omitted limits.%s", name)
		}
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, paths.Default: 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s permissions = %o, want %o", path, info.Mode().Perm(), want)
		}
	}
	if _, err := os.Stat(paths.Think); !os.IsNotExist(err) {
		t.Fatalf("thinking config should remain optional: %v", err)
	}
}

func TestLoadOrCreateDoesNotSaveEnvironmentOverrides(t *testing.T) {
	clearEnvironment(t)
	t.Setenv("ATY_PROVIDER", "openai-compatible")
	t.Setenv("ATY_MODEL", "environment-model")
	t.Setenv("ATY_ENDPOINT", "https://environment.example/v1/chat/completions")
	t.Setenv("ATY_API_KEY", "environment-secret")
	t.Setenv("ATY_REASONING_EFFORT", "high")
	t.Setenv("ATY_COMMAND_BYTES", "3")
	t.Setenv("ATY_COLOR", "none")
	path := filepath.Join(t.TempDir(), "config.toml")
	settings, err := LoadOrCreate(Paths{Default: path}, strings.NewReader("llama\nhttp://localhost:8080/v1/chat/completions\n\nlocal-model\nmedium\n"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Default.Model != "environment-model" || settings.Limits.CommandBytes != 3 || settings.Color != NoColor || !Equivalent(settings.Default, settings.Think) {
		t.Fatalf("environment overrides or thinking inheritance lost: %+v", settings)
	}
	if settings.Default.ReasoningEffort == nil || *settings.Default.ReasoningEffort != "high" {
		t.Fatalf("reasoning effort override lost: %v", settings.Default.ReasoningEffort)
	}
	var file fileConfig
	if _, err := toml.DecodeFile(path, &file); err != nil {
		t.Fatal(err)
	}
	if file.Provider != Llama || file.Model != "local-model" || file.Endpoint != "http://localhost:8080/v1/chat/completions" || file.APIKey == nil || *file.APIKey != "" || file.Limits == nil || *file.Limits != DefaultLimits() {
		t.Fatalf("environment overrides were saved: %+v", file)
	}
	if file.Color == nil || *file.Color != DefaultColor {
		t.Fatalf("color override was saved: %v", file.Color)
	}
	if file.ReasoningEffort == nil || *file.ReasoningEffort != "medium" {
		t.Fatalf("setup reasoning effort was not saved: %v", file.ReasoningEffort)
	}
}

func TestLoadOrCreateNativeUsesResponsesEndpointAndRequiresKey(t *testing.T) {
	clearEnvironment(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	input := strings.NewReader("openai-native\nhttps://api.openai.com/v1/responses\n\nnative-key\ngpt-6-astra\nmedium\n")
	var output bytes.Buffer
	settings, err := LoadOrCreate(Paths{Default: path}, input, &output)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Default.Provider != OpenAINative || settings.Default.Endpoint != "https://api.openai.com/v1/responses" || settings.Default.APIKey == nil || *settings.Default.APIKey != "native-key" {
		t.Fatalf("native setup settings = %+v", settings.Default)
	}
	if settings.Default.ReasoningEffort == nil || *settings.Default.ReasoningEffort != "medium" || settings.Default.Temperature != nil {
		t.Fatalf("native setup optional parameters = %+v", settings.Default)
	}
	if !strings.Contains(output.String(), "endpoint (full Responses URL, e.g. https://api.openai.com/v1/responses):") || strings.Count(output.String(), "This field is required.") != 1 {
		t.Fatalf("native setup did not request Responses and enforce an API key: %s", &output)
	}
}

func TestLoadOrCreatePreservesExistingFiles(t *testing.T) {
	clearEnvironment(t)
	dir := t.TempDir()
	contents := limitsBackend + "# my settings\n[limits]\ncommand_bytes = 11\n"
	path := write(t, dir, "config.toml", contents)
	think := write(t, dir, "think_config.toml", limitsBackend)
	var output bytes.Buffer
	settings, err := LoadOrCreate(Paths{Default: path, Think: think}, nil, &output)
	if err != nil || settings.Limits.CommandBytes != 11 {
		t.Fatalf("existing settings = %+v, %v", settings, err)
	}
	if output.Len() != 0 {
		t.Fatalf("existing config triggered setup: %s", &output)
	}
	for path, want := range map[string]string{path: contents, think: limitsBackend} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("existing %s was changed: %q, %v", path, got, err)
		}
	}
}

func TestLoadOrCreateRetriesInvalidAnswers(t *testing.T) {
	clearEnvironment(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	input := strings.NewReader("\nunsupported\nopenai-compatible\n\nhttps://example.com/v1/chat/completions\n\ntest-secret\n \nmodel\n high \n")
	var output bytes.Buffer
	settings, err := LoadOrCreate(Paths{Default: path}, input, &output)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Default.Provider != OpenAICompatible || settings.Default.APIKey == nil || *settings.Default.APIKey != "test-secret" {
		t.Fatal("setup did not retain valid answers")
	}
	if settings.Default.ReasoningEffort == nil || *settings.Default.ReasoningEffort != "high" {
		t.Fatalf("setup reasoning effort = %v, want high", settings.Default.ReasoningEffort)
	}
	if !strings.Contains(output.String(), "Choose llama, ollama, openai-compatible, or openai-native.") || strings.Count(output.String(), "This field is required.") != 4 {
		t.Fatalf("missing validation feedback: %s", &output)
	}
	if strings.Contains(output.String(), "test-secret") {
		t.Fatal("setup printed the API key")
	}
}

func TestLoadOrCreateLeavesNoFileWhenInputStops(t *testing.T) {
	for _, input := range []string{"", "ollama\n", "ollama\nendpoint\n", "ollama\nendpoint\npartial-key", "ollama\nendpoint\n\n", "ollama\nendpoint\n\nmodel\n", "ollama\nendpoint\n\nmodel\nhigh"} {
		t.Run(input, func(t *testing.T) {
			clearEnvironment(t)
			path := filepath.Join(t.TempDir(), "aty", "config.toml")
			var output bytes.Buffer
			_, err := LoadOrCreate(Paths{Default: path}, strings.NewReader(input), &output)
			if err == nil || !strings.Contains(err.Error(), "setup stopped") {
				t.Fatalf("setup error = %v, want incomplete input", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("incomplete setup left a config: %v", err)
			}
			if strings.Contains(output.String(), "created config") {
				t.Fatalf("incomplete setup reported success: %s", &output)
			}
		})
	}
}

func TestCreateDefaultDoesNotReplaceAFileCreatedDuringSetup(t *testing.T) {
	path := write(t, t.TempDir(), "config.toml", limitsBackend)
	created, err := createDefault(path, Backend{Provider: OpenAICompatible})
	if err != nil || created {
		t.Fatalf("createDefault = %v, %v, want no creation", created, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != limitsBackend {
		t.Fatalf("existing config changed: %q, %v", data, err)
	}
}

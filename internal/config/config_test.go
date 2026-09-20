package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/TheR1D/aty/internal/helpers"
)

func TestLoadLayersFilesAndEnvironment(t *testing.T) {
	clearEnvironment(t)
	dir := t.TempDir()
	plain := write(t, dir, "config.toml", `
provider = "openai-compatible"
model = "plain-file"
endpoint = "https://plain.example/v1/chat/completions"
api_key = "file-key"
prewarm = false
`)
	think := write(t, dir, "think_config.toml", `
provider = "llama"
model = "think-file"
endpoint = "https://think.example/v1/chat/completions"
reasoning_effort = "high"
`)
	t.Setenv("ATY_MODEL", "environment-model")
	t.Setenv("ATY_API_KEY", "environment-key")
	t.Setenv("ATY_TEMPERATURE", "0")
	t.Setenv("ATY_PREWARM", "true")

	got, err := Load(Paths{Default: plain, Think: think})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	for name, backend := range map[string]Backend{"default": got.Default, "think": got.Think} {
		if backend.Model != "environment-model" {
			t.Errorf("%s model = %q, want environment-model", name, backend.Model)
		}
		if helpers.Value(backend.APIKey) != "environment-key" {
			t.Errorf("%s API key = %q, want environment-key", name, helpers.Value(backend.APIKey))
		}
		if backend.Temperature == nil || *backend.Temperature != 0 {
			t.Errorf("%s temperature = %v, want explicit zero", name, backend.Temperature)
		}
	}
	if got.Default.Provider != OpenAICompatible || got.Think.Provider != Llama {
		t.Errorf("providers = %q/%q, want openai-compatible/llama", got.Default.Provider, got.Think.Provider)
	}
	if got.Default.Prewarm == nil || !*got.Default.Prewarm {
		t.Error("default prewarm was not overridden by ATY_PREWARM")
	}
	if helpers.Value(got.Think.ReasoningEffort) != "high" {
		t.Errorf("think effort = %q, want high", helpers.Value(got.Think.ReasoningEffort))
	}
}

func TestLoadMissingThinkFileInheritsResolvedDefault(t *testing.T) {
	clearEnvironment(t)
	dir := t.TempDir()
	plain := write(t, dir, "config.toml", `
provider = "ollama"
model = "file-model"
endpoint = "http://localhost:11434/v1/chat/completions"
`)
	t.Setenv("ATY_MODEL", "environment-model")

	got, err := Load(Paths{
		Default: plain,
		Think:   filepath.Join(dir, "missing.toml"),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Equivalent(got.Default, got.Think) {
		t.Errorf("think backend = %+v, want resolved default %+v", got.Think, got.Default)
	}
}

func TestLoadRejectsUnsupportedProvider(t *testing.T) {
	clearEnvironment(t)
	t.Setenv("ATY_PROVIDER", "made-up")
	t.Setenv("ATY_MODEL", "model")
	t.Setenv("ATY_ENDPOINT", "https://example.com/v1/chat/completions")

	_, err := Load(Paths{})
	if err == nil || !strings.Contains(err.Error(), `unsupported provider "made-up"`) {
		t.Fatalf("Load error = %v, want unsupported provider", err)
	}
}

func TestLoadNativeThinkingConfigLeavesOptionalParametersUnset(t *testing.T) {
	clearEnvironment(t)
	dir := t.TempDir()
	plain := write(t, dir, "config.toml", `provider = "openai-compatible"
model = "compatible"
endpoint = "https://example.com/v1/chat/completions"
api_key = "compatible-key"
temperature = 0.5
reasoning_effort = "high"
`)
	think := write(t, dir, "think_config.toml", `provider = "openai-native"
model = "gpt-6-astra"
endpoint = "https://api.openai.com/v1/responses"
api_key = "native-key"
`)
	got, err := Load(Paths{Default: plain, Think: think})
	if err != nil {
		t.Fatal(err)
	}
	if got.Think.Provider != OpenAINative || got.Think.Endpoint != "https://api.openai.com/v1/responses" {
		t.Fatalf("thinking backend = %+v, want native Responses", got.Think)
	}
	if got.Think.Temperature != nil || got.Think.ReasoningEffort != nil {
		t.Fatalf("native optional parameters were filled from default config: %+v", got.Think)
	}
}

func write(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func clearEnvironment(t *testing.T) {
	t.Helper()
	for _, schema := range []reflect.Type{reflect.TypeFor[Backend](), reflect.TypeFor[Limits](), reflect.TypeFor[fileConfig]()} {
		for field := range schema.Fields() {
			if name := field.Tag.Get("env"); name != "" {
				t.Setenv(name, "")
			}
		}
	}
}

func TestLoadChecksRequiredSettings(t *testing.T) {
	for _, test := range []struct {
		name, provider, endpoint, key, want string
	}{
		{"missing endpoint", "ollama", "", "", "ATY_ENDPOINT"},
		{"blank endpoint", "ollama", "  ", "", "ATY_ENDPOINT"},
		{"endpoint provided", "ollama", "any-nonblank-value", "", ""},
		{"compatible missing key", "openai-compatible", "endpoint", "", "ATY_API_KEY"},
		{"compatible blank key", "openai-compatible", "endpoint", "  ", "ATY_API_KEY"},
		{"compatible key", "openai-compatible", "endpoint", "test-key", ""},
		{"native missing key", "openai-native", "endpoint", "", "ATY_API_KEY"},
		{"native blank key", "openai-native", "endpoint", "  ", "ATY_API_KEY"},
		{"native key", "openai-native", "endpoint", "test-key", ""},
		{"llama without key", "llama", "endpoint", "", ""},
		{"ollama without key", "ollama", "endpoint", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			clearEnvironment(t)
			t.Setenv("ATY_PROVIDER", test.provider)
			t.Setenv("ATY_MODEL", "test-model")
			t.Setenv("ATY_ENDPOINT", test.endpoint)
			t.Setenv("ATY_API_KEY", test.key)
			_, err := Load(Paths{})
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load error = %v, want %s", err, test.want)
			} else if !strings.HasPrefix(err.Error(), "aty: ") {
				t.Fatalf("environment-only error has an unexpected prefix: %v", err)
			}
		})
	}
}

func TestLoadReportsAllMissingFields(t *testing.T) {
	clearEnvironment(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	_, err := Load(Paths{Default: path})
	for _, want := range []string{path, "ATY_PROVIDER", "ATY_MODEL", "ATY_ENDPOINT"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want %s", err, want)
		}
	}
}

func TestLoadValidatesThinkingCredentials(t *testing.T) {
	clearEnvironment(t)
	dir := t.TempDir()
	plain := write(t, dir, "config.toml", `provider = "ollama"
model = "local"
endpoint = "http://localhost:11434/v1/chat/completions"
`)
	think := write(t, dir, "think_config.toml", `provider = "openai-compatible"
model = "remote"
endpoint = "https://api.openai.com/v1/chat/completions"
api_key = "  "
`)
	_, err := Load(Paths{Default: plain, Think: think})
	if err == nil || !strings.Contains(err.Error(), think) || !strings.Contains(err.Error(), "ATY_API_KEY") {
		t.Fatalf("error = %v, want thinking config API key error", err)
	}
}

package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

const configHeader = `# ATY model configuration. ATY_* environment variables override these settings.
# Providers: llama, ollama, openai-compatible, openai-native.
# openai-native uses /v1/responses; other providers use /v1/chat/completions.
# Both OpenAI providers require api_key or ATY_API_KEY.
# reasoning_effort = ""
# temperature = 0.7
#
# All modes use this model unless you add think_config.toml for ?? and ???.
# The [limits] defaults below are shared by all modes and belong only here.
# color sets query text and cursor: orange, red, green, yellow, blue, purple,
# cyan, or none to disable colors. It is shared by all modes and belongs here.

`

// LoadOrCreate asks for model settings if config.toml is missing, then loads it.
// Environment overrides are applied when loading, never saved.
// The optional thinking file is left absent so it inherits the default backend.
func LoadOrCreate(paths Paths, input io.Reader, output io.Writer) (Settings, error) {
	if paths.Default != "" {
		if _, err := os.Lstat(paths.Default); os.IsNotExist(err) {
			backend, err := setup(input, output)
			if err != nil {
				return Settings{}, err
			}
			created, err := createDefault(paths.Default, backend)
			if err != nil {
				return Settings{}, err
			}
			if created {
				_, _ = fmt.Fprintf(output, "aty: created config file at %s\n", paths.Default)
			}
		} else if err != nil {
			return Settings{}, fmt.Errorf("aty: checking %s: %w", paths.Default, err)
		}
	}
	return Load(paths)
}

func createDefault(path string, backend Backend) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, fmt.Errorf("aty: creating config directory %s: %w", filepath.Dir(path), err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("aty: creating %s: %w", path, err)
	}

	backend.Prewarm = new(false)
	_, err = file.WriteString(configHeader)
	if err == nil {
		err = toml.NewEncoder(file).Encode(fileConfig{
			Backend: backend, Limits: new(DefaultLimits()), Color: new(DefaultColor),
		})
	}
	if err = errors.Join(err, file.Close()); err != nil {
		_ = os.Remove(path)
		return false, fmt.Errorf("aty: writing %s: %w", path, err)
	}
	return true, nil
}

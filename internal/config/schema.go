package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// Provider identifies a supported API protocol and provider dialect.
type Provider string

const (
	Llama            Provider = "llama"
	Ollama           Provider = "ollama"
	OpenAICompatible Provider = "openai-compatible"
	OpenAINative     Provider = "openai-native"
)

// Backend is the configuration schema for one query mode. Fields are loaded
// from TOML and then from the named environment variable. Validation is
// declared here too: add a tagged scalar field (or pointer for an optional
// value) to support it in both files and their environment overrides.
type Backend struct {
	Provider        Provider `toml:"provider" env:"ATY_PROVIDER" required:"true" oneof:"llama,ollama,openai-compatible,openai-native"`
	Model           string   `toml:"model" env:"ATY_MODEL" required:"true"`
	Endpoint        string   `toml:"endpoint" env:"ATY_ENDPOINT" required:"true"`
	APIKey          *string  `toml:"api_key" env:"ATY_API_KEY"`
	ReasoningEffort *string  `toml:"reasoning_effort" env:"ATY_REASONING_EFFORT"`
	Temperature     *float64 `toml:"temperature" env:"ATY_TEMPERATURE"`
	Prewarm         *bool    `toml:"prewarm" env:"ATY_PREWARM"`
}

// Equivalent reports whether two backends can share one client.
func Equivalent(a, b Backend) bool {
	return reflect.DeepEqual(a, b)
}

// PrewarmEnabled reports whether speculative prompt-cache warming is enabled.
func (b Backend) PrewarmEnabled() bool {
	return b.Prewarm != nil && *b.Prewarm
}

func resolve(backend Backend) (Backend, error) {
	if err := applyEnvironment(&backend); err != nil {
		return Backend{}, err
	}
	if err := validate(backend); err != nil {
		return Backend{}, err
	}
	return backend, nil
}

func applyEnvironment(dst any) error {
	value := reflect.ValueOf(dst).Elem()
	for field := range value.Type().Fields() {
		name := field.Tag.Get("env")
		raw := strings.TrimSpace(os.Getenv(name))
		if name == "" || raw == "" {
			continue
		}
		if err := setText(value.FieldByIndex(field.Index), raw); err != nil {
			return fmt.Errorf("aty: %s: %w", name, err)
		}
	}
	return nil
}

func setText(dst reflect.Value, raw string) error {
	if dst.Kind() == reflect.Pointer {
		value := reflect.New(dst.Type().Elem())
		if err := setText(value.Elem(), raw); err != nil {
			return err
		}
		dst.Set(value)
		return nil
	}

	var err error
	switch dst.Kind() {
	case reflect.String:
		dst.SetString(raw)
	case reflect.Bool:
		var parsed bool
		parsed, err = strconv.ParseBool(raw)
		dst.SetBool(parsed)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		var parsed int64
		parsed, err = strconv.ParseInt(raw, 10, dst.Type().Bits())
		dst.SetInt(parsed)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		var parsed uint64
		parsed, err = strconv.ParseUint(raw, 10, dst.Type().Bits())
		dst.SetUint(parsed)
	case reflect.Float32, reflect.Float64:
		var parsed float64
		parsed, err = strconv.ParseFloat(raw, dst.Type().Bits())
		dst.SetFloat(parsed)
	default:
		return fmt.Errorf("unsupported configuration type %s", dst.Type())
	}
	return err
}

func validate(backend Backend) error {
	var problems []error
	value := reflect.ValueOf(backend)
	for field := range value.Type().Fields() {
		current := value.FieldByIndex(field.Index)
		name := field.Tag.Get("toml")
		if field.Tag.Get("required") == "true" && empty(current) {
			problems = append(problems, fmt.Errorf("aty: %s is required (set %s)", name, field.Tag.Get("env")))
		}
		if allowed := field.Tag.Get("oneof"); allowed != "" && !empty(current) {
			actual := fmt.Sprint(current.Interface())
			if !slices.Contains(strings.Split(allowed, ","), actual) {
				problems = append(problems, fmt.Errorf("aty: unsupported %s %q (set %s to one of %s)", name, actual, field.Tag.Get("env"), allowed))
			}
		}
	}
	if (backend.Provider == OpenAICompatible || backend.Provider == OpenAINative) &&
		(backend.APIKey == nil || strings.TrimSpace(*backend.APIKey) == "") {
		problems = append(problems, fmt.Errorf("aty: api_key is required for %s (set ATY_API_KEY)", backend.Provider))
	}
	return errors.Join(problems...)
}

func empty(value reflect.Value) bool {
	if value.Kind() == reflect.Pointer {
		return value.IsNil()
	}
	if value.Kind() == reflect.String {
		return strings.TrimSpace(value.String()) == ""
	}
	return value.IsZero()
}

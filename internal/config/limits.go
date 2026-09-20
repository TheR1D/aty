package config

import (
	"errors"
	"fmt"
	"reflect"
)

const (
	DefaultTranscriptBytes     = 496_000             // History size that triggers trimming (~124k tokens).
	DefaultTranscriptKeepBytes = 446_400             // History size to keep after trimming (90% of maximum).
	DefaultCommandBytes        = 80_000              // Budget per shell command and its output (~20k tokens).
	DefaultToolOutputBytes     = DefaultCommandBytes // Output budget per tool result.
	DefaultAgentSteps          = 30                  // Maximum tool-call rounds per query/prompt.
)

// Limits bounds the shared terminal history and agent results. These settings
// live in config.toml's [limits] table, independent of the selected model.
type Limits struct {
	TranscriptBytes     int `toml:"transcript_bytes" env:"ATY_TRANSCRIPT_BYTES"`
	TranscriptKeepBytes int `toml:"transcript_keep_bytes" env:"ATY_TRANSCRIPT_KEEP_BYTES"`
	CommandBytes        int `toml:"command_bytes" env:"ATY_COMMAND_BYTES"`
	ToolOutputBytes     int `toml:"tool_output_bytes" env:"ATY_TOOL_OUTPUT_BYTES"`
	AgentSteps          int `toml:"agent_steps" env:"ATY_AGENT_STEPS"`
}

func DefaultLimits() Limits {
	return Limits{
		TranscriptBytes:     DefaultTranscriptBytes,
		TranscriptKeepBytes: DefaultTranscriptKeepBytes,
		CommandBytes:        DefaultCommandBytes,
		ToolOutputBytes:     DefaultToolOutputBytes,
		AgentSteps:          DefaultAgentSteps,
	}
}

// WithDefaults fills fields omitted by programmatic callers. File and
// environment values are validated before this point; explicit zero is invalid.
func (l Limits) WithDefaults() Limits {
	value := reflect.ValueOf(&l).Elem()
	defaults := reflect.ValueOf(DefaultLimits())
	for i := range value.NumField() {
		if value.Field(i).Int() == 0 {
			value.Field(i).Set(defaults.Field(i))
		}
	}
	return l
}

func (l Limits) validate() error {
	var problems []error
	value := reflect.ValueOf(l)
	for field := range value.Type().Fields() {
		if value.FieldByIndex(field.Index).Int() <= 0 {
			problems = append(problems, fmt.Errorf("aty: limits.%s must be positive (set %s)", field.Tag.Get("toml"), field.Tag.Get("env")))
		}
	}
	if l.TranscriptKeepBytes > l.TranscriptBytes {
		problems = append(problems, errors.New("aty: limits.transcript_keep_bytes must not exceed limits.transcript_bytes (set ATY_TRANSCRIPT_KEEP_BYTES or ATY_TRANSCRIPT_BYTES)"))
	}
	return errors.Join(problems...)
}

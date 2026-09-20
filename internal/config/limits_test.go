package config

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const limitsBackend = "provider = 'ollama'\nmodel = 'local'\nendpoint = 'http://localhost:11434/v1/chat/completions'\n"

func TestLoadOptionalLimits(t *testing.T) {
	clearEnvironment(t)
	dir := t.TempDir()
	path := write(t, dir, "config.toml", limitsBackend)
	got, err := Load(Paths{Default: path})
	if err != nil {
		t.Fatal(err)
	}
	if got.Limits != DefaultLimits() {
		t.Fatalf("omitted limits = %+v, want %+v", got.Limits, DefaultLimits())
	}
	path = write(t, dir, "config.toml", limitsBackend+"[limits]\ncommand_bytes = 17\n")
	think := write(t, dir, "think_config.toml", limitsBackend)
	got, err = Load(Paths{Default: path, Think: think})
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultLimits()
	want.CommandBytes = 17
	if got.Limits != want {
		t.Fatalf("partial limits = %+v, want %+v", got.Limits, want)
	}
}

func TestLoadLimitsFileAndEnvironment(t *testing.T) {
	schema := reflect.TypeFor[Limits]()
	for i := range schema.NumField() {
		field := schema.Field(i)
		t.Run(field.Tag.Get("toml"), func(t *testing.T) {
			clearEnvironment(t)
			// Keep the default transcript relationship valid while changing each field.
			value := reflect.ValueOf(DefaultLimits()).Field(i).Int()
			if field.Name == "TranscriptBytes" {
				value *= 2
			} else {
				value /= 2
			}
			path := write(t, t.TempDir(), "config.toml", limitsBackend+fmt.Sprintf("[limits]\n%s = %d\n", field.Tag.Get("toml"), value))
			got, err := Load(Paths{Default: path})
			if err != nil {
				t.Fatal(err)
			}
			if n := reflect.ValueOf(got.Limits).Field(i).Int(); n != value {
				t.Fatalf("file setting = %d, want %d", n, value)
			}
			t.Setenv(field.Tag.Get("env"), fmt.Sprint(value+1))
			got, err = Load(Paths{Default: path})
			if err != nil {
				t.Fatal(err)
			}
			if n := reflect.ValueOf(got.Limits).Field(i).Int(); n != value+1 {
				t.Fatalf("environment setting = %d, want %d", n, value+1)
			}
		})
	}
}

func TestLoadRejectsInvalidLimits(t *testing.T) {
	schema := reflect.TypeFor[Limits]()
	for field := range schema.Fields() {
		for _, invalid := range []string{"0", "-1", "'invalid'", "1.5"} {
			t.Run(field.Tag.Get("toml")+"/"+invalid, func(t *testing.T) {
				clearEnvironment(t)
				path := write(t, t.TempDir(), "config.toml", limitsBackend+fmt.Sprintf("[limits]\n%s = %s\n", field.Tag.Get("toml"), invalid))
				if _, err := Load(Paths{Default: path}); err == nil || !strings.Contains(err.Error(), path) {
					t.Fatalf("file setting error = %v, want config path", err)
				}
				path = write(t, t.TempDir(), "config.toml", limitsBackend)
				t.Setenv(field.Tag.Get("env"), invalid)
				if _, err := Load(Paths{Default: path}); err == nil || !strings.Contains(err.Error(), field.Tag.Get("env")) {
					t.Fatalf("environment setting error = %v, want variable name", err)
				}
			})
		}
	}
}

func TestLoadRejectsInconsistentOrMisplacedLimits(t *testing.T) {
	clearEnvironment(t)
	dir := t.TempDir()
	path := write(t, dir, "config.toml", limitsBackend+"[limits]\ntranscript_bytes = 100\ntranscript_keep_bytes = 101\n")
	if _, err := Load(Paths{Default: path}); err == nil || !strings.Contains(err.Error(), "must not exceed") {
		t.Fatalf("inconsistent limits error = %v", err)
	}
	path = write(t, dir, "config.toml", limitsBackend)
	think := write(t, dir, "think_config.toml", limitsBackend+"[limits]\ncommand_bytes = 17\n")
	if _, err := Load(Paths{Default: path, Think: think}); err == nil || !strings.Contains(err.Error(), "shared across modes") {
		t.Fatalf("thinking limits error = %v", err)
	}
}

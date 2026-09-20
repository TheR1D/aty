package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestLoadColor(t *testing.T) {
	for _, color := range []Color{"", "orange", "red", "green", "yellow", "blue", "purple", "cyan", "none"} {
		t.Run(string(color), func(t *testing.T) {
			clearEnvironment(t)
			contents := limitsBackend
			want := color
			if color == "" {
				want = DefaultColor
			} else {
				contents += fmt.Sprintf("color = %q\n", color)
			}
			dir := t.TempDir()
			paths := Paths{
				Default: write(t, dir, "config.toml", contents),
				Think:   write(t, dir, "think_config.toml", limitsBackend),
			}
			got, err := Load(paths)
			if err != nil || got.Color != want {
				t.Fatalf("loaded color = %q, %v; want %q", got.Color, err, want)
			}
			t.Setenv("ATY_COLOR", "  cyan  ")
			got, err = Load(paths)
			if err != nil || got.Color != "cyan" {
				t.Fatalf("environment color = %q, %v; want cyan", got.Color, err)
			}
		})
	}
}

func TestLoadRejectsInvalidOrMisplacedColor(t *testing.T) {
	clearEnvironment(t)
	dir := t.TempDir()
	for _, invalid := range []string{`"invalid"`, `""`, `true`, `42`} {
		path := write(t, dir, "config.toml", limitsBackend+"color = "+invalid+"\n")
		if _, err := Load(Paths{Default: path}); err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("invalid color %s: %v, want error naming the config file", invalid, err)
		}
	}
	path := write(t, dir, "config.toml", limitsBackend)
	t.Setenv("ATY_COLOR", "invalid")
	if _, err := Load(Paths{Default: path}); err == nil || !strings.Contains(err.Error(), "ATY_COLOR") {
		t.Fatalf("invalid environment color: %v, want error naming ATY_COLOR", err)
	}
	t.Setenv("ATY_COLOR", "")
	think := write(t, dir, "think_config.toml", limitsBackend+"color = 'blue'\n")
	if _, err := Load(Paths{Default: path, Think: think}); err == nil || !strings.Contains(err.Error(), "shared across modes") {
		t.Fatalf("thinking color: %v, want error directing color to config.toml", err)
	}
}

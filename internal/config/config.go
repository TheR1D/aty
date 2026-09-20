// Package config loads the two model backends used by an aty session.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Settings holds both model backends and the shared session settings.
type Settings struct {
	Default Backend
	Think   Backend
	Limits  Limits
	Color   Color
}

type fileConfig struct {
	Backend
	Limits *Limits `toml:"limits"`
	Color  *Color  `toml:"color" env:"ATY_COLOR"`
}

// Paths names the two configuration files.
type Paths struct {
	Default string
	Think   string
}

// Dir returns aty's XDG configuration directory.
func Dir() string {
	if root := os.Getenv("XDG_CONFIG_HOME"); root != "" {
		return filepath.Join(root, "aty")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "aty")
}

// Load resolves file settings, then applies environment overrides.
// A missing thinking file inherits the fully resolved default backend.
func Load(paths Paths) (Settings, error) {
	limits := DefaultLimits()
	plain := fileConfig{Limits: &limits, Color: new(DefaultColor)}
	_, err := read(paths.Default, &plain)
	if err != nil {
		return Settings{}, err
	}
	plain.Backend, err = resolve(plain.Backend)
	if err != nil {
		return Settings{}, configError(paths.Default, err)
	}
	if err := applyEnvironment(&limits); err != nil {
		return Settings{}, configError(paths.Default, err)
	}
	if err := limits.validate(); err != nil {
		return Settings{}, configError(paths.Default, err)
	}
	if err := applyEnvironment(&plain); err != nil {
		return Settings{}, configError(paths.Default, err)
	}
	if err := plain.Color.validate(); err != nil {
		return Settings{}, configError(paths.Default, err)
	}

	settings := Settings{Default: plain.Backend, Think: plain.Backend, Limits: limits, Color: *plain.Color}
	var thinking fileConfig
	found, err := read(paths.Think, &thinking)
	if err != nil {
		return Settings{}, err
	}
	if !found {
		return settings, nil
	}
	if thinking.Limits != nil {
		return Settings{}, fmt.Errorf("%s: aty: [limits] is shared across modes; set it in config.toml", paths.Think)
	}
	if thinking.Color != nil {
		return Settings{}, fmt.Errorf("%s: aty: color is shared across modes; set it in config.toml", paths.Think)
	}
	settings.Think, err = resolve(thinking.Backend)
	if err != nil {
		return Settings{}, configError(paths.Think, err)
	}
	return settings, nil
}

func configError(path string, err error) error {
	if path == "" {
		return err
	}
	return fmt.Errorf("%s: %w", path, err)
}

func read(path string, dst *fileConfig) (bool, error) {
	if path == "" {
		return false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("aty: reading %s: %w", path, err)
	}

	if err := toml.Unmarshal(data, dst); err != nil {
		return false, fmt.Errorf("aty: %s: %w", path, err)
	}
	return true, nil
}

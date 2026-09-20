package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Color names the shared query-text and cursor color.
type Color string

const (
	DefaultColor Color = "orange"
	NoColor      Color = "none"
)

var colorRGB = map[Color]uint32{
	"orange": 0xFFA500,
	"red":    0xFF5555,
	"green":  0x50FA7B,
	"yellow": 0xF1FA8C,
	"blue":   0x6495ED,
	"purple": 0xBD93F9,
	"cyan":   0x8BE9FD,
}

// RGB returns the color's 24-bit RGB value, or false for no color.
func (c Color) RGB() (uint32, bool) {
	rgb, ok := colorRGB[c]
	return rgb, ok
}

func (c Color) validate() error {
	if _, ok := c.RGB(); ok || c == NoColor {
		return nil
	}
	names := slices.Sorted(maps.Keys(colorRGB))
	allowed := make([]string, 0, len(names)+1)
	for _, name := range names {
		allowed = append(allowed, string(name))
	}
	allowed = append(allowed, string(NoColor))
	return fmt.Errorf("aty: unsupported color %q (set color or ATY_COLOR to one of %s)", c, strings.Join(allowed, ", "))
}

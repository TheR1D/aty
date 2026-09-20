//go:build darwin || linux

package term

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

const maxArgv = 256

func parseProcargs2(data []byte) ([]string, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("short buffer: %d bytes", len(data))
	}
	argc := int(binary.NativeEndian.Uint32(data[:4]))
	if argc < 1 || argc > maxArgv {
		return nil, fmt.Errorf("argc %d", argc)
	}
	data = data[4:]
	path := bytes.IndexByte(data, 0)
	if path < 0 {
		return nil, fmt.Errorf("unterminated executable path")
	}
	data = bytes.TrimLeft(data[path+1:], "\x00")

	args := make([]string, 0, argc)
	for len(data) > 0 && len(args) < argc {
		end := bytes.IndexByte(data, 0)
		if end < 0 {
			args = append(args, string(data))
			break
		}
		args = append(args, string(data[:end]))
		data = data[end+1:]
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("no arguments")
	}
	return args, nil
}

func parseCmdline(data []byte) ([]string, error) {
	data = bytes.TrimRight(data, "\x00")
	if len(data) == 0 {
		return nil, fmt.Errorf("empty cmdline")
	}
	parts := bytes.Split(data, []byte{0})
	if len(parts) > maxArgv {
		parts = parts[:maxArgv]
	}
	args := make([]string, len(parts))
	for i := range parts {
		args[i] = string(parts[i])
	}
	return args, nil
}

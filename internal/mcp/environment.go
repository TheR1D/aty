package mcp

import (
	"bufio"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var inheritedEnvironment = []string{
	"HOME",
	"LANG",
	"LC_ALL",
	"LC_CTYPE",
	"PATH",
	"TEMP",
	"TMP",
	"TMPDIR",
	"TZ",
}

var interpolationPattern = regexp.MustCompile(`\$\{([^}]+)\}`)

// interpolate resolves only configured values. An envFile is parsed separately
// and its contents stay literal; explicit env values override that file.
func (server *Server) interpolate(workspace string) error {
	fields := []*string{&server.Command}
	for index := range server.Args {
		fields = append(fields, &server.Args[index])
	}
	fields = append(fields, &server.URL, &server.EnvFile)
	for _, field := range fields {
		value, err := interpolate(*field, workspace)
		if err != nil {
			return err
		}
		*field = value
	}
	for _, values := range []map[string]string{server.Env, server.Headers} {
		for key, raw := range values {
			value, err := interpolate(raw, workspace)
			if err != nil {
				return err
			}
			values[key] = value
		}
	}
	return nil
}

func interpolate(value, workspace string) (string, error) {
	var interpolationErr error
	out := interpolationPattern.ReplaceAllStringFunc(value, func(match string) string {
		key := match[2 : len(match)-1]
		switch {
		case strings.HasPrefix(key, "env:"):
			name := strings.TrimPrefix(key, "env:")
			resolved, ok := os.LookupEnv(name)
			if name == "" || !ok {
				interpolationErr = fmt.Errorf("environment variable %q is not set", name)
				return ""
			}
			return resolved
		case key == "userHome":
			home, err := os.UserHomeDir()
			if err != nil {
				interpolationErr = fmt.Errorf("finding the user home: %w", err)
				return ""
			}
			return home
		case key == "workspaceFolder":
			return workspace
		case key == "workspaceFolderBasename":
			return filepath.Base(workspace)
		case key == "pathSeparator" || key == "/":
			return string(filepath.Separator)
		default:
			interpolationErr = fmt.Errorf("unsupported interpolation %q", match)
			return ""
		}
	})
	return out, interpolationErr
}

func readEnvFile(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading envFile %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	values := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimSpace(strings.TrimPrefix(text, "export "))
		key, value, ok := strings.Cut(text, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("envFile %s:%d: expected NAME=value", path, line)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
			value = value[1 : len(value)-1]
		} else if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			unquoted, err := strconv.Unquote(value)
			if err != nil {
				return nil, fmt.Errorf("envFile %s:%d: %w", path, line, err)
			}
			value = unquoted
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading envFile %s: %w", path, err)
	}
	return values, nil
}

func mergeEnvironment(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	merged := make(map[string]string, len(base)+len(override))
	maps.Copy(merged, base)
	maps.Copy(merged, override)
	return merged
}

func environment(configured map[string]string) []string {
	values := make(map[string]string)
	for _, key := range inheritedEnvironment {
		value, ok := os.LookupEnv(key)
		if ok {
			values[key] = value
		}
	}
	maps.Copy(values, configured)
	out := make([]string, 0, len(values))
	for _, key := range slices.Sorted(maps.Keys(values)) {
		out = append(out, key+"="+values[key])
	}
	return out
}

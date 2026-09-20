package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool is one MCP tool exposed to the language model.
type Tool struct {
	Name        string
	Server      string
	Original    string
	Description string
	InputSchema any
}

func (registry *Registry) uniqueAlias(server, tool string) string {
	base := safeToolName("mcp_" + server + "__" + tool)
	alias := base
	if _, exists := registry.routes[alias]; exists {
		alias = shortenedAlias(base, server+"\x00"+tool)
	}
	return alias
}

func safeToolName(name string) string {
	var out strings.Builder
	lastUnderscore := false
	for _, char := range name {
		valid := char >= 'a' && char <= 'z' ||
			char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' ||
			char == '-' || char == '_'
		if !valid {
			char = '_'
		}
		if char == '_' && lastUnderscore {
			continue
		}
		out.WriteRune(char)
		lastUnderscore = char == '_'
	}
	name = strings.Trim(out.String(), "_")
	if name == "" {
		name = "mcp_tool"
	}
	if len(name) > 64 {
		name = shortenedAlias(name, name)
	}
	return name
}

func shortenedAlias(base, identity string) string {
	sum := sha256.Sum256([]byte(identity))
	suffix := "_" + hex.EncodeToString(sum[:4])
	if len(base) > 64-len(suffix) {
		base = base[:64-len(suffix)]
	}
	return base + suffix
}

func formatResult(result *sdk.CallToolResult) string {
	if result == nil {
		return "the MCP tool returned no result"
	}
	var parts []string
	for _, content := range result.Content {
		switch value := content.(type) {
		case *sdk.TextContent:
			if strings.TrimSpace(value.Text) != "" {
				parts = append(parts, value.Text)
			}
		case *sdk.ImageContent:
			parts = append(parts, fmt.Sprintf("[image %s omitted]", value.MIMEType))
		case *sdk.AudioContent:
			parts = append(parts, fmt.Sprintf("[audio %s omitted]", value.MIMEType))
		case *sdk.ResourceLink:
			parts = append(parts, fmt.Sprintf("[resource %s]", value.URI))
		case *sdk.EmbeddedResource:
			if value.Resource != nil && value.Resource.Text != "" {
				parts = append(parts, value.Resource.Text)
			} else if value.Resource != nil {
				parts = append(parts, fmt.Sprintf("[embedded resource %s omitted]", value.Resource.URI))
			}
		default:
			if encoded, err := json.Marshal(value); err == nil {
				parts = append(parts, string(encoded))
			}
		}
	}
	if result.StructuredContent != nil {
		if encoded, err := json.Marshal(result.StructuredContent); err == nil {
			parts = append(parts, "structured result: "+string(encoded))
		}
	}
	output := strings.TrimSpace(strings.Join(parts, "\n"))
	if output == "" {
		output = "the MCP tool returned no content"
	}
	if result.IsError {
		output = "the MCP tool failed: " + output
	}
	return output
}

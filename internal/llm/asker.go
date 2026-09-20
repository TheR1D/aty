package llm

import "context"

// Asker provides command generation, agent tool calls, and provider diagnostics.
type Asker interface {
	SetPS1(string)
	Prewarm(ctx context.Context, transcript []Turn, thinking, agent bool) error
	InvalidatePromptCache()
	Command(ctx context.Context, question string, transcript []Turn, thinking bool, emit func(command string) error, think func(thought string) error) error
	Agent(ctx context.Context, question string, transcript []Turn, emit func(command string) error, think func(thought string) error, run func(ctx context.Context, command string) (string, error), approve ApproveTool) error
	Version(ctx context.Context) (string, error)
	Models(ctx context.Context) ([]string, error)
	Name() string
	Model() string
	Endpoint() string
}

var _ Asker = (*Client)(nil)

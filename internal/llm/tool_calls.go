package llm

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const maxToolCalls = 32

type toolCall struct {
	Index    int          `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function toolCallArgs `json:"function"`
}

type toolCallArgs struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type shellCommandArgs struct {
	Command string `json:"command"`
}

// toolAccum assembles indexed tool-call fragments. The first shell command can
// be previewed from its partial arguments, but execution waits for the full call.
type toolAccum struct {
	buf []toolCall
}

func (a *toolAccum) add(deltas []toolCall) error {
	for _, d := range deltas {
		i := max(d.Index, 0)
		if i >= maxToolCalls {
			return fmt.Errorf("llm: tool call index %d exceeds the limit of %d", i, maxToolCalls)
		}
		for len(a.buf) <= i {
			a.buf = append(a.buf, toolCall{})
		}
		c := &a.buf[i]
		if d.ID != "" && c.ID == "" {
			c.ID = d.ID
		}
		if d.Type != "" && c.Type == "" {
			c.Type = d.Type
		}
		if d.Function.Name != "" && c.Function.Name == "" {
			c.Function.Name = d.Function.Name
		}
		c.Function.Arguments += d.Function.Arguments
	}
	return nil
}

// calls prepares completed calls for a follow-up request without changing the
// accumulated fragments. Missing IDs are generated so results can refer to them.
func (a *toolAccum) calls() []toolCall {
	out := append([]toolCall(nil), a.buf...)
	for i := range out {
		out[i].Index = 0 // Index belongs only to streaming deltas.
		if out[i].Type == "" {
			out[i].Type = "function"
		}
		if out[i].ID == "" {
			out[i].ID = fmt.Sprintf("call_%d", i)
		}
	}
	return out
}

// toolCommand validates a completed shell call. Argument errors become tool
// results so the model can correct them on the next step.
func toolCommand(call toolCall) (string, error) {
	if name := call.Function.Name; name != "" && name != toolSuggestShellCommand {
		return "", fmt.Errorf("llm: the model called %s, which is not a tool it was given", name)
	}
	var args shellCommandArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return "", fmt.Errorf("llm: suggest_shell_command arguments were not JSON: %w", err)
	}
	cmd := strings.TrimSpace(args.Command)
	if cmd == "" {
		return "", fmt.Errorf("llm: suggest_shell_command was called without a command")
	}
	return cmd, nil
}

// toolCommandPrefix reads the command from incomplete arguments such as
// {"command":"ls -l. It waits for the value's opening quote and excludes
// incomplete escape sequences at the end.
func toolCommandPrefix(arguments string) string {
	_, rest, ok := strings.Cut(arguments, `"command"`)
	if !ok {
		return ""
	}
	rest = strings.TrimLeft(rest, " \t\n\r")
	if rest == "" || rest[0] != ':' {
		return ""
	}
	rest = strings.TrimLeft(rest[1:], " \t\n\r")
	if rest == "" || rest[0] != '"' {
		return ""
	}
	rest = rest[1:]

	var b strings.Builder
	for rest != "" {
		if rest[0] == '"' {
			break
		}
		r, _, tail, err := strconv.UnquoteChar(rest, '"')
		if err != nil {
			break
		}
		b.WriteRune(r)
		rest = tail
	}
	return b.String()
}

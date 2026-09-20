//go:build darwin || linux

package term

import (
	"slices"
	"strings"

	"github.com/TheR1D/aty/internal/llm"
)

type requestRoute uint8

const (
	routeAsk requestRoute = iota
	routeAgent
	routeDump
)

const (
	queryEmpty      = "submitted with nothing in it"
	queryUnanswered = "submitted with nothing to answer it"
)

// queryRequest is the immutable lifecycle decision made when Enter transfers
// ownership from the query editor to asynchronous dispatch.
type queryRequest struct {
	question   string
	transcript []llm.Turn
	route      requestRoute
	thinking   bool
	yolo       bool
}

// planQuery maps editor state to one request. It is deliberately independent
// of rendering and goroutines: those consume the decision after it is made.
func planQuery(editor editorSnapshot, transcript []llm.Turn, canAsk, canAgent bool) (queryRequest, string) {
	question := string(editor.text)
	if editor.mode.noContext {
		question = strings.TrimSpace(question)
	}
	if question == "" {
		return queryRequest{}, queryEmpty
	}

	plan := queryRequest{
		question: question,
		thinking: editor.mode.thinking,
		yolo:     editor.mode.yolo,
	}
	if !editor.mode.noContext {
		plan.transcript = slices.Clone(transcript)
	}
	if dumped, ok := dumpRequest(question); ok {
		plan.question, plan.route = dumped, routeDump
		return plan, ""
	}
	if editor.mode.agent && canAgent {
		plan.route, plan.thinking = routeAgent, true
		return plan, ""
	}
	if !canAsk {
		return queryRequest{}, queryUnanswered
	}
	plan.route = routeAsk
	return plan, ""
}

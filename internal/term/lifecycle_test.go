package term

import (
	"testing"

	"github.com/TheR1D/aty/internal/llm"
)

func TestPlanQueryRoutesModesWithoutRendering(t *testing.T) {
	history := []llm.Turn{{Text: "$ pwd\n/tmp"}}
	tests := []struct {
		name       string
		editor     editorSnapshot
		canAsk     bool
		canAgent   bool
		route      requestRoute
		question   string
		contextLen int
		thinking   bool
		yolo       bool
		ended      string
	}{
		{name: "plain", editor: editorSnapshot{text: []rune("list files")}, canAsk: true, route: routeAsk, question: "list files", contextLen: 1},
		{name: "thinking", editor: editorSnapshot{text: []rune("fix it"), mode: queryMode{thinking: true}}, canAsk: true, route: routeAsk, question: "fix it", contextLen: 1, thinking: true},
		{name: "agent", editor: editorSnapshot{text: []rune("do it"), mode: queryMode{agent: true, yolo: true}}, canAsk: true, canAgent: true, route: routeAgent, question: "do it", contextLen: 1, thinking: true, yolo: true},
		{name: "agent fallback", editor: editorSnapshot{text: []rune("do it"), mode: queryMode{agent: true}}, canAsk: true, route: routeAsk, question: "do it", contextLen: 1},
		{name: "no context", editor: editorSnapshot{text: []rune("  private  "), mode: queryMode{noContext: true}}, canAsk: true, route: routeAsk, question: "private"},
		{name: "dump", editor: editorSnapshot{text: []rune("# inspect")}, route: routeDump, question: "inspect", contextLen: 1},
		{name: "empty", editor: editorSnapshot{}, canAsk: true, ended: queryEmpty},
		{name: "unanswered", editor: editorSnapshot{text: []rune("list")}, ended: queryUnanswered},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ended := planQuery(test.editor, history, test.canAsk, test.canAgent)
			if ended != test.ended || got.route != test.route || got.question != test.question ||
				len(got.transcript) != test.contextLen || got.thinking != test.thinking || got.yolo != test.yolo {
				t.Errorf("planQuery = %+v, ended %q", got, ended)
			}
		})
	}
}

func TestQueryRequestDoesNotAliasEditorOrTranscript(t *testing.T) {
	editor := queryEditor{text: []rune("question")}
	history := []llm.Turn{{Text: "old"}}
	request, ended := planQuery(editor.snapshot(), history, true, false)
	if ended != "" {
		t.Fatalf("plan ended as %q", ended)
	}
	editor.text[0] = 'X'
	history[0].Text = "changed"
	if request.question != "question" || request.transcript[0].Text != "old" {
		t.Fatalf("request changed after planning: %+v", request)
	}
}

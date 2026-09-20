package term

import (
	"testing"

	appconfig "github.com/TheR1D/aty/internal/config"
)

func TestQueryExecutionReleasesOwnershipExactlyOnce(t *testing.T) {
	state := newState(0, nil, appconfig.Limits{})
	q := &query{editor: queryEditor{text: []rune("where am i")}, submitted: "where am i"}
	keyboard := &keyboard{state: state, query: q}
	execution := &queryExecution{
		keyboard: keyboard,
		query:    q,
		plan:     queryRequest{},
		inject:   &injection{typed: []rune("pwd")},
	}

	execution.complete(nil)
	execution.complete(nil)

	if keyboard.query != nil {
		t.Fatal("completion did not release keyboard ownership")
	}
	if got := state.capture.pendingAnswer; got != "pwd" {
		t.Errorf("recorded answer = %q, want pwd", got)
	}
}

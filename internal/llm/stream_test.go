package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestReadStreamChoosesTheFirstNonEmptyReasoningField(t *testing.T) {
	for _, test := range []struct{ delta, want string }{
		{`{"reasoning_content":null,"reasoning":null,"thinking":null}`, ""},
		{`{"reasoning_content":"","reasoning":null,"thinking":"thinking"}`, "thinking"},
		{`{"reasoning_content":null,"reasoning":"reasoning","thinking":"thinking"}`, "reasoning"},
		{`{"reasoning_content":"content","reasoning":"reasoning","thinking":"thinking"}`, "content"},
	} {
		piece, err := readStream(fmt.Sprintf(`data: {"choices":[{"delta":%s}]}`, test.delta))
		if err != nil || piece.reasoning != test.want || piece.content != "" {
			t.Errorf("readStream(%s) = %+v, %v; want reasoning %q and no command", test.delta, piece, err, test.want)
		}
	}
}

func TestRejectedStreamDoesNotWaitForModel(t *testing.T) {
	for _, test := range []struct {
		name      string
		agent     bool
		reasoning bool
	}{
		{name: "command"},
		{name: "command reasoning", reasoning: true},
		{name: "agent", agent: true},
		{name: "agent reasoning", agent: true, reasoning: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
				piece := token("pwd")
				if test.reasoning {
					piece = "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"checking\"}}]}\n\n"
				}
				send(t, w, piece)
				// The model remains silent until the client closes the stream.
				<-r.Context().Done()
			})
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			rejected := errors.New("query no longer accepts output")
			emit, think := ignoreThought, ignoreThought
			refuse := func(string) error { return rejected }
			if test.reasoning {
				think = refuse
			} else {
				emit = refuse
			}

			var err error
			if test.agent {
				err = client.Agent(ctx, "where am I", nil, emit, think, nil, nil)
			} else {
				err = client.Command(ctx, "where am I", nil, test.reasoning, emit, think)
			}
			if !errors.Is(err, rejected) {
				t.Fatalf("request error = %v, want callback rejection", err)
			}
			if ctx.Err() != nil {
				t.Fatal("request waited for the deadline after its output was rejected")
			}
		})
	}
}

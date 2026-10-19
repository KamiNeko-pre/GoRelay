package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
)

func TestEquivalentSelectorsAndTinyRefinementsCannotRenewForever(t *testing.T) {
	for _, mode := range []string{"selector-alias", "tiny-refinement"} {
		t.Run(mode, func(t *testing.T) {
			s := progressReadState()
			s.Budget.LoopVersion = 5
			body := `<article id="content"><p>` + strings.Repeat("bounded source text ", 100) + `</p></article>`
			out, _ := toolData(ToolCall{ID: "seed", Function: FunctionCall{Name: "inspect_source"}}, inspectResponse(acquisition.Response{Body: body, Receipt: acquisition.Receipt{Method: "GET", URL: "https://fixture.example/detail", Status: 200, FetchedAt: time.Now()}}, acquisition.Stats{}))
			raw, _ := json.Marshal(out)
			s.Operations[0].ResultJSON = string(raw)
			s.HostProgress = advanceHostProgressForTest(HostProgress{}, s)
			for n := 0; n < 30; n++ {
				id := uint64(len(s.Operations) + 1)
				s.Operations = append(s.Operations, recordedModelRound(s, id))
				selector := "article"
				text, offset := strings.Repeat("x", 1000), 0
				if mode == "selector-alias" {
					selector = strings.Repeat(" ", n) + "#content" + strings.Repeat(" ", n)
				} else if n > 0 {
					text, offset = "x", n
				}
				read, _ := toolData(ToolCall{ID: fmt.Sprint(id), Function: FunctionCall{Name: "read_data"}}, map[string]any{"handle": "seed", "pointer": "", "selector": selector, "view_version": htmlInformationVersion, "offset": offset, "text": text})
				raw, _ := json.Marshal(read)
				s.Operations = append(s.Operations, agentdata.Operation{ID: id + 1, Kind: "read_data", Stage: "succeeded", ResultJSON: string(raw)})
				s.HostProgress = advanceHostProgressForTest(s.HostProgress, s)
			}
			action, err := RunTaskLoop(context.Background(), s)
			if err != nil || action.Kind != "finish" {
				t.Fatal("same already-read source manufactured progress", mode, action, err)
			}
		})
	}
}

func TestStructuredReadingProgressIsNotMisclassifiedAsEmptyText(t *testing.T) {
	s := progressReadState()
	s.Budget.LoopVersion = 5
	s.HostProgress = advanceHostProgressForTest(HostProgress{}, s)
	for n := 0; n < 20; n++ {
		id := uint64(len(s.Operations) + 1)
		s.Operations = append(s.Operations, recordedModelRound(s, id))
		out, _ := toolData(ToolCall{ID: fmt.Sprint(id), Function: FunctionCall{Name: "read_data"}}, map[string]any{"handle": "seed", "pointer": "/entries", "view_version": "json-value-v1", "offset": n, "value": []any{map[string]any{"id": fmt.Sprint(n)}}})
		raw, _ := json.Marshal(out)
		s.Operations = append(s.Operations, agentdata.Operation{ID: id + 1, Kind: "read_data", Stage: "succeeded", ResultJSON: string(raw)})
		s.HostProgress = advanceHostProgressForTest(s.HostProgress, s)
		action, err := RunTaskLoop(context.Background(), s)
		if err != nil || action.Kind != "model" || s.HostProgress.NoProgressModelRounds != 0 {
			t.Fatal("valid structured paging was killed", action, err, s.HostProgress)
		}
	}
}

func TestReadingItsOwnReceiptCannotInventNewSourceWork(t *testing.T) {
	s := progressReadState()
	s.Budget.LoopVersion = 5
	s.HostProgress = advanceHostProgressForTest(HostProgress{}, s)
	handle, pointer, version := "seed", "", "plain-text-v1"
	for n := 0; n < 25; n++ {
		id := uint64(len(s.Operations) + 1)
		s.Operations = append(s.Operations, recordedModelRound(s, id))
		callID := fmt.Sprint(id)
		// A real read_data reply always carries the resource URL, and that URL —
		// not the per-fetch handle — is what identifies the material. Every
		// round here reads the same document, so the URL is fixed while the
		// handle is fresh each time: exactly the shape a recorded run produced
		// when it re-inspected one page and read it again under a new handle.
		out, _ := toolData(ToolCall{ID: callID, Function: FunctionCall{Name: "read_data"}}, map[string]any{"handle": handle, "url": "https://fixture.example/detail", "pointer": pointer, "view_version": version, "offset": 0, "text": "same source text forever"})
		raw, _ := json.Marshal(out)
		s.Operations = append(s.Operations, agentdata.Operation{ID: id + 1, Key: "tool:" + callID, Kind: "read_data", Stage: "succeeded", ResultJSON: string(raw)})
		s.HostProgress = advanceHostProgressForTest(s.HostProgress, s)
		handle, pointer, version = callID, "/text", "receipt-json-v1"
	}
	action, err := RunTaskLoop(context.Background(), s)
	if err != nil || action.Kind != "finish" {
		t.Fatal("receipt IDs manufactured an infinite source-reading loop", action, err)
	}
}

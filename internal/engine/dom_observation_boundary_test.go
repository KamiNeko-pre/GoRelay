package engine

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/acquisition"
	"testing"
	"time"
)

func TestOnlyCapturedDOMSelectionsProduceExecutionObservations(t *testing.T) {
	for _, body := range []string{`<dl><dd class="value">15 GB</dd></dl>`, `<table><tr><td class="value">Annual 99</td></tr></table>`} {
		r, err := RunProgram(context.Background(), ProgramRequest{TargetURL: "https://source.example/item", Code: `local b=assert(http_get(TARGET_URL));html_find_all(b,".value");html_find_all("<b class='invented'>made up</b>",".invented");return true,{items={{report="arbitrary"}}}`}, DefaultProgramLimits(), func(context.Context, string, string) (acquisition.Response, error) {
			return acquisition.Response{Body: body, Receipt: acquisition.Receipt{Method: "GET", URL: "https://source.example/item", Status: 200, FetchedAt: time.Now()}}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(r)
		var wire struct {
			Observations []struct {
				Selector string `json:"selector"`
				Digest   string `json:"value_digest"`
				Matches  int    `json:"matches"`
			} `json:"execution_observations"`
		}
		json.Unmarshal(raw, &wire)
		if len(wire.Observations) != 1 || wire.Observations[0].Selector != ".value" || wire.Observations[0].Digest == "" || wire.Observations[0].Matches != 1 {
			t.Fatalf("captured source selection missing or invented markup trusted: %s", raw)
		}
	}
}

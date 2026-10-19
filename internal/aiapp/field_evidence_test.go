package aiapp

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/engine"
)

func TestFieldSupportUsesHostResponseAndLocalLocator(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		value      any
		locator    map[string]any
		supported  bool
	}{
		{"forged_price", `<article id="main"><h1>Project A</h1><p>Budget: 53</p></article>`, "199", map[string]any{"selector": "#main p"}, false},
		{"other_subject", `<article id="main"><h1>Project A</h1><p>Budget: 53</p></article><aside>Budget: 199</aside>`, "Budget: 199", map[string]any{"selector": "#main p"}, false},
		{"supported_dom", `<article id="main"><h1>Project A</h1><p>Budget: 53</p></article>`, "Budget: 53", map[string]any{"selector": "#main p"}, true},
		{"supported_json", `{"project":{"budget":53},"other":{"budget":199}}`, json.Number("53"), map[string]any{"json_pointer": "/project/budget"}, true},
		{"wrong_json_field", `{"project":{"budget":53},"other":{"budget":199}}`, json.Number("199"), map[string]any{"json_pointer": "/project/budget"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := "https://source.example/detail"
			receipt := acquisition.Receipt{URL: url, Method: http.MethodGet, Status: 200, FetchedAt: time.Now()}
			row := map[string]any{"value": tc.value, "source_url": url, "_evidence": map[string]any{"value": tc.locator}}
			result := engine.ProgramResult{Collection: map[string]any{"items": []any{row}}, Requests: []acquisition.Receipt{receipt}, SourceEvidence: []acquisition.Response{{Body: tc.body, Receipt: receipt}}}
			records, err := BindRecords(result, RecordBinding{SourceID: "s", DataVersion: "v", TaskID: 1, SchemaVersion: "v1"})
			if err != nil {
				t.Fatal(err)
			}
			got := records[0].Provenance.FieldSupport["value"].Status == "supported"
			if got != tc.supported {
				t.Fatalf("support=%v wanted %v: %+v", got, tc.supported, records[0])
			}
			if _, ok := records[0].Fields["_evidence"]; ok {
				t.Fatal("proof proposals entered business fields")
			}
		})
	}
}

func TestTrialModelReceiptDoesNotExposeHostHTML(t *testing.T) {
	raw, _ := json.Marshal(engine.ProgramResult{SourceEvidence: []acquisition.Response{{Body: "<html>host-only-body</html>"}}, Collection: map[string]any{"items": []any{}}})
	view, err := candidateReceipt(Candidate{ChecksJSON: `{}`, ResultJSON: string(raw)})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	json.Unmarshal(view, &decoded)
	if _, ok := decoded["result"].(map[string]any)["source_evidence"]; ok {
		t.Fatal("raw response crossed model receipt boundary")
	}
}

func TestCandidateReceiptNamesTheExactSubmissionIdentity(t *testing.T) {
	for _, id := range []string{"new-adapter-candidate", "temporary-composition-candidate"} {
		raw, err := candidateReceipt(Candidate{ID: id, ChecksJSON: `{}`, ResultJSON: `{"collection":{"items":[]}}`})
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		_ = json.Unmarshal(raw, &got)
		if got["candidate_id"] != id {
			t.Fatalf("tool asks for candidate_id but receipt labels it only id: %s", raw)
		}
	}
}

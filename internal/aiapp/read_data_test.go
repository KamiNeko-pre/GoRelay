package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func TestUnifiedReadUsesNamedFieldsAndRunScopedReceipts(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "read-data", Goal: "read inspected data"})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`[{"version":"1.2","date":"2026-10-01","noise":{"x":"large metadata"}},{"version":"1.1","date":"2026-09-01"}]`, `{"data":{"items":[{"version":"2.0","date":"2026-10-01"}]},"metadata":{"nested":"unused"}}`} {
		id := agentdata.Token()
		receipt, _ := toolData(ToolCall{ID: id, Function: FunctionCall{Name: "inspect_source"}}, inspectResponse(acquisition.Response{Body: body, Receipt: acquisition.Receipt{Method: "GET", URL: "https://fixture.example/data", Status: 200, FetchedAt: time.Now()}}, acquisition.Stats{}))
		raw, _ := json.Marshal(receipt)
		if err := s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: "tool:" + id, Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(raw)}).Error; err != nil {
			t.Fatal(err)
		}
		args, _ := json.Marshal(map[string]any{"handle": id, "fields": []string{"version", "date"}})
		call := ToolCall{ID: "read-" + id, Type: "function", Function: FunctionCall{Name: "read_data", Arguments: string(args)}}
		result, err := s.executeDurableTool(ctx, r, call)
		if err != nil {
			t.Fatal("named-field reader unavailable", err)
		}
		var data struct {
			Matches  []map[string]any `json:"matches"`
			Requests int              `json:"new_http_requests"`
		}
		_ = json.Unmarshal(result.Data, &data)
		if len(data.Matches) == 0 || data.Requests != 0 {
			t.Fatal("no field values or another HTTP request")
		}
		foreign, _ := s.Create(ctx, CreateRunRequest{RequestKey: "foreign-" + id, Goal: "other task"})
		foreign, err = s.Data.Claim(ctx, foreign.ID, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.executeDurableTool(ctx, foreign, call); err == nil {
			t.Fatal("another run borrowed the receipt")
		}
	}
}

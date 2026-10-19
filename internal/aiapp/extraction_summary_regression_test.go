package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"testing"
)

func TestExtractionFinishDoesNotPromoteOptionalTextToRAG(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, summary                string
		required, citations, wantErr bool
	}{
		{"titles", "Three titles extracted from the source.", false, false, false},
		{"prices", "The requested prices are in the delivered records.", false, false, false},
		{"required-summary", "", true, false, true},
		{"invalid-citation", "Claim", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, _ := json.Marshal(TaskSpec{RequireSummary: tc.required})
			args := map[string]any{"status": "completed", "reason": "records ready", "summary": tc.summary}
			if tc.citations {
				args["citations"] = []string{"ev_unknown"}
			}
			raw, _ := json.Marshal(args)
			out, err := s.finishProposal(context.Background(), agentdata.Run{ID: tc.name, SpecJSON: string(spec)}, ToolCall{ID: "finish", Function: FunctionCall{Name: "finish_task", Arguments: string(raw)}})
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected finish error: %v", err)
			}
			if err == nil {
				var p finishProposal
				_ = json.Unmarshal(out.Data, &p)
				if p.Answer != nil {
					t.Fatal("optional text falsely became evidence-backed RAG answer")
				}
			}
		})
	}
}

package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func TestRegistrationAndReceiptRollBackAsOneTransaction(t *testing.T) {
	db := testdb.Open(t)
	s := NewService(db)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	spec := TaskSpec{Goal: "goal", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{"name", "string"}}}
	r, _ := s.Create(ctx, CreateRunRequest{RequestKey: "atomic-registry", Goal: "goal", Spec: &spec})
	r, _ = s.Data.Claim(ctx, r.ID, time.Minute)
	a, _ := s.Data.SaveArtifact(ctx, agentdata.ProgramArtifact{Code: "code", PolicyVersion: "public-read-v1"})
	c := Candidate{ID: agentdata.Token(), RunID: r.ID, ArtifactHash: a.Hash, SourceID: "test", ChecksJSON: `{"fixture":true}`}
	if err := db.Create(&c).Error; err != nil {
		t.Fatal(err)
	}
	s.Admission = func(context.Context, AdmissionContext) (AdmissionDecision, error) {
		return AdmissionDecision{Allowed: true}, nil
	} // test-only core fixture
	if err := db.Exec(`CREATE TRIGGER fail_register_receipt BEFORE UPDATE ON operations FOR EACH ROW BEGIN IF NEW.kind='register_source' AND NEW.stage='succeeded' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='receipt fault'; END IF; END`).Error; err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"candidate_id": c.ID, "description": "fixture"})
	call := ToolCall{ID: "reg", Type: "function", Function: FunctionCall{Name: "register_source", Arguments: string(args)}}
	if _, err := s.executeDurableTool(ctx, r, call); err == nil {
		t.Fatal("receipt fault ignored")
	}
	var count int64
	db.Model(&agentdata.SourceHead{}).Count(&count)
	if count != 0 {
		t.Fatal("registry activated without durable receipt")
	}
	db.Exec("DROP TRIGGER fail_register_receipt")
	if _, err := s.executeDurableTool(ctx, r, call); err != nil {
		t.Fatal(err)
	}
	ops, _ := s.Data.Operations(ctx, r.ID)
	if len(ops) != 1 || ops[0].Stage != "succeeded" {
		t.Fatal("idempotent receipt not completed")
	}
}

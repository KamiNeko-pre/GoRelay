package aiapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
)

// These are deliberately invalid proposals that a hostile document might
// induce. Host policy, not the model recognizing hostile wording, rejects them.
func TestSourceInducedPermissionProposalsCannotExpandHostAuthority(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	spec := TaskSpec{Goal: "read public component facts", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "name", Type: "string"}}}
	r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "permissions", Goal: spec.Goal, SourceScope: []string{"fixture.example"}, Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(context.Background(), r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { attempts.Add(1); w.Write([]byte("must never be read")) }))
	defer trap.Close()
	for n, a := range []FunctionCall{
		{Name: "read_local_file", Arguments: `{"path":"C:/private.txt"}`},
		{Name: "inspect_source", Arguments: `{"url":"file:///C:/private.txt"}`},
		{Name: "inspect_source", Arguments: `{"url":"https://outside.example/data"}`},
		{Name: "inspect_source", Arguments: `{"url":"https://fixture.example@127.0.0.1/private"}`},
		{Name: "inspect_source", Arguments: `{"url":"` + trap.URL + `"}`},
		{Name: "plan_task", Arguments: `{"goal":"replace original goal","required_fields":[{"name":"name","type":"string"}]}`},
	} {
		call := ToolCall{ID: agentdata.Token(), Type: "function", Function: a}
		if _, err := s.executeDurableTool(context.Background(), r, call); err == nil {
			t.Fatal("host accepted forbidden proposal", n)
		}
	}
	if attempts.Load() != 0 {
		t.Fatal("loopback HTTP reached before policy rejection")
	}
	current, err := s.Data.Run(context.Background(), r.ID)
	if err != nil || current.Goal != r.Goal || !sameContractJSON([]byte(current.SpecJSON), []byte(r.SpecJSON)) {
		t.Fatal("goal or frozen contract changed", err)
	}
	var children int64
	if err := s.Data.DB.Model(&agentdata.RunTask{}).Where("run_id = ?", r.ID).Count(&children).Error; err != nil || children != 0 {
		t.Fatal("invalid proposal created work", err)
	}
	var input CreateRunRequest
	if err := json.Unmarshal([]byte(current.InputJSON), &input); err != nil || len(input.SourceScope) != 1 || input.SourceScope[0] != "fixture.example" {
		t.Fatal("source scope changed", err)
	}
}

package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"strings"
	"testing"
	"time"
)

func TestDeliveryContractRejectsNonObjectArguments(t *testing.T) {
	for _, raw := range []string{"null", "[]", `"text"`, `true`, `{"count":1} {}`} {
		var a struct {
			Count int `json:"count"`
		}
		err := decodeArgs(raw, &a)
		if err == nil {
			t.Fatalf("non-object accepted: %s", raw)
		}
		f := captureFailure(err, 0, false)
		if f.Category != "validation" || f.ExecutionStatus != "not_started" || f.Details == nil {
			t.Fatalf("wrong decode receipt: %+v", f)
		}
	}
}

func TestDeliveryReaderFailureOffersActualStructureInFinalContext(t *testing.T) {
	for _, body := range []string{`<main><h1 class="notice-title">Notice</h1></main>`, `<article><h2 id="product-name">Product</h2></article>`} {
		s, run, _ := storedHTMLReader(t, body)
		run, err := s.Data.Claim(context.Background(), run.ID, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		call := ToolCall{ID: "bad-selector", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"source","view":"dom","selector":".does-not-exist"}`}}
		reply, err := s.executeDurableTool(context.Background(), run, call)
		if err == nil {
			t.Fatal("missing selector unexpectedly succeeded")
		}
		if reply.Failure == nil || reply.Failure.Code != "SOURCE_SELECTOR_NOT_FOUND" || reply.Failure.ExecutionStatus != "not_started" || reply.Failure.Details == nil {
			t.Fatalf("selector not typed: %+v", reply)
		}
		raw, _ := json.Marshal(reply)
		ops, e := s.Data.Operations(context.Background(), run.ID)
		if e != nil {
			t.Fatal(e)
		}
		state := LoopState{Run: run, Operations: ops, Budget: BudgetSnapshot{LoopVersion: 5}, Messages: []Message{{Role: "assistant", ToolCalls: []ToolCall{call}}, {Role: "tool", ToolCallID: call.ID, Content: string(raw)}}}
		got, e := CompactContext(state, state.Messages)
		if e != nil {
			t.Fatal(e)
		}
		encoded := string(mustJSON(got))
		if !strings.Contains(encoded, "selector_catalog") || !strings.Contains(encoded, "read_with") || !strings.Contains(encoded, "source") {
			t.Fatal("actual recovery structure did not reach model")
		}
		var stored agentdata.Operation
		for _, op := range ops {
			if op.Key == "tool:"+call.ID {
				stored = op
			}
		}
		if !strings.Contains(stored.ResultJSON, "SOURCE_SELECTOR_NOT_FOUND") {
			t.Fatal("durable receipt differs from model feedback")
		}
	}
}

func TestDeliveryExtractionErrorNamesFieldAndActualSource(t *testing.T) {
	for _, locator := range []string{`{"title":{"selector":".missing"}}`, `{"title":{"json_pointer":"/missing"}}`} {
		body := `<main><h1 id="heading">Actual title</h1></main>`
		if strings.Contains(locator, "json_pointer") {
			body = `{"actual_title":"Actual API title"}`
		}
		s, r, _ := storedHTMLReader(t, body)
		var err error
		r, err = s.Data.Claim(context.Background(), r.ID, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		spec := TaskSpec{Goal: r.Goal, ContractVersion: "v2", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "title", Type: "string"}}, JoinKeys: []string{"title"}, RequireFieldSupport: true}
		raw, _ := json.Marshal(spec)
		r.SpecJSON = string(raw)
		if err = s.Data.DB.Model(&r).Update("spec_json", r.SpecJSON).Error; err != nil {
			t.Fatal(err)
		}
		call := ToolCall{ID: "bad-extraction", Type: "function", Function: FunctionCall{Name: "submit_extraction", Arguments: `{"inspection_call_id":"source","field_selectors":` + locator + `}`}}
		reply, err := s.executeDurableTool(context.Background(), r, call)
		if err == nil {
			t.Fatal("missing field unexpectedly extracted")
		}
		if reply.Failure == nil || reply.Failure.Code != "FIELD_EXTRACTION_ERROR" || reply.Failure.Details == nil || reply.Failure.ExecutionStatus != "not_started" {
			t.Fatalf("missing field diagnosis: %+v", reply)
		}
		data := string(mustJSON(reply.Failure))
		if !strings.Contains(data, `"field":"title"`) || !strings.Contains(data, `"handle":"source"`) {
			t.Fatalf("lost target: %s", data)
		}
		children, e := s.ChildResults(context.Background(), r.ID)
		if e != nil || len(children) != 0 {
			t.Fatalf("invalid extraction created results: %d %v", len(children), e)
		}
	}
}

func TestDeliveryOutputDeclarationErrorIncludesFrozenRequirements(t *testing.T) {
	spec := TaskSpec{Goal: "collect title", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "title", Type: "string"}, {Name: "body", Type: "string"}}, JoinKeys: []string{"title"}}
	for _, fields := range [][]string{{"href"}, {"title", "title"}, {"body"}} {
		_, err := OutputContract(spec, fields)
		if err == nil {
			t.Fatal("invalid declaration accepted")
		}
		f := captureFailure(err, 0, false)
		if f.Code != "OUTPUT_DECLARATION_ERROR" || f.ExecutionStatus != "not_started" || f.Details == nil {
			t.Fatalf("frozen contract diagnosis lost: %+v", f)
		}
	}
	out, err := OutputContract(spec, []string{"title"})
	if err != nil || len(out.RequiredFields) != 1 {
		t.Fatal("valid complementary source rejected", err)
	}
}

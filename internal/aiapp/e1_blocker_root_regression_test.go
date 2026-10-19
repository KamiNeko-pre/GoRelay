package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/engine"
	"strings"
	"testing"
	"time"
)

func blockerSource(url, body string) acquisition.Response {
	return acquisition.Response{Body: body, Receipt: acquisition.Receipt{Method: "GET", URL: url, Status: 200, FetchedAt: time.Now()}}
}
func TestE1BlockerCrossSourceProof(t *testing.T) {
	for _, name := range []string{"Atlas", "Beacon"} {
		t.Run(name, func(t *testing.T) {
			a := blockerSource("https://proof.example/identity", `{"name":"`+name+`"}`)
			b := blockerSource("https://proof.example/policy", `{"owner":"Lab"}`)
			r := engine.ProgramResult{SourceEvidence: []acquisition.Response{a, b}}
			fields := map[string]any{"name": name, "owner": "Lab"}
			proof := map[string]any{"name": map[string]any{"source_url": a.Receipt.URL, "json_pointer": "/name"}, "owner": map[string]any{"json_pointer": "/owner"}}
			support := hostFieldSupport(r, b.Receipt.URL, fields, proof)
			for k, s := range support {
				if s.Status != "supported" {
					t.Errorf("valid cross-source %s rejected: %+v", k, s)
				}
			}
			proof["name"].(map[string]any)["source_url"] = "https://proof.example/unfetched"
			if s := hostFieldSupport(r, b.Receipt.URL, fields, proof)["name"]; s.Status == "supported" {
				t.Fatal("unfetched URL accepted")
			}
			plain := hostFieldSupport(r, a.Receipt.URL, map[string]any{"name": name}, map[string]any{"name": map[string]any{"json_pointer": "/name"}})
			if plain["name"].Status != "supported" {
				t.Fatal("valid single-source path regressed")
			}
		})
	}
}
func TestE1BlockerJSONMemberKeys(t *testing.T) {
	for _, c := range []struct{ body, pointer, want string }{{`{"members":{"unit-7":{"name":"Atlas"}}}`, "/members/unit-7", "unit-7"}, {`{"x":{"a/b~c":5}}`, "/x/a~1b~0c", "a/b~c"}} {
		var loc fieldLocator
		json.Unmarshal([]byte(`{"json_key_pointer":"`+c.pointer+`"}`), &loc)
		v, _, err := evidenceValue(blockerSource("https://proof.example/data", c.body), loc)
		if err != nil || v != c.want {
			t.Errorf("JSON member key not reproducible: %v %v", v, err)
		}
	}
	var loc fieldLocator
	json.Unmarshal([]byte(`{"json_key_pointer":"/items/0"}`), &loc)
	if _, _, err := evidenceValue(blockerSource("https://proof.example/data", `{"items":["zero"]}`), loc); err == nil {
		t.Fatal("array position misrepresented as object key")
	}
}
func blockerFailure(t *testing.T, proof any) ToolResult {
	t.Helper()
	u := "https://proof.example/data"
	source := blockerSource(u, `<main><b data-id="7">seven</b></main>`)
	support := hostFieldSupport(engine.ProgramResult{SourceEvidence: []acquisition.Response{source}}, u, map[string]any{"id": "7"}, proof)
	rec := Record{Fields: map[string]any{"id": "7"}, Provenance: Provenance{URL: u, FieldSupport: support}}
	err := requireSupportedFields(TaskSpec{RequireFieldSupport: true}, []Record{rec})
	if err == nil {
		t.Fatal("expected a real field proof failure")
	}
	return toolFailureReceipt(ToolCall{ID: "failed-proof", Function: FunctionCall{Name: "validate_program"}}, ToolResult{}, err)
}
func TestE1BlockerFailureIdentityAndBehavior(t *testing.T) {
	missing := blockerFailure(t, map[string]any{})
	dom := blockerFailure(t, map[string]any{"id": map[string]any{"pattern": "(7)"}})
	if structuralFailureKey("validate_program", missing) == structuralFailureKey("validate_program", dom) {
		t.Error("different actionable diagnoses collapsed into same field error")
	}
	a := blockerFailure(t, map[string]any{"id": map[string]any{"selector": ".missing-a"}})
	b := blockerFailure(t, map[string]any{"id": map[string]any{"selector": ".missing-b"}})
	state := efficiencyState()
	appendFailure := func(id uint64, out ToolResult) agentdata.Operation {
		call := ToolCall{ID: out.CallID, Function: FunctionCall{Name: "validate_program", Arguments: "{}"}}
		model := modelRound("round", call)
		model.ID = id
		model.RunID = state.Run.ID
		state.Operations = append(state.Operations, model, agentdata.Operation{ID: id + 1, RunID: state.Run.ID, Kind: "validate_program", Key: "tool:" + out.CallID, Stage: "failed", ResultJSON: string(mustJSON(out))})
		return model
	}
	a.CallID = "a"
	b.CallID = "b"
	ma := appendFailure(1, a)
	mb := appendFailure(3, b)
	if correctionBehavior(state, ma) == correctionBehavior(state, mb) {
		t.Error("changed locator treated as unchanged behavior")
	}
	b.Failure.Message = "different prose for same mechanical cause"
	b.Error = "different wording"
	b.CallID = "c"
	mc := appendFailure(5, b)
	if correctionBehavior(state, mb) != correctionBehavior(state, mc) {
		t.Error("message wording changed behavior identity")
	}
}

func TestE1BlockerDurableCrossSourceExtraction(t *testing.T) {
	for _, name := range []string{"Atlas", "Beacon"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s, r, _ := storedHTMLReader(t, `{"id":"unit-7","name":"`+name+`"}`)
			page, err := s.inspection(ctx, r, "source")
			if err != nil {
				t.Fatal(err)
			}
			u := page.Source.Receipt.URL + "/policy"
			second := inspectResponse(blockerSource(u, `{"owner":"North"}`), acquisition.Stats{})
			out, _ := toolData(ToolCall{ID: "policy-source", Function: FunctionCall{Name: "inspect_source"}}, second)
			if err = s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: "tool:policy-source", InputHash: "policy-source", Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(mustJSON(out))}).Error; err != nil {
				t.Fatal(err)
			}
			r, err = s.Data.Claim(ctx, r.ID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			spec := TaskSpec{Goal: r.Goal, ContractVersion: "v2", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "id", Type: "string"}, {Name: "name", Type: "string"}, {Name: "owner", Type: "string"}}, JoinKeys: []string{"id"}, MinRecords: 1, RequireFieldSupport: true}
			r.SpecJSON = string(mustJSON(spec))
			if err = s.Data.DB.Model(&r).Update("spec_json", r.SpecJSON).Error; err != nil {
				t.Fatal(err)
			}
			args := map[string]any{"inspection_call_id": "source", "field_selectors": map[string]any{"id": map[string]any{"json_pointer": "/id"}, "name": map[string]any{"json_pointer": "/name"}, "owner": map[string]any{"source_url": u, "json_pointer": "/owner"}}}
			call := ToolCall{ID: "joined-fields", Function: FunctionCall{Name: "submit_extraction", Arguments: string(mustJSON(args))}}
			if _, err = s.executeDurableTool(ctx, r, call); err != nil {
				t.Fatal("durable joined extraction rejected", err)
			}
			result, err := s.Evaluate(ctx, r)
			if err != nil || !result.Coverage.Satisfied {
				t.Fatal("full delivery coverage not reached", result, err)
			}
			children, childErr := s.ChildResults(ctx, r.ID)
			if childErr != nil || len(children) != 1 || len(children[0].Records) != 1 {
				t.Fatal("missing persisted evidence", children, childErr)
			}
			if !strings.Contains(strings.Join(children[0].Records[0].Provenance.FieldSupport["owner"].EvidenceRefs, " "), u) {
				t.Fatal("field provenance rebound to wrong primary source")
			}
			args["field_selectors"].(map[string]any)["owner"].(map[string]any)["source_url"] = u + "-unfetched"
			call.ID = "unfetched-proof"
			call.Function.Arguments = string(mustJSON(args))
			if _, err = s.executeDurableTool(ctx, r, call); err == nil {
				t.Fatal("unfetched source admitted by durable path")
			}
		})
	}
}

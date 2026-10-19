package aiapp

import (
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/testdb"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func deliveryConditionSpec(t *testing.T, object string) TaskSpec {
	t.Helper()
	raw := `{"goal":"compare modes","contract_version":"v2","schema_version":"v1","required_fields":[{"name":"object","type":"string"},{"name":"mode","type":"string"},{"name":"price","type":"number"}],"join_keys":["object","mode"],"min_records":2,"required_combinations":[{"id":"basic","equals":{"object":"` + object + `","mode":"basic"},"min_records":1},{"id":"extended","equals":{"object":"` + object + `","mode":"extended"},"min_records":1}]}`
	var spec TaskSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

func deliveryRecord(object, mode string, now time.Time) Record {
	return Record{Key: object + ":" + mode, SchemaVersion: "v1", Fields: map[string]any{"object": object, "mode": mode, "price": 1.25}, Provenance: Provenance{SourceID: "pricing", URL: "https://example.com/pricing", FetchedAt: now, DataVersion: "page-1", TaskID: 1}}
}

func TestDeliveryConditionsMissingCombinationCannotComplete(t *testing.T) {
	now := time.Now().UTC()
	for _, object := range []string{"Alpha", "Beta"} {
		t.Run(object, func(t *testing.T) {
			spec := deliveryConditionSpec(t, object)
			rows := []Record{deliveryRecord(object, "basic", now), deliveryRecord("Other", "extended", now)}
			for _, merged := range []bool{false, true} {
				var got CoverageReport
				var err error
				if merged {
					joined, e := MergeRecords(rows, spec.JoinKeys)
					if e != nil {
						t.Fatal(e)
					}
					got, err = EvaluateMergedRecords(spec, joined, now)
				} else {
					got, err = EvaluateRecords(spec, rows, now)
				}
				if err != nil || got.Satisfied || taskDeliverySatisfied(&TaskResult{Coverage: got}) {
					t.Fatalf("missing mode falsely completed: %+v %v", got, err)
				}
				encoded, _ := json.Marshal(got)
				var detail struct {
					Combinations []struct {
						ID        string `json:"id"`
						Satisfied bool   `json:"satisfied"`
					} `json:"combinations"`
				}
				if err := json.Unmarshal(encoded, &detail); err != nil || len(detail.Combinations) != 2 || detail.Combinations[1].ID != "extended" || detail.Combinations[1].Satisfied {
					t.Fatal("missing condition not explained", string(encoded))
				}
			}
			rows[1] = deliveryRecord(object, "extended", now)
			joined, _ := MergeRecords(rows, spec.JoinKeys)
			got, err := EvaluateMergedRecords(spec, joined, now)
			if err != nil || !got.Satisfied {
				t.Fatal("complete sibling rejected", got, err)
			}
		})
	}
}

func TestDeliveryConditionsDuplicateRowsCannotMeetMinimum(t *testing.T) {
	now := time.Now().UTC()
	spec := deliveryConditionSpec(t, "Alpha")
	raw, _ := json.Marshal(spec)
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	delete(fields, "required_combinations")
	raw, _ = json.Marshal(fields)
	spec = TaskSpec{}
	_ = json.Unmarshal(raw, &spec)
	row := deliveryRecord("Alpha", "basic", now)
	duplicate := row
	duplicate.Key = "another-source-key"
	got, err := EvaluateRecords(spec, []Record{row, duplicate}, now)
	if err != nil || got.Satisfied {
		t.Fatal("duplicate business row counted twice", got, err)
	}
}

func TestDeliveryConditionsRejectInvalidContractAndKeepLegacyReadable(t *testing.T) {
	base := `{"goal":"x","schema_version":"v1","required_fields":[{"name":"name","type":"string"}]`
	for _, suffix := range []string{
		`,"contract_version":"future"}`,
		`,"min_records":-1}`,
		`,"contract_version":"v2","min_records":2}`,
		`,"contract_version":"v2","required_combinations":[{"id":"bad","equals":{"unknown":"x"},"min_records":1}]}`,
		`,"contract_version":"v2","required_combinations":[{"id":"bad","equals":{"name":12},"min_records":1}]}`,
		`,"contract_version":"v2","required_combinations":[{"id":"bad","equals":{},"min_records":1}]}`,
		`,"required_combinations":[{"id":"unversioned","equals":{"name":"x"},"min_records":1}]}`,
	} {
		var spec TaskSpec
		if err := json.Unmarshal([]byte(base+suffix), &spec); err != nil {
			t.Fatal(err)
		}
		if err := spec.Validate(); err == nil {
			t.Fatal("invalid conditions accepted", suffix)
		}
	}
	var legacy TaskSpec
	_ = json.Unmarshal([]byte(base+`}`), &legacy)
	if err := legacy.Validate(); err != nil {
		t.Fatal("legacy contract became unreadable", err)
	}
}

func TestDeliveryConditionsRealURLDoesNotProveFieldSupport(t *testing.T) {
	now := time.Now().UTC()
	raw := `{"goal":"x","contract_version":"v2","schema_version":"v1","required_fields":[{"name":"price","type":"number"}],"require_field_support":true}`
	var spec TaskSpec
	_ = json.Unmarshal([]byte(raw), &spec)
	row := deliveryRecord("Alpha", "basic", now)
	row.Fields["price"] = 999999
	got, err := EvaluateRecords(spec, []Record{row}, now)
	if err != nil || got.Satisfied {
		t.Fatal("URL-only fabricated value counted as supported", got, err)
	}
	joined, _ := MergeRecords([]Record{row}, nil)
	got, err = EvaluateMergedRecords(spec, joined, now)
	if err != nil || got.Satisfied {
		t.Fatal("merge laundered unsupported field", got, err)
	}
}

func TestDeliveryConditionsSourceSubsetDoesNotInheritTaskCompleteness(t *testing.T) {
	spec := deliveryConditionSpec(t, "Alpha")
	for _, produced := range [][]string{nil, {"object", "mode"}} {
		out, err := OutputContract(spec, produced)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(out)
		var fields map[string]any
		_ = json.Unmarshal(raw, &fields)
		if fields["required_combinations"] != nil || fields["min_records"] != nil {
			t.Fatal("partial source required to deliver the whole task", string(raw))
		}
	}
}

func TestDeliveryConditionsLuaCannotDeclareOwnFieldSupport(t *testing.T) {
	now := time.Now().UTC()
	spec := TaskSpec{Goal: "price", ContractVersion: "v2", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{"price", "number"}}, RequireFieldSupport: true}
	result := engine.ProgramResult{
		Collection: map[string]any{"items": []any{map[string]any{"price": 999, "source_url": "https://example.com/pricing", "field_support": map[string]any{"price": map[string]any{"status": "supported", "checker_version": "invented", "evidence_refs": []string{"invented"}}}}}},
		Requests:   []acquisition.Receipt{{Method: http.MethodGet, URL: "https://example.com/pricing", Status: 200, FetchedAt: now}},
	}
	rows, err := BindRecords(result, RecordBinding{SourceID: "prices", DataVersion: "v1", TaskID: 1, SchemaVersion: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := EvaluateRecords(spec, rows, now)
	if err != nil || got.Satisfied || fieldIsSupported(rows[0].Provenance, "price") || fieldIsSupported(rows[0].Provenance, "field_support") {
		t.Fatal("Lua counterfeit check became trusted metadata", got, err)
	}
}

func TestDeliveryConditionsNormalEntryAndFrozenProposal(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GORELAY_ADMIN_TOKEN", "condition-test")
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRoutes(router, s)
	spec := deliveryConditionSpec(t, "Alpha")
	request := CreateRunRequest{RequestKey: "conditions-normal-entry", Goal: spec.Goal, Spec: &spec}
	body, _ := json.Marshal(request)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/runs", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer condition-test")
	out := httptest.NewRecorder()
	router.ServeHTTP(out, req)
	if out.Code != http.StatusAccepted {
		t.Fatal(out.Code, out.Body.String())
	}
	var accepted struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	r, err := s.Data.Claim(context.Background(), accepted.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	proposal := spec
	proposal.Goal, proposal.SchemaVersion, proposal.ContractVersion = "", "", ""
	args, _ := json.Marshal(proposal)
	call := ToolCall{ID: "repeat-plan", Type: "function", Function: FunctionCall{Name: "plan_task", Arguments: string(args)}}
	if _, err := s.executeDurableTool(context.Background(), r, call); err != nil {
		t.Fatal("host metadata omission rejected unchanged requirements", err)
	}
	proposal.RequiredCombinations = proposal.RequiredCombinations[:1]
	args, _ = json.Marshal(proposal)
	call.ID, call.Function.Arguments = "reduced-plan", string(args)
	if _, err := s.executeDurableTool(context.Background(), r, call); err == nil {
		t.Fatal("model deleted a frozen condition")
	}
	stored, err := s.Data.Run(context.Background(), r.ID)
	if err != nil || !sameContractJSON([]byte(stored.SpecJSON), []byte(r.SpecJSON)) {
		t.Fatal("frozen conditions changed", err)
	}
}

func TestDeliveryConditionsToolContractPublishesNewFields(t *testing.T) {
	for _, tool := range AgentToolDefinitions() {
		if tool.Function.Name != "plan_task" {
			continue
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		raw, _ := json.Marshal(tool.Function.Parameters)
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"contract_version", "min_records", "required_combinations", "require_field_support"} {
			if schema.Properties[name] == nil {
				t.Fatal("model tool contract lacks", name)
			}
		}
		return
	}
	t.Fatal("plan_task missing")
}

func TestDeliveryConditionsSupportedFieldsRemainUsableAndNumericMatchesAreExact(t *testing.T) {
	now := time.Now().UTC()
	spec := deliveryConditionSpec(t, "Alpha")
	spec.MinRecords = 1
	spec.RequiredCombinations = []RecordCondition{{ID: "number", Equals: map[string]any{"price": json.Number("1.250")}, MinRecords: 1}}
	spec.RequireFieldSupport = true
	row := deliveryRecord("Alpha", "basic", now)
	row.Provenance.FieldSupport = map[string]FieldSupport{}
	for _, field := range spec.RequiredFields {
		row.Provenance.FieldSupport[field.Name] = FieldSupport{Status: "supported", CheckerVersion: "fixture-check-v1", EvidenceRefs: []string{"receipt-1/node-1"}}
	}
	for _, merged := range []bool{false, true} {
		var got CoverageReport
		var err error
		if merged {
			joined, _ := MergeRecords([]Record{row}, spec.JoinKeys)
			got, err = EvaluateMergedRecords(spec, joined, now)
		} else {
			got, err = EvaluateRecords(spec, []Record{row}, now)
		}
		if err != nil || !got.Satisfied {
			t.Fatal("host checked fields or equivalent number rejected", got, err)
		}
	}
}

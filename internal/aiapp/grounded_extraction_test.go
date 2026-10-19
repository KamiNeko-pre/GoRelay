package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/acquisition"
	"strings"
	"testing"
	"time"
)

func TestDeclaredFieldExtractionUsesThePersistedSource(t *testing.T) {
	for _, label := range []string{"Procurement", "Game update"} {
		t.Run(label, func(t *testing.T) {
			body := `<nav>menu</nav><article><h2 id="heading">` + label + `</h2><p>` + strings.Repeat("Complete original paragraph. ", 250) + `</p><script>execute()</script></article>`
			s, r, _ := storedHTMLReader(t, body)
			var err error
			r, err = s.Data.Claim(context.Background(), r.ID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			spec := TaskSpec{Goal: r.Goal, ContractVersion: "v2", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "title", Type: "string"}, {Name: "body", Type: "source_text"}}, JoinKeys: []string{"title"}, MinRecords: 1, RequireFieldSupport: true}
			raw, _ := json.Marshal(spec)
			r.SpecJSON = string(raw)
			if err := s.Data.DB.Model(&r).Update("spec_json", r.SpecJSON).Error; err != nil {
				t.Fatal(err)
			}
			call := ToolCall{ID: "extract-declared", Type: "function", Function: FunctionCall{Name: "submit_extraction", Arguments: `{"inspection_call_id":"source","field_selectors":{"title":{"selector":"#heading"},"body":{"source_text":true}}}`}}
			if _, err := s.executeDurableTool(context.Background(), r, call); err != nil {
				t.Fatal(err)
			}
			if _, err := s.executeDurableTool(context.Background(), r, call); err != nil {
				t.Fatal("retry lost idempotency", err)
			}
			children, err := s.ChildResults(context.Background(), r.ID)
			if err != nil || len(children) != 1 || len(children[0].Records) != 1 {
				t.Fatalf("missing or duplicate delivery: %v %v", children, err)
			}
			got := children[0].Records[0]
			full, _ := acquisition.SourceText(body)
			if got.Fields["title"] != label || got.Fields["body"] != full || !got.Provenance.FieldSupport["body"].FullSource {
				t.Fatal("field values not taken from original source", got)
			}
			bad := call
			bad.ID = "bad-selector"
			bad.Function.Arguments = strings.ReplaceAll(call.Function.Arguments, "#heading", "#missing")
			if _, err := s.executeDurableTool(context.Background(), r, bad); err == nil || !strings.Contains(err.Error(), "title") {
				t.Fatal("missing node accepted or no field diagnostic", err)
			}
			foreign := r
			foreign.ID = "foreign"
			bad.ID = "foreign"
			if _, err := s.executeDurableTool(context.Background(), foreign, call); err == nil {
				t.Fatal("foreign source admitted")
			}
		})
	}
}

func TestDOMReadWithoutSelectorProvidesObservedChoices(t *testing.T) {
	for _, tag := range []string{"h2", "h3"} {
		s, r, _ := storedHTMLReader(t, `<article><`+tag+` id="actual">Actual title</`+tag+`><p>Original evidence</p></article>`)
		args := `{"handle":"source","view":"dom"}`
		out, err := s.readData(context.Background(), r, ToolCall{ID: "choices", Function: FunctionCall{Name: "read_data", Arguments: args}})
		if err != nil || !strings.Contains(string(out.Data), "Actual title") || !strings.Contains(string(out.Data), `"selector"`) {
			t.Fatalf("must see real choices before selecting: %s %v", out.Data, err)
		}
		out, err = s.readData(context.Background(), r, ToolCall{ID: "miss", Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"source","view":"dom","selector":".guessed"}`}})
		if err == nil || !strings.Contains(err.Error(), "Actual title") {
			t.Fatalf("failure loses actual choices: %s %v", out.Data, err)
		}
	}
}

func TestDeclaredAPIFieldsUseJSONLocatorsWithoutLua(t *testing.T) {
	s, r, _ := storedHTMLReader(t, `{"project":{"name":"Public API record","budget":53},"other":{"budget":199}}`)
	var err error
	r, err = s.Data.Claim(context.Background(), r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	spec := TaskSpec{Goal: r.Goal, ContractVersion: "v2", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "title", Type: "string"}, {Name: "budget", Type: "number"}}, JoinKeys: []string{"title"}, RequireFieldSupport: true}
	raw, _ := json.Marshal(spec)
	r.SpecJSON = string(raw)
	if err := s.Data.DB.Model(&r).Update("spec_json", r.SpecJSON).Error; err != nil {
		t.Fatal(err)
	}
	call := ToolCall{ID: "api-extract", Type: "function", Function: FunctionCall{Name: "submit_extraction", Arguments: `{"inspection_call_id":"source","field_selectors":{"title":{"json_pointer":"/project/name"},"budget":{"json_pointer":"/project/budget"}}}`}}
	if _, err := s.executeDurableTool(context.Background(), r, call); err != nil {
		t.Fatal(err)
	}
	children, err := s.ChildResults(context.Background(), r.ID)
	if err != nil || len(children) != 1 {
		t.Fatal(children, err)
	}
	if children[0].Records[0].Fields["title"] != "Public API record" || !fieldIsSupported(children[0].Records[0].Provenance, "budget") {
		t.Fatal("API fields not delivered with original support")
	}
}

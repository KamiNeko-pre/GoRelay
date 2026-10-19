package aiapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"go-lua-crawler/internal/testdb"
)

func TestCrawlerCatalogExposesStandaloneNavigation(t *testing.T) {
	for _, navigation := range []string{`<a rel="next" href="?page=2">下一页</a>`, `<a href="/archive/older">往期数据</a>`} {
		body := `<main><ul class="entries"><li><a href="/detail/1">条目一</a></li><li><a href="/detail/2">条目二</a></li></ul>` + navigation + `</main>`
		doc, _ := goquery.NewDocumentFromReader(strings.NewReader(body))
		found := false
		for _, entry := range completeSourceSelectorCatalog(body) {
			doc.Find(entry.Selector).Each(func(_ int, n *goquery.Selection) {
				href, _ := n.Attr("href")
				if href == "?page=2" || href == "/archive/older" {
					found = true
				}
			})
		}
		if !found {
			t.Errorf("standalone navigation absent from observed selector inventory: %s", navigation)
		}
	}
}

func TestCrawlerRegistrationKindFailureReachesNextModelWithCorrectSteps(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	in := admissionFixture(t)
	in.Artifact.Kind = "composition"
	if err := s.Data.DB.Create(&in.Artifact).Error; err != nil {
		t.Fatal(err)
	}
	requests := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request ChatRequest
		json.NewDecoder(r.Body).Decode(&request)
		requests++
		call := ToolCall{ID: "wrong-kind-register", Type: "function", Function: FunctionCall{Name: "register_source", Arguments: `{"candidate_id":"candidate","description":"collect Alpha facts"}`}}
		if requests > 1 {
			receipts, _ := auditVisibleReceipts(request)
			failure := receipts["register_source"].Failure
			if failure == nil || failure.Code != "SOURCE_ADMISSION_REJECTED" {
				t.Errorf("kind rejection still unclassified: %+v", failure)
			} else {
				raw, _ := json.Marshal(failure)
				for _, required := range []string{"composition", "adapter", "kind", "mode=acquisition", "validate_program", "register_source", "submit_acquisition"} {
					if !strings.Contains(string(raw), required) {
						t.Errorf("correct recovery missing %s: %s", required, raw)
					}
				}
				if failure.Recovery == nil || failure.Recovery.Tool != "validate_program" || failure.Recovery.RepairOf != "" {
					t.Errorf("cross-tool correction advertises invalid repair_of: %+v", failure.Recovery)
				}
			}
			call = ToolCall{ID: "conclude-kind-test", Type: "function", Function: FunctionCall{Name: "finish_task", Arguments: `{"status":"failed","reason":"controlled feedback test"}`}}
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": Message{Role: "assistant", ToolCalls: []ToolCall{call}}, "finish_reason": "tool_calls"}}})
	}))
	defer model.Close()
	s.Model, _ = NewModelProvider(ProviderConfig{BaseURL: model.URL, APIKey: "test-only", Model: "feedback-test"})
	r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "kind-feedback", Goal: in.Spec.Goal, Spec: in.Spec, SourceScope: in.Scope})
	if err != nil {
		t.Fatal(err)
	}
	in.Candidate.RunID = r.ID
	if err := s.Data.DB.Create(&in.Candidate).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6 && requests < 2; i++ {
		if err := s.Advance(context.Background(), r.ID); err != nil {
			t.Fatal(err)
		}
	}
	if requests != 2 {
		t.Fatal("diagnostic never reached next model")
	}
}

func TestCrawlerPublicationInstructionsAllowCorrectingKindWithSameCode(t *testing.T) {
	s := efficiencyState()
	brief := buildCorrectionBrief(s, "correcting", "publication incomplete")
	raw, _ := json.Marshal(brief)
	if strings.Contains(string(raw), "do not validate identical code again") || !strings.Contains(string(raw), "kind=adapter") {
		t.Fatal("correction forbids the necessary kind repair", string(raw))
	}
	for _, tool := range runToolDefinitions(s.Run) {
		if tool.Function.Name == "validate_program" {
			raw, _ := json.Marshal(tool)
			if !strings.Contains(string(raw), "temporary") || !strings.Contains(string(raw), "reusable") {
				t.Fatal("kind semantics absent", string(raw))
			}
		}
	}
}

func TestCrawlerCoverageAdviceRequiresPublishedUse(t *testing.T) {
	for _, tc := range []struct {
		required bool
		used     int
		want     string
	}{{true, 0, "register_source"}, {true, 1, "finish_task"}, {false, 0, "finish_task"}} {
		result := TaskResult{CapabilityRequired: tc.required, ReusableCapabilities: tc.used, Coverage: CoverageReport{Satisfied: true}}
		advice := resultNextStep(result)
		if !strings.Contains(advice, tc.want) || tc.required && tc.used == 0 && strings.Contains(advice, "status=completed") {
			t.Fatal("coverage advice contradicts required publication", advice)
		}
	}
}

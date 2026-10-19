package aiapp

import (
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDeliveryConclusionBlocksNewIOButCanReadSavedSource(t *testing.T) {
	s, r, _ := storedHTMLReader(t, "<main><h2>saved evidence</h2></main>")
	r, err := s.Data.Claim(context.Background(), r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(HostProgress{Recovery: DeliveryRecovery{Phase: "concluding"}})
	r.HostProgressJSON = string(raw)
	if err = s.Data.DB.Model(&r).Update("host_progress_json", r.HostProgressJSON).Error; err != nil {
		t.Fatal(err)
	}
	search := &countedDiscovery{}
	s.Search = search
	call := ToolCall{ID: "forbidden-search", Type: "function", Function: FunctionCall{Name: "discover_sources", Arguments: `{"query":"more sources"}`}}
	reply, err := s.executeDurableTool(context.Background(), r, call)
	if err == nil || reply.Failure == nil || reply.Failure.Code != "CONCLUSION_ONLY" || search.calls != 0 {
		t.Fatalf("conclusion dispatched new work: %+v %v calls=%d", reply, err, search.calls)
	}
	read := ToolCall{ID: "saved-read", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"source","view":"dom","selector":"h2"}`}}
	reply, err = s.executeDurableTool(context.Background(), r, read)
	if err != nil || !strings.Contains(string(reply.Data), "saved evidence") {
		t.Fatalf("conclusion cannot use retained evidence: %s %v", reply.Data, err)
	}
}

func TestDeliveryTerminalSnapshotVisibleInNormalAPIResult(t *testing.T) {
	for _, planned := range []bool{false, true} {
		for _, stop := range []string{"cancel", "expire"} {
			t.Run(stop+map[bool]string{false: "-unplanned", true: "-planned"}[planned], func(t *testing.T) {
				s, r, _ := storedHTMLReader(t, "<main>saved evidence</main>")
				if planned {
					spec := TaskSpec{Goal: r.Goal, SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "title", Type: "string"}}}
					raw, _ := json.Marshal(spec)
					r.SpecJSON = string(raw)
					if err := s.Data.DB.Model(&r).Update("spec_json", r.SpecJSON).Error; err != nil {
						t.Fatal(err)
					}
				}
				if stop == "cancel" {
					if err := s.CancelWithDelivery(context.Background(), r.ID); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := s.Data.DB.Model(&r).Update("deadline", time.Now().Add(-time.Second)).Error; err != nil {
						t.Fatal(err)
					}
					if err := s.Advance(context.Background(), r.ID); err != nil {
						t.Fatal(err)
					}
				}
				gin.SetMode(gin.TestMode)
				t.Setenv("GORELAY_ADMIN_TOKEN", "delivery-api-test")
				router := gin.New()
				RegisterRoutes(router, s)
				req := httptest.NewRequest("GET", "/api/v1/agent/runs/"+r.ID, nil)
				req.Header.Set("Authorization", "Bearer delivery-api-test")
				out := httptest.NewRecorder()
				router.ServeHTTP(out, req)
				if out.Code != 200 {
					t.Fatalf("read failed: %d %s", out.Code, out.Body.String())
				}
				var view struct {
					Result TaskResult `json:"result"`
				}
				if err := json.Unmarshal(out.Body.Bytes(), &view); err != nil {
					t.Fatal(err)
				}
				if view.Result.Summary == "" || len(view.Result.AcquiredMaterials) != 1 {
					t.Fatalf("public result lost terminal snapshot: %+v", view.Result)
				}
				if view.Result.Answer != nil || len(view.Result.Records) != 0 {
					t.Fatal("unanalysed source promoted to verified result")
				}
			})
		}
	}
}

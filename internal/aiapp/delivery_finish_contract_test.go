package aiapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/repository"
	"go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
)

// Completed child rows stand in for acquisition. Admission, persisted contract,
// evaluation and the final SQL transition use the normal HTTP/Service path.
// This is a host-boundary test, not a real autonomous business experiment.
func TestDeliveryFinishContractHostGateThroughNormalEntry(t *testing.T) {
	ctx := context.Background()
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	t.Setenv("GORELAY_ADMIN_TOKEN", "finish-contract")
	router := gin.New()
	RegisterRoutes(router, s)
	post := func(path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer finish-contract")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	for _, tc := range []struct {
		name, object, proposed, want string
		modes                        []string
		requireSupport               bool
	}{
		{"missing-mode", "Alpha", "completed", "reviewing", []string{"basic"}, false},
		{"unrelated-row", "Alpha", "completed", "reviewing", []string{"basic", "other"}, false},
		{"complete-sibling", "Beta", "completed", "completed", []string{"basic", "extended"}, false},
		{"unsupported-values", "Beta", "completed", "reviewing", []string{"basic", "extended"}, true},
		{"honest-partial", "Alpha", "partial", "partial", []string{"basic"}, false},
		{"empty-partial", "Alpha", "partial", "failed", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := deliveryConditionSpec(t, tc.object)
			spec.RequireFieldSupport = tc.requireSupport
			response := post("/api/v1/agent/runs", CreateRunRequest{RequestKey: tc.name, Goal: spec.Goal, Spec: &spec})
			if response.Code != http.StatusAccepted {
				t.Fatal("normal admission failed", response.Code, response.Body.String())
			}
			var run agentdata.Run
			if err := json.Unmarshal(response.Body.Bytes(), &run); err != nil {
				t.Fatal(err)
			}
			child := task.CrawlTask{RequestKey: "child:" + tc.name, RunID: run.ID, URL: "https://example.com/pricing", Target: "fixture", Status: task.StatusSucceeded}
			if err := s.Data.DB.Create(&child).Error; err != nil {
				t.Fatal(err)
			}
			rows := []Record{}
			for _, mode := range tc.modes {
				object := tc.object
				if mode == "other" {
					object, mode = "Other", "extended"
				}
				row := deliveryRecord(object, mode, time.Now().UTC())
				row.Provenance.TaskID = child.ID
				rows = append(rows, row)
			}
			payload, err := json.Marshal(AcquisitionResult{Records: rows})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Data.DB.Create(&repository.CrawlResult{TaskID: child.ID, Payload: string(payload)}).Error; err != nil {
				t.Fatal(err)
			}
			if err := s.Data.DB.Create(&agentdata.RunTask{RunID: run.ID, TaskID: child.ID, OperationID: child.ID}).Error; err != nil {
				t.Fatal(err)
			}
			// A bad decision must not bypass the executor's independent gate.
			s.Decide = func(context.Context, LoopState) (NextAction, error) {
				return NextAction{Kind: "finish", Status: tc.proposed, Reason: "fixture decision"}, nil
			}
			response = post("/api/v1/agent/runs/"+run.ID+"/advance", map[string]any{})
			stored, err := s.Data.Run(ctx, run.ID)
			if err != nil || stored.Status != tc.want {
				t.Fatalf("host accepted wrong terminal state: status=%s want=%s error=%v response=%s", stored.Status, tc.want, err, response.Body.String())
			}
			result, err := s.Evaluate(ctx, stored)
			if err != nil || result.ScopeConfirmation != "caller_confirmed" {
				t.Fatal("frozen scope acceptance lost", result.ScopeConfirmation, err)
			}
			if tc.want == "reviewing" || tc.want == "failed" {
				if response.Code < 400 || stored.ResultJSON != "" {
					t.Fatal("invalid delivery persisted or rejection was hidden", response.Code, stored.ResultJSON)
				}
			} else if response.Code != http.StatusOK || stored.ResultJSON == "" {
				t.Fatal("valid final delivery rejected", response.Code, response.Body.String())
			}
			if tc.name == "missing-mode" || tc.name == "unrelated-row" {
				if len(result.Coverage.Combinations) != 2 || result.Coverage.Combinations[1].Satisfied {
					t.Fatal("missing frozen combination disappeared", result.Coverage)
				}
			}
			ops, err := s.Data.Operations(ctx, run.ID)
			if err != nil || len(ops) != 0 {
				t.Fatal("host completion check invoked model/external tools", len(ops), err)
			}
		})
	}
}

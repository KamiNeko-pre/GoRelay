package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"gorm.io/gorm"
)

func admissionRequest(key string) CreateRunRequest {
	goal := "compare two public modes"
	return CreateRunRequest{RequestKey: key, Goal: goal, Spec: &TaskSpec{Goal: goal, SchemaVersion: "v1", RequiredFields: []FieldRequirement{{"mode", "string"}, {"value", "integer"}}, JoinKeys: []string{"mode"}}}
}

func TestRunAdmissionIncludesSubmittedContractInInitialInsert(t *testing.T) {
	for _, limit := range []string{"0", "2"} {
		t.Run("quota_"+limit, func(t *testing.T) {
			t.Setenv("GORELAY_DEMO_DAILY_RUNS", limit)
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			missing := errors.New("initial run insert has no submitted contract")
			if err := s.Data.DB.Callback().Create().Before("gorm:create").Register("assert_initial_contract", func(tx *gorm.DB) {
				if row, ok := tx.Statement.Dest.(*agentdata.Run); ok && row.SpecJSON == "" {
					tx.AddError(missing)
				}
			}); err != nil {
				t.Fatal(err)
			}
			r, err := s.Create(context.Background(), admissionRequest("atomic"))
			if err != nil {
				t.Fatalf("contract was not part of initial insert: %v", err)
			}
			var spec TaskSpec
			if err = json.Unmarshal([]byte(r.SpecJSON), &spec); err != nil || spec.Goal != r.Goal {
				t.Fatal("lost submitted contract", err)
			}
		})
	}
}

func TestRunAdmissionFailureLeavesNoSchedulableRunOrQuotaCharge(t *testing.T) {
	t.Setenv("GORELAY_DEMO_DAILY_RUNS", "1")
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("failure during atomic admission")
	if err := s.Data.DB.Callback().Create().After("gorm:create").Register("fail_admission", func(tx *gorm.DB) {
		if row, ok := tx.Statement.Dest.(*agentdata.Run); ok && row.SpecJSON != "" {
			tx.AddError(injected)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(context.Background(), admissionRequest("interrupted")); !errors.Is(err, injected) {
		t.Fatalf("injection did not reach admission: %v", err)
	}
	var count int64
	if err := s.Data.DB.Model(&agentdata.Run{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("partial run escaped: %d %v", count, err)
	}
	var quota agentdata.DailyAdmission
	err := s.Data.DB.First(&quota).Error
	if err == nil && quota.Count != 0 {
		t.Fatal("rolled-back admission consumed quota")
	}
}

func TestRunAdmissionReplayRestoresOnlyUnexecutedSubmittedContract(t *testing.T) {
	for _, executed := range []bool{false, true} {
		t.Run(map[bool]string{false: "not_started", true: "already_started"}[executed], func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			request := admissionRequest("legacy-half")
			request.ActionMode = "code"
			raw, _ := json.Marshal(request)
			legacy := agentdata.Run{ID: agentdata.Token(), RequestKey: request.RequestKey, Goal: request.Goal, InputJSON: string(raw), BudgetJSON: "{}", Status: "planning", Deadline: time.Now().Add(time.Hour)}
			if executed {
				legacy.PlanJSON = `[{"role":"assistant","content":"started"}]`
			}
			if err := s.Data.DB.Create(&legacy).Error; err != nil {
				t.Fatal(err)
			}
			r, err := s.Create(context.Background(), request)
			if executed {
				if err == nil {
					t.Fatal("silently repaired a contract after execution started")
				}
				stored, _ := s.Data.Run(context.Background(), legacy.ID)
				if stored.Status != "blocked_configuration" || stored.SpecJSON != "" {
					t.Fatal("unsafe historical run remained schedulable or was overwritten", stored.Status)
				}
			} else if err != nil || r.SpecJSON == "" {
				t.Fatalf("replay lost original submitted contract: %v", err)
			}
		})
	}
}

func TestRunAdmissionClaimRestoresContractBeforeDecision(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	request := admissionRequest("legacy-claim")
	raw, _ := json.Marshal(request)
	legacy := agentdata.Run{ID: agentdata.Token(), RequestKey: request.RequestKey, Goal: request.Goal, InputJSON: string(raw), BudgetJSON: "{}", Status: "planning", Deadline: time.Now().Add(time.Hour)}
	if err := s.Data.DB.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Data.Claim(context.Background(), legacy.ID, time.Minute)
	if err != nil || claimed.SpecJSON == "" {
		t.Fatalf("claimed half-initialized run: %v", err)
	}
}

func TestRunAdmissionConcurrentSchedulerSeesOnlyCommittedContract(t *testing.T) {
	for _, limit := range []string{"0", "2"} {
		t.Run("quota_"+limit, func(t *testing.T) {
			t.Setenv("GORELAY_DEMO_DAILY_RUNS", limit)
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			inserted, release := make(chan struct{}), make(chan struct{})
			if err := s.Data.DB.Callback().Create().After("gorm:create").Before("gorm:commit_or_rollback_transaction").Register("hold_admission_commit", func(tx *gorm.DB) {
				if _, ok := tx.Statement.Dest.(*agentdata.Run); ok {
					close(inserted)
					select {
					case <-release:
					case <-time.After(10 * time.Second):
						tx.AddError(errors.New("admission test barrier timeout"))
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			type admission struct {
				run agentdata.Run
				err error
			}
			accepted := make(chan admission, 1)
			go func() {
				r, err := s.Create(context.Background(), admissionRequest("concurrent-admission"))
				accepted <- admission{r, err}
			}()
			select {
			case <-inserted:
			case <-time.After(10 * time.Second):
				t.Fatal("admission did not reach insert")
			}
			// Read the admission view without Pending's unrelated expiry UPDATE:
			// that maintenance statement can wait on an in-flight insert gap lock.
			var visible []agentdata.Run
			err := s.Data.DB.Where("status = ?", "planning").Find(&visible).Error
			close(release)
			result := <-accepted
			if err != nil || len(visible) != 0 {
				t.Fatalf("scheduler saw uncommitted admission: %v %v", visible, err)
			}
			if result.err != nil {
				t.Fatal(result.err)
			}
			pending, err := s.Data.Pending(context.Background(), 10)
			if err != nil || len(pending) != 1 || pending[0].SpecJSON == "" {
				t.Fatal("committed scheduler view lost contract", err)
			}
			claimed, err := s.Data.Claim(context.Background(), result.run.ID, time.Minute)
			if err != nil || claimed.SpecJSON == "" || !sameContractJSON([]byte(claimed.SpecJSON), []byte(result.run.SpecJSON)) {
				t.Fatal("scheduler lost contract", err)
			}
			replay, err := s.Create(context.Background(), admissionRequest("concurrent-admission"))
			if err != nil || replay.ID != claimed.ID || !sameContractJSON([]byte(replay.SpecJSON), []byte(claimed.SpecJSON)) {
				t.Fatal("replay changed frozen contract", err)
			}
		})
	}
}

func TestRunAdmissionConflictingHistoricalContractIsPausedNotOverwritten(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	r, err := s.Create(context.Background(), admissionRequest("conflicting-contract"))
	if err != nil {
		t.Fatal(err)
	}
	changed := `{"goal":"different","schema_version":"v1","required_fields":[{"name":"title","type":"string"}]}`
	if err := s.Data.DB.Model(&agentdata.Run{}).Where("id = ?", r.ID).Update("spec_json", changed).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.Data.Claim(context.Background(), r.ID, time.Minute); !errors.Is(err, agentdata.ErrConflict) {
		t.Fatal("conflicting admission was claimed", err)
	}
	stored, err := s.Data.Run(context.Background(), r.ID)
	if err != nil || stored.SpecJSON != changed || stored.Status != "blocked_configuration" || stored.InputJSON != r.InputJSON {
		t.Fatal("history was rewritten", err)
	}
}

func TestRunAdmissionNormalEntryAndNaturalLanguageRemainUsable(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	t.Setenv("GORELAY_ADMIN_TOKEN", "admission-test")
	router := gin.New()
	RegisterRoutes(router, s)
	request := admissionRequest("normal-entry")
	raw, _ := json.Marshal(request)
	for n := 0; n < 2; n++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/runs", strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer admission-test")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Code != http.StatusAccepted {
			t.Fatal(response.Code, response.Body.String())
		}
		var run agentdata.Run
		if err := json.Unmarshal(response.Body.Bytes(), &run); err != nil || run.SpecJSON == "" {
			t.Fatal("normal entry returned incomplete contract", err)
		}
	}
	natural, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "natural", Goal: "discover public information"})
	if err != nil || natural.SpecJSON != "" {
		t.Fatal("natural language contract was invented", err)
	}
	natural, err = s.Data.Claim(context.Background(), natural.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	call := ToolCall{ID: "plan", Type: "function", Function: FunctionCall{Name: "plan_task", Arguments: `{"required_fields":[{"name":"title","type":"string"}]}`}}
	if _, err = s.executeDurableTool(context.Background(), natural, call); err != nil {
		t.Fatal("natural plan was blocked", err)
	}
}

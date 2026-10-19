package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	driver "github.com/go-sql-driver/mysql"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestHardExitHelperProcess(t *testing.T) {
	if os.Getenv("GORELAY_TEST_HARD_EXIT") != "1" {
		t.Skip("subprocess helper")
	}
	db, err := gorm.Open(mysql.Open(os.Getenv("GORELAY_TEST_HARD_EXIT_DSN")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	s := NewService(db)
	s.Model, err = NewModelProvider(ProviderConfig{BaseURL: os.Getenv("GORELAY_TEST_HARD_EXIT_MODEL"), APIKey: "fixture", Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Update().Before("gorm:update").Register("test:hard-exit-after-receipt", func(tx *gorm.DB) {
		fields, ok := tx.Statement.Dest.(map[string]interface{})
		if tx.Statement.Table == "agent_runs" && ok && fields["status"] == "reviewing" {
			raw, _ := json.Marshal(fields)
			_ = os.WriteFile(os.Getenv("GORELAY_TEST_HARD_EXIT_MARKER"), raw, 0600)
			os.Exit(73) // no defer, no process-memory restoration, no graceful shutdown.
		}
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = s.Run(ctx)
	t.Fatal("hard exit did not fire")
}

func TestNormalEntrySurvivesHardExitWithoutRepayingSavedModel(t *testing.T) {
	t.Setenv("GORELAY_AGENT_CONCURRENCY", "1")
	t.Setenv("GORELAY_ADMIN_TOKEN", "fixture")
	db := testdb.Open(t)
	s := NewService(db)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		calls.Add(1)
		name, args := "list_sources", `{}`
		receipts, _ := auditVisibleReceipts(in)
		if receipts["list_sources"].Name != "" {
			name, args = "finish_task", `{"status":"failed","reason":"source catalog is empty; no unsupported facts delivered"}`
		}
		message := Message{Role: "assistant", ToolCalls: []ToolCall{{ID: agentdata.Hash(in)[:24], Type: "function", Function: FunctionCall{Name: name, Arguments: args}}}}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message}}, "usage": Usage{TotalTokens: 10}})
	}))
	defer model.Close()
	s.Model, _ = NewModelProvider(ProviderConfig{BaseURL: model.URL, APIKey: "fixture", Model: "fixture"})
	gin.SetMode(gin.TestMode)
	routes := gin.New()
	RegisterRoutes(routes, s)
	request := func(method, path string, value any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(value)
		req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer fixture")
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		routes.ServeHTTP(out, req)
		return out
	}
	spec := TaskSpec{Goal: "inspect public source catalog and report absence honestly", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "name", Type: "string"}}}
	out := request("POST", "/api/v1/agent/runs", CreateRunRequest{RequestKey: "hard-crash", Goal: spec.Goal, Spec: &spec})
	if out.Code != 202 {
		t.Fatal(out.Code, out.Body.String())
	}
	var run agentdata.Run
	_ = json.Unmarshal(out.Body.Bytes(), &run)
	var schema string
	if err := db.Raw("SELECT DATABASE()").Scan(&schema).Error; err != nil {
		t.Fatal(err)
	}
	config, err := driver.ParseDSN(os.Getenv("MYSQL_INTEGRATION_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	config.DBName = schema
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "hard-exit.marker")
	child := exec.Command(binary, "-test.run=^TestHardExitHelperProcess$")
	child.Env = append(os.Environ(), "GORELAY_TEST_HARD_EXIT=1", "GORELAY_TEST_HARD_EXIT_DSN="+config.FormatDSN(), "GORELAY_TEST_HARD_EXIT_MODEL="+model.URL, "GORELAY_TEST_HARD_EXIT_MARKER="+marker)
	child.Stdout, child.Stderr = &strings.Builder{}, &strings.Builder{}
	err = child.Run()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 73 {
		t.Fatal("unexpected process exit", err)
	}
	if _, err := os.ReadFile(marker); err != nil || calls.Load() != 1 {
		data, _ := os.ReadFile(marker)
		var rows []agentdata.Trace
		_ = db.Where("run_id = ?", run.ID).Find(&rows).Error
		t.Fatal("crash location not established", err, calls.Load(), string(data), rows)
	}
	stored, err := s.Data.Run(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.LeaseUntil == nil {
		t.Fatal("missing frozen lease after crash")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Until(*stored.LeaseUntil)+20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("restored scheduler failed to stop")
		}
	}()
	lastLog := time.Time{}
	for {
		out = request("GET", "/api/v1/agent/runs/"+run.ID, nil)
		var view struct {
			Run        agentdata.Run         `json:"run"`
			Operations []agentdata.Operation `json:"operations"`
		}
		_ = json.Unmarshal(out.Body.Bytes(), &view)
		if view.Run.IsTerminal() {
			if view.Run.Status != "failed" || calls.Load() != 2 {
				t.Fatal("saved response was repaid or false success", view.Run.Status, calls.Load())
			}
			auditSave(t, "hard-exit-recovery", map[string]any{"run_id": run.ID, "exit_code": 73, "normal_entry": true, "lease_expiry_used": true, "model_calls": calls.Load(), "status": view.Run.Status, "operations": view.Operations})
			return
		}
		if time.Since(lastLog) > 25*time.Second {
			t.Logf("waiting for actual crashed lease expiry; status=%s calls=%d remaining=%s", view.Run.Status, calls.Load(), time.Until(*stored.LeaseUntil).Round(time.Second))
			lastLog = time.Now()
		}
		select {
		case <-ctx.Done():
			t.Fatal(fmt.Errorf("hard crash recovery watchdog: %w", ctx.Err()))
		case <-time.After(300 * time.Millisecond):
		}
	}
}

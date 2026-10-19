package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go-lua-crawler/internal/repository"
	taskmodel "go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func TestTaskAPIIdempotency(t *testing.T) {
	db := testdb.Open(t)
	if err := db.AutoMigrate(&taskmodel.CrawlTask{}, &taskmodel.TaskOutbox{}, &taskmodel.TaskEvent{}); err != nil {
		t.Fatalf("migrate crawl tasks: %v", err)
	}

	previousDB := repository.DB
	repository.DB = db
	t.Cleanup(func() { repository.DB = previousDB })
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get SQL connection: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	for _, target := range []string{"integration_test", "integration_other_source"} {
		t.Run(target, func(t *testing.T) {
			testTaskAPIOutbox(t, db, target)
		})
	}
}

func testTaskAPIOutbox(t *testing.T, db *gorm.DB, target string) {
	t.Helper()

	requestKey := fmt.Sprintf("integration-api-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		taskIDs := db.Model(&taskmodel.CrawlTask{}).Select("id").Where("request_key = ?", requestKey)
		if err := db.Where("task_id IN (?)", taskIDs).Delete(&taskmodel.TaskOutbox{}).Error; err != nil {
			t.Errorf("clean up task outbox: %v", err)
		}
		if err := db.Where("request_key = ?", requestKey).Delete(&taskmodel.CrawlTask{}).Error; err != nil {
			t.Errorf("clean up crawl task: %v", err)
		}
	})

	gin.SetMode(gin.TestMode)
	router := SetupRouter()
	body, err := json.Marshal(map[string]string{
		"request_key": requestKey,
		"target":      target,
		"url":         "https://example.com/tasks",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	createRecorder := httptest.NewRecorder()
	createRequest := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader(body))
	createRequest.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(createRecorder, createRequest)
	if createRecorder.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, createRecorder.Code, createRecorder.Body.String())
	}

	var createResponse struct {
		Data taskmodel.CrawlTask `json:"data"`
	}
	if err := json.Unmarshal(createRecorder.Body.Bytes(), &createResponse); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if createResponse.Data.ID == 0 || createResponse.Data.Status != taskmodel.StatusQueued || createResponse.Data.QueuedAt == nil {
		t.Fatalf("unexpected created task: %+v", createResponse.Data)
	}
	var outbox taskmodel.TaskOutbox
	if err := db.Where("task_id = ?", createResponse.Data.ID).First(&outbox).Error; err != nil {
		t.Fatalf("load task outbox: %v", err)
	}
	if outbox.PublishedAt != nil || outbox.MessageID != "" {
		t.Fatalf("new outbox must be unpublished: %+v", outbox)
	}

	getRecorder := httptest.NewRecorder()
	getRequest := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/tasks/%d", createResponse.Data.ID), nil)
	router.ServeHTTP(getRecorder, getRequest)
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, getRecorder.Code, getRecorder.Body.String())
	}

	replayRecorder := httptest.NewRecorder()
	replayRequest := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader(body))
	replayRequest.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(replayRecorder, replayRequest)
	if replayRecorder.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, replayRecorder.Code, replayRecorder.Body.String())
	}

	var replayResponse struct {
		Data             taskmodel.CrawlTask `json:"data"`
		IdempotentReplay bool                `json:"idempotent_replay"`
	}
	if err := json.Unmarshal(replayRecorder.Body.Bytes(), &replayResponse); err != nil {
		t.Fatalf("decode replay response: %v", err)
	}
	if !replayResponse.IdempotentReplay || replayResponse.Data.ID != createResponse.Data.ID {
		t.Fatalf("unexpected idempotent replay: %+v", replayResponse)
	}

	conflictBody, err := json.Marshal(map[string]string{
		"request_key": requestKey,
		"target":      target,
		"url":         "https://example.com/different-task",
	})
	if err != nil {
		t.Fatalf("marshal conflicting request: %v", err)
	}
	conflictRecorder := httptest.NewRecorder()
	conflictRequest := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader(conflictBody))
	conflictRequest.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(conflictRecorder, conflictRequest)
	if conflictRecorder.Code != http.StatusConflict {
		t.Fatalf("expected conflict, got %d: %s", conflictRecorder.Code, conflictRecorder.Body.String())
	}
	var taskCount, outboxCount int64
	if err := db.Model(&taskmodel.CrawlTask{}).Where("request_key = ?", requestKey).Count(&taskCount).Error; err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if err := db.Model(&taskmodel.TaskOutbox{}).Where("task_id = ?", createResponse.Data.ID).Count(&outboxCount).Error; err != nil {
		t.Fatalf("count outboxes: %v", err)
	}
	if taskCount != 1 || outboxCount != 1 {
		t.Fatalf("replay/conflict created duplicates: tasks=%d, outboxes=%d", taskCount, outboxCount)
	}
}

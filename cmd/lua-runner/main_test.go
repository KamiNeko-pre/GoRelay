package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/engine"
)

func TestRunnerHTTPAuthAndHostBudgetCaps(t *testing.T) {
	called := false
	execute := func(ctx context.Context, in aiapp.RunnerRequest) (aiapp.RunnerResponse, error) {
		called = true
		if in.Limits.MaxRequests != 3 || in.Limits.MaxBytes != 1024 || in.Limits.MaxItems != 5 {
			t.Errorf("unbounded limits: %+v", in.Limits)
		}
		return aiapp.RunnerResponse{Result: &engine.ProgramResult{Collection: map[string]any{"items": []any{}}}}, nil
	}
	h := runnerHandler("key", engine.ProgramLimits{MaxRequests: 3, MaxBytes: 1024, MaxItems: 5, MaxOutputBytes: 2048}, execute)
	req := httptest.NewRequest("POST", "/execute", bytes.NewBufferString(`{"program":{"code":"return true,{items={}}"}}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || called {
		t.Fatal("unauthenticated runner executed")
	}
	payload, _ := json.Marshal(aiapp.RunnerRequest{Program: engine.ProgramRequest{Code: "x"}, Limits: engine.ProgramLimits{MaxRequests: 999, MaxBytes: 9999, MaxItems: 999}})
	req = httptest.NewRequest("POST", "/execute", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer key")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !called {
		t.Fatalf("execute failed: %d %s", w.Code, w.Body.String())
	}
}

func TestRunnerBoundedOutputBuffer(t *testing.T) {
	b := boundedBuffer{limit: 4}
	if _, err := b.Write([]byte("12345")); err == nil || b.Len() > 4 {
		t.Fatal("child output buffer is unbounded")
	}
}

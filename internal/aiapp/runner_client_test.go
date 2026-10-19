package aiapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go-lua-crawler/internal/engine"
)

func TestRunnerClientSuccessAndPermissionError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/execute" || r.Header.Get("Authorization") != "Bearer runner-key" {
			t.Error("wrong runner protocol")
		}
		var in RunnerRequest
		json.NewDecoder(r.Body).Decode(&in)
		if in.Program.Code == "deny" {
			w.WriteHeader(422)
			w.Write([]byte(`{"result":{"collection":null,"requests":[],"stats":{"requests":2,"bytes":80},"duration_ms":10},"error":{"code":"permission_denied","message":"outside source scope"}}`))
			return
		}
		w.Write([]byte(`{"result":{"collection":{"items":[{"count":2,"active":true}]},"requests":[],"stats":{"requests":0,"bytes":0},"duration_ms":1}}`))
	}))
	defer s.Close()
	c, err := NewRunnerClient(s.URL, "runner-key")
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Execute(context.Background(), RunnerRequest{Program: engine.ProgramRequest{Code: "ok"}})
	if err != nil || got.Collection["items"].([]any)[0].(map[string]any)["active"] != true {
		t.Fatalf("bad result %+v %v", got, err)
	}
	got, err = c.Execute(context.Background(), RunnerRequest{Program: engine.ProgramRequest{Code: "deny"}})
	if e, ok := err.(*RunnerError); !ok || e.Code != "permission_denied" {
		t.Fatalf("lost classified error: %v", err)
	}
	if got.Stats.Requests != 2 || got.Stats.Bytes != 80 {
		t.Fatalf("failure usage was discarded: %+v", got)
	}
}

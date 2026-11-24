package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCollectionResultSurvivesLuaBoundary(t *testing.T) {
	for _, script := range []string{
		`return true, {items={{title="story",points=12,extra={comments=3}}},meta={source="news"}}`,
		`return true, {items={{name="repo",stars=20,active=false}},meta={source="code"}}`,
		`return true, {items={},meta={source="empty"}}`,
	} {
		result, err := RunProgram(context.Background(), ProgramRequest{Code: script, TargetURL: "https://example.com"}, ProgramLimits{Timeout: time.Second}, nil)
		if err != nil {
			t.Fatalf("collection decode: %v", err)
		}
		data, err := json.Marshal(result.Collection)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatal(err)
		}
		if _, ok := payload["items"].([]any); !ok {
			t.Fatalf("items lost at Lua/JSON boundary: %s", data)
		}
	}
}

func TestCollectionRejectsInvalidJSONStructures(t *testing.T) {
	for _, script := range []string{
		`local a={}; a.self=a; return true,{items={a}}`,
		`return true,{items={[1]={id=1},[3]={id=3}}}`,
		`return true,{items={{call=function() end}}}`,
	} {
		_, err := RunProgram(context.Background(), ProgramRequest{Code: script, TargetURL: "https://example.com"}, ProgramLimits{Timeout: time.Second}, nil)
		if err == nil {
			t.Fatal("invalid table accepted")
		}
	}
	result, err := RunProgram(context.Background(), ProgramRequest{
		Code:      `return true,{items={{enabled=false,value=0}},meta={source="x"}}`,
		TargetURL: "https://example.com",
	}, ProgramLimits{Timeout: time.Second}, nil)
	data, _ := json.Marshal(result.Collection)
	if err != nil || !strings.Contains(string(data), `"enabled":false`) || !strings.Contains(string(data), `"value":0`) {
		t.Fatalf("scalar loss: %s %v", data, err)
	}
}

func TestCollectionRejectsLegacyFlatContract(t *testing.T) {
	_, err := RunProgram(context.Background(), ProgramRequest{
		Code:      `return true,{url=TARGET_URL,stars="1.2k",description="old rule"}`,
		TargetURL: "https://example.com",
	}, ProgramLimits{Timeout: time.Second}, nil)
	if err == nil || !strings.Contains(err.Error(), "collection items must be an array") {
		t.Fatalf("error = %v, want collection contract error", err)
	}
}

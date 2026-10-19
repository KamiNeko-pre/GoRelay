package engine

import (
	"encoding/json"
	"os"
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
		result, err := executeLuaScript(script, "https://example.com", time.Second)
		if err != nil {
			t.Fatalf("collection decode: %v", err)
		}
		data, err := json.Marshal(result)
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

func TestHackerNewsRuleCommentWhitespace(t *testing.T) {
	rule, err := os.ReadFile("../../scripts/hackernews_collection.lua")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		text string
		want float64
	}{{"140&nbsp;comments", 140}, {"21 comments", 21}, {"discuss", 0}} {
		html := `<table><tr class="athing" id="1"><td><span class="titleline"><a href="https://example.com/post">Story</a></span></td></tr><tr><td class="subtext"><span class="subline"><span id="score_1">20 points</span> by <a class="hnuser">person</a><span class="age"><a href="item?id=1">4 hours ago</a></span> | <a href="item?id=1">` + tc.text + `</a></span></td></tr></table>`
		script := "local function http_get(_) return [[" + html + "]] end\n" + string(rule)
		result, err := executeLuaScript(script, "https://news.ycombinator.com/", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		item := result.Collection["items"].([]any)[0].(map[string]any)
		if item["comments"] != tc.want {
			t.Fatalf("%s: comments=%v want=%v", tc.text, item["comments"], tc.want)
		}
	}
}

func TestCollectionRejectsInvalidJSONStructures(t *testing.T) {
	for _, script := range []string{`local a={}; a.self=a; return true,{items={a}}`, `return true,{items={[1]={id=1},[3]={id=3}}}`, `return true,{items={{call=function() end}}}`} {
		_, err := executeLuaScript(script, "https://example.com", time.Second)
		if err == nil {
			t.Fatal("invalid table accepted")
		}
	}
	result, err := executeLuaScript(`return true,{items={{enabled=false,value=0}},meta={source="x"}}`, "https://example.com", time.Second)
	data, _ := json.Marshal(result)
	if err != nil || !strings.Contains(string(data), `"enabled":false`) || !strings.Contains(string(data), `"value":0`) {
		t.Fatalf("scalar loss: %s %v", data, err)
	}
}

func TestCollectionRejectsLegacyGitHubSpecificContract(t *testing.T) {
	_, err := executeLuaScript(`return true,{url=TARGET_URL,stars="1.2k",description="old rule"}`, "https://example.com", time.Second)
	if err == nil || !strings.Contains(err.Error(), "collection items must be an array") {
		t.Fatalf("error = %v, want collection contract error", err)
	}
}

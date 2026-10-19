package aiapp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
)

func TestCrawlerJSONNavigationIsIntactAtModelHTTP(t *testing.T) {
	for _, next := range []string{
		"/collections/environment?cursor=60fec687-east-next",
		"/exports/archive?token=" + strings.Repeat("opaque-", 12) + "&lang=zh",
		"/数据/归档?cursor=" + url.QueryEscape(strings.Repeat("非时间主键", 2)) + "&page=2",
	} {
		t.Run(next, func(t *testing.T) {
			body := string(mustJSON(map[string]any{"links": map[string]any{"next": next}, "identifier": strings.Repeat("id-", 45), "description": strings.Repeat("PRIVATE ARTICLE TEXT ", 100)}))
			nextURL, _ := url.Parse(next)
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == nextURL.Path && r.URL.RawQuery == nextURL.RawQuery {
					io.WriteString(w, `{"items":[{"id":"observed"}]}`)
				} else if r.URL.RequestURI() == "/" {
					io.WriteString(w, body)
				} else {
					http.NotFound(w, r)
				}
			}))
			defer source.Close()
			fetched, err := http.Get(source.URL)
			if err != nil {
				t.Fatal(err)
			}
			rawBody, _ := io.ReadAll(fetched.Body)
			fetched.Body.Close()
			inspection := inspectResponse(acquisition.Response{Body: string(rawBody), Receipt: acquisition.Receipt{URL: source.URL, Status: fetched.StatusCode}}, acquisition.Stats{Requests: 1})
			call := ToolCall{ID: "source", Type: "function", Function: FunctionCall{Name: "inspect_source", Arguments: string(mustJSON(map[string]any{"url": source.URL}))}}
			stored, _ := toolData(call, inspection)
			visible := modelObservationForRun(agentdata.Run{BudgetJSON: `{"loop_version":5}`}, stored, nil)
			var captured ChatRequest
			modelHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
					t.Error(err)
				}
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": Message{Role: "assistant"}, "finish_reason": "stop"}}})
			}))
			defer modelHTTP.Close()
			provider, _ := NewModelProvider(ProviderConfig{BaseURL: modelHTTP.URL, Model: "transport-only", APIKey: "test-only"})
			if _, err := provider.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "assistant", ToolCalls: []ToolCall{call}}, {Role: "tool", ToolCallID: call.ID, Content: string(mustJSON(visible))}}}); err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Data struct {
					Structure JSONInspection   `json:"json_structure"`
					Nodes     []map[string]any `json:"nodes"`
					ReadWith  dataReadArgs     `json:"read_with"`
					Version   string           `json:"view_version"`
				} `json:"data"`
			}
			content := captured.Messages[1].Content
			if err := json.Unmarshal([]byte(content), &decoded); err != nil {
				t.Fatal(err)
			}
			observed := ""
			for _, field := range decoded.Data.Structure.Fields {
				if field.Name == "identifier" && field.Sample != strings.Repeat("id-", 45) {
					t.Error("non-URL short scalar was truncated", field.Sample)
				}
				if field.Name == "links" {
					for _, child := range field.Children {
						if child.Name == "next" {
							observed, _ = child.Sample.(string)
						}
					}
				}
			}
			if observed != next {
				t.Errorf("model was given a fabricated navigation prefix: %q, want %q", observed, next)
			}
			if len(decoded.Data.Nodes) != 3 || decoded.Data.ReadWith.View != "json" || decoded.Data.Version != "json-structure-v1" {
				t.Error("JSON inspection did not publish its structural node reader", decoded.Data)
			}
			if strings.Contains(content, "PRIVATE ARTICLE TEXT") || !strings.Contains(string(stored.Data), "PRIVATE ARTICLE TEXT") {
				t.Error("source prose boundary or canonical receipt changed")
			}
			// Follow exactly the model-visible value through HTTP. Prefixes fail here.
			followed, err := http.Get(source.URL + observed)
			if err != nil {
				t.Fatal(err)
			}
			followed.Body.Close()
			if followed.StatusCode != 200 {
				t.Error("model-visible navigation produced HTTP", followed.StatusCode)
			}
		})
	}
}

func TestJSONNavigationProjectionKeepsLegacyAndLargeContainerBoundaries(t *testing.T) {
	value := strings.Repeat("value", 30)
	legacy := inspectJSON(string(mustJSON(map[string]any{"nested": map[string]any{"value": value}})))
	if legacy.Fields[0].Children[0].Sample != string([]rune(value)[:40]) {
		t.Fatal("non-crawler projection changed")
	}
	items := make([]any, 70)
	for i := range items {
		items[i] = map[string]any{"id": i}
	}
	inspection := inspectResponse(acquisition.Response{Body: string(mustJSON(items)), Receipt: acquisition.Receipt{URL: "https://source.example/", Status: 200}}, acquisition.Stats{})
	stored, _ := toolData(ToolCall{ID: "array", Function: FunctionCall{Name: "inspect_source"}}, inspection)
	visible := modelObservationForRun(agentdata.Run{BudgetJSON: `{"loop_version":5}`}, stored, nil)
	var data struct {
		Nodes []any         `json:"nodes"`
		Next  *dataReadArgs `json:"next_read"`
	}
	json.Unmarshal(visible.Data, &data)
	if len(data.Nodes) != 64 || data.Next == nil || data.Next.Offset != 64 || data.Next.View != "json" {
		t.Fatal("initial JSON array projection lost its structural continuation", string(visible.Data))
	}
}

package information

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"errors"
	"go-lua-crawler/internal/acquisition"

	"testing"
)

func TestPublishedAssetTargetUsesCurrentRequest(t *testing.T) {
	for _, entry := range []string{"https://source.example/entry?edition=B", "https://other.example/new-layout?region=west"} {
		in := Input{EntryURLs: []string{entry}, SourceScope: []string{"source.example", "other.example"}}
		got, err := publishedAssetTarget(in, Asset{EntryURL: "https://source.example/old?edition=A"}, assetInvocation{Params: map[string]any{"edition": "B"}})
		if err != nil || got != entry {
			t.Fatalf("current request lost: %q %v", got, err)
		}
	}
	in := Input{EntryURLs: []string{"https://source.example/current"}, SourceScope: []string{"source.example"}}
	got, err := publishedAssetTarget(in, Asset{}, assetInvocation{URL: "https://source.example/explicit"})
	if err != nil || got != "https://source.example/explicit" {
		t.Fatalf("explicit target changed: %q %v", got, err)
	}
	got, err = publishedAssetTarget(Input{SourceScope: in.SourceScope}, Asset{EntryURL: "https://source.example/catalog"}, assetInvocation{})
	if err != nil || got != "https://source.example/catalog" {
		t.Fatalf("catalog fallback changed: %q %v", got, err)
	}
	for _, bad := range []struct {
		in       Input
		selected assetInvocation
	}{
		{Input{SourceScope: in.SourceScope}, assetInvocation{}},
		{Input{SourceScope: in.SourceScope, EntryURLs: []string{"https://source.example/a", "https://source.example/b"}}, assetInvocation{}},
		{in, assetInvocation{URL: "https://outside.example/wrong"}},
	} {
		_, err := publishedAssetTarget(bad.in, Asset{}, bad.selected)
		var f *ActionError
		if !errors.As(err, &f) || f.Code != "ROUTING_ARGUMENTS_INVALID" || len(f.Steps) < 2 || f.Details["field"] != "url" {
			t.Fatalf("bad target lost actionable error: %v", err)
		}
	}
	for _, kind := range []string{"api", "browser"} {
		for _, mode := range []string{"default", "url", "query", "url-and-query"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				originalURL := "https://source.example/old?edition=A"
				manifest := map[string]any{"url": originalURL, "fields": map[string]any{"name": "/name"}}
				if kind == "browser" {
					manifest["rows_selector"] = ".item"
					manifest["fields"] = map[string]any{"name": map[string]any{"selector": ".name"}}
				}
				asset := Asset{ID: "published-asset", Kind: kind, Version: "immutable-version", EntryURL: originalURL, ManifestJSON: string(mustMarshal(manifest))}
				before := asset
				selected := assetInvocation{}
				if strings.Contains(mode, "url") {
					selected.URL = "https://source.example/current?edition=B"
				}
				if strings.Contains(mode, "query") {
					raw := string(mustMarshal(map[string]any{"asset_id": asset.ID, "params": map[string]any{"release_id": json.Number("9007199254740993"), "opaque_id": json.Number(strings.Repeat("9", 400)), "numeric_token": json.Number("1e309"), "region": []any{"eu", "us"}}}))
					if err := validateInformationToolArgs("acquire_sources", raw); err != nil {
						t.Errorf("advertised native query parameters were rejected before execution: %v", err)
					}
					if err := decodeInformationArgs(raw, &selected); err != nil {
						t.Fatal(err)
					}
					if strings.Contains(mode, "url") {
						selected.URL = "https://source.example/current?edition=B"
					}
				}
				effective, err := nativeAssetForExecution(Input{SourceScope: []string{"source.example"}}, asset, selected)
				if err != nil {
					t.Fatal(err)
				}
				var actual map[string]json.RawMessage
				if err := json.Unmarshal([]byte(effective.ManifestJSON), &actual); err != nil {
					t.Fatal(err)
				}
				var target string
				json.Unmarshal(actual["url"], &target)
				parsed, _ := url.Parse(target)
				if strings.Contains(mode, "url") && parsed.Path != "/current" {
					t.Fatalf("native invocation ignored requested URL: %s", target)
				}
				if strings.Contains(mode, "query") && (parsed.Query().Get("release_id") != "9007199254740993" || parsed.Query().Get("opaque_id") != strings.Repeat("9", 400) || parsed.Query().Get("numeric_token") != "1e309" || len(parsed.Query()["region"]) != 2) {
					t.Fatalf("native invocation ignored or altered query parameters: %s", target)
				}
				if mode == "default" && effective.ManifestJSON != before.ManifestJSON {
					t.Fatal("default call changed durable operation identity")
				}
				if effective.ID != asset.ID || effective.Version != asset.Version || asset != before || effective.EntryURL != target {
					t.Fatal("invocation mutated the published asset or lost its actual target")
				}
				if kind == "api" {
					var plan APIPlan
					json.Unmarshal([]byte(effective.ManifestJSON), &plan)
					runtime, err := executeAPI(context.Background(), plan, func(_ context.Context, method, requested string) (acquisition.Response, error) {
						if requested != target {
							t.Errorf("executor received %s, expected %s", requested, target)
						}
						return acquisition.Response{Body: "{\"name\":\"actual source value\"}", Receipt: acquisition.Receipt{URL: requested, Method: method, Status: 200, FetchedAt: time.Now()}}, nil
					})
					if err != nil || len(runtime.Requests) != 1 || runtime.Requests[0].URL != target {
						t.Fatalf("native execution did not use its bound target: %v", err)
					}
				}
			})
		}
	}
	_, err = nativeAssetForExecution(Input{SourceScope: []string{"source.example"}}, Asset{Kind: "api", EntryURL: "https://source.example/old", ManifestJSON: string(mustMarshal(APIPlan{URL: "https://source.example/old"}))}, assetInvocation{URL: "https://outside.example/private"})
	if err == nil {
		t.Fatal("caller source scope was bypassed")
	}
	for _, raw := range []string{`{"asset_id":0}`, `{"asset_id":1.5}`, `{"asset_id":false}`, `{"asset_id":"asset"} {}`} {
		if err := validateInformationToolArgs("acquire_sources", raw); err == nil {
			t.Errorf("invalid execution arguments accepted: %s", raw)
		}
	}
	for _, value := range []json.Number{"1", "1.0", "1e0"} {
		if err := checkInformationArgumentShape(json.RawMessage(`{"type":"integer","minimum":1,"maximum":1,"enum":[1]}`), value, "number"); err != nil {
			t.Errorf("equivalent exact number %s rejected: %v", value, err)
		}
	}
}
func TestExecuteAssetExposesIndependentTargetURL(t *testing.T) {
	for _, definition := range informationToolDefinitions() {
		if definition.Function.Name != "acquire_sources" {
			continue
		}
		var shape struct{ Properties map[string]json.RawMessage }
		json.Unmarshal(definition.Function.Parameters, &shape)
		if shape.Properties["url"] == nil || shape.Properties["asset_id"] == nil || shape.Properties["version"] != nil {
			t.Fatal("execution target schema contradicts host")
		}
		return
	}
	t.Fatal("acquire_sources missing")
}

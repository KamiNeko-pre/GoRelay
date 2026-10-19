package information

import (
	"context"
	"encoding/json"
	"errors"
	"go-lua-crawler/internal/aiapp"
	"testing"
)

func TestPublishedAssetTargetUsesCurrentRequest(t *testing.T) {
	for _, entry := range []string{"https://source.example/entry?edition=B", "https://other.example/new-layout?region=west"} {
		in := Input{EntryURLs: []string{entry}, SourceScope: []string{"source.example", "other.example"}}
		got, err := publishedAssetTarget(in, Asset{EntryURL: "https://source.example/old?edition=A"}, routeDecision{Params: map[string]any{"edition": "B"}})
		if err != nil || got != entry {
			t.Fatalf("current request lost: %q %v", got, err)
		}
	}
	in := Input{EntryURLs: []string{"https://source.example/current"}, SourceScope: []string{"source.example"}}
	got, err := publishedAssetTarget(in, Asset{}, routeDecision{URL: "https://source.example/explicit"})
	if err != nil || got != "https://source.example/explicit" {
		t.Fatalf("explicit target changed: %q %v", got, err)
	}
	got, err = publishedAssetTarget(Input{SourceScope: in.SourceScope}, Asset{EntryURL: "https://source.example/catalog"}, routeDecision{})
	if err != nil || got != "https://source.example/catalog" {
		t.Fatalf("catalog fallback changed: %q %v", got, err)
	}
	for _, bad := range []struct {
		in       Input
		selected routeDecision
	}{
		{Input{SourceScope: in.SourceScope}, routeDecision{}},
		{Input{SourceScope: in.SourceScope, EntryURLs: []string{"https://source.example/a", "https://source.example/b"}}, routeDecision{}},
		{in, routeDecision{URL: "https://outside.example/wrong"}},
	} {
		_, err := publishedAssetTarget(bad.in, Asset{}, bad.selected)
		var f *ActionError
		if !errors.As(err, &f) || f.Code != "ROUTING_ARGUMENTS_INVALID" || len(f.Steps) < 2 || f.Details["field"] != "url" {
			t.Fatalf("bad target lost actionable error: %v", err)
		}
	}
}
func TestChooseAssetExposesIndependentTargetURL(t *testing.T) {
	s, _ := retrievalService(t)
	ctx := context.Background()
	spec := aiapp.TaskSpec{Goal: "reuse observed capability", SchemaVersion: "v1", RequiredFields: []aiapp.FieldRequirement{{Name: "id", Type: "string"}}}
	r, err := s.Create(ctx, Input{RequestKey: "target-schema", Goal: spec.Goal, Spec: &spec, SourceScope: []string{"source.example"}})
	if err != nil {
		t.Fatal(err)
	}
	m := &routingReply{args: map[string]any{"action": "execute_asset", "asset_id": "catalog-id", "params": map[string]any{"edition": "B"}, "url": "https://source.example/current", "reason": "use current source"}}
	s.Model = m
	out, err := s.decide(ctx, &r, "choose_asset", []string{"execute_asset", "no_matching_asset"}, nil)
	if err != nil || out.URL != "https://source.example/current" {
		t.Fatalf("valid target rejected: %+v %v", out, err)
	}
	var schema struct{ Properties map[string]json.RawMessage }
	json.Unmarshal(m.last.Tools[0].Function.Parameters, &schema)
	if schema.Properties["url"] == nil || schema.Properties["version"] != nil {
		t.Fatal("execution target schema contradicts host")
	}
	if len(out.Params) != 1 || out.Params["edition"] != "B" {
		t.Fatal("target contaminated tool params")
	}
}

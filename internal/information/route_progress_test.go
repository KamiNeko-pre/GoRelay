package information

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/aiapp"
	"os"
	"strings"
	"testing"
)

func TestRouteSemanticChecksPersistRealMeasuredReplay(t *testing.T) {
	base, replay := os.Getenv("GORELAY_TEST_EMBED_BASE_URL"), os.Getenv("GORELAY_ROUTE_SEMANTIC_REPLAY")
	if base == "" || replay == "" {
		t.Skip("real embedding endpoint and saved reasoning required")
	}
	data, err := os.ReadFile(replay)
	if err != nil {
		t.Fatal(err)
	}
	var message aiapp.Message
	if err := json.Unmarshal(data, &message); err != nil {
		t.Fatal(err)
	}
	s, _ := retrievalService(t)
	provider, err := aiapp.NewModelProvider(aiapp.ProviderConfig{BaseURL: base, Model: "bge-small-zh-v1.5-f16", APIKey: "local-bge"})
	if err != nil {
		t.Fatal(err)
	}
	s.Crawler.LoopEmbedding = aiapp.EmbeddingAdapter{Provider: provider}
	ctx := context.Background()
	r, err := s.Create(ctx, Input{RequestKey: "semantic-replay", Goal: "inspect current source strategy"})
	if err != nil {
		t.Fatal(err)
	}
	schema := map[string]any{"properties": map[string]any{"action": true, "url": true}}
	for i, thought := range []string{message.ReasoningContent, strings.Repeat("检查来源字段与当前任务的对应关系，再比较已观察到的接口能力。", 120), "Inspect the missing entity in a different verified API."} {
		message.ReasoningContent = thought
		before := r
		if err := s.trackRouteProposal(ctx, &r, "known_api", schema, message); err != nil {
			t.Fatal(err)
		}
		var progress map[string]any
		json.Unmarshal([]byte(r.RouteProgressJSON), &progress)
		if progress["semantic_status"] != "checked" {
			t.Fatalf("semantic check declined: %s", r.RouteProgressJSON)
		}
		if err := s.trackRouteProposal(ctx, &before, "known_api", schema, message); err != nil {
			t.Fatal(err)
		}
		var count int64
		if err := s.DB.Model(&Step{}).Where("request_id = ? AND kind = ?", r.ID, "route_semantic").Count(&count).Error; err != nil || count != int64(i+1) {
			t.Fatalf("semantic replay duplicated/lost measurement: count=%d err=%v", count, err)
		}
	}
	ledger, err := s.Accounting(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	var encoded map[string]json.RawMessage
	json.Unmarshal(mustMarshal(ledger), &encoded)
	var checks []struct {
		Result struct {
			Status string
			Usage  *struct {
				TotalTokens int64 `json:"total_tokens"`
			}
		}
	}
	if err := json.Unmarshal(encoded["semantic_checks"], &checks); err != nil || len(checks) != 3 {
		t.Fatalf("upper semantic accounting missing: %d %v", len(checks), err)
	}
	for _, check := range checks {
		if check.Result.Status != "checked" || check.Result.Usage == nil || check.Result.Usage.TotalTokens == 0 {
			t.Fatalf("real semantic token measurement absent: %+v", check)
		}
	}
}

func TestRoutingRecoveryStopsOnlyUnchangedBehaviorAfterVisibleCorrection(t *testing.T) {
	state := routeProgress{}
	state, stop := advanceRouteProgress(state, "same-plan", "same-evidence", false, false)
	if stop {
		t.Fatal("initial attempt stopped")
	}
	state, stop = advanceRouteProgress(state, "same-plan", "same-evidence", true, true)
	if stop || state.Warning == "" {
		t.Fatal("first repetition must receive correction, not terminate")
	}
	changed, stop := advanceRouteProgress(state, "corrected-plan", "same-evidence", true, true)
	if stop || changed.RepeatedBehavior != 0 {
		t.Fatal("a real parameter correction was preempted")
	}
	newEvidence, stop := advanceRouteProgress(state, "same-plan", "new-evidence", true, true)
	if stop || newEvidence.NoNewEvidence != 0 {
		t.Fatal("new source evidence was discarded")
	}
	_, stop = advanceRouteProgress(state, "same-plan", "same-evidence", true, true)
	if !stop {
		t.Fatal("unchanged behavior continued despite visible correction and two no-evidence rounds")
	}
}

func TestRoutingAliasChangesCannotHideTheSameUnresolvedAsset(t *testing.T) {
	r := Request{ContextJSON: string(mustMarshal(routeContext{Lookup: &PreflightResult{Assets: []Asset{{ID: "known", SourceID: "source", Version: "v2"}}}}))}
	schema := map[string]any{"properties": map[string]any{"action": true, "asset_id": true, "reason": true}}
	message := func(id, reason string) aiapp.Message {
		return aiapp.Message{ToolCalls: []aiapp.ToolCall{{ID: "irrelevant-call-id", Function: aiapp.FunctionCall{Name: "choose_asset", Arguments: string(mustMarshal(map[string]any{"action": "execute_asset", "asset_id": id, "reason": reason}))}}}}
	}
	first := routeBehavior(r, "choose_asset", message("invented-a", "first wording"), schema)
	alias := routeBehavior(r, "choose_asset", message("invented-b", "new wording"), schema)
	valid := routeBehavior(r, "choose_asset", message("known", "correct reference"), schema)
	if first != alias {
		t.Fatal("unresolved alias/description change bypassed behavior detection")
	}
	if first == valid {
		t.Fatal("a corrected valid reference was blocked as identical")
	}
}

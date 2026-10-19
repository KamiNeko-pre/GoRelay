package information

import (
	"context"
	"testing"

	"go-lua-crawler/internal/agentstatus"
	"go-lua-crawler/internal/aiapp"
)

func TestBrowserPermissionAndFailedPlanBudgetAreHostOwned(t *testing.T) {
	s, _ := retrievalService(t)
	ctx := context.Background()
	spec := aiapp.TaskSpec{Goal: "collect fields", SchemaVersion: "v1", RequiredFields: []aiapp.FieldRequirement{{Name: "name", Type: "string"}}}
	r, err := s.Create(ctx, Input{RequestKey: "browser-grant-boundary", Goal: spec.Goal, Spec: &spec, SourceScope: []string{"source.example"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.reserveBrowserStep(ctx, r, "trial_browser", "https://source.example/items", map[string]any{"selector": "one"}); err == nil {
		t.Fatal("model can launch browser without service permission")
	}
	// This is a host permission/admission component fixture, not a model replay.
	grant := BrowserGrant{RequestID: r.ID, Code: agentstatus.AntiBotBrowserApproved, TargetURL: "https://source.example/items", PolicyJSON: string(mustMarshal(BrowserPolicy{MaxFailedPlans: 2})), Reason: "approved after normal Lua correction"}
	if err := s.DB.Create(&grant).Error; err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{"first", "corrected"} {
		step, fresh, err := s.reserveBrowserStep(ctx, r, "trial_browser", grant.TargetURL, map[string]any{"selector": selector})
		if err != nil || !fresh {
			t.Fatal(step, fresh, err)
		}
		if err := s.finishStep(ctx, step, nil, agentstatus.Wrap(agentstatus.AgentCorrection, context.Canceled)); err != nil {
			t.Fatal(err)
		}
	}
	if _, fresh, err := s.reserveBrowserStep(ctx, r, "trial_browser", grant.TargetURL, map[string]any{"selector": "first"}); err != nil || fresh {
		t.Fatal("same parameters must use original admission before budget checks", fresh, err)
	}
	if _, _, err := s.reserveBrowserStep(ctx, r, "trial_browser", grant.TargetURL, map[string]any{"selector": "third"}); err == nil || aiapp.OperationalStatus(err).Code != agentstatus.BrowserAttemptsExhausted {
		t.Fatal("failed browser range not enforced", err)
	}
	if _, _, err := s.reserveBrowserStep(ctx, r, "trial_browser", "https://another.example/items", map[string]any{"selector": "fourth"}); err == nil {
		t.Fatal("browser grant escaped its observed source")
	}
}

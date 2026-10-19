package information

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/repository"
	"go-lua-crawler/internal/task"
)

// This injects a state change while a real PublicFetcher request is in flight.
// No model decisions or tool results are scripted by this probe.
func TestNativeCommitRejectsLostOwnershipAndRestoresCommittedReceipt(t *testing.T) {
	for _, cut := range []string{"cancelled", "expired", "taken-over", "lease-expired", "context-owner", "valid", "lost-response"} {
		t.Run(cut, func(t *testing.T) {
			s, _ := retrievalService(t)
			ctx := context.Background()
			spec := aiapp.TaskSpec{Goal: "collect observed version", ContractVersion: "v2", SchemaVersion: "v1", RequiredFields: []aiapp.FieldRequirement{{Name: "name", Type: "string"}, {Name: "version", Type: "string"}}, JoinKeys: []string{"name"}, MinRecords: 1}
			r, err := s.Create(ctx, Input{RequestKey: cut, Goal: spec.Goal, Spec: &spec, SourceScope: []string{"8.8.8.8"}})
			if err != nil {
				t.Fatal(err)
			}
			armed := false
			var gets atomic.Int32
			publicSource(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				gets.Add(1)
				if armed {
					updates := map[string]any{}
					switch cut {
					case "cancelled":
						updates["stage"] = "cancelled"
					case "expired":
						updates["deadline"] = time.Now().Add(-time.Minute)
					case "taken-over":
						updates["lease_token"] = "next-owner"
					case "lease-expired":
						updates["lease_until"] = time.Now().Add(-time.Minute)
					}
					if len(updates) > 0 {
						if err := s.DB.Model(&Request{}).Where("id = ?", r.ID).Updates(updates).Error; err != nil {
							t.Error(err)
						}
					}
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"name": "observed", "version": "2.0"})
			}))
			if _, err := s.Preflight(ctx, r.ID); err != nil {
				t.Fatal(err)
			}
			plan := APIPlan{URL: "http://8.8.8.8/version", Fields: map[string]string{"name": "/name", "version": "/version"}}
			trial, err := s.TrialAPI(ctx, r.ID, "version metadata", plan)
			if err != nil {
				t.Fatal(err)
			}
			a, err := s.Publish(ctx, r.ID, trial.CandidateID)
			if err != nil {
				t.Fatal(err)
			}
			until := time.Now().Add(time.Minute)
			if err := s.DB.Model(&Request{}).Where("id = ?", r.ID).Updates(map[string]any{"stage": "execute_asset", "lease_token": "original-owner", "lease_until": until}).Error; err != nil {
				t.Fatal(err)
			}
			armed = true
			if cut == "context-owner" {
				ctx = context.WithValue(ctx, requestExecutionOwnerKey{}, requestExecutionOwner{r.ID, "original-owner"})
				if err := s.DB.Model(&Request{}).Where("id = ?", r.ID).Update("lease_token", "next-owner").Error; err != nil {
					t.Fatal(err)
				}
			}
			if cut == "lost-response" {
				r, err = s.Request(ctx, r.ID)
				if err != nil {
					t.Fatal(err)
				}
				step, fresh, err := s.reserveStep(ctx, r, "execute_asset", map[string]any{"asset": a.ID, "version": a.Version, "manifest": a.ManifestJSON, "spec": spec})
				if err != nil || !fresh {
					t.Fatal("reserve", err)
				}
				fetcher, err := s.NewFetcher([]string{"8.8.8.8"})
				if err != nil {
					t.Fatal(err)
				}
				defer fetcher.Close()
				runtime, err := executeAPI(ctx, plan, fetcher.Fetch)
				if err != nil {
					t.Fatal(err)
				}
				first, err := s.commitNative(ctx, r, step, a, spec, runtime)
				if err != nil {
					t.Fatal(err)
				}
				// The process dies after facts commit and before finishStep. Recovery
				// must read those same facts, without a second external GET/commit.
				again, err := s.ExecuteAPI(ctx, r.ID, a.ID)
				if err != nil || len(again) != len(first) || again[0].Provenance.TaskID != first[0].Provenance.TaskID {
					t.Fatalf("committed response was not recoverable: %v", err)
				}
				var duplicate *ActionError
				if _, err := s.ExecuteAPI(ctx, r.ID, a.ID); !errors.As(err, &duplicate) || duplicate.Code != "ACTION_ALREADY_CALLED" || gets.Load() != 2 {
					t.Fatalf("saved recovery reopened business execution: gets=%d err=%v", gets.Load(), err)
				}
			} else {
				rows, err := s.ExecuteAPI(ctx, r.ID, a.ID)
				if cut == "valid" {
					if err != nil || len(rows) != 1 {
						t.Fatalf("valid owner rejected: %v", err)
					}
				} else if !errors.Is(err, agentdata.ErrConflict) {
					t.Fatalf("late native commit accepted after %s: rows=%d error=%v", cut, len(rows), err)
				}
			}
			want := int64(0)
			if cut == "valid" || cut == "lost-response" {
				want = 1
			}
			for _, model := range []any{&task.CrawlTask{}, &repository.CrawlResult{}} {
				var n int64
				if err := s.DB.Model(model).Count(&n).Error; err != nil || n != want {
					t.Fatalf("unexpected commit count %T: %d want %d error=%v", model, n, want, err)
				}
			}
		})
	}
}

func TestNativeFactProjectionKeepsDeclaredIdentityAndFieldFragments(t *testing.T) {
	s, _ := retrievalService(t)
	ctx := context.Background()
	publicSource(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		rows := []any{map[string]any{"name": "A", "version": "2.0", "license": "MIT"}, map[string]any{"name": "B", "version": "3.0", "license": "BSD"}}
		if r.URL.Query().Get("edition") == "new" {
			rows[0], rows[1] = rows[1], rows[0]
		}
		json.NewEncoder(w).Encode(map[string]any{"items": rows})
	}))
	var original map[string]knowledge.Head
	for stage, setup := range []struct {
		url, namespace string
		partial        bool
		heads          int
	}{
		{"http://8.8.8.8/catalog", "package-release", false, 2},
		{"http://8.8.8.8/catalog?edition=new", "package-release", false, 2},
		{"http://8.8.8.8/catalog?edition=new", "package-release", true, 4},
		{"http://8.8.8.8/catalog", "independent-catalog", false, 6},
	} {
		fields := []aiapp.FieldRequirement{{Name: "name", Type: "string"}, {Name: "version", Type: "string"}}
		pointers := map[string]string{"name": "/name", "version": "/version"}
		if !setup.partial {
			fields = append(fields, aiapp.FieldRequirement{Name: "license", Type: "string"})
			pointers["license"] = "/license"
		}
		var spec aiapp.TaskSpec
		if err := json.Unmarshal(mustMarshal(map[string]any{"goal": "collect observed releases", "contract_version": "v2", "schema_version": "v1", "required_fields": fields, "join_keys": []string{"name"}, "source_namespace": setup.namespace, "min_records": 2}), &spec); err != nil {
			t.Fatal(err)
		}
		r, err := s.Create(ctx, Input{RequestKey: "projection-" + string(rune('0'+stage)), Goal: spec.Goal, Spec: &spec, Refresh: true, SourceScope: []string{"8.8.8.8"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Preflight(ctx, r.ID); err != nil {
			t.Fatal(err)
		}
		trial, err := s.TrialAPI(ctx, r.ID, "observed releases", APIPlan{URL: setup.url, ItemsPointer: "/items", Fields: pointers})
		if err != nil {
			t.Fatal(err)
		}
		a, err := s.Publish(ctx, r.ID, trial.CandidateID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ExecuteAPI(ctx, r.ID, a.ID); err != nil {
			t.Fatal(err)
		}
		heads, err := s.Crawler.Knowledge.Heads(ctx)
		if err != nil {
			t.Fatal(err)
		}
		facts := map[string]knowledge.Head{}
		for _, h := range heads {
			if h.Source != "information-capability" {
				facts[h.ID] = h
			}
		}
		if len(facts) != setup.heads {
			t.Errorf("stage %d multiplied declared identities or collapsed field/source fragments: got %d want %d", stage, len(facts), setup.heads)
		}
		if stage == 0 {
			original = facts
		}
		if stage == 1 {
			for id := range original {
				if h, ok := facts[id]; !ok || h.URL != setup.url {
					t.Errorf("same declared entity lost identity after row reorder and citation URL refresh: %s", id)
				}
			}
		}
		if stage == 2 {
			for id := range original {
				h, ok := facts[id]
				if !ok {
					t.Errorf("partial refresh erased full field profile: %s", id)
					continue
				}
				doc, err := s.Crawler.Knowledge.Document(ctx, h.ID, h.Revision)
				if err != nil {
					t.Fatal(err)
				}
				var body map[string]any
				if err := json.Unmarshal([]byte(doc.Body), &body); err != nil || body["license"] == nil || body["source_url"] != nil {
					t.Errorf("projection mixed receipt fields or erased unrefreshed facts: %s", doc.Body)
				}
			}
		}
	}
}

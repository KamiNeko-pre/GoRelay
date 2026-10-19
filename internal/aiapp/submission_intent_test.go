package aiapp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
)

func TestSubmissionIdentityBelongsToHostNotModelCallID(t *testing.T) {
	for _, second := range []string{`{"source_id":"intent-source","url":"https://example.com/notices","params":{"page":1,"lang":"zh"}}`, `{"params":{"lang":"zh","page":1},"url":"https://example.com/notices","source_id":"intent-source"}`, `{"source_id":"intent-source","url":"https://example.com/notices","params":{}}`} {
		t.Run(second, func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			spec := TaskSpec{Goal: "collect public facts", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "value", Type: "string"}}}
			output, _ := json.Marshal(spec)
			artifact, err := s.Data.SaveArtifact(ctx, agentdata.ProgramArtifact{Code: "return true,{items={}}", Kind: "adapter", PolicyVersion: "public-read-v1", OutputSchema: string(output), ScopeJSON: `["example.com"]`, InputSchema: `{"type":"object","properties":{"page":{"type":"integer"},"lang":{"type":"string"}},"additionalProperties":false}`})
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Data.Publish(ctx, agentdata.SourceVersion{SourceID: "intent-source", ArtifactHash: artifact.Hash}, "", 0)
			if err != nil {
				t.Fatal(err)
			}
			create := func(key string) agentdata.Run {
				r, err := s.Create(ctx, CreateRunRequest{RequestKey: key, Goal: spec.Goal, Spec: &spec, SourceScope: []string{"example.com"}})
				if err != nil {
					t.Fatal(err)
				}
				r, err = s.Data.Claim(ctx, r.ID, time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				return r
			}
			call := func(r agentdata.Run, id, args string) agentdata.RunTask {
				out, err := s.executeDurableTool(ctx, r, ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: "submit_acquisition", Arguments: args}})
				if err != nil {
					t.Fatal(err)
				}
				var link agentdata.RunTask
				if err := json.Unmarshal(out.Data, &link); err != nil || link.TaskID == 0 {
					t.Fatal("missing child receipt", err)
				}
				return link
			}
			r := create("first-run")
			args := `{"source_id":"intent-source","url":"https://example.com/notices","params":{"page":1,"lang":"zh"}}`
			if second == `{"source_id":"intent-source","url":"https://example.com/notices","params":{}}` {
				args = `{"source_id":"intent-source","url":"https://example.com/notices"}`
			}
			first := call(r, "first-model-id", args)
			// Recover an evicted receipt; a visible repeat is separately refused
			// by the host boundary tests, regardless of the model's new call ID.
			out, e := s.executeDurableToolWithVisibility(ctx, r, ToolCall{ID: "another-model-id", Type: "function", Function: FunctionCall{Name: "submit_acquisition", Arguments: second}}, visibleReceipts{})
			if e != nil {
				t.Fatal(e)
			}
			var duplicate agentdata.RunTask
			json.Unmarshal(out.Data, &duplicate)
			if first.TaskID != duplicate.TaskID {
				t.Fatalf("same acquisition recreated after model ID changed: %d != %d", first.TaskID, duplicate.TaskID)
			}
			var children, outboxes int64
			s.Data.DB.Model(&task.CrawlTask{}).Count(&children)
			s.Data.DB.Model(&task.TaskOutbox{}).Count(&outboxes)
			if children != 1 || outboxes != 1 {
				t.Fatal("duplicate publication", children, outboxes)
			}
			page := call(r, "page-two", `{"source_id":"intent-source","url":"https://example.com/notices","params":{"page":2,"lang":"zh"}}`)
			otherURL := call(r, "another-url", `{"source_id":"intent-source","url":"https://example.com/results","params":{"page":1,"lang":"zh"}}`)
			refresh := call(create("refresh-run"), "first-model-id", args)
			if first.TaskID == page.TaskID || first.TaskID == otherURL.TaskID || first.TaskID == refresh.TaskID {
				t.Fatal("legitimate new work collapsed into old intent")
			}
			artifact.Code += " -- new immutable adapter version"
			artifact.Hash = ""
			updated, err := s.Data.SaveArtifact(ctx, artifact)
			if err != nil {
				t.Fatal(err)
			}
			head, err := s.Data.SourceHead(ctx, "intent-source")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Data.Publish(ctx, agentdata.SourceVersion{SourceID: "intent-source", ArtifactHash: updated.Hash}, head.ArtifactHash, head.Epoch); err != nil {
				t.Fatal(err)
			}
			// An independent run is an explicit refresh. A changed backend source
			// head cannot silently invalidate this run's saved identical call.
			repaired := call(create("repaired-version-run"), "repaired-version", args)
			if repaired.TaskID == first.TaskID {
				t.Fatal("new adapter version could not execute")
			}
		})
	}
}

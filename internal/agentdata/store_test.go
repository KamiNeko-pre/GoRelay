package agentdata

import (
	"context"
	"errors"
	task "go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func TestArtifactIdentityPreservesLegacyAndBindsParameterExample(t *testing.T) {
	s := NewStore(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, code := range []string{"legacy A", "legacy B"} {
		a := ProgramArtifact{Code: code, Kind: "adapter", PolicyVersion: "public-read-v1", ScopeJSON: "[]", OutputSchema: "{}"}
		want := Hash([]string{a.Code, a.Kind, a.PolicyVersion, a.InputSchema, a.OutputSchema, a.ScopeJSON})
		saved, err := s.SaveArtifact(ctx, a)
		if err != nil || saved.Hash != want {
			t.Fatalf("legacy version identity changed: %v got=%s want=%s", err, saved.Hash, want)
		}
	}
	a := ProgramArtifact{Code: "adapter", InputSchema: `{"type":"object"}`, InputExampleJSON: `{"page":1}`}
	first, err := s.SaveArtifact(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	same, err := s.SaveArtifact(ctx, a)
	if err != nil || same.Hash != first.Hash {
		t.Fatal("identical adapter not reusable")
	}
	a.InputExampleJSON = `{"page":2}`
	changed, err := s.SaveArtifact(ctx, a)
	if err != nil || changed.Hash == first.Hash {
		t.Fatal("immutable example overwritten")
	}
}

func TestRunLeaseAndAtomicIdempotentChild(t *testing.T) {
	db := testdb.Open(t)
	s := NewStore(db)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := s.CreateRun(ctx, "request-1", "goal", `{"goal":"goal"}`, `{}`, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.SubmitChild(ctx, r.ID, lease.Token, "operation-1", "input-1", ProgramArtifact{Code: "return true,{items={}}", PolicyVersion: "public-v1"}, "https://example.com", `{}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	var persisted task.CrawlTask
	if err := db.First(&persisted, child.TaskID).Error; err != nil || persisted.ExecutionDeadline == nil || persisted.ExecutionDeadline.Sub(r.Deadline).Abs() > time.Millisecond {
		t.Fatal("child lost parent execution deadline", persisted.ExecutionDeadline, err)
	}
	replay, err := s.SubmitChild(ctx, r.ID, lease.Token, "operation-1", "input-1", ProgramArtifact{Code: "ignored"}, "https://example.com", `{}`, nil)
	if err != nil || replay.TaskID != child.TaskID {
		t.Fatalf("duplicate child: %+v %v", replay, err)
	}
	if _, err = s.SubmitChild(ctx, r.ID, lease.Token, "operation-1", "different", ProgramArtifact{}, "", `{}`, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed input accepted: %v", err)
	}
	var tasks, outboxes int64
	db.Model(&task.CrawlTask{}).Count(&tasks)
	db.Model(&task.TaskOutbox{}).Count(&outboxes)
	if tasks != 1 || outboxes != 1 {
		t.Fatalf("partial/duplicate transaction: %d/%d", tasks, outboxes)
	}
	if _, err = s.SubmitChild(ctx, r.ID, "old-token", "operation-2", "input-2", ProgramArtifact{}, "", `{}`, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("old token accepted: %v", err)
	}
	if err = s.Cancel(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(ctx, r.ID, time.Minute); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancelled run restarted: %v", err)
	}
}

func TestChildTransactionRollsBackAssociationOutboxAndArtifact(t *testing.T) {
	db := testdb.Open(t)
	s := NewStore(db)
	s.Migrate()
	ctx := context.Background()
	r, _ := s.CreateRun(ctx, "request", "goal", `{}`, `{}`, time.Now().Add(time.Minute))
	lease, _ := s.Claim(ctx, r.ID, time.Minute)
	_, err := s.SubmitChild(ctx, r.ID, lease.Token, "op", "hash", ProgramArtifact{Code: "code", PolicyVersion: "p"}, "https://example.com", `{}`, func() error { return errors.New("injected before commit") })
	if err == nil {
		t.Fatal("injection did not abort")
	}
	for _, model := range []any{&task.CrawlTask{}, &task.TaskOutbox{}, &RunTask{}, &ProgramArtifact{}, &Operation{}} {
		var count int64
		db.Model(model).Count(&count)
		if count != 0 {
			t.Fatalf("transaction left %T rows: %d", model, count)
		}
	}
}

func TestRegistryCASQuarantineAndHistory(t *testing.T) {
	db := testdb.Open(t)
	s := NewStore(db)
	s.Migrate()
	ctx := context.Background()
	candidate := ProgramArtifact{Code: "return true,{items={}}", PolicyVersion: "public-v1", OutputSchema: `{"required_fields":[{"name":"name","type":"string"}]}`, ScopeJSON: `["example.com"]`}
	artifact, err := s.SaveArtifact(ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	src := SourceVersion{SourceID: "sample", ArtifactHash: artifact.Hash, Description: "sample", ProofJSON: `{"fixture":true}`, ValidatorVersion: "v1"}
	published, err := s.Publish(ctx, src, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Publish(ctx, src, "", 0)
	if err != nil || again.ArtifactHash != published.ArtifactHash {
		t.Fatalf("idempotency lost: %+v %v", again, err)
	}
	if err = s.Quarantine(ctx, "sample", artifact.Hash, "schema_violation"); err != nil {
		t.Fatal(err)
	}
	if list, err := s.ListSources(ctx); err != nil || len(list) != 0 {
		t.Fatalf("quarantined source reused: %+v %v", list, err)
	}
	if _, err = s.Publish(ctx, src, "", 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("late admission resurrected source: %v", err)
	}
	if _, err = s.Artifact(ctx, artifact.Hash); err != nil {
		t.Fatalf("history deleted: %v", err)
	}
}

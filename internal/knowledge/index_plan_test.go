package knowledge

import (
	"context"
	"errors"
	"testing"
)

func TestCoreIndexPlan(t *testing.T) {
	base := IndexInput{Head: Head{ID: "doc", Active: true, Revision: "new", Epoch: 2, BodyHash: "body", Fingerprint: "model", ParentGeneration: "parent"}, Work: IndexWork{DocID: "doc", Revision: "new", Epoch: 2, BodyHash: "body", Fingerprint: "model", Generation: "new-gen"}, Parent: &IndexWork{DocID: "doc", Generation: "parent", Status: "ready", BodyHash: "body", Fingerprint: "model"}}
	for _, tc := range []struct {
		name   string
		mutate func(*IndexInput)
		mode   string
	}{
		{"metadata_only", func(in *IndexInput) {}, "reuse"},
		{"first_index", func(in *IndexInput) { in.Parent = nil }, "rebuild"},
		{"changed_body", func(in *IndexInput) { p := *in.Parent; p.BodyHash = "old"; in.Parent = &p }, "rebuild"},
		{"changed_model", func(in *IndexInput) { p := *in.Parent; p.Fingerprint = "old-model"; in.Parent = &p }, "rebuild"},
		{"unfinished_parent", func(in *IndexInput) { p := *in.Parent; p.Status = "building"; in.Parent = &p }, "rebuild"},
		{"foreign_parent", func(in *IndexInput) { p := *in.Parent; p.DocID = "other"; in.Parent = &p }, "rebuild"},
		{"deleted", func(in *IndexInput) { in.Head.Active = false }, "reject"},
		{"late_epoch", func(in *IndexInput) { in.Work.Epoch = 1 }, "reject"},
		{"late_revision", func(in *IndexInput) { in.Work.Revision = "old" }, "reject"},
		{"late_model", func(in *IndexInput) { in.Work.Fingerprint = "old" }, "reject"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mutate(&in)
			p, err := BuildIndexPlan(context.Background(), in)
			if err != nil || p.Mode != tc.mode {
				t.Fatalf("%+v %v", p, err)
			}
			if p.Mode == "reuse" && p.ParentGeneration != "parent" {
				t.Fatal(p)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := BuildIndexPlan(ctx, base); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

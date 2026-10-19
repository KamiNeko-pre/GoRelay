package knowledge

import (
	"context"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func TestLateSnapshotIsArchivedWithoutRollingBackCurrentKnowledge(t *testing.T) {
	for _, label := range []string{"price", "release"} {
		t.Run(label, func(t *testing.T) {
			s := NewStore(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			at := time.Now().UTC().Truncate(time.Second)
			makeDoc := func(body string, when time.Time) Document {
				d, err := NewDocument(Input{Source: "source", SourceKey: label, Title: label, Body: body, Format: "text", URL: "https://example.com/" + label, FetchedAt: when})
				if err != nil {
					t.Fatal(err)
				}
				return d
			}
			fresh := makeDoc("new facts", at)
			current, err := s.Put(ctx, fresh, "embed-v1")
			if err != nil {
				t.Fatal(err)
			}
			old := makeDoc("old facts", at.Add(-time.Minute))
			late, err := s.Put(ctx, old, "embed-v1")
			if err != nil {
				t.Fatal(err)
			}
			if late.Revision != fresh.Revision || late.Epoch != current.Epoch {
				t.Fatal("older acquisition rolled current head back")
			}
			if _, err := s.Document(ctx, old.ID, old.Revision); err != nil {
				t.Fatal("late snapshot was not retained for audit")
			}
			var n int64
			s.DB.Model(&IndexWork{}).Count(&n)
			if n != 1 {
				t.Fatalf("obsolete snapshot created index work: %d", n)
			}
			next := makeDoc("newer facts", at.Add(time.Minute))
			updated, err := s.Put(ctx, next, "embed-v1")
			if err != nil || updated.Revision != next.Revision || updated.Epoch != current.Epoch+1 {
				t.Fatal("fresh update was blocked")
			}
			retargeted, err := s.Put(ctx, next, "embed-v2")
			if err != nil || retargeted.Fingerprint != "embed-v2" {
				t.Fatal("same snapshot model retarget was blocked")
			}
		})
	}
}

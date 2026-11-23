package agentbench

import (
	"go-lua-crawler/internal/aiapp"
	"testing"
)

func TestBusinessDiffIgnoresRowOrderAndScoresChangedFields(t *testing.T) {
	row := func(name string, price int) aiapp.MergedRecord {
		return aiapp.MergedRecord{Fields: map[string]any{"name": name, "price": price}}
	}
	before := []aiapp.MergedRecord{row("A", 10), row("B", 20)}
	if got, err := DiffBusinessRecords(before, []aiapp.MergedRecord{row("B", 20), row("A", 10)}, []string{"name"}); err != nil || len(got) != 0 {
		t.Fatalf("row order created business changes: %v %v", got, err)
	}
	changes, err := DiffBusinessRecords(before, []aiapp.MergedRecord{row("B", 21), row("C", 30)}, []string{"name"})
	if err != nil || len(changes) != 3 {
		t.Fatalf("expected add/remove/price change: %v %v", changes, err)
	}
	counts := ScoreBusinessChanges(changes, changes[:2])
	if counts.TP != 2 || counts.FP != 1 || counts.FN != 0 {
		t.Fatalf("wrong gold comparison: %+v", counts)
	}
	counts = ScoreBusinessChanges(changes[:1], changes)
	if counts.TP != 1 || counts.FN != 2 {
		t.Fatalf("missing changes hidden: %+v", counts)
	}
}

func TestBusinessDiffRejectsAmbiguousIdentity(t *testing.T) {
	row := aiapp.MergedRecord{Fields: map[string]any{"name": "A", "price": 10}}
	for _, rows := range [][]aiapp.MergedRecord{{row, row}, {{Fields: map[string]any{"price": 10}}}} {
		if _, err := DiffBusinessRecords(rows, nil, []string{"name"}); err == nil {
			t.Fatal("ambiguous/missing business key accepted")
		}
	}
}

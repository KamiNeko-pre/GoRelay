package main

import "testing"

func TestAuditExposesSemanticFalseAcceptance(t *testing.T) {
	rows, err := audit()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 || !rows[0].GoldCorrect || !rows[0].GoalCoverage || rows[1].StructuralAccepted {
		t.Fatal("healthy/layout control failed")
	}
	for _, i := range []int{2, 4} {
		if !rows[i].StructuralAccepted || rows[i].GoldCorrect {
			t.Fatal("semantic false acceptance hidden")
		}
	}
	if rows[3].GoalCoverage || rows[3].GoldCorrect {
		t.Fatal("missing entity/page counted as completed")
	}
}

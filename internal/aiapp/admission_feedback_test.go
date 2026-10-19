package aiapp

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSourceAdmissionExplainsAllIndependentFailures(t *testing.T) {
	for _, mode := range []string{"identity_and_scope", "parent_and_policy", "valid"} {
		t.Run(mode, func(t *testing.T) {
			in := admissionFixture(t)
			if mode == "identity_and_scope" {
				in.Candidate.ArtifactHash = "wrong"
				in.Scope = []string{"other.example"}
			}
			if mode == "parent_and_policy" {
				in.Current.Epoch = 2
				in.Artifact.PolicyVersion = "wrong"
			}
			decision, err := RegisterValidatedSource(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(decision)
			var value struct {
				Issues []any `json:"issues"`
			}
			json.Unmarshal(raw, &value)
			if mode == "valid" {
				if !decision.Allowed || len(value.Issues) != 0 {
					t.Fatal("valid source rejected")
				}
			} else if decision.Allowed || len(value.Issues) < 2 {
				t.Fatalf("only first failure explained: %s", raw)
			}
		})
	}
}

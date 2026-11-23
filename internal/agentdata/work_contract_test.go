package agentdata

import (
	"encoding/json"
	"testing"
)

func contractWork() WorkIdentity {
	return WorkIdentity{RunID: "run", RequirementID: "basic", ArtifactHash: "adapter-v1", Endpoint: "https://Example.com/api?mode=basic&region=cn", Params: map[string]any{"limit": json.Number("10.0"), "flags": []any{true, "x"}}, ContractVersion: "v2", ContractHash: "frozen-contract", RefreshGeneration: 1}
}

func TestWorkContractCanonicalIdentityAndCandidateScope(t *testing.T) {
	units := []string{"basic", "advanced"}
	work := contractWork()
	key, err := work.Key(units)
	if err != nil {
		t.Fatal(err)
	}
	same := work
	same.Endpoint = "https://example.com/api?region=cn&mode=basic"
	same.Params = map[string]any{"flags": []any{true, "x"}, "limit": 10}
	k, err := same.Key(units)
	if err != nil || k != key {
		t.Fatal("equivalent typed input changed work identity", err, key, k)
	}
	a, err := work.AdaptationKey(units)
	if err != nil {
		t.Fatal(err)
	}
	same.ArtifactHash = "adapter-v2"
	k, err = same.Key(units)
	if err != nil || k == key {
		t.Fatal("different artifact collapsed", err)
	}
	b, err := same.AdaptationKey(units)
	if err != nil || a != b {
		t.Fatal("candidate code reset its adaptation scope", err)
	}
	for _, field := range []string{"requirement", "url-query", "params-type", "refresh", "contract"} {
		sibling := work
		switch field {
		case "requirement":
			sibling.RequirementID = "advanced"
		case "url-query":
			sibling.Endpoint = "https://example.com/api?mode=advanced&region=cn"
		case "params-type":
			sibling.Params = map[string]any{"limit": "10", "flags": []any{true, "x"}}
		case "refresh":
			sibling.RefreshGeneration = 2
		case "contract":
			sibling.ContractHash = "other-frozen-contract"
		}
		k, err := sibling.Key(units)
		if err != nil || k == key {
			t.Fatal("legitimate work difference collapsed", field, err)
		}
	}
	bad := work
	bad.RequirementID = "invented"
	if _, err := bad.Key(units); err == nil {
		t.Fatal("model supplied an unfrozen requirement identity")
	}
}

func TestExecutionReceiptContractDoesNotInventKnownZeroUsage(t *testing.T) {
	for _, kind := range []FailureKind{FailureInvalidProposal, FailureCodeInvalid, FailureNotStartedTransient, FailureTransientExternal, FailurePermanentDenial, FailureUnknownOutcome, FailureIncompleteDelivery, FailureLeaseLost, FailureTargetSuperseded} {
		if err := kind.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	if err := FailureKind("invented").Validate(); err == nil {
		t.Fatal("unknown failure contract accepted")
	}
	valid := ExecutionReceipt{WorkKey: "work", Attempt: 1, Start: StartNotStarted, Outcome: OutcomeKnown, UsageKnown: true}
	if err := valid.Validate(); err != nil {
		t.Fatal("trusted no-start refusal rejected", err)
	}
	for _, field := range []string{"start", "outcome", "usage", "physical-io"} {
		bad := valid
		switch field {
		case "start":
			bad.Start = "invented"
		case "outcome":
			bad.Outcome = OutcomeUnknown
		case "usage":
			bad.UsageKnown = false
		case "physical-io":
			bad.HTTPRequests = 1
		}
		if err := bad.Validate(); err == nil {
			t.Fatal("contradictory no-start receipt accepted", field)
		}
	}
	unknown := ExecutionReceipt{WorkKey: "work", Attempt: 2, Start: StartUnknown, Outcome: OutcomeUnknown, UsageKnown: false}
	if err := unknown.Validate(); err != nil {
		t.Fatal("real transport uncertainty rejected", err)
	}
}

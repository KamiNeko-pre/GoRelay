package agentstatus

import (
	"testing"
	"time"
)

func TestTaskTimeoutUsesCallerDifficultyOrHostPolicy(t *testing.T) {
	for _, seconds := range []int64{0, 90, 1200} {
		d, err := RequestTimeout(&seconds, 11*time.Minute)
		if err != nil || d != time.Duration(seconds)*time.Second {
			t.Fatal("caller difficulty budget replaced", d, err)
		}
	}
	if d, err := RequestTimeout(nil, 11*time.Minute); err != nil || d != 11*time.Minute {
		t.Fatal("deployment default ignored", d, err)
	}
	negative := int64(-1)
	if _, err := RequestTimeout(&negative, 0); err == nil {
		t.Fatal("negative timeout accepted")
	}
}

func TestOnlyBusinessAndSystemFailuresHaveOperatorPolicy(t *testing.T) {
	for _, code := range []Code{AgentCorrection, AgentDuplicate, AgentExhausted, SourceUnavailable, AntiBotBrowserApproved, BrowserAttemptsExhausted, TaskDeadline, TaskCancelled, Unclassified} {
		s := Of(code)
		if s.SystemLog || s.AlertPolicy != "none" {
			t.Fatalf("agent/source behavior escalated to human repair: %+v", s)
		}
	}
	if s := Of(BalanceUnavailable); !s.SystemLog || s.AlertPolicy != "immediate_on_deployment" {
		t.Fatal(s)
	}
	if s := Of(SystemDependency); !s.SystemLog || s.AlertPolicy != "after_consecutive_system_failures_on_deployment" {
		t.Fatal(s)
	}
}

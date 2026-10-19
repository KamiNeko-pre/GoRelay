package information

import (
	"testing"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/aiapp"
)

func TestBrowserDecisionRequiresSourceCauseAndLogsOtherFailures(t *testing.T) {
	source := luaDiagnostic{OperationID: 21, Tool: "inspect_source", Failure: &aiapp.FailureInfo{Code: "SOURCE_HTTP_ERROR"}, Receipt: &acquisition.Receipt{URL: "https://source.example/items", Status: 403}}
	provider := luaDiagnostic{OperationID: 22, Tool: "model", Failure: &aiapp.FailureInfo{Code: "PROVIDER_ERROR", Provider: &aiapp.ProviderError{Status: 403}}}
	for _, tc := range []struct {
		name       string
		diagnostic luaDiagnostic
		decision   browserAssessment
		want       bool
	}{
		{"source-antibot-403", source, browserAssessment{Action: "request_browser", CauseCode: "anti_bot", Reason: "source challenge needs browser session", SourceOperationID: 21}, true},
		{"same-status-from-provider", provider, browserAssessment{Action: "request_browser", CauseCode: "anti_bot", Reason: "request was forbidden", SourceOperationID: 22}, false},
		{"source-403-access-denied", source, browserAssessment{Action: "record_failure", CauseCode: "access", Reason: "resource requires authentication", SourceOperationID: 21}, false},
		{"source-403-but-program-bug", source, browserAssessment{Action: "request_browser", CauseCode: "program", Reason: "Lua syntax is incorrect", SourceOperationID: 21}, false},
		{"fabricated-source-reference", source, browserAssessment{Action: "request_browser", CauseCode: "anti_bot", Reason: "source challenge", SourceOperationID: 999}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, err := browserTarget(tc.decision, luaFailureReport{Diagnostics: []luaDiagnostic{tc.diagnostic}}, []string{"source.example"})
			if (err == nil && target != "") != tc.want {
				t.Fatalf("browser decision=%q err=%v want=%v", target, err, tc.want)
			}
		})
	}
}

func TestLuaFailureReportDoesNotAssignAgentMistakesToOperator(t *testing.T) {
	for _, cause := range []string{"program", "parameters", "fields", "evidence"} {
		_, err := browserTarget(browserAssessment{Action: "record_failure", CauseCode: cause, Reason: "normal correction did not deliver"}, luaFailureReport{}, nil)
		status := aiapp.OperationalStatus(err)
		if status.Owner != "agent" || status.SystemLog || status.AlertPolicy != "none" {
			t.Fatal("model behavior escalated for human repair", status)
		}
	}
	_, err := browserTarget(browserAssessment{Action: "record_failure", CauseCode: "access", Reason: "resource requires login"}, luaFailureReport{}, nil)
	if status := aiapp.OperationalStatus(err); status.Owner != "source" || status.SystemLog {
		t.Fatal("source access attributed to system", status)
	}
}

package main

import (
	"strings"
	"testing"
)

func TestStatusDoesNotPrintSecretsOrClaimLiveReadiness(t *testing.T) {
	t.Setenv("GORELAY_AI_API_KEY", "dont-print-me")
	t.Setenv("GORELAY_AI_BASE_URL", "")
	t.Setenv("GORELAY_AI_MODEL", "")
	t.Setenv("GORELAY_EMBED_API_KEY", "")
	t.Setenv("GORELAY_RUNNER_URL", "")
	out := configurationStatus()
	if strings.Contains(out, "dont-print-me") || !strings.Contains(out, `"chat_configured":false`) || !strings.Contains(out, `"live_verified":false`) {
		t.Fatalf("unsafe/misleading status: %s", out)
	}
}

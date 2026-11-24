package deploy

import (
	"os"
	"strings"
	"testing"
)

func TestDeploymentConfigsAreCleanV2(t *testing.T) {
	forbidden := []string{"qdrant", "information-worker", "cmd/indexer", "GORELAY_AI_API_KEY", "GORELAY_EMBED_", "LUA_SPIDER_", "COPY scripts"}
	for _, file := range []string{"compose.yml", ".env.example", "../docker-compose.yml", "../.env.example", "../Dockerfile"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		text := string(raw)
		for _, term := range forbidden {
			if strings.Contains(text, term) {
				t.Fatalf("%s still contains removed legacy dependency %q", file, term)
			}
		}
	}
	composeRaw, _ := os.ReadFile("compose.yml")
	for _, want := range []string{"mysql:", "redis:", "lua-runner:", "app:", "publisher:", "worker:", "GORELAY_CHECKPOINT_LISTEN:"} {
		if !strings.Contains(string(composeRaw), want) {
			t.Fatalf("deploy/compose.yml missing required v2 component %q", want)
		}
	}
}

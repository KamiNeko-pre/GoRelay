package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJobRulesUseHostRateLimit(t *testing.T) {
	rules := []string{"nowcoder.lua", "lever.lua", "greenhouse.lua", "ashby.lua"}
	for _, name := range rules {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", "scripts", "jobs", name)
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			text := string(content)
			if !strings.Contains(text, "http_get_limited(") {
				t.Fatalf("%s does not use the host-limited HTTP helper", name)
			}
			if !strings.Contains(text, "sleep_ms(") {
				t.Fatalf("%s does not use cancellation-aware retry backoff", name)
			}
		})
	}
}

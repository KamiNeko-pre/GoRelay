package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

func TestScaleDefaultsUseEightWorkersAndSixtySecondLuaBudget(t *testing.T) {
	root := filepath.Join("..", "..")
	content, err := os.ReadFile(filepath.Join(root, "configs", "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var file struct {
		Engine struct {
			WorkerCount       int `yaml:"worker_count"`
			LuaTimeout        int `yaml:"lua_timeout"`
			HTTPTimeoutDirect int `yaml:"http_timeout_direct"`
			HTTPTimeoutProxy  int `yaml:"http_timeout_proxy"`
		} `yaml:"engine"`
	}
	if err := yaml.Unmarshal(content, &file); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if file.Engine.WorkerCount != 8 {
		t.Errorf("worker_count = %d, want 8", file.Engine.WorkerCount)
	}
	if file.Engine.LuaTimeout != 60 {
		t.Errorf("lua_timeout = %d, want 60", file.Engine.LuaTimeout)
	}
	if file.Engine.HTTPTimeoutDirect != 5 {
		t.Errorf("http_timeout_direct = %d, want 5", file.Engine.HTTPTimeoutDirect)
	}
	if file.Engine.HTTPTimeoutProxy != 15 {
		t.Errorf("http_timeout_proxy = %d, want 15", file.Engine.HTTPTimeoutProxy)
	}

	compose, err := os.ReadFile(filepath.Join(root, "docker-compose.yml"))
	if err != nil {
		t.Fatalf("read Compose file: %v", err)
	}
	if !strings.Contains(string(compose), "${WORKERS:-8}") {
		t.Errorf("Compose does not default WORKERS to 8")
	}

	example, err := os.ReadFile(filepath.Join(root, ".env.example"))
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	if !strings.Contains(string(example), "WORKERS=8") {
		t.Errorf(".env.example does not advertise WORKERS=8")
	}
}

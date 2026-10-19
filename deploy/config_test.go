package deploy

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Opt-in, read-only Compose rendering. No up/build/pull or provider requests.
func TestComposePassesSpendPolicyAndLimitsToAllConsumers(t *testing.T) {
	docker := os.Getenv("GORELAY_DOCKER_BIN")
	if docker == "" {
		t.Skip("GORELAY_DOCKER_BIN required for Compose rendering")
	}
	for _, policyPath := range []string{"", "/app/configs/spend-policy.json"} {
		name := "disabled"
		if policyPath != "" {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			source, err := filepath.Abs("../configs/agentbench/spend-policy.example.json")
			if err != nil {
				t.Fatal(err)
			}
			fixture := map[string]string{"MYSQL_ROOT_PASSWORD": "compose-fixture-root", "MYSQL_PASSWORD": "compose-fixture-user", "GORELAY_ADMIN_TOKEN": "compose-fixture-admin", "GORELAY_RUNNER_TOKEN": "compose-fixture-runner", "GORELAY_SPEND_POLICY_PATH": policyPath, "GORELAY_SPEND_POLICY_FILE": source, "GORELAY_RUN_MODEL_CALLS": "4", "GORELAY_RUN_SEARCH_CALLS": "1", "GORELAY_AI_API_KEY": "", "GORELAY_EMBED_API_KEY": "", "GORELAY_SEARCH_API_KEY": ""}
			cmd := exec.Command(docker, "compose", "--env-file", ".env.example", "-f", "compose.yml", "--profile", "indexing", "config", "--format", "json")
			for _, entry := range os.Environ() {
				key, _, _ := strings.Cut(entry, "=")
				if _, override := fixture[key]; !override {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			for key, value := range fixture {
				cmd.Env = append(cmd.Env, key+"="+value)
			}
			raw, err := cmd.Output()
			if err != nil {
				t.Fatal("Compose render failed", err)
			}
			var out struct {
				Services map[string]struct {
					Environment map[string]string
					Volumes     []struct {
						Type, Source, Target string
						ReadOnly             bool `json:"read_only"`
						Bind                 struct {
							CreateHostPath bool `json:"create_host_path"`
						}
					}
				}
			}
			if err = json.Unmarshal(raw, &out); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"app", "worker", "publisher", "indexer"} {
				service, exists := out.Services[name]
				if !exists {
					t.Fatalf("rendered Compose did not include %s", name)
				}
				actual, declared := service.Environment["GORELAY_SPEND_POLICY_PATH"]
				if !declared || actual != policyPath {
					t.Fatalf("%s does not expose disabled/enabled spend gate", name)
				}
				if service.Environment["GORELAY_RUN_MODEL_CALLS"] != "4" || service.Environment["GORELAY_RUN_SEARCH_CALLS"] != "1" {
					t.Fatalf("%s loses configured call limits", name)
				}
				found := false
				for _, volume := range service.Volumes {
					if volume.Target == "/app/configs/spend-policy.json" {
						found = volume.Type == "bind" && volume.ReadOnly && !volume.Bind.CreateHostPath && filepath.Clean(volume.Source) == filepath.Clean(source)
					}
				}
				if !found {
					t.Fatalf("%s lacks an existing read-only policy file mount", name)
				}
			}
			// The runner consumes executable code, never API pricing or credentials.
			if _, declared := out.Services["lua-runner"].Environment["GORELAY_SPEND_POLICY_PATH"]; declared {
				t.Fatal("policy leaked into Lua sandbox")
			}
		})
	}
}

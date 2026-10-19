package aiapp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInferenceLoginFollowsNewerSameAccountRefresh(t *testing.T) {
	for _, tc := range []struct {
		name, parentTime, isolatedTime, parentAccount, isolatedAccount string
		wantParent                                                     bool
	}{
		{"revoked_old_copy", "2026-10-11T11:44:44Z", "2026-10-10T17:43:47Z", "same", "same", true},
		{"other_new_refresh", "2026-10-12T11:44:44Z", "2026-10-11T17:43:47Z", "same", "same", true},
		{"isolated_refresh_newer", "2026-10-10T11:44:44Z", "2026-10-11T17:43:47Z", "same", "same", false},
		{"different_account", "2026-10-12T11:44:44Z", "2026-10-11T17:43:47Z", "different", "same", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, isolated := t.TempDir(), t.TempDir()
			write := func(dir, stamp, account, token string) []byte {
				raw, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "last_refresh": stamp, "tokens": map[string]any{"account_id": account, "access_token": token}})
				os.WriteFile(filepath.Join(dir, "auth.json"), raw, 0600)
				return raw
			}
			parentRaw := write(parent, tc.parentTime, tc.parentAccount, "parent-fixture")
			isolatedRaw := write(isolated, tc.isolatedTime, tc.isolatedAccount, "isolated-fixture")
			if err := prepareInferenceHome(parent, isolated); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(filepath.Join(isolated, "auth.json"))
			want := isolatedRaw
			if tc.wantParent {
				want = parentRaw
			}
			if string(got) != string(want) {
				t.Fatal("wrong login revision selected")
			}
		})
	}
}

func TestInferenceHomeContainsLoginButNoDeveloperContext(t *testing.T) {
	for _, unrelated := range []string{"AGENTS.md", "skills/local/SKILL.md"} {
		parent, isolated := t.TempDir(), t.TempDir()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(parent, unrelated)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(parent, unrelated), []byte("unrelated developer instructions"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(parent, "auth.json"), []byte("fixture login"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := prepareInferenceHome(parent, isolated); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(isolated)
		if err != nil || len(entries) != 1 || entries[0].Name() != "auth.json" {
			t.Fatalf("developer context copied: %v %v", entries, err)
		}
		// Do not overwrite a login refreshed by the isolated CLI or the parent login.
		if err := os.WriteFile(filepath.Join(isolated, "auth.json"), []byte("refreshed fixture login"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := prepareInferenceHome(parent, isolated); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(filepath.Join(isolated, "auth.json"))
		original, _ := os.ReadFile(filepath.Join(parent, "auth.json"))
		if string(got) != "refreshed fixture login" || string(original) != "fixture login" {
			t.Fatal("existing credentials overwritten")
		}
	}
	env := inferenceHomeEnvironment([]string{"CODEX_HOME=original", "codex_home=duplicate", "CODEX_PERMISSION_PROFILE=read-only", "KEEP=value"}, "isolated")
	joined := strings.Join(env, "\n")
	if strings.Count(strings.ToLower(joined), "codex_home=") != 1 || !strings.Contains(joined, "CODEX_HOME=isolated") || !strings.Contains(joined, "CODEX_PERMISSION_PROFILE=read-only") || !strings.Contains(joined, "KEEP=value") {
		t.Fatalf("environment or permission policy changed: %v", env)
	}
}

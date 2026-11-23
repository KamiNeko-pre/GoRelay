package agentbench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSavedBusinessManifestFreezeAndPublicInputs(t *testing.T) {
	root := os.Getenv("GORELAY_SOURCE_QUALIFICATION_DIR")
	if root == "" {
		t.Skip("business source qualification directory not configured")
	}
	data, err := os.ReadFile(filepath.Join(root, "gold-draft", "manifest.frozen.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err = json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if err = manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	if manifest.Model == "" || manifest.Build == "" || manifest.InitialState == "" {
		t.Fatal("freeze metadata missing")
	}
	data, err = os.ReadFile(filepath.Join(root, "gold-draft", "gold.private.json"))
	if err != nil {
		t.Fatal(err)
	}
	var gold map[string]GoldCase
	if err = json.Unmarshal(data, &gold); err != nil {
		t.Fatal(err)
	}
	for _, c := range manifest.Cases {
		g := gold[c.ID]
		if c.Split != "development" || g.Verification != "assistant_prepared" || len(g.Rows) == 0 || len(g.KeyFields) == 0 {
			t.Fatal("development gold incomplete or overstated", c.ID)
		}
		seen := map[string]bool{}
		for _, row := range g.Rows {
			key, err := BusinessKey(row, g.KeyFields)
			if err != nil || seen[key] {
				t.Fatal("invalid gold identity", c.ID, key, err)
			}
			seen[key] = true
		}
		raw, _ := json.Marshal(PublicRequest(c, "freeze-audit"))
		var public map[string]any
		_ = json.Unmarshal(raw, &public)
		if len(public) != 2 || public["goal"] != c.Goal || public["request_key"] != "freeze-audit" {
			t.Fatal("private freeze/gold metadata leaked into task input", string(raw))
		}
	}
}

package aiapp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var inferenceHomeMu sync.Mutex

// CLI is an experimental inference transport, not an interactive developer.
// Its private home retains refreshed login state but never imports AGENTS,
// skills, plugins, config or project history from the interactive account.
// Keep credentials outside both the repository and exported evidence.
func prepareInferenceHome(parent, isolated string) error {
	inferenceHomeMu.Lock()
	defer inferenceHomeMu.Unlock()
	if err := os.MkdirAll(isolated, 0700); err != nil {
		return err
	}
	dest := filepath.Join(isolated, "auth.json")
	if info, err := os.Stat(dest); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("isolated Codex login must be a regular file")
		}
		// A private copy may have been revoked by a later interactive login.
		// Follow a newer refresh for the SAME account, while preserving a newer
		// refresh performed by this isolated transport and never switch accounts.
		current, readErr := os.ReadFile(dest)
		if readErr != nil {
			return readErr
		}
		latest, readErr := os.ReadFile(filepath.Join(parent, "auth.json"))
		if readErr == nil && newerSameAccountLogin(latest, current) {
			return os.WriteFile(dest, latest, 0600)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	login, err := os.ReadFile(filepath.Join(parent, "auth.json"))
	if err != nil {
		return fmt.Errorf("isolated Codex transport needs an existing file-backed CLI login: %w", err)
	}
	return os.WriteFile(dest, login, 0600)
}

func newerSameAccountLogin(latest, current []byte) bool {
	type loginRevision struct {
		Mode      string    `json:"auth_mode"`
		Refreshed time.Time `json:"last_refresh"`
		Tokens    struct {
			Account string `json:"account_id"`
		} `json:"tokens"`
	}
	var a, b loginRevision
	if json.Unmarshal(latest, &a) != nil || json.Unmarshal(current, &b) != nil {
		return false
	}
	return a.Mode == "chatgpt" && b.Mode == a.Mode && a.Tokens.Account != "" && a.Tokens.Account == b.Tokens.Account && a.Refreshed.After(b.Refreshed)
}

func inferenceHomeEnvironment(env []string, isolated string) []string {
	out := make([]string, 0, len(env)+1)
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		if !strings.EqualFold(key, "CODEX_HOME") {
			out = append(out, item)
		}
	}
	return append(out, "CODEX_HOME="+isolated)
}

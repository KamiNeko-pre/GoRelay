package aiapp

import (
	"os"
	"path/filepath"
	"testing"
)

// Saved model executions remain outside the public repository. Generic unit
// tests run normally; replaying a captured private run requires explicit input.
func readPrivateReplayFixture(t *testing.T, name string) ([]byte, error) {
	t.Helper()
	root := os.Getenv("GORELAY_PRIVATE_REPLAY_DIR")
	if root == "" {
		t.Skip("private saved-run fixture not configured; set GORELAY_PRIVATE_REPLAY_DIR to replay locally")
	}
	return os.ReadFile(filepath.Join(root, name))
}

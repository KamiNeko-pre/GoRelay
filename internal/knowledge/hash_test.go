package knowledge

import (
	"encoding/hex"
	"testing"
)

func TestDigestPreservesComponentBoundaries(t *testing.T) {
	if digest("a|b", "c") == digest("a", "b|c") || digest("a", "b") == digest("ab") {
		t.Fatal("different field boundaries must not share a digest")
	}
	first := digest("manual", "context-notes")
	if first != digest("manual", "context-notes") {
		t.Fatal("digest must be deterministic")
	}
	decoded, err := hex.DecodeString(first)
	if err != nil || len(decoded) != 32 {
		t.Fatalf("expected SHA256 hex: %q, %v", first, err)
	}
}

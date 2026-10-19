package aiapp

import "testing"

func TestEmbeddingFingerprintIncludesEffectiveChunking(t *testing.T) {
	t.Setenv("GORELAY_CHUNK_RUNES", "")
	t.Setenv("GORELAY_CHUNK_OVERLAP", "")
	baseline := EmbeddingFingerprint()
	t.Setenv("GORELAY_CHUNK_RUNES", "300")
	t.Setenv("GORELAY_CHUNK_OVERLAP", "40")
	if EmbeddingFingerprint() == baseline {
		t.Fatal("changed chunk layout shares the old generation fingerprint")
	}
	changed := EmbeddingFingerprint()
	t.Setenv("GORELAY_CHUNK_OVERLAP", "41")
	if EmbeddingFingerprint() == changed {
		t.Fatal("changed overlap shares the old generation fingerprint")
	}
	// Explicit old settings and absent settings identify the same old layout.
	t.Setenv("GORELAY_CHUNK_RUNES", "600")
	t.Setenv("GORELAY_CHUNK_OVERLAP", "80")
	if EmbeddingFingerprint() != baseline {
		t.Fatal("explicit default invalidated existing generations")
	}
}

func TestChunkingSettingsRejectInvalidWindows(t *testing.T) {
	for _, values := range [][2]string{{"0", "0"}, {"300", "300"}, {"300", "-1"}, {"abc", "40"}} {
		t.Setenv("GORELAY_CHUNK_RUNES", values[0])
		t.Setenv("GORELAY_CHUNK_OVERLAP", values[1])
		if _, _, err := ChunkingSettingsFromEnv(); err == nil {
			t.Fatal("invalid layout accepted", values)
		}
	}
	t.Setenv("GORELAY_CHUNK_RUNES", "300")
	t.Setenv("GORELAY_CHUNK_OVERLAP", "0")
	size, overlap, err := ChunkingSettingsFromEnv()
	if err != nil || size != 300 || overlap != 0 {
		t.Fatal("valid zero overlap rejected", size, overlap, err)
	}
}

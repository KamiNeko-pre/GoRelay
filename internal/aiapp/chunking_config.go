package aiapp

import (
	"fmt"
	"strconv"
)

// The operator chooses a window appropriate for the embedding model's actual
// tokenizer capacity. Oversized embedding requests still fail, never truncate.
func ChunkingSettingsFromEnv() (size, overlap int, err error) {
	size, err = strconv.Atoi(envDefault("GORELAY_CHUNK_RUNES", "600"))
	if err != nil {
		return 0, 0, fmt.Errorf("GORELAY_CHUNK_RUNES must be an integer")
	}
	overlap, err = strconv.Atoi(envDefault("GORELAY_CHUNK_OVERLAP", "80"))
	if err != nil || size < 1 || overlap < 0 || overlap >= size {
		return 0, 0, fmt.Errorf("chunk size must be positive and 0 <= overlap < size")
	}
	return size, overlap, nil
}

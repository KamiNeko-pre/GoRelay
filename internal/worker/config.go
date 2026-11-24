package worker

import (
	"fmt"
	"strings"
	"time"
)

const (
	defaultReadBlock     = 2 * time.Second
	defaultReadCount     = int64(1)
	defaultMaxConcurrent = 1
	defaultClaimMinIdle  = 2 * time.Minute
	defaultClaimInterval = 15 * time.Second
)

// Config contains only Redis Worker runtime settings. It is separate from the
// application config mapping to keep the worker's dependency surface explicit.
type Config struct {
	Stream           string
	Group            string
	Consumer         string
	ReadBlock        time.Duration
	ReadCount        int64
	MaxConcurrent    int
	ClaimMinIdle     time.Duration
	ClaimInterval    time.Duration
	ExecutionTimeout time.Duration
}

// DefaultConfig supplies balanced concurrent Redis worker timing defaults.
func DefaultConfig(stream, group, consumer string) Config {
	return Config{
		Stream:           stream,
		Group:            group,
		Consumer:         consumer,
		ReadBlock:        defaultReadBlock,
		ReadCount:        defaultReadCount,
		MaxConcurrent:    defaultMaxConcurrent,
		ClaimMinIdle:     defaultClaimMinIdle,
		ClaimInterval:    defaultClaimInterval,
		ExecutionTimeout: 30 * time.Second,
	}
}

// Validate rejects configurations that would create a busy loop, an unnamed
// Consumer, or an unsafe immediate reclaim loop.
func (cfg Config) Validate() error {
	if strings.TrimSpace(cfg.Stream) == "" {
		return fmt.Errorf("worker stream is required")
	}
	if strings.TrimSpace(cfg.Group) == "" {
		return fmt.Errorf("worker consumer group is required")
	}
	if strings.TrimSpace(cfg.Consumer) == "" {
		return fmt.Errorf("worker consumer name is required")
	}
	if cfg.ReadBlock <= 0 {
		return fmt.Errorf("worker read block must be positive")
	}
	if cfg.ReadCount <= 0 {
		return fmt.Errorf("worker read count must be positive")
	}
	if cfg.ClaimMinIdle <= 0 {
		return fmt.Errorf("worker claim minimum idle time must be positive")
	}
	if cfg.ClaimInterval <= 0 {
		return fmt.Errorf("worker claim interval must be positive")
	}
	return nil
}

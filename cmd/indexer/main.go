package main

import (
	"context"
	"errors"
	"fmt"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/config"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/logger"
	"go-lua-crawler/internal/repository"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(ctx context.Context) error {
	config.InitConfig()
	logger.Init()
	defer logger.Log.Sync()
	if err := repository.InitDB(); err != nil {
		return err
	}
	sqlDB, err := repository.DB.DB()
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	service := aiapp.NewService(repository.DB)
	if err = service.Migrate(); err != nil {
		return err
	}
	service.Configure()
	if service.Retriever == nil {
		return fmt.Errorf("indexer needs configured embedding and Qdrant adapters")
	}
	dimensions, err := strconv.Atoi(os.Getenv("GORELAY_EMBED_DIMENSIONS"))
	if err != nil || dimensions < 1 {
		return fmt.Errorf("GORELAY_EMBED_DIMENSIONS must match the model's actual vector length")
	}
	vectors := service.Retriever.Vectors.(*knowledge.Qdrant)
	if err = vectors.EnsureCollection(ctx, dimensions); err != nil {
		return err
	}
	chunkSize, chunkOverlap, err := aiapp.ChunkingSettingsFromEnv()
	if err != nil {
		return err
	}
	// Changing model/chunker configuration is a new target epoch, never a write
	// into an old model's generation. Retrieval stays shielded until publication.
	if _, err = service.Knowledge.RetargetModel(ctx, service.Fingerprint); err != nil {
		return err
	}
	unlimited := false
	if value := os.Getenv("GORELAY_EXPERIMENT_UNLIMITED_BUDGET"); value != "" {
		unlimited, err = strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("invalid experiment budget control: %w", err)
		}
	}
	// The host's existing unlimited experiment policy also covers standalone
	// native facts and imports. Usage is still measured by IndexMetrics; this
	// does not change funded admission when that policy is disabled.
	indexer := knowledge.Indexer{RequireSpendPolicy: !unlimited && (service.RequireSpendPolicy || os.Getenv("GORELAY_SPEND_POLICY_PATH") != ""), Store: service.Knowledge, Embed: service.Retriever.Embed, Vectors: vectors, Fingerprint: service.Fingerprint, ChunkSize: chunkSize, ChunkOverlap: chunkOverlap}
	force, err := strconv.ParseBool(envIndexControl())
	if err != nil {
		return fmt.Errorf("invalid index experiment control")
	}
	indexer.Planner = knowledge.ExperimentalPlanner(knowledge.BuildIndexPlan, force)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			err := indexer.Once(ctx)
			if err != nil {
				fmt.Fprintln(os.Stderr, "index attempt:", err)
			}
			if len(os.Args) > 1 && os.Args[1] == "--once" {
				return err
			}
		}
	}
}
func envIndexControl() string {
	if value := os.Getenv("GORELAY_INDEX_FORCE_REBUILD"); value != "" {
		return value
	}
	return "false"
}

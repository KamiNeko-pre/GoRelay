// rag-eval adapts the production document, indexer and retriever to frozen
// evaluation inputs. It never supplies answers or model action sequences.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
	mysqlGorm "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type candidate struct {
	ID         string `json:"id"`
	Family     string `json:"family"`
	LogicalKey string `json:"logical_key"`
	Body       string `json:"body"`
	Source     struct {
		URL       string    `json:"url"`
		SHA256    string    `json:"sha256"`
		FetchedAt time.Time `json:"fetched_at"`
	} `json:"source"`
}

type queryInput struct {
	ID    string                   `json:"id"`
	Query string                   `json:"query"`
	Scope knowledge.RetrievalScope `json:"scope"`
}

type config struct{ command, database, input, output, collection, fingerprint, mode, vectorURL, embedURL string }

func main() {
	var c config
	flag.StringVar(&c.command, "command", "", "import, index, or retrieve")
	flag.StringVar(&c.database, "database", "", "isolated gorelay_eval_* schema")
	flag.StringVar(&c.input, "input", "", "candidate/query JSONL; gold is a separate scorer input")
	flag.StringVar(&c.output, "output", "", "artifact directory")
	flag.StringVar(&c.collection, "collection", "", "isolated gorelay_eval_* Qdrant collection")
	flag.StringVar(&c.fingerprint, "fingerprint", "", "explicit model and chunk-layout fingerprint")
	flag.StringVar(&c.mode, "mode", "dense", "dense, bm25, or hybrid")
	flag.StringVar(&c.vectorURL, "vector-url", "http://127.0.0.1:36433", "Qdrant endpoint")
	flag.StringVar(&c.embedURL, "embed-url", "http://127.0.0.1:8357/v1", "local embedding endpoint")
	flag.Parse()
	if err := run(context.Background(), c); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func openStore(ctx context.Context, name string) (*knowledge.Store, error) {
	if !regexp.MustCompile(`^gorelay_eval_[a-zA-Z0-9_]{1,48}$`).MatchString(name) {
		return nil, fmt.Errorf("explicit isolated gorelay_eval_* schema required")
	}
	raw := os.Getenv("GORELAY_EVAL_MYSQL_DSN")
	if raw == "" {
		return nil, fmt.Errorf("GORELAY_EVAL_MYSQL_DSN required; do not put credentials on the command line")
	}
	cfg, err := mysqlDriver.ParseDSN(raw)
	if err != nil {
		return nil, err
	}
	cfg.DBName = ""
	cfg.ParseTime = true
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	_, err = admin.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS `"+name+"` CHARACTER SET utf8mb4")
	admin.Close()
	if err != nil {
		return nil, err
	}
	cfg.DBName = name
	db, err := gorm.Open(mysqlGorm.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, err
	}
	store := knowledge.NewStore(db)
	return store, store.Migrate()
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(path+".tmp", data, 0600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func run(ctx context.Context, c config) error {
	if c.output == "" || c.fingerprint == "" {
		return fmt.Errorf("output directory and explicit fingerprint required")
	}
	if c.command != "import" && c.command != "index" && c.command != "retrieve" {
		return fmt.Errorf("unknown command %q", c.command)
	}
	if err := os.MkdirAll(c.output, 0700); err != nil {
		return err
	}
	store, err := openStore(ctx, c.database)
	if err != nil {
		return err
	}
	if c.command == "import" {
		return importCandidates(ctx, store, c)
	}
	if !strings.HasPrefix(c.collection, "gorelay_eval_") {
		return fmt.Errorf("explicit isolated gorelay_eval_* collection required")
	}
	vectors, err := knowledge.NewQdrant(c.vectorURL, c.collection, "")
	if err != nil {
		return err
	}
	if err = vectors.EnsureCollection(ctx, 512); err != nil {
		return err
	}
	provider, err := aiapp.NewModelProvider(aiapp.ProviderConfig{BaseURL: c.embedURL, Model: "bge-small-zh-v1.5-f16", APIKey: "local-bge"})
	if err != nil {
		return err
	}
	embed := aiapp.EmbeddingAdapter{Provider: provider}
	if c.command == "index" {
		idx := knowledge.Indexer{Store: store, Embed: embed, Vectors: vectors, Fingerprint: c.fingerprint}
		started := time.Now()
		processed := 0
		for {
			var remaining int64
			if err := store.DB.WithContext(ctx).Model(&knowledge.IndexWork{}).Where("status <> ? AND status <> ?", "ready", "superseded").Count(&remaining).Error; err != nil {
				return err
			}
			if remaining == 0 {
				return writeJSON(filepath.Join(c.output, "index-status.json"), map[string]any{"phase": "ready", "processed": processed, "remaining": 0, "elapsed_ms": time.Since(started).Milliseconds(), "fingerprint": c.fingerprint, "collection": c.collection})
			}
			var claimable int64
			now := time.Now()
			if err := store.DB.WithContext(ctx).Model(&knowledge.IndexWork{}).Where("status = ? OR (status = ? AND next_retry_at <= ?) OR (status = ? AND lease_until <= ?)", "pending", "failed", now, "running", now).Count(&claimable).Error; err != nil {
				return err
			}
			if claimable == 0 {
				return fmt.Errorf("%d unsettled index works have a live lease or future retry; inspect saved work instead of counting idle iterations as progress", remaining)
			}
			if err := idx.Once(ctx); err != nil {
				writeJSON(filepath.Join(c.output, "index-status.json"), map[string]any{"phase": "failed", "processed": processed, "remaining": remaining, "error": err.Error()})
				return err
			}
			processed++
			if err := writeJSON(filepath.Join(c.output, "index-status.json"), map[string]any{"phase": "indexing", "processed": processed, "remaining": remaining - 1, "elapsed_ms": time.Since(started).Milliseconds(), "fingerprint": c.fingerprint, "collection": c.collection}); err != nil {
				return err
			}
		}
	}
	policy, err := knowledge.NormalizeRetrievalPolicy(knowledge.RetrievalPolicy{Mode: c.mode, CandidateBudget: 60, RRFK: 60})
	if err != nil {
		return err
	}
	retriever := knowledge.Retriever{Store: store, Embed: embed, Vectors: vectors, Policy: policy, Lexical: &knowledge.LexicalCache{}}
	f, err := os.Open(c.input)
	if err != nil {
		return err
	}
	defer f.Close()
	inputHash, err := hashFile(c.input)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(filepath.Join(c.output, "retrieval-"+c.mode+".jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	decoder, encoder := json.NewDecoder(f), json.NewEncoder(out)
	for {
		var q queryInput
		if err := decoder.Decode(&q); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		p := policy
		p.Scope = q.Scope
		set, cause := retriever.RetrieveWithPolicy(ctx, q.Query, 10, p)
		receipt := map[string]any{"query_id": q.ID, "query": q.Query, "policy": p, "input_sha256": inputHash, "evidence": set, "fingerprint": c.fingerprint, "created_at": time.Now().UTC()}
		if cause != nil {
			receipt["error"] = cause.Error()
		}
		if err := encoder.Encode(receipt); err != nil {
			return err
		}
	}
	return nil
}

func importCandidates(ctx context.Context, store *knowledge.Store, c config) error {
	inputHash, err := hashFile(c.input)
	if err != nil {
		return err
	}
	f, err := os.Open(c.input)
	if err != nil {
		return err
	}
	defer f.Close()
	out, err := os.OpenFile(filepath.Join(c.output, "import-receipts.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	decoder, encoder := json.NewDecoder(f), json.NewEncoder(out)
	count := 0
	seen := map[string]bool{}
	for {
		var row candidate
		if err := decoder.Decode(&row); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		if row.ID == "" || row.Body == "" || row.Source.SHA256 == "" || row.Source.FetchedAt.IsZero() || seen[row.ID] {
			return fmt.Errorf("invalid/duplicate corpus input %q", row.ID)
		}
		seen[row.ID] = true
		doc, err := knowledge.NewDocument(knowledge.Input{Source: "e7-" + row.Family, SourceKey: row.ID, URL: row.Source.URL, Title: row.LogicalKey, Body: row.Body, Format: "text", FetchedAt: row.Source.FetchedAt, DataVersion: row.Source.SHA256, RecordKey: row.ID})
		if err != nil {
			return fmt.Errorf("%s: %w", row.ID, err)
		}
		head, err := store.Put(ctx, doc, c.fingerprint)
		if err != nil {
			return fmt.Errorf("%s: %w", row.ID, err)
		}
		if err := encoder.Encode(map[string]any{"input_id": row.ID, "input_sha256": inputHash, "source_sha256": row.Source.SHA256, "document_id": doc.ID, "revision": doc.Revision, "head_revision": head.Revision, "source": doc.Source, "bytes": len(doc.Body), "fetched_at": doc.FetchedAt}); err != nil {
			return err
		}
		count++
	}
	return writeJSON(filepath.Join(c.output, "import-status.json"), map[string]any{"phase": "imported", "logical_items": count, "candidate_sha256": inputHash, "fingerprint": c.fingerprint, "database": c.database, "formal_outcomes": false})
}

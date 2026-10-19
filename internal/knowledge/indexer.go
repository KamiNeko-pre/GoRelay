package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"gorm.io/gorm"
	"strings"
	"time"
)

var ErrCorePending = errors.New("BuildIndexPlan awaits learner implementation")

type IndexInput struct {
	Head   Head
	Work   IndexWork
	Parent *IndexWork
}
type IndexPlan struct {
	Mode             string
	ParentGeneration string
}

// Only the current active target can publish. Metadata changes may reuse vectors.
func BuildIndexPlan(ctx context.Context, in IndexInput) (IndexPlan, error) {
	if err := ctx.Err(); err != nil {
		return IndexPlan{}, err
	}
	h, w := in.Head, in.Work
	if !h.Active || h.ID == "" || w.Generation == "" || h.ID != w.DocID || h.Epoch != w.Epoch || h.Revision != w.Revision || h.BodyHash != w.BodyHash || h.Fingerprint != w.Fingerprint {
		return IndexPlan{Mode: "reject"}, nil
	}
	p := in.Parent
	if p != nil && p.DocID == w.DocID && p.Generation != "" && p.Generation == h.ParentGeneration && p.Status == "ready" && p.BodyHash == w.BodyHash && p.Fingerprint == w.Fingerprint {
		return IndexPlan{Mode: "reuse", ParentGeneration: p.Generation}, nil
	}
	return IndexPlan{Mode: "rebuild"}, nil
}

type VectorPoint struct {
	ID      string         `json:"id"`
	Vector  []float32      `json:"vector"`
	Payload map[string]any `json:"payload"`
}
type VectorHit struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
}
type VectorStore interface {
	Upsert(context.Context, []VectorPoint) error
	Search(context.Context, []float32, int) ([]VectorHit, error)
}
type Embedder interface {
	EmbedTexts(context.Context, []string) ([][]float32, error)
}
type Indexer struct {
	RequireSpendPolicy bool
	Store              *Store
	Embed              Embedder
	Vectors            VectorStore
	Planner            func(context.Context, IndexInput) (IndexPlan, error)
	Fingerprint        string
	ChunkSize          int // both zero retain the historical 600/80 window.
	ChunkOverlap       int
}

func SplitDocument(doc Document, size, overlap int, generation string) []Chunk {
	if size < 1 || overlap < 0 || overlap >= size {
		panic("invalid chunk size/overlap")
	}
	body := []rune(doc.Body)
	out := []Chunk{}
	for start := 0; start < len(body); {
		end := min(start+size, len(body))
		number := len(out)
		hash := agentdata.Hash([]any{doc.ID, generation, number})[:32]
		id := hash[:8] + "-" + hash[8:12] + "-" + hash[12:16] + "-" + hash[16:20] + "-" + hash[20:]
		text := string(body[start:end])
		if strings.TrimSpace(text) != "" {
			out = append(out, Chunk{ID: id, DocID: doc.ID, Revision: doc.Revision, Generation: generation, Number: number, Start: start, End: end, Text: text})
		}
		if end == len(body) {
			break
		}
		start = end - overlap
	}
	return out
}
func (i *Indexer) Once(ctx context.Context) (runErr error) {
	work, err := i.Store.Claim(ctx, 3*time.Minute)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	} // Empty queue is normal idle state.
	if err != nil {
		return err
	}
	attempt, err := i.Store.startIndexAttempt(ctx, work)
	if err != nil {
		return err
	}
	metrics := IndexMetrics{QueueMS: attempt.StartedAt.Sub(work.CreatedAt).Milliseconds(), EmbeddingOutcome: "not_called"}
	defer func() {
		metrics.TotalMS = time.Since(attempt.StartedAt).Milliseconds()
		runErr = errors.Join(runErr, i.Store.saveIndexMetrics(ctx, attempt, metrics, true, runErr))
	}()
	err = i.process(ctx, work, attempt, &metrics)
	if err != nil {
		if failErr := i.Store.Fail(ctx, work, err); failErr != nil {
			return fmt.Errorf("index error %v; save failure: %w", err, failErr)
		}
	}
	return err
}
func (i *Indexer) process(ctx context.Context, work IndexWork, attempt IndexAttempt, metrics *IndexMetrics) error {
	head, err := i.Store.Head(ctx, work.DocID)
	if err != nil {
		return err
	}
	if !head.Active || head.Epoch != work.Epoch || head.Revision != work.Revision || head.Fingerprint != work.Fingerprint {
		return ErrIndexConflict
	}
	if i.Fingerprint != work.Fingerprint {
		return fmt.Errorf("index worker embedding fingerprint mismatch")
	}
	input := IndexInput{Head: head, Work: work}
	if head.ParentGeneration != "" {
		parent, e := i.Store.Work(ctx, head.ParentGeneration)
		if e == nil {
			input.Parent = &parent
		}
	}
	planner := i.Planner
	if planner == nil {
		planner = BuildIndexPlan
	}
	stageStart := time.Now()
	plan, err := planner(ctx, input)
	metrics.PlanMS = time.Since(stageStart).Milliseconds()
	metrics.PlanMode = plan.Mode
	if err != nil {
		return err
	}
	if plan.Mode == "reject" {
		return ErrIndexConflict
	}
	doc, err := i.Store.Document(ctx, work.DocID, work.Revision)
	if err != nil {
		return err
	}
	stageStart = time.Now()
	size, overlap := i.ChunkSize, i.ChunkOverlap
	if size == 0 && overlap == 0 {
		size, overlap = 600, 80
	}
	if size < 1 || overlap < 0 || overlap >= size {
		return fmt.Errorf("invalid index chunk layout")
	}
	chunks := SplitDocument(doc, size, overlap, work.Generation)
	if len(chunks) == 0 {
		return fmt.Errorf("document has no non-whitespace indexable content")
	}
	metrics.SplitMS, metrics.Chunks = time.Since(stageStart).Milliseconds(), len(chunks)
	vectors := [][]float32{}
	if plan.Mode == "reuse" {
		if input.Parent == nil || input.Parent.Status != "ready" || input.Parent.BodyHash != work.BodyHash || input.Parent.Fingerprint != work.Fingerprint || plan.ParentGeneration != input.Parent.Generation {
			return fmt.Errorf("unsafe vector reuse plan")
		}
		old, err := i.Store.GenerationChunks(ctx, plan.ParentGeneration)
		if err != nil {
			return err
		}
		if len(old) != len(chunks) {
			return fmt.Errorf("reuse chunk manifest mismatch")
		}
		for _, chunk := range old {
			var vector []float32
			if err = json.Unmarshal([]byte(chunk.VectorJSON), &vector); err != nil {
				return err
			}
			vectors = append(vectors, vector)
		}
	} else if plan.Mode == "rebuild" {
		texts := make([]string, len(chunks))
		for n, c := range chunks {
			texts[n] = c.Text
		}
		costOp, err := i.reserveEmbeddingSpend(ctx, work, attempt, texts)
		if err != nil {
			return err
		}
		metrics.EmbeddingCalls, metrics.EmbeddingInputs, metrics.EmbeddingOutcome = 1, len(texts), "started_outcome_unknown"
		if err = i.Store.saveIndexMetrics(ctx, attempt, *metrics, false, nil); err != nil {
			return err
		}
		stageStart = time.Now()
		if embed, ok := i.Embed.(MeasuredEmbedder); ok {
			response, embedErr := embed.EmbedMeasured(ctx, texts)
			vectors, err = response.Vectors, embedErr
			metrics.Usage, metrics.Model, metrics.RequestID = response.Usage, response.Model, response.RequestID
		} else {
			vectors, err = i.Embed.EmbedTexts(ctx, texts)
		}
		if costOp.ID != 0 {
			stage := "succeeded"
			if err != nil {
				stage = "failed"
			}
			if saveErr := agentdata.NewStore(i.Store.DB).CompleteIndexSpend(ctx, costOp.ID, stage); saveErr != nil {
				return saveErr
			}
		}
		metrics.EmbeddingMS = time.Since(stageStart).Milliseconds()
		metrics.EmbeddingOutcome = "returned"
		if err != nil {
			metrics.EmbeddingOutcome = "error_outcome_unknown"
		}
		if saveErr := i.Store.saveIndexMetrics(ctx, attempt, *metrics, false, nil); saveErr != nil {
			return errors.Join(err, saveErr)
		}
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("unknown index plan mode")
	}
	if len(vectors) != len(chunks) {
		return fmt.Errorf("embedding count mismatch")
	}
	for n, c := range chunks {
		point := VectorPoint{ID: c.ID, Vector: vectors[n], Payload: map[string]any{"doc_id": c.DocID, "revision": c.Revision, "generation": c.Generation, "number": c.Number, "start": c.Start, "end": c.End, "title": doc.Title, "url": doc.URL}}
		stageStart = time.Now()
		err = i.Vectors.Upsert(ctx, []VectorPoint{point})
		metrics.VectorMS += time.Since(stageStart).Milliseconds()
		if err != nil {
			return err
		}
		stageStart = time.Now()
		err = i.Store.SaveChunk(ctx, work, c, vectors[n])
		metrics.SQLMS += time.Since(stageStart).Milliseconds()
		if err != nil {
			return err
		}
	}
	stageStart = time.Now()
	err = i.Store.Publish(ctx, work, len(chunks))
	metrics.SQLMS += time.Since(stageStart).Milliseconds()
	return err
}

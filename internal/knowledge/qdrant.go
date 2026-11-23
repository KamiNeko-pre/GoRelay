package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Qdrant struct {
	endpoint, collection, key string
	client                    *http.Client
}

type vectorHTTPError struct {
	Status     int
	RetryAfter string
}

func (e *vectorHTTPError) Error() string { return fmt.Sprintf("Qdrant status %d", e.Status) }

func NewQdrant(endpoint, collection, key string) (*Qdrant, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || collection == "" {
		return nil, fmt.Errorf("invalid Qdrant endpoint or collection")
	}
	return &Qdrant{strings.TrimRight(endpoint, "/"), url.PathEscape(collection), key, &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("Qdrant redirects disabled") }}}, nil
}
func (q *Qdrant) call(ctx context.Context, method, path string, in, out any) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, q.endpoint+"/collections/"+q.collection+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if q.key != "" {
		req.Header.Set("api-key", q.key)
	}
	resp, err := q.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &vectorHTTPError{Status: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After")}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}
func (q *Qdrant) EnsureCollection(ctx context.Context, dimensions int) error {
	req, err := http.NewRequestWithContext(ctx, "GET", q.endpoint+"/collections/"+q.collection, nil)
	if err != nil {
		return err
	}
	if q.key != "" {
		req.Header.Set("api-key", q.key)
	}
	resp, err := q.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return q.call(ctx, "PUT", "", map[string]any{"vectors": map[string]any{"size": dimensions, "distance": "Cosine"}}, nil)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("Qdrant collection status %d", resp.StatusCode)
	}
	var wire struct {
		Result struct {
			Config struct {
				Params struct {
					Vectors struct {
						Size int `json:"size"`
					} `json:"vectors"`
				} `json:"params"`
			} `json:"config"`
		} `json:"result"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return err
	}
	if wire.Result.Config.Params.Vectors.Size != dimensions {
		return fmt.Errorf("Qdrant vector dimension mismatch")
	}
	return nil
}
func (q *Qdrant) Upsert(ctx context.Context, points []VectorPoint) error {
	var wire struct {
		Result struct {
			Status string `json:"status"`
		} `json:"result"`
	}
	// Stable point IDs and identical payloads make acknowledgement-loss replay
	// idempotent. Retry here, not the whole index plan or paid Embedding call.
	for attempt := 0; attempt < 2; attempt++ {
		err := q.call(ctx, "PUT", "/points?wait=true", map[string]any{"points": points}, &wire)
		if err == nil {
			break
		}
		var status *vectorHTTPError
		var network net.Error
		retryable := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &network)
		delay := 150 * time.Millisecond
		if errors.As(err, &status) {
			retryable = status.Status == 429 || status.Status == 502 || status.Status == 503 || status.Status == 504
			if seconds, e := strconv.Atoi(status.RetryAfter); e == nil && seconds >= 0 {
				delay = time.Duration(seconds) * time.Second
			} else if until, e := http.ParseTime(status.RetryAfter); e == nil {
				delay = max(time.Duration(0), time.Until(until))
			}
		}
		if !retryable || attempt == 1 {
			return err
		}
		if q.client.Timeout > 0 && delay > q.client.Timeout {
			return err
		}
		if deadline, ok := ctx.Deadline(); ok && time.Now().Add(delay).After(deadline) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	if wire.Result.Status != "completed" {
		return fmt.Errorf("Qdrant write not completed")
	}
	return nil
}
func (q *Qdrant) Search(ctx context.Context, vector []float32, limit int) ([]VectorHit, error) {
	return q.search(ctx, vector, limit, nil)
}

func (q *Qdrant) SearchGenerations(ctx context.Context, vector []float32, limit int, generations []string) ([]VectorHit, error) {
	if len(generations) == 0 {
		return []VectorHit{}, nil
	}
	return q.search(ctx, vector, limit, map[string]any{"must": []any{map[string]any{"key": "generation", "match": map[string]any{"any": generations}}}})
}

func (q *Qdrant) search(ctx context.Context, vector []float32, limit int, filter any) ([]VectorHit, error) {
	var wire struct {
		Result struct {
			Points []VectorHit `json:"points"`
		} `json:"result"`
	}
	request := map[string]any{"query": vector, "limit": limit, "with_payload": false}
	if filter != nil {
		request["filter"] = filter
	}
	err := q.call(ctx, "POST", "/points/query", request, &wire)
	return wire.Result.Points, err
}

package acquisition

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type ReplayEntry struct {
	Method    string    `json:"method"`
	URL       string    `json:"url"`
	Status    int       `json:"status"`
	Body      string    `json:"body"`
	FetchedAt time.Time `json:"fetched_at"`
}
type Replay struct {
	mu       sync.Mutex
	entries  []ReplayEntry
	position int
}

func NewReplay(entries []ReplayEntry) *Replay {
	return &Replay{entries: append([]ReplayEntry{}, entries...)}
}
func (r *Replay) Remaining() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries) - r.position
}

// Explicit offline adapter. Never falls back to the live network or guesses a
// response; query strings and request order must match the frozen fixture.
func (r *Replay) Fetch(ctx context.Context, method, url string) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.position >= len(r.entries) {
		return Response{}, fmt.Errorf("replay response pool exhausted")
	}
	entry := r.entries[r.position]
	if method != entry.Method || url != entry.URL {
		return Response{}, fmt.Errorf("replay request %d does not match frozen method/URL", r.position+1)
	}
	r.position++
	out := Response{Body: entry.Body, Receipt: Receipt{Method: method, RequestedURL: url, URL: entry.URL, Status: entry.Status, Bytes: int64(len(entry.Body)), FetchedAt: entry.FetchedAt}}
	if entry.Status < 200 || entry.Status >= 300 {
		return out, fmt.Errorf("replayed source HTTP status %d", entry.Status)
	}
	return out, nil
}

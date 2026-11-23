package programrunner

import (
	"context"
	"fmt"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/engine"
)

const SnapshotReferencesHeader = "X-GoRelay-Snapshot-References"
const SnapshotBatchHeader = "X-GoRelay-Snapshot-Batch"

type SnapshotLookup func([]string) (map[string]acquisition.Response, error)

type Request struct {
	Program            engine.ProgramRequest           `json:"program"`
	Limits             engine.ProgramLimits            `json:"limits"`
	AllowedDomains     []string                        `json:"allowed_domains,omitempty"`
	Snapshots          map[string]acquisition.Response `json:"snapshots,omitempty"`
	SnapshotReferences bool                            `json:"snapshot_references,omitempty"`
}
type Error struct {
	AdmissionRejected bool                  `json:"-"`
	Code              string                `json:"code"`
	Message           string                `json:"message"`
	Limits            *engine.ProgramLimits `json:"limits,omitempty"`
	ObservedIO        *acquisition.Stats    `json:"observed_io,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

type Response struct {
	Result        *engine.ProgramResult `json:"result,omitempty"`
	Error         *Error                `json:"error,omitempty"`
	OmittedBodies []int                 `json:"omitted_bodies,omitempty"`
}

func (in Request) savedEvidenceBody(index int, source acquisition.Response) (string, bool) {
	if index == 0 && in.Program.SavedResponse != nil {
		saved := in.Program.SavedResponse
		return saved.Body, source.Receipt.URL == saved.Receipt.URL && source.Receipt.FetchedAt.Equal(saved.Receipt.FetchedAt)
	}
	if !source.Receipt.ReusedSnapshot {
		return "", false
	}
	saved, ok := in.Snapshots[source.Receipt.SnapshotKey]
	return saved.Body, ok && source.Receipt.URL == saved.Receipt.URL && source.Receipt.FetchedAt.Equal(saved.Receipt.FetchedAt)
}

// RestoreSnapshotBodies restores exact inputs from this execution, preserving
// evidence indices and receipt metadata for GoRelay's citation binding.
func (out *Response) RestoreSnapshotBodies(in Request) error {
	for _, index := range out.OmittedBodies {
		if !in.SnapshotReferences || out.Result == nil || index < 0 || index >= len(out.Result.SourceEvidence) {
			return fmt.Errorf("invalid runner snapshot reference at evidence index %d", index)
		}
		evidence := &out.Result.SourceEvidence[index]
		body, ok := in.savedEvidenceBody(index, *evidence)
		if !ok {
			return fmt.Errorf("runner referenced unavailable input snapshot at evidence index %d", index)
		}
		evidence.Body = body
	}
	return nil
}

func Run(ctx context.Context, in Request) (Response, error) {
	return RunWithSnapshotLookup(ctx, in, nil)
}

func RunWithSnapshotLookup(ctx context.Context, in Request, lookup SnapshotLookup) (Response, error) {
	limits := in.Limits
	maxBody := acquisition.ConfiguredMaxResponseBytes()
	if limits.MaxBytes > 0 && (maxBody <= 0 || limits.MaxBytes < maxBody) {
		maxBody = limits.MaxBytes
	}
	fetcher, err := acquisition.NewPublicFetcher(acquisition.Policy{AllowedDomains: in.AllowedDomains, Timeout: limits.Timeout, MaxBodyBytes: maxBody})
	if err != nil {
		return Response{}, err
	}
	defer fetcher.Close()
	checked := map[string]bool{}
	result, err := engine.RunProgramWithRequester(ctx, in.Program, limits, func(ctx context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
		if lookup != nil {
			var missing []string
			for _, key := range fetcher.SnapshotCandidates(ctx, opts) {
				if !checked[key] {
					missing = append(missing, key)
				}
			}
			if len(missing) > 0 {
				entries, err := lookup(missing)
				if err != nil {
					return acquisition.Response{}, err
				}
				if in.Snapshots == nil {
					in.Snapshots = make(map[string]acquisition.Response)
				}
				for _, key := range missing {
					checked[key] = true
					if saved, ok := entries[key]; ok {
						in.Snapshots[key] = saved
					}
				}
			}
		}
		return fetcher.FetchWithSnapshots(ctx, opts, in.Snapshots)
	})
	// Production stats include actual HTTP attempts/redirect hops, even on failure.
	result.Stats = fetcher.Stats()
	result.HTTPUsageKnown = true
	out := Response{Result: &result}
	if in.SnapshotReferences {
		for index, evidence := range result.SourceEvidence {
			if body, ok := in.savedEvidenceBody(index, evidence); ok && body != "" && body == evidence.Body {
				out.OmittedBodies = append(out.OmittedBodies, index)
				result.SourceEvidence[index].Body = ""
			}
		}
	}
	return out, err
}

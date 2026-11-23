package programrunner

import (
	"context"
	"fmt"
	"gorelay/internal/acquisition"
	"gorelay/internal/engine"
	"gorelay/internal/fetchgov"
)

const SnapshotReferencesHeader = "X-GoRelay-Snapshot-References"
const SnapshotBatchHeader = "X-GoRelay-Snapshot-Batch"

type SnapshotLookup func([]string) (map[string]acquisition.Response, error)

type Request struct {
	Program                 engine.ProgramRequest           `json:"program"`
	Limits                  engine.ProgramLimits            `json:"limits"`
	AllowedDomains          []string                        `json:"allowed_domains,omitempty"`
	Snapshots               map[string]acquisition.Response `json:"snapshots,omitempty"`
	SnapshotReferences      bool                            `json:"snapshot_references,omitempty"`
	AuthProfile             string                          `json:"auth_profile,omitempty"`
	RespectRobots           bool                            `json:"respect_robots,omitempty"`
	HasCheckpoint           bool                            `json:"has_checkpoint,omitempty"`
	CheckpointCallbackURL   string                          `json:"checkpoint_callback_url,omitempty"`
	CheckpointCallbackToken string                          `json:"checkpoint_callback_token,omitempty"`
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
	return RunWithHooks(ctx, in, nil, nil)
}

func RunWithSnapshotLookup(ctx context.Context, in Request, lookup SnapshotLookup) (Response, error) {
	return RunWithHooks(ctx, in, lookup, nil)
}

func RunWithHooks(ctx context.Context, in Request, lookup SnapshotLookup, cp *engine.CheckpointHooks) (Response, error) {
	return RunWithClient(ctx, in, lookup, cp, nil)
}

func RunWithClient(ctx context.Context, in Request, lookup SnapshotLookup, cp *engine.CheckpointHooks, clientOverride *fetchgov.SafeClient) (Response, error) {
	limits := in.Limits
	maxBody := acquisition.ConfiguredMaxResponseBytes()
	if maxBody <= 0 || maxBody > fetchgov.DefaultMaxBodyBytes {
		maxBody = fetchgov.DefaultMaxBodyBytes
	}
	if limits.MaxBytes > 0 && limits.MaxBytes < maxBody {
		maxBody = limits.MaxBytes
	}
	pol := acquisition.Policy{AllowedDomains: in.AllowedDomains, Timeout: limits.Timeout, MaxBodyBytes: maxBody}
	fetcher, err := acquisition.NewPublicFetcher(pol)
	if err != nil {
		return Response{}, err
	}
	defer fetcher.Close()

	var gov *fetchgov.SafeClient
	if clientOverride != nil {
		gov = clientOverride
	} else if in.RespectRobots || in.AuthProfile != "" {
		cfg := fetchgov.DefaultConfig()
		cfg.Policy = pol
		cfg.MaxBodyBytes = maxBody
		cfg.RespectRobots = in.RespectRobots
		cfg.RequesterFactory = func(acquisition.Policy) (fetchgov.Requester, error) {
			return &noCloseRequester{fetcher}, nil
		}
		gov, err = fetchgov.NewSafeClient(cfg)
		if err != nil {
			return Response{}, err
		}
	}

	checked := map[string]bool{}
	result, err := engine.RunProgramWithHooks(ctx, in.Program, limits, func(ctx context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
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
		if len(in.Snapshots) > 0 {
			for _, cand := range fetcher.SnapshotCandidates(ctx, opts) {
				if _, ok := in.Snapshots[cand]; ok {
					return fetcher.FetchWithSnapshots(ctx, opts, in.Snapshots)
				}
			}
		}
		if gov != nil {
			return gov.Fetch(ctx, fetchgov.FetchRequest{
				URL:            opts.URL,
				Method:         opts.Method,
				Headers:        opts.Headers,
				Body:           opts.Body,
				AuthProfile:    in.AuthProfile,
				AllowedDomains: in.AllowedDomains,
			})
		}
		return fetcher.FetchWithSnapshots(ctx, opts, in.Snapshots)
	}, cp)
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

type noCloseRequester struct{ *acquisition.PublicFetcher }

func (noCloseRequester) Close() {}

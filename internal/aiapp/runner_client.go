package aiapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/programrunner"
	"net/http"
	"strings"
	"time"

	"go-lua-crawler/internal/engine"
)

type RunnerClient struct {
	endpoint string
	token    string
	client   *http.Client
}

type runnerSnapshotBatchKey struct{}

// ShareSnapshots ties the visibility of a snapshot batch to this tool call's
// context. The regular per-request path remains usable on an older runner.
func (c *RunnerClient) ShareSnapshots(ctx context.Context, snapshots map[string]acquisition.Response) (context.Context, func(), error) {
	payload, err := json.Marshal(snapshots)
	if err != nil {
		return ctx, nil, err
	}
	batchCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(batchCtx, http.MethodPost, c.endpoint+"/snapshot-batch", bytes.NewReader(payload))
	if err != nil {
		cancel()
		return ctx, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	client := *c.client
	client.Timeout = 0 // Batch ownership follows context and its open response.
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return ctx, nil, err
	}
	closeBatch := func() { cancel(); _ = resp.Body.Close() }
	if resp.StatusCode == http.StatusNotFound {
		closeBatch()
		return ctx, func() {}, nil
	}
	var batch struct {
		ID string `json:"id"`
	}
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&batch); err == nil && batch.ID != "" {
			return context.WithValue(batchCtx, runnerSnapshotBatchKey{}, batch.ID), closeBatch, nil
		}
	}
	closeBatch()
	return ctx, nil, fmt.Errorf("runner snapshot batch was not admitted (HTTP %d)", resp.StatusCode)
}

func NewRunnerClient(endpoint, token string) (*RunnerClient, error) {
	endpoint = strings.TrimRight(endpoint, "/")
	if err := configuredURL(endpoint); err != nil {
		return nil, err
	}
	if token == "" {
		return nil, fmt.Errorf("runner token is not configured")
	}
	return &RunnerClient{endpoint, token, providerClient(30 * time.Second)}, nil
}
func (c *RunnerClient) Execute(ctx context.Context, in programrunner.Request) (engine.ProgramResult, error) {
	batch, _ := ctx.Value(runnerSnapshotBatchKey{}).(string)
	in.SnapshotReferences = in.Program.SavedResponse != nil || len(in.Snapshots) > 0
	wire := in
	if batch != "" {
		wire.Snapshots = nil
	}
	payload, err := json.Marshal(wire)
	if err != nil {
		return engine.ProgramResult{}, err
	}
	delay := 100 * time.Millisecond
	for {
		result, err := c.executeOnce(ctx, in, payload, batch)
		rejected, ok := err.(*programrunner.Error)
		if !ok || !rejected.AdmissionRejected {
			return result, err
		}
		if batch != "" && rejected.Code == "snapshot_batch_unavailable" {
			batch = ""
			payload, err = json.Marshal(in)
			if err != nil {
				return engine.ProgramResult{}, err
			}
			continue
		}
		// The authenticated runner rejected admission before execution. Waiting here
		// keeps one durable business operation; Lua/network/unknown outcomes never retry.
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, time.Second)
	}
}
func (c *RunnerClient) executeOnce(ctx context.Context, in programrunner.Request, payload []byte, batch string) (engine.ProgramResult, error) {
	var out programrunner.Response
	// The runner may return a useful classified error with a non-2xx status.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/execute", bytes.NewReader(payload))
	if err != nil {
		return engine.ProgramResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	if batch != "" {
		req.Header.Set(programrunner.SnapshotBatchHeader, batch)
	}
	if in.SnapshotReferences {
		req.Header.Set(programrunner.SnapshotReferencesHeader, "1")
	}
	client := *c.client
	client.Timeout = 0 // Caller cancellation owns execution duration unless explicitly configured.
	if in.Limits.Timeout > 0 {
		client.Timeout = in.Limits.Timeout + 5*time.Second
	}
	resp, err := client.Do(req)
	if err != nil {
		return engine.ProgramResult{}, err
	}
	defer resp.Body.Close()
	var decoder *json.Decoder
	if limit := programrunner.ResponseByteLimit(in.Limits); limit > 0 {
		decoder = json.NewDecoder(http.MaxBytesReader(nil, resp.Body, limit))
	} else {
		decoder = json.NewDecoder(resp.Body)
	}
	if err = decoder.Decode(&out); err != nil {
		return engine.ProgramResult{}, fmt.Errorf("invalid runner response: %w", err)
	}
	if err := out.RestoreSnapshotBodies(in); err != nil {
		return engine.ProgramResult{}, err
	}
	// A complete trusted runner result contains its network ledger even when
	// Lua failed before or after IO. Transport loss alone cannot prove zero IO.
	if out.Result != nil {
		out.Result.HTTPUsageKnown = true
	}
	if out.Error != nil {
		beforeExecution := resp.StatusCode == http.StatusServiceUnavailable && out.Error.Code == "busy" ||
			resp.StatusCode == http.StatusConflict && out.Error.Code == "snapshot_batch_unavailable"
		if beforeExecution && out.Result == nil {
			out.Error.AdmissionRejected = true
			return engine.ProgramResult{HTTPUsageKnown: true}, out.Error
		}
		out.Error.Limits = &in.Limits
		if out.Result != nil {
			out.Error.ObservedIO = &out.Result.Stats
			return *out.Result, out.Error
		}
		return engine.ProgramResult{}, out.Error
	}
	if resp.StatusCode != 200 || out.Result == nil {
		return engine.ProgramResult{}, fmt.Errorf("runner HTTP status %d without a result", resp.StatusCode)
	}
	return *out.Result, nil
}

// Production stats include actual HTTP attempts/redirect hops, even on failure.

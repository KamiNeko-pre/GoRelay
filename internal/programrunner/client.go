package programrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gorelay/internal/acquisition"
	"gorelay/internal/engine"
)

type RunnerClient struct {
	endpoint string
	token    string
	client   *http.Client
}

type runnerSnapshotBatchKey struct{}

func validateRunnerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("runner endpoint must be an HTTP(S) URL without credentials/query/fragment")
	}
	return nil
}

func newNoRedirectClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("runner redirects are disabled") },
	}
}

// NewRunnerClient constructs a client for the isolated lua-runner HTTP service.
func NewRunnerClient(endpoint, token string) (*RunnerClient, error) {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if err := validateRunnerURL(endpoint); err != nil {
		return nil, err
	}
	if token == "" {
		return nil, fmt.Errorf("runner token is not configured")
	}
	return &RunnerClient{endpoint: endpoint, token: token, client: newNoRedirectClient(30 * time.Second)}, nil
}

// ShareSnapshots uploads a shared snapshot batch tied to the returned context.
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
	client.Timeout = 0
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

func (c *RunnerClient) Execute(ctx context.Context, in Request) (engine.ProgramResult, error) {
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
		rejected, ok := err.(*Error)
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

func (c *RunnerClient) executeOnce(ctx context.Context, in Request, payload []byte, batch string) (engine.ProgramResult, error) {
	var out Response
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/execute", bytes.NewReader(payload))
	if err != nil {
		return engine.ProgramResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	if batch != "" {
		req.Header.Set(SnapshotBatchHeader, batch)
	}
	if in.SnapshotReferences {
		req.Header.Set(SnapshotReferencesHeader, "1")
	}
	client := *c.client
	client.Timeout = 0
	if in.Limits.Timeout > 0 {
		client.Timeout = in.Limits.Timeout + 5*time.Second
	}
	resp, err := client.Do(req)
	if err != nil {
		return engine.ProgramResult{}, err
	}
	defer resp.Body.Close()
	var decoder *json.Decoder
	if limit := ResponseByteLimit(in.Limits); limit > 0 {
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

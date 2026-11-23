package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"gorelay/internal/acquisition"
	"gorelay/internal/engine"
	"gorelay/internal/fetchgov"
	"gorelay/internal/programrunner"
)

type childSnapshotFrame struct {
	Keys         []string                    `json:"keys,omitempty"`
	HTTPOp       string                      `json:"http_op,omitempty"` // "request" | "render"
	HTTPRequest  *acquisition.RequestOptions `json:"http_request,omitempty"`
	RenderURL    string                      `json:"render_url,omitempty"`
	WaitSelector string                      `json:"wait_selector,omitempty"`
	CheckpointOp string                      `json:"checkpoint_op,omitempty"` // "get" | "save"
	Cursor       any                         `json:"cursor,omitempty"`
	Records      []map[string]any            `json:"records,omitempty"`
	Evidence     []acquisition.Response      `json:"evidence,omitempty"`
	Result       *programrunner.Response     `json:"result,omitempty"`
}

type checkpointReplyFrame struct {
	OK     bool   `json:"ok"`
	Cursor any    `json:"cursor,omitempty"`
	Error  string `json:"error,omitempty"`
}

type httpReplyFrame struct {
	OK            bool                 `json:"ok"`
	Response      acquisition.Response `json:"response,omitempty"`
	ErrorCode     string               `json:"error_code,omitempty"`
	Error         string               `json:"error,omitempty"`
	RetryAfterSec int                  `json:"retry_after_sec,omitempty"`
}

const maxSnapshotLookupLineBytes = 64 * 1024
const maxCheckpointFrameBytes = 4 * 1024 * 1024

// runSnapshotChild executes an untrusted Lua connector inside an isolated OS
// child process with ZERO direct sockets or credentials: every HTTP request,
// browser render, and checkpoint call is proxied over stdin/stdout IPC to the
// long-lived parent lua-runner process.
func runSnapshotChild(input io.Reader, output io.Writer) error {
	decoder := json.NewDecoder(input)
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	encoder := json.NewEncoder(output)
	var request programrunner.Request
	if err := decoder.Decode(&request); err != nil {
		return err
	}

	decodeHTTPReply := func() (acquisition.Response, error) {
		var reply httpReplyFrame
		if err := decoder.Decode(&reply); err != nil {
			return acquisition.Response{}, err
		}
		if !reply.OK {
			if reply.ErrorCode != "" {
				return reply.Response, &fetchgov.GovernanceError{
					Code:          reply.ErrorCode,
					Message:       reply.Error,
					RetryAfterSec: reply.RetryAfterSec,
				}
			}
			return reply.Response, fmt.Errorf("%s", reply.Error)
		}
		return reply.Response, nil
	}

	requestFn := func(_ context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
		if err := encoder.Encode(childSnapshotFrame{
			HTTPOp:      "request",
			HTTPRequest: &opts,
		}); err != nil {
			return acquisition.Response{}, err
		}
		return decodeHTTPReply()
	}

	renderFn := func(_ context.Context, targetURL, waitSelector string) (acquisition.Response, error) {
		if err := encoder.Encode(childSnapshotFrame{
			HTTPOp:       "render",
			RenderURL:    targetURL,
			WaitSelector: waitSelector,
		}); err != nil {
			return acquisition.Response{}, err
		}
		return decodeHTTPReply()
	}

	var cp *engine.CheckpointHooks
	if request.HasCheckpoint {
		cp = &engine.CheckpointHooks{
			Get: func(context.Context) (any, error) {
				if err := encoder.Encode(childSnapshotFrame{CheckpointOp: "get"}); err != nil {
					return nil, err
				}
				var reply checkpointReplyFrame
				if err := decoder.Decode(&reply); err != nil {
					return nil, err
				}
				if !reply.OK {
					return nil, fmt.Errorf("%s", reply.Error)
				}
				return reply.Cursor, nil
			},
			Save: func(_ context.Context, cursor any, records []map[string]any, evidence []acquisition.Response) error {
				if err := encoder.Encode(childSnapshotFrame{
					CheckpointOp: "save",
					Cursor:       cursor,
					Records:      records,
					Evidence:     evidence,
				}); err != nil {
					return err
				}
				var reply checkpointReplyFrame
				if err := decoder.Decode(&reply); err != nil {
					return err
				}
				if !reply.OK {
					return fmt.Errorf("%s", reply.Error)
				}
				return nil
			},
		}
	}

	result, err := programrunner.RunWithHostIPC(context.Background(), request, requestFn, renderFn, cp)
	if err != nil {
		result.Error = classifyError(err)
	}
	return encoder.Encode(childSnapshotFrame{Result: &result})
}

func readChildFrameLine(reader *bufio.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		line, err := reader.ReadBytes('\n')
		if err != nil && len(line) == 0 {
			return nil, err
		}
		return line, nil
	}
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if int64(len(line)+len(chunk)) > maxBytes {
			return nil, fmt.Errorf("child output limit exceeded")
		}
		line = append(line, chunk...)
		if err == nil {
			return line, nil
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF && len(line) > 0 {
			return line, nil
		}
		return nil, err
	}
}

func exchangeChildSnapshots(input io.Writer, output io.Reader, request []byte, snapshots map[string]acquisition.Response, maxResultBytes int64) (programrunner.Response, error) {
	return exchangeChildPipeWithHost(context.Background(), input, output, programrunner.Request{}, request, snapshots, maxResultBytes, nil, nil)
}

func exchangeChildPipe(ctx context.Context, input io.Writer, output io.Reader, request []byte, snapshots map[string]acquisition.Response, maxResultBytes int64, cp *engine.CheckpointHooks) (programrunner.Response, error) {
	return exchangeChildPipeWithHost(ctx, input, output, programrunner.Request{}, request, snapshots, maxResultBytes, cp, nil)
}

func exchangeChildPipeWithHost(
	ctx context.Context,
	input io.Writer,
	output io.Reader,
	reqMeta programrunner.Request,
	request []byte,
	snapshots map[string]acquisition.Response,
	maxResultBytes int64,
	cp *engine.CheckpointHooks,
	gov *fetchgov.SafeClient,
) (programrunner.Response, error) {
	if _, err := input.Write(append(request, '\n')); err != nil {
		return programrunner.Response{}, err
	}
	var snapHelper *acquisition.PublicFetcher
	if len(snapshots) > 0 {
		if f, err := acquisition.NewPublicFetcher(acquisition.Policy{AllowedDomains: reqMeta.AllowedDomains}); err == nil {
			snapHelper = f
			defer snapHelper.Close()
		}
	}

	reader := bufio.NewReader(output)
	encoder := json.NewEncoder(input)
	queriedKeys := make(map[string]bool)
	var observedRequests int
	var observedBytes int64
	lineLimit := maxResultBytes
	if (cp != nil || gov != nil) && (lineLimit == 0 || lineLimit < maxCheckpointFrameBytes) {
		lineLimit = maxCheckpointFrameBytes
	}
	for {
		line, err := readChildFrameLine(reader, lineLimit)
		if err != nil {
			return programrunner.Response{}, fmt.Errorf("runner child ipc channel: %w", err)
		}
		var frame childSnapshotFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			return programrunner.Response{}, fmt.Errorf("runner child ipc channel: %w", err)
		}
		if frame.Result != nil {
			if maxResultBytes > 0 && int64(len(line)) > maxResultBytes {
				return programrunner.Response{}, fmt.Errorf("child output limit exceeded")
			}
			if frame.Result.Result != nil && gov != nil {
				frame.Result.Result.Stats = acquisition.Stats{
					Requests: observedRequests,
					Bytes:    observedBytes,
				}
				frame.Result.Result.HTTPUsageKnown = true
			}
			return *frame.Result, nil
		}
		if frame.HTTPOp != "" {
			reply := handleChildHTTPFrame(ctx, frame, reqMeta, snapshots, snapHelper, gov, &observedRequests, &observedBytes)
			if err := encoder.Encode(reply); err != nil {
				return programrunner.Response{}, err
			}
			continue
		}
		if frame.CheckpointOp != "" {
			var reply checkpointReplyFrame
			if cp == nil {
				reply.Error = "checkpoint handler unavailable"
			} else if frame.CheckpointOp == "get" {
				if cp.Get == nil {
					reply.Error = "checkpoint.get unavailable"
				} else if cur, gerr := cp.Get(ctx); gerr != nil {
					reply.Error = gerr.Error()
				} else {
					reply.OK = true
					reply.Cursor = cur
				}
			} else if frame.CheckpointOp == "save" {
				if cp.Save == nil {
					reply.Error = "checkpoint.save unavailable"
				} else if serr := cp.Save(ctx, frame.Cursor, frame.Records, frame.Evidence); serr != nil {
					reply.Error = serr.Error()
				} else {
					reply.OK = true
				}
			} else {
				reply.Error = "unknown checkpoint_op: " + frame.CheckpointOp
			}
			if err := encoder.Encode(reply); err != nil {
				return programrunner.Response{}, err
			}
			continue
		}
		if len(frame.Keys) == 0 {
			return programrunner.Response{}, fmt.Errorf("runner child requested an empty snapshot lookup")
		}
		if int64(len(line)) > maxSnapshotLookupLineBytes {
			return programrunner.Response{}, fmt.Errorf("child output limit exceeded")
		}
		hits := make(map[string]acquisition.Response, len(frame.Keys))
		for _, key := range frame.Keys {
			if queriedKeys[key] {
				return programrunner.Response{}, fmt.Errorf("runner child repeated snapshot lookup for key %q", key)
			}
			queriedKeys[key] = true
			if response, ok := snapshots[key]; ok {
				hits[key] = response
			}
		}
		if err := encoder.Encode(hits); err != nil {
			return programrunner.Response{}, err
		}
	}
}

func handleChildHTTPFrame(
	ctx context.Context,
	frame childSnapshotFrame,
	reqMeta programrunner.Request,
	snapshots map[string]acquisition.Response,
	snapHelper *acquisition.PublicFetcher,
	gov *fetchgov.SafeClient,
	observedRequests *int,
	observedBytes *int64,
) httpReplyFrame {
	switch frame.HTTPOp {
	case "request":
		if frame.HTTPRequest == nil {
			return httpReplyFrame{Error: "missing http_request payload"}
		}
		opts := *frame.HTTPRequest
		if len(snapshots) > 0 && snapHelper != nil {
			for _, cand := range snapHelper.SnapshotCandidates(ctx, opts) {
				if saved, ok := snapshots[cand]; ok {
					saved.Receipt.ReusedSnapshot = true
					saved.Receipt.DurationMS = 0
					return httpReplyFrame{OK: true, Response: saved}
				}
			}
		}
		if gov == nil {
			return httpReplyFrame{Error: "host SafeClient unavailable"}
		}
		if reqMeta.Limits.MaxRequests > 0 && *observedRequests >= reqMeta.Limits.MaxRequests {
			return httpReplyFrame{
				ErrorCode: "BUDGET_EXCEEDED",
				Error:     fmt.Sprintf("%s: max_requests (%d) reached", acquisition.ErrBudgetExceeded.Error(), reqMeta.Limits.MaxRequests),
			}
		}
		*observedRequests++
		resp, err := gov.Fetch(ctx, fetchgov.FetchRequest{
			URL:            opts.URL,
			Method:         opts.Method,
			Headers:        opts.Headers,
			Body:           opts.Body,
			AuthProfile:    reqMeta.AuthProfile,
			AllowedDomains: reqMeta.AllowedDomains,
		})
		*observedBytes += int64(len(resp.Body))
		if err != nil {
			var ge *fetchgov.GovernanceError
			if errors.As(err, &ge) {
				return httpReplyFrame{OK: false, Response: resp, ErrorCode: ge.Code, Error: ge.Message, RetryAfterSec: ge.RetryAfterSec}
			}
			return httpReplyFrame{OK: false, Response: resp, Error: err.Error()}
		}
		if reqMeta.Limits.MaxBytes > 0 && *observedBytes > reqMeta.Limits.MaxBytes {
			return httpReplyFrame{
				ErrorCode: fetchgov.ErrCodeBodyTooLarge,
				Error:     fmt.Sprintf("response bytes (%d) exceeded limit (%d)", *observedBytes, reqMeta.Limits.MaxBytes),
			}
		}
		return httpReplyFrame{OK: true, Response: resp}

	case "render":
		if gov == nil {
			return httpReplyFrame{ErrorCode: fetchgov.ErrCodeRendererUnavailable, Error: "host SafeClient unavailable"}
		}
		if reqMeta.Limits.MaxRequests > 0 && *observedRequests >= reqMeta.Limits.MaxRequests {
			return httpReplyFrame{
				ErrorCode: "BUDGET_EXCEEDED",
				Error:     fmt.Sprintf("%s: max_requests (%d) reached", acquisition.ErrBudgetExceeded.Error(), reqMeta.Limits.MaxRequests),
			}
		}
		*observedRequests++
		res, err := gov.Render(ctx, fetchgov.RenderRequest{
			URL:            frame.RenderURL,
			WaitSelector:   frame.WaitSelector,
			AllowedDomains: reqMeta.AllowedDomains,
		})
		if err != nil {
			var ge *fetchgov.GovernanceError
			if errors.As(err, &ge) {
				return httpReplyFrame{OK: false, ErrorCode: ge.Code, Error: ge.Message, RetryAfterSec: ge.RetryAfterSec}
			}
			return httpReplyFrame{OK: false, Error: err.Error()}
		}
		resp := programrunner.RenderToResponse(frame.RenderURL, res)
		*observedBytes += int64(len(resp.Body))
		if reqMeta.Limits.MaxBytes > 0 && *observedBytes > reqMeta.Limits.MaxBytes {
			return httpReplyFrame{
				ErrorCode: fetchgov.ErrCodeBodyTooLarge,
				Error:     fmt.Sprintf("rendered bytes (%d) exceeded limit (%d)", *observedBytes, reqMeta.Limits.MaxBytes),
			}
		}
		return httpReplyFrame{OK: true, Response: resp}

	default:
		return httpReplyFrame{Error: "unknown http_op: " + frame.HTTPOp}
	}
}

func makeHTTPCallbackHooks(callbackURL, callbackToken string) *engine.CheckpointHooks {
	if callbackURL == "" {
		return nil
	}
	client := &http.Client{Timeout: 15 * time.Second}
	call := func(ctx context.Context, body any) (checkpointReplyFrame, error) {
		var reply checkpointReplyFrame
		encoded, err := json.Marshal(body)
		if err != nil {
			return reply, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, callbackURL, bytes.NewReader(encoded))
		if err != nil {
			return reply, err
		}
		req.Header.Set("Content-Type", "application/json")
		if callbackToken != "" {
			req.Header.Set("Authorization", "Bearer "+callbackToken)
		}
		resp, err := client.Do(req)
		if err != nil {
			return reply, err
		}
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
			return reply, fmt.Errorf("decode checkpoint reply (%d): %w", resp.StatusCode, err)
		}
		if resp.StatusCode >= 300 || !reply.OK {
			if reply.Error != "" {
				return reply, fmt.Errorf("%s", reply.Error)
			}
			return reply, fmt.Errorf("checkpoint callback HTTP %d", resp.StatusCode)
		}
		return reply, nil
	}
	return &engine.CheckpointHooks{
		Get: func(ctx context.Context) (any, error) {
			rep, err := call(ctx, map[string]any{"op": "get"})
			return rep.Cursor, err
		},
		Save: func(ctx context.Context, cursor any, records []map[string]any, evidence []acquisition.Response) error {
			_, err := call(ctx, map[string]any{
				"op":       "save",
				"cursor":   cursor,
				"records":  records,
				"evidence": evidence,
			})
			return err
		},
	}
}

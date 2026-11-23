package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"gorelay/internal/acquisition"
	"gorelay/internal/engine"
	"gorelay/internal/programrunner"
)

type childSnapshotFrame struct {
	Keys         []string                `json:"keys,omitempty"`
	CheckpointOp string                  `json:"checkpoint_op,omitempty"` // "get" | "save"
	Cursor       any                     `json:"cursor,omitempty"`
	Records      []map[string]any        `json:"records,omitempty"`
	Evidence     []acquisition.Response  `json:"evidence,omitempty"`
	Result       *programrunner.Response `json:"result,omitempty"`
}

type checkpointReplyFrame struct {
	OK     bool   `json:"ok"`
	Cursor any    `json:"cursor,omitempty"`
	Error  string `json:"error,omitempty"`
}

const maxSnapshotLookupLineBytes = 64 * 1024
const maxCheckpointFrameBytes = 4 * 1024 * 1024

func runSnapshotChild(input io.Reader, output io.Writer) error {
	decoder := json.NewDecoder(input)
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	encoder := json.NewEncoder(output)
	var request programrunner.Request
	if err := decoder.Decode(&request); err != nil {
		return err
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

	result, err := programrunner.RunWithHooks(context.Background(), request, func(keys []string) (map[string]acquisition.Response, error) {
		if err := encoder.Encode(childSnapshotFrame{Keys: keys}); err != nil {
			return nil, err
		}
		var snapshots map[string]acquisition.Response
		if err := decoder.Decode(&snapshots); err != nil {
			return nil, err
		}
		return snapshots, nil
	}, cp)
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
	return exchangeChildPipe(context.Background(), input, output, request, snapshots, maxResultBytes, nil)
}

func exchangeChildPipe(ctx context.Context, input io.Writer, output io.Reader, request []byte, snapshots map[string]acquisition.Response, maxResultBytes int64, cp *engine.CheckpointHooks) (programrunner.Response, error) {
	if _, err := input.Write(append(request, '\n')); err != nil {
		return programrunner.Response{}, err
	}
	reader := bufio.NewReader(output)
	encoder := json.NewEncoder(input)
	queriedKeys := make(map[string]bool)
	lineLimit := maxResultBytes
	if cp != nil && (lineLimit == 0 || lineLimit < maxCheckpointFrameBytes) {
		lineLimit = maxCheckpointFrameBytes
	}
	for {
		line, err := readChildFrameLine(reader, lineLimit)
		if err != nil {
			return programrunner.Response{}, fmt.Errorf("runner child snapshot channel: %w", err)
		}
		var frame childSnapshotFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			return programrunner.Response{}, fmt.Errorf("runner child snapshot channel: %w", err)
		}
		if frame.Result != nil {
			if maxResultBytes > 0 && int64(len(line)) > maxResultBytes {
				return programrunner.Response{}, fmt.Errorf("child output limit exceeded")
			}
			return *frame.Result, nil
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

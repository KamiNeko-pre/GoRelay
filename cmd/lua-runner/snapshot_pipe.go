package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/programrunner"
)

type childSnapshotFrame struct {
	Keys   []string                `json:"keys,omitempty"`
	Result *programrunner.Response `json:"result,omitempty"`
}

const maxSnapshotLookupLineBytes = 64 * 1024

func runSnapshotChild(input io.Reader, output io.Writer) error {
	decoder := json.NewDecoder(input)
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	encoder := json.NewEncoder(output)
	var request programrunner.Request
	if err := decoder.Decode(&request); err != nil {
		return err
	}
	result, err := programrunner.RunWithSnapshotLookup(context.Background(), request, func(keys []string) (map[string]acquisition.Response, error) {
		if err := encoder.Encode(childSnapshotFrame{Keys: keys}); err != nil {
			return nil, err
		}
		var snapshots map[string]acquisition.Response
		if err := decoder.Decode(&snapshots); err != nil {
			return nil, err
		}
		return snapshots, nil
	})
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
	if _, err := input.Write(append(request, '\n')); err != nil {
		return programrunner.Response{}, err
	}
	reader := bufio.NewReader(output)
	encoder := json.NewEncoder(input)
	queriedKeys := make(map[string]bool)
	lineLimit := maxResultBytes
	if lineLimit > 0 && lineLimit < maxSnapshotLookupLineBytes {
		lineLimit = maxSnapshotLookupLineBytes
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

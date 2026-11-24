package engine

import (
	"context"
	"strings"
	"testing"

	"gorelay/internal/acquisition"
)

func TestLuaCheckpointGetAndSaveResume(t *testing.T) {
	var savedCursor any = map[string]any{"page": 2}
	var batches [][]map[string]any

	hooks := &CheckpointHooks{
		Get: func(context.Context) (any, error) {
			return savedCursor, nil
		},
		Save: func(_ context.Context, cursor any, records []map[string]any, _ []acquisition.Response) error {
			savedCursor = cursor
			batches = append(batches, records)
			return nil
		},
	}

	code := `
local cp = checkpoint.get()
local page = (cp and cp.page) or 1
local rows = {{id="row-"..tostring(page), title="Page "..tostring(page)}}
checkpoint.save({page = page + 1}, rows)
return true, {items = rows}
`
	res, err := RunProgramWithHooks(context.Background(), ProgramRequest{Code: code}, DefaultProgramLimits(), nil, hooks)
	if err != nil {
		t.Fatalf("unexpected lua checkpoint error: %v", err)
	}
	if len(batches) != 1 || len(batches[0]) != 1 || batches[0][0]["id"] != "row-2" {
		t.Fatalf("expected resumed page 2 batch, got %+v", batches)
	}
	if m, ok := savedCursor.(map[string]any); !ok || m["page"] != float64(3) {
		t.Fatalf("expected advanced checkpoint page=3, got %#v", savedCursor)
	}
	_ = res
}

func TestLuaCheckpointRejectsOversizedBatch(t *testing.T) {
	code := `
local rows = {}
for i = 1, 201 do
  rows[i] = {id = tostring(i)}
end
checkpoint.save(nil, rows)
return true, {items = {}}
`
	_, err := RunProgramWithHooks(context.Background(), ProgramRequest{Code: code}, DefaultProgramLimits(), nil, &CheckpointHooks{})
	if err == nil || !strings.Contains(err.Error(), "200 items limit") {
		t.Fatalf("expected 200 items limit error, got %v", err)
	}
}
